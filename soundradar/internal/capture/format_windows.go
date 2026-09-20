//go:build windows

package capture

import (
	"fmt"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// ---------------------------------------------------------------------------
// WAVEFORMATEXTENSIBLE construction
// ---------------------------------------------------------------------------
//
// go-wca has no constructor for a format, and the shared-mode loopback path
// needs one: when an endpoint refuses its own mix format, the only remaining
// lever is to offer it a different format and see whether the engine accepts
// that. The formats below are built in memory here and handed to
// IAudioClient::Initialize.

var (
	// KSDATAFORMAT_SUBTYPE_PCM and _IEEE_FLOAT, the SubFormat GUIDs of
	// WAVEFORMATEXTENSIBLE.
	subFormatPCM = ole.GUID{
		Data1: 0x00000001, Data2: 0x0000, Data3: 0x0010,
		Data4: [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71},
	}
	subFormatFloat = ole.GUID{
		Data1: 0x00000003, Data2: 0x0000, Data3: 0x0010,
		Data4: [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71},
	}
)

// formatBytes is the size of a WAVEFORMATEXTENSIBLE: the 18-byte WAVEFORMATEX
// base plus the 22-byte extension, with no padding (the whole blob is laid out
// by hand precisely so Go cannot insert any).
const formatBytes = 18 + 22

// channelMaskFor returns the standard speaker mask for a channel count.
func channelMaskFor(channels int) uint32 {
	switch channels {
	case 1:
		return 0x4 // SPEAKER_FRONT_CENTER
	case 2:
		return 0x3 // FRONT_LEFT | FRONT_RIGHT
	case 3:
		return 0x7
	case 4:
		return 0x33
	case 6:
		return 0x3F
	case 8:
		return 0x63F
	}
	return 0
}

// buildFormat lays out a WAVEFORMATEXTENSIBLE for the given parameters.
//
// The layout is written byte by byte on purpose: Go would pad the 18-byte
// WAVEFORMATEX base to 20 bytes inside a struct, and every field after it would
// then land at the wrong offset, which the audio engine would read as garbage.
func buildFormat(sampleRate, channels, bits int, isFloat bool) []byte {
	bytesPerSample := (bits + 7) / 8
	blockAlign := channels * bytesPerSample
	buf := make([]byte, formatBytes)
	le := func(off int, v uint16) { buf[off] = byte(v); buf[off+1] = byte(v >> 8) }
	le32 := func(off int, v uint32) {
		buf[off] = byte(v)
		buf[off+1] = byte(v >> 8)
		buf[off+2] = byte(v >> 16)
		buf[off+3] = byte(v >> 24)
	}
	le(0, wavFormatExtensible)
	le(2, uint16(channels))
	le32(4, uint32(sampleRate))
	le32(8, uint32(sampleRate*blockAlign))
	le(12, uint16(blockAlign))
	le(14, uint16(bits))
	// Extension: wValidBitsPerSample(2) + dwChannelMask(4) + SubFormat(16).
	le(16, 22)
	le(18, uint16(bits))
	le32(20, channelMaskFor(channels))
	sub := subFormatPCM
	if isFloat {
		sub = subFormatFloat
	}
	copy(buf[24:40], guidBytes(sub))
	return buf
}

// guidBytes serialises a GUID the way Windows lays one out in memory
// (Data1/Data2/Data3 little-endian, Data4 verbatim).
func guidBytes(g ole.GUID) []byte {
	b := make([]byte, 16)
	b[0] = byte(g.Data1)
	b[1] = byte(g.Data1 >> 8)
	b[2] = byte(g.Data1 >> 16)
	b[3] = byte(g.Data1 >> 24)
	b[4] = byte(g.Data2)
	b[5] = byte(g.Data2 >> 8)
	b[6] = byte(g.Data3)
	b[7] = byte(g.Data3 >> 8)
	copy(b[8:], g.Data4[:])
	return b
}

// asWaveFormatex views a blob built by buildFormat as the WAVEFORMATEX the COM
// call expects (only the first 18 bytes are read by whoever respects cbSize).
func asWaveFormatex(buf []byte) *wca.WAVEFORMATEX {
	return (*wca.WAVEFORMATEX)(unsafe.Pointer(&buf[0]))
}

// formatDescription renders a candidate for the log.
func formatDescription(sampleRate, channels, bits int, isFloat bool) string {
	kind := "PCM"
	if isFloat {
		kind = "float"
	}
	return fmt.Sprintf("%d Hz / %d ch / %d-bit %s", sampleRate, channels, bits, kind)
}

// ---------------------------------------------------------------------------
// Candidate formats
// ---------------------------------------------------------------------------

// formatCandidate is one format to offer an endpoint.
type formatCandidate struct {
	sampleRate int
	channels   int
	bits       int
	isFloat    bool
	// mix marks the endpoint's own mix format (handed over as-is rather than
	// rebuilt).
	mix bool
}

// fallbackFormats returns the formats to try, in order, after the endpoint's own
// mix format was refused.
//
// Why this exists: shared-mode loopback normally accepts only the mix format, so
// an endpoint that refuses it is usually broken by its own DSP chain (audio
// enhancements, spatial sound, a Bluetooth headset that switched profile). But
// "usually" is not "always": some drivers accept a plain format when their own
// enhanced one fails, and a 48 kHz stereo offer is the one that works in
// practice. Trying costs one Initialize call per format and can turn a hard
// failure into a working capture, so it is worth it.
//
// The list is ordered from "most likely to be accepted and most useful" to
// "last resort": the canonical 48 kHz stereo pair first (the recognition
// pipeline's native rate, so no resampling), then 44.1 kHz (the most common
// consumer rate), then mono 48 kHz, then the endpoint's own rate in case it only
// likes that one.
func fallbackFormats(mix SampleFormat) []formatCandidate {
	var out []formatCandidate
	seen := map[string]bool{}
	add := func(c formatCandidate) {
		key := fmt.Sprintf("%d/%d/%d/%v", c.sampleRate, c.channels, c.bits, c.isFloat)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, c)
	}

	// 48 kHz stereo, float then 16-bit.
	add(formatCandidate{sampleRate: 48000, channels: 2, bits: 32, isFloat: true})
	add(formatCandidate{sampleRate: 48000, channels: 2, bits: 16})
	// 44.1 kHz stereo.
	add(formatCandidate{sampleRate: 44100, channels: 2, bits: 32, isFloat: true})
	add(formatCandidate{sampleRate: 44100, channels: 2, bits: 16})
	// Mono variants (Bluetooth hands-free endpoints and some virtual devices).
	add(formatCandidate{sampleRate: 48000, channels: 1, bits: 16})
	add(formatCandidate{sampleRate: 16000, channels: 1, bits: 16})
	// The endpoint's own rate, in case only that one is accepted.
	if mix.SampleRate > 0 {
		ch := mix.Channels
		if ch <= 0 {
			ch = 2
		}
		add(formatCandidate{sampleRate: mix.SampleRate, channels: ch, bits: 32, isFloat: true})
		add(formatCandidate{sampleRate: mix.SampleRate, channels: ch, bits: 16})
	}
	return out
}
