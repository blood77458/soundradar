//go:build !windows

package recall

import "os"

// replaceFile is the POSIX equivalent: rename(2) is atomic and replaces an
// existing destination.
func replaceFile(src, dst string) error { return os.Rename(src, dst) }
