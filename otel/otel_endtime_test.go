package otel

import (
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"
)

func TestBuildTraceEndTime(t *testing.T) {
	session := tail.SessionMeta{
		Key:       "test-key",
		ID:        "sess-1",
		Harness:   tail.HarnessClaudeCode,
		StartedAt: "2026-01-01T10:00:00Z",
	}
	events := []classify.Event{
		{Seq: 0, Timestamp: "2026-01-01T10:00:01Z", Tool: "Read", Action: classify.ActionRead, Summary: "read foo.go"},
		{Seq: 1, Timestamp: "2026-01-01T10:00:03Z", Tool: "Edit", Action: classify.ActionEdit, Summary: "edit bar.go"},
	}
	marks := []classify.Mark{
		{Seq: 0, Timestamp: "2026-01-01T10:00:00Z", Type: "user-message", Note: "fix the bug"},
	}
	trace := BuildTrace(session, events, marks)
	if len(trace.Spans) == 0 {
		t.Fatal("expected spans")
	}
	for _, span := range trace.Spans {
		if span.EndTime.IsZero() {
			t.Errorf("span %q has zero EndTime", span.Name)
		}
		if span.EndTime.Before(span.StartTime) {
			t.Errorf("span %q EndTime %v before StartTime %v", span.Name, span.EndTime, span.StartTime)
		}
	}
}

func TestBuildTraceNoTimeNow(t *testing.T) {
	// Building the same trace twice should produce identical timings.
	session := tail.SessionMeta{Key: "k", ID: "s", Harness: tail.HarnessCodex, StartedAt: "2026-01-01T10:00:00Z"}
	events := []classify.Event{
		{Seq: 0, Timestamp: "2026-01-01T10:00:01Z", Tool: "Bash", Action: classify.ActionExec},
	}
	t1 := BuildTrace(session, events, nil)
	t2 := BuildTrace(session, events, nil)
	for i := range t1.Spans {
		if !t1.Spans[i].StartTime.Equal(t2.Spans[i].StartTime) {
			t.Errorf("span %d StartTime not deterministic", i)
		}
		if !t1.Spans[i].EndTime.Equal(t2.Spans[i].EndTime) {
			t.Errorf("span %d EndTime not deterministic", i)
		}
	}
}

func TestTruncateGuard(t *testing.T) {
	if truncate("hello", 0) != "" {
		t.Error("truncate with max=0 should return empty string")
	}
}
