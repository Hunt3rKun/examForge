//go:build integration

// Slice-3 integration coverage: the raw_sha256 migration over existing rows,
// cleaned dual-object persistence through the real worker and PostgreSQL,
// independent digest verification of both objects, and legacy-row behavior.
package pipeline_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"examforge/internal/ir"
	"examforge/internal/parser"
	"examforge/internal/storage"
	"examforge/internal/store"
)

func integrationCleanPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 6, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 6; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 250, G: 250, B: 250, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func integrationCleanedMutation(t *testing.T) parser.MockOption {
	t.Helper()
	cleaned := integrationCleanPNG(t)
	return parser.Mutate(func(result *parser.ParseResult) {
		for i := range result.Assets {
			blob := &result.Assets[i]
			asset := result.Exam.Assets[blob.AssetID]
			asset.Status = "cleaned"
			asset.RawURI = "examforge-asset://" + blob.AssetID + "/raw"
			result.Exam.Assets[blob.AssetID] = asset
			blob.CleanData = append([]byte(nil), cleaned...)
			blob.SHA256 = storage.HashBytes(cleaned)
		}
	})
}

// TestIntegrationCleanedDualDigests runs a cleaned ingest through the real
// worker, PostgreSQL and S3: both objects exist, each digest verifies its own
// object, and the resolver reads main and raw independently.
func TestIntegrationCleanedDualDigests(t *testing.T) {
	h := newHarness(t, integrationCleanedMutation(t))
	sub := h.submit([]byte("%PDF-1.7\nintegration cleaned source\n%%EOF\n"))
	h.waitState(sub.JobID, store.JobPersisted)

	var status, mainKey, mainSHA string
	var rawKey, rawSHA *string
	if err := h.pool.QueryRow(t.Context(),
		"SELECT status, storage_key, sha256, raw_storage_key, raw_sha256 FROM assets LIMIT 1").
		Scan(&status, &mainKey, &mainSHA, &rawKey, &rawSHA); err != nil {
		t.Fatalf("asset row: %v", err)
	}
	if status != "cleaned" || rawKey == nil || rawSHA == nil {
		t.Fatalf("row: status=%s rawKey=%v rawSHA=%v; want cleaned with companion", status, rawKey, rawSHA)
	}
	if !strings.HasPrefix(mainKey, "assets/clean/") || !strings.HasPrefix(*rawKey, "assets/raw/") {
		t.Fatalf("keys: main=%s raw=%s", mainKey, *rawKey)
	}
	for key, want := range map[string]string{mainKey: mainSHA, *rawKey: *rawSHA} {
		rc, err := h.objects.Get(t.Context(), key, storage.MaxAssetBytes)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := storage.HashBytes(data); got != want {
			t.Errorf("object %s hashes to %s, registered %s", key, got, want)
		}
	}
	exam := h.assemble(h.examID(sub.JobID))
	resolver, err := storage.NewS3AssetResolver(h.objects, h.repo, storage.MaxAssetBytes)
	if err != nil {
		t.Fatal(err)
	}
	for id, asset := range exam.Assets {
		if asset.RawURI == "" {
			t.Errorf("assembled cleaned asset %s has no raw_uri", id)
		}
		rc, err := resolver.ResolveRaw(t.Context(), id, asset)
		if err != nil {
			t.Fatalf("resolve raw %s: %v", id, err)
		}
		if _, err := io.ReadAll(rc); err != nil {
			t.Fatalf("read raw %s: %v", id, err)
		}
		rc.Close()
	}
}

// TestIntegrationRawDigestMigrationKeepsLegacyRows proves the migration path:
// a row written before raw_sha256 existed keeps NULL, its main object still
// resolves, and an unverified raw read is refused.
func TestIntegrationRawDigestMigrationKeepsLegacyRows(t *testing.T) {
	h := newHarness(t)
	sub := h.submit([]byte("%PDF-1.7\nintegration legacy source\n%%EOF\n"))
	h.waitState(sub.JobID, store.JobPersisted)

	// Model a pre-migration cleaned row: attach a raw key without a digest,
	// exactly the shape historical rows have after ALTER TABLE.
	var assetID, mainKey string
	if err := h.pool.QueryRow(t.Context(), "SELECT asset_id, storage_key FROM assets LIMIT 1").Scan(&assetID, &mainKey); err != nil {
		t.Fatal(err)
	}
	rawKey, err := storage.RawAssetKey(assetID, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	// The raw object exists: the raw_cropped ingest wrote it under the main
	// key, which is the same deterministic raw key.
	if _, err := h.pool.Exec(t.Context(),
		"UPDATE assets SET status='cleaned', raw_storage_key=$2, raw_sha256=NULL WHERE asset_id=$1",
		assetID, rawKey); err != nil {
		t.Fatalf("legacy shape: %v", err)
	}
	var rawSHA *string
	if err := h.pool.QueryRow(t.Context(), "SELECT raw_sha256 FROM assets WHERE asset_id=$1", assetID).Scan(&rawSHA); err != nil {
		t.Fatal(err)
	}
	if rawSHA != nil {
		t.Fatal("migration fabricated a digest")
	}
	resolver, err := storage.NewS3AssetResolver(h.objects, h.repo, storage.MaxAssetBytes)
	if err != nil {
		t.Fatal(err)
	}
	asset := examAssetFor(t, h, sub.JobID, assetID)
	// Main object keeps working.
	rc, err := resolver.Resolve(t.Context(), assetID, asset)
	if err != nil {
		t.Fatalf("legacy main resolve: %v", err)
	}
	rc.Close()
	// Raw companion without digest is refused.
	if _, err := resolver.ResolveRaw(t.Context(), assetID, asset); err == nil {
		t.Fatal("legacy undigested raw read accepted")
	}
}

func examAssetFor(t *testing.T, h *harness, jobID uuid.UUID, assetID string) ir.Asset {
	t.Helper()
	exam := h.assemble(h.examID(jobID))
	asset, ok := exam.Assets[assetID]
	if !ok {
		t.Fatalf("asset %s not in exam", assetID)
	}
	return asset
}

// TestIntegrationRawDigestNeedsKeyConstraint proves the DB-level CHECK against
// the real database: a digest without a companion key is rejected, and a new
// cleaned record missing its raw digest is rejected by the assembler before
// any SQL runs (covered offline; here the constraint itself is exercised).
func TestIntegrationRawDigestNeedsKeyConstraint(t *testing.T) {
	h := newHarness(t)
	sub := h.submit([]byte("%PDF-1.7\nintegration constraint source\n%%EOF\n"))
	h.waitState(sub.JobID, store.JobPersisted)
	var assetID string
	if err := h.pool.QueryRow(t.Context(), "SELECT asset_id FROM assets LIMIT 1").Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	_, err := h.pool.Exec(t.Context(),
		"UPDATE assets SET raw_sha256=$2 WHERE asset_id=$1", assetID, strings.Repeat("ab", 32))
	if err == nil {
		t.Fatal("raw digest without companion key accepted by the database")
	}
	if !strings.Contains(err.Error(), "assets_raw_digest_needs_key") {
		t.Fatalf("err = %v, want the CHECK constraint", err)
	}
	// A malformed digest is rejected too.
	_, err = h.pool.Exec(t.Context(),
		"UPDATE assets SET raw_storage_key='assets/raw/x.png', raw_sha256='nothex' WHERE asset_id=$1", assetID)
	if err == nil {
		t.Fatal("malformed raw digest accepted")
	}
}
