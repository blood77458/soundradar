package recall

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/wav"
)

// ---------------------------------------------------------------------------
// A1 / A2: Ring exactness and boundedness
// ---------------------------------------------------------------------------

// chunkPattern is deliberately NOT aligned to the capacity and contains both
// tiny and multi-kilobyte chunks, so the wrap-around path is exercised in every
// possible phase.
var chunkPattern = []int{137, 1, 4096, 3, 999, 256, 17}

// signal makes a deterministic, easily comparable sample stream. It is not
// random on purpose: a test failure should be reproducible and readable.
//
// Values are the EXACT int16 results of the float32 conversion PushBlock does
// (round(v/32768*32768)), so a test can push the stream as float32 blocks and
// compare the stored WAV against the original int16 sample for sample.
func signal(n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		raw := (i*7919)%65536 - 32768 // [-32768, 32767]
		f := float32(raw) / 32768
		out[i] = int16(math.Round(float64(f) * 32768))
	}
	return out
}

// tail returns the last n samples of s (or all of s when shorter).
func tail(s []int16, n int) []int16 {
	if n >= len(s) {
		return s
	}
	return s[len(s)-n:]
}

// eqSamples compares two int16 streams sample by sample.
func eqSamples(t *testing.T, got, want []int16, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: 长度不一致 got=%d want=%d", what, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: 第 %d 个样本不一致 got=%d want=%d（共 %d 个）", what, i, got[i], want[i], len(got))
		}
	}
}

// decodeToInt16 recovers the exact 16-bit PCM a canonical WAV holds.
//
// wav.Decode normalises by /32768, so multiplying by 32768 and rounding is an
// exact inverse for every int16 value. (Also note wav.ClampInt16 is deliberately
// NOT used here: it scales by 32767 - the P0 convention - and therefore loses
// one LSB on large magnitudes, which is fine for audio but useless as a
// bit-exactness check.)
func decodeToInt16(t *testing.T, path string) []int16 {
	t.Helper()
	a, err := wav.ReadFile(path)
	if err != nil {
		t.Fatalf("读回 WAV 失败: %v", err)
	}
	if a.Info.SampleRate != SampleRate || a.Info.Channels != 1 || a.Info.BitsPerSample != 16 {
		t.Fatalf("WAV 格式 %s，期望 48000 Hz / 1 ch / 16-bit", a.Info.Layout())
	}
	out := make([]int16, len(a.Samples))
	for i, v := range a.Samples {
		out[i] = int16(math.Round(v * 32768))
	}
	return out
}

// TestRingSnapshotMatchesInputTail is the P4 A1 acceptance test: for a total
// push length T that is greater than, equal to and smaller than the window N,
// Snapshot() must equal the last min(T, N) seconds of the input SAMPLE BY SAMPLE
// - both when pushed as one slice and when pushed through unaligned chunks.
func TestRingSnapshotMatchesInputTail(t *testing.T) {
	const windowSec = 2
	capacity := windowSec * SampleRate // 96000

	cases := []struct {
		name  string
		total int
	}{
		{"T > N（窗口满，应保留最新 N 秒）", capacity + 12345},
		{"T == N（刚好填满）", capacity},
		{"T < N（还没填满，应保留全部）", capacity - 4321},
		{"T 远大于 N（3.5 倍容量）", capacity*7/2 + 7},
		{"T 极小（187 个样本）", 187},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := signal(tc.total)
			wantLen := tc.total
			if wantLen > capacity {
				wantLen = capacity
			}
			want := tail(in, capacity)

			// (1) single push
			one := NewRing(windowSec)
			one.Push(in)
			gotOne := one.Snapshot()
			if len(gotOne) != wantLen {
				t.Fatalf("一次性 Push：Snapshot 长度 %d，期望 %d", len(gotOne), wantLen)
			}
			eqSamples(t, gotOne, want, "一次性 Push 的 Snapshot")

			// (2) unaligned chunked push
			chunked := NewRing(windowSec)
			off, k := 0, 0
			for off < len(in) {
				n := chunkPattern[k%len(chunkPattern)]
				k++
				if off+n > len(in) {
					n = len(in) - off
				}
				chunked.Push(in[off : off+n])
				off += n
			}
			gotChunked := chunked.Snapshot()
			eqSamples(t, gotChunked, want, "非对齐分块 Push 的 Snapshot")
			eqSamples(t, gotChunked, gotOne, "分块 Push 与一次性 Push")

			// Seconds()/Len() must agree with the content.
			if chunked.Len() != wantLen {
				t.Fatalf("Len=%d，期望 %d", chunked.Len(), wantLen)
			}
			if math.Abs(chunked.Seconds()-float64(wantLen)/SampleRate) > 1e-12 {
				t.Fatalf("Seconds=%v，期望 %v", chunked.Seconds(), float64(wantLen)/SampleRate)
			}
		})
	}
}

// TestRingPushLargerThanCapacityKeepsNewest covers the "one block longer than
// the whole window" path (a very small window plus a big WASAPI packet).
func TestRingPushLargerThanCapacityKeepsNewest(t *testing.T) {
	r := NewRing(1) // 48000 samples
	in := signal(48000*3 + 11)
	r.Push(in)
	if r.Len() != 48000 {
		t.Fatalf("Len=%d，期望 48000", r.Len())
	}
	eqSamples(t, r.Snapshot(), tail(in, 48000), "超大单块 Push")
}

// TestRingResetAndEmpty pins the empty behaviour.
func TestRingResetAndEmpty(t *testing.T) {
	r := NewRing(1)
	if got := r.Snapshot(); got != nil {
		t.Fatalf("空 Ring 的 Snapshot 应为 nil，得到 %v", got)
	}
	if r.Seconds() != 0 || r.Len() != 0 {
		t.Fatalf("空 Ring 的 Seconds/Len 应为 0，得到 %v/%d", r.Seconds(), r.Len())
	}
	r.Push([]int16{1, 2, 3})
	if r.Len() != 3 {
		t.Fatalf("Push 3 个样本后 Len=%d", r.Len())
	}
	r.Reset()
	if r.Len() != 0 || r.Snapshot() != nil {
		t.Fatalf("Reset 后应回到空状态，得到 Len=%d", r.Len())
	}
	// Reset must not resize the buffer.
	if r.Cap() != SampleRate {
		t.Fatalf("Reset 后容量应保持 %d，得到 %d", SampleRate, r.Cap())
	}
}

// TestRingClampsWindow pins the configuration guard rails.
func TestRingClampsWindow(t *testing.T) {
	if got := NewRing(0).Cap(); got != MinSeconds*SampleRate {
		t.Fatalf("NewRing(0).Cap()=%d，期望 %d", got, MinSeconds*SampleRate)
	}
	if got := NewRing(-5).Cap(); got != MinSeconds*SampleRate {
		t.Fatalf("NewRing(-5).Cap()=%d，期望 %d", got, MinSeconds*SampleRate)
	}
	if got := NewRing(9999).Cap(); got != MaxSeconds*SampleRate {
		t.Fatalf("NewRing(9999).Cap()=%d，期望 %d", got, MaxSeconds*SampleRate)
	}
}

// TestRingDoesNotGrow is the P4 A2 acceptance test: pushing ten times the
// capacity must not grow Len() beyond the capacity nor the Go heap beyond a
// small constant (the ring must not accumulate anything).
func TestRingDoesNotGrow(t *testing.T) {
	const windowSec = 2
	capacity := windowSec * SampleRate
	r := NewRing(windowSec)

	block := signal(4096)
	pushes := (capacity * 10) / len(block)
	if pushes < 100 {
		t.Fatalf("用例设计错误：只有 %d 次 Push", pushes)
	}

	// Warm up first so the measurement does not include one-off allocations.
	for i := 0; i < 200; i++ {
		r.Push(block)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < pushes; i++ {
		r.Push(block)
		if r.Len() != capacity {
			t.Fatalf("第 %d 次 Push 后 Len=%d，容量应为 %d", i, r.Len(), capacity)
		}
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	pushesTotal := 200 + pushes
	pushedSamples := int64(pushesTotal) * int64(len(block))
	heapDelta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if r.Cap() != capacity {
		t.Fatalf("容量变成了 %d，应为 %d", r.Cap(), capacity)
	}
	t.Logf("连续 Push %d 次（%d 个样本 = %.1f 倍容量）后：Len=%d（容量 %d），HeapAlloc %d → %d（Δ=%+d 字节）",
		pushesTotal, pushedSamples, float64(pushedSamples)/float64(capacity),
		r.Len(), capacity, before.HeapAlloc, after.HeapAlloc, heapDelta)
	// 200 MB of audio went through the ring (2 s window); anything above 4 MB of
	// retained heap would mean the ring is accumulating.
	if heapDelta > 4<<20 {
		t.Fatalf("堆增长 %d 字节，超过 4 MiB：Ring 可能在无限增长", heapDelta)
	}
}

// ---------------------------------------------------------------------------
// A3: Store round trip, atomicity, prune
// ---------------------------------------------------------------------------

func newTestStore(t *testing.T, maxFiles int) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir, maxFiles)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestStoreRoundTrip(t *testing.T) {
	s := newTestStore(t, 100)
	pcm := signal(SampleRate * 3) // exactly 3 s, the default recall window
	cand, err := s.Add(pcm, CandidateMeta{GuessID: "abcd1234", GuessName: "880Hz 测试音", GuessScore: 0.9812, Source: "cli"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if cand.ID == "" || strings.ContainsAny(cand.ID, `\/:*?"<>|`) {
		t.Fatalf("候选 id 不合法: %q", cand.ID)
	}
	if math.Abs(cand.Seconds-3.0) > 1e-9 {
		t.Fatalf("Seconds=%v，期望 3.0", cand.Seconds)
	}
	if cand.Silent {
		t.Fatalf("非静音音频被标成 Silent")
	}
	if cand.PeakDBFS > 0 || math.IsInf(cand.PeakDBFS, 0) || math.IsNaN(cand.PeakDBFS) {
		t.Fatalf("PeakDBFS=%v 不合理（应落在 (-inf, 0]）", cand.PeakDBFS)
	}
	if cand.GuessID != "abcd1234" || cand.GuessName != "880Hz 测试音" || !cand.GuessScoreValid {
		t.Fatalf("猜测字段丢失: %+v", cand)
	}
	if filepath.Dir(cand.File) != s.Dir() {
		t.Fatalf("File=%s 不在 %s 里", cand.File, s.Dir())
	}

	// List must contain exactly this one.
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != cand.ID {
		t.Fatalf("List=%+v，期望只有 %s", list, cand.ID)
	}
	// Get must round trip the metadata (compare the JSON-visible fields).
	got, err := s.Get(cand.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Seconds != cand.Seconds || got.PeakDBFS != cand.PeakDBFS || got.Silent != cand.Silent ||
		got.GuessID != cand.GuessID || got.GuessName != cand.GuessName || got.GuessScore != cand.GuessScore ||
		got.Frames != cand.Frames || got.Source != cand.Source || !got.CreatedAt.Equal(cand.CreatedAt) {
		t.Fatalf("Get 与 Add 不一致:\n got=%+v\nwant=%+v", got, cand)
	}

	// The WAV must decode back to the very samples handed to Add.
	a, err := wav.ReadFile(cand.File)
	if err != nil {
		t.Fatalf("读回 WAV 失败: %v", err)
	}
	if a.Info.SampleRate != SampleRate || a.Info.Channels != 1 || a.Info.BitsPerSample != 16 {
		t.Fatalf("WAV 格式 %s，期望 48000 Hz / 1 ch / 16-bit", a.Info.Layout())
	}
	if len(a.Samples) != len(pcm) {
		t.Fatalf("WAV 样本数 %d，期望 %d", len(a.Samples), len(pcm))
	}
	eqSamples(t, decodeToInt16(t, cand.File), pcm, "Add 写出的 WAV 与写入的 PCM")

	// The JSON on disk must not contain ±Inf / NaN and must carry silent:false.
	raw, err := os.ReadFile(filepath.Join(s.Dir(), cand.ID+".json"))
	if err != nil {
		t.Fatalf("读 JSON 失败: %v", err)
	}
	text := string(raw)
	for _, bad := range []string{"-Inf", "+Inf", "Infinity", "NaN"} {
		if strings.Contains(text, bad) {
			t.Fatalf("meta.json 含 %q（encoding/json 不能表示 ±Inf）: %s", bad, text)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("meta.json 不是合法 JSON: %v", err)
	}
	if decoded["silent"] != false {
		t.Fatalf("silent=%v，期望 false", decoded["silent"])
	}
	if decoded["peakDbfs"].(float64) > 0 {
		t.Fatalf("peakDbfs=%v 应 <= 0（dBFS 越大越响，满刻度是 0）", decoded["peakDbfs"])
	}

	// Delete removes both halves.
	if err := s.Delete(cand.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(cand.File); !os.IsNotExist(err) {
		t.Fatalf("Delete 后 WAV 仍存在: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), cand.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("Delete 后 JSON 仍存在: %v", err)
	}
	if err := s.Delete(cand.ID); err == nil {
		t.Fatalf("重复 Delete 应该报错")
	}
	if _, err := s.Get(cand.ID); err == nil {
		t.Fatalf("删除后 Get 应该报错")
	}
}

// TestStoreSilentCandidate pins the -Inf lesson: a digital-silence candidate is
// written with silent:true, peakDbfs:0 and still produces valid JSON.
func TestStoreSilentCandidate(t *testing.T) {
	s := newTestStore(t, 10)
	silence := make([]int16, SampleRate)
	cand, err := s.Add(silence, CandidateMeta{Source: "api"})
	if err != nil {
		t.Fatalf("Add(静音): %v", err)
	}
	if !cand.Silent {
		t.Fatalf("全零音频应标 Silent=true，得到 %+v", cand)
	}
	if cand.PeakDBFS != 0 {
		t.Fatalf("静音时 PeakDBFS 应为 0（不能是 -Inf），得到 %v", cand.PeakDBFS)
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir(), cand.ID+".json"))
	if err != nil {
		t.Fatalf("读 JSON: %v", err)
	}
	if strings.Contains(string(raw), "Inf") || strings.Contains(string(raw), "NaN") {
		t.Fatalf("静音候选项 JSON 含 Inf/NaN: %s", raw)
	}
	got, err := s.Get(cand.ID)
	if err != nil || !got.Silent || got.PeakDBFS != 0 {
		t.Fatalf("静音候选项往返失败: %+v err=%v", got, err)
	}
}

// TestStoreAddIsAtomicAndSequential pins two properties at once:
//
//   - a candidate that exists is complete (JSON and WAV both fully written);
//   - a failed Add leaves NO trace (no half file) and does not consume an id,
//     which is what makes "中途失败不留半个文件、不破坏已有候选项" true.
func TestStoreAddIsAtomicAndSequential(t *testing.T) {
	s := newTestStore(t, 100)

	first, err := s.Add(signal(1000), CandidateMeta{Source: "hotkey"})
	if err != nil {
		t.Fatalf("第一次 Add: %v", err)
	}

	// An empty buffer is rejected BEFORE anything is written.
	if _, err := s.Add(nil, CandidateMeta{}); err == nil {
		t.Fatalf("Add(nil) 应该报错")
	}
	// No temp file may survive a rejected Add.
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("失败后残留临时文件: %s", e.Name())
		}
	}
	if len(entries) != 2 {
		t.Fatalf("目录里应有 2 个文件（1 个 wav + 1 个 json），实际 %d: %v", len(entries), entryNames(entries))
	}

	// The next successful Add must not reuse the id of the failed attempt.
	second, err := s.Add(signal(1000), CandidateMeta{Source: "hotkey"})
	if err != nil {
		t.Fatalf("第二次 Add: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("两次 Add 得到同一个 id: %s", second.ID)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List 应有 2 个候选项，得到 %d", len(list))
	}
	// 最新的在最前。
	if list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("List 顺序不是时间倒序: %s, %s", list[0].ID, list[1].ID)
	}
	// 第一个候选项没有被破坏。
	if _, err := wav.ReadFile(first.File); err != nil {
		t.Fatalf("第二次 Add 破坏了第一个候选项: %v", err)
	}
	eqSamples(t, decodeToInt16(t, first.File), signal(1000), "第一个候选项的内容未被第二次 Add 改变")
}

// TestStorePruneRemovesOldest pins the retention policy.
func TestStorePruneRemovesOldest(t *testing.T) {
	s := newTestStore(t, 5)
	ids := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		c, err := s.Add(signal(480), CandidateMeta{Source: "cli"})
		if err != nil {
			t.Fatalf("Add #%d: %v", i, err)
		}
		ids = append(ids, c.ID)
		// Prune after every save is exactly what Recaller.Save does.
		if _, err := s.Prune(); err != nil {
			t.Fatalf("Prune #%d: %v", i, err)
		}
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 5 {
		t.Fatalf("Prune 后应保留 5 个，得到 %d: %v", len(list), entryNames2(list))
	}
	// The three oldest must be gone from disk completely.
	for _, gone := range ids[:3] {
		if _, err := os.Stat(filepath.Join(s.Dir(), gone+".json")); !os.IsNotExist(err) {
			t.Fatalf("被剪掉的 %s 仍留有 .json", gone)
		}
		if _, err := os.Stat(filepath.Join(s.Dir(), gone+".wav")); !os.IsNotExist(err) {
			t.Fatalf("被剪掉的 %s 仍留有 .wav", gone)
		}
	}
	// The five newest must be intact and readable.
	for _, keep := range ids[3:] {
		if _, err := s.Get(keep); err != nil {
			t.Fatalf("应保留的 %s 读不到: %v", keep, err)
		}
	}

	// An orphan .wav (crash between the two writes of Add) is cleaned up and not
	// counted as a candidate.
	orphan := filepath.Join(s.Dir(), "999999-20200101-000000.wav")
	if err := os.WriteFile(orphan, []byte("RIFFxxxxWAVE"), 0o644); err != nil {
		t.Fatalf("造孤儿 wav: %v", err)
	}
	removed, err := s.Prune()
	if err != nil {
		t.Fatalf("Prune(孤儿): %v", err)
	}
	if removed == 0 {
		t.Fatalf("Prune 应报告删除了孤儿 wav")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("孤儿 wav 没有被清理")
	}
	if l2, _ := s.List(); len(l2) != 5 {
		t.Fatalf("孤儿 wav 不该被当成候选项，List=%d", len(l2))
	}
}

// TestStorePruneMaxFilesOne covers the boundary value.
func TestStorePruneMaxFilesOne(t *testing.T) {
	s := newTestStore(t, 1)
	for i := 0; i < 4; i++ {
		if _, err := s.Add(signal(96), CandidateMeta{}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if _, err := s.Prune(); err != nil {
			t.Fatalf("Prune: %v", err)
		}
	}
	list, _ := s.List()
	if len(list) != 1 {
		t.Fatalf("maxFiles=1 时应只有 1 个，得到 %d", len(list))
	}
}

// TestStoreMarkUsed pins the promote bookkeeping: consumed candidates leave the
// inbox, stay on disk as .used.* and are pruned later.
func TestStoreMarkUsed(t *testing.T) {
	s := newTestStore(t, 8)
	c, err := s.Add(signal(480), CandidateMeta{Source: "api"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.MarkUsed(c.ID); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}
	list, _ := s.List()
	if len(list) != 0 {
		t.Fatalf("已处理的候选项不该出现在收件箱里: %v", entryNames2(list))
	}
	used, err := s.ListUsed()
	if err != nil {
		t.Fatalf("ListUsed: %v", err)
	}
	if len(used) != 1 || used[0].ID != c.ID {
		t.Fatalf("ListUsed=%v，期望 1 个 %s", entryNames2(used), c.ID)
	}
	if _, err := s.Get(c.ID); err == nil {
		t.Fatalf("已处理的候选项不该能 Get 到")
	}
	if err := s.MarkUsed(c.ID); err == nil {
		t.Fatalf("重复 MarkUsed 应该报错")
	}
	// The audio is still there for an undo.
	if _, err := os.Stat(filepath.Join(s.Dir(), c.ID+".used.wav")); err != nil {
		t.Fatalf("已处理候选项的音频应保留: %v", err)
	}
}

// TestStoreRejectsBadIDs pins the path-traversal guard.
func TestStoreRejectsBadIDs(t *testing.T) {
	s := newTestStore(t, 4)
	for _, bad := range []string{"", "../evil", "a/b", `a\b`, "a.b", strings.Repeat("x", 65)} {
		if _, err := s.Get(bad); err == nil {
			t.Fatalf("Get(%q) 应该被拒绝", bad)
		}
		if err := s.Delete(bad); err == nil {
			t.Fatalf("Delete(%q) 应该被拒绝", bad)
		}
	}
}

func entryNames(es []os.DirEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func entryNames2(cs []Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

// ---------------------------------------------------------------------------
// Recaller: the hotkey/API path
// ---------------------------------------------------------------------------

func TestRecallerSaveAndGuess(t *testing.T) {
	s := newTestStore(t, 50)
	r := NewRecaller(2, s) // 2 s window

	// 3 s of audio through the same 960-sample blocks internal/live delivers.
	want := signal(3 * SampleRate)
	for off := 0; off < len(want); off += 960 {
		end := off + 960
		if end > len(want) {
			end = len(want)
		}
		blk := make([]float32, end-off)
		for i, v := range want[off:end] {
			blk[i] = float32(v) / 32768
		}
		r.PushBlock(blk)
	}
	if math.Abs(r.Seconds()-2.0) > 1e-9 {
		t.Fatalf("Recaller 覆盖 %.6f 秒，期望 2.0", r.Seconds())
	}

	r.Tick([]index.ItemScore{{ID: "abcd1234", Name: "880Hz 测试音", Score: index.ItemScoreValue{Value: 0.9812}}})
	id, name, score := r.Guess()
	if id != "abcd1234" || name != "880Hz 测试音" || math.Abs(score-0.9812) > 1e-9 {
		t.Fatalf("Guess=(%q,%q,%v)，期望 880Hz 条目", id, name, score)
	}

	var notified []Candidate
	r.OnSave(func(c Candidate) { notified = append(notified, c) })

	cand, err := r.Save("hotkey")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if cand.GuessID != "abcd1234" || cand.GuessName != "880Hz 测试音" {
		t.Fatalf("候选项没有带上猜测: %+v", cand)
	}
	if math.Abs(cand.Seconds-2.0) > 1e-9 {
		t.Fatalf("候选项时长 %.6f，期望 2.0", cand.Seconds)
	}
	if cand.Source != "hotkey" {
		t.Fatalf("Source=%q，期望 hotkey", cand.Source)
	}
	if len(notified) != 1 || notified[0].ID != cand.ID {
		t.Fatalf("OnSave 回调没有收到候选项: %v", entryNames2(notified))
	}
	if r.Saves() != 1 {
		t.Fatalf("Saves=%d，期望 1", r.Saves())
	}

	// The stored WAV is exactly the tail of the input, sample for sample.
	wantTail := tail(want, 2*SampleRate)
	eqSamples(t, decodeToInt16(t, cand.File), wantTail, "Save 写出的 WAV 与输入尾部")

	desc := Describe(cand)
	if !strings.Contains(desc, cand.ID) || !strings.Contains(desc, "猜测：880Hz 测试音 0.98") {
		t.Fatalf("Describe 输出不符合约定: %s", desc)
	}
}

func TestRecallerSaveWithoutAudioFails(t *testing.T) {
	s := newTestStore(t, 4)
	r := NewRecaller(1, s)
	if _, err := r.Save("api"); err == nil {
		t.Fatalf("空环形缓冲的 Save 应该报错")
	}
	if _, err := r.Save("api"); err == nil || !strings.Contains(err.Error(), "环形缓冲") {
		t.Fatalf("错误信息应说明环形缓冲为空，得到 %v", err)
	}
}

func TestRecallerTickEmptyClearsGuess(t *testing.T) {
	s := newTestStore(t, 4)
	r := NewRecaller(1, s)
	r.Tick([]index.ItemScore{{ID: "x", Name: "X", Score: index.ItemScoreValue{Value: 0.9}}})
	r.Tick(nil)
	id, _, _ := r.Guess()
	if id != "" {
		t.Fatalf("空排名应清掉猜测，得到 %q", id)
	}
	r.PushBlock([]float32{0.1, -0.1})
	c, err := r.Save("cli")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if c.GuessID != "" || c.GuessScoreValid {
		t.Fatalf("没有排名时候选项不该带猜测: %+v", c)
	}
	if !strings.Contains(Describe(c), "猜测：无") {
		t.Fatalf("Describe 应显示“猜测：无”: %s", Describe(c))
	}
}

// TestStoreSurvivesReopen checks that a new Store over the same directory does
// not reuse an existing id (it scans the highest sequence on disk).
func TestStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir, 10)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	a, err := s1.Add(signal(48), CandidateMeta{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	s2, err := NewStore(dir, 10)
	if err != nil {
		t.Fatalf("重新 NewStore: %v", err)
	}
	b, err := s2.Add(signal(48), CandidateMeta{})
	if err != nil {
		t.Fatalf("Add after reopen: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("重开目录后复用了 id %s", a.ID)
	}
	// A file with a foreign name must not break the scan.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写无关文件: %v", err)
	}
	if _, err := s2.Add(signal(48), CandidateMeta{}); err != nil {
		t.Fatalf("有无关文件时 Add 失败: %v", err)
	}
	_ = fmt.Sprintf("%v", b)
}
