// Package wav implements the minimal subset of RIFF/WAVE that the soundradar
// prototype needs:
//
//   - a writer that emits canonical 16-bit PCM WAV files (the Go standard
//     library ships no WAV encoder),
//   - a reader that decodes 8/16/24/32-bit integer PCM as well as 32/64-bit
//     IEEE float PCM (including WAVE_FORMAT_EXTENSIBLE) into normalised
//     float64 samples in [-1, 1],
//   - small helpers for PCM16 sample conversion and level statistics.
//
// Only stdlib packages are used.
package wav

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// WAVE format tags (mmreg.h).
const (
	FormatPCM        uint16 = 0x0001
	FormatIEEEFloat  uint16 = 0x0003
	FormatALaw       uint16 = 0x0006
	FormatMuLaw      uint16 = 0x0007
	FormatExtensible uint16 = 0xFFFE
)

const headerSize = 44

// Info describes the on-disk format of a decoded WAV file.
type Info struct {
	FormatTag     uint16
	SampleRate    int
	Channels      int
	BitsPerSample int
	Float         bool
	Extensible    bool
	Frames        int64
	DataBytes     int64
	BytesPerFrame int
	DurationSec   float64
}

// Layout returns a canonical Windows-style mmreg description of the format,
// e.g. "48000 Hz / 2 ch / 16-bit PCM".
func (i Info) Layout() string {
	kind := "PCM"
	if i.Float {
		kind = "IEEE float"
	}
	return fmt.Sprintf("%d Hz / %d ch / %d-bit %s", i.SampleRate, i.Channels, i.BitsPerSample, kind)
}

// WriteInt16 writes interleaved 16-bit signed PCM samples as a canonical
// 44-byte-header RIFF/WAVE file. The RIFF chunk size is corrected for the
// mandatory pad byte when the data chunk has an odd length.
func WriteInt16(w io.Writer, sampleRate, channels int, pcm []int16) error {
	if sampleRate <= 0 {
		return fmt.Errorf("wav: invalid sample rate %d", sampleRate)
	}
	if channels <= 0 {
		return fmt.Errorf("wav: invalid channel count %d", channels)
	}

	dataBytes := int64(len(pcm)) * 2
	pad := dataBytes % 2
	// RIFF size = 4 ("WAVE") + (8 + 16) fmt + (8 + dataBytes + pad)
	riffSize := uint32(4 + 24 + 8 + dataBytes + pad)

	blockAlign := channels * 2
	var hdr [headerSize]byte
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], riffSize)
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(hdr[20:22], FormatPCM)
	binary.LittleEndian.PutUint16(hdr[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(hdr[28:32], uint32(sampleRate*blockAlign)) // byte rate
	binary.LittleEndian.PutUint16(hdr[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(hdr[34:36], 16) // bits per sample
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], uint32(dataBytes))

	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}

	// Encode in chunks so we never allocate a second full-size buffer for
	// long captures.
	const chunkSamples = 1 << 16
	buf := make([]byte, chunkSamples*2)
	for off := 0; off < len(pcm); {
		n := len(pcm) - off
		if n > chunkSamples {
			n = chunkSamples
		}
		for i, s := range pcm[off : off+n] {
			binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
		}
		if _, err := w.Write(buf[:n*2]); err != nil {
			return err
		}
		off += n
	}
	if pad == 1 {
		if _, err := w.Write([]byte{0}); err != nil {
			return err
		}
	}
	return nil
}

// WriteInt16File writes pcm to path using WriteInt16.
func WriteInt16File(path string, sampleRate, channels int, pcm []int16) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	bw := bufio.NewWriterSize(f, 1<<16)
	if err := WriteInt16(bw, sampleRate, channels, pcm); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	return f.Close()
}

// Audio is a fully decoded WAV file. Samples are interleaved and normalised to
// [-1, 1] regardless of the on-disk sample format.
type Audio struct {
	Info    Info
	Samples []float64
}

// ReadFile reads and decodes the WAV file at path.
func ReadFile(path string) (*Audio, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	a, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return a, nil
}

// Read decodes a RIFF/WAVE stream. Unknown chunks are skipped.
func Read(r io.Reader) (*Audio, error) {
	br := bufio.NewReaderSize(r, 1<<16)

	var riff [12]byte
	if _, err := io.ReadFull(br, riff[:]); err != nil {
		return nil, fmt.Errorf("wav: reading RIFF header: %w", err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return nil, errors.New("wav: not a RIFF/WAVE file")
	}

	var info Info
	var raw []byte
	haveFmt := false

	for {
		var chdr [8]byte
		if _, err := io.ReadFull(br, chdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, fmt.Errorf("wav: reading chunk header: %w", err)
		}
		id := string(chdr[0:4])
		size := int64(binary.LittleEndian.Uint32(chdr[4:8]))
		if size < 0 {
			return nil, fmt.Errorf("wav: negative chunk size for %q", id)
		}

		switch id {
		case "fmt ":
			body := make([]byte, size)
			if _, err := io.ReadFull(br, body); err != nil {
				return nil, fmt.Errorf("wav: reading fmt chunk: %w", err)
			}
			if len(body) < 16 {
				return nil, fmt.Errorf("wav: fmt chunk too short (%d bytes)", len(body))
			}
			info.FormatTag = binary.LittleEndian.Uint16(body[0:2])
			info.Channels = int(binary.LittleEndian.Uint16(body[2:4]))
			info.SampleRate = int(binary.LittleEndian.Uint32(body[4:8]))
			info.BitsPerSample = int(binary.LittleEndian.Uint16(body[14:16]))
			info.Float = info.FormatTag == FormatIEEEFloat
			if info.FormatTag == FormatExtensible {
				info.Extensible = true
				if len(body) < 40 {
					return nil, errors.New("wav: WAVE_FORMAT_EXTENSIBLE fmt chunk shorter than 40 bytes")
				}
				// SubFormat GUID starts at byte 24; the first two bytes are
				// the effective format tag.
				sub := binary.LittleEndian.Uint16(body[24:26])
				info.Float = sub == FormatIEEEFloat
			}
			info.BytesPerFrame = info.Channels * ((info.BitsPerSample + 7) / 8)
			haveFmt = true

		case "data":
			buf := make([]byte, size)
			if _, err := io.ReadFull(br, buf); err != nil {
				return nil, fmt.Errorf("wav: reading data chunk: %w", err)
			}
			raw = buf
			info.DataBytes = size
			if size%2 == 1 {
				var pad [1]byte
				if _, err := io.ReadFull(br, pad[:]); err != nil && !errors.Is(err, io.EOF) {
					return nil, fmt.Errorf("wav: reading data pad byte: %w", err)
				}
			}

		default:
			if _, err := br.Discard(int(size)); err != nil {
				return nil, fmt.Errorf("wav: skipping chunk %q: %w", id, err)
			}
			if size%2 == 1 {
				if _, err := br.Discard(1); err != nil {
					return nil, fmt.Errorf("wav: skipping chunk %q pad: %w", id, err)
				}
			}
		}
		if size%2 == 1 && id == "fmt " {
			if _, err := br.Discard(1); err != nil {
				return nil, fmt.Errorf("wav: skipping fmt pad: %w", err)
			}
		}
	}

	if !haveFmt {
		return nil, errors.New("wav: no fmt chunk found")
	}
	if info.Channels <= 0 || info.BytesPerFrame <= 0 {
		return nil, fmt.Errorf("wav: unusable format (channels=%d bpf=%d)", info.Channels, info.BytesPerFrame)
	}

	samples, err := Decode(raw, info)
	if err != nil {
		return nil, err
	}
	info.Frames = int64(len(samples) / info.Channels)
	info.DurationSec = float64(info.Frames) / float64(info.SampleRate)

	return &Audio{Info: info, Samples: samples}, nil
}

// Decode converts a raw PCM byte buffer into normalised float64 samples using
// the format described by info.
func Decode(raw []byte, info Info) ([]float64, error) {
	if info.BytesPerFrame <= 0 {
		return nil, errors.New("wav: bytes per frame must be positive")
	}
	// width is the size of ONE sample (one channel of one frame); `n` is the
	// total number of interleaved samples, i.e. frames * channels.
	width := (info.BitsPerSample + 7) / 8
	if width <= 0 {
		return nil, fmt.Errorf("wav: invalid bits per sample %d", info.BitsPerSample)
	}
	n := len(raw) / width
	out := make([]float64, n)

	switch {
	case info.Float && info.BitsPerSample == 32:
		for i := 0; i < n; i++ {
			bits := binary.LittleEndian.Uint32(raw[i*4:])
			out[i] = float64(math.Float32frombits(bits))
		}
	case info.Float && info.BitsPerSample == 64:
		for i := 0; i < n; i++ {
			bits := binary.LittleEndian.Uint64(raw[i*8:])
			out[i] = math.Float64frombits(bits)
		}
	case !info.Float && info.BitsPerSample == 8:
		for i := 0; i < n; i++ {
			// 8-bit PCM in WAV is unsigned.
			out[i] = (float64(raw[i]) - 128) / 128
		}
	case !info.Float && width == 2:
		for i := 0; i < n; i++ {
			out[i] = float64(int16(binary.LittleEndian.Uint16(raw[i*2:]))) / 32768
		}
	case !info.Float && width == 3:
		for i := 0; i < n; i++ {
			u := uint32(raw[i*3]) | uint32(raw[i*3+1])<<8 | uint32(raw[i*3+2])<<16
			v := int32(u<<8) >> 8 // sign-extend 24 -> 32 bit
			out[i] = float64(v) / 8388608
		}
	case !info.Float && width == 4:
		for i := 0; i < n; i++ {
			out[i] = float64(int32(binary.LittleEndian.Uint32(raw[i*4:]))) / 2147483648
		}
	default:
		return nil, fmt.Errorf("wav: unsupported sample format (tag=0x%04X bits=%d)", info.FormatTag, info.BitsPerSample)
	}
	return out, nil
}

// Mono downmixes interleaved samples by averaging all channels.
func (a *Audio) Mono() []float64 {
	ch := a.Info.Channels
	if ch <= 1 {
		return a.Samples
	}
	n := len(a.Samples) / ch
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		var sum float64
		for c := 0; c < ch; c++ {
			sum += a.Samples[i*ch+c]
		}
		out[i] = sum / float64(ch)
	}
	return out
}

// ClampInt16 converts a normalised sample in [-1, 1] to 16-bit signed PCM with
// saturation. Values outside the range are clamped rather than wrapped.
func ClampInt16(v float64) int16 {
	if v >= 1 {
		return 32767
	}
	if v <= -1 {
		return -32768
	}
	return int16(math.Round(v * 32767))
}

// Float32ToInt16 converts non-interleaved-in-memory (but interleaved-ordered)
// float32 samples to 16-bit PCM with saturation.
func Float32ToInt16(src []float32, dst []int16) {
	for i, v := range src {
		dst[i] = ClampInt16(float64(v))
	}
}

// Int32ToInt16 converts 32-bit integer PCM to 16-bit PCM by dropping the low
// 16 bits (the standard, dither-free way to narrow integer PCM).
func Int32ToInt16(src []int32, dst []int16) {
	for i, v := range src {
		dst[i] = int16(v >> 16)
	}
}

// PCMStats summarizes a 16-bit PCM buffer.
type PCMStats struct {
	Frames     int64
	Channels   int
	Peak       float64 // linear peak in [0, 1] (full scale = 1.0)
	PeakDBFS   float64 // 20*log10(Peak); -Inf when fully silent
	RMS        float64
	RMSDBFS    float64
	Silent     bool
	SilentRate float64 // fraction of samples that were exactly zero
}

// StatsInt16 computes peak/RMS statistics over interleaved 16-bit PCM.
func StatsInt16(pcm []int16, channels int) PCMStats {
	st := PCMStats{Channels: channels}
	if channels <= 0 {
		channels = 1
	}
	if len(pcm) == 0 {
		st.PeakDBFS = math.Inf(-1)
		st.RMSDBFS = math.Inf(-1)
		st.Silent = true
		st.SilentRate = 1
		return st
	}

	var sumSq float64
	var zeros int64
	var peak int32
	for _, s := range pcm {
		v := int32(s)
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
		if s == 0 {
			zeros++
		}
		f := float64(s) / 32768
		sumSq += f * f
	}

	st.Frames = int64(len(pcm) / channels)
	st.Peak = float64(peak) / 32768
	st.RMS = math.Sqrt(sumSq / float64(len(pcm)))
	st.SilentRate = float64(zeros) / float64(len(pcm))
	st.Silent = peak == 0
	st.PeakDBFS = DBFS(st.Peak)
	st.RMSDBFS = DBFS(st.RMS)
	return st
}

// DBFS converts a linear full-scale ratio to dBFS. Zero and negative values map
// to negative infinity.
func DBFS(linear float64) float64 {
	if linear <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(linear)
}

// FormatDBFS renders a dBFS value for human consumption.
func FormatDBFS(v float64) string {
	if math.IsInf(v, -1) {
		return "-inf"
	}
	return fmt.Sprintf("%.2f", v)
}
