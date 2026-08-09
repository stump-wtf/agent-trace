package tail

import (
	"path/filepath"
	"testing"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
)

func TestCodexParse(t *testing.T) {
	adapter := CodexAdapter{Dir: "testdata"}
	path := filepath.Join("testdata", "codex_marks.jsonl")

	events, marks, meta, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(events) != 2 {
		t.Errorf("expected 2 events, got %d", len(events))
	}
	for i, ev := range events {
		if ev.Seq != i {
			t.Errorf("event %d has Seq %d", i, ev.Seq)
		}
	}

	if len(marks) != 3 {
		t.Errorf("expected 3 marks (2 user-message, 1 compaction), got %d", len(marks))
	}

	// Verify mark types and seq interleaving.
	// Timeline: user(0) → call(0) → user(1) → call(1) → compaction(2)
	expectedMarks := []struct {
		seq      int
		markType string
	}{
		{0, "user-message"},
		{1, "user-message"},
		{2, "compaction"},
	}
	for i, want := range expectedMarks {
		if i >= len(marks) {
			break
		}
		if marks[i].Seq != want.seq {
			t.Errorf("mark %d: Seq = %d, want %d", i, marks[i].Seq, want.seq)
		}
		if marks[i].Type != want.markType {
			t.Errorf("mark %d: Type = %q, want %q", i, marks[i].Type, want.markType)
		}
	}

	if meta.ID != "sess-001" {
		t.Errorf("meta.ID = %q, want sess-001", meta.ID)
	}
	if meta.Cwd != "/home/user/project" {
		t.Errorf("meta.Cwd = %q", meta.Cwd)
	}
	if meta.Title == "" {
		t.Error("meta.Title should not be empty")
	}
}

func TestCodexParseOrphanedCall(t *testing.T) {
	// A call without a result should still produce an event.
	path := writeTempJSONL(t, "codex_orphan.jsonl",
		`{"type":"session_meta","timestamp":"2026-01-01T10:00:00Z","payload":{"id":"s1","cwd":"/tmp"}}
{"type":"response_item","timestamp":"2026-01-01T10:00:01Z","payload":{"type":"function_call","call_id":"c1","name":"shell","arguments":{"command":"ls"}}}
`)
	adapter := CodexAdapter{}
	events, _, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (orphaned call), got %d", len(events))
	}
	if events[0].Tool != "shell" {
		t.Errorf("expected tool 'shell', got %q", events[0].Tool)
	}
}

func TestCodexParseOrphanedOutput(t *testing.T) {
	// An output without a call should be ignored.
	path := writeTempJSONL(t, "codex_orphan_out.jsonl",
		`{"type":"session_meta","timestamp":"2026-01-01T10:00:00Z","payload":{"id":"s1","cwd":"/tmp"}}
{"type":"response_item","timestamp":"2026-01-01T10:00:01Z","payload":{"type":"function_call_output","call_id":"unknown","output":"result"}}
`)
	adapter := CodexAdapter{}
	events, _, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for orphan output, got %d", len(events))
	}
}

func TestCodexParseDuplicateCall(t *testing.T) {
	// Duplicate call IDs should be first-wins.
	path := writeTempJSONL(t, "codex_dup.jsonl",
		`{"type":"session_meta","timestamp":"2026-01-01T10:00:00Z","payload":{"id":"s1","cwd":"/tmp"}}
{"type":"response_item","timestamp":"2026-01-01T10:00:01Z","payload":{"type":"function_call","call_id":"c1","name":"shell","arguments":{"command":"first"}}}
{"type":"response_item","timestamp":"2026-01-01T10:00:02Z","payload":{"type":"function_call","call_id":"c1","name":"shell","arguments":{"command":"second"}}}
{"type":"response_item","timestamp":"2026-01-01T10:00:03Z","payload":{"type":"function_call_output","call_id":"c1","output":"done"}}
`)
	adapter := CodexAdapter{}
	events, _, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (deduped), got %d", len(events))
	}
}

func TestCodexParseResponseItemMessage(t *testing.T) {
	// User messages in response_item payloads (current rollout format).
	path := writeTempJSONL(t, "codex_rimsg.jsonl",
		`{"type":"session_meta","timestamp":"2026-01-01T10:00:00Z","payload":{"id":"s1","cwd":"/tmp"}}
{"type":"response_item","timestamp":"2026-01-01T10:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"text","text":"hello world"}]}}
`)
	adapter := CodexAdapter{}
	_, marks, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	found := false
	for _, m := range marks {
		if m.Type == "user-message" && m.Note == "hello world" {
			found = true
		}
	}
	if !found {
		t.Error("expected a user-message mark from response_item message payload")
	}
}

func TestCodexParseTopLevelMessageString(t *testing.T) {
	// Top-level message line with string content (older format).
	path := writeTempJSONL(t, "codex_strmsg.jsonl",
		`{"type":"session_meta","timestamp":"2026-01-01T10:00:00Z","payload":{"id":"s1","cwd":"/tmp"}}
{"type":"message","timestamp":"2026-01-01T10:00:01Z","role":"user","content":"plain text message"}
`)
	adapter := CodexAdapter{}
	_, marks, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	found := false
	for _, m := range marks {
		if m.Type == "user-message" && m.Note == "plain text message" {
			found = true
		}
	}
	if !found {
		t.Error("expected a user-message mark from top-level string content")
	}
}

func TestCodexParseRejectsNonSession(t *testing.T) {
	path := writeTempJSONL(t, "codex_bad.jsonl",
		`{"type":"something_else","timestamp":"2026-01-01T10:00:00Z"}
`)
	adapter := CodexAdapter{}
	_, _, _, err := adapter.Parse(path)
	if err == nil {
		t.Error("expected error for non-Codex file")
	}
}

func TestCodexParseMarksTimestamp(t *testing.T) {
	path := filepath.Join("testdata", "codex_marks.jsonl")
	adapter := CodexAdapter{Dir: "testdata"}
	_, marks, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	for _, m := range marks {
		if m.Timestamp == "" {
			t.Errorf("mark type %q has empty Timestamp", m.Type)
		}
	}
	_ = classify.ActionEdit // keep import alive for future expansion
}
