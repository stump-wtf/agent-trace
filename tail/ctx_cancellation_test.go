package tail

import (
	"context"
	"database/sql"
	"errors"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"testing"
)

// insertCrushSessionWithMessage adds one session and one user message to an
// existing test Crush database.
func insertCrushSessionWithMessage(t *testing.T, dbPath, sessionID string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
		sessionID, "S", 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"msg-1", sessionID, "user", `[{"type":"text","data":{"text":"hi"}}]`, "", 1, 1); err != nil {
		t.Fatal(err)
	}
}

// createOpenCodeSession builds a one-session OpenCode database.
func createOpenCodeSession(t *testing.T, dbPath, sessionID string) {
	t.Helper()
	createTestOpenCodeDB(t, dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, "proj", "slug", "/test", "S", "1.0", 1784148215000, 1784148220000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		"msg-1", sessionID, 1784148216000, 1784148216000, `{"role":"user","content":"hello"}`); err != nil {
		t.Fatal(err)
	}
}

// The watcher threads its cancellation context into every adapter call it
// makes. These tests pin that a cancelled context actually stops the
// SQLite-backed adapters mid-work instead of running queries to completion —
// the behavior that made a stopped watcher keep hammering databases after
// Stop returned.

func TestCrushParseHonorsContextCancellation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)
	insertCrushSessionWithMessage(t, dbPath, "sess-ctx")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	if _, _, _, err := a.Parse(ctx, dbPath+"/sess-ctx"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Parse with cancelled context: err = %v, want context.Canceled", err)
	}
}

func TestCrushListSessionsFilteredHonorsContextCancellation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)
	insertCrushSessionWithMessage(t, dbPath, "sess-ctx")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The unexported scan is asserted directly: the public listing treats a
	// failed database as contributing nothing, which would mask the
	// cancellation.
	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	if _, err := a.listDBSessionsSince(ctx, dbPath, "/test", 0, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("session scan with cancelled context: err = %v, want context.Canceled", err)
	}
}

func TestOpenCodeParseHonorsContextCancellation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	createOpenCodeSession(t, dbPath, "ses_ctx_1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a := OpenCodeAdapter{DBPath: dbPath}
	if _, _, _, err := a.Parse(ctx, dbPath+"/ses_ctx_1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Parse with cancelled context: err = %v, want context.Canceled", err)
	}
}

// The file-backed adapters check cancellation between files while walking a
// session directory. That half of the Adapter contract had no coverage — the
// context parameter was accepted and discarded, so a cancelled caller kept
// opening and summarizing every session file in the directory and got a
// listing back as though nothing were wrong.
func TestJSONLListSessionsHonorsContextCancellation(t *testing.T) {
	dir := t.TempDir()
	line := `{"type":"user","timestamp":"2026-01-01T10:00:00Z","message":{"role":"user","content":"hi"}}` + "\n"
	for _, name := range []string{"a.jsonl", "b.jsonl", "c.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name    string
		adapter Adapter
	}{
		{"claudecode", &ClaudeCodeAdapter{Dir: dir}},
		{"codex", &CodexAdapter{Dir: dir}},
		{"pi", &PiAdapter{Dir: dir}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.adapter.ListSessions(ctx)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("ListSessions with cancelled context: err = %v, want context.Canceled", err)
			}
			if got != nil {
				t.Errorf("ListSessions returned %d sessions alongside the cancellation, want none", len(got))
			}
		})
	}
}
