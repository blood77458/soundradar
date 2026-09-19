package live

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/match"
	"github.com/znz/soundradar/internal/wav"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// tone returns n samples of a sine at freq Hz.
func tone(n int, freq, amp float64) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(amp * math.Sin(2*math.Pi*freq*float64(i)/SampleRate))
	}
	return out
}

// sawtooth returns n samples of a band-unlimited sawtooth at freq Hz.
func sawtooth(n int, freq, amp float64) []float32 {
	out := make([]float32, n)
	for i := range out {
		p := float64(i) * freq / SampleRate
		out[i] = float32(amp * 2 * (p - math.Floor(p+0.5)))
	}
	return out
}

// noise returns n samples of deterministic white noise (xorshift), so the test
// is reproducible.
func noise(n int, amp float64) []float32 {
	out := make([]float32, n)
	s := uint32(0x12345678)
	for i := range out {
		s ^= s << 13
		s ^= s >> 17
		s ^= s << 5
		out[i] = float32(amp * (float64(s)/float64(1<<32)*2 - 1))
	}
	return out
}

func wavBytes(t *testing.T, pcm []float32) []byte {
	t.Helper()
	i16 := make([]int16, len(pcm))
	wav.Float32ToInt16(pcm, i16)
	var buf strings.Builder
	_ = buf
	return marshalWAV(t, i16)
}

func marshalWAV(t *testing.T, pcm []int16) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tmp.wav")
	if err := wav.WriteInt16File(path, SampleRate, 1, pcm); err != nil {
		t.Fatalf("WriteInt16File: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixture builds a temporary library holding three sounds and its index, and
// returns the library path, the index path and the index itself.
func fixture(t *testing.T) (libPath, idxPath string, ix *index.Index) {
	t.Helper()
	dir := t.TempDir()
	libPath = filepath.Join(dir, "library.srz")

	type item struct {
		name string
		pcm  []float32
	}
	items := []item{
		{"880Hz 测试音", tone(SampleRate/2, 880, 0.6)},
		{"1500Hz 测试音", tone(SampleRate/2, 1500, 0.6)},
		{"白噪突发", noise(SampleRate/2, 0.4)},
	}
	store := library.New(libPath, "live 测试库")
	for _, it := range items {
		i16 := make([]int16, len(it.pcm))
		wav.Float32ToInt16(it.pcm, i16)
		li := &library.Item{
			Name:       it.name,
			Threshold:  match.DefaultOptions().DefaultThreshold,
			CooldownMs: 200,
			Samples:    []library.Sample{{File: library.SampleFile(0), Source: "generated"}},
			Tags:       []string{"test"},
		}
		if err := store.AddItem(li, nil, [][]byte{marshalWAV(t, i16)}); err != nil {
			t.Fatalf("AddItem(%s): %v", it.name, err)
		}
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	idxPath = filepath.Join(dir, "index.bin")
	built, rebuilt, why, err := index.LoadOrBuild(libPath, idxPath, dsp.DefaultParams())
	if err != nil {
		t.Fatalf("LoadOrBuild: %v", err)
	}
	if !rebuilt {
		t.Fatalf("索引应当是新建的，why=%q", why)
	}
	itemsN, samples, bytes := built.Stats()
	t.Logf("测试索引: %d 条目 / %d 模板 / %d 字节，指纹 %s", itemsN, samples, bytes, built.Fingerprint()[:16])
	return libPath, idxPath, built
}

// writeReplay writes a 48 kHz mono WAV: leadS silence, midS signal, tailS silence.
func writeReplay(t *testing.T, dir, name string, lead, mid, tail []float32) string {
	t.Helper()
	all := make([]float32, 0, len(lead)+len(mid)+len(tail))
	all = append(all, lead...)
	all = append(all, mid...)
	all = append(all, tail...)
	i16 := make([]int16, len(all))
	wav.Float32ToInt16(all, i16)
	path := filepath.Join(dir, name)
	if err := wav.WriteInt16File(path, SampleRate, 1, i16); err != nil {
		t.Fatalf("WriteInt16File: %v", err)
	}
	return path
}

// runFile replays path (non-realtime) through a fresh engine and returns the
// events, the ticks and the final stats.
func runFile(t *testing.T, path string, ix *index.Index, opts Options) ([]match.Event, []Tick, Stats, time.Time) {
	t.Helper()
	src, err := NewFileSource(path, false)
	if err != nil {
		t.Fatalf("NewFileSource: %v", err)
	}
	var events []match.Event
	var ticks []Tick
	eng, err := New(src, ix, opts, func(tk Tick) { ticks = append(ticks, tk) }, func(ev match.Event) { events = append(events, ev) })
	if err != nil {
		t.Fatalf("live.New: %v", err)
	}
	runStart := time.Now()
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return events, ticks, eng.Stats(), runStart
}

// ---------------------------------------------------------------------------
// A1: file replay identifies the 880 Hz tone, and the silence stays silent
// ---------------------------------------------------------------------------

func TestFileReplayIdentifiesTone(t *testing.T) {
	_, _, ix := fixture(t)
	dir := t.TempDir()
	const mid = SampleRate / 2 // 0.5 s
	wavPath := writeReplay(t, dir, "replay.wav",
		make([]float32, SampleRate/2), // 0.5 s digital silence
		tone(mid, 880, 0.6),           // 0.5 s of the registered tone
		make([]float32, SampleRate/2), // 0.5 s digital silence
	)

	events, ticks, st, runStart := runFile(t, wavPath, ix, Options{})

	if len(events) == 0 {
		t.Fatalf("回放 880 Hz 没有产生任何事件（tick %d）", len(ticks))
	}
	best := events[0]
	if !strings.Contains(best.Name, "880") {
		t.Fatalf("事件名称 = %q，期望 880 Hz 那一项", best.Name)
	}
	off := best.Time.Sub(runStart).Seconds()
	t.Logf("命中: %q 分数 %.4f margin %.4f 电平 %.2f dBFS 时间偏移 %.3f s（信号在 0.500-1.000 s）",
		best.Name, best.Score, best.Margin, best.LevelDBFS, off)
	for i, ev := range events {
		t.Logf("  事件 #%d: %q 分数 %.4f margin %.4f 偏移 %.3f s", i+1, ev.Name, ev.Score, ev.Margin,
			ev.Time.Sub(runStart).Seconds())
	}
	if off < 0.25 || off > 1.25 {
		t.Fatalf("事件时间偏移 %.3f s 不在 0.5-1.0 s ±0.25 容差内", off)
	}
	if best.Score < 0.75 {
		t.Fatalf("命中分数 %.4f 低于阈值", best.Score)
	}
	// The silence segments must not produce an event: the earliest allowed
	// event is the tone start minus the tolerance.
	for i, ev := range events {
		if got := ev.Time.Sub(runStart).Seconds(); got < 0.25 {
			t.Fatalf("第 %d 个事件发生在静音段（%.3f s）: %q", i, got, ev.Name)
		}
	}
	if st.Events != int64(len(events)) {
		t.Fatalf("Stats.Events = %d，回调收到 %d 个", st.Events, len(events))
	}
	// Level must be far below the silence gate during the leading silence and
	// healthy inside the tone; check through the ticks.
	var minLevel, maxLevel = math.Inf(1), math.Inf(-1)
	for _, tk := range ticks {
		if math.IsInf(tk.LevelDBFS, -1) {
			minLevel = math.Inf(-1)
			continue
		}
		minLevel = math.Min(minLevel, tk.LevelDBFS)
		maxLevel = math.Max(maxLevel, tk.LevelDBFS)
	}
	t.Logf("ticks=%d 电平范围 %.1f .. %.1f dBFS，最终统计: %s", len(ticks), minLevel, maxLevel, st.Summary())
	if maxLevel < -20 {
		t.Fatalf("音频里的峰值电平只有 %.1f dBFS，回放数据不对", maxLevel)
	}
	if len(ticks) < 10 {
		t.Fatalf("tick 只有 %d 条（1.5 s 音频、50 ms 一个 tick 应有约 30 条）", len(ticks))
	}
}

// ---------------------------------------------------------------------------
// A2: negative control - an unregistered sound produces no event
// ---------------------------------------------------------------------------

func TestNegativeControlNoEvent(t *testing.T) {
	_, _, ix := fixture(t)
	dir := t.TempDir()
	wavPath := writeReplay(t, dir, "saw.wav",
		make([]float32, SampleRate/4),
		sawtooth(SampleRate, 220, 0.6), // 1 s of 220 Hz sawtooth, NOT registered
		make([]float32, SampleRate/4),
	)

	events, ticks, st, _ := runFile(t, wavPath, ix, Options{})

	var best float64
	for _, tk := range ticks {
		if len(tk.Top) > 0 && tk.Top[0].Score.Float() > best {
			best = tk.Top[0].Score.Float()
		}
	}
	t.Logf("负对照（220 Hz 锯齿）: 事件 %d 个，最高分 %.4f，tick %d，统计: %s",
		len(events), best, len(ticks), st.Summary())
	if len(events) != 0 {
		for _, ev := range events {
			t.Logf("  意外事件: %q %.4f (margin %.4f)", ev.Name, ev.Score, ev.Margin)
		}
		t.Fatalf("未注册的 220 Hz 锯齿不应产生事件，实际 %d 个", len(events))
	}
	if len(ticks) == 0 {
		t.Fatal("负对照没有 tick，说明音频根本没被处理")
	}
}

// ---------------------------------------------------------------------------
// A3: long-run memory is bounded (10 minutes of audio pushed as fast as possible)
// ---------------------------------------------------------------------------

// genSource is an endless synthetic source. It applies backpressure (blocking
// send) so the engine sees every block and the audio timeline is exact.
type genSource struct {
	gen      func(block int) []float32
	mu       sync.Mutex
	ch       chan []float32
	quit     chan struct{}
	done     chan struct{}
	started  bool
	stopOnce sync.Once
}

func newGenSource(gen func(block int) []float32) *genSource {
	return &genSource{gen: gen, ch: make(chan []float32, 4), quit: make(chan struct{}), done: make(chan struct{})}
}

func (s *genSource) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	s.mu.Unlock()
	go func() {
		defer close(s.done)
		defer close(s.ch)
		for i := 0; ; i++ {
			blk := s.gen(i)
			select {
			case s.ch <- blk:
			case <-s.quit:
				return
			}
		}
	}()
	return nil
}

func (s *genSource) Frames() <-chan []float32 { return s.ch }

func (s *genSource) Stop() error {
	s.stopOnce.Do(func() { close(s.quit) })
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started {
		<-s.done
	}
	return nil
}

func (s *genSource) Info() SourceInfo {
	return SourceInfo{Kind: "custom", Detail: "合成音频源（测试）", Rate: SampleRate, Channels: 1}
}

func TestLongRunMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("10 分钟音频的处理在 -short 模式下跳过")
	}
	_, _, ix := fixture(t)

	const blocksPerSecond = SampleRate / BlockFrames // 50
	const minutes = 10
	totalBlocks := minutes * 60 * blocksPerSecond
	oneMinute := 60 * blocksPerSecond

	// A repeating 1 s tone pattern so real scoring work happens the whole time
	// (the analyzer does not know this is synthetic).
	pattern := tone(SampleRate, 880, 0.5)
	src := newGenSource(func(block int) []float32 {
		off := (block * BlockFrames) % len(pattern)
		out := make([]float32, BlockFrames)
		for i := range out {
			out[i] = pattern[(off+i)%len(pattern)]
		}
		return out
	})

	eng, err := New(src, ix, Options{}, nil, nil)
	if err != nil {
		t.Fatalf("live.New: %v", err)
	}

	var heapAtOneMinute, heapAtEnd uint64
	var bufferedSamples, bufferedFrames int
	var mu sync.Mutex
	srcHook := func() {}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()

	// Wait until the requested amount of audio has been processed.
	deadline := time.Now().Add(4 * time.Minute)
	for {
		st := eng.Stats()
		if st.Blocks >= int64(totalBlocks) {
			break
		}
		if st.Blocks == 0 && time.Now().After(deadline) {
			t.Fatal("引擎没有处理任何数据")
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待处理 %d 块超时（只处理了 %d 块）", totalBlocks, st.Blocks)
		}
		if heapAtOneMinute == 0 && st.Blocks >= int64(oneMinute) {
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			heapAtOneMinute = ms.HeapInuse
			mu.Lock()
			bufferedSamples, bufferedFrames = 0, 0
			mu.Unlock()
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	srcHook()

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	heapAtEnd = ms.HeapInuse
	st := eng.Stats()

	// The strongest available assertion: the analyzer's own retention counters.
	// (They are read through the engine's private analyzer.)
	mu.Lock()
	bufferedSamples, bufferedFrames = eng.an.BufferedSamples(), eng.an.BufferedFrames()
	mu.Unlock()

	t.Logf("灌入 %d 块 = %.1f 分钟音频（%.1f s）", st.Blocks, st.AudioSeconds/60, st.AudioSeconds)
	t.Logf("Analyzer 保留: %d 采样（%.1f KiB）/ %d 帧（上界 %d / %d）",
		bufferedSamples, float64(bufferedSamples)*4/1024, bufferedFrames,
		eng.Params().FrameSize+eng.Params().HopSize, 2*eng.Params().WindowFrames)
	t.Logf("HeapInuse: 第 1 分钟 %d KiB -> 第 %d 分钟 %d KiB（差 %d KiB）",
		heapAtOneMinute/1024, minutes, heapAtEnd/1024, int64(heapAtEnd-heapAtOneMinute)/1024)
	t.Logf("处理耗时: 平均 %.3f ms/块，峰值 %.3f ms，每 20 ms 音频 %.3f ms（预算 %.1f ms）",
		st.AvgBlockMs, st.MaxBlockMs, st.MsPer20msAudio, RealtimeBudgetMs)

	if maxS := eng.Params().FrameSize + eng.Params().HopSize; bufferedSamples > maxS {
		t.Fatalf("Analyzer 保留了 %d 个采样，超过上界 %d：内存随时间线性增长", bufferedSamples, maxS)
	}
	if maxF := 2 * eng.Params().WindowFrames; bufferedFrames > maxF {
		t.Fatalf("Analyzer 保留了 %d 帧，超过上界 %d", bufferedFrames, maxF)
	}
	if heapAtOneMinute == 0 {
		t.Fatal("没有采到第 1 分钟的堆大小")
	}
	// 9 more minutes at 192 kB/s would be ~100 MiB of unbounded growth; 8 MiB is
	// a generous allowance for allocator noise.
	if heapAtEnd > heapAtOneMinute+8<<20 {
		t.Fatalf("HeapInuse 从 %d KiB 涨到 %d KiB（>8 MiB），长跑内存不有界",
			heapAtOneMinute/1024, heapAtEnd/1024)
	}
	if st.Events == 0 {
		t.Fatal("10 分钟音频一个事件都没有，说明匹配链路没跑起来")
	}
	if st.AudioSeconds < 0.99*float64(minutes*60) {
		t.Fatalf("只处理了 %.1f s 音频，期望 %d 分钟", st.AudioSeconds, minutes)
	}
}

// ---------------------------------------------------------------------------
// A4: a source pushing faster than the engine drops blocks, visibly
// ---------------------------------------------------------------------------

// fastSource publishes as fast as it can with NON-blocking sends, exactly like
// the loopback adapter does, and counts what it had to throw away.
type fastSource struct {
	ch      chan []float32
	quit    chan struct{}
	done    chan struct{}
	dropped atomic.Int64
	blocks  atomic.Int64
	once    sync.Once
}

func newFastSource() *fastSource {
	return &fastSource{ch: make(chan []float32, 2), quit: make(chan struct{}), done: make(chan struct{})}
}

func (s *fastSource) Start() error {
	go func() {
		defer close(s.done)
		defer close(s.ch)
		buf := tone(BlockFrames, 880, 0.5)
		for {
			select {
			case <-s.quit:
				return
			default:
			}
			s.blocks.Add(1)
			select {
			case s.ch <- buf:
			default:
				s.dropped.Add(1)
			}
		}
	}()
	return nil
}

func (s *fastSource) Frames() <-chan []float32 { return s.ch }
func (s *fastSource) Stop() error {
	s.once.Do(func() { close(s.quit) })
	<-s.done
	return nil
}
func (s *fastSource) Dropped() int64 { return s.dropped.Load() }
func (s *fastSource) Info() SourceInfo {
	return SourceInfo{Kind: "custom", Detail: "高速合成源（测试）", Rate: SampleRate, Channels: 1}
}

func TestDroppedBlocksVisibleAndNonBlocking(t *testing.T) {
	_, _, ix := fixture(t)
	src := newFastSource()

	eng, err := New(src, ix, Options{}, nil, nil)
	if err != nil {
		t.Fatalf("live.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()

	// Let the producer outrun the consumer for a while.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if eng.Stats().Dropped > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := eng.Stats()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 返回错误: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run 在取消后 10 s 仍未返回（被阻塞）")
	}
	st = eng.Stats()
	t.Logf("高速源: 生产 %d 块，丢弃 %d 块，引擎处理 %d 块 / %d 窗口",
		src.blocks.Load(), st.Dropped, st.Blocks, st.Windows)
	if st.Dropped <= 0 {
		t.Fatal("推送速度远超处理速度时 Stats().Dropped 应当 > 0")
	}
	if st.Blocks == 0 {
		t.Fatal("引擎一块都没处理：丢弃策略不能变成完全饿死")
	}
}

// ---------------------------------------------------------------------------
// A5: block/sample accounting matches the real audio length
// ---------------------------------------------------------------------------

func TestStatsBlockAccounting(t *testing.T) {
	_, _, ix := fixture(t)
	dir := t.TempDir()
	seconds := 2.5
	n := int(seconds * SampleRate)
	sig := tone(n, 880, 0.6)
	wavPath := writeReplay(t, dir, "acc.wav", nil, sig, nil)

	_, _, st, _ := runFile(t, wavPath, ix, Options{})
	audio := st.AudioSeconds
	if math.Abs(audio-seconds) > 0.01*seconds {
		t.Fatalf("处理的音频长度 %.4f s 与文件长度 %.3f s 相差超过 1%%", audio, seconds)
	}
	if got := float64(st.Blocks*BlockFrames) / SampleRate; math.Abs(got-seconds) > 0.01*seconds {
		t.Fatalf("块数 %d × 块长 %d 采样 = %.4f s，与 %.3f s 相差超过 1%%", st.Blocks, BlockFrames, got, seconds)
	}
	// Window count: one per hop, minus the windows the first patch cannot form.
	hop := eng0Hop(t)
	want := int((seconds*SampleRate - float64(eng0Window(t))) / float64(hop)) // rough
	t.Logf("块 %d（%.4f s）/ 采样 %d（%.4f s）/ 窗口 %d（理论约 %d）/ tick %d / 事件 %d",
		st.Blocks, float64(st.Blocks*BlockFrames)/SampleRate, st.Samples, audio, st.Windows, want, st.Ticks, st.Events)
	if st.Windows < int64(0.9*float64(want)) {
		t.Fatalf("窗口数 %d 明显少于理论值 %d", st.Windows, want)
	}
	if st.MsPer20msAudio <= 0 || st.AvgBlockMs <= 0 {
		t.Fatalf("处理耗时统计未生效: avg=%.4f ms/20ms=%.4f", st.AvgBlockMs, st.MsPer20msAudio)
	}
	if st.MsPer20msAudio > RealtimeBudgetMs {
		t.Logf("警告: 每 20 ms 音频耗时 %.3f ms 超过预算 %.1f ms", st.MsPer20msAudio, RealtimeBudgetMs)
	}
}

func eng0Hop(t *testing.T) int {
	t.Helper()
	return dsp.DefaultParams().HopSize
}

func eng0Window(t *testing.T) int {
	t.Helper()
	return dsp.DefaultParams().WindowFrames
}

// ---------------------------------------------------------------------------
// extra: injected frames, error propagation, SetThreshold
// ---------------------------------------------------------------------------

func TestInjectedFramesProducesEvents(t *testing.T) {
	_, _, ix := fixture(t)
	pcm := tone(SampleRate, 880, 0.6)
	eng, err := New(nil, ix, Options{Frames: pcm}, nil, nil)
	if err != nil {
		t.Fatalf("live.New: %v", err)
	}
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	st := eng.Stats()
	t.Logf("预置帧: %s", st.Summary())
	if st.Events == 0 {
		t.Fatal("预置帧模式应当产生事件")
	}
	if st.AudioSeconds != 1 {
		t.Fatalf("AudioSeconds = %.4f，期望 1", st.AudioSeconds)
	}
}

// TestThresholdGateBlocksThenAllows drives the same audio with a threshold
// above what the audio can reach and then relaxes it through SetThreshold,
// which is what the management UI's slider does.
func TestThresholdGateBlocksThenAllows(t *testing.T) {
	_, _, ix := fixture(t)
	// A quieter, slightly detuned tone so the score is good but not perfect.
	pcm := tone(SampleRate, 884, 0.25)
	src, err := NewFileSource(writeReplay(t, t.TempDir(), "q.wav", nil, pcm, nil), false)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []match.Event
	eng, err := New(src, ix, Options{TopN: 3}, nil, func(ev match.Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	// Push an impossible threshold for the 880 item before running: nothing may
	// fire for it.
	id := ""
	for _, it := range ix.Items() {
		if strings.Contains(ix.ItemName(it), "880") {
			id = it
		}
	}
	if id == "" {
		t.Fatal("找不到 880 Hz 条目")
	}
	eng.SetThreshold(id, 0.9999)
	eng.SetMinMargin(0)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	n := len(events)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("阈值 0.9999 时不应命中，实际 %d 个事件", n)
	}
	t.Logf("阈值 0.9999 -> 0 个事件（阈值门控生效）")

	// Now the same audio with a permissive threshold.
	src2, err := NewFileSource(writeReplay(t, t.TempDir(), "q2.wav", nil, pcm, nil), false)
	if err != nil {
		t.Fatal(err)
	}
	eng2, err := New(src2, ix, Options{}, nil, func(ev match.Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	eng2.SetThreshold(id, 0.5)
	eng2.SetMinMargin(0)
	if err := eng2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events)-n == 0 {
		t.Fatal("阈值 0.5 时应当命中")
	}
	t.Logf("阈值 0.5 -> %d 个事件（首个 %q %.4f）", len(events)-n, events[n].Name, events[n].Score)
}
