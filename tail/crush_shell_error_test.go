package tail

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// TestCrushShellFailed pins the text rule for a Crush shell result. Crush
// reports a non-zero exit, or an interrupted command, as an ordinary text
// response: formatOutput in its bash tool appends "Exit code N" or "Command
// was aborted before completion" as the last line, bash then appends a
// "<cwd>…</cwd>" block, and job_output appends "Exit code N" last with no
// block. is_error stays false for all of them.
func TestCrushShellFailed(t *testing.T) {
	cases := []struct {
		name, tool, content string
		want                bool
	}{
		{"bash non-zero exit", "bash", "FAIL\tpkg\n\nExit code 1\n\n<cwd>/repo</cwd>", true},
		{"bash exit code only", "bash", "\nExit code 2\n\n<cwd>/repo</cwd>", true},
		{"bash high exit code", "bash", "fatal: not a git repository\nExit code 128\n\n<cwd>/repo</cwd>", true},
		{"bash aborted", "bash", "partial output\n\nCommand was aborted before completion\n\n<cwd>/repo</cwd>", true},
		{"bash non-zero exit without cwd", "bash", "boom\nExit code 1", true},
		{"job_output non-zero exit", "job_output", "Status: completed\n\nbuild failed\nExit code 2", true},
		{"bash success", "bash", "ok\tpkg\t0.1s\n\n<cwd>/repo</cwd>", false},
		{"bash no output", "bash", "no output", false},
		{"bash exit code 0 is not a failure", "bash", "\nExit code 0\n\n<cwd>/repo</cwd>", false},
		{"bash exit line mid-output", "bash", "log: Exit code 1\nmore output\n\n<cwd>/repo</cwd>", false},
		{"bash exit words inside a line", "bash", "saw Exit code 1\n\n<cwd>/repo</cwd>", false},
		{"job_output running", "job_output", "Status: running\n\npartial", false},
		{"job_output success", "job_output", "Status: completed\n\ndone", false},
		{"view of a file ending in an exit line", "view", "notes\nExit code 1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := crushShellFailed(tc.tool, tc.content); got != tc.want {
				t.Errorf("crushShellFailed(%q, %q) = %v, want %v", tc.tool, tc.content, got, tc.want)
			}
		})
	}
}

// TestCrushShellResultIsError checks a failed shell command reaches the event
// as IsError from both Parse and ParseSince, while a successful one and a
// non-shell result whose text happens to end in an exit line do not (#115).
func TestCrushShellResultIsError(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	type result struct{ tool, input, content string }
	results := []result{
		{"bash", `{"command":"go test ./..."}`, "FAIL\tpkg\n\nExit code 1\n\n<cwd>/repo</cwd>"},
		{"bash", `{"command":"go build ./..."}`, "\n\n<cwd>/repo</cwd>"},
		{"bash", `{"command":"sleep 100"}`, "\nCommand was aborted before completion\n\n<cwd>/repo</cwd>"},
		{"job_output", `{"shell_id":"001"}`, "Status: completed\n\nbuild failed\nExit code 2"},
		{"view", `{"file_path":"notes.txt"}`, "notes\nExit code 1"},
	}
	want := []bool{true, false, true, true, false}

	var roles, parts []string
	for i, r := range results {
		id := string(rune('a' + i))
		input, _ := json.Marshal(r.input)
		content, _ := json.Marshal(r.content)
		roles = append(roles, "assistant", "tool")
		parts = append(parts,
			`[{"type":"tool_call","data":{"id":"`+id+`","name":"`+r.tool+`","input":`+string(input)+`,"finished":true}},{"type":"finish","data":{"reason":"tool_use"}}]`,
			`[{"type":"tool_result","data":{"tool_call_id":"`+id+`","name":"`+r.tool+`","content":`+string(content)+`,"is_error":false}}]`,
		)
	}

	dbPath := newCrushSession(t, "s1", 1789120000)
	insertCrushMessages(t, dbPath, "s1", 1789120000, roles, parts)
	a := CrushAdapter{DBPath: dbPath, Cwd: "/repo"}
	path := dbPath + "/s1"

	parsed, _, _, err := a.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	incremental, _, _, _, err := a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	for name, events := range map[string][]classify.Event{"Parse": parsed, "ParseSince": incremental} {
		if len(events) != len(want) {
			t.Fatalf("%s: got %d events, want %d", name, len(events), len(want))
		}
		for i, ev := range events {
			if ev.IsError != want[i] {
				t.Errorf("%s: event %d (%s): IsError = %v, want %v", name, i, results[i].tool, ev.IsError, want[i])
			}
			if want[i] && !strings.HasSuffix(ev.Summary, " error") {
				t.Errorf("%s: event %d (%s): Summary = %q, want an error summary", name, i, results[i].tool, ev.Summary)
			}
		}
	}
}

// TestCrushShellResultIsErrorAcrossPolls is the watcher's shape: one poll
// reads the bash call before Crush has written its result, and the next reads
// the failed result in a row of its own. The call is held below the watermark
// and re-read with its result, and the event it then yields is IsError at the
// seq the watcher continues from (#115).
func TestCrushShellResultIsErrorAcrossPolls(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	const now = 1789120000
	dbPath := newCrushSession(t, "polls", now)
	insertCrushMessages(t, dbPath, "polls", now, []string{"user", "assistant", "tool", "assistant"}, []string{
		`[{"type":"text","data":{"text":"run the tests"}}]`,
		`[{"type":"tool_call","data":{"id":"c1","name":"view","input":"{\"file_path\":\"a.go\"}","finished":true}},{"type":"finish","data":{"reason":"tool_use"}}]`,
		`[{"type":"tool_result","data":{"tool_call_id":"c1","name":"view","content":"package a"}}]`,
		`[{"type":"tool_call","data":{"id":"c2","name":"bash","input":"{\"command\":\"go test ./...\"}","finished":true}},{"type":"finish","data":{"reason":"tool_use"}}]`,
	})
	a := CrushAdapter{DBPath: dbPath, Cwd: "/repo"}
	path := dbPath + "/polls"

	first, _, _, wm, err := a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("first ParseSince: %v", err)
	}
	if len(first) != 1 || first[0].IsError {
		t.Fatalf("first poll = %+v, want only the successful view", first)
	}

	insertCrushMessages(t, dbPath, "polls", now+10, []string{"tool"}, []string{
		`[{"type":"tool_result","data":{"tool_call_id":"c2","name":"bash","content":"FAIL\tpkg\n\nExit code 1\n\n<cwd>/repo</cwd>","is_error":false}}]`,
	})

	second, _, _, _, err := a.ParseSince(t.Context(), path, wm, len(first))
	if err != nil {
		t.Fatalf("second ParseSince: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second poll = %d events, want the bash call re-read with its result", len(second))
	}
	if ev := second[0]; !ev.IsError || ev.Seq != 1 {
		t.Errorf("second poll event: IsError = %v, Seq = %d; want true, 1", ev.IsError, ev.Seq)
	}
}
