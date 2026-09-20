//go:build windows

package capture

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// streamQueueBlocks is how many blocks the capture goroutine may run ahead of
// the consumer before it starts dropping. 32 blocks is ~320 ms at the packet
// sizes WASAPI delivers in shared mode: enough to ride out a scheduling hiccup,
// small enough that the panel stays close to real time and that a stalled
// consumer cannot grow the process.
const streamQueueBlocks = 32

// OpenStream opens deviceSubstr (empty = the default render endpoint) for
// continuous speaker loopback capture in WASAPI shared mode.
//
// Everything COM related happens on one goroutine that is locked to its OS
// thread, exactly like Capturer, because WASAPI is thread affine; the caller
// only ever sees Read/Close.
func OpenStream(deviceSubstr string) (*Stream, error) {
	ch := make(chan []int16, streamQueueBlocks)
	quit := make(chan struct{})
	done := make(chan struct{})
	ready := make(chan streamHandshake, 1)
	var drops, silent, discontig atomic.Int64

	go func() {
		defer close(done)
		// Close the block channel on every exit path. Otherwise a failed or
		// finished capture leaves Read blocked forever, and Stop never returns.
		defer close(ch)
		// newLoopbackCapturer locks THIS goroutine to its OS thread and
		// initialises COM in MTA, so every call below stays in the same
		// apartment.
		c, err := newLoopbackCapturer()
		if err != nil {
			ready <- streamHandshake{err: err}
			return
		}
		defer c.Close()

		h, note, skipped, err := openLoopback(c, deviceSubstr)
		if err != nil {
			ready <- streamHandshake{err: err}
			return
		}
		defer h.releaseCOM()

		ready <- streamHandshake{dev: h.dev, format: h.mix, note: note, skipped: skipped}

		if err := h.iac.Start(); err != nil {
			h.fail(fmt.Errorf("IAudioClient::Start failed: %w", err))
			return
		}
		defer h.stop()

		for {
			select {
			case <-quit:
				return
			default:
			}

			var pending uint32
			if err := h.acc.GetNextPacketSize(&pending); err != nil {
				h.fail(fmt.Errorf("IAudioCaptureClient::GetNextPacketSize failed: %w", err))
				return
			}
			if pending == 0 {
				if sleepOrQuit(quit, pollInterval) {
					return
				}
				continue
			}

			for {
				var next uint32
				if err := h.acc.GetNextPacketSize(&next); err != nil {
					h.fail(fmt.Errorf("IAudioCaptureClient::GetNextPacketSize failed: %w", err))
					return
				}
				if next == 0 {
					break
				}
				blk, stop, err := h.packet(&silent, &discontig)
				if err != nil {
					h.fail(err)
					return
				}
				if stop {
					return
				}
				if len(blk) == 0 {
					continue
				}
				select {
				case ch <- blk:
				case <-quit:
					return
				default:
					// The consumer is behind: drop this block rather than
					// stalling the WASAPI polling loop (which would make the
					// engine's latency grow without bound).
					drops.Add(1)
				}
			}
		}
	}()

	hs := <-ready
	if hs.err != nil {
		close(quit)
		<-done
		return nil, hs.err
	}

	return &Stream{
		Device:     hs.dev,
		Format:     hs.format,
		Note:       hs.note,
		Skipped:    hs.skipped,
		queueDepth: streamQueueBlocks,
		drops:      &drops,
		silent:     &silent,
		discontig:  &discontig,
		next: func() ([]int16, error) {
			select {
			case blk, ok := <-ch:
				if !ok {
					return nil, io.EOF
				}
				return blk, nil
			case <-quit:
				return nil, ErrStreamClosed
			}
		},
		release: func() error {
			close(quit)
			<-done
			return nil
		},
	}, nil
}

// sleepOrQuit sleeps for d unless the stream is being torn down; it reports
// whether the caller should stop.
func sleepOrQuit(quit <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-quit:
		return true
	case <-t.C:
		return false
	}
}

type streamHandshake struct {
	dev    Device
	format SampleFormat
	// note explains a non-obvious setup: a skipped endpoint, or a format other
	// than the endpoint's own that had to be negotiated. It is never empty when
	// something was substituted, so the substitution cannot go unnoticed.
	note string
	// skipped lists the endpoints that were tried and refused before dev.
	skipped []EndpointFailure
	err     error
}

// loopback is one opened IAudioClient/IAudioCaptureClient pair plus the state
// needed to convert its packets.
type loopback struct {
	dev      Device
	mix      SampleFormat
	iac      *wca.IAudioClient
	acc      *wca.IAudioCaptureClient
	bytesPer int
	err      error
}

// releaseCOM releases the COM objects this handle owns.
func (h *loopback) releaseCOM() {
	if h.acc != nil {
		h.acc.Release()
		h.acc = nil
	}
	if h.iac != nil {
		h.iac.Release()
		h.iac = nil
	}
}

// stop halts the audio client (idempotent, safe after a failed Start).
func (h *loopback) stop() {
	if h.iac != nil {
		_ = h.iac.Stop()
	}
}

// fail records the error that ended the stream.
func (h *loopback) fail(err error) {
	if h.err == nil {
		h.err = err
	}
}

// openLoopback resolves the endpoint and initialises a shared-mode loopback
// IAudioClient on it. It is the setup half that Capturer.Capture and Stream
// share, including the endpoint fallback (see candidateDevices) and the
// alternate-format retry (see openEndpoint).
func openLoopback(c *loopbackCapturer, deviceSubstr string) (*loopback, string, []EndpointFailure, error) {
	sess, skipped, err := c.openFirstEndpoint(deviceSubstr)
	if err != nil {
		return nil, "", nil, err
	}
	h := &loopback{
		dev:      sess.dev,
		iac:      sess.iac,
		acc:      sess.acc,
		mix:      sess.mix,
		bytesPer: sess.bytesPer,
	}
	// The session's objects now belong to the loopback, which releases them.
	sess.iac, sess.acc = nil, nil
	sess.wfx = nil
	return h, sess.note(skipped), endpointFailures(skipped), nil
}

// endpointFailures converts the internal skip list into the public report type.
func endpointFailures(skipped []skippedEndpoint) []EndpointFailure {
	if len(skipped) == 0 {
		return nil
	}
	out := make([]EndpointFailure, 0, len(skipped))
	for _, sk := range skipped {
		out = append(out, EndpointFailure{Device: sk.dev, Reason: firstLine(sk.err.Error())})
	}
	return out
}

// packet converts the next queued WASAPI packet into a freshly allocated block
// of interleaved 16-bit PCM (nil when the packet carried no frames). stop is
// true when the endpoint disappeared.
//
// The block is always newly allocated: the WASAPI buffer it was read from is
// released before the caller consumes it, and the consumer runs on another
// goroutine.
func (h *loopback) packet(silent, discontig *atomic.Int64) (blk []int16, stop bool, err error) {
	var (
		data   *byte
		frames uint32
		flags  uint32
		devPos uint64
		qpcPos uint64
	)
	gerr := h.acc.GetBuffer(&data, &frames, &flags, &devPos, &qpcPos)
	if gerr != nil {
		var oleErr *ole.OleError
		if errors.As(gerr, &oleErr) {
			switch uint32(oleErr.Code()) {
			case hresultBufferEmpty:
				// Race between GetNextPacketSize and GetBuffer.
				return nil, false, nil
			case hresultDeviceInvalidated:
				return nil, true, fmt.Errorf("capture: endpoint invalidated during capture (device removed or default device changed): %w", gerr)
			}
		}
		return nil, true, fmt.Errorf("IAudioCaptureClient::GetBuffer failed: %w", gerr)
	}

	n := int(frames) * h.mix.Channels
	switch {
	case frames == 0:
		// Empty packet: nothing to convert, nothing to release.
	case flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0:
		// Loopback reports "silent" while nothing is playing. The data pointer
		// is meaningless then, so emit real zeros: skipping the packet would
		// shift the whole timeline.
		silent.Add(1)
		blk = make([]int16, n)
	case data == nil:
		blk = make([]int16, n)
	default:
		blk = appendPacket(make([]int16, 0, n), data, n, int(frames), h.bytesPer, h.mix)
	}
	if flags&wca.AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY != 0 {
		discontig.Add(1)
	}
	if flags&wca.AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR != 0 {
		discontig.Add(1)
	}

	if rerr := h.acc.ReleaseBuffer(frames); rerr != nil {
		return nil, true, fmt.Errorf("IAudioCaptureClient::ReleaseBuffer failed: %w", rerr)
	}
	return blk, false, nil
}
