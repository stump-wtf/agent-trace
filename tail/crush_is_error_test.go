package tail

import (
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// TestCrushToolResultIsError checks the adapter carries a tool_result part's
// own is_error flag onto the event, from both Parse and ParseSince. Before
// this every Crush event read IsError false, so a failed view or edit looked
// like a success in the summary and in exported spans.
func TestCrushToolResultIsError(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	dbPath := newCrushSession(t, "s1", 1789120000)
	insertCrushMessages(t, dbPath, "s1", 1789120000,
		[]string{"assistant", "tool", "assistant", "tool"},
		[]string{
			`[{"type":"tool_call","data":{"id":"c1","name":"view","input":"{\"file_path\":\"missing.go\"}","finished":true}},{"type":"finish","data":{"reason":"tool_use"}}]`,
			`[{"type":"tool_result","data":{"tool_call_id":"c1","name":"view","content":"file not found: /repo/missing.go","is_error":true}}]`,
			`[{"type":"tool_call","data":{"id":"c2","name":"view","input":"{\"file_path\":\"main.go\"}","finished":true}},{"type":"finish","data":{"reason":"tool_use"}}]`,
			`[{"type":"tool_result","data":{"tool_call_id":"c2","name":"view","content":"package main","is_error":false}}]`,
		})
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
		if len(events) != 2 {
			t.Fatalf("%s: got %d events, want 2", name, len(events))
		}
		if !events[0].IsError || !strings.HasSuffix(events[0].Summary, " error") {
			t.Errorf("%s: failed view: IsError = %v, Summary = %q; want an error", name, events[0].IsError, events[0].Summary)
		}
		if events[1].IsError {
			t.Errorf("%s: successful view: IsError = true", name)
		}
	}
}
