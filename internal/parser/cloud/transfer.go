package cloud

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"math"

	"examforge/internal/parser"
)

// transferJPEG builds the bounded VLM transfer copy of one high-resolution
// page: proportional downscale so the long edge is at most maxLongEdge, then
// JPEG at the configured quality with a hard byte ceiling. The original page
// image is never modified; final crops always use the original.
func transferJPEG(page image.Image, maxLongEdge, quality, maxBytes int) ([]byte, error) {
	b := page.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, parser.Permanent("raster_invalid", fmt.Errorf("page image is empty"))
	}
	long := max(w, h)
	if long > maxLongEdge {
		scale := float64(maxLongEdge) / float64(long)
		w = max(1, int(math.Round(float64(w)*scale)))
		h = max(1, int(math.Round(float64(h)*scale)))
	}
	scaled := image.NewRGBA(image.Rect(0, 0, w, h))
	// Nearest-neighbor sampling: deterministic, no external dependency, and
	// adequate for a transfer copy whose only job is legibility.
	for y := 0; y < h; y++ {
		sy := b.Min.Y + y*b.Dy()/h
		for x := 0; x < w; x++ {
			sx := b.Min.X + x*b.Dx()/w
			scaled.Set(x, y, page.At(sx, sy))
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: quality}); err != nil {
		return nil, parser.Permanent("raster_invalid", fmt.Errorf("encode transfer JPEG: %w", err))
	}
	if buf.Len() > maxBytes {
		return nil, parser.Permanent("transfer_too_large", fmt.Errorf("transfer JPEG is %d bytes; limit %d", buf.Len(), maxBytes))
	}
	return buf.Bytes(), nil
}
