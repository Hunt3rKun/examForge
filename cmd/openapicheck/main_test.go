package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckOpenAPIAndExternalFragments(t *testing.T) {
	path := copyContract(t)
	if err := check(path); err != nil {
		t.Fatalf("check() = %v", err)
	}
}

func TestCheckRejectsBrokenExternalFragment(t *testing.T) {
	path := copyContract(t)
	mutateContract(t, path, "../schemas/exam-ir-v1.json#/$defs/blocks", "../schemas/exam-ir-v1.json#/$defs/missing")
	if err := check(path); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("check() error = %v, want a missing JSON Pointer error", err)
	}
}

func TestCheckRejectsRemoteReference(t *testing.T) {
	path := copyContract(t)
	mutateContract(t, path, "../schemas/exam-ir-v1.json#/$defs/blocks", "https://example.invalid/schema.json#/$defs/blocks")
	if err := check(path); err == nil || !strings.Contains(err.Error(), "remote references") {
		t.Fatalf("check() error = %v, want a remote-reference error", err)
	}
}

func TestCheckRejectsReferencePathTraversal(t *testing.T) {
	path := copyContract(t)
	mutateContract(t, path, "../schemas/exam-ir-v1.json#/$defs/blocks", "../../outside.json#/$defs/blocks")
	if err := check(path); err == nil || !strings.Contains(err.Error(), "path traversal") {
		t.Fatalf("check() error = %v, want a path-traversal error", err)
	}
}

func TestCheckRejectsBrokenLocalPointer(t *testing.T) {
	path := copyContract(t)
	mutateContract(t, path, "#/components/schemas/Error", "#/components/schemas/MissingError")
	if err := check(path); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("check() error = %v, want a missing local JSON Pointer error", err)
	}
}

func copyContract(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	tmp := t.TempDir()
	apiDir := filepath.Join(tmp, "api")
	schemaDir := filepath.Join(tmp, "schemas")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile := func(src, dst string) {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(apiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(filepath.Join(root, "api", "openapi.yaml"), filepath.Join(apiDir, "openapi.yaml"))
	for _, name := range []string{"exam-ir-v1.json", "layout-spec-v1.json"} {
		copyFile(filepath.Join(root, "schemas", name), filepath.Join(schemaDir, name))
	}
	return filepath.Join(apiDir, "openapi.yaml")
}

func mutateContract(t *testing.T, path, old, new string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, old) {
		t.Fatalf("contract does not contain mutation target %q", old)
	}
	text = strings.Replace(text, old, new, 1)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
