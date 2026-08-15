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

// TestClaudeCodeParseSince verifies byte-offset incremental parsing for
// Claude Code sessions: ParseSince reads only lines after the offset and
// continues seq numbering from startSeq.
func TestClaudeCodeParseSince(t *testing.T) {
	// Write initial content: a tool_use + tool_result pair.
	initial := `{"type":"assistant","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","cwd":"/tmp","message":{"role":"assistant","model":"m","content":[{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":"echo hello"}}]}}` + "\n" +
		`{"type":"user","timestamp":"2026-01-01T10:00:01Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"hello"}]}}` + "\n"
	path := writeTempJSONL(t, "session.jsonl", initial)

	adapter := ClaudeCodeAdapter{}

	// Full parse first.
	events1, _, _, err := adapter.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events1) != 1 {
		t.Fatalf("expected 1 event from full parse, got %d", len(events1))
	}

	// Record the watermark.
	wm := adapter.Watermark(t.Context(), path)
	if wm == 0 {
		t.Fatal("Watermark should be non-zero for a non-empty file")
	}

	// Append a new tool_use + tool_result.
	appended := `{"type":"assistant","timestamp":"2026-01-01T10:01:00Z","sessionId":"s1","message":{"role":"assistant","model":"m","content":[{"type":"tool_use","id":"call-2","name":"Read","input":{"file_path":"src/main.go"}}]}}` + "\n" +
		`{"type":"user","timestamp":"2026-01-01T10:01:01Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-2","content":"package main"}]}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(appended); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Incremental parse from the watermark.
	events2, _, _, newWm, err := adapter.ParseSince(t.Context(), path, wm, 1)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events2) != 1 {
		t.Fatalf("expected 1 new event from incremental parse, got %d", len(events2))
	}
	if events2[0].Seq != 1 {
		t.Errorf("seq = %d, want 1 (continues from startSeq)", events2[0].Seq)
	}
	if events2[0].Action != "read" {
		t.Errorf("action = %q, want read", events2[0].Action)
	}
	if newWm <= wm {
		t.Errorf("newWatermark %d should be > old %d", newWm, wm)
	}
}

// TestClaudeCodeParseSinceFileShrunk verifies that ParseSince returns empty
// results when the file shrinks (truncation/rotation).
func TestClaudeCodeParseSinceFileShrunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	adapter := ClaudeCodeAdapter{}
	events, _, _, _, err := adapter.ParseSince(t.Context(), path, 999999, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for shrunk file, got %d", len(events))
	}
}

// TestCrushParseSince verifies SQLite watermark incremental parsing for
// Crush sessions: ParseSince queries only rows with created_at > watermark.
func TestCrushParseSince(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)

	sessionID := "incr-test-001"
	now := time.Now().UnixMilli()

	// Insert initial messages.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
		sessionID, "Incr Test", now, now+5000)
	if err != nil {
		t.Fatal(err)
	}
	userParts1 := `[{"type":"text","data":{"text":"first message"}}]`
	assistantParts1 := `[{"type":"tool_call","data":{"id":"call-1","name":"view","input":"{\"file_path\":\"a.go\"}","finished":true}}]`
	toolParts1 := `[{"type":"tool_result","data":{"tool_call_id":"call-1","name":"view","content":"package a"}}]`
	for i, parts := range []string{userParts1, assistantParts1, toolParts1} {
		role := []string{"user", "assistant", "tool"}[i]
		_, err = db.Exec(`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"msg-"+string(rune('0'+i)), sessionID, role, parts, "test-model", now+int64(i*1000), now+int64(i*1000))
		if err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	adapter := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	path := dbPath + "/" + sessionID

	// Full parse.
	events1, _, _, err := adapter.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events1) != 1 {
		t.Fatalf("expected 1 event from full parse, got %d", len(events1))
	}

	// Record the watermark.
	wm := adapter.Watermark(t.Context(), path)
	if wm == 0 {
		t.Fatal("Watermark should be non-zero")
	}

	// Insert new messages.
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	userParts2 := `[{"type":"text","data":{"text":"second message"}}]`
	assistantParts2 := `[{"type":"tool_call","data":{"id":"call-2","name":"view","input":"{\"file_path\":\"b.go\"}","finished":true}}]`
	toolParts2 := `[{"type":"tool_result","data":{"tool_call_id":"call-2","name":"view","content":"package b"}}]`
	baseMs := wm + 10000
	for i, parts := range []string{userParts2, assistantParts2, toolParts2} {
		role := []string{"user", "assistant", "tool"}[i]
		_, err = db.Exec(`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"msg2-"+string(rune('0'+i)), sessionID, role, parts, "test-model", baseMs+int64(i*1000), baseMs+int64(i*1000))
		if err != nil {
			t.Fatal(err)
		}
	}
	// Update session's updated_at.
	_, err = db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, baseMs+2000, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Clear the cache so the next open sees fresh data.
	resetDBCache()

	// Incremental parse from the watermark.
	events2, marks2, _, _, err := adapter.ParseSince(t.Context(), path, wm, 1)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events2) != 1 {
		t.Fatalf("expected 1 new event from incremental parse, got %d", len(events2))
	}
	if events2[0].Seq != 1 {
		t.Errorf("seq = %d, want 1", events2[0].Seq)
	}
	// Should also get a user-message mark for "second message".
	if len(marks2) == 0 {
		t.Error("expected at least 1 mark from incremental parse")
	}
}

// TestWatcherIncrementalClaudeCode verifies the watcher uses byte-offset
// incremental parsing for Claude Code sessions across polls.
func TestWatcherIncrementalClaudeCode(t *testing.T) {
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "session.jsonl")

	// Write initial content.
	initial := `{"type":"assistant","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","cwd":"/tmp","message":{"role":"assistant","model":"m","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"echo hi"}}]}}` + "\n" +
		`{"type":"user","timestamp":"2026-01-01T10:00:01Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"hi"}]}}` + "\n"
	if err := os.WriteFile(sessionFile, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	adapter := &ClaudeCodeAdapter{Dir: dir}
	w := NewWatcherWithConfig(WatchConfig{
		IdleConfig:   IdleConfig{IdleAfter: 1 * time.Hour},
		PollInterval: 50 * time.Millisecond,
	}, []Adapter{adapter})
	defer w.Stop()

	go w.Start(context.Background())

	// Wait for the first event.
	var got []Event
	timeout := time.After(3 * time.Second)
	for len(got) < 1 {
		select {
		case ev := <-w.Events():
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timed out waiting for first event, got %d", len(got))
		}
	}

	// Append new content.
	appended := `{"type":"assistant","timestamp":"2026-01-01T10:01:00Z","sessionId":"s1","message":{"role":"assistant","model":"m","content":[{"type":"tool_use","id":"c2","name":"Read","input":{"file_path":"x.go"}}]}}` + "\n" +
		`{"type":"user","timestamp":"2026-01-01T10:01:01Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c2","content":"package x"}]}}` + "\n"
	time.Sleep(100 * time.Millisecond) // ensure EndedAt changes on next poll
	f, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(appended); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Wait for the second event (from incremental parse).
	timeout = time.After(3 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-w.Events():
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timed out waiting for second event, got %d", len(got))
		}
	}

	// Verify the second event is the new one.
	if got[1].Classified.Seq != 1 {
		t.Errorf("second event seq = %d, want 1", got[1].Classified.Seq)
	}
	if got[1].Classified.Action != "read" {
		t.Errorf("second event action = %q, want read", got[1].Classified.Action)
	}
}
