package render

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
