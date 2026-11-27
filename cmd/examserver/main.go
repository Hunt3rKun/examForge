package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"examforge"
	"examforge/internal/api"
	"examforge/internal/ir"
	"examforge/internal/parser"
	"examforge/internal/parser/cloud"
	"examforge/internal/pipeline"
	"examforge/internal/render"
	"examforge/internal/storage"
	"examforge/internal/store"
	"examforge/internal/store/migrate"
	"examforge/internal/vision/clean"
	"examforge/internal/vision/raster"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "examserver:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := readConfig()
	if err != nil {
		return err
	}
	// Parser cloud credentials are validated in readConfig before any DB/S3
	// construction or connection attempt.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	objects, err := storage.NewS3Store(storage.S3Config{Endpoint: cfg.s3Endpoint, Region: cfg.s3Region, Bucket: cfg.s3Bucket, AccessKeyID: cfg.s3AccessKeyID, SecretAccessKey: cfg.s3SecretAccessKey, ForcePathStyle: true})
	if err != nil {
		return fmt.Errorf("configure object storage: %w", err)
	}
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	poolOpen := true
	defer func() {
		if poolOpen {
			pool.Close()
		}
	}()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	probe := dependencyProbe{repo: store.NewRepository(pool), objects: objects}
	probeCtx, probeCancel := context.WithTimeout(ctx, cfg.readyTimeout)
	err = probe.Ping(probeCtx)
	probeCancel()
	if err != nil {
		return fmt.Errorf("probe dependencies: %w", err)
	}
	migrations, err := migrate.Load(examforge.Migrations)
	if err != nil {
		return err
	}
	status, err := migrate.Current(ctx, pool, migrations)
	if err != nil {
		return fmt.Errorf("inspect application migrations: %w", err)
	}
	if len(status.Pending) > 0 {
		return fmt.Errorf("application schema has %d pending migration(s); run the migration operator command", len(status.Pending))
	}
	if len(status.Applied) != len(migrations) {
		return errors.New("application schema ledger is incomplete")
	}
	riverVersion, err := pipeline.RiverVersion(ctx, pool)
	if err != nil {
		return fmt.Errorf("inspect River schema: %w", err)
	}
	if riverVersion <= 0 {
		return errors.New("river schema has no applied migrations; run the River migration operator command")
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
	adapter, err := buildParser(cfg, validator)
	if err != nil {
		return err
	}
	workers := river.NewWorkers()
	worker, err := pipeline.NewIngestWorker(repo, persister, objects, adapter, validator, nil, pipeline.WorkerConfig{Secrets: []string{cfg.s3SecretAccessKey}, JobTimeout: cfg.jobTimeout})
	if err != nil {
		return err
	}
	if err := river.AddWorkerSafely(workers, worker); err != nil {
		return err
	}
	riverCfg := &river.Config{}
	if cfg.runWorker {
		riverCfg.Queues = map[string]river.QueueConfig{"ingest": {MaxWorkers: 4}}
	}
	riverClient, err := pipeline.NewRiverClient(pool, workers, riverCfg)
	if err != nil {
		return err
	}
	// River remains the producer even when this process does not consume jobs.
	submitter, err := pipeline.NewSubmitter(pipeline.PGXRunner{Pool: pool}, pipeline.RiverClient{Client: riverClient}, objects, pipeline.SubmitConfig{})
	if err != nil {
		return err
	}
	resolver, err := storage.NewS3AssetResolver(objects, repo, storage.MaxAssetBytes)
	if err != nil {
		return err
	}
	renderer, err := render.NewTypstRenderer(render.TypstConfig{TypstPath: cfg.typstPath, TemplateDir: cfg.templateDir, PackageDir: cfg.packageDir, SchemaDir: cfg.schemaDir, Validator: validator, Timeout: cfg.renderTimeout, AssetResolver: resolver})
	if err != nil {
		return err
	}
	domain := api.NewStoreDomain(repo, assembler, objects, validator)
	apiServer, handler := api.NewServer(api.Dependencies{Submitter: submitter, Domain: domain, Objects: objects, Renderer: renderer, Validator: validator, Probe: probe, Logger: slog.Default(), Config: api.Config{AllowedOrigins: cfg.allowedOrigins, JSONLimit: cfg.jsonLimit, UploadLimit: cfg.uploadLimit, SourceLimit: cfg.sourceLimit, AssetLimit: cfg.assetLimit, RequestTimeout: cfg.requestTimeout, UploadTimeout: cfg.uploadTimeout, RenderTimeout: cfg.renderTimeout, MaxConcurrent: cfg.maxConcurrent, MaxUploads: cfg.maxUploads, MaxRenders: cfg.maxRenders, ReadyTimeout: cfg.readyTimeout}})
	httpServer := &http.Server{Addr: cfg.bindAddress, Handler: handler, ReadHeaderTimeout: cfg.readHeaderTimeout, IdleTimeout: cfg.idleTimeout}
	listener, err := net.Listen("tcp", cfg.bindAddress)
	if err != nil {
		return fmt.Errorf("listen HTTP: %w", err)
	}
	var riverStopper riverStopper
	if cfg.runWorker {
		if err := riverClient.Start(context.Background()); err != nil {
			_ = listener.Close()
			return fmt.Errorf("start River worker: %w", err)
		}
		riverStopper.client = riverClient
	}
	serveErr := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()
	select {
	case err := <-serveErr:
		if err != nil {
			_ = shutdownWithDependencies(context.Background(), cfg.shutdownTimeout, httpServer, riverStopper, pool, apiServer.SetReady)
			poolOpen = false
			return fmt.Errorf("serve HTTP: %w", err)
		}
	case <-ctx.Done():
	}
	shutdownErr := shutdownWithDependencies(context.Background(), cfg.shutdownTimeout, httpServer, riverStopper, pool, apiServer.SetReady)
	poolOpen = false
	return shutdownErr
}

type riverStopper struct {
	client interface{ Stop(context.Context) error }
}

func (r riverStopper) Stop(ctx context.Context) error {
	if r.client == nil {
		return nil
	}
	return r.client.Stop(ctx)
}

type dependencyProbe struct {
	repo    interface{ Ping(context.Context) error }
	objects storage.ObjectStore
}

func (p dependencyProbe) Ping(ctx context.Context) error {
	if p.repo == nil || p.objects == nil {
		return errors.New("dependency unavailable")
	}
	if err := p.repo.Ping(ctx); err != nil {
		return err
	}
	_, err := p.objects.Stat(ctx, "examforge-healthcheck-canary")
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}
func shutdownWithDependencies(parent context.Context, timeout time.Duration, srv *http.Server, rc stopper, pool *pgxpool.Pool, ready func(bool)) error {
	var poolCloser closer
	if pool != nil {
		poolCloser = pool
	}
	return (&lifecycle{ready: ready, http: srv, river: rc, pool: poolCloser, timeout: timeout}).shutdown(parent)
}

func buildParser(cfg config, validator *ir.Validator) (parser.ParserAdapter, error) {
	if cfg.parserMode == "mock" {
		return parser.NewMockParserAdapter()
	}
	baseURL, apiKey, model, err := cloud.EnvConfig()
	if err != nil {
		return nil, err
	}
	rasterizer, err := raster.NewMuPDFRasterizer(raster.Config{})
	if err != nil {
		return nil, fmt.Errorf("rasterizer: %w", err)
	}
	cleaner, err := clean.NewHSVCleaner(clean.DefaultConfig())
	if err != nil {
		return nil, fmt.Errorf("cleaner: %w", err)
	}
	return cloud.NewCloudAPIParser(cloud.Config{BaseURL: baseURL, APIKey: apiKey, Model: model, Rasterizer: rasterizer, Cleaner: cleaner, Validator: validator, TotalParseTimeout: cfg.parseBudget, JobTimeout: cfg.jobTimeout})
}
