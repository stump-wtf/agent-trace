package tail

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// Input Digests Through An Adapter
//
// classify.Event.InputDigest is only useful if an adapter hands BuildEventWith
// the call's real input. This replays the shape of a runaway loop seen on
// 2026-09-21 — the same MCP call over and over, tool_use then tool_result with
// nothing in between — with the repeat's keys written in a
// different order, and a third call with different arguments, through both
// Parse and ParseSince.
//
// @joestump-agent 09/23/2026 - Added with classify.Event.InputDigest.

func TestInputDigestThroughClaudeCode(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "s.jsonl"), strings.Join([]string{
		`{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","cwd":"/repo","message":{"role":"user","content":"triage"}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:00:01Z","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"mcp_gitea_issue_write","input":{"method":"add_comment","index":383,"body":"."}}]}}`,
		`{"type":"user","timestamp":"2026-01-01T10:00:02Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"{\"id\":1}"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:00:14Z","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c2","name":"mcp_gitea_issue_write","input":{"body":".","index":383,"method":"add_comment"}}]}}`,
		`{"type":"user","timestamp":"2026-01-01T10:00:15Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c2","content":"{\"id\":2}"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:00:27Z","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c3","name":"mcp_gitea_issue_write","input":{"method":"add_comment","index":384,"body":"."}}]}}`,
		`{"type":"user","timestamp":"2026-01-01T10:00:28Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c3","content":"{\"id\":3}"}]}}`,
	}, "\n")+"\n")
	a := &ClaudeCodeAdapter{Dir: dir}

	check := func(t *testing.T, events []classify.Event) {
		t.Helper()
		if len(events) != 3 {
			t.Fatalf("got %d events, want 3", len(events))
		}
		if events[0].InputDigest == "" {
			t.Fatal("InputDigest empty: the adapter did not pass the call's input")
		}
		if events[0].InputDigest != events[1].InputDigest {
			t.Errorf("a repeat with reordered keys got a new digest: %q vs %q", events[0].InputDigest, events[1].InputDigest)
		}
		if events[0].InputDigest == events[2].InputDigest {
			t.Errorf("a different issue index shares digest %q", events[0].InputDigest)
		}
	}
	t.Run("Parse", func(t *testing.T) {
		events, _, _, err := a.Parse(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		check(t, events)
	})
	t.Run("ParseSince", func(t *testing.T) {
		events, _, _, _, err := a.ParseSince(context.Background(), path, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		check(t, events)
	})
}
