// Package tray puts a soundradar icon in the Windows notification area (the
// "system tray") with a right-click menu, so a double-clicked soundradar.exe has
// a face and a way out instead of being a background process the user cannot
// find or stop.
//
// # Shape
//
// Everything lives on ONE OS thread, exactly like internal/overlay and
// internal/hotkey: a window's message queue belongs to a thread, so the window
// is created, the Shell_NotifyIconW registration happens and the GetMessageW
// loop runs on the same locked thread. Every method is therefore safe to call
// from other goroutines and simply posts a message.
//
// The window is a real (but never shown) top-level window rather than a
// message-only one: the shell sends its callbacks - and, importantly,
// "TaskbarCreated" after an Explorer restart - as window messages, and a
// message-only window does not receive broadcasts.
//
// # No CGO, no third-party dependency
//
// Only golang.org/x/sys/windows, matching the rest of this program (the build
// runs with CGO_ENABLED=0).
package tray

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Menu identifiers. They are returned by Action and are small integers so they
// can travel as a WM_COMMAND wParam.
const (
	ActionOpenPanel = 1 + iota
	ActionToggleOverlay
	ActionCandidates
	ActionLiveStatus
	ActionOpenDataDir
	ActionQuit
)

// Action names the default menu's entries, in order. A caller that supplies its
// own Menu may use any identifiers it likes.
type Action int

// Item is one context-menu entry. Separator marks a divider and ignores the
// other fields.
type Item struct {
	ID        Action
	Label     string
	Separator bool
	// Checked draws a check mark; it is driven by the check callback.
	Checked bool
	// Disabled greys the entry out.
	Disabled bool
}

// Options configures the tray icon.
type Options struct {
	// Tooltip is the hover text. Keep it short: Windows truncates at 127
	// characters.
	Tooltip string
	// ICO is a whole .ico file (as embedded in the binary) used for the icon.
	// When it is empty the application icon, and finally a stock system icon, is
	// used instead.
	ICO []byte
	// Menu is the context menu. nil means DefaultMenu().
	Menu []Item
	// OnAction is called for every menu selection, on a background goroutine
	// (never on the message thread, so it may block).
	OnAction func(Action)
	// OnActivate is called when the user double-clicks or single-clicks the icon
	// with the left button.
	OnActivate func()
}

// Tray is a notification-area icon. The zero value is not usable: call New.
type Tray struct {
	opts    Options
	cmdCh   chan command
	done    chan struct{}
	hwnd    atomic.Uint64
	tid     atomic.Uint32
	icon    atomic.Uint64 // current HICON for the tooltip/icon updates
	hwndCh  chan struct{}
	hwndErr error
	hwndOne sync.Once
	closed  atomic.Bool

	// menu is the current menu, guarded so the check callback can read it while
	// the owner thread rebuilds it.
	mu   sync.Mutex
	menu []Item
	// optsMu guards the callbacks, which OnAction/OnActivate can replace while
	// the message thread is running.
	optsMu sync.Mutex
}

// callbacks returns the current callbacks under the lock.
func (t *Tray) callbacks() (action func(Action), activate func()) {
	t.optsMu.Lock()
	defer t.optsMu.Unlock()
	return t.opts.OnAction, t.opts.OnActivate
}

// command is one request from another goroutine to the message thread.
type command struct {
	kind  string // "close", "notify", "menu", "tooltip", "icon"
	title string
	text  string
	items []Item
	ico   []byte
	reply chan error
}

// New creates the hidden window, registers the icon and starts pumping messages.
// It returns only once the icon is really in the notification area, so a failure
// is reported instead of leaving an invisible process behind.
func New(opts Options) (*Tray, error) {
	if opts.Tooltip == "" {
		opts.Tooltip = "soundradar"
	}
	if opts.Menu == nil {
		opts.Menu = DefaultMenu()
	}
	t := &Tray{
		opts:   opts,
		cmdCh:  make(chan command, 8),
		done:   make(chan struct{}),
		hwndCh: make(chan struct{}),
		menu:   append([]Item(nil), opts.Menu...),
	}
	go t.messageThread()
	select {
	case <-t.hwndCh:
		if t.hwndErr != nil {
			return nil, t.hwndErr
		}
		return t, nil
	case <-time.After(10 * time.Second):
		return nil, errors.New("创建托盘图标超时（10 秒内消息线程没有就绪）")
	}
}

// DefaultMenu is the menu a bare soundradar uses.
func DefaultMenu() []Item {
	return []Item{
		{ID: ActionOpenPanel, Label: "打开管理界面"},
		{ID: ActionLiveStatus, Label: "查看实时识别状态"},
		{ID: ActionToggleOverlay, Label: "显示/隐藏命中悬浮窗"},
		{Separator: true},
		{ID: ActionCandidates, Label: "打开候选项目录"},
		{ID: ActionOpenDataDir, Label: "打开程序数据目录"},
		{Separator: true},
		{ID: ActionQuit, Label: "退出 soundradar"},
	}
}

// HWND returns the hidden window handle (0 before the thread is ready).
func (t *Tray) HWND() uintptr { return uintptr(t.hwnd.Load()) }

// SetMenu replaces the context menu (for example to update check marks).
func (t *Tray) SetMenu(items []Item) error {
	t.mu.Lock()
	t.menu = append([]Item(nil), items...)
	t.mu.Unlock()
	return t.send(command{kind: "menu", items: items})
}

// currentMenu returns a copy of the menu for the message thread.
func (t *Tray) currentMenu() []Item {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Item(nil), t.menu...)
}

// SetTooltip updates the hover text.
func (t *Tray) SetTooltip(s string) error {
	return t.send(command{kind: "tooltip", text: s})
}

// OnAction installs (or replaces) the menu-action callback. It is separate from
// Options.OnAction so a caller can close over the Tray it just created, which is
// what an action handler needs for balloon messages. Actions only arrive from
// the message loop, so installing the handler right after New is always in time.
func (t *Tray) OnAction(fn func(Action)) {
	if t == nil {
		return
	}
	t.optsMu.Lock()
	t.opts.OnAction = fn
	t.optsMu.Unlock()
}

// OnActivate installs (or replaces) the click callback, for the same reason as
// OnAction.
func (t *Tray) OnActivate(fn func()) {
	if t == nil {
		return
	}
	t.optsMu.Lock()
	t.opts.OnActivate = fn
	t.optsMu.Unlock()
}

// SetIcon replaces the icon from a whole .ico file (as embedded in the binary).
func (t *Tray) SetIcon(ico []byte) error {
	return t.send(command{kind: "icon", ico: ico})
}

// Notify shows a balloon notification. It is how a double-clicked program says
// "I am running, look for me next to the clock" without stealing focus.
func (t *Tray) Notify(title, text string) error {
	return t.send(command{kind: "notify", title: title, text: text})
}

// Close removes the icon and shuts the message thread down. It is idempotent.
func (t *Tray) Close() error {
	if t == nil || t.closed.Swap(true) {
		return nil
	}
	err := t.send(command{kind: "close"})
	select {
	case <-t.done:
	case <-time.After(5 * time.Second):
		if err == nil {
			err = errors.New("托盘线程未在 5 秒内退出")
		}
	}
	return err
}

// send queues a command on the message thread and waits for it.
func (t *Tray) send(c command) error {
	if t == nil {
		return errors.New("托盘未创建")
	}
	c.reply = make(chan error, 1)
	select {
	case t.cmdCh <- c:
	case <-t.done:
		return errors.New("托盘已关闭")
	}
	postAppCmd(t.HWND())
	select {
	case err := <-c.reply:
		return err
	case <-t.done:
		// The thread is going away; a "close" command is allowed to report
		// success because the icon is gone either way.
		if c.kind == "close" {
			return nil
		}
		return errors.New("托盘已关闭")
	case <-time.After(5 * time.Second):
		return fmt.Errorf("托盘命令 %q 超时", c.kind)
	}
}

// disposeIcon destroys an icon we created and clears the handle.
func (t *Tray) disposeIcon() {
	if h := t.icon.Swap(0); h != 0 {
		procDestroyIcon.Call(uintptr(h))
	}
}

// ---------------------------------------------------------------------------
// .ico parsing (platform independent, so it is testable anywhere)
// ---------------------------------------------------------------------------

// icoImage is one image inside an .ico file.
type icoImage struct {
	Width  int
	Height int
	Data   []byte // the raw image bytes (PNG in the files this program writes)
}

// parseICO reads an .ico file and returns its images in file order.
//
// The format is a 6-byte header (reserved, type, count), then one 16-byte
// directory entry per image, then the images themselves. A byte width/height of
// 0 means 256.
func parseICO(ico []byte) ([]icoImage, error) {
	const (
		headerSize = 6
		entrySize  = 16
	)
	if len(ico) < headerSize {
		return nil, errors.New("无效的 .ico：文件太短")
	}
	if typ := binary.LittleEndian.Uint16(ico[2:4]); typ != 1 {
		return nil, fmt.Errorf("无效的 .ico：类型字段是 %d，应当是 1（图标）", typ)
	}
	count := int(binary.LittleEndian.Uint16(ico[4:6]))
	if count <= 0 || len(ico) < headerSize+count*entrySize {
		return nil, errors.New("无效的 .ico：目录项数量不对")
	}
	images := make([]icoImage, 0, count)
	for i := 0; i < count; i++ {
		e := ico[headerSize+i*entrySize:]
		w, h := int(e[0]), int(e[1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		size := int(binary.LittleEndian.Uint32(e[8:12]))
		off := int(binary.LittleEndian.Uint32(e[12:16]))
		if size <= 0 || off < headerSize+count*entrySize || off+size > len(ico) {
			return nil, fmt.Errorf("无效的 .ico：第 %d 个图像越界（off=%d size=%d 文件=%d）",
				i, off, size, len(ico))
		}
		images = append(images, icoImage{Width: w, Height: h, Data: ico[off : off+size]})
	}
	return images, nil
}

// pickICOImage returns the image whose size is closest to want, preferring one
// that is at least as large (so a 16-pixel request never picks the 256-pixel
// image and lets Windows downscale it into mush).
func pickICOImage(images []icoImage, want int) (icoImage, bool) {
	best, bestScore := icoImage{}, -1
	for _, im := range images {
		side := im.Width
		if im.Height < side {
			side = im.Height
		}
		score := 1000
		if side >= want {
			score = side - want
		} else {
			score = 1000 + (want - side)
		}
		if bestScore < 0 || score < bestScore {
			best, bestScore = im, score
		}
	}
	return best, bestScore >= 0
}

// ValidateICOForTest reports whether b is a readable .ico file. It exists so the
// generated icon data can be checked by a test without creating a real icon
// (which needs a window station).
func ValidateICOForTest(b []byte) error {
	images, err := parseICO(b)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		return errors.New("无效的 .ico：一个图像都没有")
	}
	return nil
}
