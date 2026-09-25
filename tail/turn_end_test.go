package tail

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
)

// turnEndMarksIn returns the marks of type "turn-end".
func turnEndMarksIn(marks []classify.Mark) []classify.Mark {
	var out []classify.Mark
	for _, m := range marks {
		if m.Type == "turn-end" {
			out = append(out, m)
		}
	}
	return out
}

// assertSameMarks compares two mark streams field by field, in order.
func assertSameMarks(t *testing.T, label string, got, want []classify.Mark) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: marks = %d, want %d:\n got  %+v\n want %+v", label, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: mark %d = %+v, want %+v", label, i, got[i], want[i])
		}
	}
}

// --- Claude Code ---

// ccAssistantLine is one line of an assistant response: Claude Code writes each
// content block of a response on a line of its own, all sharing the message
// id. stop is the message's stop_reason; "" writes null, which is what older
// Claude Code versions put on every line but the response's last.
func ccAssistantLine(msgID, stop, block, ts string) string {
	sr := "null"
	if stop != "" {
		sr = `"` + stop + `"`
	}
	var content string
	switch block {
	case "thinking":
		content = `{"type":"thinking","thinking":"weighing it","signature":"sig"}`
	case "text":
		content = `{"type":"text","text":"done"}`
	default:
		content = `{"type":"tool_use","id":"` + block + `","name":"Read","input":{"file_path":"main.go"}}`
	}
	return `{"type":"assistant","timestamp":"` + ts + `","sessionId":"s1","cwd":"/tmp","message":{"id":"` + msgID +
		`","role":"assistant","model":"claude-test-model","stop_reason":` + sr + `,"content":[` + content + `]}}` + "\n"
}

func ccUserLine(text, ts string) string {
	return `{"type":"user","timestamp":"` + ts + `","sessionId":"s1","cwd":"/tmp","message":{"role":"user","content":"` + text + `"}}` + "\n"
}

// ccStopHookLine is the system record Claude Code writes once a turn's Stop
// hooks have run — the usual record after a turn's last assistant line.
func ccStopHookLine(ts string) string {
	return `{"type":"system","subtype":"stop_hook_summary","timestamp":"` + ts + `","sessionId":"s1","cwd":"/tmp"}` + "\n"
}

// TestClaudeCodeTurnEndStopReasons pins which stop_reasons end a turn. Real
// transcripts carry end_turn and stop_sequence on text-only responses and
// tool_use on every response that issues a call; the agent loop runs the
// calls and sends another request, so tool_use is not a boundary, and neither
// is pause_turn, which the API returns to be resumed.
func TestClaudeCodeTurnEndStopReasons(t *testing.T) {
	cases := []struct {
		stop string
		want bool
	}{
		{"end_turn", true},
		{"stop_sequence", true},
		{"max_tokens", true},
		{"refusal", true},
		{"tool_use", false},
		{"pause_turn", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.stop, func(t *testing.T) {
			block := "text"
			if tc.stop == "tool_use" {
				block = "toolu_1"
			}
			path := writeTempJSONL(t, "s.jsonl", ccUserLine("go", "2026-04-01T10:00:00.000Z")+
				ccAssistantLine("msg_1", tc.stop, block, "2026-04-01T10:00:05.000Z"))
			_, marks, _, err := ClaudeCodeAdapter{}.Parse(t.Context(), path)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			ends := turnEndMarksIn(marks)
			if !tc.want {
				if len(ends) != 0 {
					t.Fatalf("stop_reason %q produced turn-end marks %+v, want none", tc.stop, ends)
				}
				return
			}
			want := classify.Mark{Seq: 0, Timestamp: "2026-04-01T10:00:05.000Z", Type: "turn-end", Note: tc.stop}
			if len(ends) != 1 || ends[0] != want {
				t.Fatalf("turn-end marks = %+v, want [%+v]", ends, want)
			}
		})
	}
}

// TestClaudeCodeTurnEndNotFromOtherRecords: a stop_reason that ends a turn
// does not make a turn-end when the record is not a model response of this
// conversation. A failed API call is its own "error" mark; "No response
// requested." is a synthetic record Claude Code writes without calling the
// model; and an inline subagent line (isSidechain with no agentId, the older
// layout that interleaves subagents with the parent) ends the subagent's
// turn, not the session's.
func TestClaudeCodeTurnEndNotFromOtherRecords(t *testing.T) {
	cases := map[string]string{
		"api error": `{"type":"assistant","timestamp":"2026-04-01T10:00:05.000Z","sessionId":"s1","isApiErrorMessage":true,"error":"rate_limit","apiErrorStatus":429,` +
			`"message":{"id":"msg_e","role":"assistant","model":"<synthetic>","stop_reason":"stop_sequence","content":[{"type":"text","text":"limit"}]}}` + "\n",
		"synthetic": `{"type":"assistant","timestamp":"2026-04-01T10:00:05.000Z","sessionId":"s1",` +
			`"message":{"id":"msg_s","role":"assistant","model":"<synthetic>","stop_reason":"stop_sequence","content":[{"type":"text","text":"No response requested."}]}}` + "\n",
		"inline sidechain": `{"type":"assistant","timestamp":"2026-04-01T10:00:05.000Z","sessionId":"s1","isSidechain":true,` +
			`"message":{"id":"msg_c","role":"assistant","model":"claude-test-model","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}` + "\n",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTempJSONL(t, "s.jsonl", ccUserLine("go", "2026-04-01T10:00:00.000Z")+line)
			_, marks, _, err := ClaudeCodeAdapter{}.Parse(t.Context(), path)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if ends := turnEndMarksIn(marks); len(ends) != 0 {
				t.Fatalf("turn-end marks = %+v, want none", ends)
			}
		})
	}
}

// TestClaudeCodeSubagentFileTurnEnd: a subagent's own transcript is all
// isSidechain lines carrying its agentId, and its turn ends are real.
func TestClaudeCodeSubagentFileTurnEnd(t *testing.T) {
	line := `{"type":"assistant","timestamp":"2026-04-01T10:00:05.000Z","sessionId":"s1","isSidechain":true,"agentId":"a1b2",` +
		`"message":{"id":"msg_1","role":"assistant","model":"claude-test-model","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}` + "\n"
	path := writeTempJSONL(t, "agent-a1b2.jsonl", line)
	_, marks, _, err := ClaudeCodeAdapter{}.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ends := turnEndMarksIn(marks); len(ends) != 1 {
		t.Fatalf("turn-end marks = %+v, want 1", ends)
	}
}

// ccTurnEndSession is a session exercising every shape a turn's last
// response takes in real transcripts:
//
//   - a tool round (stop_reason tool_use on every line) then a response whose
//     thinking and text lines BOTH carry end_turn — current Claude Code copies
//     the final stop_reason onto every line of the response;
//   - a one-line response;
//   - a response whose thinking line carries null and only its last line the
//     stop_reason — the layout of older versions and of subagent transcripts;
//   - two turn-ending lines of one response with a record between them, which
//     one real transcript in ~1,600 multi-line responses shows.
var ccTurnEndSession = []string{
	ccUserLine("fix the build", "2026-04-01T10:00:00.000Z"),
	ccAssistantLine("msg_1", "tool_use", "thinking", "2026-04-01T10:00:02.000Z"),
	ccAssistantLine("msg_1", "tool_use", "toolu_1", "2026-04-01T10:00:02.010Z"),
	ccToolResultLine("toolu_1", "2026-04-01T10:00:03.000Z"),
	ccAssistantLine("msg_2", "end_turn", "thinking", "2026-04-01T10:00:09.000Z"),
	ccAssistantLine("msg_2", "end_turn", "text", "2026-04-01T10:00:09.020Z"),
	ccStopHookLine("2026-04-01T10:00:11.000Z"),
	ccUserLine("and the tests", "2026-04-01T10:01:00.000Z"),
	ccAssistantLine("msg_3", "end_turn", "text", "2026-04-01T10:01:04.000Z"),
	ccUserLine("once more", "2026-04-01T10:02:00.000Z"),
	ccAssistantLine("msg_4", "", "thinking", "2026-04-01T10:02:03.000Z"),
	ccAssistantLine("msg_4", "max_tokens", "text", "2026-04-01T10:02:04.000Z"),
	ccUserLine("last", "2026-04-01T10:03:00.000Z"),
	ccAssistantLine("msg_5", "end_turn", "thinking", "2026-04-01T10:03:05.000Z"),
	ccStopHookLine("2026-04-01T10:03:05.500Z"),
	ccAssistantLine("msg_5", "end_turn", "text", "2026-04-01T10:03:06.000Z"),
}

func ccToolResultLine(id, ts string) string {
	return `{"type":"user","timestamp":"` + ts + `","sessionId":"s1","cwd":"/tmp","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"package main"}]}}` + "\n"
}

// ccTurnEndSessionWant is the turn-end marks ccTurnEndSession must produce:
// one per response, at the first line of the response that carries its
// turn-ending stop_reason, dated by that line — except the last response,
// whose lines are split by another record and so mark twice. That duplicate
// is the documented cost of deciding from the record just before a line.
var ccTurnEndSessionWant = []classify.Mark{
	{Seq: 1, Timestamp: "2026-04-01T10:00:09.000Z", Type: "turn-end", Note: "end_turn"},
	{Seq: 1, Timestamp: "2026-04-01T10:01:04.000Z", Type: "turn-end", Note: "end_turn"},
	{Seq: 1, Timestamp: "2026-04-01T10:02:04.000Z", Type: "turn-end", Note: "max_tokens"},
	{Seq: 1, Timestamp: "2026-04-01T10:03:05.000Z", Type: "turn-end", Note: "end_turn"},
	{Seq: 1, Timestamp: "2026-04-01T10:03:06.000Z", Type: "turn-end", Note: "end_turn"},
}

// TestClaudeCodeTurnEndOncePerResponse checks Parse: a response written as
// several lines that all carry end_turn is one turn end, not one per line.
func TestClaudeCodeTurnEndOncePerResponse(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", strings.Join(ccTurnEndSession, ""))
	events, marks, _, err := ClaudeCodeAdapter{}.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	assertSameMarks(t, "Parse", turnEndMarksIn(marks), ccTurnEndSessionWant)
}

// TestClaudeCodeParseSinceTurnEndExactlyOnce is the incremental contract: at
// every place a poll can stop — including between two lines of one response,
// where the second poll cannot see the first line — the marks ParseSince
// delivers across polls are exactly Parse's, each turn end once.
func TestClaudeCodeParseSinceTurnEndExactlyOnce(t *testing.T) {
	a := ClaudeCodeAdapter{}
	full := writeTempJSONL(t, "full.jsonl", strings.Join(ccTurnEndSession, ""))
	wantEvents, wantMarks, _, err := a.Parse(t.Context(), full)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(turnEndMarksIn(wantMarks)) != len(ccTurnEndSessionWant) {
		t.Fatalf("Parse gave %d turn-end marks, want %d; the comparison would prove nothing",
			len(turnEndMarksIn(wantMarks)), len(ccTurnEndSessionWant))
	}

	// One poll per appended line: every record boundary is a poll boundary.
	path := writeTempJSONL(t, "s.jsonl", ccTurnEndSession[0])
	var events []classify.Event
	var marks []classify.Mark
	var wm int64
	for i := range ccTurnEndSession {
		if i > 0 {
			appendLines(t, path, ccTurnEndSession[i])
		}
		ev, mk, _, next, err := a.ParseSince(t.Context(), path, wm, len(events))
		if err != nil {
			t.Fatalf("poll %d: ParseSince: %v", i, err)
		}
		events, marks, wm = append(events, ev...), append(marks, mk...), next
	}
	if len(events) != len(wantEvents) {
		t.Fatalf("events: incremental gave %d, Parse gave %d", len(events), len(wantEvents))
	}
	// Parse leaves user-message marks undated where ParseSince dates them,
	// so compare turn-end marks, which both date.
	assertSameMarks(t, "line-by-line", turnEndMarksIn(marks), turnEndMarksIn(wantMarks))
}

// --- Codex ---

func codexEventLine(payload, ts string) string {
	return `{"type":"event_msg","timestamp":"` + ts + `","payload":` + payload + `}` + "\n"
}

// codexTurnEndSession follows a real rollout's turn: task_started, the user's
// message, a call and its output, then task_complete. Current Codex writes the
// event as "task_complete" with a turn_id; "turn_complete" is the alias its
// deserializer also accepts.
var codexTurnEndSession = []string{
	codexSessionMetaLine("2026-04-01T10:00:00Z"),
	codexEventLine(`{"type":"task_started","turn_id":"t1"}`, "2026-04-01T10:00:01Z"),
	codexUserLine("fix the build", "2026-04-01T10:00:01Z"),
	codexCallLine("call-1", "shell", `"{\"command\":[\"go\",\"build\"]}"`, "2026-04-01T10:00:02Z"),
	codexOutputLine("call-1", `"ok"`, "2026-04-01T10:00:04Z"),
	codexEventLine(`{"type":"task_complete","turn_id":"t1","last_agent_message":"built"}`, "2026-04-01T10:00:06Z"),
	codexEventLine(`{"type":"task_started","turn_id":"t2"}`, "2026-04-01T10:01:00Z"),
	codexUserLine("and test", "2026-04-01T10:01:00Z"),
	codexEventLine(`{"type":"turn_complete","turn_id":"t2","last_agent_message":null}`, "2026-04-01T10:01:07Z"),
}

var codexTurnEndSessionWant = []classify.Mark{
	{Seq: 1, Timestamp: "2026-04-01T10:00:06Z", Type: "turn-end"},
	{Seq: 1, Timestamp: "2026-04-01T10:01:07Z", Type: "turn-end"},
}

func TestCodexTurnEndMarks(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", strings.Join(codexTurnEndSession, ""))
	_, marks, _, err := CodexAdapter{}.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assertSameMarks(t, "Parse", turnEndMarksIn(marks), codexTurnEndSessionWant)
}

func TestCodexParseSinceTurnEndExactlyOnce(t *testing.T) {
	a := CodexAdapter{}
	path := writeTempJSONL(t, "s.jsonl", codexTurnEndSession[0])
	var events []classify.Event
	var marks []classify.Mark
	var wm int64
	for i := range codexTurnEndSession {
		if i > 0 {
			appendLines(t, path, codexTurnEndSession[i])
		}
		ev, mk, _, next, err := a.ParseSince(t.Context(), path, wm, len(events))
		if err != nil {
			t.Fatalf("poll %d: ParseSince: %v", i, err)
		}
		events, marks, wm = append(events, ev...), append(marks, mk...), next
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	assertSameMarks(t, "line-by-line", turnEndMarksIn(marks), codexTurnEndSessionWant)
}

// --- Crush ---

// TestCrushTurnEndMarks: an assistant row's finish part is a turn end when
// its reason is one the agent stops on — end_turn, max_tokens, or a provider
// refusal (content_filter). tool_use is a step inside the turn; error keeps
// its own "error" mark; canceled and unknown are not a completed turn; and
// the finish parts Crush writes on tool and user rows (reason "stop") are not
// the model's.
func TestCrushTurnEndMarks(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	now := time.Now().Unix()
	dbPath := newCrushSession(t, "turns", now)
	insertCrushMessages(t, dbPath, "turns", now,
		[]string{"user", "assistant", "tool", "assistant", "user", "assistant", "assistant", "assistant", "assistant", "assistant"},
		[]string{
			`[{"type":"text","data":{"text":"fix the build"}},{"type":"finish","data":{"reason":"stop","time":1789127700}}]`,
			`[{"type":"tool_call","data":{"id":"call-1","name":"view","input":"{\"file_path\":\"a.go\"}","finished":true}},{"type":"finish","data":{"reason":"tool_use","time":1789127701}}]`,
			`[{"type":"tool_result","data":{"tool_call_id":"call-1","name":"view","content":"package a"}},{"type":"finish","data":{"reason":"stop","time":1789127702}}]`,
			`[{"type":"text","data":{"text":"fixed"}},{"type":"finish","data":{"reason":"end_turn","time":1789127710}}]`,
			`[{"type":"text","data":{"text":"again"}}]`,
			`[{"type":"text","data":{"text":"long"}},{"type":"finish","data":{"reason":"max_tokens","time":1789127720}}]`,
			`[{"type":"finish","data":{"reason":"content_filter","time":1789127730}}]`,
			`[{"type":"finish","data":{"reason":"canceled","time":1789127740}}]`,
			`[{"type":"finish","data":{"reason":"unknown","time":1789127750}}]`,
			finishErrorParts,
		})

	_, marks, _, err := CrushAdapter{DBPath: dbPath, Cwd: "/test"}.Parse(t.Context(), dbPath+"/turns")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assertSameMarks(t, "Parse", turnEndMarksIn(marks), []classify.Mark{
		{Seq: 1, Timestamp: secToRFC3339(1789127710), Type: "turn-end", Note: "end_turn"},
		{Seq: 1, Timestamp: secToRFC3339(1789127720), Type: "turn-end", Note: "max_tokens"},
		{Seq: 1, Timestamp: secToRFC3339(1789127730), Type: "turn-end", Note: "content_filter"},
	})
	if errs := errorMarksIn(marks); len(errs) != 1 {
		t.Errorf("error marks = %+v, want exactly the one finish-error", errs)
	}
}

// TestCrushParseSinceTurnEndOnce: Crush inserts the assistant row when the
// turn starts and writes its finish part into that same row when it ends. The
// poll that reads the row still streaming emits nothing for it; the poll after
// the finish lands emits the turn end; the next emits nothing again.
func TestCrushParseSinceTurnEndOnce(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	now := time.Now().Unix()
	dbPath := newCrushSession(t, "live", now)
	insertCrushMessages(t, dbPath, "live", now, []string{"user", "assistant"}, []string{
		`[{"type":"text","data":{"text":"fix the build"}}]`,
		`[{"type":"text","data":{"text":"working"}}]`,
	})
	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	path := dbPath + "/live"

	_, marks, _, wm, err := a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	if ends := turnEndMarksIn(marks); len(ends) != 0 {
		t.Fatalf("poll 1: turn-end before the finish was written: %+v", ends)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE messages SET parts = ? WHERE id = ?`,
		`[{"type":"text","data":{"text":"fixed"}},{"type":"finish","data":{"reason":"end_turn","time":1789127710}}]`,
		fmt.Sprintf("live-%d-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	resetDBCache()

	_, marks2, _, wm2, err := a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	assertSameMarks(t, "poll 2", turnEndMarksIn(marks2), []classify.Mark{
		{Seq: 0, Timestamp: secToRFC3339(1789127710), Type: "turn-end", Note: "end_turn"},
	})

	_, marks3, _, _, err := a.ParseSince(t.Context(), path, wm2, 0)
	if err != nil {
		t.Fatalf("poll 3: %v", err)
	}
	if ends := turnEndMarksIn(marks3); len(ends) != 0 {
		t.Errorf("poll 3 re-emitted the turn end: %+v", ends)
	}
}

// TestCodexTurnAbortedIsNotTurnEnd: an interrupted turn is recorded as
// turn_aborted, not task_complete, and is not a turn the agent finished.
func TestCodexTurnAbortedIsNotTurnEnd(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", strings.Join([]string{
		codexSessionMetaLine("2026-04-01T10:00:00Z"),
		codexEventLine(`{"type":"task_started","turn_id":"t1"}`, "2026-04-01T10:00:01Z"),
		codexUserLine("fix the build", "2026-04-01T10:00:01Z"),
		codexEventLine(`{"type":"turn_aborted","turn_id":"t1","reason":"interrupted"}`, "2026-04-01T10:00:03Z"),
	}, ""))
	_, marks, _, err := CodexAdapter{}.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ends := turnEndMarksIn(marks); len(ends) != 0 {
		t.Fatalf("turn-end marks = %+v, want none for an aborted turn", ends)
	}
}

// TestCrushTurnEndOnlyOnAssistantRows pins the role guard: a turn-ending
// reason on a user or tool row's finish part is not the model's, so it is no
// turn end. No real store has one — Crush writes "stop" there — which is why
// only this test exercises the guard.
func TestCrushTurnEndOnlyOnAssistantRows(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	now := time.Now().Unix()
	dbPath := newCrushSession(t, "roles", now)
	insertCrushMessages(t, dbPath, "roles", now, []string{"user", "assistant", "tool"}, []string{
		`[{"type":"text","data":{"text":"go"}},` + crushFinish("end_turn") + `]`,
		`[` + crushCallPart("c1", "true") + `,` + crushFinish("tool_use") + `]`,
		`[{"type":"tool_result","data":{"tool_call_id":"c1","name":"bash","content":"ok"}},` + crushFinish("end_turn") + `]`,
	})
	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	path := dbPath + "/roles"

	_, marks, _, err := a.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ends := turnEndMarksIn(marks); len(ends) != 0 {
		t.Errorf("Parse: turn-end marks = %+v, want none from user and tool rows", ends)
	}
	_, marks, _, _, err = a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if ends := turnEndMarksIn(marks); len(ends) != 0 {
		t.Errorf("ParseSince: turn-end marks = %+v, want none from user and tool rows", ends)
	}
}

// TestCrushParseTurnEndAfterAbandonedCall is the full-parse half of
// TestCrushParseSinceReleasesCallItsStepAbandoned: a step cut short at
// max_tokens never runs the call it streamed, and the turn-end lands after
// that call is released, ahead of the next turn's user message.
func TestCrushParseTurnEndAfterAbandonedCall(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	now := time.Now().Unix()
	dbPath := newCrushSession(t, "cut", now)
	insertCrushMessages(t, dbPath, "cut", now, []string{"user", "assistant", "user"}, []string{
		crushUserRow("start"),
		`[` + crushCallPart("c1", "echo orphan") + `,` + crushFinish("max_tokens") + `]`,
		crushUserRow("resumed"),
	})
	events, marks, _, err := CrushAdapter{DBPath: dbPath, Cwd: "/test"}.Parse(t.Context(), dbPath+"/cut")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events) != 1 || events[0].Seq != 0 {
		t.Fatalf("events = %+v, want the abandoned c1 at seq 0", events)
	}
	if len(marks) != 3 || marks[1].Type != "turn-end" || marks[1].Seq != 1 || marks[1].Note != "max_tokens" ||
		marks[2].Note != "resumed" || marks[2].Seq != 1 {
		t.Errorf("marks = %+v, want start at 0, then the turn end and the resumed message at seq 1", marks)
	}
}
