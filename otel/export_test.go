package otel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"
)

// TestSpanAttributeTypes verifies that Span.Attributes can hold non-string
// values (int64, bool) per the OTel spec (issue #13).
func TestSpanAttributeTypes(t *testing.T) {
	session := tail.SessionMeta{Key: "test-key", ID: "sess-1", Harness: tail.HarnessClaudeCode}
	events := []classify.Event{
		{Seq: 0, Tool: "Bash", Action: classify.ActionExec, ResultBytes: 4096, Summary: "ran something"},
	}
	trace := BuildTrace(session, events, nil)

	// Find the tool span (not the turn parent).
	for _, s := range trace.Spans {
		if s.Name == "ran something" {
			v, ok := s.Attributes["agent.result.bytes"]
			if !ok {
				t.Fatal("missing agent.result.bytes attribute")
			}
			// Should be int64, not string.
			n, isInt := v.(int64)
			if !isInt {
				t.Fatalf("agent.result.bytes should be int64, got %T (%v)", v, v)
			}
			if n != 4096 {
				t.Errorf("agent.result.bytes = %d, want 4096", n)
			}
			return
		}
	}
	t.Fatal("tool span not found")
}

// TestSpanOutsideCountIsInt verifies outside_count is an int, not a stringified int.
func TestSpanOutsideCountIsInt(t *testing.T) {
	session := tail.SessionMeta{Key: "test-key", ID: "sess-1", Harness: tail.HarnessClaudeCode}
	events := []classify.Event{
		{
			Seq: 0, Tool: "Bash", Action: classify.ActionExec,
			Outside:     []classify.OutsideTouch{{Scope: "tmp", Path: "/tmp/foo"}},
			ResultBytes: 100, Summary: "touched outside",
		},
	}
	trace := BuildTrace(session, events, nil)
	for _, s := range trace.Spans {
		if s.Name == "touched outside" {
			v, ok := s.Attributes["agent.outside_count"]
			if !ok {
				t.Fatal("missing agent.outside_count attribute")
			}
			n, isInt := v.(int64)
			if !isInt {
				t.Fatalf("agent.outside_count should be int64, got %T (%v)", v, v)
			}
			if n != 1 {
				t.Errorf("agent.outside_count = %d, want 1", n)
			}
			return
		}
	}
	t.Fatal("tool span not found")
}

// TestSpanJSONSerialization verifies that Span attributes serialize correctly
// to JSON with proper value types.
func TestSpanJSONSerialization(t *testing.T) {
	span := Span{
		TraceID:   "trace-1",
		SpanID:    "span-1",
		Name:      "test-span",
		Kind:      SpanKindInternal,
		StartTime: parseEventTimestamp("2026-08-09T12:00:00Z"),
		EndTime:   parseEventTimestamp("2026-08-09T12:01:00Z"),
		Attributes: map[string]any{
			"agent.session.id":   "sess-1",
			"agent.result.bytes": int64(2048),
			"agent.is_error":     true,
		},
		Status: StatusOK,
	}

	data, err := json.Marshal(span)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	jsonStr := string(data)

	// The int64 value should NOT be quoted in JSON.
	if strings.Contains(jsonStr, `"2048"`) {
		t.Errorf("int64 attribute should not be stringified in JSON: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"agent.result.bytes":2048`) {
		t.Errorf("int64 attribute not serialized correctly: %s", jsonStr)
	}

	// The bool value should NOT be quoted.
	if !strings.Contains(jsonStr, `"agent.is_error":true`) {
		t.Errorf("bool attribute not serialized correctly: %s", jsonStr)
	}

	// String values should be quoted.
	if !strings.Contains(jsonStr, `"agent.session.id":"sess-1"`) {
		t.Errorf("string attribute not serialized correctly: %s", jsonStr)
	}
}

// TestWriteJSON verifies the WriteJSON helper produces valid JSON output
// (issue #15).
func TestWriteJSON(t *testing.T) {
	session := tail.SessionMeta{Key: "test-key", ID: "sess-1", Harness: tail.HarnessClaudeCode}
	trace := Trace{
		TraceID: "trace-1",
		Session: session,
		Spans: []Span{
			{
				TraceID:   "trace-1",
				SpanID:    "span-1",
				Name:      "test",
				Kind:      SpanKindInternal,
				StartTime: parseEventTimestamp("2026-08-09T12:00:00Z"),
				EndTime:   parseEventTimestamp("2026-08-09T12:01:00Z"),
				Attributes: map[string]any{
					"key": "value",
				},
				Status: StatusOK,
			},
		},
	}

	var sb strings.Builder
	if err := WriteJSON(&sb, trace); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	output := sb.String()
	if !strings.Contains(output, `"traceId":"trace-1"`) {
		t.Errorf("output missing traceId: %s", output)
	}
	if !strings.Contains(output, `"spans"`) {
		t.Errorf("output missing spans array: %s", output)
	}

	// Verify the output is valid JSON.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		t.Fatalf("WriteJSON output is not valid JSON: %v", err)
	}
}

// TestTraceJSONShape verifies that a Trace marshals to the package's own
// JSON shape: a top-level traceId plus a spans array. This is not OTLP —
// see WriteJSON.
func TestTraceJSONShape(t *testing.T) {
	trace := Trace{
		TraceID: "abc123",
		Spans: []Span{
			{
				TraceID:   "abc123",
				SpanID:    "span-0",
				Name:      "test",
				StartTime: parseEventTimestamp("2026-08-09T12:00:00Z"),
				EndTime:   parseEventTimestamp("2026-08-09T12:01:00Z"),
				Status:    StatusOK,
			},
		},
	}

	data, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}

	if parsed["traceId"] != "abc123" {
		t.Errorf("traceId = %v, want abc123", parsed["traceId"])
	}
	spans, ok := parsed["spans"].([]any)
	if !ok || len(spans) != 1 {
		t.Fatalf("expected 1 span, got %v", parsed["spans"])
	}
}
