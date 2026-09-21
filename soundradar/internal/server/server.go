// Package server implements the P1 management API and the embedded web UI.
//
// It serves http://127.0.0.1:<port> only: the GUI can upload files into the
// library and delete entries, so it is deliberately unreachable from the
// network. Static assets are embedded with //go:embed, so the executable is
// still a single self-contained file.
package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/znz/soundradar/internal/audio"
	"github.com/znz/soundradar/internal/config"
	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/recall"
)

// StaticFS holds the hand-written UI (no framework, no CDN).
//
//go:embed static/index.html static/app.js static/style.css
var StaticFS embed.FS

// Upload limits.
const (
	maxAudioBytes = 128 << 20 // 128 MiB per audio upload
	maxIconBytes  = 8 << 20   // 8 MiB per icon upload
	maxFormMemory = 16 << 20
)

// OverlayBridge is the small contract between the HTTP API and a live overlay
// window. internal/overlay is Windows-only while the CLI depends on both
// packages, so the server never imports it: cmd/soundradar passes a concrete
// adapter that satisfies this interface.
type OverlayBridge interface {
	// ApplyConfig hot-applies a config change (move / resize / opacity / ...).
	ApplyConfig(cfg config.OverlayConfig) error
	// ApplyHotkey re-registers the global toggle hotkey.
	ApplyHotkey(spec string) error
	// SetVisible shows or hides the window.
	SetVisible(v bool) error
	// Visible reports the last requested visibility.
	Visible() bool
	// State returns a JSON-serialisable snapshot of the live window.
	State() any
	// ShowHit enqueues one recognised hit (compact: one icon + one line).
	ShowHit(id, name string, score float64)
	// ShowCards replaces the overlay with one card per grid hint, grouped by
	// format (same WxH on one row). Used when overlay.showAll is on.
	ShowCards(id string, score float64, cards []OverlayCard)
}

// OverlayCard is one grid row for the expanded overlay.
type OverlayCard struct {
	Grid string
	Name string
	PNG  []byte
}

// Options configures a Server.
type Options struct {
	Store  *library.Store
	Logger *log.Logger
	Bind   string // defaults to 127.0.0.1

	// NewLiveSource, when non-nil, replaces the frame source that
	// POST /api/live/start opens. Production leaves it nil (the sound card, or
	// a WAV file when the request asks for one); tests inject a synthetic
	// source so the whole live/SSE path can be exercised without a device.
	NewLiveSource LiveStarter

	// Overlay is the P3 hit overlay bridge (nil when `serve` was started
	// without --overlay, in which case the overlay API reports "unavailable").
	Overlay OverlayBridge

	// Config is the application config to serve from GET /api/config. Nil means
	// "load the file on first use".
	Config *config.Config

	// ConfigPath pins WHERE PATCH /api/config writes. Empty means "resolve the
	// default path every time" (config.DefaultPath), which is what a bare
	// `serve` run wants; `serve --config <file>` sets it so the settings page
	// edits the very file the user named instead of the default one.
	ConfigPath string

	// Params is the fingerprint configuration used to build or rebuild the index
	// and to drive the live link. It carries the environment-noise settings from
	// config.json, which are part of the fingerprint: with them installed the
	// server rebuilds the index instead of searching vectors produced by a
	// different pipeline. The zero value means dsp.DefaultParams().
	Params dsp.Params
}

// Server is the management API server.
type Server struct {
	store  *library.Store
	logger *log.Logger
	bind   string
	http   *http.Server
	live   *liveHub

	// newLiveSource is the injected live source factory (nil = default).
	newLiveSource LiveStarter

	// overlay is the P3 overlay bridge (nil = no overlay in this process).
	overlay OverlayBridge
	cfgMu   sync.Mutex
	cfg     *config.Config

	// recall is the P4 candidate inbox (nil = the feature is off).
	recallMu sync.Mutex
	recall   RecallSink
	// audioFeed, when set, receives every block the live recognizer analyses so
	// the recall ring is the same audio. The fallback loopback cannot share the
	// endpoint with that session.
	audioFeed func([]float32)
	// onLive is called with true when a recognition session takes the capture
	// endpoint, and false when it releases it.
	onLive func(bool)

	// configPath, when set, is the exact file PATCH /api/config writes.
	configPath string

	// params is the fingerprint configuration (Options.Params, defaulted).
	params dsp.Params
}

// New creates a Server.
func New(o Options) *Server {
	lg := o.Logger
	if lg == nil {
		lg = log.New(os.Stderr, "", log.LstdFlags)
	}
	bind := o.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	params := o.Params
	if err := params.Validate(); err != nil {
		// A caller that passed nothing gets the defaults; a caller that passed
		// something unusable is a programming error, and silently falling back
		// would search an index built by a different pipeline.
		if params == (dsp.Params{}) {
			params = dsp.DefaultParams()
		} else {
			panic("server: 无效的特征参数: " + err.Error())
		}
	}
	s := &Server{
		store:         o.Store,
		logger:        lg,
		bind:          bind,
		newLiveSource: o.NewLiveSource,
		overlay:       o.Overlay,
		cfg:           o.Config,
		configPath:    o.ConfigPath,
		params:        params,
	}
	s.live = newLiveHub(s)
	s.http = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s
}

// SetOverlay installs the overlay bridge after construction (cmd/soundradar
// creates the window and the server in either order).
func (s *Server) SetOverlay(b OverlayBridge) {
	s.overlay = b
	s.live.setOverlaySink(b)
}

// SetConfig sets the live config document.
func (s *Server) SetConfig(cfg *config.Config) {
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
}

// Shutdown gracefully stops the HTTP loop started by Serve. A running realtime
// session is stopped as well, so `serve` never leaves WASAPI open.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.live != nil {
		if _, err := s.live.stop(); err != nil {
			s.logger.Printf("[serve] 停止实时识别: %v", err)
		}
	}
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

// Handler builds the HTTP routing tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/style.css", s.staticFile("static/style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("/app.js", s.staticFile("static/app.js", "application/javascript; charset=utf-8"))
	mux.HandleFunc("/index.html", s.staticFile("static/index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("/", s.root)
	return s.withLogging(mux)
}

func (s *Server) staticFile(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "只支持 GET/HEAD")
			return
		}
		b, err := StaticFS.ReadFile(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "内嵌静态资源缺失: "+name)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
	}
}

// Listen binds the requested port on 127.0.0.1, retrying with port+1 when the
// port is taken (up to 20 attempts) unless strictPort is set.
func (s *Server) Listen(port int, strictPort bool) (net.Listener, int, error) {
	if port <= 0 || port > 65535 {
		return nil, 0, fmt.Errorf("端口 %d 无效（应在 1-65535）", port)
	}
	last := port
	for i := 0; i < 20; i++ {
		try := port + i
		if try > 65535 {
			break
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(s.bind, strconv.Itoa(try)))
		if err == nil {
			if i > 0 {
				s.logger.Printf("[serve] 端口 %d 已被占用，改用 %d", port, try)
			}
			return ln, try, nil
		}
		last = try
		if strictPort {
			return nil, 0, fmt.Errorf("无法监听 127.0.0.1:%d: %w", try, err)
		}
	}
	return nil, 0, fmt.Errorf("从 %d 起连续 20 个端口都无法监听（最后一个 %d）；请用 --port 指定别的端口", port, last)
}

// Serve runs the blocking HTTP loop on ln.
func (s *Server) Serve(ln net.Listener) error {
	err := s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------

func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" || r.URL.Path == "" {
		s.serveIndex(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.api(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	b, err := StaticFS.ReadFile("static/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "内嵌 index.html 缺失")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// api dispatches /api/... by path shape.
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api"), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "未知接口 /api/")
		return
	}
	parts := strings.Split(rest, "/")

	switch {
	case rest == "library":
		s.handleLibrary(w, r)
	case rest == "live":
		s.handleLive(w, r)
	case rest == "live/start":
		s.handleLiveStart(w, r)
	case rest == "live/stop":
		s.handleLiveStop(w, r)
	case rest == "live/devices":
		s.handleLiveDevices(w, r)
	case rest == "live/stream":
		s.handleLiveStream(w, r)
	case rest == "config":
		s.handleConfig(w, r)
	case rest == "overlay":
		s.handleOverlay(w, r)
	case rest == "overlay/preview":
		s.handleOverlayPreview(w, r)
	case rest == "overlay/visible":
		s.handleOverlayVisible(w, r)
	case rest == "match":
		s.handleMatch(w, r)
	case rest == "candidates":
		s.handleCandidates(w, r)
	case rest == "recall":
		s.handleRecall(w, r)
	case rest == "recall/trigger":
		s.handleRecallTrigger(w, r)
	case rest == "items":
		switch r.Method {
		case http.MethodGet:
			s.handleLibrary(w, r)
		case http.MethodPost:
			s.handleCreateItem(w, r)
		default:
			methodNotAllowed(w, "GET, POST")
		}
	case len(parts) == 2 && parts[0] == "candidates":
		// /api/candidates/{id} (DELETE) and /api/candidates/{id}.wav (GET) share
		// this shape; the trailing .wav decides which one it is.
		if strings.HasSuffix(parts[1], ".wav") {
			s.handleCandidate(w, r, parts[1], "wav")
		} else {
			s.handleCandidate(w, r, parts[1], "")
		}
	case len(parts) == 3 && parts[0] == "candidates" && parts[2] == "promote":
		s.handleCandidate(w, r, parts[1], "promote")
	case len(parts) == 2 && parts[0] == "items":
		s.handleItem(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "items" && parts[2] == "icon.png":
		s.handleIcon(w, r, parts[1])
	case len(parts) == 5 && parts[0] == "items" && parts[2] == "hints" && parts[4] == "icon.png":
		s.handleHintIcon(w, r, parts[1], parts[3])
	case len(parts) == 4 && parts[0] == "items" && parts[2] == "samples":
		// POST   /api/items/{id}/samples
		// DELETE /api/items/{id}/samples/{n}
		// GET    /api/items/{id}/samples/{n}.wav
		s.handleSample(w, r, parts[1], parts[3])
	case len(parts) == 3 && parts[0] == "items" && parts[2] == "samples":
		// POST /api/items/{id}/samples (append a variant)
		s.handleSample(w, r, parts[1], "")
	default:
		writeError(w, http.StatusNotFound, "未知接口 "+r.URL.Path)
	}
}

// handleItem covers GET/PATCH/DELETE /api/items/{id}.
func (s *Server) handleItem(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		it := s.store.Get(id)
		if it == nil {
			writeError(w, http.StatusNotFound, "条目不存在: "+id)
			return
		}
		writeJSON(w, http.StatusOK, s.itemDTO(it))
	case http.MethodPatch, http.MethodPut:
		s.handlePatch(w, r, id)
	case http.MethodDelete:
		if err := s.store.Delete(id); err != nil {
			writeStoreError(w, err)
			return
		}
		if err := s.store.Save(); err != nil {
			writeError(w, http.StatusInternalServerError, "写库失败: "+err.Error())
			return
		}
		s.logger.Printf("[api] DELETE /api/items/%s ok", id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
	default:
		methodNotAllowed(w, "GET, PATCH, DELETE")
	}
}

// handleSample covers the three request shapes that live under
// /api/items/{id}/samples/...:
//
//	GET    /api/items/{id}/samples/{n}.wav   -> play one sample (Range aware)
//	DELETE /api/items/{id}/samples/{n}       -> remove one sample
//	POST   /api/items/{id}/samples           -> append a sample (n is empty)
func (s *Server) handleSample(w http.ResponseWriter, r *http.Request, id, nRaw string) {
	nRaw = strings.TrimSpace(nRaw)
	if nRaw == "" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		s.handleAddSample(w, r, id)
		return
	}

	wavSuffix := strings.HasSuffix(strings.ToLower(nRaw), ".wav")
	idxPart := strings.TrimSuffix(strings.TrimSuffix(nRaw, ".wav"), ".WAV")

	if wavSuffix {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, "GET")
			return
		}
		s.serveSampleWAV(w, r, id, idxPart)
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, "DELETE")
		return
	}
	n, err := strconv.Atoi(idxPart)
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "样本序号必须是正整数: "+idxPart)
		return
	}
	if _, err := s.store.DeleteSample(id, n-1); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.store.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, "写库失败: "+err.Error())
		return
	}
	s.logger.Printf("[api] DELETE /api/items/%s/samples/%d ok", id, n)
	it := s.store.Get(id)
	if it == nil {
		writeError(w, http.StatusNotFound, "条目不存在: "+id)
		return
	}
	writeJSON(w, http.StatusOK, s.itemDTO(it))
}

func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET")
		return
	}
	it := s.store.Get(id)
	if it == nil {
		writeError(w, http.StatusNotFound, "条目不存在: "+id)
		return
	}
	icon := it.IconPNG()
	if len(icon) == 0 {
		writeError(w, http.StatusNotFound, "条目 "+id+" 没有图标")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "icon.png", it.UpdatedAt, bytes.NewReader(icon))
}

func (s *Server) handleHintIcon(w http.ResponseWriter, r *http.Request, id, idxPart string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET")
		return
	}
	n, err := strconv.Atoi(idxPart)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, "格子序号无效: "+idxPart)
		return
	}
	it := s.store.Get(id)
	if it == nil {
		writeError(w, http.StatusNotFound, "条目不存在: "+id)
		return
	}
	if n >= len(it.DisplayHints) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("条目 %s 只有 %d 条格子对照", id, len(it.DisplayHints)))
		return
	}
	icon := it.HintIcon(n)
	if len(icon) == 0 {
		writeError(w, http.StatusNotFound, "该格子还没有单独的图片")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "hint.png", it.UpdatedAt, bytes.NewReader(icon))
}

func (s *Server) serveSampleWAV(w http.ResponseWriter, r *http.Request, id, idxPart string) {
	n, err := strconv.Atoi(idxPart)
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "样本序号必须是正整数: "+idxPart)
		return
	}
	it := s.store.Get(id)
	if it == nil {
		writeError(w, http.StatusNotFound, "条目不存在: "+id)
		return
	}
	if n > len(it.Samples) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("条目 %s 只有 %d 个样本，请求的是第 %d 个", id, len(it.Samples), n))
		return
	}
	b := it.SampleWAV(n - 1)
	if len(b) == 0 {
		writeError(w, http.StatusNotFound, "样本数据缺失")
		return
	}
	// http.ServeContent implements Range / If-Range / HEAD for us, which is what
	// lets <audio> seek inside a sample.
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, fmt.Sprintf("%04d.wav", n), it.UpdatedAt, bytes.NewReader(b))
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET")
		return
	}
	st := s.store.Stats()
	ids := s.store.Order()
	items := make([]ItemDTO, 0, len(ids))
	for _, id := range ids {
		if it := s.store.Get(id); it != nil {
			items = append(items, s.itemDTO(it).ItemDTO)
		}
	}
	// Newest first: the freshly created item must be immediately visible.
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	writeJSON(w, http.StatusOK, LibraryDTO{
		Path:        st.Path,
		Name:        st.Name,
		Schema:      st.Schema,
		CreatedAt:   st.CreatedAt,
		ItemCount:   st.ItemCount,
		SampleCount: st.SampleCount,
		FileBytes:   st.FileBytes,
		SampleRate:  st.SampleRate,
		Channels:    st.Channels,
		Bits:        st.Bits,
		Feature:     st.Feature,
		Warnings:    nonNilWarnings(s.store.Warnings()),
		Items:       items,
	})
}

func nonNilWarnings(w []library.Warning) []library.Warning {
	if w == nil {
		return []library.Warning{}
	}
	return w
}

func (s *Server) handleCreateItem(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxFormMemory); err != nil {
		writeError(w, http.StatusBadRequest, "无法解析 multipart 表单: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()

	audioFile, hdr, err := r.FormFile("audio")
	if err != nil {
		writeError(w, http.StatusBadRequest, "缺少必填字段 audio（wav 或 mp3 文件）")
		return
	}
	defer audioFile.Close()

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "缺少必填字段 name")
		return
	}

	conv, err := s.convertUpload(audioFile, hdr)
	if err != nil {
		writeError(w, statusFor(err), err.Error())
		return
	}

	var iconPNG []byte
	if f, ihdr, ferr := r.FormFile("icon"); ferr == nil {
		defer f.Close()
		raw, rerr := library.ReadAllLimited(f, maxIconBytes)
		if rerr != nil {
			writeError(w, http.StatusBadRequest, "图标读取失败: "+rerr.Error())
			return
		}
		iconPNG, rerr = library.ScaleIconNamed(raw, ihdr.Filename)
		if rerr != nil {
			writeError(w, http.StatusBadRequest, rerr.Error())
			return
		}
	}

	threshold := clampFloat(parseFloatDefault(r.FormValue("threshold"), 0.72), 0, 1)
	cooldown := int(clampFloat(parseFloatDefault(r.FormValue("cooldownMs"), 400), 0, 600000))
	profile := strings.TrimSpace(r.FormValue("profile"))
	if profile == "" {
		profile = "default"
	}

	it := &library.Item{
		Name:         name,
		Threshold:    threshold,
		CooldownMs:   cooldown,
		Profile:      profile,
		Tags:         parseTagsValues(r.MultipartForm.Value["tags"]),
		Note:         strings.TrimSpace(r.FormValue("note")),
		DisplayHints: parseDisplayHintsForm(r.FormValue("displayHints")),
		Samples:      []library.Sample{newSample(conv, 0)},
	}
	if err := s.store.AddItem(it, iconPNG, [][]byte{conv.WAV}); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.applyHintIcons(r, it.ID, len(it.DisplayHints)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, "写库失败: "+err.Error())
		return
	}
	created := s.store.Get(it.ID)
	s.logger.Printf("[api] POST /api/items ok id=%s name=%q samples=1 wav=%d B peak=%.2f dBFS",
		it.ID, name, len(conv.WAV), conv.PeakDBFS)
	writeJSON(w, http.StatusCreated, s.itemDTO(created))
}

func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request, id string) {
	if s.store.Get(id) == nil {
		writeError(w, http.StatusNotFound, "条目不存在: "+id)
		return
	}
	body, iconRaw, iconName, hintPNGs, err := readPatchBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var f patchFields
	if len(body) > 0 {
		if err := json.Unmarshal(body, &f); err != nil {
			writeError(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
			return
		}
	}

	var newIcon []byte
	if len(iconRaw) > 0 {
		newIcon, err = library.ScaleIconNamed(iconRaw, iconName)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	updated, err := s.store.Update(id, func(it *library.Item) error {
		if f.Name != nil {
			n := strings.TrimSpace(*f.Name)
			if n == "" {
				return errors.New("名称不能为空")
			}
			it.Name = n
		}
		if f.Tags != nil {
			it.Tags = append([]string{}, (*f.Tags)...)
		}
		if f.Note != nil {
			it.Note = *f.Note
		}
		if f.DisplayHints != nil {
			hints, herr := library.NormalizeDisplayHints(*f.DisplayHints)
			if herr != nil {
				return herr
			}
			it.DisplayHints = hints
		}
		if f.Threshold != nil {
			it.Threshold = clampFloat(*f.Threshold, 0, 1)
		}
		if f.CooldownMs != nil {
			it.CooldownMs = int(clampFloat(float64(*f.CooldownMs), 0, 600000))
		}
		if f.Profile != nil {
			p := strings.TrimSpace(*f.Profile)
			if p == "" {
				p = "default"
			}
			it.Profile = p
		}
		if newIcon != nil {
			// SetIcon goes through Update as well, so assign directly here.
			// (library.Store.Update clones, so the blob must be set on the copy.)
			it.Icon = library.IconName
		}
		return nil
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if newIcon != nil {
		if updated, err = s.store.SetIcon(id, newIcon); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	if len(hintPNGs) > 0 {
		if updated, err = s.store.SetHintIcons(id, hintPNGs); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	if err := s.store.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, "写库失败: "+err.Error())
		return
	}
	s.logger.Printf("[api] PATCH /api/items/%s ok (icon=%v)", id, newIcon != nil)
	// Keep a running realtime session in sync with the new threshold: the panel's
	// slider edits the item, and the matcher must see the change immediately.
	if full := s.store.Get(id); full != nil {
		s.live.setThreshold(id, full.Threshold)
		writeJSON(w, http.StatusOK, s.itemDTO(full))
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleAddSample(w http.ResponseWriter, r *http.Request, id string) {
	if s.store.Get(id) == nil {
		writeError(w, http.StatusNotFound, "条目不存在: "+id)
		return
	}
	if err := r.ParseMultipartForm(maxFormMemory); err != nil {
		writeError(w, http.StatusBadRequest, "无法解析 multipart 表单: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()

	f, hdr, err := r.FormFile("audio")
	if err != nil {
		writeError(w, http.StatusBadRequest, "缺少必填字段 audio（wav 或 mp3 文件）")
		return
	}
	defer f.Close()

	conv, err := s.convertUpload(f, hdr)
	if err != nil {
		writeError(w, statusFor(err), err.Error())
		return
	}
	offset := parseFloatDefault(r.FormValue("offsetS"), 0)
	updated, err := s.store.AddSample(id, newSample(conv, offset), conv.WAV)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.store.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, "写库失败: "+err.Error())
		return
	}
	s.logger.Printf("[api] POST /api/items/%s/samples ok samples=%d", id, len(updated.Samples))
	full := s.store.Get(id)
	writeJSON(w, http.StatusCreated, s.itemDTO(full))
}

// ---------------------------------------------------------------------------
// upload helpers
// ---------------------------------------------------------------------------

func (s *Server) convertUpload(f multipart.File, hdr *multipart.FileHeader) (*audio.Converted, error) {
	raw, err := library.ReadAllLimited(f, maxAudioBytes)
	if err != nil {
		return nil, fmt.Errorf("音频读取失败: %w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("上传的音频文件为空")
	}
	name := "audio"
	if hdr != nil && hdr.Filename != "" {
		name = hdr.Filename
	}
	return audio.Convert(raw, name)
}

// newSample builds the metadata entry for a freshly converted upload.
func newSample(conv *audio.Converted, offsetS float64) library.Sample {
	src := conv.Source
	return library.Sample{
		OffsetS:    offsetS,
		LenS:       round3(src.DurationS),
		GainDb:     0,
		Source:     "upload",
		AddedAt:    time.Now().UTC(),
		StoredRate: library.SampleRate, StoredChans: library.Channels, StoredBits: library.BitsPerSample,
		Frames: int64(len(conv.Mono48k)),
		Origin: library.SourceFormat{
			FileName:      src.FileName,
			Container:     string(src.Container),
			FormatTag:     src.FormatTag,
			SampleRate:    src.SampleRate,
			Channels:      src.Channels,
			BitsPerSample: src.BitsPerSample,
			DurationS:     round3(src.DurationS),
			Frames:        src.Frames,
			Bytes:         src.Bytes,
			Resampled:     src.SampleRate != library.SampleRate,
			Downmixed:     src.Channels != library.Channels,
		},
	}
}

type patchFields struct {
	Name         *string                `json:"name"`
	Tags         *[]string              `json:"tags"`
	Note         *string                `json:"note"`
	DisplayHints *[]library.DisplayHint `json:"displayHints"`
	Threshold    *float64               `json:"threshold"`
	CooldownMs   *int                   `json:"cooldownMs"`
	Profile      *string                `json:"profile"`
}

// readPatchBody accepts either application/json or multipart/form-data (the
// latter so an icon can be replaced in the same request; the JSON document then
// travels in the "patch" field).
func readPatchBody(r *http.Request) (jsonBody []byte, icon []byte, iconName string, hintPNGs [][]byte, err error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(maxFormMemory); err != nil {
			return nil, nil, "", nil, fmt.Errorf("无法解析 multipart 表单: %w", err)
		}
		defer r.MultipartForm.RemoveAll()
		if v := r.FormValue("patch"); v != "" {
			jsonBody = []byte(v)
		} else {
			// Fall back to individual fields when the client did not send a
			// "patch" document.
			obj := map[string]any{}
			for _, k := range []string{"name", "note", "profile"} {
				if v := r.FormValue(k); v != "" {
					obj[k] = v
				}
			}
			if v := r.MultipartForm.Value["tags"]; len(v) > 0 {
				obj["tags"] = parseTagsValues(v)
			}
			if v := r.FormValue("threshold"); v != "" {
				obj["threshold"] = parseFloatDefault(v, 0)
			}
			if v := r.FormValue("cooldownMs"); v != "" {
				obj["cooldownMs"] = int(parseFloatDefault(v, 0))
			}
			if v := strings.TrimSpace(r.FormValue("displayHints")); v != "" {
				var hints []library.DisplayHint
				if json.Unmarshal([]byte(v), &hints) == nil {
					obj["displayHints"] = hints
				}
			}
			if len(obj) > 0 {
				jsonBody, _ = json.Marshal(obj)
			}
		}
		if f, hdr, ferr := r.FormFile("icon"); ferr == nil {
			defer f.Close()
			icon, err = library.ReadAllLimited(f, maxIconBytes)
			if err != nil {
				return nil, nil, "", nil, fmt.Errorf("图标读取失败: %w", err)
			}
			iconName = hdr.Filename
		}
		hintPNGs, err = readHintIconFiles(r)
		if err != nil {
			return nil, nil, "", nil, err
		}
		return jsonBody, icon, iconName, hintPNGs, nil
	}

	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("读取请求体失败: %w", err)
	}
	return b, nil, "", nil, nil
}

// ---------------------------------------------------------------------------
// JSON DTOs
// ---------------------------------------------------------------------------

// ItemDTO is the item summary returned inside the library listing.
type ItemDTO struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Icon         string           `json:"icon"`
	Threshold    float64          `json:"threshold"`
	CooldownMs   int              `json:"cooldownMs"`
	Profile      string           `json:"profile"`
	Tags         []string         `json:"tags"`
	Note         string           `json:"note"`
	DisplayHints []DisplayHintDTO `json:"displayHints"`
	SampleCount  int              `json:"sampleCount"`
	TotalLenS    float64          `json:"totalLenS"`
	CreatedAt    time.Time        `json:"createdAt"`
	UpdatedAt    time.Time        `json:"updatedAt"`
	IconURL      string           `json:"iconUrl"`
}

// DisplayHintDTO is one grid→name row with an optional icon URL.
type DisplayHintDTO struct {
	Grid    string `json:"grid"`
	Name    string `json:"name"`
	IconURL string `json:"iconUrl,omitempty"`
}

// SampleDTO is one audio variant in the item detail response.
//
// PeakDBFS uses a pointer so that "the sample is digital silence" can be
// reported as null: an infinite dBFS value cannot be encoded as JSON (the
// encoder would fail and, without care, produce an empty response body).
type SampleDTO struct {
	Index       int                  `json:"index"`
	File        string               `json:"file"`
	URL         string               `json:"url"`
	OffsetS     float64              `json:"offsetS"`
	LenS        float64              `json:"lenS"`
	GainDb      float64              `json:"gainDb"`
	Source      string               `json:"source"`
	AddedAt     time.Time            `json:"addedAt"`
	Bytes       int                  `json:"bytes"`
	Origin      library.SourceFormat `json:"origin"`
	StoredRate  int                  `json:"storedSampleRate"`
	StoredChans int                  `json:"storedChannels"`
	StoredBits  int                  `json:"storedBitsPerSample"`
	Frames      int64                `json:"frames"`
	PeakDBFS    *float64             `json:"peakDbfs"`
	Silent      bool                 `json:"silent"`
	WAVRate     int                  `json:"wavSampleRate"`
	WAVChans    int                  `json:"wavChannels"`
	WAVBits     int                  `json:"wavBitsPerSample"`
}

// ItemDetailDTO is the full item document.
type ItemDetailDTO struct {
	ItemDTO
	Samples []SampleDTO `json:"samples"`
}

// LibraryDTO is GET /api/library.
type LibraryDTO struct {
	Path        string            `json:"path"`
	Name        string            `json:"name"`
	Schema      int               `json:"schema"`
	CreatedAt   time.Time         `json:"createdAt"`
	ItemCount   int               `json:"itemCount"`
	SampleCount int               `json:"sampleCount"`
	FileBytes   int64             `json:"fileBytes"`
	SampleRate  int               `json:"sampleRate"`
	Channels    int               `json:"channels"`
	Bits        int               `json:"bitsPerSample"`
	Feature     library.Feature   `json:"feature"`
	Warnings    []library.Warning `json:"warnings"`
	Items       []ItemDTO         `json:"items"`
}

func (s *Server) itemDTO(it *library.Item) ItemDetailDTO {
	base := ItemDTO{
		ID:          it.ID,
		Name:        it.Name,
		Icon:        it.Icon,
		Threshold:   it.Threshold,
		CooldownMs:  it.CooldownMs,
		Profile:     it.Profile,
		Tags:        append([]string{}, it.Tags...),
		Note:        it.Note,
		SampleCount: len(it.Samples),
		CreatedAt:   it.CreatedAt,
		UpdatedAt:   it.UpdatedAt,
		IconURL:     "/api/items/" + it.ID + "/icon.png",
	}
	base.DisplayHints = make([]DisplayHintDTO, 0, len(it.DisplayHints))
	for i, h := range it.DisplayHints {
		dto := DisplayHintDTO{Grid: h.Grid, Name: h.Name}
		if len(it.HintIcon(i)) > 0 {
			dto.IconURL = fmt.Sprintf("/api/items/%s/hints/%d/icon.png?v=%d", it.ID, i, it.UpdatedAt.Unix())
		}
		base.DisplayHints = append(base.DisplayHints, dto)
	}
	dto := ItemDetailDTO{ItemDTO: base, Samples: make([]SampleDTO, 0, len(it.Samples))}
	for i, sm := range it.Samples {
		raw := it.SampleWAV(i)
		sd := SampleDTO{
			Index:       i + 1,
			File:        library.SampleFile(i),
			URL:         fmt.Sprintf("/api/items/%s/samples/%d.wav", it.ID, i+1),
			OffsetS:     finiteOrZero(sm.OffsetS),
			LenS:        finiteOrZero(sm.LenS),
			GainDb:      finiteOrZero(sm.GainDb),
			Source:      sm.Source,
			AddedAt:     sm.AddedAt,
			Bytes:       len(raw),
			Origin:      sm.Origin,
			StoredRate:  sm.StoredRate,
			StoredChans: sm.StoredChans,
			StoredBits:  sm.StoredBits,
			Frames:      sm.Frames,
		}
		if rate, ch, bits, peak, err := audio.DecodeCanonicalWAV(raw); err == nil {
			sd.WAVRate, sd.WAVChans, sd.WAVBits = rate, ch, bits
			if math.IsInf(peak, 0) || math.IsNaN(peak) {
				// Digital silence: report null instead of an unencodable -Inf.
				sd.PeakDBFS = nil
				sd.Silent = true
			} else {
				p := peak
				sd.PeakDBFS = &p
			}
		} else {
			sd.PeakDBFS = nil
		}
		dto.Samples = append(dto.Samples, sd)
		base.TotalLenS += sm.LenS
	}
	if math.IsInf(base.TotalLenS, 0) || math.IsNaN(base.TotalLenS) {
		base.TotalLenS = 0
	}
	dto.ItemDTO = base
	return dto
}

// ---------------------------------------------------------------------------
// small utilities
// ---------------------------------------------------------------------------

// writeJSON always answers with a complete JSON document.
//
// The body is marshalled into memory first: if encoding fails we can still send
// a well-formed {"error":"..."} instead of a Content-Type header followed by an
// empty 200 body. (encoding/json refuses to encode NaN/Inf, which is exactly
// what a silent sample produces for its peak level, so this is not theoretical.)
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		body = []byte(`{"error":"服务器内部错误：无法序列化响应（` + strings.ReplaceAll(err.Error(), `"`, `'`) + `）"}`)
		status = http.StatusInternalServerError
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError always answers with {"error":"..."} — never a bare text body.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "该接口不支持此方法（允许: "+allow+"）")
}

// statusFor maps a conversion error to an HTTP status: an unsupported container
// is the client's fault (400), anything else is treated as a bad request too
// because the payload came from the client.
func statusFor(err error) int {
	if err == nil {
		return http.StatusOK
	}
	return http.StatusBadRequest
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, library.ErrNotFound), errors.Is(err, recall.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, library.ErrExists):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func parseFloatDefault(s string, def float64) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func round3(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return float64(int64(v*1000+0.5)) / 1000
}

// finiteOrZero keeps a float JSON-encodable.
func finiteOrZero(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// parseTagsValues accepts either repeated form fields or one comma/space
// separated string.
func parseTagsValues(vals []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range vals {
		for _, part := range strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == '，' || r == ';' || r == '；' || r == ' ' || r == '\t' || r == '\n'
		}) {
			p := strings.TrimSpace(part)
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// parseDisplayHintsForm decodes a JSON array of DisplayHint from a form field.
// Invalid or empty input yields an empty slice (validation happens in AddItem).
func parseDisplayHintsForm(raw string) []library.DisplayHint {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var hints []library.DisplayHint
	if err := json.Unmarshal([]byte(raw), &hints); err != nil {
		return nil
	}
	return hints
}

func readHintIconFiles(r *http.Request) ([][]byte, error) {
	if r == nil || r.MultipartForm == nil {
		return nil, nil
	}
	max := -1
	for i := 0; i < library.MaxDisplayHints; i++ {
		if _, ok := r.MultipartForm.File[fmt.Sprintf("hintIcon%d", i)]; ok {
			max = i
		}
	}
	if max < 0 {
		return nil, nil
	}
	out := make([][]byte, max+1)
	for i := 0; i <= max; i++ {
		f, hdr, err := r.FormFile(fmt.Sprintf("hintIcon%d", i))
		if err != nil {
			continue
		}
		raw, rerr := library.ReadAllLimited(f, maxIconBytes)
		f.Close()
		if rerr != nil {
			return nil, fmt.Errorf("对照图 %d 读取失败: %w", i+1, rerr)
		}
		png, rerr := library.ScaleIconNamed(raw, hdr.Filename)
		if rerr != nil {
			return nil, fmt.Errorf("对照图 %d: %w", i+1, rerr)
		}
		out[i] = png
	}
	return out, nil
}

func (s *Server) applyHintIcons(r *http.Request, id string, nHints int) error {
	pngs, err := readHintIconFiles(r)
	if err != nil {
		return err
	}
	if len(pngs) == 0 || nHints == 0 {
		return nil
	}
	if len(pngs) > nHints {
		pngs = pngs[:nHints]
	}
	_, err = s.store.SetHintIcons(id, pngs)
	return err
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		s.logger.Printf("[http] %s %s -> %d (%d B, %s)",
			r.Method, r.URL.RequestURI(), rec.status, rec.bytes, time.Since(start).Round(time.Millisecond))
	})
}
