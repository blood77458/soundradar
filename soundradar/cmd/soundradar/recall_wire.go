package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/znz/soundradar/internal/config"
	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/hotkey"
	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/live"
	"github.com/znz/soundradar/internal/recall"
	"github.com/znz/soundradar/internal/wav"
)

// recallRecorder is the CLI-side wiring of the P4 feature: it owns the candidate
// inbox, the ring buffer and the global hotkey that saves a snapshot.
//
// It is shared by `overlay`, `serve` and `live`, so the three subcommands cannot
// drift apart. A nil *recallRecorder is valid and means "recall is off".
type recallRecorder struct {
	rec   *recall.Recaller
	store *recall.Store
	hk    *hotkey.Manager
	// label is the configured hotkey spelling ("F8").
	label string
	// registered reports whether RegisterHotKey actually succeeded.
	registered bool
	// regErr is the Chinese reason when it did not.
	regErr string
	quiet  bool
	// notify, when set, receives every saved candidate (the HTTP layer uses it
	// to publish an SSE event; the CLI leaves it nil and prints instead).
	notify func(recall.Candidate)

	mu    sync.Mutex
	saves int
}

// startRecaller opens the candidate inbox, prepares the ring and registers the
// recall hotkey. It returns (nil, nil) when the feature is disabled in the
// configuration, so callers can simply not care.
func startRecaller(cfg *config.Config, quiet bool) (*recallRecorder, error) {
	if cfg == nil || !cfg.Recall.Enabled {
		return nil, nil
	}
	dir, err := config.ResolveRecallDir(cfg.Recall.Dir)
	if err != nil {
		return nil, err
	}
	store, err := recall.NewStore(dir, cfg.Recall.MaxFiles)
	if err != nil {
		return nil, err
	}
	r := &recallRecorder{
		rec:   recall.NewRecaller(cfg.Recall.Seconds, store),
		store: store,
		label: strings.TrimSpace(cfg.Hotkeys.RecallLabel),
		quiet: quiet,
	}
	if r.label == "" {
		r.label = "none"
	}

	// The hotkey is best-effort: a busy F8 must never stop the recognition link
	// from running, it only means "press the key" is unavailable and the
	// management page / `soundradar recall` have to be used instead.
	mgr, hkErr := hotkey.NewManager()
	if hkErr != nil {
		r.regErr = hkErr.Error()
		return r, nil
	}
	r.hk = mgr
	if err := mgr.Register("recall", r.label, r.OnHotkey); err != nil {
		r.regErr = err.Error()
		return r, nil
	}
	if !mgr.OK("recall") {
		r.regErr = mgr.Error("recall")
		return r, nil
	}
	r.registered = true
	return r, nil
}

// OnHotkey is the callback RegisterHotKey ends up invoking. It runs on the
// hotkey message thread, so it must not block: the actual snapshot (a copy of up
// to 30 s of audio plus two file writes) happens on a fresh goroutine, exactly
// as the P4 spec requires.
func (r *recallRecorder) OnHotkey() {
	go r.Save("hotkey")
}

// Buffer is the live.Options.OnBlock tap.
func (r *recallRecorder) Buffer(block []float32) { r.rec.PushBlock(block) }

// Observe is the live.Options.OnTick tap: it keeps the "guess" as fresh as the
// ranking is, which is what a candidate saved right now should be labelled with.
//
// It deliberately does NOT watch for silence. The realtime views already show a
// live level meter (the `live` panel and the 实时打分 tab), so "am I hearing
// anything at all?" is answerable by looking at it; a timed popup on top of that
// is noise, and it would fire during every pause, menu and loading screen.
func (r *recallRecorder) Observe(tk live.Tick) {
	r.rec.Tick(tk.Top)
}

// Store exposes the candidate inbox.
func (r *recallRecorder) Store() *recall.Store {
	if r == nil {
		return nil
	}
	return r.store
}

// Saves is the number of candidates written by this recorder.
func (r *recallRecorder) Saves() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saves
}

// Save snapshots the ring right now and always reports the outcome.
func (r *recallRecorder) Save(source string) (recall.Candidate, error) {
	if r == nil || r.rec == nil {
		return recall.Candidate{}, errors.New("回溯保存未启用（配置里 recall.enabled=false）")
	}
	if source == "" {
		source = "hotkey"
	}
	start := time.Now()
	cand, err := r.rec.Save(source)
	if err != nil {
		if !r.quiet {
			fmt.Fprintf(os.Stderr, "[recall] 保存失败: %v\n", err)
		}
		return recall.Candidate{}, err
	}
	r.mu.Lock()
	r.saves++
	r.mu.Unlock()
	if !r.quiet {
		fmt.Printf("[recall] 已保存候选项 %s（耗时 %s）\n", recall.Describe(cand), time.Since(start).Round(time.Millisecond))
	}
	if r.notify != nil {
		r.notify(cand)
	}
	return cand, nil
}

// SetNotify installs the save notification sink.
func (r *recallRecorder) SetNotify(fn func(recall.Candidate)) {
	if r == nil {
		return
	}
	r.notify = fn
}

// Trigger invokes the registered hotkey callback WITHOUT any keyboard input.
// It is the "internal trigger" the P4 acceptance run uses to prove that
// RegisterHotKey -> callback -> saved candidate is wired up.
func (r *recallRecorder) Trigger() error {
	if r == nil || r.hk == nil {
		return errors.New("回溯保存未启用（没有热键管理器）")
	}
	return r.hk.Trigger("recall")
}

// Enabled reports whether the recorder exists.
func (r *recallRecorder) Enabled() bool { return r != nil && r.rec != nil }

// RingSeconds is the length of one snapshot.
func (r *recallRecorder) RingSeconds() float64 {
	if r == nil || r.rec == nil {
		return 0
	}
	return float64(r.rec.Ring().Cap()) / recall.SampleRate
}

// CoverSeconds is how much audio is in the ring right now.
func (r *recallRecorder) CoverSeconds() float64 {
	if r == nil || r.rec == nil {
		return 0
	}
	return r.rec.Seconds()
}

// HotkeyStatus renders the one-line banner text for the recall hotkey.
func (r *recallRecorder) HotkeyStatus() string {
	if r == nil {
		return "未启用（配置 recall.enabled=false）"
	}
	if r.registered {
		return fmt.Sprintf("%s（RegisterHotKey 成功，回调 → 保存最近 %.1f 秒）", r.label, r.RingSeconds())
	}
	if r.regErr != "" {
		return fmt.Sprintf("%s 注册失败（%s）；仍可用管理端「立即保存」或 /api/recall/trigger", r.label, r.regErr)
	}
	return "未注册"
}

// Close releases the hotkey and its window. Idempotent.
func (r *recallRecorder) Close() {
	if r == nil || r.hk == nil {
		return
	}
	_ = r.hk.Close()
	r.hk = nil
	r.registered = false
}

// levelText renders a candidate's level as "-inf" or a dBFS number; it is shared
// by the CLI banners and the serve log.
func levelText(c recall.Candidate) string {
	if c.Silent {
		return "-inf"
	}
	return wav.FormatDBFS(c.PeakDBFS)
}

// ---------------------------------------------------------------------------
// fallback capture: keeps the ring warm without a recognition session
// ---------------------------------------------------------------------------

// captureRecorder feeds a recall ring from a plain loopback capture, so
// POST /api/recall/trigger works in `serve` even when no realtime session is
// running. Without it the button would always answer "环形缓冲里还没有音频".
//
// It also runs the same log-mel analyzer the realtime link uses, so a candidate
// saved this way still carries a top-1 "guess".
type captureRecorder struct {
	src live.FrameSource
	rec *recallRecorder
	ix  *index.Index

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	once sync.Once
	err  error
}

// startCaptureRecorder opens the given endpoint ("" = system default) and pumps
// its audio into the recall ring until stop() is called.
func startCaptureRecorder(ctx context.Context, device string, rec *recallRecorder, ix *index.Index) (*captureRecorder, error) {
	src, err := live.NewCaptureSource(device)
	if err != nil {
		return nil, err
	}
	c, cancel := context.WithCancel(ctx)
	cr := &captureRecorder{src: src, rec: rec, ix: ix, ctx: c, cancel: cancel, done: make(chan struct{})}
	if err := src.Start(); err != nil {
		cancel()
		_ = src.Stop()
		return nil, err
	}
	go cr.pump()
	return cr, nil
}

// recallScoringParams returns the fingerprint parameters the recall ring's
// "guess what this was" scorer uses. It mirrors what the long-lived commands use
// (serve/overlay/recall), so a candidate saved from `serve` and one saved from
// the standalone `recall` CLI score against the same pipeline.
func recallScoringParams() dsp.Params {
	return dspParamsFor(loadNoiseConfig(""))
}

// pump converts, buffers and scores until the context is cancelled.
func (cr *captureRecorder) pump() {
	defer close(cr.done)
	const (
		blockFrames = 960                   // 20 ms at 48 kHz
		tickEvery   = 50 * time.Millisecond // one ranking per 50 ms, like live
	)
	var (
		acc      []float32
		an       *dsp.Analyzer
		params   = recallScoringParams()
		lastTick = time.Now()
	)
	if cr.ix != nil && !cr.ix.Empty() {
		if a, err := dsp.NewAnalyzer(params); err == nil {
			an = a
		}
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-cr.ctx.Done():
			_ = cr.src.Stop()
			return
		case <-ticker.C:
		}
		// Drain every block the source has right now.
		for n := 0; n < 16; n++ {
			select {
			case blk, ok := <-cr.src.Frames():
				if !ok {
					_ = cr.src.Stop()
					return
				}
				acc = append(acc, blk...)
				if an != nil {
					an.Push(blk)
				}
			default:
				n = 16
			}
		}
		// Buffer in canonical blocks so the ring is filled in time order.
		for len(acc) >= blockFrames {
			cr.rec.Buffer(acc[:blockFrames])
			acc = append(acc[:0:0], acc[blockFrames:]...)
		}
		// Score the newest window for the "guess".
		if an != nil && time.Since(lastTick) >= tickEvery {
			lastTick = time.Now()
			if w := an.Window(); w != nil {
				cr.rec.Observe(live.Tick{Time: time.Now(), Top: cr.ix.Search(w, 1)})
			}
		}
		if an != nil {
			an.Trim(2 * params.WindowFrames)
		}
	}
}

// stop shuts the capture down and waits (bounded) for the pump goroutine.
func (cr *captureRecorder) stop() error {
	if cr == nil {
		return nil
	}
	cr.once.Do(func() {
		cr.cancel()
		select {
		case <-cr.done:
		case <-time.After(3 * time.Second):
			cr.err = errors.New("停止回溯采集超时（3 秒）")
		}
	})
	return cr.err
}

// ---------------------------------------------------------------------------
// offline: feed a recall ring from a file (tests / --wav)
// ---------------------------------------------------------------------------

// feedFileRecaller pushes a WAV/MP3 file through a recall ring in 20 ms blocks,
// exactly like the realtime path does, so an offline run exercises the same ring
// and the same store.
func feedFileRecaller(path string, rc *recall.Recaller, ix *index.Index) error {
	src, err := live.NewFileSource(path, false)
	if err != nil {
		return err
	}
	if err := src.Start(); err != nil {
		return err
	}
	defer src.Stop()
	return feedFrameSource(src, rc, ix)
}

// feedFileRecorder is feedFileRecaller for a CLI recorder.
func feedFileRecorder(path string, rec *recallRecorder, ix *index.Index) error {
	return feedFileRecaller(path, rec.rec, ix)
}

// feedFrameSource is the shared "drain a FrameSource into a ring" helper.
func feedFrameSource(src live.FrameSource, rc *recall.Recaller, ix *index.Index) error {
	params := recallScoringParams()
	var (
		an  *dsp.Analyzer
		acc []float32
	)
	if ix != nil && !ix.Empty() {
		if a, err := dsp.NewAnalyzer(params); err == nil {
			an = a
		}
	}
	const blockFrames = 960
	for blk := range src.Frames() {
		if an != nil {
			an.Push(blk)
		}
		acc = append(acc, blk...)
		for len(acc) >= blockFrames {
			rc.PushBlock(acc[:blockFrames])
			acc = append(acc[:0:0], acc[blockFrames:]...)
		}
		if an != nil {
			if w := an.Window(); w != nil {
				rc.Tick(ix.Search(w, 1))
			}
			an.Trim(2 * params.WindowFrames)
		}
	}
	if len(acc) > 0 {
		rc.PushBlock(acc)
	}
	return nil
}
