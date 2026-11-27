package raster

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
)

// mmPerInch is the fixed unit conversion; 25.4 mm per inch everywhere.
const mmPerInch = 25.4

// CropConfig bounds and shapes one crop.
type CropConfig struct {
	// PaddingMM expands the bbox on every side before clamping, converted to
	// output pixels through the DPI. Default 0.
	PaddingMM float64
	// MaxContentWidthMM caps SuggestedWidthMM (the layout's text column).
	// Default 160.
	MaxContentWidthMM float64
	// MaxPixels bounds the crop's pixel count. Default DefaultMaxPagePixels.
	MaxPixels int
}

// CropResult is one cropped raw asset image and its measured geometry.
type CropResult struct {
	PNG              []byte
	SHA256           string
	Width, Height    int
	AspectRatio      float64
	SuggestedWidthMM float64
}

// CropPNG cuts rect ([x1,y1,x2,y2], normalized corner coordinates) out of a
// high-resolution page image. Coordinates are multiplied by the page's pixel
// size and rounded outward so edge content is never swallowed; padding is
// added first, then the rectangle is clamped to the image bounds. An invalid
// order, NaN/Inf, out-of-range coordinate or empty final rectangle is an
// error. The PNG digest is the raw crop digest.
func CropPNG(page image.Image, rect [4]float64, dpi int, cfg CropConfig) (CropResult, error) {
	if dpi <= 0 || dpi > MaxDPI {
		return CropResult{}, fmt.Errorf("raster: crop DPI %d is outside 1..%d", dpi, MaxDPI)
	}
	if cfg.MaxPixels < 0 || cfg.PaddingMM < 0 {
		return CropResult{}, errors.New("raster: crop config values must not be negative")
	}
	if cfg.MaxPixels == 0 {
		cfg.MaxPixels = DefaultMaxPagePixels
	}
	if cfg.MaxContentWidthMM <= 0 {
		cfg.MaxContentWidthMM = 160
	}
	if math.IsNaN(cfg.PaddingMM) || math.IsInf(cfg.PaddingMM, 0) ||
		math.IsNaN(cfg.MaxContentWidthMM) || math.IsInf(cfg.MaxContentWidthMM, 0) {
		return CropResult{}, errors.New("raster: crop config is not finite")
	}
	for _, v := range rect {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return CropResult{}, fmt.Errorf("raster: bbox coordinate %v is outside [0,1]", v)
		}
	}
	if rect[0] >= rect[2] || rect[1] >= rect[3] {
		return CropResult{}, fmt.Errorf("raster: bbox corners are not ordered: %v", rect)
	}
	b := page.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return CropResult{}, errors.New("raster: page image is empty")
	}
	pad := int(math.Ceil(cfg.PaddingMM / mmPerInch * float64(dpi)))
	x1 := int(math.Floor(rect[0]*float64(w))) - pad
	y1 := int(math.Floor(rect[1]*float64(h))) - pad
	x2 := int(math.Ceil(rect[2]*float64(w))) + pad
	y2 := int(math.Ceil(rect[3]*float64(h))) + pad
	x1, y1 = max(x1, 0), max(y1, 0)
	x2, y2 = min(x2, w), min(y2, h)
	if x1 >= x2 || y1 >= y2 {
		return CropResult{}, fmt.Errorf("raster: bbox %v maps to an empty rectangle on a %dx%d page", rect, w, h)
	}
	cw, ch := x2-x1, y2-y1
	if cw*ch > cfg.MaxPixels {
		return CropResult{}, fmt.Errorf("raster: crop is %dx%d pixels; limit %d", cw, ch, cfg.MaxPixels)
	}
	cropped := image.NewRGBA(image.Rect(0, 0, cw, ch))
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			cropped.Set(x, y, page.At(b.Min.X+x1+x, b.Min.Y+y1+y))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, cropped); err != nil {
		return CropResult{}, fmt.Errorf("raster: encode crop: %w", err)
	}
	data := buf.Bytes()
	if len(data) == 0 {
		return CropResult{}, errors.New("raster: crop encoded to zero bytes")
	}
	sum := sha256.Sum256(data)
	widthMM := float64(cw) / float64(dpi) * mmPerInch
	return CropResult{
		PNG:              data,
		SHA256:           hex.EncodeToString(sum[:]),
		Width:            cw,
		Height:           ch,
		AspectRatio:      float64(cw) / float64(ch),
		SuggestedWidthMM: math.Min(widthMM, cfg.MaxContentWidthMM),
	}, nil
}
