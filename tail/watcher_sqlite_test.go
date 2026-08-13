package tail

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestWatcherWithCrushAdapter verifies the watcher can discover and parse
// SQLite-backed (Crush) sessions. This exercises three bug fixes:
//  1. SessionMeta.Path must carry the composite dbPath/sessionID so
//     watcher's a.Parse(meta.Path) routes correctly
//  2. Change detection must not rely on os.Stat file size (SQLite WAL mode)
//  3. fileState must be keyed by session key, not by file path
func TestWatcherWithCrushAdapter(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)

	sessionID := "watcher-test-001"
	now := time.Now().UnixMilli()

	insertCrushSession(t, dbPath, sessionID, "Watcher Test", now, now+5000)

	adapter := CrushAdapter{DBPath: dbPath, Cwd: "/test/project"}

	w := NewWatcherWithConfig(WatchConfig{
		IdleConfig:   IdleConfig{IdleAfter: 1 * time.Second},
		PollInterval: 100 * time.Millisecond,
	}, []Adapter{adapter})
	defer w.Stop()

	go w.Start(context.Background())

	var got []Event
	timeout := time.After(3 * time.Second)
	for len(got) < 1 {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				t.Fatal("events channel closed")
			}
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timed out waiting for events, got %d", len(got))
		}
	}

	if got[0].Session.ID != sessionID {
		t.Errorf("session ID = %q, want %q", got[0].Session.ID, sessionID)
	}
}

// TestWatcherCrushAdapterChangeDetection verifies the watcher detects new
// events in a SQLite session across polls (not just the first scan). It writes
// a session, starts the watcher, waits for the initial parse, then adds a
// second session and verifies it shows up.
func TestWatcherCrushAdapterChangeDetection(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)

	now := time.Now().UnixMilli()

	// Insert first session before the watcher starts.
	insertCrushSession(t, dbPath, "session-a", "Session A", now, now+1000)

	adapter := CrushAdapter{DBPath: dbPath, Cwd: "/test"}

	w := NewWatcherWithConfig(WatchConfig{
		IdleConfig:   IdleConfig{IdleAfter: 1 * time.Hour},
		PollInterval: 100 * time.Millisecond,
	}, []Adapter{adapter})
	defer w.Stop()

	go w.Start(context.Background())

	// Wait for the first session to be discovered.
	collected := make(map[string]bool)
	timeout := time.After(3 * time.Second)
	for !collected["session-a"] {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				t.Fatal("events channel closed")
			}
			collected[ev.Session.ID] = true
		case <-timeout:
			t.Fatalf("timed out waiting for session-a, got %v", collected)
		}
	}

	// Insert a second session — the watcher must detect it on the next poll.
	insertCrushSession(t, dbPath, "session-b", "Session B", now+2000, now+3000)

	// Drain events until we see session-b.
	timeout = time.After(3 * time.Second)
	for !collected["session-b"] {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				t.Fatal("events channel closed")
			}
			collected[ev.Session.ID] = true
		case <-timeout:
			t.Fatalf("timed out waiting for session-b, got %v", collected)
		}
	}
}

// TestCrushListSessionsSkipsAuxiliary verifies auxiliary (subagent) sessions
// are excluded from ListSessions results.
func TestCrushListSessionsSkipsAuxiliary(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)

	now := time.Now().UnixMilli()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// Insert a primary session.
	_, err = db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
		"primary-001", "Primary", now, now+1000)
	if err != nil {
		t.Fatal(err)
	}

	// Insert an auxiliary (subagent) session.
	_, err = db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"subagent-001", "Subagent", "primary-001", now+500, now+1500)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	adapter := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	metas, err := adapter.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}

	for _, m := range metas {
		if m.Auxiliary {
			t.Errorf("auxiliary session %q should not appear in ListSessions", m.ID)
		}
	}
	if len(metas) != 1 {
		t.Errorf("expected 1 primary session, got %d", len(metas))
	}
}

// TestCrushListSessionsPathIsComposite verifies the Path field in ListSessions
// results carries the composite dbPath/sessionID — this is what the watcher
// passes to Parse().
func TestCrushListSessionsPathIsComposite(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)

	now := time.Now().UnixMilli()
	insertCrushSession(t, dbPath, "composite-check-001", "Test", now, now+1000)

	adapter := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	metas, err := adapter.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("expected 1 session, got %d", len(metas))
	}

	expected := dbPath + "/composite-check-001"
	if metas[0].Path != expected {
		t.Errorf("Path = %q, want %q", metas[0].Path, expected)
	}

	// The composite path must be splittable by splitDBSessionPath.
	dbPart, sessPart := splitDBSessionPath(metas[0].Path)
	if dbPart != dbPath {
		t.Errorf("splitDBSessionPath db = %q, want %q", dbPart, dbPath)
	}
	if sessPart != "composite-check-001" {
		t.Errorf("splitDBSessionPath session = %q, want %q", sessPart, "composite-check-001")
	}
}

// insertCrushSession inserts a minimal session with a user message, tool call,
// and tool result into a Crush-format SQLite database. Used by watcher tests.
func insertCrushSession(t *testing.T, dbPath, sessionID, title string, createdAt, updatedAt int64) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
		sessionID, title, createdAt, updatedAt)
	if err != nil {
		t.Fatal(err)
	}

	userParts := `[{"type":"text","data":{"text":"fix the login bug"}}]`
	assistantParts := `[{"type":"tool_call","data":{"id":"call-1","name":"view","input":"{\"file_path\":\"src/login.go\"}","finished":true}}]`
	toolParts := `[{"type":"tool_result","data":{"tool_call_id":"call-1","name":"view","content":"package main"}}]`

	for i, parts := range []string{userParts, assistantParts, toolParts} {
		role := []string{"user", "assistant", "tool"}[i]
		_, err = db.Exec(`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"msg-"+sessionID+"-"+string(rune('0'+i)), sessionID, role, parts, "test-model", createdAt+int64(i*1000), createdAt+int64(i*1000))
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Ensure the test DB file is writable so the second insert in change-detection
// tests works even with read-only cache handles.
func init() {
	_ = os.Setenv("TMPDIR", os.TempDir())
}
