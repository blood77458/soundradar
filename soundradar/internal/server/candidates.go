package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/znz/soundradar/internal/audio"
	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/recall"
)

// ---------------------------------------------------------------------------
// P4: the candidate inbox API
// ---------------------------------------------------------------------------
//
//	GET    /api/candidates                  the inbox, newest first
//	GET    /api/candidates/{id}.wav         playback (Range aware)
//	DELETE /api/candidates/{id}             drop one candidate
//	POST   /api/candidates/{id}/promote     name it: create an item or append
//	POST   /api/recall/trigger              save "the last N seconds" now
//	GET    /api/recall                      compact status for the UI
//
// The promote endpoint is the core of the P4 loop: it takes a nameless clip and
// turns it into real library content, which removes the "record it later and
// trim it" step entirely.

// RecallSink is the small contract between the HTTP layer and whatever runs the
// recall ring in this process. cmd/soundradar installs it; the server itself
// never imports internal/hotkey (Windows-only) or the CLI wiring.
type RecallSink interface {
	// Enabled reports whether "save the last N seconds" is available.
	Enabled() bool
	// Save snapshots the ring NOW. source is recorded on the candidate.
	Save(source string) (recall.Candidate, error)
	// Trigger invokes the registered hotkey callback WITHOUT any keyboard
	// input, proving the RegisterHotKey -> callback path end to end.
	Trigger() error
	// List returns the inbox, newest first.
	List() ([]recall.Candidate, error)
	// Get returns one candidate.
	Get(id string) (recall.Candidate, error)
	// WAVBytes returns the candidate audio.
	WAVBytes(id string) ([]byte, error)
	// Delete removes a candidate.
	Delete(id string) error
	// Dir is the absolute inbox directory.
	Dir() string
	// RingSeconds is the length of one snapshot.
	RingSeconds() float64
	// CoverSeconds is how much audio is buffered right now.
	CoverSeconds() float64
	// HotkeyStatus describes the registration result in one Chinese line.
	HotkeyStatus() string
	// MaxFiles is the retention limit.
	MaxFiles() int
	// EnsureCapture makes sure audio is flowing into the ring even when no
	// recognition session is running (otherwise POST /api/recall/trigger would
	// always find an empty buffer). It must be cheap and idempotent.
	EnsureCapture() error
}

// SetRecall installs the recall sink after construction.
func (s *Server) SetRecall(r RecallSink) {
	s.recallMu.Lock()
	s.recall = r
	s.recallMu.Unlock()
}

// SetAudioFeed installs the tap that copies live-recognition audio into the
// recall ring, and the hook that is told when that session takes or releases
// the capture endpoint.
func (s *Server) SetAudioFeed(feed func([]float32), onLive func(bool)) {
	s.recallMu.Lock()
	s.audioFeed = feed
	s.onLive = onLive
	s.recallMu.Unlock()
}

func (s *Server) audioFeedFn() func([]float32) {
	s.recallMu.Lock()
	defer s.recallMu.Unlock()
	return s.audioFeed
}

func (s *Server) onLiveFn() func(bool) {
	s.recallMu.Lock()
	defer s.recallMu.Unlock()
	return s.onLive
}

// recallSink returns the installed sink (nil when P4 is unavailable).
func (s *Server) recallSink() RecallSink {
	s.recallMu.Lock()
	defer s.recallMu.Unlock()
	return s.recall
}

// CandidateDTO is one row of GET /api/candidates.
//
// PeakDBFS is a POINTER so digital silence is reported as null: -Inf cannot be
// encoded as JSON, and an empty 200 body is exactly the P1 bug this type is
// shaped to avoid.
type CandidateDTO struct {
	ID         string    `json:"id"`
	Seconds    float64   `json:"seconds"`
	PeakDBFS   *float64  `json:"peakDbfs"`
	Silent     bool      `json:"silent"`
	Frames     int64     `json:"frames"`
	CreatedAt  time.Time `json:"createdAt"`
	GuessID    string    `json:"guessId,omitempty"`
	GuessName  string    `json:"guessName,omitempty"`
	GuessScore *float64  `json:"guessScore,omitempty"`
	Source     string    `json:"source,omitempty"`
	WAVURL     string    `json:"wavUrl"`
	File       string    `json:"file"`
	// Promoted is always false inside the inbox (promoted candidates leave it);
	// the field exists so the UI can render one shape for both lists.
	Promoted bool `json:"promoted"`
}

// candidatesDTO is GET /api/candidates.
type candidatesDTO struct {
	Available   bool           `json:"available"`
	Dir         string         `json:"dir"`
	Count       int            `json:"count"`
	MaxFiles    int            `json:"maxFiles"`
	RingSeconds float64        `json:"ringSeconds"`
	CoveredSec  float64        `json:"coveredSeconds"`
	Hotkey      string         `json:"hotkey"`
	HotkeyOK    bool           `json:"hotkeyRegistered"`
	Status      string         `json:"status,omitempty"`
	Note        string         `json:"note,omitempty"`
	Items       []CandidateDTO `json:"items"`
}

func newCandidateDTO(c recall.Candidate) CandidateDTO {
	dto := CandidateDTO{
		ID:        c.ID,
		Seconds:   round3(c.Seconds),
		Silent:    c.Silent,
		Frames:    c.Frames,
		CreatedAt: c.CreatedAt,
		GuessID:   c.GuessID,
		GuessName: c.GuessName,
		Source:    c.Source,
		WAVURL:    "/api/candidates/" + c.ID + ".wav",
		File:      c.File,
	}
	if !c.Silent && isFinite(c.PeakDBFS) {
		p := round2(c.PeakDBFS)
		dto.PeakDBFS = &p
	}
	if c.GuessID != "" && c.GuessScoreValid && isFinite(c.GuessScore) {
		g := round4(c.GuessScore)
		dto.GuessScore = &g
	}
	return dto
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// round2 / round4 keep the JSON numbers short and, more importantly, keep a
// NaN/Inf from ever reaching the encoder.
func round2(v float64) float64 {
	if !isFinite(v) {
		return 0
	}
	return math.Round(v*100) / 100
}

func round4(v float64) float64 {
	if !isFinite(v) {
		return 0
	}
	return math.Round(v*10000) / 10000
}

// handleCandidates serves GET /api/candidates.
func (s *Server) handleCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET")
		return
	}
	sink := s.recallSink()
	if sink == nil {
		cfg := s.currentConfig()
		writeJSON(w, http.StatusOK, candidatesDTO{
			Available: false,
			Dir:       cfg.Recall.Dir,
			Hotkey:    cfg.Hotkeys.RecallLabel,
			Note:      "本进程没有启用回溯保存（配置 recall.enabled=false，或热键子系统不可用）",
			Items:     []CandidateDTO{},
		})
		return
	}
	cands, err := sink.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取候选项失败: "+err.Error())
		return
	}
	ok, spec := s.recallHotkeyStatus()
	out := candidatesDTO{
		Available:   sink.Enabled(),
		Dir:         sink.Dir(),
		Count:       len(cands),
		MaxFiles:    sink.MaxFiles(),
		RingSeconds: round3(sink.RingSeconds()),
		CoveredSec:  round3(sink.CoverSeconds()),
		Hotkey:      spec,
		HotkeyOK:    ok,
		Status:      sink.HotkeyStatus(),
		Items:       make([]CandidateDTO, 0, len(cands)),
	}
	for _, c := range cands {
		out.Items = append(out.Items, newCandidateDTO(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// recallHotkeyStatus parses the CLI's one-line hotkey description into
// (ok, spelling). A registered key mentions either RegisterHotKey success or
// the in-game poller ("含游戏内轮询").
func (s *Server) recallHotkeyStatus() (bool, string) {
	sink := s.recallSink()
	if sink == nil {
		return false, s.currentConfig().Hotkeys.RecallLabel
	}
	status := sink.HotkeyStatus()
	ok := strings.Contains(status, "成功") || strings.Contains(status, "轮询") || strings.Contains(status, "回调")
	if strings.Contains(status, "注册失败") || strings.Contains(status, "未启用") || strings.Contains(status, "未注册") {
		ok = false
	}
	spec := status
	if i := strings.IndexAny(spec, "（("); i > 0 {
		spec = spec[:i]
	}
	return ok, strings.TrimSpace(spec)
}

// handleCandidate covers /api/candidates/{id}[.wav] and .../{id}/promote.
func (s *Server) handleCandidate(w http.ResponseWriter, r *http.Request, idRaw string, action string) {
	id := strings.TrimSuffix(idRaw, ".wav")
	if err := validateCandidateID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sink := s.recallSink()

	switch action {
	case "wav":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, "GET")
			return
		}
		if sink == nil {
			writeError(w, http.StatusConflict, "本进程没有启用回溯保存")
			return
		}
		s.serveCandidateWAV(w, r, sink, id)
	case "promote":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		if sink == nil {
			writeError(w, http.StatusConflict, "本进程没有启用回溯保存")
			return
		}
		s.handlePromote(w, r, sink, id)
	case "":
		if r.Method != http.MethodDelete {
			methodNotAllowed(w, "DELETE")
			return
		}
		if sink == nil {
			writeError(w, http.StatusConflict, "本进程没有启用回溯保存")
			return
		}
		if err := sink.Delete(id); err != nil {
			writeStoreError(w, err)
			return
		}
		s.logger.Printf("[api] DELETE /api/candidates/%s ok", id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
	default:
		writeError(w, http.StatusNotFound, "未知接口 "+r.URL.Path)
	}
}

func (s *Server) serveCandidateWAV(w http.ResponseWriter, r *http.Request, sink RecallSink, id string) {
	raw, err := sink.WAVBytes(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// http.ServeContent gives Range/HEAD/If-Range for free, which is what lets
	// <audio> seek inside a candidate.
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, id+".wav", time.Time{}, bytes.NewReader(raw))
}

// promoteRequest is the JSON shape of POST /api/candidates/{id}/promote.
type promoteRequest struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
	Note string   `json:"note"`
	// Icon is either an existing item id to borrow the PNG from, or ignored when
	// a file was uploaded in the same multipart request.
	Icon         string                `json:"icon"`
	TargetItemID string                `json:"targetItemId"`
	Threshold    *float64              `json:"threshold"`
	CooldownMs   *int                  `json:"cooldownMs"`
	Profile      string                `json:"profile"`
	DisplayHints []library.DisplayHint `json:"displayHints"`
}

// promoteResponse is the body of a successful promote.
type promoteResponse struct {
	OK                bool          `json:"ok"`
	Action            string        `json:"action"` // created | appended
	ItemID            string        `json:"id"`
	CandidateID       string        `json:"candidateId"`
	CandidateConsumed bool          `json:"candidateConsumed"`
	Message           string        `json:"message"`
	Item              ItemDetailDTO `json:"item"`
}

// handlePromote turns one candidate into library content.
//
// Two paths:
//
//	targetItemId == ""  -> create a new item whose first sample is the candidate
//	targetItemId != ""  -> append the candidate as a new sample of that item
//
// On success the candidate is CONSUMED: its files are deleted, so it leaves the
// inbox and the UI badge drops. The library is the durable copy at that point
// (see recall.Store.MarkUsed for the alternative retention policy this
// deliberately does not use - it keeps the inbox honest about "what still needs
// naming").
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request, sink RecallSink, id string) {
	cand, err := sink.Get(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	req, iconPNG, err := readPromoteBody(r, s)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	wavBytes, err := sink.WAVBytes(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The candidate is written by internal/recall in exactly the canonical
	// format, but promote is the boundary between "a file in a directory anyone
	// could touch" and "content inside library.srz", so the P1 pipeline's
	// guarantee (48k/mono/16-bit + origin traceability) is re-established here
	// rather than assumed.
	conv, err := audio.Convert(wavBytes, cand.ID+".wav")
	if err != nil {
		writeError(w, http.StatusBadRequest, "候选项音频无法入库: "+err.Error())
		return
	}

	target := strings.TrimSpace(req.TargetItemID)
	if target == "" {
		name := strings.TrimSpace(req.Name)
		if name == "" {
			// A candidate always carries a timestamp, so a usable default name
			// exists and the form cannot fail on an empty field.
			name = defaultCandidateName(cand)
		}
		it := &library.Item{
			Name:         name,
			Threshold:    clampFloat(derefFloat(req.Threshold, 0.72), 0, 1),
			CooldownMs:   int(clampFloat(float64(derefInt(req.CooldownMs, 400)), 0, 600000)),
			Profile:      orDefaultString(strings.TrimSpace(req.Profile), "default"),
			Tags:         normalizeTags(req.Tags),
			Note:         strings.TrimSpace(req.Note),
			DisplayHints: req.DisplayHints,
			Samples:      []library.Sample{newRecallSample(conv, cand)},
		}
		if err := s.store.AddItem(it, iconPNG, [][]byte{conv.WAV}); err != nil {
			writeStoreError(w, err)
			return
		}
		if err := s.store.Save(); err != nil {
			writeError(w, http.StatusInternalServerError, "写库失败: "+err.Error())
			return
		}
		consumed := s.consumeCandidate(sink, id)
		created := s.store.Get(it.ID)
		s.logger.Printf("[api] POST /api/candidates/%s/promote -> 新条目 id=%s name=%q samples=1 wav=%d B（候选项已消费=%v）",
			id, it.ID, name, len(conv.WAV), consumed)
		writeJSON(w, http.StatusOK, promoteResponse{
			OK: true, Action: "created", ItemID: it.ID, Item: s.itemDTO(created),
			CandidateID: id, CandidateConsumed: consumed,
			Message: fmt.Sprintf("已创建条目「%s」（id=%s），样本 1 个", name, it.ID),
		})
		return
	}

	if s.store.Get(target) == nil {
		writeError(w, http.StatusNotFound, "targetItemId 指向的条目不存在: "+target)
		return
	}
	updated, err := s.store.AddSample(target, newRecallSample(conv, cand), conv.WAV)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.store.Save(); err != nil {
		writeError(w, http.StatusInternalServerError, "写库失败: "+err.Error())
		return
	}
	consumed := s.consumeCandidate(sink, id)
	full := s.store.Get(target)
	s.logger.Printf("[api] POST /api/candidates/%s/promote -> 追加到 %s，样本数 %d（候选项已消费=%v）",
		id, target, len(updated.Samples), consumed)
	writeJSON(w, http.StatusOK, promoteResponse{
		OK: true, Action: "appended", ItemID: target, Item: s.itemDTO(full),
		CandidateID: id, CandidateConsumed: consumed,
		Message: fmt.Sprintf("已把候选项追加为「%s」的第 %d 个样本", full.Name, len(full.Samples)),
	})
}

// consumeCandidate removes the candidate from the inbox after a successful
// promote. A failure here must not fail the promote (the library item already
// exists), so it is only reported in the response.
func (s *Server) consumeCandidate(sink RecallSink, id string) bool {
	if err := sink.Delete(id); err != nil {
		s.logger.Printf("[api] 候选项 %s 已入库但删除失败: %v", id, err)
		return false
	}
	return true
}

// newRecallSample builds the library sample metadata for a promoted candidate.
// Origin keeps the candidate's real provenance, so the library still records
// "this came from a recall snapshot" - the P1 origin traceability rule.
func newRecallSample(conv *audio.Converted, cand recall.Candidate) library.Sample {
	src := conv.Source
	return library.Sample{
		OffsetS:    0,
		LenS:       round3(src.DurationS),
		GainDb:     0,
		Source:     "recall",
		AddedAt:    time.Now().UTC(),
		StoredRate: library.SampleRate, StoredChans: library.Channels, StoredBits: library.BitsPerSample,
		Frames: int64(len(conv.Mono48k)),
		Origin: library.SourceFormat{
			FileName:      cand.ID + ".wav",
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

// readPromoteBody accepts either application/json or multipart/form-data (so an
// icon file can travel in the same request). A JSON "icon" string means
// "borrow the icon of this existing item".
func readPromoteBody(r *http.Request, s *Server) (promoteRequest, []byte, error) {
	var req promoteRequest
	var iconPNG []byte
	ct := r.Header.Get("Content-Type")

	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(maxFormMemory); err != nil {
			return req, nil, fmt.Errorf("无法解析 multipart 表单: %w", err)
		}
		defer r.MultipartForm.RemoveAll()

		if v := strings.TrimSpace(r.FormValue("promote")); v != "" {
			if err := json.Unmarshal([]byte(v), &req); err != nil {
				return req, nil, fmt.Errorf("promote 字段不是合法 JSON: %w", err)
			}
		} else {
			req.Name = r.FormValue("name")
			req.Note = r.FormValue("note")
			req.Profile = r.FormValue("profile")
			req.TargetItemID = r.FormValue("targetItemId")
			req.Icon = r.FormValue("icon")
			req.Tags = parseTagsValues(r.MultipartForm.Value["tags"])
			if v := r.FormValue("threshold"); v != "" {
				f := parseFloatDefault(v, 0.72)
				req.Threshold = &f
			}
			if v := r.FormValue("cooldownMs"); v != "" {
				n := int(parseFloatDefault(v, 400))
				req.CooldownMs = &n
			}
			if v := strings.TrimSpace(r.FormValue("displayHints")); v != "" {
				req.DisplayHints = parseDisplayHintsForm(v)
			}
		}
		// "icon" is a FILE when the browser sends one and a plain form value
		// otherwise; FormFile only reports the upload case.
		if f, hdr, ferr := r.FormFile("icon"); ferr == nil && hdr != nil && hdr.Filename != "" {
			defer f.Close()
			raw, rerr := library.ReadAllLimited(f, maxIconBytes)
			if rerr != nil {
				return req, nil, fmt.Errorf("图标读取失败: %w", rerr)
			}
			png, rerr := library.ScaleIconNamed(raw, hdr.Filename)
			if rerr != nil {
				return req, nil, rerr
			}
			iconPNG = png
		}
	} else {
		body, err := readLimited(r, 1<<20)
		if err != nil {
			return req, nil, fmt.Errorf("读取请求体失败: %w", err)
		}
		if len(strings.TrimSpace(string(body))) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				return req, nil, fmt.Errorf("JSON 解析失败: %w", err)
			}
		}
	}

	if len(iconPNG) == 0 {
		if src := strings.TrimSpace(req.Icon); src != "" {
			it := s.store.Get(src)
			if it == nil {
				return req, nil, fmt.Errorf("icon 指向的条目不存在: %s", src)
			}
			iconPNG = it.IconPNG()
		}
	}
	return req, iconPNG, nil
}

// handleRecallTrigger serves POST /api/recall/trigger: save "the last N
// seconds" right now. The management UI's button and the acceptance script both
// use it, and neither needs a keyboard.
//
// body (optional): {"source":"api"}
func (s *Server) handleRecallTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	sink := s.recallSink()
	if sink == nil || !sink.Enabled() {
		writeError(w, http.StatusConflict, "本进程没有启用回溯保存（配置 recall.enabled=false，或热键子系统不可用）")
		return
	}
	source := "api"
	if body, err := readLimited(r, 1<<16); err == nil && len(strings.TrimSpace(string(body))) > 0 {
		var req struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(body, &req); err == nil && strings.TrimSpace(req.Source) != "" {
			source = strings.TrimSpace(req.Source)
		}
	}
	// Without a recognition session the ring still has to be filling: this
	// lazily opens a plain loopback capture so the button works in `serve` too.
	if err := sink.EnsureCapture(); err != nil {
		s.logger.Printf("[api] 回溯保存的采集兜底启动失败: %v", err)
	}
	cand, err := sink.Save(source)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.logger.Printf("[api] POST /api/recall/trigger ok id=%s %.2fs peak=%s dBFS guess=%q",
		cand.ID, cand.Seconds, levelForLog(cand), cand.GuessName)
	s.publishRecall(cand)
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":        true,
		"candidate": newCandidateDTO(cand),
		"count":     s.candidateCount(sink),
	})
}

func (s *Server) candidateCount(sink RecallSink) int {
	if list, err := sink.List(); err == nil {
		return len(list)
	}
	return 0
}

// publishRecall pushes a "recall" SSE event so an open management page updates
// its badge without polling.
func (s *Server) publishRecall(c recall.Candidate) {
	if s.live == nil {
		return
	}
	s.live.publish("recall", map[string]any{
		"id": c.ID, "seconds": round3(c.Seconds), "silent": c.Silent,
		"peakDbfs": peakOrNil(c), "guessName": c.GuessName, "source": c.Source,
	})
}

func peakOrNil(c recall.Candidate) *float64 {
	if c.Silent || !isFinite(c.PeakDBFS) {
		return nil
	}
	p := round2(c.PeakDBFS)
	return &p
}

func levelForLog(c recall.Candidate) string {
	if c.Silent {
		return "-inf"
	}
	return strconv.FormatFloat(c.PeakDBFS, 'f', 2, 64)
}

// handleRecall serves GET /api/recall: the compact status the UI reads.
func (s *Server) handleRecall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET")
		return
	}
	sink := s.recallSink()
	cfg := s.currentConfig()
	out := map[string]any{
		"available":   false,
		"enabled":     cfg.Recall.Enabled,
		"seconds":     cfg.Recall.Seconds,
		"maxFiles":    cfg.Recall.MaxFiles,
		"dir":         cfg.Recall.Dir,
		"hotkey":      cfg.Hotkeys.RecallLabel,
		"ringSeconds": float64(cfg.Recall.Seconds),
		"count":       0,
	}
	if sink != nil {
		ok, spec := s.recallHotkeyStatus()
		out["available"] = sink.Enabled()
		out["dir"] = sink.Dir()
		out["maxFiles"] = sink.MaxFiles()
		out["ringSeconds"] = round3(sink.RingSeconds())
		out["coveredSeconds"] = round3(sink.CoverSeconds())
		out["hotkey"] = spec
		out["hotkeyRegistered"] = ok
		out["status"] = sink.HotkeyStatus()
		out["count"] = s.candidateCount(sink)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// RecallSink adapter
// ---------------------------------------------------------------------------

// staticRecaller adapts the CLI's recall wiring to RecallSink.
//
// It is intentionally dumb: every field is a function, so internal/server never
// has to know how the ring is filled (live engine tap, CLI capture, ...).
type staticRecaller struct {
	enabled  bool
	dir      string
	ringSec  float64
	maxFiles int

	coverSec  func() float64
	hotkey    func() string
	saveFn    func(source string) (recall.Candidate, error)
	triggerFn func() error
	listFn    func() ([]recall.Candidate, error)
	getFn     func(id string) (recall.Candidate, error)
	wavFn     func(id string) ([]byte, error)
	deleteFn  func(id string) error
	ensureFn  func() error

	mu          sync.Mutex
	captureErr  error
	captureDone bool
}

// Enabled reports whether recall is usable.
func (r *staticRecaller) Enabled() bool { return r != nil && r.enabled }

// Save snapshots the ring now.
func (r *staticRecaller) Save(source string) (recall.Candidate, error) {
	if r == nil || r.saveFn == nil {
		return recall.Candidate{}, errors.New("回溯保存未启用")
	}
	return r.saveFn(source)
}

// Trigger invokes the registered hotkey callback without keyboard input.
func (r *staticRecaller) Trigger() error {
	if r == nil || r.triggerFn == nil {
		return errors.New("回溯保存未启用")
	}
	return r.triggerFn()
}

// List returns the inbox.
func (r *staticRecaller) List() ([]recall.Candidate, error) {
	if r == nil || r.listFn == nil {
		return nil, nil
	}
	return r.listFn()
}

// Get returns one candidate.
func (r *staticRecaller) Get(id string) (recall.Candidate, error) {
	if r == nil || r.getFn == nil {
		return recall.Candidate{}, errors.New("回溯保存未启用")
	}
	return r.getFn(id)
}

// WAVBytes returns the candidate audio.
func (r *staticRecaller) WAVBytes(id string) ([]byte, error) {
	if r == nil || r.wavFn == nil {
		return nil, errors.New("回溯保存未启用")
	}
	return r.wavFn(id)
}

// Delete removes a candidate.
func (r *staticRecaller) Delete(id string) error {
	if r == nil || r.deleteFn == nil {
		return errors.New("回溯保存未启用")
	}
	return r.deleteFn(id)
}

// Dir is the inbox directory.
func (r *staticRecaller) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

// RingSeconds is the snapshot length.
func (r *staticRecaller) RingSeconds() float64 {
	if r == nil {
		return 0
	}
	return r.ringSec
}

// CoverSeconds is the buffered audio length.
func (r *staticRecaller) CoverSeconds() float64 {
	if r == nil || r.coverSec == nil {
		return 0
	}
	return r.coverSec()
}

// HotkeyStatus is the one-line hotkey description.
func (r *staticRecaller) HotkeyStatus() string {
	if r == nil || r.hotkey == nil {
		return ""
	}
	return r.hotkey()
}

// MaxFiles is the retention limit.
func (r *staticRecaller) MaxFiles() int {
	if r == nil {
		return 0
	}
	return r.maxFiles
}

// EnsureCapture runs the caller's lazy-capture hook once.
func (r *staticRecaller) EnsureCapture() error {
	if r == nil || r.ensureFn == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.captureDone {
		return r.captureErr
	}
	r.captureDone = true
	r.captureErr = r.ensureFn()
	return r.captureErr
}

// RecallOptions is the function set cmd/soundradar uses to install the P4 sink.
type RecallOptions struct {
	Enabled    bool
	Dir        string
	RingSec    float64
	MaxFiles   int
	CoverSec   func() float64
	Hotkey     func() string
	Save       func(source string) (recall.Candidate, error)
	Trigger    func() error
	List       func() ([]recall.Candidate, error)
	Get        func(id string) (recall.Candidate, error)
	WAVBytes   func(id string) ([]byte, error)
	Delete     func(id string) error
	EnsureFunc func() error
}

// NewRecallSink builds the adapter the server exposes through /api/candidates.
func NewRecallSink(o RecallOptions) RecallSink {
	return &staticRecaller{
		enabled:   o.Enabled,
		dir:       o.Dir,
		ringSec:   o.RingSec,
		maxFiles:  o.MaxFiles,
		coverSec:  o.CoverSec,
		hotkey:    o.Hotkey,
		saveFn:    o.Save,
		triggerFn: o.Trigger,
		listFn:    o.List,
		getFn:     o.Get,
		wavFn:     o.WAVBytes,
		deleteFn:  o.Delete,
		ensureFn:  o.EnsureFunc,
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func validateCandidateID(id string) error {
	if id == "" {
		return errors.New("候选项 id 为空")
	}
	if len(id) > 64 {
		return errors.New("候选项 id 太长")
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '-' || r == '_':
		default:
			return fmt.Errorf("候选项 id 含非法字符 %q", string(r))
		}
	}
	return nil
}

// defaultCandidateName builds a name from the candidate id, which always carries
// its creation timestamp ("000004-20260101-120000").
func defaultCandidateName(c recall.Candidate) string {
	if len(c.ID) >= 15 {
		if ts, err := time.ParseInLocation("20060102-150405", c.ID[len(c.ID)-15:], time.Local); err == nil {
			return "候选项 " + ts.Format("2006-01-02 15:04:05")
		}
	}
	if c.ID != "" {
		return "候选项 " + c.ID
	}
	return "候选项"
}

func normalizeTags(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range in {
		p := strings.TrimSpace(t)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func derefFloat(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

func derefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func orDefaultString(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
