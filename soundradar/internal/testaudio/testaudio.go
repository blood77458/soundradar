// Package testaudio synthesises audio fixtures for tests and for the
// verification subcommands of cmd/wavstat.
//
// It produces structurally valid MPEG-1 Layer III streams whose granules are
// silent: every granule uses Huffman table 0 ("no data") with big_values = 0,
// so a decoder must run its entire frame / side-info / main-data machinery
// while emitting 1152 zero samples per channel per frame.
//
// The sandbox used to build this project has no MP3 encoder and no network
// access to a sample file, which is why the fixture is generated instead of
// downloaded. The function was validated by decoding its output with the real
// go-mp3 decoder (see internal/audio tests).
package testaudio

import (
	"bytes"
	"encoding/binary"
	"math"

	"github.com/znz/soundradar/internal/wav"
)

// MPEG frame geometry.
const (
	// SamplesPerMPEGFrame is the number of samples per channel in one MPEG-1
	// Layer III frame.
	SamplesPerMPEGFrame = 1152
)

// bitWriter packs MSB-first bit fields.
type bitWriter struct {
	buf  []byte
	cur  byte
	nbit uint
}

func (w *bitWriter) writeBits(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		bit := byte((v >> uint(i)) & 1)
		w.cur = (w.cur << 1) | bit
		w.nbit++
		if w.nbit == 8 {
			w.buf = append(w.buf, w.cur)
			w.cur = 0
			w.nbit = 0
		}
	}
}

func (w *bitWriter) flush() []byte {
	if w.nbit > 0 {
		w.cur <<= (8 - w.nbit)
		w.buf = append(w.buf, w.cur)
		w.cur, w.nbit = 0, 0
	}
	return w.buf
}

// SilentMP3FrameSize returns the frame size in bytes and the side-info size for
// the single supported configuration (MPEG-1, Layer III, 48 kHz, stereo,
// 128 kbit/s).
func SilentMP3FrameSize() (frameBytes, sideInfoBytes int) {
	return 384, 32
}

// SilentMP3 builds n structurally valid silent MPEG-1 Layer III frames:
// 48 kHz, stereo, 128 kbit/s (384 bytes per frame, 32 bytes of side info).
// The result decodes to n*1152 zero samples per channel.
func SilentMP3(n int) []byte {
	frameSize, sideInfo := SilentMP3FrameSize()
	out := make([]byte, 0, n*frameSize)

	for f := 0; f < n; f++ {
		frame := make([]byte, frameSize)
		// Header bits: sync 0xFFE | MPEG-1 (11) | Layer III (01) | no CRC |
		// bitrate index 9 (128 kbit/s) | 48 kHz (01) | no padding | stereo (01).
		hdr := uint32(0xFFE00000) |
			3<<19 | // version: MPEG-1
			1<<17 | // layer: III
			1<<16 | // protection bit: no CRC
			9<<12 | // bitrate index: 128 kbit/s
			1<<10 | // sampling frequency: 48 kHz
			1<<6 // channel mode: stereo
		binary.BigEndian.PutUint32(frame[0:4], hdr)

		var w bitWriter
		w.writeBits(0, 9) // main_data_begin
		w.writeBits(0, 3) // private_bits
		for ch := 0; ch < 2; ch++ {
			w.writeBits(0, 4) // scfsi
		}
		for gr := 0; gr < 2; gr++ {
			for ch := 0; ch < 2; ch++ {
				w.writeBits(0, 12) // part2_3_length = 0 -> nothing to decode
				w.writeBits(0, 9)  // big_values = 0
				w.writeBits(0, 8)  // global_gain
				w.writeBits(0, 4)  // scalefac_compress
				w.writeBits(0, 1)  // window_switching_flag = 0 (long blocks)
				w.writeBits(0, 5)  // table_select[0]
				w.writeBits(0, 5)  // table_select[1]
				w.writeBits(0, 5)  // table_select[2]
				w.writeBits(0, 4)  // region0_count
				w.writeBits(0, 3)  // region1_count
				w.writeBits(0, 1)  // preflag
				w.writeBits(0, 1)  // scalefac_scale
				w.writeBits(0, 1)  // count1table_select
			}
		}
		si := w.flush()
		if len(si) != sideInfo {
			// Cannot happen: the field widths above are fixed.
			panic("testaudio: unexpected side info size")
		}
		copy(frame[4:], si)
		out = append(out, frame...)
	}
	return out
}

// ToneWAV synthesises a canonical 16-bit PCM WAV holding a sine tone.
// channels > 1 duplicates the tone into every channel.
func ToneWAV(sampleRate, channels int, seconds, freq, amp float64) []byte {
	n := int(seconds * float64(sampleRate))
	pcm := make([]int16, n*channels)
	for i := 0; i < n; i++ {
		v := wav.ClampInt16(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate)))
		for c := 0; c < channels; c++ {
			pcm[i*channels+c] = v
		}
	}
	var buf bytes.Buffer
	if err := wav.WriteInt16(&buf, sampleRate, channels, pcm); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
