//go:build windows

package hotkey

import (
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"github.com/znz/soundradar/internal/config"
	"golang.org/x/sys/windows"
)

// ---------------------------------------------------------------------------
// Win32 plumbing (stdlib + golang.org/x/sys/windows only, no cgo)
// ---------------------------------------------------------------------------

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	kernel32            = windows.NewLazySystemDLL("kernel32.dll")
	procRegisterClassEx = user32.NewProc("RegisterClassExW")
	procUnregisterClass = user32.NewProc("UnregisterClassW")
	procCreateWindowEx  = user32.NewProc("CreateWindowExW")
	procDestroyWindow   = user32.NewProc("DestroyWindow")
	procDefWindowProc   = user32.NewProc("DefWindowProcW")
	procGetMessage      = user32.NewProc("GetMessageW")
	procTranslateMsg    = user32.NewProc("TranslateMessage")
	procDispatchMsg     = user32.NewProc("DispatchMessageW")
	procPostMessage     = user32.NewProc("PostMessageW")
	procPeekMessage     = user32.NewProc("PeekMessageW")
	procRegisterHotKey  = user32.NewProc("RegisterHotKey")
	procUnregisterHK    = user32.NewProc("UnregisterHotKey")
	procGetCurrentTID   = kernel32.NewProc("GetCurrentThreadId")
)

// Window messages.
const (
	wmDestroy = 0x0002
	wmClose   = 0x0010
	wmHotkey  = 0x0312
	// wmAppCmd is posted by other goroutines to wake GetMessageW. It carries no
	// payload; the actual Register/Unregister/Close request sits on cmdCh.
	wmAppCmd = 0x8000 + 1
)

// hwndMessage is HWND_MESSAGE ((HWND)-3): a message-only window. It is never
// shown, never appears in Alt-Tab and exists purely to own the hotkey
// registrations and their message queue.
var hwndMessage = ^uintptr(2) // (uintptr)-3

// RegisterHotKey error codes worth naming in Chinese.
const (
	errHotkeyAlreadyRegistered = 1409
	errHotkeyInvalidWindow     = 1400
	errHotkeyInvalidKey        = 1401
)

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

// wndProcRef pins the callback so the GC cannot collect it while the class is
// registered (windows.NewCallback returns a bare function pointer).
var wndProcRef func(hwnd windows.Handle, msg uint32, wparam, lparam uintptr) uintptr

// ---------------------------------------------------------------------------
// window thread
// ---------------------------------------------------------------------------

// NewManager creates the hidden message-only window and starts its message
// loop. It returns only once the window exists, so Register can be called
// immediately and reports the real RegisterHotKey result.
func NewManager() (*Manager, error) {
	m := &Manager{
		cmdCh: make(chan command, 8),
		done:  make(chan struct{}),
		regs:  make(map[string]string),
		fns:   make(map[string]func()),
		errs:  make(map[string]string),
		defs:  make(map[string]string),
		ids:   make(map[string]int),
	}
	ready := make(chan error, 1)
	go m.windowThread(ready)
	select {
	case err := <-ready:
		if err != nil {
			return nil, err
		}
		return m, nil
	case <-time.After(10 * time.Second):
		return nil, errors.New("创建热键窗口超时（10 秒内消息线程没有就绪）")
	}
}

// windowThread owns the hidden window and the message loop. It runs locked to
// one OS thread for its entire life. CreateWindowExW, RegisterHotKey and
// GetMessageW must share that thread: WM_HOTKEY is posted to the registering
// thread's queue, and a pump on any other thread never sees the key.
func (m *Manager) windowThread(ready chan<- error) {
	runtime.LockOSThread()
	// Unlock happens last. Drain before the thread returns to the pool so a
	// WM_QUIT from DestroyWindow cannot make the next manager's GetMessageW
	// return immediately.
	defer runtime.UnlockOSThread()
	defer close(m.done)
	defer drainThreadQueue()

	tid, _, _ := procGetCurrentTID.Call()
	m.mu.Lock()
	m.tid = uint32(tid)
	m.mu.Unlock()

	hwnd, err := createMessageWindow()
	if err != nil {
		select {
		case ready <- err:
		default:
		}
		return
	}
	m.mu.Lock()
	m.hwnd = uintptr(hwnd)
	m.mu.Unlock()

	className := fmt.Sprintf("SoundRadarHotkey_%d", tid)
	defer func() {
		procDestroyWindow.Call(uintptr(hwnd))
		procUnregisterClass.Call(uintptr(unsafe.Pointer(utf16Ptr(className))), 0)
		m.mu.Lock()
		m.hwnd = 0
		m.mu.Unlock()
	}()

	select {
	case ready <- nil:
	default:
	}

	var msg msgStruct
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 { // 0 = WM_QUIT, -1 = error
			return
		}
		switch msg.Message {
		case wmAppCmd:
			if m.drainCommands(hwnd) {
				return
			}
		case wmHotkey:
			// Handle it here and do not also DispatchMessageW: wndProc would
			// fire the callback a second time.
			m.handleHotkey(int(msg.WParam))
		default:
			procTranslateMsg.Call(uintptr(unsafe.Pointer(&msg)))
			procDispatchMsg.Call(uintptr(unsafe.Pointer(&msg)))
		}
	}
}

// drainCommands runs every pending Register/Unregister/Close on this thread.
// It returns true when the manager is closing.
func (m *Manager) drainCommands(hwnd windows.Handle) bool {
	for {
		select {
		case cmd := <-m.cmdCh:
			switch cmd.kind {
			case "register":
				id, rerr := m.doRegister(hwnd, cmd.name, cmd.spec)
				replyTo(cmd, reply{err: rerr, id: id})
			case "unregister":
				m.doUnregister(hwnd, cmd.name)
				replyTo(cmd, reply{})
			case "quit":
				m.doUnregisterAll(hwnd)
				return true
			default:
				replyTo(cmd, reply{err: fmt.Errorf("未知热键命令 %q", cmd.kind)})
			}
		default:
			return false
		}
	}
}

func replyTo(cmd command, r reply) {
	if cmd.rep == nil {
		return
	}
	cmd.rep <- r
}

// wake interrupts GetMessageW so a command queued on cmdCh is processed.
// PostMessageW is safe to call from any thread; the message lands on the
// queue of the thread that created hwnd.
func (m *Manager) wake() {
	if m == nil {
		return
	}
	m.mu.Lock()
	hwnd := m.hwnd
	m.mu.Unlock()
	if hwnd == 0 {
		return
	}
	procPostMessage.Call(hwnd, wmAppCmd, 0, 0)
}

// createMessageWindow registers the class and creates the message-only window.
func createMessageWindow() (windows.Handle, error) {
	if wndProcRef == nil {
		wndProcRef = wndProc
	}
	tid, _, _ := procGetCurrentTID.Call()
	classNameStr := fmt.Sprintf("SoundRadarHotkey_%d", tid)
	className := utf16Ptr(classNameStr)
	title := utf16Ptr("SoundRadar Hotkey")

	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   windows.NewCallback(wndProcRef),
		LpszClassName: className,
	}
	if atom, _, callErr := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return 0, fmt.Errorf("RegisterClassExW(%s) 失败: %v", classNameStr, callErr)
	}
	hwnd, _, callErr := procCreateWindowEx.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		0, 0, 0, 0, 0,
		hwndMessage, 0, 0, 0,
	)
	if hwnd == 0 {
		procUnregisterClass.Call(uintptr(unsafe.Pointer(className)), 0)
		return 0, fmt.Errorf("CreateWindowExW(HWND_MESSAGE) 失败: %v", callErr)
	}
	return windows.Handle(hwnd), nil
}

// wndProc runs on the message loop thread. WM_HOTKEY is handled in the
// GetMessage loop, not here, so a single press cannot fire the callback twice.
// Do not PostQuitMessage: this OS thread is returned to the Go pool, and a
// leftover WM_QUIT kills the next manager.
func wndProc(hwnd windows.Handle, msg uint32, wparam, lparam uintptr) uintptr {
	ret, _, _ := procDefWindowProc.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
	return ret
}

// drainThreadQueue removes every pending message, including WM_QUIT, before
// the OS thread goes back to the pool.
func drainThreadQueue() {
	var msg msgStruct
	for {
		ret, _, _ := procPeekMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 1) // PM_REMOVE
		if ret == 0 {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// hotkey registration (message thread only)
// ---------------------------------------------------------------------------

// doRegister calls RegisterHotKey for a name and returns its Win32 id.
func (m *Manager) doRegister(hwnd windows.Handle, name, spec string) (int, error) {
	hk, err := config.ParseHotkey(spec)
	if err != nil {
		return 0, err
	}
	if hk.VK == 0 {
		return 0, nil
	}
	// Re-registering the same name on the same thread fails with
	// ERROR_HOTKEY_ALREADY_REGISTERED, so release the old id first.
	m.mu.Lock()
	if old, ok := m.ids[name]; ok {
		procUnregisterHK.Call(uintptr(hwnd), uintptr(old))
		delete(m.ids, name)
	}
	m.nextID++
	id := m.nextID
	m.ids[name] = id
	m.mu.Unlock()

	ret, _, callErr := procRegisterHotKey.Call(uintptr(hwnd), uintptr(id),
		uintptr(hk.Modifiers), uintptr(hk.VK))
	if ret == 0 {
		m.mu.Lock()
		delete(m.ids, name)
		m.mu.Unlock()
		return 0, registerError(hk, callErr)
	}
	return id, nil
}

// registerError renders the RegisterHotKey failure in Chinese, naming the key
// and the most likely cause.
func registerError(hk config.Hotkey, callErr error) error {
	code := uintptr(0)
	if errno, ok := callErr.(windows.Errno); ok {
		code = uintptr(errno)
	}
	switch int(code) {
	case errHotkeyAlreadyRegistered:
		return fmt.Errorf("%s 已被其它程序占用（RegisterHotKey 返回 ERROR_HOTKEY_ALREADY_REGISTERED=1409）", hk.String())
	case errHotkeyInvalidWindow:
		return fmt.Errorf("%s 注册失败：热键窗口句柄无效（错误 1400）", hk.String())
	case errHotkeyInvalidKey:
		return fmt.Errorf("%s 注册失败：按键不被系统接受（错误 1401）", hk.String())
	case 0:
		return fmt.Errorf("%s 注册失败：%v（可能已被其它程序占用）", hk.String(), callErr)
	default:
		return fmt.Errorf("%s 注册失败：%v（Win32 错误 %d，可能已被其它程序占用）", hk.String(), callErr, code)
	}
}

// doUnregister releases one name's hotkey.
func (m *Manager) doUnregister(hwnd windows.Handle, name string) {
	m.mu.Lock()
	id, ok := m.ids[name]
	delete(m.ids, name)
	m.mu.Unlock()
	if ok {
		procUnregisterHK.Call(uintptr(hwnd), uintptr(id))
	}
}

// doUnregisterAll releases every hotkey before the window goes away.
func (m *Manager) doUnregisterAll(hwnd windows.Handle) {
	m.mu.Lock()
	ids := make([]int, 0, len(m.ids))
	for _, id := range m.ids {
		ids = append(ids, id)
	}
	m.ids = make(map[string]int)
	m.mu.Unlock()
	for _, id := range ids {
		procUnregisterHK.Call(uintptr(hwnd), uintptr(id))
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func utf16Ptr(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return p
}
