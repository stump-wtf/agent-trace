package tail

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// ccAPIErrorFixtures pins what each hand-built API-error fixture must produce.
// Every mark is listed in order, so an error mark that lands at the wrong seq
// or displaces another mark fails; Timestamp is listed only for error marks,
// because Parse leaves the other Claude Code marks undated.
var ccAPIErrorFixtures = []struct {
	name   string
	file   string
	events int
	model  string
	marks  []classify.Mark
}{
	{
		// A rate limit and an auth failure, each between tool calls.
		name:   "between tool calls",
		file:   "claudecode_api_errors.jsonl",
		events: 3,
		model:  "claude-test-model",
		marks: []classify.Mark{
			{Seq: 0, Type: "user-message", Note: "run the migration"},
			{Seq: 1, Type: "error", Timestamp: "2026-03-02T10:00:03.000Z",
				Note: "rate_limit (429): You've hit your session limit · resets 3pm"},
			{Seq: 1, Type: "user-message", Note: "keep going"},
			{Seq: 2, Type: "error", Timestamp: "2026-03-02T15:01:03.000Z",
				Note: "authentication_failed (401): Failed to authenticate. API Error: 401 invalid credentials"},
			{Seq: 2, Type: "user-message", Note: "retry after login"},
		},
	},
	{
		// The outage shape: every call fails, so there is no tool event at
		// all, and two of the three failures never got an HTTP response.
		name:   "outage with no tool calls",
		file:   "claudecode_api_error_outage.jsonl",
		events: 0,
		model:  "", // every assistant record is "<synthetic>"; none is the model
		marks: []classify.Mark{
			{Seq: 0, Type: "user-message", Note: "summarize the logs"},
			{Seq: 0, Type: "error", Timestamp: "2026-03-03T09:00:31.000Z",
				Note: "server_error: API Error: Unable to connect to API (ConnectionRefused)"},
			{Seq: 0, Type: "user-message", Note: "try again"},
			{Seq: 0, Type: "error", Timestamp: "2026-03-03T09:05:12.000Z",
				Note: "server_error (529): API Error: 529 Overloaded. Try again later."},
			{Seq: 0, Type: "user-message", Note: "once more"},
			{Seq: 0, Type: "error", Timestamp: "2026-03-03T09:20:30.000Z",
				Note: "server_error: API Error: Can't reach the API. Check your network connection."},
		},
	},
}

// assertCCMarks compares marks against want in order: Seq, Type and Note
// always, Timestamp for error marks.
func assertCCMarks(t *testing.T, got, want []classify.Mark) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("marks = %d, want %d:\n got  %+v\n want %+v", len(got), len(want), got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Seq != w.Seq || g.Type != w.Type || g.Note != w.Note {
			t.Errorf("mark %d = {Seq:%d Type:%q Note:%q}, want {Seq:%d Type:%q Note:%q}",
				i, g.Seq, g.Type, g.Note, w.Seq, w.Type, w.Note)
		}
		if w.Type == "error" && g.Timestamp != w.Timestamp {
			t.Errorf("mark %d timestamp = %q, want the error record's own %q", i, g.Timestamp, w.Timestamp)
		}
	}
}

// TestClaudeCodeAPIErrorMarks checks that a failed API call surfaces as an
// "error" mark from both Parse and ParseSince: at the seq of the next tool
// event, dated by its own record, with the code and status leading the note —
// and that it neither disturbs tool pairing nor lends the session its
// "<synthetic>" model.
func TestClaudeCodeAPIErrorMarks(t *testing.T) {
	a := ClaudeCodeAdapter{}
	for _, fx := range ccAPIErrorFixtures {
		path := filepath.Join("testdata", fx.file)
		check := func(t *testing.T, events []classify.Event, marks []classify.Mark, meta SessionMeta) {
			t.Helper()
			if len(events) != fx.events {
				t.Errorf("events = %d, want %d — an error record must not become or break a tool event", len(events), fx.events)
			}
			for i, ev := range events {
				if ev.Seq != i {
					t.Errorf("event %d Seq = %d, want %d", i, ev.Seq, i)
				}
			}
			if meta.Model != fx.model {
				t.Errorf("meta.Model = %q, want %q", meta.Model, fx.model)
			}
			assertCCMarks(t, marks, fx.marks)
		}
		t.Run(fx.name+"/Parse", func(t *testing.T) {
			events, marks, meta, err := a.Parse(t.Context(), path)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			check(t, events, marks, meta)
		})
		t.Run(fx.name+"/ParseSince", func(t *testing.T) {
			events, marks, meta, wm, err := a.ParseSince(t.Context(), path, 0, 0)
			if err != nil {
				t.Fatalf("ParseSince: %v", err)
			}
			check(t, events, marks, meta)
			if want := a.Watermark(t.Context(), path); wm != want {
				t.Errorf("watermark = %d, want %d — nothing is outstanding, so it must reach the end", wm, want)
			}
		})
	}
}

// TestClaudeCodeParseSinceAPIErrorSeqContinues checks that an incremental
// poll numbers its error mark from startSeq, as it does its events.
func TestClaudeCodeParseSinceAPIErrorSeqContinues(t *testing.T) {
	path := filepath.Join("testdata", "claudecode_api_errors.jsonl")
	_, marks, _, _, err := ClaudeCodeAdapter{}.ParseSince(t.Context(), path, 0, 10)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	errs := errorMarksIn(marks)
	if len(errs) != 2 || errs[0].Seq != 11 || errs[1].Seq != 12 {
		t.Errorf("error marks = %+v, want seqs 11 and 12 (startSeq 10, after one and two events)", errs)
	}
}

// TestClaudeCodeSummarizeIgnoresSyntheticModel: the summary the session list
// shows must not name "<synthetic>" as the model of a session whose only
// assistant records are failed calls.
func TestClaudeCodeSummarizeIgnoresSyntheticModel(t *testing.T) {
	path := filepath.Join("testdata", "claudecode_api_error_outage.jsonl")
	meta, err := ClaudeCodeAdapter{}.Summarize(t.Context(), path)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if meta.Model != "" {
		t.Errorf("Summarize Model = %q, want empty — a failed call is not a model turn", meta.Model)
	}
	if want := "2026-03-03T09:20:30.000Z"; meta.EndedAt != want {
		t.Errorf("Summarize EndedAt = %q, want the last failure %q — it is still activity", meta.EndedAt, want)
	}
}

// TestClaudeCodeAPIErrorNeedsTheFlag pins detection to isApiErrorMessage on an
// assistant record. Error-looking text without the flag — an older Claude
// Code, or an agent quoting an error — stays silent, as does the synthetic
// "No response requested." record, which is flagged false.
func TestClaudeCodeAPIErrorNeedsTheFlag(t *testing.T) {
	path := writeTempJSONL(t, "noflag.jsonl", strings.Join([]string{
		`{"type":"user","timestamp":"2026-03-04T10:00:00Z","sessionId":"s1","cwd":"/tmp","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","timestamp":"2026-03-04T10:00:01Z","sessionId":"s1","cwd":"/tmp","error":"server_error","apiErrorStatus":529,"message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"API Error: 529 Overloaded"}]}}`,
		`{"type":"assistant","timestamp":"2026-03-04T10:00:02Z","sessionId":"s1","cwd":"/tmp","isApiErrorMessage":false,"message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"No response requested."}]}}`,
		`{"type":"system","timestamp":"2026-03-04T10:00:03Z","sessionId":"s1","cwd":"/tmp","isApiErrorMessage":true,"error":"server_error"}`,
	}, "\n")+"\n")
	a := ClaudeCodeAdapter{}
	_, marks, _, err := a.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if errs := errorMarksIn(marks); len(errs) != 0 {
		t.Errorf("Parse error marks = %+v, want none without the flag on an assistant record", errs)
	}
	_, marks, _, _, err = a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if errs := errorMarksIn(marks); len(errs) != 0 {
		t.Errorf("ParseSince error marks = %+v, want none without the flag on an assistant record", errs)
	}
}

// ccRetryRecord is the system record Claude Code writes for each retry of a
// failing call. Its "error" is an object, not the string code the final
// assistant record carries.
const ccRetryRecord = `{"type":"system","subtype":"api_error","level":"error","timestamp":"2026-03-04T11:00:09Z","sessionId":"s1","cwd":"/tmp","retryAttempt":1,"maxRetries":10,"retryInMs":500,"error":{"status":529,"message":"overloaded","isNetworkDown":false}}`

// TestClaudeCodeRetryRecordsStillDecode guards the field this change adds: a
// record whose "error" is an object must still decode, or every retry line
// would silently vanish — timestamp and all. It also pins that a retry is not
// itself an error mark: only the final failure is.
func TestClaudeCodeRetryRecordsStillDecode(t *testing.T) {
	path := writeTempJSONL(t, "retry.jsonl",
		`{"type":"user","timestamp":"2026-03-04T11:00:00Z","sessionId":"s1","cwd":"/tmp","message":{"role":"user","content":"go"}}`+"\n"+
			ccRetryRecord+"\n")
	const want = "2026-03-04T11:00:09Z"
	a := ClaudeCodeAdapter{}

	_, marks, meta, err := a.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if meta.EndedAt != want {
		t.Errorf("Parse EndedAt = %q, want %q — the retry record was dropped", meta.EndedAt, want)
	}
	if errs := errorMarksIn(marks); len(errs) != 0 {
		t.Errorf("Parse error marks = %+v, want none for a retry", errs)
	}

	_, marks, meta, _, err = a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if meta.EndedAt != want {
		t.Errorf("ParseSince EndedAt = %q, want %q — the retry record was dropped", meta.EndedAt, want)
	}
	if errs := errorMarksIn(marks); len(errs) != 0 {
		t.Errorf("ParseSince error marks = %+v, want none for a retry", errs)
	}

	sum, err := a.Summarize(t.Context(), path)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.EndedAt != want {
		t.Errorf("Summarize EndedAt = %q, want %q — the retry record was dropped", sum.EndedAt, want)
	}
}

// TestClaudeCodeSidechainAPIError: a subagent's failed call is reported from
// its own session, and the error record does not disturb the sidechain
// metadata that keeps that session out of the listing.
func TestClaudeCodeSidechainAPIError(t *testing.T) {
	path := writeTempJSONL(t, "agent-a1.jsonl", strings.Join([]string{
		`{"type":"user","timestamp":"2026-03-05T12:00:00Z","sessionId":"s1","agentId":"a1","isSidechain":true,"cwd":"/tmp","message":{"role":"user","content":"find the config"}}`,
		`{"type":"assistant","timestamp":"2026-03-05T12:00:04Z","sessionId":"s1","agentId":"a1","isSidechain":true,"cwd":"/tmp","error":"rate_limit","apiErrorStatus":429,"isApiErrorMessage":true,"message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"You're out of usage credits"}]}}`,
	}, "\n")+"\n")
	_, marks, meta, err := ClaudeCodeAdapter{}.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !meta.IsSidechain || !meta.Auxiliary || meta.AgentID != "a1" {
		t.Errorf("meta = {IsSidechain:%v Auxiliary:%v AgentID:%q}, want a sidechain for agent a1",
			meta.IsSidechain, meta.Auxiliary, meta.AgentID)
	}
	errs := errorMarksIn(marks)
	if len(errs) != 1 || errs[0].Note != "rate_limit (429): You're out of usage credits" {
		t.Errorf("error marks = %+v, want the subagent's rate limit", errs)
	}
}

func TestCCAPIErrorNote(t *testing.T) {
	long := strings.Repeat("x", 5000)
	cases := []struct {
		name   string
		code   string
		status int
		text   string
		want   string
	}{
		{"code, status and text", "rate_limit", 429, "You've hit your session limit", "rate_limit (429): You've hit your session limit"},
		{"no status on a connection failure", "server_error", 0, "API Error: Unable to connect to API", "server_error: API Error: Unable to connect to API"},
		{"missing code", "", 401, "Failed to authenticate", "unknown (401): Failed to authenticate"},
		{"no text", "server_error", 529, "  ", "server_error (529)"},
		{"text trimmed", "unknown", 0, "\n  API Error: Connection reset \n", "unknown: API Error: Connection reset"},
		{"code cannot break the front", " rate limit:(x) ", 429, "t", "rate_limit__x_ (429): t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ccAPIErrorNote(tc.code, tc.status, tc.text); got != tc.want {
				t.Errorf("ccAPIErrorNote(%q, %d, %q) = %q, want %q", tc.code, tc.status, tc.text, got, tc.want)
			}
		})
	}
	t.Run("truncated, front kept", func(t *testing.T) {
		got := ccAPIErrorNote("rate_limit", 429, long)
		if n := len([]rune(got)); n != 2000 {
			t.Errorf("note = %d runes, want 2000 (truncated as the Crush reader's error note is)", n)
		}
		if !strings.HasPrefix(got, "rate_limit (429): xxx") || !strings.HasSuffix(got, "…") {
			t.Errorf("truncated note lost its front or its ellipsis: %q…%q", got[:24], got[len(got)-6:])
		}
	})
}
