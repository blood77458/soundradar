package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/znz/soundradar/internal/library"
)

// runMergeTingsheng folds old 拿起/拖动 samples into 同音类, deletes 放下,
// and attaches icons from the 大金 picture folder.
//
//	soundradar merge-tingsheng [--library PATH] [--icons DIR]
func runMergeTingsheng(args []string) error {
	fs := flag.NewFlagSet("merge-tingsheng", flag.ContinueOnError)
	libPath := fs.String("library", "", "library file path (default <exe dir>/data/library.srz)")
	iconDir := fs.String("icons", `C:\Users\Administrator\Pictures\大金`, "folder of item PNG/JPG icons")
	iconsOnly := fs.Bool("icons-only", false, "only refresh per-grid pictures from --icons; do not merge or delete items")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := resolveLibraryPath(*libPath)
	if err != nil {
		return err
	}
	store, err := library.Open(path)
	if err != nil {
		return err
	}
	if *iconsOnly {
		n, err := store.RefreshHintIcons(strings.TrimSpace(*iconDir))
		if err != nil {
			return err
		}
		if err := store.Save(); err != nil {
			return fmt.Errorf("写库失败: %w", err)
		}
		fmt.Printf("[merge] 只刷新格子图: %s（%d 张）\n", path, n)
		return nil
	}
	res, err := store.MergeTingshengAssets(strings.TrimSpace(*iconDir))
	if err != nil {
		return err
	}
	if err := store.Save(); err != nil {
		return fmt.Errorf("写库失败: %w", err)
	}
	fmt.Printf("[merge] 音效库: %s\n", path)
	fmt.Printf("[merge] 图标目录: %s\n", *iconDir)
	for class, n := range res.MergedSamples {
		if n > 0 {
			fmt.Printf("[merge] 合并样本 %s ← +%d\n", class, n)
		}
	}
	if len(res.Renamed) > 0 {
		fmt.Printf("[merge] 重命名唯一类: %s\n", strings.Join(res.Renamed, "；"))
	}
	if len(res.Deleted) > 0 {
		fmt.Printf("[merge] 已删除 %d 条: %s\n", len(res.Deleted), strings.Join(res.Deleted, "、"))
	}
	fmt.Printf("[merge] 格子图标 %d 张，类图标 %d 张\n", res.IconsApplied, res.ClassIcons)
	fmt.Println("[merge] 请运行 `soundradar index rebuild` 使新样本进入指纹索引。")
	return nil
}
