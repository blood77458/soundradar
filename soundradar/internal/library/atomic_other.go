//go:build !windows

package library

import (
	"os"
	"path/filepath"
)

// replaceFile is an atomic rename on POSIX systems.
func replaceFile(src, dst string) error {
	return os.Rename(src, dst)
}

// syncDir fsyncs the directory entry so the rename survives a power loss.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
	_ = filepath.Base(dir)
}
