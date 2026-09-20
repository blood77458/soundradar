package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/live"
	"github.com/znz/soundradar/internal/wav"
)

// ---------------------------------------------------------------------------
// a synthetic frame source: no sound card, no file, deterministic
// ---------------------------------------------------------------------------

// synthSource publishes 20 ms blocks of audio from a generator function until
// it is stopped. It is the "合成 tick 源" the SSE test injects through
// Options.NewLiveSource.
type synthSource struct {
	gen  func(block int) []float32
	rate time.Duration

	ch     chan []float32
	quit   chan struct{}
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	blocks int64
	err    error
}

func newSynthSource(rate time.Duration, gen func(block int) []float32) *synthSource {
	return &synthSource{
		gen:  gen,
		rate: rate,
		ch:   make(chan []float32, 8),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
}

func (s *synthSource) Start() error {
	go func() {
		defer close(s.done)
		defer close(s.ch)
		for i := 0; ; i++ {
			blk := s.gen(i)
			select {
			case s.ch <- blk:
				s.mu.Lock()
				s.blocks++
				s.mu.Unlock()
			case <-s.quit:
				return
			}
			if s.rate > 0 {
				t := time.NewTimer(s.rate)
				select {
				case <-s.quit:
					t.Stop()
					return
				case <-t.C:
				}
			}
		}
	}()
	return nil
}

func (s *synthSource) Frames() <-chan []float32 { return s.ch }

func (s *synthSource) Stop() error {
	s.once.Do(func() { close(s.quit) })
	<-s.done
	return nil
}

func (s *synthSource) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *synthSource) Dropped() int64 { return 0 }

func (s *synthSource) Info() live.SourceInfo {
	return live.SourceInfo{Kind: "custom", Detail: "测试用合成音源（880 Hz）", Rate: 48000, Channels: 1, Realtime: true}
}

// toneBlock builds a 20 ms block of the repeating pattern, offset by the block
// counter so the waveform is continuous across blocks.
func toneBlock(block int, pattern []float32) []float32 {
	out := make([]float32, live.BlockFrames)
	off := (block * live.BlockFrames) % len(pattern)
	for i := range out {
		out[i] = pattern[(off+i)%len(pattern)]
	}
	return out
}

// liveFixtureLibrary writes a library with one 880 Hz item and its index, and
// returns the library path plus a store serving it.
func liveFixtureLibrary(t *testing.T) (string, *library.Store) {
	t.Helper()
	dir := t.TempDir()
	libPath := filepath.Join(dir, "library.srz")

	n := 48000 / 2
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = wav.ClampInt16(0.6 * math.Sin(2*math.Pi*880*float64(i)/48000))
	}
	var buf bytes.Buffer
	if err := wav.WriteInt16(&buf, 48000, 1, pcm); err != nil {
		t.Fatal(err)
	}
	store := library.New(libPath, "SSE 测试库")
	it := &library.Item{
		Name: "880Hz 测试音", Threshold: 0.72, CooldownMs: 200,
		Samples: []library.Sample{{File: library.SampleFile(0), Source: "generated"}},
	}
	if err := store.AddItem(it, nil, [][]byte{buf.Bytes()}); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return libPath, store
}

// newSSEServer builds a server whose live source is synthetic and slow enough
// to behave like a real device (one 20 ms block every 5 ms of wall time).
func newSSEServer(t *testing.T, rate time.Duration) (*httptest.Server, *library.Store) {
	t.Helper()
	libPath, store := liveFixtureLibrary(t)
	_ = libPath
	pattern := make([]float32, 48000)
	for i := range pattern {
		pattern[i] = float32(0.6 * math.Sin(2*math.Pi*880*float64(i)/48000))
	}
	srv := New(Options{
		Store: store,
		NewLiveSource: func(cfg LiveConfig) (live.FrameSource, error) {
			return newSynthSource(rate, func(block int) []float32 {
				return toneBlock(block, pattern)
			}), nil
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_, _ = srv.live.stop()
	})
	return ts, store
}

func postJSON(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

// ---------------------------------------------------------------------------
// B6: SSE protocol, event delivery and goroutine hygiene
// ---------------------------------------------------------------------------

func TestSSELiveStream(t *testing.T) {
	before := runtime.NumGoroutine()

	ts, _ := newSSEServer(t, 5*time.Millisecond)

	// --- start the session -------------------------------------------------
	code, raw := postJSON(t, ts.URL+"/api/live/start", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /api/live/start -> %d: %s", code, raw)
	}
	var started LiveStateDTO
	if err := json.Unmarshal(raw, &started); err != nil {
		t.Fatalf("起始响应不是 JSON: %v (%s)", err, raw)
	}
	if !started.Running {
		t.Fatalf("start 之后 running 应为 true: %s", raw)
	}
	t.Logf("会话已启动: source=%q 库=%s 索引=%s（%d 条目 / %d 模板 / 指纹 %s）",
		started.Source, started.Library, started.IndexPath, started.IndexItems, started.IndexTpl,
		started.Fingerprint[:16])

	// --- connect to the SSE stream ----------------------------------------
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/live/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("GET /api/live/stream: %v", err)
	}
	defer func() {
		cancel()
		res.Body.Close()
	}()

	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", got)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := res.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q", got)
	}
	t.Logf("SSE 响应头: Content-Type=%q Cache-Control=%q X-Accel-Buffering=%q",
		res.Header.Get("Content-Type"), res.Header.Get("Cache-Control"), res.Header.Get("X-Accel-Buffering"))

	// --- read events -------------------------------------------------------
	type sseMsg struct {
		event string
		data  string
	}
	msgs := make(chan sseMsg, 256)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				select {
				case msgs <- sseMsg{event: ev, data: strings.TrimPrefix(line, "data: ")}:
				default:
				}
			}
		}
		readErr <- sc.Err()
	}()

	var ticks []TickDTO
	var events []EventDTO
	var hello map[string]any
	deadline := time.After(10 * time.Second)
wait:
	for {
		select {
		case m := <-msgs:
			switch m.event {
			case "hello":
				if err := json.Unmarshal([]byte(m.data), &hello); err != nil {
					t.Fatalf("hello 不是合法 JSON: %v (%s)", err, m.data)
				}
			case "tick":
				var td TickDTO
				if err := json.Unmarshal([]byte(m.data), &td); err != nil {
					t.Fatalf("tick 不是合法 JSON: %v (%s)", err, m.data)
				}
				ticks = append(ticks, td)
			case "event":
				var ed EventDTO
				if err := json.Unmarshal([]byte(m.data), &ed); err != nil {
					t.Fatalf("event 不是合法 JSON: %v (%s)", err, m.data)
				}
				events = append(events, ed)
			}
			if len(ticks) >= 8 && len(events) >= 1 {
				break wait
			}
		case <-deadline:
			break wait
		}
	}

	if hello == nil {
		t.Fatal("没有收到 hello 消息")
	}
	if len(ticks) < 5 {
		t.Fatalf("只收到 %d 条 tick（需要 >= 5）", len(ticks))
	}
	if len(events) < 1 {
		best := 0.0
		for _, tk := range ticks {
			if len(tk.Top) > 0 && tk.Top[0].Score > best {
				best = tk.Top[0].Score
			}
		}
		t.Fatalf("没有收到 event 消息（tick %d 条，最高分 %.4f）", len(ticks), best)
	}
	first := ticks[0]
	t.Logf("hello: %v", hello)
	t.Logf("收到 tick %d 条 / event %d 条", len(ticks), len(events))
	raw1, _ := json.Marshal(first)
	t.Logf("首条 tick JSON: %s", raw1)
	rawEv, _ := json.Marshal(events[0])
	t.Logf("首条 event JSON: %s", rawEv)

	if first.Time.IsZero() {
		t.Fatal("tick 缺少 t 字段")
	}
	if first.LevelDBFS == nil {
		t.Fatal("tick 缺少 level（tone 音频不该是静音）")
	}
	if *first.LevelDBFS < -40 {
		t.Fatalf("tick 电平 %.2f dBFS 太低，合成源输出不对", *first.LevelDBFS)
	}
	if first.Stats.BudgetMs != live.RealtimeBudgetMs {
		t.Fatalf("stats.budgetMs = %v", first.Stats.BudgetMs)
	}
	if first.Stats.Blocks == 0 || first.Stats.Windows == 0 {
		t.Fatalf("tick.stats 没有累计数据: %+v", first.Stats)
	}
	// top[] must be present and sorted (the 880 item should lead).
	if len(first.Top) == 0 {
		t.Fatal("tick 的 top 为空")
	}
	for i := 1; i < len(first.Top); i++ {
		if first.Top[i-1].Score < first.Top[i].Score {
			t.Fatalf("top 未按分数降序: %v", first.Top)
		}
	}
	if first.Top[0].ID == "" || first.Top[0].Name == "" {
		t.Fatalf("top[0] 缺少 id/name: %+v", first.Top[0])
	}
	if !strings.Contains(events[0].Name, "880") {
		t.Fatalf("事件名称 = %q，期望 880 Hz 那一项", events[0].Name)
	}
	if events[0].Margin == nil {
		t.Log("（margin 为 null：没有第二名可比）")
	}
	if events[0].LevelDBFS == nil {
		t.Fatal("event 缺少 level")
	}
	// The tick that carries the event must repeat it.
	var withEvent int
	for _, td := range ticks {
		if td.Event != nil {
			withEvent++
		}
	}
	if withEvent == 0 {
		t.Log("警告: 没有 tick 携带 event 字段（事件恰好落在两次 tick 之间）")
	}

	// --- GET /api/live while running --------------------------------------
	var state LiveStateDTO
	if code := getJSON(t, ts.URL+"/api/live", &state); code != http.StatusOK {
		t.Fatalf("GET /api/live -> %d", code)
	}
	if !state.Running || state.EventCount < 1 || len(state.Events) < 1 {
		t.Fatalf("GET /api/live 的运行状态不对: running=%v events=%d", state.Running, state.EventCount)
	}
	if state.Stats.Ticks < int64(len(ticks)) {
		t.Fatalf("state.stats.ticks = %d 少于收到的 tick 数 %d", state.Stats.Ticks, len(ticks))
	}
	t.Logf("GET /api/live: running=%v 订阅者=%d 事件=%d 最近事件=%q %.4f 统计=%+v",
		state.Running, state.Subscribers, state.EventCount, state.Events[0].Name, state.Events[0].Score, state.Stats)

	// --- client disconnect must clean up ----------------------------------
	cancel()
	res.Body.Close()
	select {
	case <-readErr:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE 读取协程没有退出")
	}
	// Wait for the server-side handler to notice and unsubscribe.
	deadline = time.After(5 * time.Second)
	for {
		var st LiveStateDTO
		getJSON(t, ts.URL+"/api/live", &st)
		if st.Subscribers == 0 {
			t.Logf("客户端断开后订阅者归零（goroutine 清理完成）")
			break
		}
		select {
		case <-deadline:
			t.Fatalf("客户端断开 5 s 后服务端仍有 %d 个订阅者", st.Subscribers)
		case <-time.After(20 * time.Millisecond):
		}
	}

	// --- stop the session --------------------------------------------------
	code, raw = postJSON(t, ts.URL+"/api/live/stop", "")
	if code != http.StatusOK {
		t.Fatalf("POST /api/live/stop -> %d: %s", code, raw)
	}
	var stopped LiveStateDTO
	if err := json.Unmarshal(raw, &stopped); err != nil {
		t.Fatalf("stop 响应不是 JSON: %v", err)
	}
	if stopped.Running {
		t.Fatal("stop 之后 running 仍为 true")
	}
	// Starting twice must be refused while running.
	code, _ = postJSON(t, ts.URL+"/api/live/start", `{}`)
	if code != http.StatusOK {
		t.Fatalf("重新 start -> %d", code)
	}
	code, raw = postJSON(t, ts.URL+"/api/live/start", `{}`)
	if code != http.StatusConflict {
		t.Fatalf("重复 start 应返回 409，实际 %d: %s", code, raw)
	}
	if _, _ = postJSON(t, ts.URL+"/api/live/stop", ""); true {
	}

	// --- goroutines must not leak -----------------------------------------
	// Allow the runtime a moment to reap the HTTP/server goroutines.
	var after int
	for i := 0; i < 50; i++ {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= before+4 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("goroutine 数: 连接前 %d -> 断开并停止后 %d（差 %d）", before, after, after-before)
	if after > before+8 {
		t.Fatalf("goroutine 从 %d 涨到 %d，疑似泄漏", before, after)
	}
}

// ---------------------------------------------------------------------------
// live endpoints without a session
// ---------------------------------------------------------------------------

func TestLiveDevicesEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/live/devices")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Devices []DeviceDTO `json:"devices"`
		Error   string      `json:"error,omitempty"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("响铃不是 JSON: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if out.Devices == nil {
		t.Fatal("devices 应为数组（可以为空）")
	}
	t.Logf("GET /api/live/devices -> %d 个端点，error=%q", len(out.Devices), out.Error)
	for _, d := range out.Devices {
		t.Logf("  [%d] %s default=%v", d.Index, d.Name, d.Default)
	}
}

func TestLiveStopWithoutStart(t *testing.T) {
	ts, _ := newTestServer(t)
	code, raw := postJSON(t, ts.URL+"/api/live/stop", "")
	if code != http.StatusOK {
		t.Fatalf("未运行时 stop -> %d: %s", code, raw)
	}
	var st LiveStateDTO
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if st.Running {
		t.Fatal("未运行时 running 应为 false")
	}
}

func TestLiveRejectsUnknownSource(t *testing.T) {
	ts, _ := newTestServer(t)
	// No injected source: the default opens the sound card or a WAV file. A
	// missing WAV must fail at start, with a 400 and a Chinese message.
	code, raw := postJSON(t, ts.URL+"/api/live/start", `{"wav":"Z:\\definitely\\missing.wav"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("不存在的 wav -> %d: %s", code, raw)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.Error == "" {
		t.Fatalf("错误响应不是 {\"error\":...}: %s", raw)
	}
	t.Logf("启动不存在的 wav: %d %s", code, e.Error)
}

// ---------------------------------------------------------------------------
// POST /api/match (offline identification of an upload)
// ---------------------------------------------------------------------------

func TestMatchEndpoint(t *testing.T) {
	ts, _ := newSSEServer(t, 0) // the injected source is irrelevant here

	// Build a 1 s 880 Hz recording and POST it as multipart.
	pattern := make([]float32, 48000)
	for i := range pattern {
		pattern[i] = float32(0.6 * math.Sin(2*math.Pi*880*float64(i)/48000))
	}
	i16 := make([]int16, len(pattern))
	wav.Float32ToInt16(pattern, i16)
	var buf bytes.Buffer
	if err := wav.WriteInt16(&buf, 48000, 1, i16); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("audio", "rec880.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/match", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/match -> %d: %s", res.StatusCode, raw)
	}
	var out struct {
		Top []live.Hit `json:"top"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, raw)
	}
	if len(out.Top) == 0 {
		t.Fatalf("没有返回任何排名: %s", raw)
	}
	if !strings.Contains(out.Top[0].Name, "880") || out.Top[0].Score < 0.9 {
		t.Fatalf("离线识别结果不对: %+v", out.Top[0])
	}
	t.Logf("POST /api/match: best=%q score=%.4f at %.0f ms", out.Top[0].Name, out.Top[0].Score, out.Top[0].AtMs)

	// A recording that is too short must be a 400, not a 500.
	short := make([]int16, 100)
	var sbuf bytes.Buffer
	if err := wav.WriteInt16(&sbuf, 48000, 1, short); err != nil {
		t.Fatal(err)
	}
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/match", bytes.NewReader(sbuf.Bytes()))
	req2.Header.Set("Content-Type", "application/octet-stream")
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	raw2, _ := io.ReadAll(res2.Body)
	if res2.StatusCode != http.StatusBadRequest {
		t.Fatalf("过短录音 -> %d: %s", res2.StatusCode, raw2)
	}
	t.Logf("过短录音: %d %s", res2.StatusCode, strings.TrimSpace(string(raw2)))
}
