package match

import (
	"bytes"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/wav"
)

// ---------------------------------------------------------------------------
// fixtures: a real library + a real index + real probe windows
// ---------------------------------------------------------------------------

const testRate = 48000

func testTone(freq, seconds float64) []float32 {
	n := int(seconds * testRate)
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(0.6 * math.Sin(2*math.Pi*freq*float64(i)/testRate))
	}
	return out
}

func toneWAV(t *testing.T, pcm []float32) []byte {
	t.Helper()
	i16 := make([]int16, len(pcm))
	for i, v := range pcm {
		i16[i] = wav.ClampInt16(float64(v))
	}
	var buf bytes.Buffer
	if err := wav.WriteInt16(&buf, testRate, 1, i16); err != nil {
		t.Fatalf("WriteInt16: %v", err)
	}
	return buf.Bytes()
}

var testFreqs = []float64{880, 1500, 3000}

// newTestIndex builds a library of three pure tones and the index over it.
func newTestIndex(t *testing.T) *index.Index {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "library.srz")
	store := library.New(path, "match 测试库")
	for _, f := range testFreqs {
		name := "音效 " + trimFloat(f) + " Hz"
		it := &library.Item{
			Name: name, Threshold: 0.75, CooldownMs: 400,
			Samples: []library.Sample{{File: library.SampleFile(0), Source: "generated"}},
		}
		if err := store.AddItem(it, nil, [][]byte{toneWAV(t, testTone(f, 0.5))}); err != nil {
			t.Fatalf("AddItem: %v", err)
		}
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	ix, err := index.Build(path)
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	return ix
}

func trimFloat(f float64) string { return fmt.Sprintf("%.0f", f) }

// probe is a genuine normalised feature window computed from a tone. Fixtures
// are cached because the tests call it dozens of times.
var probeCache = map[float64][]float32{}

func probe(t *testing.T, freq float64) []float32 {
	t.Helper()
	if w, ok := probeCache[freq]; ok {
		return w
	}
	p := dsp.DefaultParams()
	a, err := dsp.NewAnalyzer(p)
	if err != nil {
		t.Fatalf("NewAnalyzer: %v", err)
	}
	a.Push(testTone(freq, 0.5))
	w := a.Window()
	if w == nil {
		t.Fatal("probe window is nil")
	}
	probeCache[freq] = w
	return w
}

// newTestEngine builds an engine over the three-tone index.
func newTestEngine(t *testing.T, opts Options) *Engine {
	t.Helper()
	return NewEngine(newTestIndex(t), opts)
}

// engineQuery returns a real window of the tone that belongs to item `item`
// (items are in testFreqs order).
func engineQuery(t *testing.T, e *Engine, item int) []float32 {
	t.Helper()
	if item < 0 || item >= len(testFreqs) {
		t.Fatalf("item %d out of range", item)
	}
	return probe(t, testFreqs[item])
}

func TestEngineGates(t *testing.T) {
	opts := DefaultOptions()
	opts.MinMargin = 0.05
	e := newTestEngine(t, opts)
	q := engineQuery(t, e, 0)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 1) nil window -> nothing
	if ev, top := e.Tick(nil, -20, now); ev != nil || top != nil {
		t.Fatalf("nil 窗口应返回 (nil, nil)，得到 %v / %v", ev, top)
	}
	// 2) silence -> skipped before scoring
	if ev, top := e.Tick(q, -80, now); ev != nil || top != nil {
		t.Fatalf("静音窗口应返回 (nil, nil)，得到 %v / %v", ev, top)
	}
	if _, _, silent, _ := e.Stats(); silent != 1 {
		t.Fatalf("静音计数 = %d，期望 1", silent)
	}

	// 3) a good window fires
	ev, top := e.Tick(q, -20, now)
	if ev == nil {
		t.Fatalf("正常窗口应当命中，top=%v", top)
	}
	if len(top) == 0 {
		t.Fatal("命中时应返回 top 列表")
	}
	if ev.Score < 0.9 {
		t.Fatalf("自匹配分数 %.4f 太低", ev.Score)
	}
	if ev.Name != e.ix.ItemName(ev.ID) {
		t.Fatalf("事件名称 %q 与索引不符 (%q)", ev.Name, e.ix.ItemName(ev.ID))
	}
	t.Logf("命中: %s(id=%s) %.4f margin=%.4f level=%.1f dBFS", ev.Name, ev.ID, ev.Score, ev.Margin, ev.LevelDBFS)

	// 4) per-item cooldown: the same item cannot fire again straight away
	if ev2, _ := e.Tick(q, -20, now.Add(10*time.Millisecond)); ev2 != nil {
		t.Fatalf("冷却期内不应再次命中，得到 %v", ev2)
	}
	// ... even after the global refractory has passed (cooldown > refractory)
	if ev2, _ := e.Tick(q, -20, now.Add(100*time.Millisecond)); ev2 != nil {
		t.Fatalf("条目冷却期内（100ms < 400ms）不应再次命中，得到 %v", ev2)
	}
	// ... but it may fire once its own cooldown expired
	ev3, _ := e.Tick(q, -20, now.Add(401*time.Millisecond))
	if ev3 == nil {
		t.Fatal("超过条目冷却期后应当再次命中")
	}
	if id, score := e.Pending(); id != "" || score != 0 {
		t.Fatalf("命中后 Pending 应清空，得到 %s %.4f", id, score)
	}
}

func TestEngineMarginGate(t *testing.T) {
	// Isolate the margin gate: the threshold is deliberately low so the only
	// reason a candidate can be rejected is that the runner-up is too close.
	// (With the default 0.75 threshold a blended query cannot clear the
	// threshold AND sit inside a 0.05 margin at the same time, because two
	// pure-tone templates are nearly orthogonal - the blend that makes the two
	// scores nearly equal is also a poor match for both.)
	opts := DefaultOptions()
	opts.DefaultThreshold = 0.3
	e := newTestEngine(t, opts)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	blend := make([]float32, e.ix.Dim())
	q0 := engineQuery(t, e, 0)
	q1 := engineQuery(t, e, 1)
	// b = (w*q0 + q1)/|...|. The open-loop estimate w ~ 1 + margin*sqrt(2) only
	// has to be right to within a factor of two because the gap is monotone in w;
	// a handful of bisection steps then land it just inside the margin.
	score := func(w float64) (float64, float64) {
		b := make([]float32, len(q0))
		for i := range b {
			b[i] = float32(w)*q0[i] + q1[i]
		}
		b = dsp.Normalize(b)
		var a, c2 float64
		for i := range b {
			a += float64(b[i]) * float64(q0[i])
			c2 += float64(b[i]) * float64(q1[i])
		}
		return a, c2
	}
	lo, hi := 1+opts.MinMargin*math.Sqrt2/4, 1+opts.MinMargin*math.Sqrt2*4
	w := 1 + opts.MinMargin*math.Sqrt2
	for i := 0; i < 40; i++ {
		w = (lo + hi) / 2
		a, b := score(w)
		if a-b > opts.MinMargin*0.98 {
			hi = w
		} else {
			lo = w
		}
	}
	t.Logf("margin 测试: w=%.6f 时两个模板的分数 %.4f / %.4f（差 %.4f）", w, func() float64 { a, _ := score(w); return a }(), func() float64 { _, b := score(w); return b }(), func() float64 { a, b := score(w); return a - b }())
	for i := range blend {
		blend[i] = float32(w)*q0[i] + q1[i]
	}
	blend = dsp.Normalize(blend)

	ev, top := e.Tick(blend, -20, now)
	if len(top) < 2 {
		t.Fatalf("top 列表应至少 2 条，得到 %d", len(top))
	}
	gap := top[0].Score.Float() - top[1].Score.Float()
	if top[0].Score.Float() < e.Options().DefaultThreshold {
		t.Fatalf("构造的模糊查询 top1 = %.4f 低于阈值 %.2f（w=%.4f），测试前提不成立",
			top[0].Score.Float(), e.Options().DefaultThreshold, w)
	}
	if gap >= e.Options().MinMargin {
		t.Fatalf("构造的模糊查询 margin = %.4f 不小于 %.2f（w=%.4f），测试前提不成立",
			gap, e.Options().MinMargin, w)
	}
	if ev != nil {
		t.Fatalf("margin 不足时不应命中，得到 %s %.4f (margin %.4f)", ev.Name, ev.Score, ev.Margin)
	}
	if id, score := e.Pending(); id == "" || score == 0 {
		t.Fatal("被 margin 拦下的候选应当记录在 Pending 里")
	}
	t.Logf("margin 拦截: top1=%s %.4f top2=%s %.4f (差 %.4f < %.2f，top1 已过阈值 %.2f)",
		top[0].Name, top[0].Score.Float(), top[1].Name, top[1].Score.Float(), gap, e.Options().MinMargin, e.Options().DefaultThreshold)

	// Lowering the margin lets the very same window through.
	e2 := newTestEngine(t, opts)
	e2.SetMinMargin(0.0)
	ev2, _ := e2.Tick(blend, -20, now)
	if ev2 == nil {
		t.Fatal("margin 门槛降到 0 后应当命中")
	}
	t.Logf("margin 放宽后命中: %s %.4f margin=%.4f", ev2.Name, ev2.Score, ev2.Margin)
}

func TestEngineThresholdGate(t *testing.T) {
	e := newTestEngine(t, DefaultOptions())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := engineQuery(t, e, 0)

	// Learn which item the probe belongs to, then raise its threshold above the
	// achievable score -> no event, recorded as pending. This is the path the P1
	// management UI uses per item.
	ev, _ := e.Tick(q, -20, now)
	if ev == nil {
		t.Fatal("第一次应当命中")
	}
	id := ev.ID
	e.Reset()
	e.SetThreshold(id, 0.999)
	if got := e.Threshold(id); got != 0.999 {
		t.Fatalf("Threshold(%s) = %v，期望 0.999", id, got)
	}
	ev2, _ := e.Tick(q, -20, now.Add(time.Second))
	if ev2 != nil {
		t.Fatalf("阈值 0.999 时不应命中（分数 %.4f）", ev2.Score)
	}
	pid, pscore := e.Pending()
	t.Logf("阈值拦截: Pending = %s %.4f（阈值 %.3f）", pid, pscore, e.Threshold(pid))
	if pid != id {
		t.Fatalf("Pending 条目 = %s，期望 %s", pid, id)
	}
	// Dropping the override makes the default (0.75) apply again.
	e.SetThreshold(id, 0)
	if e.Threshold(id) != DefaultOptions().DefaultThreshold {
		t.Fatalf("清除覆盖后阈值 = %v，期望默认值", e.Threshold(id))
	}
	if ev3, _ := e.Tick(q, -20, now.Add(2*time.Second)); ev3 == nil {
		t.Fatal("恢复默认阈值后应当命中")
	}
}

func TestEngineRefractorySuppressesSecondItem(t *testing.T) {
	e := newTestEngine(t, DefaultOptions())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	qA := engineQuery(t, e, 0)
	qB := engineQuery(t, e, 1)

	evA, _ := e.Tick(qA, -20, now)
	if evA == nil {
		t.Fatal("A 应当命中")
	}
	// Learn B's identity from a one-off probe tick, then reset the cooldowns so
	// the refractory test starts from a clean slate.
	evBprobe, _ := e.Tick(qB, -20, now.Add(time.Second))
	if evBprobe == nil {
		t.Fatal("B 应当命中")
	}
	idB := evBprobe.ID
	e.Reset()
	if evA2, _ := e.Tick(qA, -20, now); evA2 == nil {
		t.Fatal("Reset 后 A 应当命中")
	}
	// B arrives 20 ms later: inside the 50 ms global refractory, so it is
	// dropped even though it is a different item and its own cooldown is clean.
	evB, _ := e.Tick(qB, -20, now.Add(20*time.Millisecond))
	if evB != nil {
		t.Fatalf("全局不应期内不应命中第二个条目，得到 %v", evB)
	}
	pid, _ := e.Pending()
	if pid != idB {
		t.Fatalf("被不应期拦下的条目 = %s，期望 %s", pid, idB)
	}
	// 60 ms later the refractory window has passed -> B fires.
	evB2, _ := e.Tick(qB, -20, now.Add(60*time.Millisecond))
	if evB2 == nil {
		t.Fatal("不应期过后 B 应当命中")
	}
	if evB2.ID != idB {
		t.Fatalf("命中的是 %s，期望 %s", evB2.ID, idB)
	}
	t.Logf("不应期: %s@0ms 命中，%s(id=%s)@20ms 被抑制，@60ms 命中（%.4f）",
		evA.Name, evB2.Name, idB, evB2.Score)
}

func TestEngineOptionsNormalize(t *testing.T) {
	// An all-zero Options must not disable every gate.
	e := NewEngine(nil, Options{})
	o := e.Options()
	if o.DefaultThreshold <= 0 || o.CooldownMs <= 0 || o.RefractoryMs <= 0 || o.SilenceDBFS == 0 {
		t.Fatalf("零值 Options 没有被补全: %+v", o)
	}
	// A nil index is legal and inert.
	e2 := NewEngine(nil, DefaultOptions())
	if ev, top := e2.Tick([]float32{1, 0, 0}, 0, time.Now()); ev != nil || top != nil {
		t.Fatalf("nil 索引应返回 (nil, nil)，得到 %v / %v", ev, top)
	}
}

func TestEngineReset(t *testing.T) {
	e := newTestEngine(t, DefaultOptions())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := engineQuery(t, e, 0)
	if ev, _ := e.Tick(q, -20, now); ev == nil {
		t.Fatal("第一次应当命中")
	}
	if ev, _ := e.Tick(q, -20, now.Add(10*time.Millisecond)); ev != nil {
		t.Fatal("冷却期内不应命中")
	}
	e.Reset()
	ticks, fired, silent, rejected := e.Stats()
	if ticks != 0 || fired != 0 || silent != 0 || rejected != 0 {
		t.Fatalf("Reset 后计数未清空: %d/%d/%d/%d", ticks, fired, silent, rejected)
	}
	if ev, _ := e.Tick(q, -20, now.Add(10*time.Millisecond)); ev == nil {
		t.Fatal("Reset 后应当可以立即再次命中")
	}
}

func TestEngineQuietLevelIsSkipped(t *testing.T) {
	e := newTestEngine(t, DefaultOptions())
	q := engineQuery(t, e, 0)
	now := time.Now()
	// exactly at the gate -> allowed (>= SilenceDBFS)
	if ev, _ := e.Tick(q, DefaultOptions().SilenceDBFS, now); ev == nil {
		t.Fatal("电平正好等于静音门槛时应当参与判定")
	}
	// one dB below -> skipped, and -Inf (digital silence) too
	e.Reset()
	if ev, _ := e.Tick(q, DefaultOptions().SilenceDBFS-0.001, now); ev != nil {
		t.Fatal("低于静音门槛时不应命中")
	}
	if ev, _ := e.Tick(q, math.Inf(-1), now); ev != nil {
		t.Fatal("-Inf 电平不应命中")
	}
	if _, _, silent, _ := e.Stats(); silent != 2 {
		t.Fatalf("静音计数 = %d，期望 2", silent)
	}
}
