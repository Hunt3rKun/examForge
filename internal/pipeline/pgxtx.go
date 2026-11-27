package pipeline

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"examforge/internal/store"
	"examforge/internal/store/sqlcgen"
)

// PGXRunner runs a submission transaction over a pgx pool.
type PGXRunner struct {
	Pool *pgxpool.Pool
}

var _ TxRunner = PGXRunner{}

type pgxScope struct {
	tx pgx.Tx
}

func (s pgxScope) Jobs() store.IngestJobs { return sqlcgen.New(s.tx) }
func (s pgxScope) Raw() pgx.Tx            { return s.tx }

// Run begins, calls fn and commits; any error or panic rolls back.
func (r PGXRunner) Run(ctx context.Context, fn func(ctx context.Context, scope TxScope) error) (err error) {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if err = fn(ctx, pgxScope{tx: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RiverClient adapts a River client to the enqueuer and canceller seams.
type RiverClient struct {
	Client *river.Client[pgx.Tx]
}

func (c RiverClient) InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return c.Client.InsertTx(ctx, tx, args, opts)
}

func (c RiverClient) JobCancel(ctx context.Context, jobID int64) (*rivertype.JobRow, error) {
	return c.Client.JobCancel(ctx, jobID)
}

// NewRiverClient builds a River client over the same pool the store uses.
// Workers are registered by the caller with river.AddWorker.
func NewRiverClient(pool *pgxpool.Pool, workers *river.Workers, cfg *river.Config) (*river.Client[pgx.Tx], error) {
	if cfg == nil {
		cfg = &river.Config{}
	}
	if workers != nil {
		cfg.Workers = workers
	}
	return river.NewClient[pgx.Tx](riverpgxv5.New(pool), cfg)
}

// RiverMigrate applies River's own schema. The application migrations never
// create or alter River tables.
func RiverMigrate(ctx context.Context, pool *pgxpool.Pool, targetVersion int) (*rivermigrate.MigrateResult, error) {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return nil, err
	}
	return migrator.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{TargetVersion: targetVersion})
}

// RiverVersion reports the highest applied River migration.
func RiverVersion(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return 0, err
	}
	versions, err := migrator.ExistingVersions(ctx)
	if err != nil {
		return 0, err
	}
	highest := 0
	for _, v := range versions {
		if v.Version > highest {
			highest = v.Version
		}
	}
	return highest, nil
}
