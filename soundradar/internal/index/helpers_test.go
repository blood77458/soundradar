package index

import (
	"math"

	"github.com/znz/soundradar/internal/dsp"
)

// testIndex builds an in-memory index of random templates: one item per entry
// in counts, each with counts[i] samples of dim random values. It is only used
// by tests (Search/Encode/Decode do not care where a template came from).
func testIndex(p dsp.Params, counts []int, seed uint64) *Index {
	ix := &Index{params: p, fp: p.Fingerprint(), dim: p.Dim()}
	rnd := uint64(seed*6364136223846793005 + 1442695040888963407)
	next := func() float64 {
		rnd = rnd*6364136223846793005 + 1442695040888963407
		return float64(rnd>>11) / float64(1<<53)
	}
	for i, n := range counts {
		id := idFor(i)
		ix.items = append(ix.items, item{
			id: id, name: nameFor(i), first: len(ix.vecs),
			threshold: 0.75, cooldownMs: 400,
		})
		for k := 0; k < n; k++ {
			v := make([]float32, p.Dim())
			for d := range v {
				// log-mel values live in roughly [-100, 0]
				v[d] = float32(-100 + 100*next())
			}
			v = dsp.Normalize(v)
			q, scale := Quantize(v)
			ix.vecs = append(ix.vecs, vec{
				q:           q,
				scale:       scale,
				item:        i,
				sampleFile:  sampleNameFor(k),
				sampleIndex: k,
			})
		}
		ix.items[i].samples = n
	}
	return ix
}

func idFor(i int) string {
	const hexd = "0123456789abcdef"
	b := []byte{hexd[(i>>12)&15], hexd[(i>>8)&15], hexd[(i>>4)&15], hexd[i&15], hexd[(i*7>>4)&15], hexd[(i*13)&15]}
	return string(b)
}

func nameFor(i int) string { return "音效-" + idFor(i) }

func sampleNameFor(k int) string {
	return "samples/000" + string(rune('1'+k%9)) + ".wav"
}

// bruteSearch is the straightforward float reference implementation used to
// validate Index.Search: it dequantises every template and does a plain dot
// product, keeping the maximum per item, then sorts.
func bruteSearch(ix *Index, query []float32, topN int) []ItemScore {
	scores := make([]float64, len(ix.items))
	best := make([]int, len(ix.items))
	for i := range scores {
		scores[i] = math.Inf(-1)
		best[i] = -1
	}
	for ti := range ix.vecs {
		v := ix.vecs[ti]
		f := Dequantize(v.q, v.scale)
		var dot float64
		for d := range f {
			dot += float64(f[d]) * float64(query[d])
		}
		if dot > scores[v.item] {
			scores[v.item] = dot
			best[v.item] = ti
		}
	}
	type sc struct {
		i int
		s float64
	}
	all := make([]sc, 0, len(ix.items))
	for i := range ix.items {
		if best[i] >= 0 {
			all = append(all, sc{i, scores[i]})
		}
	}
	// insertion sort keeps the reference free of assumptions about the
	// production sort's stability
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].s > all[j-1].s; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	if topN > 0 && len(all) > topN {
		all = all[:topN]
	}
	out := make([]ItemScore, 0, len(all))
	for _, s := range all {
		out = append(out, ItemScore{
			ID:    ix.items[s.i].id,
			Name:  ix.items[s.i].name,
			Score: ItemScoreValue{Value: s.s, template: best[s.i]},
		})
	}
	return out
}
