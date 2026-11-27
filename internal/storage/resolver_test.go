package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"examforge/internal/ir"
	"examforge/internal/render"
)

// staticLookup is a registry over fixed records.
type staticLookup struct {
	records map[string]AssetRecord
	err     error
	calls   int
	mu      sync.Mutex
}

func (l *staticLookup) AssetRecord(_ context.Context, assetID string) (AssetRecord, error) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	if l.err != nil {
		return AssetRecord{}, l.err
	}
	record, ok := l.records[assetID]
	if !ok {
		return AssetRecord{}, ErrNotFound
	}
	return record, nil
}

// seededStore returns a MemStore holding one registered asset.
func seededStore(t *testing.T, data []byte) (*MemStore, *staticLookup, AssetRecord) {
	t.Helper()
	id := "ast_01jexam0000000000000000001"
	store := NewMemStore()
	key, err := RawAssetKey(id, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), key, bytes.NewReader(data), int64(len(data)), "image/png"); err != nil {
		t.Fatal(err)
	}
	record := AssetRecord{AssetID: id, StorageKey: key, MediaType: "image/png", SHA256: HashBytes(data)}
	lookup := &staticLookup{records: map[string]AssetRecord{id: record}}
	return store, lookup, record
}

func TestResolverReadsOnlyTheRegisteredKey(t *testing.T) {
	ctx := context.Background()
	data := []byte("registered-png-bytes")
	store, lookup, record := seededStore(t, data)
	resolver, err := NewS3AssetResolver(store, lookup, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A hostile URI and raw_uri must not influence the read.
	asset := ir.Asset{
		AssetID:   record.AssetID,
		MediaType: "image/png",
		URI:       "../../etc/passwd",
		RawURI:    "s3://other-bucket/secret",
	}
	rc, err := resolver.Resolve(ctx, record.AssetID, asset)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("resolved %q, %v", got, err)
	}
	// No object outside the registered key exists.
	if keys := store.Keys(); len(keys) != 1 || keys[0] != record.StorageKey {
		t.Errorf("store keys = %v", keys)
	}
}

func TestResolverRejectsUnregisteredAndMismatchedAssets(t *testing.T) {
	ctx := context.Background()
	store, lookup, record := seededStore(t, []byte("bytes"))
	resolver, _ := NewS3AssetResolver(store, lookup, 0)

	if _, err := resolver.Resolve(ctx, "ast_unknown", ir.Asset{AssetID: "ast_unknown", MediaType: "image/png"}); !errors.Is(err, render.ErrAsset) {
		t.Errorf("unknown asset err = %v, want ErrAsset", err)
	}
	if _, err := resolver.Resolve(ctx, "not-an-asset-id", ir.Asset{AssetID: "not-an-asset-id"}); !errors.Is(err, render.ErrAsset) {
		t.Errorf("bad ID err = %v, want ErrAsset", err)
	}
	if _, err := resolver.Resolve(ctx, record.AssetID, ir.Asset{AssetID: "ast_other", MediaType: "image/png"}); !errors.Is(err, render.ErrAsset) {
		t.Errorf("ID mismatch err = %v, want ErrAsset", err)
	}
	if _, err := resolver.Resolve(ctx, record.AssetID, ir.Asset{AssetID: record.AssetID, MediaType: "image/jpeg"}); !errors.Is(err, render.ErrAsset) {
		t.Errorf("media type mismatch err = %v, want ErrAsset", err)
	}
	// A registry row with a traversal key is rejected before any read.
	traversal := &staticLookup{records: map[string]AssetRecord{
		record.AssetID: {AssetID: record.AssetID, StorageKey: "../../etc/passwd", MediaType: "image/png", SHA256: record.SHA256},
	}}
	bad, _ := NewS3AssetResolver(store, traversal, 0)
	if _, err := bad.Resolve(ctx, record.AssetID, ir.Asset{AssetID: record.AssetID, MediaType: "image/png"}); !errors.Is(err, render.ErrAsset) {
		t.Errorf("traversal key err = %v, want ErrAsset", err)
	}
	// A row with no digest is not usable: content could not be verified.
	noDigest := &staticLookup{records: map[string]AssetRecord{
		record.AssetID: {AssetID: record.AssetID, StorageKey: record.StorageKey, MediaType: "image/png"},
	}}
	plain, _ := NewS3AssetResolver(store, noDigest, 0)
	if _, err := plain.Resolve(ctx, record.AssetID, ir.Asset{AssetID: record.AssetID, MediaType: "image/png"}); !errors.Is(err, render.ErrAsset) {
		t.Errorf("missing digest err = %v, want ErrAsset", err)
	}
}

func TestResolverVerifiesContentDigestOnRead(t *testing.T) {
	ctx := context.Background()
	data := []byte("bytes-to-be-replaced")
	store, lookup, record := seededStore(t, data)
	resolver, _ := NewS3AssetResolver(store, lookup, 0)
	// The object is replaced behind the registry's back: the stream must fail
	// instead of rendering content that is not the registered image.
	replaced := []byte("different-bytes!!!!!")
	store.mu.Lock()
	store.objects[record.StorageKey] = memObject{data: replaced, contentType: "image/png", sha: HashBytes(replaced)}
	store.mu.Unlock()
	rc, err := resolver.Resolve(ctx, record.AssetID, ir.Asset{AssetID: record.AssetID, MediaType: "image/png"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = io.ReadAll(rc)
	rc.Close()
	if !errors.Is(err, render.ErrAsset) {
		t.Errorf("read of replaced content err = %v, want ErrAsset", err)
	}
}

func TestResolverBoundsSizeAndMapsTransientErrors(t *testing.T) {
	ctx := context.Background()
	store, lookup, record := seededStore(t, bytes.Repeat([]byte("x"), 100))
	resolver, _ := NewS3AssetResolver(store, lookup, 10)
	if _, err := resolver.Resolve(ctx, record.AssetID, ir.Asset{AssetID: record.AssetID, MediaType: "image/png"}); !errors.Is(err, render.ErrAssetTooLarge) {
		t.Errorf("oversize err = %v, want ErrAssetTooLarge", err)
	}

	store.SetFault(1, ErrUnavailable)
	_, err := resolver.Resolve(ctx, record.AssetID, ir.Asset{AssetID: record.AssetID, MediaType: "image/png"})
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, render.ErrAsset) {
		t.Errorf("transient err = %v, want ErrAsset wrapping ErrUnavailable", err)
	}

	store.SetFault(1, ErrNotFound)
	if _, err := resolver.Resolve(ctx, record.AssetID, ir.Asset{AssetID: record.AssetID, MediaType: "image/png"}); !errors.Is(err, render.ErrAsset) {
		t.Errorf("missing object err = %v, want ErrAsset", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := resolver.Resolve(ctx, record.AssetID, ir.Asset{AssetID: record.AssetID, MediaType: "image/png"}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled err = %v, want context.Canceled", err)
	}
}

func TestRenderScopeRejectsMismatchedAssetID(t *testing.T) {
	ctx := context.Background()
	store, lookup, record := seededStore(t, []byte("registered-png-bytes"))
	resolver, err := NewS3AssetResolver(store, lookup, 0)
	if err != nil {
		t.Fatal(err)
	}
	scope := resolver.ForRender().(*renderScope)
	_, err = scope.Resolve(ctx, record.AssetID, ir.Asset{AssetID: "ast_other", MediaType: "image/png"})
	if !errors.Is(err, render.ErrAsset) {
		t.Fatalf("mismatched asset ID err = %v, want ErrAsset", err)
	}
	if lookup.calls != 0 {
		t.Errorf("mismatched asset ID caused %d registry lookups, want 0", lookup.calls)
	}
	if assets, _ := scope.Cached(); assets != 0 {
		t.Errorf("mismatched asset ID populated %d cache entries", assets)
	}
}

func TestRenderScopeDownloadsEachAssetOnce(t *testing.T) {
	ctx := context.Background()
	data := []byte("one-download")
	store, lookup, record := seededStore(t, data)
	resolver, _ := NewS3AssetResolver(store, lookup, 0)
	scope := resolver.ForRender()
	asset := ir.Asset{AssetID: record.AssetID, MediaType: "image/png"}
	for i := 0; i < 5; i++ {
		rc, err := scope.Resolve(ctx, record.AssetID, asset)
		if err != nil {
			t.Fatalf("Resolve %d: %v", i, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("read %d = %q, %v", i, got, err)
		}
	}
	// A fresh scope does not share the cache.
	scope2 := resolver.ForRender()
	if assets, _ := scope2.(*renderScope).Cached(); assets != 0 {
		t.Errorf("a new scope started with %d cached assets", assets)
	}
	if _, err := scope2.Resolve(ctx, record.AssetID, asset); err != nil {
		t.Fatal(err)
	}
	if assets, total := scope2.(*renderScope).Cached(); assets != 1 || total != int64(len(data)) {
		t.Errorf("scope cache = %d assets, %d bytes", assets, total)
	}
}

func TestRenderScopeCachesBoundedAndConcurrent(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	lookup := &staticLookup{records: map[string]AssetRecord{}}
	payload := bytes.Repeat([]byte("y"), 40)
	for i, id := range []string{"ast_a", "ast_b", "ast_c"} {
		key, _ := RawAssetKey(id, "image/png")
		if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), "image/png"); err != nil {
			t.Fatal(err)
		}
		lookup.records[id] = AssetRecord{AssetID: id, StorageKey: key, MediaType: "image/png", SHA256: HashBytes(payload)}
		_ = i
	}
	resolver, _ := NewS3AssetResolver(store, lookup, 0)
	scope := resolver.ForRender().(*renderScope)
	scope.maxBytes = 100 // room for two of three assets

	var wg sync.WaitGroup
	for _, id := range []string{"ast_a", "ast_b", "ast_a", "ast_c", "ast_b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			rc, err := scope.Resolve(ctx, id, ir.Asset{AssetID: id, MediaType: "image/png"})
			if err != nil {
				t.Errorf("concurrent Resolve(%s): %v", id, err)
				return
			}
			defer rc.Close()
			if _, err := io.ReadAll(rc); err != nil {
				t.Errorf("concurrent read(%s): %v", id, err)
			}
		}(id)
	}
	wg.Wait()
	assets, total := scope.Cached()
	if total > scope.maxBytes {
		t.Errorf("cache holds %d bytes, limit %d", total, scope.maxBytes)
	}
	if assets > 2 {
		t.Errorf("cache holds %d assets, limit is the byte budget", assets)
	}
	// Every asset was still served correctly even when not cached.
	for _, id := range []string{"ast_a", "ast_b", "ast_c"} {
		rc, err := scope.Resolve(ctx, id, ir.Asset{AssetID: id, MediaType: "image/png"})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, payload) {
			t.Errorf("asset %s served %d bytes", id, len(got))
		}
	}
	_ = strings.TrimSpace
}

func TestRedactRemovesSecretsAndURLs(t *testing.T) {
	msg := "PUT http://user:pass@minio.local:9000/bucket/key?X-Amz-Signature=abc failed with AKIAABCDEFGHIJKLMNOP token=abcdef\nsecond line"
	out := Redact(msg, 200, "abcdef")
	for _, leak := range []string{"user:pass", "X-Amz-Signature=abc", "AKIAABCDEFGHIJKLMNOP", "token=abcdef", "\n"} {
		if strings.Contains(out, leak) {
			t.Errorf("redacted text still contains %q: %s", leak, out)
		}
	}
	if got := Redact(strings.Repeat("x", 100), 10); len(got) != 10 {
		t.Errorf("bounded length = %d, want 10", len(got))
	}
}
