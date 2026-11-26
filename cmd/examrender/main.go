// Command examrender renders one Exam IR with one Layout Spec to a PDF.
//
//	examrender -ir <exam-ir.json> -layout <layout-spec.json> -assets <asset-root> -out <output.pdf>
//
// Layout policy lives only in the Layout Spec: there are no layout flags.
// Resource flags locate the schemas, template and vendored Typst packages.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"examforge/internal/ir"
	"examforge/internal/render"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "examrender:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		irPath      = flag.String("ir", "", "Exam IR JSON file (required)")
		layoutPath  = flag.String("layout", "", "Layout Spec JSON file (required)")
		assetsRoot  = flag.String("assets", "", "asset root directory with manifest.json (required)")
		outPath     = flag.String("out", "", "output PDF file (required)")
		schemaDir   = flag.String("schemas", "schemas", "directory holding exam-ir-v1.json and layout-spec-v1.json")
		templateDir = flag.String("templates", render.DefaultTemplateDir, "template directory")
		packageDir  = flag.String("packages", render.DefaultPackageDir, "vendored Typst package cache root")
		typstPath   = flag.String("typst", render.DefaultTypstPath, "Typst executable")
		timeout     = flag.Duration("timeout", render.DefaultTimeout, "per-render timeout when the caller has no deadline")
		checkOnly   = flag.Bool("check", false, "validate inputs and resources without rendering")
	)
	flag.Parse()
	if *irPath == "" || *layoutPath == "" || *assetsRoot == "" || (*outPath == "" && !*checkOnly) {
		flag.Usage()
		return errors.New("-ir, -layout, -assets and -out are required")
	}

	// Inputs and output must not overwrite each other.
	if err := checkDistinct(*irPath, *layoutPath, *assetsRoot, *outPath); err != nil {
		return err
	}

	validator, err := ir.NewValidator(
		filepath.Join(*schemaDir, "exam-ir-v1.json"),
		filepath.Join(*schemaDir, "layout-spec-v1.json"))
	if err != nil {
		return err
	}
	exam, err := loadExam(validator, *irPath)
	if err != nil {
		return err
	}
	layout, err := loadLayout(validator, *layoutPath)
	if err != nil {
		return err
	}
	resolver, err := render.NewLocalAssetResolver(*assetsRoot)
	if err != nil {
		return err
	}
	renderer, err := render.NewTypstRenderer(render.TypstConfig{
		TypstPath:     *typstPath,
		TemplateDir:   *templateDir,
		PackageDir:    *packageDir,
		SchemaDir:     *schemaDir,
		Validator:     validator,
		Timeout:       *timeout,
		AssetResolver: resolver,
	})
	if err != nil {
		return err
	}
	if *checkOnly {
		if _, err := render.Precompile(exam, layout); err != nil {
			return err
		}
		fmt.Println("examrender: inputs and resources OK")
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	artifact, err := renderer.Render(ctx, exam, layout)
	if err != nil {
		return err
	}
	for _, w := range artifact.Warnings {
		fmt.Fprintf(os.Stderr, "examrender: warning %s %s: %s\n", w.Code, w.Path, w.Message)
	}
	if err := writeAtomic(*outPath, artifact.Bytes); err != nil {
		return err
	}
	fmt.Printf("examrender: wrote %s (%d bytes, %s)\n", *outPath, len(artifact.Bytes), artifact.MIMEType)
	return nil
}

// checkDistinct rejects an output path that would overwrite an input file or
// the asset manifest, comparing resolved absolute paths.
func checkDistinct(irPath, layoutPath, assetsRoot, outPath string) error {
	if outPath == "" {
		return nil
	}
	outResolved, err := filepath.EvalSymlinks(outPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resolve output: %w", err)
	}
	outAbs, err := filepath.Abs(outPath)
	if err != nil {
		return err
	}
	if outResolved != "" {
		outAbs = outResolved
	}
	inputs := []string{irPath, layoutPath, filepath.Join(assetsRoot, render.AssetManifestFile)}
	for _, in := range inputs {
		abs, err := filepath.EvalSymlinks(in)
		if err != nil {
			return fmt.Errorf("resolve input %q: %w", in, err)
		}
		if abs == outAbs {
			return fmt.Errorf("output %q would overwrite input %q", outPath, in)
		}
	}
	return nil
}

func loadExam(v *ir.Validator, path string) (ir.ExamDocumentIR, error) {
	f, err := os.Open(path)
	if err != nil {
		return ir.ExamDocumentIR{}, err
	}
	defer f.Close()
	exam, err := v.DecodeExamIR(f)
	if err != nil {
		return ir.ExamDocumentIR{}, fmt.Errorf("exam IR: %w", err)
	}
	return exam, nil
}

func loadLayout(v *ir.Validator, path string) (ir.LayoutSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return ir.LayoutSpec{}, err
	}
	defer f.Close()
	layout, err := v.DecodeLayoutSpec(f)
	if err != nil {
		return ir.LayoutSpec{}, fmt.Errorf("layout spec: %w", err)
	}
	return layout, nil
}

// writeAtomic writes data to a temporary file beside the target and renames it
// into place, so a failed render never leaves a half-written PDF. The output
// parent directory must already exist.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".examrender-*.tmp")
	if err != nil {
		return fmt.Errorf("create output temporary file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write output: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("set output permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync output: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("rename output into place: %w", err)
	}
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}
