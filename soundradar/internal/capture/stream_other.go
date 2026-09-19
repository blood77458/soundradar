//go:build !windows

package capture

// OpenStream always fails on non-Windows platforms: WASAPI loopback does not
// exist there. The stub keeps `go build ./...` working everywhere.
func OpenStream(string) (*Stream, error) {
	return nil, ErrUnsupported
}
