package tail

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The third finding reported in #159 is that a Crush schema change yielded an
// empty or partial result instead of an error: a row that failed to scan was
// skipped by a bare `continue`, and both multi-database loops skipped any
// database that failed outright, so "every database disagrees with this reader"
// was indistinguishable from "there is nothing here".
//
// The two nullable columns behind the earlier incidents (sessions.parent_session_id
// and messages.model) are data, and stay scanned as such. What is now an error is
// a row that will not scan at all, plus a store where every database failed. These
// tests pin that line: NULLs still decode to zero values, an unscannable row fails
// loudly, and one unreadable project among many is still tolerated.

const crushSessionsStandardSchema = `CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	parent_session_id TEXT,
	title TEXT NOT NULL DEFAULT '',
	message_count INTEGER NOT NULL DEFAULT 0,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost REAL NOT NULL DEFAULT 0.0,
	updated_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL
)`

const crushSessionsJunkTimestampsSchema = `CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	parent_session_id TEXT,
	title TEXT,
	message_count INTEGER NOT NULL DEFAULT 0,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost REAL NOT NULL DEFAULT 0.0,
	updated_at TEXT NOT NULL,
	created_at TEXT NOT NULL
)`

const crushSessionsNullableSchema = `CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	parent_session_id TEXT,
	title TEXT,
	message_count INTEGER NOT NULL DEFAULT 0,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost REAL NOT NULL DEFAULT 0.0,
	updated_at INTEGER,
	created_at INTEGER
)`

const crushMessagesJunkTimestampsSchema = `CREATE TABLE messages (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	role TEXT NOT NULL,
	parts TEXT NOT NULL DEFAULT '[]',
	model TEXT,
	created_at TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	finished_at INTEGER
)`

func createCrushDBWithSchema(t *testing.T, path string, schema ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, s := range schema {
		if _, err := db.ExecContext(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
}

func execInCrushDB(t *testing.T, path, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), stmt, args...); err != nil {
		t.Fatal(err)
	}
}

// writeCrushProjectsMulti points a projects.json at several projects.
func writeCrushProjectsMulti(t *testing.T, path string, entries ...crushProjectEntry) {
	t.Helper()
	data, err := json.Marshal(crushProjectsFile{Projects: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCrushListingFailsOnRowsThatWillNotScan(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "crush.db")
	createCrushDBWithSchema(t, dbPath, crushSessionsJunkTimestampsSchema, crushMessagesJunkTimestampsSchema)
	execInCrushDB(t, dbPath,
		`INSERT INTO sessions (id, parent_session_id, title, updated_at, created_at)
		 VALUES ('sess-00000001', NULL, 'drifted', 'not-a-timestamp', 'not-a-timestamp')`)

	projects := filepath.Join(dir, "projects.json")
	writeCrushProjectsMulti(t, projects, crushProjectEntry{Path: filepath.Join(dir, "project"), DataDir: filepath.Dir(dbPath)})
	a := CrushAdapter{ProjectsPath: projects}

	metas, err := a.ListSessions(t.Context())
	if err == nil {
		t.Fatalf("ListSessions returned %d sessions and no error; a row this reader cannot scan must be reported, not skipped", len(metas))
	}
	if !strings.Contains(err.Error(), dbPath) {
		t.Errorf("error should name the database that failed, got %v", err)
	}
}

func TestCrushListingStillToleratesNullColumns(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "crush.db")
	createCrushDBWithSchema(t, dbPath, crushSessionsNullableSchema, crushMessagesJunkTimestampsSchema)
	execInCrushDB(t, dbPath,
		`INSERT INTO sessions (id, parent_session_id, title, updated_at, created_at)
		 VALUES ('sess-00000001', NULL, NULL, NULL, NULL)`)

	projects := filepath.Join(dir, "projects.json")
	writeCrushProjectsMulti(t, projects, crushProjectEntry{Path: filepath.Join(dir, "project"), DataDir: filepath.Dir(dbPath)})
	a := CrushAdapter{ProjectsPath: projects}

	metas, err := a.ListSessions(t.Context())
	if err != nil {
		t.Fatalf("ListSessions: %v — a NULL is a value, not a schema change", err)
	}
	if len(metas) != 1 {
		t.Fatalf("got %d sessions, want 1", len(metas))
	}
	if metas[0].ID != "sess-00000001" {
		t.Errorf("expected the row to survive with its id, got %q", metas[0].ID)
	}
	if metas[0].StartedAt != "" || metas[0].EndedAt != "" {
		t.Errorf("NULL timestamps should degrade to empty, got %q / %q", metas[0].StartedAt, metas[0].EndedAt)
	}
}

func TestCrushParseFailsOnMessageRowsThatWillNotScan(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "crush.db")
	// The messages table declares created_at as text holding a non-numeric
	// value: that is what a migration changing the column's type leaves behind.
	createCrushDBWithSchema(t, dbPath, crushSessionsStandardSchema, crushMessagesJunkTimestampsSchema)
	execInCrushDB(t, dbPath,
		`INSERT INTO sessions (id, parent_session_id, title, updated_at, created_at)
		 VALUES ('sess-00000001', NULL, 'ok', 1000, 1000)`)
	execInCrushDB(t, dbPath,
		`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at)
		 VALUES ('msg-00000001', 'sess-00000001', 'user', '[]', NULL, 'not-a-timestamp', 1000)`)

	a := CrushAdapter{DBPath: dbPath, Cwd: filepath.Join(dir, "project")}
	_, _, _, err := a.Parse(t.Context(), dbPath+"/sess-00000001")
	if err == nil {
		t.Fatal("Parse returned no error; an unscannable message row must not read as a session with fewer messages")
	}
	if !strings.Contains(err.Error(), "sess-00000001") {
		t.Errorf("error should name the session, got %v", err)
	}
}

func TestCrushListingToleratesOneUnreadableDatabase(t *testing.T) {
	dir := t.TempDir()
	goodDB := filepath.Join(dir, "good", "crush.db")
	badDB := filepath.Join(dir, "bad", "crush.db")
	createTestCrushDB(t, goodDB)
	execInCrushDB(t, goodDB,
		`INSERT INTO sessions (id, parent_session_id, title, updated_at, created_at)
		 VALUES ('sess-00000001', NULL, 'good', 1000, 1000)`)
	if err := os.MkdirAll(filepath.Dir(badDB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badDB, []byte("not a database file at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	projects := filepath.Join(dir, "projects.json")
	writeCrushProjectsMulti(t, projects,
		crushProjectEntry{Path: filepath.Join(dir, "good"), DataDir: filepath.Dir(goodDB), LastAccess: "2026-10-01T00:00:00Z"},
		crushProjectEntry{Path: filepath.Join(dir, "bad"), DataDir: filepath.Dir(badDB), LastAccess: "2026-10-02T00:00:00Z"})
	a := CrushAdapter{ProjectsPath: projects}

	metas, err := a.ListSessions(t.Context())
	if err != nil {
		t.Fatalf("ListSessions: %v — one unreadable project must not hide every other project", err)
	}
	if len(metas) != 1 || metas[0].ID != "sess-00000001" {
		t.Fatalf("got %+v, want the one readable project's session", metas)
	}
}

func TestCrushListingFailsWhenEveryDatabaseIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	badDB := filepath.Join(dir, "bad", "crush.db")
	if err := os.MkdirAll(filepath.Dir(badDB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badDB, []byte("not a database file at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	projects := filepath.Join(dir, "projects.json")
	writeCrushProjectsMulti(t, projects, crushProjectEntry{Path: filepath.Join(dir, "bad"), DataDir: filepath.Dir(badDB)})
	a := CrushAdapter{ProjectsPath: projects}

	metas, err := a.ListSessions(t.Context())
	if err == nil {
		t.Fatalf("ListSessions returned %d sessions and no error; a store nothing can read is not an empty store", len(metas))
	}
	if !strings.Contains(err.Error(), badDB) {
		t.Errorf("error should name the database that failed, got %v", err)
	}
}

func TestCrushAgentGraphToleratesOneUnreadableDatabase(t *testing.T) {
	dir := t.TempDir()
	goodDB := filepath.Join(dir, "good", "crush.db")
	badDB := filepath.Join(dir, "bad", "crush.db")
	createTestCrushDB(t, goodDB)
	execInCrushDB(t, goodDB,
		`INSERT INTO sessions (id, parent_session_id, title, created_at, updated_at) VALUES ('root-00000001', NULL, 'root', 1000, 1000)`)
	if err := os.MkdirAll(filepath.Dir(badDB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badDB, []byte("not a database file at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	projects := filepath.Join(dir, "projects.json")
	writeCrushProjectsMulti(t, projects,
		crushProjectEntry{Path: filepath.Join(dir, "good"), DataDir: filepath.Dir(goodDB), LastAccess: "2026-10-01T00:00:00Z"},
		crushProjectEntry{Path: filepath.Join(dir, "bad"), DataDir: filepath.Dir(badDB), LastAccess: "2026-10-02T00:00:00Z"})
	a := CrushAdapter{ProjectsPath: projects}

	graph, err := a.AgentGraph(t.Context())
	if err != nil {
		t.Fatalf("AgentGraph: %v", err)
	}
	if len(graph.Roots) != 1 || graph.Roots[0].SessionID != "root-00000001" {
		t.Fatalf("got %+v, want the one readable project's root", graph.Roots)
	}
}

func TestCrushAgentGraphFailsWhenEveryDatabaseIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	badDB := filepath.Join(dir, "bad", "crush.db")
	if err := os.MkdirAll(filepath.Dir(badDB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badDB, []byte("not a database file at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	projects := filepath.Join(dir, "projects.json")
	writeCrushProjectsMulti(t, projects, crushProjectEntry{Path: filepath.Join(dir, "bad"), DataDir: filepath.Dir(badDB)})
	a := CrushAdapter{ProjectsPath: projects}

	graph, err := a.AgentGraph(t.Context())
	if err == nil {
		t.Fatalf("AgentGraph returned %+v and no error; a store nothing can read is not an empty graph", graph)
	}
	if !strings.Contains(err.Error(), badDB) {
		t.Errorf("error should name the database that failed, got %v", err)
	}
}
