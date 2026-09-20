//go:build windows

package tray

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---------------------------------------------------------------------------
// Win32 plumbing (stdlib + golang.org/x/sys/windows only, no cgo)
// ---------------------------------------------------------------------------

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")

	procRegisterClassEx   = user32.NewProc("RegisterClassExW")
	procUnregisterClass   = user32.NewProc("UnregisterClassW")
	procCreateWindowEx    = user32.NewProc("CreateWindowExW")
	procDestroyWindow     = user32.NewProc("DestroyWindow")
	procDefWindowProc     = user32.NewProc("DefWindowProcW")
	procGetMessage        = user32.NewProc("GetMessageW")
	procTranslateMsg      = user32.NewProc("TranslateMessage")
	procDispatchMsg       = user32.NewProc("DispatchMessageW")
	procPostMessage       = user32.NewProc("PostMessageW")
	procPeekMessage       = user32.NewProc("PeekMessageW")
	procCreatePopupMenu   = user32.NewProc("CreatePopupMenu")
	procAppendMenu        = user32.NewProc("AppendMenuW")
	procDestroyMenu       = user32.NewProc("DestroyMenu")
	procTrackPopupMenu    = user32.NewProc("TrackPopupMenu")
	procSetForeground     = user32.NewProc("SetForegroundWindow")
	procGetCursorPos      = user32.NewProc("GetCursorPos")
	procLoadIcon          = user32.NewProc("LoadIconW")
	procDestroyIcon       = user32.NewProc("DestroyIcon")
	procGetSystemMetrics  = user32.NewProc("GetSystemMetrics")
	procRegisterWindowMsg = user32.NewProc("RegisterWindowMessageW")
	procCreateIconFromRes = user32.NewProc("CreateIconFromResourceEx")
	procLookupIconId      = user32.NewProc("LookupIconIdFromDirectoryEx")
	procGetCurrentTID     = kernel32.NewProc("GetCurrentThreadId")
	procShellNotifyIcon   = shell32.NewProc("Shell_NotifyIconW")
	procShellExecute      = shell32.NewProc("ShellExecuteW")
	procExtractIcon       = shell32.NewProc("ExtractIconExW")
)

// Window messages and notification-area constants.
const (
	wmNull       = 0x0000
	wmCommand    = 0x0111
	wmContext    = 0x007B
	wmLButtonUp  = 0x0202
	wmLButtonDbl = 0x0203
	wmRButtonUp  = 0x0205
	// wmAppCmd is posted by other goroutines to wake GetMessageW; the actual
	// request sits on the channel.
	wmAppCmd = 0x8000 + 1
	// wmTray is the callback message the shell sends for our icon.
	wmTray = 0x8000 + 2
	// trayCallbackMsg is the message id the icon is registered with.
	trayCallbackMsg = wmTray

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002
	// nimSetVersion must be passed as uFlags (not as a message) to select the
	// modern NOTIFYICONDATAW tail.
	nimSetVersion = 0x00000004

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	niifInfo = 0x00000001

	lrDefaultSize = 0x00000040
	lrShared      = 0x00008000

	mfString    = 0x00000000
	mfSeparator = 0x00000800
	mfChecked   = 0x00000008
	mfGrayed    = 0x00000001

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	swShowNormal = 1

	smCXSmallIcon = 49
	smCYSmallIcon = 50

	idiApplication = 32512

	// The "taskbar was created/recreated" broadcast.
	taskbarCreatedName = "TaskbarCreated"
)

// notifyIconDataW mirrors NOTIFYICONDATAW (Vista+ layout). Field order and sizes
// matter: the struct is handed to Shell_NotifyIconW by pointer.
type notifyIconDataW struct {
	CbSize           uint32
	HWnd             windows.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            windows.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     windows.Handle
}

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

type msgStruct struct {
	Hwnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

type point struct{ X, Y int32 }

// wndProcRef pins the callback: windows.NewCallback returns a bare function
// pointer that the garbage collector cannot see.
var wndProcRef func(hwnd windows.Handle, msg uint32, wparam, lparam uintptr) uintptr

// trays maps an HWND to its Tray for the lifetime of the window.
var trays = struct {
	mu sync.RWMutex
	m  map[windows.Handle]*Tray
}{m: make(map[windows.Handle]*Tray)}

func registerTray(hwnd windows.Handle, t *Tray) {
	trays.mu.Lock()
	trays.m[hwnd] = t
	trays.mu.Unlock()
}

func unregisterTray(hwnd windows.Handle) {
	trays.mu.Lock()
	delete(trays.m, hwnd)
	trays.mu.Unlock()
}

func lookupTray(hwnd windows.Handle) *Tray {
	trays.mu.RLock()
	defer trays.mu.RUnlock()
	return trays.m[hwnd]
}

// ---------------------------------------------------------------------------
// message thread
// ---------------------------------------------------------------------------

// messageThread owns the window, the icon registration and the message loop for
// its whole life, locked to one OS thread: a window's message queue belongs to a
// thread, so GetMessageW, the window and the shell callbacks must share one.
func (t *Tray) messageThread() {
	runtime.LockOSThread()
	// Unlock last; the queue is drained before the thread returns to the Go pool
	// so a leftover WM_QUIT cannot kill the next tray on that OS thread.
	defer runtime.UnlockOSThread()
	defer close(t.done)
	defer drainThreadQueue()

	tid, _, _ := procGetCurrentTID.Call()
	t.tid.Store(uint32(tid))

	hwnd, className, err := createTrayWindow(uint32(tid))
	if err != nil {
		t.hwndErr = err
		t.hwndOne.Do(func() { close(t.hwndCh) })
		return
	}
	t.hwnd.Store(uint64(hwnd))
	registerTray(hwnd, t)

	if err := t.addIcon(hwnd); err != nil {
		t.hwndErr = err
		t.hwndOne.Do(func() { close(t.hwndCh) })
		destroyTrayWindow(hwnd, className)
		return
	}
	t.hwndOne.Do(func() { close(t.hwndCh) })

	var msg msgStruct
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 { // 0 = WM_QUIT, -1 = error
			break
		}
		switch msg.Message {
		case wmAppCmd:
			if t.drainCommands(hwnd) {
				t.removeIcon(hwnd)
				destroyTrayWindow(hwnd, className)
				return
			}
		default:
			procTranslateMsg.Call(uintptr(unsafe.Pointer(&msg)))
			procDispatchMsg.Call(uintptr(unsafe.Pointer(&msg)))
		}
	}
	t.removeIcon(hwnd)
	destroyTrayWindow(hwnd, className)
}

// destroyTrayWindow tears the window and its class down.
func destroyTrayWindow(hwnd windows.Handle, className string) {
	procDestroyWindow.Call(uintptr(hwnd))
	procUnregisterClass.Call(uintptr(unsafe.Pointer(utf16Ptr(className))), 0)
	unregisterTray(hwnd)
}

// drainCommands runs every pending request on the message thread. It reports
// true when the tray is closing.
func (t *Tray) drainCommands(hwnd windows.Handle) bool {
	for {
		select {
		case cmd := <-t.cmdCh:
			switch cmd.kind {
			case "close":
				replyErr(cmd, nil)
				return true
			case "notify":
				replyErr(cmd, t.showBalloon(hwnd, cmd.title, cmd.text))
			case "menu":
				t.mu.Lock()
				t.menu = append([]Item(nil), cmd.items...)
				t.mu.Unlock()
				replyErr(cmd, nil)
			case "tooltip":
				replyErr(cmd, t.modifyTip(hwnd, cmd.text))
			case "icon":
				replyErr(cmd, t.modifyIcon(hwnd, cmd.ico))
			default:
				replyErr(cmd, fmt.Errorf("未知托盘命令 %q", cmd.kind))
			}
		default:
			return false
		}
	}
}

func replyErr(cmd command, err error) {
	if cmd.reply != nil {
		cmd.reply <- err
	}
}

// postAppCmd wakes GetMessageW from any goroutine.
func postAppCmd(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	procPostMessage.Call(hwnd, wmAppCmd, 0, 0)
}

// createTrayWindow registers the class and creates the (never shown) window.
func createTrayWindow(tid uint32) (windows.Handle, string, error) {
	if wndProcRef == nil {
		wndProcRef = wndProc
	}
	classNameStr := fmt.Sprintf("SoundRadarTray_%d", tid)
	className := utf16Ptr(classNameStr)
	title := utf16Ptr("SoundRadar")

	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   windows.NewCallback(wndProcRef),
		LpszClassName: className,
	}
	if atom, _, callErr := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return 0, classNameStr, fmt.Errorf("RegisterClassExW(%s) 失败: %v", classNameStr, callErr)
	}
	// A real top-level window with no WS_VISIBLE: it never appears, but it does
	// receive the shell's broadcasts (TaskbarCreated). A message-only window
	// would not, and the icon would be lost after an Explorer restart.
	hwnd, _, callErr := procCreateWindowEx.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		0, 0, 0, 0, 0,
		0, 0, 0, 0,
	)
	if hwnd == 0 {
		procUnregisterClass.Call(uintptr(unsafe.Pointer(className)), 0)
		return 0, classNameStr, fmt.Errorf("CreateWindowExW 失败: %v", callErr)
	}
	return windows.Handle(hwnd), classNameStr, nil
}

// wndProc runs on the message thread.
func wndProc(hwnd windows.Handle, msg uint32, wparam, lparam uintptr) uintptr {
	switch {
	case msg == wmTray:
		if t := lookupTray(hwnd); t != nil {
			t.onTrayMessage(lparam, wparam)
		}
	case msg == wmCommand:
		if t := lookupTray(hwnd); t != nil {
			t.dispatch(Action(uint32(wparam) & 0xffff))
		}
	case msg == taskbarCreatedMsg() && msg != 0:
		// Explorer restarted (a crash, "restart explorer", a Windows update):
		// the shell forgot every icon, so ours has to be added again or it
		// stays missing until the program is restarted.
		if t := lookupTray(hwnd); t != nil {
			_ = t.addIcon(hwnd)
		}
	}
	ret, _, _ := procDefWindowProc.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
	return ret
}

// onTrayMessage handles the icon's mouse events. Which of lParam/wParam carries
// the mouse message differs between the legacy and modern shell callbacks, so
// both are considered.
func (t *Tray) onTrayMessage(lparam, wparam uintptr) {
	event := uint32(lparam)
	if event < 0x0200 || event > 0x0300 {
		event = uint32(wparam)
	}
	switch event {
	case wmLButtonUp, wmLButtonDbl:
		if _, activate := t.callbacks(); activate != nil {
			go activate()
		}
	case wmRButtonUp, wmContext:
		t.showMenu()
	}
}

// showMenu tracks the context menu at the cursor.
func (t *Tray) showMenu() {
	hwnd := windows.Handle(t.hwnd.Load())
	if hwnd == 0 {
		return
	}
	hmenu, _, _ := procCreatePopupMenu.Call()
	if hmenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hmenu)

	for _, it := range t.currentMenu() {
		if it.Separator {
			procAppendMenu.Call(hmenu, mfSeparator, 0, 0)
			continue
		}
		flags := uintptr(mfString)
		if it.Checked {
			flags |= mfChecked
		}
		if it.Disabled {
			flags |= mfGrayed
		}
		procAppendMenu.Call(hmenu, flags, uintptr(it.ID), uintptr(unsafe.Pointer(utf16Ptr(it.Label))))
	}
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// SetForegroundWindow before and a posted WM_NULL after are the documented
	// dance that makes the menu close when the user clicks elsewhere.
	procSetForeground.Call(uintptr(hwnd))
	cmd, _, _ := procTrackPopupMenu.Call(hmenu,
		tpmRightButton|tpmReturnCmd, uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
	procPostMessage.Call(uintptr(hwnd), wmNull, 0, 0)
	if cmd != 0 {
		t.dispatch(Action(cmd))
	}
}

// dispatch runs a menu selection on its own goroutine so a slow action (opening
// a browser, shutting the server down) cannot block the message loop.
func (t *Tray) dispatch(a Action) {
	action, _ := t.callbacks()
	if action == nil {
		return
	}
	go action(a)
}

// ---------------------------------------------------------------------------
// notification-area registration
// ---------------------------------------------------------------------------

func (t *Tray) data(hwnd windows.Handle) notifyIconDataW {
	var d notifyIconDataW
	d.CbSize = uint32(unsafe.Sizeof(d))
	d.HWnd = hwnd
	d.UID = 1
	d.UCallbackMessage = trayCallbackMsg
	d.HIcon = windows.Handle(t.icon.Load())
	copyStringToUTF16(d.SzTip[:], t.tooltip())
	return d
}

// tooltip reads the current tooltip under the lock.
func (t *Tray) tooltip() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.opts.Tooltip
}

// copyStringToUTF16 fills a fixed-size UTF-16 buffer, truncating safely (a
// too-long tooltip or balloon must not corrupt the struct).
func copyStringToUTF16(dst []uint16, s string) {
	if len(dst) == 0 {
		return
	}
	u := windows.StringToUTF16(s)
	n := copy(dst, u)
	if n < len(dst) {
		dst[n] = 0
		return
	}
	dst[len(dst)-1] = 0
}

// addIcon registers the icon (NIM_ADD).
func (t *Tray) addIcon(hwnd windows.Handle) error {
	t.ensureIcon()
	// Ask for the modern struct tail first; an older shell simply fails this and
	// the NIM_ADD below still works with the legacy layout.
	ver := t.data(hwnd)
	ver.UFlags = nifMessage | nifIcon | nifTip
	ver.UVersion = 4
	procShellNotifyIcon.Call(nimSetVersion, uintptr(unsafe.Pointer(&ver)))

	d := t.data(hwnd)
	d.UFlags = nifMessage | nifIcon | nifTip
	if ret, _, callErr := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&d))); ret == 0 {
		return fmt.Errorf("Shell_NotifyIconW(NIM_ADD) 失败: %v", callErr)
	}
	return nil
}

// modifyTip updates the tooltip (NIM_MODIFY with NIF_TIP).
func (t *Tray) modifyTip(hwnd windows.Handle, tip string) error {
	t.mu.Lock()
	t.opts.Tooltip = tip
	t.mu.Unlock()
	d := t.data(hwnd)
	d.UFlags = nifTip
	if ret, _, callErr := procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&d))); ret == 0 {
		return fmt.Errorf("Shell_NotifyIconW(NIM_MODIFY tip) 失败: %v", callErr)
	}
	return nil
}

// modifyIcon rebuilds the icon from an .ico file and updates the shell.
func (t *Tray) modifyIcon(hwnd windows.Handle, ico []byte) error {
	t.mu.Lock()
	t.opts.ICO = ico
	t.mu.Unlock()
	t.disposeIcon()
	t.ensureIcon()
	d := t.data(hwnd)
	d.UFlags = nifIcon
	if ret, _, callErr := procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&d))); ret == 0 {
		return fmt.Errorf("Shell_NotifyIconW(NIM_MODIFY icon) 失败: %v", callErr)
	}
	return nil
}

// removeIcon unregisters the icon (NIM_DELETE). Safe to call when it was never
// added.
func (t *Tray) removeIcon(hwnd windows.Handle) {
	d := t.data(hwnd)
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&d)))
	t.disposeIcon()
}

// ensureIcon makes sure t.icon holds a usable HICON: our own embedded .ico when
// there is one, otherwise a stock system icon, so the tray is never empty.
//
// It deliberately does NOT go through ExtractIconExW on our own executable (the
// obvious "show the exe's icon" approach): that call was measured to fault
// inside shell32 on a Go-built exe (0xc0000005) even with correct pointer
// handling, and it is also pointless - the embedded .ico is the same artwork.
func (t *Tray) ensureIcon() {
	if t.icon.Load() != 0 {
		return
	}
	t.mu.Lock()
	ico := t.opts.ICO
	t.mu.Unlock()
	if len(ico) > 0 {
		if h, err := iconFromICO(ico, iconSize()); err == nil && validIcon(windows.Handle(h)) {
			t.icon.Store(uint64(h))
			return
		}
	}
	h, _, _ := procLoadIcon.Call(0, uintptr(idiApplication))
	t.icon.Store(uint64(h))
}

// iconSize asks the shell how big a notification-area icon should be.
func iconSize() int {
	cx, _, _ := procGetSystemMetrics.Call(smCXSmallIcon)
	if cx == 0 {
		return 16
	}
	return int(cx)
}

// iconFromICO creates an HICON straight out of an in-memory .ico file, without
// touching the disk. It follows the format parser in tray.go: a 6-byte header,
// then one 16-byte directory entry per image, then the image data (a
// BITMAPINFOHEADER with the XOR and AND masks concatenated, or a PNG, which is
// exactly the "icon resource" format CreateIconFromResourceEx expects).
func iconFromICO(ico []byte, want int) (uintptr, error) {
	images, err := parseICO(ico)
	if err != nil {
		return 0, err
	}
	img, ok := pickICOImage(images, want)
	if !ok {
		return 0, errors.New("无效的 .ico：没有可用的图像")
	}
	h, _, callErr := procCreateIconFromRes.Call(
		uintptr(unsafe.Pointer(&img.Data[0])), uintptr(len(img.Data)),
		1 /* fIcon */, uintptr(want), uintptr(want), lrDefaultSize)
	if h == 0 {
		return 0, fmt.Errorf("CreateIconFromResourceEx 失败: %v", callErr)
	}
	return h, nil
}

// iconFromExe takes the icon of our own executable, so a caller can show the
// same picture Explorer shows for the file. It is not used by the tray (see
// ensureIcon) but is kept for tests and for future callers that want it.
//
// The two out-parameters are declared as windows.Handle (pointer-sized) and
// converted through unsafe.Pointer, which is what the P/Invoke rules require:
// passing a *uintptr directly is undefined behaviour and shows up as an access
// violation.
func iconFromExe() uintptr {
	path := exePath()
	if path == "" {
		return 0
	}
	exe := utf16Ptr(path)
	var large, small windows.Handle
	n, _, _ := procExtractIcon.Call(0, uintptr(unsafe.Pointer(exe)),
		^uintptr(0) /* -1 = all icons */,
		uintptr(unsafe.Pointer(&large)), uintptr(unsafe.Pointer(&small)), 1)
	// ExtractIconExW returns UINT_MAX (-1) for "no icons".
	if n == 0 || n == ^uintptr(0) {
		return 0
	}
	if validIcon(large) {
		if validIcon(small) {
			procDestroyIcon.Call(uintptr(small))
		}
		return uintptr(large)
	}
	if validIcon(small) {
		return uintptr(small)
	}
	return 0
}

// validIcon reports whether an icon handle is usable (Win32 explicitly reserves
// the values 0 and 1).
func validIcon(h windows.Handle) bool { return h != 0 && h != 1 }

// showBalloon posts a balloon notification. Modern Windows renders it as a
// toast; either way it is the non-intrusive way to say "I am running".
func (t *Tray) showBalloon(hwnd windows.Handle, title, text string) error {
	d := t.data(hwnd)
	d.UFlags = nifInfo
	d.DwInfoFlags = niifInfo
	copyStringToUTF16(d.SzInfoTitle[:], title)
	copyStringToUTF16(d.SzInfo[:], text)
	if ret, _, callErr := procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&d))); ret == 0 {
		return fmt.Errorf("Shell_NotifyIconW(NIM_MODIFY balloon) 失败: %v", callErr)
	}
	return nil
}

// drainThreadQueue empties the thread's queue, including WM_QUIT, before the OS
// thread goes back to the pool.
func drainThreadQueue() {
	var msg msgStruct
	for {
		ret, _, _ := procPeekMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 1) // PM_REMOVE
		if ret == 0 {
			return
		}
	}
}

// TaskbarCreated is broadcast by the shell when the taskbar is (re)created; the
// message id is registered on first use, which is why it is looked up lazily.
var (
	taskbarOnce  sync.Once
	taskbarValue uint32
)

func taskbarCreatedMsg() uint32 {
	taskbarOnce.Do(func() {
		name := utf16Ptr(taskbarCreatedName)
		v, _, _ := procRegisterWindowMsg.Call(uintptr(unsafe.Pointer(name)))
		taskbarValue = uint32(v)
	})
	return taskbarValue
}

// OpenURL opens a URL or a directory with the shell, without going through
// cmd.exe (so a path with spaces or an & cannot change the command).
func OpenURL(target string) error {
	if target == "" {
		return errors.New("空路径")
	}
	verb := utf16Ptr("open")
	file := utf16Ptr(target)
	ret, _, callErr := procShellExecute.Call(0,
		uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), 0, 0, swShowNormal)
	if ret <= 32 {
		return fmt.Errorf("ShellExecuteW(%s) 失败: %v", target, callErr)
	}
	return nil
}

// utf16Ptr converts s for the Win32 API. It panics on an embedded NUL, which
// cannot come from a label but must not reach the API either.
func utf16Ptr(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return p
}

// exePath returns the running executable's path (empty when it cannot be read).
func exePath() string {
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetModuleFileName(0, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}
