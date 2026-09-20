//go:build windows

package capture

import (
	"encoding/binary"
	"testing"
)

// TestBuildFormatLayout locks the byte layout of the WAVEFORMATEXTENSIBLE this
// package hands to IAudioClient::Initialize.
//
// The layout is written by hand precisely because Go would pad the 18-byte
// WAVEFORMATEX base to 20 bytes inside a struct, and every field after it would
// then be read by the audio engine from the wrong offset. A mistake here does
// not fail to compile - it produces a garbage format the engine rejects with
// exactly the AUDCLNT_E_UNSUPPORTED_FORMAT this feature exists to work around.
func TestBuildFormatLayout(t *testing.T) {
	buf := buildFormat(48000, 2, 32, true)
	if len(buf) != formatBytes {
		t.Fatalf("格式长度 %d，应当是 %d", len(buf), formatBytes)
	}
	u16 := func(off int) uint16 { return binary.LittleEndian.Uint16(buf[off:]) }
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(buf[off:]) }

	if got := u16(0); got != wavFormatExtensible {
		t.Errorf("WFormatTag = 0x%04X，应当是 0x%04X", got, wavFormatExtensible)
	}
	if got := u16(2); got != 2 {
		t.Errorf("NChannels = %d，应当是 2", got)
	}
	if got := u32(4); got != 48000 {
		t.Errorf("NSamplesPerSec = %d，应当是 48000", got)
	}
	// 48000 Hz * 2 ch * 4 bytes = 384000 bytes/s
	if got := u32(8); got != 384000 {
		t.Errorf("NAvgBytesPerSec = %d，应当是 384000", got)
	}
	if got := u16(12); got != 8 {
		t.Errorf("NBlockAlign = %d，应当是 8", got)
	}
	if got := u16(14); got != 32 {
		t.Errorf("WBitsPerSample = %d，应当是 32", got)
	}
	if got := u16(16); got != 22 {
		t.Errorf("CbSize = %d，应当是 22（WAVEFORMATEXTENSIBLE）", got)
	}
	if got := u16(18); got != 32 {
		t.Errorf("wValidBitsPerSample = %d，应当是 32", got)
	}
	if got := u32(20); got != 0x3 {
		t.Errorf("dwChannelMask = 0x%X，立体声应当是 0x3", got)
	}
	if want := guidBytes(subFormatFloat); string(buf[24:40]) != string(want) {
		t.Errorf("SubFormat 不是 KSDATAFORMAT_SUBTYPE_IEEE_FLOAT：% X", buf[24:40])
	}
}

// TestBuildFormatPCMSubformat checks the integer variant uses the PCM GUID and
// that a hand-built format is described back consistently by mixFormat.
func TestBuildFormatPCMSubformat(t *testing.T) {
	buf := buildFormat(44100, 1, 16, false)
	if got := binary.LittleEndian.Uint16(buf[12:]); got != 2 {
		t.Errorf("单声道 16 位的 NBlockAlign = %d，应当是 2", got)
	}
	if want := guidBytes(subFormatPCM); string(buf[24:40]) != string(want) {
		t.Errorf("SubFormat 不是 KSDATAFORMAT_SUBTYPE_PCM：% X", buf[24:40])
	}
	// mixFormat must read back what buildFormat wrote, otherwise the "what
	// format did we end up with" report lies.
	f, err := mixFormat(asWaveFormatex(buf))
	if err != nil {
		t.Fatalf("mixFormat 读不回自己造的格式: %v", err)
	}
	if f.SampleRate != 44100 || f.Channels != 1 || f.BitsPerSample != 16 || f.Float {
		t.Fatalf("读回的格式不对: %+v", f)
	}
	if !supportedMixFormat(f) {
		t.Fatalf("自己造的格式竟然不受支持: %+v", f)
	}
}

// TestMixFormatReadsExtensibleSubformat checks the SubFormat GUID is what
// decides float vs integer, at the offset mixFormat documents (byte 24).
func TestMixFormatReadsExtensibleSubformat(t *testing.T) {
	fl := buildFormat(48000, 2, 32, true)
	if f, err := mixFormat(asWaveFormatex(fl)); err != nil || !f.Float {
		t.Fatalf("32 位浮点格式被读成 %+v (err=%v)", f, err)
	}
	i32 := buildFormat(48000, 2, 32, false)
	if f, err := mixFormat(asWaveFormatex(i32)); err != nil || f.Float {
		t.Fatalf("32 位整数格式被读成 %+v (err=%v)", f, err)
	}
}

// TestSupportedMixFormatIsWide checks the accepted set, because refusing a
// format is a hard failure: the endpoint cannot be captured at all.
func TestSupportedMixFormatIsWide(t *testing.T) {
	cases := []struct {
		f    SampleFormat
		want bool
	}{
		{SampleFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 32, Float: true}, true},
		{SampleFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 64, Float: true}, true},
		{SampleFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16}, true},
		{SampleFormat{SampleRate: 44100, Channels: 2, BitsPerSample: 8}, true},
		{SampleFormat{SampleRate: 48000, Channels: 6, BitsPerSample: 24}, true},
		// The unusual packed widths some DSPs report must NOT be refused.
		{SampleFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 20}, true},
		{SampleFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 28}, true},
		// Nonsense is refused.
		{SampleFormat{SampleRate: 0, Channels: 2, BitsPerSample: 16}, false},
		{SampleFormat{SampleRate: 48000, Channels: 0, BitsPerSample: 16}, false},
		{SampleFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 4}, false},
		{SampleFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16, Float: true}, false},
	}
	for _, c := range cases {
		if got := supportedMixFormat(c.f); got != c.want {
			t.Errorf("supportedMixFormat(%+v) = %v，期望 %v", c.f, got, c.want)
		}
	}
}

// TestFallbackFormatsAreSane checks the alternates are exactly the ones worth
// offering: the canonical 48 kHz stereo pair first, no duplicates, nothing
// impossible.
func TestFallbackFormatsAreSane(t *testing.T) {
	mix := SampleFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 32, Float: true}
	cands := fallbackFormats(mix)
	if len(cands) < 4 {
		t.Fatalf("候选格式只有 %d 个，太少了", len(cands))
	}
	first := cands[0]
	if first.sampleRate != 48000 || first.channels != 2 || first.bits != 32 || !first.isFloat {
		t.Errorf("第一个候选应当是 48 kHz 立体声 32 位浮点（识别链路的原生格式），得到 %+v", first)
	}
	seen := map[string]bool{}
	for _, c := range cands {
		key := formatDescription(c.sampleRate, c.channels, c.bits, c.isFloat)
		if seen[key] {
			t.Errorf("候选格式重复: %s", key)
		}
		seen[key] = true
		if c.sampleRate <= 0 || c.channels <= 0 || c.bits <= 0 {
			t.Errorf("候选格式不合法: %+v", c)
		}
		if !supportedMixFormat(SampleFormat{
			SampleRate: c.sampleRate, Channels: c.channels,
			BitsPerSample: c.bits, Float: c.isFloat,
		}) {
			t.Errorf("候选格式 %s 我们自己也转换不了", key)
		}
	}
	// An endpoint that only reports 16 kHz mono must still get its own rate as a
	// candidate even if it is not in the standard list.
	odd := fallbackFormats(SampleFormat{SampleRate: 22050, Channels: 1})
	found := false
	for _, c := range odd {
		if c.sampleRate == 22050 {
			found = true
		}
	}
	if !found {
		t.Error("端点自己的采样率没有被列为候选")
	}
}

// TestChannelMaskCoversCommonLayouts checks the masks are the documented ones
// (a wrong mask is another way to make Initialize reject an otherwise fine
// format).
func TestChannelMaskCoversCommonLayouts(t *testing.T) {
	for _, c := range []struct {
		ch   int
		want uint32
	}{
		{1, 0x4}, {2, 0x3}, {6, 0x3F}, {8, 0x63F},
	} {
		if got := channelMaskFor(c.ch); got != c.want {
			t.Errorf("channelMaskFor(%d) = 0x%X，期望 0x%X", c.ch, got, c.want)
		}
	}
}
