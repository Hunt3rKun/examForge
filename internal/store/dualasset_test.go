package store_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"examforge/internal/ir"
	"examforge/internal/storage"
	"examforge/internal/store"
	"examforge/internal/store/sqlcgen"
	"examforge/internal/store/storetest"
)

// cleanPNG builds a second, distinct PNG so raw and clean payloads are truly
// different objects.
func cleanPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// cleanedFixture turns the golden raw_cropped asset into a cleaned dual-object
// asset and returns the exam plus its verified upload records.
func cleanedFixture(t *testing.T) (ir.ExamDocumentIR, []store.VerifiedAsset) {
	t.Helper()
	exam := clone(t, loadGolden(t))
	raw := mustRead(t, goldenAsset)
	cleaned := cleanPNG(t)
	var out []store.VerifiedAsset
	for id, asset := range exam.Assets {
		rawKey, err := storage.RawAssetKey(id, asset.MediaType)
		if err != nil {
			t.Fatal(err)
		}
		cleanKey, err := storage.CleanAssetKey(id, asset.MediaType)
		if err != nil {
			t.Fatal(err)
		}
		asset.Status = "cleaned"
		asset.RawURI = store.AssetURIScheme + id + "/raw"
		exam.Assets[id] = asset
		out = append(out, store.VerifiedAsset{
			AssetID: id, StorageKey: cleanKey, SHA256: storage.HashBytes(cleaned),
			RawStorageKey: rawKey, RawSHA256: storage.HashBytes(raw),
		})
	}
	return exam, out
}

func TestPersistCleanedAssetStoresBothDigests(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	exam, ups := cleanedFixture(t)
	jobID := activeJob(t, db, sourceSHA)
	if _, err := a.Persist(t.Context(), db, exam, store.SourceRecord{
		JobID: jobID, SourcePDFKey: "uploads/" + jobID.String() + "/source.pdf",
		SourceSHA256: sourceSHA, Assets: ups,
	}); err != nil {
		t.Fatalf("Persist cleaned: %v", err)
	}
	rows := db.Assets()
	if len(rows) != 1 {
		t.Fatalf("assets = %d", len(rows))
	}
	row := rows[0]
	if row.Status != "cleaned" || !row.RawStorageKey.Valid || !row.RawSha256.Valid {
		t.Fatalf("cleaned row missing companion fields: %+v", row)
	}
	if row.Sha256 != ups[0].SHA256 || row.RawSha256.String != ups[0].RawSHA256 {
		t.Fatal("main and raw digests not stored independently")
	}
	if row.StorageKey != ups[0].StorageKey || !strings.HasPrefix(row.StorageKey, "assets/clean/") {
		t.Fatalf("main key = %q, want clean key", row.StorageKey)
	}
	// Re-persisting identical uploads is idempotent.
	jobID2 := activeJob(t, db, sourceSHA)
	if _, err := a.Persist(t.Context(), db, clone(t, exam), store.SourceRecord{
		JobID: jobID2, SourcePDFKey: "uploads/" + jobID2.String() + "/source.pdf",
		SourceSHA256: sourceSHA, Assets: ups,
	}); err != nil {
		t.Fatalf("idempotent re-persist: %v", err)
	}
}

func TestCleanedPersistRejectsBadCompanionRecords(t *testing.T) {
	a := newAssembler(t)
	base, baseUps := cleanedFixture(t)
	cases := map[string]func(*[]store.VerifiedAsset){
		"missing raw digest": func(ups *[]store.VerifiedAsset) { (*ups)[0].RawSHA256 = "" },
		"missing raw key":    func(ups *[]store.VerifiedAsset) { (*ups)[0].RawStorageKey = "" },
		"bad raw digest":     func(ups *[]store.VerifiedAsset) { (*ups)[0].RawSHA256 = "nothex" },
		"wrong raw key": func(ups *[]store.VerifiedAsset) {
			(*ups)[0].RawStorageKey = "assets/raw/ast_other0000000000000000001.png"
		},
		"main key is raw": func(ups *[]store.VerifiedAsset) { (*ups)[0].StorageKey = (*ups)[0].RawStorageKey },
	}
	for name, mutate := range cases {
		db := storetest.New()
		exam := clone(t, base)
		ups := append([]store.VerifiedAsset(nil), baseUps...)
		mutate(&ups)
		jobID := activeJob(t, db, sourceSHA)
		_, err := a.Persist(t.Context(), db, exam, store.SourceRecord{
			JobID: jobID, SourceSHA256: sourceSHA, Assets: ups,
		})
		if !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: err = %v, want store.ErrInvalid", name, err)
		}
		if len(db.Assets()) != 0 {
			t.Errorf("%s: rejected persist wrote asset rows", name)
		}
	}
}

func TestRawCroppedRejectsCompanionRecord(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	exam := loadGolden(t) // raw_cropped
	ups := uploads(t, exam)
	ups[0].RawStorageKey = ups[0].StorageKey
	ups[0].RawSHA256 = ups[0].SHA256
	jobID := activeJob(t, db, sourceSHA)
	_, err := a.Persist(t.Context(), db, exam, store.SourceRecord{JobID: jobID, SourceSHA256: sourceSHA, Assets: ups})
	if !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("raw_cropped with companion accepted: %v", err)
	}
}

func TestRawDigestConflictIsRejected(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	exam, ups := cleanedFixture(t)
	jobID := activeJob(t, db, sourceSHA)
	if _, err := a.Persist(t.Context(), db, exam, store.SourceRecord{
		JobID: jobID, SourceSHA256: sourceSHA, Assets: ups,
	}); err != nil {
		t.Fatal(err)
	}
	// Same clean digest, different raw digest: conflict, nothing overwritten.
	changed := append([]store.VerifiedAsset(nil), ups...)
	changed[0].RawSHA256 = strings.Repeat("cd", 32)
	jobID2 := activeJob(t, db, sourceSHA)
	_, err := a.Persist(t.Context(), db, clone(t, exam), store.SourceRecord{
		JobID: jobID2, SourceSHA256: sourceSHA, Assets: changed,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("different raw digest accepted: %v", err)
	}
	if got := db.Assets()[0].RawSha256.String; got != ups[0].RawSHA256 {
		t.Fatalf("registered raw digest was overwritten: %s", got)
	}
}

// TestLegacyRowWithoutRawDigestKeepsMainBehavior models a pre-migration row:
// raw key present, raw digest NULL. Re-persisting the same clean object keeps
// the legacy row (no digest fabricated), and the record surfaces the missing
// raw digest so the resolver can refuse unverified raw reads.
func TestLegacyRowWithoutRawDigestKeepsMainBehavior(t *testing.T) {
	a, db := newAssembler(t), storetest.New()
	exam, ups := cleanedFixture(t)
	legacy := ups[0]
	jobID := activeJob(t, db, sourceSHA)
	// Write the legacy row directly: raw key present, raw digest NULL.
	id := legacy.AssetID
	bbox, err := json.Marshal(exam.Assets[id].SourceBBox)
	if err != nil {
		t.Fatal(err)
	}
	asset := exam.Assets[id]
	if _, err := db.InsertAsset(t.Context(), sqlcgen.InsertAssetParams{
		AssetID: id, MediaType: asset.MediaType,
		StorageKey:       legacy.StorageKey,
		RawStorageKey:    pgtype.Text{String: legacy.RawStorageKey, Valid: true},
		Status:           "cleaned",
		AspectRatio:      pgtype.Float8{Float64: asset.AspectRatio, Valid: asset.AspectRatio > 0},
		SuggestedWidthMm: pgtype.Float8{Float64: asset.SuggestedWidthMM, Valid: asset.SuggestedWidthMM > 0},
		Caption:          pgtype.Text{String: asset.Caption, Valid: asset.Caption != ""},
		Alt:              pgtype.Text{String: asset.Alt, Valid: asset.Alt != ""},
		SourceBbox:       bbox,
		Sha256:           legacy.SHA256,
		// RawSha256 deliberately unset: the pre-migration shape.
		//
		// CleanStorageKey stays NULL too, modelling a row that predates the
		// variant columns; the resolver must fall back to the main key for
		// clean. active_variant is NOT NULL with a CHECK, so the selector is
		// always written explicitly.
		ActiveVariant: string(storage.VariantClean),
	}); err != nil {
		t.Fatal(err)
	}
	// The assembler must accept the same main object without inventing a raw
	// digest for the historical row.
	if _, err := a.Persist(t.Context(), db, clone(t, exam), store.SourceRecord{
		JobID: jobID, SourceSHA256: sourceSHA, Assets: ups,
	}); err != nil {
		t.Fatalf("legacy row re-persist: %v", err)
	}
	row := db.Assets()[0]
	if row.RawSha256.Valid {
		t.Fatal("a digest was fabricated for a legacy row")
	}
	rec, err := db.AssetRecord(t.Context(), ups[0].AssetID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.RawStorageKey == "" || rec.RawSHA256 != "" {
		t.Fatalf("legacy record = %+v, want raw key with empty raw digest", rec)
	}
}
