// Command p3lib builds the small sound-effect library used by the P3 acceptance
// run (verify_p3.ps1). It is a development tool, not part of the product: it
// generates deterministic tones so the overlay screenshots always match a known
// icon colour, and it exercises the same library API the management UI uses.
//
//	go run ./cmd/p3lib -out artifacts\p3\cli_library.srz
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"

	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/match"
	"github.com/znz/soundradar/internal/wav"
)

type item struct {
	name  string
	hz    float64
	color color.NRGBA
}

func main() {
	out := flag.String("out", filepath.Join("artifacts", "p3", "cli_library.srz"), "output library path")
	flag.Parse()

	// The 880 Hz entry is the one verify_p3.ps1 recognises end to end; the others
	// give --demo something to cycle through and prove the icons differ.
	items := []item{
		{"880Hz 测试音", 880, color.NRGBA{R: 30, G: 144, B: 255, A: 255}},
		{"脚步-近-隔墙-耳机-01", 1200, color.NRGBA{R: 255, G: 64, B: 96, A: 255}},
		{"枪声-远-无耳机", 1500, color.NRGBA{R: 255, G: 196, B: 0, A: 255}},
		{"脚步-隔墙-音箱", 3000, color.NRGBA{R: 0, G: 200, B: 120, A: 255}},
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fatal(err)
	}
	store := library.New(*out, "P3 验收库")
	for _, it := range items {
		pcm := tone(48000, it.hz, 0.6) // 1.0 s
		i16 := make([]int16, len(pcm))
		wav.Float32ToInt16(pcm, i16)
		raw, err := wavBytes(48000, 1, i16)
		if err != nil {
			fatal(err)
		}
		icon, err := solidPNG(it.color)
		if err != nil {
			fatal(err)
		}
		li := &library.Item{
			Name:       it.name,
			Threshold:  match.DefaultOptions().DefaultThreshold,
			CooldownMs: 200,
			Tags:       []string{"p3"},
			Note:       fmt.Sprintf("%.0f Hz 正弦，P3 验收素材", it.hz),
			Samples:    []library.Sample{{File: library.SampleFile(0), Source: "generated"}},
		}
		if err := store.AddItem(li, icon, [][]byte{raw}); err != nil {
			fatal(err)
		}
		fmt.Printf("added %-24s %s  %.0f Hz  icon=%v\n", li.Name, li.ID, it.hz, it.color)
	}
	if err := store.Save(); err != nil {
		fatal(err)
	}
	abs, _ := filepath.Abs(*out)
	fmt.Printf("library: %s (%d items)\n", abs, store.Len())
}

func tone(rate int, hz, amp float64) []float32 {
	out := make([]float32, rate) // exactly 1 s
	for i := range out {
		out[i] = float32(amp * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
	}
	return out
}

func wavBytes(rate, channels int, pcm []int16) ([]byte, error) {
	dir, err := os.MkdirTemp("", "p3lib-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.wav")
	if err := wav.WriteInt16File(path, rate, channels, pcm); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// solidPNG builds the 128x128 icon P1 would have normalised an upload to, with a
// contrasting ring so the scaled icon keeps its centre colour.
func solidPNG(c color.NRGBA) ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	// A darker border keeps the icon readable on a dark game scene.
	border := color.NRGBA{R: uint8(int(c.R) / 3), G: uint8(int(c.G) / 3), B: uint8(int(c.B) / 3), A: 255}
	for i := 0; i < 8; i++ {
		draw.Draw(img, image.Rect(i, i, 128-i, i+1), image.NewUniform(border), image.Point{}, draw.Src)
		draw.Draw(img, image.Rect(i, 127-i, 128-i, 128-i), image.NewUniform(border), image.Point{}, draw.Src)
		draw.Draw(img, image.Rect(i, i, i+1, 128-i), image.NewUniform(border), image.Point{}, draw.Src)
		draw.Draw(img, image.Rect(127-i, i, 128-i, 128-i), image.NewUniform(border), image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "p3lib:", err)
	os.Exit(1)
}
