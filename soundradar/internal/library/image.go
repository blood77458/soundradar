package library

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"

	_ "image/jpeg" // decoding only; no encoder is needed
)

// ---------------------------------------------------------------------------
// icon normalisation
// ---------------------------------------------------------------------------

// IconFormat names accepted for the `icon` upload field.
var iconFormats = map[string]bool{
	"png":  true,
	"jpeg": true,
	"jpg":  true,
}

// ErrIconFormat reports an image container we refuse to handle.
var ErrIconFormat = errors.New("不支持的图片格式")

// ScaleIcon decodes an uploaded icon (PNG or JPEG) and returns a canonical
// 128x128 PNG. Aspect ratios other than 1:1 are centre-cropped first, then the
// square is resampled with a Catmull-Rom kernel (golang.org/x/image/draw).
func ScaleIcon(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("图标内容为空")
	}
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		// webp is optional in x/image, try it explicitly before giving up so the
		// error message can be accurate.
		if m, werr := webp.Decode(bytes.NewReader(raw)); werr == nil {
			img, format = m, "webp"
		} else {
			return nil, fmt.Errorf("无法解析图标文件（仅支持 PNG / JPEG）: %w", err)
		}
	}
	if !iconFormats[format] {
		return nil, fmt.Errorf("%w: %s（仅支持 PNG / JPEG）", ErrIconFormat, format)
	}
	return encodeIcon128(img)
}

// ScaleIconNamed is ScaleIcon with an extra file-name hint, so a file whose
// extension says png but whose bytes are something else still gets a decent
// error message.
func ScaleIconNamed(raw []byte, name string) ([]byte, error) {
	out, err := ScaleIcon(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

func encodeIcon128(src image.Image) ([]byte, error) {
	sq := centerSquare(toNRGBA(src))
	var scaled *image.NRGBA
	if sq.Bounds().Dx() == IconSize && sq.Bounds().Dy() == IconSize {
		scaled = sq
	} else {
		scaled = image.NewNRGBA(image.Rect(0, 0, IconSize, IconSize))
		draw.CatmullRom.Scale(scaled, scaled.Bounds(), sq, sq.Bounds(), draw.Over, nil)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, scaled); err != nil {
		return nil, fmt.Errorf("编码 icon.png: %w", err)
	}
	return buf.Bytes(), nil
}

// ScaleIconFit is like ScaleIcon but letterboxes the whole picture into the
// 128×128 canvas (transparent bars) instead of centre-cropping. Hint icons use
// this so the image matches the file the user uploaded.
func ScaleIconFit(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("图标内容为空")
	}
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		if m, werr := webp.Decode(bytes.NewReader(raw)); werr == nil {
			img, format = m, "webp"
		} else {
			return nil, fmt.Errorf("无法解析图标文件（仅支持 PNG / JPEG）: %w", err)
		}
	}
	if !iconFormats[format] {
		return nil, fmt.Errorf("%w: %s（仅支持 PNG / JPEG）", ErrIconFormat, format)
	}
	return encodeIconFit(img)
}

func encodeIconFit(src image.Image) ([]byte, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, errors.New("图标尺寸无效")
	}
	dst := image.NewNRGBA(image.Rect(0, 0, IconSize, IconSize))
	scale := float64(IconSize) / float64(w)
	if float64(IconSize)/float64(h) < scale {
		scale = float64(IconSize) / float64(h)
	}
	dw := int(float64(w)*scale + 0.5)
	dh := int(float64(h)*scale + 0.5)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	ox := (IconSize - dw) / 2
	oy := (IconSize - dh) / 2
	draw.CatmullRom.Scale(dst, image.Rect(ox, oy, ox+dw, oy+dh), src, b, draw.Over, nil)
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, fmt.Errorf("编码 icon.png: %w", err)
	}
	return buf.Bytes(), nil
}

// centerSquare crops img to the largest centred square.
func centerSquare(img *image.NRGBA) *image.NRGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == h {
		return img
	}
	side := w
	if h < side {
		side = h
	}
	ox := b.Min.X + (w-side)/2
	oy := b.Min.Y + (h-side)/2
	out := image.NewNRGBA(image.Rect(0, 0, side, side))
	for y := 0; y < side; y++ {
		srcRow := img.Pix[(oy+y-img.Rect.Min.Y)*img.Stride+(ox-img.Rect.Min.X)*4:]
		dstRow := out.Pix[y*out.Stride:]
		copy(dstRow, srcRow[:side*4])
	}
	return out
}

// IconInfo reports the dimensions of a canonical icon (used by tests).
func IconInfo(pngBytes []byte) (w, h int, err error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(pngBytes))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// ReadAllLimited reads at most limit bytes from r, reporting an error when the
// stream is longer (so a huge upload cannot exhaust memory silently).
func ReadAllLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("文件超过上限 %d 字节", limit)
	}
	return b, nil
}
