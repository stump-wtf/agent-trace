package tail

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
)

// finishErrorParts is the last message of a real Crush run that died: an
// assistant row holding only a finish part with reason "error".
const finishErrorParts = `[{"type":"finish","data":{"reason":"error","time":1789127781,"message":"Bad Request","details":"litellm.ContextWindowExceededError: prompt contains at least 196609 input tokens"}}]`

// newCrushSession creates a database holding one top-level session.
func newCrushSession(t *testing.T, sessionID string, now int64) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "crush.db")
	createTestCrushDB(t, dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, 'Run', NULL, ?, ?)`,
		sessionID, now, now+60); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

// insertCrushMessages appends one message per parts string, a second apart
// from start. Timestamps are Unix seconds, as Crush stores them.
func insertCrushMessages(t *testing.T, dbPath, sessionID string, start int64, roles, parts []string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for i, p := range parts {
		id := fmt.Sprintf("%s-%d-%d", sessionID, start, i)
		if _, err := db.Exec(`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, sessionID, roles[i], p, "test-model", start+int64(i), start+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
}

// errorMarksIn returns the marks of type "error".
func errorMarksIn(marks []classify.Mark) []classify.Mark {
	var out []classify.Mark
	for _, m := range marks {
		if m.Type == "error" {
			out = append(out, m)
		}
	}
	return out
}

// TestCrushParseEmitsFinishErrorMark checks that a failed turn surfaces as an
// "error" mark carrying the provider's message and details, at the time of the
// message that recorded it — and that ordinary finishes do not.
func TestCrushParseEmitsFinishErrorMark(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	now := time.Now().Unix()
	dbPath := newCrushSession(t, "died", now)
	insertCrushMessages(t, dbPath, "died", now, []string{"user", "assistant", "tool", "assistant", "assistant"}, []string{
		`[{"type":"text","data":{"text":"run the sweep"}}]`,
		`[{"type":"tool_call","data":{"id":"call-1","name":"view","input":"{\"file_path\":\"a.go\"}","finished":true}},{"type":"finish","data":{"reason":"tool_use"}}]`,
		`[{"type":"tool_result","data":{"tool_call_id":"call-1","name":"view","content":"package a"}}]`,
		`[{"type":"finish","data":{"reason":"stop"}}]`,
		finishErrorParts,
	})

	events, marks, _, err := CrushAdapter{DBPath: dbPath, Cwd: "/test"}.Parse(t.Context(), dbPath+"/died")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("events = %d, want 1 (a finish part must not disturb tool pairing)", len(events))
	}
	errs := errorMarksIn(marks)
	if len(errs) != 1 {
		t.Fatalf("error marks = %d (%+v), want exactly 1 — tool_use and stop finishes are not errors", len(errs), marks)
	}
	if !strings.HasPrefix(errs[0].Note, "Bad Request: litellm.ContextWindowExceededError") {
		t.Errorf("note = %q, want message then details", errs[0].Note)
	}
	if want := secToRFC3339(1789127781); errs[0].Timestamp != want {
		t.Errorf("timestamp = %q, want the finish part's own time %q, not the row's", errs[0].Timestamp, want)
	}
}

// TestCrushFinishTimestamp: a finish part with no time keeps the row's.
func TestCrushFinishTimestamp(t *testing.T) {
	row := secToRFC3339(1789127000)
	if got := crushFinishTimestamp(crushPartData{Time: 1789127300}, row); got != secToRFC3339(1789127300) {
		t.Errorf("with time = %q, want the part's", got)
	}
	if got := crushFinishTimestamp(crushPartData{}, row); got != row {
		t.Errorf("without time = %q, want the row's %q", got, row)
	}
}

// TestCrushParseSinceEmitsFinishErrorOnce checks the incremental path: the
// error mark arrives in the poll that first reads it, the watermark moves past
// it, and the next poll does not emit it again.
func TestCrushParseSinceEmitsFinishErrorOnce(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	now := time.Now().Unix()
	dbPath := newCrushSession(t, "incr", now)
	insertCrushMessages(t, dbPath, "incr", now, []string{"user", "assistant", "tool"}, []string{
		`[{"type":"text","data":{"text":"run the sweep"}}]`,
		`[{"type":"tool_call","data":{"id":"call-1","name":"view","input":"{\"file_path\":\"a.go\"}","finished":true}}]`,
		`[{"type":"tool_result","data":{"tool_call_id":"call-1","name":"view","content":"package a"}}]`,
	})
	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	path := dbPath + "/incr"

	events, marks, _, wm, err := a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("first ParseSince: %v", err)
	}
	if errs := errorMarksIn(marks); len(errs) != 0 {
		t.Fatalf("error mark before any failure was written: %+v", errs)
	}

	insertCrushMessages(t, dbPath, "incr", now+10, []string{"assistant"}, []string{finishErrorParts})

	_, marks2, _, wm2, err := a.ParseSince(t.Context(), path, wm, len(events))
	if err != nil {
		t.Fatalf("second ParseSince: %v", err)
	}
	if errs := errorMarksIn(marks2); len(errs) != 1 {
		t.Fatalf("second poll error marks = %d (%+v), want 1", len(errs), marks2)
	}
	if wm2 <= wm {
		t.Errorf("watermark did not advance past the finish row: %d -> %d", wm, wm2)
	}

	_, marks3, _, wm3, err := a.ParseSince(t.Context(), path, wm2, len(events))
	if err != nil {
		t.Fatalf("third ParseSince: %v", err)
	}
	if len(marks3) != 0 || wm3 != wm2 {
		t.Errorf("third poll re-read the finish: marks=%+v watermark %d -> %d", marks3, wm2, wm3)
	}
}

func TestCrushFinishErrorNote(t *testing.T) {
	cases := []struct {
		d    crushPartData
		want string
	}{
		{crushPartData{Message: "Bad Request", Details: "too long"}, "Bad Request: too long"},
		{crushPartData{Message: "Bad Request"}, "Bad Request"},
		{crushPartData{Details: "  too long  "}, "too long"},
		{crushPartData{}, "agent turn finished with an error"},
	}
	for _, tc := range cases {
		if got := crushFinishErrorNote(tc.d); got != tc.want {
			t.Errorf("crushFinishErrorNote(%+v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
