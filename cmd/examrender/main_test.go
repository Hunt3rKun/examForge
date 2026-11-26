package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI builds the CLI once and runs it with the given arguments from the
// repository root, returning combined output and the exit error.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "examrender")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = "../.."
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func goldenArgs(out string) []string {
	return []string{
		"-ir", "testdata/golden/ir/sample.json",
		"-layout", "testdata/golden/layout/student.json",
		"-assets", "testdata/golden/assets",
		"-out", out,
	}
}

func requireTypst(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("typst"); err != nil {
		t.Skipf("typst is not installed: %v", err)
	}
}

// TestCLIRendersGolden covers the end-to-end path and the PDF header.
func TestCLIRendersGolden(t *testing.T) {
	requireTypst(t)
	out := filepath.Join(t.TempDir(), "golden.pdf")
	log, err := runCLI(t, goldenArgs(out)...)
	if err != nil {
		t.Fatalf("examrender: %v\n%s", err, log)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		t.Errorf("output is not a PDF: %q", data[:min(8, len(data))])
	}
	// The golden float fallback warning is reported on stderr, not hidden.
	if !strings.Contains(log, "float_fallback") {
		t.Errorf("expected float_fallback warning in output: %s", log)
	}
}

// TestCLIRejectsBadInput covers schema validation and missing flags.
func TestCLIRejectsBadInput(t *testing.T) {
	if _, err := runCLI(t); err == nil {
		t.Error("missing flags accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"schema_version":"1.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := goldenArgs(filepath.Join(t.TempDir(), "out.pdf"))
	args[1] = bad
	log, err := runCLI(t, args...)
	if err == nil {
		t.Error("invalid exam IR accepted")
	}
	if !strings.Contains(log, "exam IR") {
		t.Errorf("error lacks context: %s", log)
	}
}

// TestCLIRefusesOverwrite asserts the output cannot overwrite an input.
func TestCLIRefusesOverwrite(t *testing.T) {
	args := goldenArgs("testdata/golden/ir/sample.json")
	log, err := runCLI(t, args...)
	if err == nil {
		t.Fatal("output overwriting the IR input accepted")
	}
	if !strings.Contains(log, "overwrite") {
		t.Errorf("error lacks overwrite context: %s", log)
	}
	// The input file is untouched.
	data, err := os.ReadFile("../../testdata/golden/ir/sample.json")
	if err != nil || !bytes.Contains(data, []byte("exam_math_2026_mock_01")) {
		t.Errorf("input file was modified: %v", err)
	}
}

// TestCLICheckOnly validates inputs and resources without rendering.
func TestCLICheckOnly(t *testing.T) {
	requireTypst(t)
	args := []string{
		"-ir", "testdata/golden/ir/sample.json",
		"-layout", "testdata/golden/layout/student.json",
		"-assets", "testdata/golden/assets",
		"-check",
	}
	log, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("check: %v\n%s", err, log)
	}
	if !strings.Contains(log, "OK") {
		t.Errorf("check output: %s", log)
	}
}

// TestCLIAtomicOutput asserts a failed render leaves no output or temp file.
func TestCLIAtomicOutput(t *testing.T) {
	requireTypst(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out.pdf")
	// A layout that fails schema validation: nothing is written.
	bad := filepath.Join(dir, "bad-layout.json")
	if err := os.WriteFile(bad, []byte(`{"schema_version":"1.0.0","name":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := goldenArgs(out)
	args[3] = bad
	if _, err := runCLI(t, args...); err == nil {
		t.Fatal("invalid layout accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "bad-layout.json" {
			t.Errorf("failed render left %s behind", e.Name())
		}
	}
}
