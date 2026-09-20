// Package capture is the platform-independent entry point for grabbing the
// audio that the system is currently playing (speaker loopback).
//
// The concrete implementation lives in loopback_windows.go (WASAPI shared-mode
// loopback via github.com/moutend/go-wca, CGO-free). A stub implementation
// covers every other platform so the module still builds everywhere.
package capture

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors returned by the capture implementations.
var (
	// ErrNoEndpoint means the machine has no active render endpoint at all
	// (headless VM, no audio driver, ...).
	ErrNoEndpoint = errors.New("capture: no active audio render endpoint found")
	// ErrDeviceNotFound means --device <substr> matched nothing.
	ErrDeviceNotFound = errors.New("capture: no render endpoint matches the given name")
	// ErrUnsupported is returned on non-Windows platforms.
	ErrUnsupported = errors.New("capture: speaker loopback is only implemented on Windows")
)

// SampleFormat describes the PCM layout of a stream.
type SampleFormat struct {
	FormatTag     uint16 // 0x0001 PCM, 0x0003 IEEE float, 0xFFFE extensible
	SampleRate    int
	Channels      int
	BitsPerSample int
	Float         bool
	Extensible    bool
}

// Kind renders the sample format as "PCM" or "IEEE float".
func (f SampleFormat) Kind() string {
	if f.Float {
		return "IEEE float"
	}
	return "PCM"
}

// String renders a compact human-readable description.
func (f SampleFormat) String() string {
	ext := ""
	if f.Extensible {
		ext = ", WAVE_FORMAT_EXTENSIBLE"
	}
	return fmt.Sprintf("%d Hz / %d ch / %d-bit %s (tag=0x%04X%s)",
		f.SampleRate, f.Channels, f.BitsPerSample, f.Kind(), f.FormatTag, ext)
}

// Device is one audio endpoint as reported by the OS.
type Device struct {
	// Index is the stable position in the enumeration (0-based).
	Index int
	// ID is the WASAPI endpoint ID string.
	ID string
	// Name is PKEY_Device_FriendlyName.
	Name string
	// Default marks the default render endpoint for the console role, i.e.
	// the "Default Device" shown in Windows Settings.
	Default bool
	// DefaultComms marks the default endpoint for the communications role
	// ("Default Communication Device").
	DefaultComms bool
}

// EndpointFailure is one endpoint that could not be opened for loopback,
// together with the reason. Recovery guidance is derived from Reason by the
// caller (see DescribeFailure), so the text lives in one place.
type EndpointFailure struct {
	Device Device
	// Reason is the Chinese explanation produced by the capture layer, which
	// already names the symbolic HRESULT and what usually causes it.
	Reason string
}

// EndpointReport is what a startup banner prints about one endpoint.
func (f EndpointFailure) String() string {
	return fmt.Sprintf("[%d] %s：%s", f.Device.Index, f.Device.Name, f.Reason)
}

// TroubleshootingSteps is the ordered list of things to change on the machine
// when no render endpoint can be opened for shared-mode loopback. It is
// deliberately ordered by how often each cause turns out to be the real one.
//
// It lives in this platform-independent file because every front end (the CLI,
// the server banner, the tray) prints the same list, and because the wording is
// a product decision rather than a Windows detail.
var TroubleshootingSteps = []string{
	"关掉该播放设备的“音频增强 / 音效”：设置 → 系统 → 声音 → 选中该设备 → 属性 → 高级 → 取消“启用音频增强”；并在 Realtek Audio Console / Nahimic / 杜比 / DTS / Sonic Studio 里关掉均衡器与音效。",
	"关掉空间音效：设置 → 系统 → 声音 → 该设备 → 空间音效 → 关闭（Windows Sonic / 杜比全景声 / DTS 都关）。",
	"蓝牙耳机：在“声音设置”里选立体声（Stereo / A2DP），不要选“免提 / 通话 / Hands-Free”。",
	"关掉可能独占声卡的程序：播放器、直播/录音软件、带 ASIO 的软件、其它录屏工具。",
	"确认音频服务在运行：Win+R → services.msc → “Windows Audio”(Audiosrv) 与 “Windows Audio Endpoint Builder”(AudioEndpointBuilder) 都应为“正在运行”。",
	"更新或重装声卡驱动；插拔一次耳机/USB 声卡，让端点重新枚举。",
	"实在不行就用虚拟声卡兜底：装 VB-Cable，把游戏声音同时输出到它，然后用 soundradar devices --probe 找到它的端点，--device \"CABLE\" 指定。",
}

// PrintTroubleshooting writes the ordered guidance with the diagnostic commands.
func PrintTroubleshooting(w io.Writer) {
	fmt.Fprintf(w, "\n按顺序试这几件事（每改一项就重跑一次）：\n")
	for i, s := range TroubleshootingSteps {
		fmt.Fprintf(w, "  %d. %s\n", i+1, s)
	}
	fmt.Fprintf(w, "\n诊断命令：\n")
	fmt.Fprintf(w, "  soundradar devices --probe       逐个端点试开一次，看谁可用、各自的混合格式\n")
	fmt.Fprintf(w, "  soundradar capture --seconds 3 --out test.wav --device \"<名字片段>\"\n")
}

// PrintEndpointFailureReport is what a command prints when it cannot capture at
// all. It lists every endpoint with its status and then what to do, so the user
// never has to interpret an HRESULT.
//
// A failure that is NOT about the audio device (a typo in --device, or a
// platform without loopback) only gets the endpoint list and a short hint: the
// full "turn off audio enhancements" list would be noise there.
func PrintEndpointFailureReport(w io.Writer, devices []Device, failures []EndpointFailure, cause error) {
	deviceProblem := isDeviceProblem(cause)

	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "╔══════════════════════════════════════════════════════════════════╗\n")
	if deviceProblem {
		fmt.Fprintf(w, "║  音频采集无法启动：现在收不到任何系统声音，识别不会有结果        ║\n")
	} else {
		fmt.Fprintf(w, "║  音频采集无法启动                                                ║\n")
	}
	fmt.Fprintf(w, "╚══════════════════════════════════════════════════════════════════╝\n")

	if len(devices) == 0 {
		fmt.Fprintf(w, "\n这台机器上没有任何活动的播放端点（eRender）。\n")
		fmt.Fprintf(w, "请先确认声卡驱动已安装、耳机/音箱已插好，然后在“声音设置”里能看到播放设备。\n")
		PrintTroubleshooting(w)
		return
	}

	bad := map[int]string{}
	for _, f := range failures {
		bad[f.Device.Index] = f.Reason
	}
	fmt.Fprintf(w, "\n这台机器上的播放端点：\n")
	for _, d := range devices {
		marks := ""
		if d.Default {
			marks += " [默认]"
		}
		if d.DefaultComms {
			marks += " [默认通讯]"
		}
		if reason, isBad := bad[d.Index]; isBad {
			fmt.Fprintf(w, "  [%d] %s%s  —— 打不开\n", d.Index, d.Name, marks)
			fmt.Fprintf(w, "       原因：%s\n", reason)
			continue
		}
		fmt.Fprintf(w, "  [%d] %s%s\n", d.Index, d.Name, marks)
	}

	if cause != nil {
		fmt.Fprintf(w, "\n程序报的错：\n  %v\n", cause)
	}

	if !deviceProblem {
		fmt.Fprintf(w, "\n--device 没有匹配到任何端点。用上面列出的名字（或它的一部分，不区分大小写）重试，\n")
		fmt.Fprintf(w, "例如：soundradar capture --seconds 3 --out test.wav --device \"%s\"\n",
			firstDeviceHint(devices))
		return
	}
	PrintTroubleshooting(w)
}

// firstDeviceHint returns a usable --device snippet from the first endpoint: the
// name inside the parentheses when there is one ("扬声器 (Realtek(R) Audio)" ->
// "Realtek"), otherwise the whole friendly name.
func firstDeviceHint(devices []Device) string {
	if len(devices) == 0 {
		return "<名字片段>"
	}
	name := devices[0].Name
	if i := strings.IndexByte(name, '('); i > 0 {
		inner := name[i+1:]
		if j := strings.IndexByte(inner, ')'); j > 1 {
			inner = inner[:j]
		}
		// Cut at the next parenthesis so "Realtek(R) Audio" suggests "Realtek".
		if k := strings.IndexByte(inner, '('); k > 1 {
			inner = inner[:k]
		}
		if s := strings.TrimSpace(inner); s != "" {
			return s
		}
	}
	return strings.TrimSpace(name)
}

// isDeviceProblem reports whether the failure is about the audio device (and so
// deserves the audio troubleshooting list) rather than about the request.
func isDeviceProblem(err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrDeviceNotFound):
		return false
	case errors.Is(err, ErrUnsupported):
		return false
	}
	return true
}


// Result is one finished loopback capture.
type Result struct {
	// Device that was captured.
	Device Device
	// MixFormat is the engine mix format the endpoint was opened with.
	MixFormat SampleFormat
	// PCM holds interleaved 16-bit PCM, len(PCM) == Frames*Channels.
	PCM []int16
	// Frames is the number of sample frames actually captured.
	Frames int64
	// Channels is len(PCM)/Frames.
	Channels int
	// SampleRate is the mix-format sample rate (no resampling is done).
	SampleRate int
	// Duration is Frames/SampleRate.
	Duration time.Duration
	// Wall is the wall-clock time the capture loop ran for.
	Wall time.Duration

	// Peak is the linear peak in [0,1]; PeakDBFS is its dBFS value.
	Peak     float64
	PeakDBFS float64
	RMS      float64
	RMSDBFS  float64
	// Silent reports whether every captured sample was exactly zero.
	Silent bool

	// Packets is the number of non-empty packets delivered by WASAPI.
	Packets int64
	// SilentPackets counts packets flagged AUDCLNT_BUFFERFLAGS_SILENT.
	SilentPackets int64
	// Discontinuities counts packets flagged AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY.
	Discontinuities int64
	// PaddingSpins counts GetCurrentPadding/GetNextPacketSize polls that
	// returned no data (used to show the polling really happened).
	PaddingSpins int64
}

// Capturer is the platform-independent capture interface.
//
// Implementations are NOT goroutine-safe and must be used from the single
// goroutine that created them, because the Windows implementation pins that
// goroutine to its OS thread for COM.
type Capturer interface {
	// Enumerate lists the active audio render endpoints.
	Enumerate() ([]Device, error)
	// Capture records speaker loopback for d.
	//
	// deviceSubstr selects the endpoint by case-insensitive substring match on
	// the friendly name (or endpoint ID); an empty string selects the default
	// render endpoint. When the selected endpoint cannot be opened, the other
	// active endpoints are tried in order and the substitution is reported.
	Capture(deviceSubstr string, d time.Duration) (*Result, error)
	// Probe tries to open every active render endpoint for shared-mode loopback
	// and reports which ones work. It captures nothing; it exists so
	// "IAudioClient::Initialize failed" can be diagnosed before a game session
	// (`soundradar devices --probe`).
	Probe() ([]DeviceProbe, error)
	// Close releases the COM apartment and every COM object still held.
	Close() error
}

// MatchDevice picks the endpoint whose friendly name (or ID) contains substr,// case-insensitively. An empty substr selects the default endpoint, falling
// back to the first endpoint when the OS reports no default.
func MatchDevice(devices []Device, substr string) (Device, error) {
	if len(devices) == 0 {
		return Device{}, ErrNoEndpoint
	}
	if strings.TrimSpace(substr) == "" {
		for _, d := range devices {
			if d.Default {
				return d, nil
			}
		}
		return devices[0], nil
	}

	needle := strings.ToLower(substr)
	for _, d := range devices {
		if strings.Contains(strings.ToLower(d.Name), needle) || strings.Contains(strings.ToLower(d.ID), needle) {
			return d, nil
		}
	}
	return Device{}, fmt.Errorf("%w: %q", ErrDeviceNotFound, substr)
}

// ---------------------------------------------------------------------------
// Streaming capture (added for the P2 realtime link)
// ---------------------------------------------------------------------------

// ErrStreamClosed is returned by Stream.Read after Close.
var ErrStreamClosed = errors.New("capture: stream is closed")

// Stream is an open-ended loopback capture: Read returns the next block of
// interleaved 16-bit PCM as long as the endpoint is delivering audio.
//
// Why it exists next to Capturer.Capture: Capture records a fixed duration and
// keeps the whole recording in memory (fine for `soundradar capture --seconds
// 5`), while the realtime recognition link has to run for hours. Stream never
// buffers more than a fixed number of blocks: when the consumer falls behind,
// the oldest blocks are counted in Dropped() and discarded instead of blocking
// the WASAPI polling goroutine or growing without bound.
//
// A Stream is safe to Read from one goroutine at a time; Close may be called
// from any goroutine.
type Stream struct {
	// Device is the endpoint that was opened; Format is the engine mix format
	// the endpoint was opened with (the samples Read returns use it unchanged).
	Device Device
	Format SampleFormat
	// Note explains a non-obvious setup - a skipped endpoint, or a format other
	// than the endpoint's own that had to be negotiated. It is empty when the
	// requested endpoint accepted its own mix format.
	Note string
	// Skipped lists the endpoints that were tried and refused BEFORE Device, in
	// the order they were tried, with the reason each one failed. It is what a
	// startup banner needs to tell the user "your default endpoint is broken,
	// here is why, and here is what to do".
	Skipped []EndpointFailure

	// queueDepth is the number of blocks buffered before dropping.
	queueDepth int

	next    func() ([]int16, error)
	release func() error

	mu     sync.Mutex
	closed bool
	err    error
	drops  *atomic.Int64

	// silent / discontig count WASAPI packet flags (loopback reports silent
	// packets while nothing plays, and flags discontinuities when the stream
	// glitches). They are diagnostics for the realtime banner.
	silent    *atomic.Int64
	discontig *atomic.Int64
}

// Read returns the next audio block. len(block) is a multiple of
// Format.Channels; the samples are at Format.SampleRate (no conversion is
// done here). Read returns io.EOF when the endpoint stopped delivering and
// ErrStreamClosed after Close.
func (s *Stream) Read() ([]int16, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrStreamClosed
	}
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	blk, err := s.next()
	if err != nil {
		if errors.Is(err, ErrStreamClosed) {
			return nil, err
		}
		s.mu.Lock()
		if s.err == nil {
			s.err = err
		}
		s.mu.Unlock()
		return nil, err
	}
	return blk, nil
}

// Err returns the error that stopped the stream, if any.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Dropped reports how many audio blocks were discarded because the consumer
// was not keeping up. The realtime link surfaces this number so a saturated
// pipeline is visible instead of silently losing audio.
func (s *Stream) Dropped() int64 {
	if s.drops == nil {
		return 0
	}
	return s.drops.Load()
}

// SilentBlocks reports how many packets the audio engine flagged as silent
// (loopback keeps delivering those while nothing is playing).
func (s *Stream) SilentBlocks() int64 {
	if s.silent == nil {
		return 0
	}
	return s.silent.Load()
}

// Discontinuities reports how many packets were flagged as discontinuous or
// timestamp-broken.
func (s *Stream) Discontinuities() int64 {
	if s.discontig == nil {
		return 0
	}
	return s.discontig.Load()
}

// Close stops the capture goroutine and releases every COM object. It is
// idempotent and safe to call concurrently with Read.
func (s *Stream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	if s.release != nil {
		return s.release()
	}
	return nil
}
