package tail

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func createTestOpenCodeDB(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, s := range []string{
		`CREATE TABLE session (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			workspace_id TEXT,
			parent_id TEXT,
			slug TEXT NOT NULL,
			directory TEXT NOT NULL,
			path TEXT,
			title TEXT NOT NULL,
			version TEXT NOT NULL,
			share_url TEXT,
			cost REAL NOT NULL DEFAULT 0,
			tokens_input INTEGER NOT NULL DEFAULT 0,
			tokens_output INTEGER NOT NULL DEFAULT 0,
			tokens_reasoning INTEGER NOT NULL DEFAULT 0,
			tokens_cache_read INTEGER NOT NULL DEFAULT 0,
			tokens_cache_write INTEGER NOT NULL DEFAULT 0,
			model TEXT,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			time_compacting INTEGER,
			time_archived INTEGER
		)`,
		`CREATE TABLE message (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			data TEXT NOT NULL
		)`,
		`CREATE TABLE part (
			id TEXT PRIMARY KEY,
			message_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			data TEXT NOT NULL
		)`,
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenCodeParseBasic(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	sessionID := "oc-session-001"
	modelJSON := `{"id":"claude-sonnet-4-20250514","providerID":"anthropic"}`

	_, err = db.Exec(`INSERT INTO session (id, project_id, slug, directory, title, version, model, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, "proj-1", "test-slug", "/test/project", "Test Session", "1.0.0", modelJSON, 1784148215000, 1784148220000)
	if err != nil {
		t.Fatal(err)
	}

	// Insert user message.
	_, err = db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		"msg-1", sessionID, 1784148216000, 1784148216000, `{"role":"user","content":"fix the login bug"}`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert assistant message.
	_, err = db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		"msg-2", sessionID, 1784148217000, 1784148217000, `{"role":"assistant","content":"I'll fix it"}`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert tool part (completed state).
	_, err = db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
		"part-1", "msg-2", sessionID, 1784148217100, 1784148217100,
		`{"type":"tool","tool":"write","callID":"call-1","state":{"status":"completed","input":{"path":"src/login.go"},"output":"File written"}}`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert tool part (error state).
	_, err = db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
		"part-2", "msg-2", sessionID, 1784148217500, 1784148217500,
		`{"type":"tool","tool":"bash","callID":"call-2","state":{"status":"error","input":{"command":"go test"},"error":"test failed"}}`)
	if err != nil {
		t.Fatal(err)
	}

	db.Close()

	adapter := OpenCodeAdapter{DBPath: dbPath}
	path := dbPath + "/" + sessionID

	events, marks, meta, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(events) != 2 {
		t.Errorf("expected 2 events, got %d", len(events))
	}

	// Verify write classified as edit
	if len(events) > 0 && events[0].Action != "edit" {
		t.Errorf("event 0 action = %q, want edit (write maps to edit)", events[0].Action)
	}

	// Verify error event
	if len(events) > 1 && !events[1].IsError {
		t.Error("event 1 should have IsError=true")
	}

	// Verify marks
	foundUserMsg := false
	for _, m := range marks {
		if m.Type == "user-message" && m.Note == "fix the login bug" {
			foundUserMsg = true
		}
	}
	if !foundUserMsg {
		t.Error("expected a user-message mark")
	}

	if meta.ID != sessionID {
		t.Errorf("meta.ID = %q", meta.ID)
	}
	if meta.Title != "Test Session" {
		t.Errorf("meta.Title = %q", meta.Title)
	}
	if meta.Cwd != "/test/project" {
		t.Errorf("meta.Cwd = %q", meta.Cwd)
	}
	if meta.Model != "claude-sonnet-4-20250514" {
		t.Errorf("meta.Model = %q", meta.Model)
	}
}

func TestOpenCodeListSessions(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 3; i++ {
		_, err = db.Exec(`INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"sess-"+string(rune('0'+i)), "proj-1", "slug", "/test", "Session", "1.0", 1784148215000+int64(i*1000), 1784148215000+int64(i*1000+500))
		if err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	adapter := OpenCodeAdapter{DBPath: dbPath}
	metas, err := adapter.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(metas) != 3 {
		t.Errorf("expected 3 sessions, got %d", len(metas))
	}
}

func TestOpenCodeSubagent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`INSERT INTO session (id, project_id, slug, directory, title, version, parent_id, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"sub-1", "proj-1", "sub-slug", "/test", "Sub", "1.0", "parent-1", 1784148215000, 1784148215000)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	adapter := OpenCodeAdapter{DBPath: dbPath}
	metas, err := adapter.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("expected 1 session, got %d", len(metas))
	}
	if !metas[0].Auxiliary {
		t.Error("subagent session should be Auxiliary")
	}
}

func TestOpenCodeCompactionMark(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	sessionID := "oc-compact"
	_, err = db.Exec(`INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, "proj-1", "slug", "/test", "Test", "1.0", 1784148215000, 1784148215000)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		"msg-1", sessionID, 1784148216000, 1784148216000, `{"role":"assistant","content":""}`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
		"part-1", "msg-1", sessionID, 1784148216100, 1784148216100,
		`{"type":"compaction","auto":true}`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	adapter := OpenCodeAdapter{DBPath: dbPath}
	_, marks, _, err := adapter.Parse(dbPath + "/" + sessionID)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	found := false
	for _, m := range marks {
		if m.Type == "compaction" {
			found = true
		}
	}
	if !found {
		t.Error("expected a compaction mark")
	}
}

func TestOpenCodeMissingDB(t *testing.T) {
	adapter := OpenCodeAdapter{DBPath: "/nonexistent/opencode.db"}
	metas, err := adapter.ListSessions()
	if err != nil {
		t.Fatalf("should handle missing DB gracefully: %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("expected 0 sessions for missing DB, got %d", len(metas))
	}
}
