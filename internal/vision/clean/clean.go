// Package clean removes handwritten red/blue annotations and light gray
// watermarks from cropped exam images with pure Go pixel processing
// (image/color), never CGO or OpenCV. Printed dark content — text, thin
// geometric lines, dashed lines — is protected before any cleaning rule runs.
package clean

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io"
	"math"
)

// ImageCleaner cleans one image read from src.
type ImageCleaner interface {
	Clean(ctx context.Context, src io.Reader) (*CleanResult, error)
}

// CleanResult is the cleaned PNG, its digest and bounded warnings.
type CleanResult struct {
	PNG      []byte
	SHA256   string
	Warnings []string
}

// HSVConfig parameterizes the pixel classification. Hue ranges are degrees in
// [0,360); the red range may wrap through 0. Saturation and value thresholds
// are normalized [0,1].
type HSVConfig struct {
	// RedHueLow/RedHueHigh bound the red hue range; Low > High means the range
	// wraps through 0° (e.g. 340..20).
	RedHueLow, RedHueHigh float64
	// BlueHueLow/BlueHueHigh bound the blue hue range (no wrap needed but the
	// same circular rule applies).
	BlueHueLow, BlueHueHigh float64
	// SaturationMin is the minimum saturation for a pixel to count as a
	// colored annotation.
	SaturationMin float64
	// MarkValueMin is the minimum brightness of a colored annotation: very
	// dark saturated pixels stay protected as print.
	MarkValueMin float64
	// WatermarkSaturationMax/WatermarkValueMin classify light gray watermark
	// pixels: saturation at most Max and value at least Min.
	WatermarkSaturationMax float64
	WatermarkValueMin      float64
	// DarkProtectValueMax protects every pixel darker than this before any
	// cleaning rule: printed text and thin black lines survive.
	DarkProtectValueMax float64
	// MaxPixels bounds the decoded input; MaxInputBytes bounds the read.
	MaxPixels     int
	MaxInputBytes int64
}

// DefaultConfig is the acceptance configuration: red wraps 340°..20°, blue
// 190°..260°, saturated marks at or above the dark-protection ceiling,
// watermark gray brighter than 0.75 with saturation below 0.18, dark print
// protected below 0.35.
func DefaultConfig() HSVConfig {
	return HSVConfig{
		RedHueLow: 340, RedHueHigh: 20,
		BlueHueLow: 190, BlueHueHigh: 260,
		SaturationMin:          0.35,
		MarkValueMin:           0.35,
		WatermarkSaturationMax: 0.18,
		WatermarkValueMin:      0.75,
		DarkProtectValueMax:    0.35,
		MaxPixels:              40 << 20,
		MaxInputBytes:          32 << 20,
	}
}

// Validate rejects NaN/Inf, out-of-range and contradictory thresholds at
// construction time.
func (c HSVConfig) Validate() error {
	all := []float64{c.RedHueLow, c.RedHueHigh, c.BlueHueLow, c.BlueHueHigh,
		c.SaturationMin, c.MarkValueMin, c.WatermarkSaturationMax, c.WatermarkValueMin, c.DarkProtectValueMax}
	for _, v := range all {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("clean: threshold is not finite")
		}
	}
	for _, h := range []float64{c.RedHueLow, c.RedHueHigh, c.BlueHueLow, c.BlueHueHigh} {
		if h < 0 || h >= 360 {
			return fmt.Errorf("clean: hue %v is outside [0,360)", h)
		}
	}
	for _, v := range []float64{c.SaturationMin, c.MarkValueMin, c.WatermarkSaturationMax, c.WatermarkValueMin, c.DarkProtectValueMax} {
		if v < 0 || v > 1 {
			return fmt.Errorf("clean: threshold %v is outside [0,1]", v)
		}
	}
	if c.DarkProtectValueMax >= c.WatermarkValueMin {
		return errors.New("clean: dark protection must end below the watermark brightness floor")
	}
	if c.MarkValueMin < c.DarkProtectValueMax {
		return errors.New("clean: annotation brightness floor must not undercut dark protection")
	}
	if c.WatermarkSaturationMax >= c.SaturationMin {
		return errors.New("clean: watermark saturation ceiling must stay below the annotation saturation floor")
	}
	if c.MaxPixels <= 0 || c.MaxInputBytes <= 0 {
		return errors.New("clean: pixel and byte limits must be positive")
	}
	return nil
}

// HSVCleaner implements ImageCleaner with the configured thresholds. It
// replaces classified pixels with pure white and keeps everything else.
type HSVCleaner struct {
	cfg HSVConfig
}

var _ ImageCleaner = (*HSVCleaner)(nil)

// NewHSVCleaner validates the configuration and builds the cleaner.
func NewHSVCleaner(cfg HSVConfig) (*HSVCleaner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &HSVCleaner{cfg: cfg}, nil
}

// Clean decodes a PNG or JPEG (bounded), classifies every pixel and encodes
// the result as PNG. The context is checked per row during pixel processing.
// Transparent pixels become white: an exam crop has no meaningful alpha.
func (c *HSVCleaner) Clean(ctx context.Context, src io.Reader) (*CleanResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(src, c.cfg.MaxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("clean: read input: %w", err)
	}
	if int64(len(data)) > c.cfg.MaxInputBytes {
		return nil, fmt.Errorf("clean: input exceeds %d bytes", c.cfg.MaxInputBytes)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("clean: decode input: %w", err)
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 || b.Dx()*b.Dy() > c.cfg.MaxPixels {
		return nil, fmt.Errorf("clean: image is %dx%d pixels; limit %d", b.Dx(), b.Dy(), c.cfg.MaxPixels)
	}
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	removed := 0
	for y := 0; y < b.Dy(); y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			if a == 0 {
				out.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
				continue
			}
			// Composite over white so semi-transparency cannot smuggle a
			// colored mark past the classifier.
			r8, g8, b8 := overWhite(r, a), overWhite(g, a), overWhite(bl, a)
			h, s, v := rgbToHSV(r8, g8, b8)
			if c.classify(h, s, v) {
				out.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
				removed++
			} else {
				out.SetRGBA(x, y, color.RGBA{R: r8, G: g8, B: b8, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, fmt.Errorf("clean: encode output: %w", err)
	}
	pngBytes := buf.Bytes()
	sum := sha256.Sum256(pngBytes)
	result := &CleanResult{PNG: pngBytes, SHA256: hex.EncodeToString(sum[:])}
	if removed == 0 {
		result.Warnings = append(result.Warnings, "cleaner removed no pixels")
	}
	return result, nil
}

// classify applies the priority order: dark protection first, then colored
// annotations, then light gray watermark. Everything else is kept.
func (c *HSVCleaner) classify(h, s, v float64) bool {
	if v <= c.cfg.DarkProtectValueMax {
		return false // printed text, thin black geometry, dashed lines
	}
	if s >= c.cfg.SaturationMin && v >= c.cfg.MarkValueMin &&
		(hueInRange(h, c.cfg.RedHueLow, c.cfg.RedHueHigh) || hueInRange(h, c.cfg.BlueHueLow, c.cfg.BlueHueHigh)) {
		return true // red/blue handwritten annotation
	}
	if s <= c.cfg.WatermarkSaturationMax && v >= c.cfg.WatermarkValueMin && v < 1 {
		return true // light gray watermark / scan shading (pure white kept as white anyway)
	}
	return false
}

// hueInRange tests a circular hue interval; low > high wraps through 0°.
func hueInRange(h, low, high float64) bool {
	if low <= high {
		return h >= low && h <= high
	}
	return h >= low || h <= high
}

func overWhite(c16, a16 uint32) uint8 {
	// c16 and a16 are alpha-premultiplied 16-bit; composite over white.
	v := uint64(c16) + uint64(0xffff-a16)
	if v > 0xffff {
		v = 0xffff
	}
	return uint8(v >> 8)
}

// rgbToHSV converts 8-bit RGB to hue in degrees, saturation and value in
// [0,1]. Achromatic pixels get hue 0.
func rgbToHSV(r, g, b uint8) (h, s, v float64) {
	rf, gf, bf := float64(r)/255, float64(g)/255, float64(b)/255
	maxC := math.Max(rf, math.Max(gf, bf))
	minC := math.Min(rf, math.Min(gf, bf))
	v = maxC
	delta := maxC - minC
	if maxC > 0 {
		s = delta / maxC
	}
	if delta == 0 {
		return 0, s, v
	}
	switch maxC {
	case rf:
		h = 60 * math.Mod((gf-bf)/delta, 6)
	case gf:
		h = 60 * ((bf-rf)/delta + 2)
	default:
		h = 60 * ((rf-gf)/delta + 4)
	}
	if h < 0 {
		h += 360
	}
	return h, s, v
}
