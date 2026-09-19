// Package audio converts user-supplied sound files into the canonical format
// stored inside a soundradar library: 48000 Hz / 16-bit PCM / mono WAV.
//
// It deliberately reuses internal/wav for WAV parsing and writing (no second
// RIFF implementation) and adds:
//
//   - format sniffing that produces Chinese, user-facing error messages,
//   - a pure-Go MP3 decoder (github.com/hajimehoshi/go-mp3),
//   - linear-interpolation resampling to 48 kHz,
//   - mono downmixing,
//   - recording of the ORIGINAL format so a stored sample can always be traced
//     back to what the user actually uploaded.
package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"

	"github.com/hajimehoshi/go-mp3"

	"github.com/znz/soundradar/internal/wav"
)

// Format identifies the container of an uploaded file.
type Format string

const (
	FormatWAV     Format = "wav"
	FormatMP3     Format = "mp3"
	FormatUnknown Format = "unknown"
)

// MPEGFrameSamples is the number of samples per channel in one MPEG-1 Layer III
// frame, which is what go-mp3 emits.
const MPEGFrameSamples = 1152

// ErrUnsupportedFormat is wrapped by every "we cannot read this" error so
// callers can map it to HTTP 400 without string matching.
var ErrUnsupportedFormat = errors.New("不支持的音频格式")

// Source describes the uploaded file exactly as it arrived.
type Source struct {
	FileName      string
	Container     Format
	FormatTag     string
	SampleRate    int
	Channels      int
	BitsPerSample int
	Frames        int64
	DurationS     float64
	Bytes         int64
}

// Converted is the result of decoding + normalising an upload.
type Converted struct {
	Source Source
	// Mono48k holds the normalised mono samples at 48 kHz in [-1, 1].
	Mono48k []float64
	// WAV holds the canonical 48 kHz / 16-bit PCM / mono RIFF/WAVE file.
	WAV []byte
	// PeakDBFS of the canonical WAV, for reporting.
	PeakDBFS float64
}

// Ext returns the lower-case extension (with dot) of a file name.
func Ext(name string) string {
	return strings.ToLower(filepath.Ext(name))
}

// Sniff guesses the container from magic bytes, with the file extension as a
// tie-breaker for short or truncated uploads.
func Sniff(raw []byte, name string) Format {
	if len(raw) >= 12 && string(raw[0:4]) == "RIFF" && string(raw[8:12]) == "WAVE" {
		return FormatWAV
	}
	if hasMP3Magic(raw) {
		return FormatMP3
	}
	switch Ext(name) {
	case ".wav", ".wave":
		return FormatWAV
	case ".mp3":
		return FormatMP3
	}
	return FormatUnknown
}

func hasMP3Magic(raw []byte) bool {
	if len(raw) >= 3 && string(raw[0:3]) == "ID3" {
		return true
	}
	// Look for a frame sync (11 set bits) within the first 8 KiB, skipping ID3v2.
	if len(raw) < 4 {
		return false
	}
	off := 0
	if len(raw) >= 10 && string(raw[0:3]) == "ID3" {
		// ID3v2 size is a 28-bit synchsafe integer at bytes 6..9.
		sz := int(raw[6]&0x7F)<<21 | int(raw[7]&0x7F)<<14 | int(raw[8]&0x7F)<<7 | int(raw[9]&0x7F)
		off = 10 + sz
	}
	limit := len(raw) - 1
	if limit > off+8192 {
		limit = off + 8192
	}
	for i := off; i+1 < limit; i++ {
		if raw[i] == 0xFF && raw[i+1]&0xE0 == 0xE0 {
			return true
		}
	}
	return false
}

// UnsupportedError builds the Chinese error returned for a rejected upload.
func UnsupportedError(raw []byte, name string) error {
	ext := Ext(name)
	label := strings.TrimPrefix(ext, ".")
	if label == "" {
		label = "未知扩展名"
	}
	switch ext {
	case ".ogg", ".oga", ".opus", ".flac", ".m4a", ".aac", ".wma", ".aiff", ".aif", ".caf", ".mp4", ".webm", ".wv":
		return fmt.Errorf("%w: 暂不支持 %s，请先转成 wav（或 mp3）", ErrUnsupportedFormat, ext)
	default:
		return fmt.Errorf("%w: 无法识别的音频文件 %s（仅支持 wav / mp3）", ErrUnsupportedFormat, label)
	}
}

// decodeFunc is the signature shared by the WAV and MP3 decoders.
type decodeFunc func(raw []byte) (src Source, samples []float64, err error)

// Convert decodes raw (an uploaded wav/mp3), downmixes to mono and resamples to
// 48 kHz. name is only used for error messages and provenance.
func Convert(raw []byte, name string) (*Converted, error) {
	if len(raw) == 0 {
		return nil, errors.New("上传的音频文件为空")
	}
	format := Sniff(raw, name)
	var dec decodeFunc
	switch format {
	case FormatWAV:
		dec = decodeWAV
	case FormatMP3:
		dec = decodeMP3
	default:
		return nil, UnsupportedError(raw, name)
	}

	src, samples, err := dec(raw)
	if err != nil {
		if errors.Is(err, ErrUnsupportedFormat) {
			return nil, err
		}
		return nil, fmt.Errorf("解析音频失败（%s）: %w", name, err)
	}
	src.FileName = filepath.Base(name)
	src.Container = format
	src.Bytes = int64(len(raw))

	if src.SampleRate <= 0 || src.Channels <= 0 {
		return nil, fmt.Errorf("音频格式信息无效: %d Hz / %d 声道", src.SampleRate, src.Channels)
	}

	mono := Downmix(samples, src.Channels)
	// Frames/DurationS always describe the DOWNMIXED mono signal measured at the
	// ORIGINAL sample rate. (Source.Frames used to be filled in by the decoders,
	// which made a stereo file report twice its real length.) Use
	// len(Mono48k)/48000 for the stored length.
	src.Frames = int64(len(mono))
	src.DurationS = float64(len(mono)) / float64(src.SampleRate)

	res := Resample(mono, src.SampleRate, wavRate)

	pcm := ToInt16(res)
	var buf bytes.Buffer
	if err := wav.WriteInt16(&buf, wavRate, 1, pcm); err != nil {
		return nil, fmt.Errorf("生成标准 WAV 失败: %w", err)
	}
	stats := wav.StatsInt16(pcm, 1)

	return &Converted{
		Source:   src,
		Mono48k:  res,
		WAV:      buf.Bytes(),
		PeakDBFS: stats.PeakDBFS,
	}, nil
}

// wavRate mirrors library.SampleRate without importing it (avoids a cycle in
// spirit: audio is a leaf package).
const wavRate = 48000

// ---------------------------------------------------------------------------
// decoders
// ---------------------------------------------------------------------------

func decodeWAV(raw []byte) (Source, []float64, error) {
	a, err := wav.Read(bytes.NewReader(raw))
	if err != nil {
		return Source{}, nil, err
	}
	tag := "PCM"
	switch {
	case a.Info.Float:
		tag = "IEEE float"
	case a.Info.FormatTag == wav.FormatExtensible:
		tag = "WAVE_FORMAT_EXTENSIBLE"
	case a.Info.FormatTag != wav.FormatPCM:
		tag = fmt.Sprintf("format tag 0x%04X", a.Info.FormatTag)
	}
	return Source{
		Container:     FormatWAV,
		FormatTag:     tag,
		SampleRate:    a.Info.SampleRate,
		Channels:      a.Info.Channels,
		BitsPerSample: a.Info.BitsPerSample,
		// Frames/DurationS are filled in by Convert after downmixing.
	}, a.Samples, nil
}

func decodeMP3(raw []byte) (Source, []float64, error) {
	info, err := probeMP3(raw)
	if err != nil {
		return Source{}, nil, err
	}
	dec, err := mp3.NewDecoder(bytes.NewReader(raw))
	if err != nil {
		return Source{}, nil, fmt.Errorf("MP3 解码器初始化失败: %w", err)
	}
	if dec.SampleRate() != info.SampleRate {
		info.SampleRate = dec.SampleRate()
	}

	step := MPEGFrameSamples * 2
	buf := make([]byte, step)
	var samples []float64
	for {
		n, rerr := io.ReadFull(dec, buf)
		if n > 0 {
			frames := n / 4 // stereo 16-bit LE
			for i := 0; i < frames; i++ {
				l := int16(binary.LittleEndian.Uint16(buf[i*4:]))
				samples = append(samples, float64(l)/32768)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				break
			}
			return Source{}, nil, rerr
		}
	}
	if len(samples) == 0 {
		return Source{}, nil, errors.New("MP3 中没有可解码的音频帧")
	}
	return info, samples, nil
}

// probeMP3 decodes only the first frame to learn the stream parameters.
func probeMP3(raw []byte) (Source, error) {
	dec, err := mp3.NewDecoder(bytes.NewReader(raw))
	if err != nil {
		return Source{}, fmt.Errorf("MP3 解码器初始化失败: %w", err)
	}
	return Source{
		Container:  FormatMP3,
		FormatTag:  "MPEG-1/2 Layer III",
		SampleRate: dec.SampleRate(),
		Channels:   2, // go-mp3 always decodes MP3 into stereo 16-bit
		// MP3 has no meaningful "bits per sample"; 16 is what go-mp3 emits.
		BitsPerSample: 16,
	}, nil
}

// ---------------------------------------------------------------------------
// DSP
// ---------------------------------------------------------------------------

// Downmix averages all channels of an interleaved buffer into mono. The
// original channel count is preserved by the caller in Source.
func Downmix(interleaved []float64, channels int) []float64 {
	if channels <= 1 {
		return append([]float64(nil), interleaved...)
	}
	n := len(interleaved) / channels
	out := make([]float64, n)
	inv := 1 / float64(channels)
	for i := 0; i < n; i++ {
		var sum float64
		base := i * channels
		for c := 0; c < channels; c++ {
			sum += interleaved[base+c]
		}
		out[i] = sum * inv
	}
	return out
}

// Resample converts mono float samples from srcRate to dstRate using linear
// interpolation.
//
// The output length is derived from the DURATION (round(n * dst/src)) rather
// than from a per-sample accumulator, which keeps 44100 <-> 48000 round trips
// within a few samples (far below the 0.1% accuracy budget) and guarantees that
// a resampled file never drifts in time.
//
// Linear interpolation is not an anti-aliasing filter: downsampling 48 kHz
// material that contains energy above 22.05 kHz folds it back instead of
// removing it. Game sound effects rarely carry significant energy that high and
// P2 fingerprints use band-limited features, so the trade-off is deliberate
// (and documented in the report).
func Resample(src []float64, srcRate, dstRate int) []float64 {
	if srcRate <= 0 || dstRate <= 0 || len(src) == 0 {
		return append([]float64(nil), src...)
	}
	if srcRate == dstRate {
		return append([]float64(nil), src...)
	}
	n := len(src)
	outLen := int(math.Round(float64(n) * float64(dstRate) / float64(srcRate)))
	if outLen < 1 {
		outLen = 1
	}
	out := make([]float64, outLen)
	if n == 1 {
		for i := range out {
			out[i] = src[0]
		}
		return out
	}
	// Source position of output sample i, in source samples.
	ratio := float64(srcRate) / float64(dstRate)
	last := n - 1
	for i := 0; i < outLen; i++ {
		pos := float64(i) * ratio
		idx := int(pos)
		if idx >= last {
			out[i] = src[last]
			continue
		}
		frac := pos - float64(idx)
		out[i] = src[idx] + frac*(src[idx+1]-src[idx])
	}
	return out
}

// ToInt16 renders normalised samples (mono) as canonical 16-bit PCM with
// saturation (reusing wav.ClampInt16 so the rounding rule is identical to P0).
func ToInt16(samples []float64) []int16 {
	out := make([]int16, len(samples))
	for i, v := range samples {
		out[i] = wav.ClampInt16(v)
	}
	return out
}

// DecodeCanonicalWAV verifies that b is a 48 kHz mono 16-bit PCM WAV and
// returns its peak level in dBFS. Used by the HTTP layer and by tests.
func DecodeCanonicalWAV(b []byte) (rate, channels, bits int, peakDBFS float64, err error) {
	a, err := wav.Read(bytes.NewReader(b))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	stats := wav.StatsInt16(ToInt16(a.Samples), a.Info.Channels)
	return a.Info.SampleRate, a.Info.Channels, a.Info.BitsPerSample, stats.PeakDBFS, nil
}
