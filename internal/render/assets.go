package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"examforge/internal/ir"
)

// AssetManifestFile names the manifest inside an asset root. It maps
// asset_id -> path relative to the root and is the only way an ID becomes a
// file: the IR uri is never used to locate content.
const AssetManifestFile = "manifest.json"

const maxAssetManifestBytes = 1 << 20

// assetIDRe is the only accepted shape of an asset ID. It rejects empty values,
// separators, "."/".." segments, absolute paths, NUL, backslashes and any other
// character outside the schema pattern.
var assetIDRe = regexp.MustCompile(`^ast_[A-Za-z0-9_-]+$`)

// validateAssetID reports whether id is a well-formed, path-safe asset ID.
func validateAssetID(id string) error {
	if !assetIDRe.MatchString(id) {
		return fmt.Errorf("asset ID %q is not a valid ID (must match %s)", id, assetIDRe.String())
	}
	return nil
}

// LocalAssetResolver serves assets from a caller-designated root directory
// through its manifest. It is the slice 1 resolver; object storage adapters
// implement AssetResolver the same way and keep the root private.
type LocalAssetResolver struct {
	root     string // absolute, symlink-resolved
	manifest map[string]string
}

// NewLocalAssetResolver loads <root>/manifest.json. The manifest, not the IR
// uri, decides which file an asset ID refers to.
func NewLocalAssetResolver(root string) (*LocalAssetResolver, error) {
	resolved, err := canonicalDir(root)
	if err != nil {
		return nil, fmt.Errorf("%w: asset root is unusable: %v", ErrAsset, pathOnly(err))
	}
	data, err := os.ReadFile(filepath.Join(resolved, AssetManifestFile))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s in the asset root: %v", ErrAsset, AssetManifestFile, pathOnly(err))
	}
	if len(data) > maxAssetManifestBytes {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrAsset, AssetManifestFile, maxAssetManifestBytes)
	}
	manifest := map[string]string{}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("%w: %s is not a JSON object of asset IDs to paths: %v", ErrAsset, AssetManifestFile, err)
	}
	for id, rel := range manifest {
		if err := validateAssetID(id); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrAsset, AssetManifestFile, err)
		}
		if err := validateManifestPath(rel); err != nil {
			return nil, fmt.Errorf("%w: %s entry %q: %v", ErrAsset, AssetManifestFile, id, err)
		}
	}
	return &LocalAssetResolver{root: resolved, manifest: manifest}, nil
}

// validateManifestPath accepts only clean relative slash paths inside the root.
func validateManifestPath(rel string) error {
	switch {
	case rel == "" || strings.ContainsRune(rel, 0):
		return errors.New("path is empty or contains NUL")
	case strings.Contains(rel, `\`):
		return errors.New("path contains a backslash separator")
	case strings.HasPrefix(rel, "/") || filepath.IsAbs(rel):
		return errors.New("path is absolute")
	case path.Clean(rel) != rel || strings.HasPrefix(rel, "../") || rel == "..":
		return errors.New("path is not a clean relative path inside the root")
	}
	return nil
}

// Resolve opens the asset for assetID. The candidate is resolved and checked
// to be strictly inside the root before and after opening.
func (r *LocalAssetResolver) Resolve(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("resolve asset %q: %w", assetID, err)
	}
	if err := validateAssetID(assetID); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAsset, err)
	}
	rel, ok := r.manifest[assetID]
	if !ok {
		return nil, fmt.Errorf("%w: asset %q is not listed in %s", ErrAsset, assetID, AssetManifestFile)
	}
	// Resolve every symlink first and open the resolved path, so a link cannot
	// be swapped for one outside the root between the check and the open.
	resolved, err := filepath.EvalSymlinks(filepath.Join(r.root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("%w: asset %q: cannot resolve in the asset root: %v", ErrAsset, assetID, pathOnly(err))
	}
	if _, err := relUnder(r.root, resolved); err != nil {
		return nil, fmt.Errorf("%w: asset %q resolves outside the asset root", ErrAsset, assetID)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("%w: asset %q: cannot open: %v", ErrAsset, assetID, pathOnly(err))
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%w: asset %q is not a regular file", ErrAsset, assetID)
	}
	if target, lerr := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd()))); lerr == nil {
		// Linux: verify the file this process actually opened is inside the root.
		if _, rerr := relUnder(r.root, strings.TrimSuffix(filepath.Clean(target), " (deleted)")); rerr != nil {
			f.Close()
			return nil, fmt.Errorf("%w: asset %q opened outside the asset root", ErrAsset, assetID)
		}
	}
	return f, nil
}

// copyHash copies src into dst and returns the SHA-256 of the copied bytes. It
// follows the context and fails with ErrAssetTooLarge once limit is exceeded.
func copyHash(ctx context.Context, dst io.Writer, src io.Reader, limit int64) ([]byte, error) {
	buf := make([]byte, 64<<10)
	sum := sha256.New()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("copy asset: %w", err)
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > limit {
				return nil, fmt.Errorf("%w: limit is %d bytes", ErrAssetTooLarge, limit)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return nil, fmt.Errorf("write asset into workspace: %w", werr)
			}
			sum.Write(buf[:n])
		}
		if errors.Is(rerr, io.EOF) {
			return sum.Sum(nil), nil
		}
		if rerr != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, fmt.Errorf("copy asset: %w", cerr)
			}
			return nil, fmt.Errorf("read asset: %v", rerr)
		}
	}
}

// validateImageContent checks the bytes against the declared media type before
// Typst sees them. Raster images are limited by decoded pixel count, not just
// file size, which rejects decompression bombs without decoding the pixels.
func validateImageContent(mediaType string, data []byte, maxPixels int) error {
	ext, err := imageExt(mediaType)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAsset, err)
	}
	if ext == "svg" {
		if len(data) > maxSVGBytes {
			return fmt.Errorf("%w: SVG is %d bytes; limit is %d", ErrAssetTooLarge, len(data), maxSVGBytes)
		}
		if err := validateSVG(data); err != nil {
			return fmt.Errorf("%w: %v", ErrAsset, err)
		}
		return nil
	}
	if ext == "webp" {
		width, height, err := webpDimensions(data)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrAsset, err)
		}
		if int64(width)*int64(height) > int64(maxPixels) {
			return fmt.Errorf("%w: image is %dx%d pixels; limit is %d pixels", ErrAssetTooLarge, width, height, maxPixels)
		}
		return nil
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%w: decode image header: %v", ErrAsset, err)
	}
	wantFormat := map[string]string{"png": "png", "jpg": "jpeg", "gif": "gif"}[ext]
	if format != wantFormat {
		return fmt.Errorf("%w: declared %s but content is %s", ErrAsset, mediaType, format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > int64(maxPixels) {
		return fmt.Errorf("%w: image is %dx%d pixels; limit is %d pixels", ErrAssetTooLarge, cfg.Width, cfg.Height, maxPixels)
	}
	return nil
}

// webpDimensions reads the canvas size from the three WebP container variants
// without decoding pixel data.
func webpDimensions(data []byte) (int, int, error) {
	if len(data) < 30 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, errors.New("content is not a WebP image")
	}
	switch string(data[12:16]) {
	case "VP8 ":
		if data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return 0, 0, errors.New("invalid VP8 frame header")
		}
		w := int(data[26]) | int(data[27])<<8
		h := int(data[28]) | int(data[29])<<8
		return w & 0x3fff, h & 0x3fff, nil
	case "VP8L":
		if data[20] != 0x2f {
			return 0, 0, errors.New("invalid VP8L header")
		}
		b1, b2, b3, b4 := int(data[21]), int(data[22]), int(data[23]), int(data[24])
		w := 1 + (((b2 & 0x3f) << 8) | b1)
		h := 1 + (((b4 & 0x0f) << 10) | (b3 << 2) | ((b2 & 0xc0) >> 6))
		return w, h, nil
	case "VP8X":
		w := 1 + (int(data[24]) | int(data[25])<<8 | int(data[26])<<16)
		h := 1 + (int(data[27]) | int(data[28])<<8 | int(data[29])<<16)
		return w, h, nil
	}
	return 0, 0, errors.New("unsupported WebP chunk")
}

// validateSVG bounds the structural complexity Typst's SVG renderer may see.
func validateSVG(data []byte) error {
	lower := bytes.ToLower(data)
	if bytes.Contains(lower, []byte("<!doctype")) {
		return errors.New("SVG must not contain a DOCTYPE")
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	depth, elements, sawRoot := 0, 0, false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid SVG XML: %v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !sawRoot {
				sawRoot = t.Name.Local == "svg"
			}
			depth++
			elements++
			if depth > 256 {
				return errors.New("SVG nesting exceeds 256 levels")
			}
			if elements > 100_000 {
				return errors.New("SVG has more than 100000 elements")
			}
		case xml.EndElement:
			depth--
		}
	}
	if !sawRoot {
		return errors.New("SVG root element is missing")
	}
	return nil
}

// imageExt maps an image media type to the workspace file extension and
// rejects types Typst cannot load.
func imageExt(mediaType string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0])) {
	case "image/png":
		return "png", nil
	case "image/jpeg":
		return "jpg", nil
	case "image/gif":
		return "gif", nil
	case "image/svg+xml":
		return "svg", nil
	case "image/webp":
		return "webp", nil
	}
	return "", fmt.Errorf("media type %q is not a supported image type", mediaType)
}
