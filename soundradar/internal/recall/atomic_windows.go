//go:build windows

package recall

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// replaceFile puts src at dst.
//
// os.Rename maps to MoveFileW, which FAILS when dst already exists, so an
// atomic overwrite needs MoveFileExW with MOVEFILE_REPLACE_EXISTING (the same
// primitive internal/library and internal/config use). The temp file lives in
// the destination directory, so the rename never crosses a volume.
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
