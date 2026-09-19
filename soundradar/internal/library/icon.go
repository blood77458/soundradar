package library

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sync"
)

// IconSize is the canonical edge length of every stored icon.
const IconSize = 128

var (
	placeholderOnce sync.Once
	placeholderPNG  []byte
)

// PlaceholderIcon returns the generated 128x128 icon used when the user did not
// upload one. It is drawn procedurally (no embedded binary asset) and cached
// after the first call.
func PlaceholderIcon() []byte {
	placeholderOnce.Do(func() {
		img := image.NewNRGBA(image.Rect(0, 0, IconSize, IconSize))
		// Vertical gradient background.
		for y := 0; y < IconSize; y++ {
			t := float64(y) / float64(IconSize-1)
			r := uint8(38 + 24*t)
			g := uint8(44 + 30*t)
			b := uint8(58 + 40*t)
			for x := 0; x < IconSize; x++ {
				img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 255})
			}
		}
		// A simple waveform: vertical bars whose height follows a damped sine,
		// so the placeholder reads as "audio" at a glance.
		ink := color.NRGBA{R: 108, G: 198, B: 255, A: 255}
		const bars = 9
		cx := float64(IconSize) / 2
		for i := 0; i < bars; i++ {
			fx := (float64(i) + 0.5) / float64(bars)
			amp := math.Sin((float64(i)+0.5)/float64(bars)*math.Pi) * 0.78
			h := int(amp * float64(IconSize) * 0.72)
			if h < 6 {
				h = 6
			}
			x := int(fx*float64(IconSize)) - 3
			for dx := 0; dx < 6; dx++ {
				xx := x + dx
				if xx < 0 || xx >= IconSize {
					continue
				}
				for y := int(cx) - h/2; y < int(cx)+h/2; y++ {
					if y < 0 || y >= IconSize {
						continue
					}
					// Rounded ends: shrink near the extremes.
					edge := math.Abs(float64(y)-cx) / (float64(h) / 2)
					if edge > 0.82 && (dx == 0 || dx == 5) {
						continue
					}
					img.SetNRGBA(xx, y, ink)
				}
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			// png.Encode into a bytes.Buffer cannot realistically fail.
			panic(fmt.Sprintf("library: encoding placeholder icon: %v", err))
		}
		placeholderPNG = buf.Bytes()
	})
	return append([]byte(nil), placeholderPNG...)
}

// toNRGBA converts any decoded image into a non-premultiplied RGBA image.
// image/draw's scaling kernels only look at the 8-bit colour channels, and
// premultiplied alpha would darken semi-transparent icons, so the conversion is
// explicit rather than relying on image.Image.At returning NRGBA already.
func toNRGBA(src image.Image) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}
