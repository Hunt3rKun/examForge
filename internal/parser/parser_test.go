package parser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"examforge/internal/ir"
	"examforge/internal/storage"
)

// pdfFixture returns a minimal source whose bytes start with the PDF magic.
// sourceOver returns a Source over arbitrary bytes with a matching digest.
func sourceOver(data []byte) Source {
	return Source{
		JobID:      uuid.New(),
		StorageKey: "uploads/source.pdf",
		SHA256:     storage.HashBytes(data),
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}
}

func pdfFixture(t *testing.T, body string) (Source, []byte) {
	t.Helper()
	data := []byte("%PDF-1.7\n" + body)
	return Source{
		JobID:      uuid.New(),
		StorageKey: "uploads/source.pdf",
		SHA256:     storage.HashBytes(data),
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}, data
}

// golden points the mock at the repository's golden fixture set.
var golden = FromGolden("../../testdata/golden/ir/sample.json", "../../testdata/golden/assets")

func newMock(t *testing.T, opts ...MockOption) *MockParserAdapter {
	t.Helper()
	mock, err := NewMockParserAdapter(append([]MockOption{golden}, opts...)...)
	if err != nil {
		t.Fatalf("NewMockParserAdapter: %v", err)
	}
	return mock
}

func TestMockParseReplaysGoldenFixture(t *testing.T) {
	mock := newMock(t)
	source, _ := pdfFixture(t, "fixture")
	result, err := mock.Parse(t.Context(), source)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	validator, err := ir.NewValidator("../../schemas/exam-ir-v1.json", "../../schemas/layout-spec-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateExamDTO(result.Exam); err != nil {
		t.Errorf("mock IR does not validate: %v", err)
	}
	if len(result.Assets) != 1 || result.Assets[0].AssetID != "ast_01jexam0000000000000000001" {
		t.Fatalf("assets = %+v", result.Assets)
	}
	blob := result.Assets[0]
	png, err := os.ReadFile("../../testdata/golden/assets/sample_fig.png")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blob.RawData, png) || blob.RawSHA256 != storage.HashBytes(png) || blob.MediaType != "image/png" {
		t.Errorf("asset blob does not match the fixture file")
	}
	if len(blob.CleanData) != 0 || blob.SHA256 != "" {
		t.Errorf("raw_cropped fixture asset carries clean payload")
	}
}

func TestReadSourceRejectsBadSourcesPermanently(t *testing.T) {
	mock := newMock(t)
	cases := map[string]func(*Source){
		"not a PDF": func(s *Source) {
			data := []byte("not a pdf at all")
			s.SHA256 = storage.HashBytes(data)
			s.Open = func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
		},
		"hash mismatch": func(s *Source) { s.SHA256 = strings.Repeat("0", 64) },
	}
	for name, mutate := range cases {
		source, _ := pdfFixture(t, "body")
		mutate(&source)
		_, err := mock.Parse(t.Context(), source)
		if !IsPermanent(err) {
			t.Errorf("%s: err = %v, want a permanent parser error", name, err)
		}
	}
	// A reader that cannot be opened keeps its own class: an object store
	// outage there is transient, not a property of the input.
	source, _ := pdfFixture(t, "body")
	source.Open = func(context.Context) (io.ReadCloser, error) { return nil, storage.ErrUnavailable }
	if _, err := mock.Parse(t.Context(), source); !errors.Is(err, storage.ErrUnavailable) || IsPermanent(err) {
		t.Errorf("open error err = %v, want the transient storage error", err)
	}
	// An oversize source is cut off at the limit and rejected.
	big := bytes.Repeat([]byte("x"), storage.MaxSourceBytes+16)
	big = append([]byte("%PDF-1.7"), big...)
	oversize := sourceOver(big)
	if _, err := mock.Parse(t.Context(), oversize); !IsPermanent(err) {
		t.Errorf("oversize source err = %v, want permanent", err)
	}
}

func TestMockFaultHooks(t *testing.T) {
	source, _ := pdfFixture(t, "body")

	t.Run("invalid once then valid", func(t *testing.T) {
		mock := newMock(t, InvalidTimes(1))
		bad, err := mock.Parse(t.Context(), source)
		if err != nil {
			t.Fatalf("invalid parse returned an error instead of a bad IR: %v", err)
		}
		validator, _ := ir.NewValidator("../../schemas/exam-ir-v1.json", "../../schemas/layout-spec-v1.json")
		if validator.ValidateExamDTO(bad.Exam) == nil {
			t.Error("InvalidTimes did not produce an IR that fails validation")
		}
		good, err := mock.Parse(t.Context(), source)
		if err != nil || validator.ValidateExamDTO(good.Exam) != nil {
			t.Errorf("parse after the invalid one: %v", err)
		}
	})
	t.Run("transient once then valid", func(t *testing.T) {
		mock := newMock(t, FailTimes(1, errors.New("model gateway timeout")))
		_, err := mock.Parse(t.Context(), source)
		if !IsParser(err) || IsPermanent(err) {
			t.Errorf("FailTimes err = %v, want a transient parser error", err)
		}
		if _, err := mock.Parse(t.Context(), source); err != nil {
			t.Errorf("parse after the transient failure: %v", err)
		}
	})
	t.Run("duplicated fingerprints", func(t *testing.T) {
		mock := newMock(t, DuplicateQuestions(2))
		result, err := mock.Parse(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}
		questions := result.Exam.Sections[0].Questions
		if len(questions) != 4 {
			t.Fatalf("%d questions, want 4", len(questions))
		}
		// The duplicates share the first question's semantics; the store
		// fingerprint is the authority on that.
		if questions[0].Stem[0].Markdown != questions[2].Stem[0].Markdown {
			t.Error("duplicate does not share the stem")
		}
	})
	t.Run("delay honors cancellation", func(t *testing.T) {
		mock := newMock(t, Delay(time.Hour))
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := mock.Parse(ctx, source)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
			t.Errorf("cancelled parse err = %v after %s", err, time.Since(start))
		}
		if IsPermanent(err) {
			t.Error("cancellation was classified as permanent")
		}
	})
}
