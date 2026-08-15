package tail

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

// The rows.Err() propagation these tests pin is invisible to ordinary fixture
// databases: a query that fails at prepare time is already handled, and a
// query whose ORDER BY forces a sort evaluates every row before returning the
// first, so a scalar-function error surfaces at QueryContext — a path the
// adapters already check. To land an error strictly BETWEEN QueryContext and
// the end of iteration, each test renames a real table for a VIEW that routes
// a selected-but-unordered column through fail_nth — a registered scalar
// function with a call counter that raises a SQL error once the count passes
// its second argument — and gives the real table an index on the query's
// ORDER BY column so SQLite streams rows via an index scan instead of sorting.
// The driver then evaluates the function one row at a time while the caller
// steps rows.Next(): row 6 of 8 raises the error, rows 1-5 scan cleanly, and
// the failure is visible ONLY through rows.Err() — exactly where a SQLITE_BUSY
// or corrupt-page error would appear, and exactly where a missing check
// silently returned a truncated result as success.

var (
	failNthOnce  sync.Once
	failNthErr   error
	failNthCount atomic.Int64
)

// registerFailNth registers fail_nth exactly once for the process. The
// function is stateful by design — the counter is the failure mechanism — so
// every test must call resetFailNth before the adapter query, and the
// package's tests must not run in parallel.
func registerFailNth(t *testing.T) {
	t.Helper()
	failNthOnce.Do(func() {
		failNthErr = sqlite.RegisterScalarFunction("agenttrace_fail_nth", 2,
			func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
				n := failNthCount.Add(1)
				if after, _ := args[1].(int64); n > after {
					return nil, fmt.Errorf("synthetic iteration failure at call %d", n)
				}
				return args[0], nil
			})
	})
	if failNthErr != nil {
		t.Fatalf("registering fail_nth: %v", failNthErr)
	}
}

func resetFailNth() {
	failNthCount.Store(0)
}

// crushFlakySessionsDB builds a single-project Crush database whose sessions
// table is a view failing on its 6th evaluated row, with an index that lets
// the adapter's ORDER BY stream instead of sort. It returns the dbPath.
func crushFlakySessionsDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`ALTER TABLE sessions RENAME TO sessions_real`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE INDEX sessions_real_by_updated ON sessions_real(updated_at DESC)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE INDEX sessions_real_by_created ON sessions_real(created_at)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE VIEW sessions AS
		SELECT id, agenttrace_fail_nth(title, 5) AS title, parent_session_id, created_at, updated_at
		FROM sessions_real`); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		if _, err := db.Exec(`INSERT INTO sessions_real (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
			fmt.Sprintf("sess-%d", i), "S", int64(i+1), int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath
}

func TestCrushListSessionsPropagatesIterationError(t *testing.T) {
	registerFailNth(t)
	resetFailNth()
	dbPath := crushFlakySessionsDB(t)

	// The unexported scan is called directly: ListSessionsFiltered deliberately
	// treats a failed project database as contributing nothing, so the error it
	// now receives is observable here rather than through ListSessions.
	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	if _, err := a.listDBSessionsSince(context.Background(), dbPath, "/test", 0, false); err == nil {
		t.Fatal("expected a mid-iteration failure to propagate as an error, got nil")
	}
}

func TestCrushAgentGraphPropagatesIterationError(t *testing.T) {
	registerFailNth(t)
	resetFailNth()
	dbPath := crushFlakySessionsDB(t)

	// graphNodes is called directly: AgentGraph deliberately treats a failed
	// project database as contributing nothing to the tree, so the error is
	// observable here rather than through the public method.
	a := CrushAdapter{DBPath: dbPath}
	if _, err := a.graphNodes(dbPath); err == nil {
		t.Fatal("expected a mid-iteration failure to propagate as an error, got nil")
	}
}

func TestCrushParsePropagatesIterationError(t *testing.T) {
	registerFailNth(t)
	resetFailNth()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
		"sess-x", "S", 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE messages RENAME TO messages_real`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE INDEX messages_real_by_session ON messages_real(session_id, created_at)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE VIEW messages AS
		SELECT id, session_id, role, agenttrace_fail_nth(parts, 5) AS parts, model, created_at, updated_at
		FROM messages_real`); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		if _, err := db.Exec(`INSERT INTO messages_real (id, session_id, role, parts, model, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("msg-%d", i), "sess-x", "user", `[{"type":"text","data":{"text":"hi"}}]`, "", int64(i+1), int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	if _, _, _, err := a.Parse(context.Background(), dbPath+"/sess-x"); err == nil {
		t.Fatal("expected a mid-iteration failure to propagate as an error, got nil")
	}
}

func TestOpenCodeListSessionsPropagatesIterationError(t *testing.T) {
	registerFailNth(t)
	resetFailNth()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`CREATE TABLE session_real (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		slug TEXT NOT NULL,
		directory TEXT NOT NULL,
		title TEXT NOT NULL,
		version TEXT NOT NULL,
		model TEXT,
		time_created INTEGER NOT NULL,
		time_updated INTEGER NOT NULL,
		parent_id TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE INDEX session_real_by_updated ON session_real(time_updated DESC, id DESC)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE VIEW session AS
		SELECT id, project_id, slug, directory, agenttrace_fail_nth(title, 5) AS title, version, model,
		       time_created, time_updated, parent_id
		FROM session_real`); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		if _, err := db.Exec(`INSERT INTO session_real (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("ses_%d", i), "proj", "slug", "/test", "S", "1.0", int64(i+1), int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	a := OpenCodeAdapter{DBPath: dbPath}
	odb, err := openSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.listSessionsSince(context.Background(), odb, dbPath, 0, false); err == nil {
		t.Fatal("expected a mid-iteration failure to propagate as an error, got nil")
	}
}
