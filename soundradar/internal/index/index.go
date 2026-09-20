// Package index implements the soundradar P2 quantised fingerprint index: the
// lightest possible "vector database" for sound-effect matching.
//
// # What is stored
//
// One template per library SAMPLE (an item may carry several variants: near /
// far / through a wall). A template is the 2048-dimensional normalised log-mel
// patch (see internal/dsp) taken from the loudest part of that sample, reduced
// to one int8 per dimension plus a float32 scale factor. An item's score is the
// best score of any of its samples.
//
// # Binary format of index.bin (v1)
//
// All integers are little-endian. There is no padding anywhere.
//
//	offset  size  field
//	0       4     magic "SRZ1"
//	4       4     format version (uint32) = 1
//	8       2     algorithm name length (uint16)
//	10      n     algorithm name, ASCII, e.g. "mel-goertzel-v1"
//	...     2     parameter JSON length (uint16)
//	...     m     parameter JSON (canonical, the same bytes that are hashed)
//	...     64    fingerprint: sha256 hex (lower case ASCII) of the parameter JSON
//	...     4     dimension (uint32) == MelBands * WindowFrames
//	...     8     template count (uint64)
//	...     4     item count (uint32)
//	then, for every item:
//	        2     id length (uint16)
//	        a     id bytes (UTF-8)
//	        2     name length (uint16)
//	        b     name bytes (UTF-8)
//	        4     sample count (uint32), i.e. how many templates it owns
//	        4     template start index (uint32) into the template block
//	        4     detection threshold (float32, 0 = "use the engine default")
//	        4     cooldown in ms (uint32, 0 = "use the engine default")
//	then, for every template:
//	        2     sample file name length (uint16)
//	        s     sample file name bytes (UTF-8), e.g. "samples/0001.wav"
//	        4     sample index inside the item (uint32)
//	        2     item index (uint16)
//	        4     quantisation scale (float32): value = int8 * scale
//	then the int8 block: templateCount * dim bytes, one int8 per dimension,
//	     templates in order (template t occupies bytes [t*dim, (t+1)*dim)).
//	then, for every item, once more:
//	        4     thumbnail template index (uint32), a convenience for diagnostics
//
// Every size is validated while reading; a truncated file, a wrong magic, a
// future version or a mismatched fingerprint all produce a Chinese error
// instead of a panic.
package index

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/wav"
)

// Format identity.
const (
	// Magic is the 4-byte file signature.
	Magic = "SRZ1"
	// FormatVersion is bumped whenever the layout below changes.
	FormatVersion uint32 = 1
	// DefaultName is the file name used when the caller does not choose one.
	DefaultName = "index.bin"
	// MaxNameLen bounds the strings read from a file (defensive limit).
	MaxNameLen = 4096
)

// ---------------------------------------------------------------------------
// public types
// ---------------------------------------------------------------------------

// ItemScore is one search hit. Score is an ItemScoreValue (a float64 plus the
// winning template index), so `s.Score.Float()` is the cosine similarity in
// [-1, 1] and `ix.ItemAt(s)` reports which stored sample variant produced it.
type ItemScore struct {
	ID    string
	Name  string
	Score ItemScoreValue
}

// ItemScoreValue is a score that remembers where it came from.
type ItemScoreValue struct {
	Value float64 // cosine similarity in [-1, 1]

	template int // index into the template block, -1 when unknown
}

// Float returns the score as a plain float64.
func (s ItemScoreValue) Float() float64 { return s.Value }

// TemplateIndex returns the index of the template that produced the score
// (negative when the score did not come from a search).
func (s ItemScoreValue) TemplateIndex() int { return s.template }

// ItemAt returns the sample provenance of a hit produced by Search.
func (ix *Index) ItemAt(hit ItemScore) (file string, sampleIndex int) {
	ti := hit.Score.template
	if ti < 0 || ti >= len(ix.vecs) {
		return "", 0
	}
	return ix.vecs[ti].sampleFile, ix.vecs[ti].sampleIndex
}

// Stats summarises an index (also used by the CLI report).
type Stats struct {
	Items       int
	Samples     int
	Dim         int
	Bytes       int // size of the quantised vector block
	FileBytes   int64
	Fingerprint string
}

// Warning records a sample that could not be used.
type Warning struct {
	Item    string
	Sample  string
	Message string
}

// BuildStats is the result of scanning a library.
type BuildStats struct {
	LibraryPath string
	LibraryName string
	Items       int
	Samples     int // samples turned into templates
	Skipped     int
	Dim         int
	Bytes       int // quantised bytes written for vectors
	Duration    time.Duration
	PerItem     []ItemInfo
	Warnings    []Warning
}

// ItemInfo is the per-item part of a build report.
type ItemInfo struct {
	ID      string
	Name    string
	Samples int
	Skipped int
}

// vec is one quantised template: dim int8 values plus the scale that maps them
// back to floats (value = int8 * scale).
type vec struct {
	q           []int8
	scale       float32
	item        int    // index into Index.items
	sampleFile  string // container path of the sample it came from
	sampleIndex int    // 0-based sample index inside the item
}

// item is one library entry.
type item struct {
	id      string
	name    string
	samples int // number of templates
	first   int // index of the first template (informational; the per-template
	// item field is authoritative for scoring)
	thumb      int     // template index of the anchor, for diagnostics
	threshold  float64 // per-item detection threshold (0 = engine default)
	cooldownMs int     // per-item cooldown (0 = engine default)
}

// Index is a loaded, searchable fingerprint index.
type Index struct {
	params    dsp.Params
	fp        string
	dim       int
	items     []item
	vecs      []vec
	fileBytes int64
	// build carries the diagnostics of the Build that produced this index; it is
	// nil after a Load (nothing was scanned in this process).
	build *BuildStats
}

// ---------------------------------------------------------------------------
// building
// ---------------------------------------------------------------------------

// Build opens libraryPath and computes an index over every sample of every
// item. Samples that are too short for one window are skipped with a warning
// instead of failing the build.
func Build(libraryPath string) (*Index, error) {
	return BuildWithParams(libraryPath, dsp.DefaultParams())
}

// BuildWithParams is Build with explicit fingerprint parameters.
func BuildWithParams(libraryPath string, p dsp.Params) (*Index, error) {
	start := time.Now()
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("特征参数无效: %w", err)
	}
	if strings.TrimSpace(libraryPath) == "" {
		return nil, errors.New("音效库路径为空")
	}

	store, err := library.Open(libraryPath)
	if err != nil {
		return nil, fmt.Errorf("打开音效库失败: %w", err)
	}

	ix := &Index{
		params: p,
		fp:     p.Fingerprint(),
		dim:    p.Dim(),
	}
	bs := &BuildStats{
		LibraryPath: store.Path(),
		LibraryName: store.Manifest().Name,
		Dim:         p.Dim(),
	}
	ix.build = bs

	minSamples := p.FrameSize + (p.WindowFrames-1)*p.HopSize
	for _, id := range store.Order() {
		it := store.Get(id)
		if it == nil {
			continue
		}
		bs.Items++
		info := ItemInfo{ID: id, Name: it.Name}
		ii := len(ix.items)
		ix.items = append(ix.items, item{
			id:         id,
			name:       it.Name,
			first:      len(ix.vecs),
			threshold:  it.Threshold,
			cooldownMs: it.CooldownMs,
		})

		for si := range it.Samples {
			wavBytes := it.SampleWAV(si)
			sampleName := it.Samples[si].File
			if len(wavBytes) == 0 {
				info.Skipped++
				bs.Skipped++
				bs.Warnings = append(bs.Warnings, Warning{Item: id, Sample: sampleName, Message: "样本数据为空（库里没有这一项音频）"})
				continue
			}
			audio, err := wav.Read(bytes.NewReader(wavBytes))
			if err != nil {
				info.Skipped++
				bs.Skipped++
				bs.Warnings = append(bs.Warnings, Warning{Item: id, Sample: sampleName, Message: "WAV 解析失败: " + err.Error()})
				continue
			}
			// The library canonicalises every sample to 48 kHz / mono / 16-bit,
			// so anything else means the container was hand-edited. Rescale
			// instead of silently analysing wrong-rate audio.
			samples, how, err := canonicalMono(audio, p.SampleRate)
			if err != nil {
				info.Skipped++
				bs.Skipped++
				bs.Warnings = append(bs.Warnings, Warning{Item: id, Sample: sampleName, Message: err.Error()})
				continue
			}
			if how != "" {
				bs.Warnings = append(bs.Warnings, Warning{Item: id, Sample: sampleName, Message: how})
			}
			if len(samples) < minSamples {
				info.Skipped++
				bs.Skipped++
				bs.Warnings = append(bs.Warnings, Warning{Item: id, Sample: sampleName, Message: fmt.Sprintf(
					"样本太短（%d 个采样 = %.3f s），凑不满一个特征窗口（需要 %d 个采样 = %.3f s）",
					len(samples), float64(len(samples))/float64(p.SampleRate),
					minSamples, float64(minSamples)/float64(p.SampleRate))})
				continue
			}
			patch, anchor, err := AnchorPatch(p, samples)
			if err != nil {
				info.Skipped++
				bs.Skipped++
				bs.Warnings = append(bs.Warnings, Warning{Item: id, Sample: sampleName, Message: err.Error()})
				continue
			}
			if !patchUsable(patch) {
				info.Skipped++
				bs.Skipped++
				bs.Warnings = append(bs.Warnings, Warning{Item: id, Sample: sampleName, Message: "样本太安静，频谱被压成全零，无法用来打分（把播放音量开大一点再录）"})
				continue
			}
			q, scale := Quantize(patch)
			ix.vecs = append(ix.vecs, vec{
				q:           q,
				scale:       scale,
				item:        ii,
				sampleFile:  sampleName,
				sampleIndex: si,
			})
			ix.items[ii].thumb = anchor
			info.Samples++
			bs.Samples++
		}

		// Item bookkeeping: how many templates ended up in this item's range.
		ix.items[ii].samples = info.Samples
		bs.PerItem = append(bs.PerItem, info)
	}

	bs.Bytes = len(ix.vecs) * ix.dim
	bs.Duration = time.Since(start)
	return ix, nil
}

// canonicalMono returns mono samples at rate, converting whatever the WAV
// actually holds. It reports a non-empty human message when a conversion was
// necessary (so the build report can warn about a non-canonical library).
func canonicalMono(a *wav.Audio, rate int) ([]float32, string, error) {
	msg := ""
	if a.Info.SampleRate != rate {
		msg = fmt.Sprintf("样本采样率是 %d Hz（库里本应是 %d Hz），已按线性插值重采样", a.Info.SampleRate, rate)
	}
	mono := a.Samples
	if a.Info.Channels > 1 {
		mono = a.Mono()
		if msg != "" {
			msg += "；"
		}
		msg += fmt.Sprintf("样本是 %d 声道（库里本应是单声道），已降混", a.Info.Channels)
	}
	out := make([]float32, len(mono))
	for i, v := range mono {
		out[i] = float32(v)
	}
	if a.Info.SampleRate != rate {
		out = resample(out, a.Info.SampleRate, rate)
	}
	return out, msg, nil
}

// resample is a linear-interpolation resampler (the same trade-off as
// internal/audio: no anti-aliasing filter, but the library never needs it
// because P1 always stores 48 kHz mono).
func resample(src []float32, srcRate, dstRate int) []float32 {
	if srcRate == dstRate || srcRate <= 0 || dstRate <= 0 || len(src) == 0 {
		return src
	}
	outLen := int(math.Round(float64(len(src)) * float64(dstRate) / float64(srcRate)))
	if outLen < 1 {
		outLen = 1
	}
	out := make([]float32, outLen)
	ratio := float64(srcRate) / float64(dstRate)
	last := len(src) - 1
	for i := 0; i < outLen; i++ {
		pos := float64(i) * ratio
		idx := int(pos)
		if idx >= last {
			out[i] = src[last]
			continue
		}
		frac := float32(pos - float64(idx))
		out[i] = src[idx] + frac*(src[idx+1]-src[idx])
	}
	return out
}

// anchorSmoothFrames is the length of the centred moving average applied to the
// frame-energy curve before the anchor frame is chosen.
//
// Why this is not a nicety: a sound effect is typically a burst of tens of
// milliseconds, so several frames around its middle have nearly identical
// energy. Picking the single loudest frame then means that a little ambience
// decides which frame wins - measured on synthetic bursts, the argmax moved by
// up to +/-5 frames (27 ms) between two noise realisations. The template and the
// live window were therefore describing DIFFERENT physical frames, and the
// cosine score collapsed even though the sound was clearly audible. Smoothing
// the curve makes the choice a property of the sound's envelope rather than of
// the noise.
//
// 15 frames is about 80 ms at the canonical 5.333 ms hop: long enough to settle
// the burst's plateau, short enough not to drag the anchor onto a longer
// background swell.
const anchorSmoothFrames = 15

// frameEnergies returns the per-frame mean-square of pcm (the same quantity the
// anchor has always been chosen from).
func frameEnergies(p dsp.Params, pcm []float32) []float64 {
	n := p.FrameCount(len(pcm))
	if n <= 0 {
		return nil
	}
	e := make([]float64, n)
	for i := 0; i < n; i++ {
		start := i * p.HopSize
		end := start + p.FrameSize
		if end > len(pcm) {
			end = len(pcm)
		}
		if start >= end {
			continue
		}
		var sq float64
		for _, v := range pcm[start:end] {
			sq += float64(v) * float64(v)
		}
		e[i] = sq / float64(end-start)
	}
	return e
}

// smoothEnergies applies a centred moving average of length n (n <= 1 is a
// no-op). The window shrinks at the edges instead of being padded, so a sound at
// the very start of a recall is still found.
func smoothEnergies(e []float64, n int) []float64 {
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
		hi := i + half + 1
		if hi > len(e) {
			hi = len(e)
		}
		var s float64
		for k := lo; k < hi; k++ {
			s += e[k]
		}
		out[i] = s / float64(hi-lo)
	}
	return out
}

// anchorFrame returns the frame index the patch should be centred on: the argmax
// of the smoothed frame-energy curve.
func anchorFrame(p dsp.Params, pcm []float32) (int, []float64) {
	e := frameEnergies(p, pcm)
	if len(e) == 0 {
		return -1, nil
	}
	sm := smoothEnergies(e, anchorSmoothFrames)
	best, bestV := 0, -1.0
	for i, v := range sm {
		if v > bestV {
			best, bestV = i, v
		}
	}
	return best, e
}

// AnchorPatch analyses pcm and returns the normalised patch centred on the
// sound's energy peak, together with the index of the frame the patch ends on.
//
// The anchor is the argmax of the SMOOTHED frame energy (see
// anchorSmoothFrames), not of the raw frame energy and not the 186 ms window
// with the highest summed log-mel. A recall snapshot is several seconds long and
// the real sound is often only a few tens of milliseconds: summing energy across
// the whole window would let the long, quieter background outvote that click,
// and the stored vector would then match other snapshots' background.
//
// The patch is centred on the anchor so a short click is not clipped by the
// window edge. The live matcher slides the same window over the audio, so the
// stored alignment only has to be consistent - which is exactly what the
// smoothing buys.
func AnchorPatch(p dsp.Params, pcm []float32) ([]float32, int, error) {
	frames := p.Frames(pcm)
	if len(frames) < p.WindowFrames {
		return nil, -1, fmt.Errorf("样本太短（%d 帧），凑不满一个 %d 帧的特征窗口", len(frames), p.WindowFrames)
	}
	best, _ := anchorFrame(p, pcm)
	if best < 0 {
		return nil, -1, fmt.Errorf("样本太短（%d 个采样），一帧都算不出来", len(pcm))
	}
	half := (p.WindowFrames - 1) / 2
	start := best - half
	if start < 0 {
		start = 0
	}
	nWin := len(frames) - p.WindowFrames + 1
	if start > nWin-1 {
		start = nWin - 1
	}

	dim := p.Dim()
	patch := make([]float32, 0, dim)
	for f := start; f < start+p.WindowFrames; f++ {
		patch = append(patch, frames[f]...)
	}
	return dsp.Normalize(patch), start + p.WindowFrames - 1, nil
}

// patchUsable reports whether a normalised patch carries any shape. The zero
// vector is what a silent window collapses to, and it scores 0 against
// everything, so storing it would create an item that can never hit.
func patchUsable(patch []float32) bool {
	for _, x := range patch {
		if x != 0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// quantisation
// ---------------------------------------------------------------------------

// Quantize maps a unit vector to symmetric int8 codes.
//
// The scale is maxAbs/127 so the largest component always uses the full
// positive range (and -maxAbs the full negative range). Rounding is
// round-half-away-from-zero, and the result is clamped to [-127, 127] so a
// value can never wrap to -128 (which would flip a large positive component to
// a large negative one). An all-zero vector gets scale 0 and is stored as
// zeros.
func Quantize(v []float32) ([]int8, float32) {
	maxAbs := 0.0
	for _, x := range v {
		if a := math.Abs(float64(x)); a > maxAbs {
			maxAbs = a
		}
	}
	if maxAbs == 0 || math.IsNaN(maxAbs) || math.IsInf(maxAbs, 0) {
		return make([]int8, len(v)), 0
	}
	scale := maxAbs / 127
	q := make([]int8, len(v))
	for i, x := range v {
		r := int(math.Round(float64(x) / scale))
		if r > 127 {
			r = 127
		}
		if r < -127 {
			r = -127
		}
		q[i] = int8(r)
	}
	return q, float32(scale)
}

// Dequantize reconstructs the float vector from the stored codes.
// Dequantize(Quantize(v)) is the lossy round trip measured by the SNR test.
func Dequantize(q []int8, scale float32) []float32 {
	out := make([]float32, len(q))
	for i, c := range q {
		out[i] = float32(c) * scale
	}
	return out
}

// ---------------------------------------------------------------------------
// accessors
// ---------------------------------------------------------------------------

// Dim returns the fingerprint dimension.
func (ix *Index) Dim() int { return ix.dim }

// Fingerprint returns the parameter fingerprint the index was built with. It
// equals dsp.Params.Fingerprint() of the analyzer that must be used with it.
func (ix *Index) Fingerprint() string { return ix.fp }

// Params returns the fingerprint parameters stored in the index.
func (ix *Index) Params() dsp.Params { return ix.params }

// Stats returns (items, samples, bytes).
func (ix *Index) Stats() (items, samples, bytes int) {
	return len(ix.items), len(ix.vecs), len(ix.vecs) * ix.dim
}

// BuildReport returns the diagnostics of the Build that produced this index, or
// nil when the index came from Load.
func (ix *Index) BuildReport() *BuildStats { return ix.build }

// Threshold returns the per-item detection threshold stored in the index, or 0
// when the item has none (the caller then applies its own default).
func (ix *Index) Threshold(id string) float64 {
	for i := range ix.items {
		if ix.items[i].id == id {
			return ix.items[i].threshold
		}
	}
	return 0
}

// CooldownMs returns the per-item cooldown stored in the index, or 0 when the
// item has none.
func (ix *Index) CooldownMs(id string) int {
	for i := range ix.items {
		if ix.items[i].id == id {
			return ix.items[i].cooldownMs
		}
	}
	return 0
}

// ItemName returns the display name for an id (the id itself when unknown).
func (ix *Index) ItemName(id string) string {
	for i := range ix.items {
		if ix.items[i].id == id {
			return ix.items[i].name
		}
	}
	return id
}

// Items returns every item id, in index order.
func (ix *Index) Items() []string {
	out := make([]string, 0, len(ix.items))
	for i := range ix.items {
		out = append(out, ix.items[i].id)
	}
	return out
}

// LibraryPath returns the library this index was built from, or "" when the
// index was only loaded from disk (nothing was scanned in this process).
func (ix *Index) LibraryPath() string {
	if ix.build == nil {
		return ""
	}
	return ix.build.LibraryPath
}

// LibraryName returns the library name recorded at build time.
func (ix *Index) LibraryName() string {
	if ix.build == nil {
		return ""
	}
	return ix.build.LibraryName
}

// Empty reports whether the index holds no templates.
func (ix *Index) Empty() bool { return len(ix.vecs) == 0 }

// ---------------------------------------------------------------------------
// search
// ---------------------------------------------------------------------------

// Search scores window (an already-normalised unit vector of Dim() values)
// against every stored template and returns the best topN items, highest score
// first. Each item appears at most once, with the score of its best sample.
//
// The inner loop is a plain dot product over int8 codes scaled on the fly so no
// dequantised copy has to be kept in memory; the int8 block is contiguous and
// walked in order, which is what keeps 1600x2048 under the 3 ms budget.
func (ix *Index) Search(window []float32, topN int) []ItemScore {
	if len(window) != ix.dim || len(ix.vecs) == 0 {
		return nil
	}
	scores := make([]float64, len(ix.items))
	best := make([]int, len(ix.items))
	for i := range scores {
		scores[i] = math.Inf(-1)
		best[i] = -1
	}
	for ti := range ix.vecs {
		v := &ix.vecs[ti]
		var acc float64
		q := v.q
		for d := 0; d < ix.dim; d++ {
			if c := q[d]; c != 0 {
				acc += float64(window[d]) * float64(c)
			}
		}
		acc *= float64(v.scale)
		// int8 rounding can leave the stored template's norm a hair above 1, so
		// clamp to the [-1, 1] a cosine similarity is defined on.
		if acc > 1 {
			acc = 1
		} else if acc < -1 {
			acc = -1
		}
		if acc > scores[v.item] {
			scores[v.item] = acc
			best[v.item] = ti
		}
	}

	idx := make([]int, 0, len(ix.items))
	for i := range ix.items {
		if best[i] >= 0 {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return scores[idx[a]] > scores[idx[b]]
	})
	if topN > 0 && len(idx) > topN {
		idx = idx[:topN]
	}
	out := make([]ItemScore, 0, len(idx))
	for _, i := range idx {
		out = append(out, ItemScore{
			ID:    ix.items[i].id,
			Name:  ix.items[i].name,
			Score: ItemScoreValue{Value: scores[i], template: best[i]},
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

type writer struct {
	w   *bytes.Buffer
	err error
}

func (w *writer) u16(v uint16) { w.rawU16(v) }
func (w *writer) u32(v uint32) { w.rawU32(v) }
func (w *writer) u64(v uint64) { w.rawU64(v) }

func (w *writer) rawU16(v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	w.w.Write(b[:])
}
func (w *writer) rawU32(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.w.Write(b[:])
}
func (w *writer) rawU64(v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.w.Write(b[:])
}
func (w *writer) f32(v float32) {
	w.rawU32(math.Float32bits(v))
}
func (w *writer) str(s string) {
	if len(s) > MaxNameLen {
		s = s[:MaxNameLen]
	}
	w.u16(uint16(len(s)))
	w.w.WriteString(s)
}

// Encode serialises the index using the layout documented at the top of this
// file. It is exported so tests and tools can inspect the exact bytes.
func (ix *Index) Encode() ([]byte, error) {
	if ix.dim <= 0 || ix.dim > 1<<20 {
		return nil, fmt.Errorf("索引维数异常: %d", ix.dim)
	}
	var buf bytes.Buffer
	w := &writer{w: &buf}
	w.w.WriteString(Magic)
	w.u32(FormatVersion)
	w.str(dsp.Algorithm)
	w.str(string(ix.params.ParamJSON()))
	w.str(ix.fp)
	w.u32(uint32(ix.dim))
	w.u64(uint64(len(ix.vecs)))
	w.u32(uint32(len(ix.items)))

	for _, it := range ix.items {
		w.str(it.id)
		w.str(it.name)
		w.u32(uint32(it.samples))
		w.u32(uint32(it.first))
		w.f32(float32(it.threshold))
		w.u32(uint32(it.cooldownMs))
	}
	for _, v := range ix.vecs {
		if v.item < 0 || v.item >= len(ix.items) {
			return nil, fmt.Errorf("内部错误: 模板的条目下标 %d 越界", v.item)
		}
		w.str(v.sampleFile)
		w.u32(uint32(v.sampleIndex))
		w.u16(uint16(v.item))
		w.f32(v.scale)
	}
	for _, v := range ix.vecs {
		if len(v.q) != ix.dim {
			return nil, fmt.Errorf("内部错误: 模板维数 %d != %d", len(v.q), ix.dim)
		}
		b := make([]byte, len(v.q))
		for i, c := range v.q {
			b[i] = byte(c)
		}
		buf.Write(b)
	}
	for _, it := range ix.items {
		w.u32(uint32(it.thumb))
	}
	return buf.Bytes(), w.err
}

// Save writes the index atomically: the bytes go to a temporary file in the
// target directory, are flushed, and only then replace path.
func (ix *Index) Save(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("索引输出路径为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	raw, err := ix.Encode()
	if err != nil {
		return err
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录 %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".index-*.bin.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(raw); err != nil {
		cleanup()
		return fmt.Errorf("写入临时索引: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("同步临时索引: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("关闭临时索引: %w", err)
	}
	if err := replaceFile(tmpName, abs); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("替换索引文件 %s: %w", abs, err)
	}
	syncDir(dir)
	return nil
}

// reader wraps a byte slice with bounds-checked little-endian reads.
type reader struct {
	b   []byte
	off int
}

var errTruncated = errors.New("文件被截断（数据不足）")

func (r *reader) need(n int) error {
	if n < 0 || r.off+n > len(r.b) {
		return fmt.Errorf("%w: 需要 %d 字节，剩余 %d 字节", errTruncated, n, len(r.b)-r.off)
	}
	return nil
}

func (r *reader) u16() (uint16, error) {
	if err := r.need(2); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint16(r.b[r.off:])
	r.off += 2
	return v, nil
}
func (r *reader) u32() (uint32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint32(r.b[r.off:])
	r.off += 4
	return v, nil
}
func (r *reader) u64() (uint64, error) {
	if err := r.need(8); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint64(r.b[r.off:])
	r.off += 8
	return v, nil
}
func (r *reader) f32() (float32, error) {
	v, err := r.u32()
	if err != nil {
		return 0, err
	}
	return math.Float32frombits(v), nil
}
func (r *reader) str() (string, error) {
	n, err := r.u16()
	if err != nil {
		return "", err
	}
	if int(n) > MaxNameLen {
		return "", fmt.Errorf("字符串长度 %d 超过上限 %d（索引文件已损坏）", n, MaxNameLen)
	}
	if err := r.need(int(n)); err != nil {
		return "", err
	}
	s := string(r.b[r.off : r.off+int(n)])
	r.off += int(n)
	return s, nil
}

// Load reads an index and verifies magic, version, parameter fingerprint and
// every declared length. A damaged or incompatible file produces a Chinese
// error; Load never panics on malformed input.
func Load(path string) (*Index, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ix, err := Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if fi, err := os.Stat(path); err == nil {
		ix.fileBytes = fi.Size()
	}
	return ix, nil
}

// Decode parses the in-memory image of index.bin.
func Decode(raw []byte) (*Index, error) {
	r := &reader{b: raw}
	if len(raw) < 4 || string(raw[:4]) != Magic {
		return nil, fmt.Errorf("不是 soundradar 索引文件（文件头应为 %q）", Magic)
	}
	r.off = 4

	ver, err := r.u32()
	if err != nil {
		return nil, err
	}
	if ver != FormatVersion {
		return nil, fmt.Errorf("索引格式版本 %d 不受支持（本程序支持 %d），请重新执行 index rebuild", ver, FormatVersion)
	}
	algo, err := r.str()
	if err != nil {
		return nil, err
	}
	if algo != dsp.Algorithm {
		return nil, fmt.Errorf("索引的算法是 %q，本程序是 %q，请重新执行 index rebuild", algo, dsp.Algorithm)
	}
	paramJSON, err := r.str()
	if err != nil {
		return nil, err
	}
	fp, err := r.str()
	if err != nil {
		return nil, err
	}
	dim, err := r.u32()
	if err != nil {
		return nil, err
	}
	if dim == 0 || dim > 1<<20 {
		return nil, fmt.Errorf("索引声明的维数 %d 不合理（文件已损坏）", dim)
	}
	nVec, err := r.u64()
	if err != nil {
		return nil, err
	}
	nItem, err := r.u32()
	if err != nil {
		return nil, err
	}
	if nItem > 1<<24 {
		return nil, fmt.Errorf("索引声明的条目数 %d 不合理（文件已损坏）", nItem)
	}

	ix := &Index{dim: int(dim), fp: fp, fileBytes: int64(len(raw))}
	if err := ix.paramsFromJSON(paramJSON); err != nil {
		return nil, err
	}
	want := ix.params.Fingerprint()
	if fp != want {
		return nil, fmt.Errorf("特征指纹不匹配：索引是 %s，当前程序是 %s（参数已改动，请重新执行 index rebuild）",
			short(fp), short(want))
	}
	if ix.params.Dim() != int(dim) {
		return nil, fmt.Errorf("索引维数 %d 与参数维数 %d 不一致（文件已损坏）", dim, ix.params.Dim())
	}

	ix.items = make([]item, 0, nItem)
	for i := uint32(0); i < nItem; i++ {
		id, err := r.str()
		if err != nil {
			return nil, err
		}
		name, err := r.str()
		if err != nil {
			return nil, err
		}
		ns, err := r.u32()
		if err != nil {
			return nil, err
		}
		first, err := r.u32()
		if err != nil {
			return nil, err
		}
		threshold, err := r.f32()
		if err != nil {
			return nil, err
		}
		cooldown, err := r.u32()
		if err != nil {
			return nil, err
		}
		if uint64(first) > nVec {
			return nil, fmt.Errorf("条目 %d 的模板起点 %d 超出模板总数 %d（文件已损坏）", i, first, nVec)
		}
		if uint64(first)+uint64(ns) > nVec {
			return nil, fmt.Errorf("条目 %d 声明 %d 个样本，模板起点 %d，超出模板总数 %d（文件已损坏）", i, ns, first, nVec)
		}
		if math.IsNaN(float64(threshold)) || threshold < 0 || threshold > 1 {
			return nil, fmt.Errorf("条目 %d 的阈值 %v 不在 [0,1] 内（文件已损坏）", i, threshold)
		}
		ix.items = append(ix.items, item{
			id: id, name: name, samples: int(ns), first: int(first),
			threshold: float64(threshold), cooldownMs: int(cooldown),
		})
	}

	ix.vecs = make([]vec, 0, nVec)
	for i := uint64(0); i < nVec; i++ {
		file, err := r.str()
		if err != nil {
			return nil, err
		}
		si, err := r.u32()
		if err != nil {
			return nil, err
		}
		owner, err := r.u16()
		if err != nil {
			return nil, err
		}
		scale, err := r.f32()
		if err != nil {
			return nil, err
		}
		if int(owner) >= len(ix.items) {
			return nil, fmt.Errorf("模板 %d 的条目下标 %d 超出条目总数 %d（文件已损坏）", i, owner, len(ix.items))
		}
		ix.vecs = append(ix.vecs, vec{
			scale:       scale,
			item:        int(owner),
			sampleFile:  file,
			sampleIndex: int(si),
		})
	}

	blockBytes := int(nVec) * int(dim)
	if err := r.need(blockBytes); err != nil {
		return nil, fmt.Errorf("量化向量块不完整: %w", err)
	}
	block := raw[r.off : r.off+blockBytes]
	r.off += blockBytes
	for i := range ix.vecs {
		seg := block[i*int(dim) : (i+1)*int(dim)]
		q := make([]int8, len(seg))
		for j, b := range seg {
			q[j] = int8(b)
		}
		ix.vecs[i].q = q
	}

	for i := range ix.items {
		thumb, err := r.u32()
		if err != nil {
			return nil, err
		}
		ix.items[i].thumb = int(thumb)
	}
	if r.off != len(raw) {
		return nil, fmt.Errorf("索引尾部有 %d 字节的多余数据（文件版本或内容不匹配）", len(raw)-r.off)
	}
	return ix, nil
}

// paramsFromJSON restores dsp.Params from the stored canonical document. It
// decodes into dsp.ParamDoc (the same type dsp hashes) and recomputes the
// fingerprint from the re-marshalled document, so this package cannot drift from
// the fingerprint definition: a field added on the dsp side is carried through
// automatically. TestParamsJSONRoundTrip in dsp locks that property down.
func (ix *Index) paramsFromJSON(doc string) error {
	var raw dsp.ParamDoc
	if err := json.Unmarshal([]byte(doc), &raw); err != nil {
		return fmt.Errorf("索引里的特征参数无法解析: %w", err)
	}
	if raw.Algorithm != dsp.Algorithm || raw.Version != dsp.Version {
		return fmt.Errorf("索引的算法/版本是 %s v%d，本程序是 %s v%d，请重新执行 index rebuild",
			raw.Algorithm, raw.Version, dsp.Algorithm, dsp.Version)
	}
	if raw.Norm != dsp.Norm {
		return fmt.Errorf("索引的归一化方式 %q 与当前程序 %q 不一致，请重新执行 index rebuild", raw.Norm, dsp.Norm)
	}
	ix.params = dsp.Params{
		SampleRate:   raw.SampleRate,
		FrameSize:    raw.FrameSize,
		HopSize:      raw.HopSize,
		MelBands:     raw.MelBands,
		FMinHz:       int(math.Round(raw.FMinHz)),
		FMaxHz:       int(math.Round(raw.FMaxHz)),
		WindowFrames: raw.WindowFrames,
	}
	if len(raw.Noise) > 0 {
		var n dsp.NoiseParams
		if err := json.Unmarshal(raw.Noise, &n); err != nil {
			return fmt.Errorf("索引里的环境音参数无法解析: %w", err)
		}
		ix.params.Noise = n
	}
	if err := ix.params.Validate(); err != nil {
		return fmt.Errorf("索引里的特征参数不合法: %w", err)
	}
	ix.fp = dsp.FingerprintDoc(raw)
	return nil
}

func short(fp string) string {
	if len(fp) > 12 {
		return fp[:12] + "…"
	}
	return fp
}

// ---------------------------------------------------------------------------
// helpers used by the CLI
// ---------------------------------------------------------------------------

// DefaultPathFor returns the default index path for a library path:
// <library directory>/index.bin.
func DefaultPathFor(libraryPath string) string {
	dir := filepath.Dir(libraryPath)
	if dir == "" || dir == "." {
		return DefaultName
	}
	return filepath.Join(dir, DefaultName)
}

// LoadOrBuild loads the index at indexPath when it exists, matches the current
// parameters, and is at least as new as the library. Otherwise it builds the
// index from libraryPath and saves it. rebuilt reports whether a rebuild
// happened; reason explains why (empty when the file was reused).
//
// A library that was edited after the index was written is stale even when the
// feature fingerprint still matches: live scoring would otherwise keep ranking
// against sounds that are no longer the library.
func LoadOrBuild(libraryPath, indexPath string, p dsp.Params) (ix *Index, rebuilt bool, reason string, err error) {
	if strings.TrimSpace(indexPath) == "" {
		return nil, false, "", errors.New("索引路径为空")
	}
	if _, statErr := os.Stat(indexPath); statErr == nil {
		loaded, lerr := Load(indexPath)
		if lerr == nil && loaded.fp == p.Fingerprint() {
			if newer, why := libraryNewerThanIndex(libraryPath, indexPath); newer {
				reason = why
			} else {
				return loaded, false, "", nil
			}
		} else if lerr == nil {
			reason = "索引的特征指纹与当前参数不一致"
		} else {
			reason = "索引文件无法读取（" + lerr.Error() + "）"
		}
	} else if errors.Is(statErr, os.ErrNotExist) {
		reason = "索引文件不存在"
	} else {
		reason = "无法访问索引文件（" + statErr.Error() + "）"
	}

	ix, err = BuildWithParams(libraryPath, p)
	if err != nil {
		return nil, false, reason, err
	}
	if err := ix.Save(indexPath); err != nil {
		return nil, false, reason, fmt.Errorf("保存索引失败: %w", err)
	}
	return ix, true, reason, nil
}

// libraryNewerThanIndex reports whether the library file was written after the
// index. Equal timestamps are not newer: a rebuild just saved the index, and
// FAT/NTFS can stamp both files in the same second.
func libraryNewerThanIndex(libraryPath, indexPath string) (bool, string) {
	libInfo, err := os.Stat(libraryPath)
	if err != nil {
		return false, ""
	}
	idxInfo, err := os.Stat(indexPath)
	if err != nil {
		return false, ""
	}
	if libInfo.ModTime().After(idxInfo.ModTime()) {
		return true, "音效库比索引新（入库之后索引没有更新）"
	}
	return false, ""
}

// FileBytes returns the on-disk size recorded at Load/Save time (0 when the
// index was only built in memory).
func (ix *Index) FileBytes() int64 { return ix.fileBytes }

// Report renders the human-readable build report used by the CLI.
func (bs *BuildStats) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "库文件      : %s\n", bs.LibraryPath)
	fmt.Fprintf(&b, "库名称      : %s\n", bs.LibraryName)
	fmt.Fprintf(&b, "特征维数    : %d (%s)\n", bs.Dim, dsp.Algorithm)
	fmt.Fprintf(&b, "条目 / 样本 : %d / %d（跳过 %d）\n", bs.Items, bs.Samples, bs.Skipped)
	fmt.Fprintf(&b, "量化字节数  : %d 字节 (%.1f KiB)\n", bs.Bytes, float64(bs.Bytes)/1024)
	fmt.Fprintf(&b, "建库耗时    : %s\n", bs.Duration.Round(time.Microsecond))
	return b.String()
}
