package tail

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func createTestCrushDB(t *testing.T, path string) {
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
	schema := []string{
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			parent_session_id TEXT,
			title TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0.0,
			updated_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE messages (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			parts TEXT NOT NULL DEFAULT '[]',
			model TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			finished_at INTEGER
		)`,
	}
	for _, s := range schema {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCrushAdapterParse(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UnixMilli()
	sessionID := "test-session-001"

	// Insert a session.
	_, err = db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
		sessionID, "Test Session", now, now+5000)
	if err != nil {
		t.Fatal(err)
	}

	// Insert messages: user message, tool_call, tool_result.
	userParts := `[{"type":"text","data":{"text":"fix the login bug"}}]`
	assistantParts := `[{"type":"tool_call","data":{"id":"call-1","name":"view","input":"{\"file_path\":\"src/login.go\"}","finished":true}}]`
	toolParts := `[{"type":"tool_result","data":{"tool_call_id":"call-1","name":"view","content":"package main"}}]`

	for i, parts := range []string{userParts, assistantParts, toolParts} {
		role := []string{"user", "assistant", "tool"}[i]
		_, err = db.Exec(`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"msg-"+string(rune('0'+i)), sessionID, role, parts, "test-model", now+int64(i*1000), now+int64(i*1000))
		if err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	adapter := CrushAdapter{DBPath: dbPath, Cwd: "/test/project"}
	path := dbPath + "/" + sessionID

	events, marks, meta, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(events) != 1 {
		t.Errorf("expected 1 event, got %d", len(events))
	}
	if len(marks) != 1 {
		t.Errorf("expected 1 user-message mark, got %d", len(marks))
	} else {
		if marks[0].Type != "user-message" {
			t.Errorf("mark type = %q", marks[0].Type)
		}
		if marks[0].Note != "fix the login bug" {
			t.Errorf("mark note = %q", marks[0].Note)
		}
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
	if meta.Model != "test-model" {
		t.Errorf("meta.Model = %q", meta.Model)
	}
	if meta.StartedAt == "" {
		t.Error("meta.StartedAt should not be empty")
	}

	// Verify the tool call was classified as read (view = Read in Crush).
	if len(events) > 0 && events[0].Action != "read" {
		t.Errorf("event 0 action = %q, want read (view maps to read)", events[0].Action)
	}
}

func TestCrushAdapterListSessions(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UnixMilli()
	for i := range 3 {
		_, err = db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
			fmt.Sprintf("sess-%d", i), fmt.Sprintf("Session %d", i), now+int64(i*1000), now+int64(i*1000+500))
		if err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	adapter := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	metas, err := adapter.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(metas) != 3 {
		t.Errorf("expected 3 sessions, got %d", len(metas))
	}
	// Should be sorted newest-first by EndedAt.
	if len(metas) >= 2 {
		if metas[0].EndedAt < metas[1].EndedAt {
			t.Error("sessions should be sorted newest-first")
		}
	}
}

func TestCrushAdapterSubagent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UnixMilli()
	_, err = db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"sub-1", "Subagent", "parent-1", now, now)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	adapter := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
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

func TestCrushAdapterRejectsMissingDB(t *testing.T) {
	adapter := CrushAdapter{DBPath: "/nonexistent/crush.db"}
	metas, err := adapter.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions should handle missing DB gracefully: %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("expected 0 sessions for missing DB, got %d", len(metas))
	}
}

func TestCrushMsToRFC3339(t *testing.T) {
	if msToRFC3339(0) != "" {
		t.Error("ms=0 should produce empty string")
	}
	ts := msToRFC3339(1784148215000)
	if ts == "" {
		t.Error("valid ms should produce RFC3339 string")
	}
	if ts[:4] != "2026" {
		t.Errorf("expected 2026 date, got %q", ts[:4])
	}
}
