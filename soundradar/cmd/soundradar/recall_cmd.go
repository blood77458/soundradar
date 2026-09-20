package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/znz/soundradar/internal/config"
	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/live"
	"github.com/znz/soundradar/internal/match"
	"github.com/znz/soundradar/internal/recall"
	"github.com/znz/soundradar/internal/wav"
)

// runRecall implements `soundradar recall`: the no-keyboard path through the
// exact same code the F8 hotkey uses.
//
// It is the main end-to-end acceptance tool for P4 because it needs nothing that
// cannot be scripted: open the loopback endpoint, run the recognition link (so
// the ring gets real audio through live.Options.OnBlock), trigger a recall save
// at --at seconds, then exit and print what was saved.
//
// `--at` triggers Save() - the same recall.Recaller.Save the hotkey callback
// would call - so this subcommand proves the capture -> ring -> WAV path without
// synthesising a single key press.
const recallUsage = `soundradar recall - 回溯保存：把"刚才那几秒"存成候选项（不需要按键）

用法:
  soundradar recall [选项]

选项:
  --seconds N      总共采集多少秒（默认 10）
  --at N           在第 N 秒触发一次回溯保存（默认 2，0 = 采集结束再保存）
  --device SUBSTR  采集端点（渲染设备名的子串；留空 = 配置文件里的端点，再留空 = 系统默认）
  --library PATH   音效库文件（默认 <exe 目录>\data\library.srz）
  --index PATH     指纹索引文件（默认与库文件同目录的 index.bin）
  --out PATH       额外把这次快照写到这个 WAV（可选；候选项本身总是写进收件箱）
  --wav PATH       用音频文件当音源（离线再验一次，不需要声卡）
  --realtime       回放 --wav 时按真实速度播放
  --dir PATH       候选项目录（默认取配置里的 recall.dir，再默认 data/candidates）
  --ring N         环形缓冲秒数（默认取配置里的 recall.seconds，再默认 3）
  --config PATH    配置文件路径（默认 <exe 目录>\config.json）
  --json           只输出一行 JSON（脚本断言用）
  --quiet          只打印保存结果

说明:
  存下来的 WAV 恒为 48000 Hz / 16-bit / 单声道，长度 = 环形缓冲秒数（可用 --ring 覆盖）。
  "猜测"来自保存瞬间索引里的 top-1，和悬浮窗/实时面板看到的是同一条链路。
  本子命令同样不会模拟按键：它直接调用回溯保存函数。`

// recallJSON is the machine-readable result of one `recall` run.
type recallJSON struct {
	OK        bool     `json:"ok"`
	Saved     bool     `json:"saved"`
	Path      string   `json:"path,omitempty"`
	Candidate string   `json:"candidateId,omitempty"`
	HasSaved  bool     `json:"hasSavedFile"`
	Seconds   float64  `json:"seconds"`
	PeakDBFS  *float64 `json:"peakDbfs"`
	Silent    bool     `json:"silent"`
	Frames    int64    `json:"frames"`
	GuessID   string   `json:"guessId,omitempty"`
	GuessName string   `json:"guessName,omitempty"`
	Guess     *float64 `json:"guessScore,omitempty"`
	Dir       string   `json:"dir"`
	Library   string   `json:"library"`
	Device    string   `json:"device"`
	Source    string   `json:"source"`
	Hotkey    string   `json:"hotkey,omitempty"`
	TriggerAt float64  `json:"triggerAt"`
	ElapsedS  float64  `json:"elapsedSeconds"`
	AudioBlks int64    `json:"audioBlocks"`
	AudioSec  float64  `json:"audioSeconds"`
	Dropped   int64    `json:"droppedBlocks"`
	Error     string   `json:"error,omitempty"`
}

func runRecall(args []string) error {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	seconds := fs.Float64("seconds", 10, "total capture seconds")
	at := fs.Float64("at", 2, "trigger the save at N seconds (0 = at the end)")
	device := fs.String("device", "", "case-insensitive substring of the render endpoint name")
	libPath := fs.String("library", "", "library file (default <exe dir>/data/library.srz)")
	idxPath := fs.String("index", "", "index file (default <library dir>/index.bin)")
	out := fs.String("out", "", "also write this snapshot to this WAV path")
	wavPath := fs.String("wav", "", "replay this file instead of capturing (offline)")
	realtime := fs.Bool("realtime", false, "pace a --wav replay at real speed")
	dirFlag := fs.String("dir", "", "candidate directory (default: config recall.dir)")
	ringFlag := fs.Int("ring", 0, "ring seconds (default: config recall.seconds)")
	cfgPath := fs.String("config", "", "config file (default <exe dir>/config.json)")
	asJSON := fs.Bool("json", false, "print one JSON object")
	quiet := fs.Bool("quiet", false, "print only the saved candidate")
	fs.SetOutput(os.Stdout)
	fs.Usage = func() { fmt.Fprint(fs.Output(), recallUsage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *seconds <= 0 {
		return fmt.Errorf("--seconds 必须 > 0，当前 %v", *seconds)
	}
	if *at < 0 {
		return fmt.Errorf("--at 不能为负，当前 %v", *at)
	}
	if *ringFlag < 0 {
		return fmt.Errorf("--ring 不能为负，当前 %d", *ringFlag)
	}

	result := recallJSON{OK: false, TriggerAt: *at, PeakDBFS: nil}
	started := time.Now()

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
	if strings.TrimSpace(*device) != "" {
		cfg.Capture.Device = *device
	}

	// The ring length comes from the config unless --ring overrides it.
	ringSeconds := cfg.Recall.Seconds
	if *ringFlag > 0 {
		ringSeconds = *ringFlag
	}
	if ringSeconds < 1 || ringSeconds > 30 {
		return fmt.Errorf("环形缓冲秒数必须在 1..30 之间，当前 %d（配置文件 %s 的 recall.seconds=%d）",
			ringSeconds, cfgFile, cfg.Recall.Seconds)
	}

	// ---- candidate inbox -------------------------------------------------
	dir := strings.TrimSpace(*dirFlag)
	if dir == "" {
		dir = cfg.Recall.Dir
	}
	dir, err = config.ResolveRecallDir(dir)
	if err != nil {
		return err
	}
	maxFiles := cfg.Recall.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 200
	}
	store, err := recall.NewStore(dir, maxFiles)
	if err != nil {
		return err
	}
	rc := recall.NewRecaller(ringSeconds, store)
	result.Dir = store.Dir()

	// ---- library + index -------------------------------------------------
	lib, err := resolveLibraryPath(*libPath)
	if err != nil {
		return err
	}
	result.Library = lib
	idx := strings.TrimSpace(*idxPath)
	if idx == "" {
		idx = index.DefaultPathFor(lib)
	}
	idx, err = filepath.Abs(idx)
	if err != nil {
		return fmt.Errorf("索引路径无效: %w", err)
	}
	params := dspParamsFor(cfg.Noise)
	ix, rebuilt, why, ixErr := index.LoadOrBuild(lib, idx, params)
	var ixNote string
	if ixErr != nil {
		// No usable index means no guess, but the recall itself must still work:
		// saving "what I just heard" does not need a library at all.
		ixNote = "索引不可用（" + ixErr.Error() + "），本次不填猜测"
		ix = nil
	} else if ix.Empty() {
		ixNote = fmt.Sprintf("索引为空（库 %s 里没有可用样本），本次不填猜测", lib)
		ix = nil
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
				reportCaptureFailure(os.Stderr, err)
				return fmt.Errorf("打开采集端点失败: %w", err)
			}
			fmt.Fprintf(os.Stderr, "[recall] 端点 %q 打不开（%v），回退到系统默认端点\n", cfg.Capture.Device, err)
			src, err = live.NewCaptureSource("")
			if err != nil {
				reportCaptureFailure(os.Stderr, err)
				return fmt.Errorf("打开默认采集端点失败: %w", err)
			}
		}
	}
	defer src.Stop()
	printSourceNote("[recall] ", src, *quiet)

	// ---- recognition link (this is what fills the ring) ------------------
	opts := live.Options{
		TopN:         1,
		SilenceDBFS:  match.DefaultOptions().SilenceDBFS,
		AdaptiveGate: params.EffectiveNoise().AdaptiveGate,
		OnBlock:      rc.PushBlock,
	}
	eng, err := live.New(src, ix, opts, func(tk live.Tick) {
		// Keep the "guess" as fresh as the ranking is: the candidate stores
		// whatever was top-1 at the instant of the save.
		rc.Tick(tk.Top)
	}, nil)
	if err != nil {
		return err
	}
	info := eng.SourceInfo()
	result.Source = info.Detail
	result.Device = info.Device

	if !*asJSON && !*quiet {
		fmt.Printf("[recall] 音源       : %s\n", info.Detail)
		if info.Device != "" {
			fmt.Printf("[recall] 采集端点   : %s\n", info.Device)
		}
		fmt.Printf("[recall] 音效库     : %s\n", lib)
		fmt.Printf("[recall] 索引       : %s", idx)
		switch {
		case rebuilt:
			fmt.Printf("（已自动重建：%s）", why)
		case ix == nil:
			fmt.Printf("（不可用）")
		default:
			fmt.Printf("（直接复用）")
		}
		fmt.Println()
		if ixNote != "" {
			fmt.Printf("[recall] 提示       : %s\n", ixNote)
		}
		fmt.Printf("[recall] 候选项目录 : %s（最多保留 %d 个，Prune 后）\n", store.Dir(), store.MaxFiles())
		fmt.Printf("[recall] 环形缓冲   : %.1f 秒（%.0f KiB）\n", rc.Ring().Seconds(), float64(rc.Ring().Cap()*2)/1024)
		fmt.Printf("[recall] 计划       : 采集 %.1f 秒，在第 %.1f 秒触发一次回溯保存\n", *seconds, *at)
	}

	// ---- run -------------------------------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Trigger time: `--at` seconds from now, or at the end for --at 0.
	trigger := time.Duration(*at * float64(time.Second))
	if *at <= 0 {
		trigger = time.Duration(*seconds * float64(time.Second))
	}
	if trigger > time.Duration(*seconds*float64(time.Second)) {
		trigger = time.Duration(*seconds * float64(time.Second))
	}

	var cand recall.Candidate
	var saveErr error
	var savedAt time.Time

	if strings.TrimSpace(*wavPath) != "" {
		// Offline replay: push the file through the recall ring directly and save
		// after --at seconds of AUDIO time.
		//
		// Why not the live engine here: with realtime=false it replays the whole
		// file as fast as the source can push it, so "--at seconds" can only mean
		// audio time, and the wall clock stops being meaningful. The file path
		// exists so the ring/store can be exercised without a sound card, and a
		// direct feed is exactly that. The real end-to-end evidence is the
		// capture run (no --wav), which goes through live.Options.OnBlock the
		// same way `overlay` and `serve` do.
		feedDone := make(chan error, 1)
		go func() { feedDone <- feedFileRecaller(*wavPath, rc, ix) }()
		deadline := time.Now().Add(trigger)
		waiting := true
		for waiting && time.Now().Before(deadline) && rc.Seconds() < float64(ringSeconds) {
			select {
			case err := <-feedDone:
				if err != nil {
					return fmt.Errorf("回放 %s 失败: %w", *wavPath, err)
				}
				waiting = false
			case <-time.After(10 * time.Millisecond):
			}
		}
		cand, saveErr = rc.Save("cli")
		savedAt = time.Now()
		result.AudioSec = rc.Seconds()
		result.AudioBlks = int64(result.AudioSec * 50) // 20 ms per block
	} else {
		runErrCh := make(chan error, 1)
		go func() { runErrCh <- eng.Run(ctx) }()

		timer := time.NewTimer(trigger)
		defer timer.Stop()
		engineDone := false
		select {
		case <-timer.C:
			// Wait until the ring actually covers the requested window, otherwise
			// a very early --at would save a shorter clip than the user asked for.
			for rc.Seconds() < float64(ringSeconds)-0.02 {
				if time.Since(started) > time.Duration(*seconds*float64(time.Second))+5*time.Second {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			cand, saveErr = rc.Save("cli")
			savedAt = time.Now()
		case <-runErrCh:
			// The source ended before the trigger; save whatever is in the ring.
			cand, saveErr = rc.Save("cli")
			savedAt = time.Now()
			engineDone = true
		}

		// Let the capture run for the rest of --seconds (unless the source ended).
		if !engineDone {
			select {
			case <-runErrCh:
				engineDone = true
			default:
			}
		}
		if !engineDone {
			remaining := time.Duration(*seconds*float64(time.Second)) - time.Since(started)
			if remaining > 0 {
				select {
				case <-time.After(remaining):
				case <-runErrCh:
					engineDone = true
				}
			}
		}
		cancel()
		if !engineDone {
			select {
			case err := <-runErrCh:
				if err != nil && !errors.Is(err, context.Canceled) && saveErr == nil {
					// A capture error after a successful save is still worth
					// reporting, but the saved candidate is the important artefact.
					result.Error = err.Error()
				}
			case <-time.After(3 * time.Second):
				if result.Error == "" {
					st := eng.Stats()
					result.Error = fmt.Sprintf("识别链路没有在 3 秒内退出（候选项已经保存，不受影响）：块 %d / 音频 %.2f s / 窗口 %d / tick %d",
						st.Blocks, st.AudioSeconds, st.Windows, st.Ticks)
				}
			}
		}
		stats := eng.Stats()
		result.AudioBlks = stats.Blocks
		result.AudioSec = stats.AudioSeconds
		result.Dropped = stats.Dropped
	}

	result.ElapsedS = time.Since(started).Seconds()
	if saveErr != nil {
		result.Error = saveErr.Error()
		if *asJSON {
			return printRecallJSON(result)
		}
		return fmt.Errorf("回溯保存失败: %w", saveErr)
	}

	result.OK = true
	result.Saved = true
	result.Candidate = cand.ID
	result.Path = cand.File
	result.Seconds = cand.Seconds
	result.Frames = cand.Frames
	result.Silent = cand.Silent
	result.GuessID = cand.GuessID
	result.GuessName = cand.GuessName
	if !cand.Silent {
		p := cand.PeakDBFS
		result.PeakDBFS = &p
	}
	if cand.GuessID != "" {
		g := cand.GuessScore
		result.Guess = &g
	}
	// This subcommand registers no hotkey at all (it IS the no-keyboard path);
	// the field exists so a script can tell "no hotkey here" from "hotkey
	// failed". The real registration result is reported by overlay/serve/live.
	result.Hotkey = "无（本子命令不注册热键）"

	// --out writes the same snapshot next to the candidate (the acceptance run
	// uses this path so wavstat can analyse it directly).
	if strings.TrimSpace(*out) != "" {
		if err := copySnapshot(cand.File, *out); err != nil {
			if *asJSON {
				result.OK = false
				result.Error = err.Error()
				return printRecallJSON(result)
			}
			return err
		}
		result.HasSaved = true
	}

	if *asJSON {
		return printRecallJSON(result)
	}
	if !*quiet {
		fmt.Printf("\n[recall] 采集 %.2f 秒（音频 %d 块 = %.2f 秒音频，丢帧 %d），保存时刻 %s\n",
			result.ElapsedS, result.AudioBlks, result.AudioSec, result.Dropped, savedAt.Format("15:04:05.000"))
	}
	fmt.Printf("[recall] 已保存候选项 %s\n", recall.Describe(cand))
	fmt.Printf("[recall] WAV        : %s\n", cand.File)
	if result.HasSaved {
		fmt.Printf("[recall] 另外写出   : %s\n", *out)
	}
	if cand.Silent {
		fmt.Printf("[recall] 峰值       : -inf dBFS（数字静音：确认一下采集端点选对了没）\n")
	} else {
		fmt.Printf("[recall] 峰值       : %s dBFS\n", wav.FormatDBFS(cand.PeakDBFS))
	}
	fmt.Printf("[recall] 时长       : %.3f 秒 / %d 个样本（48000 Hz 单声道 16-bit）\n", cand.Seconds, cand.Frames)
	if cand.GuessID != "" {
		fmt.Printf("[recall] 猜测       : %s（%s）分数 %.4f\n", cand.GuessName, cand.GuessID, cand.GuessScore)
	} else {
		fmt.Printf("[recall] 猜测       : 无（索引不可用或保存瞬间没有排名）\n")
	}
	fmt.Printf("[recall] 收件箱     : %s（用 `soundradar serve` 的「候选项」页签录入）\n", store.Dir())
	return nil
}

// copySnapshot copies the candidate WAV to path, creating the directory.
func copySnapshot(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("读取候选项音频失败: %w", err)
	}
	if dir := filepath.Dir(dst); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建输出目录 %s 失败: %w", dir, err)
		}
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		return fmt.Errorf("写出 %s 失败: %w", dst, err)
	}
	// Sanity: the WAV must be readable and carry exactly the expected format.
	if a, err := wav.ReadFile(dst); err != nil {
		return fmt.Errorf("刚写出的 %s 读不回来: %w", dst, err)
	} else if a.Info.SampleRate != recall.SampleRate || a.Info.Channels != 1 || a.Info.BitsPerSample != 16 {
		return fmt.Errorf("刚写出的 %s 格式不对: %s（期望 48000 Hz / 1 ch / 16-bit）", dst, a.Info.Layout())
	}
	return nil
}

func printRecallJSON(r recallJSON) error {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("序列化 JSON 失败: %w", err)
	}
	fmt.Println(string(b))
	return nil
}

