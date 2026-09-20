//go:build windows

package capture

import (
	"fmt"
	"strings"

	"github.com/moutend/go-wca/pkg/wca"
)

// DeviceProbe is the result of trying to OPEN one render endpoint for shared-mode
// loopback, without capturing anything.
//
// It exists because the failure the user sees - IAudioClient::Initialize
// returning AUDCLNT_E_UNSUPPORTED_FORMAT - happens on one specific endpoint, and
// the only reliable way to find out WHICH endpoints work is to try them. Windows
// audio "enhancements", spatial sound, exclusive-mode apps and Bluetooth
// headsets switching into hands-free mode all change what an endpoint's engine
// accepts, and they do it per endpoint.
type DeviceProbe struct {
	Device Device
	// SampleRate/Channels/BitsPerSample/Float describe the engine mix format the
	// endpoint reported, even when opening it failed.
	SampleRate    int
	Channels      int
	BitsPerSample int
	Float         bool
	// OK reports whether IAudioClient::Initialize(shared/loopback) succeeded.
	OK bool
	// Err is the failure, or "" when OK.
	Err string
}

// Format renders the mix format compactly ("" when the format could not be
// read).
func (p DeviceProbe) Format() string {
	if p.SampleRate == 0 {
		return ""
	}
	kind := "PCM"
	if p.Float {
		kind = "float"
	}
	return fmt.Sprintf("%d Hz / %d ch / %d-bit %s", p.SampleRate, p.Channels, p.BitsPerSample, kind)
}

// String is the one-line report used by `soundradar devices --probe`.
func (p DeviceProbe) String() string {
	mark := "OK  "
	if !p.OK {
		mark = "FAIL"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  [%d] %s", mark, p.Device.Index, p.Device.Name)
	if f := p.Format(); f != "" {
		fmt.Fprintf(&b, "\n        混合格式: %s", f)
	}
	if !p.OK {
		fmt.Fprintf(&b, "\n        打不开  : %s", p.Err)
	}
	return b.String()
}

// Probe tries to open every active render endpoint for shared-mode loopback and
// reports the outcome. Nothing is captured: Initialize + GetBufferSize is enough
// to prove an endpoint works, and it is what fails on the affected machines.
//
// The first probe (the default endpoint) is the one the program would pick by
// default, so a FAIL there with an OK further down the list is exactly the
// situation the automatic fallback in Capture is there for.
func (c *loopbackCapturer) Probe() ([]DeviceProbe, error) {
	if c.mde == nil {
		return nil, ErrNoEndpoint
	}
	devices, err := c.Enumerate()
	if err != nil {
		return nil, err
	}
	out := make([]DeviceProbe, 0, len(devices))
	for _, dev := range devices {
		out = append(out, c.ProbeDevice(dev))
	}
	return out, nil
}

// ProbeDevice tries to open one endpoint. It never returns an error: the failure
// belongs in the report, not in the caller's control flow.
func (c *loopbackCapturer) ProbeDevice(dev Device) DeviceProbe {
	p := DeviceProbe{Device: dev}
	mmd, err := c.deviceByIndex(dev.Index)
	if err != nil {
		p.Err = err.Error()
		return p
	}
	defer mmd.Release()

	var iac *wca.IAudioClient
	if err := mmd.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &iac); err != nil {
		p.Err = fmt.Sprintf("IMMDevice::Activate(IAudioClient) 失败: %v", err)
		return p
	}
	defer iac.Release()

	var wfx *wca.WAVEFORMATEX
	if err := iac.GetMixFormat(&wfx); err != nil {
		p.Err = fmt.Sprintf("IAudioClient::GetMixFormat 失败: %v", err)
		return p
	}
	defer freeWaveFormat(wfx)

	if f, ferr := mixFormat(wfx); ferr == nil {
		p.SampleRate, p.Channels, p.BitsPerSample, p.Float = f.SampleRate, f.Channels, f.BitsPerSample, f.Float
	}

	if err := iac.Initialize(
		wca.AUDCLNT_SHAREMODE_SHARED,
		wca.AUDCLNT_STREAMFLAGS_LOOPBACK,
		bufferDuration,
		0,
		wfx,
		nil,
	); err != nil {
		p.Err = explainInitializeError(err, p.Format())
		return p
	}
	var frames uint32
	if err := iac.GetBufferSize(&frames); err != nil {
		p.Err = fmt.Sprintf("IAudioClient::GetBufferSize 失败: %v", err)
		return p
	}
	p.OK = true
	return p
}
