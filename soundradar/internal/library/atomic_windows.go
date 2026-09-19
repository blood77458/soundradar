//go:build windows

package library

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// replaceFile puts src at dst.
//
// On Windows os.Rename maps to MoveFileW which FAILS when dst already exists,
// so a library could only ever be created once. We therefore call MoveFileExW
// with MOVEFILE_REPLACE_EXISTING, Windows' documented atomic replace primitive
// (same volume required, which holds because the temp file is created in the
// target directory). MOVEFILE_WRITE_THROUGH makes the call return only once the
// rename itself is flushed.
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

// syncDir is a no-op on Windows: directories cannot be opened for
// FlushFileBuffers without FILE_FLAG_BACKUP_SEMANTICS plumbing, and
// MOVEFILE_WRITE_THROUGH already covers durability of the rename.
func syncDir(dir string) {}
