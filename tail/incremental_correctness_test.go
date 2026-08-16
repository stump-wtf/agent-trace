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

	// Fixed fixture timestamps; discovery scope is not what this tests.
	w := NewWatcherWithConfig(WatchConfig{MaxAge: -1}, []Adapter{&ClaudeCodeAdapter{Dir: dir}})
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

// --- Codex ---

func codexSessionMetaLine(ts string) string {
	return `{"type":"session_meta","timestamp":"` + ts + `","payload":{"id":"cx-1","cwd":"/tmp","git":{"branch":"main"}}}` + "\n"
}

func codexCallLine(callID, name, args, ts string) string {
	return `{"type":"response_item","timestamp":"` + ts + `","payload":{"type":"function_call","call_id":"` + callID + `","name":"` + name + `","arguments":` + args + `}}` + "\n"
}

func codexOutputLine(callID, output, ts string) string {
	return `{"type":"response_item","timestamp":"` + ts + `","payload":{"type":"function_call_output","call_id":"` + callID + `","output":` + output + `}}` + "\n"
}

func codexUserLine(text, ts string) string {
	return `{"type":"response_item","timestamp":"` + ts + `","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}}` + "\n"
}

// TestCodexParseSinceHoldsUnresolvedCall is the Codex twin of the Claude Code
// and Crush tests above: a function_call and its output land in different
// polls, and the mark truncates with the events.
func TestCodexParseSinceHoldsUnresolvedCall(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", codexSessionMetaLine("2026-01-01T10:00:00Z"))
	a := CodexAdapter{}
	wm0 := a.Watermark(t.Context(), path)

	appendLines(t, path, codexUserLine("run the tests", "2026-01-01T10:00:01Z")+codexCallLine("c1", "bash", `{"command":"ls -la"}`, "2026-01-01T10:00:02Z"))
	wmFull := a.Watermark(t.Context(), path)
	events, marks, _, wm, err := a.ParseSince(t.Context(), path, wm0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	// The user message precedes the open call, so its mark sits inside the
	// safe prefix and goes out now (the watcher parks it until an event
	// follows); the call itself withholds.
	if len(events) != 0 {
		t.Fatalf("poll 1: got %d events, want 0 while the call is unresolved", len(events))
	}
	if len(marks) != 1 || marks[0].Type != "user-message" || marks[0].Seq != 0 {
		t.Fatalf("poll 1: marks = %+v, want one user-message mark at seq 0", marks)
	}
	if wm >= wmFull {
		t.Errorf("poll 1: watermark advanced to %d at/past the unresolved call, want < %d", wm, wmFull)
	}

	appendLines(t, path, codexOutputLine("c1", `"ok"`, "2026-01-01T10:00:09Z"))
	wantEvents, _, _, err := a.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	events, marks, _, wm2, err := a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("poll 2: got %d events, want 1", len(events))
	}
	if len(wantEvents) == 1 && events[0].Action != wantEvents[0].Action {
		t.Errorf("action = %q, want %q (same as a full Parse)", events[0].Action, wantEvents[0].Action)
	}
	if len(marks) != 0 {
		t.Errorf("poll 2: marks = %+v, want none — the mark went out in poll 1", marks)
	}

	events, marks, _, _, err = a.ParseSince(t.Context(), path, wm2, 1)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 || len(marks) != 0 {
		t.Errorf("poll 3: got %d events / %d marks, want 0/0 (no re-emission)", len(events), len(marks))
	}
}

// TestCodexParseSincePatchEnrichmentAcrossPolls pins the patch lookahead: a
// real transcript writes patch_apply_end on the line AFTER the custom tool's
// output (testdata/codex_fidelity.jsonl), so a poll ending on the output must
// not advance the watermark past the pair — the enrichment belongs to the
// same window the event is emitted in.
func TestCodexParseSincePatchEnrichmentAcrossPolls(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", codexSessionMetaLine("2026-01-01T10:00:00Z"))
	a := CodexAdapter{}
	wm0 := a.Watermark(t.Context(), path)

	// The call and its output land between two incremental polls; the patch
	// event does not. The window sees both call and output, and must still
	// withhold: the pair's enrichment is one line away.
	appendLines(t, path,
		`{"type":"response_item","timestamp":"2026-01-01T10:00:01Z","payload":{"type":"custom_tool_call","call_id":"cp","name":"apply_patch","input":"*** Begin Patch\n*** Update File: a.go\n"}}`+"\n"+
			`{"type":"response_item","timestamp":"2026-01-01T10:00:02Z","payload":{"type":"custom_tool_call_output","call_id":"cp","output":"done"}}`+"\n")
	events, _, _, wm, err := a.ParseSince(t.Context(), path, wm0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("poll 1: got %d events, want 0 — the patch_apply_end has one more line to arrive in", len(events))
	}
	if wm != wm0 {
		t.Errorf("poll 1: watermark advanced to %d past a resolved patch call whose patch event has not arrived, want %d", wm, wm0)
	}

	appendLines(t, path, `{"type":"event_msg","timestamp":"2026-01-01T10:00:03Z","payload":{"type":"patch_apply_end","call_id":"cp","success":true,"changes":{"src/new.go":{"type":"add"}}}}`+"\n")
	events, _, _, wm2, err := a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("poll 2: got %d events, want 1", len(events))
	}
	found := false
	for _, target := range events[0].Targets {
		if target.Path == "src/new.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("patch enrichment missing: targets = %+v, want src/new.go from the authoritative change list", events[0].Targets)
	}
	if wm2 <= wm {
		t.Errorf("poll 2: watermark %d did not advance past %d", wm2, wm)
	}

	// A patch call whose next line is NOT a patch event releases the hold:
	// nothing is coming, and withholding forever would stall the session.
	path2 := writeTempJSONL(t, "s2.jsonl", codexSessionMetaLine("2026-01-01T11:00:00Z"))
	wmB := a.Watermark(t.Context(), path2)
	appendLines(t, path2,
		`{"type":"response_item","timestamp":"2026-01-01T11:00:01Z","payload":{"type":"custom_tool_call","call_id":"cq","name":"apply_patch","input":"*** Begin Patch\n*** Update File: b.go\n"}}`+"\n"+
			`{"type":"response_item","timestamp":"2026-01-01T11:00:02Z","payload":{"type":"custom_tool_call_output","call_id":"cq","output":"done"}}`+"\n"+
			codexUserLine("next turn", "2026-01-01T11:00:03Z"))
	events, _, _, wmC, err := a.ParseSince(t.Context(), path2, wmB, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("release: got %d events, want 1 once a non-patch line follows the output", len(events))
	}
	if wmC <= wmB {
		t.Errorf("release: watermark %d did not advance past %d", wmC, wmB)
	}
}

// --- Pi ---

func piHeaderLine(id, ts string) string {
	return `{"type":"session","id":"` + id + `","timestamp":"` + ts + `","cwd":"/tmp"}` + "\n"
}

func piEntryLine(id, parent, body, ts string) string {
	return `{"type":"message","id":"` + id + `","parentId":"` + parent + `","timestamp":"` + ts + `","message":` + body + `}` + "\n"
}

// TestPiParseSinceLinearAppend covers the common shape: entries appended in
// chain order extend the leaf, and only the appended records are read.
func TestPiParseSinceLinearAppend(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", piHeaderLine("s1", "2026-01-01T10:00:00Z")+
		piEntryLine("e1", "", `{"role":"user","content":[{"type":"text","text":"hello"}]}`, "2026-01-01T10:00:01Z"))
	a := PiAdapter{}
	wm := a.Watermark(t.Context(), path)

	appendLines(t, path,
		piEntryLine("e2", "e1", `{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}]}`, "2026-01-01T10:00:02Z"))
	events, _, _, wm2, err := a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("poll 1: got %d events, want 0 while the call is unresolved", len(events))
	}
	if wm2 != wm {
		t.Errorf("poll 1: watermark advanced to %d past an unresolved call, want %d", wm2, wm)
	}

	appendLines(t, path,
		piEntryLine("e3", "e2", `{"role":"toolResult","toolCallId":"c1","content":[{"type":"text","text":"ok"}]}`, "2026-01-01T10:00:03Z")+
			piEntryLine("e4", "e3", `{"role":"user","content":[{"type":"text","text":"again"}]}`, "2026-01-01T10:00:04Z"))
	events, marks, _, wm3, err := a.ParseSince(t.Context(), path, wm2, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 || events[0].Seq != 0 {
		t.Fatalf("poll 2: got %+v, want one event at seq 0", events)
	}
	if len(marks) != 1 || marks[0].Type != "user-message" || marks[0].Seq != 1 {
		t.Fatalf("poll 2: marks = %+v, want one user-message mark at seq 1", marks)
	}
	if wm3 <= wm2 {
		t.Errorf("poll 2: watermark %d did not advance past %d", wm3, wm2)
	}

	events, _, _, _, err = a.ParseSince(t.Context(), path, wm3, 1)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("poll 3: got %d events, want 0 (no re-emission)", len(events))
	}
}

// TestPiParseSinceV1Continuation covers pre-tree files: entries without ids
// pass through in file order, so every append is a continuation by
// construction.
func TestPiParseSinceV1Continuation(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", piHeaderLine("", "2026-01-01T10:00:00Z")+
		piEntryLine("", "", `{"role":"user","content":[{"type":"text","text":"hi"}]}`, "2026-01-01T10:00:01Z"))
	a := PiAdapter{}
	wm := a.Watermark(t.Context(), path)

	appendLines(t, path,
		piEntryLine("", "", `{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}]}`, "2026-01-01T10:00:02Z")+
			piEntryLine("", "", `{"role":"toolResult","toolCallId":"c1","content":[{"type":"text","text":"ok"}]}`, "2026-01-01T10:00:03Z"))
	events, _, _, _, err := a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 — v1 entries are a continuation by construction", len(events))
	}
}

// TestPiParseSinceBranchFallsBackToFullParse covers the shape incremental
// parsing cannot express: a new entry whose parent is not the previous leaf
// re-linearizes the chain, so the appended bytes are not the delta. The
// fallback returns the new timeline's events beyond what the watcher already
// emitted — the same thing a full-parse poll would have produced.
func TestPiParseSinceBranchFallsBackToFullParse(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", piHeaderLine("s1", "2026-01-01T10:00:00Z")+
		piEntryLine("e1", "", `{"role":"user","content":[{"type":"text","text":"v1"}]}`, "2026-01-01T10:00:01Z")+
		piEntryLine("e2", "e1", `{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}]}`, "2026-01-01T10:00:02Z")+
		piEntryLine("e3", "e2", `{"role":"toolResult","toolCallId":"c1","content":[{"type":"text","text":"ok"}]}`, "2026-01-01T10:00:03Z"))
	a := PiAdapter{}
	wm := a.Watermark(t.Context(), path)

	// A branch off e1: the new leaf's chain is e1 → b1 → b2 → b3 → b4, with
	// two tool calls, where the old chain had one.
	appendLines(t, path,
		piEntryLine("b1", "e1", `{"role":"user","content":[{"type":"text","text":"v2"}]}`, "2026-01-01T10:01:01Z")+
			piEntryLine("b2", "b1", `{"role":"assistant","content":[{"type":"toolCall","id":"c2","name":"bash","arguments":{"command":"pwd"}}]}`, "2026-01-01T10:01:02Z")+
			piEntryLine("b3", "b2", `{"role":"toolResult","toolCallId":"c2","content":[{"type":"text","text":"/tmp"}]}`, "2026-01-01T10:01:03Z")+
			piEntryLine("b4", "b3", `{"role":"assistant","content":[{"type":"toolCall","id":"c3","name":"bash","arguments":{"command":"date"}}]}`, "2026-01-01T10:01:04Z")+
			piEntryLine("b5", "b4", `{"role":"toolResult","toolCallId":"c3","content":[{"type":"text","text":"now"}]}`, "2026-01-01T10:01:05Z"))

	events, _, _, wm2, err := a.ParseSince(t.Context(), path, wm, 1)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 — the new timeline's second event, beyond the one already emitted", len(events))
	}
	if events[0].Seq != 1 {
		t.Errorf("seq = %d, want 1 (continues from startSeq)", events[0].Seq)
	}
	if len(events) == 1 && !strings.Contains(events[0].Summary, "date") {
		t.Errorf("delta picked the wrong call: summary = %q, want the c3 call from the new chain", events[0].Summary)
	}
	if wm2 <= wm {
		t.Errorf("watermark %d did not advance past %d", wm2, wm)
	}
}

// --- OpenCode ---

func openTestOpenCodeDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"ses_1", "proj", "slug", "/test", "T", "1.0", 1784148215000, 1784148215000); err != nil {
		t.Fatal(err)
	}
	return db, dbPath
}

func insertOpenCodeMessage(t *testing.T, db *sql.DB, id, session, data string, at int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		id, session, at, at, data); err != nil {
		t.Fatal(err)
	}
}

func insertOpenCodePart(t *testing.T, db *sql.DB, id, msgID, session, data string, at int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
		id, msgID, session, at, at, data); err != nil {
		t.Fatal(err)
	}
}

const ocCompletedWrite = `{"type":"tool","tool":"write","callID":"call-1","state":{"status":"completed","input":{"path":"src/login.go"},"output":"File written"}}`

// TestOpenCodeParseSinceMatchesFullParse is the oracle the FilteredLister
// work made standard, applied to incremental parsing: with no outstanding
// rows, an incremental window from zero must agree with a full Parse on
// events and on mark placement.
func TestOpenCodeParseSinceMatchesFullParse(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)
	db, dbPath := openTestOpenCodeDB(t)

	insertOpenCodeMessage(t, db, "m1", "ses_1", `{"role":"user","content":"fix the login bug"}`, 1784148216000)
	insertOpenCodeMessage(t, db, "m2", "ses_1", `{"role":"assistant","content":"on it"}`, 1784148217000)
	insertOpenCodePart(t, db, "p1", "m2", "ses_1", ocCompletedWrite, 1784148217100)
	insertOpenCodePart(t, db, "p2", "m2", "ses_1", `{"type":"tool","tool":"bash","callID":"call-2","state":{"status":"completed","input":{"command":"go build"},"output":"ok"}}`, 1784148217500)

	a := OpenCodeAdapter{DBPath: dbPath}
	wantEvents, wantMarks, _, err := a.Parse(t.Context(), dbPath+"/ses_1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	gotEvents, gotMarks, _, _, err := a.ParseSince(t.Context(), dbPath+"/ses_1", 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(gotEvents) != len(wantEvents) {
		t.Fatalf("events: ParseSince gave %d, Parse gave %d", len(gotEvents), len(wantEvents))
	}
	for i := range gotEvents {
		if gotEvents[i].Seq != wantEvents[i].Seq || gotEvents[i].Action != wantEvents[i].Action {
			t.Errorf("event %d: ParseSince gave seq=%d action=%q, Parse gave seq=%d action=%q",
				i, gotEvents[i].Seq, gotEvents[i].Action, wantEvents[i].Seq, wantEvents[i].Action)
		}
	}
	if len(gotMarks) != len(wantMarks) {
		t.Fatalf("marks: ParseSince gave %d, Parse gave %d", len(gotMarks), len(wantMarks))
	}
	for i := range gotMarks {
		if gotMarks[i].Seq != wantMarks[i].Seq || gotMarks[i].Type != wantMarks[i].Type {
			t.Errorf("mark %d: ParseSince gave seq=%d type=%q, Parse gave seq=%d type=%q",
				i, gotMarks[i].Seq, gotMarks[i].Type, wantMarks[i].Seq, wantMarks[i].Type)
		}
	}
}

// TestOpenCodeParseSinceHoldsRunningTool pins the mutation-aware withhold
// rule: an OpenCode tool call is one row whose state changes in place, so a
// running part is withheld and the watermark held back until the row turns
// terminal — at which point the event is emitted once, with its final result.
func TestOpenCodeParseSinceHoldsRunningTool(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)
	db, dbPath := openTestOpenCodeDB(t)

	insertOpenCodeMessage(t, db, "m1", "ses_1", `{"role":"assistant","content":""}`, 1784148217000)
	insertOpenCodePart(t, db, "p1", "m1", "ses_1", `{"type":"tool","tool":"bash","callID":"call-1","state":{"status":"running","input":{"command":"go test"}}}`, 1784148217100)

	a := OpenCodeAdapter{DBPath: dbPath}
	events, _, _, wm, err := a.ParseSince(t.Context(), dbPath+"/ses_1", 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("poll 1: got %d events, want 0 while the tool is running", len(events))
	}
	if wm != 0 {
		t.Errorf("poll 1: watermark advanced to %d past a running tool, want 0", wm)
	}

	// The row mutates in place: same time_created, new state.
	if _, err := db.Exec(`UPDATE part SET time_updated = ?, data = ? WHERE id = 'p1'`,
		1784148219000, `{"type":"tool","tool":"bash","callID":"call-1","state":{"status":"completed","input":{"command":"go test"},"output":"ok"}}`); err != nil {
		t.Fatal(err)
	}
	resetDBCache()

	events, _, _, wm2, err := a.ParseSince(t.Context(), dbPath+"/ses_1", wm, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("poll 2: got %d events, want 1 once the row turned terminal", len(events))
	}
	if wm2 != 1784148217100 {
		t.Errorf("poll 2: watermark = %d, want the part's time_created %d", wm2, 1784148217100)
	}

	events, _, _, _, err = a.ParseSince(t.Context(), dbPath+"/ses_1", wm2, 1)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("poll 3: got %d events, want 0 (no re-emission)", len(events))
	}
}

// TestOpenCodeParseSinceWatermarkTieHazard covers the timestamp tie the safe
// point stack exists for: a terminal part and a still-running part sharing
// time_created (millisecond resolution makes this routine). A watermark equal
// to that shared time would exclude the running row from every later poll —
// its completion would never be read.
func TestOpenCodeParseSinceWatermarkTieHazard(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)
	db, dbPath := openTestOpenCodeDB(t)

	insertOpenCodeMessage(t, db, "m1", "ses_1", `{"role":"assistant","content":""}`, 1784148217000)
	insertOpenCodePart(t, db, "p1", "m1", "ses_1", ocCompletedWrite, 1784148217100)
	insertOpenCodePart(t, db, "p2", "m1", "ses_1", `{"type":"tool","tool":"bash","callID":"call-2","state":{"status":"running","input":{"command":"go test"}}}`, 1784148217100)

	a := OpenCodeAdapter{DBPath: dbPath}
	events, _, _, wm, err := a.ParseSince(t.Context(), dbPath+"/ses_1", 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("poll 1: got %d events, want 0 — the tie withholds the whole window", len(events))
	}
	if wm >= 1784148217100 {
		t.Fatalf("poll 1: watermark = %d, want strictly below the tied time %d", wm, 1784148217100)
	}

	if _, err := db.Exec(`UPDATE part SET time_updated = ?, data = ? WHERE id = 'p2'`,
		1784148219000, `{"type":"tool","tool":"bash","callID":"call-2","state":{"status":"completed","input":{"command":"go test"},"output":"ok"}}`); err != nil {
		t.Fatal(err)
	}
	resetDBCache()

	events, _, _, wm2, err := a.ParseSince(t.Context(), dbPath+"/ses_1", wm, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("poll 2: got %d events, want 2 — both tied parts emitted exactly once", len(events))
	}
	if wm2 != 1784148217100 {
		t.Errorf("poll 2: watermark = %d, want %d", wm2, 1784148217100)
	}
}

// TestOpenCodeWatermarkCoversMessages pins Watermark to the max over BOTH
// tables: ParseSince filters its user-message pass on the same value, so a
// message-only session (a conversational turn) must still move it.
func TestOpenCodeWatermarkCoversMessages(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)
	db, dbPath := openTestOpenCodeDB(t)

	insertOpenCodePart(t, db, "p1", "m1", "ses_1", ocCompletedWrite, 1784148217100)
	insertOpenCodeMessage(t, db, "m2", "ses_1", `{"role":"user","content":"thanks"}`, 1784148218000)

	a := OpenCodeAdapter{DBPath: dbPath}
	if wm := a.Watermark(t.Context(), dbPath+"/ses_1"); wm != 1784148218000 {
		t.Fatalf("Watermark = %d, want the message time 1784148218000", wm)
	}
}

// --- Watcher wiring ---

// TestWatcherOpenCodeIncremental drives the full watcher loop for the adapter
// the issue was filed about: the first scan establishes the baseline with a
// full Parse, and later scans read only the rows past the watermark instead
// of re-deriving the whole session.
func TestWatcherOpenCodeIncremental(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)
	db, dbPath := openTestOpenCodeDB(t)

	insertOpenCodeMessage(t, db, "m1", "ses_1", `{"role":"user","content":"start"}`, 1784148216000)
	insertOpenCodePart(t, db, "p1", "m1", "ses_1", ocCompletedWrite, 1784148217100)

	w := NewWatcherWithConfig(WatchConfig{}, []Adapter{&OpenCodeAdapter{DBPath: dbPath}})
	seen := map[int]int{}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range w.Events() {
			seen[ev.Classified.Seq]++
		}
	}()

	w.scanOnce(context.Background())

	insertOpenCodePart(t, db, "p2", "m1", "ses_1", `{"type":"tool","tool":"bash","callID":"call-2","state":{"status":"completed","input":{"command":"go build"},"output":"ok"}}`, 1784148220000)
	if _, err := db.Exec(`UPDATE session SET time_updated = ? WHERE id = 'ses_1'`, 1784148220000); err != nil {
		t.Fatal(err)
	}
	resetDBCache()

	w.scanOnce(context.Background())
	close(w.events)
	<-drained

	if len(seen) != 2 {
		t.Fatalf("got %d distinct events, want 2: %v", len(seen), seen)
	}
	for seq, n := range seen {
		if n != 1 {
			t.Errorf("event seq %d emitted %d times, want exactly 1", seq, n)
		}
	}
}

// TestAllAdaptersImplementIncrementalParser keeps the optional-interface
// coverage honest: #62 existed because the PR that landed the interface only
// implemented it for two of five adapters, and nothing failed.
func TestAllAdaptersImplementIncrementalParser(t *testing.T) {
	for _, a := range DefaultAdapters() {
		if _, ok := a.(IncrementalParser); !ok {
			t.Errorf("%T does not implement IncrementalParser", a)
		}
	}
}

// TestCrushParseSinceWatermarkTieHazard is the Crush case of the rule the
// OpenCode adapter above states in full: messages.created_at has second
// resolution, so a resolved call and a still-open call routinely share a
// timestamp, and a safe point AT that second puts the watermark equal to the
// open call — excluded forever by the strict `created_at > watermark`.
func TestCrushParseSinceWatermarkTieHazard(t *testing.T) {
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
	// Two tool calls in the same second: one resolved within the message,
	// one left open.
	insertCrushMsg(t, db, "m1", "s1", "assistant",
		`[{"type":"tool_call","data":{"id":"c1","name":"view","input":"{\"file_path\":\"a.go\"}"}},{"type":"tool_result","data":{"tool_call_id":"c1","content":"ok"}}]`, now)
	insertCrushMsg(t, db, "m2", "s1", "assistant",
		`[{"type":"tool_call","data":{"id":"c2","name":"view","input":"{\"file_path\":\"b.go\"}"}}]`, now)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	a := CrushAdapter{DBPath: dbPath, Cwd: dir}
	events, _, _, wm, err := a.ParseSince(t.Context(), dbPath+"/s1", 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("poll 1: got %d events, want 0 — the tie withholds the whole window", len(events))
	}
	if wm >= now {
		t.Fatalf("poll 1: watermark = %d, want strictly below the tied second %d", wm, now)
	}

	// The result lands a second later; both calls must come out, exactly once.
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	insertCrushMsg(t, db, "m3", "s1", "tool",
		`[{"type":"tool_result","data":{"tool_call_id":"c2","content":"ok"}}]`, now+1000)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	resetDBCache()

	events, _, _, wm2, err := a.ParseSince(t.Context(), dbPath+"/s1", wm, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("poll 2: got %d events, want 2 — both tied-second calls emitted exactly once", len(events))
	}
	if wm2 != now+1000 {
		t.Errorf("poll 2: watermark = %d, want %d", wm2, now+1000)
	}
}
