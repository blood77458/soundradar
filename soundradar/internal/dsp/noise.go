// Environment-noise reduction for the soundradar fingerprint (added for the
// "game audio has ambience" problem).
//
// # Why this exists
//
// The fingerprint is a normalised log-mel patch (see dsp.go). The normalisation
// removes any constant gain, so it is immune to the volume being turned down -
// but it is NOT immune to a constant noise FLOOR. Broadband ambience (air
// conditioning, wind, engine rumble, distant gunfire, the game's own music bed)
// lifts the ~2000 mel bands that would otherwise sit at the -20 dB floor. Those
// bands carry no information about the sound effect yet they take up almost the
// whole vector, so the cosine similarity between "clean template" and "template
// plus ambience" collapses - which is exactly the reported symptom: recognition
// degrades whenever there is background noise.
//
// # What this file does
//
// A causal, streaming front-end that runs BEFORE the STFT of every frame and is
// applied identically to library samples (index build) and to live audio, so
// template and query always see the same processing:
//
//  1. high-pass   a 2nd-order Butterworth high-pass (default 120 Hz) removes
//     rumble, wind and DC.
//  2. noise track per-bin event-aware tracking (not minimum statistics). A
//     bin that sticks up above the floor is a tone or a short event and is
//     NOT taught to the floor. A rise that covers most bins for ~200 ms is a
//     scene change and is learned. This is the tracker that does not assume
//     the target is speech, and it does not need a white-noise bias factor.
//  3. Wiener gain each analysis bin is multiplied by S/(S+αN), floored at
//     GainFloorDB. The patch is normalised, so only spectral SHAPE matters:
//     a gain keeps high-SNR bins (the sound) and suppresses low-SNR bins
//     (the ambience). Classic magnitude subtraction would flatten that
//     contrast. Listening-oriented floors around 0.05 are deliberately not
//     used; hollowing a bin out costs more cosine than leaving mild noise.
//  4. transients  a ~10 ms energy detector marks short bursts. Those analysis
//     frames skip the Wiener gain so a broadband click is not erased bin by
//     bin. Longer rises update the energy floor instead (a new scene, not a
//     click). This is the fingerprint equivalent of paste-back: we do not
//     reconstruct a waveform.
//
// # The reported noise floor
//
// The adaptive silence gate needs a level, not a spectrum, and a level that
// means the same thing as LevelDBFS. The per-bin tracker is a subtraction
// reference that is however NOT a dBFS level. So the floor is tracked
// separately, in the TIME domain: a slow minimum of the 20 ms block RMS,
// which drops instantly and rises with a ~2 s time constant.
//
// # Why it does not hurt clean recordings
//
// The estimator is relative and self-scaling: a studio-clean sample has a noise
// floor far below its signal, so αN is negligible and the gain is a no-op. A
// quiet sample keeps its low floor, so "sound recorded quietly" still matches
// "the same sound heard through game ambience".
//
// # Cost
//
// One biquad per sample, plus one extra 1024-point FFT per analysis hop, which
// is still far below the 10 ms per 20 ms of audio budget the live link must
// respect.
package dsp

import (
	"fmt"
	"math"
)

// NoiseReduction method names accepted by NoiseParams.Method.
const (
	// NoiseOff disables the whole front-end (the pre-noise-reduction pipeline).
	NoiseOff = "off"
	// NoiseHighPass runs only the biquad high-pass (cheapest, no FFT).
	NoiseHighPass = "highpass"
	// NoiseSpectral is the documented default: high-pass + spectral gain.
	NoiseSpectral = "subtract"
)

// Event-aware tracker / short-transient detector. Time constants match the
// listening-demo spectralDenoise; the Wiener floor and over-subtraction stay
// at the fingerprint-oriented defaults (see DefaultNoiseParams).
const (
	noiseTrackFast     = 0.55 // track = (1-fast)*track + fast*power
	noiseTrackSlow     = 0.03 // noise += slow*(track-noise) on non-event bins
	noiseSceneMix      = 0.25 // noise += mix*(track-noise) on a scene change
	noiseHotRatio      = 4.0  // track > noise*this => bin is "hot"
	noiseEventRatio    = 3.5  // track > noise*this => do not update that bin
	noiseBroadFrac     = 3    // hot bins > bins/this => broadband rise
	noiseInitFrames    = 5    // average this many frames before the tracker is live
	noiseSceneSeconds  = 0.20 // broadband rise this long is a new scene
	transientWinSec    = 0.010
	transientRatio     = 2.0  // ~3 dB; 2.8 was for waveform paste-back
	transientMaxHops   = 6    // ~32 ms of analysis hops; longer rises are a new scene
	transientFloorFast = 0.15 // energy floor when a long rise is a new scene
	transientFloorSlow = 0.08 // energy floor on ordinary frames
	gainReleasePrev    = 0.75 // per-bin gain falls slowly, rises immediately
)

// NoiseParams configures the environment-noise front-end.
//
// The zero value is NOT "off": withDefaults turns it into the recommended
// defaults, exactly like a zero match.Options gets sane gates. Set Method to
// NoiseOff to disable the front-end explicitly.
type NoiseParams struct {
	// Method is NoiseOff, NoiseHighPass or NoiseSpectral (see above).
	Method string `json:"method"`
	// HighPassHz is the -3 dB corner of the 2nd-order high-pass. 0 uses the
	// default (120 Hz); a value >= SampleRate/2 disables just the filter.
	HighPassHz float64 `json:"highPassHz"`
	// OverSubtract (alpha) inflates the estimated noise power before the gain is
	// derived. It is the main "how aggressive" knob.
	OverSubtract float64 `json:"overSubtract"`
	// GainFloorDB bounds how far ONE bin may be attenuated. It is a gain in dB
	// (default -8 dB, i.e. a bin whose SNR is hopeless is kept at about 0.4 of
	// its amplitude). A floor expressed as "keep x % of the power" would clamp
	// every quiet bin to the same value and leave the noise's spectral shape in
	// the patch, which is exactly what breaks the cosine score.
	GainFloorDB float64 `json:"gainFloorDb"`
	// Mix blends the noise-reduced spectrum back into the original one: 1 keeps
	// only the cleaned spectrum, 0 makes the gain a no-op. It exists so the
	// effect can be dialled back without being switched off.
	Mix float64 `json:"mix"`
	// FrameGateDB is the optional frame-level signal-to-noise gate: a frame whose
	// level is less than this many dB above the tracked noise floor is dropped
	// before the patch is normalised. It is DISABLED by default (a negative
	// value), because measurement showed it does more harm than good.
	//
	// Why it is off: the patch is an L2-normalised concatenation of WindowFrames
	// frames, so a frame holding only ambience contributes its energy and its own
	// unrelated shape. Dropping those frames sounds ideal, but in practice a
	// frame next to a sound effect already contains part of that sound, and
	// dropping it removes real signal. Measured on synthetic bursts with a 3 dB
	// gate, the self-similarity at heavy ambience fell from 0.98 to 0.46 while
	// the per-bin gain below (which is always on) lifted it from 0.88 to 0.98.
	// The knob is kept because the trade-off depends on the material, and a
	// positive value enables the gate with the same semantics.
	FrameGateDB float64 `json:"frameGateDb"`
	// AdaptiveGate lifts the silence gate above the measured noise floor.
	AdaptiveGate bool `json:"adaptiveGate"`
	// GateMarginDB is how far above the noise floor the adaptive gate sits
	// (default 4 dB) and GateFloorDBFS is the lowest value it may take
	// (default -70 dBFS), so a digitally quiet passage keeps the configured
	// gate.
	GateMarginDB  float64 `json:"gateMarginDb"`
	GateFloorDBFS float64 `json:"gateFloorDbfs"`
}

// DefaultNoiseParams returns the recommended settings (see the file comment).
func DefaultNoiseParams() NoiseParams {
	return NoiseParams{
		Method:        NoiseSpectral,
		HighPassHz:    120,
		OverSubtract:  1.5,
		GainFloorDB:   -8,
		Mix:           1.0,
		FrameGateDB:   -1, // disabled; see the field documentation
		AdaptiveGate:  true,
		GateMarginDB:  4,
		GateFloorDBFS: -70,
	}
}

// withDefaults fills every unset field from DefaultNoiseParams. It is the single
// place where "zero value means default" is decided, so Validate, the fingerprint
// and the runtime always agree.
func (n NoiseParams) withDefaults() NoiseParams {
	d := DefaultNoiseParams()
	if n.Method == "" {
		n.Method = d.Method
	} else {
		n.Method = NormalizeNoiseMethod(n.Method)
	}
	if n.HighPassHz == 0 {
		n.HighPassHz = d.HighPassHz
	}
	if n.OverSubtract == 0 {
		n.OverSubtract = d.OverSubtract
	}
	if n.GainFloorDB == 0 {
		n.GainFloorDB = d.GainFloorDB
	}
	if n.Mix == 0 {
		n.Mix = d.Mix
	}
	if n.FrameGateDB == 0 {
		// 0 is not a usable gate value (it would gate nothing), so it keeps the
		// documented default (disabled). A caller that wants the gate sets a
		// positive dB value explicitly, and a negative value keeps it off.
		n.FrameGateDB = d.FrameGateDB
	}
	if n.GateMarginDB == 0 {
		n.GateMarginDB = d.GateMarginDB
	}
	if n.GateFloorDBFS == 0 {
		n.GateFloorDBFS = d.GateFloorDBFS
	}
	return n
}

// NormalizeNoiseMethod maps the accepted spellings onto the canonical names.
// An unknown value is returned unchanged so Validate can reject it.
func NormalizeNoiseMethod(s string) string {
	switch s {
	case NoiseOff, "none", "false", "0":
		return NoiseOff
	case NoiseHighPass, "hp", "high-pass", "high_pass":
		return NoiseHighPass
	case NoiseSpectral, "spectral", "noise", "sub", "on", "true", "1":
		return NoiseSpectral
	}
	return s
}

// Validate checks the front-end parameters. The zero value is valid (it means
// "use the defaults").
func (n NoiseParams) Validate() error {
	if n.Method == "" {
		return nil
	}
	switch NormalizeNoiseMethod(n.Method) {
	case NoiseOff, NoiseHighPass, NoiseSpectral:
	default:
		return fmt.Errorf("未知的环境音过滤方式 %q（可选 %s / %s / %s）",
			n.Method, NoiseOff, NoiseHighPass, NoiseSpectral)
	}
	if n.HighPassHz < 0 || n.HighPassHz >= defaultSampleRate/2 {
		return fmt.Errorf("高通截止频率必须在 [0, %d) Hz 之间，当前 %v",
			defaultSampleRate/2, n.HighPassHz)
	}
	if NormalizeNoiseMethod(n.Method) == NoiseSpectral {
		if n.OverSubtract < 0.1 || n.OverSubtract > 20 {
			return fmt.Errorf("噪声过减系数应在 0.1-20 之间，当前 %v", n.OverSubtract)
		}
		if n.GainFloorDB < -60 || n.GainFloorDB > 0 {
			return fmt.Errorf("单频点增益下限应在 -60-0 dB 之间，当前 %v", n.GainFloorDB)
		}
	}
	if n.Mix < 0 || n.Mix > 1 {
		return fmt.Errorf("干湿比应在 [0, 1] 之间，当前 %v", n.Mix)
	}
	if n.AdaptiveGate {
		if n.GateMarginDB < 0 || n.GateMarginDB > 40 {
			return fmt.Errorf("自适应门限余量应在 0-40 dB 之间，当前 %v", n.GateMarginDB)
		}
		if n.GateFloorDBFS < -120 || n.GateFloorDBFS > 0 {
			return fmt.Errorf("自适应门限下限应在 -120-0 dBFS 之间，当前 %v", n.GateFloorDBFS)
		}
	}
	return nil
}

// Enabled reports whether any processing happens at all.
func (n NoiseParams) Enabled() bool { return n.withDefaults().Method != NoiseOff }

// defaultSampleRate mirrors Params' documented canonical rate; Validate needs it
// before any Params exists.
const defaultSampleRate = 48000

// ---------------------------------------------------------------------------
// biquad high-pass
// ---------------------------------------------------------------------------

// biquad is a direct-form-I second-order IIR section.
type biquad struct {
	b0, b1, b2 float64
	a1, a2     float64
	x1, x2     float64
	y1, y2     float64
}

// highPassBiquad designs a 2nd-order Butterworth high-pass with the RBJ audio
// EQ cookbook formulas (Q = 1/sqrt(2)).
func highPassBiquad(rate, fc float64) biquad {
	omega := 2 * math.Pi * fc / rate
	cosw, sinw := math.Cos(omega), math.Sin(omega)
	alpha := sinw / (2 * math.Sqrt2)
	a0 := 1 + alpha
	return biquad{
		b0: (1 + cosw) / 2 / a0,
		b1: -(1 + cosw) / a0,
		b2: (1 + cosw) / 2 / a0,
		a1: -2 * cosw / a0,
		a2: (1 - alpha) / a0,
	}
}

// filterTo appends the filtered copy of v to dst, leaving v untouched. The
// per-sample arithmetic is identical to process, so a filtered stream does not
// depend on how the caller chunked it.
func (b *biquad) filterTo(dst, v []float32) []float32 {
	b0, b1, b2, a1, a2 := b.b0, b.b1, b.b2, b.a1, b.a2
	x1, x2, y1, y2 := b.x1, b.x2, b.y1, b.y2
	for _, x32 := range v {
		x := float64(x32)
		y := b0*x + b1*x1 + b2*x2 - a1*y1 - a2*y2
		x2, x1 = x1, x
		y2, y1 = y1, y
		dst = append(dst, float32(y))
	}
	b.x1, b.x2, b.y1, b.y2 = x1, x2, y1, y2
	return dst
}

// reset clears the filter state.
func (b *biquad) reset() { b.x1, b.x2, b.y1, b.y2 = 0, 0, 0, 0 }

// ---------------------------------------------------------------------------
// streaming reducer
// ---------------------------------------------------------------------------

// levelBlockSamples is the time-domain window of the noise-floor tracker
// (20 ms at 48 kHz), i.e. the same length as a live audio block.
const (
	levelBlockSamples = 960
	// floorRisePerBlock is the one-pole coefficient of the floor tracker's
	// release: at one update per 20 ms it is a ~2.4 s time constant, slow
	// enough that a game's music bed does not drag the gate up.
	floorRisePerBlock = 0.0083
	// peakDecayPerBlock is the one-pole release of the tracked peak level, about
	// 6 dB/s: fast enough to follow a volume change, slow enough to survive the
	// gaps between sound effects.
	peakDecayPerBlock = 0.0014
	// gateMinRangeDB is how much dynamic range the tracker must have seen before
	// the adaptive gate is allowed to open above the configured one for LOUD
	// continuous material.
	//
	// It exists because the floor tracker cannot tell "quiet between sounds" from
	// "a continuous sound": for a steady drone, music bed or an engine looping
	// forever, the quietest recent 20 ms IS the sound itself, and an unguarded
	// adaptive gate would set its threshold right on top of the thing it is
	// supposed to detect - the sound would never be scored at all. Requiring the
	// peak to stand clear of the floor means the gate only engages when there
	// really is silence between events — except for quiet continuous room tone
	// (see gateQuietCeilingDBFS), which must still raise the gate so ambience
	// alone cannot flood the matcher.
	gateMinRangeDB = 12
	// gateQuietCeilingDBFS: when dynamic range is missing, still raise the
	// adaptive gate if the tracked floor is at or below this level. Measured
	// game room-tone sits around -50..-55 dBFS; continuous content (BGM, engine)
	// sits well above -42 and keeps the configured gate instead.
	gateQuietCeilingDBFS = -42
)

// noiseReducer is the streaming state of the front-end. Like Analyzer it is
// driven from a single goroutine.
type noiseReducer struct {
	p     NoiseParams
	hp    biquad
	hpOff bool

	plan *fft // the analyzer's FFT plan (FrameSize points)
	hop  int  // analyzer hop; windows are aligned with analysis frames

	// out is exactly the filtered audio of the current Push call, i.e. what the
	// analyzer analyses, indexed like the input the caller passed in.
	out []float32

	// histBuf is the estimator's own rolling history of the filtered stream:
	// histBuf[i] is the sample at absolute position histBase+i. It only has to
	// cover the windows that have not been built yet plus one window of overlap,
	// and it is trimmed when a window is consumed.
	histBuf  []float32
	rawBuf   []float32 // original samples, for the 10 ms transient detector
	histBase int       // absolute position of histBuf[0]
	histEnd  int       // absolute position one past histBuf[len-1]
	// winStart is the absolute start position of the next estimator window to
	// build. Windows are hop-aligned, so it is always a multiple of hop.
	winStart int

	// track is the per-bin smoothed power; noise is the per-bin floor. totalNoise
	// is the sum of noise (0 means "nothing measured yet, do not touch the
	// spectrum").
	track      []float64
	noise      []float64
	gainMem    []float64
	totalNoise float64
	noiseReady bool
	initCount  int
	initNeed   int
	broadRun   int
	broadNeed  int

	// scratch
	binPow []float64
	win    []float32
	block  []float32

	// short-transient detector: ~10 ms energy ending at the current frame.
	// It is evaluated on the same window apply() will see, so a click that
	// starts inside this frame is not delayed until a later 10 ms hop ends.
	energyWin    int
	energyFloor  float64
	energyRuns   int
	energyPrimed bool
	protectFrame bool
	lastEnergy   float64
	// lastCutDB is how many dB the latest analysis frame's broadband power was
	// reduced by the Wiener gain. Zero when the frame was passed through
	// (tracker not ready, or a short click that skips the gain).
	lastCutDB float64

	// --- time-domain noise-floor tracker (dBFS-comparable) ---
	frames int
	floor  float64 // linear power; 0 means "not measured yet"
	// peak is the loudest recent block power (slow release). floor/peak is
	// meaningless until both have seen some audio.
	peak      float64
	floorInit bool
	// floorCarry accumulates the partial 20 ms block between two Push calls, so
	// the tracker's block boundaries depend on the sample position only and not
	// on how the caller happened to chunk its audio (the streaming/batch
	// identity the analyzer guarantees must survive the front-end).
	floorCarry []float32
	// peakLevel / peakSet track the loudest raw frame level seen so far, so the
	// frame gate can never silence a whole window (see frameGate).
	peakLevel float64
	peakSet   bool
}

// newNoiseReducer builds the front-end. plan is the analyzer's FFT plan; the
// reducer reuses it (same goroutine, never nested).
func newNoiseReducer(p NoiseParams, rate, frameSize, hopSize int, plan *fft) *noiseReducer {
	p = p.withDefaults()
	hop := hopSize
	if hop <= 0 || hop > plan.n {
		hop = plan.n / 4
		if hop < 1 {
			hop = 1
		}
	}
	broadNeed := int(math.Round(noiseSceneSeconds / (float64(hop) / float64(rate))))
	if broadNeed < 4 {
		broadNeed = 4
	}
	energyWin := int(math.Round(transientWinSec * float64(rate)))
	if energyWin < 32 {
		energyWin = 32
	}
	_ = frameSize
	r := &noiseReducer{
		p:         p,
		plan:      plan,
		hop:       hop,
		track:     make([]float64, plan.n/2+1),
		noise:     make([]float64, plan.n/2+1),
		gainMem:   make([]float64, plan.n/2+1),
		binPow:    make([]float64, plan.n/2+1),
		win:       make([]float32, plan.n),
		block:     make([]float32, plan.n),
		initNeed:  noiseInitFrames,
		broadNeed: broadNeed,
		energyWin: energyWin,
		// out grows to at most one Push worth plus the estimator's two-window
		// lookahead, and is compacted at the end of every Push.
		out: make([]float32, 0, 4*plan.n),
		// levelBlockSamples is not a multiple of every possible chunk size, so
		// the tracker keeps its own carry and never looks at a partial block.
		floorCarry: make([]float32, 0, levelBlockSamples),
	}
	hannPeriodic(r.win)
	if p.Method != NoiseOff && p.HighPassHz > 0 && p.HighPassHz < float64(rate)/2 {
		r.hp = highPassBiquad(float64(rate), p.HighPassHz)
	} else {
		r.hpOff = true
	}
	return r
}

// Push filters one block of samples into the per-call output buffer and appends
// them to the estimator's history.
//
// v is NEVER modified: the filtered samples are written to the buffer returned by
// Out, which the analyzer consumes in place before calling Push again. Mutating
// the caller's audio would be a trap: the same buffer is handed to more than one
// analyzer in tests and in the offline match path, and filtering it twice would
// silently change the audio.
//
// Push itself does NOT advance the estimator; the analyzer calls AdvanceTo for
// every frame it analyses (see there for why that matters).
func (r *noiseReducer) Push(v []float32) {
	if r == nil {
		return
	}
	out := r.out[:0]
	if r.hpOff {
		out = append(out, v...)
	} else {
		out = r.hp.filterTo(out, v)
	}
	r.out = out

	r.trackFloor(r.out)
	if !r.spectral() {
		return
	}
	r.histBuf = append(r.histBuf, r.out...)
	r.rawBuf = append(r.rawBuf, v...)
	r.histEnd += len(r.out)
}

// AdvanceTo brings the noise estimator up to sample position pos, counted in
// samples received since the last Reset. The analyzer calls it once per frame,
// right before that frame is analysed.
//
// Windows are hop-aligned with the analysis frames and end at pos, so the last
// window built is the frame about to be scored. Event bins in that frame are
// not written into the noise floor (see trackBlock); the Wiener gain then uses
// the floor as it was before the event. Advancing once per Push instead would
// make the result depend on how the caller chunked its audio.
func (r *noiseReducer) AdvanceTo(pos int) {
	if r == nil || !r.spectral() {
		return
	}
	frame := r.plan.n
	hop := r.hop
	for r.winStart+frame <= pos {
		if r.winStart < r.histBase {
			// The samples were already trimmed away (the analyzer skipped
			// ahead); skip this window instead of building it from wrong audio.
			r.winStart += hop
			continue
		}
		off := r.winStart - r.histBase
		if off < 0 || off+frame > len(r.histBuf) {
			r.winStart += hop
			continue
		}
		for i := 0; i < frame; i++ {
			r.block[i] = r.histBuf[off+i] * r.win[i]
		}
		r.trackBlock()
		r.protectFrame = r.feedFrameEnergy(r.winStart, r.winStart+frame)
		r.winStart += hop
	}
	r.trimHistory()
}

// trimHistory drops the samples the estimator can no longer need: everything
// before the next STFT window.
func (r *noiseReducer) trimHistory() {
	keepFrom := r.winStart - r.histBase
	if keepFrom <= 0 {
		return
	}
	if keepFrom > len(r.histBuf) {
		keepFrom = len(r.histBuf)
	}
	if keepFrom > len(r.rawBuf) {
		keepFrom = len(r.rawBuf)
	}
	n := copy(r.histBuf, r.histBuf[keepFrom:])
	r.histBuf = r.histBuf[:n]
	n = copy(r.rawBuf, r.rawBuf[keepFrom:])
	r.rawBuf = r.rawBuf[:n]
	r.histBase += keepFrom
}

// frameGate decides whether a frame at the given raw level (dBFS) carries any
// information about a sound, i.e. whether it is meaningfully above the tracked
// noise floor.
//
// Two guards keep it from throwing the sound away:
//
//   - FrameGateDB is deliberately small (3.5 dB by default). A game sound is
//     often only a few dB above the ambience in raw power even when it is
//     clearly audible, because the ambience is broadband while the sound's
//     energy is concentrated in a few bands.
//   - The loudest frame seen so far is NEVER gated. Without that, a window in
//     which the ambience is as strong as the sound collapses to the zero vector
//     and can never match anything - a catastrophic failure mode for a filter
//     whose whole purpose is to help in noisy environments.
func (r *noiseReducer) frameGate(levelDBFS float64) bool {
	if r == nil || !r.spectral() || r.p.FrameGateDB <= 0 || !r.floorInit || r.floor <= 0 {
		return false
	}
	if levelDBFS >= r.NoiseFloorDBFS()+r.p.FrameGateDB {
		return false
	}
	// The frame is below the gate. Keep it if it is the loudest one seen so far
	// (the sound's own peak), so a window can never end up entirely gated.
	if !r.peakSet || levelDBFS >= r.peakLevel {
		r.peakSet = true
		r.peakLevel = levelDBFS
		return false
	}
	return true
}

// Out returns the filtered audio of the most recent Push call. It is valid until
// the next Push.
func (r *noiseReducer) Out() []float32 {
	if r == nil {
		return nil
	}
	return r.out
}

// spectral reports whether the STFT half of the front-end is active.
func (r *noiseReducer) spectral() bool {
	return r != nil && r.p.Method == NoiseSpectral &&
		r.p.OverSubtract > 0 && r.p.Mix > 0 && r.plan != nil
}

// trackFloor maintains the time-domain noise floor: the minimum 20 ms block RMS
// seen recently, rising slowly. That is the number the adaptive gate uses, and
// it is directly comparable with Analyzer.LevelDBFS. Its block boundaries are a
// pure function of the sample position, so chunked and whole-buffer pushes see
// exactly the same blocks.
func (r *noiseReducer) trackFloor(v []float32) {
	r.floorCarry = append(r.floorCarry, v...)
	for len(r.floorCarry) >= levelBlockSamples {
		block := r.floorCarry[:levelBlockSamples]
		var sq float64
		for _, x := range block {
			sq += float64(x) * float64(x)
		}
		rms := math.Sqrt(sq / float64(levelBlockSamples))
		r.feedFloor(rms * rms)
		n := copy(r.floorCarry, r.floorCarry[levelBlockSamples:])
		r.floorCarry = r.floorCarry[:n]
	}
}

// feedFloor folds one 20 ms block power into the floor tracker: instantaneous
// attack (a quieter moment is evidence) and a slow release, so a game's music
// bed cannot drag the gate up with it. The same block feeds the peak tracker,
// whose ratio to the floor is what tells a real noise floor apart from a
// continuous sound (see gateMinRangeDB).
func (r *noiseReducer) feedFloor(p float64) {
	if !r.floorInit {
		r.floor = p
		r.peak = p
		r.floorInit = true
		r.frames = 1
		return
	}
	r.frames++
	if p < r.floor {
		r.floor = p
	} else {
		r.floor += floorRisePerBlock * (p - r.floor)
	}
	if p > r.peak {
		r.peak = p
	} else {
		r.peak += peakDecayPerBlock * (p - r.peak)
	}
}

// trackBlock folds one analysis-aligned window into the event-aware noise floor.
//
// A bin whose smoothed power is well above the floor is a tone or a short
// event: it is left out of the update so a beep is not learned as ambience. A
// rise that lights up more than a third of the bins for ~200 ms is a scene
// change and is mixed into the floor. Everything else creeps toward the
// smoothed power.
func (r *noiseReducer) trackBlock() {
	r.plan.spectrum(r.binPow, r.block)
	bins := r.plan.n/2 + 1
	hot := 0
	for b := 0; b < bins; b++ {
		p := r.binPow[b]
		r.track[b] = (1-noiseTrackFast)*r.track[b] + noiseTrackFast*p
		if r.noiseReady && r.track[b] > r.noise[b]*noiseHotRatio {
			hot++
		}
	}

	if !r.noiseReady {
		for b := 0; b < bins; b++ {
			r.noise[b] += r.binPow[b]
		}
		r.initCount++
		if r.initCount >= r.initNeed {
			scale := 1 / float64(r.initNeed)
			var total float64
			for b := 0; b < bins; b++ {
				r.noise[b] *= scale
				total += r.noise[b]
			}
			r.totalNoise = total
			r.noiseReady = true
		}
		return
	}

	if hot > bins/noiseBroadFrac {
		r.broadRun++
	} else {
		r.broadRun = 0
	}
	broad := r.broadRun >= r.broadNeed

	var total float64
	for b := 0; b < bins; b++ {
		switch {
		case broad:
			r.noise[b] = (1-noiseSceneMix)*r.noise[b] + noiseSceneMix*r.track[b]
		case r.track[b] > r.noise[b]*noiseEventRatio:
			// Narrow or short event: do not teach it to the noise floor.
		default:
			r.noise[b] = (1-noiseTrackSlow)*r.noise[b] + noiseTrackSlow*r.track[b]
		}
		total += r.noise[b]
	}
	r.totalNoise = total
}

// feedFrameEnergy looks at ~10 ms of the ORIGINAL audio ending at the current
// analysis frame (the listening demo's paste-back also used the raw waveform;
// the high-pass would otherwise strip the click's envelope and hide it).
func (r *noiseReducer) feedFrameEnergy(start, end int) bool {
	if end <= start {
		return false
	}
	win := r.energyWin
	if win > end-start {
		win = end - start
	}
	seg := end - win
	if seg < r.histBase {
		return false
	}
	off := seg - r.histBase
	if off < 0 || off+win > len(r.rawBuf) {
		return false
	}
	var e float64
	for i := 0; i < win; i++ {
		x := float64(r.rawBuf[off+i])
		e += x * x
	}
	e /= float64(win)
	r.lastEnergy = e
	return r.feedEnergy(e)
}

// feedEnergy updates the time-domain energy floor and reports whether this hop
// is a short transient that should skip Wiener gain.
func (r *noiseReducer) feedEnergy(e float64) bool {
	if !r.energyPrimed {
		r.energyFloor = e
		r.energyPrimed = true
		return false
	}
	if r.energyFloor > 0 && e > r.energyFloor*transientRatio {
		r.energyRuns++
		if r.energyRuns <= transientMaxHops {
			return true
		}
		r.energyFloor = (1-transientFloorFast)*r.energyFloor + transientFloorFast*e
		return false
	}
	r.energyRuns = 0
	r.energyFloor = (1-transientFloorSlow)*r.energyFloor + transientFloorSlow*e
	return false
}

// apply attenuates one frame's one-sided power spectrum by a per-bin Wiener
// gain derived from the tracked noise estimate, in place. It is called by
// Analyzer.emitFrameAt right after the FFT and before the mel bank.
//
// Why a gain and not a subtraction: the patch is normalised, so only the SHAPE
// of the spectrum matters. Subtracting a constant noise power from every bin
// changes the shape in the wrong direction - it takes more energy out of the
// loud signal bins than out of the quiet ones (relative to their size), flattening
// the very contrast that identifies the sound. A gain, on the other hand,
// multiplies each bin by its own signal-to-noise ratio, which is the maximum
// likelihood estimate of the clean spectrum under a Gaussian noise model. It
// leaves high-SNR bins (the signal) almost untouched and suppresses low-SNR bins
// (the ambience) - exactly the contrast that the normalised cosine rewards.
//
//	G[b] = max( S/(S+N), gainFloor )   with S = P (the observed power) and
//	                                          N = OverSubtract * N_est
//
// S is the observed power itself rather than P - N on purpose: P is the maximum
// likelihood estimate of the clean power in the Wiener sense, and using it keeps
// the gain monotone in the local SNR (loud bins are barely touched, hopeless bins
// are pushed to the floor). The floor is a gain, not a fraction of the power, so
// it still lets a hopeless bin fall far below the signal bins - which is what
// removes the noise's spectral shape from the normalised patch.
//
// Gain rises immediately (keeps onsets) and falls with a one-pole release, the
// same attack/release the listening demo used. A short transient skips the gain
// entirely so a click is not erased.
func (r *noiseReducer) apply(pow []float64) {
	if !r.spectral() || !r.noiseReady || r.totalNoise <= 0 {
		r.lastCutDB = 0
		return
	}
	if r.protectFrame {
		r.lastCutDB = 0
		return
	}
	alpha, mix := r.p.OverSubtract, r.p.Mix
	gainFloor := math.Pow(10, r.p.GainFloorDB/20)
	bins := len(pow)
	if bins > len(r.noise) {
		bins = len(r.noise)
	}
	var pre, post float64
	for b := 0; b < bins; b++ {
		p := pow[b]
		if p <= 0 {
			continue
		}
		noise := alpha * r.noise[b]
		g := p / (p + noise)
		if g < gainFloor {
			g = gainFloor
		}
		if mix < 1 {
			g = mix*g + (1 - mix)
		}
		if g > r.gainMem[b] {
			r.gainMem[b] = g
		} else {
			r.gainMem[b] = gainReleasePrev*r.gainMem[b] + (1-gainReleasePrev)*g
		}
		applied := r.gainMem[b]
		pow[b] = p * applied
		pre += p
		post += p * applied
	}
	if pre > 0 && post > 0 && post < pre {
		r.lastCutDB = 10 * math.Log10(pre/post)
	} else {
		r.lastCutDB = 0
	}
}

// ReductionDB reports how many dB the latest analysed frame was attenuated
// across the whole spectrum. It is a meter reading, not part of the fingerprint.
func (r *noiseReducer) ReductionDB() float64 {
	if r == nil || r.lastCutDB < 0 {
		return 0
	}
	return r.lastCutDB
}

// reset clears the streaming state.
func (r *noiseReducer) reset() {
	if r == nil {
		return
	}
	r.hp.reset()
	r.histBuf = r.histBuf[:0]
	r.rawBuf = r.rawBuf[:0]
	r.histBase = 0
	r.histEnd = 0
	r.winStart = 0
	r.out = r.out[:0]
	for i := range r.track {
		r.track[i] = 0
	}
	for i := range r.noise {
		r.noise[i] = 0
	}
	for i := range r.gainMem {
		r.gainMem[i] = 0
	}
	r.totalNoise = 0
	r.noiseReady = false
	r.initCount = 0
	r.broadRun = 0
	r.energyFloor = 0
	r.energyRuns = 0
	r.energyPrimed = false
	r.protectFrame = false
	r.lastEnergy = 0
	r.lastCutDB = 0
	r.frames = 0
	r.floor = 0
	r.peak = 0
	r.floorInit = false
	r.floorCarry = r.floorCarry[:0]
	r.peakLevel = 0
	r.peakSet = false
}

// NoiseFloorDBFS returns the tracked noise floor in dBFS, or -Inf before the
// first measurement.
func (r *noiseReducer) NoiseFloorDBFS() float64 {
	if r == nil || !r.floorInit || r.floor <= 0 {
		return math.Inf(-1)
	}
	return 10 * math.Log10(r.floor)
}

// gateDBFS is the adaptive silence gate for a configured absolute gate. It only
// ever RAISES the gate (a noise floor below the configured silence gate leaves
// the configuration alone), it is clamped to GateFloorDBFS, and it only engages
// when either (a) the audio has shown enough dynamic range to prove that the
// tracked floor is really silence rather than a continuous loud sound, or
// (b) the floor itself is quiet enough to be room tone (see gateQuietCeilingDBFS).
func (r *noiseReducer) gateDBFS(configured float64) float64 {
	if r == nil || !r.p.AdaptiveGate || !r.floorInit || r.floor <= 0 || r.peak <= 0 {
		return configured
	}
	floorDB := r.NoiseFloorDBFS()
	if 10*math.Log10(r.peak/r.floor) < gateMinRangeDB {
		// No clear silence between events. Quiet continuous ambience still
		// raises the gate; a continuous loud bed must not.
		if floorDB > gateQuietCeilingDBFS {
			return configured
		}
	}
	g := floorDB + r.p.GateMarginDB
	if g < r.p.GateFloorDBFS {
		g = r.p.GateFloorDBFS
	}
	if g <= configured {
		return configured
	}
	return g
}
