package overlay

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A4. fade curve
// ---------------------------------------------------------------------------

func TestFadeAlphaShape(t *testing.T) {
	const fadeIn, hold, fadeOut = 100, 400, 200
	cases := []struct {
		age  int
		want float64
	}{
		{-50, 0},   // clamped
		{0, 0},     // starts invisible
		{25, 0.25}, // quarter of the fade-in
		{50, 0.5},
		{99, 0.99},
		{100, 1},   // fade-in finished
		{499, 1},   // still holding (hold is 400 ms)
		{500, 1},   // the hold ends exactly here
		{600, 0.5}, // half way through the fade-out
		{699, 0.005},
		{700, 0}, // gone
		{5000, 0},
	}
	for _, tc := range cases {
		got := FadeAlpha(tc.age, fadeIn, hold, fadeOut)
		if diff := got - tc.want; diff > 0.011 || diff < -0.011 {
			t.Errorf("FadeAlpha(%d) = %.4f，期望 %.4f", tc.age, got, tc.want)
		}
	}
	if got := TotalLifetimeMs(fadeIn, hold, fadeOut); got != 700 {
		t.Errorf("TotalLifetimeMs = %d，期望 700", got)
	}
	// Degenerate configurations must not divide by zero or panic.
	for _, tc := range [][3]int{{0, 100, 0}, {0, 0, 0}, {-5, -5, -5}, {10, 0, 10}} {
		for age := -10; age < 50; age++ {
			a := FadeAlpha(age, tc[0], tc[1], tc[2])
			if a < 0 || a > 1 {
				t.Fatalf("FadeAlpha(%d, %v) = %v 越界", age, tc, a)
			}
		}
	}
}

func TestFadeAlphaMonotoneInEveryPhase(t *testing.T) {
	const fadeIn, hold, fadeOut = 60, 1150, 250
	prev := -1.0
	// Fade in: strictly non-decreasing.
	for age := 0; age <= fadeIn; age++ {
		a := FadeAlpha(age, fadeIn, hold, fadeOut)
		if a < prev-1e-9 {
			t.Fatalf("淡入阶段在 %d ms 处不单调：%.4f < %.4f", age, a, prev)
		}
		prev = a
	}
	// Hold: exactly 1.
	for age := fadeIn + 1; age <= fadeIn+hold; age++ {
		if a := FadeAlpha(age, fadeIn, hold, fadeOut); a != 1 {
			t.Fatalf("保持阶段在 %d ms 处 alpha=%.4f，期望 1", age, a)
		}
	}
	// Fade out: strictly non-increasing.
	prev = 1
	for age := fadeIn + hold; age <= fadeIn+hold+fadeOut+5; age++ {
		a := FadeAlpha(age, fadeIn, hold, fadeOut)
		if a > prev+1e-9 {
			t.Fatalf("淡出阶段在 %d ms 处不单调：%.4f > %.4f", age, a, prev)
		}
		prev = a
	}
	if prev != 0 {
		t.Errorf("淡出结束后 alpha = %.4f，期望 0", prev)
	}
}

func TestOverallAlphaUsesTheMostVisibleEvent(t *testing.T) {
	timing := Timing{FadeInMs: 100, DurationMs: 400, FadeOutMs: 200}
	// A single fresh event drives the fade-in.
	if got := OverallAlpha([]DisplayItem{{AgeMs: 50}}, timing); got != 0.5 {
		t.Errorf("单个 50 ms 的事件 alpha = %.3f，期望 0.5", got)
	}
	// A brand-new event next to one that is fading out must NOT drag the whole
	// canvas to 0: the shared canvas keeps the larger alpha, so the new hit is
	// visible immediately and the old one keeps fading on its own pixel level.
	items := []DisplayItem{{AgeMs: 0}, {AgeMs: 650}}
	if got := OverallAlpha(items, timing); got != 0.25 {
		t.Errorf("新事件 + 正在淡出的事件 alpha = %.3f，期望 0.25（取较大的）", got)
	}
	// Two held events -> fully opaque.
	if got := OverallAlpha([]DisplayItem{{AgeMs: 150}, {AgeMs: 300}}, timing); got != 1 {
		t.Errorf("两个保持期事件 alpha = %.3f，期望 1", got)
	}
	// All events gone -> transparent.
	if got := OverallAlpha([]DisplayItem{{AgeMs: 9999}}, timing); got != 0 {
		t.Errorf("全部过期 alpha = %.3f，期望 0", got)
	}
	if got := OverallAlpha(nil, timing); got != 0 {
		t.Errorf("空队列 alpha = %.3f，期望 0", got)
	}
}

// ---------------------------------------------------------------------------
// A4. queue behaviour
// ---------------------------------------------------------------------------

func TestQueueDropsOldestWhenFull(t *testing.T) {
	q := NewQueue(3, Timing{FadeInMs: 10, DurationMs: 1000, FadeOutMs: 10})
	for _, id := range []string{"e1", "e2", "e3"} {
		q.Show(DisplayItem{ID: id, Name: id})
	}
	if q.Len() != 3 {
		t.Fatalf("队列长度 = %d，期望 3", q.Len())
	}
	if q.Dropped() != 0 {
		t.Errorf("未溢出时 Dropped = %d，期望 0", q.Dropped())
	}
	q.Show(DisplayItem{ID: "e4", Name: "e4"})
	items := q.Snapshot(time.Now())
	if len(items) != 3 {
		t.Fatalf("溢出后队列长度 = %d，期望 3", len(items))
	}
	got := []string{items[0].ID, items[1].ID, items[2].ID}
	want := []string{"e2", "e3", "e4"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("溢出后队列 = %v，期望 %v（丢最旧、留最新）", got, want)
		}
	}
	if q.Dropped() != 1 {
		t.Errorf("Dropped = %d，期望 1", q.Dropped())
	}
	// Five more DISTINCT events on a 3-deep queue drop exactly five.
	// （同一个 ID 重复 Show 不再算新事件——它只刷新已有格子，见下一个测试。）
	for i := 0; i < 5; i++ {
		q.Show(DisplayItem{ID: "x" + string(rune('0'+i))})
	}
	if q.Dropped() != 6 {
		t.Errorf("Dropped = %d，期望 6", q.Dropped())
	}
	if q.Len() != 3 {
		t.Errorf("队列长度 = %d，期望 3", q.Len())
	}
}

// TestQueueRefreshInPlaceForSameItem 锁定"同一个音效刷新已有格子、不再占新格子"这条
// 行为。否则持续的声音（引擎声、连续脚步、循环提示音）会在几百毫秒内把
// maxSimultaneous 个格子全占满，并把其它真正不同的音效挤出去。
func TestQueueRefreshInPlaceForSameItem(t *testing.T) {
	q := NewQueue(3, Timing{FadeInMs: 0, DurationMs: 1000, FadeOutMs: 0})
	q.Show(DisplayItem{ID: "a", Name: "a", Score: 0.10})
	q.Show(DisplayItem{ID: "b", Name: "b", Score: 0.20})
	for i := 0; i < 10; i++ {
		q.Show(DisplayItem{ID: "a", Name: "a", Score: 0.90})
	}
	items := q.Snapshot(time.Now())
	if len(items) != 2 {
		t.Fatalf("队列长度 = %d，期望 2（同一个 ID 只占一个格子）", len(items))
	}
	if d := q.Dropped(); d != 0 {
		t.Errorf("Dropped = %d，期望 0（刷新同一格不算丢帧）", d)
	}
	last := items[len(items)-1]
	if last.ID != "a" {
		t.Errorf("刷新的条目应被挪到最新位置，实际最后一个是 %q", last.ID)
	}
	if last.Score != 0.90 {
		t.Errorf("刷新后的分数 = %v，期望 0.90", last.Score)
	}
}

func TestQueueExpiresAfterLifetime(t *testing.T) {
	q := NewQueue(3, Timing{FadeInMs: 60, DurationMs: 1150, FadeOutMs: 250})
	life := TotalLifetimeMs(60, 1150, 250)
	if life != 1460 {
		t.Fatalf("总时长 = %d，期望 1460", life)
	}
	q.Show(DisplayItem{ID: "a"})
	start := time.Now()

	// Just before the end: still there, and its age is reported.
	items := q.Snapshot(start.Add(time.Duration(life-1) * time.Millisecond))
	if len(items) != 1 {
		t.Fatalf("寿命结束前事件被移除了")
	}
	if items[0].AgeMs != life-1 {
		t.Errorf("AgeMs = %d，期望 %d", items[0].AgeMs, life-1)
	}
	// At the end: gone.
	if items := q.Snapshot(start.Add(time.Duration(life) * time.Millisecond)); len(items) != 0 {
		t.Errorf("寿命结束后仍有 %d 个事件", len(items))
	}
	if q.Len() != 0 {
		t.Errorf("过期事件没有被清理：Len=%d", q.Len())
	}
	// Expiry is not a capacity drop.
	if q.Dropped() != 0 {
		t.Errorf("过期不应算作丢弃，Dropped=%d", q.Dropped())
	}
}

func TestQueueSetMaxShrinksAndDrops(t *testing.T) {
	q := NewQueue(4, Timing{FadeInMs: 0, DurationMs: 1000, FadeOutMs: 0})
	for i := 0; i < 4; i++ {
		q.Show(DisplayItem{ID: string(rune('a' + i))})
	}
	q.SetMax(2)
	if q.Len() != 2 {
		t.Fatalf("缩小容量后长度 = %d，期望 2", q.Len())
	}
	if q.Dropped() != 2 {
		t.Errorf("缩小容量丢弃 %d 个，期望 2", q.Dropped())
	}
	items := q.Snapshot(time.Now())
	if items[0].ID != "c" || items[1].ID != "d" {
		t.Errorf("缩小容量后保留 = %s,%s，期望 c,d（保留最新的）", items[0].ID, items[1].ID)
	}
}

func TestQueueSetTimingChangesTheCurve(t *testing.T) {
	q := NewQueue(2, Timing{FadeInMs: 100, DurationMs: 100, FadeOutMs: 100})
	q.Show(DisplayItem{ID: "a"})
	time.Sleep(20 * time.Millisecond)
	if a := OverallAlpha(q.Snapshot(time.Now()), q.Timing()); a <= 0 || a >= 1 {
		t.Errorf("200 ms 谱线下 20 ms 的 alpha = %.3f，期望 (0,1)", a)
	}
	q.SetTiming(Timing{FadeInMs: 0, DurationMs: 1000, FadeOutMs: 0})
	if a := OverallAlpha(q.Snapshot(time.Now()), q.Timing()); a != 1 {
		t.Errorf("改成瞬时淡入后 alpha = %.3f，期望 1", a)
	}
}

func TestQueueClear(t *testing.T) {
	q := NewQueue(3, Timing{FadeInMs: 0, DurationMs: 100, FadeOutMs: 0})
	q.Show(DisplayItem{ID: "a"})
	q.Show(DisplayItem{ID: "b"})
	q.Clear()
	if q.Len() != 0 {
		t.Errorf("Clear 之后还有 %d 个事件", q.Len())
	}
	if items := q.Snapshot(time.Now()); len(items) != 0 {
		t.Errorf("Clear 之后 Snapshot 返回 %d 个", len(items))
	}
}
