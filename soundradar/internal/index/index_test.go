package index

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znz/soundradar/internal/dsp"
)

// ---------------------------------------------------------------------------
// B7 - quantisation accuracy
// ---------------------------------------------------------------------------

// TestQuantizeSNR measures the signal-to-noise ratio of the symmetric int8
// quantiser on random vectors whose dynamic range matches a real patch.
//
// Expected value: with scale = maxAbs/127 the quantisation step is
// delta = maxAbs/127, and for a unit vector maxAbs is about 3..6 (2048
// dimensions), so delta ~ 0.03..0.05. Uniform rounding noise has variance
// delta^2/12 and the signal (unit vector) has energy 1, hence
// SNR = 10*log10(1/(delta^2/12)) which lands around 45-50 dB - comfortably
// above the 30 dB requirement.
func TestQuantizeSNR(t *testing.T) {
	p := dsp.DefaultParams()
	rnd := newRand(11)

	var sigEnergy, errEnergy float64
	dim := p.Dim()
	const trials = 20
	for trial := 0; trial < trials; trial++ {
		v := make([]float32, dim)
		for i := range v {
			// log-mel-like values: mostly around -40, occasionally near 0
			v[i] = float32(-60 + 60*rnd.next()*rnd.next())
		}
		v = dsp.Normalize(v)
		q, scale := Quantize(v)
		back := Dequantize(q, scale)
		for i := range v {
			sigEnergy += float64(v[i]) * float64(v[i])
			d := float64(v[i]) - float64(back[i])
			errEnergy += d * d
		}
	}
	snr := 10 * math.Log10(sigEnergy/errEnergy)
	t.Logf("int8 量化 SNR = %.2f dB（%d 次随机向量，步长 delta = maxAbs/127）", snr, trials)
	if snr <= 30 {
		t.Fatalf("量化 SNR %.2f dB 未超过 30 dB", snr)
	}

	// The quantised search must return the same top-1 as the unquantised float
	// reference for realistic queries.
	for trial := 0; trial < 50; trial++ {
		counts := []int{3, 5, 2, 8, 1}
		ix := testIndex(p, counts, uint64(100+trial))
		// query one of the stored anchor patches, slightly perturbed
		anchor := ix.vecs[int(rnd.next()*float64(len(ix.vecs)))]
		query := Dequantize(anchor.q, anchor.scale)
		for i := range query {
			query[i] += float32(0.01 * (rnd.next()*2 - 1))
		}
		query = dsp.Normalize(query)

		gotTop := ix.Search(query, 1)
		refTop := bruteSearch(ix, query, 1)
		if len(gotTop) == 0 || len(refTop) == 0 {
			t.Fatalf("第 %d 次查询没有结果", trial)
		}
		if gotTop[0].ID != refTop[0].ID {
			t.Fatalf("第 %d 次查询 top-1 不一致: Search=%s(%.6f) 参考=%s(%.6f)",
				trial, gotTop[0].ID, gotTop[0].Score.Float(), refTop[0].ID, refTop[0].Score.Float())
		}
	}

	// edge cases of the quantiser itself
	q, scale := Quantize(make([]float32, 8))
	if scale != 0 {
		t.Fatalf("全零向量 scale = %v，期望 0", scale)
	}
	for i, c := range q {
		if c != 0 {
			t.Fatalf("全零向量第 %d 维 = %d，期望 0", i, c)
		}
	}
	q, scale = Quantize([]float32{1, -1, 0.5, -0.5})
	if q[0] != 127 || q[1] != -127 {
		t.Fatalf("满量程向量的编码 = %v，期望首项 127、次项 -127", q)
	}
	// scale is stored as float32, so compare with float32 precision: the
	// dequantised maximum must come back to exactly 1.0.
	if back := float32(q[0]) * scale; math.Abs(float64(back)-1) > 1e-6 {
		t.Fatalf("反量化后的最大值 = %v，期望 1.0（scale=%v）", back, scale)
	}
	if q[2] != 64 || q[3] != -64 {
		t.Fatalf("半量程编码 = %v，期望 64 / -64", q[2:4])
	}
}

// ---------------------------------------------------------------------------
// B8 - Search == brute force reference
// ---------------------------------------------------------------------------

func TestSearchMatchesBruteForce(t *testing.T) {
	p := dsp.DefaultParams()
	rnd := newRand(2024)

	// 50 items with between 1 and 4 samples each
	counts := make([]int, 50)
	for i := range counts {
		counts[i] = 1 + int(rnd.next()*4)
	}
	ix := testIndex(p, counts, 7)

	var worstScoreDiff float64
	for trial := 0; trial < 30; trial++ {
		query := make([]float32, p.Dim())
		for i := range query {
			query[i] = float32(-60 + 60*rnd.next())
		}
		query = dsp.Normalize(query)

		for _, topN := range []int{1, 3, 5, 50, 1000} {
			got := ix.Search(query, topN)
			ref := bruteSearch(ix, query, topN)
			if len(got) != len(ref) {
				t.Fatalf("topN=%d: Search 返回 %d 条，参考 %d 条", topN, len(got), len(ref))
			}
			for i := range got {
				if got[i].ID != ref[i].ID {
					t.Fatalf("topN=%d 第 %d 名不一致: Search=%s(%.12f) 参考=%s(%.12f)",
						topN, i, got[i].ID, got[i].Score.Float(), ref[i].ID, ref[i].Score.Float())
				}
				if d := math.Abs(got[i].Score.Float() - ref[i].Score.Float()); d > worstScoreDiff {
					worstScoreDiff = d
				}
			}
		}
	}
	t.Logf("Search 与暴力 float 点积: 30 次查询 x 5 种 topN 完全一致，最大分数误差 = %.3e", worstScoreDiff)
	if worstScoreDiff >= 1e-6 {
		t.Fatalf("分数误差 %.3e 超过 1e-6", worstScoreDiff)
	}

	// each item must appear exactly once and the score must be its best sample
	query := dsp.Normalize(randomVec(p.Dim(), rnd))
	all := ix.Search(query, 0)
	if len(all) != len(counts) {
		t.Fatalf("--all 返回 %d 个条目，期望 %d", len(all), len(counts))
	}
	seen := map[string]bool{}
	for i, s := range all {
		if seen[s.ID] {
			t.Fatalf("条目 %s 在结果里出现多次", s.ID)
		}
		seen[s.ID] = true
		if i > 0 && s.Score.Float() > all[i-1].Score.Float() {
			t.Fatalf("结果没有按分数降序: [%d]=%.6f > [%d]=%.6f", i, s.Score.Float(), i-1, all[i-1].Score.Float())
		}
	}

	// degenerate inputs
	if got := ix.Search(make([]float32, 10), 3); got != nil {
		t.Fatalf("维数不对的查询应返回 nil，得到 %v", got)
	}
	if got := ix.Search(query, 0); len(got) == 0 {
		t.Fatal("topN=0 应表示全部")
	}
	empty := &Index{params: p, dim: p.Dim()}
	if got := empty.Search(query, 3); got != nil {
		t.Fatalf("空索引应返回 nil，得到 %v", got)
	}
}

// ---------------------------------------------------------------------------
// B9 - round trip and corruption
// ---------------------------------------------------------------------------

func TestSaveLoadRoundTrip(t *testing.T) {
	p := dsp.DefaultParams()
	counts := []int{4, 1, 2}
	ix := testIndex(p, counts, 5)

	dir := t.TempDir()
	path := filepath.Join(dir, "index.bin")
	if err := ix.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.Dim() != ix.Dim() || back.Fingerprint() != ix.Fingerprint() {
		t.Fatalf("往返后 维数/指纹 不一致: %d/%s vs %d/%s",
			back.Dim(), back.Fingerprint(), ix.Dim(), ix.Fingerprint())
	}
	i1, s1, b1 := ix.Stats()
	i2, s2, b2 := back.Stats()
	if i1 != i2 || s1 != s2 || b1 != b2 {
		t.Fatalf("往返后 Stats 不一致: (%d,%d,%d) vs (%d,%d,%d)", i1, s1, b1, i2, s2, b2)
	}
	if back.items[0].id != ix.items[0].id || back.items[0].name != ix.items[0].name {
		t.Fatalf("往返后条目信息不一致: %+v vs %+v", back.items[0], ix.items[0])
	}
	if back.vecs[0].sampleFile != ix.vecs[0].sampleFile || back.vecs[0].sampleIndex != ix.vecs[0].sampleIndex {
		t.Fatalf("往返后样本出处不一致: %+v vs %+v", back.vecs[0], ix.vecs[0])
	}
	if back.vecs[0].scale != ix.vecs[0].scale {
		t.Fatalf("往返后量化 scale 不一致: %v vs %v", back.vecs[0].scale, ix.vecs[0].scale)
	}

	// identical results from the reloaded index
	rnd := newRand(3)
	for trial := 0; trial < 5; trial++ {
		query := dsp.Normalize(randomVec(p.Dim(), rnd))
		a := ix.Search(query, 3)
		b := back.Search(query, 3)
		for i := range a {
			if a[i].ID != b[i].ID || a[i].Score.Float() != b[i].Score.Float() {
				t.Fatalf("往返后 Search 结果不一致: 第 %d 名 %s/%.12f vs %s/%.12f",
					i, a[i].ID, a[i].Score.Float(), b[i].ID, b[i].Score.Float())
			}
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("index.bin: %d 字节 (%d 条目, %d 模板, 维数 %d); 编码长度可解码校验 = %v",
		len(raw), i1, s1, p.Dim(), len(raw) > 0)
	// layout sanity: magic + version at fixed offsets
	if string(raw[:4]) != Magic {
		t.Fatalf("magic = %q，期望 %q", raw[:4], Magic)
	}
	if v := binary.LittleEndian.Uint32(raw[4:8]); v != FormatVersion {
		t.Fatalf("版本 = %d，期望 %d", v, FormatVersion)
	}
}

func TestLoadRejectsCorruption(t *testing.T) {
	p := dsp.DefaultParams()
	ix := testIndex(p, []int{2, 2}, 9)
	dir := t.TempDir()
	good := filepath.Join(dir, "good.bin")
	if err := ix.Save(good); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		raw  []byte
	}{
		{"空文件", nil},
		{"magic 被改", append(append([]byte(nil), "XXXX"[:]...), raw[4:]...)},
		{"截断一半", raw[:len(raw)/2]},
		{"截断到向量块中间", raw[:len(raw)-8]},
		{"尾部多出 8 字节垃圾", append(append([]byte(nil), raw...), 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22)},
		{"版本号改成 99", versionPatched(raw, 99)},
	}
	for _, tc := range cases {
		path := filepath.Join(dir, "bad.bin")
		if err := os.WriteFile(path, tc.raw, 0o644); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s: Load panic 了: %v", tc.name, r)
				}
			}()
			got, err := Load(path)
			if err == nil {
				t.Fatalf("%s: Load 应当报错，却返回了 %+v", tc.name, got)
			}
			t.Logf("%s -> 错误信息: %v", tc.name, err)
		}()
	}

	// Decode must also reject a nil/empty buffer without panicking.
	if _, err := Decode(nil); err == nil {
		t.Fatal("Decode(nil) 应当报错")
	}
}

func TestLoadRejectsFingerprintMismatch(t *testing.T) {
	p := dsp.DefaultParams()
	dir := t.TempDir()

	// A proper library with one short sample, so LoadOrBuild can actually run.
	libPath := writeTestLibrary(t, dir)

	// Build and store an index with DIFFERENT parameters.
	other := p
	other.HopSize = 128
	otherIx, err := BuildWithParams(libPath, other)
	if err != nil {
		t.Fatalf("BuildWithParams(other): %v", err)
	}
	path := filepath.Join(dir, "index.bin")
	if err := otherIx.Save(path); err != nil {
		t.Fatalf("Save(other): %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("不同参数写出的索引本身应当可读: %v", err)
	}
	if loaded.Fingerprint() == p.Fingerprint() {
		t.Fatal("改了 HopSize 之后指纹不应相同")
	}
	if loaded.Params().HopSize != 128 {
		t.Fatalf("往返后 HopSize = %d，期望 128", loaded.Params().HopSize)
	}

	// The CLI guard: LoadOrBuild must notice the mismatch, rebuild with the
	// current parameters and say why.
	ix, rebuilt, reason, err := LoadOrBuild(libPath, path, p)
	if err != nil {
		t.Fatalf("LoadOrBuild: %v", err)
	}
	if !rebuilt {
		t.Fatal("指纹不匹配时应当自动重建")
	}
	if reason == "" {
		t.Fatal("自动重建应当给出原因")
	}
	if ix.Fingerprint() != p.Fingerprint() {
		t.Fatalf("重建后的指纹 %s != %s", ix.Fingerprint(), p.Fingerprint())
	}
	t.Logf("指纹不匹配 -> 自动重建，原因: %s", reason)

	// ... and a matching file must be reused, not rebuilt.
	again, rebuilt2, reason2, err := LoadOrBuild(libPath, path, p)
	if err != nil {
		t.Fatalf("LoadOrBuild(second): %v", err)
	}
	if rebuilt2 {
		t.Fatalf("指纹匹配时不应重建（原因 %q）", reason2)
	}
	if again.Fingerprint() != p.Fingerprint() {
		t.Fatal("复用后的指纹不对")
	}

	// Missing file -> build.
	missing := filepath.Join(dir, "nope", "index.bin")
	ix3, rebuilt3, reason3, err := LoadOrBuild(libPath, missing, p)
	if err != nil {
		t.Fatalf("LoadOrBuild(missing): %v", err)
	}
	if !rebuilt3 || ix3 == nil {
		t.Fatalf("索引不存在时应当重建（rebuilt=%v）", rebuilt3)
	}
	if _, err := os.Stat(missing); err != nil {
		t.Fatalf("应当把新建的索引写回 %s: %v", missing, err)
	}
	t.Logf("索引不存在 -> 自动重建，原因: %s", reason3)

	// Corrupt file -> rebuild instead of failing.
	if err := os.WriteFile(path, []byte("garbage-not-an-index"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, rebuilt4, reason4, err := LoadOrBuild(libPath, path, p)
	if err != nil {
		t.Fatalf("索引损坏时应重建而不是报错: %v", err)
	}
	if !rebuilt4 {
		t.Fatal("索引损坏时应当重建")
	}
	t.Logf("索引损坏 -> 自动重建，原因: %s", reason4)
}

// TestLoadOrBuildRebuildsWhenLibraryIsNewer is the "入库之后再播放没有分" case:
// the feature fingerprint still matches, but the library was saved after the
// index, so the old index must not be reused.
func TestLoadOrBuildRebuildsWhenLibraryIsNewer(t *testing.T) {
	p := dsp.DefaultParams()
	dir := t.TempDir()
	libPath := writeTestLibrary(t, dir)
	idxPath := filepath.Join(dir, "index.bin")

	first, rebuilt, _, err := LoadOrBuild(libPath, idxPath, p)
	if err != nil {
		t.Fatalf("首次 LoadOrBuild: %v", err)
	}
	if !rebuilt || first.Empty() {
		t.Fatalf("首次应当建出非空索引（rebuilt=%v empty=%v）", rebuilt, first.Empty())
	}

	again, rebuilt2, reason2, err := LoadOrBuild(libPath, idxPath, p)
	if err != nil {
		t.Fatalf("第二次 LoadOrBuild: %v", err)
	}
	if rebuilt2 {
		t.Fatalf("库没有更新时不应重建（原因 %q）", reason2)
	}
	if again.Fingerprint() != first.Fingerprint() {
		t.Fatal("复用后的指纹变了")
	}

	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(libPath, future, future); err != nil {
		t.Fatal(err)
	}
	third, rebuilt3, reason3, err := LoadOrBuild(libPath, idxPath, p)
	if err != nil {
		t.Fatalf("库更新后 LoadOrBuild: %v", err)
	}
	if !rebuilt3 {
		t.Fatal("音效库比索引新时应当自动重建")
	}
	if reason3 == "" || !strings.Contains(reason3, "音效库") {
		t.Fatalf("重建原因应当说明库已更新，得到 %q", reason3)
	}
	if third.Empty() {
		t.Fatal("重建后的索引是空的")
	}
	t.Logf("库比索引新 -> 自动重建，原因: %s", reason3)
}

// ---------------------------------------------------------------------------
// B10 - performance
// ---------------------------------------------------------------------------

// TestSearchPerformance builds 200 items x 8 samples (1600 templates of 2048
// int8 values = 3.3 MB) and measures a single Search call.
func TestSearchPerformance(t *testing.T) {
	p := dsp.DefaultParams()
	counts := make([]int, 200)
	for i := range counts {
		counts[i] = 8
	}
	buildStart := time.Now()
	ix := testIndex(p, counts, 12345)
	buildDur := time.Since(buildStart)

	items, samples, bytes := ix.Stats()
	if items != 200 || samples != 1600 {
		t.Fatalf("索引规模 = %d 条目 / %d 模板，期望 200 / 1600", items, samples)
	}
	if bytes != 1600*2048 {
		t.Fatalf("量化字节数 = %d，期望 %d", bytes, 1600*2048)
	}
	t.Logf("索引规模: %d 条目 x 8 样本 = %d 模板 x %d 维 = %d 字节 (%.1f MiB), 构建耗时 %s",
		items, samples, p.Dim(), bytes, float64(bytes)/(1<<20), buildDur.Round(time.Millisecond))

	rnd := newRand(77)
	query := dsp.Normalize(randomVec(p.Dim(), rnd))

	const rounds = 50
	// warm-up
	for i := 0; i < 5; i++ {
		ix.Search(query, 5)
	}
	start := time.Now()
	for i := 0; i < rounds; i++ {
		ix.Search(query, 5)
	}
	perCall := time.Since(start) / rounds
	t.Logf("Search 单次耗时: %v（%d 次平均，含排序与 topN 截断；目标 < 3 ms）", perCall.Round(time.Microsecond), rounds)
	if perCall >= 3*time.Millisecond {
		t.Fatalf("Search 单次耗时 %v 超过 3 ms 目标", perCall)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func versionPatched(raw []byte, v uint32) []byte {
	b := append([]byte(nil), raw...)
	binary.LittleEndian.PutUint32(b[4:8], v)
	return b
}

// TestAnchorFollowsShortBurst is the 3-second recall case: the real sound is
// a few tens of milliseconds inside a long quiet buffer. The stored patch has
// to sit on that burst, and two different bursts must not collapse into the
// same room-tone vector.
func TestAnchorFollowsShortBurst(t *testing.T) {
	p := dsp.DefaultParams()
	n := 3 * p.SampleRate
	makeClip := func(freq float64, seed uint64) []float32 {
		pcm := make([]float32, n)
		rnd := newRand(seed)
		noise := float32(math.Pow(10, -65.0/20))
		for i := range pcm {
			pcm[i] = noise * float32(rnd.next()*2-1)
		}
		amp := float32(math.Pow(10, -18.0/20))
		at := int(1.5 * float64(p.SampleRate))
		dur := p.SampleRate / 25
		for i := 0; i < dur; i++ {
			pcm[at+i] += amp * float32(math.Sin(2*math.Pi*freq*float64(i)/float64(p.SampleRate)))
		}
		return pcm
	}
	patch, end, err := AnchorPatch(p, makeClip(880, 1))
	if err != nil {
		t.Fatal(err)
	}
	center := end - p.WindowFrames/2
	centerS := float64(center*p.HopSize) / float64(p.SampleRate)
	if math.Abs(centerS-1.5) > 0.12 {
		t.Fatalf("锚点在 %.2f s，应该靠近 1.50 s 的短促声", centerS)
	}
	other, _, err := AnchorPatch(p, makeClip(3000, 1))
	if err != nil {
		t.Fatal(err)
	}
	var dot float64
	for i := range patch {
		dot += float64(patch[i]) * float64(other[i])
	}
	t.Logf("短促声锚点 %.2f s，880 Hz 与 3000 Hz 余弦 %.3f", centerS, dot)
	if dot > 0.75 {
		t.Fatalf("两段不同的短促声余弦 %.3f，安静背景把向量拉成一样了", dot)
	}
}

func randomVec(dim int, r *rand64) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(-60 + 60*r.next())
	}
	return v
}

// rand64 is a deterministic PRNG so the reported numbers do not depend on the
// Go version's math/rand stream.
type rand64 struct{ s uint64 }

func newRand(seed uint64) *rand64 { return &rand64{s: seed*6364136223846793005 + 1442695040888963407} }

func (r *rand64) next() float64 {
	r.s = r.s*6364136223846793005 + 1442695040888963407
	return float64(r.s>>11) / float64(1<<53)
}
