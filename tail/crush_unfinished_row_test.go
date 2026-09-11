package tail

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// updateCrushParts rewrites one message's parts in place, the way Crush
// streams an assistant turn into the row it created when the turn began.
func updateCrushParts(t *testing.T, dbPath, msgID, parts string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.Exec(`UPDATE messages SET parts = ? WHERE id = ?`, parts, msgID)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("updated %d rows for %s, want 1", n, msgID)
	}
	resetDBCache()
}

// TestCrushParseSinceRereadsStreamingAssistantRow is the live-watcher shape of
// a turn that dies on a provider error. Crush inserts the assistant row when
// the turn starts and writes its parts — the finish part last — into that same
// row as the stream progresses. A poll that lands mid-stream must not move the
// cursor past the row, or the finish error it later records is never read.
func TestCrushParseSinceRereadsStreamingAssistantRow(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	now := time.Now().Unix()
	dbPath := newCrushSession(t, "streaming", now)
	insertCrushMessages(t, dbPath, "streaming", now, []string{"user", "assistant"}, []string{
		`[{"type":"text","data":{"text":"run the sweep"}}]`,
		`[{"type":"text","data":{"text":"Let me"}}]`,
	})
	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	path := dbPath + "/streaming"

	_, marks, _, wm, err := a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("first ParseSince: %v", err)
	}
	if len(marks) != 1 || marks[0].Type != "user-message" {
		t.Fatalf("first poll marks = %+v, want the user message only", marks)
	}

	updateCrushParts(t, dbPath, fmt.Sprintf("streaming-%d-1", now), finishErrorParts)

	_, marks, _, _, err = a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("second ParseSince: %v", err)
	}
	if errs := errorMarksIn(marks); len(errs) != 1 {
		t.Errorf("second poll error marks = %d (%+v), want the finish error recorded after the first poll", len(errs), marks)
	}
	for _, m := range marks {
		if m.Type == "user-message" {
			t.Errorf("second poll re-emitted the user message: %+v", m)
		}
	}
}

// TestCrushWatermarkHoldsBelowStreamingRow: the watcher's first scan is a full
// Parse followed by Watermark, so Watermark must not skip a last row that is
// still being streamed into either — but a finished last row, or an
// unfinished one with rows after it, is complete.
func TestCrushWatermarkHoldsBelowStreamingRow(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	now := time.Now().Unix()
	dbPath := newCrushSession(t, "wm", now)
	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	path := dbPath + "/wm"

	if got := a.Watermark(t.Context(), path); got != 0 {
		t.Errorf("empty session watermark = %d, want 0", got)
	}

	insertCrushMessages(t, dbPath, "wm", now, []string{"assistant"}, []string{`[]`})
	resetDBCache()
	if got := a.Watermark(t.Context(), path); got != 0 {
		t.Errorf("lone streaming row watermark = %d, want 0", got)
	}

	insertCrushMessages(t, dbPath, "wm", now+1, []string{"user", "assistant"}, []string{
		`[{"type":"text","data":{"text":"again"}}]`,
		`[{"type":"text","data":{"text":"Let me"}}]`,
	})
	resetDBCache()
	// rowids 1..3: the unfinished row 1 is followed by rows, row 3 is streaming.
	if got := a.Watermark(t.Context(), path); got != 2 {
		t.Errorf("streaming tail watermark = %d, want 2 (the row before it)", got)
	}

	updateCrushParts(t, dbPath, fmt.Sprintf("wm-%d-1", now+1), finishErrorParts)
	if got := a.Watermark(t.Context(), path); got != 3 {
		t.Errorf("finished tail watermark = %d, want 3", got)
	}
}

// TestCrushParseSinceRereadsToolCallStreamedIntoRow is the same shape for a
// tool call: the call part lands in the assistant row after a poll already
// read it empty, and the result follows in a row of its own.
func TestCrushParseSinceRereadsToolCallStreamedIntoRow(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	now := time.Now().Unix()
	dbPath := newCrushSession(t, "calling", now)
	insertCrushMessages(t, dbPath, "calling", now, []string{"user", "assistant"}, []string{
		`[{"type":"text","data":{"text":"read a.go"}}]`,
		`[]`,
	})
	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	path := dbPath + "/calling"

	events, _, _, wm, err := a.ParseSince(t.Context(), path, 0, 0)
	if err != nil || len(events) != 0 {
		t.Fatalf("first ParseSince = %d events, %v; want none", len(events), err)
	}

	updateCrushParts(t, dbPath, fmt.Sprintf("calling-%d-1", now),
		`[{"type":"tool_call","data":{"id":"call-1","name":"view","input":"{\"file_path\":\"a.go\"}","finished":true}},{"type":"finish","data":{"reason":"tool_use"}}]`)
	insertCrushMessages(t, dbPath, "calling", now+5, []string{"tool"}, []string{
		`[{"type":"tool_result","data":{"tool_call_id":"call-1","name":"view","content":"package a"}}]`,
	})

	events, _, _, _, err = a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("second ParseSince: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("second poll events = %d, want the call streamed into the row after the first poll", len(events))
	}
}

// TestCrushParseSinceUnfinishedRowDoesNotStall bounds the rule above. A Crush
// killed mid-stream leaves an assistant row with no finish part forever; once
// anything follows it, that turn is over, and holding the cursor below it would
// withhold every later event from the watcher for good.
func TestCrushParseSinceUnfinishedRowDoesNotStall(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	now := time.Now().Unix()
	dbPath := newCrushSession(t, "resumed", now)
	insertCrushMessages(t, dbPath, "resumed", now, []string{"user", "assistant", "user", "assistant", "tool", "assistant"}, []string{
		`[{"type":"text","data":{"text":"first try"}}]`,
		`[{"type":"text","data":{"text":"killed mid-stream"}}]`,
		`[{"type":"text","data":{"text":"second try"}}]`,
		`[{"type":"tool_call","data":{"id":"call-1","name":"view","input":"{\"file_path\":\"a.go\"}","finished":true}},{"type":"finish","data":{"reason":"tool_use"}}]`,
		`[{"type":"tool_result","data":{"tool_call_id":"call-1","name":"view","content":"package a"}}]`,
		finishErrorParts,
	})
	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	path := dbPath + "/resumed"

	events, marks, _, wm, err := a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 || len(errorMarksIn(marks)) != 1 {
		t.Errorf("events = %d, error marks = %d (%+v); want 1 and 1 — the dead turn must not hold back what followed it", len(events), len(errorMarksIn(marks)), marks)
	}
	if full := a.Watermark(t.Context(), path); wm != full {
		t.Errorf("watermark = %d, want %d: every row is complete, so the cursor reaches the end", wm, full)
	}
}
