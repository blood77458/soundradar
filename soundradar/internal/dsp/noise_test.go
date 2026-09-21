package dsp

import (
	"math"
	"math/rand"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// test signal helpers
// ---------------------------------------------------------------------------

// burstPCM builds a n-sample buffer of deterministic broadband noise at RMS
// `amb` with a short (length-sample) burst of gen() in the middle. That is the
// realistic shape of a game sound effect: the sound itself lasts tens of
// milliseconds, the ambience lasts the whole buffer.
func burstPCM(n, at, length int, amb float64, gen func(int) float64) []float32 {
	return burstSeeded(n, at, length, amb, gen, 99)
}

// burstSeeded is burstPCM with an explicit noise seed, so tests can measure how
// much a result depends on the particular noise realisation.
func burstSeeded(n, at, length int, amb float64, gen func(int) float64, seed uint32) []float32 {
	pcm := make([]float32, n)
	s := seed
	for i := range pcm {
		s = s*1664525 + 1013904223
		u := (float64(s>>8)/float64(1<<24))*2 - 1
		v := u * amb * math.Sqrt(3) // uniform with RMS == amb
		if i >= at && i < at+length {
			env := 1.0
			if i-at < length/8 {
				env = float64(i-at) / float64(length/8)
			} else if at+length-i < length/8 {
				env = float64(at+length-i) / float64(length/8)
			}
			v += gen(i) * env
		}
		pcm[i] = float32(v)
	}
	return pcm
}

// sound returns a deterministic harmonic stack, i.e. a "sound effect" whose
// log-mel shape is stable.
func sound(i int) float64 {
	return 0.45*math.Sin(2*math.Pi*500*float64(i)/48000) +
		0.25*math.Sin(2*math.Pi*1500*float64(i)/48000) +
		0.12*math.Sin(2*math.Pi*3000*float64(i)/48000)
}

// otherSound is a clearly different sound, used as the competing item.
func otherSound(i int) float64 {
	return 0.45*math.Sin(2*math.Pi*1600*float64(i)/48000) +
		0.25*math.Sin(2*math.Pi*4800*float64(i)/48000)
}

// cosine of two unit vectors.
func cosine(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// patchOf returns the normalised patch anchored on the sound's energy peak, the
// same convention internal/index.AnchorPatch uses: the raw frame energy is
// smoothed with a centred moving average before the argmax is taken, because the
// raw argmax of a burst's plateau is decided by ambience rather than by the
// sound.
func patchOf(t *testing.T, p Params, pcm []float32) []float32 {
	t.Helper()
	frames := p.Frames(pcm)
	if len(frames) < p.WindowFrames {
		t.Fatalf("信号太短: %d 帧（需要 %d 帧）", len(frames), p.WindowFrames)
	}
	e := make([]float64, len(frames))
	for i := range frames {
		from := i * p.HopSize
		to := from + p.FrameSize
		if to > len(pcm) {
			break
		}
		var sq float64
		for _, v := range pcm[from:to] {
			sq += float64(v) * float64(v)
		}
		e[i] = sq / float64(p.FrameSize)
	}
	// Centred moving average, mirroring index.anchorSmoothFrames.
	const smooth = 15
	half := smooth / 2
	sm := make([]float64, len(e))
	for i := range e {
		lo, hi := max(0, i-half), min(len(e), i+half+1)
		var s float64
		for k := lo; k < hi; k++ {
			s += e[k]
		}
		sm[i] = s / float64(hi-lo)
	}
	best, bestV := 0, -1.0
	for i := range sm {
		if sm[i] > bestV {
			best, bestV = i, sm[i]
		}
	}
	halfWin := (p.WindowFrames - 1) / 2
	start := max(0, min(best-halfWin, len(frames)-p.WindowFrames))
	patch := make([]float32, 0, p.Dim())
	for f := start; f < start+p.WindowFrames; f++ {
		patch = append(patch, frames[f]...)
	}
	return Normalize(patch)
}

// withNoise is the convenience wrapper used by most tests here: the standard
// sound with a given ambience level.
func withNoise(n int, amb float64) []float32 {
	return burstPCM(n, 9600, 4800, amb, sound)
}

// paramsWith returns the default parameters with the noise front-end set up:
// method overrides the default when non-empty, and a non-zero alpha/gainFloorDB
// overrides the spectral defaults.
func paramsWith(method string, alpha, gainFloorDB float64) Params {
	p := DefaultParams()
	if method != "" {
		p.Noise.Method = method
	}
	if alpha != 0 {
		p.Noise.OverSubtract = alpha
	}
	if gainFloorDB != 0 {
		p.Noise.GainFloorDB = gainFloorDB
	}
	return p
}

// ---------------------------------------------------------------------------
// the core acceptance tests
// ---------------------------------------------------------------------------

// TestNoiseReductionSeparatesSoundFromAmbience is the main acceptance test for the
// environment-noise front-end: with a library of two sounds, a query that is one
// of them heard through ambience must be pulled back towards its own template by
// the front-end. The interesting number is the MARGIN over the competing item,
// because that is what the matcher's MinMargin gate and the per-item threshold
// compare against.
func TestNoiseReductionSeparatesSoundFromAmbience(t *testing.T) {
	const (
		n      = 28800
		at     = 9600
		length = 4800
	)
	loud := func(g func(int) float64) func(int) float64 {
		return func(i int) float64 { return g(i) * 3 }
	}
	measure := func(method string, amb float64) (self, other float64) {
		p := DefaultParams()
		p.Noise.Method = method
		refSelf := patchOf(t, p, burstPCM(n, at, length, 0.002, loud(sound)))
		refOther := patchOf(t, p, burstPCM(n, at, length, 0.002, loud(otherSound)))
		live := patchOf(t, p, burstPCM(n, at, length, amb, sound))
		lo := patchOf(t, p, burstPCM(n, at, length, amb, otherSound))
		return cosine(refSelf, live), cosine(refOther, lo)
	}

	// A loud ambience (about -1 dB SNR for the raw frame level) is where the old
	// pipeline started losing sounds.
	const heavy = 1.0
	offSelf, offOther := measure(NoiseOff, heavy)
	onSelf, onOther := measure(NoiseSpectral, heavy)
	t.Logf("重环境音（%.1f）：过滤前 自身 %.4f / 干扰 %.4f（领先 %+.4f）", heavy, offSelf, offOther, offSelf-offOther)
	t.Logf("重环境音（%.1f）：过滤后 自身 %.4f / 干扰 %.4f（领先 %+.4f）", heavy, onSelf, onOther, onSelf-onOther)

	if onSelf <= offSelf {
		t.Fatalf("环境音过滤没有提升自身相似度: %.4f -> %.4f", offSelf, onSelf)
	}
	if marginOn, marginOff := onSelf-onOther, offSelf-offOther; marginOn <= marginOff {
		t.Fatalf("环境音过滤没有拉开与干扰音的距离: %.4f -> %.4f", marginOff, marginOn)
	}

	// A clean recording must be left alone, otherwise the whole existing library
	// would suddenly score differently.
	cleanOff, _ := measure(NoiseOff, 0.002)
	cleanOn, _ := measure(NoiseSpectral, 0.002)
	t.Logf("干净信号：过滤前 %.4f -> 过滤后 %.4f", cleanOff, cleanOn)
	if cleanOn < cleanOff-0.02 {
		t.Fatalf("干净信号被过滤破坏：%.4f -> %.4f", cleanOff, cleanOn)
	}
}

// TestNoiseReductionKeepsCleanAudioIntact guards the "do not break the existing
// library" property at the patch level.
func TestNoiseReductionKeepsCleanAudioIntact(t *testing.T) {
	clean := burstPCM(28800, 9600, 4800, 0.0005, sound)
	off := DefaultParams()
	off.Noise.Method = NoiseOff
	a := patchOf(t, off, clean)
	b := patchOf(t, DefaultParams(), clean)
	dot := cosine(a, b)
	t.Logf("干净样本经环境音过滤前后的自相似度 = %.6f", dot)
	if dot < 0.995 {
		t.Fatalf("干净样本被过滤前后面目全非（相似度 %.6f）", dot)
	}
}

// TestNoiseFloorTrackedAgainstKnownLevel checks that the tracked noise floor is
// comparable with LevelDBFS (the adaptive gate depends on that calibration).
func TestNoiseFloorTrackedAgainstKnownLevel(t *testing.T) {
	for _, db := range []float64{-30, -45, -60} {
		p := DefaultParams()
		a, err := NewAnalyzer(p)
		if err != nil {
			t.Fatal(err)
		}
		amp := math.Pow(10, db/20) // RMS == amp for uniform noise
		pcm := make([]float32, 48000)
		rng := rand.New(rand.NewSource(11))
		for i := range pcm {
			pcm[i] = float32((rng.Float64()*2 - 1) * amp * math.Sqrt(3))
		}
		a.Push(pcm)
		got := a.NoiseFloorDBFS()
		t.Logf("噪声电平 %.1f dBFS -> 估计值 %.1f dBFS（差 %.2f dB）", db, got, math.Abs(got-db))
		if math.Abs(got-db) > 3 {
			t.Fatalf("噪声底估计 %.1f dBFS 与真实 %.1f dBFS 相差超过 3 dB", got, db)
		}
	}
}

// TestNoiseGateBehaviour documents the adaptive gate contract: it only raises the
// configured gate, never lowers it, it follows the ambience, and it refuses to
// engage when the audio has no silence to measure (a continuous sound).
func TestNoiseGateBehaviour(t *testing.T) {
	p := DefaultParams()

	// Digital silence: the configured gate must survive untouched.
	silent, err := NewAnalyzer(p)
	if err != nil {
		t.Fatal(err)
	}
	silent.Push(make([]float32, 24000))
	if gate := silent.GateDBFS(-55); gate != -55 {
		t.Fatalf("数字静音时门限被改成 %.1f，应当保持配置值 -55", gate)
	}

	// A quiet ambience with loud bursts in between: the tracker sees real
	// silence, so the gate must rise to floor+margin dB.
	rng := rand.New(rand.NewSource(3))
	loud, err := NewAnalyzer(p)
	if err != nil {
		t.Fatal(err)
	}
	amp := math.Pow(10, -30.0/20)
	pcm := make([]float32, 96000)
	for i := range pcm {
		pcm[i] = float32((rng.Float64()*2 - 1) * amp * math.Sqrt(3))
		if i%24000 < 2000 { // a burst every half second
			pcm[i] += float32(0.5 * math.Sin(2*math.Pi*1000*float64(i)/48000))
		}
	}
	loud.Push(pcm)
	floor := loud.NoiseFloorDBFS()
	gate := loud.GateDBFS(-60)
	t.Logf("有起有伏的音频：噪声底 %.2f dBFS -> 门限 %.2f dBFS（配置值 -60）", floor, gate)
	margin := p.Noise.GateMarginDB
	if math.Abs(gate-(floor+margin)) > 1.0 {
		t.Fatalf("自适应门限 %.2f 不是噪声底 %.2f 加 %.0f dB", gate, floor, margin)
	}
	if gate <= -60 {
		t.Fatalf("自适应门限 %.2f 没有抬到配置值 -60 以上", gate)
	}

	// A CONTINUOUS LOUD sound has no measurable silence: the tracker cannot tell
	// it apart from a noise floor, so the gate must stay at the configured value
	// instead of rising above the sound and silencing it forever.
	steady, err := NewAnalyzer(p)
	if err != nil {
		t.Fatal(err)
	}
	tone := make([]float32, 96000)
	for i := range tone {
		tone[i] = float32(0.6 * math.Sin(2*math.Pi*880*float64(i)/48000))
	}
	steady.Push(tone)
	if gate := steady.GateDBFS(-60); gate != -60 {
		t.Fatalf("持续响音的门限被抬到 %.2f，会把自己彻底屏蔽掉（应当保持 -60）", gate)
	}
	t.Logf("持续 880 Hz 响音：噪声底 %.2f dBFS -> 门限 %.2f dBFS（保持配置值）",
		steady.NoiseFloorDBFS(), steady.GateDBFS(-60))

	// Quiet continuous room tone (no bursts) must still raise the gate: otherwise
	// game ambience at ~-52 dBFS keeps the configured -60 gate and floods the
	// matcher with 0.6+ cosine hits against soft library items.
	amb, err := NewAnalyzer(p)
	if err != nil {
		t.Fatal(err)
	}
	ambAmp := math.Pow(10, -52.0/20)
	room := make([]float32, 96000)
	for i := range room {
		room[i] = float32((rng.Float64()*2 - 1) * ambAmp * math.Sqrt(3))
	}
	amb.Push(room)
	ambFloor := amb.NoiseFloorDBFS()
	ambGate := amb.GateDBFS(-60)
	t.Logf("持续安静环境音：噪声底 %.2f dBFS -> 门限 %.2f dBFS（配置值 -60）", ambFloor, ambGate)
	if ambGate <= -60 {
		t.Fatalf("安静持续环境音的自适应门限 %.2f 没有抬到配置值 -60 以上（底 %.2f）", ambGate, ambFloor)
	}
	if math.Abs(ambGate-(ambFloor+p.Noise.GateMarginDB)) > 1.5 {
		t.Fatalf("安静环境音门限 %.2f 不是噪声底 %.2f 加 %.0f dB", ambGate, ambFloor, p.Noise.GateMarginDB)
	}
}

func TestDenoisedLevelSitsBelowSteadyNoise(t *testing.T) {
	a, err := NewAnalyzer(DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(7))
	amp := math.Pow(10, -30.0/20)
	pcm := make([]float32, 48000*2)
	for i := range pcm {
		pcm[i] = float32((rng.Float64()*2 - 1) * amp * math.Sqrt(3))
	}
	a.Push(pcm)
	lv, dn := a.LevelDBFS(), a.DenoisedLevelDBFS()
	t.Logf("持续噪声 电平 %.2f dBFS，去噪后 %.2f dBFS，压低 %.2f dB", lv, dn, lv-dn)
	if dn > lv-1 {
		t.Fatalf("去噪后电平 %.2f 应明显低于当前电平 %.2f", dn, lv)
	}
}

// clickBurst is a deterministic broadband click sample, used to check that a
// short mechanical hit is not erased bin-by-bin by the Wiener gain.
func clickBurst(i int) float64 {
	s := uint32(i+17)*1664525 + 1013904223
	return (float64(s>>8)/float64(1<<24))*2 - 1
}

// TestNoiseReductionKeepsShortClick is the fingerprint equivalent of the
// listening-demo's short-transient paste-back: a 20 ms broadband hit must
// trigger the energy detector, and the Wiener stage must not erase it on top
// of the high-pass. Broadband clicks in heavy ambience are not expected to
// beat the unfiltered cosine — the high-pass strips the click envelope, which
// is the same on the template and the query.
func TestNoiseReductionKeepsShortClick(t *testing.T) {
	const (
		n      = 28800
		at     = 9600
		length = 960 // 20 ms at 48 kHz
		amb    = 0.5
	)
	loudClick := func(i int) float64 { return 1.0 * clickBurst(i) }
	measure := func(method string) float64 {
		p := DefaultParams()
		p.Noise.Method = method
		ref := patchOf(t, p, burstPCM(n, at, length, 0.002, loudClick))
		live := patchOf(t, p, burstPCM(n, at, length, amb, loudClick))
		return cosine(ref, live)
	}
	off, hp, on := measure(NoiseOff), measure(NoiseHighPass), measure(NoiseSpectral)
	t.Logf("20 ms 点击自身相似度：关闭 %.4f / 仅高通 %.4f / 谱减 %.4f", off, hp, on)
	if on < hp-0.03 {
		t.Fatalf("谱减把短点击压得比仅高通更差: %.4f -> %.4f", hp, on)
	}

	live := burstPCM(n, at, length, amb, loudClick)
	a, err := NewAnalyzer(DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	protected := 0
	for off := 0; off < n; {
		step := 256
		if off+step > n {
			step = n - off
		}
		a.Push(live[off : off+step])
		off += step
		if a.noise != nil && a.noise.protectFrame {
			protected++
		}
	}
	t.Logf("短点击期间保护了 %d 帧", protected)
	if protected < 2 {
		t.Fatalf("20 ms 点击没有触发短时保护（保护帧 %d）", protected)
	}

	clean := burstPCM(n, at, length, 0.0005, loudClick)
	hpP := DefaultParams()
	hpP.Noise.Method = NoiseHighPass
	dot := cosine(patchOf(t, hpP, clean), patchOf(t, DefaultParams(), clean))
	t.Logf("干净短点击 仅高通 vs 谱减 自相似度 = %.6f", dot)
	if dot < 0.98 {
		t.Fatalf("谱减把干净短点击改得比仅高通更厉害（相似度 %.6f）", dot)
	}
}

// TestNoiseTrackerSkipsNarrowEvent checks the event-aware floor: a 1 kHz beep
// lasting a quarter second must not be written into the 1 kHz noise bin,
// otherwise the next beep would be treated as ambience.
func TestNoiseTrackerSkipsNarrowEvent(t *testing.T) {
	p := DefaultParams()
	a, err := NewAnalyzer(p)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(7))
	const (
		n   = 48000
		amb = 0.08
	)
	pcm := make([]float32, n)
	for i := range pcm {
		pcm[i] = float32((rng.Float64()*2 - 1) * amb * math.Sqrt(3))
		if i >= n/2 && i < n*3/4 {
			pcm[i] += float32(0.18 * math.Sin(2*math.Pi*1000*float64(i)/48000))
		}
	}
	a.Push(pcm)
	if a.noise == nil || !a.noise.noiseReady {
		t.Fatal("噪声估计还没就绪")
	}
	bin := int(math.Round(1000 * float64(p.FrameSize) / float64(p.SampleRate)))
	if bin <= 2 || bin >= len(a.noise.noise)-2 {
		t.Fatalf("1 kHz 频点 %d 超出范围", bin)
	}
	toneN := a.noise.noise[bin]
	var neigh float64
	count := 0
	for _, b := range []int{bin - 4, bin - 3, bin + 3, bin + 4} {
		neigh += a.noise.noise[b]
		count++
	}
	neigh /= float64(count)
	t.Logf("1 kHz 噪声估计 %.3e，邻频 %.3e（比 %.2f 倍）", toneN, neigh, toneN/math.Max(neigh, 1e-18))
	if toneN > neigh*20 {
		t.Fatalf("1 kHz 提示音被学进了底噪（该频点 %.3e，邻频 %.3e）", toneN, neigh)
	}
}

// ---------------------------------------------------------------------------
// pipeline invariants
// ---------------------------------------------------------------------------

// TestNoiseChunkIndependence is the guard for the analyzer's streaming/batch
// identity: the noise front-end must not make the produced frames depend on how
// the caller chunked its audio.
func TestNoiseChunkIndependence(t *testing.T) {
	for _, method := range []string{NoiseSpectral, NoiseHighPass, NoiseOff} {
		t.Run(method, func(t *testing.T) {
			p := paramsWith(method, 0, 0)
			n := 48000
			pcm := burstPCM(n, 9600, 4800, 0.05, sound)

			whole, err := NewAnalyzer(p)
			if err != nil {
				t.Fatal(err)
			}
			whole.Push(pcm)

			chunked, err := NewAnalyzer(p)
			if err != nil {
				t.Fatal(err)
			}
			chunks := []int{137, 1, 4096, 3, 999, 256, 17}
			off, ci := 0, 0
			for off < n {
				c := chunks[ci%len(chunks)]
				ci++
				if off+c > n {
					c = n - off
				}
				// A fresh copy per chunk: also proves Push does not depend on
				// the caller's slice staying alive or unchanged.
				chunk := make([]float32, c)
				copy(chunk, pcm[off:off+c])
				chunked.Push(chunk)
				off += c
			}

			if len(whole.frames) != len(chunked.frames) {
				t.Fatalf("帧数 %d != %d", len(whole.frames), len(chunked.frames))
			}
			worst, at := 0.0, -1
			for i := range whole.frames {
				if d := maxAbsDiffF32(whole.frames[i], chunked.frames[i]); d > worst {
					worst, at = d, i
				}
			}
			if worst != 0 {
				t.Fatalf("第 %d 帧分块与一次性不一致，最大差 %.6g", at, worst)
			}
			if w, c := whole.Window(), chunked.Window(); maxAbsDiffF32(w, c) != 0 {
				t.Fatal("最新窗口在分块与一次性推送下不一致")
			}
			if a, b := whole.NoiseFloorDBFS(), chunked.NoiseFloorDBFS(); a != b {
				t.Fatalf("噪声底在分块与一次性推送下不一致: %v vs %v", a, b)
			}
			if a, b := whole.GateDBFS(-60), chunked.GateDBFS(-60); a != b {
				t.Fatalf("自适应门限在分块与一次性推送下不一致: %v vs %v", a, b)
			}
		})
	}
}

// TestNoiseDoesNotMutateInput guards the "Push never touches the caller's audio"
// contract: the same buffer is handed to more than one analyzer in the offline
// paths, and filtering it in place would silently filter it twice.
func TestNoiseDoesNotMutateInput(t *testing.T) {
	p := DefaultParams()
	pcm := burstPCM(9600, 4800, 1200, 0.05, sound)
	before := make([]float32, len(pcm))
	copy(before, pcm)

	a, err := NewAnalyzer(p)
	if err != nil {
		t.Fatal(err)
	}
	a.Push(pcm)
	for i := range pcm {
		if pcm[i] != before[i] {
			t.Fatalf("Push 修改了调用方的音频：第 %d 个采样 %v -> %v", i, before[i], pcm[i])
		}
	}
}

// TestNoiseHighPassRemovesRumble checks the high-pass stage on its own: a 30 Hz
// rumble must be attenuated while a 2 kHz tone passes through.
func TestNoiseHighPassRemovesRumble(t *testing.T) {
	measure := func(freq float64) float64 {
		p := paramsWith(NoiseHighPass, 0, 0)
		p.Noise.HighPassHz = 120
		a, err := NewAnalyzer(p)
		if err != nil {
			t.Fatal(err)
		}
		n := 48000
		pcm := make([]float32, n)
		for i := range pcm {
			pcm[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/48000))
		}
		a.Push(pcm)
		return a.LevelDBFS()
	}
	rumble, tone := measure(30), measure(2000)
	t.Logf("30 Hz 隆隆声 %.1f dBFS，2 kHz 有用声音 %.1f dBFS", rumble, tone)
	if rumble > tone-20 {
		t.Fatalf("高通没有滤掉 30 Hz 隆隆声（%.1f vs %.1f）", rumble, tone)
	}
}

// ---------------------------------------------------------------------------
// configuration surface
// ---------------------------------------------------------------------------

// TestNoiseParamsValidate covers the configuration surface the settings file and
// the CLI expose.
func TestNoiseParamsValidate(t *testing.T) {
	cases := []struct {
		name string
		p    NoiseParams
		ok   bool
	}{
		{"默认零值", NoiseParams{}, true},
		{"推荐值", DefaultNoiseParams(), true},
		{"关闭", NoiseParams{Method: NoiseOff}, true},
		{"大写拼写被拒", NoiseParams{Method: "OFF"}, false},
		{"小写关闭", NoiseParams{Method: "off"}, true},
		{"未知方式", NoiseParams{Method: "magic"}, false},
		{"过减越界", NoiseParams{Method: NoiseSpectral, OverSubtract: 99}, false},
		{"增益下限越界", NoiseParams{Method: NoiseSpectral, GainFloorDB: 3}, false},
		{"增益下限过低", NoiseParams{Method: NoiseSpectral, GainFloorDB: -99}, false},
		{"干湿比越界", NoiseParams{Method: NoiseHighPass, Mix: 3}, false},
		{"高通越界", NoiseParams{Method: NoiseHighPass, HighPassHz: 40000}, false},
		{"门限余量越界", NoiseParams{Method: NoiseOff, AdaptiveGate: true, GateMarginDB: 99}, false},
	}
	for _, c := range cases {
		err := c.p.Validate()
		if c.ok && err != nil {
			t.Errorf("%s: 期望通过，得到 %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: 期望报错，却通过了", c.name)
		}
	}
}

// TestNoiseFingerprintChanges is the guard that keeps library and index in step:
// changing any noise setting must change the fingerprint, so LoadOrBuild
// rebuilds instead of searching vectors built by a different pipeline.
func TestNoiseFingerprintChanges(t *testing.T) {
	base := DefaultParams()
	seen := map[string]string{base.Fingerprint(): "默认"}
	mutate := map[string]func(p *Params){
		"关闭":     func(p *Params) { p.Noise.Method = NoiseOff },
		"仅高通":    func(p *Params) { p.Noise.Method = NoiseHighPass },
		"截止 200": func(p *Params) { p.Noise.HighPassHz = 200 },
		"过减 1.2": func(p *Params) { p.Noise.OverSubtract = 1.2 },
		"增益下限":   func(p *Params) { p.Noise.GainFloorDB = -30 },
		"干湿 0.7": func(p *Params) { p.Noise.Mix = 0.7 },
		"门限余量":   func(p *Params) { p.Noise.GateMarginDB = 8 },
		"自适应关":   func(p *Params) { p.Noise.AdaptiveGate = false },
	}
	for name, fn := range mutate {
		p := base
		p.Noise = DefaultNoiseParams()
		fn(&p)
		g := p.Fingerprint()
		if prev, dup := seen[g]; dup {
			t.Errorf("%s 与 %s 的指纹相同，参数改动没有被指纹覆盖", name, prev)
		}
		seen[g] = name
	}
	if doc := string(base.ParamJSON()); !strings.Contains(doc, `"noise"`) {
		t.Fatalf("参数文档里没有 noise 段:\n%s", doc)
	}
}
