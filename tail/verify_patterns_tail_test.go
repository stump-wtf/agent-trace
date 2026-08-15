package tail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// TestVerifyPatternsFlowThroughWatcher verifies that WatchConfig.VerifyPatterns
// reaches each adapter's classify.Options via the OptionsSetter interface, so
// custom verify commands (e.g. "just test") are classified as ActionVerify
// rather than ActionExec.
func TestVerifyPatternsFlowThroughWatcher(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, ".claude", "projects", "test")
	sessionFile := filepath.Join(sessionDir, "session.jsonl")

	// Write a Claude Code session with a shell tool call running "just test".
	writeClaudeCodeSession(t, sessionFile, "just test")

	adapter := &ClaudeCodeAdapter{Dir: sessionDir}
	adapters := []Adapter{adapter}
	NewWatcherWithConfig(WatchConfig{
		VerifyPatterns: []string{"just test"},
	}, adapters)

	// Verify the adapter received the patterns via SetOptions.
	if adapter.opts == nil {
		t.Fatal("ClaudeCodeAdapter.opts is nil — SetOptions was not called")
	}
	if len(adapter.opts.VerifyPatterns) != 1 || adapter.opts.VerifyPatterns[0] != "just test" {
		t.Fatalf("VerifyPatterns = %v, want [just test]", adapter.opts.VerifyPatterns)
	}

	// Verify the patterns actually affect classification.
	events, _, _, err := adapter.Parse(t.Context(), sessionFile)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least 1 event")
	}
	if events[0].Action != classify.ActionVerify {
		t.Errorf("action = %q, want %q (just test should be verify)", events[0].Action, classify.ActionVerify)
	}
}

// TestVerifyPatternsEmptyLeavesDefaultsUnchanged verifies that a zero-length
// VerifyPatterns in WatchConfig does not call SetOptions (adapters use defaults).
func TestVerifyPatternsEmptyLeavesDefaultsUnchanged(t *testing.T) {
	adapters := DefaultAdapters()

	w := NewWatcherWithConfig(WatchConfig{}, adapters)

	for _, a := range w.adapters {
		if os, ok := a.(OptionsSetter); ok {
			_ = os // should not happen with empty VerifyPatterns
		}
		// Check that opts was NOT set (no SetOptions call).
		switch v := a.(type) {
		case *ClaudeCodeAdapter:
			if v.opts != nil {
				t.Error("ClaudeCodeAdapter.opts should be nil with empty VerifyPatterns")
			}
		case *CodexAdapter:
			if v.opts != nil {
				t.Error("CodexAdapter.opts should be nil with empty VerifyPatterns")
			}
		}
	}
}

// writeClaudeCodeSession writes a minimal Claude Code session JSONL with a
// single bash tool call containing the given command.
func writeClaudeCodeSession(t *testing.T, path, command string) {
	t.Helper()
	toolUse := `[{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":"` + command + `"}}]`
	toolResult := `[{"type":"tool_result","tool_use_id":"call-1","content":"ok"}]`
	lines := []string{
		`{"type":"assistant","message":{"content":` + toolUse + `}}`,
		`{"type":"user","message":{"content":` + toolResult + `}}`,
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
}
