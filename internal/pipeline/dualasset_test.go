package pipeline

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"examforge/internal/ir"
	"examforge/internal/parser"
	"examforge/internal/storage"
	"examforge/internal/store"
)

// distinctPNG builds a small white PNG different from the golden fixture.
func distinctPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// cleanedMutation turns the golden raw_cropped result into a cleaned dual
// payload, keeping the IR and blob contract consistent.
func cleanedMutation(t *testing.T) parser.MockOption {
	t.Helper()
	cleaned := distinctPNG(t)
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

func TestWorkerCleanedUploadsTwoVerifiedObjects(t *testing.T) {
	worker, db, objects, _, jobID := workerFixture(t, cleanedMutation(t))
	job := &river.Job[IngestArgs]{JobRow: &rivertype.JobRow{ID: 1}, Args: IngestArgs{JobID: jobID.String()}}
	if err := worker.Work(t.Context(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	rows := db.Assets()
	if len(rows) != 1 {
		t.Fatalf("assets = %d", len(rows))
	}
	row := rows[0]
	if row.Status != "cleaned" || !strings.HasPrefix(row.StorageKey, "assets/clean/") ||
		!row.RawStorageKey.Valid || !strings.HasPrefix(row.RawStorageKey.String, "assets/raw/") ||
		!row.RawSha256.Valid || !row.CleanStorageKey.Valid || row.CleanStorageKey.String != row.StorageKey ||
		!row.CleanSha256.Valid || row.CleanSha256.String != row.Sha256 ||
		row.ActiveVariant != string(storage.VariantClean) {
		t.Fatalf("cleaned row = %+v", row)
	}
	// Source + raw + clean objects exist.
	if keys := objects.Keys(); len(keys) != 3 {
		t.Fatalf("object keys = %v, want source + raw + clean", keys)
	}
	// Both digests verify their own object.
	for key, want := range map[string]string{row.StorageKey: row.Sha256, row.RawStorageKey.String: row.RawSha256.String} {
		rc, err := objects.Get(t.Context(), key, storage.MaxAssetBytes)
		if err != nil {
			t.Fatal(err)
		}
		data := make([]byte, storage.MaxAssetBytes)
		n, _ := rc.Read(data)
		rc.Close()
		if got := storage.HashBytes(data[:n]); got != want {
			t.Errorf("object %s hashes to %s, registered %s", key, got, want)
		}
	}
	// The resolver reads the clean main object and the raw companion.
	resolver, err := storage.NewS3AssetResolver(objects, db, storage.MaxAssetBytes)
	if err != nil {
		t.Fatal(err)
	}
	asset := ir.Asset{AssetID: row.AssetID, MediaType: row.MediaType}
	for _, read := range []func() error{
		func() error {
			rc, err := resolver.Resolve(t.Context(), row.AssetID, asset)
			if rc != nil {
				rc.Close()
			}
			return err
		},
		func() error {
			rc, err := resolver.ResolveRaw(t.Context(), row.AssetID, asset)
			if rc != nil {
				rc.Close()
			}
			return err
		},
	} {
		if err := read(); err != nil {
			t.Errorf("resolver read: %v", err)
		}
	}
}

func TestWorkerRawCroppedUploadsSingleObject(t *testing.T) {
	worker, db, objects, _, jobID := workerFixture(t)
	job := &river.Job[IngestArgs]{JobRow: &rivertype.JobRow{ID: 1}, Args: IngestArgs{JobID: jobID.String()}}
	if err := worker.Work(t.Context(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	row := db.Assets()[0]
	// A raw_cropped asset has exactly one variant: the raw object, which is
	// also the main object. There is no clean variant and no clean columns.
	if row.Status != "raw_cropped" || !strings.HasPrefix(row.StorageKey, "assets/raw/") ||
		!row.RawStorageKey.Valid || row.RawStorageKey.String != row.StorageKey ||
		!row.RawSha256.Valid || row.RawSha256.String != row.Sha256 ||
		row.CleanStorageKey.Valid || row.CleanSha256.Valid ||
		row.ActiveVariant != string(storage.VariantRaw) {
		t.Fatalf("raw_cropped row = %+v", row)
	}
	if keys := objects.Keys(); len(keys) != 2 {
		t.Fatalf("object keys = %v, want source + single raw asset", keys)
	}
}

func TestWorkerRejectsInconsistentDualPayloads(t *testing.T) {
	cases := map[string]parser.MockOption{
		"cleaned without clean bytes": parser.Mutate(func(result *parser.ParseResult) {
			for i := range result.Assets {
				asset := result.Exam.Assets[result.Assets[i].AssetID]
				asset.Status = "cleaned"
				result.Exam.Assets[result.Assets[i].AssetID] = asset
			}
		}),
		"clean bytes equal raw": parser.Mutate(func(result *parser.ParseResult) {
			for i := range result.Assets {
				blob := &result.Assets[i]
				asset := result.Exam.Assets[blob.AssetID]
				asset.Status = "cleaned"
				result.Exam.Assets[blob.AssetID] = asset
				blob.CleanData = append([]byte(nil), blob.RawData...)
				blob.SHA256 = blob.RawSHA256
			}
		}),
		"raw_cropped with clean bytes": parser.Mutate(func(result *parser.ParseResult) {
			for i := range result.Assets {
				result.Assets[i].CleanData = []byte("extra")
			}
		}),
		"declared raw digest mismatch": parser.Mutate(func(result *parser.ParseResult) {
			for i := range result.Assets {
				result.Assets[i].RawSHA256 = strings.Repeat("00", 32)
			}
		}),
		"declared clean digest mismatch": parser.Mutate(func(result *parser.ParseResult) {
			for i := range result.Assets {
				blob := &result.Assets[i]
				asset := result.Exam.Assets[blob.AssetID]
				asset.Status = "cleaned"
				result.Exam.Assets[blob.AssetID] = asset
				blob.CleanData = distinctPNG(t)
				blob.SHA256 = strings.Repeat("00", 32)
			}
		}),
		"payload media type mismatch": parser.Mutate(func(result *parser.ParseResult) {
			for i := range result.Assets {
				result.Assets[i].MediaType = "image/gif"
			}
		}),
	}
	for name, opt := range cases {
		t.Run(name, func(t *testing.T) {
			worker, db, _, fake, jobID := workerFixture(t, opt)
			job := &river.Job[IngestArgs]{JobRow: &rivertype.JobRow{ID: 3}, Args: IngestArgs{JobID: jobID.String()}}
			err := worker.Work(t.Context(), job)
			if err == nil || !IsPermanent(err) {
				t.Fatalf("inconsistent payload accepted: %v", err)
			}
			rows := db.JobRows()
			if len(rows) != 1 || rows[0].State != store.JobFailed {
				t.Fatalf("job state = %+v, want failed", rows)
			}
			if len(fake.cancelledJobs()) != 1 {
				t.Fatalf("river job not cancelled")
			}
		})
	}
}
