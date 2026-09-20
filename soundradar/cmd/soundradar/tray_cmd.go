package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/znz/soundradar/internal/config"
	"github.com/znz/soundradar/internal/tray"
)

const trayUsage = `soundradar tray - 系统托盘启动器

用法：
  soundradar tray [--port 8765] [--library <path>] [--config <path>] [--no-open]

它做三件事：
  1. 在系统托盘放一个图标（左键或双击 = 打开管理界面，右键 = 菜单）；
  2. 启动本程序自带的 serve --overlay（管理页面 + 实时识别 + 命中悬浮窗）；
  3. 托盘菜单「退出 soundradar」结束时，连同子进程一起停掉。

双击 soundradar.exe（不带任何参数）等价于执行这条命令。

选项：
  --port N        管理页面端口（默认 8765；被占用时自动往后找空闲端口）
  --library PATH  音效库文件（默认 <exe 目录>\data\library.srz）
  --config PATH   设置文件（默认 <exe 目录>\config.json）
  --no-open       启动后不自动打开浏览器
  --no-serve      只放托盘图标，不启动 serve（调试用）
`

// serverChild supervises the `serve --overlay` process the tray keeps alive.
type serverChild struct {
	mu    sync.Mutex
	cmd   *exec.Cmd
	port  int
	base  int
	exe   string
	cfg   string
	lib   string
	open  bool
}

// start launches the server unless one is already running, and returns the port
// the management page answers on.
//
// Single instance by construction: the configured port is probed BEFORE a child
// is started (not after a failed bind), so a second double click reuses the
// running server instead of starting a second one. Two servers would mean two
// tray icons and two attempts to capture the same loopback endpoint.
func (s *serverChild) start() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		return s.port, nil
	}
	if s.port != 0 && portListening(s.port) {
		return s.port, nil
	}
	want := s.base
	if want == 0 {
		want = 8765
	}
	if portListening(want) {
		s.port = want
		return want, nil
	}
	port, free := pickPort(want)
	if !free {
		s.port = port
		return port, nil
	}
	args := s.childArgs(port)
	cmd := exec.Command(s.exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("启动 serve 失败: %w", err)
	}
	// Tie the child's lifetime to this process, so killing the launcher (Task
	// Manager, a crash) cannot leave a recording server running with no tray
	// icon to stop it. A graceful exit still stops the child properly first.
	adoptChild(cmd)
	s.cmd = cmd
	s.port = port
	go func() {
		werr := cmd.Wait()
		s.mu.Lock()
		if s.cmd == cmd {
			s.cmd = nil
		}
		s.mu.Unlock()
		if werr != nil {
			fmt.Fprintf(os.Stderr, "[tray] serve 已退出: %v\n", werr)
		}
	}()
	return port, nil
}

// childArgs builds the command line for the serve child process.
//
// It is a separate function so a test can lock down the one property that is
// easy to regress and annoying to debug: the child must NOT be started with
// --tray. This launcher owns the notification-area icon, and a second icon from
// the child shows up as a duplicate (with a second "exit" entry that would only
// stop the child). --tray remains a serve flag for someone running serve
// directly from a shell.
func (s *serverChild) childArgs(port int) []string {
	args := []string{"serve", "--overlay", "--port", fmt.Sprint(port)}
	if s.cfg != "" {
		args = append(args, "--config", s.cfg)
	}
	if s.lib != "" {
		args = append(args, "--library", s.lib)
	}
	if s.open {
		args = append(args, "--open")
	}
	return args
}

// stop shuts the child down, first politely (so it can flush the library and
// unregister its hotkey) and then forcefully.
func (s *serverChild) stop() {
	s.mu.Lock()
	cmd := s.cmd
	s.cmd = nil
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		_ = cmd.Process.Kill()
	}
}

// running reports whether this launcher owns a live server process.
func (s *serverChild) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil
}

// portOf returns the port the child was started on (0 before start).
func (s *serverChild) portOf() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// runTray implements `soundradar tray` and the double-click path: one visible
// face in the notification area plus a supervised server child process.
func runTray(args []string) error {
	fs := flag.NewFlagSet("tray", flag.ContinueOnError)
	port := fs.Int("port", 8765, "management UI port")
	libPath := fs.String("library", "", "library file path")
	cfgPath := fs.String("config", "", "settings file path")
	noOpen := fs.Bool("no-open", false, "do not open the browser")
	noServe := fs.Bool("no-serve", false, "only show the tray icon")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("--port 无效: %d（应在 1-65535 之间）", *port)
	}

	// Resolve the config so the tray can point its menu at the right data
	// directory, and so a first run writes config.json with the defaults.
	cfgFile := strings.TrimSpace(*cfgPath)
	if cfgFile == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return err
		}
		cfgFile = p
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	cfg = cfg.Normalize()

	exe := exePath()
	if exe == "" {
		return errors.New("无法确定自身可执行文件路径")
	}

	srv := &serverChild{
		exe:  exe,
		base: *port,
		cfg:  cfgFile,
		lib:  strings.TrimSpace(*libPath),
		open: !*noOpen,
	}

	// panelURL is the address of whichever server is (or will be) running.
	panelURL := func() (string, error) {
		if p := srv.portOf(); p != 0 && portListening(p) {
			return fmt.Sprintf("http://127.0.0.1:%d/", p), nil
		}
		p, err := srv.start()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("http://127.0.0.1:%d/", p), nil
	}
	openPanel := func() error {
		u, err := panelURL()
		if err != nil {
			return err
		}
		return tray.OpenURL(u)
	}

	t, err := tray.New(tray.Options{
		Tooltip: "SoundRadar 声音识别（双击打开管理界面）",
		ICO:     trayIconICO,
	})
	if err != nil {
		return fmt.Errorf("创建系统托盘图标失败: %w", err)
	}
	defer t.Close()
	t.OnActivate(func() {
		if err := openPanel(); err != nil {
			fmt.Fprintf(os.Stderr, "[tray] 打开管理界面失败: %v\n", err)
		}
	})
	t.OnAction(func(a tray.Action) {
		switch a {
		case tray.ActionOpenPanel, tray.ActionLiveStatus:
			if err := openPanel(); err != nil {
				fmt.Fprintf(os.Stderr, "[tray] 打开管理界面失败: %v\n", err)
			}
		case tray.ActionToggleOverlay:
			// The overlay is toggled with its own global hotkey, which the serve
			// child owns; synthesising a key press is exactly what this program
			// never does, so tell the user instead.
			key := cfg.Hotkeys.ToggleOverlay
			if key == "" || strings.EqualFold(key, "none") {
				key = "F9"
			}
			_ = t.Notify("显示 / 隐藏悬浮窗", "按 "+key+" 切换（游戏里也可以按）")
		case tray.ActionCandidates:
			dir, derr := config.ResolveRecallDir(cfg.Recall.Dir)
			if derr != nil {
				fmt.Fprintf(os.Stderr, "[tray] 候选项目录不可用: %v\n", derr)
				return
			}
			if err := tray.OpenURL(dir); err != nil {
				fmt.Fprintf(os.Stderr, "[tray] 打开候选项目录失败: %v\n", err)
			}
		case tray.ActionOpenDataDir:
			if err := tray.OpenURL(filepath.Dir(cfgFile)); err != nil {
				fmt.Fprintf(os.Stderr, "[tray] 打开数据目录失败: %v\n", err)
			}
		case tray.ActionQuit:
			srv.stop()
			_ = t.Close()
		}
	})

	if *noServe {
		_ = t.Notify("SoundRadar 托盘", "只启动了托盘图标（--no-serve）")
	} else {
		p, serr := srv.start()
		if serr != nil {
			_ = t.Notify("SoundRadar 启动失败", serr.Error())
			return serr
		}
		_ = t.Notify("SoundRadar 已在运行",
			fmt.Sprintf("识别已开始。管理界面 http://127.0.0.1:%d/ ，右键托盘图标可退出。", p))
	}

	// Ctrl+C in the console that started us stops everything too.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	srv.stop()
	_ = t.Close()
	return nil
}

// pickPort returns the first free port at or after want, scanning 20 ports.
// free=false means the very first port is already in use.
func pickPort(want int) (port int, free bool) {
	for i := 0; i < 20; i++ {
		p := want + i
		if p > 65535 {
			break
		}
		if !portListening(p) {
			return p, true
		}
	}
	return want, false
}

// portListening reports whether something accepts TCP connections on
// 127.0.0.1:port.
func portListening(port int) bool {
	if port <= 0 {
		return false
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
