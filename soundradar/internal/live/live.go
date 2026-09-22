// Package live is the soundradar P2 realtime recognition link: it wires
// capture -> feature analysis -> quantised index search -> debounced events
// together and reports a per-tick scoring snapshot for the management UI.
//
// # Shape of the pipeline
//
//	FrameSource ──blocks──> bounded queue ──> dsp.Analyzer ──window──> match.Engine
//	 (loopback / file /      (decoupled,        (one window per          (gates:
//	  synthetic)              counted drops)     HopSize, 5.333 ms)       silence,
//	                                                                      threshold,
//	                                                                      margin,
//	                                                                      cooldown)
//
// Three properties are deliberate:
//
//  1. Capture is never blocked. The source delivers blocks through its own
//     buffer; the engine's feeder goroutine does a BLOCKING hand-off into the
//     engine queue, so backpressure reaches the source's buffer, where the real
//     loopback source counts what it had to drop (capture.Stream.Dropped)
//     instead of stalling WASAPI. A synthetic "publish faster than the engine
//     can consume" source behaves the same way. Nothing is lost silently: every
//     dropped block is counted and surfaces in Stats.Dropped.
//
//  2. Memory is bounded for an hours-long run. Every block ends with
//     dsp.Analyzer.Trim, which keeps the newest windows plus just enough
//     samples for the next frame; without it the analyzer's sample ring grows
//     by 192 kB/s (659 MiB/h) forever.
//
//  3. Time is the AUDIO clock (samples processed / 48 kHz) offset by the wall
//     clock at Run start, not the wall clock at the moment a block happened to
//     be processed. That makes the event timeline a property of the audio - a
//     file replay produces the same timestamps no matter how fast the machine
//     is - and keeps ticks evenly spaced when the process is descheduled.
package live

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/znz/soundradar/internal/capture"
	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/match"
)

// Canonical realtime format. The whole recognition path only ever sees this
// (internal/dsp and internal/index are defined on it), so a source is
// responsible for downmixing and resampling.
const (
	// SampleRate is the canonical rate of every sample in this package.
	SampleRate = 48000
	// BlockFrames is the nominal audio block ("20 ms chunk") the built-in
	// sources emit and the engine measures its per-block processing cost on.
	BlockFrames = 960
)

// RealtimeBudgetMs is the per-20-ms-of-audio processing budget the P2 spec
// asks for; Stats.MsPer20msAudio is the number to compare against it.
const RealtimeBudgetMs = 10.0

// FrameSource is a stream of mono 48 kHz float32 audio in [-1, 1].
//
// Start begins producing, Stop releases the device or file and is idempotent,
// and closing the Frames channel means "no more audio", never "an error" (see
// ErrorReporter).
type FrameSource interface {
	Start() error
	// Frames returns the audio blocks; the channel is closed when the source is
	// exhausted or stopped.
	Frames() <-chan []float32
	Stop() error
}

// ErrorReporter is implemented by sources that can fail mid-stream (a USB
// headset unplugged, a truncated file). The engine reports that error from Run
// instead of pretending the stream ended normally.
type ErrorReporter interface{ Err() error }

// DropReporter is implemented by sources that discard audio to stay
// realtime-safe. Stats.Dropped includes their count.
type DropReporter interface{ Dropped() int64 }

// SourceInfo describes a source for the startup banner and the HTTP API.
type SourceInfo struct {
	Kind     string // "capture" | "file" | "buffer" | "custom"
	Detail   string // human readable one-liner
	Device   string // endpoint friendly name (capture only)
	Path     string // file path (file only)
	Rate     int    // source rate before conversion
	Channels int
	Format   string
	Seconds  float64 // duration when known
	Realtime bool
}

// InfoReporter is implemented by the built-in sources.
type InfoReporter interface{ Info() SourceInfo }

// StreamNoteReporter is implemented by sources that had to substitute an
// endpoint or a format to start capturing. The returned note is empty when the
// requested endpoint accepted its own mix format, and skipped lists the
// endpoints that were tried and refused first (oldest first).
type StreamNoteReporter interface {
	StreamNote() (note string, skipped []capture.EndpointFailure)
}

// EventSink receives recognised events.
type EventSink interface{ OnEvent(ev match.Event) }

// ScoreSink receives the per-tick scoring snapshot.
type ScoreSink interface{ OnTick(t Tick) }

// Tick is one scoring snapshot published to the panel/CLI. Top is sorted by
// descending score.
type Tick struct {
	Time      time.Time
	LevelDBFS float64
	// DenoisedDBFS is LevelDBFS after the latest frame's noise-reduction gain.
	DenoisedDBFS float64
	Top          []index.ItemScore
	Event        *match.Event

	// Silent reports that this tick reused the previous ranking because the
	// window was below the silence gate (Top is then the last scored one).
	Silent bool
	// AudioTime is the position in the audio timeline this tick describes.
	AudioTime time.Duration
	// Stats is the cumulative engine state at this tick.
	Stats Stats
}

// Stats is the cumulative realtime state. Every duration is measured on the
// processing goroutine with time.Now, so the numbers cover feature extraction
// plus index search but exclude waiting for audio.
type Stats struct {
	Blocks  int64 // audio blocks processed
	Windows int64 // hop windows scored (one index search each)
	Ticks   int64 // ticks published
	Events  int64 // events fired
	Samples int64 // 48 kHz mono samples processed

	// Dropped counts audio blocks the SOURCE had to discard because the engine
	// was behind (see DropReporter). It stays 0 for a source that applies
	// backpressure instead (the file source does).
	Dropped int64
	// SilentWindows counts windows that the silence gate skipped before any
	// scoring happened (match.Engine's skippedSilent counter).
	SilentWindows int64
	// SilentTicks counts published ticks whose Top is a reused ranking.
	SilentTicks int64

	AudioSeconds float64
	Elapsed      time.Duration

	// AvgBlockMs / MaxBlockMs are the wall-clock cost of processing one
	// delivered block (all hops of that block: feature + match).
	AvgBlockMs float64
	MaxBlockMs float64
	// MsPer20msAudio normalises that cost to 20 ms of audio, which is the
	// comparable "must stay below realtime" number for any block size:
	// MsPer20msAudio = totalProcessTime / audioSeconds * 0.020.
	MsPer20msAudio float64

	// HeapInuseBytes is runtime.MemStats.HeapInuse (Go has no portable RSS, and
	// HeapInuse is the number that actually shows an analyzer leak).
	HeapInuseBytes uint64
	// HeapSampleAt is when HeapInuseBytes was last refreshed (it is cached for
	// at most 500 ms because runtime.ReadMemStats is comparatively expensive).
	HeapSampleAt time.Time

	StartedAt   time.Time
	LastEventAt time.Time
}

// Options configures the engine. The zero value is usable: every field has a
// documented default.
type Options struct {
	// TopN is how many ranked items each tick reports. Default 8.
	TopN int
	// TickEvery is the minimum audio time between two ticks (the panel refresh
	// interval). Default 50 ms = 20 fps.
	TickEvery time.Duration
	// SilenceDBFS is passed to match.Options.SilenceDBFS. Default -60.
	SilenceDBFS float64
	// AdaptiveGate, when true, raises the silence gate above the analyzed
	// audio's own noise floor (dsp.NoiseParams.AdaptiveGate) instead of using
	// SilenceDBFS as a constant. It defaults to OFF so that a zero-value Options
	// behaves exactly as it always did; the application enables it from
	// config.json's noise.adaptiveGate.
	AdaptiveGate bool
	// Frames, when non-empty, is processed directly instead of the source
	// (offline/test injection); the engine still runs the full feature+match
	// path. src may then be nil.
	Frames []float32
	// QueueBlocks is the engine-side buffer depth. Default 16 blocks (320 ms).
	QueueBlocks int
	// KeepFrames is how many log-mel frames dsp.Analyzer.Trim retains.
	// Default 2*WindowFrames (one spare window).
	KeepFrames int
	// MatchOptions overrides the debounce gates. When it is the zero value the
	// engine uses match.DefaultOptions() (with SilenceDBFS from this struct);
	// when any field is set the struct is used AS IS, so a caller can
	// deliberately pass MinMargin 0 to disable the margin gate.
	MatchOptions match.Options

	// OnBlock, when set, receives every processed audio block in time order
	// (canonical 48 kHz mono float32, the same samples the analyzer sees), which
	// is how the P4 recall ring is filled. It is called on the Run goroutine
	// BEFORE the block is analysed, so the recall window includes the audio that
	// is only now being scored - "the sound I just heard" really is in the
	// buffer when the hotkey fires. It must not block: the P4 implementation
	// does one int16 conversion plus one slice copy.
	OnBlock func(block []float32)

	// ConfirmEnabled turns on the secondary embedding gate (mel still scores;
	// confirm only runs when mel is about to fire). Default false so a zero
	// Options keeps legacy behaviour; the app enables it from config.json.
	ConfirmEnabled bool
	// ConfirmMinScore is the minimum cosine of the confirm embedding (0..1).
	// Default 0.52.
	ConfirmMinScore float64
	// ConfirmOnsetDB is the minimum frame-energy rise (dB) required before a
	// confirm is attempted. 0 disables the onset check. Default 2.5.
	ConfirmOnsetDB float64
}

func (o Options) withDefaults() Options {
	d := match.DefaultOptions()
	if o.TopN <= 0 {
		o.TopN = 8
	}
	if o.TickEvery <= 0 {
		o.TickEvery = 50 * time.Millisecond
	}
	if o.SilenceDBFS == 0 {
		o.SilenceDBFS = d.SilenceDBFS
	}
	if o.QueueBlocks <= 0 {
		o.QueueBlocks = 16
	}
	if o.KeepFrames <= 0 {
		o.KeepFrames = 2 * dsp.DefaultParams().WindowFrames
	}
	mo := o.MatchOptions
	if mo == (match.Options{}) {
		mo = d
	}
	if mo.SilenceDBFS == 0 {
		mo.SilenceDBFS = o.SilenceDBFS
	}
	mo.TopN = o.TopN
	o.MatchOptions = mo
	if o.ConfirmMinScore <= 0 {
		o.ConfirmMinScore = 0.52
	}
	return o
}

// Engine is the realtime link. Create it with New and drive it with Run.
type Engine struct {
	src     FrameSource
	ix      *index.Index
	params  dsp.Params
	an      *dsp.Analyzer
	matcher *match.Engine
	opts    Options

	onTick  func(Tick)
	onEvent func(match.Event)

	queue chan []float32

	mu       sync.Mutex
	stats    Stats
	procTime time.Duration
	started  bool
	lastErr  error
	heapAt   time.Time
	heapVal  uint64
	// tapBroken is set once the OnBlock callback panicked; the tap is then
	// skipped for the rest of the run instead of panicking 50 times a second.
	tapBroken bool

	// --- Run-goroutine-only state (audio clock) ---
	startWall   time.Time
	pushed      int64
	lastTickPos int64
	lastTickSet bool
	tickSamples int64
	lastTop     []index.ItemScore
	lastSilent  bool

	// adaptive turns on the noise-floor-driven silence gate, and gateDBFS is the
	// value last pushed into the matcher (so the mutex is only taken when it
	// actually changed).
	adaptive bool
	gateDBFS float64
}

// New builds an engine over idx (which may be nil: the level meter and the
// packet counters still work, there are simply no items to score).
//
// onTick and onEvent are optional and are called from the goroutine that runs
// Run; they must not block (they are UI/CLI sinks).
func New(src FrameSource, idx *index.Index, opts Options, onTick func(Tick), onEvent func(match.Event)) (*Engine, error) {
	opts = opts.withDefaults()
	// Always analyse with the same parameters the index was built with. Hard-
	// coding DefaultParams here used to break live start after the user tuned
	// config.json's noise section (index fingerprint ≠ default fingerprint).
	p := dsp.DefaultParams()
	if idx != nil {
		p = idx.Params()
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("特征参数无效: %w", err)
	}
	if idx != nil && idx.Fingerprint() != p.Fingerprint() {
		return nil, fmt.Errorf("索引指纹 %s 与当前特征参数 %s 不一致，请重建索引",
			shortFP(idx.Fingerprint()), shortFP(p.Fingerprint()))
	}
	an, err := dsp.NewAnalyzer(p)
	if err != nil {
		return nil, fmt.Errorf("初始化分析器失败: %w", err)
	}
	if src == nil && len(opts.Frames) == 0 {
		return nil, errors.New("live: 需要音频源或预置帧")
	}
	tickSamples := int64(opts.TickEvery.Seconds() * SampleRate)
	if tickSamples < 1 {
		tickSamples = 1
	}
	eng := &Engine{
		src:         src,
		ix:          idx,
		params:      p,
		an:          an,
		matcher:     match.NewEngine(idx, opts.MatchOptions),
		opts:        opts,
		onTick:      onTick,
		onEvent:     onEvent,
		queue:       make(chan []float32, opts.QueueBlocks),
		tickSamples: tickSamples,
		adaptive:    opts.AdaptiveGate,
		gateDBFS:    opts.SilenceDBFS,
	}
	if opts.ConfirmEnabled && idx != nil {
		eng.matcher.SetConfirmer(&confirmGate{
			ix:       idx,
			an:       an,
			enabled:  true,
			minScore: opts.ConfirmMinScore,
			onsetDB:  opts.ConfirmOnsetDB,
		})
	}
	return eng, nil
}

// adaptGate recomputes the adaptive silence gate from the analyzer's noise-floor
// tracker and pushes it into the matcher when it moved by more than a hair.
// Called on the Run goroutine, once per audio block.
func (e *Engine) adaptGate() {
	want := e.an.GateDBFS(e.opts.MatchOptions.SilenceDBFS)
	if want == e.gateDBFS {
		return
	}
	e.gateDBFS = want
	e.matcher.SetSilenceDBFS(want)
}

// NoiseFloorDBFS returns the current environment-noise floor estimate in dBFS,
// or -Inf when the front-end has not measured anything yet. It is comparable
// with Tick.LevelDBFS and with Options.SilenceDBFS.
func (e *Engine) NoiseFloorDBFS() float64 { return e.an.NoiseFloorDBFS() }

// GateDBFS returns the silence gate currently in force (the configured one, or
// the adaptive one when that is enabled and the floor is higher).
func (e *Engine) GateDBFS() float64 {
	if !e.adaptive {
		return e.opts.MatchOptions.SilenceDBFS
	}
	return e.an.GateDBFS(e.opts.MatchOptions.SilenceDBFS)
}

func shortFP(fp string) string {
	if len(fp) > 16 {
		return fp[:16] + "…"
	}
	return fp
}

// Params returns the fingerprint parameters in use.
func (e *Engine) Params() dsp.Params { return e.params }

// Fingerprint returns the fingerprint the engine's analyzer implements.
func (e *Engine) Fingerprint() string { return e.params.Fingerprint() }

// Index returns the index being searched (may be nil).
func (e *Engine) Index() *index.Index { return e.ix }

// Options returns the effective options.
func (e *Engine) Options() Options { return e.opts }

// Matcher exposes the debounce engine so callers can push per-item thresholds
// from the management UI (SetThreshold is the shorthand).
func (e *Engine) Matcher() *match.Engine { return e.matcher }

// SetThreshold overrides one item's detection threshold (<= 0 clears it).
func (e *Engine) SetThreshold(id string, v float64) { e.matcher.SetThreshold(id, v) }

// SetMinMargin changes the margin gate at runtime.
func (e *Engine) SetMinMargin(v float64) { e.matcher.SetMinMargin(v) }

// SourceInfo describes the configured source, when it can describe itself.
func (e *Engine) SourceInfo() SourceInfo {
	if ir, ok := e.src.(InfoReporter); ok {
		return ir.Info()
	}
	if len(e.opts.Frames) > 0 {
		return SourceInfo{Kind: "buffer", Detail: "内存中的预置音频帧", Rate: SampleRate, Channels: 1,
			Seconds: float64(len(e.opts.Frames)) / SampleRate}
	}
	return SourceInfo{Kind: "custom", Detail: "自定义音频源"}
}

// Stats returns a snapshot of the cumulative counters.
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	st := e.stats
	proc := e.procTime
	started := e.startWall
	e.mu.Unlock()

	if !started.IsZero() {
		st.Elapsed = time.Since(started)
	}
	st.AudioSeconds = float64(st.Samples) / SampleRate
	if st.Blocks > 0 {
		st.AvgBlockMs = proc.Seconds() * 1000 / float64(st.Blocks)
	}
	if st.AudioSeconds > 0 {
		st.MsPer20msAudio = proc.Seconds() * 1000 / st.AudioSeconds * 0.020
	}
	st.HeapInuseBytes, st.HeapSampleAt = e.heapInuse()
	if e.src != nil {
		if dr, ok := e.src.(DropReporter); ok {
			st.Dropped += dr.Dropped()
		}
	}
	if _, _, silent, _ := e.matcher.Stats(); silent > 0 {
		st.SilentWindows = silent
	}
	return st
}

// heapInuse caches runtime.MemStats.HeapInuse for 500 ms (ReadMemStats is
// comparatively expensive and ticks arrive 20 times per second).
func (e *Engine) heapInuse() (uint64, time.Time) {
	e.mu.Lock()
	if !e.heapAt.IsZero() && time.Since(e.heapAt) < 500*time.Millisecond {
		v, at := e.heapVal, e.heapAt
		e.mu.Unlock()
		return v, at
	}
	e.mu.Unlock()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	e.mu.Lock()
	e.heapVal, e.heapAt = ms.HeapInuse, time.Now()
	v, at := e.heapVal, e.heapAt
	e.mu.Unlock()
	return v, at
}

// Err returns the error that ended the stream, if any.
func (e *Engine) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastErr
}

// Run drives the link until ctx is cancelled or the source is exhausted. It
// returns nil for both of those (a normal stop); a non-nil error means the
// source failed.
func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return errors.New("live: Engine.Run 只能调用一次")
	}
	e.started = true
	e.startWall = time.Now()
	e.stats.StartedAt = e.startWall
	e.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Injected frames: process them and finish (offline mode).
	if len(e.opts.Frames) > 0 {
		e.processAll(e.opts.Frames)
		e.finish()
		return nil
	}

	if err := e.src.Start(); err != nil {
		return err
	}
	defer e.src.Stop()

	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		for {
			select {
			case <-ctx.Done():
				return
			case blk, ok := <-e.src.Frames():
				if !ok {
					return
				}
				if len(blk) == 0 {
					continue
				}
				// Blocking hand-off: backpressure for a source that can afford
				// it (a file), while a realtime source drops inside its own
				// buffer instead of stalling WASAPI.
				select {
				case e.queue <- blk:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			e.finish()
			return nil
		case blk := <-e.queue:
			e.process(blk)
		case <-feedDone:
			for {
				select {
				case blk := <-e.queue:
					e.process(blk)
				default:
					e.finish()
					if rep, ok := e.src.(ErrorReporter); ok {
						if err := rep.Err(); err != nil {
							e.mu.Lock()
							e.lastErr = err
							e.mu.Unlock()
							return err
						}
					}
					return nil
				}
			}
		}
	}
}

// processAll runs the whole slice through process() in 20 ms blocks.
func (e *Engine) processAll(pcm []float32) {
	for off := 0; off < len(pcm); off += BlockFrames {
		e.process(pcm[off:min(off+BlockFrames, len(pcm))])
	}
}

// windowLevelMarginDB is how far the loudest frame in the feature window must
// sit above the silence gate before that window is scored on the strength of
// the frame alone. See process.
const windowLevelMarginDB = 6.0

// process analyses one delivered block and scores every hop it completes.
//
// The counters are updated as the work happens (not at the end of the block),
// so a tick published in the middle of a block already reports the block, the
// samples and the windows it belongs to.
func (e *Engine) process(blk []float32) {
	t0 := time.Now()
	hop := e.params.HopSize
	var silentTicks int

	// P4 recall tap: hand the raw block to the caller before anything else, so
	// the ring holds the audio that is about to be scored. A misbehaving tap is
	// contained: the recognition link must survive whatever a callback does.
	if e.opts.OnBlock != nil && len(blk) > 0 {
		e.tapBlock(blk)
	}

	e.mu.Lock()
	e.stats.Blocks++
	e.stats.Samples += int64(len(blk))
	e.mu.Unlock()

	for off := 0; off < len(blk); off += hop {
		end := min(off+hop, len(blk))
		e.an.Push(blk[off:end])
		e.pushed += int64(end - off)

		if w := e.an.Window(); w != nil {
			e.mu.Lock()
			e.stats.Windows++
			e.mu.Unlock()

			pos := e.pushed
			level := e.an.LevelDBFS()
			// Adaptive silence gate: with the environment-noise tracker running,
			// the gate follows the ambience instead of the fixed -60 dBFS, so a
			// noisy game stops scoring (and reporting) empty windows while a
			// quiet one keeps the configured sensitivity. It is pushed at most
			// once per hop, and only when it actually moved, so the matcher's
			// hot path stays untouched.
			if e.adaptive {
				e.adaptGate()
			}
			// A short click leaves the 50 ms meter before the 187 ms patch is
			// centred on it, so the meter is already back in the bed at the
			// window that would match. Score that window when its loudest frame
			// is clearly above the gate. A frame only a hair over the gate is
			// the bed itself.
			scoreLevel := level
			if wlv := e.an.WindowLevelDBFS(); wlv > scoreLevel && wlv >= e.GateDBFS()+windowLevelMarginDB {
				scoreLevel = wlv
			}
			ev, top := e.matcher.Tick(w, scoreLevel, e.stamp(pos))
			if top != nil {
				e.lastTop, e.lastSilent = top, false
			} else {
				e.lastSilent = true
			}
			if ev != nil {
				e.mu.Lock()
				e.stats.Events++
				e.stats.LastEventAt = ev.Time
				e.mu.Unlock()
				if e.onEvent != nil {
					e.onEvent(*ev)
				}
			}
			if !e.lastTickSet || pos-e.lastTickPos >= e.tickSamples {
				e.lastTickPos, e.lastTickSet = pos, true
				if e.lastSilent {
					silentTicks++
				}
				e.emitTick(pos, level, ev)
			}
		}
		// Bound memory: keep the newest windows, release everything older.
		e.an.Trim(e.opts.KeepFrames)
	}

	dur := time.Since(t0)
	e.mu.Lock()
	e.stats.SilentTicks += int64(silentTicks)
	e.procTime += dur
	if ms := dur.Seconds() * 1000; ms > e.stats.MaxBlockMs {
		e.stats.MaxBlockMs = ms
	}
	e.mu.Unlock()
}

// tapBlock runs the OnBlock hook with a panic barrier. Panics are converted into
// a one-shot error recorded on the engine, because losing the whole realtime run
// to a bug in an auxiliary tap would be far worse than losing the tap.
func (e *Engine) tapBlock(blk []float32) {
	defer func() {
		if r := recover(); r != nil {
			e.mu.Lock()
			if e.lastErr == nil {
				e.lastErr = fmt.Errorf("OnBlock 回调 panic: %v", r)
			}
			e.tapBroken = true
			e.mu.Unlock()
		}
	}()
	e.mu.Lock()
	broken := e.tapBroken
	e.mu.Unlock()
	if broken {
		return
	}
	e.opts.OnBlock(blk)
}

// emitTick publishes one scoring snapshot. Called on the Run goroutine.
func (e *Engine) emitTick(pos int64, level float64, ev *match.Event) {
	e.mu.Lock()
	e.stats.Ticks++
	e.mu.Unlock()
	if e.onTick == nil {
		return
	}
	e.onTick(Tick{
		Time:         e.stamp(pos),
		LevelDBFS:    level,
		DenoisedDBFS: e.an.DenoisedLevelDBFS(),
		Top:          e.lastTop,
		Event:        ev,
		Silent:       e.lastSilent,
		AudioTime:    e.audioTime(pos),
		Stats:        e.Stats(),
	})
}

// finish fills in the derived statistics once the run is over.
func (e *Engine) finish() {
	e.mu.Lock()
	e.stats.Elapsed = time.Since(e.startWall)
	e.mu.Unlock()
	// Touch Stats once so the derived fields (averages, ms/20 ms) are current
	// for anyone that only reads the struct afterwards.
	_ = e.Stats()
}

// audioTime maps an absolute sample position to a duration on the audio clock.
func (e *Engine) audioTime(pos int64) time.Duration {
	return time.Duration(pos) * time.Second / SampleRate
}

// stamp maps an absolute sample position to a wall-clock timestamp: the wall
// clock at Run start plus the audio position.
func (e *Engine) stamp(pos int64) time.Time {
	return e.startWall.Add(e.audioTime(pos))
}

// Summary renders the one-line statistics used by the CLI and the API.
func (st Stats) Summary() string {
	return fmt.Sprintf("块 %d / 采样 %.1f s / 窗口 %d / tick %d / 事件 %d / 丢帧 %d / 单块 %.2f ms(峰值 %.2f) / 每20ms %.2f ms",
		st.Blocks, st.AudioSeconds, st.Windows, st.Ticks, st.Events, st.Dropped,
		st.AvgBlockMs, st.MaxBlockMs, st.MsPer20msAudio)
}
