package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/znz/soundradar/internal/wav"
)

const canonRate = 48000

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func sine(rate, n int, freq, amp float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate))
	}
	return out
}

func stereoInterleave(mono []float64, rightGain float64) []float64 {
	out := make([]float64, 0, len(mono)*2)
	for _, v := range mono {
		out = append(out, v, v*rightGain)
	}
	return out
}

func wavBytes(t *testing.T, rate, chans int, samples []float64, float32Fmt bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	if !float32Fmt {
		if err := wav.WriteInt16(&buf, rate, chans, ToInt16(samples)); err != nil {
			t.Fatalf("wav.WriteInt16: %v", err)
		}
		return buf.Bytes()
	}
	// Hand-build a 32-bit IEEE float WAV, which internal/wav must be able to read.
	data := new(bytes.Buffer)
	for _, v := range samples {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(float32(v)))
		data.Write(b[:])
	}
	blockAlign := chans * 4
	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(36+data.Len()))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)
	binary.LittleEndian.PutUint16(hdr[20:22], 3) // IEEE float
	binary.LittleEndian.PutUint16(hdr[22:24], uint16(chans))
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(hdr[28:32], uint32(rate*blockAlign))
	binary.LittleEndian.PutUint16(hdr[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(hdr[34:36], 32)
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], uint32(data.Len()))
	buf.Write(hdr)
	buf.Write(data.Bytes())
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// resampling
// ---------------------------------------------------------------------------

// TestResampleLengthAccuracy checks the "error < 0.1%" requirement for the
// 48k <-> 44.1k conversions the library actually performs.
func TestResampleLengthAccuracy(t *testing.T) {
	cases := []struct {
		name             string
		srcRate, dstRate int
		seconds          float64
	}{
		{"44100->48000 10s", 44100, 48000, 10},
		{"48000->44100 10s", 48000, 44100, 10},
		{"44100->48000 1s", 44100, 48000, 1},
		{"48000->44100 1s", 48000, 44100, 1},
		{"32000->48000 3s", 32000, 48000, 3},
		{"48000->48000 1s", 48000, 48000, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := int(math.Round(tc.seconds * float64(tc.srcRate)))
			src := sine(tc.srcRate, n, 440, 0.5)
			out := Resample(src, tc.srcRate, tc.dstRate)
			want := tc.seconds * float64(tc.dstRate)
			rel := math.Abs(float64(len(out))-want) / want
			t.Logf("%s: src=%d -> dst=%d samples (want %.6f, got %d, rel err %.6f%%)",
				tc.name, len(src), len(out), want, len(out), rel*100)
			if rel >= 0.001 {
				t.Fatalf("resample length error %.4f%% exceeds 0.1%%", rel*100)
			}
		})
	}
}

// TestResampleRoundTripPreservesSignal verifies that a 44.1k -> 48k conversion
// keeps the waveform recognisable at the right frequency (zero crossings) and
// amplitude.
func TestResampleRoundTripPreservesSignal(t *testing.T) {
	const seconds = 2.0
	srcRate := 44100
	n := int(seconds * float64(srcRate))
	src := sine(srcRate, n, 440, 0.8)

	out := Resample(src, srcRate, canonRate)
	if got, want := len(out), int(seconds*float64(canonRate)); math.Abs(float64(got-want))/float64(want) > 0.001 {
		t.Fatalf("length %d, want ~%d", got, want)
	}

	var peak float64
	zeros := 0
	for i, v := range out {
		if math.Abs(v) > peak {
			peak = math.Abs(v)
		}
		if i > 0 && (out[i-1] < 0) != (v < 0) {
			zeros++
		}
	}
	// 440 Hz over 2 s -> about 2*440 = 880 full cycles -> ~1760 zero crossings.
	if zeros < 1700 || zeros > 1820 {
		t.Fatalf("zero crossings %d, want ~1760 (frequency was not preserved)", zeros)
	}
	if math.Abs(peak-0.8) > 0.02 {
		t.Fatalf("peak %.4f, want ~0.8", peak)
	}
}

func TestResampleEdgeCases(t *testing.T) {
	if got := Resample(nil, 44100, 48000); len(got) != 0 {
		t.Fatalf("empty input should stay empty, got %d", len(got))
	}
	if got := Resample([]float64{0.5}, 44100, 48000); len(got) != 1 || got[0] != 0.5 {
		t.Fatalf("single sample: got %v", got)
	}
	same := Resample([]float64{1, 2, 3}, 48000, 48000)
	if len(same) != 3 {
		t.Fatalf("same-rate resample changed the length: %d", len(same))
	}
}

// ---------------------------------------------------------------------------
// decode + convert
// ---------------------------------------------------------------------------

func TestConvertStereo44100WAV(t *testing.T) {
	mono := sine(44100, 44100, 880, 0.7) // 1 second
	raw := wavBytes(t, 44100, 2, stereoInterleave(mono, 1.0), false)

	conv, err := Convert(raw, "tone.wav")
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if conv.Source.Container != FormatWAV {
		t.Fatalf("container = %q", conv.Source.Container)
	}
	if conv.Source.SampleRate != 44100 || conv.Source.Channels != 2 {
		t.Fatalf("origin not recorded: %+v", conv.Source)
	}
	if !strings.HasPrefix(conv.Source.FormatTag, "PCM") {
		t.Fatalf("format tag = %q", conv.Source.FormatTag)
	}

	rate, chans, bits, peakDBFS, err := DecodeCanonicalWAV(conv.WAV)
	if err != nil {
		t.Fatalf("DecodeCanonicalWAV: %v", err)
	}
	if rate != 48000 || chans != 1 || bits != 16 {
		t.Fatalf("canonical format = %d Hz / %d ch / %d bit", rate, chans, bits)
	}
	if len(conv.Mono48k) != 48000 {
		t.Fatalf("stored frames = %d, want 48000", len(conv.Mono48k))
	}
	// 0.7 amplitude sine -> 20*log10(0.7) = -3.10 dBFS
	if math.Abs(peakDBFS-(-3.10)) > 0.05 {
		t.Fatalf("peak %.3f dBFS, want about -3.10", peakDBFS)
	}
}

func TestConvertMono48kIsIdentity(t *testing.T) {
	mono := sine(48000, 4800, 1000, 0.5)
	raw := wavBytes(t, 48000, 1, mono, false)
	conv, err := Convert(raw, "beep.wav")
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(conv.Mono48k) != 4800 {
		t.Fatalf("frames = %d, want 4800", len(conv.Mono48k))
	}
	if conv.Source.SampleRate != canonRate || conv.Source.Channels != 1 {
		t.Fatalf("48k mono should stay 48k mono: %+v", conv.Source)
	}
}

func TestConvertFloat32WAV(t *testing.T) {
	mono := sine(48000, 4800, 500, 0.25)
	raw := wavBytes(t, 48000, 1, mono, true)
	conv, err := Convert(raw, "float.wav")
	if err != nil {
		t.Fatalf("Convert float WAV: %v", err)
	}
	if conv.Source.FormatTag != "IEEE float" {
		t.Fatalf("format tag = %q", conv.Source.FormatTag)
	}
}

func TestConvertRejectsUnsupported(t *testing.T) {
	raw := []byte("OggS\x00\x02\x00\x00some ogg payload")
	_, err := Convert(raw, "boom.ogg")
	if err == nil {
		t.Fatal("expected an error for .ogg")
	}
	if !isUnsupported(err) {
		t.Fatalf("error is not ErrUnsupportedFormat: %v", err)
	}
	if want := "暂不支持 .ogg，请先转成 wav"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

func TestConvertRejectsEmpty(t *testing.T) {
	if _, err := Convert(nil, "nothing.wav"); err == nil {
		t.Fatal("expected an error for an empty upload")
	}
}

// ---------------------------------------------------------------------------
// downmix
// ---------------------------------------------------------------------------

func TestDownmixAveragesChannels(t *testing.T) {
	inter := []float64{1, 0, 0.5, 0.5, -1, 1}
	got := Downmix(inter, 2)
	want := []float64{0.5, 0.5, 0}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-12 {
			t.Fatalf("Downmix = %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// tiny helpers for assertions
// ---------------------------------------------------------------------------

func isUnsupported(err error) bool { return errors.Is(err, ErrUnsupportedFormat) }
