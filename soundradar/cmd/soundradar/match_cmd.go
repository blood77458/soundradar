package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/znz/soundradar/internal/audio"
	"github.com/znz/soundradar/internal/dsp"
	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/match"
	"github.com/znz/soundradar/internal/wav"
)

// runMatch implements `soundradar match --wav FILE`.
//
// It is the "which sound effect is this recording?" tool: the whole WAV is
// converted to 48 kHz mono, turned into log-mel frames once, and then EVERY
// window position is scored against the quantised index. Each item keeps its
// global best score over all window positions, so a sound effect that starts
// somewhere in the middle of the recording is still found.
const matchUsage = `soundradar match - 拿一段录音去认音效

用法:
  soundradar match --wav <file.wav|file.mp3> [选项]

选项:
  --wav PATH      待识别的录音（必填，支持 wav / mp3）
  --library PATH  音效库文件（默认 <exe 目录>\data\library.srz）
  --index PATH    指纹索引文件（默认与库文件同目录的 index.bin）
  --top N         显示前 N 个条目（默认 5；--all 显示全部）
  --all           显示库里所有条目
  --json          以 JSON 输出（便于脚本/后续实时链路复用）
  --min-score F   低于该分数时不给出结论（默认 0.75）
  --quiet         只打印结论

说明:
  会在"所有可能的窗口位置"上滑窗匹配（窗长 186.7 ms，步进 5.333 ms），
  每个条目取其最高分。索引不存在或特征参数变了会自动重建并提示。`

// matchResultJSON is the machine readable form of a match run.
type matchResultJSON struct {
	Input struct {
		Path       string  `json:"path"`
		SampleRate int     `json:"sampleRate"`
		Channels   int     `json:"channels"`
		Frames     int     `json:"frames"`
		Seconds    float64 `json:"seconds"`
		RMSDBFS    float64 `json:"rmsDbfs"`
		PeakDBFS   float64 `json:"peakDbfs"`
		Resampled  bool    `json:"resampled"`
		Downmixed  bool    `json:"downmixed"`
	} `json:"input"`
	Index struct {
		Path        string `json:"path"`
		Rebuilt     bool   `json:"rebuilt"`
		RebuildWhy  string `json:"rebuildWhy,omitempty"`
		Fingerprint string `json:"fingerprint"`
		Algorithm   string `json:"algorithm"`
		Dim         int    `json:"dim"`
		Items       int    `json:"items"`
		Samples     int    `json:"samples"`
		Bytes       int    `json:"samplesBytes"`
	} `json:"index"`
	Search struct {
		Frames    int     `json:"frames"`
		Windows   int     `json:"windows"`
		WindowMs  float64 `json:"windowMs"`
		HopMs     float64 `json:"hopMs"`
		ElapsedMs float64 `json:"elapsedMs"`
		MinScore  float64 `json:"minScore"`
	} `json:"search"`
	Top     []matchHitJSON `json:"top"`
	Best    *matchHitJSON  `json:"best"`
	Verdict string         `json:"verdict"`
}

type matchHitJSON struct {
	Rank    int     `json:"rank"`
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Score   float64 `json:"score"`
	AtMs    float64 `json:"atMs"`
	Sample  string  `json:"sample"`
	SampleN int     `json:"sampleIndex"`
}

func runMatch(args []string) error {
	fs := flag.NewFlagSet("match", flag.ContinueOnError)
	wavPath := fs.String("wav", "", "recording to identify (wav or mp3)")
	libPath := fs.String("library", "", "library file (default <exe dir>/data/library.srz)")
	idxPath := fs.String("index", "", "index file (default <library dir>/index.bin)")
	topN := fs.Int("top", 5, "how many items to list")
	all := fs.Bool("all", false, "list every item")
	asJSON := fs.Bool("json", false, "machine readable output")
	minScore := fs.Float64("min-score", match.DefaultOptions().DefaultThreshold, "score below which no verdict is given")
	quiet := fs.Bool("quiet", false, "print only the verdict")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*wavPath) == "" {
		return fmt.Errorf("--wav 是必填项\n\n%s", matchUsage)
	}

	lib, err := resolveLibraryPath(*libPath)
	if err != nil {
		return err
	}
	idx := strings.TrimSpace(*idxPath)
	if idx == "" {
		idx = index.DefaultPathFor(lib)
	}
	idx, err = filepath.Abs(idx)
	if err != nil {
		return fmt.Errorf("索引路径无效: %w", err)
	}

	params := dsp.DefaultParams()
	ix, rebuilt, why, err := index.LoadOrBuild(lib, idx, params)
	if err != nil {
		return fmt.Errorf("准备索引失败: %w", err)
	}
	if ix.Empty() {
		return fmt.Errorf("索引里一个模板都没有（库文件 %s 没有可用音频样本），先往库里加音效", lib)
	}

	// ---- decode the recording (reuse internal/audio: wav + mp3, 48k mono) ----
	raw, err := os.ReadFile(*wavPath)
	if err != nil {
		return fmt.Errorf("读取录音失败: %w", err)
	}
	conv, err := audio.Convert(raw, filepath.Base(*wavPath))
	if err != nil {
		return err
	}
	pcm := make([]float32, len(conv.Mono48k))
	for i, v := range conv.Mono48k {
		pcm[i] = float32(v)
	}
	level := dsp.LevelDBFSOf(pcm)

	if !*asJSON && !*quiet {
		printMatchHeader(*wavPath, conv, pcm, level, ix, idx, lib, rebuilt, why, params)
	}

	// ---- analyse once, then slide over every window position ----
	an, err := dsp.NewAnalyzer(params)
	if err != nil {
		return fmt.Errorf("初始化分析器失败: %w", err)
	}
	start := time.Now()
	an.Push(pcm)
	frames := an.FrameCount()
	if frames < params.WindowFrames {
		return fmt.Errorf("录音太短：只有 %d 帧（需要至少 %d 帧 = %.3f 秒）",
			frames, params.WindowFrames, float64(params.FrameSize+(params.WindowFrames-1)*params.HopSize)/float64(params.SampleRate))
	}

	type hit struct {
		score   float64
		atFrame int
		sample  string
		sampleN int
		name    string
	}
	best := make(map[string]*hit, len(ix.Items()))
	windows := 0
	for end := params.WindowFrames - 1; end < frames; end++ {
		window := an.WindowAt(end)
		if window == nil {
			continue
		}
		windows++
		for _, sc := range ix.Search(window, 0) {
			score := sc.Score.Float()
			h, ok := best[sc.ID]
			if !ok {
				h = &hit{score: math.Inf(-1)}
				best[sc.ID] = h
			}
			if score > h.score {
				h.score = score
				h.atFrame = end
				h.name = sc.Name
				h.sample, h.sampleN = ix.ItemAt(sc)
			}
		}
	}
	elapsed := time.Since(start)

	hits := make([]matchHitJSON, 0, len(best))
	for id, h := range best {
		hits = append(hits, matchHitJSON{
			ID:      id,
			Name:    h.name,
			Score:   h.score,
			AtMs:    float64(h.atFrame) * params.HopDurationS() * 1000,
			Sample:  h.sample,
			SampleN: h.sampleN,
		})
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].Score != hits[b].Score {
			return hits[a].Score > hits[b].Score
		}
		return hits[a].Name < hits[b].Name
	})
	limit := len(hits)
	if !*all && *topN > 0 && *topN < limit {
		limit = *topN
	}
	shown := hits[:limit]

	// ---- verdict ---------------------------------------------------------
	verdict := "没有匹配到任何已注册音效（最高分 %.4f 低于门槛 %.2f）"
	var bestHit *matchHitJSON
	if len(hits) > 0 && hits[0].Score >= *minScore {
		bestHit = &hits[0]
		verdict = fmt.Sprintf("最像的是「%s」，相似度 %.4f（出现在 %.2f 秒处）", hits[0].Name, hits[0].Score, hits[0].AtMs/1000)
	} else if len(hits) > 0 {
		verdict = fmt.Sprintf("没有可靠命中：最高分是「%s」%.4f，低于门槛 %.2f", hits[0].Name, hits[0].Score, *minScore)
	} else {
		verdict = fmt.Sprintf("没有匹配到任何已注册音效（门槛 %.2f）", *minScore)
	}

	if *asJSON {
		return printMatchJSON(*wavPath, conv, pcm, level, ix, idx, rebuilt, why, params,
			frames, windows, elapsed, *minScore, shown, hits, bestHit, verdict)
	}
	if !*quiet {
		printMatchTable(shown, hits, ix, params)
	}
	fmt.Printf("\n结论: %s\n", verdict)
	return nil
}

// printMatchHeader prints the human-readable banner. libPath is the library the
// caller resolved (used when the index came from disk and carries no build
// report).
func printMatchHeader(path string, conv *audio.Converted, pcm []float32, level float64,
	ix *index.Index, idxPath, libPath string, rebuilt bool, why string, p dsp.Params) {

	src := conv.Source
	fmt.Printf("[match] 录音       : %s\n", path)
	fmt.Printf("[match] 原始格式   : %s / %d Hz / %d 声道 / %s\n",
		strings.ToUpper(string(src.Container)), src.SampleRate, src.Channels, src.FormatTag)
	fmt.Printf("[match] 统一到     : 48000 Hz / 单声道 / %.3f 秒（%d 采样）%s%s\n",
		float64(len(pcm))/48000, len(pcm),
		resampleNote(src.SampleRate != 48000), downmixNote(src.Channels != 1))
	fmt.Printf("[match] 电平       : RMS %s dBFS", wav.FormatDBFS(level))
	peak := 0.0
	for _, v := range pcm {
		if a := math.Abs(float64(v)); a > peak {
			peak = a
		}
	}
	fmt.Printf("，峰值 %s dBFS\n", wav.FormatDBFS(wav.DBFS(peak)))

	items, samples, bytes := ix.Stats()
	lib := ix.LibraryPath()
	if lib == "" {
		// The index was loaded from disk, so the build report is not available;
		// the library path is still known from the command line.
		lib = libPath
	}
	fmt.Printf("[match] 音效库     : %s\n", lib)
	fmt.Printf("[match] 索引       : %s", idxPath)
	if rebuilt {
		fmt.Printf("（已自动重建：%s）", why)
	} else {
		fmt.Printf("（直接复用）")
	}
	fmt.Println()
	fmt.Printf("[match] 索引规模   : %d 个条目 / %d 个模板 / %d 维 / 量化 %d 字节 (%.1f KiB)\n",
		items, samples, ix.Dim(), bytes, float64(bytes)/1024)
	fmt.Printf("[match] 特征参数   : %s v%d，指纹 %s\n", dsp.Algorithm, dsp.Version, shortFP(ix.Fingerprint()))
	fmt.Printf("[match] 滑窗       : 窗长 %.1f ms（%d 帧），步进 %.3f ms（%d 采样）\n",
		p.PatchDurationS()*1000, p.WindowFrames, p.HopDurationS()*1000, p.HopSize)
}

func printMatchTable(shown []matchHitJSON, all []matchHitJSON, ix *index.Index, p dsp.Params) {
	fmt.Printf("\n[match] 排名（每个条目取其全部样本在所有窗口位置上的最高分）:\n")
	fmt.Printf("        %-4s %-6s %-26s %-16s %s\n", "名次", "分数", "音效名称", "出现位置", "命中的样本")
	for i, h := range shown {
		fmt.Printf("        %-4d %-6.4f %-26s %-16s %s\n",
			i+1, h.Score, trim(h.Name, 26), fmt.Sprintf("%.3f s", h.AtMs/1000), sampleLabel(h))
	}
	if len(shown) < len(all) {
		fmt.Printf("        …还有 %d 个条目（加 --all 显示全部）\n", len(all)-len(shown))
	}
}

func sampleLabel(h matchHitJSON) string {
	if h.Sample == "" {
		return "-"
	}
	return fmt.Sprintf("%s（第 %d 个样本）", h.Sample, h.SampleN+1)
}

func resampleNote(b bool) string {
	if b {
		return "，已重采样"
	}
	return ""
}

func downmixNote(b bool) string {
	if b {
		return "，已降混"
	}
	return ""
}

func shortFP(fp string) string {
	if len(fp) > 16 {
		return fp[:16] + "…"
	}
	return fp
}

func printMatchJSON(path string, conv *audio.Converted, pcm []float32, level float64,
	ix *index.Index, idxPath string, rebuilt bool, why string, p dsp.Params,
	frames, windows int, elapsed time.Duration, minScore float64,
	shown, all []matchHitJSON, bestHit *matchHitJSON, verdict string) error {

	var out matchResultJSON
	out.Input.Path = path
	out.Input.SampleRate = conv.Source.SampleRate
	out.Input.Channels = conv.Source.Channels
	out.Input.Frames = len(pcm)
	out.Input.Seconds = float64(len(pcm)) / 48000
	out.Input.RMSDBFS = level
	peak := 0.0
	for _, v := range pcm {
		if a := math.Abs(float64(v)); a > peak {
			peak = a
		}
	}
	out.Input.PeakDBFS = wav.DBFS(peak)
	out.Input.Resampled = conv.Source.SampleRate != 48000
	out.Input.Downmixed = conv.Source.Channels != 1

	items, samples, bytes := ix.Stats()
	out.Index.Path = idxPath
	out.Index.Rebuilt = rebuilt
	out.Index.RebuildWhy = why
	out.Index.Fingerprint = ix.Fingerprint()
	out.Index.Algorithm = dsp.Algorithm
	out.Index.Dim = ix.Dim()
	out.Index.Items = items
	out.Index.Samples = samples
	out.Index.Bytes = bytes

	out.Search.Frames = frames
	out.Search.Windows = windows
	out.Search.WindowMs = p.PatchDurationS() * 1000
	out.Search.HopMs = p.HopDurationS() * 1000
	out.Search.ElapsedMs = float64(elapsed.Microseconds()) / 1000
	out.Search.MinScore = minScore

	out.Top = shown
	out.Best = bestHit
	out.Verdict = verdict
	for i := range out.Top {
		out.Top[i].Rank = i + 1
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(&out)
}
