package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/znz/soundradar/internal/config"
	"github.com/znz/soundradar/internal/match"
)

// ---------------------------------------------------------------------------
// P3: application config + overlay API
// ---------------------------------------------------------------------------
//
// The management UI's "设置" tab talks to five endpoints:
//
//	GET   /api/config            the whole document + where it lives
//	PATCH /api/config            partial update (any subset of the keys)
//	GET   /api/overlay           overlay state (visible? which rect? hotkey?)
//	POST  /api/overlay/preview   show one sample item right now
//	POST  /api/overlay/visible   {"visible":true|false}
//
// A PATCH takes effect IMMEDIATELY: the overlay is moved/resized/re-tinted in
// place and a running live session is restarted when the capture endpoint
// changed. The file itself is written atomically by internal/config.

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

// configDTO is GET /api/config.
type configDTO struct {
	Path     string        `json:"path"`
	Source   string        `json:"source"`
	ExeDir   string        `json:"exeDir"`
	Cwd      string        `json:"cwd"`
	Exists   bool          `json:"exists"`
	Writable bool          `json:"writable"`
	Config   config.Config `json:"config"`
	SavedAt  *time.Time    `json:"savedAt,omitempty"`
	Effects  []string      `json:"effects,omitempty"`
	Error    string        `json:"error,omitempty"`
}

// patchConfigDTO is PATCH /api/config: every field is optional and the nested
// sections are merged field by field, so the UI can send {"overlay":{"x":100}}.
type patchConfigDTO struct {
	Capture *capturePatch `json:"capture"`
	Overlay *overlayPatch `json:"overlay"`
	Hotkeys *hotkeyPatch  `json:"hotkeys"`
	Recall  *recallPatch  `json:"recall"`
	Profile *string       `json:"profile"`
}

type capturePatch struct {
	Device            *string `json:"device"`
	FallbackToDefault *bool   `json:"fallbackToDefault"`
}

// recallPatch is the P4 section of a PATCH /api/config body.
type recallPatch struct {
	Enabled  *bool   `json:"enabled"`
	Seconds  *int    `json:"seconds"`
	Dir      *string `json:"dir"`
	MaxFiles *int    `json:"maxFiles"`
}

type overlayPatch struct {
	Enabled         *bool    `json:"enabled"`
	Monitor         *int     `json:"monitor"`
	X               *int     `json:"x"`
	Y               *int     `json:"y"`
	Anchor          *string  `json:"anchor"`
	Size            *int     `json:"size"`
	Opacity         *float64 `json:"opacity"`
	DurationMs      *int     `json:"durationMs"`
	FadeInMs        *int     `json:"fadeInMs"`
	FadeOutMs       *int     `json:"fadeOutMs"`
	MaxSimultaneous *int     `json:"maxSimultaneous"`
	ShowName        *bool    `json:"showName"`
	ShowScore       *bool    `json:"showScore"`
	Margin          *int     `json:"margin"`
}

type hotkeyPatch struct {
	ToggleOverlay *string `json:"toggleOverlay"`
	RecallLabel   *string `json:"recallLabel"`
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// handleConfig serves GET/PATCH /api/config.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.writeConfig(w)
	case http.MethodPatch, http.MethodPut, http.MethodPost:
		s.patchConfig(w, r)
	default:
		methodNotAllowed(w, "GET, PATCH")
	}
}

func (s *Server) writeConfig(w http.ResponseWriter) {
	info, err := config.ResolvePath()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "无法确定配置文件路径: "+err.Error())
		return
	}
	name := info.Path
	cfg := s.currentConfig()
	writeJSON(w, http.StatusOK, configDTO{
		Path: name, Source: info.Source, ExeDir: info.ExeDir, Cwd: info.Cwd,
		Exists: info.Exists, Writable: info.Writable,
		Config: cfg,
	})
}

// currentConfig returns the live document (falling back to the file, then to the
// defaults), so GET /api/config can never fail.
func (s *Server) currentConfig() config.Config {
	s.cfgMu.Lock()
	if s.cfg != nil {
		c := *s.cfg
		s.cfgMu.Unlock()
		return c
	}
	s.cfgMu.Unlock()

	cfg, _, err := config.LoadDefault()
	if err != nil || cfg == nil {
		return *config.Default()
	}
	return *cfg
}

func (s *Server) patchConfig(w http.ResponseWriter, r *http.Request) {
	body, err := readLimited(r, 1<<20)
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	var patch patchConfigDTO
	if err := json.Unmarshal(body, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}

	// Validate BEFORE touching anything, so a rejected patch can never leave a
	// half-applied state behind.
	next := s.currentConfig()
	before := next
	applyConfigPatch(&next, patch)
	if err := next.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	path, err := config.DefaultPath()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "无法确定配置文件路径: "+err.Error())
		return
	}
	if s.configPath != "" {
		path = s.configPath
	}
	if err := next.Save(path); err != nil {
		writeError(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
		return
	}
	s.cfgMu.Lock()
	s.cfg = &next
	s.cfgMu.Unlock()

	// --- hot effects -----------------------------------------------------
	effects := []string{}
	if s.overlay != nil {
		if err := s.overlay.ApplyConfig(next.Overlay); err != nil {
			effects = append(effects, "悬浮窗: "+err.Error())
		} else {
			effects = append(effects, "悬浮窗已即时生效")
		}
		if err := s.overlay.ApplyHotkey(next.Hotkeys.ToggleOverlay); err != nil {
			effects = append(effects, "热键: "+err.Error())
		}
	}
	if next.Capture.Device != before.Capture.Device {
		if err := s.retargetCapture(next.Capture.Device); err != nil {
			effects = append(effects, "采集端点: "+err.Error())
		} else {
			effects = append(effects, fmt.Sprintf("采集端点已切换到 %q", next.Capture.Device))
		}
	}

	now := time.Now()
	info, _ := config.ResolvePath()
	writeJSON(w, http.StatusOK, configDTO{
		Path: info.Path, Source: info.Source, ExeDir: info.ExeDir, Cwd: info.Cwd,
		Exists: info.Exists, Writable: info.Writable,
		Config: next, SavedAt: &now, Effects: effects,
	})
	s.logger.Printf("[api] PATCH /api/config ok overlay=(%d,%d) size=%d opacity=%.2f device=%q",
		next.Overlay.X, next.Overlay.Y, next.Overlay.Size, next.Overlay.Opacity, next.Capture.Device)
}

// applyConfigPatch merges a partial document into cfg.
// applyConfigPatch merges a partial document into cfg. Every section is merged
// field by field, so the UI can send {"recall":{"seconds":5}} alone.
func applyConfigPatch(cfg *config.Config, p patchConfigDTO) {
	if p.Profile != nil {
		cfg.Profile = strings.TrimSpace(*p.Profile)
	}
	if c := p.Capture; c != nil {
		if c.Device != nil {
			cfg.Capture.Device = *c.Device
		}
		if c.FallbackToDefault != nil {
			cfg.Capture.FallbackToDefault = *c.FallbackToDefault
		}
	}
	if h := p.Hotkeys; h != nil {
		if h.ToggleOverlay != nil {
			v := strings.TrimSpace(*h.ToggleOverlay)
			if v == "" {
				v = "none"
			}
			cfg.Hotkeys.ToggleOverlay = v
		}
		if h.RecallLabel != nil {
			v := strings.TrimSpace(*h.RecallLabel)
			if v == "" {
				v = "none"
			}
			cfg.Hotkeys.RecallLabel = v
		}
	}
	if rc := p.Recall; rc != nil {
		if rc.Enabled != nil {
			cfg.Recall.Enabled = *rc.Enabled
		}
		if rc.Seconds != nil {
			cfg.Recall.Seconds = *rc.Seconds
		}
		if rc.Dir != nil {
			cfg.Recall.Dir = strings.TrimSpace(*rc.Dir)
		}
		if rc.MaxFiles != nil {
			cfg.Recall.MaxFiles = *rc.MaxFiles
		}
	}

	o := p.Overlay
	if o == nil {
		return
	}
	set := func(dst *int, src *int) {
		if src != nil {
			*dst = *src
		}
	}
	setBool := func(dst *bool, src *bool) {
		if src != nil {
			*dst = *src
		}
	}
	setBool(&cfg.Overlay.Enabled, o.Enabled)
	set(&cfg.Overlay.Monitor, o.Monitor)
	set(&cfg.Overlay.X, o.X)
	set(&cfg.Overlay.Y, o.Y)
	if o.Anchor != nil {
		cfg.Overlay.Anchor = *o.Anchor
	}
	set(&cfg.Overlay.Size, o.Size)
	if o.Opacity != nil {
		cfg.Overlay.Opacity = *o.Opacity
	}
	set(&cfg.Overlay.DurationMs, o.DurationMs)
	set(&cfg.Overlay.FadeInMs, o.FadeInMs)
	set(&cfg.Overlay.FadeOutMs, o.FadeOutMs)
	set(&cfg.Overlay.MaxSimultaneous, o.MaxSimultaneous)
	setBool(&cfg.Overlay.ShowName, o.ShowName)
	setBool(&cfg.Overlay.ShowScore, o.ShowScore)
	set(&cfg.Overlay.Margin, o.Margin)
}

// overlayDTO is GET /api/overlay.
type overlayDTO struct {
	Available bool                 `json:"available"`
	Visible   bool                 `json:"visible"`
	Config    config.OverlayConfig `json:"config"`
	Hotkey    string               `json:"hotkey"`
	State     any                  `json:"state,omitempty"`
	Note      string               `json:"note,omitempty"`
}

// handleOverlay serves GET /api/overlay.
func (s *Server) handleOverlay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET")
		return
	}
	cfg := s.currentConfig()
	out := overlayDTO{
		Available: s.overlay != nil,
		Config:    cfg.Overlay,
		Hotkey:    cfg.Hotkeys.ToggleOverlay,
	}
	if s.overlay != nil {
		out.Visible = s.overlay.Visible()
		out.State = s.overlay.State()
	} else {
		out.Note = "本进程没有创建悬浮窗（用 `soundradar serve --overlay` 或 `soundradar overlay` 启动）"
	}
	writeJSON(w, http.StatusOK, out)
}

// handleOverlayVisible serves POST /api/overlay/visible {"visible":true}.
func (s *Server) handleOverlayVisible(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPatch {
		methodNotAllowed(w, "POST")
		return
	}
	if s.overlay == nil {
		writeError(w, http.StatusConflict, "本进程没有创建悬浮窗；请用 `soundradar serve --overlay` 启动")
		return
	}
	var req struct {
		Visible *bool `json:"visible"`
	}
	if body, err := readLimited(r, 1<<16); err == nil && len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
			return
		}
	}
	target := !s.overlay.Visible()
	if req.Visible != nil {
		target = *req.Visible
	}
	if err := s.overlay.SetVisible(target); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg := s.currentConfig()
	if cfg.Overlay.Enabled != target {
		cfg.Overlay.Enabled = target
		if path, err := config.DefaultPath(); err == nil {
			if err := cfg.Save(path); err != nil {
				s.logger.Printf("[api] 保存 visible 到配置失败: %v", err)
			}
		}
		s.cfgMu.Lock()
		s.cfg = &cfg
		s.cfgMu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "visible": s.overlay.Visible()})
}

// handleOverlayPreview serves POST /api/overlay/preview: show one real library
// item so the user can adjust the coordinates and see the result immediately.
// Body (both optional): {"id":"...","seconds":3}
func (s *Server) handleOverlayPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	if s.overlay == nil {
		writeError(w, http.StatusConflict, "本进程没有创建悬浮窗；请用 `soundradar serve --overlay` 启动")
		return
	}
	var req struct {
		ID      string  `json:"id"`
		Seconds float64 `json:"seconds"`
	}
	if body, err := readLimited(r, 1<<16); err == nil && len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
			return
		}
	}

	id := strings.TrimSpace(req.ID)
	name := ""
	if id == "" {
		// With no selection, preview the first item so the button always works.
		for _, candidate := range s.store.Order() {
			if it := s.store.Get(candidate); it != nil {
				id, name = it.ID, it.Name
				break
			}
		}
	} else if it := s.store.Get(id); it != nil {
		name = it.Name
	} else {
		writeError(w, http.StatusNotFound, "条目不存在: "+id)
		return
	}
	if id == "" {
		writeError(w, http.StatusConflict, "库里还没有条目，先添加一个音效再预览")
		return
	}
	if name == "" {
		name = "预览音效"
	}
	const previewScore = 0.93
	s.overlay.ShowHit(id, name, previewScore)
	s.logger.Printf("[api] POST /api/overlay/preview id=%s name=%q", id, name)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": id, "name": name, "score": previewScore,
		"visible": s.overlay.Visible(),
	})
}

// ---------------------------------------------------------------------------
// live integration
// ---------------------------------------------------------------------------

// overlayOnEvent pops the overlay for a recognised hit; the live hub calls it
// once per event while a session is running.
func (s *Server) overlayOnEvent(ev match.Event) {
	if s.overlay == nil {
		return
	}
	s.overlay.ShowHit(ev.ID, ev.Name, ev.Score)
}

// retargetCapture restarts a running live session on another endpoint. It is a
// no-op when no session is running (the next start uses the new config anyway).
func (s *Server) retargetCapture(device string) error {
	lcfg := s.live.currentConfig()
	lcfg.Device = device
	lcfg.WAV = ""
	if !s.live.runningNow() {
		return nil
	}
	if _, err := s.live.stop(); err != nil {
		return fmt.Errorf("停止旧会话失败: %w", err)
	}
	if err := s.live.start(lcfg); err != nil {
		return fmt.Errorf("用新端点重新开始失败: %w", err)
	}
	return nil
}

var _ = sync.Mutex{}
