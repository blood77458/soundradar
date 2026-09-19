//go:build windows

package capture

import (
	"testing"
	"time"

	"github.com/znz/soundradar/internal/wav"
)

// TestP0EnumerationAndCapture is the executable-free form of the P0 regression
// check (`soundradar devices` / `soundradar capture --seconds 3`).
//
// The sandbox's Windows Smart App Control policy refuses to load freshly built
// unsigned executables (CodeIntegrity Event ID 3077/3033: "did not meet the
// Enterprise signing level requirements"), so the CLI cannot be launched here.
// This test drives exactly the same capture package entry points the CLI uses:
// capture.New() -> Enumerate() -> Capture() -> wav.WriteInt16File().
func TestP0EnumerationAndCapture(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	defer c.Close()

	devices, err := c.Enumerate()
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	t.Logf("active render endpoints: %d", len(devices))
	defaults := 0
	for _, d := range devices {
		tag := ""
		if d.Default {
			defaults++
			tag = " [DEFAULT]"
		}
		t.Logf("  [%d] %s%s", d.Index, d.Name, tag)
		t.Logf("      id: %s", d.ID)
	}
	if len(devices) == 0 {
		t.Skip("no active render endpoints on this machine")
	}
	if defaults != 1 {
		t.Errorf("expected exactly one default render endpoint, got %d", defaults)
	}

	// Same call the CLI makes for `capture --seconds 3`.
	const seconds = 3.0
	res, err := c.Capture("", time.Duration(seconds*float64(time.Second)))
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	t.Logf("device     : [%d] %s", res.Device.Index, res.Device.Name)
	t.Logf("mix format : %s", res.MixFormat)
	t.Logf("written    : %d Hz / %d ch / 16-bit PCM", res.SampleRate, res.Channels)
	t.Logf("frames     : %d", res.Frames)
	t.Logf("seconds    : %.3f s (audio timeline)", res.Duration.Seconds())
	t.Logf("peak       : %.6f full scale = %s dBFS", res.Peak, wav.FormatDBFS(res.PeakDBFS))
	t.Logf("rms        : %.6f full scale = %s dBFS", res.RMS, wav.FormatDBFS(res.RMSDBFS))
	t.Logf("packets    : %d total, %d silent-flagged, %d discontinuities",
		res.Packets, res.SilentPackets, res.Discontinuities)
	t.Logf("silent     : %v", res.Silent)

	if len(res.PCM) == 0 {
		t.Fatal("capture produced no samples")
	}
	if res.SampleRate <= 0 || res.Channels <= 0 {
		t.Fatalf("bad capture format: %d Hz / %d ch", res.SampleRate, res.Channels)
	}
	// The audio timeline must roughly match the requested duration (the CLI
	// reports the same numbers); allow generous slack for a silent desktop.
	if d := res.Duration.Seconds(); d < seconds*0.5 || d > seconds*1.5 {
		t.Fatalf("captured %.3f s, want about %.1f s", d, seconds)
	}

	// Write it out through the same helper the CLI uses.
	out := t.TempDir() + "/regress.wav"
	if err := wav.WriteInt16File(out, res.SampleRate, res.Channels, res.PCM); err != nil {
		t.Fatalf("WriteInt16File: %v", err)
	}
	a, err := wav.ReadFile(out)
	if err != nil {
		t.Fatalf("re-reading %s: %v", out, err)
	}
	if a.Info.SampleRate != res.SampleRate || a.Info.Channels != res.Channels {
		t.Fatalf("round trip format = %s", a.Info.Layout())
	}
	t.Logf("wrote %s: %s, %d frames", out, a.Info.Layout(), a.Info.Frames)
}
