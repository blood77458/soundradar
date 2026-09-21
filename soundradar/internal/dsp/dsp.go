// Package dsp computes the soundradar P2 acoustic fingerprint.
//
// # Feature pipeline ("mel-goertzel-v2", FFT variant)
//
// Input is always 48 kHz / mono / float32 in [-1, 1] (the canonical rate of
// internal/library, so no resampling happens anywhere on the recognition path).
//
//  1. frame        FrameSize = 1024 samples  (21.333 ms)
//     HopSize   = 256 samples   (5.333 ms, i.e. 75 % overlap)
//  2. de-mean      subtract the frame mean (kills DC offset / rumble at 0 Hz)
//  3. window       periodic Hann: w[n] = 0.5 - 0.5*cos(2*pi*n/N)
//  4. FFT          1024-point radix-2 (iterative, precomputed twiddles)
//  5. power        |X[k]|^2 for k = 0..N/2, doubled for k = 1..N/2-1 so the
//     one-sided spectrum preserves the signal's total energy
//     (Parseval). Bin spacing is 48000/1024 = 46.875 Hz.
//  6. mel bank     64 triangular filters, FMin 40 Hz .. FMax 16000 Hz,
//     mel(f) = 2595*log10(1 + f/700), each triangle normalised to
//     unit area (divide by its bandwidth in Hz) so a broadband
//     signal yields a flat log-mel floor instead of a rising one.
//  7. log-mel      features = 10*log10(max(E, 1e-12)), an absolute log so a
//     frame keeps its real level. The -20 dB floor is applied later,
//     to the whole patch, relative to that patch's loudest band.
//     Doing it per frame stretches a quiet frame up to the same
//     contrast as the click beside it. A 3 s recall whose real sound
//     is ~20 ms then becomes a template of the room tone, and that
//     room tone matches other recalls. A window-level floor keeps a
//     quiet click's shape (the floor follows the window peak, not an
//     absolute -20 dBFS) and leaves the surrounding quiet frames flat.
//  8. patch        WindowFrames = 32 consecutive frames => 64*32 = 2048 values.
//     The patch spans FrameSize + 31*HopSize = 8960 samples =
//     186.7 ms of raw audio.
//  9. normalise    per patch: subtract the patch mean, then L2-normalise to a
//     unit vector. Cosine similarity therefore reduces to a dot
//     product in [-1, 1]; the de-mean removes any constant offset
//     shared by all 2048 values (a pure loudness change is a
//     constant offset in the log domain, because log(a*x) =
//     log(a) + log(x)).
//
// Everything is pure Go: the FFT, the mel filter bank and the analysis use only
// the standard library, and CGO is never required.
package dsp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// Algorithm / version identity of this fingerprint implementation.
const (
	// Algorithm names the feature family; it is stored in index files and must
	// change whenever the pipeline changes shape.
	//
	// v2 added the environment-noise front-end (noise.go), which changes every
	// produced vector, so it is a NEW algorithm name rather than a new version:
	// an index built by v1 is refused with a readable message instead of being
	// searched with mismatched vectors. v3 (this Version) replaced the
	// minimum-statistics estimator with event-aware tracking; same algorithm
	// family, different numbers, so LoadOrBuild rebuilds.
	Algorithm = "mel-goertzel-v2"
	// Version is the fingerprint implementation version, bumped on any change
	// that alters the produced numbers.
	Version = 3
	// Norm describes the patch post-processing; it is stored in the index so a
	// mismatch is detected as clearly as a parameter mismatch.
	Norm = "mean-subtract+l2"

	// LogFloorDB / LogFloor are the log-mel floor DEPTH, relative to the loudest
	// mel band of the same frame: a band quieter than that sits at
	// 10*log10(peak * LogFloor) = peak_dB - 20.
	//
	// It must not be an absolute -20 dBFS clamp. Game loopback clips often peak
	// around -30 dBFS; every band then clamps to the same constant, step 9
	// subtracts that constant, and the stored template is the zero vector. A
	// zero template scores 0 against every window, including itself, so the
	// item can never hit. Measuring the floor from the frame's own peak keeps
	// the shape (and therefore the cosine) stable when the player turns the
	// volume down. The -20 dB depth is still what stops empty bands from
	// dominating the normalised patch; both the depth and the "relative-peak"
	// mode are stored in the fingerprint.
	LogFloorDB = -20.0
	// LogFloor is the relative floor as a linear ratio (10^(-20/10) = 0.01):
	// a band is clamped when it is more than 20 dB below the frame's loudest band.
	LogFloor = 0.01
	// absLogFloor is only there so a digitally silent frame does not call log(0).
	// It is far below any real signal, so it never replaces the relative floor.
	absLogFloor = 1e-12

	// LevelWindowSamples is the length of the RMS window used by LevelDBFS
	// (~50 ms at 48 kHz, matching the "silence gate" requirement).
	LevelWindowSamples = 2400
)

// logFloor is the effective floor. SetLogFloor lets tests measure how sensitive
// the feature is to it; production code keeps the LogFloor default.
var logFloor = float64(LogFloor)

// SetLogFloor overrides the log-mel energy floor.
//
// The floor is the single most influential choice in the pipeline: the silent
// mel bands of a pure tone all sit AT the floor, so the floor decides how much
// of the normalised patch's energy those ~2000 empty bands carry, and therefore
// how fast the cosine score degrades once broadband noise lifts them off it.
// The exported knob exists so tests can quantify that; production code must not
// touch it (the value is part of the fingerprint document).
func SetLogFloor(v float64) {
	if v > 0 {
		logFloor = v
	}
}

// LogFloorLinear returns the effective (possibly test-overridden) floor ratio.
func LogFloorLinear() float64 { return logFloor }

// logMel converts raw mel-band energies into absolute log values. The -20 dB
// floor is not applied here: a frame does not know whether it is the click or
// the quiet padding around it. applyWindowFloor does that once the patch is
// assembled. absLogFloor only keeps a digitally silent band out of log(0).
func logMel(raw []float64) []float32 {
	out := make([]float32, len(raw))
	for i, e := range raw {
		if e < absLogFloor {
			e = absLogFloor
		}
		out[i] = float32(10 * math.Log10(e))
	}
	return out
}

// applyWindowFloor clamps every band to logFloor below the loudest band in the
// patch. Quiet frames, whose own peak would otherwise be stretched to a full
// 20 dB of spectral shape, become a flat floor and stop dominating the
// normalised vector. A uniformly quiet patch is unchanged in shape: its floor
// sits 20 dB below its own peak, which is the same relative floor a single
// frame used to get.
func applyWindowFloor(v []float32) {
	if len(v) == 0 {
		return
	}
	maxV := float32(math.Inf(-1))
	for _, x := range v {
		if x > maxV {
			maxV = x
		}
	}
	if math.IsInf(float64(maxV), 0) {
		return
	}
	floor := maxV + float32(10*math.Log10(logFloor))
	for i, x := range v {
		if x < floor {
			v[i] = floor
		}
	}
}

// Params is the complete, self-describing fingerprint configuration.
type Params struct {
	SampleRate   int // 48000
	FrameSize    int // 1024
	HopSize      int // 256
	MelBands     int // 64
	FMinHz       int // 40
	FMaxHz       int // 16000
	WindowFrames int // 32

	// Noise is the environment-noise front-end (noise.go). Its zero value means
	// "the recommended defaults", so a hand-built Params from before this
	// feature still describes the current pipeline. Params is a value type:
	// NormalizeNoise copies a Params with an explicitly filled-in Noise field.
	Noise NoiseParams
}

// DefaultParams returns the parameters mandated by the P2 specification, with
// the recommended environment-noise settings.
func DefaultParams() Params {
	return Params{
		SampleRate:   48000,
		FrameSize:    1024,
		HopSize:      256,
		MelBands:     64,
		FMinHz:       40,
		FMaxHz:       16000,
		WindowFrames: 32,
		Noise:        DefaultNoiseParams(),
	}
}

// WithNoise returns a copy of p with n installed (defaults filled in). It is how
// the application config reaches the fingerprint without every caller having to
// remember which fields exist.
func (p Params) WithNoise(n NoiseParams) Params {
	p.Noise = n
	return p
}

// EffectiveNoise returns the noise parameters with defaults filled in.
func (p Params) EffectiveNoise() NoiseParams { return p.Noise.withDefaults() }

// Dim is the fingerprint vector length: MelBands values per frame times
// WindowFrames frames in one patch.
func (p Params) Dim() int { return p.MelBands * p.WindowFrames }

// Validate checks that the parameters describe a workable pipeline.
func (p Params) Validate() error {
	switch {
	case p.SampleRate <= 0:
		return fmt.Errorf("采样率必须为正数，当前 %d", p.SampleRate)
	case p.FrameSize < 16:
		return fmt.Errorf("帧长太小: %d（至少 16）", p.FrameSize)
	case p.FrameSize&(p.FrameSize-1) != 0:
		return fmt.Errorf("帧长必须是 2 的幂（radix-2 FFT 要求），当前 %d", p.FrameSize)
	case p.HopSize <= 0:
		return fmt.Errorf("跳步必须为正数，当前 %d", p.HopSize)
	case p.HopSize > p.FrameSize:
		return fmt.Errorf("跳步 %d 不能大于帧长 %d", p.HopSize, p.FrameSize)
	case p.MelBands < 2:
		return fmt.Errorf("Mel 滤波器数量太少: %d", p.MelBands)
	case p.MelBands > p.FrameSize/2:
		return fmt.Errorf("Mel 滤波器数量 %d 超过可用频点数 %d", p.MelBands, p.FrameSize/2)
	case p.FMinHz < 0:
		return fmt.Errorf("最低频率不能为负: %d", p.FMinHz)
	case p.FMaxHz <= p.FMinHz:
		return fmt.Errorf("最高频率 %d 必须大于最低频率 %d", p.FMaxHz, p.FMinHz)
	case p.FMaxHz > p.SampleRate/2:
		return fmt.Errorf("最高频率 %d 超过奈奎斯特频率 %d", p.FMaxHz, p.SampleRate/2)
	case p.WindowFrames < 1:
		return fmt.Errorf("窗口帧数必须为正数，当前 %d", p.WindowFrames)
	}
	return p.Noise.Validate()
}

// fpDoc is the canonical JSON document hashed by Fingerprint. Field order is
// fixed by the struct, floats are rounded and no maps are involved, so the same
// parameters always hash to the same string - and different ones do not.
type fpDoc struct {
	Algorithm    string  `json:"algorithm"`
	Version      int     `json:"version"`
	SampleRate   int     `json:"sampleRate"`
	FrameSize    int     `json:"frameSize"`
	HopSize      int     `json:"hopSize"`
	Window       string  `json:"window"`
	MelBands     int     `json:"melBands"`
	FMinHz       float64 `json:"fMinHz"`
	FMaxHz       float64 `json:"fMaxHz"`
	WindowFrames int     `json:"windowFrames"`
	Norm         string  `json:"norm"`
	LogFloorDB   float64 `json:"logFloorDb"`
	// FloorMode distinguishes the window-level floor from the older per-frame
	// relative floor and from the original absolute -20 dBFS clamp. Changing
	// it changes the stored vectors, so LoadOrBuild rebuilds the index.
	FloorMode string `json:"floorMode"`
	// Noise is the environment-noise front-end (noise.go). It is part of the
	// document because it changes every produced vector: an index built with
	// different noise settings must be rebuilt, and hashing this block is what
	// detects that.
	Noise noiseDoc `json:"noise"`
}

// noiseDoc is the canonical, JSON-safe form of NoiseParams: every field is a
// scalar and the method is already normalised by withDefaults, so the same
// settings always produce the same bytes (and therefore the same fingerprint).
type noiseDoc struct {
	Method        string  `json:"method"`
	HighPassHz    float64 `json:"highPassHz"`
	OverSubtract  float64 `json:"overSubtract"`
	GainFloorDB   float64 `json:"gainFloorDb"`
	Mix           float64 `json:"mix"`
	AdaptiveGate  bool    `json:"adaptiveGate"`
	GateMarginDB  float64 `json:"gateMarginDb"`
	GateFloorDBFS float64 `json:"gateFloorDbfs"`
}

// noiseParamsDoc rounds the noise settings for the fingerprint document.
func noiseParamsDoc(n NoiseParams) noiseDoc {
	n = n.withDefaults()
	return noiseDoc{
		Method:        n.Method,
		HighPassHz:    math.Round(n.HighPassHz*1000) / 1000,
		OverSubtract:  math.Round(n.OverSubtract*1000) / 1000,
		GainFloorDB:   math.Round(n.GainFloorDB*1000) / 1000,
		Mix:           math.Round(n.Mix*1000) / 1000,
		AdaptiveGate:  n.AdaptiveGate,
		GateMarginDB:  math.Round(n.GateMarginDB*1000) / 1000,
		GateFloorDBFS: math.Round(n.GateFloorDBFS*1000) / 1000,
	}
}

// doc builds the canonical parameter document.
func (p Params) doc() fpDoc {
	return fpDoc{
		Algorithm:    Algorithm,
		Version:      Version,
		SampleRate:   p.SampleRate,
		FrameSize:    p.FrameSize,
		HopSize:      p.HopSize,
		Window:       "hann-periodic",
		MelBands:     p.MelBands,
		FMinHz:       math.Round(float64(p.FMinHz)),
		FMaxHz:       math.Round(float64(p.FMaxHz)),
		WindowFrames: p.WindowFrames,
		Norm:         Norm,
		LogFloorDB:   math.Round(10 * math.Log10(LogFloor)),
		FloorMode:    "window-relative",
		Noise:        noiseParamsDoc(p.Noise),
	}
}

// Fingerprint returns the sha256 of the canonical parameter JSON. It is the
// guard against "rebuilt the library with different parameters but kept the old
// index": index files store it and Load rejects a mismatch.
func (p Params) Fingerprint() string {
	b, err := json.Marshal(p.doc())
	if err != nil {
		// Unreachable: fpDoc has only JSON-safe scalar fields.
		panic("dsp: 无法序列化特征参数: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ParamJSON returns the canonical parameter JSON itself (for reports/debugging
// and for the parameter block of an index file).
func (p Params) ParamJSON() []byte {
	b, err := json.MarshalIndent(p.doc(), "", "  ")
	if err != nil {
		panic("dsp: 无法序列化特征参数: " + err.Error())
	}
	return b
}

// ParamDoc is the canonical parameter document as an opaque JSON value. It is
// what internal/index stores and re-marshals to recompute the fingerprint, so
// the two packages cannot drift: a field added to the document here is carried
// through the index file automatically.
type ParamDoc struct {
	Algorithm    string          `json:"algorithm"`
	Version      int             `json:"version"`
	SampleRate   int             `json:"sampleRate"`
	FrameSize    int             `json:"frameSize"`
	HopSize      int             `json:"hopSize"`
	Window       string          `json:"window"`
	MelBands     int             `json:"melBands"`
	FMinHz       float64         `json:"fMinHz"`
	FMaxHz       float64         `json:"fMaxHz"`
	WindowFrames int             `json:"windowFrames"`
	Norm         string          `json:"norm"`
	LogFloorDB   float64         `json:"logFloorDb"`
	FloorMode    string          `json:"floorMode"`
	Noise        json.RawMessage `json:"noise"`
}

// ParamDocOf builds the canonical document of p (the same bytes Fingerprint
// hashes and ParamJSON renders).
func (p Params) ParamDocOf() ParamDoc {
	return ParamDoc{
		Algorithm:    Algorithm,
		Version:      Version,
		SampleRate:   p.SampleRate,
		FrameSize:    p.FrameSize,
		HopSize:      p.HopSize,
		Window:       "hann-periodic",
		MelBands:     p.MelBands,
		FMinHz:       math.Round(float64(p.FMinHz)),
		FMaxHz:       math.Round(float64(p.FMaxHz)),
		WindowFrames: p.WindowFrames,
		Norm:         Norm,
		LogFloorDB:   math.Round(10 * math.Log10(LogFloor)),
		FloorMode:    "window-relative",
		Noise:        noiseRaw(p.Noise),
	}
}

// FingerprintDoc returns the sha256 of the given document. internal/index uses
// it to recompute the fingerprint of a document read from a file, which is what
// detects "the parameters changed" without duplicating this struct.
func FingerprintDoc(d ParamDoc) string {
	b, err := json.Marshal(d)
	if err != nil {
		panic("dsp: 无法序列化特征参数: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FingerprintOfDocJSON is FingerprintDoc for a document that is still JSON text.
func FingerprintOfDocJSON(doc []byte) (string, error) {
	var d ParamDoc
	if err := json.Unmarshal(doc, &d); err != nil {
		return "", err
	}
	return FingerprintDoc(d), nil
}

// noiseRaw marshals the noise block into the document. A marshal failure is
// impossible for a struct of scalars; it panics rather than silently producing a
// fingerprint that does not cover the noise settings.
func noiseRaw(n NoiseParams) json.RawMessage {
	b, err := json.Marshal(noiseParamsDoc(n))
	if err != nil {
		panic("dsp: 无法序列化环境音参数: " + err.Error())
	}
	return b
}

// HopDurationS is the time step between two consecutive frames.
func (p Params) HopDurationS() float64 {
	return float64(p.HopSize) / float64(p.SampleRate)
}

// FrameDurationS is the raw audio length of one frame.
func (p Params) FrameDurationS() float64 {
	return float64(p.FrameSize) / float64(p.SampleRate)
}

// PatchDurationS is the raw audio span covered by one patch:
// FrameSize + (WindowFrames-1)*HopSize samples.
func (p Params) PatchDurationS() float64 {
	if p.WindowFrames < 1 {
		return 0
	}
	n := p.FrameSize + (p.WindowFrames-1)*p.HopSize
	return float64(n) / float64(p.SampleRate)
}

// FrameCount returns how many full frames of FrameSize can be taken from n
// samples with the given hop. It is the single source of truth shared by the
// batch path (Frames) and the streaming path (Analyzer), which is what makes
// them bit-identical.
func (p Params) FrameCount(n int) int {
	if n < p.FrameSize {
		return 0
	}
	return 1 + (n-p.FrameSize)/p.HopSize
}

// Frames converts a whole PCM buffer into the log-mel frame sequence used for
// building the index and for offline (sliding-window) analysis. Frames are in
// chronological order, each one is MelBands long and NOT normalised.
func (p Params) Frames(pcm []float32) [][]float32 {
	if err := p.Validate(); err != nil {
		return nil
	}
	a, err := NewAnalyzer(p)
	if err != nil {
		return nil
	}
	a.Push(pcm)
	// flush() analyses the trailing frame that does not start on a multiple of
	// HopSize, which is exactly the extra frame p.FrameCount(len(pcm)) counts.
	a.flush()
	return a.frames
}

// ---------------------------------------------------------------------------
// window + normalisation
// ---------------------------------------------------------------------------

// hannPeriodic fills w with the periodic Hann window used by the STFT. The
// periodic (DFT-even) form is the one whose overlapping copies sum to a
// constant, which is what analysis wants; the symmetric form is for filter
// design.
func hannPeriodic(w []float32) {
	n := len(w)
	if n == 1 {
		w[0] = 1
		return
	}
	for i := range w {
		w[i] = float32(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n)))
	}
}

// Normalize returns a NEW slice holding the window-floored, mean-subtracted,
// L2-normalised copy of patch. Degenerate inputs map to an all-zero vector
// (nil when patch is empty) instead of NaN, so callers can test the result
// for being usable. The floor is relative to the patch peak, so a constant
// gain in the log domain is still removed.
func Normalize(patch []float32) []float32 {
	out := make([]float32, len(patch))
	copy(out, patch)
	return normalizeInPlace(out)
}

// normalizeInPlace mutates v and returns it.
func normalizeInPlace(v []float32) []float32 {
	if len(v) == 0 {
		return nil
	}
	applyWindowFloor(v)
	var sum float64
	for _, x := range v {
		sum += float64(x)
	}
	mean := sum / float64(len(v))

	var sq float64
	for i, x := range v {
		d := float64(x) - mean
		v[i] = float32(d)
		sq += d * d
	}
	norm := math.Sqrt(sq)
	if norm <= 1e-20 {
		for i := range v {
			v[i] = 0
		}
		return v
	}
	inv := float32(1 / norm)
	for i := range v {
		v[i] *= inv
	}
	return v
}

// norm32 returns the L2 norm of v as float64.
func norm32(v []float32) float64 {
	var sq float64
	for _, x := range v {
		sq += float64(x) * float64(x)
	}
	return math.Sqrt(sq)
}

// ---------------------------------------------------------------------------
// mel filter bank
// ---------------------------------------------------------------------------

// hzToMel / melToHz are the standard formulas named by the specification:
// mel = 2595*log10(1 + f/700).
func hzToMel(f float64) float64 { return 2595 * math.Log10(1+f/700) }
func melToHz(m float64) float64 { return 700 * (math.Pow(10, m/2595) - 1) }

// MelBank holds MelBands triangular filters over the one-sided power spectrum.
// Weights is a flat MelBands x (FrameSize/2+1) matrix of float64.
type MelBank struct {
	Bands   int
	Bins    int
	Weights []float64

	// CentersHz[c] is where filter c peaks. Filters are equally spaced on the
	// mel scale, so this slice is strictly increasing.
	CentersHz []float64
}

// Weight returns the filter coefficient (band c, bin b); out-of-range is 0.
func (mb *MelBank) Weight(c, b int) float64 {
	if c < 0 || c >= mb.Bands || b < 0 || b >= mb.Bins {
		return 0
	}
	return mb.Weights[c*mb.Bins+b]
}

// NewMelBank designs the triangular filter bank.
//
// Construction: MelBands+2 points equally spaced on the mel scale from
// mel(FMinHz) to mel(FMaxHz) are mapped back to Hz. Points c..c+2 bound filter
// c, which rises linearly from the left edge to the centre and falls back to
// zero at the right edge. The triangle is divided by its bandwidth in Hz so
// every filter has unit area; without that, high bands (much wider in Hz
// because mel spacing is logarithmic) would collect more energy than low bands
// and the log-mel floor of white noise would slope upwards.
func NewMelBank(p Params) (*MelBank, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	bins := p.FrameSize/2 + 1
	mb := &MelBank{
		Bands:     p.MelBands,
		Bins:      bins,
		Weights:   make([]float64, p.MelBands*bins),
		CentersHz: make([]float64, p.MelBands),
	}

	lo, hi := hzToMel(float64(p.FMinHz)), hzToMel(float64(p.FMaxHz))
	edges := make([]float64, p.MelBands+2)
	for i := range edges {
		edges[i] = melToHz(lo + (hi-lo)*float64(i)/float64(p.MelBands+1))
	}
	binHz := float64(p.SampleRate) / float64(p.FrameSize)

	for c := 0; c < p.MelBands; c++ {
		left, center, right := edges[c], edges[c+1], edges[c+2]
		mb.CentersHz[c] = center
		bw := right - left
		if bw <= 0 {
			return nil, fmt.Errorf("第 %d 个 Mel 滤波器带宽非正（%.3f Hz）", c, bw)
		}
		// Only bins inside [left, right] can be non-zero.
		b0 := int(math.Ceil(left / binHz))
		b1 := int(math.Floor(right / binHz))
		if b0 < 0 {
			b0 = 0
		}
		if b1 > bins-1 {
			b1 = bins - 1
		}
		for b := b0; b <= b1; b++ {
			f := float64(b) * binHz
			var w float64
			if f <= center {
				w = (f - left) / (center - left)
			} else {
				w = (right - f) / (right - center)
			}
			if w < 0 {
				w = 0
			}
			mb.Weights[c*bins+b] = w / bw
		}
	}
	return mb, nil
}

// ---------------------------------------------------------------------------
// FFT
// ---------------------------------------------------------------------------

// fft is an iterative radix-2 decimation-in-time Cooley-Tukey FFT.
//
// Layout choices:
//   - tw holds the N/2 unit-magnitude roots e^(-2*pi*i*k/N), precomputed once
//     (one Analyzer reuses its plan for every frame, so this cost is paid once
//     per analyzer rather than once per frame);
//   - rev holds the bit-reversal permutation, also precomputed, so reordering
//     is a straight loop instead of per-sample bit twiddling.
//
// The forward transform is unnormalised (X[k] = sum x[n] e^(-iwn)) and the
// inverse divides by N, which is how Parseval's identity is usually stated.
type fft struct {
	n    int
	rev  []int
	tw   []complex128
	work []complex128
}

// newFFT builds an FFT plan for size n (n must be a power of two).
func newFFT(n int) *fft {
	if n < 2 || n&(n-1) != 0 {
		panic(fmt.Sprintf("dsp: FFT 长度必须是 2 的幂，得到 %d", n))
	}
	f := &fft{n: n, rev: make([]int, n), tw: make([]complex128, n/2), work: make([]complex128, n)}

	bits := 0
	for 1<<bits < n {
		bits++
	}
	for i := 0; i < n; i++ {
		r := 0
		for b := 0; b < bits; b++ {
			if i&(1<<b) != 0 {
				r |= 1 << (bits - 1 - b)
			}
		}
		f.rev[i] = r
	}
	for k := 0; k < n/2; k++ {
		ang := -2 * math.Pi * float64(k) / float64(n)
		f.tw[k] = complex(math.Cos(ang), math.Sin(ang))
	}
	return f
}

// forward transforms src in place; len(src) must equal the plan size.
func (f *fft) forward(src []complex128) { f.transform(src, false) }

// inverse transforms src in place and scales by 1/N.
func (f *fft) inverse(src []complex128) {
	f.transform(src, true)
	for i := range src {
		src[i] /= complex(float64(f.n), 0)
	}
}

func (f *fft) transform(src []complex128, inv bool) {
	n := f.n
	if len(src) != n {
		panic(fmt.Sprintf("dsp: FFT 长度不匹配: %d != %d", len(src), n))
	}
	// bit-reversal permutation
	for i := 0; i < n; i++ {
		if j := f.rev[i]; j > i {
			src[i], src[j] = src[j], src[i]
		}
	}
	// butterflies
	for size := 2; size <= n; size <<= 1 {
		half := size >> 1
		stride := n / size
		for i := 0; i < n; i += size {
			for j := 0; j < half; j++ {
				w := f.tw[j*stride]
				if inv {
					w = complex(real(w), -imag(w))
				}
				u := src[i+j]
				v := src[i+j+half] * w
				src[i+j] = u + v
				src[i+j+half] = u - v
			}
		}
	}
}

// spectrum converts a real frame into the one-sided power spectrum.
//
// The doubling of bins 1..N/2-1 makes the one-sided vector carry the same total
// energy as the (complex) two-sided spectrum, so Parseval's theorem holds for
// the vector we actually use downstream.
func (f *fft) spectrum(dst []float64, frame []float32) {
	n := f.n
	if len(dst) != n/2+1 || len(frame) != n {
		panic("dsp: spectrum 长度不匹配")
	}
	w := f.work
	for i, v := range frame {
		w[i] = complex(float64(v), 0)
	}
	f.forward(w)
	dst[0] = real(w[0]) * real(w[0])
	for k := 1; k < n/2; k++ {
		re, im := real(w[k]), imag(w[k])
		dst[k] = 2 * (re*re + im*im)
	}
	re, im := real(w[n/2]), imag(w[n/2])
	dst[n/2] = re*re + im*im
}

// powerSpectrum is a convenience wrapper that allocates a plan and returns the
// one-sided power spectrum of one frame (used by tests and one-off analysis).
func powerSpectrum(frame []float32) ([]float64, error) {
	n := len(frame)
	if n < 2 || n&(n-1) != 0 {
		return nil, fmt.Errorf("帧长必须是 2 的幂，得到 %d", n)
	}
	f := newFFT(n)
	out := make([]float64, n/2+1)
	f.spectrum(out, frame)
	return out, nil
}

// ---------------------------------------------------------------------------
// Analyzer (streaming)
// ---------------------------------------------------------------------------

// Analyzer turns an arbitrarily chunked 48 kHz mono stream into normalised
// log-mel patches.
//
// It is deliberately "sample driven": every complete frame whose window lies
// entirely inside the samples received so far is emitted exactly once, in
// chronological order, with frame start indices HopSize*i. Params.Frames calls
// the very same code path, so streaming and batch analysis of the same buffer
// produce bit-identical frames.
//
// An Analyzer is NOT safe for concurrent use (the realtime engine of P3 drives
// it from a single capture goroutine).
type Analyzer struct {
	p      Params
	bank   *MelBank
	plan   *fft
	win    []float32 // precomputed periodic Hann coefficients
	demean []float32 // de-meaned frame scratch
	wframe []float32 // windowed frame scratch
	power  []float64

	// noise is the environment-noise front-end (noise.go). It is created from
	// Params.Noise and, when active, filters every sample in Push and applies
	// a per-bin Wiener gain to every frame's power spectrum in emitFrameAt.
	// It is deliberately applied to templates (index build) and live audio
	// alike, so both sides of the comparison see the same processing.
	noise *noiseReducer

	// sring holds the samples still addressable by emitFrameAt, i.e. the
	// absolute sample range [sbase, stot). Frame i starts at i*HopSize, so a
	// frame needs at most the previous FrameSize samples; keeping the whole
	// history is what lets WindowAt address any past frame. Trim lowers sbase
	// and releases the dropped samples (the offline paths never trim, so their
	// behaviour is unchanged).
	sring []float32
	// rring holds the SAME sample range as sring but unfiltered: the audio
	// exactly as it arrived. It exists solely for frameEnergyAt, because both
	// the frame-level noise gate and the index anchor must be judged on the real
	// audio rather than on the front-end's output (the high-pass stage would
	// otherwise change what counts as "loud").
	rring []float32
	sbase int64 // absolute index of sring[0]
	stot  int64 // total samples ever pushed

	// lring is the level ring: the last LevelWindowSamples samples (circular).
	lring []float32
	ltot  int64

	// frames holds the log-mel frames still addressable by WindowAt/Frame, i.e.
	// the absolute frame range [fbase, fbase+len(frames)). Without trimming this
	// is EVERY frame ever emitted, in chronological order, so WindowAt can build
	// any window of the history (the index builder needs the loudest window, the
	// match CLI needs every window position, and the realtime engine needs the
	// newest one). Memory is MelBands*4 bytes per 5.333 ms ~= 48 kB/s, which is
	// why the realtime path calls Trim.
	frames [][]float32
	fbase  int64 // absolute index of frames[0]
	// rawEnergy holds the mean-square of every frame measured on the audio as
	// received, BEFORE the noise front-end ran. It exists because the frame-level
	// noise gate can legitimately zero a frame that contains the sound itself
	// (when the ambience is as loud as the sound), and an anchor chosen from the
	// gated frames would then point at the ambience instead of at the sound. The
	// index builder and the live anchor search therefore use this raw curve.
	rawEnergy []float64
	rbase     int64 // absolute index of rawEnergy[0]
	// ftot is the total number of frames ever emitted (NOT len(frames) once
	// trimming has happened); WindowAt/Frame take absolute frame indices.
	ftot int64
}

// NewAnalyzer validates p and builds the reusable FFT plan, window and mel bank
// so that Push itself allocates nothing for the analysis path.
func NewAnalyzer(p Params) (*Analyzer, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	bank, err := NewMelBank(p)
	if err != nil {
		return nil, err
	}
	a := &Analyzer{
		p:      p,
		bank:   bank,
		plan:   newFFT(p.FrameSize),
		win:    make([]float32, p.FrameSize),
		demean: make([]float32, p.FrameSize),
		wframe: make([]float32, p.FrameSize),
		power:  make([]float64, p.FrameSize/2+1),
		lring:  make([]float32, LevelWindowSamples),
		frames: make([][]float32, 0, 256),
	}
	hannPeriodic(a.win)
	a.noise = newNoiseReducer(p.Noise, p.SampleRate, p.FrameSize, p.HopSize, a.plan)
	return a, nil
}

// Params returns the analyzer configuration.
func (a *Analyzer) Params() Params { return a.p }

// Reset drops all buffered audio, frames and level history.
func (a *Analyzer) Reset() {
	a.sring = a.sring[:0]
	for i := range a.lring {
		a.lring[i] = 0
	}
	a.frames = a.frames[:0]
	a.rawEnergy = a.rawEnergy[:0]
	a.stot, a.ltot, a.ftot = 0, 0, 0
	a.sbase, a.fbase = 0, 0
	a.rbase = 0
	a.noise.reset()
}

// ReleaseFrames drops the historical frame buffer (and therefore makes
// WindowAt unavailable for old frames) while keeping the sample and level
// buffers intact. A long-running realtime engine can call it periodically to
// bound memory; P2's offline paths never need to (the CLI drives one finite
// file per process).
//
// It resets the frame clock (FrameCount drops back to 0), exactly as it always
// did, and it deliberately leaves the SAMPLE ring alone - which is why a
// realtime link needs Trim instead: this method alone still leaks 192 kB/s.
func (a *Analyzer) ReleaseFrames() {
	a.frames = a.frames[:0]
	a.rawEnergy = a.rawEnergy[:0]
	a.fbase = 0
	a.rbase = 0
	a.ftot = 0
}

// ---------------------------------------------------------------------------
// Realtime memory bounding (added for the P2 realtime link)
// ---------------------------------------------------------------------------

// Trim bounds the memory of a long-running streaming analyzer.
//
// Why it exists: ReleaseFrames only drops the log-mel frame history (~48 kB/s),
// while the SAMPLE ring keeps every sample ever pushed (~192 kB/s = 691 MB/h at
// 48 kHz), which is unusable for a link that stays up for hours. Trim keeps the
// newest keepFrames frames (raised to WindowFrames, so Window/Ready keep
// working) plus just enough samples for the next frame, and releases
// everything older to the garbage collector.
//
// Absolute frame indices stay valid: WindowAt/Frame take an index counted from
// the first frame ever emitted, and indices that have been trimmed away return
// nil instead of stale data. Trim is a no-op when the analyzer already holds
// less than keepFrames frames.
func (a *Analyzer) Trim(keepFrames int) {
	if keepFrames < a.p.WindowFrames {
		keepFrames = a.p.WindowFrames
	}
	if drop := len(a.frames) - keepFrames; drop > 0 {
		copy(a.frames, a.frames[drop:])
		for i := keepFrames; i < len(a.frames); i++ {
			// Nil the tail so the dropped frames (and their backing arrays) can
			// actually be collected instead of being pinned by the slice.
			a.frames[i] = nil
		}
		a.frames = a.frames[:keepFrames]
		a.fbase += int64(drop)
	}
	if drop := len(a.rawEnergy) - keepFrames; drop > 0 {
		copy(a.rawEnergy, a.rawEnergy[drop:])
		a.rawEnergy = a.rawEnergy[:keepFrames]
		a.rbase += int64(drop)
	}
	if len(a.frames) < cap(a.frames)/4 && cap(a.frames) > 4*keepFrames {
		// A one-off huge Push would otherwise leave a big backing array behind.
		slim := make([][]float32, len(a.frames), keepFrames)
		copy(slim, a.frames)
		a.frames = slim
	}
	if len(a.rawEnergy) < cap(a.rawEnergy)/4 && cap(a.rawEnergy) > 4*keepFrames {
		slim := make([]float64, len(a.rawEnergy), keepFrames)
		copy(slim, a.rawEnergy)
		a.rawEnergy = slim
	}
	a.ftot = a.fbase + int64(len(a.frames))

	// Samples: keep one frame plus one hop, which by construction contains the
	// next frame this Analyzer will emit (Push emits the frame ending at
	// stot-1, and emitFrameAt reads sring[start-sbase : start-sbase+FrameSize]).
	keep := a.p.FrameSize + a.p.HopSize
	if drop := len(a.sring) - keep; drop > 0 {
		copy(a.sring, a.sring[drop:])
		a.sring = a.sring[:keep]
		copy(a.rring, a.rring[drop:])
		a.rring = a.rring[:keep]
		a.sbase += int64(drop)
	}
	if cap(a.sring) > 4*keep {
		slim := make([]float32, len(a.sring), keep)
		copy(slim, a.sring)
		a.sring = slim
	}
	if cap(a.rring) > 4*keep {
		slim := make([]float32, len(a.rring), keep)
		copy(slim, a.rring)
		a.rring = slim
	}
}

// BufferedSamples returns how many raw samples the analyzer still retains. The
// realtime link uses it (and BufferedFrames) to prove that memory does not grow
// with runtime; it is 0 for a freshly created or Reset analyzer.
func (a *Analyzer) BufferedSamples() int { return len(a.sring) }

// BufferedFrames returns how many log-mel frames the analyzer still retains.
func (a *Analyzer) BufferedFrames() int { return len(a.frames) }

// Push appends an arbitrary number of 48 kHz mono samples and analyses every
// frame that has become complete.
//
// The noise front-end runs first and hands back a filtered copy of the same
// samples, so the audio the caller passed in is never modified. A front-end that
// is switched off leaves the samples untouched, which is what keeps the
// `noise.method: off` path bit-identical to the pre-noise-reduction pipeline.
func (a *Analyzer) Push(pcm []float32) {
	p := a.p
	if len(pcm) == 0 {
		return
	}
	in := pcm
	filtered := pcm
	if a.noise != nil {
		a.noise.Push(pcm)
		if out := a.noise.Out(); len(out) == len(in) {
			filtered = out
		}
	}
	for i, raw := range in {
		v := filtered[i]
		a.sring = append(a.sring, v)
		a.rring = append(a.rring, raw)
		a.lring[int(a.ltot)%LevelWindowSamples] = v
		a.stot++
		a.ltot++
		// A frame ending at sample stot-1 starts at stot-FrameSize. The noise
		// estimator is advanced to this frame's END first so the last tracker
		// window is this same frame; event bins are not written into the floor
		// (see noiseReducer.AdvanceTo).
		if off := a.stot - int64(p.FrameSize); off >= 0 && off%int64(p.HopSize) == 0 {
			a.noise.AdvanceTo(int(a.stot))
			a.emitFrameAt(off)
		}
	}
}

// flush analyses whatever trailing samples remain, even when the next frame
// start does not fall on a HopSize boundary. The frame it emits has start index
// HopSize*FrameCount(n); because that index is strictly greater than every index
// Push has already emitted, no frame is ever emitted twice, and the frame is
// exactly the one a whole-buffer call would have produced.
func (a *Analyzer) flush() {
	p := a.p
	next := a.nextStart()
	if next < 0 {
		return
	}
	if next+int64(p.FrameSize) <= a.stot {
		a.emitFrameAt(next)
	}
}

// nextStart returns the start index of the next frame not yet emitted, or -1
// when the buffer does not hold one.
func (a *Analyzer) nextStart() int64 {
	p := a.p
	if a.stot < int64(p.FrameSize) {
		return -1
	}
	return a.ftot * int64(p.HopSize)
}

// emitFrameAt analyses the frame starting at absolute sample index start and
// appends it to the frame history. It is a no-op when the frame is not fully
// buffered any more (Trim dropped its samples) or not buffered yet.
func (a *Analyzer) emitFrameAt(start int64) {
	p := a.p
	if start < a.sbase || start+int64(p.FrameSize) > a.stot {
		return
	}
	off := int(start - a.sbase)
	// The frame-level noise gate and the anchor both judge the REAL audio, so
	// the raw energy is recorded before anything is decided.
	raw := a.rring[off : off+p.FrameSize]
	a.rawEnergy = append(a.rawEnergy, frameMeanSquare(raw))
	// Frame-level SNR gate (noise.go): a frame that holds nothing but ambience
	// must not contribute to the patch at all. Skipping the whole analysis is
	// both cheaper and cleaner than attenuating a spectrum that is pure noise.
	if a.noise.frameGate(frameLevelDBFS(raw)) {
		a.frames = append(a.frames, make([]float32, p.MelBands))
		a.ftot = a.fbase + int64(len(a.frames))
		return
	}
	// Gather + de-mean + window.
	var sum float32
	for i := 0; i < p.FrameSize; i++ {
		sum += a.sring[off+i]
	}
	mean := sum / float32(p.FrameSize)
	for i := 0; i < p.FrameSize; i++ {
		a.demean[i] = a.sring[off+i] - mean
		a.wframe[i] = a.demean[i] * a.win[i]
	}
	a.plan.spectrum(a.power, a.wframe)
	// Environment-noise reduction: Wiener-attenuate the tracked stationary
	// noise power before the mel bank integrates it. Inactive (and a single
	// branch) when the front-end is off or has no estimate yet.
	a.noise.apply(a.power)

	melRaw := make([]float64, p.MelBands)
	for c := 0; c < p.MelBands; c++ {
		var e float64
		base := c * a.bank.Bins
		for b := 0; b < a.bank.Bins; b++ {
			if w := a.bank.Weights[base+b]; w != 0 {
				e += w * a.power[b]
			}
		}
		melRaw[c] = e
	}
	a.frames = append(a.frames, logMel(melRaw))
	a.ftot = a.fbase + int64(len(a.frames))
}

// FrameCount returns the number of complete frames emitted so far (absolute,
// i.e. it keeps counting across Trim/ReleaseFrames).
func (a *Analyzer) FrameCount() int { return int(a.ftot) }

// Ready reports whether at least WindowFrames frames are available, i.e.
// whether Window will return a patch.
func (a *Analyzer) Ready() bool { return len(a.frames) >= a.p.WindowFrames }

// Window returns the most recent WindowFrames frames as one normalised
// (mean-subtracted, unit L2 norm) MelBands*WindowFrames patch, or nil when not
// Ready. The result is a fresh slice the caller owns.
func (a *Analyzer) Window() []float32 {
	if !a.Ready() {
		return nil
	}
	return a.WindowAt(int(a.ftot) - 1)
}

// WindowAt returns the normalised patch whose LAST frame is endFrame (0-based,
// counted from the first frame ever emitted). endFrame must be >=
// WindowFrames-1 and must still be buffered (see Trim); otherwise WindowAt
// returns nil. It is the primitive the offline sliding-window search is built
// on: analyse the audio once, then walk every window position without
// re-running the STFT.
//
// The returned slice is freshly allocated and owned by the caller.
func (a *Analyzer) WindowAt(endFrame int) []float32 {
	p := a.p
	i := endFrame - int(a.fbase)
	if i < p.WindowFrames-1 || i >= len(a.frames) {
		return nil
	}
	out := make([]float32, p.Dim())
	for k := 0; k < p.WindowFrames; k++ {
		f := a.frames[i-p.WindowFrames+1+k]
		copy(out[k*p.MelBands:(k+1)*p.MelBands], f)
	}
	return normalizeInPlace(out)
}

// Frame returns the raw (un-normalised) log-mel values of absolute frame i, or
// nil when i is outside the retained history. The slice must not be modified by
// the caller.
func (a *Analyzer) Frame(i int) []float32 {
	j := i - int(a.fbase)
	if j < 0 || j >= len(a.frames) {
		return nil
	}
	return a.frames[j]
}

// FrameEnergyAt returns the mean-square of the audio of absolute frame i,
// measured on the audio AS RECEIVED (before the noise front-end). It is what the
// anchor search must use: the front-end's frame gate can zero a frame that holds
// the sound itself, and an anchor picked from gated frames would point at the
// ambience instead. It returns -1 when i is outside the retained history.
func (a *Analyzer) FrameEnergyAt(i int) float64 {
	j := i - int(a.rbase)
	if j < 0 || j >= len(a.rawEnergy) {
		return -1
	}
	return a.rawEnergy[j]
}

// FrameEnergyCount returns how many raw frame energies are retained.
func (a *Analyzer) FrameEnergyCount() int { return len(a.rawEnergy) }

// FrameEnergy returns the mean-square of the audio of every retained frame
// measured before the noise front-end, in chronological order, together with the
// absolute index of the first one.
func (a *Analyzer) FrameEnergy() ([]float64, int) {
	out := make([]float64, len(a.rawEnergy))
	copy(out, a.rawEnergy)
	return out, int(a.rbase)
}

// WindowLevelDBFS is the loudest analysis frame still inside the current
// feature window, in dBFS. LevelDBFS only covers the last ~50 ms, while a
// patch spans ~187 ms, so a short click has already fallen back into the bed
// by the time the patch is centred on it. The realtime matcher uses this
// (alongside LevelDBFS) so that aligned window is still scored. Digital
// silence maps to -Inf. The value is measured on the audio as received, before
// the noise front-end.
func (a *Analyzer) WindowLevelDBFS() float64 {
	if len(a.rawEnergy) == 0 {
		return math.Inf(-1)
	}
	n := a.p.WindowFrames
	if n > len(a.rawEnergy) {
		n = len(a.rawEnergy)
	}
	maxSq := 0.0
	for i := len(a.rawEnergy) - n; i < len(a.rawEnergy); i++ {
		if a.rawEnergy[i] > maxSq {
			maxSq = a.rawEnergy[i]
		}
	}
	if maxSq <= 0 {
		return math.Inf(-1)
	}
	return 10 * math.Log10(maxSq)
}

// LevelDBFS returns the RMS level of roughly the last 50 ms (2400 samples) in
// dBFS, i.e. 20*log10(rms). Digital silence maps to -Inf.
func (a *Analyzer) LevelDBFS() float64 {
	n := int(a.ltot)
	if n > LevelWindowSamples {
		n = LevelWindowSamples
	}
	if n == 0 {
		return math.Inf(-1)
	}
	var sq float64
	for i := 0; i < n; i++ {
		v := float64(a.lring[int(a.ltot-int64(n)+int64(i))%LevelWindowSamples])
		sq += v * v
	}
	rms := math.Sqrt(sq / float64(n))
	if rms <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(rms)
}

// DenoisedLevelDBFS is LevelDBFS after the latest frame's broadband Wiener
// attenuation. It uses the same 50 ms window as LevelDBFS, shifted down by how
// many dB the noise gain removed. A short click that skips the gain reads the
// same as LevelDBFS. The value is for the live meter only; it is not part of
// the fingerprint.
func (a *Analyzer) DenoisedLevelDBFS() float64 {
	lv := a.LevelDBFS()
	if math.IsInf(lv, 0) || a.noise == nil {
		return lv
	}
	return lv - a.noise.ReductionDB()
}

// NoiseFloorDBFS returns the tracked environment-noise floor in dBFS, or -Inf
// while the front-end is off or has not measured anything yet. It is comparable
// with LevelDBFS: both are the RMS of a ~20 ms window, one of the newest audio
// and one of the quietest recent audio.
func (a *Analyzer) NoiseFloorDBFS() float64 {
	if a.noise == nil {
		return math.Inf(-1)
	}
	return a.noise.NoiseFloorDBFS()
}

// GateDBFS returns the silence gate the realtime matcher should use, given the
// configured absolute gate. With Noise.AdaptiveGate on, it is
// max(configured, noiseFloor+Noise.GateMarginDB) clamped to Noise.GateFloorDBFS,
// so a noisy game raises the gate above its own ambience while a quiet one keeps
// the configured -60 dBFS. It never lowers the gate below the configuration.
func (a *Analyzer) GateDBFS(configured float64) float64 {
	if a.noise == nil {
		return configured
	}
	return a.noise.gateDBFS(configured)
}

// frameLevelDBFS is the RMS level of one analysis frame in dBFS, used by the
// frame-level SNR gate. Digital silence maps to -Inf.
func frameLevelDBFS(frame []float32) float64 {
	sq := frameMeanSquare(frame)
	if sq <= 0 {
		return math.Inf(-1)
	}
	return 10 * math.Log10(sq)
}

// frameMeanSquare is the mean of the squares of frame, i.e. the frame's power.
func frameMeanSquare(frame []float32) float64 {
	if len(frame) == 0 {
		return 0
	}
	var sq float64
	for _, v := range frame {
		sq += float64(v) * float64(v)
	}
	return sq / float64(len(frame))
}

// LevelDBFSOf returns the RMS level in dBFS of a whole buffer (helper for
// offline analysis, e.g. the `match` CLI's silence gate).
func LevelDBFSOf(pcm []float32) float64 {
	if len(pcm) == 0 {
		return math.Inf(-1)
	}
	var sq float64
	for _, v := range pcm {
		sq += float64(v) * float64(v)
	}
	rms := math.Sqrt(sq / float64(len(pcm)))
	if rms <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(rms)
}

// FrameEnergies returns the mean log-mel value of every frame; a cheap and
// robust "how loud is this frame" proxy used to pick index anchors.
func (a *Analyzer) FrameEnergies() []float64 {
	out := make([]float64, 0, len(a.frames))
	for _, f := range a.frames {
		var s float64
		for _, v := range f {
			s += float64(v)
		}
		out = append(out, s/float64(len(f)))
	}
	return out
}

// ErrShortSignal is returned by helpers that need at least one full window.
var ErrShortSignal = errors.New("音频太短，凑不满一个特征窗口")
