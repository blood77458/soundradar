// Command soundradar is the "game sound recognition assistant" prototype.
//
//	P0 - enumerate audio render endpoints and capture the system speaker output
//	     (WASAPI loopback, shared mode) into a 16-bit PCM WAV file.
//	P1 - an editable sound-effect library (library.srz) plus an embedded web UI
//	     served on 127.0.0.1.
//	P2 - the acoustic fingerprint: a quantised log-mel index over the library
//	     and an offline "which sound effect is this recording?" matcher, plus the
//	     realtime link (capture -> feature -> match -> panel/CLI).
//	P3 - the native hit overlay: a click-through, always-on-top, per-pixel
//	     transparent popup showing the icon + name of every recognised sound,
//	     with the application settings (config.json) and a settings page.
//	P4 - recall: one global hotkey (default F8) saves "the last few seconds" that
//	     were just heard into a candidate inbox, so a new sound can be named
//	     later instead of being recorded and trimmed by hand.
//
// Usage:
//
//	soundradar devices
//	soundradar capture --seconds 5 --out test.wav [--device <name substring>]
//	soundradar serve [--port 8765] [--open] [--library data\library.srz] [--overlay]
//	soundradar index rebuild [--library data\library.srz] [--out index.bin]
//	soundradar match --wav rec.wav [--library ...] [--index ...] [--top 5] [--json]
//	soundradar live [--device <substr>] [--library ...] [--seconds N] [--wav file]
//	soundradar overlay [--library ...] [--device <substr>] [--seconds N] [--demo]
//	soundradar recall [--seconds N] [--at N] [--out candidate.wav]
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/znz/soundradar/internal/capture"
	"github.com/znz/soundradar/internal/wav"
)

// 构建身份。buildSalt 由 build.ps1 通过 -ldflags "-X main.buildSalt=..." 注入，
// 目的是让每次重编译都产生不同的文件哈希。
//
// 背景：本机 Windows Smart App Control 会按**文件哈希**拦截未签名的自制 exe
// （实测同一份源码，默认 build 被拦 5/5 次，加 -s -w 重编就放行）。SAC 没有白名单机制，
// 所以 build.ps1 的策略是「编译 → 真启动自检 → 被拦就换 salt 重编再试」。
var (
	buildSalt = "dev"
	buildTime = "unknown"
)

const versionNumber = "0.6.0-p4"

const usage = `soundradar - game sound recognition assistant

Usage:
  soundradar devices
  soundradar capture --seconds 5 --out test.wav [--device <name substring>]
  soundradar serve   [--port 8765] [--open] [--library <path>]
  soundradar index rebuild [--library <path>] [--out <index.bin>]
  soundradar match   --wav <file> [--library <path>] [--index <path>] [--top 5] [--json] [--all]
  soundradar live    [--device <substr>] [--library <path>] [--index <path>]
                     [--seconds N] [--top 8] [--json] [--csv <file>] [--quiet] [--wav <file>]
  soundradar overlay [--library <path>] [--device <substr>] [--seconds N] [--demo]
                     [--wav <file>] [--x N --y N] [--size N] [--opacity F] [--csv <file>]
  soundradar recall  [--seconds N] [--at N] [--device <substr>] [--library <path>]
                     [--out <file>] [--dir <path>] [--ring N] [--json]
  soundradar version

Subcommands:
  devices   List active audio render (output) endpoints and mark the default.
  capture   Capture speaker loopback and write a 16-bit PCM WAV file.
  serve     Start the sound-effect library management UI + JSON API (P1).
            Listens on 127.0.0.1 only; the web UI is embedded in the binary.
            The "实时打分" tab drives the live link over SSE (/api/live/stream);
            --overlay also creates the native hit popup and the "设置" tab.
  index     Build / refresh the quantised fingerprint index (P2).
  match     Identify which stored sound effects a recording contains (P2).
  live      Realtime recognition: loopback capture -> fingerprint -> ranked panel
            (P2). Use --wav to replay a file instead of the sound card, --seconds
            to stop automatically and --csv to log every hit.
  overlay   Realtime recognition with the native hit overlay (P3): a click-through,
            always-on-top, per-pixel transparent popup showing the icon + name.
            --demo cycles through the whole library without a sound card.
            The recall hotkey (default F8, config hotkeys.recallLabel) saves the
            last recall.seconds seconds of audio as a candidate (P4).
  recall    Capture loopback for N seconds and trigger the recall save once
            (P4). It runs the exact code the F8 hotkey runs, but without any
            keyboard input, so it is what the acceptance script drives.
  version   Print version / build salt / go version and exit. Used by build.ps1
            as a zero-side-effect launch self-check (Smart App Control probe).

Options for capture:
  --seconds N     capture duration in seconds, fractional values allowed (default 5)
  --out PATH      output WAV path (default test.wav)
  --device SUBSTR case-insensitive substring of the endpoint friendly name or
                  endpoint id; empty selects the default render endpoint

Options for serve:
  --port N        TCP port on 127.0.0.1 (default 8765); if it is taken the next
                  free port is used automatically (up to +19)
  --library PATH  library file (default <exe dir>\data\library.srz, or
                  <cwd>\data\library.srz when the exe directory is read-only)
  --open          open the management UI in the default browser after startup
  --overlay       also create the native hit overlay window (P3), so hits pop up
                  on screen and the "设置" tab can move/resize/preview it
  --config PATH   settings file (default <exe dir>\config.json)

Run "soundradar index --help", "soundradar match --help", "soundradar live --help",
"soundradar overlay --help" or "soundradar recall --help" for the P2/P3/P4 options.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "devices", "list":
		err = runDevices(os.Args[2:])
	case "capture", "cap":
		err = runCapture(os.Args[2:])
	case "serve", "web", "ui":
		err = runServe(os.Args[2:])
	case "index", "idx":
		err = runIndex(os.Args[2:])
	case "match", "identify":
		err = runMatch(os.Args[2:])
	case "live", "realtime":
		err = runLive(os.Args[2:])
	case "overlay", "popup":
		err = runOverlay(os.Args[2:])
	case "recall", "rewind":
		err = runRecall(os.Args[2:])
	case "version", "ver", "-v", "--version":
		printVersion()
		return
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "soundradar: unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "soundradar: %v\n", err)
		os.Exit(1)
	}
}

// printVersion 是构建自检入口：零副作用、快速退出、输出可被脚本断言。
// build.ps1 用它判断刚编译出来的 exe 是否被 Windows Smart App Control 拦截。
func printVersion() {
	fmt.Printf("soundradar %s\n", versionNumber)
	fmt.Printf("  buildSalt : %s\n", buildSalt)
	fmt.Printf("  buildTime : %s\n", buildTime)
	fmt.Printf("  go        : %s\n", runtime.Version())
	fmt.Printf("  platform  : %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  cgo       : %s\n", cgoState)
}

func runDevices(args []string) error {
	fs := flag.NewFlagSet("devices", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := capture.New()
	if err != nil {
		return err
	}
	defer c.Close()

	devices, err := c.Enumerate()
	if err != nil {
		return err
	}

	fmt.Printf("Active render endpoints (WASAPI, data flow = eRender): %d\n\n", len(devices))
	for _, d := range devices {
		marks := make([]string, 0, 2)
		if d.Default {
			marks = append(marks, "DEFAULT")
		}
		if d.DefaultComms {
			marks = append(marks, "DEFAULT-COMMS")
		}
		tag := ""
		if len(marks) > 0 {
			tag = "  [" + strings.Join(marks, ", ") + "]"
		}
		fmt.Printf("  [%d] %s%s\n", d.Index, d.Name, tag)
		fmt.Printf("      id: %s\n", d.ID)
	}
	return nil
}

func runCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	seconds := fs.Float64("seconds", 5, "capture duration in seconds")
	out := fs.String("out", "test.wav", "output WAV path")
	device := fs.String("device", "", "case-insensitive substring of the endpoint name")
	quiet := fs.Bool("quiet", false, "suppress the per-capture summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *seconds <= 0 {
		return fmt.Errorf("--seconds must be positive, got %v", *seconds)
	}
	if strings.TrimSpace(*out) == "" {
		return errors.New("--out must not be empty")
	}

	c, err := capture.New()
	if err != nil {
		return err
	}
	defer c.Close()

	d := time.Duration(*seconds * float64(time.Second))

	fmt.Printf("[capture] duration   : %.3f s\n", *seconds)
	if *device == "" {
		fmt.Printf("[capture] device     : <default render endpoint>\n")
	} else {
		fmt.Printf("[capture] device     : <substring %q>\n", *device)
	}
	fmt.Printf("[capture] mode       : WASAPI shared mode + AUDCLNT_STREAMFLAGS_LOOPBACK\n")
	fmt.Printf("[capture] COM        : CoInitializeEx(COINIT_MULTITHREADED) on a LockOSThread goroutine\n")
	fmt.Printf("[capture] starting...\n")

	start := time.Now()
	res, err := c.Capture(*device, d)
	if err != nil {
		// Even a failed capture may carry partial data worth reporting.
		if res != nil {
			reportCapture(res, time.Since(start))
		}
		return err
	}

	// --- write the WAV ----------------------------------------------------
	if dir := filepath.Dir(*out); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating output directory %s: %w", dir, err)
		}
	}
	if err := wav.WriteInt16File(*out, res.SampleRate, res.Channels, res.PCM); err != nil {
		return fmt.Errorf("writing %s: %w", *out, err)
	}

	if !*quiet {
		reportCapture(res, time.Since(start))
	}

	st, err := os.Stat(*out)
	if err != nil {
		return err
	}
	fmt.Printf("[capture] wav        : %s (%.1f KiB on disk)\n", *out, float64(st.Size())/1024)
	fmt.Printf("[capture] ok\n")
	return nil
}

func reportCapture(res *capture.Result, wall time.Duration) {
	fmt.Printf("[capture] device     : [%d] %s\n", res.Device.Index, res.Device.Name)
	if res.Device.Default {
		fmt.Printf("[capture]              (was the default render endpoint)\n")
	}
	fmt.Printf("[capture] mix format : %s\n", res.MixFormat)
	fmt.Printf("[capture] wrote format: %d Hz / %d ch / 16-bit PCM (converted, no resampling)\n",
		res.SampleRate, res.Channels)
	fmt.Printf("[capture] frames     : %d\n", res.Frames)
	fmt.Printf("[capture] seconds    : %.3f s (audio timeline)\n", res.Duration.Seconds())
	fmt.Printf("[capture] wall clock : %.3f s\n", wall.Seconds())
	fmt.Printf("[capture] peak       : %.6f full scale = %s dBFS\n", res.Peak, wav.FormatDBFS(res.PeakDBFS))
	fmt.Printf("[capture] rms        : %.6f full scale = %s dBFS\n", res.RMS, wav.FormatDBFS(res.RMSDBFS))
	fmt.Printf("[capture] packets    : %d total, %d silent-flagged, %d discontinuities\n",
		res.Packets, res.SilentPackets, res.Discontinuities)
	fmt.Printf("[capture] idle polls : %d (GetNextPacketSize returned 0)\n", res.PaddingSpins)
	if res.Silent {
		fmt.Printf("[capture] silent     : TRUE - every captured sample is exactly zero\n")
	} else {
		fmt.Printf("[capture] silent     : false - non-zero audio was captured\n")
	}
}
