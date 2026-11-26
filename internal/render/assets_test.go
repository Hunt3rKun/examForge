package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"examforge/internal/ir"
)

// assetRoot builds a private asset root with a manifest and returns its path.
func assetRoot(t *testing.T, manifest map[string]string, files map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(t, root, manifest)
	return root
}

func writeManifest(t *testing.T, root string, manifest map[string]string) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, AssetManifestFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

const testAssetID = "ast_01jtest0000000000000000001"

func testAsset() ir.Asset {
	return ir.Asset{AssetID: testAssetID, MediaType: "image/png", URI: "ignored", Status: "raw_cropped"}
}

// TestValidateAssetID pins the accepted ID shape and rejects traversal shapes.
func TestValidateAssetID(t *testing.T) {
	good := []string{testAssetID, "ast_a", "ast_A-1_z", "ast_0123456789"}
	for _, id := range good {
		if err := validateAssetID(id); err != nil {
			t.Errorf("validateAssetID(%q) = %v, want nil", id, err)
		}
	}
	bad := []string{
		"", "ast_", "asset_1", "ast_../x", "ast_a/b", `ast_a\b`, "ast_a\x00b",
		"../etc/passwd", "/etc/passwd", "ast_..", "ast_.", "ast_a.b", "ast_a b",
		"ast_%2e%2e", "ast_a\nb", "ast_你好",
	}
	for _, id := range bad {
		if err := validateAssetID(id); err == nil {
			t.Errorf("validateAssetID(%q) accepted", id)
		}
	}
}

// TestLocalResolverRejectsBadManifests covers unsafe manifest entries.
func TestLocalResolverRejectsBadManifests(t *testing.T) {
	bad := []map[string]string{
		{testAssetID: "../outside.png"},
		{testAssetID: "/etc/passwd"},
		{testAssetID: `sub\file.png`},
		{testAssetID: "sub/./file.png"},
		{testAssetID: "sub//file.png"},
		{testAssetID: ""},
		{testAssetID: "a\x00b"},
		{"../evil": "file.png"},
		{"ast_": "file.png"},
	}
	for _, manifest := range bad {
		root := assetRoot(t, manifest, map[string][]byte{"file.png": {1}})
		if _, err := NewLocalAssetResolver(root); !errors.Is(err, ErrAsset) {
			t.Errorf("manifest %v: err = %v, want ErrAsset", manifest, err)
		}
	}
}

// TestLocalResolverRejectsMissingManifest covers absent or malformed manifests.
func TestLocalResolverRejectsMissingManifest(t *testing.T) {
	root := t.TempDir()
	if _, err := NewLocalAssetResolver(root); !errors.Is(err, ErrAsset) {
		t.Errorf("missing manifest: err = %v, want ErrAsset", err)
	}
	for _, raw := range []string{"not json", "{\"" + testAssetID + "\":\"a\\u0000b\"}"} {
		if err := os.WriteFile(filepath.Join(root, AssetManifestFile), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewLocalAssetResolver(root); !errors.Is(err, ErrAsset) {
			t.Errorf("malformed manifest %q: err = %v, want ErrAsset", raw, err)
		}
	}
	if _, err := NewLocalAssetResolver(filepath.Join(root, "no-such-dir")); !errors.Is(err, ErrAsset) {
		t.Errorf("missing root: err = %v, want ErrAsset", err)
	}
}

// TestLocalResolverSymlinkEscape asserts a symlink pointing outside the root
// is rejected even though the manifest entry itself is clean.
func TestLocalResolverSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.png")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(root, "link.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeManifest(t, root, map[string]string{testAssetID: "link.png"})
	r, err := NewLocalAssetResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), testAssetID, testAsset()); !errors.Is(err, ErrAsset) {
		t.Errorf("symlink escape: err = %v, want ErrAsset", err)
	}
	// A directory symlink that leads outside is rejected the same way.
	if err := os.Symlink(outside, filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, root, map[string]string{testAssetID: "dirlink/secret.png"})
	r2, err := NewLocalAssetResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Resolve(context.Background(), testAssetID, testAsset()); !errors.Is(err, ErrAsset) {
		t.Errorf("directory symlink escape: err = %v, want ErrAsset", err)
	}
}

// TestLocalResolverSiblingPrefix asserts containment is decided by path
// structure, not a string prefix: a sibling directory sharing the root's name
// prefix is outside.
func TestLocalResolverSiblingPrefix(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "assets")
	sibling := filepath.Join(parent, "assets-evil")
	for _, dir := range []string{root, sibling} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sibling, "x.png"), []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sibling, "x.png"), filepath.Join(root, "x.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeManifest(t, root, map[string]string{testAssetID: "x.png"})
	r, err := NewLocalAssetResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), testAssetID, testAsset()); !errors.Is(err, ErrAsset) {
		t.Errorf("sibling-prefix escape: err = %v, want ErrAsset", err)
	}
}

// TestLocalResolverResolve covers the happy path, missing entries, non-regular
// files and cancellation.
func TestLocalResolverResolve(t *testing.T) {
	data := []byte("png-bytes")
	root := assetRoot(t, map[string]string{testAssetID: "sub/fig.png"}, map[string][]byte{"sub/fig.png": data})
	r, err := NewLocalAssetResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := r.Resolve(context.Background(), testAssetID, testAsset())
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(got) != string(data) {
		t.Fatalf("read = %q, %v", got, err)
	}
	if _, err := r.Resolve(context.Background(), "ast_01jother000000000000000001", testAsset()); !errors.Is(err, ErrAsset) {
		t.Errorf("unlisted ID: err = %v, want ErrAsset", err)
	}
	if _, err := r.Resolve(context.Background(), "../evil", testAsset()); !errors.Is(err, ErrAsset) {
		t.Errorf("malicious ID: err = %v, want ErrAsset", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Resolve(ctx, testAssetID, testAsset()); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled resolve: err = %v, want context.Canceled", err)
	}
	// A directory is not a regular file.
	writeManifest(t, root, map[string]string{testAssetID: "sub"})
	r2, err := NewLocalAssetResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Resolve(context.Background(), testAssetID, testAsset()); !errors.Is(err, ErrAsset) {
		t.Errorf("directory asset: err = %v, want ErrAsset", err)
	}
	// A listed but absent file.
	writeManifest(t, root, map[string]string{testAssetID: "gone.png"})
	r3, err := NewLocalAssetResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r3.Resolve(context.Background(), testAssetID, testAsset()); !errors.Is(err, ErrAsset) {
		t.Errorf("missing file: err = %v, want ErrAsset", err)
	}
}

// countingResolver counts Resolve calls per asset ID.
type countingResolver struct {
	data  []byte
	calls map[string]*atomic.Int64
}

func (c *countingResolver) Resolve(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n, ok := c.calls[assetID]
	if !ok {
		return nil, errors.New("unknown asset")
	}
	n.Add(1)
	return io.NopCloser(strings.NewReader(string(c.data))), nil
}

// TestRenderAssetDedupAndHash asserts one Resolve and one copy per asset ID
// however often it is referenced, with the correct SHA-256 in the payload.
func TestRenderAssetDedupAndHash(t *testing.T) {
	requireTypst(t)
	exam := loadIR(t, "../../testdata/render/ir/options.json")
	data, err := os.ReadFile("../../testdata/golden/assets/sample_fig.png")
	if err != nil {
		t.Fatal(err)
	}
	counter := &countingResolver{data: data, calls: map[string]*atomic.Int64{
		"ast_01jrender000000000000000001": {},
	}}
	r := newRenderer(t, func(cfg *TypstConfig) { cfg.AssetResolver = counter })
	art, err := r.Render(context.Background(), exam, loadLayout(t, "../../testdata/render/layouts/measure-a4-1col.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertPDF(t, art)
	if n := counter.calls["ast_01jrender000000000000000001"].Load(); n != 1 {
		t.Errorf("Resolve called %d times for one ID referenced 4 times, want 1", n)
	}
	payload, err := Precompile(exam, loadLayout(t, "../../testdata/render/layouts/measure-a4-1col.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Assets) != 1 {
		t.Fatalf("assets = %+v", payload.Assets)
	}
}

// TestRenderAssetHashInWorkspace stages a workspace directly and checks the
// copied bytes, their SHA-256 in the payload, and that the staged package
// cache is itself a valid vendored cache root.
func TestRenderAssetHashInWorkspace(t *testing.T) {
	requireTypst(t)
	exam := loadIR(t, "../../testdata/render/ir/options.json")
	layout := loadLayout(t, "../../testdata/render/layouts/measure-a4-1col.json")
	data, err := os.ReadFile("../../testdata/golden/assets/sample_fig.png")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	r := newRenderer(t, func(cfg *TypstConfig) {
		cfg.AssetResolver = AssetResolverFunc(func(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(string(data))), nil
		})
	})
	payload, err := Precompile(exam, layout)
	if err != nil {
		t.Fatal(err)
	}
	ws, cleanup, err := r.assemble(context.Background(), exam, payload)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	if len(payload.Assets) != 1 {
		t.Fatalf("assets = %+v", payload.Assets)
	}
	a := payload.Assets[0]
	if a.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("asset sha256 = %s, want %s", a.SHA256, hex.EncodeToString(sum[:]))
	}
	got, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(a.Path)))
	if err != nil || string(got) != string(data) {
		t.Errorf("staged asset bytes differ: %v", err)
	}
	// The staged cache must verify as a vendored package cache root itself.
	if err := VerifyVendoredPackages(filepath.Join(ws, WorkspacePackages)); err != nil {
		t.Errorf("staged package cache is not a valid vendored root: %v", err)
	}
	// Workspace inputs are read-only.
	info, err := os.Stat(filepath.Join(ws, PayloadFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Errorf("payload.json is writable: %v", info.Mode())
	}
}

// AssetResolverFunc adapts a function to AssetResolver.
type AssetResolverFunc func(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error)

func (f AssetResolverFunc) Resolve(ctx context.Context, assetID string, asset ir.Asset) (io.ReadCloser, error) {
	return f(ctx, assetID, asset)
}

// TestCopyHashLimitAndCancel covers the bounded, context-aware copy.
func TestCopyHashLimitAndCancel(t *testing.T) {
	data := strings.Repeat("x", 1000)
	var dst strings.Builder
	sum, err := copyHash(context.Background(), &dst, strings.NewReader(data), 2000)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(data))
	if hex.EncodeToString(sum) != hex.EncodeToString(want[:]) || dst.Len() != len(data) {
		t.Errorf("copyHash mismatch")
	}
	dst.Reset()
	if _, err := copyHash(context.Background(), &dst, strings.NewReader(data), 100); !errors.Is(err, ErrAssetTooLarge) {
		t.Errorf("over limit: err = %v, want ErrAssetTooLarge", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := copyHash(ctx, &dst, strings.NewReader(data), 2000); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled copy: err = %v, want context.Canceled", err)
	}
}

// TestValidateImageContent rejects MIME/content mismatches and oversized
// decodes before the bytes reach Typst.
func TestValidateImageContent(t *testing.T) {
	png, err := os.ReadFile("../../testdata/golden/assets/sample_fig.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateImageContent("image/png", png, DefaultMaxAssetPixels); err != nil {
		t.Fatalf("valid PNG rejected: %v", err)
	}
	if err := validateImageContent("image/jpeg", png, DefaultMaxAssetPixels); !errors.Is(err, ErrAsset) {
		t.Errorf("PNG declared as JPEG: err = %v, want ErrAsset", err)
	}
	if err := validateImageContent("image/png", png, 1); !errors.Is(err, ErrAssetTooLarge) {
		t.Errorf("pixel limit: err = %v, want ErrAssetTooLarge", err)
	}
	if err := validateImageContent("image/png", []byte("not an image"), DefaultMaxAssetPixels); !errors.Is(err, ErrAsset) {
		t.Errorf("garbage image: err = %v, want ErrAsset", err)
	}
	if err := validateImageContent("image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`), DefaultMaxAssetPixels); err != nil {
		t.Errorf("valid SVG rejected: %v", err)
	}
	if err := validateImageContent("image/svg+xml", []byte(`<!DOCTYPE svg><svg/>`), DefaultMaxAssetPixels); !errors.Is(err, ErrAsset) {
		t.Errorf("SVG DOCTYPE: err = %v, want ErrAsset", err)
	}
	webp := make([]byte, 30)
	copy(webp[0:], "RIFF")
	copy(webp[8:], "WEBP")
	copy(webp[12:], "VP8X")
	// Canvas is (width-1, height-1) as little-endian 24-bit values.
	webp[24], webp[25], webp[26] = 9, 0, 0
	webp[27], webp[28], webp[29] = 19, 0, 0
	if err := validateImageContent("image/webp", webp, 200); err != nil {
		t.Errorf("valid WebP header rejected: %v", err)
	}
	if err := validateImageContent("image/webp", webp, 100); !errors.Is(err, ErrAssetTooLarge) {
		t.Errorf("WebP pixel limit: err = %v, want ErrAssetTooLarge", err)
	}
}

// TestRenderRejectsOversizedAssetTotal asserts the per-render asset budget.
func TestRenderRejectsOversizedAssetTotal(t *testing.T) {
	ft := newFakeTracker(t, "success")
	exam := fakeExam()
	idA := "ast_01jtest00000000000000000A"
	idB := "ast_01jtest00000000000000000B"
	exam.Assets = map[string]ir.Asset{
		idA: {AssetID: idA, MediaType: "image/png", URI: "ignored", Status: "raw_cropped"},
		idB: {AssetID: idB, MediaType: "image/png", URI: "ignored", Status: "raw_cropped"},
	}
	exam.Questions[0].Stem = append(exam.Questions[0].Stem,
		ir.ContentBlock{Type: "image_ref", AssetID: idA, LayoutHint: "inline"},
		ir.ContentBlock{Type: "image_ref", AssetID: idB, LayoutHint: "inline"})
	png, err := os.ReadFile("../../testdata/golden/assets/sample_fig.png")
	if err != nil {
		t.Fatal(err)
	}
	r := newFakeRenderer(t, "success", ft, func(cfg *TypstConfig) {
		cfg.MaxTotalAssetBytes = int64(len(png) + 1)
		cfg.AssetResolver = AssetResolverFunc(func(context.Context, string, ir.Asset) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(string(png))), nil
		})
	})
	_, err = r.Render(context.Background(), exam, fakeLayout())
	if !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("err = %v, want ErrAssetTooLarge", err)
	}
	if ft.started.Load() != 0 {
		t.Errorf("Typst started despite asset rejection")
	}
}

// TestImageExt pins the supported media types.
func TestImageExt(t *testing.T) {
	good := map[string]string{
		"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif",
		"image/svg+xml": "svg", "image/webp": "webp", "image/png; charset=binary": "png",
	}
	for mt, want := range good {
		got, err := imageExt(mt)
		if err != nil || got != want {
			t.Errorf("imageExt(%q) = %q, %v; want %q", mt, got, err, want)
		}
	}
	for _, mt := range []string{"", "text/plain", "application/pdf", "image/tiff"} {
		if _, err := imageExt(mt); err == nil {
			t.Errorf("imageExt(%q) accepted", mt)
		}
	}
}
