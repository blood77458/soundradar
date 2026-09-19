//go:build windows

package overlay

import (
	"errors"
	"fmt"
	"image"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/znz/soundradar/internal/config"
	"golang.org/x/sys/windows"
)

// ---------------------------------------------------------------------------
// Win32 plumbing
// ---------------------------------------------------------------------------
//
// Only the standard library plus golang.org/x/sys/windows is used: no GUI
// toolkit, no cgo. The handful of user32/gdi32/shcore entry points the overlay
// needs are declared as LazyProc, so nothing is loaded and no window class is
// registered until an Overlay is actually created.

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shcore   = windows.NewLazySystemDLL("shcore.dll")

	procRegisterClassExW          = user32.NewProc("RegisterClassExW")
	procUnregisterClassW          = user32.NewProc("UnregisterClassW")
	procCreateWindowExW           = user32.NewProc("CreateWindowExW")
	procDestroyWindow             = user32.NewProc("DestroyWindow")
	procDefWindowProcW            = user32.NewProc("DefWindowProcW")
	procGetMessageW               = user32.NewProc("GetMessageW")
	procTranslateMessage          = user32.NewProc("TranslateMessage")
	procDispatchMessageW          = user32.NewProc("DispatchMessageW")
	procPostMessageW              = user32.NewProc("PostMessageW")
	procPostQuitMessage           = user32.NewProc("PostQuitMessage")
	procUpdateLayeredWindow       = user32.NewProc("UpdateLayeredWindow")
	procSetWindowPos              = user32.NewProc("SetWindowPos")
	procShowWindow                = user32.NewProc("ShowWindow")
	procGetWindowRect             = user32.NewProc("GetWindowRect")
	procRegisterHotKey            = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey          = user32.NewProc("UnregisterHotKey")
	procSetTimer                  = user32.NewProc("SetTimer")
	procKillTimer                 = user32.NewProc("KillTimer")
	procGetDC                     = user32.NewProc("GetDC")
	procReleaseDC                 = user32.NewProc("ReleaseDC")
	procGetSystemMetrics          = user32.NewProc("GetSystemMetrics")
	procEnumDisplayMonitors       = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW           = user32.NewProc("GetMonitorInfoW")
	procSetProcessDpiAwarenessCtx = user32.NewProc("SetProcessDpiAwarenessContext")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")

	procGetDpiForMonitor = shcore.NewProc("GetDpiForMonitor")
	procGetCurrentThread = kernel32.NewProc("GetCurrentThreadId")
)

// Window styles. This exact combination is the point of the overlay: popup,
// per-pixel transparent, click-through, topmost, never activated, and absent
// from the taskbar / Alt-Tab list.
const (
	wsPopup = 0x80000000

	wsExLayered     = 0x00080000
	wsExTransparent = 0x00000020
	wsExTopmost     = 0x00000008
	wsExNoActivate  = 0x08000000
	wsExToolWindow  = 0x00000080
)

// Window messages. WM_APP+n is the documented private range.
const (
	wmNcCreate   = 0x0081
	wmDestroy    = 0x0002
	wmClose      = 0x0010
	wmTimer      = 0x0113
	wmHotkey     = 0x0312
	wmAppFrame   = 0x8000 + 1 // animation frame: re-composite and blit
	wmAppConfig  = 0x8000 + 2 // re-read geometry from the shared config
	wmAppVisible = 0x8000 + 3 // apply the visible flag
	wmAppQuit    = 0x8000 + 4 // leave the message loop

	hotkeyIDToggle = 1
	timerIDRender  = 1
)

// ShowWindow / SetWindowPos constants.
const (
	swHide           = 0
	swShowNoActivate = 4
	swpNoSize        = 0x0001
	swpNoMove        = 0x0002
	swpNoActivate    = 0x0010
)

// UpdateLayeredWindow / DIB constants.
const (
	ulwAlpha     = 0x00000002
	dibRGBColors = 0
	biRGB        = 0
)

// dpiAwarenessContextPerMonitorAwareV2 is the HANDLE -4 that
// SetProcessDpiAwarenessContext expects.
const dpiAwarenessContextPerMonitorAwareV2 = ^uintptr(0) - 3

const monitorInfoPrimary = 0x00000001

// RenderFrameMs is the animation frame interval (~50 fps): the fade has to be
// smooth and compositing one small canvas costs tens of microseconds.
const RenderFrameMs = 20

// hwndTopmost is (HWND)-1.
var hwndTopmost = ^uintptr(0)

// ---------------------------------------------------------------------------
// Win32 structures
// ---------------------------------------------------------------------------

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type point struct{ X, Y int32 }
type msgStruct struct {
	Hwnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type rect struct{ Left, Top, Right, Bottom int32 }

// size 是 Win32 的 SIZE：**两个 32 位 LONG**（cx、cy），不是两个 16 位半字。
// 这里单独定义类型是刻意的——曾经把尺寸打包成 uint32(h<<16|w) 当 SIZE* 传进
// UpdateLayeredWindow，结果 cx 被读成整个打包值（108<<16|300 = 7078188），
// 窗口被改成 700 万像素宽、0 像素高，ULW 仍返回成功但屏幕上什么都看不到。
type size struct{ CX, CY int32 }

func (r rect) String() string {
	return fmt.Sprintf("(%d,%d)-(%d,%d) %dx%d", r.Left, r.Top, r.Right, r.Bottom,
		r.Right-r.Left, r.Bottom-r.Top)
}

// monitorInfoExW is MONITORINFOEXW: the monitor/work rectangles plus the GDI
// device name.
type monitorInfoExW struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
	SzDevice  [32]uint16
}

type bitmapInfoHeader struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

type rgbQuad struct{ Blue, Green, Red, Reserved byte }

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]rgbQuad
}

// blendFunction is BLENDFUNCTION: AC_SRC_OVER with AC_SRC_ALPHA, which tells the
// compositor to honour the per-pixel alpha channel of the DIB.
type blendFunction struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

var blendAlpha = blendFunction{BlendOp: 0 /* AC_SRC_OVER */, SourceConstantAlpha: 255, AlphaFormat: 1 /* AC_SRC_ALPHA */}

// ---------------------------------------------------------------------------
// IconProvider
// ---------------------------------------------------------------------------

// IconProvider turns an item id into its icon. internal/server implements it on
// top of the library store; the unit tests use a counting provider to prove that
// the cache decodes each icon once.
type IconProvider interface {
	Icon(id string) (image.Image, error)
}

// ---------------------------------------------------------------------------
// Overlay
// ---------------------------------------------------------------------------

// State is a JSON-friendly snapshot of the overlay, used by GET /api/overlay
// and by the CLI self-check.
type State struct {
	Visible   bool                 `json:"visible"`
	Enabled   bool                 `json:"enabled"`
	HWND      uint64               `json:"hwnd"`
	HWNDHex   string               `json:"hwndHex"`
	ClassName string               `json:"className"`
	X         int                  `json:"x"`
	Y         int                  `json:"y"`
	Width     int                  `json:"width"`
	Height    int                  `json:"height"`
	ExStyle   uint32               `json:"exStyle"`
	Style     uint32               `json:"style"`
	Hotkey    string               `json:"hotkey"`
	HotkeyOK  bool                 `json:"hotkeyRegistered"`
	HotkeyErr string               `json:"hotkeyError,omitempty"`
	Pending   int                  `json:"pending"`
	Dropped   int64                `json:"dropped"`
	Frames    int64                `json:"frames"`
	Font      string               `json:"font"`
	FontASCII bool                 `json:"fontAsciiOnly"`
	Monitors  []config.MonitorInfo `json:"-"`
	// MonitorList carries the same monitors with their rectangles (the struct
	// itself is reported without them to keep GET /api/overlay small).
	MonitorList []map[string]any     `json:"monitors"`
	Config      config.OverlayConfig `json:"config"`
	LastError   string               `json:"lastError,omitempty"`
	Alpha       float64              `json:"alpha"`
	// ThreadID is the OS thread running the message loop (proves the
	// LockOSThread model is in place).
	ThreadID uint32 `json:"threadId"`
}

// Overlay is one layered popup window plus its message-loop thread.
type Overlay struct {
	className string

	mu       sync.Mutex
	cfg      config.OverlayConfig
	toggle   string // hotkeys.toggleOverlay, as configured
	queue    *Queue
	icons    IconProvider
	visible  bool
	lastErr  string
	renderer *Renderer
	alpha    float64

	// hwnd is published by the window thread once CreateWindowExW returns.
	hwnd     atomic.Uint64
	hwndErr  error
	hwndOnce sync.Once
	hwndCh   chan struct{}

	threadID atomic.Uint32
	closed   atomic.Bool
	done     chan struct{}

	hotkey    config.Hotkey
	hotkeyOK  bool
	hotkeyErr string
	// hotkeyDone is closed once RegisterHotKey has been attempted, so New()
	// returns only after HotkeyRegistered() has its final answer (otherwise the
	// CLI banner would race with the window thread).
	hotkeyDone chan struct{}
	hotkeyOnce sync.Once

	// peakAlpha remembers the highest per-pixel alpha any frame has reached; the
	// acceptance run's frame dump uses it to keep the most visible frame.
	peakAlpha atomic.Int64

	frames atomic.Int64
}

// New creates the overlay window and starts its message loop.
//
// toggleHotkey is the hotkeys.toggleOverlay value ("F9", "none", ...). cfg is
// applied immediately (position, size, opacity) and icons resolves the ids
// passed to Show. The returned Overlay is safe for concurrent use.
func New(cfg config.OverlayConfig, toggleHotkey string, icons IconProvider) (*Overlay, error) {
	o := &Overlay{
		className:  fmt.Sprintf("SoundRadarOverlay_%d", os.Getpid()),
		cfg:        cfg,
		toggle:     toggleHotkey,
		icons:      icons,
		visible:    cfg.Enabled,
		done:       make(chan struct{}),
		hwndCh:     make(chan struct{}),
		hotkeyDone: make(chan struct{}),
	}
	// PER_MONITOR_AWARE_V2 makes every coordinate a physical pixel, which is what
	// config.json documents. A failure (very old Windows) is recorded but not
	// fatal.
	if err := setProcessDpiAwareness(); err != nil {
		o.lastErr = err.Error()
	}
	o.queue = NewQueue(cfg.MaxSimultaneous, Timing{
		FadeInMs: cfg.FadeInMs, DurationMs: cfg.DurationMs, FadeOutMs: cfg.FadeOutMs,
	})

	go o.windowThread()

	select {
	case <-o.hwndCh:
	case <-time.After(10 * time.Second):
		return nil, errors.New("创建悬浮窗超时（10 秒内窗口线程没有返回）")
	}
	if o.hwndErr != nil {
		return nil, o.hwndErr
	}
	// Wait for the hotkey attempt so HotkeyRegistered() is already final.
	select {
	case <-o.hotkeyDone:
	case <-time.After(3 * time.Second):
	}
	return o, nil
}

// Show queues one event. It never blocks the recognition goroutine: it touches
// the queue under a mutex and posts a message to the window thread.
func (o *Overlay) Show(it DisplayItem) error {
	if o == nil {
		return errors.New("悬浮窗未创建")
	}
	if o.closed.Load() {
		return errors.New("悬浮窗已关闭")
	}
	o.mu.Lock()
	enabled := o.cfg.Enabled
	o.mu.Unlock()
	if !enabled {
		return nil
	}
	if it.Icon == nil && it.ID != "" && o.icons != nil {
		if img, err := o.icons.Icon(it.ID); err == nil {
			it.Icon = img
		}
	}
	o.queue.Show(it)
	hwnd := windows.Handle(o.hwnd.Load())
	if hwnd == 0 {
		return errors.New("悬浮窗句柄尚未就绪")
	}
	// PostMessageW is the ONLY cross-thread window call: the window thread owns
	// the HWND, so resize/blit/visibility all happen there.
	if err := postMessage(hwnd, wmAppFrame, 0, 0); err != nil {
		return fmt.Errorf("通知窗口线程失败: %w", err)
	}
	return nil
}

// ApplyConfig hot-applies a new overlay configuration: position, size, opacity,
// fade timings and queue depth all take effect immediately, with no process
// restart. The hotkey spelling lives in config.Hotkeys and is only re-read by
// ApplyHotkey.
func (o *Overlay) ApplyConfig(cfg config.OverlayConfig) error {
	if o == nil {
		return errors.New("悬浮窗未创建")
	}
	o.mu.Lock()
	oldEnabled := o.cfg.Enabled
	toggle := o.toggle
	o.cfg = cfg
	o.mu.Unlock()

	probe := &config.Config{
		Overlay: cfg,
		Hotkeys: config.HotkeyConfig{ToggleOverlay: toggle},
		Profile: "default",
	}
	if err := probe.Validate(); err != nil {
		return err
	}

	o.queue.SetTiming(Timing{FadeInMs: cfg.FadeInMs, DurationMs: cfg.DurationMs, FadeOutMs: cfg.FadeOutMs})
	o.queue.SetMax(cfg.MaxSimultaneous)

	hwnd := windows.Handle(o.hwnd.Load())
	if hwnd == 0 {
		return errors.New("悬浮窗句柄尚未就绪")
	}
	if cfg.Enabled != oldEnabled {
		_ = o.SetVisible(cfg.Enabled)
	}
	return postMessage(hwnd, wmAppConfig, 0, 0)
}

// ApplyHotkey re-registers the global hotkey. It is a no-op when the spelling
// did not change, so a settings-page save that only moves the window never
// briefly releases the hotkey.
func (o *Overlay) ApplyHotkey(spec string) error {
	if o == nil {
		return errors.New("悬浮窗未创建")
	}
	o.mu.Lock()
	changed := spec != o.toggle
	if changed {
		o.toggle = spec
	}
	o.mu.Unlock()
	if !changed {
		return nil
	}
	hwnd := windows.Handle(o.hwnd.Load())
	if hwnd == 0 {
		return errors.New("悬浮窗句柄尚未就绪")
	}
	o.unregisterHotkey(hwnd)
	o.registerHotkey(hwnd)
	return nil
}

// ToggleHotkey returns the configured hotkey spelling.
func (o *Overlay) ToggleHotkey() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.toggle
}

// SetVisible shows or hides the window (also the F9 hotkey path).
func (o *Overlay) SetVisible(v bool) error {
	if o == nil {
		return errors.New("悬浮窗未创建")
	}
	hwnd := windows.Handle(o.hwnd.Load())
	if hwnd == 0 {
		return errors.New("悬浮窗句柄尚未就绪")
	}
	o.mu.Lock()
	o.visible = v
	o.mu.Unlock()
	if err := postMessage(hwnd, wmAppVisible, boolToUintptr(v), 0); err != nil {
		return fmt.Errorf("切换可见性失败: %w", err)
	}
	return nil
}

// Visible reports the last requested visibility.
func (o *Overlay) Visible() bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.visible
}

// Rect returns the window rectangle in physical pixels.
func (o *Overlay) Rect() (x, y, w, h int, err error) {
	if o == nil {
		return 0, 0, 0, 0, errors.New("悬浮窗未创建")
	}
	hwnd := windows.Handle(o.hwnd.Load())
	if hwnd == 0 {
		return 0, 0, 0, 0, errors.New("悬浮窗句柄尚未就绪")
	}
	var r rect
	if err := getWindowRect(hwnd, &r); err != nil {
		return 0, 0, 0, 0, err
	}
	if r.Right < r.Left || r.Bottom < r.Top || r.Right-r.Left > 20000 || r.Bottom-r.Top > 20000 {
		return 0, 0, 0, 0, fmt.Errorf("GetWindowRect 返回了不合理的数据 %s（hwnd=%#x）", r, uintptr(hwnd))
	}
	return int(r.Left), int(r.Top), int(r.Right - r.Left), int(r.Bottom - r.Top), nil
}

// HWND returns the window handle (0 before the window thread publishes it).
func (o *Overlay) HWND() uintptr { return uintptr(o.hwnd.Load()) }

// ClassName returns the window class name (the verification script feeds it to
// FindWindowW).
func (o *Overlay) ClassName() string { return o.className }

// HotkeyRegistered reports the configured hotkey and whether RegisterHotKey
// succeeded.
func (o *Overlay) HotkeyRegistered() (string, bool) {
	if o == nil {
		return "", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.hotkey.VK == 0 {
		return "none", false
	}
	return o.hotkey.String(), o.hotkeyOK
}

// HotkeyError returns the RegisterHotKey failure, if any.
func (o *Overlay) HotkeyError() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hotkeyErr
}

// Frames counts rendered frames (evidence that the window is being repainted).
func (o *Overlay) Frames() int64 { return o.frames.Load() }

// ThreadID returns the OS thread that owns the window message queue.
func (o *Overlay) ThreadID() uint32 { return o.threadID.Load() }

// RectDump is a debugging/reporting helper: it returns both GetWindowRect and
// GetClientRect as raw quadrants plus the derived sizes, so a mismatch between
// what the process thinks the window is and what another process sees can be
// diagnosed from the outside (GET /api/overlay exposes it).
func (o *Overlay) RectDump() map[string]any {
	hwnd := windows.Handle(o.hwnd.Load())
	out := map[string]any{"hwndHex": fmt.Sprintf("0x%X", uintptr(hwnd))}
	if hwnd == 0 {
		return out
	}
	var wr rect
	if err := getWindowRect(hwnd, &wr); err == nil {
		out["windowRect"] = []int32{wr.Left, wr.Top, wr.Right, wr.Bottom}
		out["windowSize"] = []int32{wr.Right - wr.Left, wr.Bottom - wr.Top}
	} else {
		out["windowRectError"] = err.Error()
	}
	out["style"] = windowStyle()
	out["exStyle"] = exStyle()
	if x, y, w, h, err := o.Rect(); err == nil {
		out["resolved"] = []int{x, y, w, h}
	} else {
		out["resolvedError"] = err.Error()
	}
	return out
}

// State renders a snapshot for the API / CLI.
func (o *Overlay) State() State {
	o.mu.Lock()
	cfg := o.cfg
	vis := o.visible
	lastErr := o.lastErr
	hk, hkOK, hkErr := o.hotkey, o.hotkeyOK, o.hotkeyErr
	rend := o.renderer
	alpha := o.alpha
	o.mu.Unlock()

	hwnd := o.hwnd.Load()
	mons := Monitors()
	list := make([]map[string]any, 0, len(mons))
	for _, m := range mons {
		list = append(list, monitorJSON(m))
	}
	st := State{
		Visible:     vis,
		Enabled:     cfg.Enabled,
		HWND:        hwnd,
		HWNDHex:     fmt.Sprintf("0x%X", hwnd),
		ClassName:   o.className,
		ExStyle:     exStyle(),
		Style:       windowStyle(),
		Hotkey:      hk.String(),
		HotkeyOK:    hkOK,
		HotkeyErr:   hkErr,
		Pending:     o.queue.Len(),
		Dropped:     o.queue.Dropped(),
		Frames:      o.frames.Load(),
		MonitorList: list,
		Config:      cfg,
		LastError:   lastErr,
		Alpha:       alpha,
		ThreadID:    o.ThreadID(),
	}
	if rend != nil {
		st.Font = rend.FontName()
		st.FontASCII = rend.ASCIIOnly()
	}
	if x, y, w, h, err := o.Rect(); err == nil {
		st.X, st.Y, st.Width, st.Height = x, y, w, h
	}
	return st
}

// The exact style bits the window was created with. They are captured at
// CreateWindowExW time so State can report them; the verification script
// independently reads them back from the live window with GetWindowLongW.
var (
	styleMu    sync.RWMutex
	styleValue uint32
	exStyleVal uint32
)

func exStyle() uint32 {
	styleMu.RLock()
	defer styleMu.RUnlock()
	return exStyleVal
}

func windowStyle() uint32 {
	styleMu.RLock()
	defer styleMu.RUnlock()
	return styleValue
}

// Close tears the window down and waits for the message loop to exit.
func (o *Overlay) Close() error {
	if o == nil {
		return nil
	}
	if o.closed.Swap(true) {
		return nil
	}
	hwnd := windows.Handle(o.hwnd.Load())
	if hwnd == 0 {
		<-o.done
		return nil
	}
	_ = postMessage(hwnd, wmAppQuit, 0, 0)
	select {
	case <-o.done:
	case <-time.After(5 * time.Second):
		return errors.New("关闭悬浮窗超时（5 秒）")
	}
	return nil
}

// ---------------------------------------------------------------------------
// the window thread
// ---------------------------------------------------------------------------

// windowThread owns the HWND for its whole life. It is the only goroutine that
// mutates the window; the exported methods talk to it through PostMessageW.
func (o *Overlay) windowThread() {
	// A window's message queue belongs to a THREAD, so this goroutine must never
	// migrate: LockOSThread keeps the GetMessageW loop, the animation timer and
	// RegisterHotKey's WM_HOTKEY delivery on the same OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(o.done)

	tid, _, _ := procGetCurrentThread.Call()
	o.threadID.Store(uint32(tid))

	hwnd, err := o.createWindow()
	if err != nil {
		o.hwndErr = err
		o.hwndOnce.Do(func() { close(o.hwndCh) })
		return
	}
	o.hwnd.Store(uint64(hwnd))
	o.hwndOnce.Do(func() { close(o.hwndCh) })

	// RegisterHotKey must run on this thread: WM_HOTKEY is posted to the thread
	// that registered the hotkey.
	o.registerHotkey(hwnd)

	o.applyGeometry()
	o.renderFrame()
	o.startTimer(hwnd)

	var msg msgStruct
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 { // 0 = WM_QUIT, -1 = error
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
	o.stopTimer(hwnd)
	o.unregisterHotkey(hwnd)
	procDestroyWindow.Call(uintptr(hwnd))
	procUnregisterClassW.Call(uintptr(unsafe.Pointer(utf16Ptr(o.className))), 0)
}

// wndProcRef pins the callback so the GC cannot collect it: NewCallback returns
// a bare function pointer that must stay alive for as long as the class does.
var wndProcRef func(hwnd windows.Handle, msg uint32, wparam, lparam uintptr) uintptr

var overlayByHW = struct {
	sync.RWMutex
	m map[windows.Handle]*Overlay
}{m: make(map[windows.Handle]*Overlay)}

func registerWndProc(hwnd windows.Handle, o *Overlay) {
	overlayByHW.Lock()
	overlayByHW.m[hwnd] = o
	overlayByHW.Unlock()
}

func unregisterWndProc(hwnd windows.Handle) {
	overlayByHW.Lock()
	delete(overlayByHW.m, hwnd)
	overlayByHW.Unlock()
}

func lookupOverlay(hwnd windows.Handle) *Overlay {
	overlayByHW.RLock()
	defer overlayByHW.RUnlock()
	return overlayByHW.m[hwnd]
}

// wndProc runs on the window thread (dispatched by DispatchMessageW), so it may
// call user32 directly.
func wndProc(hwnd windows.Handle, msg uint32, wparam, lparam uintptr) uintptr {
	if msg == wmNcCreate {
		return 1 // continue window creation
	}
	o := lookupOverlay(hwnd)
	if o == nil {
		ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
		return ret
	}
	switch msg {
	case wmAppFrame, wmTimer:
		o.renderFrame()
		return 0
	case wmAppConfig:
		o.applyGeometry()
		o.renderFrame()
		return 0
	case wmAppVisible:
		o.applyVisible(wparam != 0)
		return 0
	case wmHotkey:
		if uint32(wparam) == hotkeyIDToggle {
			o.toggleVisible()
			return 0
		}
	case wmAppQuit, wmClose:
		procPostQuitMessage.Call(0)
		return 0
	case wmDestroy:
		unregisterWndProc(hwnd)
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
	return ret
}

// createWindow registers the class and creates the layered popup, hidden.
func (o *Overlay) createWindow() (windows.Handle, error) {
	if wndProcRef == nil {
		wndProcRef = wndProc
	}
	className := utf16Ptr(o.className)
	title := utf16Ptr("SoundRadar Overlay")

	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   windows.NewCallback(wndProcRef),
		LpszClassName: className,
	}
	atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		return 0, fmt.Errorf("RegisterClassExW(%s) 失败: %v", o.className, callErr)
	}

	x, y, w, h, err := o.geometry()
	if err != nil {
		procUnregisterClassW.Call(uintptr(unsafe.Pointer(className)), 0)
		return 0, err
	}

	style := uint32(wsPopup)
	ex := uint32(wsExLayered | wsExTransparent | wsExTopmost | wsExNoActivate | wsExToolWindow)
	styleMu.Lock()
	styleValue, exStyleVal = style, ex
	styleMu.Unlock()

	hwnd, _, callErr := procCreateWindowExW.Call(
		uintptr(ex), uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		uintptr(style), uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		0, 0, 0, 0,
	)
	if hwnd == 0 {
		procUnregisterClassW.Call(uintptr(unsafe.Pointer(className)), 0)
		return 0, fmt.Errorf("CreateWindowExW 失败: %v", callErr)
	}
	o.hwnd.Store(uint64(hwnd))
	registerWndProc(windows.Handle(hwnd), o)

	// WS_EX_NOACTIVATE plus SW_SHOWNOACTIVATE: showing the overlay must never
	// move the foreground window, or it would take the game out of focus.
	showWindow(windows.Handle(hwnd), o.Visible())
	return windows.Handle(hwnd), nil
}

// geometry resolves the window rectangle from the current config.
func (o *Overlay) geometry() (x, y, w, h int, err error) {
	o.mu.Lock()
	cfg := o.cfg
	o.mu.Unlock()
	return config.ResolvePosition(cfg, Monitors())
}

// applyGeometry repositions and resizes the window for the current config.
func (o *Overlay) applyGeometry() {
	hwnd := windows.Handle(o.hwnd.Load())
	if hwnd == 0 {
		return
	}
	x, y, w, h, err := o.geometry()
	if err != nil {
		o.setErr(err)
		return
	}
	o.rebuildRenderer(w, h)
	ret, _, callErr := procSetWindowPos.Call(uintptr(hwnd), hwndTopmost,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h), swpNoActivate)
	if ret == 0 {
		o.setErr(fmt.Errorf("SetWindowPos(%d,%d,%d,%d) 失败: %v", x, y, w, h, callErr))
	}
}

// rebuildRenderer recreates the renderer when the canvas shape changes.
func (o *Overlay) rebuildRenderer(w, h int) {
	o.mu.Lock()
	cfg := o.cfg
	rend := o.renderer
	o.mu.Unlock()
	if rend != nil {
		cur := rend.Options()
		if cur.Width == w && cur.Height == h &&
			cur.ShowName == cfg.ShowName && cur.ShowScore == cfg.ShowScore {
			return
		}
	}
	size := cfg.Size
	if size <= 0 {
		size = 96
	}
	nr, err := NewRenderer(RenderOptions{
		Width: w, Height: h, IconSize: size,
		ShowName: cfg.ShowName, ShowScore: cfg.ShowScore, Alpha: cfg.Opacity,
	})
	if err != nil {
		o.setErr(err)
		return
	}
	o.mu.Lock()
	o.renderer = nr
	o.mu.Unlock()
}

// renderFrame composites the queue and blits the result.
func (o *Overlay) renderFrame() {
	hwnd := windows.Handle(o.hwnd.Load())
	if hwnd == 0 {
		return
	}
	o.mu.Lock()
	cfg := o.cfg
	rend := o.renderer
	o.mu.Unlock()
	if rend == nil {
		return
	}

	timing := Timing{FadeInMs: cfg.FadeInMs, DurationMs: cfg.DurationMs, FadeOutMs: cfg.FadeOutMs}
	items := o.queue.Snapshot(time.Now())
	alpha := 0.0
	if len(items) > 0 {
		alpha = OverallAlpha(items, timing) * cfg.Opacity
	}
	rend.SetAlpha(alpha)
	canvas := rend.Draw(items, int(o.frames.Load()))
	if err := o.blit(hwnd, canvas, o.frames.Load(), int64(alpha*1000000)); err != nil {
		o.setErr(err)
		return
	}
	// 注意：早期版本在这里每帧调用 reassertGeometry() 去"修"被 UpdateLayeredWindow
	// 写坏的窗口矩形。那其实是在给 blit 里传错 SIZE 打补丁；真正的根因（SIZE 被
	// 打包成 uint32）修掉之后，ULW 不会再动窗口矩形，这段补丁已删除。
	// 将来若又看到矩形异常，先怀疑 blit 传给 ULW 的参数，而不是再打补丁。
	o.mu.Lock()
	o.alpha = alpha
	o.mu.Unlock()
	o.frames.Add(1)
}

// applyVisible shows/hides the window without ever activating it.
func (o *Overlay) applyVisible(v bool) {
	hwnd := windows.Handle(o.hwnd.Load())
	if hwnd == 0 {
		return
	}
	showWindow(hwnd, v)
}

func (o *Overlay) toggleVisible() {
	o.mu.Lock()
	o.visible = !o.visible
	v := o.visible
	o.mu.Unlock()
	o.applyVisible(v)
}

func (o *Overlay) setErr(err error) {
	if err == nil {
		return
	}
	o.mu.Lock()
	o.lastErr = err.Error()
	o.mu.Unlock()
}

// ---------------------------------------------------------------------------
// layered-window blit
// ---------------------------------------------------------------------------

// blit hands the RGBA canvas to the compositor.
//
// The path is: RGBA (Go's image.RGBA is alpha-premultiplied) -> BGRA
// premultiplied bytes in a top-down 32bpp DIBSection -> UpdateLayeredWindow.
//
// Premultiplying in Go rather than in GDI is not an optimisation, it is the only
// way the window is visible at all: GDI does NOT write the alpha byte when it
// renders into a 32bpp DIB (it stays 0), so anything drawn through GDI -
// TextOutW included - arrives fully transparent. Compositing in Go also keeps
// the renderer unit-testable.
func (o *Overlay) blit(hwnd windows.Handle, canvas *image.RGBA, frame, canvasAlpha int64) error {
	b := canvas.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}

	screenDC, _, callErr := procGetDC.Call(0)
	if screenDC == 0 {
		return fmt.Errorf("GetDC(NULL) 失败: %v", callErr)
	}
	defer procReleaseDC.Call(0, screenDC)

	memDC, _, callErr := procCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		return fmt.Errorf("CreateCompatibleDC 失败: %v", callErr)
	}
	defer procDeleteDC.Call(memDC)

	bi := bitmapInfo{
		Header: bitmapInfoHeader{
			BiSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
			BiWidth:       int32(w),
			BiHeight:      -int32(h), // negative height = top-down rows
			BiPlanes:      1,
			BiBitCount:    32,
			BiCompression: biRGB,
			BiSizeImage:   uint32(w * h * 4),
		},
	}
	var bits unsafe.Pointer
	dib, _, callErr := procCreateDIBSection.Call(
		memDC, uintptr(unsafe.Pointer(&bi)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0,
	)
	if dib == 0 || bits == nil {
		return fmt.Errorf("CreateDIBSection 失败: %v", callErr)
	}
	defer procDeleteObject.Call(dib)

	old, _, _ := procSelectObject.Call(memDC, dib)
	defer procSelectObject.Call(memDC, old)

	dst := unsafe.Slice((*byte)(bits), w*h*4)
	src := canvas.Pix
	stride := canvas.Stride
	premultMax := 0
	for y := 0; y < h; y++ {
		row := src[y*stride:]
		out := dst[y*w*4:]
		for x := 0; x < w; x++ {
			a := uint32(row[x*4+3])
			if a == 0 {
				out[x*4+0], out[x*4+1], out[x*4+2], out[x*4+3] = 0, 0, 0, 0
				continue
			}
			// canvas 是 image.RGBA，而 Go 的 image.RGBA **本身就是预乘 alpha 的**
			// （image/draw 与 font.Drawer 写入时已预乘）。所以这里只做 RGBA→BGRA
			// 重排，**绝不能再乘一次 alpha**——重复预乘会让整体偏暗
			// （opacity 0.85 时每个像素又被乘 0.85，低 alpha 的文字几乎消失）。
			out[x*4+0] = row[x*4+2] // Blue
			out[x*4+1] = row[x*4+1] // Green
			out[x*4+2] = row[x*4+0] // Red
			out[x*4+3] = row[x*4+3] // Alpha（已预乘）
			if int(a) > premultMax {
				premultMax = int(a)
			}
		}
	}
	// SR_OVERLAY_DUMP=<path> writes exactly the bytes handed to the compositor,
	// which is the only way to tell "the canvas is empty" apart from "the DIB or
	// UpdateLayeredWindow lost the pixels" from the outside. Only frames that
	// actually carry content are written, so the file on disk is always the most
	// recent *visible* frame rather than the last empty one.
	//
	// In addition, the frame with the highest alpha seen so far is kept as
	// <path>.peak.bgra: the fade in/out means the "latest" frame is often a
	// nearly transparent one, and the acceptance run needs the most visible
	// frame to compare against the icon colour.
	if p := os.Getenv("SR_OVERLAY_DUMP"); p != "" && premultMax > 40 {
		raw := make([]byte, len(dst))
		copy(raw, dst)
		_ = os.WriteFile(p+".bgra", raw, 0o644)
		// Keep the most opaque frame seen so far: with the fade in/out the
		// "latest" frame is usually a nearly transparent one, while the
		// acceptance run needs the frame showing the icon at full strength.
		// The decision uses the window's own per-frame alpha (passed in as
		// canvasAlpha) because premultMax saturates at 217 as soon as a single
		// glyph pixel is opaque, which would freeze the peak on the very first
		// partially faded frame.
		if canvasAlpha > o.peakAlpha.Load() {
			o.peakAlpha.Store(canvasAlpha)
			_ = os.WriteFile(p+".peak.bgra", raw, 0o644)
			_ = os.WriteFile(p+".peak.txt", []byte(fmt.Sprintf(
				"size=%dx%d maxAlpha=%d frame=%d windowAlpha=%.4f\n",
				w, h, premultMax, frame, float64(canvasAlpha)/1000000)), 0o644)
		}
	}

	// pptDst 指向目标屏幕位置；psize 必须是一个真正的 SIZE 结构体（两个 32 位
	// LONG）。早期版本把尺寸打包成一个 uint32 传进来，ULW 把 cx 读成整个打包值
	// （108<<16|300 = 7078188）、cy 读到栈上的垃圾，于是窗口变成 700 万像素宽、
	// 0 像素高的图层：ULW 返回 1、帧缓冲内容也对，但屏幕上永远什么都看不到，
	// 并且每帧都会把窗口矩形写坏（R=7080424 B=1260）。改回真正的 SIZE 后正常。
	var wr rect
	dstPoint := point{0, 0}
	if getWindowRect(hwnd, &wr) == nil {
		dstPoint = point{wr.Left, wr.Top}
	}
	sz := size{CX: int32(w), CY: int32(h)}
	srcPoint := point{0, 0}
	ret, _, callErr := procUpdateLayeredWindow.Call(
		uintptr(hwnd), screenDC, uintptr(unsafe.Pointer(&dstPoint)), uintptr(unsafe.Pointer(&sz)),
		memDC, uintptr(unsafe.Pointer(&srcPoint)), 0,
		uintptr(unsafe.Pointer(&blendAlpha)), ulwAlpha,
	)
	if ret == 0 {
		return fmt.Errorf("UpdateLayeredWindow 失败: %v", callErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// hotkey / timer helpers
// ---------------------------------------------------------------------------

func (o *Overlay) registerHotkey(hwnd windows.Handle) {
	defer o.hotkeyOnce.Do(func() { close(o.hotkeyDone) })
	o.mu.Lock()
	raw := o.toggle
	o.mu.Unlock()
	hk, err := config.ParseHotkey(raw)
	if err != nil {
		o.mu.Lock()
		o.hotkey, o.hotkeyOK, o.hotkeyErr = config.Hotkey{}, false, err.Error()
		o.mu.Unlock()
		return
	}
	if hk.VK == 0 {
		o.mu.Lock()
		o.hotkey, o.hotkeyOK, o.hotkeyErr = hk, false, ""
		o.mu.Unlock()
		return
	}
	r, _, callErr := procRegisterHotKey.Call(uintptr(hwnd), hotkeyIDToggle,
		uintptr(hk.Modifiers), uintptr(hk.VK))
	ok := r != 0
	msg := ""
	if !ok {
		msg = fmt.Sprintf("RegisterHotKey(%s) 失败: %v（可能已被其它程序占用）", hk.String(), callErr)
	}
	o.mu.Lock()
	o.hotkey, o.hotkeyOK, o.hotkeyErr = hk, ok, msg
	o.mu.Unlock()
}

func (o *Overlay) unregisterHotkey(hwnd windows.Handle) {
	o.mu.Lock()
	hk := o.hotkey
	o.mu.Unlock()
	if hk.VK == 0 {
		return
	}
	procUnregisterHotKey.Call(uintptr(hwnd), hotkeyIDToggle)
}

func (o *Overlay) startTimer(hwnd windows.Handle) {
	procSetTimer.Call(uintptr(hwnd), timerIDRender, uintptr(RenderFrameMs), 0)
}

func (o *Overlay) stopTimer(hwnd windows.Handle) {
	procKillTimer.Call(uintptr(hwnd), timerIDRender)
}

// ---------------------------------------------------------------------------
// small Win32 wrappers
// ---------------------------------------------------------------------------

func postMessage(hwnd windows.Handle, msg uint32, wparam, lparam uintptr) error {
	ret, _, callErr := procPostMessageW.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
	if ret == 0 {
		return callErr
	}
	return nil
}

func showWindow(hwnd windows.Handle, show bool) {
	if show {
		// Stay topmost without moving or resizing, then show WITHOUT activating
		// (SW_SHOWNOACTIVATE).
		procSetWindowPos.Call(uintptr(hwnd), hwndTopmost, 0, 0, 0, 0,
			swpNoMove|swpNoSize|swpNoActivate)
		procShowWindow.Call(uintptr(hwnd), swShowNoActivate)
		return
	}
	procShowWindow.Call(uintptr(hwnd), swHide)
}

func getWindowRect(hwnd windows.Handle, r *rect) error {
	ret, _, callErr := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(r)))
	if ret == 0 {
		return fmt.Errorf("GetWindowRect 失败: %v", callErr)
	}
	return nil
}

// setProcessDpiAwareness asks Windows for PER_MONITOR_AWARE_V2 so every
// coordinate the overlay uses is a physical pixel, exactly like config.json
// documents.
func setProcessDpiAwareness() error {
	if err := procSetProcessDpiAwarenessCtx.Find(); err != nil {
		return fmt.Errorf("SetProcessDpiAwarenessContext 不可用: %w", err)
	}
	ret, _, callErr := procSetProcessDpiAwarenessCtx.Call(dpiAwarenessContextPerMonitorAwareV2)
	if ret == 0 {
		return fmt.Errorf("SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2) 失败: %v", callErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// monitor enumeration
// ---------------------------------------------------------------------------

// Monitors lists every display with its full rectangle, work area, effective DPI
// and primary flag. Coordinates are physical pixels and may be negative.
func Monitors() []config.MonitorInfo {
	found := make([]config.MonitorInfo, 0, 4)
	cb := windows.NewCallback(func(hmon, hdc, lprc, data uintptr) uintptr {
		var mie monitorInfoExW
		mie.CbSize = uint32(unsafe.Sizeof(mie))
		ret, _, _ := procGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mie)))
		if ret == 0 {
			return 1
		}
		sc := getMonitorScale(hmon)
		found = append(found, config.MonitorInfo{
			Device: windows.UTF16ToString(mie.SzDevice[:]),
			X0:     int(mie.RcMonitor.Left), Y0: int(mie.RcMonitor.Top),
			X1: int(mie.RcMonitor.Right), Y1: int(mie.RcMonitor.Bottom),
			WX0: int(mie.RcWork.Left), WY0: int(mie.RcWork.Top),
			WX1: int(mie.RcWork.Right), WY1: int(mie.RcWork.Bottom),
			DPI: sc.dpi, Scale: sc.scale,
			Primary: mie.DwFlags&monitorInfoPrimary != 0,
		})
		return 1
	})
	procEnumDisplayMonitors.Call(0, 0, cb, 0)

	// Primary first: "monitor: 0" means the primary monitor everywhere else.
	out := make([]config.MonitorInfo, 0, len(found))
	for _, m := range found {
		if m.Primary {
			out = append(out, m)
		}
	}
	for _, m := range found {
		if !m.Primary {
			out = append(out, m)
		}
	}
	for i := range out {
		out[i].Index = i
	}
	if len(out) == 0 {
		w, _, _ := procGetSystemMetrics.Call(0)
		h, _, _ := procGetSystemMetrics.Call(1)
		out = append(out, config.MonitorInfo{
			Index: 0, Device: "virtual-screen", Primary: true,
			X0: 0, Y0: 0, X1: int(int32(w)), Y1: int(int32(h)),
			WX0: 0, WY0: 0, WX1: int(int32(w)), WY1: int(int32(h)),
			DPI: 96, Scale: 1,
		})
	}
	return out
}

type scaleInfo struct {
	dpi   int
	scale float64
}

// getMonitorScale reports the monitor's effective DPI (Shcore!GetDpiForMonitor
// when available, else 96 = 100%).
func getMonitorScale(hmon uintptr) scaleInfo {
	if err := procGetDpiForMonitor.Find(); err == nil {
		const mdtEffectiveDPI = 0
		var dx, dy uint32
		ret, _, _ := procGetDpiForMonitor.Call(hmon, uintptr(mdtEffectiveDPI),
			uintptr(unsafe.Pointer(&dx)), uintptr(unsafe.Pointer(&dy)))
		if ret == 0 && dx > 0 {
			return scaleInfo{dpi: int(dx), scale: float64(dx) / 96.0}
		}
	}
	return scaleInfo{dpi: 96, scale: 1}
}

// MonitorInfo for the state payload needs the rectangles too, so the settings
// page (and the verification script) can show the real coordinates.
func monitorJSON(m config.MonitorInfo) map[string]any {
	fx, fy, fw, fh := m.FullRect()
	wx, wy, ww, wh := m.WorkRect()
	return map[string]any{
		"index": m.Index, "device": m.Device, "primary": m.Primary,
		"dpi": m.DPI, "scale": m.Scale,
		"full":  [4]int{fx, fy, fw, fh},
		"work":  [4]int{wx, wy, ww, wh},
		"label": fmt.Sprintf("#%d %s %dx%d @ (%d,%d) DPI %d", m.Index, m.Device, ww, wh, wx, wy, m.DPI),
	}
}

func utf16Ptr(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		// UTF16PtrFromString only fails on an embedded NUL, which never occurs
		// for the class name / window title used here.
		panic(err)
	}
	return p
}

func boolToUintptr(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}
