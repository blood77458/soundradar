// Package match turns index scores into sound-effect events.
//
// The engine is deliberately stateful but tiny: the realtime capture loop of P3
// pushes one normalised window per hop (5.333 ms) into Tick and gets back at
// most one event. Everything time related comes from the caller's `now`, so the
// package is trivially testable and has no goroutines or timers of its own.
//
// # Debounce strategy
//
// Three independent gates have to pass before an event is reported:
//
//  1. SINCE  the window's level must be at or above SilenceDBFS. A silent or
//     near-silent window is skipped before any scoring happens at all, which is
//     what stops a paused game from matching whatever is closest to silence.
//  2. THRESHOLD + MARGIN  the best item must reach its threshold (per item when
//     set, otherwise Options.DefaultThreshold) and must beat the runner-up by
//     Options.MinMargin. The margin gate is what makes "BGM that resembles
//     everything" fail instead of firing every 5 ms.
//  3. COOLDOWN + GLOBAL REFRACTORY  the winner must not have fired within its
//     own cooldown (per item when set, otherwise Options.CooldownMs), and no
//     event at all may fire within Options.RefractoryMs of the previous event.
//
// When two DIFFERENT items both pass gates 1 and 2 inside one refractory window
// the engine keeps only the higher scoring one: a single Tick cannot report two
// events, and the second Tick inside the refractory window is suppressed
// entirely. That is the "BGM paragraph must not flood the log" requirement.
//
// # Peak hold
//
// A short sound's similarity score rises and falls across hops as the analysis
// window slides over the transient. Firing on the first hop past the threshold
// therefore reports a phase-dependent score: the same clip replayed a few
// milliseconds later can look 0.05–0.15 worse. PeakHoldMs waits after the first
// cross and reports the highest score seen in that window (or flushes early when
// the score drops / the window goes silent), so repeated plays of the same
// audio land on a stable peak.
//
// The engine never suppresses silently in the sense of losing information: it
// keeps the rejected candidate as Pending, and PendingScore/PendingID expose it
// for diagnostics.
package match

import (
	"math"
	"sync"
	"time"

	"github.com/znz/soundradar/internal/index"
)

// Event is one recognised sound effect.
type Event struct {
	ID        string
	Name      string
	Score     float64
	Margin    float64 // top1 - top2 (top2 is the best OTHER item, or -Inf)
	Time      time.Time
	LevelDBFS float64
}

// Options configures the debounce gates.
type Options struct {
	// DefaultThreshold is used for items without their own threshold.
	DefaultThreshold float64
	// MinMargin is how far ahead of the runner-up the winner must be.
	MinMargin float64
	// SilenceDBFS skips windows quieter than this before scoring.
	SilenceDBFS float64
	// CooldownMs is the per-item cooldown used when an item has none of its own.
	CooldownMs int
	// RefractoryMs is the global "no event at all" window: two different items
	// cannot both fire inside it.
	RefractoryMs int
	// PeakHoldMs is how long after the first threshold-cross the engine waits
	// to capture the peak score before reporting the event. 0 means "use the
	// default"; a negative value disables peak hold (fire on first cross).
	PeakHoldMs int
	// TopN caps the score list returned by Tick (0 = every item).
	TopN int
}

// DefaultOptions returns the values recommended by the P2 specification.
func DefaultOptions() Options {
	return Options{
		DefaultThreshold: 0.75,
		MinMargin:        0.05,
		SilenceDBFS:      -60,
		CooldownMs:       400,
		RefractoryMs:     50,
		PeakHoldMs:       48, // ~9 hops at 5.333 ms; enough to catch a click peak
		TopN:             5,
	}
}

// normalize fills in sane values for zero fields so a hand-built Options cannot
// silently disable a gate (an all-zero struct would otherwise allow everything).
func (o Options) normalize() Options {
	if o.DefaultThreshold <= 0 {
		o.DefaultThreshold = DefaultOptions().DefaultThreshold
	}
	if o.SilenceDBFS == 0 {
		o.SilenceDBFS = DefaultOptions().SilenceDBFS
	}
	if o.CooldownMs <= 0 {
		o.CooldownMs = DefaultOptions().CooldownMs
	}
	if o.RefractoryMs <= 0 {
		o.RefractoryMs = DefaultOptions().RefractoryMs
	}
	if o.PeakHoldMs == 0 {
		o.PeakHoldMs = DefaultOptions().PeakHoldMs
	}
	return o
}

// peakHold tracks a threshold-crossing candidate until its rising edge peaks
// (or PeakHoldMs elapses / the score drops / the window goes silent).
type peakHold struct {
	id, name      string
	score, margin float64
	level         float64
	armedAt       time.Time
}

// Engine is the detection + debounce state machine.
type Engine struct {
	ix   *index.Index
	opts Options

	mu         sync.Mutex
	thresholds map[string]float64
	lastFire   map[string]time.Time
	lastAny    time.Time
	hold       *peakHold

	// diagnostics for the most recent Tick
	pendingID     string
	pendingScore  float64
	ticks         int64
	fired         int64
	skippedSilent int64
	rejected      int64
}

// NewEngine builds an engine over ix. A nil index is allowed: every Tick then
// returns (nil, nil), which keeps P3 code simple when no library exists yet.
func NewEngine(ix *index.Index, opts Options) *Engine {
	return &Engine{
		ix:         ix,
		opts:       opts.normalize(),
		thresholds: make(map[string]float64),
		lastFire:   make(map[string]time.Time),
	}
}

// Options returns the effective options.
func (e *Engine) Options() Options { return e.opts }

// SetThreshold overrides an item's threshold (the P1 management UI can push
// per-item values here). A value <= 0 removes the override.
func (e *Engine) SetThreshold(id string, v float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v <= 0 {
		delete(e.thresholds, id)
		return
	}
	e.thresholds[id] = v
}

// Threshold returns the effective threshold of an item.
func (e *Engine) Threshold(id string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.thresholdLocked(id)
}

// thresholdLocked is Threshold for callers that already hold e.mu (Tick does).
func (e *Engine) thresholdLocked(id string) float64 {
	if v, ok := e.thresholds[id]; ok {
		return v
	}
	return e.opts.DefaultThreshold
}

// SetMinMargin changes the margin gate at runtime.
func (e *Engine) SetMinMargin(v float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opts.MinMargin = v
}

// SetSilenceDBFS changes the silence gate at runtime. The realtime link uses it
// to follow the measured noise floor (see live.Options.AdaptiveGate), so a noisy
// game stops scoring empty windows without the user having to edit config.json.
// A non-finite value is ignored: the gate must stay comparable.
func (e *Engine) SetSilenceDBFS(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opts.SilenceDBFS = v
}

// SilenceDBFS returns the silence gate currently in force.
func (e *Engine) SilenceDBFS() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.opts.SilenceDBFS
}

// Stats reports how many ticks were seen and how the gates filtered them.
func (e *Engine) Stats() (ticks, fired, skippedSilent, rejected int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ticks, e.fired, e.skippedSilent, e.rejected
}

// Pending returns the best candidate that was seen but NOT reported (because of
// the threshold, the margin or a cooldown), for diagnostics.
func (e *Engine) Pending() (id string, score float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pendingID, e.pendingScore
}

// Tick scores one window and returns the event it may have produced plus the
// current top list.
//
// window must be a normalised unit vector of ix.Dim() values; nil (no window
// yet) returns (nil, nil) without scoring. A sub-SilenceDBFS level skips
// scoring but flushes an armed peak-hold as an event. The caller owns the
// returned slice.
func (e *Engine) Tick(window []float32, levelDBFS float64, now time.Time) (*Event, []index.ItemScore) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.ticks++
	if e.ix == nil || window == nil || e.ix.Empty() {
		return nil, nil
	}
	if levelDBFS < e.opts.SilenceDBFS || math.IsInf(levelDBFS, -1) {
		e.skippedSilent++
		e.pendingID, e.pendingScore = "", 0
		if e.hold != nil {
			return e.fireHoldLocked(now), nil
		}
		return nil, nil
	}

	top := e.ix.Search(window, e.opts.TopN)
	if len(top) == 0 {
		if e.hold != nil {
			return e.fireHoldLocked(now), nil
		}
		return nil, nil
	}
	best := top[0]
	bestScore := best.Score.Float()
	second := math.Inf(-1)
	for _, s := range top[1:] {
		if s.ID != best.ID && s.Score.Float() > second {
			second = s.Score.Float()
		}
	}
	margin := math.Inf(1)
	if !math.IsInf(second, -1) {
		margin = bestScore - second
	}

	// Peak-hold update / flush before arming a new candidate.
	if e.hold != nil {
		h := e.hold
		same := best.ID == h.id
		if same && bestScore > h.score {
			h.score = bestScore
			h.margin = margin
			h.level = levelDBFS
			h.name = best.Name
		}
		drop := same && bestScore < h.score-0.02
		elapsed := e.opts.PeakHoldMs > 0 &&
			now.Sub(h.armedAt) >= time.Duration(e.opts.PeakHoldMs)*time.Millisecond
		below := same && (bestScore < e.thresholdLocked(h.id) || margin < e.opts.MinMargin)
		if !same || drop || elapsed || below {
			return e.fireHoldLocked(now), top
		}
		e.pendingID, e.pendingScore = h.id, h.score
		return nil, top
	}

	// gate: threshold (per item) and margin
	if bestScore < e.thresholdLocked(best.ID) || margin < e.opts.MinMargin {
		e.rejected++
		e.pendingID, e.pendingScore = best.ID, bestScore
		return nil, top
	}
	// gate: per-item cooldown
	if last, ok := e.lastFire[best.ID]; ok && now.Sub(last) < e.cooldown(best.ID) {
		e.rejected++
		e.pendingID, e.pendingScore = best.ID, bestScore
		return nil, top
	}
	// gate: global refractory (a different item also passing the gates inside
	// this window would be dropped - only one event per refractory window)
	if !e.lastAny.IsZero() && now.Sub(e.lastAny) < e.refractory() {
		e.rejected++
		e.pendingID, e.pendingScore = best.ID, bestScore
		return nil, top
	}

	// Peak hold disabled → fire on first cross (legacy / unit-test path).
	if e.opts.PeakHoldMs < 0 {
		return e.commitLocked(best.ID, best.Name, bestScore, margin, levelDBFS, now), top
	}

	e.hold = &peakHold{
		id: best.ID, name: best.Name,
		score: bestScore, margin: margin, level: levelDBFS,
		armedAt: now,
	}
	e.pendingID, e.pendingScore = best.ID, bestScore
	return nil, top
}

func (e *Engine) fireHoldLocked(now time.Time) *Event {
	h := e.hold
	if h == nil {
		return nil
	}
	e.hold = nil
	return e.commitLocked(h.id, h.name, h.score, h.margin, h.level, now)
}

func (e *Engine) commitLocked(id, name string, score, margin, level float64, now time.Time) *Event {
	e.lastFire[id] = now
	e.lastAny = now
	e.fired++
	e.pendingID, e.pendingScore = "", 0
	return &Event{
		ID:        id,
		Name:      name,
		Score:     score,
		Margin:    margin,
		Time:      now,
		LevelDBFS: level,
	}
}

// cooldown returns the per-item cooldown. Per-item overrides live in the index
// items of the P1 library; they are read through index.Index when available,
// otherwise the default applies.
func (e *Engine) cooldown(id string) time.Duration {
	if e.ix != nil {
		if ms := e.ix.CooldownMs(id); ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return time.Duration(e.opts.CooldownMs) * time.Millisecond
}

func (e *Engine) refractory() time.Duration {
	return time.Duration(e.opts.RefractoryMs) * time.Millisecond
}

// Reset forgets every cooldown and counter (used when the library or the
// thresholds change).
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastFire = make(map[string]time.Time)
	e.lastAny = time.Time{}
	e.hold = nil
	e.pendingID, e.pendingScore = "", 0
	e.ticks, e.fired, e.skippedSilent, e.rejected = 0, 0, 0, 0
}
