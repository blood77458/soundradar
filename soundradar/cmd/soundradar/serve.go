package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/znz/soundradar/internal/config"
	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/overlay"
	"github.com/znz/soundradar/internal/recall"
	"github.com/znz/soundradar/internal/server"
	"github.com/znz/soundradar/internal/tray"
)

// serveExit is signalled by the tray icon's "quit" entry so that a tray quit runs
// exactly the same graceful shutdown as Ctrl+C.
var (
	serveExitOnce sync.Once
	serveExit     = make(chan struct{})
)

// requestServeExit asks a running `serve` process to shut down. It is safe to
// call more than once.
func requestServeExit() {
	serveExitOnce.Do(func() { close(serveExit) })
}

// runServe implements the P1 `soundradar serve` subcommand: it opens (or
// creates) the sound-effect library and exposes the management API + embedded
// web UI on 127.0.0.1 only.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := fs.Int("port", 8765, "TCP port to listen on (127.0.0.1 only)")
	libPath := fs.String("library", "", "library file path (default <exe dir>/data/library.srz)")
	openBrowser := fs.Bool("open", false, "open the management UI in the default browser")
	strictPort := fs.Bool("strict-port", false, "fail instead of trying port+1 when the port is taken")
	quiet := fs.Bool("quiet", false, "suppress the startup banner")
	withOverlay := fs.Bool("overlay", false, "also create the native hit overlay window (P3)")
	withTray := fs.Bool("tray", false, "also put a soundradar icon in the notification area")
	cfgPath := fs.String("config", "", "settings file (default <exe dir>/config.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("--port 无效: %d（应在 1-65535 之间）", *port)
	}

	// ---- application config (P3) -----------------------------------------
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

	// Resolve the library path before anything else so the banner always shows
	// the path that is actually used.
	path := strings.TrimSpace(*libPath)
	if path == "" {
		p, err := library.DefaultPath()
		if err != nil {
			return err
		}
		path = p
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("库文件路径无效 %q: %w", path, err)
	}

	// The environment-noise settings are part of the fingerprint, so the index
	// and the live link are both built from this one value.
	params := dspParamsFor(cfg.Noise)

	store, err := library.Open(path)
	if err != nil {
		return fmt.Errorf("打开音效库失败: %w", err)
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	srv := server.New(server.Options{
		Store: store, Logger: logger, Config: cfg, ConfigPath: cfgFile, Params: params,
	})

	// ---- optional overlay window (P3) ------------------------------------
	var ov *overlay.Overlay
	if *withOverlay {
		icons := &storeIcons{store: store, cache: map[string]image.Image{}}
		var oerr error
		ov, oerr = overlay.New(cfg.Overlay, cfg.Hotkeys.ToggleOverlay, icons)
		if oerr != nil {
			return fmt.Errorf("创建悬浮窗失败: %w", oerr)
		}
		defer ov.Close()
		srv.SetOverlay(overlay.NewServerBridge(ov))
		logger.Printf("[serve] 悬浮窗已创建: 类名 %s", ov.ClassName())
	}

	// ---- P4 recall --------------------------------------------------------
	// `serve` runs no recognition link by default, but the candidate inbox still
	// has to work: a plain loopback capture is opened lazily so "立即保存最近 N
	// 秒" (and POST /api/recall/trigger) has real audio to save, and it also
	// scores the newest window so the candidate carries a guess.
	var rec *recallRecorder
	if cfg.Recall.Enabled {
		var rerr error
		rec, rerr = startRecaller(cfg, *quiet)
		if rerr != nil {
			return fmt.Errorf("初始化回溯保存失败: %w", rerr)
		}
		defer rec.Close()
		rec.SetNotify(func(c recall.Candidate) {
			logger.Printf("[recall] 已保存候选项 %s（%.2f 秒，峰值 %s dBFS）",
				c.ID, c.Seconds, levelText(c))
		})

		var ix *index.Index
		if built, _, _, berr := index.LoadOrBuild(path, index.DefaultPathFor(path), params); berr == nil && !built.Empty() {
			ix = built
		}

		// One loopback client at a time. While 实时识别 is running it owns the
		// endpoint and copies blocks into the ring; otherwise this fallback does.
		var capMu sync.Mutex
		var cap *captureRecorder
		var liveOn bool
		var liveEpoch uint64
		startCap := func() {
			capMu.Lock()
			defer capMu.Unlock()
			if liveOn || cap != nil {
				return
			}
			c, err := startCaptureRecorder(context.Background(), cfg.Capture.Device, rec, ix)
			if err != nil {
				logger.Printf("[serve] 回溯采集未能启动（%v）；候选项收件箱仍可用，但需要实时识别正在采音才能保存新片段", err)
				reportCaptureFailure(os.Stderr, err)
				return
			}
			cap = c
		}
		stopCap := func() {
			capMu.Lock()
			c := cap
			cap = nil
			capMu.Unlock()
			if c != nil {
				_ = c.stop()
			}
		}
		startCap()
		defer stopCap()
		srv.SetAudioFeed(rec.Buffer, func(running bool) {
			if running {
				capMu.Lock()
				liveEpoch++
				liveOn = true
				epoch := liveEpoch
				capMu.Unlock()
				_ = epoch
				stopCap()
				return
			}
			// Resume fallback off the live-stop path. A new live session bumps
			// liveEpoch so a late startCap cannot steal the endpoint again.
			capMu.Lock()
			liveOn = false
			epoch := liveEpoch
			capMu.Unlock()
			go func(ep uint64) {
				time.Sleep(150 * time.Millisecond)
				capMu.Lock()
				stillIdle := !liveOn && liveEpoch == ep && cap == nil
				capMu.Unlock()
				if !stillIdle {
					return
				}
				startCap()
			}(epoch)
		})

		srv.SetRecall(server.NewRecallSink(server.RecallOptions{
			Enabled:  true,
			Dir:      rec.Store().Dir(),
			RingSec:  rec.RingSeconds(),
			MaxFiles: rec.Store().MaxFiles(),
			CoverSec: rec.CoverSeconds,
			Hotkey:   rec.HotkeyStatus,
			Save:     rec.Save,
			Trigger:  rec.Trigger,
			List:     rec.Store().List,
			Get:      rec.Store().Get,
			WAVBytes: rec.Store().Open,
			Delete:   rec.Store().Delete,
			EnsureFunc: func() error {
				startCap()
				capMu.Lock()
				ok := cap != nil || liveOn
				capMu.Unlock()
				if !ok {
					return fmt.Errorf("回溯采集未启动")
				}
				return nil
			},
		}))
	}

	ln, boundPort, err := srv.Listen(*port, *strictPort)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", boundPort)

	if !*quiet {
		st := store.Stats()
		fmt.Printf("[serve] soundradar P1 音效库管理端\n")
		fmt.Printf("[serve] 监听地址   : 127.0.0.1:%d（仅本机，不对外网开放）\n", boundPort)
		fmt.Printf("[serve] 管理界面   : %s\n", url)
		fmt.Printf("[serve] API 前缀   : %sapi/\n", url)
		fmt.Printf("[serve] 库文件     : %s\n", st.Path)
		fmt.Printf("[serve] 库名称     : %s\n", st.Name)
		fmt.Printf("[serve] 现有条目   : %d 个，样本 %d 个，文件 %.1f KiB\n",
			st.ItemCount, st.SampleCount, float64(st.FileBytes)/1024)
		fmt.Printf("[serve] 配置文件   : %s（来源 %s）\n", pathInfo.Path, pathInfo.Source)
		if ov != nil {
			hk, hkOK := ov.HotkeyRegistered()
			if x, y, w, h, err := ov.Rect(); err == nil {
				fmt.Printf("[serve] 悬浮窗     : 已开启 %dx%d @ (%d,%d)，画布 %d×%d\n",
					w, h, x, y, canvasW(cfg), canvasH(cfg))
			} else {
				fmt.Printf("[serve] 悬浮窗     : 已开启\n")
			}
			if hkOK {
				fmt.Printf("[serve] 悬浮窗热键 : %s（RegisterHotKey 成功）\n", hk)
			} else {
				fmt.Printf("[serve] 悬浮窗热键 : %s 未注册（%s）\n", hk, ov.HotkeyError())
			}
		} else {
			fmt.Printf("[serve] 悬浮窗     : 未开启（加 --overlay 打开原生弹窗）\n")
		}
		if rec != nil {
			fmt.Printf("[serve] 回溯保存   : %s\n", rec.HotkeyStatus())
			fmt.Printf("[serve] 候选项目录 : %s（最近 %.1f 秒，最多 %d 个）\n",
				rec.Store().Dir(), rec.RingSeconds(), rec.Store().MaxFiles())
			fmt.Printf("[serve] 回溯采集   : %s。实时识别运行时由识别链路写入缓冲，避免和采集抢同一个耳机端点\n",
				orDefault(cfg.Capture.Device, "系统默认渲染端点"))
			fmt.Printf("[serve] 内部触发   : POST /api/recall/trigger（不需要按键）\n")
		} else {
			fmt.Printf("[serve] 回溯保存   : 未启用（config 里 recall.enabled=false）\n")
		}
		for _, wmsg := range store.Warnings() {
			fmt.Printf("[serve] 警告       : %s%s\n", itemPrefix(wmsg.Item), wmsg.Message)
		}
		fmt.Printf("[serve] 停止服务   : Ctrl+C\n")
	}

	if *openBrowser {
		go func() {
			time.Sleep(250 * time.Millisecond)
			if err := openURL(url); err != nil {
				logger.Printf("[serve] 无法自动打开浏览器: %v（请手动访问 %s）", err, url)
			}
		}()
	}

	// Ctrl+C / SIGTERM -> graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- optional notification-area icon --------------------------------
	// It is created last (so a failure cannot take the server down) and its
	// "quit" entry stops this process the same way Ctrl+C does.
	var trayIcon *tray.Tray
	if *withTray {
		panelURL := url
		trayIcon, err = newServeTray(trayOptions{
			PanelURL:     panelURL,
			DataDir:      filepath.Dir(cfgFile),
			CandidatesDir: func() string {
				if d, derr := config.ResolveRecallDir(cfg.Recall.Dir); derr == nil {
					return d
				}
				return ""
			}(),
			OverlayKey: cfg.Hotkeys.ToggleOverlay,
			Overlay:    ov,
			HasOverlay: ov != nil,
		})
		if err != nil {
			// A tray icon that cannot be created is not fatal: the server is
			// already listening and usable.
			logger.Printf("[serve] 托盘图标未能创建：%v", err)
		} else {
			defer trayIcon.Close()
			fmt.Printf("[serve] 托盘图标   : 已创建（左键打开管理界面，右键菜单，含退出）\n")
		}
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("HTTP 服务异常退出: %w", err)
		}
		return nil
	case <-ctx.Done():
	case <-serveExit:
	}

	stop()
	fmt.Printf("\n[serve] 收到退出信号，正在关闭…\n")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("关闭 HTTP 服务失败: %w", err)
	}
	fmt.Printf("[serve] 已停止，库文件未受影响: %s\n", path)
	return nil
}

func itemPrefix(id string) string {
	if id == "" {
		return ""
	}
	return "[" + id + "] "
}

// openURL launches the platform browser without going through a shell when it
// can be avoided.
func openURL(url string) error {
	if url == "" {
		return errors.New("empty url")
	}
	switch runtime.GOOS {
	case "windows":
		// `cmd /c start` needs an empty title argument so that a quoted URL is
		// not mistaken for the window title.
		return exec.Command("cmd", "/c", "start", "", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
