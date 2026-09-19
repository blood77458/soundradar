package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/znz/soundradar/internal/config"
	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/live"
	"github.com/znz/soundradar/internal/match"
	"github.com/znz/soundradar/internal/overlay"
	"github.com/znz/soundradar/internal/wav"
)

// runOverlay implements `soundradar overlay`: the "while playing a game" mode.
// It is the same recognition link as `live`, but instead of a terminal panel the
// hits appear in the native click-through popup.
const overlayUsage = `soundradar overlay - 命中时弹出原生悬浮窗（图标 + 名称）

用法:
  soundradar overlay [选项]

选项:
  --library PATH   音效库文件（默认：配置文件里的库路径，否则 <exe 目录>\data\library.srz）
  --index PATH     指纹索引文件（默认与库文件同目录的 index.bin）
  --device SUBSTR  采集端点（渲染设备名的子串；留空 = 配置文件里的端点，再留空 = 系统默认）
  --wav PATH       用音频文件当音源（离线回放，不需要声卡）
  --realtime       回放 --wav 时按真实速度播放
  --x N --y N      悬浮窗左上角物理像素坐标（给了就覆盖配置文件里的锚点）
  --size N         单个事件格子的像素尺寸（覆盖配置文件）
  --opacity F      全局不透明度 0..1（覆盖配置文件）
  --seconds N      运行 N 秒后自动退出（自动化验证用；默认 0 = 直到 Ctrl+C）
  --csv PATH       把命中事件写成 CSV：时间,条目ID,名称,分数,margin,电平dBFS
  --demo           不进采集：循环展示库里每个条目（每个 1.5 秒）后自动退出
  --hold-item ID   只把这一个条目一直显示（不淡出），配合 --hold-seconds 用于截图取证
  --hold-seconds N --hold-item 保持多少秒（默认 20）
  --config PATH    配置文件路径（默认 <exe 目录>\config.json）
  --selfcheck MS   启动后 MS 毫秒打印一次自检 JSON 并退出（验收脚本用）
  --dump-state PATH  把自检 JSON 也写到这个文件（验收脚本用）
  --quiet          不打印启动横幅

说明:
  窗口样式固定为 WS_POPUP + WS_EX_LAYERED|WS_EX_TRANSPARENT|WS_EX_TOPMOST|
  WS_EX_NOACTIVATE|WS_EX_TOOLWINDOW：逐像素透明、鼠标穿透、置顶、不抢焦点、
  不出现在任务栏/Alt-Tab。热键（默认 F9）用官方 RegisterHotKey 注册，工具本身
  绝不模拟或钩取键盘。`

// overlaySelfCheck is the machine-readable state printed by --selfcheck.
type overlaySelfCheck struct {
	OK       bool    `json:"ok"`
	Version  string  `json:"version"`
	Library  string  `json:"library"`
	Device   string  `json:"device"`
	Source   string  `json:"source"`
	Hits     int64   `json:"hits"`
	Seconds  float64 `json:"seconds"`
	Overlay  any     `json:"overlay"`
	CfgPath  string  `json:"configPath"`
	DumpPath string  `json:"dumpPath,omitempty"`
	// RectDump carries the raw GetWindowRect/GetClientRect quadrants so the
	// acceptance script can compare the process' own view with what another
	// process reads through Win32.
	RectDump any `json:"rectDump,omitempty"`
}

func runOverlay(args []string) error {
	fs := flag.NewFlagSet("overlay", flag.ContinueOnError)
	libPath := fs.String("library", "", "library file (default <exe dir>/data/library.srz)")
	idxPath := fs.String("index", "", "index file (default <library dir>/index.bin)")
	device := fs.String("device", "", "case-insensitive substring of the render endpoint name")
	wavPath := fs.String("wav", "", "replay this file instead of capturing (offline)")
	realtime := fs.Bool("realtime", false, "pace a --wav replay at real speed")
	xFlag := fs.Int("x", -1, "overlay X in physical pixels (-1 = use the config)")
	yFlag := fs.Int("y", -1, "overlay Y in physical pixels (-1 = use the config)")
	sizeFlag := fs.Int("size", 0, "overlay cell size in pixels (0 = use the config)")
	opacityFlag := fs.Float64("opacity", -1, "overlay opacity 0..1 (-1 = use the config)")
	seconds := fs.Float64("seconds", 0, "stop after N seconds (0 = until Ctrl+C)")
	csvPath := fs.String("csv", "", "append recognised events to this CSV file")
	demo := fs.Bool("demo", false, "cycle through every library item (1.5 s each) and exit")
	holdItem := fs.String("hold-item", "", "keep this library item id on screen (acceptance evidence)")
	holdSeconds := fs.Int("hold-seconds", 20, "how long --hold-item keeps the item up")
	cfgPath := fs.String("config", "", "config file (default <exe dir>/config.json)")
	selfCheckMs := fs.Int("selfcheck", 0, "print a self-check JSON after N ms and exit")
	dumpState := fs.String("dump-state", "", "also write the self-check JSON to this file")
	quiet := fs.Bool("quiet", false, "suppress the startup banner")
	fs.SetOutput(os.Stdout)
	fs.Usage = func() { fmt.Fprint(fs.Output(), overlayUsage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *seconds < 0 {
		return fmt.Errorf("--seconds 必须 >= 0，当前 %v", *seconds)
	}

	// ---- config ----------------------------------------------------------
	cfgFile := strings.TrimSpace(*cfgPath)
	var pathInfo config.PathInfo
	if cfgFile == "" {
		var err error
		pathInfo, err = config.ResolvePath()
		if err != nil {
			return err
		}
		cfgFile = pathInfo.Path
	} else {
		pathInfo, _ = config.ResolvePath()
		pathInfo.Path = cfgFile
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	// CLI flags override the file for this run only (the file is not rewritten).
	if *xFlag >= 0 {
		cfg.Overlay.X = *xFlag
	}
	if *yFlag >= 0 {
		cfg.Overlay.Y = *yFlag
	}
	if *sizeFlag > 0 {
		cfg.Overlay.Size = *sizeFlag
	}
	if *opacityFlag >= 0 {
		cfg.Overlay.Opacity = *opacityFlag
	}
	if strings.TrimSpace(*device) != "" {
		cfg.Capture.Device = *device
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("配置不合法: %w", err)
	}

	// ---- library + index -------------------------------------------------
	lib, err := resolveOverlayLibrary(cfg, *libPath)
	if err != nil {
		return err
	}
	store, err := library.Open(lib)
	if err != nil {
		return fmt.Errorf("打开音效库失败: %w", err)
	}
	idx := strings.TrimSpace(*idxPath)
	if idx == "" {
		idx = index.DefaultPathFor(lib)
	}
	idx, err = filepath.Abs(idx)
	if err != nil {
		return fmt.Errorf("索引路径无效: %w", err)
	}
	params := dsp.DefaultParams()
	ix, rebuilt, why, err := index.LoadOrBuild(lib, idx, params)
	if err != nil {
		return fmt.Errorf("准备索引失败: %w", err)
	}
	if ix.Empty() {
		return fmt.Errorf("索引里一个模板都没有（库文件 %s 没有可用音频样本），先往库里加音效", lib)
	}

	// ---- overlay window --------------------------------------------------
	icons := &storeIcons{store: store, cache: map[string]image.Image{}}
	ov, err := overlay.New(cfg.Overlay, cfg.Hotkeys.ToggleOverlay, icons)
	if err != nil {
		return fmt.Errorf("创建悬浮窗失败: %w", err)
	}
	defer ov.Close()
	if !ov.Visible() {
		_ = ov.SetVisible(true)
	}

	hits := &hitCounter{}
	if !*quiet {
		printOverlayBanner(cfg, pathInfo, lib, idx, rebuilt, why, ix, ov)
	}

	// --hold-item: keep one library item on screen at full opacity. It exists so
	// the acceptance run has a deterministic, un-faded frame to screenshot (with
	// the fade in/out a captured frame is otherwise a random alpha).
	if *holdItem != "" {
		return runOverlayHold(store, ov, *holdItem, *holdSeconds, lib)
	}
	if *demo {
		return runOverlayDemo(store, ov, *selfCheckMs, *dumpState, pathInfo.Path, lib, cfg)
	}

	// ---- audio source ----------------------------------------------------
	var src live.FrameSource
	if strings.TrimSpace(*wavPath) != "" {
		src, err = live.NewFileSource(*wavPath, *realtime)
		if err != nil {
			return err
		}
	} else {
		src, err = live.NewCaptureSource(cfg.Capture.Device)
		if err != nil {
			if !cfg.Capture.FallbackToDefault || cfg.Capture.Device == "" {
				return fmt.Errorf("打开采集端点失败: %w", err)
			}
			fmt.Fprintf(os.Stderr, "[overlay] 端点 %q 打不开（%v），回退到系统默认端点\n", cfg.Capture.Device, err)
			src, err = live.NewCaptureSource("")
			if err != nil {
				return fmt.Errorf("打开默认采集端点失败: %w", err)
			}
		}
	}
	defer src.Stop()

	// ---- recognition link ------------------------------------------------
	csvSink, err := newHitCSV(*csvPath)
	if err != nil {
		return err
	}
	defer csvSink.Close()

	// ---- P4 recall (press F8 to keep the last few seconds) ---------------
	rec, err := startRecaller(cfg, *quiet)
	if err != nil {
		return fmt.Errorf("初始化回溯保存失败: %w", err)
	}
	defer rec.Close()
	if rec.Enabled() && !*quiet {
		fmt.Printf("[overlay] 回溯保存   : %s\n", rec.HotkeyStatus())
		fmt.Printf("[overlay] 候选项目录 : %s（最近 %.1f 秒，最多 %d 个）\n",
			rec.Store().Dir(), rec.RingSeconds(), rec.Store().MaxFiles())
	}

	opts := live.Options{TopN: 5, SilenceDBFS: match.DefaultOptions().SilenceDBFS}
	if rec.Enabled() {
		// The ring is filled from the same blocks the analyzer sees.
		opts.OnBlock = rec.Buffer
	}
	eng, err := live.New(src, ix, opts, func(tk live.Tick) {
		// Keep the "guess" current so a candidate saved right now names the item
		// that was on screen at that instant.
		if rec.Enabled() {
			rec.Observe(tk)
		}
	}, func(ev match.Event) {
		hits.add()
		_ = ov.Show(overlay.DisplayItem{ID: ev.ID, Name: ev.Name, Score: ev.Score})
		csvSink.Write(ev)
		if !*quiet {
			fmt.Printf("[overlay] 命中「%s」 分数 %.4f  电平 %s dBFS\n",
				ev.Name, ev.Score, wav.FormatDBFS(ev.LevelDBFS))
		}
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reason := "eof"
	if *seconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*seconds*float64(time.Second)))
		defer cancel()
		reason = "seconds"
	}

	// ---- self-check timer ------------------------------------------------
	if *selfCheckMs > 0 {
		go func() {
			time.Sleep(time.Duration(*selfCheckMs) * time.Millisecond)
			sc := overlaySelfCheck{
				OK: true, Version: versionNumber, Library: lib, Device: cfg.Capture.Device,
				Source: eng.SourceInfo().Detail, Hits: hits.get(),
				Seconds: float64(*selfCheckMs) / 1000, Overlay: ov.State(),
				CfgPath: pathInfo.Path, DumpPath: *dumpState,
				RectDump: ov.RectDump(),
			}
			writeSelfCheck(sc, *dumpState)
			// NOTE: the self-check does NOT stop the run. It used to call
			// stop(), which cancelled the context after N ms and made
			// "--seconds 12 --selfcheck 3000" only recognise for 3 seconds -
			// exactly the kind of interference the acceptance run must not have.
		}()
	}

	runErr := eng.Run(ctx)
	stats := eng.Stats()
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	if !*quiet {
		fmt.Printf("\n[overlay] 结束（%s）：%s\n", reasonText(reason), stats.Summary())
		fmt.Printf("[overlay] 命中 %d 次，悬浮窗渲染 %d 帧，队列丢弃 %d 个\n",
			stats.Events, ov.Frames(), ov.State().Dropped)
		if rec.Enabled() {
			fmt.Printf("[overlay] 回溯保存 %d 次，候选项目录 %s\n", rec.Saves(), rec.Store().Dir())
		}
	}
	return nil
}

// resolveOverlayLibrary picks the library path: the flag, then the profile in
// the config (unused for now), then <exe dir>/data/library.srz.
func resolveOverlayLibrary(cfg *config.Config, flagPath string) (string, error) {
	if p := strings.TrimSpace(flagPath); p != "" {
		return filepath.Abs(p)
	}
	p, err := library.DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// printOverlayBanner prints the one-time startup report.
func printOverlayBanner(cfg *config.Config, info config.PathInfo, lib, idx string,
	rebuilt bool, why string, ix *index.Index, ov *overlay.Overlay) {
	items, samples, bytes := ix.Stats()
	hk, hkOK := ov.HotkeyRegistered()
	fmt.Printf("[overlay] soundradar P3 悬浮窗模式\n")
	fmt.Printf("[overlay] 配置文件   : %s（来源 %s）\n", info.Path, info.Source)
	fmt.Printf("[overlay] 音效库     : %s\n", lib)
	fmt.Printf("[overlay] 索引       : %s%s\n", idx, rebuildNote(rebuilt, why))
	fmt.Printf("[overlay] 索引规模   : %d 个条目 / %d 个模板 / %d 维 / 量化 %d 字节\n", items, samples, ix.Dim(), bytes)
	fmt.Printf("[overlay] 采集端点   : %s\n", orDefault(cfg.Capture.Device, "<系统默认渲染端点>"))
	fmt.Printf("[overlay] 悬浮窗位置 : %s\n", config.DescribeAnchor(cfg.Overlay))
	fmt.Printf("[overlay] 悬浮窗尺寸 : 格子 %d 像素 × 最多 %d 个（画布 %d×%d）\n",
		cfg.Overlay.Size, cfg.Overlay.MaxSimultaneous, canvasW(cfg), canvasH(cfg))
	fmt.Printf("[overlay] 不透明度   : %.2f，淡入 %d ms / 保持 %d ms / 淡出 %d ms\n",
		cfg.Overlay.Opacity, cfg.Overlay.FadeInMs, cfg.Overlay.DurationMs, cfg.Overlay.FadeOutMs)
	if hkOK {
		fmt.Printf("[overlay] 热键       : %s（RegisterHotKey 成功，显示/隐藏切换）\n", hk)
	} else {
		fmt.Printf("[overlay] 热键       : %s 注册失败（%s）；可用设置页或 /api/overlay/visible 切换\n",
			hk, ov.HotkeyError())
	}
	if st := ov.State(); st.FontASCII {
		fmt.Printf("[overlay] 字体       : %s（降级：只能用 ASCII）\n", st.Font)
	} else {
		fmt.Printf("[overlay] 字体       : %s（中文字形正常）\n", st.Font)
	}
	if st := ov.State(); st.LastError != "" {
		fmt.Printf("[overlay] 警告       : %s\n", st.LastError)
	}
	fmt.Printf("[overlay] 开始识别…（Ctrl+C 退出）\n\n")
	fmt.Print(formatMonitorReport(ov))
}

// formatMonitorReport prints the physical monitor geometry and the window rect,
// which is what the acceptance script cross-checks against GetWindowRect.
func formatMonitorReport(ov *overlay.Overlay) string {
	st := ov.State()
	var sb strings.Builder
	sb.WriteString("[overlay] 显示器（物理像素）:\n")
	for _, m := range st.Monitors {
		fx, fy, fw, fh := m.FullRect()
		wx, wy, ww, wh := m.WorkRect()
		fmt.Fprintf(&sb, "[overlay]   #%d %-14s 整屏 (%d,%d) %dx%d  工作区 (%d,%d) %dx%d  DPI %d (%.0f%%)%s\n",
			m.Index, m.Device, fx, fy, fw, fh, wx, wy, ww, wh, m.DPI, m.Scale*100,
			map[bool]string{true: "  主显示器", false: ""}[m.Primary])
	}
	if x, y, w, h, err := ov.Rect(); err == nil {
		fmt.Fprintf(&sb, "[overlay] 窗口矩形: (%d,%d) %dx%d  hwnd=0x%X 类名=%s 线程=%d\n",
			x, y, w, h, ov.HWND(), ov.ClassName(), ov.ThreadID())
	} else {
		fmt.Fprintf(&sb, "[overlay] 窗口矩形: 读取失败 %v（hwnd=0x%X）\n", err, ov.HWND())
	}
	return sb.String()
}

func rebuildNote(rebuilt bool, why string) string {
	if rebuilt {
		return "（已自动重建：" + why + "）"
	}
	return "（直接复用）"
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func canvasW(cfg *config.Config) int {
	w, _ := config.CanvasSize(cfg.Overlay)
	return w
}

func canvasH(cfg *config.Config) int {
	_, h := config.CanvasSize(cfg.Overlay)
	return h
}

// ---------------------------------------------------------------------------
// --demo
// ---------------------------------------------------------------------------

// runOverlayDemo shows every library item once (1.5 s each) and exits. It is the
// "look at it" mode used by the acceptance screenshots: no sound card, no
// recognition, just the window painting real library icons.
func runOverlayDemo(store *library.Store, ov *overlay.Overlay, selfCheckMs int,
	dumpState, cfgPath, lib string, cfg *config.Config) error {

	ids := store.Order()
	if len(ids) == 0 {
		return errors.New("库里没有条目，--demo 没有东西可以展示")
	}
	per := 1500 * time.Millisecond
	fmt.Printf("[overlay] --demo：依次展示 %d 个条目，每个 %.1f 秒，共 %.1f 秒\n",
		len(ids), per.Seconds(), float64(len(ids))*per.Seconds())

	if selfCheckMs > 0 {
		go func() {
			time.Sleep(time.Duration(selfCheckMs) * time.Millisecond)
			writeSelfCheck(overlaySelfCheck{
				OK: true, Version: versionNumber, Library: lib, Device: "demo",
				Source: "demo（库内条目循环展示）", Hits: int64(len(ids)),
				Seconds: float64(selfCheckMs) / 1000, Overlay: ov.State(),
				CfgPath: cfgPath, DumpPath: dumpState,
			}, dumpState)
		}()
	}

	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			close(stop)
		case <-stop:
		}
	}()

	for _, id := range ids {
		it := store.Get(id)
		if it == nil {
			continue
		}
		if err := ov.Show(overlay.DisplayItem{ID: id, Name: it.Name, Score: 0.93}); err != nil {
			return err
		}
		fmt.Printf("[overlay] 展示 %s（%s）\n", it.Name, id)
		select {
		case <-stop:
			fmt.Println("[overlay] 收到中断，提前退出")
			return nil
		case <-time.After(per):
		}
	}
	// Let the last item finish its fade-out before the window goes away.
	time.Sleep(time.Duration(cfg.Overlay.FadeOutMs+80) * time.Millisecond)
	fmt.Printf("[overlay] --demo 结束：展示了 %d 个条目，渲染 %d 帧\n", len(ids), ov.Frames())
	return nil
}

// ---------------------------------------------------------------------------
// --hold-item
// ---------------------------------------------------------------------------

// runOverlayHold shows one item and keeps it there, for screenshot-based
// acceptance evidence. It never touches the sound card.
func runOverlayHold(store *library.Store, ov *overlay.Overlay, id string, seconds int, lib string) error {
	it := store.Get(id)
	if it == nil {
		return fmt.Errorf("条目不存在: %s", id)
	}
	if seconds < 3 {
		seconds = 3
	}
	// Re-showing the same item keeps it at full opacity: each Show restarts its
	// 60 ms fade-in and its 1150 ms hold, so the window never fades out.
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(deadline) {
		if err := ov.Show(overlay.DisplayItem{ID: it.ID, Name: it.Name, Score: 0.93}); err != nil {
			return err
		}
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Printf("[overlay] --hold-item 结束：%s（%s）保持了 %d 秒，渲染 %d 帧，库 %s\n",
		it.Name, it.ID, seconds, ov.Frames(), lib)
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type storeIcons struct {
	store *library.Store
	mu    sync.Mutex
	cache map[string]image.Image
	// decoded counts how many PNGs were actually decoded (the renderer has its
	// own cache on top of this one).
	decoded atomic.Int64
}

// Icon implements overlay.IconProvider over the library store.
func (s *storeIcons) Icon(id string) (image.Image, error) {
	s.mu.Lock()
	if img, ok := s.cache[id]; ok {
		s.mu.Unlock()
		return img, nil
	}
	s.mu.Unlock()

	it := s.store.Get(id)
	if it == nil {
		return nil, fmt.Errorf("条目不存在: %s", id)
	}
	img, err := overlay.DecodeIconPNG(it.IconPNG())
	if err != nil {
		return nil, err
	}
	s.decoded.Add(1)
	s.mu.Lock()
	s.cache[id] = img
	s.mu.Unlock()
	return img, nil
}

type hitCounter struct{ n atomic.Int64 }

func (h *hitCounter) add()       { h.n.Add(1) }
func (h *hitCounter) get() int64 { return h.n.Load() }

type hitCSV struct {
	f      *os.File
	w      *csv.Writer
	mu     sync.Mutex
	closed bool
}

func newHitCSV(path string) (*hitCSV, error) {
	if strings.TrimSpace(path) == "" {
		return &hitCSV{}, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	w := csv.NewWriter(f)
	// UTF-8 BOM so Excel does not read the Chinese headers as ANSI.
	_, _ = f.WriteString("\ufeff")
	_ = w.Write([]string{"时间", "条目ID", "名称", "分数", "margin", "电平dBFS"})
	w.Flush()
	return &hitCSV{f: f, w: w}, nil
}

func (c *hitCSV) Write(ev match.Event) {
	if c == nil || c.w == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.w.Write([]string{
		ev.Time.Format("2006-01-02 15:04:05.000"),
		ev.ID, ev.Name,
		strconv.FormatFloat(ev.Score, 'f', 4, 64),
		fmtFloat(ev.Margin),
		wav.FormatDBFS(ev.LevelDBFS),
	})
	c.w.Flush()
}

func (c *hitCSV) Close() {
	if c == nil || c.f == nil || c.closed {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.w.Flush()
	_ = c.f.Close()
}

// writeSelfCheck prints the self-check JSON to stdout and, when path is set,
// writes it to that file as well (the acceptance script reads the file so the
// PowerShell pipe cannot mangle the Chinese text).
func writeSelfCheck(sc overlaySelfCheck, path string) {
	b, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		b = []byte(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	fmt.Println("--- overlay selfcheck ---")
	fmt.Println(string(b))
	if strings.TrimSpace(path) != "" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			_ = os.MkdirAll(dir, 0o755)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "[overlay] 写自检文件失败: %v\n", err)
		}
	}
}
