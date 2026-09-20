//go:build windows

package capture

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-ole/go-ole"
)

// TestHresultUnsupportedFormatValue locks the number this whole change is about.
//
//	IAudioClient::Initialize(shared/loopback) failed: error 2290679816
//
// is what a user reported; 2290679816 == 0x88890008 ==
// AUDCLNT_E_UNSUPPORTED_FORMAT. If this constant ever drifts, every diagnostic
// below silently starts describing the wrong failure.
func TestHresultUnsupportedFormatValue(t *testing.T) {
	const reported = 2290679816
	if uint32(hresultUnsupportedFormat) != reported {
		t.Fatalf("hresultUnsupportedFormat = 0x%08X (%d)，用户报的 %d 是 AUDCLNT_E_UNSUPPORTED_FORMAT",
			hresultUnsupportedFormat, hresultUnsupportedFormat, reported)
	}
}

// TestExplainInitializeErrorNamesTheCause checks the three things a user needs
// from this message: the symbolic name, the hex code, and what to DO about it.
// Windows itself has no message text for the AUDCLNT_* codes, which is why the
// raw error prints as "The system cannot find message text...".
func TestExplainInitializeErrorNamesTheCause(t *testing.T) {
	cases := []struct {
		code     uint32
		wantName string
		wantHint string
	}{
		{hresultUnsupportedFormat, "AUDCLNT_E_UNSUPPORTED_FORMAT", "音频增强"},
		{hresultDeviceInUse, "AUDCLNT_E_DEVICE_IN_USE", "独占"},
		{hresultDeviceInvalidated, "AUDCLNT_E_DEVICE_INVALIDATED", "插拔耳机"},
		{hresultEndpointCreateFailed, "AUDCLNT_E_ENDPOINT_CREATE_FAILED", "驱动"},
		{hresultServiceNotRunning, "AUDCLNT_E_SERVICE_NOT_RUNNING", "Audiosrv"},
	}
	for _, c := range cases {
		got := explainInitializeError(ole.NewError(uintptr(c.code)), "48000 Hz / 2 ch / 32-bit float")
		if !strings.Contains(got, c.wantName) {
			t.Errorf("0x%08X 的说明里没有 %s：%s", c.code, c.wantName, got)
		}
		if !strings.Contains(got, "0x"+hex8(c.code)) {
			t.Errorf("0x%08X 的说明里没有十六进制码：%s", c.code, got)
		}
		if !strings.Contains(got, c.wantHint) {
			t.Errorf("0x%08X 的说明里没有处理建议（%s）：%s", c.code, c.wantHint, got)
		}
	}
}

// TestExplainInitializeErrorKeepsFormat checks the mix format is part of the
// message: the format is the thing the endpoint refused, so a report without it
// cannot be diagnosed.
func TestExplainInitializeErrorKeepsFormat(t *testing.T) {
	got := explainInitializeError(ole.NewError(uintptr(hresultUnsupportedFormat)), "44100 Hz / 2 ch / 16-bit PCM")
	if !strings.Contains(got, "44100 Hz / 2 ch / 16-bit PCM") {
		t.Fatalf("说明里没有混合格式：%s", got)
	}
}

// TestExplainInitializeErrorHandlesForeignErrors checks an error that is not an
// HRESULT is passed through instead of being mislabelled.
func TestExplainInitializeErrorHandlesForeignErrors(t *testing.T) {
	plain := errors.New("something else")
	if got := explainInitializeError(plain, "fmt"); !strings.Contains(got, "something else") {
		t.Fatalf("普通错误被吞掉了：%s", got)
	}
	if got := explainInitializeError(nil, "fmt"); got != "" {
		t.Fatalf("nil 错误应当返回空串，得到 %q", got)
	}
}

// TestFallbackNoteNamesBothEndpoints checks the fallback is never silent: the
// user must learn that the endpoint they asked for was skipped, and why.
func TestFallbackNoteNamesBothEndpoints(t *testing.T) {
	skipped := []skippedEndpoint{
		{dev: Device{Index: 0, Name: "扬声器 (Realtek(R) Audio)"}, err: errors.New("AUDCLNT_E_UNSUPPORTED_FORMAT")},
	}
	used := Device{Index: 1, Name: "耳机 (USB Audio)"}
	note := fallbackNote(skipped, used)
	for _, want := range []string{"扬声器 (Realtek(R) Audio)", "耳机 (USB Audio)", "AUDCLNT_E_UNSUPPORTED_FORMAT"} {
		if !strings.Contains(note, want) {
			t.Errorf("回退提示里没有 %q：%s", want, note)
		}
	}
	if fallbackNote(nil, used) != "" {
		t.Error("没有跳过任何端点时不应该有提示")
	}
}

// hex8 renders a uint32 as 8 upper-case hex digits, matching the %08X in the
// messages (which upper-cases the digits).
func hex8(v uint32) string {
	const digits = "0123456789ABCDEF"
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = digits[v&0xF]
		v >>= 4
	}
	return string(b[:])
}
