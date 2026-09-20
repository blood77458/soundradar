//go:build windows

package capture

import (
	"fmt"
	"io"
	"strings"
)

// ---------------------------------------------------------------------------
// Startup health report
// ---------------------------------------------------------------------------

// Health is the result of checking whether this machine can be listened to, and
// it is designed to be printed verbatim at startup:
//
//	[音频自检] 默认端点 [0] 扬声器 (Realtek(R) Audio) 48000 Hz / 2 ch / 32-bit float
//	[音频自检] 其余端点：[1] 耳机 (USB Audio) 可用
//
// When nothing can be opened it explains, in order of likelihood, what to change
// on the machine - that is the whole point: the user cannot be expected to read
// an HRESULT and guess.
type Health struct {
	// Devices is every active render endpoint.
	Devices []Device
	// Working is the endpoint that was actually opened, if any.
	Working Device
	HasWorking bool
	// Format is the format the working endpoint accepted.
	Format SampleFormat
	// Note explains substitutions (skipped endpoints / negotiated format).
	Note string
	// Failures lists the endpoints that could not be opened, with their reasons.
	Failures []DeviceProbe
	// Error is set when nothing could be opened at all.
	Error error
}

// CheckHealth probes every endpoint and opens the one Capture would use. It is
// the same code path as recognition, so its verdict matches what will happen.
func (c *loopbackCapturer) CheckHealth(deviceSubstr string) *Health {
	h := &Health{}
	devices, err := c.Enumerate()
	if err != nil {
		h.Error = err
		return h
	}
	h.Devices = devices
	if len(devices) == 0 {
		h.Error = ErrNoEndpoint
		return h
	}

	probes, perr := c.Probe()
	if perr != nil {
		h.Error = perr
		return h
	}
	for _, p := range probes {
		if !p.OK {
			h.Failures = append(h.Failures, p)
		}
	}

	sess, skipped, oerr := c.openFirstEndpoint(deviceSubstr)
	if oerr != nil {
		h.Error = oerr
		return h
	}
	defer sess.release()
	h.Working = sess.dev
	h.HasWorking = true
	h.Format = sess.mix
	h.Note = sess.note(skipped)
	return h
}

// Report writes the human-readable health report.
//
// quiet suppresses the "everything is fine" line, so the happy path does not add
// noise to a banner; the problem cases are ALWAYS printed.
func (h *Health) Report(w io.Writer, quiet bool) {
	if h == nil {
		return
	}
	if h.Error != nil {
		fmt.Fprintf(w, "\n")
		fmt.Fprintf(w, "╔══════════════════════════════════════════════════════════════════╗\n")
		fmt.Fprintf(w, "║  音频采集无法启动，声音识别现在收不到任何声音                    ║\n")
		fmt.Fprintf(w, "╚══════════════════════════════════════════════════════════════════╝\n")
		printDeviceList(w, h.Devices, h.Failures)
		fmt.Fprintf(w, "\n失败原因：\n  %v\n", h.Error)
		printTroubleshooting(w)
		return
	}
	if !h.HasWorking {
		return
	}

	// Something worked. Be quiet when it was the plain expected path; be loud
	// when an endpoint or a format had to be substituted, because the user needs
	// to know why the sound they expect may differ.
	if !quiet && h.Note == "" {
		fmt.Fprintf(w, "[音频] 采集端点   : [%d] %s（混合格式 %s）\n",
			h.Working.Index, h.Working.Name, h.Format.String())
	} else if h.Note != "" {
		fmt.Fprintf(w, "\n[音频] 注意：默认端点有问题，已自动换用可用的端点\n")
		fmt.Fprintf(w, "       实际使用 : [%d] %s（%s）\n", h.Working.Index, h.Working.Name, h.Format.String())
		for _, line := range strings.Split(strings.TrimRight(h.Note, "\n"), "\n") {
			fmt.Fprintf(w, "       %s\n", line)
		}
		if len(h.Failures) > 0 {
			fmt.Fprintf(w, "       打不开的端点：\n")
			for _, f := range h.Failures {
				fmt.Fprintf(w, "         - [%d] %s：%s\n", f.Device.Index, f.Device.Name, firstLine(f.Err))
			}
		}
		fmt.Fprintf(w, "       要固定用某个端点：--device \"<名字片段>\"；\n")
		fmt.Fprintf(w, "       想彻底修好默认端点，看下面这份清单：\n")
		printTroubleshooting(w)
	}
}

// printDeviceList lists every endpoint and whether it can be opened.
func printDeviceList(w io.Writer, devices []Device, failures []DeviceProbe) {
	bad := map[string]string{}
	for _, f := range failures {
		bad[f.Device.ID] = firstLine(f.Err)
	}
	fmt.Fprintf(w, "\n这台机器上的播放端点（eRender）：\n")
	for _, d := range devices {
		marks := ""
		if d.Default {
			marks += " [默认]"
		}
		if d.DefaultComms {
			marks += " [默认通讯]"
		}
		state := "可用"
		if err, isBad := bad[d.ID]; isBad {
			state = "打不开"
			fmt.Fprintf(w, "  [%d] %s%s  —— %s\n", d.Index, d.Name, marks, state)
			fmt.Fprintf(w, "       原因：%s\n", err)
			continue
		}
		fmt.Fprintf(w, "  [%d] %s%s  —— %s\n", d.Index, d.Name, marks, state)
	}
}

// printTroubleshooting writes the ordered guidance. The list itself lives in
// capturer.go so every front end prints exactly the same advice.
func printTroubleshooting(w io.Writer) {
	PrintTroubleshooting(w)
}

// firstLine keeps a multi-line failure reason to one line for the compact lists.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
