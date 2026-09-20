//go:build windows

package capture

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// hresultUnsupportedFormat is AUDCLNT_E_UNSUPPORTED_FORMAT, the error this file
// exists for:
//
//	IAudioClient::Initialize(shared/loopback) failed: error 2290679816
//
// 2290679816 == 0x88890008. It means the endpoint refused the format it had
// just reported as its own mix format, which in practice is something about the
// endpoint's DSP chain (audio "enhancements", spatial sound, an exclusive-mode
// app holding the device, or a Bluetooth headset that switched into hands-free
// mode) rather than anything about the requested buffer.
const hresultUnsupportedFormat = 0x88890008

// explainInitializeError turns an Initialize failure into text that says what to
// do, instead of the bare HRESULT (Windows has no message text for the AUDCLNT_*
// codes, so the raw error prints as "The system cannot find message text for
// message number 0x%1...").
func explainInitializeError(err error, format string) string {
	if err == nil {
		return ""
	}
	code := hresultOf(err)
	msg := fmt.Sprintf("%v", err)
	switch code {
	case hresultUnsupportedFormat:
		return fmt.Sprintf("AUDCLNT_E_UNSUPPORTED_FORMAT (0x%08X)：这个端点拒绝了自己的混合格式 %s。"+
			"常见原因：该设备的“音频增强/音效”被打开、开了空间音效（Windows Sonic / 杜比 / DTS）、"+
			"有程序用独占模式占了它，或者蓝牙耳机切到了“免提/通话”模式。"+
			"可先关掉增强与空间音效再试，或换一个端点（见 --device）。原始错误：%s",
			code, format, msg)
	case hresultDeviceInUse:
		return fmt.Sprintf("AUDCLNT_E_DEVICE_IN_USE (0x%08X)：端点正被独占占用。"+
			"关掉正在使用它的程序（部分播放器/游戏/录音软件会独占声卡）后重试。原始错误：%s", code, msg)
	case hresultDeviceInvalidated:
		return fmt.Sprintf("AUDCLNT_E_DEVICE_INVALIDATED (0x%08X)：端点在打开过程中被移除或默认设备变了。"+
			"插拔耳机后重试即可。原始错误：%s", code, msg)
	case hresultEndpointCreateFailed:
		return fmt.Sprintf("AUDCLNT_E_ENDPOINT_CREATE_FAILED (0x%08X)：音频引擎无法为该端点创建客户端，"+
			"通常是驱动或音频服务异常（可试着重启 Windows Audio 服务）。原始错误：%s", code, msg)
	case hresultServiceNotRunning:
		return fmt.Sprintf("AUDCLNT_E_SERVICE_NOT_RUNNING (0x%08X)：Windows Audio 服务没有运行。"+
			"在服务管理器里启动 Audiosrv。原始错误：%s", code, msg)
	}
	return msg
}

// Additional AUDCLNT_* codes worth naming in a diagnostic.
const (
	hresultDeviceInUse          = 0x8889000A
	hresultEndpointCreateFailed = 0x8889000F
	hresultServiceNotRunning    = 0x88890010
)

// hresultOf extracts the HRESULT from a go-ole error (0 when it is not one).
func hresultOf(err error) uint32 {
	var oleErr *ole.OleError
	if errors.As(err, &oleErr) {
		return uint32(oleErr.Code())
	}
	return 0
}

// endpointSession is one successfully opened shared-mode loopback client: the
// endpoint, its mix format and the two COM interfaces the callers need.
type endpointSession struct {
	dev      Device
	iac      *wca.IAudioClient
	acc      *wca.IAudioCaptureClient
	mix      SampleFormat
	bytesPer int
	// wfx is the engine's mix format (CoTaskMemAlloc'ed, freed by release).
	wfx *wca.WAVEFORMATEX
	// negotiated is the format Initialize actually accepted: wfx or a rebuilt
	// alternate. acceptedAlt reports which of the two it is, for the report.
	negotiated  []byte
	acceptedAlt bool
}

// openEndpoint initialises a shared-mode loopback IAudioClient on dev.
//
// It first offers the endpoint its own mix format (the documented, always-tried
// first choice). When that is refused, it offers the alternates from
// fallbackFormats one by one: an endpoint whose enhanced format is broken
// sometimes still accepts a plain one, which turns a hard failure into a working
// capture. The negotiated format is what the caller converts from.
//
// The returned session owns every COM object and byte slice it created; the
// caller must call session.release().
func (c *loopbackCapturer) openEndpoint(dev Device) (*endpointSession, error) {
	mmd, err := c.deviceByIndex(dev.Index)
	if err != nil {
		return nil, err
	}
	defer mmd.Release()

	s := &endpointSession{dev: dev}
	fail := func(err error) (*endpointSession, error) {
		s.release()
		return nil, err
	}

	if err := mmd.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &s.iac); err != nil {
		return fail(fmt.Errorf("IMMDevice::Activate(IAudioClient) 失败: %w", err))
	}

	var wfx *wca.WAVEFORMATEX
	if err := s.iac.GetMixFormat(&wfx); err != nil {
		return fail(fmt.Errorf("IAudioClient::GetMixFormat 失败: %w", err))
	}
	s.wfx = wfx

	mix, err := mixFormat(wfx)
	if err != nil {
		// A mix format we cannot even describe is not fatal any more: the
		// alternates below are built from scratch and need no mix format. Only
		// the "what does this endpoint sound like" report loses a detail.
		mix = SampleFormat{}
	}

	// --- 1. the endpoint's own mix format ---------------------------------
	initErr := s.tryInitialize(wfx)
	if initErr == nil {
		s.mix = mix
		s.bytesPer = int(wfx.NBlockAlign)
		if s.bytesPer <= 0 {
			s.bytesPer = mix.Channels * ((mix.BitsPerSample + 7) / 8)
		}
		if s.bytesPer <= 0 {
			s.bytesPer = 8 // stereo 32-bit float, the canonical fallback
		}
		return s.finishEndpoint()
	}

	// --- 2. alternate formats --------------------------------------------
	var lastErr = initErr
	for _, cand := range fallbackFormats(mix) {
		buf := buildFormat(cand.sampleRate, cand.channels, cand.bits, cand.isFloat)
		if err := s.tryInitialize(asWaveFormatex(buf)); err != nil {
			lastErr = err
			continue
		}
		s.negotiated = buf
		s.acceptedAlt = true
		s.mix = SampleFormat{
			FormatTag:     wavFormatExtensible,
			SampleRate:    cand.sampleRate,
			Channels:      cand.channels,
			BitsPerSample: cand.bits,
			Float:         cand.isFloat,
			Extensible:    true,
		}
		s.bytesPer = cand.channels * ((cand.bits + 7) / 8)
		return s.finishEndpoint()
	}

	return fail(fmt.Errorf(
		"IAudioClient::Initialize(shared/loopback) failed: %s\n"+
			"  已另外试过 %d 种常见格式（48k/44.1k 立体声、单声道等），全部被该端点拒绝",
		explainInitializeError(lastErr, mix.String()), len(fallbackFormats(mix))))
}

// tryInitialize calls IAudioClient::Initialize with one format.
func (s *endpointSession) tryInitialize(format *wca.WAVEFORMATEX) error {
	return s.iac.Initialize(
		wca.AUDCLNT_SHAREMODE_SHARED,
		wca.AUDCLNT_STREAMFLAGS_LOOPBACK,
		bufferDuration,
		0, // shared mode requires periodicity == 0
		format,
		nil,
	)
}

// finishEndpoint obtains the capture client and reports success.
func (s *endpointSession) finishEndpoint() (*endpointSession, error) {
	if err := s.iac.GetService(wca.IID_IAudioCaptureClient, &s.acc); err != nil {
		s.release()
		return nil, fmt.Errorf("IAudioClient::GetService(IAudioCaptureClient) 失败: %w", err)
	}
	return s, nil
}

// release frees every COM object the session holds. It is safe to call on a
// partially built session and does nothing after the first call.
func (s *endpointSession) release() {
	if s == nil {
		return
	}
	if s.acc != nil {
		s.acc.Release()
		s.acc = nil
	}
	if s.iac != nil {
		s.iac.Release()
		s.iac = nil
	}
	if s.wfx != nil {
		freeWaveFormat(s.wfx)
		s.wfx = nil
	}
}

// freeWaveFormat releases the CoTaskMemAlloc'ed mix format. It is used by the
// probe as well, which opens endpoints outside a session.
func freeWaveFormat(wfx *wca.WAVEFORMATEX) {
	if wfx != nil {
		ole.CoTaskMemFree(uintptr(unsafe.Pointer(wfx)))
	}
}

// candidateDevices returns the endpoints to try, in order, for a request.
//
// Order matters and is the whole point:
//
//  1. the endpoint the user asked for (or the default one when they did not);
//  2. the OTHER default-role endpoint (eConsole vs eMultimedia) when no explicit
//     device was requested - Windows lets these be different devices, and the
//     communications default is often the one that works;
//  3. every remaining active endpoint, in enumeration order.
//
// The fallback exists because "the default endpoint cannot be opened for
// loopback" is a real state (audio enhancements, spatial sound, exclusive mode,
// a Bluetooth headset in hands-free mode) and the machine usually has a second
// working endpoint. Every candidate that was skipped is reported, so a silent
// fallback never happens.
func (c *loopbackCapturer) candidateDevices(deviceSubstr string) ([]Device, error) {
	devices, err := c.Enumerate()
	if err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		return nil, ErrNoEndpoint
	}

	var ordered []Device
	seen := map[string]bool{}
	add := func(d Device) {
		key := d.ID
		if key == "" {
			key = fmt.Sprintf("#%d", d.Index)
		}
		if seen[key] {
			return
		}
		seen[key] = true
		ordered = append(ordered, d)
	}

	if strings.TrimSpace(deviceSubstr) != "" {
		d, merr := MatchDevice(devices, deviceSubstr)
		if merr != nil {
			// An explicit name that matches nothing is the user's typo: report it
			// instead of silently capturing something else.
			return nil, merr
		}
		add(d)
	} else {
		if d, merr := MatchDevice(devices, ""); merr == nil {
			add(d)
		}
		// The default endpoint of the other role. Windows keeps eConsole (what
		// the Settings app shows as "Default Device") and eMultimedia apart, and
		// they really can be different devices.
		console := c.defaultEndpointID(wca.EConsole)
		for _, d := range devices {
			if d.DefaultComms {
				add(d)
			}
			if console != "" && d.ID == console && !d.Default {
				add(d)
			}
		}
	}
	for _, d := range devices {
		add(d)
	}
	return ordered, nil
}

// skippedEndpoint records a candidate that could not be opened.
type skippedEndpoint struct {
	dev Device
	err error
}

// openFirstEndpoint walks the candidates until one opens, returning the session
// and the devices that were skipped on the way.
func (c *loopbackCapturer) openFirstEndpoint(deviceSubstr string) (*endpointSession, []skippedEndpoint, error) {
	cands, err := c.candidateDevices(deviceSubstr)
	if err != nil {
		return nil, nil, err
	}
	var skipped []skippedEndpoint
	for _, dev := range cands {
		s, oerr := c.openEndpoint(dev)
		if oerr == nil {
			return s, skipped, nil
		}
		skipped = append(skipped, skippedEndpoint{dev: dev, err: oerr})
	}
	// Nothing worked: report the first failure, which is the endpoint the user
	// actually asked for, and mention the rest.
	first := skipped[0]
	if len(skipped) == 1 {
		return nil, skipped, first.err
	}
	return nil, skipped, fmt.Errorf("%w\n（另外 %d 个端点也打不开，用 soundradar devices --probe 看全部）",
		first.err, len(skipped)-1)
}

// note renders everything about how this session's endpoint was chosen that the
// user might want to know: which endpoints were skipped, and whether a format
// other than the endpoint's own had to be negotiated. It is empty for the plain
// happy path, so a clean start stays quiet.
func (s *endpointSession) note(skipped []skippedEndpoint) string {
	var b []byte
	if len(skipped) > 0 {
		b = append(b, fmt.Sprintf("端点 [%d] %s 打不开，已改用 [%d] %s：\n",
			skipped[0].dev.Index, skipped[0].dev.Name, s.dev.Index, s.dev.Name)...)
		for _, sk := range skipped {
			b = append(b, fmt.Sprintf("  - [%d] %s：%v\n", sk.dev.Index, sk.dev.Name, sk.err)...)
		}
	}
	if s.acceptedAlt {
		b = append(b, fmt.Sprintf("端点 [%d] %s 拒绝了自己的混合格式，改用 %s 打开成功。\n",
			s.dev.Index, s.dev.Name, s.mix.String())...)
	}
	return string(b)
}

// fallbackNote renders the "your default endpoint did not work, using this one"
// message, so a silent fallback never happens.
func fallbackNote(skipped []skippedEndpoint, used Device) string {
	if len(skipped) == 0 {
		return ""
	}
	var b []byte
	b = append(b, fmt.Sprintf("默认/指定的端点打不开，已改用 [%d] %s。原因：\n", used.Index, used.Name)...)
	for _, sk := range skipped {
		b = append(b, fmt.Sprintf("  - [%d] %s：%v\n", sk.dev.Index, sk.dev.Name, sk.err)...)
	}
	return string(b)
}
