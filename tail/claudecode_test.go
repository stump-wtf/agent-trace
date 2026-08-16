package tail

import (
	"path/filepath"
	"testing"
)

func TestClaudeCodeParseBasic(t *testing.T) {
	adapter := ClaudeCodeAdapter{Dir: "testdata"}
	path := filepath.Join("testdata", "claudecode_basic.jsonl")

	events, marks, meta, err := adapter.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(events) != 3 {
		t.Errorf("expected 3 events, got %d", len(events))
	}

	if len(marks) != 1 {
		t.Errorf("expected 1 user-message mark, got %d marks", len(marks))
	} else {
		if marks[0].Type != "user-message" {
			t.Errorf("mark type = %q, want user-message", marks[0].Type)
		}
		if marks[0].Note != "fix the login bug" {
			t.Errorf("mark note = %q", marks[0].Note)
		}
	}

	if meta.ID != "cc-session-1" {
		t.Errorf("meta.ID = %q", meta.ID)
	}
	if meta.Cwd != "/home/user/project" {
		t.Errorf("meta.Cwd = %q", meta.Cwd)
	}
	if meta.Model != "claude-sonnet-4-20250514" {
		t.Errorf("meta.Model = %q", meta.Model)
	}
	if meta.GitBranch != "main" {
		t.Errorf("meta.GitBranch = %q", meta.GitBranch)
	}

	for i, ev := range events {
		if ev.Seq != i {
			t.Errorf("event %d Seq = %d", i, ev.Seq)
		}
	}

	// Verify action classification
	if events[0].Action != "read" {
		t.Errorf("event 0 action = %q, want read", events[0].Action)
	}
	if events[1].Action != "edit" {
		t.Errorf("event 1 action = %q, want edit", events[1].Action)
	}
	if events[2].Action != "verify" {
		t.Errorf("event 2 action = %q, want verify", events[2].Action)
	}
}

func TestClaudeCodeParseOrphanedCall(t *testing.T) {
	path := writeTempJSONL(t, "cc_orphan.jsonl",
		`{"type":"assistant","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","cwd":"/tmp","message":{"role":"assistant","model":"m","content":[{"type":"tool_use","id":"c1","name":"Read","input":{"file_path":"/tmp/foo.go"}}]}}`)
	adapter := ClaudeCodeAdapter{}
	events, _, _, err := adapter.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 orphaned event, got %d", len(events))
	}
	if events[0].Tool != "Read" {
		t.Errorf("tool = %q", events[0].Tool)
	}
}

func TestClaudeCodeParseRejectsNonSession(t *testing.T) {
	path := writeTempJSONL(t, "cc_bad.jsonl",
		`{"foo":"bar"}`)
	adapter := ClaudeCodeAdapter{}
	_, _, _, err := adapter.Parse(t.Context(), path)
	if err == nil {
		t.Error("expected error for non-CC file")
	}
}

func TestClaudeCodeParseMalformedLines(t *testing.T) {
	path := writeTempJSONL(t, "cc_malformed.jsonl",
		`{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","cwd":"/tmp","message":{"role":"user","content":"hello"}}
not valid json
{"type":"assistant","timestamp":"2026-01-01T10:00:01Z","sessionId":"s1","message":{"role":"assistant","model":"m","content":[{"type":"tool_use","id":"c1","name":"bash","input":{"command":"ls"}}]}}
{"type":"user","timestamp":"2026-01-01T10:00:02Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"file.go"}]}}`)
	adapter := ClaudeCodeAdapter{}
	events, _, _, err := adapter.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("expected 1 event (malformed line skipped), got %d", len(events))
	}
}

func TestClaudeCodeSummarize(t *testing.T) {
	adapter := ClaudeCodeAdapter{Dir: "testdata"}
	path := filepath.Join("testdata", "claudecode_basic.jsonl")
	meta, err := adapter.Summarize(t.Context(), path)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}
	if meta.ID != "cc-session-1" {
		t.Errorf("ID = %q", meta.ID)
	}
	if meta.Title == "" {
		t.Error("Title should not be empty")
	}
}

func TestClaudeCodeCompactionMark(t *testing.T) {
	path := writeTempJSONL(t, "cc_compact.jsonl",
		`{"type":"system","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","cwd":"/tmp","subtype":"compact messages"}
{"type":"user","timestamp":"2026-01-01T10:00:01Z","sessionId":"s1","cwd":"/tmp","message":{"role":"user","content":"hello"}}`)
	adapter := ClaudeCodeAdapter{}
	_, marks, _, err := adapter.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	foundCompaction := false
	for _, m := range marks {
		if m.Type == "compaction" {
			foundCompaction = true
		}
	}
	if !foundCompaction {
		t.Error("expected a compaction mark")
	}
}
