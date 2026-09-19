// Command wavstat is the end-to-end verification tool for the soundradar
// prototype.
//
// It has two modes:
//
//	wavstat gen --freq 880 --seconds 2 --rate 48000 --out tone.wav
//	    Synthesise a known-frequency sine WAV (16-bit PCM) to play back while
//	    soundradar captures loopback.
//
//	wavstat analyze test.wav [--freq 880]
//	wavstat test.wav [--freq 880]
//	    Read a WAV file, print duration / sample rate / channels / peak / RMS,
//	    then run a Goertzel (single-bin DFT at an arbitrary frequency) scan to
//	    find the dominant component and measure how much energy sits exactly at
//	    the expected frequency.
//
// Exit codes: 0 = analysis ok and the expected frequency was found,
// 3 = analysis ok but the expected frequency was NOT found, 1 = error.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/znz/soundradar/internal/testaudio"
	"github.com/znz/soundradar/internal/wav"
)

const (
	exitOK           = 0
	exitError        = 1
	exitNotDetected  = 3
	defaultFreq      = 880.0
	defaultScanLo    = 20.0
	defaultScanHi    = 20000.0
	defaultScanStep  = 10.0
	detectionFloorDB = -70.0 // below this a "peak" is just numerical noise
	detectionRelDB   = -20.0 // target must be within this many dB of the peak
)

const usage = `wavstat - WAV verification tool for soundradar

Usage:
  wavstat gen --freq 880 --seconds 2 --rate 48000 --out tone.wav
  wavstat genmp3 --seconds 1 --out silence.mp3
  wavstat analyze <file.wav> [--freq 880] [--step 10] [--top 6]
  wavstat <file.wav> [--freq 880]

gen options:
  --freq HZ       tone frequency (default 880)
  --seconds N     duration (default 2)
  --rate HZ       sample rate (default 48000)
  --channels N    channel count (default 2, tone duplicated to all channels)
  --amp F         linear amplitude 0..1 (default 0.7, i.e. about -3.1 dBFS)
  --out PATH      output WAV path (default tone.wav)

genmp3 options (P1 helper: writes a silent, structurally valid MP3 so the
library's MP3 import path can be tested without an encoder):
  --seconds N     duration (default 1)
  --out PATH      output MP3 path (default silence.mp3)

analyze options:
  --freq HZ       expected frequency to look for (default 880)
  --step HZ       Goertzel scan resolution (default 10)
  --top N         how many spectral peaks to list (default 6)
  --quiet         only print the verdict
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var code int
	switch args[0] {
	case "gen", "generate", "tone":
		code = runGen(args[1:])
	case "genmp3", "silence":
		code = runGenMP3(args[1:])
	case "analyze", "stat", "check":
		code = runAnalyze(args[1:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		if strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(os.Stderr, "wavstat: expected a subcommand or a WAV path, got %q\n\n%s", args[0], usage)
			os.Exit(2)
		}
		// `wavstat test.wav ...` implies analyze.
		code = runAnalyze(args)
	}
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// gen
// ---------------------------------------------------------------------------

func runGen(args []string) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	freq := fs.Float64("freq", defaultFreq, "tone frequency in Hz")
	seconds := fs.Float64("seconds", 2, "duration in seconds")
	rate := fs.Int("rate", 48000, "sample rate in Hz")
	channels := fs.Int("channels", 2, "channel count")
	amp := fs.Float64("amp", 0.7, "linear amplitude 0..1")
	out := fs.String("out", "tone.wav", "output WAV path")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *freq <= 0 || *freq >= float64(*rate)/2 {
		fmt.Fprintf(os.Stderr, "wavstat: --freq must be in (0, %d)\n", *rate/2)
		return exitError
	}
	if *seconds <= 0 || *rate <= 0 || *channels <= 0 {
		fmt.Fprintln(os.Stderr, "wavstat: --seconds, --rate and --channels must be positive")
		return exitError
	}
	if *amp <= 0 || *amp > 1 {
		fmt.Fprintln(os.Stderr, "wavstat: --amp must be in (0, 1]")
		return exitError
	}

	total := int(*seconds * float64(*rate))
	pcm := make([]int16, total*(*channels))

	// 5 ms raised-cosine fade in/out avoids click artefacts at the edges.
	fade := int(0.005 * float64(*rate))
	if fade*2 > total {
		fade = total / 4
	}

	w := 2 * math.Pi * *freq / float64(*rate)
	for n := 0; n < total; n++ {
		v := *amp * math.Sin(w*float64(n))
		if fade > 0 {
			switch {
			case n < fade:
				v *= 0.5 - 0.5*math.Cos(math.Pi*float64(n)/float64(fade))
			case n >= total-fade:
				v *= 0.5 - 0.5*math.Cos(math.Pi*float64(total-1-n)/float64(fade))
			}
		}
		s := wav.ClampInt16(v)
		for c := 0; c < *channels; c++ {
			pcm[n*(*channels)+c] = s
		}
	}

	if dir := filepath.Dir(*out); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "wavstat: %v\n", err)
			return exitError
		}
	}
	if err := wav.WriteInt16File(*out, *rate, *channels, pcm); err != nil {
		fmt.Fprintf(os.Stderr, "wavstat: %v\n", err)
		return exitError
	}

	st, _ := os.Stat(*out)
	fmt.Printf("[gen] wrote        : %s (%.1f KiB)\n", *out, float64(st.Size())/1024)
	fmt.Printf("[gen] format       : %d Hz / %d ch / 16-bit PCM\n", *rate, *channels)
	fmt.Printf("[gen] tone         : %.3f Hz, %d frames = %.3f s\n", *freq, total, float64(total)/float64(*rate))
	fmt.Printf("[gen] amplitude    : %.4f full scale = %.2f dBFS\n", *amp, wav.DBFS(*amp))
	fmt.Printf("[gen] play it with : (New-Object Media.SoundPlayer '%s').PlaySync()\n", *out)
	return exitOK
}

// ---------------------------------------------------------------------------
// genmp3
// ---------------------------------------------------------------------------

// runGenMP3 synthesises a 48 kHz stereo MPEG-1 Layer III file of silence. It
// exists so the P1 library's MP3 import path can be exercised end to end (the
// sandbox has no MP3 encoder and no network access to a sample file).
func runGenMP3(args []string) int {
	fs := flag.NewFlagSet("genmp3", flag.ContinueOnError)
	seconds := fs.Float64("seconds", 1, "duration in seconds")
	out := fs.String("out", "silence.mp3", "output MP3 path")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *seconds <= 0 {
		fmt.Fprintln(os.Stderr, "wavstat: --seconds must be positive")
		return exitError
	}
	frames := int(math.Round(*seconds * 48000 / float64(testaudio.SamplesPerMPEGFrame)))
	if frames < 1 {
		frames = 1
	}
	raw := testaudio.SilentMP3(frames)

	if dir := filepath.Dir(*out); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "wavstat: %v\n", err)
			return exitError
		}
	}
	if err := os.WriteFile(*out, raw, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "wavstat: %v\n", err)
		return exitError
	}
	st, _ := os.Stat(*out)
	fmt.Printf("[genmp3] wrote     : %s (%d bytes)\n", *out, st.Size())
	fmt.Printf("[genmp3] format    : MPEG-1 Layer III, 48000 Hz, stereo, 128 kbit/s\n")
	fmt.Printf("[genmp3] frames    : %d MPEG frames = %d samples/channel = %.3f s\n",
		frames, frames*testaudio.SamplesPerMPEGFrame,
		float64(frames*testaudio.SamplesPerMPEGFrame)/48000)
	fmt.Printf("[genmp3] content   : digital silence (Huffman table 0, big_values = 0)\n")
	fmt.Printf("[genmp3] note      : no MP3 encoder exists in pure Go here, so the stream\n")
	fmt.Printf("[genmp3]             is synthesised; it is structurally valid and is decoded\n")
	fmt.Printf("[genmp3]             by github.com/hajimehoshi/go-mp3 in internal/audio.\n")
	return exitOK
}

// ---------------------------------------------------------------------------
// analyze
// ---------------------------------------------------------------------------

type peak struct {
	Freq  float64
	AmpDB float64
}

func runAnalyze(args []string) int {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	freq := fs.Float64("freq", defaultFreq, "expected frequency in Hz")
	step := fs.Float64("step", defaultScanStep, "Goertzel scan step in Hz")
	top := fs.Int("top", 6, "number of spectral peaks to report")
	quiet := fs.Bool("quiet", false, "print only the verdict")
	from := fs.Float64("from", 0, "start of the analysis window in seconds")
	length := fs.Float64("len", 0, "length of the analysis window in seconds (0 = to end of file)")
	// Accept flags before or after the positional path.
	var path string
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && path == "" {
			path = a
			continue
		}
		rest = append(rest, a)
	}
	if err := fs.Parse(rest); err != nil {
		return exitError
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "wavstat: analyze needs a WAV path")
		return exitError
	}
	if *step <= 0 {
		fmt.Fprintln(os.Stderr, "wavstat: --step must be positive")
		return exitError
	}

	audio, err := wav.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wavstat: %v\n", err)
		return exitError
	}
	info := audio.Info

	// --- basic statistics over the whole file -----------------------------
	var peakAbs, sumSq float64
	for _, v := range audio.Samples {
		a := math.Abs(v)
		if a > peakAbs {
			peakAbs = a
		}
		sumSq += v * v
	}
	rms := 0.0
	if len(audio.Samples) > 0 {
		rms = math.Sqrt(sumSq / float64(len(audio.Samples)))
	}

	if !*quiet {
		fmt.Printf("[stat] file         : %s\n", path)
		fmt.Printf("[stat] format       : %s\n", info.Layout())
		fmt.Printf("[stat] frames       : %d\n", info.Frames)
		fmt.Printf("[stat] duration     : %.3f s\n", info.DurationSec)
		fmt.Printf("[stat] peak         : %.6f full scale = %s dBFS\n", peakAbs, wav.FormatDBFS(wav.DBFS(peakAbs)))
		fmt.Printf("[stat] rms          : %.6f full scale = %s dBFS\n", rms, wav.FormatDBFS(wav.DBFS(rms)))
	}

	if info.Frames == 0 || peakAbs == 0 {
		fmt.Println("[stat] silent       : TRUE - the file contains no non-zero sample")
		if !*quiet {
			fmt.Printf("[stat] channels     : %d, per-channel peak/RMS below\n", info.Channels)
			for c := 0; c < info.Channels; c++ {
				p, r := channelLevels(audio.Samples, info.Channels, c)
				fmt.Printf("       ch%d: peak %s dBFS, rms %s dBFS\n", c, wav.FormatDBFS(wav.DBFS(p)), wav.FormatDBFS(wav.DBFS(r)))
			}
		}
		fmt.Printf("\nVERDICT: FAIL - captured audio is completely silent, %.1f Hz was not detected\n", *freq)
		return exitNotDetected
	}

	// --- pick the analysis signal ----------------------------------------
	// Use the loudest channel rather than a downmix: a downmix can cancel a
	// tone that is phase-inverted between channels.
	analysis := audio.Samples
	which := "mono mix"
	if info.Channels > 1 {
		bestC, bestRMS := 0, -1.0
		for c := 0; c < info.Channels; c++ {
			_, r := channelLevels(audio.Samples, info.Channels, c)
			if r > bestRMS {
				bestC, bestRMS = c, r
			}
		}
		analysis = deinterleave(audio.Samples, info.Channels, bestC)
		which = fmt.Sprintf("channel %d of %d (loudest)", bestC, info.Channels)
	}

	fsz := float64(info.SampleRate)

	// --- optional analysis window ----------------------------------------
	// A capture that starts before playback begins contains leading digital
	// silence, which dilutes every amplitude estimate by the duty cycle.
	// --from/--len restrict the analysis to a steady segment.
	if *from > 0 || *length > 0 {
		start := int(*from * fsz)
		if start < 0 {
			start = 0
		}
		if start > len(analysis) {
			start = len(analysis)
		}
		end := len(analysis)
		if *length > 0 {
			end = start + int(*length*fsz)
			if end > len(analysis) {
				end = len(analysis)
			}
		}
		analysis = analysis[start:end]
		which = fmt.Sprintf("%s, window [%.3f s, %.3f s)", which, float64(start)/fsz, float64(end)/fsz)
	}

	if !*quiet {
		fmt.Printf("[stat] channels     : %d\n", info.Channels)
		for c := 0; c < info.Channels; c++ {
			p, r := channelLevels(audio.Samples, info.Channels, c)
			fmt.Printf("       ch%d: peak %s dBFS, rms %s dBFS\n", c, wav.FormatDBFS(wav.DBFS(p)), wav.FormatDBFS(wav.DBFS(r)))
		}
		fmt.Printf("[dft]  analysis on  : %s, %d samples\n", which, len(analysis))
	}

	if len(analysis) < 16 {
		fmt.Fprintln(os.Stderr, "wavstat: analysis window is too short")
		return exitError
	}

	// --- window + reference sums -----------------------------------------
	n := len(analysis)
	win := make([]float64, n)
	// Hann window (periodic form, which is what DFT analysis wants).
	var sumW, sumW2 float64
	for i := 0; i < n; i++ {
		win[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
		sumW += win[i]
		sumW2 += win[i] * win[i]
	}
	windowed := make([]float64, n)
	var eTotal float64
	for i := 0; i < n; i++ {
		windowed[i] = analysis[i] * win[i]
		eTotal += windowed[i] * windowed[i]
	}

	nyquist := 0.5 * fsz
	scanHi := defaultScanHi
	if scanHi > nyquist*0.98 {
		scanHi = nyquist * 0.98
	}

	// --- Goertzel scan ----------------------------------------------------
	var grid []float64
	var db []float64
	for f := defaultScanLo; f <= scanHi; f += *step {
		grid = append(grid, f)
		db = append(db, ampDB(windowed, fsz, f, sumW))
	}

	idx := 0
	for i, v := range db {
		if v > db[idx] {
			idx = i
		}
	}
	dominant := grid[idx]
	if idx > 0 && idx < len(grid)-1 {
		dominant = parabolicPeak(grid[idx-1], db[idx-1], grid[idx], db[idx], grid[idx+1], db[idx+1])
	}
	peakDB := db[idx]

	// Target frequency measured exactly (not snapped to the grid).
	targetDB := ampDB(windowed, fsz, *freq, sumW)
	// ampDB already yields the sine's PEAK amplitude in full-scale units
	// (1.0 == 0 dBFS) thanks to the 2/sumW single-sided scaling, so no
	// further conversion is needed here.
	targetPeak := math.Pow(10, targetDB/20)
	// Energy a pure sine of that peak amplitude deposits into this window:
	//   sum( (A*sin(w n))^2 * win^2 )  ==  (A^2/2) * sum(win^2)
	// Using the same windowed basis for the numerator and the denominator
	// makes the ratio a meaningful "share of energy" figure.
	eTarget := 0.0
	if targetPeak > 0 {
		eTarget = (targetPeak * targetPeak / 2) * sumW2
	}
	share := 0.0
	if eTotal > 0 {
		share = eTarget / eTotal
	}
	if share > 1 {
		share = 1
	}

	if !*quiet {
		fmt.Printf("[dft]  method       : Goertzel single-bin DFT, Hann window, grid %.1f..%.1f Hz step %.1f Hz (%d bins)\n",
			defaultScanLo, scanHi, *step, len(grid))
		fmt.Printf("[dft]  dominant     : %.1f Hz at %s dBFS (bin peak at %.0f Hz)\n", dominant, wav.FormatDBFS(peakDB), grid[idx])
		fmt.Printf("[dft]  peaks        :\n")
		for _, p := range topPeaks(grid, db, *top, 50) {
			fmt.Printf("         %8.1f Hz  %8s dBFS\n", p.Freq, wav.FormatDBFS(p.AmpDB))
		}
		fmt.Printf("[dft]  target       : %.1f Hz at %s dBFS (peak amplitude %.6f full scale)\n", *freq, wav.FormatDBFS(targetDB), targetPeak)
		fmt.Printf("[dft]  target share : %.2f%% of the windowed signal energy\n", share*100)
		fmt.Printf("[dft]  target vs peak: %+.1f dB relative to the strongest component\n", targetDB-peakDB)
	}

	// --- verdict ----------------------------------------------------------
	// 1) the tone must be well above the numerical noise floor, and
	// 2) it must be within 20 dB of the strongest component in the capture.
	detected := targetDB > detectionFloorDB && (targetDB-peakDB) > detectionRelDB
	nearDominant := math.Abs(dominant-*freq) <= math.Max(*step, 2.0)

	fmt.Println()
	if detected {
		extra := ""
		if nearDominant {
			extra = fmt.Sprintf("; it is also the dominant component (%.1f Hz)", dominant)
		} else {
			extra = fmt.Sprintf("; the dominant component is %.1f Hz", dominant)
		}
		fmt.Printf("VERDICT: PASS - expected %.1f Hz found at %s dBFS, %.1f%% of energy%s\n",
			*freq, wav.FormatDBFS(targetDB), share*100, extra)
		return exitOK
	}
	fmt.Printf("VERDICT: FAIL - expected %.1f Hz not found (measured %s dBFS, %.1f%% of energy; dominant %.1f Hz at %s dBFS)\n",
		*freq, wav.FormatDBFS(targetDB), share*100, dominant, wav.FormatDBFS(peakDB))
	return exitNotDetected
}

// ampDB returns the amplitude of a single DFT bin at an arbitrary frequency in
// dBFS, using the Goertzel recurrence. The windowed input plus the window sum
// give a properly scaled single-sided amplitude estimate.
func ampDB(windowed []float64, fs, freq, sumW float64) float64 {
	mag := goertzelMagnitude(windowed, fs, freq)
	amp := 2 * mag / sumW
	if amp <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(amp)
}

// goertzelMagnitude evaluates |X(f)| of x at an arbitrary (non-integer bin)
// frequency f.
func goertzelMagnitude(x []float64, fs, freq float64) float64 {
	coeff := 2 * math.Cos(2*math.Pi*freq/fs)
	var s1, s2 float64
	for _, v := range x {
		s0 := v + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	power := s1*s1 + s2*s2 - coeff*s1*s2
	if power < 0 || math.IsNaN(power) {
		return 0
	}
	return math.Sqrt(power)
}

// parabolicPeak refines a spectral peak position from three points of the
// magnitude curve (in dB) using parabolic interpolation.
func parabolicPeak(f1, y1, f2, y2, f3, y3 float64) float64 {
	if math.IsInf(y1, -1) || math.IsInf(y3, -1) || math.IsInf(y2, -1) {
		return f2
	}
	denom := y1 - 2*y2 + y3
	if denom == 0 {
		return f2
	}
	delta := 0.5 * (y1 - y3) / denom
	if delta > 1 || delta < -1 {
		return f2
	}
	return f2 + delta*(f3-f1)/2
}

// topPeaks returns the strongest well-separated local maxima of the scan.
func topPeaks(grid, db []float64, n int, minSepHz float64) []peak {
	cands := make([]peak, 0, len(grid))
	for i := 1; i < len(grid)-1; i++ {
		if db[i] >= db[i-1] && db[i] > db[i+1] && db[i] > detectionFloorDB {
			f := grid[i]
			f = parabolicPeak(grid[i-1], db[i-1], grid[i], db[i], grid[i+1], db[i+1])
			cands = append(cands, peak{Freq: f, AmpDB: db[i]})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].AmpDB > cands[j].AmpDB })

	out := make([]peak, 0, n)
	for _, c := range cands {
		ok := true
		for _, o := range out {
			if math.Abs(o.Freq-c.Freq) < minSepHz {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, c)
			if len(out) == n {
				break
			}
		}
	}
	return out
}

func deinterleave(samples []float64, channels, c int) []float64 {
	out := make([]float64, 0, len(samples)/channels)
	for i := c; i < len(samples); i += channels {
		out = append(out, samples[i])
	}
	return out
}

func channelLevels(samples []float64, channels, c int) (peak, rms float64) {
	var sumSq float64
	var n int64
	for i := c; i < len(samples); i += channels {
		v := samples[i]
		if a := math.Abs(v); a > peak {
			peak = a
		}
		sumSq += v * v
		n++
	}
	if n > 0 {
		rms = math.Sqrt(sumSq / float64(n))
	}
	return peak, rms
}
