package tail

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// OpenCode collects user-message marks in a pass that runs after the parts
// loop, so the running seq is already past every event by then. Numbering the
// marks with it puts them beyond the last event, where the watcher's
// attach-to-the-next-event delivery can never reach them — every user message
// in the session is silently dropped. These tests pin the marks to their
// place in the event timeline instead.

// buildOpenCodeMarkSeqDB writes a session with two tool parts and user
// messages before, between, and after them.
func buildOpenCodeMarkSeqDB(t *testing.T, dbPath, sessionID string) {
	t.Helper()
	createTestOpenCodeDB(t, dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES (?,?,?,?,?,?,?,?)`,
		sessionID, "proj", "slug", "/test", "S", "1.0", 1784148210000, 1784148260000); err != nil {
		t.Fatal(err)
	}
	msgs := []struct {
		id   string
		ts   int64
		data string
	}{
		{"m1", 1784148215000, `{"role":"user","content":"before any tool"}`},
		{"m2", 1784148220000, `{"role":"assistant","content":"working"}`},
		{"m3", 1784148235000, `{"role":"user","content":"between the tools"}`},
		{"m4", 1784148250000, `{"role":"user","content":"after every tool"}`},
	}
	for _, m := range msgs {
		if _, err := db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)`,
			m.id, sessionID, m.ts, m.ts, m.data); err != nil {
			t.Fatal(err)
		}
	}
	// Two tool parts on m2, at 1784148230000 and 1784148240000 — one before
	// and one after the "between the tools" message.
	parts := []struct {
		id string
		ts int64
	}{
		{"p1", 1784148230000},
		{"p2", 1784148240000},
	}
	for _, p := range parts {
		data := `{"type":"tool","tool":"Bash","callID":"` + p.id + `","state":{"status":"completed","input":{"command":"ls"},"output":"ok"}}`
		if _, err := db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?,?)`,
			p.id, "m2", sessionID, p.ts, p.ts, data); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenCodeUserMarkSeqTracksEventTimeline(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	buildOpenCodeMarkSeqDB(t, dbPath, "ses_seq")

	a := OpenCodeAdapter{DBPath: dbPath}
	events, marks, _, err := a.Parse(context.Background(), dbPath+"/ses_seq")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}

	want := map[string]int{
		"before any tool":   0, // rides the first event
		"between the tools": 1, // rides the second event
		"after every tool":  2, // parked until the session's next event
	}
	got := map[string]int{}
	for _, m := range marks {
		if m.Type == "user-message" {
			got[m.Note] = m.Seq
		}
	}
	if len(got) != len(want) {
		t.Fatalf("user-message marks = %v, want %d of them", got, len(want))
	}
	for note, wantSeq := range want {
		if gotSeq, ok := got[note]; !ok {
			t.Errorf("missing user-message mark %q", note)
		} else if gotSeq != wantSeq {
			t.Errorf("mark %q: seq = %d, want %d", note, gotSeq, wantSeq)
		}
	}
}

// The regression itself: no user-message mark may be numbered past the last
// event, because nothing can ever carry it.
func TestOpenCodeUserMarksAreReachableByEvents(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	buildOpenCodeMarkSeqDB(t, dbPath, "ses_reach")

	a := OpenCodeAdapter{DBPath: dbPath}
	events, marks, _, err := a.Parse(context.Background(), dbPath+"/ses_reach")
	if err != nil {
		t.Fatal(err)
	}
	maxEventSeq := -1
	for _, e := range events {
		if e.Seq > maxEventSeq {
			maxEventSeq = e.Seq
		}
	}
	reachable := 0
	for _, m := range marks {
		if m.Type == "user-message" && m.Seq <= maxEventSeq {
			reachable++
		}
	}
	// Two of the three sit before the last event; only the trailing one is
	// legitimately parked.
	if reachable != 2 {
		t.Errorf("user-message marks reachable by an event = %d, want 2 (max event seq %d)", reachable, maxEventSeq)
	}
}
