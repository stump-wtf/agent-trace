package tail

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// Programs Through An Adapter
//
// classify.Event.Programs is only useful if an adapter hands BuildEventWith
// the shell call's real input. A consumer keys one node per program a run
// executed, so this parses a Claude Code transcript (Bash, with a string
// command) and a Codex one (exec_command, with its arguments stored as a JSON
// string), through Parse and ParseSince where the adapter has both.
//
// @joestump 09/27/2026 - Added with classify.Event.Programs.

func TestProgramsThroughClaudeCode(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "s.jsonl"), strings.Join([]string{
		`{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","cwd":"/repo","message":{"role":"user","content":"test it"}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:00:01Z","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"cd /repo && go test ./... 2>&1 | tee out.log"}}]}}`,
		`{"type":"user","timestamp":"2026-01-01T10:00:02Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:00:03Z","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c2","name":"Read","input":{"file_path":"/repo/go.mod"}}]}}`,
		`{"type":"user","timestamp":"2026-01-01T10:00:04Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c2","content":"module x"}]}}`,
	}, "\n")+"\n")
	a := &ClaudeCodeAdapter{Dir: dir}

	check := func(t *testing.T, events []classify.Event) {
		t.Helper()
		if len(events) != 2 {
			t.Fatalf("got %d events, want 2", len(events))
		}
		if want := []string{"go", "tee"}; !reflect.DeepEqual(events[0].Programs, want) {
			t.Errorf("Bash event Programs = %q, want %q", events[0].Programs, want)
		}
		if events[1].Programs != nil {
			t.Errorf("Read event Programs = %q, want none", events[1].Programs)
		}
	}
	t.Run("Parse", func(t *testing.T) {
		events, _, _, err := a.Parse(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		check(t, events)
	})
	t.Run("ParseSince", func(t *testing.T) {
		events, _, _, _, err := a.ParseSince(t.Context(), path, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		check(t, events)
	})
}

func TestProgramsThroughCodex(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "rollout.jsonl"), strings.Join([]string{
		`{"type":"session_meta","timestamp":"2026-01-01T10:00:00Z","payload":{"id":"sess-1","cwd":"/repo"}}`,
		`{"type":"response_item","timestamp":"2026-01-01T10:00:01Z","payload":{"type":"function_call","call_id":"call-1","name":"exec_command","arguments":"{\"cmd\":\"bash -lc 'make lint && golangci-lint run'\"}"}}`,
		`{"type":"response_item","timestamp":"2026-01-01T10:00:02Z","payload":{"type":"function_call_output","call_id":"call-1","output":"Process exited with code 0"}}`,
	}, "\n")+"\n")
	a := CodexAdapter{Dir: dir, IndexPath: "/dev/null"}
	events, _, _, err := a.Parse(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if want := []string{"make", "golangci-lint"}; !reflect.DeepEqual(events[0].Programs, want) {
		t.Errorf("exec_command event Programs = %q, want %q", events[0].Programs, want)
	}
}
