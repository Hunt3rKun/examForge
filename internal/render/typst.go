package render

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

// Defaults and workspace layout. Every path Typst sees is relative to the
// workspace; the workspace is the Typst root and is removed after every render.
const (
	DefaultTypstPath      = "typst"
	DefaultTemplateDir    = "templates"
	DefaultPackageDir     = "third_party/typst-packages"
	DefaultTimeout        = 30 * time.Second
	DefaultMaxAssetBytes  = 32 << 20 // 32 MiB per asset
	DefaultMaxAssetCopies = 4        // parallel asset copies per render
	DefaultStderrLimit    = 64 << 10 // 64 KiB of stderr kept
	DefaultOutputLimit    = 256 << 20
	DefaultMeasureLimit   = 16 << 20
	maxTemplateFiles      = 64
	maxTemplateFileBytes  = 1 << 20
	waitDelay             = 2 * time.Second

	TemplateEntry       = "exam.typ"
	PayloadFile         = "payload.json"
	OutputFile          = "out.pdf"
	WorkspacePackages   = "packages"     // workspace-relative Typst package cache
	WorkspaceNoPackages = "package-path" // empty: shadows any user package directory
	WorkspaceAssets     = "assets"

	// measureExpression is evaluated by `typst eval --in exam.typ` after layout
	// and returns the option widths the template published as metadata.
	measureExpression = "query(metadata).map(m => m.value)"
	// blackholeProxy makes an unexpected package download fail closed instead of
	// reaching the network.
	blackholeProxy = "http://127.0.0.1:9"
)

// TypstConfig configures a TypstRenderer. Zero means "use the default" for
// every numeric field; a negative value is rejected.
type TypstConfig struct {
	// TypstPath is the Typst executable. Default: "typst" found on PATH.
	TypstPath string
	// TemplateDir holds exam.typ and the .typ files it imports.
	TemplateDir string
	// PackageDir is the vendored Typst package cache root.
	PackageDir string
	// MaxConcurrent bounds concurrent renders. Default: runtime.NumCPU().
	MaxConcurrent int
	// Timeout bounds one render when the caller's context has no deadline.
	// A caller deadline is always obeyed.
	Timeout time.Duration
	// TempRoot is where per-render workspaces are created. Default: os.TempDir().
	TempRoot string
	// AssetResolver serves asset content. Required.
	AssetResolver AssetResolver
	// MaxAssetBytes caps the size of one asset.
	MaxAssetBytes int64
	// AssetConcurrency bounds parallel asset copies per render.
	AssetConcurrency int
	// StderrLimit caps the stderr kept from one subprocess.
	StderrLimit int64
	// CommandFactory creates the subprocess. It defaults to exec.CommandContext;
	// tests inject it to run fake or slow commands through the identical
	// semaphore, context, stderr, Wait and cleanup path.
	CommandFactory CommandFactory
}

// CommandFactory creates the process for one Typst invocation.
type CommandFactory func(ctx context.Context, bin string, args []string) *exec.Cmd

// TypstRenderer renders Exam IR to PDF through a bounded Typst subprocess.
// It is immutable after construction and safe for concurrent use.
type TypstRenderer struct {
	cfg      TypstConfig
	sem      chan struct{}
	template map[string]string
	resolver AssetResolver
	env      []string
	command  CommandFactory
}

func NewTypstRenderer(cfg TypstConfig) (*TypstRenderer, error) {
	if cfg.AssetResolver == nil {
		return nil, errors.New("render: TypstConfig.AssetResolver is required")
	}
	if cfg.MaxConcurrent < 0 || cfg.AssetConcurrency < 0 || cfg.Timeout < 0 || cfg.MaxAssetBytes < 0 || cfg.StderrLimit < 0 {
		return nil, fmt.Errorf("render: TypstConfig numeric fields must not be negative")
	}
	if cfg.TypstPath == "" {
		cfg.TypstPath = DefaultTypstPath
	}
	if cfg.TemplateDir == "" {
		cfg.TemplateDir = DefaultTemplateDir
	}
	if cfg.PackageDir == "" {
		cfg.PackageDir = DefaultPackageDir
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = runtime.NumCPU()
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxAssetBytes == 0 {
		cfg.MaxAssetBytes = DefaultMaxAssetBytes
	}
	if cfg.AssetConcurrency == 0 {
		cfg.AssetConcurrency = DefaultMaxAssetCopies
	}
	if cfg.StderrLimit == 0 {
		cfg.StderrLimit = DefaultStderrLimit
	}
	files, err := loadTemplateFiles(cfg.TemplateDir)
	if err != nil {
		return nil, err
	}
	if err := checkTemplateImports(TemplateEntry, files); err != nil {
		return nil, err
	}
	// Fail before the first render when the package cache is not exactly the
	// vendored MiTeX: missing, tampered or foreign files are all errors.
	if err := VerifyVendoredPackages(cfg.PackageDir); err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.PackageDir, err)
	}
	command := cfg.CommandFactory
	if command == nil {
		if _, err := exec.LookPath(cfg.TypstPath); err != nil {
			return nil, fmt.Errorf("render: typst executable %q: %w", cfg.TypstPath, err)
		}
		command = func(ctx context.Context, bin string, args []string) *exec.Cmd {
			return exec.CommandContext(ctx, bin, args...)
		}
	}
	return &TypstRenderer{
		cfg:      cfg,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
		template: files,
		resolver: cfg.AssetResolver,
		env:      childEnv(),
		command:  command,
	}, nil
}
