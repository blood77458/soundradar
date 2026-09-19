//go:build !windows

package config

import (
	"os"
	"path/filepath"
)

// replaceFile is the POSIX fallback: rename(2) already replaces atomically.
func replaceFile(src, dst string) error {
	return os.Rename(src, dst)
}

// syncDir flushes the directory entry so the rename survives a crash.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

var _ = filepath.Separator
