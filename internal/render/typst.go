package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"examforge/internal/ir"
)

// Defaults and workspace layout. Every path Typst sees is relative to the
// workspace; the workspace is the Typst root and is removed after every render.
const (
	DefaultTypstPath      = "typst"
	DefaultTemplateDir    = "templates"
	DefaultPackageDir     = "third_party/typst-packages"
	DefaultFontDir        = "third_party/fonts"
	DefaultTimeout        = 30 * time.Second
	DefaultMaxAssetBytes  = 32 << 20 // 32 MiB per asset
	DefaultMaxTotalAssets = 64 << 20 // 64 MiB of asset bytes per render
	DefaultMaxAssetPixels = 40_000_000
	DefaultMaxAssetCopies = 4        // parallel asset copies per render
	DefaultMemoryLimit    = 2 << 30  // 2 GiB address space per Typst process (Linux)
	DefaultStderrLimit    = 64 << 10 // 64 KiB of stderr kept
	DefaultOutputLimit    = 256 << 20
	DefaultMeasureLimit   = 16 << 20
	maxTemplateFiles      = 64
	maxTemplateFileBytes  = 1 << 20
	maxSVGBytes           = 8 << 20
	creationTimestamp     = "0"
	fontFamily            = "Noto Sans CJK SC"
	typstVersionPrefix    = "typst 0.15."
	fontFilename          = "NotoSansCJK-Regular.ttc"
	fontSHA256            = "b76b0433203017ca80401b2ee0dd69350349871c4b19d504c34dbdd80541690a"
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
	// FontDir is the vendored font directory. Default: "third_party/fonts".
	FontDir string
	// SchemaDir holds exam-ir-v1.json and layout-spec-v1.json. Default: the
	// "schemas" directory beside TemplateDir. Validation is mandatory.
	SchemaDir string
	// Validator overrides schema compilation; it is useful for embedders that
	// carry their own compiled schemas.
	Validator *ir.Validator
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
	// MaxTotalAssetBytes caps all assets copied for one render.
	MaxTotalAssetBytes int64
	// MaxAssetPixels caps the decoded pixel count of one raster image.
	MaxAssetPixels int
	// MemoryLimitBytes caps each Typst process's address space on Linux through
	// prlimit. Default: 2 GiB. The fake CommandFactory used by tests is never
	// wrapped, so the injected process path is unchanged.
	MemoryLimitBytes int64
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
	cfg       TypstConfig
	sem       chan struct{}
	template  map[string]string
	resolver  AssetResolver
	validator *ir.Validator
	env       []string
	command   CommandFactory
}

func NewTypstRenderer(cfg TypstConfig) (*TypstRenderer, error) {
	if cfg.AssetResolver == nil {
		return nil, errors.New("render: TypstConfig.AssetResolver is required")
	}
	if cfg.MaxConcurrent < 0 || cfg.AssetConcurrency < 0 || cfg.Timeout < 0 ||
		cfg.MaxAssetBytes < 0 || cfg.MaxTotalAssetBytes < 0 || cfg.MaxAssetPixels < 0 ||
		cfg.MemoryLimitBytes < 0 || cfg.StderrLimit < 0 {
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
	if cfg.FontDir == "" {
		// Fonts sit beside the vendored package cache by default, so a caller
		// that relocated third_party gets both resources from one root.
		cfg.FontDir = filepath.Join(filepath.Dir(cfg.PackageDir), "fonts")
		if cfg.PackageDir == DefaultPackageDir {
			cfg.FontDir = DefaultFontDir
		}
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
	if cfg.MaxTotalAssetBytes == 0 {
		cfg.MaxTotalAssetBytes = DefaultMaxTotalAssets
	}
	if cfg.MaxAssetPixels == 0 {
		cfg.MaxAssetPixels = DefaultMaxAssetPixels
	}
	if cfg.MemoryLimitBytes == 0 {
		cfg.MemoryLimitBytes = DefaultMemoryLimit
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
	fontDir, err := canonicalDir(cfg.FontDir)
	if err != nil {
		return nil, fmt.Errorf("render: font directory %q: %w", cfg.FontDir, err)
	}
	cfg.FontDir = fontDir
	if err := verifyVendoredFont(cfg.FontDir); err != nil {
		return nil, err
	}
	validator := cfg.Validator
	if validator == nil {
		if cfg.SchemaDir == "" {
			cfg.SchemaDir = filepath.Join(filepath.Dir(cfg.TemplateDir), "schemas")
		}
		validator, err = ir.NewValidator(
			filepath.Join(cfg.SchemaDir, "exam-ir-v1.json"),
			filepath.Join(cfg.SchemaDir, "layout-spec-v1.json"))
		if err != nil {
			return nil, fmt.Errorf("render: compile schemas: %w", err)
		}
	}
	command := cfg.CommandFactory
	if command == nil {
		if _, err := exec.LookPath(cfg.TypstPath); err != nil {
			return nil, fmt.Errorf("render: typst executable %q: %w", cfg.TypstPath, err)
		}
		prlimit := ""
		if runtime.GOOS == "linux" && cfg.MemoryLimitBytes > 0 {
			path, err := exec.LookPath("prlimit")
			if err != nil {
				return nil, fmt.Errorf("render: prlimit is required for the %d-byte Typst memory limit: %w", cfg.MemoryLimitBytes, err)
			}
			prlimit = path
		}
		command = func(ctx context.Context, bin string, args []string) *exec.Cmd {
			if prlimit != "" {
				wrapped := append([]string{"--as=" + strconv.FormatInt(cfg.MemoryLimitBytes, 10), "--", bin}, args...)
				return exec.CommandContext(ctx, prlimit, wrapped...)
			}
			return exec.CommandContext(ctx, bin, args...)
		}
		if err := checkTypstEnvironment(command, cfg); err != nil {
			return nil, err
		}
	}
	return &TypstRenderer{
		cfg:       cfg,
		sem:       make(chan struct{}, cfg.MaxConcurrent),
		template:  files,
		resolver:  cfg.AssetResolver,
		validator: validator,
		env:       childEnv(),
		command:   command,
	}, nil
}

// checkTypstEnvironment rejects an incompatible Typst or an unreadable vendored
// font before the first render. Both probes use a deadline and bounded stderr.
func checkTypstEnvironment(command CommandFactory, cfg TypstConfig) error {
	probe := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := command(ctx, cfg.TypstPath, args)
		cmd.Env = childEnv()
		var out limitedBuffer
		out.limit = 1 << 20
		cmd.Stdout = &out
		cmd.Stderr = &out
		cmd.WaitDelay = waitDelay
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("%w: typst %s: %v: %s", ErrTemplate, args[0], err, out.text(""))
		}
		return out.text(""), nil
	}
	version, err := probe("--version")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(version, typstVersionPrefix) {
		return fmt.Errorf("%w: Typst version %q is unsupported; this renderer is verified against %s*", ErrTemplate, strings.TrimSpace(version), typstVersionPrefix)
	}
	fonts, err := probe("fonts", "--font-path", cfg.FontDir, "--ignore-system-fonts")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(fonts, "\n") {
		if strings.TrimSpace(line) == fontFamily {
			return nil
		}
	}
	return fmt.Errorf("%w: vendored font family %q was not found in %s", ErrTemplate, fontFamily, fontFilename)
}

// verifyVendoredFont pins the exact font file used for deterministic layout.
func verifyVendoredFont(dir string) error {
	fontPath := filepath.Join(dir, fontFilename)
	data, err := os.ReadFile(fontPath)
	if err != nil {
		return fmt.Errorf("render: vendored font %s: %v", fontFilename, pathOnly(err))
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != fontSHA256 {
		return fmt.Errorf("render: vendored font %s sha256 mismatch: got %s, want %s", fontFilename, got, fontSHA256)
	}
	return nil
}
