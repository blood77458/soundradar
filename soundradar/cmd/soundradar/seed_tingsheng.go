package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/znz/soundradar/internal/library"
)

// runSeedTingsheng inserts metadata-only 同音类 items from the 听声鉴宝 doc.
//
//	soundradar seed-tingsheng [--library PATH]
func runSeedTingsheng(args []string) error {
	fs := flag.NewFlagSet("seed-tingsheng", flag.ContinueOnError)
	libPath := fs.String("library", "", "library file path (default <exe dir>/data/library.srz)")
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
	res, err := store.SeedTingshengClasses()
	if err != nil {
		return err
	}
	if err := store.Save(); err != nil {
		return fmt.Errorf("写库失败: %w", err)
	}
	fmt.Printf("[seed] 音效库: %s\n", path)
	if len(res.Added) == 0 {
		fmt.Println("[seed] 没有新增条目（同名同音类已存在）")
	} else {
		fmt.Printf("[seed] 新增 %d 条（仅元数据，无样本）:\n", len(res.Added))
		for _, n := range res.Added {
			fmt.Printf("  + %s\n", n)
		}
	}
	if len(res.Skipped) > 0 {
		fmt.Printf("[seed] 跳过已存在 %d 条: %s\n", len(res.Skipped), strings.Join(res.Skipped, "、"))
	}
	fmt.Println("[seed] 请用管理端或 F8 为同音类补真实 wav；有样本后再 `soundradar index rebuild`。")
	return nil
}
