package config

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// A3. anchor / coordinate geometry (table driven)
// ---------------------------------------------------------------------------
//
// The fixtures below are the monitors the verification script also reasons
// about, so the numbers here and the numbers the real window reports come from
// the same formulas:
//
//	canvas = (Size*MaxSimultaneous + 2*6, Size + 2*6)  = (300, 108) by default
//	margin = round(Margin * DPI/96)
//
// A monitor to the LEFT of the primary has x < 0, one ABOVE it has y < 0.

func mons() []MonitorInfo {
	return []MonitorInfo{
		{ // 0: primary, 1920x1080, taskbar 40 px at the bottom
			Index: 0, Device: `\\.\DISPLAY1`, Primary: true,
			X0: 0, Y0: 0, X1: 1920, Y1: 1080,
			WX0: 0, WY0: 0, WX1: 1920, WY1: 1040,
			DPI: 96, Scale: 1.0,
		},
		{ // 1: secondary to the LEFT, 150%
			Index: 1, Device: `\\.\DISPLAY2`,
			X0: -1920, Y0: 0, X1: 0, Y1: 1200,
			WX0: -1920, WY0: 0, WX1: 0, WY1: 1152,
			DPI: 144, Scale: 1.5,
		},
		{ // 2: secondary ABOVE the primary, 200%
			Index: 2, Device: `\\.\DISPLAY3`,
			X0: 0, Y0: -1080, X1: 1920, Y1: 0,
			WX0: 0, WY0: -1080, WX1: 1920, WY1: -32,
			DPI: 192, Scale: 2.0,
		},
	}
}

func TestCanvasSize(t *testing.T) {
	cases := []struct {
		size, maxN, wantW, wantH int
	}{
		{96, 3, 300, 108},
		{96, 1, 108, 108},
		{128, 4, 524, 140},
		{0, 0, 108, 108}, // degenerate values fall back to the defaults
		{50, -3, 62, 62}, // negative max is clamped to 1
	}
	for _, tc := range cases {
		o := Default().Overlay
		o.Size, o.MaxSimultaneous = tc.size, tc.maxN
		w, h := CanvasSize(o)
		if w != tc.wantW || h != tc.wantH {
			t.Errorf("CanvasSize(size=%d, max=%d) = %dx%d，期望 %dx%d",
				tc.size, tc.maxN, w, h, tc.wantW, tc.wantH)
		}
	}
}

func TestResolvePositionTable(t *testing.T) {
	const (
		W = 300 // canvas width  for size=96, max=3
		H = 108 // canvas height
	)
	cases := []struct {
		name    string
		monitor int
		anchor  string
		margin  int
		wantX   int
		wantY   int
	}{
		// --- 100% primary (work area 0,0,1920x1040, margin 24) ---------------
		{"主屏 top-left", 0, AnchorTopLeft, 24, 24, 24},
		{"主屏 top-center", 0, AnchorTopCenter, 24, (1920 - W) / 2, 24},
		{"主屏 top-right", 0, AnchorTopRight, 24, 1920 - W - 24, 24},
		{"主屏 middle-left", 0, AnchorMiddleLeft, 24, 24, (1040 - H) / 2},
		{"主屏 center", 0, AnchorCenter, 24, (1920 - W) / 2, (1040 - H) / 2},
		{"主屏 middle-right", 0, AnchorMiddleRight, 24, 1920 - W - 24, (1040 - H) / 2},
		{"主屏 bottom-left", 0, AnchorBottomLeft, 24, 24, 1040 - H - 24},
		{"主屏 bottom-center", 0, AnchorBottomCenter, 24, (1920 - W) / 2, 1040 - H - 24},
		{"主屏 bottom-right", 0, AnchorBottomRight, 24, 1920 - W - 24, 1040 - H - 24},

		// --- 负坐标副屏：主屏左边，150% -> margin 24*1.5 = 36 ----------------
		{"左副屏 top-left (150%)", 1, AnchorTopLeft, 24, -1920 + 36, 0 + 36},
		{"左副屏 center (150%)", 1, AnchorCenter, 24, -1920 + (1920-W)/2, (1152 - H) / 2},
		{"左副屏 bottom-right (150%)", 1, AnchorBottomRight, 24, -1920 + 1920 - W - 36, 1152 - H - 36},
		{"左副屏 middle-left (150%)", 1, AnchorMiddleLeft, 24, -1920 + 36, (1152 - H) / 2},
		{"左副屏 top-right (150%)", 1, AnchorTopRight, 24, -W - 36, 36},

		// --- 负坐标副屏：主屏上方，200% -> margin 24*2 = 48 ------------------
		{"上副屏 top-left (200%)", 2, AnchorTopLeft, 24, 48, -1080 + 48},
		{"上副屏 bottom-center (200%)", 2, AnchorBottomCenter, 24, (1920 - W) / 2, -32 - H - 48},
		{"上副屏 bottom-right (200%)", 2, AnchorBottomRight, 24, 1920 - W - 48, -32 - H - 48},
		{"上副屏 center (200%)", 2, AnchorCenter, 24, (1920 - W) / 2, -1080 + (1048-H)/2},

		// --- margin 0 / margin 100 ------------------------------------------
		{"margin 0", 0, AnchorBottomRight, 0, 1920 - W, 1040 - H},
		{"margin 100 (150%) -> 150", 1, AnchorTopLeft, 100, -1920 + 150, 150},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := Default().Overlay
			o.X, o.Y = 0, 0
			o.Monitor, o.Anchor, o.Margin = tc.monitor, tc.anchor, tc.margin
			x, y, w, h, err := ResolvePosition(o, mons())
			if err != nil {
				t.Fatalf("ResolvePosition: %v", err)
			}
			if w != W || h != H {
				t.Fatalf("画布尺寸 = %dx%d，期望 %dx%d", w, h, W, H)
			}
			if x != tc.wantX || y != tc.wantY {
				t.Errorf("位置 = (%d,%d)，期望 (%d,%d)", x, y, tc.wantX, tc.wantY)
			}
			t.Logf("%-26s 显示器#%d DPI=%d margin=%d -> x=%d y=%d w=%d h=%d",
				tc.name, tc.monitor, mons()[tc.monitor].DPI, tc.margin, x, y, w, h)
		})
	}
}

// TestResolvePositionAllNineAnchorsAtEveryDPI walks the complete 9 x 3 matrix
// and checks the invariants rather than literals: the window must stay inside
// the selected work area, and its distance to the anchored edge must be exactly
// the DPI-scaled margin.
func TestResolvePositionAllNineAnchorsAtEveryDPI(t *testing.T) {
	for _, m := range mons() {
		wx, wy, ww, wh := m.WorkRect()
		for _, anchor := range Anchors {
			o := Default().Overlay
			o.X, o.Y = 0, 0
			o.Monitor, o.Anchor, o.Margin = m.Index, anchor, 24
			x, y, w, h, err := ResolvePosition(o, mons())
			if err != nil {
				t.Fatalf("%s/%s: %v", m.Device, anchor, err)
			}
			scaled := int(24*m.Scale + 0.5)

			if x < wx || y < wy || x+w > wx+ww || y+h > wy+wh {
				t.Errorf("%s %s: 窗口 (%d,%d,%d,%d) 超出工作区 (%d,%d,%d,%d)",
					m.Device, anchor, x, y, w, h, wx, wy, ww, wh)
			}
			// Horizontal edge distance.
			switch anchor {
			case AnchorTopLeft, AnchorMiddleLeft, AnchorBottomLeft:
				if x-wx != scaled {
					t.Errorf("%s %s: 左边距 %d，期望 %d", m.Device, anchor, x-wx, scaled)
				}
			case AnchorTopRight, AnchorMiddleRight, AnchorBottomRight:
				if wx+ww-(x+w) != scaled {
					t.Errorf("%s %s: 右边距 %d，期望 %d", m.Device, anchor, wx+ww-(x+w), scaled)
				}
			default:
				if d := (x - wx) - (wx + ww - (x + w)); d > 1 || d < -1 {
					t.Errorf("%s %s: 水平不居中（左右差 %d）", m.Device, anchor, d)
				}
			}
			// Vertical edge distance.
			switch anchor {
			case AnchorTopLeft, AnchorTopCenter, AnchorTopRight:
				if y-wy != scaled {
					t.Errorf("%s %s: 上边距 %d，期望 %d", m.Device, anchor, y-wy, scaled)
				}
			case AnchorBottomLeft, AnchorBottomCenter, AnchorBottomRight:
				if wy+wh-(y+h) != scaled {
					t.Errorf("%s %s: 下边距 %d，期望 %d", m.Device, anchor, wy+wh-(y+h), scaled)
				}
			default:
				if d := (y - wy) - (wy + wh - (y + h)); d > 1 || d < -1 {
					t.Errorf("%s %s: 垂直不居中（上下差 %d）", m.Device, anchor, d)
				}
			}
			t.Logf("%-14s %-14s DPI=%3d -> (%4d,%4d) %dx%d  margin=%d",
				m.Device, anchor, m.DPI, x, y, w, h, scaled)
		}
	}
}

func TestResolvePositionIgnoresAnchorWhenXYGiven(t *testing.T) {
	// x/y win over anchor AND over the monitor selection, including negative
	// coordinates and a non-zero single axis.
	cases := []struct {
		name    string
		x, y    int
		monitor int
		anchor  string
		wantX   int
		wantY   int
	}{
		{"spec 默认值 1812/984", 1812, 984, 0, AnchorBottomRight, 1812, 984},
		{"只有 x 有值", 700, 0, 0, AnchorTopLeft, 700, 0},
		{"只有 y 有值", 0, 300, 2, AnchorTopLeft, 0, 300},
		{"负坐标（左副屏）", -1000, 200, 0, AnchorBottomRight, -1000, 200},
		{"锚点写成乱七八糟也不影响", 10, 20, 1, "top-left", 10, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := Default().Overlay
			o.X, o.Y, o.Monitor, o.Anchor = tc.x, tc.y, tc.monitor, tc.anchor
			x, y, w, h, err := ResolvePosition(o, mons())
			if err != nil {
				t.Fatalf("ResolvePosition: %v", err)
			}
			if x != tc.wantX || y != tc.wantY {
				t.Errorf("位置 = (%d,%d)，期望 (%d,%d)", x, y, tc.wantX, tc.wantY)
			}
			if w != 300 || h != 108 {
				t.Errorf("画布 = %dx%d，期望 300x108", w, h)
			}
		})
	}
}

func TestResolvePositionWithoutMonitors(t *testing.T) {
	o := Default().Overlay
	o.X, o.Y = 0, 0
	if _, _, _, _, err := ResolvePosition(o, nil); err == nil {
		t.Error("没有任何显示器信息时应报错（锚点无从计算）")
	}
	o.X, o.Y = 12, 34
	x, y, _, _, err := ResolvePosition(o, nil)
	if err != nil {
		t.Fatalf("有显式坐标时不该报错: %v", err)
	}
	if x != 12 || y != 34 {
		t.Errorf("位置 = (%d,%d)，期望 (12,34)", x, y)
	}
}

func TestMonitorLookup(t *testing.T) {
	ms := mons()

	if m, ok := MonitorByIndex(ms, 0); !ok || !m.Primary {
		t.Error("索引 0 应该是主显示器")
	}
	if m, ok := MonitorByIndex(ms, 2); !ok || m.Device != `\\.\DISPLAY3` {
		t.Errorf("索引 2 = %+v", m)
	}
	// Out-of-range index falls back to the primary rather than failing.
	if m, ok := MonitorByIndex(ms, 9); !ok || !m.Primary {
		t.Errorf("越界索引应回退到主显示器，得到 %+v", m)
	}

	// Points inside each monitor, including negative coordinates.
	for _, tc := range []struct {
		x, y int
		want string
	}{
		{100, 100, `\\.\DISPLAY1`},
		{-1900, 10, `\\.\DISPLAY2`},
		{1900, 1100, `\\.\DISPLAY1`}, // 不在任何显示器里 -> 落到最近的主屏
		{10, -1000, `\\.\DISPLAY3`},
		{-1, -1, `\\.\DISPLAY2`}, // 边界像素仍属于左副屏
	} {
		m, ok := MonitorForPoint(ms, tc.x, tc.y)
		if !ok || m.Device != tc.want {
			t.Errorf("MonitorForPoint(%d,%d) = %q，期望 %q", tc.x, tc.y, m.Device, tc.want)
		}
	}
	// Far outside: nearest monitor wins.
	if m, _ := MonitorForPoint(ms, -9000, 500); m.Device != `\\.\DISPLAY2` {
		t.Errorf("远处的点应落到最近的显示器，得到 %q", m.Device)
	}

	// DPI lookup.
	if d := DPIForPoint(ms, -1900, 10); d != 144 {
		t.Errorf("左副屏 DPI = %d，期望 144", d)
	}
	if d := DPIForPoint(ms, 10, -1000); d != 192 {
		t.Errorf("上副屏 DPI = %d，期望 192", d)
	}
	if d := DPIForPoint(ms, 100, 100); d != 96 {
		t.Errorf("主屏 DPI = %d，期望 96", d)
	}
	if d := DPIForPoint(nil, 0, 0); d != 96 {
		t.Errorf("无显示器信息时 DPI = %d，期望 96", d)
	}

	// ResolveMonitor follows the resolved position.
	o := Default().Overlay
	o.X, o.Y = -1900, 300
	if m, _ := ResolveMonitor(o, ms); m.Device != `\\.\DISPLAY2` {
		t.Errorf("显式坐标决定显示器，得到 %q", m.Device)
	}
	o.X, o.Y, o.Monitor = 0, 0, 2
	if m, _ := ResolveMonitor(o, ms); m.Device != `\\.\DISPLAY3` {
		t.Errorf("锚点模式应跟随 monitor=2，得到 %q", m.Device)
	}
}

func TestWorkAreaScaleSanity(t *testing.T) {
	for _, m := range mons() {
		if math.Abs(m.Scale-float64(m.DPI)/96.0) > 1e-9 {
			t.Errorf("%s: Scale %v 与 DPI %d 不一致", m.Device, m.Scale, m.DPI)
		}
		wx, wy, ww, wh := m.WorkRect()
		if ww <= 0 || wh <= 0 {
			t.Errorf("%s: 工作区尺寸非法 %dx%d", m.Device, ww, wh)
		}
		_, _, fw, fh := m.FullRect()
		if fw < ww || fh < wh {
			t.Errorf("%s: 工作区比整屏还大", m.Device)
		}
		if got := FormatRect(wx, wy, ww, wh); got == "" {
			t.Errorf("%s: FormatRect 为空", m.Device)
		}
		_ = wx
		_ = wy
	}
}
