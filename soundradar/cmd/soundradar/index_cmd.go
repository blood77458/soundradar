package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/library"
)

// runIndex implements `soundradar index rebuild`.
//
// Usage:
//
//	soundradar index rebuild [--library PATH] [--out PATH]
func runIndex(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("index 需要一个子命令，目前只有 rebuild\n\n%s", indexUsage)
	}
	switch args[0] {
	case "rebuild", "build", "reindex":
		return runIndexRebuild(args[1:])
	case "-h", "--help", "help":
		fmt.Print(indexUsage)
		return nil
	default:
		return fmt.Errorf("index: 未知子命令 %q\n\n%s", args[0], indexUsage)
	}
}

const indexUsage = `soundradar index - 指纹索引维护

用法:
  soundradar index rebuild [--library <library.srz>] [--out <index.bin>]

选项:
  --library PATH  音效库文件（默认 <exe 目录>\data\library.srz）
  --out PATH      索引输出路径（默认与库文件同目录的 index.bin）

说明:
  对库里每个条目的每个样本计算 2048 维 log-mel 指纹，取能量最高的窗口做模板，
  做对称 int8 量化后写入 index.bin。样本太短的会被跳过并给出警告（不会中断）。`

func runIndexRebuild(args []string) error {
	fs := flag.NewFlagSet("index rebuild", flag.ContinueOnError)
	libPath := fs.String("library", "", "library file path (default <exe dir>/data/library.srz)")
	outPath := fs.String("out", "", "index output path (default <library dir>/index.bin)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path, err := resolveLibraryPath(*libPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("音效库文件不存在: %s\n（先用 `soundradar serve` 建库，或用 --library 指定路径）", path)
	}
	out := strings.TrimSpace(*outPath)
	if out == "" {
		out = index.DefaultPathFor(path)
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("索引输出路径无效: %w", err)
	}

	p := dsp.DefaultParams()
	fmt.Printf("[index] 音效库     : %s\n", path)
	fmt.Printf("[index] 输出索引   : %s\n", out)
	fmt.Printf("[index] 特征算法   : %s v%d（%d 维 = %d Mel 带 x %d 帧）\n",
		dsp.Algorithm, dsp.Version, p.Dim(), p.MelBands, p.WindowFrames)
	fmt.Printf("[index] 帧参数     : %d Hz / 帧长 %d / 跳步 %d (%.3f ms) / Hann / 去直流\n",
		p.SampleRate, p.FrameSize, p.HopSize, p.HopDurationS()*1000)
	fmt.Printf("[index] 归一化     : %s（余弦相似度 = 点积）\n", dsp.Norm)
	fmt.Printf("[index] 参数指纹   : %s\n", p.Fingerprint())
	fmt.Printf("[index] 开始建索引…\n")

	start := time.Now()
	ix, err := index.BuildWithParams(path, p)
	if err != nil {
		return err
	}
	if err := ix.Save(out); err != nil {
		return err
	}
	elapsed := time.Since(start)

	items, samples, bytes := ix.Stats()
	bs := ix.BuildReport()
	if bs == nil {
		return fmt.Errorf("内部错误: Build 没有返回报告")
	}

	fmt.Printf("\n[index] 完成\n")
	fmt.Printf("[index] 耗时       : %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("[index] 条目数     : %d\n", items)
	fmt.Printf("[index] 样本数     : %d（跳过 %d）\n", samples, bs.Skipped)
	fmt.Printf("[index] 特征维数   : %d\n", ix.Dim())
	fmt.Printf("[index] 量化字节   : %d 字节 (%.1f KiB)\n", bytes, float64(bytes)/1024)
	if fi, err := os.Stat(out); err == nil {
		fmt.Printf("[index] 索引文件   : %.1f KiB（含表头与条目表）\n", float64(fi.Size())/1024)
	}
	if samples > 0 {
		fmt.Printf("[index] 平均耗时   : %.2f ms/样本\n", float64(elapsed.Milliseconds())/float64(samples))
	}

	// Per-item sample counts (name + how many templates).
	fmt.Printf("\n[index] 每个条目的样本数:\n")
	perItem := append([]index.ItemInfo(nil), bs.PerItem...)
	sort.SliceStable(perItem, func(a, b int) bool { return perItem[a].Name < perItem[b].Name })
	for _, it := range perItem {
		mark := ""
		if it.Samples == 0 {
			mark = "  <- 没有可用样本，不会被匹配到"
		}
		fmt.Printf("        %-28s 样本 %d%s\n", trim(it.Name, 28), it.Samples, mark)
	}

	if len(bs.Warnings) > 0 {
		fmt.Printf("\n[index] 跳过的样本 / 警告（%d 条）:\n", len(bs.Warnings))
		for _, w := range bs.Warnings {
			where := w.Item
			if w.Sample != "" {
				where += " / " + w.Sample
			}
			fmt.Printf("        %s: %s\n", where, w.Message)
		}
	} else {
		fmt.Printf("\n[index] 没有跳过的样本\n")
	}

	if samples == 0 {
		return fmt.Errorf("索引里一个模板都没有：请先往库里添加音频样本")
	}
	fmt.Printf("\n[index] 提示: 用 `soundradar match --wav <file.wav>` 拿一段录音来认音效\n")
	return nil
}

// resolveLibraryPath applies the same default rule as `serve`.
func resolveLibraryPath(flagValue string) (string, error) {
	path := strings.TrimSpace(flagValue)
	if path == "" {
		p, err := library.DefaultPath()
		if err != nil {
			return "", err
		}
		path = p
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("库文件路径无效 %q: %w", path, err)
	}
	return abs, nil
}

// trim shortens s for a fixed-width column, counting runes not bytes.
func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
