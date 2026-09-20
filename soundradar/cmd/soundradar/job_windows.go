//go:build windows

package main

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobObjectLimitKillOnJobClose makes a job object kill its processes when the
// last handle to it closes, i.e. when this process exits - gracefully or not.
const jobObjectLimitKillOnJobClose = 0x00002000

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobObjectExtendedLimitInformationStruct struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	JobPeakMemoryUsed     uintptr
}

var (
	jobOnce sync.Once
	jobHand windows.Handle
)

// adoptChild puts cmd's process into a kill-on-close job object, so the serve
// child cannot outlive this launcher.
//
// Best effort by design: a job cannot always be created (the process may
// already be inside a job that forbids nesting, or a policy may block it), and
// that must not stop the program from working. The graceful path in
// serverChild.stop is what normally shuts the child down; this is the safety net
// for "the launcher was killed from Task Manager", which would otherwise leave a
// recording server running with no tray icon to stop it.
func adoptChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	jobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return
		}
		var info jobObjectExtendedLimitInformationStruct
		info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
		_, err = windows.SetInformationJobObject(h,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)))
		if err != nil {
			windows.CloseHandle(h)
			return
		}
		jobHand = h
	})
	if jobHand == 0 {
		return
	}
	// A handle with PROCESS_SET_QUOTA and PROCESS_TERMINATE is what
	// AssignProcessToJobObject requires.
	ph, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(ph)
	_ = windows.AssignProcessToJobObject(jobHand, ph)
}
