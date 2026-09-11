package otel

import (
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"
)

// TestBuildTraceToleratesErrorMarks checks that an "error" mark — which the
// Crush adapter emits for a failed turn — neither breaks the trace nor steals
// the tool spans' parent. BuildTrace has no span for the type; it must pass it
// over rather than misfile it.
func TestBuildTraceToleratesErrorMarks(t *testing.T) {
	session := tail.SessionMeta{ID: "s", Key: "k", Harness: tail.HarnessCrush, StartedAt: "2026-09-11T11:40:02Z"}
	marks := []classify.Mark{
		{Seq: 0, Timestamp: "2026-09-11T11:40:02Z", Type: "user-message", Note: "run the sweep"},
		{Seq: 1, Timestamp: "2026-09-11T11:56:20Z", Type: "error", Note: "Bad Request: context window exceeded"},
	}
	events := []classify.Event{
		{Seq: 0, Timestamp: "2026-09-11T11:40:05Z", Tool: "view", Action: classify.ActionRead},
	}

	withError := BuildTrace(session, events, marks)
	without := BuildTrace(session, events, marks[:1])

	if len(withError.Spans) != len(without.Spans) {
		t.Fatalf("spans with an error mark = %d, without = %d; an unmapped mark must not add or drop spans",
			len(withError.Spans), len(without.Spans))
	}
	for i := range without.Spans {
		if withError.Spans[i].ParentSpanID != without.Spans[i].ParentSpanID {
			t.Errorf("span %d parent changed with an error mark: %q -> %q",
				i, without.Spans[i].ParentSpanID, withError.Spans[i].ParentSpanID)
		}
	}
}
