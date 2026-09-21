// Package config implements the soundradar P3 application settings: the small
// JSON document that lives next to the executable (or, when that directory is
// not writable, in the current working directory) and that the overlay, the
// capture endpoint and the settings page all read.
//
// It is deliberately separate from internal/library: the library is the
// *content* (sound effects + samples + icons) while this file is the *machine
// preference* (which speaker to listen to, where the overlay sits, which
// hotkey toggles it). Losing config.json is annoying; losing library.srz is
// data loss - they must not share a file.
//
// The package is pure Go (no Win32), which is what makes the whole geometry
// half of the overlay unit-testable: ResolvePosition and AnchorPosition contain
// every coordinate decision, and internal/overlay only feeds them rectangles it
// obtained from user32.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FileName is the settings file name, looked up next to the executable first.
const FileName = "config.json"

// Anchor values accepted by OverlayConfig.Anchor. The canonical spelling is
// kebab-case; NormalizeAnchor also accepts underscores and compact spellings
// ("bottomright", "BottomRight") so a hand-edited file keeps working.
const (
	AnchorTopLeft      = "top-left"
	AnchorTopCenter    = "top-center"
	AnchorTopRight     = "top-right"
	AnchorMiddleLeft   = "middle-left"
	AnchorCenter       = "center"
	AnchorMiddleRight  = "middle-right"
	AnchorBottomLeft   = "bottom-left"
	AnchorBottomCenter = "bottom-center"
	AnchorBottomRight  = "bottom-right"
)

// Anchors lists every valid anchor, in the grid order used by the settings
// page's nine buttons.
var Anchors = []string{
	AnchorTopLeft, AnchorTopCenter, AnchorTopRight,
	AnchorMiddleLeft, AnchorCenter, AnchorMiddleRight,
	AnchorBottomLeft, AnchorBottomCenter, AnchorBottomRight,
}

// CaptureConfig selects the WASAPI render endpoint to listen to.
type CaptureConfig struct {
	// Device is a case-insensitive substring of the endpoint friendly name; an
	// empty string means "the system default render endpoint".
	Device string `json:"device"`
	// FallbackToDefault allows the capture to silently use the default endpoint
	// when Device matches nothing (instead of failing to start).
	FallbackToDefault bool `json:"fallbackToDefault"`
}

// OverlayConfig configures the hit overlay window.
//
// Units: X, Y, Size and Margin are PHYSICAL pixels (DPI-aware process), so a
// value means the same thing on a 100% and a 200% monitor. DurationMs, FadeInMs
// and FadeOutMs are milliseconds. Opacity is 0..1.
type OverlayConfig struct {
	Enabled bool `json:"enabled"`
	// Monitor is the index into the monitor list reported by the overlay
	// (0 = primary). It only matters for anchored placement.
	Monitor int `json:"monitor"`
	// X and Y are absolute physical screen coordinates. When either is
	// non-zero the anchor is ignored and the window is placed there (negative
	// values are legal: a monitor to the left of the primary has x < 0).
	X int `json:"x"`
	Y int `json:"y"`
	// Anchor is the nine-grid placement used when X and Y are both 0.
	Anchor string `json:"anchor"`
	// Size is the height (and nominal width) of ONE event cell in physical
	// pixels; the real canvas is Size*MaxSimultaneous plus padding.
	Size int `json:"size"`
	// Opacity is the global alpha of the whole canvas, 0..1.
	Opacity float64 `json:"opacity"`
	// DurationMs is how long one event stays fully visible (after the fade-in,
	// before the fade-out).
	DurationMs int `json:"durationMs"`
	FadeInMs   int `json:"fadeInMs"`
	FadeOutMs  int `json:"fadeOutMs"`
	// MaxSimultaneous caps how many hits share the overlay; the oldest is
	// dropped when the queue is full.
	MaxSimultaneous int  `json:"maxSimultaneous"`
	ShowName        bool `json:"showName"`
	ShowScore       bool `json:"showScore"`
	// ShowAll lays every grid hint out as its own card (picture, grid size,
	// name), matching the live "识别对照" panel. Off keeps the compact one-line hit.
	ShowAll bool `json:"showAll"`
	// Margin is the gap to the work-area edge in anchored mode (physical px).
	Margin int `json:"margin"`
}

// HotkeyConfig holds the global hotkeys. Format: optional "Ctrl+"/"Alt+"/
// "Shift+"/"Win+" prefixes (in any order) followed by one key: F1..F24,
// 0..9/A..Z, or the names Space/Esc/Enter/Tab/Insert/Delete/Home/End/PgUp/PgDn.
//
// "none" disables a hotkey without failing validation.
type HotkeyConfig struct {
	// ToggleOverlay shows/hides the P3 hit overlay (default F9).
	ToggleOverlay string `json:"toggleOverlay"`
	// RecallLabel (P4) saves the last Recall.Seconds seconds of audio as a
	// candidate (default F8). It is registered as a real Win32 global hotkey;
	// the tool never synthesises key presses.
	RecallLabel string `json:"recallLabel"`
}

// RecallConfig is the P4 "save what I just heard" settings.
type RecallConfig struct {
	// Enabled turns the recall hotkey on for `overlay` / `serve` / `live`.
	Enabled bool `json:"enabled"`
	// Seconds is the length of the ring buffer, i.e. how much audio one hotkey
	// press saves (1..30, default 3).
	Seconds int `json:"seconds"`
	// Dir is the candidate inbox. A relative path is resolved against the
	// executable directory first (like library.srz), then the working directory.
	Dir string `json:"dir"`
	// MaxFiles caps the inbox; the oldest candidates are pruned first (1..5000).
	MaxFiles int `json:"maxFiles"`
}

// NoiseConfig is the environment-noise filter used by the recognition pipeline.
//
// It exists because the same sound heard through game ambience does not look
// like the same sound recorded in a quiet moment: the ambience lifts the quiet
// mel bands and the cosine score drops. The filter tracks a per-bin noise
// floor (skipping tones and short events, learning a ~200 ms broadband scene
// change), attenuates every frequency bin by its own signal-to-noise ratio,
// and can raise the silence gate above the measured noise floor.
//
// These values are part of the FINGERPRINT (see internal/dsp): changing any of
// them changes every stored vector, so the index is rebuilt automatically the
// next time recognition starts. That is why they live in config.json rather
// than being fixed constants.
//
// A zero value means "use the recommended default", so an existing config.json
// without a "noise" section keeps working.
type NoiseConfig struct {
	// Method is "subtract" (high-pass + spectral gain, the default),
	// "highpass" (only the high-pass, cheapest) or "off".
	Method string `json:"method"`
	// HighPassHz is the high-pass corner. It removes rumble and wind, which
	// live below the lowest mel band anyway. Default 120.
	HighPassHz float64 `json:"highPassHz"`
	// Strength is the over-subtraction factor: how much of the estimated noise
	// power is assumed to be noise. Larger means stronger suppression and a
	// slightly more altered sound. Default 1.5.
	Strength float64 `json:"strength"`
	// GainFloorDB bounds how far a single frequency bin may be attenuated
	// (default -8 dB).
	GainFloorDB float64 `json:"gainFloorDb"`
	// AdaptiveGate raises the silence gate above the measured noise floor, so a
	// noisy game stops scoring empty windows. Written explicitly (not omitempty)
	// so an intentional false survives Save→Load.
	AdaptiveGate bool `json:"adaptiveGate"`
	// GateMarginDB is how far above the noise floor that gate sits. Default 4.
	GateMarginDB float64 `json:"gateMarginDb"`
	// GateFloorDBFS is the lowest value the adaptive gate may take, so a quiet
	// environment keeps the configured gate. Default -70.
	GateFloorDBFS float64 `json:"gateFloorDbfs"`
}

// Config is the whole document.
type Config struct {
	Capture CaptureConfig `json:"capture"`
	Overlay OverlayConfig `json:"overlay"`
	Hotkeys HotkeyConfig  `json:"hotkeys"`
	Recall  RecallConfig  `json:"recall"`
	Noise   NoiseConfig   `json:"noise"`
	Profile string        `json:"profile"`
}

// Default returns the documented defaults. Note the concrete numbers: they are
// part of the P3 spec, not arbitrary.
func Default() *Config {
	return &Config{
		Capture: CaptureConfig{Device: "", FallbackToDefault: true},
		Overlay: OverlayConfig{
			Enabled:         true,
			Monitor:         0,
			X:               0,
			Y:               0,
			Anchor:          AnchorBottomRight,
			Size:            96,
			Opacity:         0.85,
			DurationMs:      1150,
			FadeInMs:        60,
			FadeOutMs:       250,
			MaxSimultaneous: 3,
			ShowName:        true,
			ShowScore:       true,
			ShowAll:         false,
			Margin:          24,
		},
		Hotkeys: HotkeyConfig{ToggleOverlay: "F9", RecallLabel: "F8"},
		Recall: RecallConfig{
			Enabled:  true,
			Seconds:  3,
			Dir:      filepath.Join("data", "candidates"),
			MaxFiles: 200,
		},
		Noise:   DefaultNoiseConfig(),
		Profile: "default",
	}
}

// DefaultNoiseConfig returns the recommended environment-noise settings. It
// mirrors dsp.DefaultNoiseParams; the tests keep the two in step.
func DefaultNoiseConfig() NoiseConfig {
	return NoiseConfig{
		Method:        "subtract",
		HighPassHz:    120,
		Strength:      1.5,
		GainFloorDB:   -8,
		AdaptiveGate:  true,
		GateMarginDB:  4,
		GateFloorDBFS: -70,
	}
}

// ---------------------------------------------------------------------------
// path resolution
// ---------------------------------------------------------------------------

// PathInfo explains which path DefaultPath picked and why.
type PathInfo struct {
	// Path is the file that Load/Save use.
	Path string `json:"path"`
	// Source is "exe" or "cwd".
	Source string `json:"source"`
	// ExeDir and Cwd are the two candidates that were considered.
	ExeDir string `json:"exeDir"`
	Cwd    string `json:"cwd"`
	// Exists reports whether Path is already on disk.
	Exists bool `json:"exists"`
	// Writable reports whether Path's directory accepted a probe file.
	Writable bool `json:"writable"`
}

// DefaultPath returns the settings path: <exe dir>\config.json when that
// directory is writable, otherwise <cwd>\config.json.
//
// The executable directory is probed (not merely inspected) because that is
// what actually matters: a Program Files install is readable but not writable,
// and silently failing to save settings there would be worse than storing them
// in the working directory.
func DefaultPath() (string, error) {
	info, err := ResolvePath()
	if err != nil {
		return "", err
	}
	return info.Path, nil
}

// ResolvePath is DefaultPath plus the reasoning behind the choice.
func ResolvePath() (PathInfo, error) {
	var info PathInfo
	wd, _ := os.Getwd()
	info.Cwd = wd

	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		info.ExeDir = dir
		if isWritableDir(dir) {
			p := filepath.Join(dir, FileName)
			info.Path, info.Source = p, "exe"
			info.Writable = true
			info.Exists = fileExists(p)
			return info, nil
		}
	}
	if wd == "" {
		return info, errors.New("无法确定配置文件路径：既拿不到 exe 目录也拿不到当前目录")
	}
	info.Path, info.Source = filepath.Join(wd, FileName), "cwd"
	info.Writable = isWritableDir(wd)
	info.Exists = fileExists(info.Path)
	return info, nil
}

// fileExists is a tiny helper so the resolvers below share one spelling.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func finishPathInfo(info *PathInfo) {
	if _, err := os.Stat(info.Path); err == nil {
		info.Exists = true
	}
}

// ResolveRecallDir turns the configured candidate inbox into an absolute path.
//
// A relative value (the documented default is "data/candidates") is resolved
// against the executable directory when that directory is writable, and against
// the working directory otherwise - the same rule internal/library uses for
// library.srz, so `serve`, `overlay` and `recall` always agree on one inbox.
// It never touches the disk.
func ResolveRecallDir(dir string) (string, error) {
	d := strings.TrimSpace(dir)
	if d == "" {
		d = Default().Recall.Dir
	}
	if filepath.IsAbs(d) {
		return filepath.Clean(d), nil
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		if isWritableDir(exeDir) {
			return filepath.Join(exeDir, d), nil
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("无法确定候选项目录（既拿不到 exe 目录也拿不到当前目录）: %w", err)
	}
	return filepath.Join(wd, d), nil
}

// isWritableDir reports whether dir exists and accepts a temporary file.
func isWritableDir(dir string) bool {
	if dir == "" {
		return false
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return false
	}
	f, err := os.CreateTemp(dir, ".cfgprobe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// ---------------------------------------------------------------------------
// load / save
// ---------------------------------------------------------------------------

// Load reads the document at path. A missing file is not an error: the
// defaults are returned (and reported through PathInfo by the caller).
func Load(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("配置文件路径为空")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	cfg := Default()
	// Unknown fields are ignored on purpose: a config written by a newer build
	// must not brick an older binary.
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败（不是合法 JSON）: %w", path, err)
	}
	normalize(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置文件 %s 不合法: %w", path, err)
	}
	return cfg, nil
}

// LoadDefault loads the resolved default path (or the defaults when there is
// no file yet).
func LoadDefault() (*Config, PathInfo, error) {
	info, err := ResolvePath()
	if err != nil {
		return nil, info, err
	}
	cfg, err := Load(info.Path)
	return cfg, info, err
}

// Save writes the document atomically: a temporary file in the same directory
// is flushed and then swapped in with MoveFileExW(MOVEFILE_REPLACE_EXISTING)
// (see atomic_windows.go), so an interrupted save can never leave a truncated
// or half-written config.json behind. Nothing is validated here implicitly -
// call Validate first (Save does it for you and refuses invalid values).
func (c *Config) Save(path string) error {
	if c == nil {
		return errors.New("配置为空")
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("配置文件路径为空")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建配置目录 %s 失败: %w", dir, err)
	}

	// MarshalIndent keeps the file readable; the trailing newline keeps text
	// tools (and git) happy.
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	body = append(body, '\n')

	tmp, err := os.CreateTemp(dir, ".config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置文件失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(body); err != nil {
		cleanup()
		return fmt.Errorf("写入临时配置文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("同步临时配置文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("关闭临时配置文件失败: %w", err)
	}
	if err := replaceFile(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("替换配置文件 %s 失败: %w", path, err)
	}
	syncDir(dir)
	return nil
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

// Validate reports the first invalid value as a Chinese, user-facing error.
// The message names the JSON field so the settings page can show it verbatim.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("配置为空")
	}
	if err := c.Overlay.Validate(); err != nil {
		return err
	}
	if len([]rune(c.Capture.Device)) > 512 {
		return errors.New("capture.device 名称过长（最多 512 个字符）")
	}
	if strings.TrimSpace(c.Profile) == "" {
		return errors.New("profile 不能为空")
	}
	if len([]rune(c.Profile)) > 64 {
		return errors.New("profile 过长（最多 64 个字符）")
	}
	if len([]rune(c.Hotkeys.ToggleOverlay)) > 0 {
		if _, err := ParseHotkey(c.Hotkeys.ToggleOverlay); err != nil {
			return fmt.Errorf("hotkeys.toggleOverlay 非法: %w", err)
		}
	} else {
		return errors.New("hotkeys.toggleOverlay 不能为空（想禁用请写 none）")
	}
	if len([]rune(c.Hotkeys.RecallLabel)) > 0 {
		if _, err := ParseHotkey(c.Hotkeys.RecallLabel); err != nil {
			return fmt.Errorf("hotkeys.recallLabel 非法: %w", err)
		}
	} else {
		return errors.New("hotkeys.recallLabel 不能为空（想禁用请写 none）")
	}

	// --- P4 recall -------------------------------------------------------
	r := c.Recall
	switch {
	case r.Seconds < 1:
		return fmt.Errorf("recall.seconds 至少为 1，当前 %d", r.Seconds)
	case r.Seconds > 30:
		return fmt.Errorf("recall.seconds 最多 30（秒），当前 %d", r.Seconds)
	}
	switch {
	case r.MaxFiles < 1:
		return fmt.Errorf("recall.maxFiles 至少为 1，当前 %d", r.MaxFiles)
	case r.MaxFiles > 5000:
		return fmt.Errorf("recall.maxFiles 最多 5000，当前 %d", r.MaxFiles)
	}
	if strings.TrimSpace(r.Dir) == "" {
		return errors.New("recall.dir 不能为空（候选项保存目录）")
	}
	if len([]rune(r.Dir)) > 512 {
		return errors.New("recall.dir 过长（最多 512 个字符）")
	}
	if strings.ContainsAny(r.Dir, "\x00") {
		return errors.New("recall.dir 含非法字符")
	}

	// --- noise -----------------------------------------------------------
	n := normalizedNoise(c.Noise)
	if _, ok := NormalizeNoiseMethod(n.Method); !ok && strings.TrimSpace(n.Method) != "" {
		return fmt.Errorf("noise.method 非法: %q（可选: %s）", n.Method, strings.Join(NoiseMethods, ", "))
	}
	if n.HighPassHz < 0 || n.HighPassHz > 4000 {
		return fmt.Errorf("noise.highPassHz 应在 0–4000，当前 %v", n.HighPassHz)
	}
	if n.Strength < 0.5 || n.Strength > 6 {
		return fmt.Errorf("noise.strength 应在 0.5–6，当前 %v", n.Strength)
	}
	if n.GainFloorDB > -1 || n.GainFloorDB < -40 {
		return fmt.Errorf("noise.gainFloorDb 应在 -40–-1，当前 %v", n.GainFloorDB)
	}
	if n.GateMarginDB < 0 || n.GateMarginDB > 30 {
		return fmt.Errorf("noise.gateMarginDb 应在 0–30，当前 %v", n.GateMarginDB)
	}
	if n.GateFloorDBFS > -20 || n.GateFloorDBFS < -100 {
		return fmt.Errorf("noise.gateFloorDbfs 应在 -100–-20，当前 %v", n.GateFloorDBFS)
	}
	return nil
}

// Validate checks only the overlay section. The live overlay applies this on
// its own, without a full config document, so it must not require hotkeys or
// recall fields that are not part of the window.
func (o OverlayConfig) Validate() error {
	if len([]rune(o.Anchor)) > 0 {
		if _, ok := NormalizeAnchor(o.Anchor); !ok {
			return fmt.Errorf("overlay.anchor 取值非法: %q（可选值: %s）",
				o.Anchor, strings.Join(Anchors, ", "))
		}
	} else if o.Anchor != "" {
		return errors.New("overlay.anchor 不能是空白字符串")
	}

	switch {
	case o.Size <= 0:
		return fmt.Errorf("overlay.size 必须大于 0，当前 %d", o.Size)
	case o.Size < 16:
		return fmt.Errorf("overlay.size 太小（%d），至少 16 个像素", o.Size)
	case o.Size > 1024:
		return fmt.Errorf("overlay.size 太大（%d），最多 1024 个像素", o.Size)
	}
	if math.IsNaN(o.Opacity) || math.IsInf(o.Opacity, 0) {
		return fmt.Errorf("overlay.opacity 必须是有限数字，当前 %v", o.Opacity)
	}
	if o.Opacity <= 0 || o.Opacity > 1 {
		return fmt.Errorf("overlay.opacity 必须在 0（不含）到 1 之间，当前 %v", o.Opacity)
	}
	if o.MaxSimultaneous < 1 {
		return fmt.Errorf("overlay.maxSimultaneous 至少为 1，当前 %d", o.MaxSimultaneous)
	}
	if o.MaxSimultaneous > 12 {
		return fmt.Errorf("overlay.maxSimultaneous 最多 12，当前 %d", o.MaxSimultaneous)
	}
	if o.DurationMs < 0 {
		return fmt.Errorf("overlay.durationMs 不能为负，当前 %d", o.DurationMs)
	}
	if o.DurationMs > 60000 {
		return fmt.Errorf("overlay.durationMs 最多 60000 毫秒，当前 %d", o.DurationMs)
	}
	if o.FadeInMs < 0 {
		return fmt.Errorf("overlay.fadeInMs 不能为负，当前 %d", o.FadeInMs)
	}
	if o.FadeOutMs < 0 {
		return fmt.Errorf("overlay.fadeOutMs 不能为负，当前 %d", o.FadeOutMs)
	}
	if o.FadeInMs > 10000 {
		return fmt.Errorf("overlay.fadeInMs 最多 10000 毫秒，当前 %d", o.FadeInMs)
	}
	if o.FadeOutMs > 10000 {
		return fmt.Errorf("overlay.fadeOutMs 最多 10000 毫秒，当前 %d", o.FadeOutMs)
	}
	if o.DurationMs+o.FadeInMs+o.FadeOutMs <= 0 {
		return errors.New("overlay.durationMs + fadeInMs + fadeOutMs 不能全为 0（那样事件会立刻消失）")
	}
	if o.Monitor < 0 {
		return fmt.Errorf("overlay.monitor 不能为负，当前 %d", o.Monitor)
	}
	if o.Margin < 0 {
		return fmt.Errorf("overlay.margin 不能为负，当前 %d", o.Margin)
	}
	if o.Margin > 4096 {
		return fmt.Errorf("overlay.margin 最多 4096 像素，当前 %d", o.Margin)
	}
	if abs(o.X) > 1000000 || abs(o.Y) > 1000000 {
		return fmt.Errorf("overlay.x/y 超出合理范围（|x|=%d, |y|=%d）", abs(o.X), abs(o.Y))
	}
	return nil
}

// normalize canonicalises the loose spellings a hand-edited file may contain.
// It never rejects a value: an unusable one is replaced by the default and
// Validate reports whatever is still wrong.
func normalize(c *Config) {
	if c == nil {
		return
	}
	if a, ok := NormalizeAnchor(c.Overlay.Anchor); ok {
		c.Overlay.Anchor = a
	} else if strings.TrimSpace(c.Overlay.Anchor) == "" {
		// A hand-written file that omits anchor.Anchor means "the default".
		c.Overlay.Anchor = AnchorBottomRight
	}
	if c.Hotkeys.ToggleOverlay != "" {
		if s, err := ParseHotkey(c.Hotkeys.ToggleOverlay); err == nil {
			c.Hotkeys.ToggleOverlay = s.String()
		}
	}
	if c.Hotkeys.RecallLabel != "" {
		if s, err := ParseHotkey(c.Hotkeys.RecallLabel); err == nil {
			c.Hotkeys.RecallLabel = s.String()
		}
	}
	if strings.TrimSpace(c.Profile) == "" {
		c.Profile = "default"
	}
	if strings.TrimSpace(c.Hotkeys.ToggleOverlay) == "" {
		c.Hotkeys.ToggleOverlay = "none"
	}
	if strings.TrimSpace(c.Hotkeys.RecallLabel) == "" {
		c.Hotkeys.RecallLabel = "none"
	}
	if strings.TrimSpace(c.Recall.Dir) == "" {
		c.Recall.Dir = filepath.Join("data", "candidates")
	}
	// Noise: an absent section or an empty method means "recommended default",
	// so a config.json written before this feature keeps working.
	c.Noise = normalizedNoise(c.Noise)
}

// NoiseMethods lists every accepted value of NoiseConfig.Method, in the order
// the settings page shows them.
var NoiseMethods = []string{"subtract", "highpass", "off"}

// NormalizeNoiseMethod maps every accepted spelling onto the canonical value. It
// reports false when the value is not a method at all.
func NormalizeNoiseMethod(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", false
	case "subtract", "spectral", "noise", "sub", "on", "true", "1":
		return "subtract", true
	case "highpass", "high-pass", "high_pass", "hp":
		return "highpass", true
	case "off", "none", "false", "0":
		return "off", true
	}
	return "", false
}

// normalizedNoise fills in every unset field of a hand-written noise section.
func normalizedNoise(n NoiseConfig) NoiseConfig {
	d := DefaultNoiseConfig()
	if m, ok := NormalizeNoiseMethod(n.Method); ok {
		n.Method = m
	} else {
		n.Method = d.Method
	}
	if n.HighPassHz == 0 {
		n.HighPassHz = d.HighPassHz
	}
	if n.Strength == 0 {
		n.Strength = d.Strength
	}
	if n.GainFloorDB == 0 {
		n.GainFloorDB = d.GainFloorDB
	}
	if n.GateMarginDB == 0 {
		n.GateMarginDB = d.GateMarginDB
	}
	if n.GateFloorDBFS == 0 {
		n.GateFloorDBFS = d.GateFloorDBFS
	}
	return n
}

// Noise docs: the recognition pipeline reads these.
//
// DSPMethod returns the fingerprint method name for this configuration, plus
// whether the spectral stage is on. The mapping lives here (and not in the
// caller) so the settings file and the recognition pipeline cannot disagree.
func (n NoiseConfig) DSPMethod() (method string, spectral bool) {
	m, ok := NormalizeNoiseMethod(n.Method)
	if !ok {
		m = DefaultNoiseConfig().Method
	}
	return m, m == "subtract"
}

// AdaptiveEnabled reports whether the adaptive silence gate is on.
func (n NoiseConfig) AdaptiveEnabled() bool { return n.AdaptiveGate }

// Pipeline describes this configuration in the vocabulary the recognition
// pipeline uses. It is a plain struct rather than a dsp.NoiseParams so that this
// package keeps no dependency on internal/dsp (the command layer does the
// one-line mapping, and the mapping is covered by a test).
type Pipeline struct {
	// Spectral reports whether the per-bin spectral gain runs. Method keeps
	// "highpass" and "off" apart, which both have it off.
	Spectral bool
	// Method is the canonical dsp method name: "subtract", "highpass" or "off".
	Method string
	// HighPassHz, Strength and GainFloorDB are the spectral settings.
	HighPassHz  float64
	Strength    float64
	GainFloorDB float64
	// AdaptiveGate, GateMarginDB and GateFloorDBFS configure the silence gate.
	AdaptiveGate  bool
	GateMarginDB  float64
	GateFloorDBFS float64
}

// Pipeline returns the recognition-pipeline view of this configuration.
func (n NoiseConfig) Pipeline() Pipeline {
	method, spectral := n.DSPMethod()
	return Pipeline{
		Method:        method,
		Spectral:      spectral,
		HighPassHz:    n.HighPassHz,
		Strength:      n.Strength,
		GainFloorDB:   n.GainFloorDB,
		AdaptiveGate:  n.AdaptiveEnabled(),
		GateMarginDB:  n.GateMarginDB,
		GateFloorDBFS: n.GateFloorDBFS,
	}
}

// dsp.Noise* method names, mirrored so callers do not need their own mapping.
// They must stay equal to dsp.NoiseOff / NoiseHighPass / NoiseSpectral; the
// TestNoiseMethodNamesMatchDSP test in the command package enforces that.
const (
	// NoiseMethodOff disables the front-end.
	NoiseMethodOff = "off"
	// NoiseMethodHighPass runs only the biquad high-pass.
	NoiseMethodHighPass = "highpass"
	// NoiseMethodSpectral runs the high-pass plus the per-bin spectral gain.
	NoiseMethodSpectral = "subtract"
)

// Normalize returns a copy with canonical spellings (anchor/hotkey).
func (c *Config) Normalize() *Config {
	if c == nil {
		return Default()
	}
	out := *c
	normalize(&out)
	return &out
}

// ---------------------------------------------------------------------------
// helpers used by validation and by the geometry below
// ---------------------------------------------------------------------------

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// NormalizeAnchor maps every accepted spelling onto the canonical kebab-case
// value. It reports false when the value is not an anchor at all.
func NormalizeAnchor(s string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.ReplaceAll(t, "_", "-")
	t = strings.ReplaceAll(t, " ", "-")
	switch t {
	case "":
		return "", false
	case "topleft", "top-left", "left-top":
		return AnchorTopLeft, true
	case "topcenter", "top-center", "top-centre", "center-top", "centre-top":
		return AnchorTopCenter, true
	case "topright", "top-right", "right-top":
		return AnchorTopRight, true
	case "middleleft", "middle-left", "center-left", "centre-left", "left":
		return AnchorMiddleLeft, true
	case "center", "centre", "middle", "middle-center", "middle-centre":
		return AnchorCenter, true
	case "middleright", "middle-right", "center-right", "centre-right", "right":
		return AnchorMiddleRight, true
	case "bottomleft", "bottom-left", "left-bottom":
		return AnchorBottomLeft, true
	case "bottomcenter", "bottom-center", "bottom-centre", "center-bottom", "centre-bottom":
		return AnchorBottomCenter, true
	case "bottomright", "bottom-right", "right-bottom":
		return AnchorBottomRight, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// hotkey parsing (no Win32: it produces the modifier mask + virtual key that
// RegisterHotKey needs, but stays testable on its own)
// ---------------------------------------------------------------------------

// HotkeyModifiers mirrors the MOD_* flags of RegisterHotKey.
const (
	ModAlt      = 0x0001
	ModControl  = 0x0002
	ModShift    = 0x0004
	ModWin      = 0x0008
	ModNoRepeat = 0x4000
)

// Hotkey is a parsed global hotkey.
type Hotkey struct {
	Modifiers uint32 // MOD_ALT | MOD_CONTROL | MOD_SHIFT | MOD_WIN | MOD_NOREPEAT
	VK        uint32 // virtual-key code
	Key       string // canonical key name, e.g. "F9" or "A"
}

// ParseHotkey parses "F9", "Ctrl+Shift+F9", "ctrl-alt-k", "none".
// The special value "none" parses to the zero Hotkey (VK 0), which callers use
// to mean "do not register anything".
func ParseHotkey(s string) (Hotkey, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Hotkey{}, errors.New("热键为空")
	}
	if strings.EqualFold(raw, "none") || raw == "无" || raw == "关闭" {
		return Hotkey{}, nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == '+' || r == '-' || r == ' ' })
	if len(parts) == 0 {
		return Hotkey{}, fmt.Errorf("无法解析热键 %q", s)
	}
	key := parts[len(parts)-1]
	hk := Hotkey{Modifiers: ModNoRepeat}
	seen := map[uint32]bool{}
	for _, mod := range parts[:len(parts)-1] {
		var m uint32
		switch strings.ToLower(strings.TrimSpace(mod)) {
		case "ctrl", "control", "ctl":
			m = ModControl
		case "alt":
			m = ModAlt
		case "shift":
			m = ModShift
		case "win", "super", "meta", "cmd":
			m = ModWin
		default:
			return Hotkey{}, fmt.Errorf("无法识别的修饰键 %q（支持 Ctrl / Alt / Shift / Win）", mod)
		}
		if seen[m] {
			return Hotkey{}, fmt.Errorf("修饰键 %q 重复", mod)
		}
		seen[m] = true
		hk.Modifiers |= m
	}

	name, vk, err := parseKeyName(key)
	if err != nil {
		return Hotkey{}, err
	}
	hk.Key, hk.VK = name, vk
	return hk, nil
}

// parseKeyName maps a single key token to a virtual-key code.
func parseKeyName(key string) (string, uint32, error) {
	k := strings.TrimSpace(key)
	up := strings.ToUpper(k)

	// Function keys F1..F24.
	if strings.HasPrefix(up, "F") {
		if n, err := strconv.Atoi(up[1:]); err == nil && n >= 1 && n <= 24 {
			return fmt.Sprintf("F%d", n), uint32(0x70 + n - 1), nil
		}
	}
	if len([]rune(up)) == 1 {
		r := []rune(up)[0]
		switch {
		case r >= 'A' && r <= 'Z':
			return string(r), uint32(r), nil
		case r >= '0' && r <= '9':
			return string(r), uint32(r), nil
		}
	}
	named := map[string]uint32{
		"SPACE": 0x20, "ESC": 0x1B, "ESCAPE": 0x1B, "ENTER": 0x0D, "RETURN": 0x0D,
		"TAB": 0x09, "BACKSPACE": 0x08, "INSERT": 0x2D, "INS": 0x2D, "DELETE": 0x2E,
		"DEL": 0x2E, "HOME": 0x24, "END": 0x23, "PGUP": 0x21, "PAGEUP": 0x21,
		"PGDN": 0x22, "PAGEDOWN": 0x22, "UP": 0x26, "DOWN": 0x28, "LEFT": 0x25, "RIGHT": 0x27,
		"NUMPAD0": 0x60, "NUMPAD1": 0x61, "NUMPAD2": 0x62, "NUMPAD3": 0x63, "NUMPAD4": 0x64,
		"NUMPAD5": 0x65, "NUMPAD6": 0x66, "NUMPAD7": 0x67, "NUMPAD8": 0x68, "NUMPAD9": 0x69,
		"OEM3": 0xC0, "GRAVE": 0xC0, "MINUS": 0xBD, "PLUS": 0xBB, "EQUAL": 0xBB,
		"COMMA": 0xBC, "PERIOD": 0xBE, "SLASH": 0xBF, "SEMICOLON": 0xBA,
		"LBRACKET": 0xDB, "RBRACKET": 0xDD, "BACKSLASH": 0xDC, "QUOTE": 0xDE,
	}
	if vk, ok := named[up]; ok {
		return up, vk, nil
	}
	return "", 0, fmt.Errorf("无法识别的按键 %q（支持 F1..F24、A..Z、0..9 与常用键名）", k)
}

// String renders the canonical form ("Ctrl+Shift+F9").
func (h Hotkey) String() string {
	if h.VK == 0 {
		return "none"
	}
	parts := make([]string, 0, 5)
	if h.Modifiers&ModControl != 0 {
		parts = append(parts, "Ctrl")
	}
	if h.Modifiers&ModAlt != 0 {
		parts = append(parts, "Alt")
	}
	if h.Modifiers&ModShift != 0 {
		parts = append(parts, "Shift")
	}
	if h.Modifiers&ModWin != 0 {
		parts = append(parts, "Win")
	}
	parts = append(parts, h.Key)
	return strings.Join(parts, "+")
}

// Display is the human label used by the settings page and the CLI banner.
func (h Hotkey) Display() string {
	if h.VK == 0 {
		return "未注册（已禁用）"
	}
	return h.String()
}
