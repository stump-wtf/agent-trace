package tail

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
)

// streamCollector gathers what ParseStream hands a StreamHandler.
type streamCollector struct {
	metas  []SessionMeta
	events []classify.Event
	marks  []classify.Mark
}

func (c *streamCollector) handler() StreamHandler {
	return StreamHandler{
		Meta:  func(m SessionMeta) { c.metas = append(c.metas, m) },
		Event: func(e classify.Event) { c.events = append(c.events, e) },
		Mark:  func(m classify.Mark) { c.marks = append(c.marks, m) },
	}
}

func parseStreamFile(t *testing.T, path string) (*streamCollector, SessionMeta, *StreamResult) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c streamCollector
	meta, res, err := ClaudeCodeAdapter{}.ParseStream(t.Context(), bytes.NewReader(data), c.handler())
	if err != nil {
		t.Fatalf("ParseStream(%s): %v", path, err)
	}
	return &c, meta, res
}

// TestClaudeCodeStreamMatchesTranscript is the equivalence the stream reader
// exists to keep: a run's stream-json and the transcript the same run wrote
// yield the same classified events and marks. The one allowed difference is
// the prompt, which -p does not echo onto stdout, so the transcript alone
// carries its user-message mark.
//
// The basic pair is synthesized in the shape Claude Code 2.1.280 writes; the
// api_error pair is a sanitized real recording of a run whose login had
// expired.
func TestClaudeCodeStreamMatchesTranscript(t *testing.T) {
	for _, name := range []string{"claudecode_stream_basic", "claudecode_stream_api_error"} {
		t.Run(name, func(t *testing.T) {
			wantEvents, wantMarks, wantMeta, err := ClaudeCodeAdapter{}.Parse(t.Context(), filepath.Join("testdata", name+".transcript.jsonl"))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(wantEvents)+len(wantMarks) < 2 {
				t.Fatalf("transcript yields %d events and %d marks; the comparison would be vacuous", len(wantEvents), len(wantMarks))
			}
			if len(wantMarks) == 0 || wantMarks[0].Type != "user-message" {
				t.Fatalf("transcript's first mark = %+v, want the prompt's user-message", wantMarks)
			}
			wantMarks = wantMarks[1:]

			c, meta, _ := parseStreamFile(t, filepath.Join("testdata", name+".stream.jsonl"))
			if !reflect.DeepEqual(c.events, wantEvents) {
				t.Errorf("events differ\nstream:     %+v\ntranscript: %+v", c.events, wantEvents)
			}
			if len(c.marks) != len(wantMarks) || (len(wantMarks) > 0 && !reflect.DeepEqual(c.marks, wantMarks)) {
				t.Errorf("marks differ\nstream:     %+v\ntranscript: %+v", c.marks, wantMarks)
			}
			if meta.ID != wantMeta.ID || meta.Cwd != wantMeta.Cwd || meta.Model != wantMeta.Model || meta.Harness != HarnessClaudeCode {
				t.Errorf("meta = %+v, want ID %q, Cwd %q, Model %q, harness %q", meta, wantMeta.ID, wantMeta.Cwd, wantMeta.Model, HarnessClaudeCode)
			}
		})
	}
}

func TestClaudeCodeStreamBasicEvents(t *testing.T) {
	c, meta, _ := parseStreamFile(t, filepath.Join("testdata", "claudecode_stream_basic.stream.jsonl"))
	if len(c.events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(c.events), c.events)
	}
	if c.events[0].Tool != "Read" || c.events[0].Action != classify.ActionRead || c.events[0].ResultBytes == 0 {
		t.Errorf("event 0 = %+v, want an answered Read", c.events[0])
	}
	if c.events[1].Tool != "Bash" || c.events[1].Action != classify.ActionSearch || c.events[1].ResultBytes == 0 {
		t.Errorf("event 1 = %+v, want an answered Bash ls", c.events[1])
	}
	if meta.Model != "claude-sonnet-4-5-20250929" {
		t.Errorf("meta.Model = %q", meta.Model)
	}
	if meta.StartedAt != "2026-09-25T06:30:02.000Z" || meta.EndedAt != "2026-09-25T06:30:06.000Z" {
		t.Errorf("meta span = %q..%q", meta.StartedAt, meta.EndedAt)
	}
	// Meta is announced once, as soon as the init record identifies the
	// session, so a consumer knows the cwd before the first event.
	if len(c.metas) != 1 || c.metas[0].ID != meta.ID || c.metas[0].Cwd != "/home/user/project" {
		t.Errorf("Meta calls = %+v, want one carrying the session's ID and cwd", c.metas)
	}
}

func TestClaudeCodeStreamResult(t *testing.T) {
	tests := []struct {
		name string
		want StreamResult
	}{
		{"claudecode_stream_basic", StreamResult{
			Subtype:       "success",
			IsError:       false,
			Turns:         3,
			DurationMS:    6120,
			APIDurationMS: 5480,
			CostUSD:       0.012345,
			StopReason:    "end_turn",
			Text:          "note.txt says hello fixture, and it is the only file here.",
			Usage:         TokenUsage{InputTokens: 62, OutputTokens: 90, CacheCreationInputTokens: 1500, CacheReadInputTokens: 9000},
		}},
		// The real recording: the login had expired, and Claude Code reports
		// that as subtype "success" with is_error set. Both are kept as sent.
		{"claudecode_stream_api_error", StreamResult{
			Subtype:    "success",
			IsError:    true,
			Turns:      1,
			DurationMS: 62,
			StopReason: "stop_sequence",
			Text:       "Failed to authenticate: OAuth session expired and could not be refreshed",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, res := parseStreamFile(t, filepath.Join("testdata", tt.name+".stream.jsonl"))
			if res == nil {
				t.Fatal("no Result from a stream that ends in a result record")
			}
			if !reflect.DeepEqual(*res, tt.want) {
				t.Errorf("Result = %+v\nwant     %+v", *res, tt.want)
			}
		})
	}
}

// TestClaudeCodeStreamErrorResult covers the error subtypes, which carry an
// errors list and no result text. An errors value of a shape this reader does
// not expect costs the list, not the rest of the Result.
func TestClaudeCodeStreamErrorResult(t *testing.T) {
	const init = `{"type":"system","subtype":"init","cwd":"/home/user/project","session_id":"s1","model":"m"}` + "\n"
	tests := []struct {
		name, errors string
		want         []string
	}{
		{"list", `["max turns reached","second"]`, []string{"max turns reached", "second"}},
		{"unexpected shape", `{"message":"max turns reached"}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := init + `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":5,"duration_ms":900,"duration_api_ms":800,"total_cost_usd":0.5,"stop_reason":null,"session_id":"s1","usage":{"input_tokens":7,"output_tokens":3},"errors":` + tt.errors + `}` + "\n"
			_, res, err := (ClaudeCodeAdapter{}).ParseStream(t.Context(), strings.NewReader(stream), StreamHandler{})
			if err != nil || res == nil {
				t.Fatalf("ParseStream = %v, %v", res, err)
			}
			want := StreamResult{
				Subtype:       "error_max_turns",
				IsError:       true,
				Turns:         5,
				DurationMS:    900,
				APIDurationMS: 800,
				CostUSD:       0.5,
				Usage:         TokenUsage{InputTokens: 7, OutputTokens: 3},
				Errors:        tt.want,
			}
			if !reflect.DeepEqual(*res, want) {
				t.Errorf("Result = %+v\nwant     %+v", *res, want)
			}
		})
	}
}

// TestClaudeCodeStreamTruncated covers a process killed mid-run: every event
// its stream got as far as is delivered, a half-written last line is ignored,
// and there is no Result — the run never reported one.
func TestClaudeCodeStreamTruncated(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "claudecode_stream_basic.stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	// init, Read call, Read result, then half of the next record.
	cut := strings.Join(lines[:3], "") + lines[3][:len(lines[3])/2]

	var c streamCollector
	meta, res, err := ClaudeCodeAdapter{}.ParseStream(t.Context(), strings.NewReader(cut), c.handler())
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if res != nil {
		t.Errorf("Result = %+v, want nil for a stream with no result record", *res)
	}
	if len(c.events) != 1 || c.events[0].Tool != "Read" || c.events[0].ResultBytes == 0 {
		t.Errorf("events = %+v, want the one answered Read", c.events)
	}
	if meta.ID == "" {
		t.Error("meta.ID is empty; the init record was read")
	}

	// Killed mid-call: the open call is still reported, as Parse reports a
	// transcript's, with an empty result.
	var c2 streamCollector
	_, res, err = ClaudeCodeAdapter{}.ParseStream(t.Context(), strings.NewReader(strings.Join(lines[:5], "")), c2.handler())
	if err != nil || res != nil {
		t.Fatalf("ParseStream = %v, %v; want no result and no error", res, err)
	}
	if len(c2.events) != 2 || c2.events[1].Tool != "Bash" || c2.events[1].ResultBytes != 0 {
		t.Errorf("events = %+v, want the Read then the unanswered Bash", c2.events)
	}
}

// TestClaudeCodeStreamEmitsBeforeEOF checks the property that makes this a
// stream reader: an event reaches the handler while the writer is still
// running, not once the pipe closes.
func TestClaudeCodeStreamEmitsBeforeEOF(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "claudecode_stream_basic.stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	pr, pw := io.Pipe()
	got := make(chan classify.Event, 8)
	done := make(chan error, 1)
	go func() {
		_, _, err := ClaudeCodeAdapter{}.ParseStream(t.Context(), pr, StreamHandler{
			Event: func(e classify.Event) { got <- e },
		})
		done <- err
	}()
	for _, l := range lines[:3] { // init, Read call, Read result
		if _, err := io.WriteString(pw, l); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case e := <-got:
		if e.Tool != "Read" {
			t.Errorf("first event = %+v, want the Read", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event while the stream is open; ParseStream is waiting for EOF")
	}
	_ = pw.Close()
	if err := <-done; err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
}

// TestClaudeCodeStreamSubagentKeepsParentCall: a subagent's records arrive on
// the parent's stream, tagged with the Task call that launched them. They are
// a separate conversation, so a subagent response must not settle the
// parent's open Task call — which would report it unanswered and then drop
// its real result.
func TestClaudeCodeStreamSubagentKeepsParentCall(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","cwd":"/home/user/project","session_id":"s1","model":"m"}`,
		`{"type":"assistant","message":{"id":"msg_P1","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_task","name":"Task","input":{"description":"look","prompt":"read a.go"}}]},"parent_tool_use_id":null,"session_id":"s1","timestamp":"2026-09-25T06:00:01Z"}`,
		`{"type":"assistant","message":{"id":"msg_S1","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_sub","name":"Read","input":{"file_path":"/home/user/project/a.go"}}]},"parent_tool_use_id":"toolu_task","session_id":"s1","timestamp":"2026-09-25T06:00:02Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_sub","content":"package a"}]},"parent_tool_use_id":"toolu_task","session_id":"s1","timestamp":"2026-09-25T06:00:03Z"}`,
		`{"type":"assistant","message":{"id":"msg_S2","role":"assistant","model":"m","content":[{"type":"text","text":"a.go is package a"}]},"parent_tool_use_id":"toolu_task","session_id":"s1","timestamp":"2026-09-25T06:00:04Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_task","content":"a.go is package a"}]},"parent_tool_use_id":null,"session_id":"s1","timestamp":"2026-09-25T06:00:05Z"}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":2,"session_id":"s1"}`,
	}, "\n") + "\n"
	var c streamCollector
	meta, res, err := ClaudeCodeAdapter{}.ParseStream(t.Context(), strings.NewReader(stream), c.handler())
	if err != nil || res == nil {
		t.Fatalf("ParseStream = %v, %v", res, err)
	}
	if len(c.events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(c.events), c.events)
	}
	if c.events[0].Tool != "Read" || c.events[1].Tool != "Task" {
		t.Errorf("events = %s, %s; want Read then Task", c.events[0].Tool, c.events[1].Tool)
	}
	if c.events[1].ResultBytes == 0 {
		t.Errorf("Task event = %+v, want its real result, not an empty one", c.events[1])
	}
	// The stream is the parent's, whatever its subagents wrote on it.
	if meta.IsSidechain || meta.Auxiliary || meta.AgentID != "" {
		t.Errorf("meta = %+v, want the parent session's", meta)
	}
}

// TestClaudeCodeStreamSubagentReleasesOwnDeadCall: a subagent is a
// conversation of its own, so a later response in it proves its own
// unanswered call dead, exactly as a parent response does for the parent's.
// That needs parent_tool_use_id read as the subagent's agentId: a sidechain
// line with no agentId releases nothing, and the dead call would sit pending
// until EOF, reported after the Task that launched it and out of seq order.
// A sibling subagent's response must still not release it.
func TestClaudeCodeStreamSubagentReleasesOwnDeadCall(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","cwd":"/home/user/project","session_id":"s1","model":"m"}`,
		`{"type":"assistant","message":{"id":"msg_P1","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_taskA","name":"Task","input":{"description":"a","prompt":"a"}}]},"parent_tool_use_id":null,"session_id":"s1","timestamp":"2026-09-25T06:00:01Z"}`,
		`{"type":"assistant","message":{"id":"msg_P1","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_taskB","name":"Task","input":{"description":"b","prompt":"b"}}]},"parent_tool_use_id":null,"session_id":"s1","timestamp":"2026-09-25T06:00:01.5Z"}`,
		`{"type":"assistant","message":{"id":"msg_A1","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_dead","name":"Bash","input":{"command":"sleep 999"}}]},"parent_tool_use_id":"toolu_taskA","session_id":"s1","timestamp":"2026-09-25T06:00:02Z"}`,
		// Subagent B's response: a different conversation, so it proves
		// nothing about A's call.
		`{"type":"assistant","message":{"id":"msg_B1","role":"assistant","model":"m","content":[{"type":"text","text":"b done"}]},"parent_tool_use_id":"toolu_taskB","session_id":"s1","timestamp":"2026-09-25T06:00:03Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_taskB","content":"b done"}]},"parent_tool_use_id":null,"session_id":"s1","timestamp":"2026-09-25T06:00:04Z"}`,
		// Subagent A's next response went out without toolu_dead's result.
		`{"type":"assistant","message":{"id":"msg_A2","role":"assistant","model":"m","content":[{"type":"text","text":"a gave up"}]},"parent_tool_use_id":"toolu_taskA","session_id":"s1","timestamp":"2026-09-25T06:00:05Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_taskA","content":"a gave up"}]},"parent_tool_use_id":null,"session_id":"s1","timestamp":"2026-09-25T06:00:06Z"}`,
	}, "\n") + "\n"
	var c streamCollector
	if _, _, err := (ClaudeCodeAdapter{}).ParseStream(t.Context(), strings.NewReader(stream), c.handler()); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	var got []string
	for i, e := range c.events {
		got = append(got, e.Tool+"@"+e.Timestamp)
		if e.Seq != i {
			t.Errorf("event %d (%s) has seq %d", i, e.Tool, e.Seq)
		}
	}
	want := []string{"Task@2026-09-25T06:00:01.5Z", "Bash@2026-09-25T06:00:02Z", "Task@2026-09-25T06:00:01Z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if c.events[1].ResultBytes != 0 {
		t.Errorf("dead Bash call = %+v, want an empty result", c.events[1])
	}
}

// TestClaudeCodeStreamMetaUserKeepsOpenCall: Claude Code writes a loaded
// skill's body as a user record between the results of one batch of parallel
// calls. On a transcript that line is isMeta; on stream-json the same flag is
// is_meta, and some records carry isSynthetic instead. Read as a typed message,
// it would settle the batch's still-open call — reporting it unanswered and
// then dropping its real result.
func TestClaudeCodeStreamMetaUserKeepsOpenCall(t *testing.T) {
	for _, flag := range []string{`"is_meta":true`, `"isSynthetic":true`} {
		t.Run(flag, func(t *testing.T) {
			stream := strings.Join([]string{
				`{"type":"system","subtype":"init","cwd":"/home/user/project","session_id":"s1","model":"m"}`,
				`{"type":"assistant","message":{"id":"msg_A","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/home/user/project/a.go"}}]},"parent_tool_use_id":null,"session_id":"s1","timestamp":"2026-09-25T06:00:01Z"}`,
				`{"type":"assistant","message":{"id":"msg_A","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_2","name":"Skill","input":{"skill":"review"}}]},"parent_tool_use_id":null,"session_id":"s1","timestamp":"2026-09-25T06:00:01Z"}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"package a"}]},"parent_tool_use_id":null,"session_id":"s1","timestamp":"2026-09-25T06:00:02Z"}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /skills/review. Read the diff first."}]},"parent_tool_use_id":null,` + flag + `,"session_id":"s1","timestamp":"2026-09-25T06:00:02Z"}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"Launching skill: review"}]},"parent_tool_use_id":null,"session_id":"s1","timestamp":"2026-09-25T06:00:03Z"}`,
			}, "\n") + "\n"
			var c streamCollector
			if _, _, err := (ClaudeCodeAdapter{}).ParseStream(t.Context(), strings.NewReader(stream), c.handler()); err != nil {
				t.Fatalf("ParseStream: %v", err)
			}
			if len(c.events) != 2 {
				t.Fatalf("got %d events, want 2: %+v", len(c.events), c.events)
			}
			for _, e := range c.events {
				if e.ResultBytes == 0 {
					t.Errorf("%s event = %+v, want its real result, not an empty one", e.Tool, e)
				}
			}
		})
	}
}

func TestClaudeCodeStreamRejectsOtherInput(t *testing.T) {
	_, _, err := ClaudeCodeAdapter{}.ParseStream(t.Context(), strings.NewReader("{\"foo\":1}\nnot json\n"), StreamHandler{})
	if err == nil {
		t.Error("ParseStream accepted a stream with no Claude Code record")
	}
	// An empty stream — a process killed before it wrote anything — is not
	// an error: there is simply nothing to report.
	meta, res, err := ClaudeCodeAdapter{}.ParseStream(t.Context(), strings.NewReader(""), StreamHandler{})
	if err != nil || res != nil || meta.ID != "" {
		t.Errorf("empty stream = %+v, %v, %v; want nothing", meta, res, err)
	}
}

func TestClaudeCodeStreamHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	data, err := os.ReadFile(filepath.Join("testdata", "claudecode_stream_basic.stream.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var c streamCollector
	_, _, err = ClaudeCodeAdapter{}.ParseStream(ctx, bytes.NewReader(data), c.handler())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(c.events) != 0 {
		t.Errorf("got %d events after cancellation", len(c.events))
	}
}

// TestStreamFormat is how a caller asks an adapter whether it has a
// structured output mode and how to turn it on.
func TestStreamFormat(t *testing.T) {
	var a Adapter = &ClaudeCodeAdapter{}
	sp, ok := a.(StreamParser)
	if !ok {
		t.Fatal("ClaudeCodeAdapter does not implement StreamParser")
	}
	f := sp.StreamFormat()
	if f.Name != "stream-json" {
		t.Errorf("Name = %q", f.Name)
	}
	// claude -p refuses stream-json without --verbose and exits before it
	// reads the prompt, so the flag set is not complete without it.
	want := []string{"--output-format", "stream-json", "--verbose"}
	if !reflect.DeepEqual(f.Args, want) {
		t.Errorf("Args = %q, want %q", f.Args, want)
	}
	if _, ok := Adapter(&CodexAdapter{}).(StreamParser); ok {
		t.Error("CodexAdapter claims a stream format nobody has confirmed yet")
	}
}
