//go:build !windows

package index

import (
	"os"
	"path/filepath"
)

// replaceFile atomically replaces dst with src (POSIX rename is atomic within a
// filesystem, and the temporary file is created in the target directory).
func replaceFile(src, dst string) error { return os.Rename(src, dst) }

// syncDir flushes the directory entry that now points at the new file.
func syncDir(dir string) {
	d, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
