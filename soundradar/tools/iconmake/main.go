// Command iconmake draws the soundradar icon and writes it as a multi-size .ico
// file, plus the Go source that embeds those bytes into the binary.
//
// It exists so the icon is reproducible from the repository instead of being an
// opaque binary blob: `go run ./tools/iconmake -out internal/server/static/icon.ico`
// regenerates it.
//
// The artwork is a radar sweep: a dark blue disc, two range rings, a bright
// wedge and a blip. It reads at 16x16 (the tray size), which is the size that
// matters most.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// defaultSizes are the images stored in the .ico. 16 and 32 are what Windows
// uses for the notification area and the taskbar; 48 and 256 cover Explorer's
// larger views when the file is also used as the executable's icon.
var defaultSizes = []int{16, 24, 32, 48, 64, 128, 256}

func main() {
	out := flag.String("out", "icon.ico", "output .ico path")
	goOut := flag.String("go", "", "optional path for a generated Go file embedding the icon")
	pkg := flag.String("package", "main", "package name for the generated Go file")
	varName := flag.String("var", "trayIconICO", "variable name for the generated Go file")
	sizeList := flag.String("sizes", "", "comma separated sizes (default 16,24,32,48,64,128,256)")
	flag.Parse()

	sizes := defaultSizes
	if strings.TrimSpace(*sizeList) != "" {
		sizes = nil
		for _, part := range strings.Split(*sizeList, ",") {
			v, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || v < 4 || v > 256 {
				log.Fatalf("无效尺寸 %q", part)
			}
			sizes = append(sizes, v)
		}
	}

	images := make([][]byte, len(sizes))
	for i, s := range sizes {
		img := render(s)
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			log.Fatalf("编码 %dx%d PNG: %v", s, s, err)
		}
		images[i] = buf.Bytes()
	}

	// A .png output writes the largest rendered image directly, which is handy
	// for eyeballing the artwork and for docs.
	if strings.EqualFold(filepath.Ext(*out), ".png") {
		idx := 0
		for i := range sizes {
			if sizes[i] > sizes[idx] {
				idx = i
			}
		}
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil && filepath.Dir(*out) != "." {
			log.Fatalf("创建目录: %v", err)
		}
		if err := os.WriteFile(*out, images[idx], 0o644); err != nil {
			log.Fatalf("写出 %s: %v", *out, err)
		}
		fmt.Printf("写入 %s（%dx%d PNG）\n", *out, sizes[idx], sizes[idx])
		return
	}

	ico := encodeICO(sizes, images)
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil && filepath.Dir(*out) != "." {
		log.Fatalf("创建目录: %v", err)
	}
	if err := os.WriteFile(*out, ico, 0o644); err != nil {
		log.Fatalf("写出 %s: %v", *out, err)
	}
	fmt.Printf("写入 %s（%d 个尺寸，共 %d 字节）\n", *out, len(sizes), len(ico))

	if *goOut != "" {
		if err := writeGo(*goOut, *pkg, *varName, ico); err != nil {
			log.Fatalf("写出 %s: %v", *goOut, err)
		}
		fmt.Printf("写入 %s（内嵌 %d 字节）\n", *goOut, len(ico))
	}
}

// render draws the icon at size n: a radar disc with two thin range rings, a
// sweep wedge aimed to the upper right and a small blip on the outer ring.
func render(n int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	f := float64(n)
	c := f / 2
	rad := f/2 - math.Max(0.5, f/32)
	// Ring thickness scales with the image but never disappears: below about
	// 24 px a thinner ring dissolves into the background, so the small sizes get
	// a proportionally heavier line.
	ringW := math.Max(1.1, f/18)

	bg := color.RGBA{0x0E, 0x18, 0x2B, 0xFF}
	ring := color.RGBA{0x3E, 0xC9, 0x8F, 0xFF}
	sweep := color.RGBA{0x63, 0xF2, 0xB0, 0xFF}
	blip := color.RGBA{0xFF, 0xD2, 0x4D, 0xFF}

	const sweepAt = -0.85 // radians; up and to the right

	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)+0.5-c, float64(y)+0.5-c
			d := math.Hypot(dx, dy)
			if d > rad {
				continue // outside the disc: transparent
			}
			// Base disc with a subtle top-to-bottom gradient.
			t := float64(y) / f
			col := color.RGBA{
				uint8(float64(bg.R) * (1 - 0.30*t)),
				uint8(float64(bg.G) * (1 - 0.20*t)),
				uint8(float64(bg.B) * (1 - 0.10*t)),
				0xFF,
			}
			// Two thin range rings plus the rim.
			for _, rr := range []float64{0.45, 0.78, 0.97} {
				if math.Abs(d-rad*rr) < ringW/2 {
					col = blend(col, ring, 0.8)
				}
			}
			// The sweep: a wedge from near the centre, fading with angle.
			ang := math.Atan2(dy, dx)
			delta := math.Abs(normalizeAngle(ang - sweepAt))
			if d > rad*0.10 && delta < 0.75 {
				col = blend(col, sweep, 0.70*(1-delta/0.75))
			}
			// The blip: a small dot on the outer ring, at the sweep angle.
			bx, by := rad*0.78*math.Cos(sweepAt), rad*0.78*math.Sin(sweepAt)
			if math.Hypot(dx-bx, dy-by) < math.Max(1.0, f/14) {
				col = blend(col, blip, 0.95)
			}
			// A small centre dot.
			if d < math.Max(0.8, f/20) {
				col = blend(col, ring, 0.9)
			}
			// Antialias the disc edge.
			if d > rad-1 {
				col.A = uint8(255 * (rad - d))
			}
			img.Set(x, y, col)
		}
	}
	return img
}

func normalizeAngle(a float64) float64 {
	for a > math.Pi {
		a -= 2 * math.Pi
	}
	for a < -math.Pi {
		a += 2 * math.Pi
	}
	return a
}

// blend mixes two colours (a is the foreground weight).
func blend(dst, src color.RGBA, a float64) color.RGBA {
	mix := func(x, y uint8) uint8 { return uint8(float64(x)*(1-a) + float64(y)*a) }
	return color.RGBA{mix(dst.R, src.R), mix(dst.G, src.G), mix(dst.B, src.B), dst.A}
}

// encodeICO writes a PNG-based .ico (supported by Windows Vista and later, which
// is every Windows this program targets).
func encodeICO(sizes []int, images [][]byte) []byte {
	var buf bytes.Buffer
	// ICONDIR
	binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	binary.Write(&buf, binary.LittleEndian, uint16(len(images)))
	offset := 6 + 16*len(images)
	for i, img := range images {
		s := sizes[i]
		w, h := byte(s), byte(s)
		if s >= 256 {
			w, h = 0, 0 // 0 means 256 in this format
		}
		buf.WriteByte(w)
		buf.WriteByte(h)
		buf.WriteByte(0) // palette size
		buf.WriteByte(0) // reserved
		binary.Write(&buf, binary.LittleEndian, uint16(1))  // colour planes
		binary.Write(&buf, binary.LittleEndian, uint16(32)) // bits per pixel
		binary.Write(&buf, binary.LittleEndian, uint32(len(img)))
		binary.Write(&buf, binary.LittleEndian, uint32(offset))
		offset += len(img)
	}
	for _, img := range images {
		buf.Write(img)
	}
	return buf.Bytes()
}

// writeGo emits a Go file with the icon as a byte literal, so the binary needs
// no external file at runtime.
func writeGo(path, pkg, varName string, ico []byte) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by tools/iconmake. DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	fmt.Fprintf(&b, "// %s is the SoundRadar icon as a whole .ico file (%d bytes).\n",
		varName, len(ico))
	fmt.Fprintf(&b, "// It is embedded so the tray icon and the window icon work even when the\n")
	fmt.Fprintf(&b, "// program runs from a single copied exe.\n")
	fmt.Fprintf(&b, "var %s = []byte{\n", varName)
	for i := 0; i < len(ico); i += 16 {
		b.WriteString("\t")
		for j := i; j < i+16 && j < len(ico); j++ {
			fmt.Fprintf(&b, "0x%02x, ", ico[j])
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "}\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}
