//go:build windows

package capture

import (
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"

	"github.com/znz/soundradar/internal/wav"
)

// ---------------------------------------------------------------------------
// COM / mmreg constants not exported by go-wca.
// ---------------------------------------------------------------------------

const (
	wavFormatPCM        = 0x0001
	wavFormatIEEEFloat  = 0x0003
	wavFormatExtensible = 0xFFFE

	// hresultBufferEmpty is AUDCLNT_S_BUFFER_EMPTY, a *success* HRESULT that
	// IAudioCaptureClient::GetBuffer can return. go-wca's wrapper treats every
	// non-zero HRESULT as an error, so we must recognise it and treat it as
	// "nothing to read right now" instead of aborting the capture.
	hresultBufferEmpty = 0x08890001

	// hresultDeviceInvalidated is AUDCLNT_E_DEVICE_INVALIDATED: the endpoint
	// disappeared (device unplugged / default device changed) mid-capture.
	hresultDeviceInvalidated = 0x88890004

	// rpcChangedMode is RPC_E_CHANGED_MODE: CoInitializeEx was already called
	// on this thread with a different apartment model.
	rpcChangedMode = 0x80010106

	// sFalse is returned by CoInitializeEx when the apartment was already
	// initialised with the same model.
	sFalse = 0x00000001

	// devicestateActive mirrors wca.DEVICE_STATE_ACTIVE for readability.
	devicestateActive = wca.DEVICE_STATE_ACTIVE
)

// referenceTime100ns is the unit of REFERENCE_TIME.
const referenceTime100ns = 100 * time.Nanosecond

// bufferDuration is the WASAPI shared-mode buffer we ask for. 1 s is the value
// used by essentially every loopback sample and is comfortably above the
// engine period on any endpoint, so Initialize() does not fail with
// AUDCLNT_E_BUFFER_SIZE_NOT_ALIGNED.
const bufferDuration = wca.REFERENCE_TIME(1 * time.Second / referenceTime100ns)

// pollInterval is the GetNextPacketSize polling period (20 ms granularity is
// plenty for a P0 prototype and keeps CPU usage near zero).
const pollInterval = 10 * time.Millisecond

// loopbackCapturer is the Windows implementation of Capturer.
//
// COM apartment: we deliberately choose COINIT_MULTITHREADED (MTA).
// WASAPI is a free-threaded API and MTA requires no message pump, whereas an
// STA would only work because we never service messages. The creating
// goroutine is pinned to its OS thread with runtime.LockOSThread so every COM
// call happens in the same apartment.
type loopbackCapturer struct {
	mde       *wca.IMMDeviceEnumerator
	comInited bool
	locked    bool
	closed    bool
}

// New creates the Windows loopback capturer and initialises COM.
//
// The returned Capturer must be used from the goroutine that called New,
// because that goroutine is pinned to its OS thread.
func New() (Capturer, error) {
	return newLoopbackCapturer()
}

// newLoopbackCapturer is New with the concrete type, so the streaming path
// (stream_windows.go) can reach the helpers that are not part of the Capturer
// interface. New's behaviour is unchanged.
func newLoopbackCapturer() (*loopbackCapturer, error) {
	c := &loopbackCapturer{}

	// WASAPI objects are COM objects and COM is apartment/thread affine.
	runtime.LockOSThread()
	c.locked = true

	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		var oleErr *ole.OleError
		if errors.As(err, &oleErr) {
			switch uint32(oleErr.Code()) {
			case sFalse, rpcChangedMode:
				// Already initialised on this thread: usable either way.
			default:
				runtime.UnlockOSThread()
				return nil, fmt.Errorf("CoInitializeEx(MTA) failed: %w", err)
			}
		} else {
			runtime.UnlockOSThread()
			return nil, fmt.Errorf("CoInitializeEx(MTA) failed: %w", err)
		}
	}
	c.comInited = true

	var mde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator,
		0,
		wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator,
		&mde,
	); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("CoCreateInstance(MMDeviceEnumerator) failed: %w", err)
	}
	c.mde = mde

	return c, nil
}

// Close releases the device enumerator and leaves the COM apartment.
func (c *loopbackCapturer) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true

	if c.mde != nil {
		c.mde.Release()
		c.mde = nil
	}
	if c.comInited {
		ole.CoUninitialize()
		c.comInited = false
	}
	if c.locked {
		runtime.UnlockOSThread()
		c.locked = false
	}
	return nil
}

// ---------------------------------------------------------------------------
// Enumeration
// ---------------------------------------------------------------------------

// Enumerate lists the active render (output) endpoints.
func (c *loopbackCapturer) Enumerate() ([]Device, error) {
	if c.mde == nil {
		return nil, errors.New("capture: device enumerator is not initialised")
	}

	var dc *wca.IMMDeviceCollection
	if err := c.mde.EnumAudioEndpoints(wca.ERender, devicestateActive, &dc); err != nil {
		return nil, fmt.Errorf("EnumAudioEndpoints(eRender) failed: %w", err)
	}
	defer dc.Release()

	var count uint32
	if err := dc.GetCount(&count); err != nil {
		return nil, fmt.Errorf("IMMDeviceCollection::GetCount failed: %w", err)
	}

	// The endpoint ID of the default console-role device, so we can flag it.
	defaultConsole := c.defaultEndpointID(wca.EConsole)
	defaultComms := c.defaultEndpointID(wca.EMultimedia)

	devices := make([]Device, 0, count)
	for i := uint32(0); i < count; i++ {
		var mmd *wca.IMMDevice
		if err := dc.Item(i, &mmd); err != nil {
			return nil, fmt.Errorf("IMMDeviceCollection::Item(%d) failed: %w", i, err)
		}

		d := Device{Index: len(devices)}
		d.Name = friendlyName(mmd)
		if err := mmd.GetId(&d.ID); err != nil {
			d.ID = ""
		}
		mmd.Release()

		if d.Name == "" {
			d.Name = fmt.Sprintf("<unnamed endpoint #%d>", i)
		}
		d.Default = defaultConsole != "" && d.ID == defaultConsole
		d.DefaultComms = defaultComms != "" && d.ID == defaultComms
		devices = append(devices, d)
	}

	if len(devices) == 0 {
		return nil, ErrNoEndpoint
	}
	return devices, nil
}

// defaultEndpointID returns the endpoint ID of the default render endpoint for
// the given role (eConsole / eMultimedia), or "" on failure.
//
// NOTE: go-wca names the second parameter of GetDefaultAudioEndpoint
// "stateMask", but the underlying COM method is
//
//	GetDefaultAudioEndpoint(EDataFlow dataFlow, ERole role, IMMDevice **)
//
// and the wrapper forwards it positionally as the *role*. So we must pass an
// ERole value (EConsole/EMultimedia/ECommunications), not a DEVICE_STATE_*.
func (c *loopbackCapturer) defaultEndpointID(role uint32) string {
	var mmd *wca.IMMDevice
	if err := c.mde.GetDefaultAudioEndpoint(wca.ERender, role, &mmd); err != nil {
		return ""
	}
	defer mmd.Release()

	var id string
	if err := mmd.GetId(&id); err != nil {
		return ""
	}
	return id
}

// friendlyName reads PKEY_Device_FriendlyName from the endpoint property store.
func friendlyName(mmd *wca.IMMDevice) string {
	var ps *wca.IPropertyStore
	if err := mmd.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return ""
	}
	defer ps.Release()

	var pv wca.PROPVARIANT
	if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err != nil {
		return ""
	}
	// PROPVARIANT.String() calls pvString(), which already frees the
	// CoTaskMemAlloc'ed LPWSTR. Do NOT call VariantClear/pv.String() again.
	return pv.String()
}

// ---------------------------------------------------------------------------
// Capture
// ---------------------------------------------------------------------------

// Capture records the speaker loopback of one render endpoint for d.
func (c *loopbackCapturer) Capture(deviceSubstr string, d time.Duration) (*Result, error) {
	if d <= 0 {
		return nil, fmt.Errorf("capture: duration must be positive, got %s", d)
	}
	if c.mde == nil {
		return nil, errors.New("capture: device enumerator is not initialised")
	}

	devices, err := c.Enumerate()
	if err != nil {
		return nil, err
	}
	dev, err := MatchDevice(devices, deviceSubstr)
	if err != nil {
		return nil, err
	}

	// --- activate IAudioClient on the chosen endpoint -----------------------
	// go-wca v0.3.0 leaves IMMDeviceEnumerator::GetDevice (lookup by endpoint
	// ID) unimplemented, so the endpoint is re-fetched by enumeration index.
	mmd, err := c.deviceByIndex(dev.Index)
	if err != nil {
		return nil, err
	}
	defer mmd.Release()

	var iac *wca.IAudioClient
	// NOTE: go-wca's Activate() takes (refIID, ctx, param, obj) but the wrapper
	// hard-codes the 4th COM argument (pActivationParams) to NULL, so `param`
	// is ignored and must be passed as nil.
	if err := mmd.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &iac); err != nil {
		return nil, fmt.Errorf("IMMDevice::Activate(IAudioClient) failed: %w", err)
	}
	defer iac.Release()

	// --- mix format ---------------------------------------------------------
	var wfx *wca.WAVEFORMATEX
	if err := iac.GetMixFormat(&wfx); err != nil {
		return nil, fmt.Errorf("IAudioClient::GetMixFormat failed: %w", err)
	}
	// The mix format is CoTaskMemAlloc'ed by the audio engine.
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(wfx)))

	mix, err := mixFormat(wfx)
	if err != nil {
		return nil, err
	}
	if !supportedMixFormat(mix) {
		return nil, fmt.Errorf("capture: unsupported mix format %s", mix)
	}
	bytesPerFrame := int(wfx.NBlockAlign)
	if bytesPerFrame <= 0 {
		bytesPerFrame = mix.Channels * ((mix.BitsPerSample + 7) / 8)
	}

	// --- shared mode + loopback --------------------------------------------
	if err := iac.Initialize(
		wca.AUDCLNT_SHAREMODE_SHARED,
		wca.AUDCLNT_STREAMFLAGS_LOOPBACK,
		bufferDuration,
		0, // shared mode requires periodicity == 0
		wfx,
		nil,
	); err != nil {
		return nil, fmt.Errorf("IAudioClient::Initialize(shared/loopback) failed: %w", err)
	}

	var bufferFrameCount uint32
	if err := iac.GetBufferSize(&bufferFrameCount); err != nil {
		return nil, fmt.Errorf("IAudioClient::GetBufferSize failed: %w", err)
	}

	var acc *wca.IAudioCaptureClient
	if err := iac.GetService(wca.IID_IAudioCaptureClient, &acc); err != nil {
		return nil, fmt.Errorf("IAudioClient::GetService(IAudioCaptureClient) failed: %w", err)
	}
	defer acc.Release()

	// --- capture loop ------------------------------------------------------
	targetFrames := int64(d.Seconds() * float64(mix.SampleRate))
	res := &Result{
		Device:     dev,
		MixFormat:  mix,
		Channels:   mix.Channels,
		SampleRate: mix.SampleRate,
		PCM:        make([]int16, 0, targetFrames*int64(mix.Channels)),
	}

	// Nothing is delivered before Start(); loopback in shared mode keeps
	// delivering (silent) packets while nothing is playing, which is exactly
	// what we want for a stable timeline.
	if err := iac.Start(); err != nil {
		return nil, fmt.Errorf("IAudioClient::Start failed: %w", err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = iac.Stop()
		}
	}
	defer stop()

	wallStart := time.Now()
	// Safety net: never spin forever if the endpoint stalls.
	hardDeadline := wallStart.Add(d + 5*time.Second)

	for res.Frames < targetFrames {
		if time.Now().After(hardDeadline) {
			break
		}

		var packetFrames uint32
		if err := acc.GetNextPacketSize(&packetFrames); err != nil {
			return res, fmt.Errorf("IAudioCaptureClient::GetNextPacketSize failed: %w", err)
		}
		if packetFrames == 0 {
			// No new data yet: this is the normal idle state.
			res.PaddingSpins++
			time.Sleep(pollInterval)
			continue
		}

		if err := c.drainPackets(acc, res, bytesPerFrame); err != nil {
			return res, err
		}
	}

	stop()
	res.Wall = time.Since(wallStart)
	res.Frames = int64(len(res.PCM) / mix.Channels)
	res.Duration = time.Duration(float64(res.Frames) / float64(mix.SampleRate) * float64(time.Second))

	st := wav.StatsInt16(res.PCM, mix.Channels)
	res.Peak = st.Peak
	res.PeakDBFS = st.PeakDBFS
	res.RMS = st.RMS
	res.RMSDBFS = st.RMSDBFS
	res.Silent = st.Silent

	return res, nil
}

// drainPackets reads every packet currently queued in the capture client.
func (c *loopbackCapturer) drainPackets(acc *wca.IAudioCaptureClient, res *Result, bytesPerFrame int) error {
	for {
		var (
			data   *byte
			frames uint32
			flags  uint32
			devPos uint64
			qpcPos uint64
		)
		err := acc.GetBuffer(&data, &frames, &flags, &devPos, &qpcPos)
		if err != nil {
			var oleErr *ole.OleError
			if errors.As(err, &oleErr) {
				switch uint32(oleErr.Code()) {
				case hresultBufferEmpty:
					// Race between GetNextPacketSize and GetBuffer.
					return nil
				case hresultDeviceInvalidated:
					return fmt.Errorf("capture: endpoint invalidated during capture (device removed or default device changed): %w", err)
				}
			}
			return fmt.Errorf("IAudioCaptureClient::GetBuffer failed: %w", err)
		}

		n := int(frames) * res.Channels
		switch {
		case frames == 0:
			// Nothing in this packet; nothing to release either.
		case flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0:
			// Loopback delivers "silent" packets while nothing is playing.
			// The data pointer is meaningless in that case, so append real
			// zeros: skipping the packet would shift the whole timeline.
			res.SilentPackets++
			res.PCM = appendZeros(res.PCM, n)
		default:
			if data == nil {
				res.PCM = appendZeros(res.PCM, n)
			} else {
				res.PCM = appendPacket(res.PCM, data, n, int(frames), bytesPerFrame, res.MixFormat)
			}
		}
		if flags&wca.AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY != 0 {
			res.Discontinuities++
		}
		if flags&wca.AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR != 0 {
			// Timestamps are not used by this prototype; just count them.
			res.Discontinuities++
		}

		if err := acc.ReleaseBuffer(frames); err != nil {
			return fmt.Errorf("IAudioCaptureClient::ReleaseBuffer failed: %w", err)
		}
		res.Packets++
		// Keep res.Frames up to date *inside* the loop: the caller's
		// `for res.Frames < targetFrames` condition depends on it, otherwise
		// the loop would only ever end at the hard deadline.
		res.Frames = int64(len(res.PCM) / res.Channels)

		var next uint32
		if err := acc.GetNextPacketSize(&next); err != nil {
			return fmt.Errorf("IAudioCaptureClient::GetNextPacketSize failed: %w", err)
		}
		if next == 0 {
			return nil
		}
	}
}

// deviceByIndex re-fetches the endpoint at the given enumeration index. Used
// because go-wca v0.3.0 does not implement IMMDeviceEnumerator::GetDevice.
func (c *loopbackCapturer) deviceByIndex(index int) (*wca.IMMDevice, error) {
	var dc *wca.IMMDeviceCollection
	if err := c.mde.EnumAudioEndpoints(wca.ERender, devicestateActive, &dc); err != nil {
		return nil, fmt.Errorf("EnumAudioEndpoints(eRender) failed: %w", err)
	}
	defer dc.Release()

	var count uint32
	if err := dc.GetCount(&count); err != nil {
		return nil, fmt.Errorf("IMMDeviceCollection::GetCount failed: %w", err)
	}
	if index < 0 || uint32(index) >= count {
		return nil, fmt.Errorf("capture: endpoint index %d out of range (have %d)", index, count)
	}

	var mmd *wca.IMMDevice
	if err := dc.Item(uint32(index), &mmd); err != nil {
		return nil, fmt.Errorf("IMMDeviceCollection::Item(%d) failed: %w", index, err)
	}
	return mmd, nil
}

// ---------------------------------------------------------------------------
// Mix format handling
// ---------------------------------------------------------------------------

// mixFormat converts the engine's WAVEFORMATEX (which in practice is a
// WAVEFORMATEXTENSIBLE with cbSize == 22) into a SampleFormat.
//
// The extension is read with explicit byte offsets rather than by overlaying a
// Go struct, because Go would pad the 18-byte WAVEFORMATEX base to 20 bytes and
// every field after it would land at the wrong address.
func mixFormat(wfx *wca.WAVEFORMATEX) (SampleFormat, error) {
	f := SampleFormat{
		FormatTag:     wfx.WFormatTag,
		SampleRate:    int(wfx.NSamplesPerSec),
		Channels:      int(wfx.NChannels),
		BitsPerSample: int(wfx.WBitsPerSample),
	}
	if f.SampleRate <= 0 || f.Channels <= 0 || f.BitsPerSample <= 0 {
		return f, fmt.Errorf("capture: nonsense mix format (rate=%d ch=%d bits=%d)", f.SampleRate, f.Channels, f.BitsPerSample)
	}

	switch wfx.WFormatTag {
	case wavFormatPCM:
		f.Float = false
	case wavFormatIEEEFloat:
		f.Float = true
	case wavFormatExtensible:
		f.Extensible = true
		if wfx.CbSize < 22 {
			return f, fmt.Errorf("capture: WAVE_FORMAT_EXTENSIBLE mix format with cbSize=%d (< 22)", wfx.CbSize)
		}
		// WAVEFORMATEXTENSIBLE: base (18 bytes) + wValidBitsPerSample(2) +
		// dwChannelMask(4) + SubFormat GUID(16). SubFormat is at byte 24.
		sub := (*ole.GUID)(unsafe.Add(unsafe.Pointer(wfx), 24))
		switch uint16(sub.Data1) {
		case wavFormatIEEEFloat:
			f.Float = true
		case wavFormatPCM:
			f.Float = false
		default:
			// Unknown subformat: the block alignment is the only remaining
			// hint. Assume float for 4-byte samples, integer otherwise.
			f.Float = wfx.NBlockAlign == wfx.NChannels*4
		}
	default:
		return f, fmt.Errorf("capture: unsupported mix format tag 0x%04X", wfx.WFormatTag)
	}
	return f, nil
}

// supportedMixFormat reports whether appendPacket can convert the format.
func supportedMixFormat(f SampleFormat) bool {
	switch {
	case f.Float && (f.BitsPerSample == 32 || f.BitsPerSample == 64):
		return true
	case !f.Float && (f.BitsPerSample == 8 || f.BitsPerSample == 16 || f.BitsPerSample == 24 || f.BitsPerSample == 32):
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Sample conversion (engine mix format -> 16-bit PCM)
// ---------------------------------------------------------------------------

func appendZeros(dst []int16, n int) []int16 {
	if n <= 0 {
		return dst
	}
	// Reuse the spare capacity instead of growing slice-by-slice.
	dst = append(dst, make([]int16, n)...)
	return dst
}

// appendPacket converts one non-silent WASAPI packet into 16-bit PCM samples
// and appends them to dst. It never resamples: the original sample rate and
// channel count are preserved.
func appendPacket(dst []int16, data *byte, n, frames, bytesPerFrame int, f SampleFormat) []int16 {
	switch {
	case f.Float && f.BitsPerSample == 32:
		src := unsafe.Slice((*float32)(unsafe.Pointer(data)), n)
		for _, v := range src {
			dst = append(dst, wav.ClampInt16(float64(v)))
		}
	case f.Float && f.BitsPerSample == 64:
		src := unsafe.Slice((*float64)(unsafe.Pointer(data)), n)
		for _, v := range src {
			dst = append(dst, wav.ClampInt16(v))
		}
	case !f.Float && f.BitsPerSample == 16:
		src := unsafe.Slice((*int16)(unsafe.Pointer(data)), n)
		dst = append(dst, src...)
	case !f.Float && f.BitsPerSample == 24:
		raw := unsafe.Slice(data, frames*bytesPerFrame)
		for i := 0; i < n; i++ {
			u := uint32(raw[i*3]) | uint32(raw[i*3+1])<<8 | uint32(raw[i*3+2])<<16
			v := int32(u<<8) >> 8 // sign-extend 24 -> 32 bit
			dst = append(dst, int16(v>>8))
		}
	case !f.Float && f.BitsPerSample == 32:
		src := unsafe.Slice((*int32)(unsafe.Pointer(data)), n)
		for _, v := range src {
			dst = append(dst, int16(v>>16))
		}
	case !f.Float && f.BitsPerSample == 8:
		// 8-bit WASAPI samples are unsigned.
		raw := unsafe.Slice(data, n)
		for _, b := range raw {
			dst = append(dst, int16(int(b)-128)<<8)
		}
	}
	return dst
}
