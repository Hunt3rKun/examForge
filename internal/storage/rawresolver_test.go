package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"examforge/internal/ir"
	"examforge/internal/render"
)

// dualStore seeds a cleaned asset: the clean object under the main key and a
// different raw object under the companion key, each with its own digest.
func dualStore(t *testing.T) (*MemStore, *staticLookup, AssetRecord, []byte, []byte) {
	t.Helper()
	id := "ast_01jexam0000000000000000001"
	raw := []byte("raw crop bytes")
	cleaned := []byte("cleaned bytes differ")
	store := NewMemStore()
	rawKey, err := RawAssetKey(id, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	cleanKey, err := CleanAssetKey(id, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, rawKey, bytes.NewReader(raw), int64(len(raw)), "image/png"); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, cleanKey, bytes.NewReader(cleaned), int64(len(cleaned)), "image/png"); err != nil {
		t.Fatal(err)
	}
	record := AssetRecord{AssetID: id, StorageKey: cleanKey, MediaType: "image/png", SHA256: HashBytes(cleaned),
		RawStorageKey: rawKey, RawSHA256: HashBytes(raw)}
	lookup := &staticLookup{records: map[string]AssetRecord{id: record}}
	return store, lookup, record, raw, cleaned
}

func TestResolveMainAndRawIndependently(t *testing.T) {
	store, lookup, record, raw, cleaned := dualStore(t)
	r, err := NewS3AssetResolver(store, lookup, MaxAssetBytes)
	if err != nil {
		t.Fatal(err)
	}
	asset := ir.Asset{AssetID: record.AssetID, MediaType: "image/png"}
	main, err := r.Resolve(t.Context(), record.AssetID, asset)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(main)
	main.Close()
	if err != nil || !bytes.Equal(got, cleaned) {
		t.Fatalf("main read = %q, %v; want clean bytes", got, err)
	}
	rawRC, err := r.ResolveRaw(t.Context(), record.AssetID, asset)
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(rawRC)
	rawRC.Close()
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("raw read = %q, %v; want raw bytes", got, err)
	}
}

func TestResolveRawVerifiesItsOwnDigest(t *testing.T) {
	store, lookup, record, _, _ := dualStore(t)
	// Corrupt the stored raw object's registered digest.
	bad := lookup.records[record.AssetID]
	bad.RawSHA256 = HashBytes([]byte("something else"))
	lookup.records[record.AssetID] = bad
	r, err := NewS3AssetResolver(store, lookup, MaxAssetBytes)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := r.ResolveRaw(t.Context(), record.AssetID, ir.Asset{AssetID: record.AssetID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(rc)
	rc.Close()
	if err == nil || !errors.Is(err, render.ErrAsset) {
		t.Fatalf("raw digest mismatch not detected: %v", err)
	}
}

func TestResolveRawRejectsLegacyUndigestedCompanion(t *testing.T) {
	store, lookup, record, _, _ := dualStore(t)
	legacy := lookup.records[record.AssetID]
	legacy.RawSHA256 = "" // pre-migration row: raw key without digest
	lookup.records[record.AssetID] = legacy
	r, err := NewS3AssetResolver(store, lookup, MaxAssetBytes)
	if err != nil {
		t.Fatal(err)
	}
	asset := ir.Asset{AssetID: record.AssetID}
	if _, err := r.ResolveRaw(t.Context(), record.AssetID, asset); err == nil || !errors.Is(err, render.ErrAsset) {
		t.Fatalf("legacy undigested raw read accepted: %v", err)
	}
	// The main (clean) object still resolves normally.
	rc, err := r.Resolve(t.Context(), record.AssetID, asset)
	if err != nil {
		t.Fatalf("legacy main read failed: %v", err)
	}
	rc.Close()
}

func TestResolveRawWithoutCompanionFails(t *testing.T) {
	data := []byte("single object")
	store, lookup, record := seededStore(t, data)
	r, err := NewS3AssetResolver(store, lookup, MaxAssetBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveRaw(t.Context(), record.AssetID, ir.Asset{AssetID: record.AssetID}); err == nil || !errors.Is(err, render.ErrAsset) {
		t.Fatalf("raw read of a raw_cropped asset accepted: %v", err)
	}
}
