package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/wav"
)

// These tests drive the real subcommand entry points (runIndexRebuild and
// runMatch) against a temporary library, so the P2 CLI is covered by `go test`
// even when the built .exe cannot be launched on this machine (Smart App
// Control blocks unsigned binaries per file hash).

const cliRate = 48000

func cliTone(freq, seconds, amp float64) []int16 {
	n := int(seconds * cliRate)
	out := make([]int16, n)
	for i := range out {
		out[i] = wav.ClampInt16(amp * math.Sin(2*math.Pi*freq*float64(i)/cliRate))
	}
	return out
}

func cliToneWAV(t *testing.T, freq, seconds float64) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := wav.WriteInt16(&buf, cliRate, 1, cliTone(freq, seconds, 0.6)); err != nil {
		t.Fatalf("WriteInt16: %v", err)
	}
	return buf.Bytes()
}

// writeCLILibrary creates a library holding one 880 Hz and one 1500 Hz tone.
func writeCLILibrary(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "library.srz")
	store := library.New(path, "CLI 测试库")
	for _, f := range []float64{880, 1500} {
		it := &library.Item{
			Name: "音效 " + strconv.FormatFloat(f, 'f', 0, 64) + "Hz", Threshold: 0.75, CooldownMs: 400,
			Samples: []library.Sample{{File: library.SampleFile(0), Source: "generated"}},
		}
		if err := store.AddItem(it, nil, [][]byte{cliToneWAV(t, f, 0.5)}); err != nil {
			t.Fatalf("AddItem: %v", err)
		}
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return path
}

func writeCLIWAV(t *testing.T, dir, name string, freq, seconds float64) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := wav.WriteInt16File(path, cliRate, 1, cliTone(freq, seconds, 0.6)); err != nil {
		t.Fatalf("WriteInt16File: %v", err)
	}
	return path
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// printed.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	runErr := fn()
	w.Close()
	os.Stdout = old
	out := <-done
	return out, runErr
}

func TestCLIIndexRebuildAndMatch(t *testing.T) {
	dir := t.TempDir()
	libPath := writeCLILibrary(t, dir)
	idxPath := filepath.Join(dir, "index.bin")

	// ---- index rebuild ---------------------------------------------------
	out, err := captureStdout(t, func() error {
		return runIndexRebuild([]string{"--library", libPath, "--out", idxPath})
	})
	if err != nil {
		t.Fatalf("index rebuild 失败: %v\n%s", err, out)
	}
	for _, want := range []string{"条目数", "样本数", "特征维数", "量化字节", "耗时", "每个条目的样本数"} {
		if !strings.Contains(out, want) {
			t.Fatalf("index rebuild 输出缺少 %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("索引文件没有生成: %v", err)
	}
	t.Logf("index rebuild 输出:\n%s", out)

	// ---- match (default library path in the same directory) --------------
	rec := writeCLIWAV(t, dir, "rec880.wav", 880, 0.6)
	out, err = captureStdout(t, func() error {
		return runMatch([]string{"--wav", rec, "--library", libPath, "--index", idxPath})
	})
	if err != nil {
		t.Fatalf("match 失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "结论") {
		t.Fatalf("match 输出没有结论行:\n%s", out)
	}
	if !strings.Contains(out, "880") {
		t.Fatalf("match 没有认出 880 Hz:\n%s", out)
	}
	t.Logf("match 输出:\n%s", out)

	// ---- match --json ----------------------------------------------------
	out, err = captureStdout(t, func() error {
		return runMatch([]string{"--wav", rec, "--library", libPath, "--index", idxPath, "--json", "--all"})
	})
	if err != nil {
		t.Fatalf("match --json 失败: %v\n%s", err, out)
	}
	var res matchResultJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("match --json 输出不是合法 JSON: %v\n%s", err, out)
	}
	if res.Best == nil {
		t.Fatalf("JSON 里没有 best: %s", out)
	}
	if !strings.Contains(res.Best.Name, "880") {
		t.Fatalf("JSON best 是 %q，期望 880 Hz 音效", res.Best.Name)
	}
	if res.Best.Score <= 0.9 {
		t.Fatalf("JSON best.Score = %.4f，未超过 0.9", res.Best.Score)
	}
	if res.Index.Items != 2 || res.Index.Samples != 2 {
		t.Fatalf("JSON 索引规模 = %d/%d，期望 2/2", res.Index.Items, res.Index.Samples)
	}
	if res.Search.Windows <= 0 || res.Search.Frames < 32 {
		t.Fatalf("JSON 滑窗统计不合理: frames=%d windows=%d", res.Search.Frames, res.Search.Windows)
	}
	if res.Index.Rebuilt {
		t.Fatal("索引已存在且指纹一致，不应重建")
	}
	t.Logf("match --json: best=%s %.4f at %.0f ms, windows=%d, frames=%d",
		res.Best.Name, res.Best.Score, res.Best.AtMs, res.Search.Windows, res.Search.Frames)

	// ---- the other tone must win for its own recording -------------------
	rec2 := writeCLIWAV(t, dir, "rec1500.wav", 1500, 0.6)
	out, err = captureStdout(t, func() error {
		return runMatch([]string{"--wav", rec2, "--library", libPath, "--index", idxPath, "--json"})
	})
	if err != nil {
		t.Fatalf("match(1500) 失败: %v\n%s", err, out)
	}
	var res2 matchResultJSON
	if err := json.Unmarshal([]byte(out), &res2); err != nil {
		t.Fatalf("match(1500) --json 解析失败: %v\n%s", err, out)
	}
	if res2.Best == nil || !strings.Contains(res2.Best.Name, "1500") {
		t.Fatalf("1500 Hz 录音的 best = %v", res2.Best)
	}
	t.Logf("match(1500) -> %s %.4f", res2.Best.Name, res2.Best.Score)
}

func TestCLIMatchRebuildsMissingIndex(t *testing.T) {
	dir := t.TempDir()
	libPath := writeCLILibrary(t, dir)
	missing := filepath.Join(dir, "sub", "index.bin")
	rec := writeCLIWAV(t, dir, "rec.wav", 880, 0.6)

	out, err := captureStdout(t, func() error {
		return runMatch([]string{"--wav", rec, "--library", libPath, "--index", missing, "--json"})
	})
	if err != nil {
		t.Fatalf("match 应自动重建索引: %v\n%s", err, out)
	}
	var res matchResultJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, out)
	}
	if !res.Index.Rebuilt {
		t.Fatalf("索引不存在时 JSON 应报告 rebuilt=true: %s", out)
	}
	if res.Index.RebuildWhy == "" {
		t.Fatal("rebuilt=true 时应给出原因")
	}
	if _, err := os.Stat(missing); err != nil {
		t.Fatalf("自动重建后索引文件应存在: %v", err)
	}
	t.Logf("自动重建原因: %s", res.Index.RebuildWhy)

	// A corrupted index must also be rebuilt rather than failing.
	if err := os.WriteFile(missing, []byte("not an index at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = captureStdout(t, func() error {
		return runMatch([]string{"--wav", rec, "--library", libPath, "--index", missing, "--json"})
	})
	if err != nil {
		t.Fatalf("索引损坏时应重建: %v\n%s", err, out)
	}
	var res2 matchResultJSON
	if err := json.Unmarshal([]byte(out), &res2); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	if !res2.Index.Rebuilt {
		t.Fatal("索引损坏时应当重建")
	}
	t.Logf("索引损坏后的重建原因: %s", res2.Index.RebuildWhy)
}

func TestCLIMatchRejectsShortRecording(t *testing.T) {
	dir := t.TempDir()
	libPath := writeCLILibrary(t, dir)
	short := writeCLIWAV(t, dir, "short.wav", 880, 0.05) // 50 ms < 186.7 ms window

	out, err := captureStdout(t, func() error {
		return runMatch([]string{"--wav", short, "--library", libPath})
	})
	if err == nil {
		t.Fatalf("过短的录音应当报错:\n%s", out)
	}
	if !strings.Contains(err.Error(), "太短") {
		t.Fatalf("错误信息应说明录音太短，实际: %v", err)
	}
	t.Logf("过短录音的错误信息: %v", err)
}

func TestCLIMatchRejectsEmptyLibrary(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.srz")
	store := library.New(empty, "空库")
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rec := writeCLIWAV(t, dir, "rec.wav", 880, 0.6)

	out, err := captureStdout(t, func() error {
		return runMatch([]string{"--wav", rec, "--library", empty})
	})
	if err == nil {
		t.Fatalf("空库应当报错:\n%s", out)
	}
	if !strings.Contains(err.Error(), "没有可用音频样本") {
		t.Fatalf("错误信息应说明库是空的，实际: %v", err)
	}
	t.Logf("空库的错误信息: %v", err)
}

func TestCLIIndexRequiresSubcommand(t *testing.T) {
	if err := runIndex(nil); err == nil {
		t.Fatal("index 不带子命令应当报错")
	} else if !strings.Contains(err.Error(), "rebuild") {
		t.Fatalf("错误信息应提示 rebuild，实际: %v", err)
	}
	out, err := captureStdout(t, func() error { return runIndex([]string{"--help"}) })
	if err != nil {
		t.Fatalf("index --help 不应报错: %v", err)
	}
	if !strings.Contains(out, "index rebuild") {
		t.Fatalf("index --help 缺少用法:\n%s", out)
	}
}

func TestCLIMatchHelp(t *testing.T) {
	out, err := captureStdout(t, func() error {
		// flag.ContinueOnError + -h prints usage and returns ErrHelp
		e := runMatch([]string{"--help"})
		if e != nil && !strings.Contains(e.Error(), "flag: help requested") && e != flag.ErrHelp {
			return e
		}
		return nil
	})
	if err != nil {
		t.Fatalf("match --help: %v", err)
	}
	_ = out
	if !strings.Contains(matchUsage, "--wav") || !strings.Contains(matchUsage, "--all") {
		t.Fatal("matchUsage 缺少选项说明")
	}
	if !strings.Contains(usage, "index rebuild") || !strings.Contains(usage, "match") {
		t.Fatal("主 usage 没有列出 P2 子命令")
	}
}
