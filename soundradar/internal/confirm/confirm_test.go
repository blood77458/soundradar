package confirm

import (
	"math"
	"testing"

	"github.com/znz/soundradar/internal/dsp"
)

func TestEmbedSelfSimilarity(t *testing.T) {
	p := dsp.DefaultParams()
	need := NeedSamples(p)
	pcm := make([]float32, need)
	for i := range pcm {
		// Short burst in the middle so the peak patch is well defined.
		if i > need/3 && i < need/3+p.FrameSize {
			pcm[i] = float32(0.4 * math.Sin(2*math.Pi*900*float64(i)/48000))
		}
	}
	a, err := Embed(p, pcm)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Embed(p, pcm)
	if err != nil {
		t.Fatal(err)
	}
	if c := Cosine(a, b); c < 0.999 {
		t.Fatalf("clean self cosine = %v", c)
	}
	variants, err := EmbedVariants(p, pcm)
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) < 1 {
		t.Fatalf("expected noise variants, got %d", len(variants))
	}
}

func TestMelEmbedMatchesSelf(t *testing.T) {
	p := dsp.DefaultParams()
	need := NeedSamples(p)
	pcm := make([]float32, need+4*p.HopSize)
	for i := need / 4; i < need/4+p.FrameSize; i++ {
		pcm[i] = float32(0.45 * math.Sin(2*math.Pi*1000*float64(i)/48000))
	}
	frames := p.Frames(pcm)
	if len(frames) < p.WindowFrames {
		t.Fatal("not enough frames")
	}
	start := 0
	patch := make([]float32, 0, p.Dim())
	for f := start; f < start+p.WindowFrames; f++ {
		patch = append(patch, frames[f]...)
	}
	patch = dsp.Normalize(patch)
	a := MelEmbed(patch)
	b := MelEmbed(patch)
	if c := Cosine(a, b); c < 0.999 {
		t.Fatalf("MelEmbed self = %v", c)
	}
}

func TestOnsetOK(t *testing.T) {
	quiet := make([]float64, 40)
	for i := range quiet {
		quiet[i] = 1e-6
	}
	if OnsetOK(quiet, 3) {
		t.Fatal("flat energy should not pass onset")
	}
	burst := append([]float64{}, quiet...)
	for i := 34; i < 40; i++ {
		burst[i] = 1e-3
	}
	if !OnsetOK(burst, 3) {
		t.Fatal("burst should pass onset")
	}
	if !OnsetOK(burst, 0) {
		t.Fatal("onsetDB 0 always passes")
	}
	// Click already left the newest frames, but still sits in the later part
	// of the recent window that mel uses when it fires a hop late.
	late := append([]float64{}, quiet...)
	for i := 24; i < 32; i++ {
		late[i] = 1e-3
	}
	if !OnsetOK(late, 3) {
		t.Fatal("burst in later part of recent window should pass onset")
	}
	// Long capture: thousands of quiet frames must not dilute the bed so a
	// real late click fails the rise check.
	long := make([]float64, 5000)
	for i := range long {
		long[i] = 1e-6
	}
	for i := 4960; i < 4985; i++ {
		long[i] = 1e-3
	}
	if !OnsetOK(long, 3) {
		t.Fatal("click after a long quiet session should still pass onset")
	}
}
