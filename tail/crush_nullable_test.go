package tail

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The Crush schema declares parent_session_id and messages.model nullable, and
// Crush uses both: NULL parent for a top-level session, NULL model for a user
// message. Every fixture in this package inserted '' instead, so nothing
// exercised the columns as the real tool writes them — and scanning a NULL into
// a string fails, which both loops turn into a silent `continue`.
//
// These tests exist because the failure is invisible: no error surfaces, the
// listing is just short. Adding these adapters to DefaultAdapters (this change)
// is what makes that reach every consumer.

// writeCrushProjects points a projects.json at one database, so the test
// exercises the discovery path DefaultAdapters actually uses rather than the
// single-database testing override.
func writeCrushProjects(t *testing.T, path, cwd, dataDir string) {
	t.Helper()
	data, err := json.Marshal(crushProjectsFile{
		Projects: []crushProjectEntry{{Path: cwd, DataDir: dataDir}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCrushListsTopLevelSessionsWithNullParent(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "project")
	dbPath := filepath.Join(dir, "data", "crush.db")
	createTestCrushDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO sessions (id, parent_session_id, title, created_at, updated_at)
		 VALUES ('toplevel-0001', NULL, 'top', 1000, 1000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO sessions (id, parent_session_id, title, created_at, updated_at)
		 VALUES ('child-0001', 'toplevel-0001', 'child', 2000, 2000)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	projects := filepath.Join(dir, "projects.json")
	writeCrushProjects(t, projects, cwd, filepath.Dir(dbPath))
	a := CrushAdapter{ProjectsPath: projects}

	metas, err := a.ListSessions(t.Context())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("got %d sessions, want 1 — the top-level row with NULL parent should appear; the child (auxiliary) should be excluded", len(metas))
	}
	if metas[0].ID != "toplevel-0001" {
		t.Errorf("expected toplevel-0001, got %q", metas[0].ID)
	}
	if metas[0].Auxiliary {
		t.Error("a NULL parent_session_id must not mark the session auxiliary")
	}
}

func TestCrushParseAndSummarizeResolveCwdFromProjects(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "project")
	dbPath := filepath.Join(dir, "data", "crush.db")
	createTestCrushDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO sessions (id, parent_session_id, title, created_at, updated_at)
		 VALUES ('s1', NULL, 'top', 1000, 1000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at)
		 VALUES ('m1','s1','assistant','[]','sonnet',1000,1000)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	projects := filepath.Join(dir, "projects.json")
	writeCrushProjects(t, projects, cwd, filepath.Dir(dbPath))
	a := CrushAdapter{ProjectsPath: projects}

	// The cwd every event is classified against must be the one the listing
	// reported, not the empty single-database override.
	_, _, meta, err := a.Parse(t.Context(), dbPath+"/s1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if meta.Cwd != cwd {
		t.Errorf("Parse cwd = %q, want %q", meta.Cwd, cwd)
	}
	sum, err := a.Summarize(t.Context(), dbPath+"/s1")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Cwd != cwd {
		t.Errorf("Summarize cwd = %q, want %q", sum.Cwd, cwd)
	}
}

func TestCrushParseKeepsMessagesWithNullModel(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "crush.db")
	createTestCrushDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO sessions (id, parent_session_id, title, created_at, updated_at)
		 VALUES ('s1', NULL, 't', 1000, 1000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at)
		 VALUES ('m1','s1','user',?,NULL,1000,1000)`,
		`[{"type":"text","data":{"text":"please run the tests"}}]`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at)
		 VALUES ('m2','s1','assistant',?,'sonnet',2000,2000)`,
		`[{"type":"tool_call","data":{"id":"c1","name":"bash","input":"{}"}},
		  {"type":"tool_result","data":{"tool_call_id":"c1","content":"ok"}}]`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	a := CrushAdapter{DBPath: dbPath, Cwd: dir}
	events, marks, meta, err := a.Parse(t.Context(), dbPath+"/s1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(marks) != 1 {
		t.Errorf("got %d marks, want 1 — the user message has a NULL model and was dropped", len(marks))
	}
	if len(events) != 1 {
		t.Errorf("got %d events, want 1", len(events))
	}
	if meta.Model != "sonnet" {
		t.Errorf("model = %q, want sonnet", meta.Model)
	}
}

// TestCrushDedupesProjectsSharingADataDir covers a registry that names one
// crush.db from more than one entry. Crush keys a project on its working
// directory, so nothing stops two entries resolving to the same store — and
// the scan loop appends what each entry finds, with no downstream dedupe: the
// duplicated sessions carry identical Keys straight through the sort and out
// to the caller.
func TestCrushDedupesProjectsSharingADataDir(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	dbPath := filepath.Join(dataDir, "crush.db")
	createTestCrushDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO sessions (id, parent_session_id, title, created_at, updated_at)
		 VALUES ('s1', NULL, 'only session', 1000, 1000)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Three spellings of one store: a stale entry, the current one, and a path
	// that only differs in the ways filepath.Join normalizes away.
	oldCwd := filepath.Join(dir, "old-checkout")
	newCwd := filepath.Join(dir, "current-checkout")
	data, err := json.Marshal(crushProjectsFile{Projects: []crushProjectEntry{
		{Path: oldCwd, DataDir: dataDir, LastAccess: "2026-01-01T00:00:00Z"},
		{Path: newCwd, DataDir: dataDir, LastAccess: "2026-06-01T00:00:00Z"},
		{Path: filepath.Join(dir, "third"), DataDir: dataDir + "/", LastAccess: "2025-01-01T00:00:00Z"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(dir, "projects.json")
	if err := os.WriteFile(projects, data, 0o644); err != nil {
		t.Fatal(err)
	}
	a := CrushAdapter{ProjectsPath: projects}

	metas, err := a.ListSessions(t.Context())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("got %d sessions, want 1 — one database registered three times is still one database", len(metas))
	}
	// The cwd of the winning entry is the one every event in the session gets
	// classified against, so it must be the most recently accessed, not
	// whichever spelling happened to be listed first.
	if metas[0].Cwd != newCwd {
		t.Errorf("Cwd = %q, want %q (the entry with the newest last_accessed)", metas[0].Cwd, newCwd)
	}

	graph, err := a.AgentGraph(t.Context())
	if err != nil {
		t.Fatalf("AgentGraph: %v", err)
	}
	if len(graph.Roots) != 1 {
		t.Errorf("got %d graph roots, want 1 — the duplicate entries doubled the tree", len(graph.Roots))
	}
}
