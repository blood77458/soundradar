package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/znz/soundradar/internal/config"
	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/live"
	"github.com/znz/soundradar/internal/match"
	"github.com/znz/soundradar/internal/wav"
)

// runLive implements `soundradar live`: the P2 realtime recognition link.
//
// It is the CLI face of internal/live: it opens a loopback capture (or replays
// a file), computes the fingerprint of every 5.333 ms hop, scores it against
// the quantised index and prints a live panel of the top-N candidates plus the
// debounced hits.
const liveUsage = `soundradar live - 实时识别（听不到也能看到）

用法:
  soundradar live [选项]

选项:
  --device SUBSTR  采集端点（渲染设备名的子串，不区分大小写；留空 = 系统默认端点）
  --wav PATH       用音频文件当音源（离线回放，不需要声卡；wav / mp3）
  --realtime       回放 --wav 时按真实速度播放（默认尽快播完）
  --library PATH   音效库文件（默认 <exe 目录>\data\library.srz）
  --index PATH     指纹索引文件（默认与库文件同目录的 index.bin）
  --seconds N      采满 N 秒后自动退出（默认 0 = 一直跑到 Ctrl+C）
  --top N          面板显示前 N 名（默认 8）
  --json           每个 tick 输出一行 JSON（JSONL，便于脚本处理）
  --csv PATH       把命中事件写成 CSV：时间,条目ID,名称,分数,margin,电平dBFS
  --recall         同时启用回溯保存热键（P4；配置 recall.enabled / hotkeys.recallLabel）
  --config PATH    配置文件路径（默认 <exe 目录>\config.json）
  --quiet          只输出命中事件（不打印横幅与结束统计）

说明:
  特征窗口 186.7 ms、步进 5.333 ms；每个 tick 对应当前窗口的相似度排行。
  静音（低于 -60 dBFS）不打分；命中需要同时通过阈值、margin、冷却三道门。
  采集链路从不阻塞声卡：处理不过来时丢帧会被计数并在统计里报告。`

// liveEventJSON is one recognised event in the machine readable output.
type liveEventJSON struct {
	Time      time.Time `json:"t"`
	AudioMs   float64   `json:"audioMs"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Score     float64   `json:"score"`
	Margin    *float64  `json:"margin"`
	LevelDBFS *float64  `json:"level"`
}

// liveStatsJSON mirrors live.Stats for --json.
type liveStatsJSON struct {
	Blocks         int64   `json:"blocks"`
	Windows        int64   `json:"windows"`
	Ticks          int64   `json:"ticks"`
	Events         int64   `json:"events"`
	SilentWindows  int64   `json:"silentWindows"`
	Samples        int64   `json:"samples"`
	AudioSeconds   float64 `json:"audioSeconds"`
	Dropped        int64   `json:"dropped"`
	ElapsedMs      float64 `json:"elapsedMs"`
	AvgBlockMs     float64 `json:"avgBlockMs"`
	MaxBlockMs     float64 `json:"maxBlockMs"`
	MsPer20msAudio float64 `json:"msPer20msAudio"`
	HeapInuseBytes uint64  `json:"heapInuseBytes"`
	BudgetMs       float64 `json:"budgetMs"`
}

// liveTickJSON is one JSONL record.
type liveTickJSON struct {
	Type    string         `json:"type"` // tick | event | summary
	T       *time.Time     `json:"t,omitempty"`
	AudioMs float64        `json:"audioMs,omitempty"`
	Level   *float64       `json:"level"`
	Silent  bool           `json:"silent"`
	Top     []live.Hit     `json:"top"`
	Event   *liveEventJSON `json:"event"`
	Stats   *liveStatsJSON `json:"stats,omitempty"`
	Reason  string         `json:"reason,omitempty"`
}

func runLive(args []string) error {
	fs := flag.NewFlagSet("live", flag.ContinueOnError)
	device := fs.String("device", "", "case-insensitive substring of the render endpoint name")
	wavPath := fs.String("wav", "", "replay this file instead of capturing (offline)")
	realtime := fs.Bool("realtime", false, "pace a --wav replay at real speed")
	libPath := fs.String("library", "", "library file (default <exe dir>/data/library.srz)")
	idxPath := fs.String("index", "", "index file (default <library dir>/index.bin)")
	seconds := fs.Float64("seconds", 0, "stop after N seconds (0 = until Ctrl+C)")
	topN := fs.Int("top", 8, "how many ranked items to show")
	asJSON := fs.Bool("json", false, "one JSON object per tick (JSONL)")
	csvPath := fs.String("csv", "", "append recognised events to this CSV file")
	quiet := fs.Bool("quiet", false, "print only recognised events")
	recallFlag := fs.Bool("recall", false, "also enable the P4 recall hotkey (config recall.enabled / hotkeys.recallLabel)")
	cfgPath := fs.String("config", "", "settings file (default <exe dir>/config.json)")
	// -h/--help prints the Chinese usage instead of Go's English flag list.
	// It goes to stdout because a requested help text is normal output.
	fs.SetOutput(os.Stdout)
	fs.Usage = func() { fmt.Fprint(fs.Output(), liveUsage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *seconds < 0 {
		return fmt.Errorf("--seconds 必须 >= 0，当前 %v", *seconds)
	}
	if *topN < 1 {
		return fmt.Errorf("--top 必须 >= 1，当前 %d", *topN)
	}

	// ---- library + index -------------------------------------------------
	lib, err := resolveLibraryPath(*libPath)
	if err != nil {
		return err
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

	// ---- audio source ----------------------------------------------------
	var src live.FrameSource
	if strings.TrimSpace(*wavPath) != "" {
		src, err = live.NewFileSource(*wavPath, *realtime)
		if err != nil {
			return err
		}
	} else {
		src, err = live.NewCaptureSource(*device)
		if err != nil {
			return fmt.Errorf("打开采集端点失败: %w", err)
		}
	}
	defer src.Stop()

	// ---- renderer / sinks ------------------------------------------------
	out := newLiveRenderer(os.Stdout, *topN, *asJSON, *quiet, *csvPath)
	defer out.Close()

	// ---- P4 recall --------------------------------------------------------
	// The recall ring is filled from the same blocks the analyzer sees, so a
	// hotkey press during `live` saves exactly what the panel was showing.
	var rec *recallRecorder
	cfgFile := strings.TrimSpace(*cfgPath)
	if cfgFile == "" {
		if p, perr := config.ResolvePath(); perr == nil {
			cfgFile = p.Path
		}
	}
	if cfgFile != "" {
		if cfg, cerr := config.Load(cfgFile); cerr == nil && cfg != nil {
			if *recallFlag {
				cfg.Recall.Enabled = true
			}
			if cfg.Recall.Enabled {
				var rerr error
				rec, rerr = startRecaller(cfg, *quiet)
				if rerr != nil {
					fmt.Fprintf(os.Stderr, "[live] 回溯保存初始化失败: %v\n", rerr)
				}
			}
		}
	}
	defer rec.Close()

	opts := live.Options{
		TopN:        *topN,
		SilenceDBFS: match.DefaultOptions().SilenceDBFS,
	}
	if rec.Enabled() {
		opts.OnBlock = rec.Buffer
	}
	eng, err := live.New(src, ix, opts, func(tk live.Tick) {
		out.OnTick(tk)
		if rec.Enabled() {
			rec.Observe(tk)
		}
	}, out.OnEvent)
	if err != nil {
		return err
	}

	// ---- banner ----------------------------------------------------------
	items, samples, bytes := ix.Stats()
	info := eng.SourceInfo()
	eff := eng.Options()
	if !*asJSON && !*quiet {
		fmt.Printf("[live] 音源       : %s\n", info.Detail)
		if info.Kind == "capture" {
			fmt.Printf("[live] 采集格式   : %s\n", info.Format)
		}
		fmt.Printf("[live] 音效库     : %s\n", lib)
		fmt.Printf("[live] 索引       : %s", idx)
		if rebuilt {
			fmt.Printf("（已自动重建：%s）", why)
		} else {
			fmt.Printf("（直接复用）")
		}
		fmt.Println()
		fmt.Printf("[live] 索引规模   : %d 个条目 / %d 个模板 / %d 维 / 量化 %d 字节\n", items, samples, ix.Dim(), bytes)
		fmt.Printf("[live] 特征参数   : %s v%d，指纹 %s\n", dsp.Algorithm, dsp.Version, params.Fingerprint())
		fmt.Printf("[live] 时间窗     : 窗长 %.1f ms（%d 帧），步进 %.3f ms（%d 采样），每 tick %.0f ms\n",
			params.PatchDurationS()*1000, params.WindowFrames, params.HopDurationS()*1000, params.HopSize,
			eff.TickEvery.Seconds()*1000)
		fmt.Printf("[live] 判定门槛   : 默认阈值 %.2f（按条目可调）、margin %.2f、静音门限 %.0f dBFS、冷却 %d ms\n",
			eff.MatchOptions.DefaultThreshold, eff.MatchOptions.MinMargin, eff.MatchOptions.SilenceDBFS, eff.MatchOptions.CooldownMs)
		if rec.Enabled() {
			fmt.Printf("[live] 回溯保存   : %s\n", rec.HotkeyStatus())
			fmt.Printf("[live] 候选项目录 : %s（最近 %.1f 秒，最多 %d 个）\n",
				rec.Store().Dir(), rec.RingSeconds(), rec.Store().MaxFiles())
		} else {
			fmt.Printf("[live] 回溯保存   : 未启用（加 --recall 或把配置里 recall.enabled 设为 true）\n")
		}
		if *seconds > 0 {
			fmt.Printf("[live] 运行时长   : %.1f 秒后自动退出\n", *seconds)
		} else {
			fmt.Printf("[live] 运行时长   : 直到 Ctrl+C\n")
		}
		fmt.Printf("[live] 开始实时识别…\n\n")
	}

	// ---- run -------------------------------------------------------------
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reason := "eof"
	if *seconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*seconds*float64(time.Second)))
		defer cancel()
		reason = "seconds"
	}

	runErr := eng.Run(ctx)
	stats := eng.Stats()

	if runErr != nil {
		// A device that disappeared mid-run still leaves usable statistics.
		out.Summary(stats, "error")
		return runErr
	}
	if err := out.Finish(stats, reason); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// rendering
// ---------------------------------------------------------------------------

type liveRenderer struct {
	w        io.Writer
	topN     int
	asJSON   bool
	quiet    bool
	tty      bool
	csvW     *csv.Writer
	csvFile  *os.File
	lines    int // lines the TTY panel currently occupies
	frame    []string
	peakDB   float64
	peakAt   time.Time
	events   int64
	lastEvt  string
	started  time.Time
	encoder  *json.Encoder
	csvCount int
}

func newLiveRenderer(w *os.File, topN int, asJSON, quiet bool, csvPath string) *liveRenderer {
	r := &liveRenderer{
		w: w, topN: topN, asJSON: asJSON, quiet: quiet,
		peakDB: math.Inf(-1), started: time.Now(),
	}
	r.tty = isTerminal(w) && !asJSON && !quiet
	r.encoder = json.NewEncoder(w)
	if strings.TrimSpace(csvPath) != "" {
		f, err := os.OpenFile(csvPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err == nil {
			r.csvFile = f
			r.csvW = csv.NewWriter(f)
			// UTF-8 BOM so Excel does not read the Chinese headers as ANSI.
			_, _ = f.WriteString("\ufeff")
			_ = r.csvW.Write([]string{"时间", "条目ID", "名称", "分数", "margin", "电平dBFS"})
			r.csvW.Flush()
		} else {
			fmt.Fprintf(os.Stderr, "[live] 警告: 无法写入 CSV %s: %v\n", csvPath, err)
		}
	}
	return r
}

// isTerminal reports whether f is an interactive console (so the animated panel
// is only used where it makes sense; redirected output gets one line per tick).
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func (r *liveRenderer) Close() {
	if r.csvW != nil {
		r.csvW.Flush()
		if r.csvFile != nil {
			_ = r.csvFile.Close()
		}
	}
	if r.lines > 0 {
		fmt.Fprintln(r.w)
	}
}

// OnTick renders one scoring snapshot.
func (r *liveRenderer) OnTick(tk live.Tick) {
	if r.asJSON {
		rec := liveTickJSON{
			Type:    "tick",
			T:       timePtr(tk.Time),
			AudioMs: float64(tk.AudioTime.Microseconds()) / 1000,
			Level:   finite(tk.LevelDBFS),
			Silent:  tk.Silent,
			Top:     live.HitsFrom(tk.Top),
		}
		if tk.Event != nil {
			rec.Event = eventJSON(*tk.Event, tk.AudioTime)
		}
		st := statsJSON(tk.Stats)
		rec.Stats = &st
		_ = r.encoder.Encode(rec)
		return
	}
	if r.quiet {
		return
	}
	if r.tty {
		r.drawPanel(tk)
		return
	}
	// Redirected output: one compact line per tick.
	names := make([]string, 0, min(3, len(tk.Top)))
	for i, h := range tk.Top {
		if i >= 3 {
			break
		}
		names = append(names, fmt.Sprintf("%s %.3f", h.Name, h.Score.Float()))
	}
	silent := ""
	if tk.Silent {
		silent = " [静音:沿用上次排名]"
	}
	fmt.Fprintf(r.w, "[live] %s  %7s dBFS%s  %s\n",
		tk.Time.Format("15:04:05.000"), levelOrNegInf(tk.LevelDBFS), silent, strings.Join(names, " | "))
}

// OnEvent prints one recognised hit.
func (r *liveRenderer) OnEvent(ev match.Event) {
	r.events++
	line := fmt.Sprintf("命中「%s」 分数 %.4f  margin %s  电平 %s dBFS",
		ev.Name, ev.Score, fmtFloat(ev.Margin), wav.FormatDBFS(ev.LevelDBFS))
	r.lastEvt = fmt.Sprintf("%s  %s", ev.Time.Format("15:04:05.000"), line)

	if r.asJSON {
		rec := liveTickJSON{
			Type:  "event",
			T:     timePtr(ev.Time),
			Level: finite(ev.LevelDBFS),
			Top:   []live.Hit{},
			Event: eventJSON(ev, 0),
		}
		_ = r.encoder.Encode(rec)
	} else if !r.tty {
		// Always show hits, even with --quiet (that is the point of --quiet).
		fmt.Fprintf(r.w, "\n[live] ★ %s\n\n", r.lastEvt)
	}
	if r.csvW != nil {
		_ = r.csvW.Write([]string{
			ev.Time.Format("2006-01-02 15:04:05.000"),
			ev.ID,
			ev.Name,
			strconv.FormatFloat(ev.Score, 'f', 4, 64),
			fmtFloat(ev.Margin),
			wav.FormatDBFS(ev.LevelDBFS),
		})
		r.csvW.Flush()
		r.csvCount++
	}
}

// drawPanel redraws the whole TTY panel in place.
func (r *liveRenderer) drawPanel(tk live.Tick) {
	now := time.Now()
	if tk.LevelDBFS > r.peakDB {
		r.peakDB, r.peakAt = tk.LevelDBFS, now
	}
	peak := r.peakDB
	if d := now.Sub(r.peakAt); d > 1500*time.Millisecond {
		peak -= d.Seconds() * 20 // 20 dB/s decay after a 1.5 s hold
	}

	frame := make([]string, 0, r.topN+4)
	frame = append(frame, fmt.Sprintf("电平  %s  %7s dBFS   峰值 %7s dBFS   音频位置 %6.2f s%s",
		barPct(dbfsPct(tk.LevelDBFS), 30), levelOrNegInf(tk.LevelDBFS), levelOrNegInf(peak),
		float64(tk.AudioTime.Microseconds())/1e6, silentMark(tk.Silent)))
	frame = append(frame, "────────────────────────────────────────────────────────────────────────")
	if len(tk.Top) == 0 {
		frame = append(frame, "（没有分数：静音或索引为空）")
	}
	for i, h := range tk.Top {
		if i >= r.topN {
			break
		}
		frame = append(frame, fmt.Sprintf("#%-2d %s %s  %.4f",
			i+1, padDisp(truncDisp(h.Name, 24), 26), barScore(h.Score.Float(), 24), h.Score.Float()))
	}
	if r.lastEvt == "" {
		frame = append(frame, "事件  等待命中…（阈值/margin 见启动横幅或条目设置）")
	} else {
		frame = append(frame, "事件  "+truncDisp(r.lastEvt, 96))
	}
	frame = append(frame, fmt.Sprintf("统计  块 %d · 音频 %.1f s · 窗口 %d · tick %d · 命中 %d · 丢帧 %d · 单块平均 %.2f ms / 峰值 %.2f ms · 每 20 ms 音频 %.2f ms",
		tk.Stats.Blocks, tk.Stats.AudioSeconds, tk.Stats.Windows, tk.Stats.Ticks, tk.Stats.Events,
		tk.Stats.Dropped, tk.Stats.AvgBlockMs, tk.Stats.MaxBlockMs, tk.Stats.MsPer20msAudio))
	if tk.Stats.Dropped > 0 {
		frame = append(frame, fmt.Sprintf("警告  有 %d 个音频块因处理不过来被丢弃（面板仍可用，但可能漏命中）", tk.Stats.Dropped))
	}
	if tk.Stats.MsPer20msAudio > live.RealtimeBudgetMs {
		frame = append(frame, fmt.Sprintf("警告  每 20 ms 音频耗时 %.2f ms，超过预算 %.1f ms",
			tk.Stats.MsPer20msAudio, live.RealtimeBudgetMs))
	}

	var sb strings.Builder
	if r.lines > 0 {
		fmt.Fprintf(&sb, "\x1b[%dA", r.lines)
	}
	for _, line := range frame {
		sb.WriteString("\x1b[2K")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	fmt.Fprint(r.w, sb.String())
	r.lines = len(frame)
}

// Summary is printed when the run ends with an error.
func (r *liveRenderer) Summary(st live.Stats, reason string) {
	if r.asJSON {
		rec := liveTickJSON{Type: "summary", Level: nil, Top: []live.Hit{}, Reason: reason}
		s := statsJSON(st)
		rec.Stats = &s
		_ = r.encoder.Encode(rec)
		return
	}
	if r.tty && r.lines > 0 {
		fmt.Fprintf(r.w, "\x1b[%dA", r.lines)
	}
	fmt.Fprintf(r.w, "\n[live] 结束（%s）: %s\n", reasonText(reason), st.Summary())
}

// Finish prints the end-of-run report and returns any CSV flush error.
func (r *liveRenderer) Finish(st live.Stats, reason string) error {
	if r.asJSON {
		rec := liveTickJSON{Type: "summary", Level: nil, Top: []live.Hit{}, Reason: reason}
		s := statsJSON(st)
		rec.Stats = &s
		_ = r.encoder.Encode(rec)
		return nil
	}
	if r.csvW != nil {
		r.csvW.Flush()
		if err := r.csvW.Error(); err != nil {
			return fmt.Errorf("写 CSV 失败: %w", err)
		}
	}
	if r.quiet {
		// --quiet means "events only": no banner, no statistics.
		return nil
	}
	if r.tty && r.lines > 0 {
		fmt.Fprintf(r.w, "\x1b[%dA", r.lines)
		r.lines = 0
	}
	fmt.Printf("\n[live] 实时统计\n")
	fmt.Printf("[live] 音频块     : %d（%d 采样 = %.2f 秒音频）\n", st.Blocks, st.Samples, st.AudioSeconds)
	fmt.Printf("[live] 打分窗口   : %d（%.3f ms 一个，静音跳过 %d）\n", st.Windows, 1000.0/187.5, st.SilentWindows)
	fmt.Printf("[live] tick / 命中: %d / %d\n", st.Ticks, st.Events)
	fmt.Printf("[live] 丢帧       : %d\n", st.Dropped)
	fmt.Printf("[live] 处理耗时   : 单块平均 %.3f ms，峰值 %.3f ms；每 20 ms 音频 %.3f ms（预算 %.1f ms）\n",
		st.AvgBlockMs, st.MaxBlockMs, st.MsPer20msAudio, live.RealtimeBudgetMs)
	fmt.Printf("[live] 堆内存     : %.1f MiB（HeapInuse）\n", float64(st.HeapInuseBytes)/(1<<20))
	fmt.Printf("[live] 结束原因   : %s\n", reasonText(reason))
	if r.csvW != nil {
		fmt.Printf("[live] CSV        : 已写入 %d 条命中事件\n", r.csvCount)
	}
	return nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// reasonText 把内部的结束原因标识翻成给人看的说明。
// 内部取值：seconds=达到 --seconds 时长、eof=音频源结束、error=出错。
func reasonText(reason string) string {
	switch reason {
	case "seconds":
		return "达到 --seconds 指定时长"
	case "eof":
		return "音频源结束"
	case "error":
		return "出错（错误信息见上方）"
	case "":
		return "未知"
	default:
		return reason
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// finite converts NaN/±Inf to nil so encoding/json can encode the value
// (-Inf dBFS is digital silence, +Inf margin means "no runner-up").
func finite(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	f := v
	return &f
}

func fmtFloat(v float64) string {
	if math.IsInf(v, 1) {
		return "+inf"
	}
	if math.IsInf(v, -1) {
		return "-inf"
	}
	return strconv.FormatFloat(v, 'f', 4, 64)
}

func levelOrNegInf(v float64) string {
	if math.IsInf(v, -1) {
		return "-inf"
	}
	return fmt.Sprintf("%.1f", v)
}

func eventJSON(ev match.Event, at time.Duration) *liveEventJSON {
	return &liveEventJSON{
		Time:      ev.Time,
		AudioMs:   float64(at.Microseconds()) / 1000,
		ID:        ev.ID,
		Name:      ev.Name,
		Score:     ev.Score,
		Margin:    finite(ev.Margin),
		LevelDBFS: finite(ev.LevelDBFS),
	}
}

func statsJSON(st live.Stats) liveStatsJSON {
	return liveStatsJSON{
		Blocks:         st.Blocks,
		Windows:        st.Windows,
		Ticks:          st.Ticks,
		Events:         st.Events,
		SilentWindows:  st.SilentWindows,
		Samples:        st.Samples,
		AudioSeconds:   st.AudioSeconds,
		Dropped:        st.Dropped,
		ElapsedMs:      float64(st.Elapsed.Microseconds()) / 1000,
		AvgBlockMs:     st.AvgBlockMs,
		MaxBlockMs:     st.MaxBlockMs,
		MsPer20msAudio: st.MsPer20msAudio,
		HeapInuseBytes: st.HeapInuseBytes,
		BudgetMs:       live.RealtimeBudgetMs,
	}
}

func silentMark(silent bool) string {
	if silent {
		return "   [静音]"
	}
	return ""
}

// dbfsPct maps -60..0 dBFS onto 0..100.
func dbfsPct(db float64) float64 {
	if math.IsInf(db, -1) || math.IsNaN(db) {
		return 0
	}
	p := (db + 60) / 60 * 100
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// barPct renders a 0..100 percentage as a filled/empty block bar of width n.
func barPct(pct float64, n int) string { return barClamped(pct/100, n) }

// barScore renders a 0..1 similarity score as a filled/empty block bar.
func barScore(score float64, n int) string { return barClamped(score, n) }

func barClamped(p float64, n int) string {
	if n <= 0 {
		return ""
	}
	if math.IsNaN(p) || p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	full := int(math.Round(p * float64(n)))
	return strings.Repeat("█", full) + strings.Repeat("░", n-full)
}

// dispWidth is the terminal display width of s, counting CJK runes as 2 cells.
func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func runeWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0xA4CF, // CJK radicals .. Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
		r >= 0xFE30 && r <= 0xFE6F, // CJK compatibility forms
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

func padDisp(s string, w int) string {
	if d := w - dispWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// truncDisp shortens s to at most w display cells, appending "…" when cut.
func truncDisp(s string, w int) string {
	if dispWidth(s) <= w {
		return s
	}
	out, used := make([]rune, 0, len(s)), 0
	for _, r := range s {
		rw := runeWidth(r)
		if used+rw > w-1 {
			break
		}
		out = append(out, r)
		used += rw
	}
	return string(out) + "…"
}
