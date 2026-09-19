// Package capture is the platform-independent entry point for grabbing the
// audio that the system is currently playing (speaker loopback).
//
// The concrete implementation lives in loopback_windows.go (WASAPI shared-mode
// loopback via github.com/moutend/go-wca, CGO-free). A stub implementation
// covers every other platform so the module still builds everywhere.
package capture

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors returned by the capture implementations.
var (
	// ErrNoEndpoint means the machine has no active render endpoint at all
	// (headless VM, no audio driver, ...).
	ErrNoEndpoint = errors.New("capture: no active audio render endpoint found")
	// ErrDeviceNotFound means --device <substr> matched nothing.
	ErrDeviceNotFound = errors.New("capture: no render endpoint matches the given name")
	// ErrUnsupported is returned on non-Windows platforms.
	ErrUnsupported = errors.New("capture: speaker loopback is only implemented on Windows")
)

// SampleFormat describes the PCM layout of a stream.
type SampleFormat struct {
	FormatTag     uint16 // 0x0001 PCM, 0x0003 IEEE float, 0xFFFE extensible
	SampleRate    int
	Channels      int
	BitsPerSample int
	Float         bool
	Extensible    bool
}

// Kind renders the sample format as "PCM" or "IEEE float".
func (f SampleFormat) Kind() string {
	if f.Float {
		return "IEEE float"
	}
	return "PCM"
}

// String renders a compact human-readable description.
func (f SampleFormat) String() string {
	ext := ""
	if f.Extensible {
		ext = ", WAVE_FORMAT_EXTENSIBLE"
	}
	return fmt.Sprintf("%d Hz / %d ch / %d-bit %s (tag=0x%04X%s)",
		f.SampleRate, f.Channels, f.BitsPerSample, f.Kind(), f.FormatTag, ext)
}

// Device is one audio endpoint as reported by the OS.
type Device struct {
	// Index is the stable position in the enumeration (0-based).
	Index int
	// ID is the WASAPI endpoint ID string.
	ID string
	// Name is PKEY_Device_FriendlyName.
	Name string
	// Default marks the default render endpoint for the console role, i.e.
	// the "Default Device" shown in Windows Settings.
	Default bool
	// DefaultComms marks the default endpoint for the communications role
	// ("Default Communication Device").
	DefaultComms bool
}

// Result is one finished loopback capture.
type Result struct {
	// Device that was captured.
	Device Device
	// MixFormat is the engine mix format the endpoint was opened with.
	MixFormat SampleFormat
	// PCM holds interleaved 16-bit PCM, len(PCM) == Frames*Channels.
	PCM []int16
	// Frames is the number of sample frames actually captured.
	Frames int64
	// Channels is len(PCM)/Frames.
	Channels int
	// SampleRate is the mix-format sample rate (no resampling is done).
	SampleRate int
	// Duration is Frames/SampleRate.
	Duration time.Duration
	// Wall is the wall-clock time the capture loop ran for.
	Wall time.Duration

	// Peak is the linear peak in [0,1]; PeakDBFS is its dBFS value.
	Peak     float64
	PeakDBFS float64
	RMS      float64
	RMSDBFS  float64
	// Silent reports whether every captured sample was exactly zero.
	Silent bool

	// Packets is the number of non-empty packets delivered by WASAPI.
	Packets int64
	// SilentPackets counts packets flagged AUDCLNT_BUFFERFLAGS_SILENT.
	SilentPackets int64
	// Discontinuities counts packets flagged AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY.
	Discontinuities int64
	// PaddingSpins counts GetCurrentPadding/GetNextPacketSize polls that
	// returned no data (used to show the polling really happened).
	PaddingSpins int64
}

// Capturer is the platform-independent capture interface.
//
// Implementations are NOT goroutine-safe and must be used from the single
// goroutine that created them, because the Windows implementation pins that
// goroutine to its OS thread for COM.
type Capturer interface {
	// Enumerate lists the active audio render endpoints.
	Enumerate() ([]Device, error)
	// Capture records speaker loopback for d.
	//
	// deviceSubstr selects the endpoint by case-insensitive substring match on
	// the friendly name (or endpoint ID); an empty string selects the default
	// render endpoint.
	Capture(deviceSubstr string, d time.Duration) (*Result, error)
	// Close releases the COM apartment and every COM object still held.
	Close() error
}

// MatchDevice picks the endpoint whose friendly name (or ID) contains substr,
// case-insensitively. An empty substr selects the default endpoint, falling
// back to the first endpoint when the OS reports no default.
func MatchDevice(devices []Device, substr string) (Device, error) {
	if len(devices) == 0 {
		return Device{}, ErrNoEndpoint
	}
	if strings.TrimSpace(substr) == "" {
		for _, d := range devices {
			if d.Default {
				return d, nil
			}
		}
		return devices[0], nil
	}

	needle := strings.ToLower(substr)
	for _, d := range devices {
		if strings.Contains(strings.ToLower(d.Name), needle) || strings.Contains(strings.ToLower(d.ID), needle) {
			return d, nil
		}
	}
	return Device{}, fmt.Errorf("%w: %q", ErrDeviceNotFound, substr)
}

// ---------------------------------------------------------------------------
// Streaming capture (added for the P2 realtime link)
// ---------------------------------------------------------------------------

// ErrStreamClosed is returned by Stream.Read after Close.
var ErrStreamClosed = errors.New("capture: stream is closed")

// Stream is an open-ended loopback capture: Read returns the next block of
// interleaved 16-bit PCM as long as the endpoint is delivering audio.
//
// Why it exists next to Capturer.Capture: Capture records a fixed duration and
// keeps the whole recording in memory (fine for `soundradar capture --seconds
// 5`), while the realtime recognition link has to run for hours. Stream never
// buffers more than a fixed number of blocks: when the consumer falls behind,
// the oldest blocks are counted in Dropped() and discarded instead of blocking
// the WASAPI polling goroutine or growing without bound.
//
// A Stream is safe to Read from one goroutine at a time; Close may be called
// from any goroutine.
type Stream struct {
	// Device is the endpoint that was opened; Format is the engine mix format
	// the endpoint was opened with (the samples Read returns use it unchanged).
	Device Device
	Format SampleFormat

	// queueDepth is the number of blocks buffered before dropping.
	queueDepth int

	next    func() ([]int16, error)
	release func() error

	mu     sync.Mutex
	closed bool
	err    error
	drops  *atomic.Int64

	// silent / discontig count WASAPI packet flags (loopback reports silent
	// packets while nothing plays, and flags discontinuities when the stream
	// glitches). They are diagnostics for the realtime banner.
	silent    *atomic.Int64
	discontig *atomic.Int64
}

// Read returns the next audio block. len(block) is a multiple of
// Format.Channels; the samples are at Format.SampleRate (no conversion is
// done here). Read returns io.EOF when the endpoint stopped delivering and
// ErrStreamClosed after Close.
func (s *Stream) Read() ([]int16, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrStreamClosed
	}
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	blk, err := s.next()
	if err != nil {
		if errors.Is(err, ErrStreamClosed) {
			return nil, err
		}
		s.mu.Lock()
		if s.err == nil {
			s.err = err
		}
		s.mu.Unlock()
		return nil, err
	}
	return blk, nil
}

// Err returns the error that stopped the stream, if any.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Dropped reports how many audio blocks were discarded because the consumer
// was not keeping up. The realtime link surfaces this number so a saturated
// pipeline is visible instead of silently losing audio.
func (s *Stream) Dropped() int64 {
	if s.drops == nil {
		return 0
	}
	return s.drops.Load()
}

// SilentBlocks reports how many packets the audio engine flagged as silent
// (loopback keeps delivering those while nothing is playing).
func (s *Stream) SilentBlocks() int64 {
	if s.silent == nil {
		return 0
	}
	return s.silent.Load()
}

// Discontinuities reports how many packets were flagged as discontinuous or
// timestamp-broken.
func (s *Stream) Discontinuities() int64 {
	if s.discontig == nil {
		return 0
	}
	return s.discontig.Load()
}

// Close stops the capture goroutine and releases every COM object. It is
// idempotent and safe to call concurrently with Read.
func (s *Stream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	if s.release != nil {
		return s.release()
	}
	return nil
}
