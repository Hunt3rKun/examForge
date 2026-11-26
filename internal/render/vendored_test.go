package render

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const testPackageDir = "../../third_party/typst-packages"

// copyPackage copies the vendored package into a private directory so a test
// can tamper with it without touching the repository.
func copyPackage(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(testPackageDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(testPackageDir, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestVerifyVendoredPackages pins the integrity contract of the package cache.
func TestVerifyVendoredPackages(t *testing.T) {
	if err := VerifyVendoredPackages(testPackageDir); err != nil {
		t.Fatalf("repository package cache: %v", err)
	}
	t.Run("missing file", func(t *testing.T) {
		dir := copyPackage(t)
		if err := os.Remove(filepath.Join(dir, MitexDir, "mitex.wasm")); err != nil {
			t.Fatal(err)
		}
		if err := VerifyVendoredPackages(dir); !errors.Is(err, ErrPackage) {
			t.Errorf("err = %v, want ErrPackage", err)
		}
	})
	t.Run("tampered file", func(t *testing.T) {
		dir := copyPackage(t)
		p := filepath.Join(dir, MitexDir, "mitex.typ")
		if err := os.WriteFile(p, []byte("// tampered\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := VerifyVendoredPackages(dir); !errors.Is(err, ErrPackage) {
			t.Errorf("err = %v, want ErrPackage", err)
		}
	})
	t.Run("extra file", func(t *testing.T) {
		dir := copyPackage(t)
		if err := os.WriteFile(filepath.Join(dir, MitexDir, "extra.typ"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := VerifyVendoredPackages(dir); !errors.Is(err, ErrPackage) {
			t.Errorf("err = %v, want ErrPackage", err)
		}
	})
	t.Run("foreign package", func(t *testing.T) {
		dir := copyPackage(t)
		p := filepath.Join(dir, "preview", "other", "1.0.0")
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "lib.typ"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := VerifyVendoredPackages(dir); !errors.Is(err, ErrPackage) {
			t.Errorf("err = %v, want ErrPackage", err)
		}
	})
	t.Run("wrong version directory", func(t *testing.T) {
		dir := copyPackage(t)
		if err := os.Rename(filepath.Join(dir, MitexDir), filepath.Join(dir, "preview", "mitex", "0.2.6")); err != nil {
			t.Fatal(err)
		}
		if err := VerifyVendoredPackages(dir); !errors.Is(err, ErrPackage) {
			t.Errorf("err = %v, want ErrPackage", err)
		}
	})
	t.Run("symlink entry", func(t *testing.T) {
		dir := copyPackage(t)
		p := filepath.Join(dir, MitexDir, "lib.typ")
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "lib.typ")
		if err := os.WriteFile(outside, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, p); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := VerifyVendoredPackages(dir); !errors.Is(err, ErrPackage) {
			t.Errorf("err = %v, want ErrPackage", err)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		if err := VerifyVendoredPackages(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, ErrPackage) {
			t.Errorf("err = %v, want ErrPackage", err)
		}
	})
}

// TestStagePackageLayout asserts the staged cache keeps the full
// preview/mitex/<version> path, so the workspace itself is a valid cache root.
func TestStagePackageLayout(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "packages")
	if err := stagePackage(testPackageDir, dst); err != nil {
		t.Fatal(err)
	}
	if err := VerifyVendoredPackages(dst); err != nil {
		t.Fatalf("staged cache: %v", err)
	}
	for name := range mitexFiles {
		info, err := os.Stat(filepath.Join(dst, MitexDir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("staged %s: %v", name, err)
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Errorf("staged %s is writable: %v", name, info.Mode())
		}
	}
}

// TestCheckTemplateImports pins the import allowlist.
func TestCheckTemplateImports(t *testing.T) {
	good := map[string]string{TemplateEntry: `#import "` + MitexImport + `": mi, mitex`}
	if err := checkTemplateImports(TemplateEntry, good); err != nil {
		t.Errorf("pinned import rejected: %v", err)
	}
	cases := []map[string]string{
		{TemplateEntry: `#import "@preview/mitex:0.2.6": mi`},
		{TemplateEntry: `#import "@preview/cetz:0.3.0": canvas`},
		{TemplateEntry: `#import "` + MitexImport + `": mi`, "other.typ": `#import "@preview/oxifmt:1.0.0": fmt`},
		{TemplateEntry: `#let x = 1`},
		{},
	}
	for i, files := range cases {
		if err := checkTemplateImports(TemplateEntry, files); !errors.Is(err, ErrTemplate) {
			t.Errorf("case %d: err = %v, want ErrTemplate", i, err)
		}
	}
}

// TestNewTypstRendererGuards covers constructor validation.
func TestNewTypstRendererGuards(t *testing.T) {
	resolver, err := NewLocalAssetResolver(testGoldenAssets)
	if err != nil {
		t.Fatal(err)
	}
	base := TypstConfig{TemplateDir: "../../templates", PackageDir: testPackageDir, AssetResolver: resolver}
	if _, err := NewTypstRenderer(TypstConfig{}); err == nil {
		t.Error("missing AssetResolver accepted")
	}
	for _, mutate := range []func(*TypstConfig){
		func(c *TypstConfig) { c.MaxConcurrent = -1 },
		func(c *TypstConfig) { c.AssetConcurrency = -1 },
		func(c *TypstConfig) { c.Timeout = -1 },
		func(c *TypstConfig) { c.MaxAssetBytes = -1 },
		func(c *TypstConfig) { c.StderrLimit = -1 },
	} {
		cfg := base
		mutate(&cfg)
		if _, err := NewTypstRenderer(cfg); err == nil {
			t.Error("negative numeric field accepted")
		}
	}
	cfg := base
	cfg.PackageDir = filepath.Join(t.TempDir(), "absent")
	if _, err := NewTypstRenderer(cfg); !errors.Is(err, ErrPackage) {
		t.Errorf("bad package dir: err = %v, want ErrPackage", err)
	}
	cfg = base
	cfg.TemplateDir = t.TempDir()
	if _, err := NewTypstRenderer(cfg); !errors.Is(err, ErrTemplate) {
		t.Errorf("empty template dir: err = %v, want ErrTemplate", err)
	}
}
