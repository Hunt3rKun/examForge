package render

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"examforge/internal/ir"
)

// testPaths are repository-relative locations used by the tests and the CLI.
var (
	testSchemaIR     = "../../schemas/exam-ir-v1.json"
	testSchemaLayout = "../../schemas/layout-spec-v1.json"
	testGoldenIR     = "../../testdata/golden/ir/sample.json"
	testGoldenLayout = "../../testdata/golden/layout"
	testGoldenAssets = "../../testdata/golden/assets"
	testRenderAssets = "../../testdata/render/assets"
)

// renderResolver serves the render-fixture asset root.
func renderResolver(t *testing.T) AssetResolver {
	t.Helper()
	resolver, err := NewLocalAssetResolver(testRenderAssets)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

// requireTypst skips a real-render test when Typst is not installed. The
// verify script checks for Typst up front so nothing is skipped unnoticed.
func requireTypst(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("typst"); err != nil {
		t.Skipf("typst is not installed: %v", err)
	}
}

// loadIR loads a schema-valid Exam IR fixture.
func loadIR(t *testing.T, path string) ir.ExamDocumentIR {
	t.Helper()
	if err := ir.ValidateFile(testSchemaIR, path); err != nil {
		t.Fatalf("fixture %s is not a valid Exam IR: %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var exam ir.ExamDocumentIR
	if err := json.Unmarshal(data, &exam); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return exam
}

// loadLayout loads a schema-valid Layout Spec fixture.
func loadLayout(t *testing.T, path string) ir.LayoutSpec {
	t.Helper()
	if err := ir.ValidateFile(testSchemaLayout, path); err != nil {
		t.Fatalf("fixture %s is not a valid Layout Spec: %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var layout ir.LayoutSpec
	if err := json.Unmarshal(data, &layout); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return layout
}

// newRenderer builds a renderer on the repository's template and vendored
// package, with a private temporary root so a leftover workspace is detected.
// The asset root defaults to the golden assets; tests with their own fixtures
// override cfg.AssetResolver.
func newRenderer(t *testing.T, mutate func(*TypstConfig)) *TypstRenderer {
	t.Helper()
	tempRoot := t.TempDir()
	resolver, err := NewLocalAssetResolver(testGoldenAssets)
	if err != nil {
		t.Fatal(err)
	}
	cfg := TypstConfig{
		TemplateDir:   "../../templates",
		PackageDir:    "../../third_party/typst-packages",
		TempRoot:      tempRoot,
		AssetResolver: resolver,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	r, err := NewTypstRenderer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { assertEmptyDir(t, tempRoot, "temporary root") })
	return r
}

// assertEmptyDir fails when a render left files behind.
func assertEmptyDir(t *testing.T, dir, what string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Errorf("read %s: %v", what, err)
		return
	}
	if len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s is not empty, %d file(s) left behind: %v", what, len(names), names)
	}
}

// dumpPDF writes an artifact for manual inspection when EXAMFORGE_DUMP_DIR is
// set. It is a development aid and asserts nothing.
func dumpPDF(t *testing.T, name string, art *Artifact) {
	dir := os.Getenv("EXAMFORGE_DUMP_DIR")
	if dir == "" {
		return
	}
	path := filepath.Join(dir, name+".pdf")
	if err := os.WriteFile(path, art.Bytes, 0o644); err != nil {
		t.Errorf("dump %s: %v", path, err)
	}
}

func assertPDF(t *testing.T, art *Artifact) {
	t.Helper()
	if art == nil {
		t.Fatal("artifact is nil")
	}
	if art.MIMEType != MIMEPDF {
		t.Errorf("MIMEType = %q, want %q", art.MIMEType, MIMEPDF)
	}
	if !bytes.HasPrefix(art.Bytes, []byte("%PDF-")) || len(art.Bytes) == 0 {
		t.Errorf("artifact is not a PDF: %d bytes, header %q", len(art.Bytes), art.Bytes[:min(8, len(art.Bytes))])
	}
}

func TestRenderGoldenRealTypst(t *testing.T) {
	requireTypst(t)
	exam := loadIR(t, testGoldenIR)
	r := newRenderer(t, nil)
	for _, mode := range []string{"student", "teacher"} {
		t.Run(mode, func(t *testing.T) {
			layout := loadLayout(t, filepath.Join(testGoldenLayout, mode+".json"))
			art, err := r.Render(t.Context(), exam, layout)
			if err != nil {
				t.Fatalf("render %s: %v", mode, err)
			}
			assertPDF(t, art)
			// The golden Q1 image is the last stem block, so its float_right
			// hint degrades visibly; nothing else may warn. In particular no
			// option_measure_failed: auto grids must really be measured.
			want := []RenderWarning{{
				Code: "float_fallback",
				Path: "sections[0].questions[0].stem[1]",
			}}
			if len(art.Warnings) != len(want) {
				t.Fatalf("warnings = %+v, want exactly %+v", art.Warnings, want)
			}
			for i, w := range art.Warnings {
				if w.Code != want[i].Code || w.Path != want[i].Path || w.Message == "" {
					t.Errorf("warning[%d] = %+v, want code %q path %q with a message", i, w, want[i].Code, want[i].Path)
				}
			}
			dumpPDF(t, "golden-"+mode, art)
		})
	}
}
