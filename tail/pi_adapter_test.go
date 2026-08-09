package tail

import (
	"testing"
)

func TestPiParseBasic(t *testing.T) {
	path := writeTempJSONL(t, "pi_basic.jsonl",
		`{"type":"session","id":"pi-sess-1","timestamp":"2026-01-01T10:00:00Z","cwd":"/home/user/project"}`+"\n"+
			`{"type":"message","id":"m1","parentId":"pi-sess-1","timestamp":"2026-01-01T10:00:01Z","message":{"role":"user","content":"fix the bug"}}`+"\n"+
			`{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-01-01T10:00:02Z","message":{"role":"assistant","model":"pi-model","content":[{"type":"toolCall","id":"tc1","name":"read","arguments":{"path":"/home/user/project/src/main.go"}}]}}`+"\n"+
			`{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-01-01T10:00:03Z","message":{"role":"toolResult","toolCallId":"tc1","content":"package main","isError":false}}`+"\n"+
			`{"type":"message","id":"m4","parentId":"m3","timestamp":"2026-01-01T10:00:04Z","message":{"role":"assistant","model":"pi-model","content":[{"type":"toolCall","id":"tc2","name":"bash","arguments":{"command":"go test ./..."}}]}}`+"\n"+
			`{"type":"message","id":"m5","parentId":"m4","timestamp":"2026-01-01T10:00:05Z","message":{"role":"toolResult","toolCallId":"tc2","content":"PASS","isError":false}}`+"\n")
	adapter := PiAdapter{}
	events, marks, meta, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("expected 2 events, got %d", len(events))
	}
	if len(marks) != 1 {
		t.Errorf("expected 1 user-message mark, got %d", len(marks))
	} else {
		if marks[0].Type != "user-message" {
			t.Errorf("mark type = %q", marks[0].Type)
		}
	}
	if meta.ID != "pi-sess-1" {
		t.Errorf("meta.ID = %q", meta.ID)
	}
	if meta.Cwd != "/home/user/project" {
		t.Errorf("meta.Cwd = %q", meta.Cwd)
	}
	if meta.Model != "pi-model" {
		t.Errorf("meta.Model = %q", meta.Model)
	}
	for i, ev := range events {
		if ev.Seq != i {
			t.Errorf("event %d Seq = %d", i, ev.Seq)
		}
	}
}

func TestPiParseOrphanedCall(t *testing.T) {
	path := writeTempJSONL(t, "pi_orphan.jsonl",
		`{"type":"session","id":"s1","timestamp":"2026-01-01T10:00:00Z","cwd":"/tmp"}`+"\n"+
			`{"type":"message","id":"m1","parentId":"s1","timestamp":"2026-01-01T10:00:01Z","message":{"role":"assistant","model":"m","content":[{"type":"toolCall","id":"tc1","name":"read","arguments":{"path":"/tmp/foo.go"}}]}}`+"\n")
	adapter := PiAdapter{}
	events, _, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 orphaned event, got %d", len(events))
	}
}

func TestPiParseRejectsNonSession(t *testing.T) {
	path := writeTempJSONL(t, "pi_bad.jsonl",
		`{"foo":"bar"}`)
	adapter := PiAdapter{}
	_, _, _, err := adapter.Parse(path)
	if err == nil {
		t.Error("expected error for non-pi file")
	}
}

func TestPiBashExecution(t *testing.T) {
	exitCode := 0
	path := writeTempJSONL(t, "pi_bash.jsonl",
		`{"type":"session","id":"s1","timestamp":"2026-01-01T10:00:00Z","cwd":"/tmp"}`+"\n"+
			`{"type":"message","id":"m1","parentId":"s1","timestamp":"2026-01-01T10:00:01Z","message":{"role":"bashExecution","command":"ls","output":"file.go","exitCode":0}}`+"\n")
	adapter := PiAdapter{}
	events, _, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Tool != "bash" {
		t.Errorf("tool = %q", events[0].Tool)
	}
	_ = exitCode
}

func TestPiBashExecutionError(t *testing.T) {
	path := writeTempJSONL(t, "pi_bash_err.jsonl",
		`{"type":"session","id":"s1","timestamp":"2026-01-01T10:00:00Z","cwd":"/tmp"}`+"\n"+
			`{"type":"message","id":"m1","parentId":"s1","timestamp":"2026-01-01T10:00:01Z","message":{"role":"bashExecution","command":"false","output":"error","exitCode":1}}`+"\n")
	adapter := PiAdapter{}
	events, _, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event")
	}
	if !events[0].IsError {
		t.Error("expected IsError=true for exit code 1")
	}
}

func TestPiCompactionMark(t *testing.T) {
	path := writeTempJSONL(t, "pi_compact.jsonl",
		`{"type":"session","id":"s1","timestamp":"2026-01-01T10:00:00Z","cwd":"/tmp"}`+"\n"+
			`{"type":"compaction","id":"c1","parentId":"s1","timestamp":"2026-01-01T10:00:01Z"}`+"\n"+
			`{"type":"message","id":"m1","parentId":"c1","timestamp":"2026-01-01T10:00:02Z","message":{"role":"user","content":"hello"}}`+"\n")
	adapter := PiAdapter{}
	_, marks, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	found := false
	for _, m := range marks {
		if m.Type == "compaction" {
			found = true
		}
	}
	if !found {
		t.Error("expected a compaction mark")
	}
}

func TestPiBranchSummaryMark(t *testing.T) {
	path := writeTempJSONL(t, "pi_branch.jsonl",
		`{"type":"session","id":"s1","timestamp":"2026-01-01T10:00:00Z","cwd":"/tmp"}`+"\n"+
			`{"type":"branch_summary","id":"b1","parentId":"s1","timestamp":"2026-01-01T10:00:01Z","summary":"switched from feature to main"}`+"\n"+
			`{"type":"message","id":"m1","parentId":"b1","timestamp":"2026-01-01T10:00:02Z","message":{"role":"user","content":"hello"}}`+"\n")
	adapter := PiAdapter{}
	_, marks, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	found := false
	for _, m := range marks {
		if m.Type == "compaction" && m.Note != "" {
			found = true
		}
	}
	if !found {
		t.Error("expected a branch_summary compaction mark with note")
	}
}

func TestPiV1FileWithoutIDs(t *testing.T) {
	// V1 files have a session header with ID but entries without IDs.
	// linearizePi should pass through entries without IDs as-is.
	path := writeTempJSONL(t, "pi_v1.jsonl",
		`{"type":"session","id":"s1","timestamp":"2026-01-01T10:00:00Z","cwd":"/tmp"}`+"\n"+
			`{"type":"message","timestamp":"2026-01-01T10:00:01Z","message":{"role":"user","content":"hello"}}`+"\n")
	adapter := PiAdapter{}
	_, marks, _, err := adapter.Parse(path)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(marks) != 1 {
		t.Errorf("expected 1 mark, got %d", len(marks))
	}
}

func TestPiSummarize(t *testing.T) {
	path := writeTempJSONL(t, "pi_summary.jsonl",
		`{"type":"session","id":"s1","timestamp":"2026-01-01T10:00:00Z","cwd":"/home/user/project"}`+"\n"+
			`{"type":"session_info","id":"si1","parentId":"s1","name":"my session"}`+"\n")
	adapter := PiAdapter{}
	meta, err := adapter.Summarize(path)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}
	if meta.Title != "my session" {
		t.Errorf("Title = %q, want 'my session'", meta.Title)
	}
}
