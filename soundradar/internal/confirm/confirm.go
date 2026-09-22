// Package confirm implements the secondary "embedding" gate used after a mel
// hit: a short multi-patch spectral embedding that is cheap enough for an
// onset-triggered check, and robust enough (with index-time noise variants)
// to reject ambience false positives without requiring many real-world samples.
//
// This is intentionally pure Go (no ONNX / CGO). Mel remains the realtime
// scorer; confirm only runs when mel is about to fire.
package confirm

import (
	"fmt"
	"math"
	"math/bits"

	"github.com/znz/soundradar/internal/dsp"
)

// Dim is the embedding length after projection.
const Dim = 256

// Patches is how many mel windows are pooled into one embedding.
const Patches = 3

// AugmentSNRs are the SNR levels (dB) used when building index variants.
// math.Inf(1) means the clean sample.
var AugmentSNRs = []float64{math.Inf(1), 12, 6, 0}

// Version tags the confirm algorithm for index format consumers.
// v3: clean variant is MelEmbed(AnchorPatch); live confirms from the mel window.
const Version = 3

// NeedSamples is how many recent mono samples the live confirm gate reads.
// It matches one mel feature window so the embedding lines up with what mel
// just scored (and with index templates built from the peak-centred crop).
func NeedSamples(p dsp.Params) int {
	return p.FrameSize + (p.WindowFrames-1)*p.HopSize
}

// MelEmbed projects an already-normalised mel patch (Params.Dim floats) into
// the confirm embedding space. Live and index clean variants both use this so
// a true mel hit confirms against itself.
func MelEmbed(patch []float32) []float32 {
	if len(patch) == 0 {
		return nil
	}
	return project(patch)
}

// Embed builds a unit embedding from pcm (48 kHz mono). Longer clips are first
// cropped to one peak-centred mel window. Used for noise-augmented index
// variants; the clean variant should prefer MelEmbed on the AnchorPatch.
func Embed(p dsp.Params, pcm []float32) ([]float32, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	need := NeedSamples(p)
	if len(pcm) < need {
		return nil, fmt.Errorf("confirm: 音频太短（%d 采样，至少 %d）", len(pcm), need)
	}
	if len(pcm) > need {
		pcm = cropToPeakPatch(p, pcm, need)
	}
	patches, err := multiPatches(p, pcm)
	if err != nil {
		return nil, err
	}
	pooled := meanPool(patches)
	return project(pooled), nil
}

// EmbedVariants returns noise-augmented embeddings for index time. The clean
// (infinite SNR) slot is omitted — callers should prepend MelEmbed(anchorPatch).
func EmbedVariants(p dsp.Params, pcm []float32) ([][]float32, error) {
	out := make([][]float32, 0, len(AugmentSNRs)-1)
	for i, snr := range AugmentSNRs {
		if math.IsInf(snr, 1) {
			continue
		}
		src := mixNoise(pcm, snr, uint32(0xC0FFEE+i*97))
		emb, err := Embed(p, src)
		if err != nil {
			continue
		}
		out = append(out, emb)
	}
	return out, nil
}

// Cosine of two unit vectors (or near-unit). Returns -1 on length mismatch.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return -1
	}
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	if s > 1 {
		return 1
	}
	if s < -1 {
		return -1
	}
	return s
}

// BestScore is the max cosine of live against a list of stored variants.
func BestScore(live []float32, variants [][]float32) float64 {
	best := -1.0
	for _, v := range variants {
		if c := Cosine(live, v); c > best {
			best = c
		}
	}
	return best
}

// OnsetOK reports whether the recent frame-energy curve shows a rise of at
// least onsetDB above the quiet background of the preceding frames, OR a
// sustained loud stretch (long tones / friction with no quiet bed to rise from).
// onsetDB <= 0 disables the check (always true).
//
// Only the newest ~0.5 s of frames are considered. Analyzer.FrameEnergy returns
// the whole session; averaging that as the "bed" on a long capture makes every
// short click look flat (rise ≈ 0 dB) and the confirm gate would reject real hits.
func OnsetOK(energies []float64, onsetDB float64) bool {
	if onsetDB <= 0 || len(energies) < 8 {
		return true
	}
	// ~0.5 s at the default 5.33 ms hop (256 / 48000).
	const recentMax = 96
	if len(energies) > recentMax {
		energies = energies[len(energies)-recentMax:]
	}
	n := len(energies)
	// Bed = earlier part of the recent window; tip = the rest (where the click
	// usually sits when mel fires). A fixed "last 8 frames" tip is too short:
	// a 20–80 ms click often lands in the middle of this half-second slice.
	end := n * 3 / 5
	if end < 4 {
		end = n / 2
	}
	if end >= n-1 {
		end = n - 2
	}
	peak := 0.0
	for i := end; i < n; i++ {
		if energies[i] > peak {
			peak = energies[i]
		}
	}
	sum := 0.0
	for i := 0; i < end; i++ {
		sum += energies[i]
	}
	bg := sum / float64(end)
	if bg <= 1e-12 {
		return peak > 1e-10
	}
	if peak <= 0 {
		return false
	}
	rise := 10 * math.Log10(peak/bg)
	if rise >= onsetDB {
		return true
	}
	// Sustained: the earlier "bed" is already loud (no quiet reference), so a
	// rise cannot appear — accept when the recent peak is clearly audible.
	if peak > 1e-5 && bg > peak*0.05 {
		return true
	}
	loud := 0
	thr := bg * math.Pow(10, onsetDB/20)
	if thr < 1e-5 {
		thr = 1e-5
	}
	for i := end; i < n; i++ {
		if energies[i] >= thr {
			loud++
		}
	}
	need := 5
	if post := n - end; post < need {
		need = post
	}
	return loud >= need
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

// cropToPeakPatch returns need samples centred on the smoothed frame-energy
// peak (same idea as index.AnchorPatch). need must be one mel window of PCM.
func cropToPeakPatch(p dsp.Params, pcm []float32, need int) []float32 {
	if len(pcm) <= need {
		return pcm
	}
	frames := p.Frames(pcm)
	if len(frames) < p.WindowFrames {
		return pcm[len(pcm)-need:]
	}
	e := make([]float64, len(frames))
	for i := range frames {
		var s float64
		for _, v := range frames[i] {
			s += float64(v) * float64(v)
		}
		e[i] = s
	}
	sm := smooth(e, 9)
	peak := 0
	for i := range sm {
		if sm[i] > sm[peak] {
			peak = i
		}
	}
	half := (p.WindowFrames - 1) / 2
	startFrame := peak - half
	if startFrame < 0 {
		startFrame = 0
	}
	nWin := len(frames) - p.WindowFrames + 1
	if startFrame > nWin-1 {
		startFrame = nWin - 1
	}
	start := startFrame * p.HopSize
	if start+need > len(pcm) {
		start = len(pcm) - need
	}
	if start < 0 {
		start = 0
	}
	return pcm[start : start+need]
}

func multiPatches(p dsp.Params, pcm []float32) ([][]float32, error) {
	frames := p.Frames(pcm)
	if len(frames) < p.WindowFrames {
		return nil, fmt.Errorf("confirm: 帧数不足（%d < %d）", len(frames), p.WindowFrames)
	}
	e := make([]float64, len(frames))
	for i := range frames {
		// frames are already log-mel; use L2 energy of the frame as a proxy.
		var s float64
		for _, v := range frames[i] {
			s += float64(v) * float64(v)
		}
		e[i] = s
	}
	// Smooth a little so ambience does not pick the argmax.
	sm := smooth(e, 9)
	peak := 0
	for i := range sm {
		if sm[i] > sm[peak] {
			peak = i
		}
	}
	half := (p.WindowFrames - 1) / 2
	centers := []int{peak - 8, peak, peak + 8}
	out := make([][]float32, 0, Patches)
	nWin := len(frames) - p.WindowFrames + 1
	for _, c := range centers {
		start := c - half
		if start < 0 {
			start = 0
		}
		if start >= nWin {
			start = nWin - 1
		}
		patch := make([]float32, 0, p.Dim())
		for f := start; f < start+p.WindowFrames; f++ {
			patch = append(patch, frames[f]...)
		}
		out = append(out, dsp.Normalize(patch))
	}
	return out, nil
}

func meanPool(patches [][]float32) []float32 {
	if len(patches) == 0 {
		return nil
	}
	n := len(patches[0])
	out := make([]float32, n)
	for _, p := range patches {
		if len(p) != n {
			continue
		}
		for i, v := range p {
			out[i] += v
		}
	}
	inv := float32(1) / float32(len(patches))
	for i := range out {
		out[i] *= inv
	}
	return out
}

// project maps a mel patch to Dim with a fixed Rademacher matrix (seeded hash).
// The same matrix is used at index build and live so scores stay comparable.
func project(src []float32) []float32 {
	out := make([]float32, Dim)
	if len(src) == 0 {
		return out
	}
	for d := 0; d < Dim; d++ {
		var acc float64
		for i, v := range src {
			if signBit(uint32(i), uint32(d)) {
				acc += float64(v)
			} else {
				acc -= float64(v)
			}
		}
		out[d] = float32(acc)
	}
	return l2(out)
}

func signBit(i, d uint32) bool {
	// SplitMix64-ish hash → one bit.
	x := i*0x9E3779B9 ^ d*0x85EBCA6B ^ 0xA5A5A5A5
	x ^= x >> 16
	x *= 0x7FEB352D
	x ^= x >> 15
	return bits.OnesCount32(x)&1 == 1
}

func l2(v []float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s <= 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
	return v
}

func smooth(e []float64, n int) []float64 {
	if n <= 1 || len(e) == 0 {
		return e
	}
	half := n / 2
	out := make([]float64, len(e))
	for i := range e {
		lo := i - half
		if lo < 0 {
			lo = 0
		}
		hi := i + half
		if hi >= len(e) {
			hi = len(e) - 1
		}
		sum := 0.0
		for j := lo; j <= hi; j++ {
			sum += e[j]
		}
		out[i] = sum / float64(hi-lo+1)
	}
	return out
}

// mixNoise overlays white noise so the mixture RMS ≈ signalRMS with the given
// SNR in dB. snrDB 0 means noise power equals signal power.
func mixNoise(pcm []float32, snrDB float64, seed uint32) []float32 {
	if len(pcm) == 0 {
		return nil
	}
	var sigPow float64
	for _, v := range pcm {
		sigPow += float64(v) * float64(v)
	}
	sigPow /= float64(len(pcm))
	if sigPow <= 0 {
		return append([]float32(nil), pcm...)
	}
	noisePow := sigPow / math.Pow(10, snrDB/10)
	noiseRMS := math.Sqrt(noisePow)
	out := make([]float32, len(pcm))
	s := seed
	if s == 0 {
		s = 1
	}
	for i, v := range pcm {
		s = s*1664525 + 1013904223
		u := (float64(s>>8)/float64(1<<24))*2 - 1
		out[i] = v + float32(u*noiseRMS*math.Sqrt(3))
	}
	return out
}
