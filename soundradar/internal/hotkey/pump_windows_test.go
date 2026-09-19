//go:build windows

package hotkey

import (
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestPostedHotkeyReachesCallback is the regression for the F8 bug: WM_HOTKEY
// must be observed by the same thread that called RegisterHotKey. Posting the
// message to the message-only window fails if GetMessageW runs elsewhere.
func TestPostedHotkeyReachesCallback(t *testing.T) {
	m, err := NewManager()
	if err != nil {
		t.Skipf("本环境没有消息窗口: %v", err)
	}
	defer m.Close()

	var calls atomic.Int64
	if err := m.Register("recall", "F8", func() { calls.Add(1) }); err != nil {
		t.Skipf("F8 注册失败: %v", err)
	}
	m.mu.Lock()
	id := m.ids["recall"]
	hwnd := m.hwnd
	m.mu.Unlock()
	if id == 0 || hwnd == 0 {
		t.Fatalf("注册成功但 id=%d hwnd=%d", id, hwnd)
	}

	procPostMessage.Call(hwnd, wmHotkey, uintptr(id), 0)
	if !waitFor(&calls, 1, 2*time.Second) {
		t.Fatalf("投递 WM_HOTKEY 后回调没有执行（calls=%d）。消息循环和 RegisterHotKey 不在同一条线程", calls.Load())
	}
	time.Sleep(40 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("回调执行了 %d 次，期望恰好 1 次", got)
	}
}

// TestSynthesizedF8ReachesCallback presses F8 through keybd_event. This is the
// path a real key takes into RegisterHotKey; it is test-only and is not used
// by the program itself.
func TestSynthesizedF8ReachesCallback(t *testing.T) {
	m, err := NewManager()
	if err != nil {
		t.Skipf("本环境没有消息窗口: %v", err)
	}
	defer m.Close()

	var calls atomic.Int64
	if err := m.Register("recall", "F8", func() { calls.Add(1) }); err != nil {
		t.Skipf("F8 注册失败: %v", err)
	}

	if err := tapVK(0x77); err != nil {
		t.Fatalf("模拟按下 F8: %v", err)
	}
	if !waitFor(&calls, 1, 2*time.Second) {
		t.Fatalf("模拟按下 F8 后回调没有执行（calls=%d）", calls.Load())
	}
}

func tapVK(vk uintptr) error {
	user32 := windows.NewLazySystemDLL("user32.dll")
	mapVK := user32.NewProc("MapVirtualKeyW")
	keybd := user32.NewProc("keybd_event")
	scan, _, err := mapVK.Call(vk, 0) // MAPVK_VK_TO_VSC
	if scan == 0 {
		return err
	}
	keybd.Call(vk, scan, 0, 0)
	time.Sleep(30 * time.Millisecond)
	keybd.Call(vk, scan, 0x0002, 0) // KEYEVENTF_KEYUP
	return nil
}
