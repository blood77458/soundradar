//go:build !windows

package main

import "os"

// consoleProcessCount is 0 elsewhere: the notion only exists on Windows.
func consoleProcessCount() uint32 { return 0 }

// launchedFromShell is never true elsewhere.
func launchedFromShell() bool { return false }

// hasConsole reports whether a shell is attached (assume one off Windows).
func hasConsole() bool { return true }

// exePath returns the running executable's path, or "" when it cannot be read.
func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}
