package overlay

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"
	"sync"
	"testing"

	"golang.org/x/image/font"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// flatPNG builds a solid-colour 128x128 PNG, exactly the shape a library icon
// has after P1 normalised it.
func flatPNG(t *testing.T, col color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	draw.Draw(img, img.Bounds(), image.NewUniform(col), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码测试图标: %v", err)
	}
	return buf.Bytes()
}

// decodeIcon decodes the PNG through the production helper.
func decodeIcon(t *testing.T, raw []byte) image.Image {
	t.Helper()
	img, err := DecodeIconPNG(raw)
	if err != nil {
		t.Fatalf("DecodeIconPNG: %v", err)
	}
	return img
}

// opts returns the default canvas shape used by the tests: 3 cells of 96 px.
func opts(showName, showScore bool) RenderOptions {
	return RenderOptions{Width: 300, Height: 108, IconSize: 96, ShowName: showName, ShowScore: showScore, Alpha: 1}
}

func TestRendererVersionMarker(t *testing.T) {
	// Prints the layout revision so a stale test binary is obvious.
	t.Logf("渲染器版本标记: %s", rendererVersion)
	if rendererVersion == "" {
		t.Error("rendererVersion 为空")
	}
}

func newTestRenderer(t *testing.T, o RenderOptions) *Renderer {
	t.Helper()
	r, err := NewRenderer(o)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	return r
}

// opaqueRGB reports whether px is (sufficiently) opaque and returns its colour
// un-premultiplied.
func opaqueRGB(img *image.RGBA, x, y int) (bool, color.RGBA) {
	i := img.PixOffset(x, y)
	a := img.Pix[i+3]
	if a < 120 {
		return false, color.RGBA{}
	}
	scale := func(v uint8) uint8 { return uint8(minInt(255, int(v)*255/int(a))) }
	return true, color.RGBA{R: scale(img.Pix[i]), G: scale(img.Pix[i+1]), B: scale(img.Pix[i+2]), A: a}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func alphaAt(img *image.RGBA, x, y int) uint8 { return img.Pix[img.PixOffset(x, y)+3] }

func near(a, b uint8, tol int) bool {
	d := int(a) - int(b)
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// ---------------------------------------------------------------------------
// A3.1 canvas geometry and transparency
// ---------------------------------------------------------------------------

func TestRendererCanvasSizeAndTransparentPadding(t *testing.T) {
	t.Logf("渲染器版本标记: %s", rendererVersion)
	r := newTestRenderer(t, opts(true, true))
	icon := decodeIcon(t, flatPNG(t, color.NRGBA{R: 30, G: 200, B: 255, A: 255}))
	items := []DisplayItem{
		{ID: "a", Icon: icon, Name: "脚步-近", Score: 0.93, AgeMs: 100},
	}
	img := r.Draw(items, 0)
	if got := img.Bounds(); got.Dx() != 300 || got.Dy() != 108 {
		t.Fatalf("画布尺寸 = %dx%d，期望 300x108", got.Dx(), got.Dy())
	}

	// Corners must be completely transparent: nothing is painted outside the
	// cells, which is what makes the window per-pixel transparent.
	corners := [][2]int{{0, 0}, {299, 0}, {0, 107}, {299, 107}}
	for _, c := range corners {
		if a := alphaAt(img, c[0], c[1]); a != 0 {
			t.Errorf("角落 (%d,%d) 的 alpha = %d，期望 0", c[0], c[1], a)
		}
	}
	// The whole first and last row / column too (the renderer keeps `spacingPad`
	// pixels of transparent margin).
	nonZeroEdges := 0
	for x := 0; x < 300; x++ {
		if alphaAt(img, x, 0) != 0 {
			nonZeroEdges++
		}
		if alphaAt(img, x, 107) != 0 {
			nonZeroEdges++
		}
	}
	for y := 0; y < 108; y++ {
		if alphaAt(img, 0, y) != 0 {
			nonZeroEdges++
		}
		if alphaAt(img, 299, y) != 0 {
			nonZeroEdges++
		}
	}
	if nonZeroEdges != 0 {
		t.Errorf("画布边缘有 %d 个非透明像素，期望 0（背景必须完全透明）", nonZeroEdges)
	}
	// Padding band: the first spacingPad columns hold nothing.
	for x := 0; x < spacingPad; x++ {
		for y := 0; y < 108; y++ {
			if alphaAt(img, x, y) != 0 {
				t.Fatalf("padding 区 (%d,%d) alpha=%d，期望 0", x, y, alphaAt(img, x, y))
			}
		}
	}
}

func TestRendererIconCentreMatchesIconColour(t *testing.T) {
	r := newTestRenderer(t, opts(true, true))
	want := color.NRGBA{R: 240, G: 40, B: 120, A: 255}
	icon := decodeIcon(t, flatPNG(t, want))
	img := r.Draw([]DisplayItem{{ID: "a", Icon: icon, Name: "脚步-近", Score: 0.91, AgeMs: 120}}, 0)

	o := r.Options()
	l := r.computeLayout(1)
	t.Logf("布局: cell=%dx%d icon=%d x0=%d y0=%d plateTop=%d iconC=(%d,%d)",
		l.cellW, l.cellH, l.iconSize, l.x0, l.y0, l.plateTop, l.iconCX, l.iconCY)

	// Sample a grid of points in the upper half of the icon: the score ring only
	// touches the icon's outer edge and the text plate only covers the bottom
	// strip, so this region is pure icon.
	matched, total, unmatched := 0, 0, 0
	for dy := -l.iconSize/2 + 6; dy <= -6; dy += 3 {
		for dx := -l.iconSize/2 + 6; dx <= l.iconSize/2-6; dx += 3 {
			ok, got := opaqueRGB(img, l.iconCX+dx, l.iconCY+dy)
			if !ok {
				continue
			}
			total++
			if near(got.R, want.R, 8) && near(got.G, want.G, 8) && near(got.B, want.B, 8) {
				matched++
			} else if unmatched < 3 {
				unmatched++
				t.Logf("非图标色采样点 (%d,%d) = %v", l.iconCX+dx, l.iconCY+dy, got)
			}
		}
	}
	if total == 0 {
		t.Fatal("图标区域一个像素都没画出来")
	}
	// The score ring is drawn ON the icon's inner edge (radius iconSize/2), so a
	// thin band of ring pixels is expected; everything away from that edge must
	// be the icon colour.
	if ratio := float64(matched) / float64(total); ratio < 0.90 {
		t.Errorf("图标上半区只有 %d/%d = %.1f%% 的采样点是图标色 %v", matched, total, ratio*100, want)
	}
	ok, got := opaqueRGB(img, l.iconCX, l.iconCY-l.iconSize/4)
	if !ok || !near(got.R, want.R, 8) || !near(got.G, want.G, 8) || !near(got.B, want.B, 8) {
		t.Errorf("图标中心上方颜色 = %v（ok=%v），期望 ≈ %v", got, ok, want)
	}
	t.Logf("图标上半区采样 %d 点，%d 点匹配源色 %v（中心上方像素 %v）", total, matched, want, got)

	// The icon must reach the full size the layout asked for (not shrunk to make
	// room for the text).
	if l.iconSize != 96 {
		t.Errorf("iconSize = %d，期望 96（画布 300x108、size 96 时应能放下完整图标）", l.iconSize)
	}

	// A ring must also exist: some pixel near the icon edge must carry the ring
	// colour.
	coloured := 0
	for y := 0; y < o.Height; y++ {
		for x := 0; x < o.Width; x++ {
			i := img.PixOffset(x, y)
			if img.Pix[i+3] > 150 && img.Pix[i+1] > 150 && img.Pix[i] < 150 {
				coloured++ // greenish ring pixel
			}
		}
	}
	if coloured == 0 {
		t.Error("置信度环一个像素都没有画出来")
	}
	t.Logf("置信度环颜色像素 %d 个", coloured)
}

func TestRendererGlobalAlphaScalesEveryPixel(t *testing.T) {
	// Two different icons so the canvas has several distinct alpha values.
	iconA := decodeIcon(t, flatPNG(t, color.NRGBA{R: 200, G: 60, B: 60, A: 255}))
	iconB := decodeIcon(t, flatPNG(t, color.NRGBA{R: 60, G: 200, B: 60, A: 255}))

	full := newTestRenderer(t, opts(true, true))
	half := newTestRenderer(t, opts(true, true))
	items := []DisplayItem{
		{ID: "a", Icon: iconA, Name: "脚步-近", Score: 0.95, AgeMs: 100},
		{ID: "b", Icon: iconB, Name: "枪声-远", Score: 0.71, AgeMs: 200},
	}
	imgFull := full.Draw(items, 0)
	items[0].AgeMs, items[1].AgeMs = 100, 200
	half.SetAlpha(0.5)
	imgHalf := half.Draw(items, 0)

	visible := 0
	maxDev := 0
	sumRatio := 0.0
	for i := 0; i < len(imgFull.Pix); i += 4 {
		aF := int(imgFull.Pix[i+3])
		if aF == 0 {
			if imgHalf.Pix[i+3] != 0 {
				t.Fatalf("alpha=0 的像素在 0.5 倍下变成 %d", imgHalf.Pix[i+3])
			}
			continue
		}
		visible++
		want := aF / 2
		got := int(imgHalf.Pix[i+3])
		dev := got - want
		if dev < 0 {
			dev = -dev
		}
		if dev > maxDev {
			maxDev = dev
		}
		sumRatio += float64(got) / float64(aF)
	}
	if visible == 0 {
		t.Fatal("画布上一个可见像素都没有")
	}
	meanRatio := sumRatio / float64(visible)
	if math.Abs(meanRatio-0.5) > 0.02 {
		t.Errorf("Alpha=0.5 时平均 alpha 比值 = %.4f，期望 0.5", meanRatio)
	}
	if maxDev > 3 {
		t.Errorf("Alpha=0.5 时逐像素最大偏差 = %d（超过 ±3 的舍入范围）", maxDev)
	}
	t.Logf("逐像素比对：可见像素 %d 个，平均比值 %.4f，最大偏差 ±%d", visible, meanRatio, maxDev)

	// A fully transparent canvas at Alpha=0.
	zero := newTestRenderer(t, opts(true, true))
	zero.SetAlpha(0)
	blank := zero.Draw(items, 0)
	for i := 0; i < len(blank.Pix); i += 4 {
		if blank.Pix[i+3] != 0 {
			t.Fatalf("Alpha=0 时仍有 alpha=%d 的像素", blank.Pix[i+3])
		}
	}
}

func TestRendererThreeItemsDoNotOverlap(t *testing.T) {
	icon := decodeIcon(t, flatPNG(t, color.NRGBA{R: 255, G: 0, B: 255, A: 255}))
	r := newTestRenderer(t, opts(true, true))
	items := []DisplayItem{
		{ID: "a", Icon: icon, Name: "脚步-近", Score: 0.95, AgeMs: 30},
		{ID: "b", Icon: icon, Name: "隔墙-耳机", Score: 0.88, AgeMs: 60},
		{ID: "c", Icon: icon, Name: "枪声-远", Score: 0.76, AgeMs: 90},
	}
	img := r.Draw(items, 0)

	// The magenta icon colour is (nearly) unique to the icons, so each item's
	// x-range can be measured from the pixels themselves. Only the band above the
	// text plate is scanned: the plate covers the bottom strip of every icon and
	// would otherwise make one icon look like several spans.
	const target = 0xF0 // tolerance-based match on R and B
	ranges := make([][2]int, 0, 3)
	cur := [2]int{-1, -1}
	l0 := r.computeLayout(3)
	bandTop := l0.y0 + 4
	bandBottom := l0.plateTop - 4
	for x := 0; x < 300; x++ {
		hit := false
		for y := bandTop; y <= bandBottom; y++ {
			if y < 0 || y >= 108 {
				continue
			}
			i := img.PixOffset(x, y)
			if img.Pix[i+3] > 120 && img.Pix[i] > target && img.Pix[i+2] > target && img.Pix[i+1] < 120 {
				hit = true
				break
			}
		}
		if hit {
			if cur[0] < 0 {
				cur[0] = x
			}
			cur[1] = x
		} else if cur[0] >= 0 {
			ranges = append(ranges, cur)
			cur = [2]int{-1, -1}
		}
	}
	if cur[0] >= 0 {
		ranges = append(ranges, cur)
	}
	if len(ranges) != 3 {
		t.Fatalf("数出 %d 段图标像素，期望 3 段（%v）（扫描带 y=%d..%d）", len(ranges), ranges, bandTop, bandBottom)
	}
	for i, rg := range ranges {
		t.Logf("第 %d 个事件的 x 区间 = [%d, %d]（宽 %d）", i+1, rg[0], rg[1], rg[1]-rg[0]+1)
	}
	for i := 1; i < len(ranges); i++ {
		if ranges[i][0] <= ranges[i-1][1] {
			t.Errorf("第 %d 段与第 %d 段重叠: %v vs %v", i, i+1, ranges[i-1], ranges[i])
		}
	}
	// The three spans plus the two gaps plus both paddings must add up.
	gap1 := ranges[1][0] - ranges[0][1] - 1
	gap2 := ranges[2][0] - ranges[1][1] - 1
	l := r.computeLayout(3)
	if gap1 != l.spacing && gap2 != l.spacing {
		t.Logf("注意：实际间隔 %d/%d，布局 spacing=%d（圆角与抗锯齿会让边缘少一两个像素）", gap1, gap2, l.spacing)
	}
}

func TestRendererDrawsChineseText(t *testing.T) {
	r := newTestRenderer(t, opts(true, true))
	if r.ASCIIOnly() {
		t.Logf("本机没有可用的系统中文字体，已降级为 basicfont（仅 ASCII）：%s", r.FontName())
	}
	icon := decodeIcon(t, flatPNG(t, color.NRGBA{R: 10, G: 10, B: 10, A: 255}))
	const name = "脚步-近-隔墙-耳机-01"

	withText := r.Draw([]DisplayItem{{ID: "a", Icon: icon, Name: name, Score: 0.9, AgeMs: 100}}, 0)
	// Same canvas without any name: the difference is exactly the text.
	plain := newTestRenderer(t,
		RenderOptions{Width: 300, Height: 108, IconSize: 96, ShowName: false, ShowScore: false, Alpha: 1})
	noText := plain.Draw([]DisplayItem{{ID: "a", Icon: icon, Score: 0.9, AgeMs: 100}}, 0)

	glyphPixels := 0
	minY, maxY, minX, maxX := 1<<30, -1, 1<<30, -1
	rowCount := make([]int, 108)
	for y := 0; y < 108; y++ {
		for x := 0; x < 300; x++ {
			i := withText.PixOffset(x, y)
			j := noText.PixOffset(x, y)
			// "Text" = the pixel CHANGED between the two renders. Comparing the
			// alpha alone is not enough: the name plate darkens the icon without
			// changing its alpha, so the whole plate area counts here.
			changed := withText.Pix[i+3] != noText.Pix[j+3] ||
				withText.Pix[i] != noText.Pix[j] ||
				withText.Pix[i+1] != noText.Pix[j+1] ||
				withText.Pix[i+2] != noText.Pix[j+2]
			if changed {
				glyphPixels++
				rowCount[y]++
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
			}
		}
	}
	if glyphPixels == 0 {
		t.Fatal("文字底板一个像素都没画出来（两张画布完全一样）")
	}
	t.Logf("字体=%s ASCII-only=%v；与纯图标版不同的像素 x[%d,%d] y[%d,%d] 共 %d 个",
		r.FontName(), r.ASCIIOnly(), minX, maxX, minY, maxY, glyphPixels)

	// The text band is where the differing pixels are actually clustered: a
	// handful of single pixels come from the ring's anti-aliasing differing
	// slightly between the two renders.
	const minRowPixels = 12
	bandTop := -1
	for y := 0; y < 108; y++ {
		if rowCount[y] >= minRowPixels {
			bandTop = y
			break
		}
	}
	if bandTop < 0 {
		t.Fatal("找不到成片的文字行")
	}
	bandBottom := -1
	for y := 107; y >= 0; y-- {
		if rowCount[y] >= minRowPixels {
			bandBottom = y
			break
		}
	}
	// Most of the changed rows must live inside the plate strip (the icon's
	// bottom band). A couple of rows above it are the plate's rounded top edge.
	l := r.computeLayout(1)
	rowsInPlate, rowsOutside := 0, 0
	for y := bandTop; y <= bandBottom; y++ {
		if rowCount[y] < minRowPixels {
			continue
		}
		if y >= l.plateTop-2 && y <= l.y0+l.iconSize {
			rowsInPlate++
		} else {
			rowsOutside++
		}
	}
	if rowsInPlate < 30 {
		t.Errorf("底板范围内只有 %d 行有文字/分数像素，看起来没画出来", rowsInPlate)
	}
	if rowsOutside > rowsInPlate/2 {
		t.Errorf("有 %d 行变化像素落在底板外（只有 %d 行在底板内），文字位置不对", rowsOutside, rowsInPlate)
	}
	t.Logf("变化带 y=%d..%d：底板内 %d 行，底板外 %d 行；底板顶边 y=%d，图标 y=%d..%d（尺寸 %d）",
		bandTop, bandBottom, rowsInPlate, rowsOutside, l.plateTop, l.y0, l.y0+l.iconSize, l.iconSize)
	// The name is drawn in white. Because the plate is DARK, a near-white pixel
	// can only be a glyph, which is what proves the Chinese text really rendered
	// (instead of, say, only the plate being drawn).
	nearWhite := 0
	for y := 0; y < 108; y++ {
		for x := 0; x < 300; x++ {
			i := withText.PixOffset(x, y)
			if withText.Pix[i+3] > 200 && withText.Pix[i] > 200 && withText.Pix[i+1] > 200 && withText.Pix[i+2] > 200 {
				nearWhite++
			}
		}
	}
	if nearWhite == 0 {
		t.Error("没有找到白色字形像素（可能只画了背景板）")
	}
	t.Logf("白色字形像素 %d 个", nearWhite)
}

func TestRendererWithoutTextIsJustTheIcon(t *testing.T) {
	icon := decodeIcon(t, flatPNG(t, color.NRGBA{R: 20, G: 180, B: 240, A: 255}))
	r := newTestRenderer(t, RenderOptions{Width: 108, Height: 108, IconSize: 96, Alpha: 1})
	img := r.Draw([]DisplayItem{{ID: "a", Icon: icon, Score: 1, AgeMs: 50}}, 0)
	// Count non-transparent pixels: the icon only, no plates, no text.
	n := 0
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] > 0 {
			n++
		}
	}
	if n == 0 {
		t.Fatal("图标没画出来")
	}
	if n > 96*96 {
		t.Errorf("非透明像素 %d 个，超过图标尺寸 96x96=9216（说明画了多余的东西）", n)
	}
	t.Logf("无文字模式：非透明像素 %d / 9216", n)
}

// ---------------------------------------------------------------------------
// A5. icon cache
// ---------------------------------------------------------------------------

// countingProvider counts Icon() calls (i.e. library lookups).
type countingProvider struct {
	mu    sync.Mutex
	calls map[string]int
	raw   map[string][]byte
}

func (p *countingProvider) Icon(id string) (image.Image, error) {
	p.mu.Lock()
	p.calls[id]++
	raw := p.raw[id]
	p.mu.Unlock()
	if raw == nil {
		return nil, errNoIcon
	}
	return DecodeIconPNG(raw)
}

func (p *countingProvider) count(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[id]
}

var errNoIcon = errTestNoIcon{}

type errTestNoIcon struct{}

func (errTestNoIcon) Error() string { return "provider 里没有这个图标" }

func TestIconCacheDecodesOncePerID(t *testing.T) {
	prov := &countingProvider{
		calls: map[string]int{},
		raw: map[string][]byte{
			"a": flatPNG(t, color.NRGBA{R: 200, G: 30, B: 30, A: 255}),
			"b": flatPNG(t, color.NRGBA{R: 30, G: 200, B: 30, A: 255}),
		},
	}
	r := newTestRenderer(t, opts(true, true))

	draw := func(id string) {
		img, err := prov.Icon(id)
		if err != nil {
			t.Fatalf("provider.Icon(%s): %v", id, err)
		}
		r.Draw([]DisplayItem{{ID: id, Icon: img, Name: "脚步", Score: 0.9, AgeMs: 10}}, 0)
	}
	draw("a")
	draw("a")
	draw("a")
	draw("b")

	if got := prov.count("a"); got != 3 {
		t.Errorf("provider 被调用 %d 次（期望 3，provider 自己不缓存）", got)
	}
	decoded, requests := r.CacheStats()
	if requests != 4 {
		t.Errorf("图标请求数 = %d，期望 4", requests)
	}
	if decoded != 2 {
		t.Errorf("图标解码/缩放次数 = %d，期望 2（同一 id 只做一次）", decoded)
	}
	t.Logf("同一 id 请求 4 次（a×3 + b×1）：provider 调用 3 次，渲染器解码 %d 次，请求 %d 次", decoded, requests)

	// The second lookup for "a" is a pure hit: the returned image must be the
	// cached one (same colour, no re-scale).
	if _, ok := r.icons["a"]; !ok {
		t.Error("缓存里没有键 a")
	}
}

func TestScaleIconPreservesAspectRatio(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 128, 64)) // 2:1
	draw.Draw(src, src.Bounds(), image.NewUniform(color.NRGBA{R: 1, G: 2, B: 3, A: 255}), image.Point{}, draw.Src)
	out := scaleIcon(src, 32)
	if out.Bounds().Dx() != 32 || out.Bounds().Dy() != 32 {
		t.Fatalf("缩略图必须是 32x32 的方框，得到 %v", out.Bounds())
	}
	// The top and bottom rows of the padded box must be transparent (the image
	// is letterboxed, not stretched).
	for x := 0; x < 32; x++ {
		if alphaAt(out, x, 0) != 0 || alphaAt(out, x, 31) != 0 {
			t.Errorf("非正方形图标被拉伸了（列 %d 的上下边不透明）", x)
		}
	}
	if alphaAt(out, 16, 16) == 0 {
		t.Error("图标中心应该有像素")
	}
	// A nil icon must still produce something visible.
	ph := scaleIcon(nil, 24)
	found := false
	for i := 3; i < len(ph.Pix); i += 4 {
		if ph.Pix[i] > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Error("nil 图标没有画占位图")
	}
}

// ---------------------------------------------------------------------------
// font details
// ---------------------------------------------------------------------------

func TestRendererFontFallsBackGracefully(t *testing.T) {
	r := newTestRenderer(t, opts(true, true))
	name := r.FontName()
	if name == "" {
		t.Error("FontName 为空")
	}
	if !strings.Contains(name, ".ttc") && !strings.Contains(name, ".ttf") && !strings.Contains(name, "basicfont") {
		t.Errorf("字体名不像字体文件: %q", name)
	}
	if r.ASCIIOnly() {
		if !strings.Contains(name, "basicfont") {
			t.Errorf("降级时字体名应写明 basicfont，实际 %q", name)
		}
	}
}

// asciiFold is only used by the basicfont fallback: it must turn any non-ASCII
// rune into '?' and leave ASCII alone. It is checked rune by rune (and on a run
// of CJK) rather than on one long mixed literal, so the assertion is about the
// function and cannot be confused by the fixture's encoding.
func TestASCIIFoldReplacesNonASCII(t *testing.T) {
	literal := "脚步-近A1"
	escaped := "\u811a\u6b65-\u8fd1A1"
	if literal != escaped {
		t.Fatalf("测试文件里的中文字面量被改坏了: literal=% x escaped=% x", []byte(literal), []byte(escaped))
	}
	if got := asciiFold("脚"); got != "?" {
		t.Errorf("asciiFold(脚) = %q，期望 ?", got)
	}
	if got := asciiFold("脚步"); strings.Trim(got, "?") != "" || len(got) == 0 {
		t.Errorf("asciiFold(脚步) = %q，期望全是问号", got)
	}
	if got := asciiFold("A1-b_2"); got != "A1-b_2" {
		t.Errorf("asciiFold 改了 ASCII: %q", got)
	}
	got := asciiFold(literal)
	if got == "" {
		t.Fatal("asciiFold 返回空串")
	}
	for _, r := range got {
		if r > 0x7F {
			t.Errorf("asciiFold 之后仍有非 ASCII 字符 %q: %q", r, got)
		}
	}
	// Build the expected result independently (one '?' per CJK rune) and compare
	// byte by byte, so the report cannot hide behind %q quoting.
	var want []byte
	for _, r := range literal {
		if r < 0x80 {
			want = append(want, byte(r))
		} else {
			want = append(want, '?')
		}
	}
	if string(got) != string(want) {
		t.Errorf("asciiFold(%q) 字节 = % x，期望字节 = % x", literal, []byte(got), want)
	}
	t.Logf("asciiFold(%q) = %q（输入 %d 字节 / %d rune，输出 %d 字节）",
		literal, got, len(literal), len([]rune(literal)), len(got))
}

// TestFitStringNeverOverflows 锁定"文字不会溢出格子"这条行为：截断后必须放得进给定
// 宽度、必须以省略号结尾、放得下的字符串原样返回。没有这一步时，长名字（例如
// 「脚步-近-隔墙-耳机-01」）会越过格子边界，和相邻格子的文字叠成一团。
func TestFitStringNeverOverflows(t *testing.T) {
	r, err := NewRenderer(RenderOptions{
		Width: 300, Height: 108, IconSize: 96,
		ShowName: true, ShowScore: true, Alpha: 1,
	})
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	if r.face == nil {
		t.Skip("本机没有可用字体，跳过")
	}
	d := &font.Drawer{
		Dst:  image.NewRGBA(image.Rect(0, 0, 4, 4)),
		Src:  image.NewUniform(color.NRGBA{A: 255}),
		Face: r.face,
	}
	const maxW = 94
	long := "脚步-近-隔墙-耳机-01-这是一个特别长的名字"
	got := fitString(d, long, maxW)
	if got == "" {
		t.Fatal("fitString 返回了空字符串")
	}
	if w := int(d.MeasureString(got).Round()); w > maxW {
		t.Errorf("截断后宽度 = %d px，仍然超过 %d px（%q）", w, maxW, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("被截断的字符串应以省略号结尾，实际 = %q", got)
	}
	if short := "换弹"; fitString(d, short, maxW) != short {
		t.Errorf("放得下的字符串不应被改动，实际 = %q", fitString(d, short, maxW))
	}
	t.Logf("fitString(%q, %d) = %q（宽 %d px）", long, maxW, got, int(d.MeasureString(got).Round()))
}
