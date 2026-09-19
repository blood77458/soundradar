package dsp

import (
	"math"
	"runtime"
	"testing"
)

// The tests in this file cover the realtime memory bounding added for the P2
// realtime link (Analyzer.Trim / BufferedSamples / BufferedFrames). They must
// not change any offline behaviour: the trimmed and the untrimmed analyzer have
// to produce bit-identical windows.

// toneStream builds deterministic broadband-ish audio (two tones + a little
// noise-free amplitude modulation) of n samples at 48 kHz.
func toneStream(n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		t := float64(i) / 48000
		v := 0.35*math.Sin(2*math.Pi*880*t) + 0.25*math.Sin(2*math.Pi*1500*t+0.7)
		v *= 0.6 + 0.4*math.Sin(2*math.Pi*3*t)
		out[i] = float32(v)
	}
	return out
}

// TestTrimKeepsWindowsBitIdentical walks the same audio twice - once keeping
// the whole history, once calling Trim after every hop - and requires every
// Window() to be exactly equal.
func TestTrimKeepsWindowsBitIdentical(t *testing.T) {
	p := DefaultParams()
	pcm := toneStream(48000) // 1 s

	full, err := NewAnalyzer(p)
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}
	trimmed, err := NewAnalyzer(p)
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}

	step := p.HopSize
	compared := 0
	for off := 0; off < len(pcm); off += step {
		end := off + step
		if end > len(pcm) {
			end = len(pcm)
		}
		full.Push(pcm[off:end])
		trimmed.Push(pcm[off:end])
		trimmed.Trim(2 * p.WindowFrames)

		wf, wt := full.Window(), trimmed.Window()
		if (wf == nil) != (wt == nil) {
			t.Fatalf("offset %d: Window() 可用性不同 (full=%v trimmed=%v)", off, wf != nil, wt != nil)
		}
		if wf == nil {
			continue
		}
		if len(wf) != len(wt) {
			t.Fatalf("维数不同: %d != %d", len(wf), len(wt))
		}
		for i := range wf {
			if wf[i] != wt[i] {
				t.Fatalf("offset %d: 第 %d 维不同: %v != %v", off, i, wf[i], wt[i])
			}
		}
		compared++
	}
	if compared < 140 {
		t.Fatalf("只比较了 %d 个窗口，测试没真正跑起来", compared)
	}

	// FrameCount() is absolute: trimming must not reset it.
	if full.FrameCount() != trimmed.FrameCount() {
		t.Fatalf("FrameCount: full=%d trimmed=%d", full.FrameCount(), trimmed.FrameCount())
	}
	// Trimmed indices that fell out of the history are gone (nil), not stale.
	if got := trimmed.WindowAt(p.WindowFrames - 1); got != nil {
		t.Fatalf("被裁掉的 WindowAt(%d) 应返回 nil，实际 %d 维", p.WindowFrames-1, len(got))
	}
	if full.WindowAt(p.WindowFrames-1) == nil {
		t.Fatal("未裁剪的 WindowAt(WindowFrames-1) 不应为 nil")
	}
	t.Logf("比较了 %d 个窗口，全部逐元素相同；FrameCount=%d，裁剪后保留 %d 帧 / %d 采样",
		compared, trimmed.FrameCount(), trimmed.BufferedFrames(), trimmed.BufferedSamples())
}

// TestTrimBoundsRetention is the direct, non-flaky memory assertion: after
// minutes of audio the internal buffers must not grow at all.
func TestTrimBoundsRetention(t *testing.T) {
	p := DefaultParams()
	a, err := NewAnalyzer(p)
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}

	block := make([]float32, 960) // 20 ms
	const minutes = 2
	blocks := minutes * 60 * 50 // 50 blocks per second of audio
	firstMinute := 60 * 50

	var heapMin1, heapMin2 uint64
	for i := 0; i < blocks; i++ {
		for j := range block {
			block[j] = float32(0.3 * math.Sin(2*math.Pi*880*float64(i*len(block)+j)/48000))
		}
		a.Push(block)
		a.Trim(2 * p.WindowFrames)
		if i == firstMinute-1 {
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			heapMin1 = ms.HeapInuse
		}
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	heapMin2 = ms.HeapInuse

	samplesPushed := blocks * len(block)
	t.Logf("灌入 %d 分钟音频（%d 采样 = %.1f s）", minutes, samplesPushed, float64(samplesPushed)/48000)
	t.Logf("保留缓冲区: 采样 %d（%.1f KiB）/ 帧 %d", a.BufferedSamples(),
		float64(a.BufferedSamples())*4/1024, a.BufferedFrames())
	t.Logf("HeapInuse: 第 1 分钟 %d KiB -> 第 %d 分钟 %d KiB（差 %d KiB）",
		heapMin1/1024, minutes, heapMin2/1024, int64(heapMin2-heapMin1)/1024)

	if maxSamples := p.FrameSize + p.HopSize; a.BufferedSamples() > maxSamples {
		t.Fatalf("保留采样数 %d 超过上界 %d：内存随时间增长", a.BufferedSamples(), maxSamples)
	}
	if maxFrames := 2 * p.WindowFrames; a.BufferedFrames() > maxFrames {
		t.Fatalf("保留帧数 %d 超过上界 %d", a.BufferedFrames(), maxFrames)
	}
	// Go's allocator keeps freed spans, so this is a loose sanity bound rather
	// than an exact equality: a leak of 192 kB/s would be ~23 MiB over the
	// second minute and would blow straight through it.
	if heapMin2 > heapMin1+8<<20 {
		t.Fatalf("HeapInuse 从 %d 涨到 %d（>8 MiB），内存未随裁剪收敛", heapMin1, heapMin2)
	}
}

// TestUntrimmedRetentionGrows is the control experiment that justifies Trim
// existing: without it the sample ring grows linearly and forever.
func TestUntrimmedRetentionGrows(t *testing.T) {
	p := DefaultParams()
	a, err := NewAnalyzer(p)
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}
	block := make([]float32, 960)
	blocksPerMinute := 60 * 50
	for i := 0; i < 2*blocksPerMinute; i++ {
		a.Push(block) // digital silence: Push stores it all the same
	}
	if a.BufferedSamples() != 2*blocksPerMinute*len(block) {
		t.Fatalf("未裁剪时应保留全部 %d 个采样，实际 %d",
			2*blocksPerMinute*len(block), a.BufferedSamples())
	}
	mbPerMinute := float64(blocksPerMinute*len(block)*4) / (1 << 20)
	t.Logf("未裁剪: 2 分钟保留 %d 采样 = %.1f MiB（约 %.0f MiB/小时）——这就是 Trim 要解决的问题",
		a.BufferedSamples(), float64(a.BufferedSamples())*4/(1<<20), mbPerMinute*60)

	// ReleaseFrames() alone (the P2 API) drops frames but not samples, and it
	// resets the frame counter the same way it always did.
	a.ReleaseFrames()
	if a.BufferedSamples() == 0 {
		t.Fatal("ReleaseFrames 不应释放采样缓冲（这正是它不够用的原因）")
	}
	if a.FrameCount() != 0 || a.Ready() {
		t.Fatalf("ReleaseFrames 之后 FrameCount/Ready 行为变了: %d/%v", a.FrameCount(), a.Ready())
	}
}

// TestTrimIsNoOpOnShortHistory guards the documented "no-op" promise and the
// keepFrames lower bound.
func TestTrimIsNoOpOnShortHistory(t *testing.T) {
	p := DefaultParams()
	a, _ := NewAnalyzer(p)
	a.Push(toneStream(1000))
	before := a.BufferedSamples()
	frames := a.BufferedFrames()
	a.Trim(0) // raised to WindowFrames
	if a.BufferedFrames() != frames {
		t.Fatalf("历史不足时 Trim 不应丢帧: %d -> %d", frames, a.BufferedFrames())
	}
	if a.BufferedSamples() > before {
		t.Fatalf("Trim 不应增加缓冲: %d -> %d", before, a.BufferedSamples())
	}
	// A trim with a large keep also must not break the analyzer.
	a.Push(toneStream(20000))
	a.Trim(1 << 20)
	if !a.Ready() {
		t.Fatal("Trim 之后应当仍然 Ready")
	}
	if w := a.Window(); w == nil || len(w) != p.Dim() {
		t.Fatalf("Trim 之后 Window 无效: %v", w == nil)
	}
}
