package audio

import (
	"bytes"
	"io"
	"math"
	"testing"

	"github.com/hajimehoshi/go-mp3"

	"github.com/znz/soundradar/internal/testaudio"
)

// mp3SilentFrames builds n silent MPEG-1 Layer III frames using the shared
// generator in internal/testaudio (see that package for the frame layout).
func mp3SilentFrames(t *testing.T, n int) []byte {
	t.Helper()
	return testaudio.SilentMP3(n)
}

// TestMP3StreamShape documents the shape of the synthetic stream: 384-byte
// frames, 32-byte side info, 1152 samples per channel per frame.
func TestMP3StreamShape(t *testing.T) {
	raw := mp3SilentFrames(t, 3)
	if len(raw) != 3*384 {
		t.Fatalf("stream is %d bytes, want %d", len(raw), 3*384)
	}
	dec, err := mp3.NewDecoder(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if got := dec.SampleRate(); got != 48000 {
		t.Fatalf("sample rate = %d", got)
	}
	n, err := io.ReadFull(dec, make([]byte, MPEGFrameSamples*4))
	if err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	t.Logf("one MPEG frame -> %d interleaved stereo bytes (expect %d)", n, MPEGFrameSamples*4)
}

// TestConvertMP3 decodes a synthetic MPEG-1 Layer III stream.
func TestConvertMP3(t *testing.T) {
	const frames = 20
	raw := mp3SilentFrames(t, frames)

	if got := Sniff(raw, "tone.mp3"); got != FormatMP3 {
		t.Fatalf("Sniff = %q, want mp3", got)
	}

	conv, err := Convert(raw, "tone.mp3")
	if err != nil {
		t.Fatalf("Convert mp3: %v", err)
	}
	// A 128 kbit/s MPEG-1 Layer III frame at 48 kHz carries 1152 samples per
	// channel; go-mp3 always emits stereo int16. Convert downmixes to mono, so
	// duration matches the per-channel sample count (not half of it).
	wantFrames := int64(frames * MPEGFrameSamples)
	wantInterleaved := wantFrames * 2
	if conv.Source.SampleRate != 48000 {
		t.Fatalf("mp3 sample rate = %d, want 48000", conv.Source.SampleRate)
	}
	if conv.Source.Channels != 2 {
		t.Fatalf("mp3 decoded channels = %d, want 2 (go-mp3 always emits stereo)", conv.Source.Channels)
	}
	// Decoder may drop a few samples at the tail of a synthetic stream.
	if diff := conv.Source.Frames - wantFrames; diff > 20 || diff < -20 {
		t.Fatalf("mp3 mono frames = %d, want ~%d (from %d interleaved samples)",
			conv.Source.Frames, wantFrames, wantInterleaved)
	}
	if math.Abs(conv.Source.DurationS-float64(conv.Source.Frames)/48000) > 1e-9 {
		t.Fatalf("mp3 duration = %v, inconsistent with Frames", conv.Source.DurationS)
	}
	if len(conv.Mono48k) != int(conv.Source.Frames) {
		t.Fatalf("stored frames = %d, want %d", len(conv.Mono48k), conv.Source.Frames)
	}
	rate, chans, bits, peak, err := DecodeCanonicalWAV(conv.WAV)
	if err != nil {
		t.Fatalf("DecodeCanonicalWAV: %v", err)
	}
	if rate != 48000 || chans != 1 || bits != 16 {
		t.Fatalf("canonical = %d/%d/%d", rate, chans, bits)
	}
	t.Logf("mp3 decode ok: %d MPEG frames -> %d stereo frames -> %d stored frames, peak %.1f dBFS",
		frames, conv.Source.Frames, len(conv.Mono48k), peak)
}

// TestSniffFormats checks magic-byte detection independently of the extension.
func TestSniffFormats(t *testing.T) {
	wavRaw := wavBytes(t, 48000, 1, sine(48000, 480, 440, 0.5), false)
	if got := Sniff(wavRaw, "no-extension"); got != FormatWAV {
		t.Fatalf("Sniff(wav magic) = %q", got)
	}
	mp3Raw := mp3SilentFrames(t, 1)
	if got := Sniff(mp3Raw, "no-extension"); got != FormatMP3 {
		t.Fatalf("Sniff(mp3 magic) = %q", got)
	}
	if got := Sniff([]byte("not audio at all"), "x.bin"); got != FormatUnknown {
		t.Fatalf("Sniff(garbage) = %q", got)
	}
	// Extension fallback for a truncated upload.
	if got := Sniff([]byte("RI"), "song.wav"); got != FormatWAV {
		t.Fatalf("Sniff(truncated .wav) = %q", got)
	}
}
