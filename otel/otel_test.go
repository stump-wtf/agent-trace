package otel

import (
	"testing"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
	"gitea.stump.rocks/stump.wtf/agent-trace/tail"
)

func TestBuildTraceEmpty(t *testing.T) {
	session := tail.SessionMeta{Key: "test-key", ID: "sess-1", Harness: tail.HarnessClaudeCode}
	trace := BuildTrace(session, nil, nil)
	if trace.TraceID == "" {
		t.Error("trace ID should not be empty")
	}
	if len(trace.Spans) != 0 {
		t.Errorf("expected 0 spans for empty input, got %d", len(trace.Spans))
	}
}

func TestBuildTraceWithEvents(t *testing.T) {
	session := tail.SessionMeta{Key: "test-key", ID: "sess-1", Harness: tail.HarnessClaudeCode}
	events := []classify.Event{
		{Seq: 0, Tool: "Read", Action: classify.ActionRead, Summary: "read foo.go"},
		{Seq: 1, Tool: "Edit", Action: classify.ActionEdit, Summary: "edit bar.go"},
		{Seq: 2, Tool: "Bash", Action: classify.ActionVerify, IsError: true, Summary: "go test failed"},
	}
	marks := []classify.Mark{
		{Seq: 0, Type: "user-message", Note: "fix the login bug"},
	}
	trace := BuildTrace(session, events, marks)
	if len(trace.Spans) < 3 {
		t.Errorf("expected at least 3 spans (1 turn + 3 tools), got %d", len(trace.Spans))
	}
	// Check that the error event has error status.
	foundError := false
	for _, s := range trace.Spans {
		if s.Status == StatusError {
			foundError = true
			break
		}
	}
	if !foundError {
		t.Error("expected at least one span with error status")
	}
}

func TestBuildTraceDeterministicIDs(t *testing.T) {
	session := tail.SessionMeta{Key: "same-key", ID: "sess-1", Harness: tail.HarnessClaudeCode}
	events := []classify.Event{{Seq: 0, Tool: "Read", Action: classify.ActionRead}}
	t1 := BuildTrace(session, events, nil)
	t2 := BuildTrace(session, events, nil)
	if t1.TraceID != t2.TraceID {
		t.Error("same session key should produce same trace ID")
	}
	if len(t1.Spans) > 0 && len(t2.Spans) > 0 && t1.Spans[0].SpanID != t2.Spans[0].SpanID {
		t.Error("same inputs should produce same span IDs")
	}
}

func TestParseEventTimestamp(t *testing.T) {
	ts := "2026-08-09T12:00:00Z"
	parsed := parseEventTimestamp(ts)
	if parsed.IsZero() {
		t.Error("should parse valid RFC3339 timestamp")
	}
	if parsed.Year() != 2026 {
		t.Errorf("expected year 2026, got %d", parsed.Year())
	}
}

func TestParseEventTimestampInvalid(t *testing.T) {
	parsed := parseEventTimestamp("not-a-timestamp")
	if !parsed.IsZero() {
		t.Error("invalid timestamp should return zero time, not time.Now()")
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		s    string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello world", 8, "hello w…"},
		{"ab", 2, "ab"},
	}
	for _, tt := range tests {
		got := truncate(tt.s, tt.max)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
		}
	}
}
