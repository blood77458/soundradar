//go:build !windows

package capture

import (
	"time"
)

// New always fails on non-Windows platforms: WASAPI loopback does not exist
// there. This stub keeps `go build ./...` working on Linux/macOS so the
// platform-independent packages can still be developed and tested.
func New() (Capturer, error) {
	return nil, ErrUnsupported
}

type unsupportedCapturer struct{}

func (unsupportedCapturer) Enumerate() ([]Device, error) { return nil, ErrUnsupported }

func (unsupportedCapturer) Capture(string, time.Duration) (*Result, error) {
	return nil, ErrUnsupported
}

func (unsupportedCapturer) Close() error { return nil }
