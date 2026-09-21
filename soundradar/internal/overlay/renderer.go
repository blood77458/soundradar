// Package overlay implements the soundradar P3 hit overlay: a per-pixel
// transparent, click-through, always-on-top popup that shows the icon and name
// of a recognised sound effect without ever stealing focus from the game.
//
// The package is split in two on purpose:
//
//   - renderer.go is PURE GO. It composites the whole popup (icons scaled with
//     golang.org/x/image/draw, Chinese text shaped with golang.org/x/image/font
//     and drawn with font.Drawer, confidence bar rasterised with
//     golang.org/x/image/vector) into an *image.RGBA whose alpha channel we
//     control. It never touches Win32, so it is unit-testable, and it is the
//     reason the text is actually visible (see the GDI note in window_windows.go).
//
//   - window_windows.go is deliberately thin: it owns the HWND, the message
//     loop, the global hotkey and the ONE thing that needs Win32 - handing the
//     finished RGBA canvas to the compositor through
//     UpdateLayeredWindow + a premultiplied BGRA DIBSection.
//
// timeline.go holds the queue/alpha state machine, which is also pure Go.
package overlay

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// RenderOptions describes one canvas shape. It is a value type: Renderer keeps
// the copy it was built with, so a config change that alters the canvas means a
// new Renderer (NewRenderer), not a mutation.
type RenderOptions struct {
	// Width, Height are the whole canvas in physical pixels.
	Width, Height int
	// IconSize is the nominal icon box (square) in physical pixels.
	IconSize  int
	ShowName  bool
	ShowScore bool
	// CardLayout draws each item as a tile: icon on top, grid size and name
	// underneath. Width/Height must be CardCanvasGrouped(IconSize, items)
	// (one row per grid format).
	CardLayout bool
	// Alpha is the global opacity 0..1 applied to every pixel of the canvas
	// (this is what fade in/out animates).
	Alpha float64
}

// withDefaults clamps the options into a shape the renderer can always draw.
func (o RenderOptions) withDefaults() RenderOptions {
	if o.Width < 8 {
		o.Width = 8
	}
	if o.Height < 8 {
		o.Height = 8
	}
	if o.IconSize <= 0 {
		o.IconSize = 64
	}
	if o.Alpha < 0 {
		o.Alpha = 0
	}
	if o.Alpha > 1 {
		o.Alpha = 1
	}
	return o
}

// Renderer composites display items into a canvas. It is NOT safe for concurrent
// use; the overlay calls it from its window thread only.
type Renderer struct {
	opt  RenderOptions
	face font.Face
	// asciiOnly is true when no CJK-capable system font could be loaded and the
	// basicfont fallback is in use (which only has ASCII glyphs).
	asciiOnly bool
	fontName  string

	// icons caches the SCALED icon per item id: decoding and CatmullRom
	// downscaling a 128x128 PNG is the expensive part of a frame, and the same
	// item lights up dozens of frames in a row.
	icons    map[string]*image.RGBA
	iconPx   int
	decodes  int
	requests int
}

// NewRenderer loads a Chinese-capable system font and returns a renderer.
//
// It never fails: when neither msyh.ttc nor simhei.ttf can be opened it falls
// back to golang.org/x/image/font/basicfont and sets asciiOnly, which makes the
// Draw path replace non-ASCII runes so the text is legible instead of boxes.
func NewRenderer(opt RenderOptions) (*Renderer, error) {
	opt = opt.withDefaults()
	r := &Renderer{opt: opt, icons: make(map[string]*image.RGBA)}
	face, name, ascii, err := loadUIFace(opt.FontSizeIfAny())
	if err != nil {
		return nil, err
	}
	r.face, r.fontName, r.asciiOnly = face, name, ascii
	return r, nil
}

// FontSizeIfAny returns the font size the layout will use, so the font can be
// loaded once at the right size. The layout may then shrink it for many items,
// in which case FaceFor reloads.
func (o RenderOptions) FontSizeIfAny() float64 { return fontSizeFor(o.IconSize) }

// Options returns the effective options.
func (r *Renderer) Options() RenderOptions { return r.opt }

// FontName reports which font file (or fallback) is in use.
func (r *Renderer) FontName() string { return r.fontName }

// ASCIIOnly reports whether the renderer fell back to basicfont.
func (r *Renderer) ASCIIOnly() bool { return r.asciiOnly }

// SetAlpha changes the global opacity without rebuilding anything (the fade
// animation does this every frame).
func (r *Renderer) SetAlpha(a float64) {
	if a < 0 {
		a = 0
	}
	if a > 1 {
		a = 1
	}
	r.opt.Alpha = a
}

// Alpha returns the current global opacity.
func (r *Renderer) Alpha() float64 { return r.opt.Alpha }

// CacheStats reports (unique icons decoded, total icon requests), which is how
// the icon-cache test proves "same id is decoded once".
func (r *Renderer) CacheStats() (decoded, requests int) { return r.decodes, r.requests }

// ---------------------------------------------------------------------------
// layout
// ---------------------------------------------------------------------------

// layout is the resolved geometry of one frame.
type layout struct {
	count    int
	spacing  int
	cellW    int
	cellH    int
	iconSize int
	textH    int
	textGap  int
	fontPx   float64
	nameW    int

	// Per-cell positions filled in by place(): x0 is the left edge of the first
	// column, y0 the top edge of every column, plateTop the top edge of the text
	// plate, iconCX/iconCY the centre of an icon. The tests assert on these so a
	// layout change cannot silently move the drawing.
	x0       int
	y0       int
	plateTop int
	iconCX   int
	iconCY   int
}

// place resolves the on-canvas positions from the cell metrics.
func (l *layout) place(o RenderOptions, showName, showScore bool) {
	if l.count <= 0 {
		return
	}
	totalW := l.count*l.cellW + (l.count-1)*l.spacing
	l.x0 = (o.Width - totalW) / 2
	if l.x0 < spacingPad {
		l.x0 = spacingPad
	}
	l.y0 = spacingPad
	if l.y0+l.cellH > o.Height {
		l.y0 = o.Height - l.cellH
	}
	if l.y0 < 0 {
		l.y0 = 0
	}
	strip := 0
	if showName {
		strip += l.textH
	}
	if showScore {
		strip += l.textH + 5
	}
	l.plateTop = l.y0 + l.iconSize - strip
	if l.plateTop < l.y0 {
		l.plateTop = l.y0
	}
	l.iconCX = l.x0 + l.cellW/2
	l.iconCY = l.y0 + l.iconSize/2
}

// computeLayout divides the canvas between n items.
//
// The icon owns the full cell (square, capped by the column width and the cell
// height); the name/score strip is drawn as an OVERLAY at the bottom of the cell
// rather than below the icon, so a full-size icon still fits the canvas and the
// text stays readable on top of the artwork. Everything is integer arithmetic so
// the horizontal spans are exactly reproducible.
func (r *Renderer) computeLayout(n int) layout {
	o := r.opt
	var l layout
	l.count = n
	if n <= 0 {
		return l
	}

	// Horizontal: one column per event, with a gap between columns and a
	// transparent margin at both ends.
	if n > 1 {
		l.spacing = o.IconSize / 12
	}
	if l.spacing < 2 {
		l.spacing = 2
	}
	if l.spacing > 12 {
		l.spacing = 12
	}
	avail := o.Width - (n+1)*l.spacing
	if avail < n {
		avail = n
	}
	cellW := avail / n
	if cellW > o.IconSize {
		cellW = o.IconSize
	}
	if cellW < 4 {
		cellW = 4
	}
	l.cellW = cellW
	if o.ShowName {
		l.nameW = cellW * 3
	} else if o.ShowScore {
		l.nameW = cellW * 2
	}

	// Text metrics derived from the column width.
	fontPx := float64(cellW) * 0.20
	if fontPx < 9 {
		fontPx = 9
	}
	if fontPx > 28 {
		fontPx = 28
	}
	l.fontPx = fontPx
	textH := int(fontPx * 1.30)
	if textH < 11 {
		textH = 11
	}
	l.textH = textH
	textGap := int(fontPx / 4)
	if textGap < 2 {
		textGap = 2
	}
	l.textGap = textGap

	// Vertical: the icon fills the cell height minus the transparent margin.
	cellH := o.Height - 2*spacingPad
	if cellH < 6 {
		cellH = 6
	}
	l.cellH = cellH
	icon := cellW
	if icon > cellH {
		icon = cellH
	}
	if icon < 4 {
		icon = 4
	}
	l.iconSize = icon
	l.place(o, o.ShowName, o.ShowScore)
	return l
}

// spacingPad mirrors config.CanvasPad: the transparent margin the canvas keeps
// around the cells. It is duplicated as a constant (not imported) so the
// renderer has no dependency on internal/config and stays trivially testable.
const spacingPad = 6

// rendererVersion is bumped on every layout change; the tests print it so a
// stale test binary is obvious instead of looking like a layout bug.
const rendererVersion = "p3-2026-09-18-A"

// ---------------------------------------------------------------------------
// drawing
// ---------------------------------------------------------------------------

// DisplayItem is one hit to show.
type DisplayItem struct {
	Icon  image.Image
	Name  string
	Score float64
	// AgeMs is how long ago the event started; the caller turns it into Alpha.
	AgeMs int
	// ID is used only for the icon cache key (empty means "do not cache").
	ID string
	// Grid is the stash size ("3×2") drawn under the icon in card layout.
	Grid string
}

// Draw composites items into a fresh RGBA canvas.
//
// The canvas background is fully transparent (alpha 0): nothing is painted
// outside the cells, which is what lets the window be a per-pixel layered
// window with no rectangle behind it.
func (r *Renderer) Draw(items []DisplayItem, frame int) *image.RGBA {
	o := r.opt
	if o.CardLayout {
		return r.drawCards(items)
	}
	canvas := image.NewRGBA(image.Rect(0, 0, o.Width, o.Height))
	if len(items) == 0 {
		return canvas
	}
	l := r.computeLayout(len(items))
	for i := range items {
		cx := l.x0 + i*(l.cellW+l.spacing)
		r.drawCell(canvas, items[i], cx, l.y0, l, frame)
	}

	applyAlpha(canvas, o.Alpha)
	return canvas
}

// cardColMax is how many hint tiles of the same grid format share one row
// before wrapping. Different formats always start a new row.
const cardColMax = 8

func cardGrid(n int) (cols, rows int) {
	if n < 1 {
		n = 1
	}
	cols = n
	if cols > cardColMax {
		cols = cardColMax
	}
	rows = (n + cols - 1) / cols
	return cols, rows
}

func gridGroupKey(g string) string {
	g = strings.ToLower(strings.TrimSpace(g))
	g = strings.ReplaceAll(g, "×", "x")
	g = strings.ReplaceAll(g, " ", "")
	return g
}

func groupCardsByGrid(items []DisplayItem) [][]DisplayItem {
	index := map[string]int{}
	var groups [][]DisplayItem
	for _, it := range items {
		key := gridGroupKey(it.Grid)
		if i, ok := index[key]; ok {
			groups[i] = append(groups[i], it)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, []DisplayItem{it})
	}
	return groups
}

func cardLayoutSize(items []DisplayItem) (cols, rows int) {
	groups := groupCardsByGrid(items)
	if len(groups) == 0 {
		return 1, 1
	}
	maxCols := 1
	totalRows := 0
	for _, g := range groups {
		c, r := cardGrid(len(g))
		if c > maxCols {
			maxCols = c
		}
		totalRows += r
	}
	return maxCols, totalRows
}

func cardGap(icon int) int {
	gap := icon / 10
	if gap < 4 {
		gap = 4
	}
	if gap > 12 {
		gap = 12
	}
	return gap
}

func cardTextBlock(icon int) int {
	h := icon / 5
	if h < 34 {
		h = 34
	}
	if h > 56 {
		h = 56
	}
	return h
}

func cardCanvasSize(icon, cols, rows int) (w, h int) {
	if icon < 48 {
		icon = 48
	}
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	gap := cardGap(icon)
	tb := cardTextBlock(icon)
	w = cols*icon + (cols+1)*gap + 2*spacingPad
	h = rows*(icon+tb) + (rows+1)*gap + 2*spacingPad
	return w, h
}

// CardCanvas is the window size for n hint tiles of a single grid format.
func CardCanvas(icon, n int) (w, h int) {
	cols, rows := cardGrid(n)
	return cardCanvasSize(icon, cols, rows)
}

// CardCanvasGrouped is the window size when tiles are grouped by grid format
// (one row per format, wrapping only within the same format).
func CardCanvasGrouped(icon int, items []DisplayItem) (w, h int) {
	cols, rows := cardLayoutSize(items)
	return cardCanvasSize(icon, cols, rows)
}

// drawCards paints hint tiles grouped by grid format: same size on one row,
// a new format starts a new row. Picture, then grid size and name underneath.
func (r *Renderer) drawCards(items []DisplayItem) *image.RGBA {
	o := r.opt
	canvas := image.NewRGBA(image.Rect(0, 0, o.Width, o.Height))
	if len(items) == 0 {
		applyAlpha(canvas, o.Alpha)
		return canvas
	}
	icon := o.IconSize
	if icon < 48 {
		icon = 48
	}
	gap := cardGap(icon)
	tb := cardTextBlock(icon)
	row := 0
	for _, g := range groupCardsByGrid(items) {
		gCols, gRows := cardGrid(len(g))
		for i, it := range g {
			c := i % gCols
			rr := row + i/gCols
			x := spacingPad + gap + c*(icon+gap)
			y := spacingPad + gap + rr*(icon+tb+gap)
			r.drawCard(canvas, it, x, y, icon, tb)
		}
		row += gRows
	}
	applyAlpha(canvas, o.Alpha)
	return canvas
}

func (r *Renderer) drawCard(dst *image.RGBA, it DisplayItem, x, y, icon, textH int) {
	img := r.iconFor(it.ID, it.Icon, icon)
	iw, ih := img.Bounds().Dx(), img.Bounds().Dy()
	ix := x + (icon-iw)/2
	iy := y + (icon-ih)/2
	draw.Draw(dst, image.Rect(ix, iy, ix+iw, iy+ih), img, img.Bounds().Min, draw.Over)

	plateY := y + icon
	drawRoundRect(dst, x, plateY, icon, textH, 6, color.NRGBA{R: 12, G: 14, B: 18, A: 220})
	half := textH / 2
	grid := it.Grid
	name := it.Name
	if r.asciiOnly {
		grid = asciiFold(grid)
		name = asciiFold(name)
	}
	if grid == "" {
		grid = " "
	}
	drawCentered(dst, r.face, grid, x, plateY, icon, half, color.NRGBA{R: 130, G: 210, B: 255, A: 255})
	drawCentered(dst, r.face, name, x, plateY+half, icon, textH-half, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
}

// drawCell paints one item inside the column starting at (x, y).
//
// Layer order, bottom to top: icon (aspect-preserving, centred in the cell),
// confidence ring, then a gradient plate carrying the name and the score. The
// plate is an OVERLAY at the bottom of the cell: the design keeps the full icon
// visible instead of shrinking it to make room for the text.
func (r *Renderer) drawCell(dst *image.RGBA, it DisplayItem, x, y int, l layout, frame int) {
	o := r.opt
	icon := r.iconFor(it.ID, it.Icon, l.iconSize)
	ix := x + (l.cellW-l.iconSize)/2
	iy := y + (l.iconSize-icon.Bounds().Dy())/2
	draw.Draw(dst, image.Rect(ix, iy, ix+icon.Bounds().Dx(), iy+icon.Bounds().Dy()),
		icon, icon.Bounds().Min, draw.Over)
	if o.ShowScore && l.iconSize >= 20 {
		r.drawScoreRing(dst, x+l.cellW/2, y+l.iconSize/2, l.iconSize/2+2, it.Score)
	}

	if !o.ShowName && !o.ShowScore {
		return
	}
	plateTop := y + l.plateTop - l.y0
	strip := y + l.iconSize - plateTop
	if plateTop < y {
		plateTop = y
	}
	// A vertical gradient (semi-transparent at the top, darker at the bottom)
	// keeps the top edge of the plate from cutting a hard line across the icon
	// while the glyphs sit on a dark, readable base.
	drawGradientRect(dst, x+1, plateTop, l.cellW-1, strip, 175)

	pos := plateTop
	if o.ShowName {
		name := it.Name
		if r.asciiOnly {
			name = asciiFold(name)
		}
		drawCentered(dst, r.face, name, x, pos, l.cellW, l.textH,
			color.NRGBA{R: 255, G: 255, B: 255, A: 255})
		pos += l.textH
	}
	if o.ShowScore {
		drawCentered(dst, r.face, fmt.Sprintf("%.0f%%", clamp01(it.Score)*100), x, pos, l.cellW, l.textH,
			scoreColor(it.Score))
		r.drawScoreBar(dst, x, pos+l.textH-3, l.cellW, it.Score)
	}
	_ = frame
}

// drawGradientRect fills a rounded rectangle whose alpha ramps from minA at the
// top to maxA at the bottom, so the icon still shows through the upper part of
// the text plate while the bottom (where the glyphs are) is solid enough to be
// readable over a bright game scene.
func drawGradientRect(dst *image.RGBA, x, y, w, h int, maxA uint8) {
	if w <= 0 || h <= 0 {
		return
	}
	const minA = 0.28 // never fully transparent: the text needs contrast
	for row := 0; row < h; row++ {
		t := float64(row) / float64(maxInt(h-1, 1))
		a := uint8(float64(maxA) * (minA + (1-minA)*t))
		if a == 0 {
			continue
		}
		drawRoundRect(dst, x, y+row, w, 1, 0, color.NRGBA{A: a})
	}
}

// drawScoreBar renders the confidence as a rounded bar plus a confidence ring
// around the icon, both anti-aliased through golang.org/x/image/vector.
func (r *Renderer) drawScoreBar(dst *image.RGBA, x, y, w int, score float64) {
	s := clamp01(score)
	if w < 8 {
		return
	}
	bh := 3
	// Track.
	drawRoundRect(dst, x+1, y, w-2, bh, bh/2, color.NRGBA{R: 255, G: 255, B: 255, A: 46})
	// Fill.
	fw := int(float64(w-2)*s + 0.5)
	if fw < 1 {
		fw = 1
	}
	drawRoundRect(dst, x+1, y, fw, bh, bh/2, scoreColor(score))
}

// drawScoreRing draws an anti-aliased arc around the icon showing the
// confidence; 0..1 maps to 0..360 degrees starting at 12 o'clock.
//
// It is a STROKE, not a disc: the filled area is (outer circle minus inner
// circle), rasterised by golang.org/x/image/vector through the even-odd fill
// rule. The middle of the icon is never covered.
func (r *Renderer) drawScoreRing(dst *image.RGBA, cx, cy, radius int, score float64) {
	s := clamp01(score)
	if radius < 6 {
		return
	}
	steps := 8 + int(s*72)
	dim := 2*radius + 2
	center := float32(radius) + 1
	outer := float32(radius)
	inner := outer - 3
	if inner < 1 {
		inner = 1
	}

	ras := vector.NewRasterizer(dim, dim)
	const twoPi = 6.283185307179586
	arc := func(rad float32, reverse bool) {
		for i := 0; i <= steps; i++ {
			k := i
			if reverse {
				k = steps - i
			}
			a := -math.Pi/2 + twoPi*s*float64(k)/float64(steps)
			x := center + rad*float32(math.Cos(a))
			y := center + rad*float32(math.Sin(a))
			if i == 0 && !reverse {
				ras.MoveTo(x, y)
			} else {
				ras.LineTo(x, y)
			}
		}
	}
	arc(outer, false)
	arc(inner, true)
	ras.ClosePath()

	// Track, so an empty bar still shows where the ring is.
	if s < 0.999 {
		track := vector.NewRasterizer(dim, dim)
		arcFull := func(rad float32, reverse bool) {
			for i := 0; i <= 64; i++ {
				k := i
				if reverse {
					k = 64 - i
				}
				a := -math.Pi/2 + twoPi*float64(k)/64
				x := center + rad*float32(math.Cos(a))
				y := center + rad*float32(math.Sin(a))
				if i == 0 && !reverse {
					track.MoveTo(x, y)
				} else {
					track.LineTo(x, y)
				}
			}
		}
		arcFull(outer, false)
		arcFull(inner, true)
		track.ClosePath()
		track.Draw(dst, image.Rect(cx-radius-1, cy-radius-1, cx+radius+1, cy+radius+1),
			image.NewUniform(color.NRGBA{R: 255, G: 255, B: 255, A: 40}), image.Point{})
	}
	ras.Draw(dst, image.Rect(cx-radius-1, cy-radius-1, cx+radius+1, cy+radius+1),
		image.NewUniform(scoreColor(score)), image.Point{})
}

// ---------------------------------------------------------------------------
// primitives
// ---------------------------------------------------------------------------

// drawRoundRect fills a rounded rectangle with anti-aliased corners.
func drawRoundRect(dst *image.RGBA, x, y, w, h, r int, col color.NRGBA) {
	if w <= 0 || h <= 0 {
		return
	}
	if r < 0 {
		r = 0
	}
	if 2*r > w {
		r = w / 2
	}
	if 2*r > h {
		r = h / 2
	}
	if col.A == 0 {
		return
	}
	rf := float64(r)
	for yy := 0; yy < h; yy++ {
		for xx := 0; xx < w; xx++ {
			// Distance outside the rounded rectangle, in pixels.
			var dx, dy float64
			if xx < r {
				dx = float64(r - xx)
			} else if xx >= w-r {
				dx = float64(xx - (w - r - 1))
			}
			if yy < r {
				dy = float64(r - yy)
			} else if yy >= h-r {
				dy = float64(yy - (h - r - 1))
			}
			cov := 1.0
			if dx > 0 && dy > 0 {
				d := math.Sqrt(dx*dx+dy*dy) - rf
				if d >= 0.5 {
					continue
				}
				if d > -0.5 {
					cov = 0.5 - d
				}
			}
			blend(dst, x+xx, y+yy, col, cov)
		}
	}
}

// drawCircle fills an anti-aliased circle outline-free disc.
func drawCircle(dst *image.RGBA, cx, cy, radius int, col color.NRGBA) {
	if radius <= 0 {
		return
	}
	rf := float64(radius)
	for yy := cy - radius; yy <= cy+radius; yy++ {
		for xx := cx - radius; xx <= cx+radius; xx++ {
			d := math.Sqrt(float64(xx-cx)*float64(xx-cx)+float64(yy-cy)*float64(yy-cy)) - rf
			if d >= 0.5 {
				continue
			}
			cov := 1.0
			if d > -0.5 {
				cov = 0.5 - d
			}
			blend(dst, xx, yy, col, cov)
		}
	}
}

// blend composites col with coverage cov onto dst (src-over, non-premultiplied
// source colour, which is what image/draw expects for a uniform colour).
func blend(dst *image.RGBA, x, y int, col color.NRGBA, cov float64) {
	if !(image.Point{x, y}).In(dst.Rect) || cov <= 0 {
		return
	}
	if cov > 1 {
		cov = 1
	}
	sa := float64(col.A) / 255 * cov
	if sa <= 0 {
		return
	}
	i := dst.PixOffset(x, y)
	da := float64(dst.Pix[i+3]) / 255
	oa := sa + da*(1-sa)
	if oa <= 0 {
		dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = 0, 0, 0, 0
		return
	}
	src := [3]float64{float64(col.R), float64(col.G), float64(col.B)}
	for c := 0; c < 3; c++ {
		dc := float64(dst.Pix[i+c]) / 255
		dst.Pix[i+c] = clamp8((src[c]*sa + dc*da*(1-sa)) / oa)
	}
	dst.Pix[i+3] = clamp8(oa * 255)
}

func clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// applyAlpha scales the whole canvas with factor f, in place.
func applyAlpha(img *image.RGBA, f float64) {
	if f >= 1 {
		return
	}
	if f < 0 {
		f = 0
	}
	for i := 0; i < len(img.Pix); i += 4 {
		a := float64(img.Pix[i+3]) * f
		if a < 0.5 {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 0, 0, 0, 0
			continue
		}
		// image.RGBA is alpha-premultiplied, so the colour channels must be
		// scaled together with alpha or the image brightens.
		img.Pix[i] = clamp8(float64(img.Pix[i]) * f)
		img.Pix[i+1] = clamp8(float64(img.Pix[i+1]) * f)
		img.Pix[i+2] = clamp8(float64(img.Pix[i+2]) * f)
		img.Pix[i+3] = clamp8(a)
	}
}

// ---------------------------------------------------------------------------
// text
// ---------------------------------------------------------------------------

// drawCentered renders s horizontally centred inside [x, x+w).
//
// 它**不会**让文字溢出格子：先按格子宽度把字符串截断并补 "…"，再把起始位置夹在
// 格子内。之前没有这一步，长名字（例如「脚步-近-隔墙-耳机-01」）会越过格子的
// 左右边界，和相邻格子的文字叠在一起，屏幕上看起来是乱码。
func drawCentered(dst *image.RGBA, face font.Face, s string, x, y, w, boxH int, col color.NRGBA) {
	if face == nil || s == "" || w <= 2 {
		return
	}
	d := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(col),
		Face: face,
	}
	s = fitString(d, s, w-2)
	adv := d.MeasureString(s)
	tx := x + (w-int(adv.Round()))/2
	if tx < x {
		tx = x
	}
	// Vertically centre using the font metrics (ascent dominates for CJK).
	m := face.Metrics()
	textH := (m.Ascent + m.Descent).Round()
	ty := y + (boxH-textH)/2 + m.Ascent.Round()
	d.Dot = fixed.P(tx, ty)
	d.DrawString(s)
}

// fitString 把 s 截短到能放进 maxW 像素（超出部分用 "…" 结尾）。
// 输入非空时返回值也非空（最差只有一个 "…"）。
func fitString(d *font.Drawer, s string, maxW int) string {
	if maxW <= 0 || s == "" {
		return s
	}
	if int(d.MeasureString(s).Round()) <= maxW {
		return s
	}
	const ellipsis = "…"
	rs := []rune(s)
	for n := len(rs) - 1; n > 0; n-- {
		cand := string(rs[:n]) + ellipsis
		if int(d.MeasureString(cand).Round()) <= maxW {
			return cand
		}
	}
	return ellipsis
}

// asciiFold replaces every non-ASCII rune with '?' so the basicfont fallback
// (which has no CJK glyphs) shows something readable instead of nothing.
//
// It works on the UTF-8 BYTES rather than ranging over the string: a byte-based
// walk cannot disagree with itself about how many runes a literal has, which
// makes the replacement count exactly checkable in a unit test.
func asciiFold(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c < 0x80:
			out = append(out, c)
			i++
		case c >= 0xF0: // 4-byte sequence
			out = append(out, '?')
			i += 4
		case c >= 0xE0: // 3-byte sequence (CJK)
			out = append(out, '?')
			i += 3
		case c >= 0xC0: // 2-byte sequence
			out = append(out, '?')
			i += 2
		default: // stray continuation byte
			out = append(out, '?')
			i++
		}
	}
	return string(out)
}

// scoreColor maps a similarity score onto white -> amber -> green.
func scoreColor(score float64) color.NRGBA {
	s := clamp01(score)
	switch {
	case s >= 0.9:
		return color.NRGBA{R: 60, G: 232, B: 120, A: 255}
	case s >= 0.8:
		return color.NRGBA{R: 150, G: 230, B: 90, A: 255}
	case s >= 0.7:
		return color.NRGBA{R: 240, G: 210, B: 80, A: 255}
	default:
		return color.NRGBA{R: 250, G: 160, B: 80, A: 255}
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ---------------------------------------------------------------------------
// icons
// ---------------------------------------------------------------------------

// iconFor returns the scaled icon for id, decoding + scaling at most once per
// id. When id is empty (or the caller supplies a different image for the same
// id) the image is scaled without caching.
func (r *Renderer) iconFor(id string, src image.Image, size int) *image.RGBA {
	if size < 1 {
		size = 1
	}
	r.requests++
	if id != "" {
		if img, ok := r.icons[id]; ok && img.Bounds().Dy() == size {
			return img
		}
	}
	scaled := scaleIcon(src, size)
	r.decodes++
	if id != "" {
		r.icons[id] = scaled
	}
	return scaled
}

// scaleIcon fits src into a size x size box preserving the aspect ratio and
// centres it on a transparent background, using a CatmullRom kernel.
func scaleIcon(src image.Image, size int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	if src == nil {
		// A neutral placeholder: a soft grey disc, so a missing icon is visible
		// but obviously "no icon".
		drawCircle(dst, size/2, size/2, size/2-1, color.NRGBA{R: 90, G: 96, B: 110, A: 200})
		return dst
	}
	sb := src.Bounds()
	if sb.Dx() <= 0 || sb.Dy() <= 0 {
		return dst
	}
	scale := float64(size) / float64(sb.Dx())
	if s2 := float64(size) / float64(sb.Dy()); s2 < scale {
		scale = s2
	}
	w := int(float64(sb.Dx())*scale + 0.5)
	h := int(float64(sb.Dy())*scale + 0.5)
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	if w > size {
		w = size
	}
	if h > size {
		h = size
	}
	box := image.Rect((size-w)/2, (size-h)/2, (size-w)/2+w, (size-h)/2+h)
	xdraw.CatmullRom.Scale(dst, box, src, sb, draw.Over, nil)
	return dst
}

// ---------------------------------------------------------------------------
// font loading
// ---------------------------------------------------------------------------

// fontCandidates are tried in order. msyh.ttc (Microsoft YaHei) is the normal
// Chinese UI font; simhei.ttf is the documented fallback; the last entry is the
// embedded bitmap font that always works but only has ASCII.
var fontCandidates = []string{
	`C:\Windows\Fonts\msyh.ttc`,
	`C:\Windows\Fonts\msyh.ttf`,
	`C:\Windows\Fonts\msyhbd.ttc`,
	`C:\Windows\Fonts\simhei.ttf`,
	`C:\Windows\Fonts\simsun.ttc`,
	`C:\Windows\Fonts\Deng.ttf`,
}

func fontSizeFor(iconSize int) float64 {
	px := float64(iconSize) * 0.20
	if px < 9 {
		px = 9
	}
	if px > 28 {
		px = 28
	}
	return px
}

// loadUIFace returns a face, the name of the font actually used, and whether
// only ASCII glyphs are available.
func loadUIFace(size float64) (font.Face, string, bool, error) {
	if size < 6 {
		size = 6
	}
	opts := &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull}
	var firstErr error
	for _, path := range fontCandidates {
		raw, err := os.ReadFile(path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		f, err := parseFontFile(raw, filepath.Base(path))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		face, err := opentype.NewFace(f, opts)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return face, filepath.Base(path), false, nil
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("没有可用的系统中文字体")
	}
	return basicfont.Face7x13, "basicfont.Face7x13（降级：仅 ASCII）", true, nil
}

// parseFontFile understands both TrueType collections (.ttc, e.g. msyh.ttc)
// and single fonts (.ttf, e.g. simhei.ttf).
func parseFontFile(raw []byte, base string) (*sfnt.Font, error) {
	if f, err := sfnt.ParseCollection(raw); err == nil {
		if n := f.NumFonts(); n > 0 {
			return f.Font(0)
		}
	}
	f, err := sfnt.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("解析字体 %s 失败: %w", base, err)
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// icon store helpers
// ---------------------------------------------------------------------------

// DecodeIconPNG decodes a library icon (128x128 PNG) into an image.
func DecodeIconPNG(b []byte) (image.Image, error) {
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("解码图标 PNG 失败: %w", err)
	}
	return img, nil
}
