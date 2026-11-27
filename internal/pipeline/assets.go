package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"examforge/internal/storage"
)

// MaxAssetPixels bounds the decoded pixel count of a raster asset, matching
// the renderer's own limit.
const MaxAssetPixels = 40 << 20

// validateAssetBytes checks that the bytes are the declared image type before
// they are stored, so the renderer never receives content that does not match
// its declared media type.
func validateAssetBytes(mediaType string, data []byte) error {
	ext, err := storage.ExtForMediaType(mediaType)
	if err != nil {
		return err
	}
	switch ext {
	case "svg":
		if len(data) == 0 || !bytes.Contains(data, []byte("<svg")) {
			return errors.New("content is not an SVG document")
		}
		return nil
	case "webp":
		if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			return errors.New("content is not a WebP image")
		}
		return nil
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("decode image header: %v", err)
	}
	want := map[string]string{"png": "png", "jpg": "jpeg", "gif": "gif"}[ext]
	if format != want {
		return fmt.Errorf("declared %s but content is %s", mediaType, format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxAssetPixels {
		return fmt.Errorf("image is %dx%d pixels; limit is %d", cfg.Width, cfg.Height, MaxAssetPixels)
	}
	return nil
}
