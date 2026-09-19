//go:build windows

package index

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// replaceFile puts src at dst.
//
// os.Rename maps to MoveFileW on Windows, which fails when dst exists, so an
// existing index could never be replaced. MoveFileExW with
// MOVEFILE_REPLACE_EXISTING is Windows' documented atomic replace (same volume
// required, which holds because the temporary file lives in the target
// directory). MOVEFILE_WRITE_THROUGH makes the call return only after the
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

// syncDir is a no-op on Windows: a directory cannot be opened for
// FlushFileBuffers without FILE_FLAG_BACKUP_SEMANTICS plumbing, and
// MOVEFILE_WRITE_THROUGH already covers the durability of the rename.
func syncDir(dir string) {}
