//go:build !windows

package main

import "os/exec"

// adoptChild is a no-op elsewhere: job objects are a Windows concept.
func adoptChild(*exec.Cmd) {}

// startBreakaway is an ordinary exec.Command where job objects do not exist.
func startBreakaway(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}
