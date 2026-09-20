package config

import (
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// overlay geometry - pure math, no Win32
// ---------------------------------------------------------------------------
//
// Everything the overlay window needs to decide "where do I put myself" lives
// here, so it can be unit-tested on any machine. internal/overlay only supplies
// the numbers it read from user32:
//
//   - MonitorInfo.Work / Full are PHYSICAL pixels straight out of
//     GetMonitorInfoW's rcWork / rcMonitor (the process is
//     PER_MONITOR_AWARE_V2, so Windows does not scale them for us).
//   - MonitorInfo.DPI is the effective DPI Windows reports for that monitor
//     (96 = 100%, 144 = 150%, 192 = 200%).
//
// The scaling rules, stated once so the tests can pin them down:
//
//	canvas width  = Size*MaxSimultaneous + 2*CanvasPad   (physical px)
//	canvas height = Size + 2*CanvasPad                   (physical px)
//	margin_px     = round(Margin * DPI/96)               (anchor mode only)
//
// i.e. Size/Margin are logical units that Windows scales, while an explicit
// X/Y is used exactly as written. CanvasPad is the transparent breathing room
// the renderer leaves around the cells.

// CanvasPad is the transparent padding (physical px) the renderer keeps around
// the event cells, on every side of the canvas.
const CanvasPad = 6

// DPI reference: 96 DPI is the 100% scale Windows uses for coordinates.
const dpiBase = 96.0

// MonitorInfo describes one display as the overlay sees it.
type MonitorInfo struct {
	// Index is the position in the enumerated list (0 = primary).
	Index int `json:"index"`
	// Device is the GDI device name, e.g. `\\.\DISPLAY1`.
	Device string `json:"device"`
	// X0/Y0/X1/Y1 is the full monitor rectangle (physical px, X1/Y1 exclusive).
	X0, Y0, X1, Y1 int `json:"-"`
	// WX0/WY0/WX1/WY1 is the work area (screen minus taskbar/docked appbars).
	WX0, WY0, WX1, WY1 int `json:"-"`
	// DPI is the effective DPI of the monitor (96 / 120 / 144 / 192 ...).
	DPI int `json:"dpi"`
	// Scale is DPI/96.
	Scale float64 `json:"scale"`
	// Primary is true for the primary monitor.
	Primary bool `json:"primary"`
	// Overlay reports where the overlay window currently sits on this monitor
	// (set by the overlay itself, not by the enumeration).
	Overlay bool `json:"overlay,omitempty"`
}

// WorkRect returns the usable rectangle as (x, y, w, h).
func (m MonitorInfo) WorkRect() (x, y, w, h int) {
	return m.WX0, m.WY0, m.WX1 - m.WX0, m.WY1 - m.WY0
}

// FullRect returns the full monitor rectangle as (x, y, w, h).
func (m MonitorInfo) FullRect() (x, y, w, h int) {
	return m.X0, m.Y0, m.X1 - m.X0, m.Y1 - m.Y0
}

// String renders a one-line description for the settings page / CLI banner.
func (m MonitorInfo) String() string {
	x, y, w, h := m.WorkRect()
	tag := ""
	if m.Primary {
		tag = "（主显示器）"
	}
	return fmt.Sprintf("#%d %s %dx%d @ (%d,%d) DPI %d (%.0f%%)%s",
		m.Index, m.Device, w, h, x, y, m.DPI, m.Scale*100, tag)
}

// DPIForPoint picks the monitor whose rectangle contains (x, y), falling back
// to the primary monitor, and returns its DPI (96 when unknown).
func DPIForPoint(monitors []MonitorInfo, x, y int) int {
	if m, ok := MonitorForPoint(monitors, x, y); ok {
		if m.DPI > 0 {
			return m.DPI
		}
	}
	for _, m := range monitors {
		if m.Primary && m.DPI > 0 {
			return m.DPI
		}
	}
	if len(monitors) > 0 && monitors[0].DPI > 0 {
		return monitors[0].DPI
	}
	return int(dpiBase)
}

// MonitorForPoint finds the monitor containing (x, y). Windows' own rule is
// "the monitor nearest the point", so a point just outside every monitor is
// assigned to the closest one rather than failing.
func MonitorForPoint(monitors []MonitorInfo, x, y int) (MonitorInfo, bool) {
	if len(monitors) == 0 {
		return MonitorInfo{}, false
	}
	contains := func(m MonitorInfo) bool {
		return x >= m.X0 && x < m.X1 && y >= m.Y0 && y < m.Y1
	}
	for _, m := range monitors {
		if contains(m) {
			return m, true
		}
	}
	best, bestD := monitors[0], -1
	for _, m := range monitors {
		// Squared distance to the rectangle (0 inside).
		dx := 0
		if x < m.X0 {
			dx = m.X0 - x
		} else if x >= m.X1 {
			dx = x - m.X1 + 1
		}
		dy := 0
		if y < m.Y0 {
			dy = m.Y0 - y
		} else if y >= m.Y1 {
			dy = y - m.Y1 + 1
		}
		d := dx*dx + dy*dy
		if bestD < 0 || d < bestD {
			best, bestD = m, d
		}
	}
	return best, true
}

// MonitorByIndex returns the monitor with the given index (0 = primary).
func MonitorByIndex(monitors []MonitorInfo, index int) (MonitorInfo, bool) {
	for _, m := range monitors {
		if m.Index == index {
			return m, true
		}
	}
	// Tolerate a list that is not numbered from 0.
	if index >= 0 && index < len(monitors) {
		return monitors[index], true
	}
	for _, m := range monitors {
		if m.Primary {
			return m, true
		}
	}
	if len(monitors) > 0 {
		return monitors[0], true
	}
	return MonitorInfo{}, false
}

// CanvasSize returns the full overlay canvas (physical px) for one event cell
// of Size pixels and a queue of max simultaneous events.
func CanvasSize(o OverlayConfig) (w, h int) {
	n := o.MaxSimultaneous
	if n < 1 {
		n = 1
	}
	size := o.Size
	if size <= 0 {
		size = 96
	}
	return size*n + 2*CanvasPad, size + 2*CanvasPad
}

// AnchorPosition computes the top-left corner of a w x h window placed inside
// the work rectangle (wx, wy, ww, wh) according to anchor and margin.
//
// The work rectangle is expected in PHYSICAL pixels; the caller scales the
// margin with the monitor DPI before calling (see ResolvePosition).
func AnchorPosition(anchor string, wx, wy, ww, wh, w, h, margin int) (int, int, error) {
	a, ok := NormalizeAnchor(anchor)
	if !ok {
		return 0, 0, fmt.Errorf("overlay.anchor 取值非法: %q", anchor)
	}
	// Horizontal.
	var x int
	switch a {
	case AnchorTopLeft, AnchorMiddleLeft, AnchorBottomLeft:
		x = wx + margin
	case AnchorTopCenter, AnchorCenter, AnchorBottomCenter:
		x = wx + (ww-w)/2
	default: // *-right
		x = wx + ww - w - margin
	}
	// Vertical.
	var y int
	switch a {
	case AnchorTopLeft, AnchorTopCenter, AnchorTopRight:
		y = wy + margin
	case AnchorMiddleLeft, AnchorCenter, AnchorMiddleRight:
		y = wy + (wh-h)/2
	default: // bottom-*
		y = wy + wh - h - margin
	}
	return x, y, nil
}

// ResolvePosition decides the overlay window rectangle.
//
// Rules:
//   - x/y non-zero (either of them)  -> absolute physical coordinates, the
//     anchor and the monitor selection are ignored.
//   - otherwise -> the monitor selected by Monitor (0 = primary), the anchor,
//     and Margin scaled by that monitor's DPI.
//
// The returned rect is (x, y, w, h) in physical pixels and may contain negative
// coordinates (a secondary monitor to the left of / above the primary one).
func ResolvePosition(o OverlayConfig, monitors []MonitorInfo) (x, y, w, h int, err error) {
	w, h = CanvasSize(o)
	if o.X != 0 || o.Y != 0 {
		return o.X, o.Y, w, h, nil
	}
	if len(monitors) == 0 {
		return 0, 0, w, h, errors.New("没有可用的显示器信息，无法按锚点定位")
	}
	m, _ := MonitorByIndex(monitors, o.Monitor)
	scale := m.Scale
	if scale <= 0 {
		scale = float64(m.DPI) / dpiBase
	}
	if scale <= 0 {
		scale = 1
	}
	margin := int(float64(o.Margin)*scale + 0.5)
	wx, wy, ww, wh := m.WorkRect()
	x, y, err = AnchorPosition(o.Anchor, wx, wy, ww, wh, w, h, margin)
	if err != nil {
		return 0, 0, w, h, err
	}
	return x, y, w, h, nil
}

// ResolvePositionSized is ResolvePosition with an explicit canvas size, so the
// expanded hint-card overlay can grow past MaxSimultaneous without moving the
// anchor math.
func ResolvePositionSized(o OverlayConfig, monitors []MonitorInfo, w, h int) (x, y int, err error) {
	if w <= 0 || h <= 0 {
		w, h = CanvasSize(o)
	}
	if o.X != 0 || o.Y != 0 {
		return o.X, o.Y, nil
	}
	if len(monitors) == 0 {
		return 0, 0, errors.New("没有可用的显示器信息，无法按锚点定位")
	}
	m, _ := MonitorByIndex(monitors, o.Monitor)
	scale := m.Scale
	if scale <= 0 {
		scale = float64(m.DPI) / dpiBase
	}
	if scale <= 0 {
		scale = 1
	}
	margin := int(float64(o.Margin)*scale + 0.5)
	wx, wy, ww, wh := m.WorkRect()
	return AnchorPosition(o.Anchor, wx, wy, ww, wh, w, h, margin)
}

// ResolveMonitor returns the monitor the overlay should live on: the one that
// contains the resolved position, or the configured index when that fails.
func ResolveMonitor(o OverlayConfig, monitors []MonitorInfo) (MonitorInfo, bool) {
	x, y, _, _, err := ResolvePosition(o, monitors)
	if err == nil {
		if m, ok := MonitorForPoint(monitors, x, y); ok {
			return m, true
		}
	}
	return MonitorByIndex(monitors, o.Monitor)
}

// FormatRect renders "(x, y, w, h)" for logs.
func FormatRect(x, y, w, h int) string {
	return fmt.Sprintf("(%d, %d, %d, %d)", x, y, w, h)
}

// DescribeAnchor renders an anchor + margin as a human sentence.
func DescribeAnchor(o OverlayConfig) string {
	a, ok := NormalizeAnchor(o.Anchor)
	if !ok {
		a = o.Anchor
	}
	if o.X != 0 || o.Y != 0 {
		return fmt.Sprintf("手动坐标 (%d, %d)，锚点 %s 不生效", o.X, o.Y, a)
	}
	return fmt.Sprintf("锚点 %s，边距 %d 像素（随显示器 DPI 缩放）", a, o.Margin)
}

// AnchorsHelp is the one-line help the CLI prints for the nine anchors.
func AnchorsHelp() string {
	return strings.Join(Anchors, " / ")
}
