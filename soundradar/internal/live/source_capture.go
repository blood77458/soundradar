package live

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/znz/soundradar/internal/capture"
)

// captureSource adapts internal/capture's continuous loopback Stream to
// FrameSource, converting the engine mix format (typically 48 kHz / 2 ch /
// 32-bit float) to the canonical 48 kHz mono float32 the recognition path
// needs.
//
// It is the one source that must never block: its pump goroutine reads WASAPI
// packets, converts them and does a NON-blocking publish. When the consumer is
// behind the block is counted (capture.Stream.Dropped, surfaced through
// Stats.Dropped) instead of stalling the audio thread or growing a buffer
// without bound.
type captureSource struct {
	st   *capture.Stream
	conv *streamConverter

	ch   chan []float32
	quit chan struct{}
	done chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once
	mu        sync.Mutex
	err       error
	started   bool
	// dropped counts blocks this adapter itself had to discard (the WASAPI
	// stream counts its own drops separately).
	dropped int64
}

// NewCaptureSource opens deviceSubstr (empty = the default render endpoint) for
// continuous loopback capture. Opening happens here, so a missing endpoint or
// an unsupported mix format fails at start-up rather than mid-run.
func NewCaptureSource(device string) (FrameSource, error) {
	st, err := capture.OpenStream(device)
	if err != nil {
		return nil, err
	}
	return &captureSource{
		st:   st,
		conv: newStreamConverter(st.Format.SampleRate, st.Format.Channels),
		ch:   make(chan []float32, 32),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}, nil
}

// Start launches the pump goroutine.
func (s *captureSource) Start() error {
	select {
	case <-s.quit:
		return errors.New("live: 采集源已停止，不能重新启动")
	default:
	}
	s.startOnce.Do(func() {
		s.mu.Lock()
		s.started = true
		s.mu.Unlock()
		go s.pump()
	})
	return nil
}

// Frames returns the block channel.
func (s *captureSource) Frames() <-chan []float32 { return s.ch }

// Stop closes the WASAPI stream and waits for the pump goroutine.
//
// Close runs before the wait: the pump only notices quit between Read calls,
// and Read stays blocked until the stream signals quit or closes its channel.
// Waiting for Close itself is bounded so a stuck COM call cannot pin the
// recognition goroutine (and therefore POST /api/live/stop) forever.
func (s *captureSource) Stop() error {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	s.stopOnce.Do(func() {
		close(s.quit)
		if !started {
			close(s.ch)
		}
	})
	if s.st != nil {
		closed := make(chan struct{})
		go func() {
			_ = s.st.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
		}
	}
	if !started {
		return nil
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// Err returns the error that ended the capture.
func (s *captureSource) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Dropped reports blocks discarded by this adapter plus the WASAPI stream's own
// count.
func (s *captureSource) Dropped() int64 {
	s.mu.Lock()
	n := s.dropped
	s.mu.Unlock()
	if s.st != nil {
		n += s.st.Dropped()
	}
	return n
}

// SilentBlocks / Discontinuities forward the WASAPI packet diagnostics.
func (s *captureSource) SilentBlocks() int64 {
	if s.st == nil {
		return 0
	}
	return s.st.SilentBlocks()
}

// Discontinuities forwards the WASAPI discontinuity counter.
func (s *captureSource) Discontinuities() int64 {
	if s.st == nil {
		return 0
	}
	return s.st.Discontinuities()
}

// Device returns the endpoint that was opened.
func (s *captureSource) Device() capture.Device { return s.st.Device }

// Stream returns the underlying capture stream (nil-safe).
func (s *captureSource) Stream() *capture.Stream { return s.st }

// StreamNote reports how the endpoint was chosen: an empty Note means the
// requested endpoint accepted its own mix format, and Skipped lists the
// endpoints that were tried and refused first. The CLI prints this at startup so
// a substitution is never silent.
func (s *captureSource) StreamNote() (string, []capture.EndpointFailure) {
	if s == nil || s.st == nil {
		return "", nil
	}
	return s.st.Note, s.st.Skipped
}

// Info describes the endpoint for the banner/API.
func (s *captureSource) Info() SourceInfo {
	d := s.st.Device
	name := d.Name
	if d.Default {
		name += "（默认端点）"
	}
	return SourceInfo{
		Kind:     "capture",
		Detail:   fmt.Sprintf("WASAPI loopback [%d] %s · 混音格式 %s → 48000 Hz 单声道", d.Index, name, s.st.Format),
		Device:   name,
		Rate:     s.st.Format.SampleRate,
		Channels: s.st.Format.Channels,
		Format:   s.st.Format.String(),
		Realtime: true,
	}
}

// pump converts and publishes until the stream ends or Stop is called.
func (s *captureSource) pump() {
	defer close(s.done)
	defer close(s.ch)
	defer s.st.Close()

	for {
		select {
		case <-s.quit:
			return
		default:
		}
		blk, err := s.st.Read()
		if err != nil {
			if !errors.Is(err, capture.ErrStreamClosed) {
				s.mu.Lock()
				if s.err == nil {
					s.err = err
				}
				s.mu.Unlock()
			}
			return
		}
		mono := s.conv.convert(blk)
		if len(mono) == 0 {
			continue
		}
		select {
		case s.ch <- mono:
		case <-s.quit:
			return
		default:
			s.mu.Lock()
			s.dropped++
			s.mu.Unlock()
		}
	}
}

// ---------------------------------------------------------------------------
// streaming format conversion
// ---------------------------------------------------------------------------

// streamConverter turns interleaved int16 at (srcRate, channels) into mono
// 48 kHz float32, carrying the downmix and the resampler phase across chunks so
// the concatenated output is continuous.
//
// The resampler is the same linear interpolation as internal/audio.Resample
// (documented there as a deliberate no-anti-aliasing trade-off); it differs
// only in that it works on a stream, which is what needs the carried phase.
type streamConverter struct {
	srcRate, dstRate int
	channels         int
	ratio            float64

	// k is the index of the next output sample (absolute, from the start of the
	// stream) and n is the number of mono samples consumed so far.
	k int64
	n int64
	// prev is mono[n-1] of the previous chunk: the left neighbour of the first
	// sample of the next chunk.
	prev float32
}

func newStreamConverter(srcRate, channels int) *streamConverter {
	if channels < 1 {
		channels = 1
	}
	if srcRate <= 0 {
		srcRate = SampleRate
	}
	return &streamConverter{
		srcRate:  srcRate,
		dstRate:  SampleRate,
		channels: channels,
		ratio:    float64(srcRate) / float64(SampleRate),
	}
}

// convert consumes one interleaved block and returns the next stretch of
// 48 kHz mono samples. A 48 kHz mono source is passed through (after copying,
// since the caller's buffer belongs to WASAPI).
func (s *streamConverter) convert(interleaved []int16) []float32 {
	m := len(interleaved) / s.channels
	if m == 0 {
		return nil
	}
	mono := make([]float32, m)
	if s.channels == 1 {
		for i, v := range interleaved[:m] {
			mono[i] = float32(v) / 32768
		}
	} else {
		inv := 1 / float32(s.channels)
		for i := 0; i < m; i++ {
			var sum float32
			base := i * s.channels
			for c := 0; c < s.channels; c++ {
				sum += float32(interleaved[base+c])
			}
			mono[i] = sum * inv / 32768
		}
	}
	if s.srcRate == s.dstRate {
		s.n += int64(m)
		s.prev = mono[m-1]
		return mono
	}

	// Sample at absolute index i is mono[i-n] (or prev when i == n-1).
	at := func(i int64) float32 {
		if i < s.n {
			return s.prev
		}
		return mono[i-s.n]
	}
	out := make([]float32, 0, int(float64(m)/s.ratio)+2)
	for {
		p := float64(s.k) * s.ratio
		i := int64(p)
		// Require the interpolation partner to be inside the samples received
		// so far; the leftover sample is emitted after the next chunk.
		if i+1 > s.n+int64(m)-1 {
			break
		}
		f := float32(p - float64(i))
		out = append(out, at(i)*(1-f)+at(i+1)*f)
		s.k++
	}
	s.n += int64(m)
	s.prev = mono[m-1]
	return out
}
