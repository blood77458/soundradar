package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/znz/soundradar/internal/capture"
	"github.com/znz/soundradar/internal/live"
)

// reportCaptureFailure prints the full "we cannot listen to this machine" report:
// every render endpoint with its status, then the ordered list of things to
// change. It is called from every place that fails to open the loopback, so the
// user gets the same actionable text whether they ran `serve`, `overlay`, `live`
// or the tray launcher.
//
// It never returns an error: the caller already has one to report, and a
// diagnostic that fails must not hide the original problem.
func reportCaptureFailure(w *os.File, cause error) {
	if w == nil {
		w = os.Stderr
	}
	devices, failures := probeEndpoints()
	capture.PrintEndpointFailureReport(w, devices, failures, cause)
}

// probeEndpoints enumerates the render endpoints and, on Windows, tries to open
// each one so the report can say which are usable. Probing is best effort: a
// machine where even enumeration fails still gets the guidance text.
func probeEndpoints() ([]capture.Device, []capture.EndpointFailure) {
	c, err := capture.New()
	if err != nil {
		return nil, nil
	}
	defer c.Close()

	devices, err := c.Enumerate()
	if err != nil {
		return nil, nil
	}
	probes, err := c.Probe()
	if err != nil {
		return devices, nil
	}
	var failures []capture.EndpointFailure
	for _, p := range probes {
		if !p.OK {
			failures = append(failures, capture.EndpointFailure{Device: p.Device, Reason: p.Err})
		}
	}
	return devices, failures
}

// printSourceNote prints the "an endpoint or a format had to be substituted"
// warning for a source that is already running, or nothing when the plain
// endpoint worked.
//
// It reads the note through live.StreamNoteReporter, so it works for the realtime
// capture source without the commands having to know its concrete type.
func printSourceNote(prefix string, src live.FrameSource, quiet bool) {
	if quiet || src == nil {
		return
	}
	nr, ok := src.(live.StreamNoteReporter)
	if !ok {
		return
	}
	note, skipped := nr.StreamNote()
	if note == "" && len(skipped) == 0 {
		return
	}
	var b strings.Builder
	if note != "" {
		b.WriteString(note)
	} else {
		b.WriteString("以下端点打不开，已改用可用的那个：\n")
	}
	for _, f := range skipped {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	fmt.Printf("%s音频注意：%s", prefix, b.String())
	fmt.Printf("%s提示：`soundradar devices --probe` 会逐个端点试开一次；\n", prefix)
	for i, step := range capture.TroubleshootingSteps {
		fmt.Printf("%s  %d. %s\n", prefix, i+1, step)
	}
}
