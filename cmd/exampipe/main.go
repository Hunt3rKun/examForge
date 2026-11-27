// Command exampipe is the minimal slice-2 ingest entry point: it uploads a
// local PDF, queues its ingest job in the same transaction, consumes the job
// with the mock parser, assembles the stored exam back into Exam IR, and
// renders it to PDF through the S3-backed asset resolver.
//
// Configuration comes from the environment only; there are no secret flags:
//
//	EXAMFORGE_DATABASE_URL          PostgreSQL connection URL
//	EXAMFORGE_S3_ENDPOINT           S3-protocol base URL
//	EXAMFORGE_S3_BUCKET             bucket name
//	EXAMFORGE_S3_REGION             region
//	EXAMFORGE_S3_ACCESS_KEY_ID      access key
//	EXAMFORGE_S3_SECRET_ACCESS_KEY  secret key
//	EXAMFORGE_S3_FORCE_PATH_STYLE   must be "true"
//
// The command connects to real services, so it is an operator tool and never
// part of the offline verify gate.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"examforge"
	"examforge/internal/ir"
	"examforge/internal/parser"
	"examforge/internal/pipeline"
	"examforge/internal/render"
	"examforge/internal/storage"
	"examforge/internal/store"
	"examforge/internal/store/migrate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "exampipe:", err)
		os.Exit(1)
	}
}

type config struct {
	pdfPath     string
	layoutPath  string
	outPath     string
	schemaDir   string
	templateDir string
	packageDir  string
	typstPath   string
	timeout     time.Duration
	migrate     bool
}

func run() error {
	cfg := config{}
	flag.StringVar(&cfg.pdfPath, "pdf", "", "source PDF file (required)")
	flag.StringVar(&cfg.layoutPath, "layout", "testdata/golden/layout/student.json", "Layout Spec JSON")
	flag.StringVar(&cfg.outPath, "out", "", "output PDF file (required)")
	flag.StringVar(&cfg.schemaDir, "schemas", "schemas", "directory holding the JSON schemas")
	flag.StringVar(&cfg.templateDir, "templates", render.DefaultTemplateDir, "Typst template directory")
	flag.StringVar(&cfg.packageDir, "packages", render.DefaultPackageDir, "vendored Typst package cache root")
	flag.StringVar(&cfg.typstPath, "typst", render.DefaultTypstPath, "Typst executable")
	flag.DurationVar(&cfg.timeout, "timeout", 2*time.Minute, "overall command timeout")
	flag.BoolVar(&cfg.migrate, "migrate", true, "apply app and River migrations before running")
	flag.Parse()
	if cfg.pdfPath == "" || cfg.outPath == "" {
		flag.Usage()
		return errors.New("-pdf and -out are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	env, err := readEnv()
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, env.databaseURL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	objects, err := storage.NewS3Store(storage.S3Config{
		Endpoint:        env.s3Endpoint,
		Region:          env.s3Region,
		Bucket:          env.s3Bucket,
		AccessKeyID:     env.s3AccessKeyID,
		SecretAccessKey: env.s3SecretAccessKey,
		ForcePathStyle:  env.s3ForcePathStyle,
	})
	if err != nil {
		return err
	}

	if cfg.migrate {
		migrations, err := migrate.Load(examforge.Migrations)
		if err != nil {
			return err
		}
		if _, err := migrate.Up(ctx, pool, migrations); err != nil {
			return fmt.Errorf("app migrations: %w", err)
		}
		status, err := migrate.Current(ctx, pool, migrations)
		if err != nil {
			return fmt.Errorf("app schema status: %w", err)
		}
		// TargetVersion 0 means "apply everything pending"; -1 is the
		// down-direction "remove all" sentinel.
		riverVersion, err := pipeline.RiverMigrate(ctx, pool, 0)
		if err != nil {
			return fmt.Errorf("river migrations: %w", err)
		}
		fmt.Printf("exampipe: app schema at version %d, river schema at version %d\n",
			lastApplied(status.Applied), highestVersion(riverVersion))
	}

	validator, err := ir.NewValidator(filepath.Join(cfg.schemaDir, "exam-ir-v1.json"), filepath.Join(cfg.schemaDir, "layout-spec-v1.json"))
	if err != nil {
		return err
	}
	assembler, err := store.NewAssembler(validator)
	if err != nil {
		return err
	}
	repo := store.NewRepository(pool)
	persister, err := store.NewPersister(repo, assembler)
	if err != nil {
		return err
	}
	adapter, err := parser.NewMockParserAdapter()
	if err != nil {
		return fmt.Errorf("mock parser: %w", err)
	}

	riverClient, err := pipeline.NewRiverClient(pool, nil, nil)
	if err != nil {
		return err
	}
	submitter, err := pipeline.NewSubmitter(pipeline.PGXRunner{Pool: pool}, pipeline.RiverClient{Client: riverClient}, objects, pipeline.SubmitConfig{})
	if err != nil {
		return err
	}
	worker, err := pipeline.NewIngestWorker(repo, persister, objects, adapter, validator,
		pipeline.RiverClient{Client: riverClient}, pipeline.WorkerConfig{Secrets: []string{env.s3SecretAccessKey}})
	if err != nil {
		return err
	}

	pdf, err := os.ReadFile(cfg.pdfPath)
	if err != nil {
		return err
	}
	sub, err := submitter.Submit(ctx, bytes.NewReader(pdf), int64(len(pdf)))
	if err != nil {
		return fmt.Errorf("submit: %w", err)
	}
	fmt.Printf("exampipe: job %s queued as river job %d\n", sub.JobID, sub.RiverJobID)

	// Consume the queued job through the identical worker path. A real
	// deployment runs a River client instead.
	job := &river.Job[pipeline.IngestArgs]{
		JobRow: &rivertype.JobRow{ID: sub.RiverJobID, Kind: pipeline.IngestArgs{}.Kind()},
		Args:   pipeline.IngestArgs{JobID: sub.JobID.String()},
	}
	if err := worker.Work(ctx, job); err != nil {
		return fmt.Errorf("work: %w", err)
	}
	row, err := repo.GetJob(ctx, uuidToPG(sub.JobID))
	if err != nil {
		return err
	}
	if row.State != store.JobPersisted || !row.ExamID.Valid {
		return fmt.Errorf("job %s ended in state %q", sub.JobID, row.State)
	}
	examID := uuid.UUID(row.ExamID.Bytes)
	fmt.Printf("exampipe: exam %s persisted\n", examID)

	var exam ir.ExamDocumentIR
	if err := repo.Read(ctx, func(ctx context.Context, rd store.Reader) error {
		var err error
		exam, err = assembler.Assemble(ctx, rd, examID)
		return err
	}); err != nil {
		return fmt.Errorf("assemble: %w", err)
	}

	layoutData, err := os.ReadFile(cfg.layoutPath)
	if err != nil {
		return err
	}
	layout, err := validator.DecodeLayoutSpec(bytes.NewReader(layoutData))
	if err != nil {
		return fmt.Errorf("layout spec: %w", err)
	}

	resolver, err := storage.NewS3AssetResolver(objects, repo, storage.MaxAssetBytes)
	if err != nil {
		return err
	}
	renderer, err := render.NewTypstRenderer(render.TypstConfig{
		TypstPath:     cfg.typstPath,
		TemplateDir:   cfg.templateDir,
		PackageDir:    cfg.packageDir,
		SchemaDir:     cfg.schemaDir,
		Validator:     validator,
		Timeout:       60 * time.Second,
		AssetResolver: resolver.ForRender(),
	})
	if err != nil {
		return err
	}
	artifact, err := renderer.Render(ctx, exam, layout)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if !bytes.HasPrefix(artifact.Bytes, []byte("%PDF-")) {
		return errors.New("renderer output does not start with %PDF-")
	}
	if err := os.WriteFile(cfg.outPath, artifact.Bytes, 0o644); err != nil {
		return err
	}
	fmt.Printf("exampipe: wrote %s (%d bytes)\n", cfg.outPath, len(artifact.Bytes))
	return nil
}

// lastApplied reports the highest ledger version after migrate.Up, or 0 when
// nothing was pending (the ledger was already current).
func lastApplied(applied []migrate.Applied) int {
	if len(applied) == 0 {
		return 0
	}
	return applied[len(applied)-1].Version
}

func highestVersion(result *rivermigrate.MigrateResult) int {
	highest := 0
	if result == nil {
		return highest
	}
	for _, applied := range result.Versions {
		if applied.Version > highest {
			highest = applied.Version
		}
	}
	return highest
}

type envConfig struct {
	databaseURL       string
	s3Endpoint        string
	s3Bucket          string
	s3Region          string
	s3AccessKeyID     string
	s3SecretAccessKey string
	s3ForcePathStyle  bool
}

func readEnv() (envConfig, error) {
	env := envConfig{
		databaseURL:       os.Getenv("EXAMFORGE_DATABASE_URL"),
		s3Endpoint:        os.Getenv("EXAMFORGE_S3_ENDPOINT"),
		s3Bucket:          os.Getenv("EXAMFORGE_S3_BUCKET"),
		s3Region:          os.Getenv("EXAMFORGE_S3_REGION"),
		s3AccessKeyID:     os.Getenv("EXAMFORGE_S3_ACCESS_KEY_ID"),
		s3SecretAccessKey: os.Getenv("EXAMFORGE_S3_SECRET_ACCESS_KEY"),
	}
	pathStyle, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("EXAMFORGE_S3_FORCE_PATH_STYLE")))
	if err != nil || !pathStyle {
		return envConfig{}, errors.New("EXAMFORGE_S3_FORCE_PATH_STYLE must be true; virtual-hosted addressing is not supported")
	}
	env.s3ForcePathStyle = true
	var missing []string
	for name, value := range map[string]string{
		"EXAMFORGE_DATABASE_URL":         env.databaseURL,
		"EXAMFORGE_S3_ENDPOINT":          env.s3Endpoint,
		"EXAMFORGE_S3_BUCKET":            env.s3Bucket,
		"EXAMFORGE_S3_REGION":            env.s3Region,
		"EXAMFORGE_S3_ACCESS_KEY_ID":     env.s3AccessKeyID,
		"EXAMFORGE_S3_SECRET_ACCESS_KEY": env.s3SecretAccessKey,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return envConfig{}, fmt.Errorf("missing environment variables: %s", strings.Join(missing, ", "))
	}
	return env, nil
}

func uuidToPG(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte(id), Valid: true}
}
