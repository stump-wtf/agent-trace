package tail

import (
	"path/filepath"
	"testing"
)

func TestCodexFidelityTurnContext(t *testing.T) {
	adapter := CodexAdapter{Dir: "testdata", IndexPath: "/dev/null"}
	path := filepath.Join("testdata", "codex_fidelity.jsonl")
	_, _, meta, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if meta.Model != "o4-mini" {
		t.Errorf("Model = %q, want o4-mini", meta.Model)
	}
	if meta.GitBranch != "main" {
		t.Errorf("GitBranch = %q, want main", meta.GitBranch)
	}
}

func TestCodexFidelityPatchApplyEnd(t *testing.T) {
	adapter := CodexAdapter{Dir: "testdata", IndexPath: "/dev/null"}
	path := filepath.Join("testdata", "codex_fidelity.jsonl")
	events, _, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	// The apply_patch call should have its patch enriched with the
	// authoritative change list from patch_apply_end.
	ev := events[0]
	if len(ev.Targets) == 0 {
		t.Error("expected targets from enriched apply_patch")
	}
	found := false
	for _, target := range ev.Targets {
		if target.Path == "src/new_test.go" {
			found = true
		}
	}
	if !found {
		t.Error("expected target src/new_test.go from patch_apply_end enrichment")
	}
	// patch_apply_end success=true should set IsError=false
	if ev.IsError {
		t.Error("expected IsError=false for successful apply_patch")
	}
}

func TestCodexOlderFormatBareID(t *testing.T) {
	// An older session with just a bare {"id":...} header should be recognized.
	path := writeTempJSONL(t, "codex_old.jsonl",
		`{"id":"old-session-123","timestamp":"2026-01-01T10:00:00Z"}
{"type":"response_item","timestamp":"2026-01-01T10:00:01Z","payload":{"type":"function_call","call_id":"c1","name":"shell","arguments":{"command":"ls"}}}
{"type":"response_item","timestamp":"2026-01-01T10:00:02Z","payload":{"type":"function_call_output","call_id":"c1","output":"file.go"}}
`)
	adapter := CodexAdapter{}
	events, _, meta, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("expected 1 event, got %d", len(events))
	}
	if meta.ID != "old-session-123" {
		t.Errorf("meta.ID = %q, want old-session-123", meta.ID)
	}
}

func TestCodexSubagentExclusion(t *testing.T) {
	path := writeTempJSONL(t, "codex_subagent.jsonl",
		`{"type":"session_meta","timestamp":"2026-01-01T10:00:00Z","payload":{"id":"sub-1","cwd":"/tmp","thread_source":"subagent"}}
`)
	adapter := CodexAdapter{}
	meta, err := adapter.Summarize(path)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}
	if !meta.Auxiliary {
		t.Error("subagent session should be marked Auxiliary")
	}
}
