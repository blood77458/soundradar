//go:build windows

package config

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// replaceFile puts src at dst atomically.
//
// os.Rename maps to MoveFileW on Windows, which FAILS when dst already exists,
// so a plain rename could never overwrite an existing config.json.
// MoveFileExW with MOVEFILE_REPLACE_EXISTING is the documented atomic replace
// (both files must be on the same volume, which holds because the temporary
// file is created in the target directory). MOVEFILE_WRITE_THROUGH makes the
// call return only once the rename itself is flushed.
func replaceFile(src, dst string) error {
	src16, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	dst16, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(src16, dst16,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("MoveFileExW(%s -> %s): %w", src, dst, err)
	}
	return nil
}

// syncDir is a no-op on Windows: MOVEFILE_WRITE_THROUGH already covers the
// durability of the rename (same reasoning as internal/index).
func syncDir(dir string) { _ = dir }
