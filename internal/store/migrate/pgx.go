package migrate

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgxDB adapts a pgx transaction to the runner's DB interface.
type PgxDB struct{ Tx pgx.Tx }

func (d PgxDB) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := d.Tx.Exec(ctx, sql, args...)
	return err
}

func (d PgxDB) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	return d.Tx.Query(ctx, sql, args...)
}

func (d PgxDB) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return d.Tx.QueryRow(ctx, sql, args...)
}

// Up applies every pending migration in one transaction and commits. Any
// failure rolls back the whole run, including ledger rows.
func Up(ctx context.Context, pool *pgxpool.Pool, migrations []Migration) (applied []Applied, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	applied, err = Apply(ctx, PgxDB{Tx: tx}, migrations)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return applied, nil
}

// Current reads the ledger in a short transaction. It fails when the ledger
// table does not exist yet; call Up first.
func Current(ctx context.Context, pool *pgxpool.Pool, migrations []Migration) (status Status, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Status{}, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	status, err = Inspect(ctx, PgxDB{Tx: tx}, migrations)
	if err != nil {
		return Status{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Status{}, err
	}
	return status, nil
}
