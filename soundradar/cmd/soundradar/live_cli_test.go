package main

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/znz/soundradar/internal/wav"
)

// These tests drive runLive in-process (no .exe, no sound card): the file
// source replays a WAV, so the whole CLI path - banner, tick lines, JSONL, CSV
// - is covered by `go test`.

// liveReplayWAV writes "<lead> s silence + <mid> s tone + <tail> s silence".
func liveReplayWAV(t *testing.T, dir, name string, lead, mid, tail, freq float64) string {
	t.Helper()
	total := int((lead + mid + tail) * cliRate)
	pcm := make([]int16, total)
	mid16 := cliTone(freq, mid, 0.6)
	copy(pcm[int(lead*cliRate):], mid16)
	path := filepath.Join(dir, name)
	if err := wav.WriteInt16File(path, cliRate, 1, pcm); err != nil {
		t.Fatalf("WriteInt16File: %v", err)
	}
	return path
}

func TestCLILiveReplayJSONAndCSV(t *testing.T) {
	dir := t.TempDir()
	libPath := writeCLILibrary(t, dir)
	idxPath := filepath.Join(dir, "index.bin")
	rec := liveReplayWAV(t, dir, "replay.wav", 0.5, 0.5, 0.5, 880)
	csvPath := filepath.Join(dir, "hits.csv")

	out, err := captureStdout(t, func() error {
		return runLive([]string{
			"--wav", rec, "--library", libPath, "--index", idxPath,
			"--json", "--csv", csvPath, "--top", "5",
		})
	})
	if err != nil {
		t.Fatalf("live --wav --json 失败: %v\n%s", err, out)
	}

	// --- JSONL -----------------------------------------------------------
	var ticks, events, summaries, withTop int
	var firstTick map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("JSONL 行不是合法 JSON: %v\n%s", err, line)
		}
		switch rec["type"] {
		case "tick":
			ticks++
			if _, ok := rec["level"]; !ok {
				t.Fatalf("tick 缺少 level 字段: %s", line)
			}
			top, _ := rec["top"].([]any)
			if _, ok := rec["top"]; !ok {
				t.Fatalf("tick 缺少 top 字段: %s", line)
			}
			if len(top) > 0 {
				withTop++
				if firstTick == nil {
					firstTick = rec
				}
			}
		case "event":
			events++
		case "summary":
			summaries++
		}
	}
	if ticks < 5 {
		t.Fatalf("只有 %d 条 tick（需要 >= 5）", ticks)
	}
	if events < 1 {
		t.Fatalf("没有 event 记录")
	}
	if summaries != 1 {
		t.Fatalf("summary 记录 %d 条，期望 1 条", summaries)
	}
	top, _ := firstTick["top"].([]any)
	if len(top) == 0 {
		t.Fatalf("%d 条 tick 里没有一条带排名: %v", ticks, firstTick)
	}
	t.Logf("JSONL: tick=%d（其中带排名 %d）event=%d summary=%d", ticks, withTop, events, summaries)
	t.Logf("首个带排名 tick 的 top 前 2 名: %v", top[:min(2, len(top))])

	// --- CSV -------------------------------------------------------------
	f, err := os.Open(csvPath)
	if err != nil {
		t.Fatalf("CSV 没有生成: %v", err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("CSV 解析失败: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("CSV 只有 %d 行（应有表头 + 至少 1 条命中）", len(rows))
	}
	header := rows[0]
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\ufeff")
	}
	want := []string{"时间", "条目ID", "名称", "分数", "margin", "电平dBFS"}
	if strings.Join(header, ",") != strings.Join(want, ",") {
		t.Fatalf("CSV 表头 = %v，期望 %v", header, want)
	}
	var hit []string
	for _, r := range rows[1:] {
		if strings.Contains(r[2], "880") {
			hit = r
			break
		}
	}
	if hit == nil {
		t.Fatalf("CSV 里没有 880 Hz 的命中行: %v", rows)
	}
	t.Logf("CSV 表头: %v", header)
	t.Logf("CSV 命中行: %v", hit)
	if len(hit) != 6 || hit[0] == "" || hit[3] == "" {
		t.Fatalf("命中行列数/内容不对: %v", hit)
	}
}

func TestCLILiveReplayTextOutput(t *testing.T) {
	dir := t.TempDir()
	libPath := writeCLILibrary(t, dir)
	idxPath := filepath.Join(dir, "index.bin")
	rec := liveReplayWAV(t, dir, "replay.wav", 0.2, 0.5, 0.2, 880)

	out, err := captureStdout(t, func() error {
		return runLive([]string{"--wav", rec, "--library", libPath, "--index", idxPath, "--top", "3"})
	})
	if err != nil {
		t.Fatalf("live --wav 失败: %v\n%s", err, out)
	}
	for _, want := range []string{"[live] 音源", "[live] 音效库", "[live] 索引", "特征参数", "指纹", "实时统计", "处理耗时", "丢帧"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "命中") {
		t.Fatalf("输出里没有命中行:\n%s", out)
	}
	t.Logf("live 文本输出（节选）:\n%s", trimLines(out, 26))
}

func TestCLILiveQuietOnlyEvents(t *testing.T) {
	dir := t.TempDir()
	libPath := writeCLILibrary(t, dir)
	idxPath := filepath.Join(dir, "index.bin")
	rec := liveReplayWAV(t, dir, "replay.wav", 0.2, 0.5, 0.2, 880)

	out, err := captureStdout(t, func() error {
		return runLive([]string{"--wav", rec, "--library", libPath, "--index", idxPath, "--quiet"})
	})
	if err != nil {
		t.Fatalf("live --quiet 失败: %v\n%s", err, out)
	}
	if strings.Contains(out, "音效库") || strings.Contains(out, "实时统计") {
		t.Fatalf("--quiet 不应打印横幅/统计:\n%s", out)
	}
	if !strings.Contains(out, "命中") {
		t.Fatalf("--quiet 应打印命中:\n%s", out)
	}
	t.Logf("--quiet 输出:\n%s", strings.TrimSpace(out))
}

func TestCLILiveSecondsStopsRealtimeReplay(t *testing.T) {
	dir := t.TempDir()
	libPath := writeCLILibrary(t, dir)
	idxPath := filepath.Join(dir, "index.bin")
	// 3 s of tone, replayed at real speed, cut off after 0.6 s.
	rec := liveReplayWAV(t, dir, "long.wav", 0, 3, 0, 880)

	out, err := captureStdout(t, func() error {
		return runLive([]string{"--wav", rec, "--library", libPath, "--index", idxPath, "--realtime", "--seconds", "0.6"})
	})
	if err != nil {
		t.Fatalf("live --realtime --seconds 失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "seconds") {
		t.Fatalf("结束原因应为 seconds:\n%s", out)
	}
	// Roughly 0.6 s of audio must have been processed, not the full 3 s.
	m := regexp.MustCompile(`(\d+) 采样 = ([\d.]+) 秒音频`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("找不到音频时长的统计行:\n%s", out)
	}
	secs, err := strconv.ParseFloat(m[2], 64)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", m[2], err)
	}
	t.Logf("--realtime --seconds 0.6 实际处理 %s 采样 = %.3f 秒音频（文件总长 3.000 秒）", m[1], secs)
	if secs > 1.5 {
		t.Fatalf("--seconds 0.6 之后仍处理了 %.3f 秒音频，超时没有生效", secs)
	}
	if secs < 0.2 {
		t.Fatalf("只处理了 %.3f 秒音频，太快退出", secs)
	}
}

func TestCLILiveNeedsWAVOrDevice(t *testing.T) {
	dir := t.TempDir()
	libPath := writeCLILibrary(t, dir)
	out, err := captureStdout(t, func() error {
		return runLive([]string{"--wav", filepath.Join(dir, "missing.wav"), "--library", libPath})
	})
	if err == nil {
		t.Fatalf("不存在的 wav 应当报错:\n%s", out)
	}
	if !strings.Contains(err.Error(), "读取音频文件失败") {
		t.Fatalf("错误信息不对: %v", err)
	}
	t.Logf("不存在的 wav -> %v", err)

	// An empty library (its own directory, so no index can be reused) must be
	// refused with a Chinese hint instead of running with nothing to match.
	emptyDir := t.TempDir()
	empty := filepath.Join(emptyDir, "empty.srz")
	if err := os.WriteFile(empty, []byte("not a library"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = captureStdout(t, func() error {
		return runLive([]string{"--wav", liveReplayWAV(t, dir, "x.wav", 0, 0.5, 0, 880), "--library", empty})
	})
	if err == nil {
		t.Fatalf("坏库应当报错，实际成功:\n%s", out)
	}
	t.Logf("坏库 -> %v", err)
}

func TestCLILiveHelp(t *testing.T) {
	out, err := captureStdout(t, func() error {
		e := runLive([]string{"--help"})
		if e != nil && !strings.Contains(e.Error(), "flag: help requested") {
			return e
		}
		return nil
	})
	if err != nil {
		t.Fatalf("live --help: %v", err)
	}
	_ = out
	for _, want := range []string{"--wav", "--csv", "--seconds", "--quiet", "--json"} {
		if !strings.Contains(liveUsage, want) {
			t.Fatalf("liveUsage 缺少 %q", want)
		}
	}
	if !strings.Contains(usage, "soundradar live") {
		t.Fatal("主 usage 没有列出 live 子命令")
	}
}

// trimLines keeps the first n lines (test output stays readable).
func trimLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + "\n…（省略 " + strconv.Itoa(len(lines)-n) + " 行）"
}
