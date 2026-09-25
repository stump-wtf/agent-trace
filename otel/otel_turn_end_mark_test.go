package otel

import (
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"
)

// TestBuildTraceTurnEndMark: a "turn-end" mark makes no span of its own and
// leaves every parent as it was, but it is a timeline entry, so the turn's
// last tool span now ends where the turn ended rather than stretching to the
// next user message across the time the agent sat idle.
func TestBuildTraceTurnEndMark(t *testing.T) {
	session := tail.SessionMeta{ID: "s", Key: "k", Harness: tail.HarnessClaudeCode, StartedAt: "2026-09-11T11:40:00Z"}
	events := []classify.Event{
		{Seq: 0, Timestamp: "2026-09-11T11:40:05Z", Tool: "Read", Action: classify.ActionRead, Summary: "read a"},
		{Seq: 1, Timestamp: "2026-09-11T11:50:05Z", Tool: "Read", Action: classify.ActionRead, Summary: "read b"},
	}
	first := classify.Mark{Seq: 0, Timestamp: "2026-09-11T11:40:02Z", Type: "user-message", Note: "one"}
	second := classify.Mark{Seq: 1, Timestamp: "2026-09-11T11:50:00Z", Type: "user-message", Note: "two"}
	turnEnd := classify.Mark{Seq: 1, Timestamp: "2026-09-11T11:41:00Z", Type: "turn-end", Note: "end_turn"}

	without := BuildTrace(session, events, []classify.Mark{first, second})
	with := BuildTrace(session, events, []classify.Mark{first, turnEnd, second})

	if len(with.Spans) != len(without.Spans) {
		t.Fatalf("spans with a turn-end mark = %d, without = %d; want no span for it", len(with.Spans), len(without.Spans))
	}
	for i := range without.Spans {
		if with.Spans[i].ParentSpanID != without.Spans[i].ParentSpanID || with.Spans[i].Name != without.Spans[i].Name {
			t.Errorf("span %d changed with a turn-end mark: %+v -> %+v", i, without.Spans[i], with.Spans[i])
		}
	}

	var readA, readAWithout *Span
	for i := range with.Spans {
		if with.Spans[i].Name == "read a" {
			readA, readAWithout = &with.Spans[i], &without.Spans[i]
		}
	}
	if readA == nil {
		t.Fatal("no span for the first turn's tool call")
	}
	if want := parseEventTimestamp(second.Timestamp); !readAWithout.EndTime.Equal(want) {
		t.Fatalf("without a turn-end the span ends %v, want the next user message %v; the comparison would prove nothing",
			readAWithout.EndTime, want)
	}
	if want := parseEventTimestamp(turnEnd.Timestamp); !readA.EndTime.Equal(want) {
		t.Errorf("tool span end = %v, want the turn end %v", readA.EndTime, want)
	}
}
