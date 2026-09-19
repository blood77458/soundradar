package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// A1. defaults + validation
// ---------------------------------------------------------------------------

func TestDefaultIsValidAndMatchesSpec(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default() 未通过校验: %v", err)
	}
	// The concrete numbers below are the P3 contract; a change here is a
	// deliberate behaviour change, not a refactor.
	if cfg.Overlay.Size != 96 {
		t.Errorf("overlay.size = %d, 期望 96", cfg.Overlay.Size)
	}
	if cfg.Overlay.Opacity != 0.85 {
		t.Errorf("overlay.opacity = %v, 期望 0.85", cfg.Overlay.Opacity)
	}
	if cfg.Overlay.DurationMs != 1150 || cfg.Overlay.FadeInMs != 60 || cfg.Overlay.FadeOutMs != 250 {
		t.Errorf("时长默认值不对: %d/%d/%d，期望 1150/60/250",
			cfg.Overlay.DurationMs, cfg.Overlay.FadeInMs, cfg.Overlay.FadeOutMs)
	}
	if cfg.Overlay.MaxSimultaneous != 3 || cfg.Overlay.Margin != 24 {
		t.Errorf("maxSimultaneous/margin 默认值不对: %d/%d", cfg.Overlay.MaxSimultaneous, cfg.Overlay.Margin)
	}
	if cfg.Overlay.Anchor != AnchorBottomRight {
		t.Errorf("overlay.anchor = %q, 期望 %q", cfg.Overlay.Anchor, AnchorBottomRight)
	}
	if !cfg.Overlay.ShowName || !cfg.Overlay.ShowScore || !cfg.Overlay.Enabled {
		t.Error("showName/showScore/enabled 应默认为 true")
	}
	if cfg.Hotkeys.ToggleOverlay != "F9" {
		t.Errorf("hotkeys.toggleOverlay = %q, 期望 F9", cfg.Hotkeys.ToggleOverlay)
	}
	if cfg.Profile != "default" {
		t.Errorf("profile = %q, 期望 default", cfg.Profile)
	}
	if !cfg.Capture.FallbackToDefault || cfg.Capture.Device != "" {
		t.Errorf("capture 默认值不对: %+v", cfg.Capture)
	}

	// camelCase field names, as mandated by the spec.
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"fallbackToDefault"`, `"maxSimultaneous"`, `"showName"`, `"showScore"`,
		`"durationMs"`, `"fadeInMs"`, `"fadeOutMs"`, `"toggleOverlay"`, `"anchor"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("默认 JSON 缺少字段名 %s", want)
		}
	}
}

func TestValidateRejectsBadValuesWithChineseErrors(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string // substring the Chinese message must contain
	}{
		{"size 为 0", func(c *Config) { c.Overlay.Size = 0 }, "size"},
		{"size 为负", func(c *Config) { c.Overlay.Size = -96 }, "size"},
		{"size 过大", func(c *Config) { c.Overlay.Size = 4096 }, "size"},
		{"opacity 为 0", func(c *Config) { c.Overlay.Opacity = 0 }, "opacity"},
		{"opacity 大于 1", func(c *Config) { c.Overlay.Opacity = 1.5 }, "opacity"},
		{"opacity 为负", func(c *Config) { c.Overlay.Opacity = -0.1 }, "opacity"},
		{"anchor 拼错", func(c *Config) { c.Overlay.Anchor = "bottom-rightt" }, "anchor"},
		{"anchor 空白", func(c *Config) { c.Overlay.Anchor = "   " }, "anchor"},
		{"hotkey 非法", func(c *Config) { c.Hotkeys.ToggleOverlay = "Ctrl+Foo" }, "toggleOverlay"},
		{"hotkey 空", func(c *Config) { c.Hotkeys.ToggleOverlay = "" }, "toggleOverlay"},
		{"maxSimultaneous 为 0", func(c *Config) { c.Overlay.MaxSimultaneous = 0 }, "maxSimultaneous"},
		{"maxSimultaneous 过大", func(c *Config) { c.Overlay.MaxSimultaneous = 99 }, "maxSimultaneous"},
		{"monitor 为负", func(c *Config) { c.Overlay.Monitor = -1 }, "monitor"},
		{"margin 为负", func(c *Config) { c.Overlay.Margin = -1 }, "margin"},
		{"durationMs 为负", func(c *Config) { c.Overlay.DurationMs = -1 }, "durationMs"},
		{"fadeInMs 为负", func(c *Config) { c.Overlay.FadeInMs = -1 }, "fadeInMs"},
		{"profile 空", func(c *Config) { c.Profile = " " }, "profile"},
		{"三个时长全 0", func(c *Config) { c.Overlay.DurationMs, c.Overlay.FadeInMs, c.Overlay.FadeOutMs = 0, 0, 0 }, "立刻消失"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mut(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("期望报错，实际通过")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", msg, tc.want)
			}
			if !hasHan(msg) {
				t.Errorf("错误信息不是中文: %q", msg)
			}
		})
	}

	// Sanity: the unmodified default must still be fine (guards against a
	// validator that rejects everything).
	if err := Default().Validate(); err != nil {
		t.Fatalf("默认配置被误判为非法: %v", err)
	}
}

// hasHan reports whether s contains at least one CJK ideograph.
func hasHan(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// P4: recall settings
// ---------------------------------------------------------------------------

// TestRecallDefaultsMatchSpec pins the documented P4 defaults. These numbers are
// part of the milestone contract, not arbitrary.
func TestRecallDefaultsMatchSpec(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("默认配置未通过校验: %v", err)
	}
	if !cfg.Recall.Enabled {
		t.Error("recall.enabled 应默认为 true")
	}
	if cfg.Recall.Seconds != 3 {
		t.Errorf("recall.seconds = %d，期望 3", cfg.Recall.Seconds)
	}
	if cfg.Recall.Dir != filepath.Join("data", "candidates") {
		t.Errorf("recall.dir = %q，期望 data/candidates", cfg.Recall.Dir)
	}
	if cfg.Recall.MaxFiles != 200 {
		t.Errorf("recall.maxFiles = %d，期望 200", cfg.Recall.MaxFiles)
	}
	if cfg.Hotkeys.RecallLabel != "F8" {
		t.Errorf("hotkeys.recallLabel = %q，期望 F8", cfg.Hotkeys.RecallLabel)
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"recall"`, `"seconds"`, `"dir"`, `"maxFiles"`, `"recallLabel"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("默认 JSON 缺少字段名 %s", want)
		}
	}
}

// TestValidateRejectsBadRecallValues checks the P4 ranges and the Chinese errors.
func TestValidateRejectsBadRecallValues(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"seconds 为 0", func(c *Config) { c.Recall.Seconds = 0 }, "recall.seconds"},
		{"seconds 为负", func(c *Config) { c.Recall.Seconds = -1 }, "recall.seconds"},
		{"seconds 过大", func(c *Config) { c.Recall.Seconds = 31 }, "recall.seconds"},
		{"seconds 上限 30 合法", func(c *Config) { c.Recall.Seconds = 30 }, ""},
		{"seconds 下限 1 合法", func(c *Config) { c.Recall.Seconds = 1 }, ""},
		{"maxFiles 为 0", func(c *Config) { c.Recall.MaxFiles = 0 }, "recall.maxFiles"},
		{"maxFiles 过大", func(c *Config) { c.Recall.MaxFiles = 5001 }, "recall.maxFiles"},
		{"maxFiles 上限 5000 合法", func(c *Config) { c.Recall.MaxFiles = 5000 }, ""},
		{"dir 为空", func(c *Config) { c.Recall.Dir = "" }, "recall.dir"},
		{"dir 全空白", func(c *Config) { c.Recall.Dir = "   " }, "recall.dir"},
		{"dir 过长", func(c *Config) { c.Recall.Dir = strings.Repeat("a", 513) }, "recall.dir"},
		{"recallLabel 非法", func(c *Config) { c.Hotkeys.RecallLabel = "Ctrl+Nope" }, "recallLabel"},
		{"recallLabel 为空", func(c *Config) { c.Hotkeys.RecallLabel = "" }, "recallLabel"},
		{"recallLabel 可以是 none", func(c *Config) { c.Hotkeys.RecallLabel = "none" }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mut(cfg)
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("期望合法，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望报错 %q，实际通过", tc.want)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", msg, tc.want)
			}
			if !hasHan(msg) {
				t.Errorf("错误信息不是中文: %q", msg)
			}
		})
	}
}

// TestRecallRoundTripThroughFile checks that the new section survives Save/Load
// and that normalization canonicalises the hotkey spelling.
func TestRecallRoundTripThroughFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := Default()
	cfg.Recall = RecallConfig{Enabled: false, Seconds: 7, Dir: filepath.Join(dir, "inbox"), MaxFiles: 42}
	cfg.Hotkeys.RecallLabel = "ctrl-alt-f8" // loose spelling on purpose
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Recall.Enabled || loaded.Recall.Seconds != 7 || loaded.Recall.MaxFiles != 42 {
		t.Fatalf("recall 字段往返失败: %+v", loaded.Recall)
	}
	if loaded.Recall.Dir != filepath.Join(dir, "inbox") {
		t.Fatalf("recall.dir 往返失败: %q", loaded.Recall.Dir)
	}
	if loaded.Hotkeys.RecallLabel != "Ctrl+Alt+F8" {
		t.Fatalf("recallLabel 没有被规范化: %q", loaded.Hotkeys.RecallLabel)
	}
	if loaded.Hotkeys.ToggleOverlay != "F9" {
		t.Fatalf("toggleOverlay 被改动: %q", loaded.Hotkeys.ToggleOverlay)
	}
}

// TestResolveRecallDirResolvesRelativeValues pins the "whose data/candidates?"
// rule: a relative path must become absolute without touching the disk.
func TestResolveRecallDirResolvesRelativeValues(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "somewhere")
	got, err := ResolveRecallDir(abs)
	if err != nil {
		t.Fatalf("ResolveRecallDir(绝对路径): %v", err)
	}
	if got != abs {
		t.Fatalf("绝对路径被改写: %q -> %q", abs, got)
	}

	got, err = ResolveRecallDir("data/candidates")
	if err != nil {
		t.Fatalf("ResolveRecallDir(相对路径): %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("相对路径没有被解析成绝对路径: %q", got)
	}
	if !strings.HasSuffix(got, filepath.Join("data", "candidates")) {
		t.Fatalf("解析结果 %q 结尾不是 data/candidates", got)
	}

	// An empty value falls back to the documented default, not to "".
	got, err = ResolveRecallDir("")
	if err != nil {
		t.Fatalf("ResolveRecallDir(空): %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join("data", "candidates")) {
		t.Fatalf("空值没有回退到默认目录: %q", got)
	}
}

func TestNormalizeAnchorAcceptsLooseSpellings(t *testing.T) {
	cases := map[string]string{
		"bottom-right":   AnchorBottomRight,
		"BottomRight":    AnchorBottomRight,
		"bottom_right":   AnchorBottomRight,
		"BOTTOMRIGHT":    AnchorBottomRight,
		" bottom-right ": AnchorBottomRight,
		"centre":         AnchorCenter,
		"middle":         AnchorCenter,
		"top-centre":     AnchorTopCenter,
		"Left":           AnchorMiddleLeft,
	}
	for in, want := range cases {
		got, ok := NormalizeAnchor(in)
		if !ok || got != want {
			t.Errorf("NormalizeAnchor(%q) = %q,%v，期望 %q,true", in, got, ok, want)
		}
	}
	if _, ok := NormalizeAnchor("diagonal"); ok {
		t.Error("NormalizeAnchor 接受了非法值 diagonal")
	}
	if _, ok := NormalizeAnchor(""); ok {
		t.Error("NormalizeAnchor 接受了空串")
	}
}

func TestParseHotkey(t *testing.T) {
	cases := []struct {
		in   string
		key  string
		vk   uint32
		mods uint32
	}{
		{"F9", "F9", 0x78, ModNoRepeat},
		{"f1", "F1", 0x70, ModNoRepeat},
		{"f24", "F24", 0x87, ModNoRepeat},
		{"Ctrl+Shift+F9", "F9", 0x78, ModControl | ModShift | ModNoRepeat},
		{"ctrl-alt-k", "K", 0x4B, ModControl | ModAlt | ModNoRepeat},
		{"Win+A", "A", 0x41, ModWin | ModNoRepeat},
		{"shift+space", "SPACE", 0x20, ModShift | ModNoRepeat},
		{"PgUp", "PGUP", 0x21, ModNoRepeat},
	}
	for _, tc := range cases {
		hk, err := ParseHotkey(tc.in)
		if err != nil {
			t.Errorf("ParseHotkey(%q) 报错: %v", tc.in, err)
			continue
		}
		if hk.Key != tc.key || hk.VK != tc.vk || hk.Modifiers != tc.mods {
			t.Errorf("ParseHotkey(%q) = %+v，期望 key=%s vk=0x%X mods=0x%X",
				tc.in, hk, tc.key, tc.vk, tc.mods)
		}
	}
	if hk, err := ParseHotkey("none"); err != nil || hk.VK != 0 {
		t.Errorf("ParseHotkey(none) = %+v, %v；期望零值", hk, err)
	}
	for _, bad := range []string{"", "Ctrl", "Ctrl+Foo", "F25", "Ctrl+Ctrl+F9", "Hyper+F9"} {
		if _, err := ParseHotkey(bad); err == nil {
			t.Errorf("ParseHotkey(%q) 应该报错", bad)
		}
	}
	if got := (Hotkey{Modifiers: ModControl | ModNoRepeat, VK: 0x78, Key: "F9"}).String(); got != "Ctrl+F9" {
		t.Errorf("Hotkey.String() = %q，期望 Ctrl+F9", got)
	}
}

// ---------------------------------------------------------------------------
// A2. Save / Load round trip + atomic write
// ---------------------------------------------------------------------------

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := Default()
	cfg.Capture.Device = "Realtek(R) Audio"
	cfg.Capture.FallbackToDefault = false
	cfg.Overlay.X, cfg.Overlay.Y = 1812, 984
	cfg.Overlay.Anchor = AnchorTopCenter
	cfg.Overlay.Size = 128
	cfg.Overlay.Opacity = 0.42
	cfg.Overlay.DurationMs = 900
	cfg.Overlay.FadeInMs = 80
	cfg.Overlay.FadeOutMs = 300
	cfg.Overlay.MaxSimultaneous = 2
	cfg.Overlay.ShowName = false
	cfg.Overlay.ShowScore = false
	cfg.Overlay.Margin = 8
	cfg.Overlay.Monitor = 2
	cfg.Hotkeys.ToggleOverlay = "Ctrl+Alt+F8"
	cfg.Profile = "ace"

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if *got != *cfg {
		t.Errorf("Save→Load 往返不一致:\n got=%+v\nwant=%+v", *got, *cfg)
	}
	// No temporary leftovers.
	left := strayFiles(t, dir)
	if len(left) != 0 {
		t.Errorf("目录里留下了临时文件: %v", left)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "nope.json"))
	if err != nil {
		t.Fatalf("Load(缺失文件) 应返回默认值，却报错: %v", err)
	}
	if *cfg != *Default() {
		t.Errorf("缺失文件时不是默认值: %+v", *cfg)
	}
	if _, err := os.Stat(filepath.Join(dir, "nope.json")); !os.IsNotExist(err) {
		t.Error("Load 不应创建文件")
	}
}

func TestLoadRejectsBrokenAndInvalidFiles(t *testing.T) {
	dir := t.TempDir()

	brokenPath := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(brokenPath, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(brokenPath); err == nil || !hasHan(err.Error()) {
		t.Errorf("Load(坏 JSON) 期望中文错误，实际 %v", err)
	}

	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte(`{"overlay":{"size":4,"opacity":3,"anchor":"nope","maxSimultaneous":0}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(badPath)
	if err == nil {
		t.Fatal("Load(非法值) 应该报错")
	}
	if !hasHan(err.Error()) || !strings.Contains(err.Error(), badPath) {
		t.Errorf("错误信息应含中文与文件路径: %v", err)
	}

	// A file that omits every field must still load: Default() supplies the
	// whole document, so an omitted hotkey keeps its default (F9). Only an
	// EXPLICITLY empty value is turned into the "none" spelling.
	emptyPath := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(emptyPath)
	if err != nil {
		t.Fatalf("Load({}) 应成功: %v", err)
	}
	if cfg.Hotkeys.ToggleOverlay != "F9" {
		t.Errorf("Load({}) 的 hotkey = %q，期望默认 F9", cfg.Hotkeys.ToggleOverlay)
	}
	if cfg.Overlay.Anchor != AnchorBottomRight || cfg.Overlay.Size != 96 || cfg.Profile != "default" {
		t.Errorf("Load({}) 未补齐默认值: anchor=%q size=%d profile=%q",
			cfg.Overlay.Anchor, cfg.Overlay.Size, cfg.Profile)
	}

	nonePath := filepath.Join(dir, "none.json")
	if err := os.WriteFile(nonePath, []byte(`{"hotkeys":{"toggleOverlay":""}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	none, err := Load(nonePath)
	if err != nil {
		t.Fatalf("Load(空 hotkey) 应成功: %v", err)
	}
	if none.Hotkeys.ToggleOverlay != "none" {
		t.Errorf("显式空 hotkey 应规范成 none，实际 %q", none.Hotkeys.ToggleOverlay)
	}
	if none.Overlay.Anchor != AnchorBottomRight || none.Overlay.Size != 96 {
		t.Errorf("Load(空 hotkey) 未补齐其余默认值: %+v", none.Overlay)
	}

	// A file that only spells things loosely is accepted and canonicalised.
	mixedPath := filepath.Join(dir, "mixed.json")
	if err := os.WriteFile(mixedPath, []byte(`{"overlay":{"anchor":"TopCenter"},"hotkeys":{"toggleOverlay":"ctrl-alt-k"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mixed, err := Load(mixedPath)
	if err != nil {
		t.Fatalf("Load(宽松拼写) 应成功: %v", err)
	}
	if mixed.Overlay.Anchor != AnchorTopCenter || mixed.Hotkeys.ToggleOverlay != "Ctrl+Alt+K" {
		t.Errorf("宽松拼写未规范化: anchor=%q hotkey=%q",
			mixed.Overlay.Anchor, mixed.Hotkeys.ToggleOverlay)
	}
}

func TestSaveIsAtomicAndKeepsOldFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	good := Default()
	good.Profile = "first"
	if err := good.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 1. An invalid config is refused BEFORE any file is touched.
	bad := Default()
	bad.Overlay.Opacity = 7
	if err := bad.Save(path); err == nil {
		t.Fatal("Save(非法配置) 应该报错")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("失败后旧文件不见了: %v", err)
	}
	if string(before) != string(after) {
		t.Error("保存失败却改动了旧文件")
	}

	// 2. A rename that cannot succeed leaves no temporary file behind. The
	//    destination is made a DIRECTORY, which MoveFileExW refuses to replace.
	blocked := filepath.Join(dir, "blocked.json")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	if err := cfg.Save(blocked); err == nil {
		t.Fatal("Save(目标是目录) 应该报错")
	}
	if left := strayFiles(t, dir); len(left) != 0 {
		t.Errorf("写失败后留下了临时文件: %v", left)
	}
	if st, err := os.Stat(blocked); err != nil || !st.IsDir() {
		t.Error("写失败破坏了目标路径")
	}

	// 3. A successful overwrite really replaces the content (this is the
	//    MoveFileExW(MOVEFILE_REPLACE_EXISTING) path: os.Rename would fail here
	//    because the destination already exists).
	good2 := Default()
	good2.Profile = "second"
	if err := good2.Save(path); err != nil {
		t.Fatalf("覆盖保存失败: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Profile != "second" {
		t.Errorf("覆盖保存后 profile = %q，期望 second", reloaded.Profile)
	}
	if left := strayFiles(t, dir); len(left) != 0 {
		t.Errorf("覆盖保存后留下了临时文件: %v", left)
	}
}

// strayFiles lists every entry of dir except config.json / blocked.json.
func strayFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range ents {
		if e.Name() == "config.json" || e.Name() == "blocked.json" {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

func TestDefaultPathIsResolvable(t *testing.T) {
	p, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if !strings.HasSuffix(p, FileName) {
		t.Errorf("DefaultPath = %q，应以 %s 结尾", p, FileName)
	}
	info, err := ResolvePath()
	if err != nil {
		t.Fatal(err)
	}
	if info.Source != "exe" && info.Source != "cwd" {
		t.Errorf("ResolvePath.Source = %q", info.Source)
	}
	if info.Path != p {
		t.Errorf("DefaultPath 与 ResolvePath 不一致: %q vs %q", p, info.Path)
	}
	// The chosen directory must be usable, otherwise Save would be a lie.
	if !info.Writable {
		t.Errorf("选中的目录 %s 不可写", filepath.Dir(p))
	}
}
