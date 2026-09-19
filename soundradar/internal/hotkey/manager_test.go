package hotkey

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestManagerRegistersAndTriggers is the P4 B acceptance test: a real
// RegisterHotKey attempt plus an internal trigger.
//
// It deliberately does NOT synthesise keyboard input. The point is to prove
// three things:
//
//  1. NewManager() really created a message-only window on its own
//     LockOSThread thread;
//  2. Register() reports the TRUE RegisterHotKey result - success, or the
//     Chinese reason it failed (a busy hotkey, a locked-down runner);
//  3. the callback -> "save a candidate" link works, exercised through
//     Trigger(), which is also what POST /api/recall/trigger drives.
func TestManagerRegistersAndTriggers(t *testing.T) {
	m, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	if m.HWND() == 0 {
		t.Fatalf("消息窗口句柄为 0（LockOSThread + CreateWindowExW 没生效）")
	}
	if m.ThreadID() == 0 {
		t.Fatalf("窗口线程 id 为 0")
	}
	t.Logf("消息窗口: hwnd=0x%X thread=%d", m.HWND(), m.ThreadID())

	var calls atomic.Int64
	err = m.Register("recall", "F8", func() { calls.Add(1) })

	regs := m.Registered()
	t.Logf("Registered() = %v", regs)
	t.Logf("Describe()   = %s", m.Describe())

	if err != nil {
		// A blocked registration is a legitimate outcome on a locked-down
		// runner, but the reason MUST be Chinese and name the key.
		if !strings.Contains(err.Error(), "F8") {
			t.Fatalf("注册失败原因没有提到按键: %v", err)
		}
		if !hasHan(err.Error()) {
			t.Fatalf("注册失败原因不是中文: %v", err)
		}
		if m.OK("recall") {
			t.Fatalf("Register 报错但 OK() 为真")
		}
		if got := m.Error("recall"); got == "" {
			t.Fatalf("失败原因没有记录下来")
		}
		t.Logf("RegisterHotKey 失败（如实报告，不伪造）：%v", err)
		return
	}

	if !m.OK("recall") {
		t.Fatalf("Register 成功但 OK() 为假")
	}
	if got := regs["recall"]; got != "F8" {
		t.Fatalf("Registered()[\"recall\"]=%q，期望 F8", got)
	}
	if got := m.Spec("recall"); got != "F8" {
		t.Fatalf("Spec=%q", got)
	}

	// Internal trigger: the callback must run (on its own goroutine, so the
	// message thread never blocks).
	if err := m.Trigger("recall"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if !waitFor(&calls, 1, 2*time.Second) {
		t.Fatalf("Trigger 之后回调没有被调用（calls=%d）", calls.Load())
	}
	t.Logf("内部触发有效：回调执行 %d 次（没有合成任何按键）", calls.Load())

	// A second registration under the same name replaces the binding cleanly.
	if err := m.Register("recall", "F8", func() { calls.Add(10) }); err != nil {
		t.Fatalf("重复 Register 同一个按键应该成功（先注销再注册）: %v", err)
	}
	if err := m.Trigger("recall"); err != nil {
		t.Fatalf("重复注册后 Trigger: %v", err)
	}
	if !waitFor(&calls, 11, 2*time.Second) {
		t.Fatalf("重新注册后回调没有换成新的（calls=%d）", calls.Load())
	}

	// Unregister stops the callback from being reachable.
	if err := m.Unregister("recall"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if _, ok := m.Registered()["recall"]; ok {
		t.Fatalf("注销后 Registered() 里还有 recall")
	}
	if err := m.Trigger("recall"); err == nil {
		t.Fatalf("注销后 Trigger 应该报错")
	}
}

// TestRegisterNoneIsNotAnError pins the config spelling for "disabled".
func TestRegisterNoneIsNotAnError(t *testing.T) {
	m, err := NewManager()
	if err != nil {
		t.Skipf("本环境没有消息窗口: %v", err)
	}
	defer m.Close()
	for _, spec := range []string{"none", "NONE", "", "  "} {
		if err := m.Register("recall", spec, func() {}); err != nil {
			t.Fatalf("Register(%q) 不该报错: %v", spec, err)
		}
	}
	if m.OK("recall") {
		t.Fatalf("none 不该算注册成功")
	}
	if err := m.Trigger("recall"); err == nil {
		t.Fatalf("none 的 Trigger 应该报错")
	}
}

// TestRegisterRejectsBadSpecWithChineseError pins the validation path.
func TestRegisterRejectsBadSpecWithChineseError(t *testing.T) {
	m, err := NewManager()
	if err != nil {
		t.Skipf("本环境没有消息窗口: %v", err)
	}
	defer m.Close()

	if err := m.Register("recall", "Ctrl+Nope", func() {}); err == nil {
		t.Fatalf("非法按键应该报错")
	} else if !hasHan(err.Error()) {
		t.Fatalf("错误信息不是中文: %v", err)
	}
	if m.Error("recall") == "" {
		t.Fatalf("失败原因没有被记录")
	}

	if err := m.Register("", "F8", func() {}); err == nil {
		t.Fatalf("空名称应该报错")
	}
	if err := m.Register("recall", "F8", nil); err == nil {
		t.Fatalf("空回调应该报错")
	}
}

// TestSecondManagerOnSameKeyFailsLoudly pins the "已被其它程序占用" path: the
// same key cannot be registered twice system-wide, and the second attempt has to
// say so in Chinese.
func TestSecondManagerOnSameKeyFailsLoudly(t *testing.T) {
	m1, err := NewManager()
	if err != nil {
		t.Skipf("本环境没有消息窗口: %v", err)
	}
	defer m1.Close()
	if err := m1.Register("recall", "F8", func() {}); err != nil {
		t.Skipf("本环境连一次 F8 都注册不上（%v），跳过冲突用例", err)
	}

	m2, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager#2: %v", err)
	}
	defer m2.Close()
	err = m2.Register("recall", "F8", func() {})
	if err == nil {
		// Some Windows configurations deliver the key to the first window
		// instead of failing; report that instead of pretending.
		t.Logf("第二个管理器也注册成功了（Windows 未报 1409 冲突）")
		return
	}
	if !strings.Contains(err.Error(), "F8") || !hasHan(err.Error()) {
		t.Fatalf("冲突原因应该是中文且带按键名: %v", err)
	}
	t.Logf("第二个管理器的注册结果（真实输出）：%v", err)
	if m2.OK("recall") {
		t.Fatalf("注册失败时 OK() 必须为假")
	}
}

// TestManagerClosesCleanly pins that Close releases the window and is
// idempotent (a leaked message thread would hang the CLI on exit).
func TestManagerClosesCleanly(t *testing.T) {
	m, err := NewManager()
	if err != nil {
		t.Skipf("本环境没有消息窗口: %v", err)
	}
	// 先注册再关闭；注册失败也不影响本用例（Close 仍必须干净）。
	regErr := m.Register("recall", "F8", func() {})
	if regErr != nil {
		t.Logf("注册失败也没关系，Close 仍必须干净：%v", regErr)
	}
	done := make(chan error, 1)
	go func() { done <- m.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("Close 超时")
	}
	// Second Close is a no-op.
	if err := m.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	// After Close, Register must fail rather than hang.
	if err := m.Register("recall", "F8", func() {}); err == nil {
		t.Fatalf("关闭后 Register 应该报错")
	} else {
		t.Logf("关闭后 Register 的真实错误：%v", err)
	}
}

// TestDescribeAndStates pins the reporting helpers used by the CLI/API banner.
func TestDescribeAndStates(t *testing.T) {
	var nilMgr *Manager
	if got := nilMgr.Describe(); got == "" {
		t.Fatalf("nil 管理器的 Describe 不该是空字符串")
	}
	if nilMgr.Registered() == nil {
		t.Fatalf("nil 管理器的 Registered 应返回空 map")
	}
	if nilMgr.OK("x") || nilMgr.Error("x") != "" {
		t.Fatalf("nil 管理器的状态查询不对")
	}
	if err := nilMgr.Trigger("x"); err == nil {
		t.Fatalf("nil 管理器的 Trigger 应该报错")
	}
	if err := nilMgr.Close(); err != nil {
		t.Fatalf("nil 管理器的 Close 应该是 no-op: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func hasHan(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// waitFor polls until c reaches want or the deadline passes.
func waitFor(c *atomic.Int64, want int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.Load() >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return c.Load() >= want
}
