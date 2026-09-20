package live

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/index"
)

// Hit is the JSON-friendly form of one search result. index.ItemScore carries
// an unexported template index (so encoding it directly would emit
// {"Value":0.93}), which is why the API/CLI convert through this type.
type Hit struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Score float64 `json:"score"`
	// TemplateIndex is the stored sample variant that produced the score
	// (-1 when unknown).
	TemplateIndex int `json:"templateIndex"`
	// AtMs is when the hit occurred (offline identification only).
	AtMs float64 `json:"atMs,omitempty"`
	// Sample is the container path of the winning variant (offline only).
	Sample      string `json:"sample,omitempty"`
	SampleIndex int    `json:"sampleIndex,omitempty"`
}

// HitsFrom converts a score list into DTOs, preserving order.
func HitsFrom(top []index.ItemScore) []Hit {
	out := make([]Hit, 0, len(top))
	for _, s := range top {
		out = append(out, Hit{
			ID:            s.ID,
			Name:          s.Name,
			Score:         s.Score.Float(),
			TemplateIndex: s.Score.TemplateIndex(),
		})
	}
	return out
}

// IdentifyResult is the outcome of an offline identification.
type IdentifyResult struct {
	Frames    int     `json:"frames"`
	Windows   int     `json:"windows"`
	ElapsedMs float64 `json:"elapsedMs"`
	Top       []Hit   `json:"top"`
}

// Identify slides a feature window over pcm and returns each item's best score.
//
// It is the offline counterpart of the realtime link (the same analyzer, the
// same index, the same window) and backs POST /api/match ("这段录音是什么").
// pcm must be 48 kHz mono float32. topN <= 0 returns every item.
func Identify(ix *index.Index, pcm []float32, topN int) (*IdentifyResult, error) {
	if ix == nil {
		return nil, errors.New("live: 没有可用的指纹索引")
	}
	if ix.Empty() {
		return nil, errors.New("live: 索引里一个模板都没有（先往库里加音效样本）")
	}
	p := ix.Params()
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("live: 索引特征参数无效: %w", err)
	}
	start := time.Now()
	an, err := dsp.NewAnalyzer(p)
	if err != nil {
		return nil, fmt.Errorf("live: 初始化分析器失败: %w", err)
	}
	an.Push(pcm)
	frames := an.FrameCount()
	if frames < p.WindowFrames {
		return nil, fmt.Errorf("live: 录音太短，只有 %d 帧（需要至少 %d 帧 = %.3f 秒）",
			frames, p.WindowFrames, p.PatchDurationS())
	}

	type best struct {
		score   float64
		atFrame int
		sample  string
		sampleN int
		name    string
	}
	byID := make(map[string]*best, 16)
	windows := 0
	for end := p.WindowFrames - 1; end < frames; end++ {
		w := an.WindowAt(end)
		if w == nil {
			continue
		}
		windows++
		for _, sc := range ix.Search(w, 0) {
			score := sc.Score.Float()
			h, ok := byID[sc.ID]
			if !ok {
				h = &best{score: -2}
				byID[sc.ID] = h
			}
			if score > h.score {
				h.score, h.atFrame, h.name = score, end, sc.Name
				h.sample, h.sampleN = ix.ItemAt(sc)
			}
		}
	}

	top := make([]Hit, 0, len(byID))
	for id, h := range byID {
		top = append(top, Hit{
			ID:            id,
			Name:          h.name,
			Score:         h.score,
			TemplateIndex: -1,
			AtMs:          float64(h.atFrame) * p.HopDurationS() * 1000,
			Sample:        h.sample,
			SampleIndex:   h.sampleN,
		})
	}
	sort.SliceStable(top, func(a, b int) bool {
		if top[a].Score != top[b].Score {
			return top[a].Score > top[b].Score
		}
		return top[a].Name < top[b].Name
	})
	if topN > 0 && len(top) > topN {
		top = top[:topN]
	}
	return &IdentifyResult{
		Frames:    frames,
		Windows:   windows,
		ElapsedMs: float64(time.Since(start).Microseconds()) / 1000,
		Top:       top,
	}, nil
}
