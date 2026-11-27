// Package migrate applies the ExamForge application schema migrations. River's
// tables are managed by rivermigrate and never appear here.
//
// One call runs everything in the caller's transaction under a PostgreSQL
// advisory lock: ledger table, history check, pending migrations (each in a
// savepoint), then a post-check that the ledger equals the embedded prefix.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LedgerTable is the application migration ledger. It is created by the
// runner rather than by a migration file, so it is never part of the set.
const LedgerTable = "examforge_schema_migrations"

// lockKey namespaces the advisory lock. It is unrelated to River's advisory
// lock prefix so the two migration tools never block each other.
const lockKey int64 = 0x4558414D46475A31 // "EXAMFGZ1"

// nonTransactionalMarker marks a migration that cannot run inside a
// transaction. No current migration uses it; Load rejects one so it gets an
// explicit out-of-transaction path instead of being wrapped silently.
const nonTransactionalMarker = "-- examforge:non-transactional"

var (
	// ErrLayout means the embedded migration files do not form an ordered,
	// gap-free sequence starting at version 1.
	ErrLayout = errors.New("migration set is invalid")
	// ErrHistory means the ledger does not match the embedded migrations.
	ErrHistory = errors.New("applied migrations do not match the embedded set")
	// ErrChecksum means an already-applied migration file was modified.
	ErrChecksum = errors.New("applied migration has a different checksum")
	// ErrUnsupported means a migration needs a mode the runner does not have.
	ErrUnsupported = errors.New("unsupported migration")
	// ErrLock means the advisory lock could not be taken.
	ErrLock = errors.New("migration advisory lock failed")
)

var fileRe = regexp.MustCompile(`^([0-9]{4})_([a-z0-9_]+)\.sql$`)

// Migration is one embedded SQL file.
type Migration struct {
	Version int
	Name    string // file stem without the version, e.g. "assets"
	SHA256  string // lowercase hex of the file bytes
	SQL     string
}

// Applied is one ledger row.
type Applied struct {
	Version int
	Name    string
	SHA256  string
}

// DB is the transaction handle the runner needs. PgxDB adapts a pgx.Tx to it.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) error
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

// Rows iterates a result set.
type Rows interface {
	Close()
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// Row scans a single result row.
type Row interface {
	Scan(dest ...any) error
}

// Status is the ledger versus the embedded set, without applying anything.
type Status struct {
	Applied []Applied
	Pending []Migration
}

// Load reads and pre-validates the embedded migration set: file names,
// strictly increasing versions from 1 with no gaps, and no unsupported
// statements. The returned slice is ordered by version.
func Load(fsys fs.FS) ([]Migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLayout, err)
	}
	migrations := make([]Migration, 0, len(names))
	for _, name := range names {
		m := fileRe.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("%w: file %q must be NNNN_lower_snake.sql", ErrLayout, name)
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("%w: file %q: %v", ErrLayout, name, err)
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("%w: read %q: %v", ErrLayout, name, err)
		}
		sql := string(data)
		if strings.Contains(sql, nonTransactionalMarker) {
			return nil, fmt.Errorf("%w: %s is marked non-transactional and the runner only applies migrations inside one transaction", ErrUnsupported, name)
		}
		sum := sha256.Sum256(data)
		migrations = append(migrations, Migration{
			Version: version,
			Name:    m[2],
			SHA256:  hex.EncodeToString(sum[:]),
			SQL:     sql,
		})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	for i, m := range migrations {
		if m.Version != i+1 {
			return nil, fmt.Errorf("%w: expected version %d, found %d", ErrLayout, i+1, m.Version)
		}
	}
	if len(migrations) == 0 {
		return nil, fmt.Errorf("%w: no migrations found", ErrLayout)
	}
	return migrations, nil
}

// Inspect reads the ledger and compares it with the embedded set. It requires
// the ledger to exist; use Apply to create it.
func Inspect(ctx context.Context, db DB, migrations []Migration) (Status, error) {
	applied, err := readLedger(ctx, db)
	if err != nil {
		return Status{}, err
	}
	if err := checkHistory(applied, migrations); err != nil {
		return Status{}, err
	}
	return Status{Applied: applied, Pending: migrations[len(applied):]}, nil
}

// Apply runs inside the caller's open transaction (the caller commits or
// rolls back) and returns the ledger after the run.
func Apply(ctx context.Context, db DB, migrations []Migration) ([]Applied, error) {
	if err := db.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLock, err)
	}
	if err := db.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+LedgerTable+` (
		version    integer  PRIMARY KEY,
		name       text     NOT NULL,
		sha256     char(64) NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("create %s: %w", LedgerTable, err)
	}
	applied, err := readLedger(ctx, db)
	if err != nil {
		return nil, err
	}
	if err := checkHistory(applied, migrations); err != nil {
		return nil, err
	}
	for _, m := range migrations[len(applied):] {
		savepoint := fmt.Sprintf("examforge_migration_%d", m.Version)
		if err := db.Exec(ctx, "SAVEPOINT "+savepoint); err != nil {
			return nil, fmt.Errorf("migrate version %d (%s): %w", m.Version, m.Name, err)
		}
		if err := db.Exec(ctx, m.SQL); err != nil {
			// Roll the statement group back; the caller still decides whether
			// the outer transaction survives (it must not commit a partial set).
			_ = db.Exec(ctx, "ROLLBACK TO SAVEPOINT "+savepoint)
			return nil, fmt.Errorf("migrate version %d (%s): %w", m.Version, m.Name, err)
		}
		if err := db.Exec(ctx, "INSERT INTO "+LedgerTable+" (version, name, sha256) VALUES ($1, $2, $3)",
			m.Version, m.Name, m.SHA256); err != nil {
			_ = db.Exec(ctx, "ROLLBACK TO SAVEPOINT "+savepoint)
			return nil, fmt.Errorf("record migration version %d (%s): %w", m.Version, m.Name, err)
		}
		if err := db.Exec(ctx, "RELEASE SAVEPOINT "+savepoint); err != nil {
			return nil, fmt.Errorf("migrate version %d (%s): %w", m.Version, m.Name, err)
		}
	}
	final, err := readLedger(ctx, db)
	if err != nil {
		return nil, err
	}
	// Post-check: the ledger must be an exact prefix of the embedded set.
	if err := checkHistory(final, migrations); err != nil {
		return nil, fmt.Errorf("after apply: %w", err)
	}
	if len(final) != len(migrations) {
		return nil, fmt.Errorf("after apply: ledger has %d of %d versions", len(final), len(migrations))
	}
	return final, nil
}

func readLedger(ctx context.Context, db DB) ([]Applied, error) {
	rows, err := db.Query(ctx, "SELECT version, name, sha256 FROM "+LedgerTable+" ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", LedgerTable, err)
	}
	defer rows.Close()
	var applied []Applied
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("read %s: %w", LedgerTable, err)
		}
		var a Applied
		var version int32
		if err := rows.Scan(&version, &a.Name, &a.SHA256); err != nil {
			return nil, fmt.Errorf("read %s: %w", LedgerTable, err)
		}
		a.Version = int(version)
		applied = append(applied, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", LedgerTable, err)
	}
	return applied, nil
}

// checkHistory requires applied to equal the first len(applied) embedded
// migrations in version, name and checksum.
func checkHistory(applied []Applied, migrations []Migration) error {
	if len(applied) > len(migrations) {
		return fmt.Errorf("%w: ledger version %d is not in the embedded set", ErrHistory, applied[len(migrations)].Version)
	}
	for i, a := range applied {
		m := migrations[i]
		switch {
		case a.Version != m.Version || a.Name != m.Name:
			return fmt.Errorf("%w: ledger position %d is version %d (%s), embedded is version %d (%s)",
				ErrHistory, i, a.Version, a.Name, m.Version, m.Name)
		case strings.TrimSpace(a.SHA256) != m.SHA256:
			return fmt.Errorf("%w: version %d (%s) was modified after it was applied", ErrChecksum, a.Version, a.Name)
		}
	}
	return nil
}
