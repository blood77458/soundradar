// Package hotkey implements global (system-wide) hotkeys for soundradar.
//
// It exists for the P4 recall feature: the player presses one key while playing
// and the last few seconds of audio are saved. Global hotkeys are official
// Win32 RegisterHotKey registrations - this package NEVER synthesises or hooks
// keyboard input (SendInput / keybd_event / SetWindowsHookEx are a ban risk
// while a game with anti-cheat is running).
//
// # Thread model
//
// RegisterHotKey binds a hotkey to the calling thread, and WM_HOTKEY is posted
// to that thread's message queue. The manager therefore owns exactly one OS
// thread for its whole life, and that same thread creates the window, calls
// RegisterHotKey, and pumps GetMessageW. A second LockOSThread goroutine would
// be a different OS thread: the key would register, then the press would be
// delivered to a queue nobody reads.
//
//	goroutine A (caller)                goroutine B (window thread, LockOSThread)
//	--------------------                ------------------------------------------
//	NewManager()  ───────────────────▶  RegisterClassExW + CreateWindowExW
//	Register(cmd) ──cmdCh─────────────▶  GetMessageW (woken by PostMessageW)
//	             ◀──reply chan─────────  RegisterHotKey(...)
//	(key press)                         WM_HOTKEY → go callback()
//	Close()       ──cmdCh(quit)────────▶ UnregisterHotKey, DestroyWindow, exit
//
// GetMessageW blocks, so commands wake it with PostMessageW instead of a
// select. The WM_HOTKEY handler must not block: Manager calls every callback
// as `go fn()`.
package hotkey

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znz/soundradar/internal/config"
)

// Errors returned by Register / RegisterAll.
var (
	// ErrClosed is returned once the manager has been closed.
	ErrClosed = errors.New("热键管理器已关闭")
	// ErrEmptyName is returned for Register("", ...).
	ErrEmptyName = errors.New("热键名称不能为空")
	// ErrDuplicate is returned when the same name is registered twice.
	ErrDuplicate = errors.New("该名称的热键已注册")
)

// specNone is the config spelling that means "do not register anything".
const specNone = "none"

// bind is one named registration.
type bind struct {
	name string
	spec string
	fn   func()
}

// command is one request processed on the window thread, so that every Win32
// call happens there (RegisterHotKey is thread-affine).
type command struct {
	kind    string // "register" | "unregister" | "quit"
	name    string
	spec    string
	rep     chan reply
	timeout time.Duration
}

type reply struct {
	err  error
	note string
	id   int
}

// Manager owns one hidden message-only window and its message loop.
type Manager struct {
	cmdCh chan command
	done  chan struct{}

	mu     sync.Mutex
	closed bool
	regs   map[string]string // name -> canonical spec (successful registrations)
	fns    map[string]func()
	errs   map[string]string // name -> Chinese failure reason
	defs   map[string]string // name -> asked-for spelling (even after a failure)
	ids    map[string]int    // name -> Win32 hotkey id (message thread only)
	nextID int               // message thread only
	hwnd   uintptr
	tid    uint32
	lastEr string
}

// NewManager creates the hidden window and starts its message loop. It returns
// only after the window exists, so Register can be called immediately.
//
// The implementation lives in hotkey_windows.go (Win32) and hotkey_other.go
// (a stub that reports the platform limitation), because the whole point of the
// Manager is a Win32 message queue.

// HWND returns the message-only window handle (0 before it exists / after Close).
func (m *Manager) HWND() uintptr {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hwnd
}

// ThreadID returns the OS thread that owns the message queue. It is evidence
// that the LockOSThread model is really in place.
func (m *Manager) ThreadID() uint32 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tid
}

// LastError returns the last failure (also kept per name in Registered).
func (m *Manager) LastError() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastEr
}

// Registered reports every name this manager knows about: the value is the
// canonical hotkey spelling on success, or a Chinese failure reason.
func (m *Manager) Registered() map[string]string {
	out := map[string]string{}
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.regs {
		out[k] = v
	}
	return out
}

// RegisteredSpec returns just the successfully registered name -> spelling map.
func (m *Manager) RegisteredSpec() map[string]string {
	out := map[string]string{}
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.regs {
		if _, bad := m.errs[k]; !bad {
			out[k] = v
		}
	}
	return out
}

// Spec returns the configured spelling for a name ("" when unknown).
func (m *Manager) Spec(name string) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.defs[name]
}

// OK reports whether a name registered successfully.
func (m *Manager) OK(name string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, bad := m.errs[name]; bad {
		return false
	}
	_, ok := m.regs[name]
	return ok
}

// Error returns the failure reason of a name ("" when it registered fine).
func (m *Manager) Error(name string) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.errs[name]
}

// Describe renders a stable, sorted one-line summary for banners and JSON:
//
//	recall=[ok] F8
//	toggle=[失败] F9 已被其它程序占用
func (m *Manager) Describe() string {
	if m == nil {
		return "（无热键管理器）"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.regs) == 0 && len(m.errs) == 0 {
		return "（未注册任何热键）"
	}
	names := make([]string, 0, len(m.regs)+len(m.errs))
	for k := range m.regs {
		names = append(names, k)
	}
	for k := range m.errs {
		if _, dup := m.regs[k]; !dup {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		if reason, bad := m.errs[n]; bad {
			parts = append(parts, fmt.Sprintf("%s=[失败] %s", n, reason))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=[ok] %s", n, m.regs[n]))
	}
	return strings.Join(parts, "  ")
}

// Register binds spec to fn under name. spec is anything config.ParseHotkey
// accepts ("F8", "Ctrl+Alt+F8"); the value "none" (or an empty string) removes
// the registration without reporting an error.
//
// The call is synchronous: it returns once the window thread has the real
// RegisterHotKey result, so a caller can print "F8 已被其它程序占用" right away.
func (m *Manager) Register(name, spec string, fn func()) error {
	if m == nil {
		return errors.New("热键管理器未初始化")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrEmptyName
	}
	if fn == nil {
		return errors.New("热键回调不能为空")
	}
	canonical, none, err := normalizeSpec(spec)
	if err != nil {
		m.setFailure(name, spec, err.Error())
		return err
	}
	if none {
		if spec2, ok := m.registration(name); ok && spec2 != "" {
			if err := m.Unregister(name); err != nil {
				return err
			}
		}
		m.mu.Lock()
		m.defs[name] = specNone
		m.mu.Unlock()
		return nil
	}
	m.mu.Lock()
	m.fns[name] = fn
	m.defs[name] = canonical
	m.mu.Unlock()

	rep, err := m.send(command{kind: "register", name: name, spec: canonical, timeout: 3 * time.Second})
	if err != nil {
		m.setFailure(name, canonical, err.Error())
		return err
	}
	if rep.err != nil {
		m.setFailure(name, canonical, rep.err.Error())
		return rep.err
	}
	m.mu.Lock()
	m.regs[name] = canonical
	m.ids[name] = rep.id
	delete(m.errs, name)
	m.lastEr = ""
	m.mu.Unlock()
	return nil
}

// Unregister removes a name. An unknown name is not an error (it is already
// gone), which keeps Close and the settings page simple.
func (m *Manager) Unregister(name string) error {
	if m == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if _, ok := m.registration(name); !ok {
		return nil
	}
	if _, err := m.send(command{kind: "unregister", name: name, timeout: 3 * time.Second}); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.regs, name)
	delete(m.fns, name)
	delete(m.ids, name)
	m.mu.Unlock()
	return nil
}

// Trigger invokes a registered callback on its own goroutine.
//
// It exists so the hotkey path can be exercised WITHOUT synthesising keyboard
// input: the acceptance run calls Trigger("recall") (or POST
// /api/recall/trigger, which ends up here) and thereby proves that
// "RegisterHotKey succeeded -> the callback -> saving a candidate" is wired up,
// while the actual key press is never simulated.
func (m *Manager) Trigger(name string) error {
	if m == nil {
		return errors.New("热键管理器未初始化")
	}
	m.mu.Lock()
	fn, ok := m.fns[strings.TrimSpace(name)]
	reason := m.errs[strings.TrimSpace(name)]
	m.mu.Unlock()
	if !ok {
		if reason != "" {
			return fmt.Errorf("热键 %q 没有注册成功：%s", name, reason)
		}
		return fmt.Errorf("热键 %q 未注册", name)
	}
	go fn()
	return nil
}

// Close stops the message loop and releases the window. It is idempotent and
// safe to call from any goroutine.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()

	select {
	case m.cmdCh <- command{kind: "quit"}:
		m.wake()
	case <-m.done:
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("热键管理器关闭超时（2 秒）")
	}
	select {
	case <-m.done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("热键消息循环退出超时（5 秒）")
	}
}

// send posts one command to the window thread and waits for the reply.
func (m *Manager) send(c command) (reply, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return reply{}, ErrClosed
	}
	m.mu.Unlock()

	c.rep = make(chan reply, 1)
	timeout := c.timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	select {
	case m.cmdCh <- c:
		m.wake()
	case <-m.done:
		return reply{}, ErrClosed
	case <-time.After(timeout):
		return reply{}, fmt.Errorf("热键消息线程没有响应（%s 超时）", timeout)
	}
	select {
	case r := <-c.rep:
		return r, nil
	case <-m.done:
		return reply{}, ErrClosed
	case <-time.After(timeout):
		return reply{}, fmt.Errorf("热键消息线程没有响应（%s 超时）", timeout)
	}
}

// registration returns the current state of a name.
func (m *Manager) registration(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.regs[name]; ok {
		return v, true
	}
	if _, ok := m.defs[name]; ok {
		return "", true
	}
	return "", false
}

func (m *Manager) setFailure(name, spec, reason string) {
	m.mu.Lock()
	m.errs[name] = reason
	m.regs[name] = reason
	m.defs[name] = spec
	m.lastEr = reason
	m.mu.Unlock()
}

// handleHotkey runs on the message thread: it must not block, so the callback
// gets its own goroutine.
func (m *Manager) handleHotkey(id int) {
	m.mu.Lock()
	name, fn := "", (func())(nil)
	for n, hkID := range m.ids {
		if hkID == id {
			name = n
			fn = m.fns[n]
			break
		}
	}
	m.mu.Unlock()
	if name == "" || fn == nil {
		return
	}
	go fn()
}

// normalizeSpec parses a hotkey spelling. It reports none=true for "none"/"".
// Parsing is delegated to internal/config so the CLI, the settings page and this
// package can never disagree about what "Ctrl+Alt+F8" means.
func normalizeSpec(spec string) (canonical string, none bool, err error) {
	raw := strings.TrimSpace(spec)
	if raw == "" || strings.EqualFold(raw, specNone) {
		return specNone, true, nil
	}
	hk, err := config.ParseHotkey(raw)
	if err != nil {
		return "", false, err
	}
	if hk.VK == 0 {
		return specNone, true, nil
	}
	return hk.String(), false, nil
}
