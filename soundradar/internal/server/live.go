package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/znz/soundradar/internal/audio"
	"github.com/znz/soundradar/internal/capture"
	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/live"
	"github.com/znz/soundradar/internal/match"
)

// ---------------------------------------------------------------------------
// SSE protocol (documented so other clients can be written against it)
// ---------------------------------------------------------------------------
//
// GET /api/live/stream speaks text/event-stream with three message types:
//
//	event: hello
//	data: {"running":bool,"device":"...","library":"...","index":"...",
//	       "fingerprint":"...","tickMs":50,"topN":8}
//
//	event: tick                       (once per ~50 ms of audio)
//	data: {"t":<RFC3339 ms>,"audioMs":1234.5,"level":-23.4|null,
//	       "silent":false,"top":[{"id":"..","name":"..","score":0.93,"templateIndex":0}],
//	       "event":{...}|null,"stats":{...}}
//
//	event: event                      (immediately when a sound is recognised)
//	data: {"t":"...","audioMs":..,"id":"..","name":"..","score":0.97,
//	       "margin":0.66,"level":-12.3|null}
//
// `level` is null when the window is digital silence (dBFS = -Inf, which JSON
// cannot encode); `margin` is null when there is no runner-up (margin = +Inf).
// A ": ping" comment is sent every 15 s so proxies do not close the stream.
// The tick's `event` field duplicates the following `event` message so a client
// that only renders ticks still sees hits.

const (
	sseEventBuffer   = 64
	ssePingInterval  = 15 * time.Second
	liveEventHistory = 50
)

// LiveConfig selects what POST /api/live/start should listen to.
type LiveConfig struct {
	// Device is a case-insensitive substring of the endpoint friendly name; ""
	// selects the default render endpoint.
	Device string `json:"device"`
	// Library overrides the library file to index (default: the served store).
	Library string `json:"library"`
	// WAV, when set, replays a file instead of opening the sound card (offline
	// verification; no audio device is needed).
	WAV string `json:"wav"`
	// Realtime paces a WAV replay at real speed (default false = as fast as
	// possible).
	Realtime bool `json:"realtime"`
	// TopN / TickMs tune the panel stream (defaults 8 / 50).
	TopN   int `json:"topN"`
	TickMs int `json:"tickMs"`
}

// LiveStarter builds the frame source for a live session. The default opens the
// sound card (or a WAV file); tests inject a synthetic source here.
type LiveStarter func(cfg LiveConfig) (live.FrameSource, error)

// liveHub owns the single realtime session of one server.
type liveHub struct {
	srv *Server

	mu        sync.Mutex
	eng       *live.Engine
	cancel    context.CancelFunc
	done      chan struct{}
	running   bool
	cfg       LiveConfig
	srcInfo   live.SourceInfo
	libPath   string
	idxPath   string
	ix        *index.Index
	startedAt time.Time
	stats     live.Stats
	lastTick  *tickDTO
	events    []EventDTO // newest first
	eventN    int64
	degrades  int64 // ticks dropped for slow SSE subscribers
	subs      map[*liveSub]struct{}
	lastErr   string
	// overlay, when set, receives every recognised hit (P3). It is read under
	// mu because SetOverlay can run after a session started.
	overlay OverlayBridge
}

// setOverlaySink installs (or clears) the P3 hit sink.
func (h *liveHub) setOverlaySink(b OverlayBridge) {
	h.mu.Lock()
	h.overlay = b
	h.mu.Unlock()
}

// currentConfig returns a copy of the config of the running (or last) session,
// so PATCH /api/config can restart the session on a new endpoint without losing
// the rest of the settings.
func (h *liveHub) currentConfig() LiveConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

// runningNow reports whether a session is live right now.
func (h *liveHub) runningNow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

type liveSub struct {
	ch chan sseMessage
}

type sseMessage struct {
	event string
	data  any
}

func newLiveHub(s *Server) *liveHub {
	return &liveHub{
		srv:  s,
		subs: make(map[*liveSub]struct{}),
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

// HitDTO is one ranked item in a tick.
type HitDTO struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Score         float64 `json:"score"`
	TemplateIndex int     `json:"templateIndex"`
}

// EventDTO is one recognised sound effect.
type EventDTO struct {
	Time      time.Time `json:"t"`
	AudioMs   float64   `json:"audioMs"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Score     float64   `json:"score"`
	Margin    *float64  `json:"margin"`
	LevelDBFS *float64  `json:"level"`
}

// TickDTO is the SSE "tick" payload (also embedded in GET /api/live).
type TickDTO struct {
	Time      time.Time `json:"t"`
	AudioMs   float64   `json:"audioMs"`
	LevelDBFS *float64  `json:"level"`
	Silent    bool      `json:"silent"`
	Top       []HitDTO  `json:"top"`
	Event     *EventDTO `json:"event"`
	Stats     StatsDTO  `json:"stats"`
}

type tickDTO = TickDTO

// StatsDTO is the JSON form of live.Stats.
type StatsDTO struct {
	Running        bool      `json:"running"`
	Blocks         int64     `json:"blocks"`
	Windows        int64     `json:"windows"`
	Ticks          int64     `json:"ticks"`
	Events         int64     `json:"events"`
	SilentWindows  int64     `json:"silentWindows"`
	Samples        int64     `json:"samples"`
	AudioSeconds   float64   `json:"audioSeconds"`
	Dropped        int64     `json:"dropped"`
	ElapsedMs      float64   `json:"elapsedMs"`
	AvgBlockMs     float64   `json:"avgBlockMs"`
	MaxBlockMs     float64   `json:"maxBlockMs"`
	MsPer20msAudio float64   `json:"msPer20msAudio"`
	HeapInuseBytes uint64    `json:"heapInuseBytes"`
	StartedAt      time.Time `json:"startedAt"`
	LastEventAt    time.Time `json:"lastEventAt"`
	// BudgetMs is the per-20-ms-of-audio processing budget (10 ms).
	BudgetMs float64 `json:"budgetMs"`
}

// LiveStateDTO is GET /api/live.
type LiveStateDTO struct {
	Running     bool       `json:"running"`
	Device      string     `json:"device"`
	Source      string     `json:"source"`
	SourceKind  string     `json:"sourceKind"`
	Realtime    bool       `json:"realtimeSource"`
	Library     string     `json:"library"`
	IndexPath   string     `json:"index"`
	IndexItems  int        `json:"indexItems"`
	IndexTpl    int        `json:"indexTemplates"`
	IndexBytes  int        `json:"indexBytes"`
	IndexDim    int        `json:"indexDim"`
	Fingerprint string     `json:"fingerprint"`
	Algorithm   string     `json:"algorithm"`
	TickMs      int        `json:"tickMs"`
	TopN        int        `json:"topN"`
	StartedAt   *time.Time `json:"startedAt"`
	Stats       StatsDTO   `json:"stats"`
	Tick        *TickDTO   `json:"tick"`
	Events      []EventDTO `json:"events"`
	EventCount  int64      `json:"eventCount"`
	Subscribers int        `json:"subscribers"`
	Degraded    int64      `json:"droppedByBackpressure"`
	Error       string     `json:"error,omitempty"`
	// Thresholds are the per-item detection thresholds currently pushed into
	// the running matcher.
	Thresholds map[string]float64 `json:"thresholds"`
	Match      MatchOptionsDTO    `json:"match"`
}

// MatchOptionsDTO exposes the debounce gates to the UI.
type MatchOptionsDTO struct {
	DefaultThreshold float64 `json:"defaultThreshold"`
	MinMargin        float64 `json:"minMargin"`
	SilenceDBFS      float64 `json:"silenceDbfs"`
	CooldownMs       int     `json:"cooldownMs"`
	RefractoryMs     int     `json:"refractoryMs"`
}

// DeviceDTO is one render endpoint for the UI's device picker.
type DeviceDTO struct {
	Index        int    `json:"index"`
	Name         string `json:"name"`
	ID           string `json:"id"`
	Default      bool   `json:"default"`
	DefaultComms bool   `json:"defaultComms"`
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET")
		return
	}
	writeJSON(w, http.StatusOK, s.live.state())
}

func (s *Server) handleLiveDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET")
		return
	}
	out := struct {
		Devices []DeviceDTO `json:"devices"`
		Error   string      `json:"error,omitempty"`
	}{Devices: []DeviceDTO{}}

	devices, err := listRenderDevices()
	if err != nil {
		// Not fatal: the panel still works with "默认端点" and with a WAV file.
		out.Error = err.Error()
	} else {
		for _, d := range devices {
			out.Devices = append(out.Devices, DeviceDTO{
				Index: d.Index, Name: d.Name, ID: d.ID, Default: d.Default, DefaultComms: d.DefaultComms,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// listRenderDevices enumerates the WASAPI render endpoints. It creates and
// closes a capturer on the calling goroutine, which is what the COM thread
// affinity in internal/capture requires.
func listRenderDevices() ([]capture.Device, error) {
	c, err := capture.New()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.Enumerate()
}

func (s *Server) handleLiveStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	var cfg LiveConfig
	if r.Body != nil {
		body, err := readLimited(r, 1<<20)
		if err != nil {
			writeError(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
			return
		}
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := json.Unmarshal(body, &cfg); err != nil {
				writeError(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
				return
			}
		}
	}
	if err := s.live.start(cfg); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errAlreadyRunning) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.live.state())
}

func (s *Server) handleLiveStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	st, err := s.live.stop()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleLiveStream is the SSE endpoint.
func (s *Server) handleLiveStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "HTTP 写入器不支持 Flush，无法建立 SSE")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub := s.live.subscribe()
	defer s.live.unsubscribe(sub)

	writeSSE := func(event string, data any) bool {
		b, err := json.Marshal(data)
		if err != nil {
			b = []byte(`{"error":"无法序列化"}`)
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if _, err := fmt.Fprint(w, "retry: 1000\n\n"); err != nil {
		return
	}
	s.live.mu.Lock()
	hello := map[string]any{
		"running":     s.live.running,
		"device":      s.live.srcInfo.Device,
		"source":      s.live.srcInfo.Detail,
		"library":     s.live.libPath,
		"index":       s.live.idxPath,
		"fingerprint": fingerprintOf(s.live.ix),
		"tickMs":      s.live.tickMs(),
		"topN":        s.live.topN(),
	}
	last := s.live.lastTick
	s.live.mu.Unlock()
	if last != nil {
		hello["tick"] = last
	}
	if !writeSSE("hello", hello) {
		return
	}
	if !writeSSE("state", map[string]any{"running": hello["running"]}) {
		return
	}

	ping := time.NewTicker(ssePingInterval)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			// The client went away: the deferred unsubscribe removes us from
			// the hub, so no goroutine and no channel outlives the request.
			return
		case msg, ok := <-sub.ch:
			if !ok {
				return
			}
			if !writeSSE(msg.event, msg.data) {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleMatch identifies an uploaded recording offline (the "这段录音是什么"
// helper). It accepts multipart/form-data with an `audio` file (wav or mp3) or
// application/octet-stream with the raw bytes, plus an optional ?library=.
func (s *Server) handleMatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	raw, name, err := readAudioUpload(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	conv, err := audio.Convert(raw, name)
	if err != nil {
		writeError(w, statusFor(err), err.Error())
		return
	}
	pcm := make([]float32, len(conv.Mono48k))
	for i, v := range conv.Mono48k {
		pcm[i] = float32(v)
	}

	lib := strings.TrimSpace(r.URL.Query().Get("library"))
	ix, idxPath, lib, rebuilt, why, err := s.live.indexFor(lib)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res, err := live.Identify(ix, pcm, 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, samples, bytes := ix.Stats()
	total := len(res.Top)
	if total > 10 {
		res.Top = res.Top[:10]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"input": map[string]any{
			"file":       name,
			"sampleRate": conv.Source.SampleRate,
			"channels":   conv.Source.Channels,
			"seconds":    float64(len(pcm)) / live.SampleRate,
			"levelDbfs":  finitePtr(dsp.LevelDBFSOf(pcm)),
		},
		"index": map[string]any{
			"path": idxPath, "library": lib, "rebuilt": rebuilt, "rebuildWhy": why,
			"items": items, "templates": samples, "bytes": bytes,
			"dim": ix.Dim(), "fingerprint": ix.Fingerprint(), "algorithm": dsp.Algorithm,
			"totalRanked": total,
		},
		"frames":    res.Frames,
		"windows":   res.Windows,
		"elapsedMs": res.ElapsedMs,
		"top":       res.Top,
	})
}

func readAudioUpload(r *http.Request) ([]byte, string, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(maxFormMemory); err != nil {
			return nil, "", fmt.Errorf("无法解析 multipart 表单: %w", err)
		}
		defer r.MultipartForm.RemoveAll()
		f, hdr, err := r.FormFile("audio")
		if err != nil {
			return nil, "", errors.New("缺少必填字段 audio（wav 或 mp3 文件）")
		}
		defer f.Close()
		raw, err := library.ReadAllLimited(f, maxAudioBytes)
		if err != nil {
			return nil, "", fmt.Errorf("音频读取失败: %w", err)
		}
		if len(raw) == 0 {
			return nil, "", errors.New("上传的音频文件为空")
		}
		return raw, hdr.Filename, nil
	}
	raw, err := library.ReadAllLimited(r.Body, maxAudioBytes)
	if err != nil {
		return nil, "", fmt.Errorf("音频读取失败: %w", err)
	}
	if len(raw) == 0 {
		return nil, "", errors.New("请求体为空（请上传 wav / mp3）")
	}
	return raw, "audio", nil
}

// readLimited reads at most max bytes of a request body (used for the small
// JSON control requests).
func readLimited(r *http.Request, max int64) ([]byte, error) {
	return library.ReadAllLimited(r.Body, max)
}

// ---------------------------------------------------------------------------
// hub lifecycle
// ---------------------------------------------------------------------------

var errAlreadyRunning = errors.New("实时识别已经在运行（先 POST /api/live/stop）")

// start opens the source, builds/reuses the index and launches the engine.
func (h *liveHub) start(cfg LiveConfig) error {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return errAlreadyRunning
	}
	h.cfg = cfg
	h.lastErr = ""
	h.events = nil
	h.eventN = 0
	h.lastTick = nil
	h.stats = live.Stats{}
	h.mu.Unlock()

	ix, idxPath, libPath, rebuilt, why, err := h.indexFor(cfg.Library)
	if err != nil {
		h.fail(err)
		return err
	}
	if rebuilt && why != "" {
		h.srv.logger.Printf("[live] 索引已自动重建：%s", why)
	}

	starter := h.srv.newLiveSource
	if starter == nil {
		starter = defaultLiveSource
	}
	src, err := starter(cfg)
	if err != nil {
		h.fail(err)
		return err
	}

	opts := live.Options{
		TopN:        cfg.TopN,
		SilenceDBFS: match.DefaultOptions().SilenceDBFS,
	}
	if cfg.TickMs > 0 {
		opts.TickEvery = time.Duration(cfg.TickMs) * time.Millisecond
	}
	// The fallback loopback and this session cannot both hold the endpoint.
	// Hand the device to recognition, and copy its blocks into the save ring
	// so F8 / 立即保存 is not looking at an empty buffer.
	if feed := h.srv.audioFeedFn(); feed != nil {
		opts.OnBlock = feed
	}
	if hook := h.srv.onLiveFn(); hook != nil {
		hook(true)
	}
	eng, err := live.New(src, ix, opts, h.onTick, h.onEvent)
	if err != nil {
		if hook := h.srv.onLiveFn(); hook != nil {
			hook(false)
		}
		_ = src.Stop()
		h.fail(err)
		return err
	}

	// Push the per-item thresholds stored in the library into the matcher, so
	// the panel's threshold sliders take effect immediately.
	for _, it := range h.thresholds(ix, libPath) {
		eng.SetThreshold(it.id, it.threshold)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	h.mu.Lock()
	h.eng = eng
	h.cancel = cancel
	h.done = done
	h.running = true
	h.ix = ix
	h.idxPath = idxPath
	h.libPath = libPath
	h.srcInfo = eng.SourceInfo()
	h.startedAt = time.Now()
	h.mu.Unlock()

	go func() {
		defer close(done)
		defer func() {
			if hook := h.srv.onLiveFn(); hook != nil {
				hook(false)
			}
		}()
		err := eng.Run(ctx)
		h.mu.Lock()
		h.running = false
		h.stats = eng.Stats()
		h.eng = nil
		h.cancel = nil
		if err != nil {
			h.lastErr = err.Error()
		}
		h.mu.Unlock()
		h.publish("state", map[string]any{"running": false, "error": errString(err)})
		h.srv.logger.Printf("[live] 会话结束: %v", err)
	}()

	h.srv.logger.Printf("[live] 开始: 源=%s 库=%s 索引=%s", eng.SourceInfo().Detail, libPath, idxPath)
	h.publish("state", map[string]any{"running": true, "source": eng.SourceInfo().Detail})
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (h *liveHub) fail(err error) {
	h.mu.Lock()
	h.lastErr = err.Error()
	h.running = false
	h.mu.Unlock()
}

// stop cancels the engine and waits for it (and the source) to unwind.
func (h *liveHub) stop() (LiveStateDTO, error) {
	h.mu.Lock()
	cancel, done, eng := h.cancel, h.done, h.eng
	h.mu.Unlock()
	if cancel == nil {
		return h.state(), nil
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		return h.state(), errors.New("停止实时识别超时（10 s）")
	}
	h.mu.Lock()
	if eng != nil {
		h.stats = eng.Stats()
	}
	h.running = false
	h.mu.Unlock()
	return h.state(), nil
}

// indexFor loads (or builds) the index for libraryPath; an empty path means
// "the library this server serves". A missing or stale index is rebuilt by
// index.LoadOrBuild, so the panel always matches the library on disk.
func (h *liveHub) indexFor(libraryPath string) (ix *index.Index, idxPath, libPath string, rebuilt bool, why string, err error) {
	libPath = strings.TrimSpace(libraryPath)
	if libPath == "" && h.srv.store != nil {
		libPath = h.srv.store.Path()
	}
	if libPath == "" {
		return nil, "", "", false, "", errors.New("没有可用的音效库路径")
	}
	if abs, aerr := filepath.Abs(libPath); aerr == nil {
		libPath = abs
	}
	idxPath = index.DefaultPathFor(libPath)
	ix, rebuilt, why, err = index.LoadOrBuild(libPath, idxPath, dsp.DefaultParams())
	if err != nil {
		return nil, idxPath, libPath, false, why, fmt.Errorf("准备索引失败: %w", err)
	}
	return ix, idxPath, libPath, rebuilt, why, nil
}

type itemThreshold struct {
	id        string
	threshold float64
}

// thresholds reads the per-item thresholds from the served store (the index
// carries the values from build time; the store is the live source of truth).
func (h *liveHub) thresholds(ix *index.Index, libPath string) []itemThreshold {
	out := []itemThreshold{}
	if h.srv.store == nil {
		return out
	}
	if stored, err := filepath.Abs(h.srv.store.Path()); err == nil && stored != libPath {
		// A different library was indexed; the store's thresholds do not apply.
		return out
	}
	for _, id := range h.srv.store.Order() {
		if it := h.srv.store.Get(id); it != nil && it.Threshold > 0 {
			out = append(out, itemThreshold{id: id, threshold: it.Threshold})
		}
	}
	if len(out) == 0 && ix != nil {
		for _, id := range ix.Items() {
			if v := ix.Threshold(id); v > 0 {
				out = append(out, itemThreshold{id: id, threshold: v})
			}
		}
	}
	return out
}

// setThreshold pushes a threshold change into the running matcher (called by
// PATCH /api/items/{id}).
func (h *liveHub) setThreshold(id string, v float64) {
	h.mu.Lock()
	eng := h.eng
	h.mu.Unlock()
	if eng != nil {
		eng.SetThreshold(id, v)
	}
}

// ---------------------------------------------------------------------------
// tick / event plumbing
// ---------------------------------------------------------------------------

func (h *liveHub) onTick(tk live.Tick) {
	dto := h.tickToDTO(tk)
	h.mu.Lock()
	h.stats = tk.Stats
	h.lastTick = &dto
	subs := h.subscriberList()
	h.mu.Unlock()
	h.send(subs, sseMessage{event: "tick", data: dto})
}

func (h *liveHub) onEvent(ev match.Event) {
	dto := h.eventToDTO(ev)
	h.mu.Lock()
	h.eventN++
	h.events = append([]EventDTO{dto}, h.events...)
	if len(h.events) > liveEventHistory {
		h.events = h.events[:liveEventHistory]
	}
	sink := h.overlay
	subs := h.subscriberList()
	h.mu.Unlock()
	// P3: pop the hit overlay. The call only enqueues an item and posts a window
	// message, so it cannot stall the recognition path.
	if sink != nil {
		sink.ShowHit(ev.ID, ev.Name, ev.Score)
	}
	h.srv.logger.Printf("[live] 命中 %s（%s）分数 %.4f margin %.4f 电平 %s dBFS",
		ev.Name, ev.ID, ev.Score, ev.Margin, formatDBFS(ev.LevelDBFS))
	h.send(subs, sseMessage{event: "event", data: dto})
}

func (h *liveHub) tickToDTO(tk live.Tick) TickDTO {
	dto := TickDTO{
		Time:      tk.Time,
		AudioMs:   float64(tk.AudioTime.Microseconds()) / 1000,
		LevelDBFS: finitePtr(tk.LevelDBFS),
		Silent:    tk.Silent,
		Top:       make([]HitDTO, 0, len(tk.Top)),
		Stats:     statsToDTO(tk.Stats, true),
	}
	for _, s := range tk.Top {
		dto.Top = append(dto.Top, HitDTO{
			ID:            s.ID,
			Name:          s.Name,
			Score:         s.Score.Float(),
			TemplateIndex: s.Score.TemplateIndex(),
		})
	}
	if tk.Event != nil {
		ev := h.eventToDTO(*tk.Event)
		dto.Event = &ev
	}
	return dto
}

func (h *liveHub) eventToDTO(ev match.Event) EventDTO {
	h.mu.Lock()
	started := h.startedAt
	h.mu.Unlock()
	var audioMs float64
	if !started.IsZero() {
		audioMs = float64(ev.Time.Sub(started).Microseconds()) / 1000
	}
	return EventDTO{
		Time:      ev.Time,
		AudioMs:   audioMs,
		ID:        ev.ID,
		Name:      ev.Name,
		Score:     ev.Score,
		Margin:    finitePtr(ev.Margin),
		LevelDBFS: finitePtr(ev.LevelDBFS),
	}
}

func statsToDTO(st live.Stats, running bool) StatsDTO {
	return StatsDTO{
		Running:        running,
		Blocks:         st.Blocks,
		Windows:        st.Windows,
		Ticks:          st.Ticks,
		Events:         st.Events,
		SilentWindows:  st.SilentWindows,
		Samples:        st.Samples,
		AudioSeconds:   st.AudioSeconds,
		Dropped:        st.Dropped,
		ElapsedMs:      float64(st.Elapsed.Microseconds()) / 1000,
		AvgBlockMs:     st.AvgBlockMs,
		MaxBlockMs:     st.MaxBlockMs,
		MsPer20msAudio: st.MsPer20msAudio,
		HeapInuseBytes: st.HeapInuseBytes,
		StartedAt:      st.StartedAt,
		LastEventAt:    st.LastEventAt,
		BudgetMs:       live.RealtimeBudgetMs,
	}
}

// state renders GET /api/live.
func (h *liveHub) state() LiveStateDTO {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := LiveStateDTO{
		Running:     h.running,
		Source:      h.srcInfo.Detail,
		SourceKind:  h.srcInfo.Kind,
		Realtime:    h.srcInfo.Realtime,
		Device:      h.srcInfo.Device,
		Library:     h.libPath,
		IndexPath:   h.idxPath,
		Fingerprint: fingerprintOf(h.ix),
		Algorithm:   dsp.Algorithm,
		TickMs:      h.tickMs(),
		TopN:        h.topN(),
		Stats:       statsToDTO(h.stats, h.running),
		Tick:        h.lastTick,
		Events:      append([]EventDTO{}, h.events...),
		EventCount:  h.eventN,
		Subscribers: len(h.subs),
		Degraded:    h.degrades,
		Error:       h.lastErr,
		Thresholds:  map[string]float64{},
	}
	if h.ix != nil {
		items, tpl, bytes := h.ix.Stats()
		out.IndexItems, out.IndexTpl, out.IndexBytes, out.IndexDim = items, tpl, bytes, h.ix.Dim()
		if h.eng != nil {
			mo := h.eng.Matcher().Options()
			if h.srv.store != nil {
				for _, id := range h.srv.store.Order() {
					if it := h.srv.store.Get(id); it != nil {
						out.Thresholds[id] = h.eng.Matcher().Threshold(id)
					}
				}
			}
			out.Match = MatchOptionsDTO{
				DefaultThreshold: mo.DefaultThreshold,
				MinMargin:        mo.MinMargin,
				SilenceDBFS:      mo.SilenceDBFS,
				CooldownMs:       mo.CooldownMs,
				RefractoryMs:     mo.RefractoryMs,
			}
		}
	}
	if !h.startedAt.IsZero() {
		t := h.startedAt
		out.StartedAt = &t
	}
	return out
}

func (h *liveHub) tickMs() int {
	if h.cfg.TickMs > 0 {
		return h.cfg.TickMs
	}
	return 50
}

func (h *liveHub) topN() int {
	if h.cfg.TopN > 0 {
		return h.cfg.TopN
	}
	return 8
}

// ---------------------------------------------------------------------------
// subscribers
// ---------------------------------------------------------------------------

func (h *liveHub) subscribe() *liveSub {
	sub := &liveSub{ch: make(chan sseMessage, sseEventBuffer)}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

func (h *liveHub) unsubscribe(sub *liveSub) {
	h.mu.Lock()
	if _, ok := h.subs[sub]; ok {
		delete(h.subs, sub)
		close(sub.ch)
	}
	h.mu.Unlock()
}

func (h *liveHub) subscriberList() []*liveSub {
	out := make([]*liveSub, 0, len(h.subs))
	for s := range h.subs {
		out = append(out, s)
	}
	return out
}

// send fans a message out. A subscriber that cannot keep up loses the message
// (counted in Degraded) rather than blocking the recognition path; it never
// blocks the RT loop on an HTTP client.
func (h *liveHub) send(subs []*liveSub, msg sseMessage) {
	if len(subs) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- msg:
		default:
			h.degrades++
		}
	}
}

// publish is send for a one-off message to every subscriber.
func (h *liveHub) publish(event string, data any) {
	h.mu.Lock()
	subs := h.subscriberList()
	h.mu.Unlock()
	h.send(subs, sseMessage{event: event, data: data})
}

func defaultLiveSource(cfg LiveConfig) (live.FrameSource, error) {
	if strings.TrimSpace(cfg.WAV) != "" {
		return live.NewFileSource(cfg.WAV, cfg.Realtime)
	}
	return live.NewCaptureSource(cfg.Device)
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// finitePtr keeps a float JSON-encodable: encoding/json refuses NaN/Inf, which
// is exactly what a silent window (-Inf dBFS) and a missing runner-up (+Inf
// margin) produce. Such values become null.
func finitePtr(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	f := v
	return &f
}

func formatDBFS(v float64) string {
	if math.IsInf(v, -1) {
		return "-inf"
	}
	return fmt.Sprintf("%.2f", v)
}

func fingerprintOf(ix *index.Index) string {
	if ix == nil {
		return dsp.DefaultParams().Fingerprint()
	}
	return ix.Fingerprint()
}
