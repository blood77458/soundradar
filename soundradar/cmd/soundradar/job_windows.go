//go:build windows

package main

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

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
		// KILL_ON_JOB_CLOSE reaps the serve child if the tray is killed.
		// BREAKAWAY_OK / SILENT_BREAKAWAY_OK keep anything that child starts
		// (the browser opened for the management page) out of the job. Without
		// that, quitting SoundRadar TerminateProcess's Chrome, and Chrome
		// reports it as a crash.
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
			windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK |
			windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK
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

// startBreakaway starts a process that must outlive this one. serve is a
// member of the tray's kill-on-close job; an ordinary child (cmd.exe opening
// the default browser) would be killed with that job.
func startBreakaway(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB,
	}
	return cmd
}
