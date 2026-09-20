//go:build windows

package main

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	consoleOnce sync.Once
	consoleCnt  uint32
	procGetConsoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")
)

// consoleProcessCount returns how many processes are attached to this process's
// console (0 when there is none). A program started from cmd.exe or PowerShell
// shares the console with the shell that spawned it (>= 2); a double-clicked
// program is alone on the console Windows creates for it (1).
func consoleProcessCount() uint32 {
	consoleOnce.Do(func() {
		var buf [4]uint32
		n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		consoleCnt = uint32(n)
	})
	return consoleCnt
}

// launchedFromShell reports whether this process looks like it was started by a
// double click in Explorer rather than typed into a shell. It is only used to
// explain the decision in the log; the decision itself is "no subcommand".
func launchedFromShell() bool {
	return consoleProcessCount() <= 1
}

// hasConsole reports whether output written to stdout/stderr can be seen, i.e.
// whether a shell is attached.
func hasConsole() bool { return consoleProcessCount() > 1 }

// exePath returns the running executable's path, or "" when it cannot be read.
func exePath() string {
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetModuleFileName(0, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}
