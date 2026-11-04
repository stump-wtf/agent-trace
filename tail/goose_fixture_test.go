package tail

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
)

// gooseFixtureName is the recorded Goose session database that #168 drops into
// tail/testdata. Goose keeps it at ~/.local/share/goose/sessions/sessions.db;
// copying that file here - with the secrets redacted the way the JSONL fixtures
// under this directory are - is the whole of the recording step. Until it
// lands, every test in this file skips, and the Goose adapter is pinned only
// to the schema its tests invent.
const gooseFixtureName = "goose_sessions.db"

// gooseFixture returns the path to the recorded Goose database. It skips
// instead of passing: a green run here must never read as validation of the
// adapter against a real Goose install, so the message names both the missing
// path and the issue that records it.
func gooseFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join("testdata", gooseFixtureName)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no recorded Goose session at tail/%s (%v): #168 owes this file. "+
			"Until it lands the Goose adapter is tested only against a schema the "+
			"test suite writes itself, so this skip is NOT validation of a real Goose install.", path, err)
	}
	return path
}

// openGooseFixture opens the recorded database the way the adapter does:
// read-only, so no test can move a real session's rows. The handle belongs to
// openSQLite's cache (see sqlite_helpers.go); resetDBCache in
// sqlite_helpers_test.go is what closes it, so this test must not.
func openGooseFixture(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := openSQLite(path)
	if err != nil {
		t.Fatalf("opening recorded Goose session %s: %v", path, err)
	}
	return db
}

// gooseFixtureSession picks the session with the most message rows, which is
// the one that exercises the parser the most.
func gooseFixtureSession(t *testing.T, db *sql.DB) string {
	t.Helper()
	var sessionID string
	err := db.QueryRow(`SELECT session_id FROM messages
		 GROUP BY session_id ORDER BY COUNT(*) DESC, session_id ASC LIMIT 1`).Scan(&sessionID)
	if err == sql.ErrNoRows {
		t.Skip("recorded Goose session contains no message rows; nothing to parse")
	}
	if err != nil {
		t.Fatalf("listing recorded Goose sessions: %v", err)
	}
	return sessionID
}

// gooseExpectedColumns mirrors the columns the queries in goose.go name. It is
// the schema question #169 leaves open: whether a real Goose install writes
// exactly these tables and columns. A mismatch is reported as the schema Goose
// actually wrote, not as an opaque "no such column" from the adapter.
var gooseExpectedColumns = map[string][]string{
	"sessions": {"id", "name", "working_dir", "session_type", "parent_session_id",
		"provider_name", "model_config_json", "created_at", "updated_at"},
	"messages": {"id", "session_id", "role", "content_json", "created_timestamp"},
	"usage_ledger": {"session_id", "created_timestamp", "model", "input_tokens", "output_tokens",
		"cache_read_tokens", "cache_write_tokens", "cost", "is_compaction"},
}

func TestGooseFixtureSchemaMatchesAdapterQueries(t *testing.T) {
	path := gooseFixture(t)
	db := openGooseFixture(t, path)

	for _, table := range []string{"sessions", "messages", "usage_ledger"} {
		want := gooseExpectedColumns[table]
		got, err := gooseTableColumns(db, table)
		if err != nil {
			t.Errorf("table %s in %s: %v", table, path, err)
			continue
		}
		if len(got) == 0 {
			t.Errorf("table %s is missing from %s; Goose wrote: %v", table, path, gooseTableNames(db))
			continue
		}
		for _, col := range want {
			if !gooseHasColumn(got, col) {
				t.Errorf("goose.go reads %s.%s, which %s does not have; it has: %v",
					table, col, path, got)
			}
		}
	}
}

func TestGooseFixtureSessionRead(t *testing.T) {
	path := gooseFixture(t)
	db := openGooseFixture(t, path)
	sessionID := gooseFixtureSession(t, db)
	a := GooseAdapter{DBPath: path}
	ctx := context.Background()
	sessionPath := path + "/" + sessionID

	meta, err := a.Summarize(ctx, sessionPath)
	if err != nil {
		t.Fatalf("Summarize on recorded session %s: %v", sessionID, err)
	}
	if meta.ID != sessionID {
		t.Errorf("meta.ID = %q, want %q", meta.ID, sessionID)
	}
	for label, ts := range map[string]string{"StartedAt": meta.StartedAt, "EndedAt": meta.EndedAt} {
		if ts == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Errorf("meta.%s = %q is not RFC3339: gooseTimeToRFC3339 does not match how Goose writes sessions.created_at/updated_at", label, ts)
		}
	}

	if wm, want := a.Watermark(ctx, sessionPath), gooseMaxMessageID(t, db, sessionID); wm != want {
		t.Errorf("Watermark = %d, want MAX(id) = %d: the watermark is not messages.id ordering on a real database", wm, want)
	}

	items, parseMeta, err := a.ParseItems(ctx, sessionPath)
	if err != nil {
		t.Fatalf("ParseItems on recorded session %s: %v", sessionID, err)
	}
	if parseMeta.ID != sessionID {
		t.Errorf("ParseItems meta.ID = %q, want %q", parseMeta.ID, sessionID)
	}
	if gooseMessageCount(t, db, sessionID) > 0 && len(items.Events)+len(items.Marks)+len(items.Usage) == 0 {
		t.Errorf("recorded session %s has messages but produced no events, marks or usage: Goose writes something the parser does not recognise", sessionID)
	}
	for _, ev := range items.Events {
		if ev.Tool == "" {
			t.Errorf("event %+v has no tool name: Goose names tool calls somewhere parseGooseToolRequest does not look", ev)
		}
	}
	for _, u := range items.Usage {
		if u.InputTokens == 0 && u.OutputTokens == 0 && u.CostUSD == nil {
			t.Errorf("usage row %+v carries no tokens and no cost: usage_ledger columns are read wrong", u)
		}
	}

	// The listing query is the other half of the schema question: a recorded
	// session that is not a sub-agent must survive it.
	if !gooseIsAuxiliary(t, db, sessionID) {
		metas, err := a.ListSessions(ctx)
		if err != nil {
			t.Fatalf("ListSessions on %s: %v", path, err)
		}
		var found bool
		for _, m := range metas {
			if m.ID == sessionID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("recorded session %s is absent from ListSessions (%d sessions): the listing query does not match what Goose writes", sessionID, len(metas))
		}
	}
}

func TestGooseFixtureReplayWatermarkParity(t *testing.T) {
	path := gooseFixture(t)
	db := openGooseFixture(t, path)
	sessionID := gooseFixtureSession(t, db)
	a := GooseAdapter{DBPath: path}
	ctx := context.Background()
	sessionPath := path + "/" + sessionID
	ids := gooseMessageIDs(t, db, sessionID)
	if len(ids) < 2 {
		t.Skipf("recorded session %s has %d message rows, too few to replay", sessionID, len(ids))
	}

	full, _, err := a.ParseItems(ctx, sessionPath)
	if err != nil {
		t.Fatalf("ParseItems on recorded session %s: %v", sessionID, err)
	}
	wantWM := ids[len(ids)-1]

	// Re-read the session the way a watcher does: each read carries the last
	// watermark and starts above it. Nothing new may arrive afterwards, and
	// every item must arrive once.
	var events []classify.Event
	var marks []classify.Mark
	var usage []classify.Usage
	watermark, seq, reads := int64(0), 0, 0
	for {
		reads++
		if reads > len(ids)+2 {
			t.Fatalf("replay of %s made %d reads without settling", sessionID, reads)
		}
		items, _, wm, err := a.ParseItemsSince(ctx, sessionPath, watermark, seq)
		if err != nil {
			t.Fatalf("replay read %d on %s: %v", reads, sessionID, err)
		}
		if wm < watermark {
			t.Fatalf("read %d moved the watermark backwards: %d then %d", reads, watermark, wm)
		}
		events = append(events, items.Events...)
		marks = append(marks, items.Marks...)
		usage = append(usage, items.Usage...)
		seq += len(items.Events)
		if wm == watermark {
			break
		}
		watermark = wm
	}
	if watermark != wantWM {
		t.Errorf("replay settled at watermark %d, want MAX(id) = %d", watermark, wantWM)
	}
	if len(marks) != len(full.Marks) {
		t.Errorf("marks across the replay = %d, full parse = %d", len(marks), len(full.Marks))
	}
	if len(usage) != len(full.Usage) {
		t.Errorf("usage across the replay = %d, full parse = %d", len(usage), len(full.Usage))
	}
	if len(events) > len(full.Events) {
		t.Errorf("replay produced %d events, more than the full parse's %d", len(events), len(full.Events))
	}
	// A full parse also flushes a tool call left open at the end of the
	// session, so the replay may legitimately hold fewer events; none of them
	// may be a call the full parse did not see. See goose.go:465.
	missing := 0
	for _, ev := range events {
		if !gooseHasEvent(full.Events, ev) {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d replayed events are absent from the full parse: a tool call paired across reads does not match its response", missing)
	}
}

// TestGooseFixtureWatermarkBoundaries reads from partway through a real
// session, where a tool request can sit below the watermark and its response
// above it. The watermark must be the newest id above the boundary, and a call
// split across the boundary must still be emitted.
func TestGooseFixtureWatermarkBoundaries(t *testing.T) {
	path := gooseFixture(t)
	db := openGooseFixture(t, path)
	sessionID := gooseFixtureSession(t, db)
	a := GooseAdapter{DBPath: path}
	ctx := context.Background()
	sessionPath := path + "/" + sessionID

	ids := gooseMessageIDs(t, db, sessionID)
	if len(ids) < 2 {
		t.Skipf("recorded session %s has %d message rows, too few to split", sessionID, len(ids))
	}
	_, parseMeta, err := a.ParseItems(ctx, sessionPath)
	if err != nil {
		t.Fatalf("ParseItems on recorded session %s: %v", sessionID, err)
	}
	if parseMeta.ID != sessionID {
		t.Fatalf("unexpected session from fixture: %q", parseMeta.ID)
	}

	step := max(1, len(ids)/8)
	for i := 0; i < len(ids)-1; i += step {
		boundary := ids[i]
		items, _, wm, err := a.ParseItemsSince(ctx, sessionPath, boundary, 0)
		if err != nil {
			t.Fatalf("read above watermark %d in %s: %v", boundary, sessionID, err)
		}
		if want := gooseMaxMessageIDAbove(t, db, sessionID, boundary); wm != want {
			t.Errorf("read above %d returned watermark %d, want %d", boundary, wm, want)
		}
		for _, ev := range items.Events {
			if ev.Timestamp == "" {
				t.Errorf("event %+v above watermark %d has no timestamp", ev, boundary)
			}
		}
	}
}

func gooseTableNames(db *sql.DB) []string {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return names
		}
		names = append(names, name)
	}
	return names
}

func gooseTableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return cols, err
		}
		cols = append(cols, col)
	}
	return cols, rows.Err()
}

func gooseHasColumn(cols []string, want string) bool {
	for _, col := range cols {
		if col == want {
			return true
		}
	}
	return false
}

func gooseMessageIDs(t *testing.T, db *sql.DB, sessionID string) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM messages WHERE session_id = ? ORDER BY id ASC`, sessionID)
	if err != nil {
		t.Fatalf("reading message ids for %s: %v", sessionID, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning message ids for %s: %v", sessionID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading message ids for %s: %v", sessionID, err)
	}
	return ids
}

func gooseMessageCount(t *testing.T, db *sql.DB, sessionID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		t.Fatalf("counting messages for %s: %v", sessionID, err)
	}
	return n
}

func gooseMaxMessageID(t *testing.T, db *sql.DB, sessionID string) int64 {
	t.Helper()
	var maxID sql.NullInt64
	err := db.QueryRow(`SELECT MAX(id) FROM messages WHERE session_id = ?`, sessionID).Scan(&maxID)
	if err != nil || !maxID.Valid {
		t.Fatalf("reading MAX(id) for %s: %v", sessionID, err)
	}
	return maxID.Int64
}

func gooseMaxMessageIDAbove(t *testing.T, db *sql.DB, sessionID string, watermark int64) int64 {
	t.Helper()
	var maxID sql.NullInt64
	err := db.QueryRow(`SELECT MAX(id) FROM messages WHERE session_id = ? AND id > ?`, sessionID, watermark).Scan(&maxID)
	if err != nil {
		t.Fatalf("reading MAX(id) above %d for %s: %v", watermark, sessionID, err)
	}
	if !maxID.Valid {
		return watermark
	}
	return maxID.Int64
}

func gooseIsAuxiliary(t *testing.T, db *sql.DB, sessionID string) bool {
	t.Helper()
	var sessionType string
	var parent sql.NullString
	err := db.QueryRow(`SELECT session_type, parent_session_id FROM sessions WHERE id = ?`, sessionID).Scan(&sessionType, &parent)
	if err != nil {
		t.Fatalf("reading session_type for %s: %v", sessionID, err)
	}
	return sessionType == "sub_agent" || (parent.Valid && parent.String != "")
}

// gooseHasEvent matches an event the replay emitted against the full parse, by
// the fields that identify a tool call's result.
func gooseHasEvent(full []classify.Event, ev classify.Event) bool {
	for _, other := range full {
		if other.Tool == ev.Tool && other.Action == ev.Action && other.Timestamp == ev.Timestamp && other.Summary == ev.Summary {
			return true
		}
	}
	return false
}
