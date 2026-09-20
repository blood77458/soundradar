//go:build !windows

package main

import "os/exec"

// adoptChild is a no-op elsewhere: job objects are a Windows concept.
func adoptChild(*exec.Cmd) {}
