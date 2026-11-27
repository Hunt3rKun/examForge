package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"examforge/internal/ir"
	"examforge/internal/render"
	"examforge/internal/storage"
)

// Default fixture paths, relative to the repository root.
const (
	DefaultGoldenIR        = "testdata/golden/ir/sample.json"
	DefaultGoldenAssetRoot = "testdata/golden/assets"
)

// MockParserAdapter replays a golden Exam IR and its asset files for any
// valid PDF source. It keeps the same contract as a real adapter: a bounded,
// hashed source read; a candidate IR; asset bytes with declared digests.
// Fault hooks make it misbehave on demand. It never touches the network.
type MockParserAdapter struct {
	irPath    string
	assetRoot string

	mu         sync.Mutex
	exam       ir.ExamDocumentIR
	assets     []AssetBlob
	invalid    int
	failures   int
	failErr    error
	delay      time.Duration
	duplicates int
	mutate     func(*ParseResult)
	calls      int
}

var _ ParserAdapter = (*MockParserAdapter)(nil)

// MockOption configures a MockParserAdapter at construction.
type MockOption func(*MockParserAdapter)

// FromGolden loads the IR and the assets it references from a golden fixture
// set: an IR file and an asset root holding manifest.json.
func FromGolden(irPath, assetRoot string) MockOption {
	return func(m *MockParserAdapter) { m.irPath, m.assetRoot = irPath, assetRoot }
}

// InvalidTimes makes the next n parses return an IR that fails validation: a
// question referencing an asset that is not part of the exam.
func InvalidTimes(n int) MockOption { return func(m *MockParserAdapter) { m.invalid = n } }

// FailTimes makes the next n parses fail with err as a transient error.
func FailTimes(n int, err error) MockOption {
	return func(m *MockParserAdapter) { m.failures, m.failErr = n, err }
}

// DuplicateQuestions appends n copies of the first question with other
// display numbers, so the exam contains questions with equal fingerprints.
func DuplicateQuestions(n int) MockOption { return func(m *MockParserAdapter) { m.duplicates = n } }

// Delay makes every parse wait for d, honoring the context.
func Delay(d time.Duration) MockOption { return func(m *MockParserAdapter) { m.delay = d } }

// Mutate lets a test edit each successful result.
func Mutate(fn func(*ParseResult)) MockOption { return func(m *MockParserAdapter) { m.mutate = fn } }

// NewMockParserAdapter builds a mock that replays a golden fixture set.
// Paths are relative to the current working directory.
func NewMockParserAdapter(opts ...MockOption) (*MockParserAdapter, error) {
	m := &MockParserAdapter{irPath: DefaultGoldenIR, assetRoot: DefaultGoldenAssetRoot}
	for _, opt := range opts {
		opt(m)
	}
	data, err := os.ReadFile(m.irPath)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &m.exam); err != nil {
		return nil, fmt.Errorf("decode golden IR %s: %w", m.irPath, err)
	}
	// The manifest, never the IR uri, decides which file an asset ID names.
	manifestData, err := os.ReadFile(filepath.Join(m.assetRoot, render.AssetManifestFile))
	if err != nil {
		return nil, err
	}
	var manifest map[string]string
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("decode asset manifest: %w", err)
	}
	ids := make([]string, 0, len(m.exam.Assets))
	for id := range m.exam.Assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rel, ok := manifest[id]
		if !ok || filepath.IsAbs(rel) || rel == "" {
			return nil, fmt.Errorf("asset %s is not in the asset manifest", id)
		}
		blob, err := os.ReadFile(filepath.Join(m.assetRoot, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		m.assets = append(m.assets, AssetBlob{
			AssetID:   id,
			MediaType: m.exam.Assets[id].MediaType,
			Data:      blob,
			SHA256:    storage.HashBytes(blob),
		})
	}
	return m, nil
}

// Parse reads and verifies the source, then replays the golden result. Fault
// hooks apply in order: delay, transient failures, invalid output,
// duplicates, mutate.
func (m *MockParserAdapter) Parse(ctx context.Context, source Source) (ParseResult, error) {
	var result ParseResult
	var h hooks
	err := func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.calls++
		h = hooks{delay: m.delay, failures: m.failures, failErr: m.failErr, invalid: m.invalid, duplicates: m.duplicates, mutate: m.mutate}
		if h.failures > 0 {
			m.failures--
		}
		if h.invalid > 0 {
			m.invalid--
		}
		// Every call gets its own copy; a hook cannot alter the fixture.
		data, err := json.Marshal(m.exam)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &result.Exam); err != nil {
			return err
		}
		result.Assets = make([]AssetBlob, len(m.assets))
		for i, blob := range m.assets {
			result.Assets[i] = AssetBlob{AssetID: blob.AssetID, MediaType: blob.MediaType,
				Data: append([]byte(nil), blob.Data...), SHA256: blob.SHA256}
		}
		return nil
	}()
	if err != nil {
		return ParseResult{}, err
	}

	if _, err := ReadSource(ctx, source); err != nil {
		return ParseResult{}, err
	}
	if h.delay > 0 {
		select {
		case <-time.After(h.delay):
		case <-ctx.Done():
			return ParseResult{}, ctx.Err()
		}
	}
	if h.failures > 0 {
		return ParseResult{}, Transient("mock_timeout", h.failErr)
	}
	if h.invalid > 0 {
		first := &result.Exam.Sections[0].Questions[0]
		first.Stem = append(first.Stem, ir.ContentBlock{Type: "image_ref", AssetID: "ast_missing000000000000000001", LayoutHint: "inline"})
	}
	for i := 0; i < h.duplicates; i++ {
		dup := cloneQuestion(result.Exam.Sections[0].Questions[0])
		dup.QuestionID = fmt.Sprintf("mock_duplicate_%d", i)
		dup.DisplayNumber = fmt.Sprintf("%d", 100+i)
		result.Exam.Sections[0].Questions = append(result.Exam.Sections[0].Questions, dup)
	}
	if h.mutate != nil {
		h.mutate(&result)
	}
	return result, nil
}

// Calls reports how many parses ran.
func (m *MockParserAdapter) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// InjectInvalid makes the next n parses return an invalid IR.
func (m *MockParserAdapter) InjectInvalid(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invalid = n
}

// InjectFailures makes the next n parses fail transiently with err.
func (m *MockParserAdapter) InjectFailures(n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures, m.failErr = n, err
}

// hooks is the snapshot of the fault state for one Parse call.
type hooks struct {
	delay      time.Duration
	failures   int
	failErr    error
	invalid    int
	duplicates int
	mutate     func(*ParseResult)
}

func cloneQuestion(q ir.QuestionItem) ir.QuestionItem {
	data, err := json.Marshal(q)
	if err != nil {
		panic(err)
	}
	var out ir.QuestionItem
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}
