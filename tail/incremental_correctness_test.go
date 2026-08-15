package tail

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	_ "modernc.org/sqlite"
)

// appendLines appends to an existing session file, the way a live harness does.
func appendLines(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func ccToolUse(id, name, ts string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","sessionId":"s1","cwd":"/tmp","message":{"role":"assistant","model":"m","content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":{"file_path":"main.go"}}]}}` + "\n"
}

func ccToolResult(id, ts string) string {
	return `{"type":"user","timestamp":"` + ts + `","sessionId":"s1","cwd":"/tmp","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"package main"}]}}` + "\n"
}

// TestParseSinceHoldsUnresolvedCall covers the failure that made incremental
// parsing lose events: a tool_use and its tool_result land in different polls,
// because the tool takes longer to run than the poll interval.
func TestParseSinceHoldsUnresolvedCall(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", ccToolUse("c1", "Read", "2026-01-01T10:00:00Z"))
	a := ClaudeCodeAdapter{}

	// Poll 1: the call is open, so nothing is emitted and the watermark must
	// not advance past it.
	events, _, _, wm, err := a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("poll 1: got %d events, want 0 while the call is unresolved", len(events))
	}
	if wm != 0 {
		t.Errorf("poll 1: watermark advanced to %d past an unresolved call, want 0", wm)
	}

	// Poll 2: the result lands and the call is emitted exactly once.
	appendLines(t, path, ccToolResult("c1", "2026-01-01T10:00:09Z"))
	events, _, _, wm2, err := a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("poll 2: got %d events, want 1", len(events))
	}
	if events[0].Action != classify.ActionRead {
		t.Errorf("action = %q, want read", events[0].Action)
	}

	// Poll 3: nothing new, and nothing repeated.
	events, _, _, _, err = a.ParseSince(t.Context(), path, wm2, 1)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("poll 3: got %d events, want 0 (no re-emission)", len(events))
	}
}

// TestParseSinceSurvivesPartialLine covers a watermark taken while the harness
// was mid-write: the fragment must be re-read whole, not skipped.
func TestParseSinceSurvivesPartialLine(t *testing.T) {
	full := ccToolUse("c1", "Read", "2026-01-01T10:00:00Z")
	full = strings.TrimSuffix(full, "\n")
	partial, rest := full[:60], full[60:]

	path := writeTempJSONL(t, "s.jsonl", partial)
	a := ClaudeCodeAdapter{}

	wm := a.Watermark(t.Context(), path)
	if wm != 0 {
		t.Fatalf("Watermark on a file with no complete line = %d, want 0", wm)
	}

	appendLines(t, path, rest+"\n"+ccToolResult("c1", "2026-01-01T10:00:01Z"))
	events, _, _, _, err := a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 — the record straddling the watermark was lost", len(events))
	}
}

// TestParseSinceClassifiesAgainstSessionCwd covers events being classified
// against an empty cwd, which silently changes how every relative path in the
// session resolves.
func TestParseSinceClassifiesAgainstSessionCwd(t *testing.T) {
	head := ccToolUse("c0", "Read", "2026-01-01T10:00:00Z") + ccToolResult("c0", "2026-01-01T10:00:01Z")
	path := writeTempJSONL(t, "s.jsonl", head)
	a := ClaudeCodeAdapter{}

	_, _, fullMeta, err := a.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wm := a.Watermark(t.Context(), path)

	appendLines(t, path, ccToolUse("c1", "Read", "2026-01-01T10:01:00Z")+ccToolResult("c1", "2026-01-01T10:01:01Z"))
	_, _, incMeta, _, err := a.ParseSince(t.Context(), path, wm, 1)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if incMeta.Cwd != fullMeta.Cwd {
		t.Errorf("ParseSince cwd = %q, want %q (same as a full Parse)", incMeta.Cwd, fullMeta.Cwd)
	}
	if incMeta.Cwd == "" {
		t.Error("ParseSince produced an empty cwd; every relative path would resolve against nothing")
	}
}

// TestParseSinceTruncatesMarkNotes keeps ParseSince's marks identical in shape
// to Parse's, which bounds them at 2000 runes.
func TestParseSinceTruncatesMarkNotes(t *testing.T) {
	head := ccToolUse("c0", "Read", "2026-01-01T10:00:00Z") + ccToolResult("c0", "2026-01-01T10:00:01Z")
	path := writeTempJSONL(t, "s.jsonl", head)
	a := ClaudeCodeAdapter{}
	wm := a.Watermark(t.Context(), path)

	appendLines(t, path, `{"type":"user","timestamp":"2026-01-01T10:01:00Z","sessionId":"s1","cwd":"/tmp","message":{"role":"user","content":[{"type":"text","text":"`+strings.Repeat("x", 5000)+`"}]}}`+"\n")
	_, marks, _, _, err := a.ParseSince(t.Context(), path, wm, 1)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	found := false
	for _, m := range marks {
		if m.Type != "user-message" {
			continue
		}
		found = true
		if n := len([]rune(m.Note)); n > 2001 {
			t.Errorf("mark note = %d runes, want <= 2001 (2000 + ellipsis)", n)
		}
	}
	if !found {
		t.Fatal("no user-message mark emitted")
	}
}

// TestWatcherEmitsEachCallExactlyOnce drives the whole loop the way the watcher
// does, with a tool call left open across a poll boundary.
func TestWatcherEmitsEachCallExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(ccToolUse("c1", "Read", "2026-01-01T10:00:00Z")), 0o644); err != nil {
		t.Fatal(err)
	}

	w := NewWatcherWithConfig(WatchConfig{}, []Adapter{&ClaudeCodeAdapter{Dir: dir}})
	seen := map[int]int{}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range w.Events() {
			seen[ev.Classified.Seq]++
		}
	}()

	w.scanOnce(context.Background())
	appendLines(t, path, ccToolResult("c1", "2026-01-01T10:00:09Z"))
	w.scanOnce(context.Background())
	appendLines(t, path, ccToolUse("c2", "Read", "2026-01-01T10:01:00Z"))
	w.scanOnce(context.Background())
	appendLines(t, path, ccToolResult("c2", "2026-01-01T10:01:09Z"))
	w.scanOnce(context.Background())

	close(w.events)
	<-drained

	if len(seen) != 2 {
		t.Fatalf("got %d distinct events, want 2 (one per tool call): %v", len(seen), seen)
	}
	for seq, n := range seen {
		if n != 1 {
			t.Errorf("event seq %d emitted %d times, want exactly 1", seq, n)
		}
	}
}

// --- Crush ---

func insertCrushRow(t *testing.T, db *sql.DB, id string, parent any, created, updated int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?,?,?,?,?)`,
		id, "T", parent, created, updated); err != nil {
		t.Fatal(err)
	}
}

func insertCrushMsg(t *testing.T, db *sql.DB, id, session, role, parts string, at int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
		id, session, role, parts, "m", at, at); err != nil {
		t.Fatal(err)
	}
}

// TestCrushParseSinceNullParent covers the regression that stopped incremental
// parsing dead for every top-level Crush session: parent_session_id is NULL for
// exactly those, and scanning it into a string reported "session not found".
func TestCrushParseSinceNullParent(t *testing.T) {
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
	insertCrushRow(t, db, "s1", nil, now, now) // NULL, as real Crush writes
	insertCrushMsg(t, db, "m1", "s1", "assistant",
		`[{"type":"tool_call","data":{"id":"c1","name":"view","input":"{\"file_path\":\"a.go\"}"}}]`, now+1000)
	insertCrushMsg(t, db, "m2", "s1", "tool",
		`[{"type":"tool_result","data":{"tool_call_id":"c1","content":"package a"}}]`, now+2000)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	a := CrushAdapter{DBPath: dbPath, Cwd: dir}
	events, _, meta, _, err := a.ParseSince(t.Context(), dbPath+"/s1", 0, 0)
	if err != nil {
		t.Fatalf("ParseSince on a NULL-parent session: %v", err)
	}
	if meta.Auxiliary {
		t.Error("a NULL parent_session_id is a root session, not an auxiliary one")
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
}

// TestCrushParseSinceWatermarkIsMessageTime covers the watermark being taken
// from sessions.updated_at while the query filters on messages.created_at: any
// message written between the two clocks was skipped permanently.
func TestCrushParseSinceWatermarkIsMessageTime(t *testing.T) {
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
	// The session row is touched well after its latest message, which is what
	// a live harness does.
	insertCrushRow(t, db, "s1", nil, now, now+60_000)
	insertCrushMsg(t, db, "m1", "s1", "assistant",
		`[{"type":"tool_call","data":{"id":"c1","name":"view","input":"{\"file_path\":\"a.go\"}"}}]`, now+1000)
	insertCrushMsg(t, db, "m2", "s1", "tool",
		`[{"type":"tool_result","data":{"tool_call_id":"c1","content":"package a"}}]`, now+2000)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	a := CrushAdapter{DBPath: dbPath, Cwd: dir}
	events, _, _, wm, err := a.ParseSince(t.Context(), dbPath+"/s1", 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("first pass: got %d events, want 1", len(events))
	}
	if wm != now+2000 {
		t.Fatalf("watermark = %d, want %d (the last message's created_at, not the session's updated_at)", wm, now+2000)
	}

	// A later message that still predates sessions.updated_at must be seen.
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	insertCrushMsg(t, db, "m3", "s1", "assistant",
		`[{"type":"tool_call","data":{"id":"c2","name":"view","input":"{\"file_path\":\"b.go\"}"}}]`, now+3000)
	insertCrushMsg(t, db, "m4", "s1", "tool",
		`[{"type":"tool_result","data":{"tool_call_id":"c2","content":"package b"}}]`, now+4000)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	resetDBCache()

	events, _, _, _, err = a.ParseSince(t.Context(), dbPath+"/s1", wm, 1)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("second pass: got %d events, want 1 — a message below sessions.updated_at was skipped", len(events))
	}
}

// TestCrushParseSinceHoldsUnresolvedCall is the Crush twin of
// TestParseSinceHoldsUnresolvedCall: call and result are separate message rows.
func TestCrushParseSinceHoldsUnresolvedCall(t *testing.T) {
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
	insertCrushRow(t, db, "s1", nil, now, now)
	insertCrushMsg(t, db, "m1", "s1", "assistant",
		`[{"type":"tool_call","data":{"id":"c1","name":"view","input":"{\"file_path\":\"a.go\"}"}}]`, now+1000)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	a := CrushAdapter{DBPath: dbPath, Cwd: dir}
	events, _, _, wm, err := a.ParseSince(t.Context(), dbPath+"/s1", 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want 0 while the call is unresolved", len(events))
	}
	if wm != 0 {
		t.Errorf("watermark = %d, want 0 — it must not advance past an open call", wm)
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	insertCrushMsg(t, db, "m2", "s1", "tool",
		`[{"type":"tool_result","data":{"tool_call_id":"c1","content":"package a"}}]`, now+2000)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	resetDBCache()

	events, _, _, _, err = a.ParseSince(t.Context(), dbPath+"/s1", wm, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 once the result landed", len(events))
	}
}
