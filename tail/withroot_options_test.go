package tail

import (
	"os"
	"path/filepath"
	"testing"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
)

// TestRetargetedAdaptersKeepOptionalInterfaces guards the coupling between
// WithRoot and every optional interface in this package. The optional
// interfaces are satisfied with pointer receivers, so an adapter returned by
// value fails their type assertions — silently, because a failed assertion is
// how a consumer asks "does this adapter support X?".
func TestRetargetedAdaptersKeepOptionalInterfaces(t *testing.T) {
	for _, a := range DefaultAdaptersIn(t.TempDir()) {
		if _, ok := a.(OptionsSetter); !ok {
			t.Errorf("%T from DefaultAdaptersIn does not implement OptionsSetter", a)
		}
		if _, ok := a.(DiagnosticsSource); !ok {
			t.Errorf("%T from DefaultAdaptersIn does not implement DiagnosticsSource", a)
		}
	}
	// WithRoot("") restores the default location and must not change this.
	for _, a := range DefaultAdaptersIn("") {
		if _, ok := a.(OptionsSetter); !ok {
			t.Errorf("%T from DefaultAdaptersIn(\"\") does not implement OptionsSetter", a)
		}
	}
}

// TestVerifyPatternsReachRetargetedAdapters is the failure this actually
// caused: the watcher injects WatchConfig.VerifyPatterns through OptionsSetter,
// so adapters obtained from DefaultAdaptersIn — the constructor a hermetic test
// or a relocated $HOME uses — classified "just test" as a plain exec instead of
// a verify, with nothing to indicate the setting had been dropped.
func TestVerifyPatternsReachRetargetedAdapters(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".claude", "projects", "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	session := `{"type":"assistant","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","cwd":"` + root + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"just test"}}]}}` + "\n" +
		`{"type":"user","timestamp":"2026-01-01T10:00:01Z","sessionId":"s1","cwd":"` + root + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(session), 0o644); err != nil {
		t.Fatal(err)
	}

	adapters := DefaultAdaptersIn(root)
	NewWatcherWithConfig(WatchConfig{VerifyPatterns: []string{"just test"}}, adapters)

	for _, a := range adapters {
		if a.Harness() != HarnessClaudeCode {
			continue
		}
		events, _, _, err := a.Parse(filepath.Join(dir, "s.jsonl"))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if events[0].Action != classify.ActionVerify {
			t.Errorf("action = %q, want %q — VerifyPatterns never reached the adapter",
				events[0].Action, classify.ActionVerify)
		}
		return
	}
	t.Fatal("no Claude Code adapter in DefaultAdaptersIn")
}
