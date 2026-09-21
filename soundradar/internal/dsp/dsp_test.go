package dsp

import (
	"math"
	"math/cmplx"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// naiveDFT is the O(N^2) reference transform. It shares the forward sign
// convention (e^{-iwn}) with fft.forward and is deliberately independent of the
// radix-2 code path so the two disagree if the plan is wrong.
func naiveDFT(x []complex128) []complex128 {
	n := len(x)
	out := make([]complex128, n)
	for k := 0; k < n; k++ {
		var acc complex128
		for t := 0; t < n; t++ {
			acc += x[t] * cmplx.Exp(complex(0, -2*math.Pi*float64(k*t)/float64(n)))
		}
		out[k] = acc
	}
	return out
}

// maxAbsDiff returns the largest |a-b| over equal-length slices.
func maxAbsDiff(a, b []complex128) float64 {
	worst := 0.0
	for i := range a {
		if d := cmplx.Abs(a[i] - b[i]); d > worst {
			worst = d
		}
	}
	return worst
}

// lcg is a tiny deterministic PRNG (no dependence on math/rand's stream
// stability across Go versions, so the reported numbers are reproducible).
type lcg struct{ s uint64 }

func newLCG(seed uint64) *lcg { return &lcg{s: seed*6364136223846793005 + 1442695040888963407} }

func (r *lcg) next() float64 {
	r.s = r.s*6364136223846793005 + 1442695040888963407
	// top 53 bits -> [0,1)
	return float64(r.s>>11) / float64(1<<53)
}

// sine builds a float32 sine at freq Hz, amplitude amp, over n samples.
func sine(n int, rate int, freq, amp float64) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return out
}

// maxAbsDiffF32 returns the largest |a-b| over equal-length float slices.
func maxAbsDiffF32(a, b []float32) float64 {
	worst := 0.0
	for i := range a {
		if d := math.Abs(float64(a[i]) - float64(b[i])); d > worst {
			worst = d
		}
	}
	return worst
}

// ---------------------------------------------------------------------------
// A1 - FFT correctness vs naive DFT
// ---------------------------------------------------------------------------

func TestFFTMatchesNaiveDFT(t *testing.T) {
	const n = 1024
	plan := newFFT(n)
	rng := newLCG(1)

	// (a) random complex input
	rand := make([]complex128, n)
	for i := range rand {
		rand[i] = complex(rng.next()*2-1, rng.next()*2-1)
	}
	got := make([]complex128, n)
	copy(got, rand)
	plan.forward(got)
	want := naiveDFT(rand)
	worstRandom := maxAbsDiff(got, want)

	// (b) impulse: delta[n] -> flat spectrum of ones
	imp := make([]complex128, n)
	imp[0] = 1
	gotImp := make([]complex128, n)
	copy(gotImp, imp)
	plan.forward(gotImp)
	wantImp := naiveDFT(imp)
	worstImpulse := maxAbsDiff(gotImp, wantImp)

	// (c) single tone
	tone := make([]complex128, n)
	for i := range tone {
		tone[i] = complex(math.Sin(2*math.Pi*880*float64(i)/48000), 0)
	}
	gotTone := make([]complex128, n)
	copy(gotTone, tone)
	plan.forward(gotTone)
	wantTone := naiveDFT(tone)
	worstTone := maxAbsDiff(gotTone, wantTone)

	worst := math.Max(worstRandom, math.Max(worstImpulse, worstTone))
	t.Logf("FFT(n=%d) vs 朴素 DFT 最大绝对误差: 随机=%.3e 冲激=%.3e 单频=%.3e -> 最大 %.3e", n, worstRandom, worstImpulse, worstTone, worst)
	if worst >= 1e-9 {
		t.Fatalf("FFT 与朴素 DFT 最大误差 %.3e 超过 1e-9", worst)
	}

	// inverse round trip (uses the same butterfly plan with the inverse twiddles)
	back := make([]complex128, n)
	copy(back, got)
	plan.inverse(back)
	rt := maxAbsDiff(back, rand)
	t.Logf("FFT 正向+逆向往返最大误差: %.3e", rt)
	if rt >= 1e-9 {
		t.Fatalf("FFT 往返误差 %.3e 超过 1e-9", rt)
	}
}

func TestFFTPlanRejectsNonPowerOfTwo(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("newFFT(1000) 应当 panic")
		}
	}()
	newFFT(1000)
}

// ---------------------------------------------------------------------------
// A2 - Parseval
// ---------------------------------------------------------------------------

func TestParseval(t *testing.T) {
	p := DefaultParams()
	frame := sine(p.FrameSize, p.SampleRate, 1500, 0.5)
	// window + de-mean, exactly like the analyzer does
	var sum float32
	for _, v := range frame {
		sum += v
	}
	mean := sum / float32(len(frame))
	var timeEnergy float64
	w := make([]float32, len(frame))
	hannPeriodic(w)
	for i, v := range frame {
		d := float64(v-mean) * float64(w[i])
		w[i] = float32(d)
		timeEnergy += d * d
	}

	pow, err := powerSpectrum(w)
	if err != nil {
		t.Fatalf("powerSpectrum: %v", err)
	}
	var freqEnergy float64
	for _, v := range pow {
		freqEnergy += v
	}
	freqEnergy /= float64(p.FrameSize) // Parseval: sum|x|^2 = (1/N) sum|X|^2

	rel := math.Abs(freqEnergy-timeEnergy) / timeEnergy
	t.Logf("Parseval: 时域能量=%.9e 频域能量=%.9e 相对误差=%.3e", timeEnergy, freqEnergy, rel)
	if rel >= 1e-6 {
		t.Fatalf("Parseval 相对误差 %.3e 超过 1e-6", rel)
	}
}

// ---------------------------------------------------------------------------
// A3 - frequency localisation
// ---------------------------------------------------------------------------

func TestFrequencyLocalisation(t *testing.T) {
	p := DefaultParams()
	binHz := float64(p.SampleRate) / float64(p.FrameSize)
	for _, freq := range []float64{880, 1000, 4000, 100} {
		frame := sine(p.FrameSize, p.SampleRate, freq, 0.8)
		pow, err := powerSpectrum(frame)
		if err != nil {
			t.Fatalf("powerSpectrum: %v", err)
		}
		best := 1
		for k := 2; k < len(pow)-1; k++ {
			if pow[k] > pow[best] {
				best = k
			}
		}
		peakHz := float64(best) * binHz
		diffBins := math.Abs(peakHz-freq) / binHz
		t.Logf("%.1f Hz -> 峰值 bin %d = %.3f Hz (bin 宽 %.3f Hz, 偏差 %.2f 个 bin)",
			freq, best, peakHz, binHz, diffBins)
		if diffBins > 1 {
			t.Fatalf("%.1f Hz 的峰值落在 %.3f Hz，偏差 %.2f 个 bin (>1)", freq, peakHz, diffBins)
		}
	}
}

// ---------------------------------------------------------------------------
// A4 - mel filter bank
// ---------------------------------------------------------------------------

func TestMelFilterBank(t *testing.T) {
	p := DefaultParams()
	mb, err := NewMelBank(p)
	if err != nil {
		t.Fatalf("NewMelBank: %v", err)
	}
	if mb.Bands != p.MelBands {
		t.Fatalf("滤波器数量 = %d，期望 %d", mb.Bands, p.MelBands)
	}
	if mb.Bins != p.FrameSize/2+1 {
		t.Fatalf("频点数 = %d，期望 %d", mb.Bins, p.FrameSize/2+1)
	}

	// centres strictly increasing and inside [FMin, FMax]
	for c := 1; c < mb.Bands; c++ {
		if mb.CentersHz[c] <= mb.CentersHz[c-1] {
			t.Fatalf("第 %d 个中心频率 %.3f Hz 没有大于前一个 %.3f Hz", c, mb.CentersHz[c], mb.CentersHz[c-1])
		}
	}
	if mb.CentersHz[0] < float64(p.FMinHz) || mb.CentersHz[mb.Bands-1] > float64(p.FMaxHz) {
		t.Fatalf("中心频率范围 [%.2f, %.2f] 超出 [%d, %d]",
			mb.CentersHz[0], mb.CentersHz[mb.Bands-1], p.FMinHz, p.FMaxHz)
	}
	// every filter must actually cover at least one FFT bin, and every weight
	// must be non-negative with a real peak
	for c := 0; c < mb.Bands; c++ {
		nz, peak := 0, 0.0
		for b := 0; b < mb.Bins; b++ {
			w := mb.Weight(c, b)
			if w < 0 {
				t.Fatalf("滤波器 %d 在 bin %d 出现负权重 %v", c, b, w)
			}
			if w > 0 {
				nz++
			}
			if w > peak {
				peak = w
			}
		}
		if nz == 0 || peak <= 0 {
			t.Fatalf("滤波器 %d 没有任何非零频点（空滤波器）", c)
		}
	}
	t.Logf("Mel 滤波器组: %d 个三角滤波器, %d 个频点, 中心 %.1f..%.1f Hz 单调递增",
		mb.Bands, mb.Bins, mb.CentersHz[0], mb.CentersHz[mb.Bands-1])

	// mel <-> Hz round trip
	for _, f := range []float64{40, 100, 1000, 4000, 16000} {
		if back := melToHz(hzToMel(f)); math.Abs(back-f) > 1e-9 {
			t.Fatalf("mel 往返 %.3f Hz -> %.9f Hz", f, back)
		}
	}

	// a pure tone must peak in the band that contains its frequency, and the
	// band index must be ordered the same way as the frequency
	bandOf := func(freq float64) (int, float64) {
		frame := sine(p.FrameSize, p.SampleRate, freq, 0.8)
		pow, err := powerSpectrum(frame)
		if err != nil {
			t.Fatalf("powerSpectrum: %v", err)
		}
		best, bestE := 0, -1.0
		for c := 0; c < mb.Bands; c++ {
			var e float64
			for b := 0; b < mb.Bins; b++ {
				e += mb.Weight(c, b) * pow[b]
			}
			if e > bestE {
				best, bestE = c, e
			}
		}
		return best, bestE
	}
	b1k, _ := bandOf(1000)
	b4k, _ := bandOf(4000)
	b880, _ := bandOf(880)
	t.Logf("纯音 mel 峰值频带: 880 Hz -> band %d, 1 kHz -> band %d, 4 kHz -> band %d", b880, b1k, b4k)
	if !(b1k < b4k) {
		t.Fatalf("1 kHz 落在 band %d，4 kHz 落在 band %d，频带顺序不对", b1k, b4k)
	}
	if b880 != b1k {
		t.Logf("注意: 880 Hz 落在 band %d，1 kHz 落在 band %d（两者相差 %.1f Hz，低端一个 mel 频带只有约 %.1f Hz 宽）",
			b880, b1k, 1000.0-880.0, mb.CentersHz[1]-mb.CentersHz[0])
	}
	// The peak band of a tone should be at (or extremely close to) the band
	// whose centre is nearest. With 64 bands over 40..16000 Hz the low-frequency
	// mel bands are only ~10 Hz wide, i.e. narrower than one 46.875 Hz FFT bin,
	// so a single bin can excite several bands and an exact match cannot be
	// required. The tolerance is therefore one FFT bin expressed in bands.
	for _, tc := range []struct {
		freq float64
		band int
	}{{1000, b1k}, {4000, b4k}} {
		bestC, bestD := -1, math.MaxFloat64
		for c := 0; c < mb.Bands; c++ {
			if d := math.Abs(mb.CentersHz[c] - tc.freq); d < bestD {
				bestC, bestD = c, d
			}
		}
		off := tc.band - bestC
		if off < 0 {
			off = -off
		}
		if off > 3 {
			t.Fatalf("%.0f Hz 的峰值 band = %d，最近中心 %.1f Hz 在 band %d，相差 %d 个频带（容忍 3）",
				tc.freq, tc.band, mb.CentersHz[bestC], bestC, off)
		}
		t.Logf("%.0f Hz: 峰值 band %d, 最近中心 band %d (%.1f Hz), 相差 %d 个频带",
			tc.freq, tc.band, bestC, mb.CentersHz[bestC], off)
	}
	if !(b1k > 0 && b4k > 0) {
		t.Fatal("mel 峰值频带必须远离直流")
	}
}

func TestParamsValidateAndFingerprint(t *testing.T) {
	p := DefaultParams()
	if err := p.Validate(); err != nil {
		t.Fatalf("DefaultParams 校验失败: %v", err)
	}
	if p.Dim() != 2048 {
		t.Fatalf("Dim() = %d，期望 2048", p.Dim())
	}
	fp := p.Fingerprint()
	t.Logf("特征参数指纹: %s", fp)

	// deterministic
	if p.Fingerprint() != fp {
		t.Fatal("同一份参数两次 Fingerprint 不一致")
	}
	// parameter change -> different fingerprint
	q := p
	q.HopSize = 128
	if q.Fingerprint() == fp {
		t.Fatal("改掉 HopSize 后指纹没有变化")
	}
	r := p
	r.WindowFrames = 16
	if r.Fingerprint() == fp {
		t.Fatal("改掉 WindowFrames 后指纹没有变化")
	}
	// zero/nonsense struct must differ too (guards against a stale index)
	if (Params{}).Fingerprint() == fp {
		t.Fatal("空参数指纹与默认参数相同")
	}

	// invalid combinations
	bad := []Params{
		{SampleRate: 0, FrameSize: 1024, HopSize: 256, MelBands: 64, FMaxHz: 16000, WindowFrames: 32},
		{SampleRate: 48000, FrameSize: 1000, HopSize: 256, MelBands: 64, FMaxHz: 16000, WindowFrames: 32},
		{SampleRate: 48000, FrameSize: 1024, HopSize: 2048, MelBands: 64, FMaxHz: 16000, WindowFrames: 32},
		{SampleRate: 48000, FrameSize: 1024, HopSize: 256, MelBands: 64, FMinHz: 16000, FMaxHz: 40, WindowFrames: 32},
		{SampleRate: 48000, FrameSize: 1024, HopSize: 256, MelBands: 64, FMaxHz: 30000, WindowFrames: 32},
		{SampleRate: 48000, FrameSize: 1024, HopSize: 256, MelBands: 64, FMaxHz: 16000, WindowFrames: 0},
	}
	for i, b := range bad {
		if err := b.Validate(); err == nil {
			t.Fatalf("第 %d 组非法参数没有被 Validate 拒绝: %+v", i, b)
		}
	}
	if _, err := NewAnalyzer(bad[1]); err == nil {
		t.Fatal("NewAnalyzer 应当拒绝非 2 的幂帧长")
	}
}

// ---------------------------------------------------------------------------
// A5 - Normalize
// ---------------------------------------------------------------------------

func TestNormalize(t *testing.T) {
	rng := newLCG(7)
	patch := make([]float32, 2048)
	for i := range patch {
		patch[i] = float32(rng.next()*200 - 100) // a wide, non-zero-mean range
	}
	orig := append([]float32(nil), patch...)
	out := Normalize(patch)

	// Normalize must not modify the input.
	if maxAbsDiffF32(patch, orig) != 0 {
		t.Fatal("Normalize 修改了输入切片")
	}

	var sum float64
	for _, v := range out {
		sum += float64(v)
	}
	mean := sum / float64(len(out))
	nrm := norm32(out)
	t.Logf("Normalize: L2 范数=%.12f 均值=%.3e (输入均值=%.3f)", nrm, mean, meanOf(orig))

	if math.Abs(nrm-1) >= 1e-6 {
		t.Fatalf("L2 范数 %.12f 偏离 1 超过 1e-6", nrm)
	}
	if math.Abs(mean) >= 1e-6 {
		t.Fatalf("均值 %.3e 偏离 0 超过 1e-6", mean)
	}

	// a pure gain change in the linear domain becomes a constant offset in the
	// log-mel domain, which the de-mean removes, so two scaled copies of the
	// same patch must be identical after Norm. Only float32 rounding limits the
	// agreement (a 12-bit window over int16-sourced data is already exact).
	lifted := make([]float32, len(orig))
	for i, v := range orig {
		lifted[i] = v + 6 // +6 in log-mel == x4 linear
	}
	n2 := Normalize(lifted)
	diff := maxAbsDiffF32(out, n2)
	t.Logf("Normalize 增益不变性(+6 dB log-mel): 最大逐元素差=%.3e", diff)
	if diff >= 1e-5 {
		t.Fatalf("常数增益没有被归一化消除，最大差 %.3e", diff)
	}

	// degenerate input must not produce NaN
	zero := Normalize(make([]float32, 2048))
	if got := norm32(zero); got != 0 {
		t.Fatalf("全零 patch 归一化后范数 = %v，期望 0", got)
	}
	for i, v := range zero {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			t.Fatalf("全零 patch 归一化后第 %d 项 = %v", i, v)
		}
	}
	if Normalize(nil) != nil {
		t.Fatal("Normalize(nil) 应为 nil")
	}
}

func meanOf(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x)
	}
	return s / float64(len(v))
}

// TestQuietToneKeepsSpectralShape is the regression for loopback clips around
// -40 dBFS: an absolute log floor flattened them to a constant frame, and the
// item could never score above 0.
func TestQuietToneKeepsSpectralShape(t *testing.T) {
	p := DefaultParams()
	amp := math.Pow(10, -40.0/20)
	pcm := sine(p.SampleRate, p.SampleRate, 880, amp)
	frames := p.Frames(pcm)
	if len(frames) == 0 {
		t.Fatal("没有产出帧")
	}
	varied := false
	for _, f := range frames {
		lo, hi := float64(f[0]), float64(f[0])
		for _, v := range f[1:] {
			x := float64(v)
			if x < lo {
				lo = x
			}
			if x > hi {
				hi = x
			}
		}
		if hi-lo > 5 { // more than 5 dB of shape inside one frame
			varied = true
			break
		}
	}
	if !varied {
		t.Fatalf("-40 dBFS 的 880 Hz 被压成了平的 log-mel（%d 帧）", len(frames))
	}
}

// ---------------------------------------------------------------------------
// A6 - streaming vs batch
// ---------------------------------------------------------------------------

func TestStreamingMatchesBatch(t *testing.T) {
	p := DefaultParams()
	const n = 48000 // 1 s
	rng := newLCG(42)
	pcm := make([]float32, n)
	for i := range pcm {
		// tones + noise so every mel band has something to chew on
		pcm[i] = float32(0.4*math.Sin(2*math.Pi*880*float64(i)/48000) +
			0.2*math.Sin(2*math.Pi*3000*float64(i)/48000) +
			0.05*(rng.next()*2-1))
	}

	batch, err := NewAnalyzer(p)
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}
	batch.Push(pcm)
	if got, want := batch.FrameCount(), p.FrameCount(n); got != want {
		t.Fatalf("一次性推送得到 %d 帧，期望 %d 帧", got, want)
	}

	// Push in awkward, non-aligned chunks: 137, 1, 4096, 3, 999 ...
	chunks := []int{137, 1, 4096, 3, 999, 256, 17}
	stream, err := NewAnalyzer(p)
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}
	off, ci := 0, 0
	worstFrame := 0.0
	checked := 0
	for off < n {
		c := chunks[ci%len(chunks)]
		ci++
		if off+c > n {
			c = n - off
		}
		stream.Push(pcm[off : off+c])
		off += c

		// After every chunk the streaming windows must equal the batch windows
		// for every frame the stream has available at that moment (the newest
		// one, the oldest reachable one, and a sample in between).
		if !stream.Ready() {
			continue
		}
		last := stream.FrameCount() - 1
		for _, end := range []int{last, last - 1, last - 7, p.WindowFrames - 1} {
			if end < p.WindowFrames-1 || end > last {
				continue
			}
			sw := stream.WindowAt(end)
			bw := batch.WindowAt(end)
			if sw == nil || bw == nil {
				t.Fatalf("WindowAt(%d) 返回 nil (stream=%v batch=%v)", end, sw == nil, bw == nil)
			}
			if d := maxAbsDiffF32(sw, bw); d > worstFrame {
				worstFrame = d
			}
			checked++
		}
	}
	if stream.FrameCount() != batch.FrameCount() {
		t.Fatalf("分块推送得到 %d 帧，一次性推送得到 %d 帧", stream.FrameCount(), batch.FrameCount())
	}
	// Window() == WindowAt(FrameCount-1)
	if d := maxAbsDiffF32(stream.Window(), batch.Window()); d > worstFrame {
		worstFrame = d
	}
	checked++
	t.Logf("流式(分块 %v) vs 一次性: 比较了 %d 个窗口, 最大逐元素差 = %.3e (帧数 %d)",
		chunks, checked, worstFrame, batch.FrameCount())
	if worstFrame != 0 {
		t.Fatalf("流式与一次性推送的窗口不一致，最大逐元素差 %.3e", worstFrame)
	}

	// The normalised window must be a unit vector with zero mean.
	w := batch.Window()
	var s float64
	for _, v := range w {
		s += float64(v)
	}
	t.Logf("窗口: 维数=%d L2 范数=%.12f 均值=%.3e", len(w), norm32(w), s/float64(len(w)))
	if len(w) != p.Dim() {
		t.Fatalf("窗口维数 %d，期望 %d", len(w), p.Dim())
	}
	if math.Abs(norm32(w)-1) >= 1e-6 {
		t.Fatalf("窗口范数 %.12f 不是 1", norm32(w))
	}
	if math.Abs(s/float64(len(w))) >= 1e-6 {
		t.Fatalf("窗口均值 %.3e 不是 0", s/float64(len(w)))
	}
}

func TestReadyAndReset(t *testing.T) {
	p := DefaultParams()
	a, err := NewAnalyzer(p)
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}
	if a.Ready() || a.Window() != nil {
		t.Fatal("没有数据时不应 Ready")
	}
	// one sample short of a full window (31 frames need 1024+30*256 samples)
	need := p.FrameSize + (p.WindowFrames-1)*p.HopSize
	a.Push(make([]float32, need-1))
	if a.Ready() {
		t.Fatalf("只有 %d 个采样时不应 Ready（需要 %d）", need-1, need)
	}
	a.Push(make([]float32, 1))
	if !a.Ready() {
		t.Fatalf("推满 %d 个采样后应当 Ready", need)
	}
	if a.Window() == nil {
		t.Fatal("Ready 后 Window 不应为 nil")
	}
	a.Reset()
	if a.Ready() || a.FrameCount() != 0 || a.Window() != nil {
		t.Fatal("Reset 之后状态没有清空")
	}
}

func TestLevelDBFS(t *testing.T) {
	p := DefaultParams()
	a, _ := NewAnalyzer(p)
	if lv := a.LevelDBFS(); !math.IsInf(lv, -1) {
		t.Fatalf("空分析器的电平 = %v，期望 -Inf", lv)
	}
	a.Reset()
	a.Push(make([]float32, 4800))
	if lv := a.LevelDBFS(); !math.IsInf(lv, -1) {
		t.Fatalf("纯数字静音的电平 = %v，期望 -Inf", lv)
	}
	a.Reset()
	sig := sine(LevelWindowSamples, p.SampleRate, 1000, 0.5)
	a.Push(sig)
	// RMS of a 0.5-amplitude sine = 0.5/sqrt(2) = -9.03 dBFS.
	//
	// The tolerance is 0.15 dB rather than 0.05 dB because the level meter now
	// measures the audio AFTER the environment-noise front-end (noise.go), whose
	// 120 Hz high-pass stage takes a fraction of a dB out of a 1 kHz tone. The
	// meter and the realtime silence gate have to see the same signal, so
	// measuring the filtered audio is the consistent choice.
	want := 20 * math.Log10(0.5/math.Sqrt2)
	got := a.LevelDBFS()
	t.Logf("LevelDBFS: 0.5 幅度正弦 = %.4f dBFS (理论 %.4f, 差 %.2e)", got, want, math.Abs(got-want))
	if math.Abs(got-want) > 0.15 {
		t.Fatalf("电平等级 %.4f 与理论 %.4f 相差过大", got, want)
	}
}

func TestWindowLevelHoldsShortClick(t *testing.T) {
	p := DefaultParams()
	a, err := NewAnalyzer(p)
	if err != nil {
		t.Fatal(err)
	}
	// A short click sits in the middle of a quiet bed. By the time 120 ms of
	// bed has followed it, the 50 ms meter has forgotten the click, but the
	// 187 ms feature window still contains it.
	const sr = 48000
	bed := sine(int(0.30*sr), sr, 1000, dbAmp(-54))
	click := sine(int(0.02*sr), sr, 1000, dbAmp(-28))
	tail := sine(int(0.12*sr), sr, 1000, dbAmp(-54))
	a.Push(bed)
	a.Push(click)
	a.Push(tail)
	lv, win, gate := a.LevelDBFS(), a.WindowLevelDBFS(), a.GateDBFS(-60)
	t.Logf("level %.1f window %.1f gate %.1f", lv, win, gate)
	if lv >= gate {
		t.Fatalf("尾部电平 %.1f 仍在门限 %.1f 之上，这个用例没有复现短促声被漏计", lv, gate)
	}
	if win <= gate {
		t.Fatalf("特征窗电平 %.1f 没有留在门限 %.1f 之上", win, gate)
	}
	if win < lv+10 {
		t.Fatalf("特征窗电平 %.1f 应当明显高于尾部电平 %.1f", win, lv)
	}
}

func dbAmp(dbfs float64) float64 {
	return math.Pow(10, dbfs/20) * math.Sqrt2
}

func TestFramesOffline(t *testing.T) {
	p := DefaultParams()
	const n = 24000 // 0.5 s, not a whole number of hops
	pcm := sine(n, p.SampleRate, 880, 0.7)
	frames := p.Frames(pcm)
	want := p.FrameCount(n)
	if len(frames) != want {
		t.Fatalf("Frames 返回 %d 帧，期望 %d 帧", len(frames), want)
	}
	for i, f := range frames {
		if len(f) != p.MelBands {
			t.Fatalf("第 %d 帧维数 %d，期望 %d", i, len(f), p.MelBands)
		}
		for j, v := range f {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				t.Fatalf("第 %d 帧第 %d 维 = %v", i, j, v)
			}
		}
	}
	t.Logf("Frames: %d 个采样 -> %d 帧 x %d 维 (帧移 %.3f ms, 补尾帧后共 %.3f s)",
		n, len(frames), p.MelBands, p.HopDurationS()*1000, float64(len(frames))*p.HopDurationS())

	// too short -> no frames
	if got := p.Frames(make([]float32, p.FrameSize-1)); len(got) != 0 {
		t.Fatalf("过短信号应产生 0 帧，得到 %d", len(got))
	}

	// The batch path and the streaming path must agree frame for frame.
	a, _ := NewAnalyzer(p)
	a.Push(pcm)
	a.flush()
	if len(a.frames) != len(frames) {
		t.Fatalf("流式 %d 帧 vs Frames %d 帧", len(a.frames), len(frames))
	}
	for i := range frames {
		if d := maxAbsDiffF32(frames[i], a.frames[i]); d != 0 {
			t.Fatalf("第 %d 帧不一致，最大差 %.3e", i, d)
		}
	}
}

func TestSpectrumLengthMismatchPanics(t *testing.T) {
	plan := newFFT(64)
	defer func() {
		if recover() == nil {
			t.Fatal("长度不匹配应当 panic")
		}
	}()
	plan.spectrum(make([]float64, 4), make([]float32, 64))
}
