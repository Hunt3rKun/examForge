package migrate

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"examforge"
)

// fakeDB is a minimal model of the statements the runner issues: an advisory
// lock call, the ledger table, savepoints and ledger rows. It knows nothing
// about SQL; it verifies the runner's control flow, not PostgreSQL.
type fakeDB struct {
	lockTaken   bool
	ledgerReady bool
	rows        []Applied
	savepoints  [][]Applied

	// live mirrors DDL effects by migration version, so a test can observe
	// what a committed run would leave behind.
	live        map[int]bool
	stmthistory []string
	failOn      string // the first statement containing this substring fails
	failN       int    // after this many earlier matching statements
}

type oneRow struct {
	vals []any
	err  error
}

func (r oneRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return scanAll(r.vals, dest)
}

type fakeRows struct {
	rows [][]any
	err  error
	i    int
}

func (r *fakeRows) Close() {}
func (r *fakeRows) Next() bool {
	r.i++
	return r.i <= len(r.rows)
}
func (r *fakeRows) Scan(dest ...any) error { return scanAll(r.rows[r.i-1], dest) }
func (r *fakeRows) Err() error             { return r.err }

// scanAll copies ints and strings between row values and scan destinations.
func scanAll(src []any, dest []any) error {
	for i := range dest {
		switch d := dest[i].(type) {
		case *int32:
			*d = int32(src[i].(int))
		case *string:
			*d = src[i].(string)
		default:
			return errors.New("fakeDB: unsupported scan destination")
		}
	}
	return nil
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) error {
	f.stmthistory = append(f.stmthistory, sql)
	if f.failOn != "" && strings.Contains(sql, f.failOn) {
		if f.failN > 0 {
			f.failN--
		} else {
			return errors.New("injected statement failure")
		}
	}
	switch {
	case strings.Contains(sql, "pg_advisory_xact_lock"):
		f.lockTaken = true
	case strings.Contains(sql, "CREATE TABLE IF NOT EXISTS "+LedgerTable):
		f.ledgerReady = true
	case strings.HasPrefix(sql, "SAVEPOINT"):
		f.savepoints = append(f.savepoints, append([]Applied(nil), f.rows...))
	case strings.HasPrefix(sql, "RELEASE SAVEPOINT"):
		f.savepoints = f.savepoints[:len(f.savepoints)-1]
	case strings.HasPrefix(sql, "ROLLBACK TO SAVEPOINT"):
		f.rows = append([]Applied(nil), f.savepoints[len(f.savepoints)-1]...)
		f.savepoints = f.savepoints[:len(f.savepoints)-1]
	case strings.HasPrefix(sql, "INSERT INTO "+LedgerTable):
		version := args[0].(int)
		f.rows = append(f.rows, Applied{Version: version, Name: args[1].(string), SHA256: args[2].(string)})
		f.live[version] = true
	default:
		// Migration DDL. Mark the version as live when the statement is
		// preceded by a savepoint for it; the runner's order makes this the
		// current version being applied.
		if !f.ledgerReady {
			return errors.New("fakeDB: migration ran before the ledger table existed")
		}
	}
	return nil
}

func (f *fakeDB) Query(_ context.Context, sql string, _ ...any) (Rows, error) {
	if !f.ledgerReady {
		return nil, errors.New(`relation "examforge_schema_migrations" does not exist`)
	}
	rows := make([][]any, 0, len(f.rows))
	for _, a := range f.rows {
		rows = append(rows, []any{a.Version, a.Name, strings.TrimSpace(a.SHA256)})
	}
	return &fakeRows{rows: rows}, nil
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, _ ...any) Row {
	return oneRow{err: errors.New("unexpected QueryRow")}
}

func digest(s string) string {
	// Any stable hex of the right length: the runner never parses it.
	return strings.Repeat(strconv.Itoa(len(s)%10), 64)
}

func migs(count int) []Migration {
	out := make([]Migration, 0, count)
	for i := 1; i <= count; i++ {
		out = append(out, Migration{Version: i, Name: "m" + strconv.Itoa(i), SHA256: digest("m" + strconv.Itoa(i)), SQL: "-- migration " + strconv.Itoa(i)})
	}
	return out
}

func TestApplyOrdersVersionsAndRecordsLedger(t *testing.T) {
	db := &fakeDB{live: map[int]bool{}}
	applied, err := Apply(context.Background(), db, migs(3))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !db.lockTaken {
		t.Error("advisory lock was not taken before applying")
	}
	if len(applied) != 3 || applied[2].Version != 3 {
		t.Fatalf("ledger after apply = %+v", applied)
	}
	for i, a := range applied {
		if a.Version != i+1 {
			t.Errorf("ledger position %d has version %d", i, a.Version)
		}
	}
	for i, sql := range db.stmthistory {
		if strings.HasPrefix(sql, "INSERT INTO "+LedgerTable) {
			// Every ledger insert must be preceded by its migration's SQL.
			if !strings.Contains(strings.Join(db.stmthistory[:i], "\n"), "-- migration") {
				t.Error("ledger row written before any migration SQL")
			}
		}
	}
	// Re-running is a no-op.
	before := len(db.stmthistory)
	if again, err := Apply(context.Background(), db, migs(3)); err != nil || len(again) != 3 {
		t.Fatalf("second Apply = %+v, %v", again, err)
	}
	for _, sql := range db.stmthistory[before:] {
		if strings.Contains(sql, "-- migration") {
			t.Error("an already-applied migration was executed again")
		}
	}
}

func TestApplyStopsAtFailureAndLeavesNoPartialRow(t *testing.T) {
	db := &fakeDB{live: map[int]bool{}, failOn: "-- migration 2"}
	if _, err := Apply(context.Background(), db, migs(3)); err == nil {
		t.Fatal("Apply succeeded although a migration failed")
	}
	if len(db.rows) != 1 || db.rows[0].Version != 1 {
		t.Fatalf("ledger after a failed second migration = %+v", db.rows)
	}
	if db.live[2] {
		t.Error("a failed migration was recorded")
	}
	// Version 3 was never attempted.
	if db.live[3] {
		t.Error("a migration after the failed one ran")
	}
}

func TestApplyRejectsTamperedAppliedMigration(t *testing.T) {
	db := &fakeDB{live: map[int]bool{}}
	if _, err := Apply(context.Background(), db, migs(2)); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	changed := migs(3)
	changed[1].SQL += "\n-- edited"
	changed[1].SHA256 = digest(changed[1].SQL)
	if _, err := Apply(context.Background(), db, changed); !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if db.live[3] {
		t.Error("a pending migration ran despite the checksum mismatch")
	}
}

func TestApplyRejectsUnknownLedgerVersion(t *testing.T) {
	db := &fakeDB{live: map[int]bool{}}
	db.ledgerReady = true
	db.rows = []Applied{{Version: 1, Name: "m1", SHA256: digest("m1")}, {Version: 9, Name: "unknown", SHA256: digest("x")}}
	if _, err := Apply(context.Background(), db, migs(2)); !errors.Is(err, ErrHistory) {
		t.Fatalf("err = %v, want ErrHistory", err)
	}
}

func files(m map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for name, body := range m {
		out[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return out
}

func TestLoadValidatesFileLayout(t *testing.T) {
	if _, err := Load(files(map[string]string{"0001_assets.sql": "a", "0002_more_tables.sql": "b"})); err != nil {
		t.Fatalf("valid layout rejected: %v", err)
	}
	bad := map[string]map[string]string{
		"empty":         {},
		"gap":           {"0001_a.sql": "a", "0003_c.sql": "c"},
		"not from one":  {"0002_b.sql": "b"},
		"wrong version": {"0001_a.sql": "a", "2_b.sql": "b"},
		"upper case":    {"0001_A.sql": "a"},
	}
	for name, set := range bad {
		if _, err := Load(files(set)); !errors.Is(err, ErrLayout) {
			t.Errorf("%s: err = %v, want ErrLayout", name, err)
		}
	}
	// Two files cannot share one version either: the second lands off sequence.
	dup := fstest.MapFS{
		"0001_a.sql":     &fstest.MapFile{Data: []byte("a")},
		"0001_other.sql": &fstest.MapFile{Data: []byte("b")},
	}
	if _, err := Load(dup); !errors.Is(err, ErrLayout) {
		t.Errorf("duplicate version: err = %v, want ErrLayout", err)
	}
	marked := files(map[string]string{"0001_a.sql": "-- examforge:non-transactional\nALTER x"})
	if _, err := Load(marked); !errors.Is(err, ErrUnsupported) {
		t.Errorf("marked file: err = %v, want ErrUnsupported", err)
	}
}

func TestEmbeddedMigrationsAreOrderedAndRiverFree(t *testing.T) {
	migrations, err := Load(examforge.Migrations)
	if err != nil {
		t.Fatalf("Load embedded: %v", err)
	}
	want := []string{"assets", "questions", "ingest_jobs", "exams", "asset_raw_digest"}
	if len(migrations) != len(want) {
		t.Fatalf("embedded %d migrations, want %d", len(migrations), len(want))
	}
	for i, m := range migrations {
		if m.Name != want[i] || m.Version != i+1 {
			t.Errorf("migration %d = %d %s, want %d %s", i, m.Version, m.Name, i+1, want[i])
		}
		if len(m.SHA256) != 64 {
			t.Errorf("%s: checksum %q", m.Name, m.SHA256)
		}
		if riverTable.MatchString(m.SQL) {
			t.Errorf("%s creates a river_* table; River owns its own schema", m.Name)
		}
		if strings.Contains(m.SQL, LedgerTable) {
			t.Errorf("%s creates the ledger table; the runner owns it", m.Name)
		}
	}
}

var riverTable = regexp.MustCompile(`(?i)create\s+table\s+(if\s+not\s+exists\s+)?river_`)
