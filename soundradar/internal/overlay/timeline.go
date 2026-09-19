package overlay

import (
	"image"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Fade state machine - pure Go, unit-testable
// ---------------------------------------------------------------------------
//
// One event's life is:
//
//	0 ────────── FadeInMs ────────── DurationMs ────────── FadeOutMs ──────> gone
//	alpha 0 ──> 1              alpha 1 (hold)          alpha 1 ──> 0
//
// Alpha() is monotone on each side of the hold, which the unit tests pin down.

// FadeAlpha returns the global alpha of an event that is ageMs old.
// It is the only place the fade curve is implemented.
func FadeAlpha(ageMs, fadeInMs, durationMs, fadeOutMs int) float64 {
	if ageMs < 0 {
		ageMs = 0
	}
	if fadeInMs < 0 {
		fadeInMs = 0
	}
	if durationMs < 0 {
		durationMs = 0
	}
	if fadeOutMs < 0 {
		fadeOutMs = 0
	}
	if fadeInMs > 0 && ageMs < fadeInMs {
		return float64(ageMs) / float64(fadeInMs)
	}
	held := ageMs - fadeInMs
	if held < durationMs {
		return 1
	}
	if fadeOutMs <= 0 {
		return 0
	}
	faded := held - durationMs
	if faded >= fadeOutMs {
		return 0
	}
	return 1 - float64(faded)/float64(fadeOutMs)
}

// TotalLifetimeMs is when an event disappears completely.
func TotalLifetimeMs(fadeInMs, durationMs, fadeOutMs int) int {
	return maxInt(0, fadeInMs) + maxInt(0, durationMs) + maxInt(0, fadeOutMs)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// entry is one queued event with its own start time.
type entry struct {
	ID    string
	Name  string
	Score float64
	Icon  image.Image
	Start time.Time
}

// ---------------------------------------------------------------------------
// Queue
// ---------------------------------------------------------------------------

// Queue is the visible-event queue: newest last, bounded by MaxSimultaneous
// (the OLDEST event is dropped when a new one arrives and the queue is full).
//
// It is safe for concurrent use: Show() is called from the recognition
// goroutine while Visible() is called from the window thread.
type Queue struct {
	mu      sync.Mutex
	items   []entry
	max     int
	cfg     Timing
	dropped int64
	seq     int64
}

// Timing groups the durations that define an event's life.
type Timing struct {
	FadeInMs   int
	DurationMs int
	FadeOutMs  int
}

// NewQueue builds a queue. max <= 0 is treated as 1.
func NewQueue(max int, t Timing) *Queue {
	if max <= 0 {
		max = 1
	}
	return &Queue{max: max, cfg: t}
}

// SetTiming updates the fade durations (hot config change; live events keep
// their start time and simply follow the new curve).
func (q *Queue) SetTiming(t Timing) {
	q.mu.Lock()
	q.cfg = t
	q.mu.Unlock()
}

// Timing returns the current durations.
func (q *Queue) Timing() Timing {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cfg
}

// SetMax changes the capacity; when it shrinks, the oldest events are dropped.
func (q *Queue) SetMax(n int) {
	if n <= 0 {
		n = 1
	}
	q.mu.Lock()
	q.max = n
	if len(q.items) > q.max {
		drop := len(q.items) - q.max
		q.items = append([]entry(nil), q.items[drop:]...)
		q.dropped += int64(drop)
	}
	q.mu.Unlock()
}

// Max returns the capacity.
func (q *Queue) Max() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.max
}

// Show enqueues an event and returns its sequence number. When the queue is
// full the oldest event is dropped first (never the new one: the newest hit is
// the most interesting).
//
// 同一个音效（ID 相同）已经在显示时，**刷新它那一个格子**（更新分数/图标并把
// 它的计时重置、挪到最右），而不是再占一个新格子。否则持续的声音（引擎声、
// 连续脚步、循环播放的提示音）会在几百毫秒内把 maxSimultaneous 个格子全占满，
// 并把其它真正不同的音效挤出去。
func (q *Queue) Show(it DisplayItem) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	now := time.Now()
	if it.ID != "" {
		for i := range q.items {
			if q.items[i].ID != it.ID {
				continue
			}
			e := q.items[i]
			e.Name, e.Score, e.Icon = it.Name, it.Score, it.Icon
			e.Start = now
			q.items = append(q.items[:i], q.items[i+1:]...)
			q.items = append(q.items, e)
			return q.seq
		}
	}
	if len(q.items) >= q.max {
		// Drop the oldest.
		drop := len(q.items) - q.max + 1
		q.items = append([]entry(nil), q.items[drop:]...)
		q.dropped += int64(drop)
	}
	q.items = append(q.items, entry{
		ID: it.ID, Name: it.Name, Score: it.Score, Icon: it.Icon, Start: now,
	})
	return q.seq
}

// Len returns the number of queued events.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Dropped counts events evicted because the queue was full.
func (q *Queue) Dropped() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// Clear removes every queued event.
func (q *Queue) Clear() {
	q.mu.Lock()
	q.items = nil
	q.mu.Unlock()
}

// Snapshot removes every event that has finished fading out and returns the
// remaining ones as DisplayItems with their AgeMs filled in. It is called once
// per animation frame from the window thread, which is also when the "started
// at" time is read - so the age is measured against the same clock the fade
// curve uses.
func (q *Queue) Snapshot(now time.Time) []DisplayItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	life := TotalLifetimeMs(q.cfg.FadeInMs, q.cfg.DurationMs, q.cfg.FadeOutMs)
	kept := q.items[:0:0]
	out := make([]DisplayItem, 0, len(q.items))
	for _, e := range q.items {
		age := int(now.Sub(e.Start).Milliseconds())
		if life > 0 && age >= life {
			// Fully faded out: remove it. (This is expiry, not a capacity
			// eviction, so Dropped does not count it.)
			continue
		}
		kept = append(kept, e)
		out = append(out, DisplayItem{
			ID: e.ID, Name: e.Name, Score: e.Score, Icon: e.Icon, AgeMs: age,
		})
	}
	q.items = kept
	return out
}

// AlphaFor returns the alpha of a DisplayItem under timing t.
func AlphaFor(ageMs int, t Timing) float64 {
	return FadeAlpha(ageMs, t.FadeInMs, t.DurationMs, t.FadeOutMs)
}

// OverallAlpha is the single global alpha the canvas is multiplied by. It is the
// MAXIMUM alpha over the visible events, which is the only choice that behaves:
//
//   - a lone event fades in, holds and fades out with its own curve;
//   - when a second hit arrives mid-fade-out of the first, the canvas does not
//     dip to the dying event's almost-zero alpha and make the NEW hit invisible
//     for the length of its own fade-in.
//
// Events fade individually only in the sense that they enter and leave the
// queue at different times; the window is one canvas and therefore has one
// global alpha, exactly as RenderOptions.Alpha documents.
func OverallAlpha(items []DisplayItem, t Timing) float64 {
	if len(items) == 0 {
		return 0
	}
	best := -1.0
	for _, it := range items {
		if a := FadeAlpha(it.AgeMs, t.FadeInMs, t.DurationMs, t.FadeOutMs); a > best {
			best = a
		}
	}
	if best < 0 {
		return 0
	}
	return best
}
