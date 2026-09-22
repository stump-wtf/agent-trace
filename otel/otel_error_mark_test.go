package otel

import (
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"
)

// TestBuildTraceAttachesErrorMarksToTurn checks that an "error" mark — which
// the Crush adapter emits for a failed turn, and the Claude Code adapter for
// an API failure — lands on the open turn's span as an exception event with an
// ERROR status, and that the tool spans' parents are untouched.
func TestBuildTraceAttachesErrorMarksToTurn(t *testing.T) {
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
		t.Fatalf("spans with an in-turn error mark = %d, without = %d; the mark lands on the turn span, not a new one",
			len(withError.Spans), len(without.Spans))
	}
	for i := range without.Spans {
		if withError.Spans[i].ParentSpanID != without.Spans[i].ParentSpanID {
			t.Errorf("span %d parent changed with an error mark: %q -> %q",
				i, without.Spans[i].ParentSpanID, withError.Spans[i].ParentSpanID)
		}
	}

	var turn *Span
	for i := range withError.Spans {
		if _, isTurn := withError.Spans[i].Attributes["agent.turn.type"]; isTurn {
			turn = &withError.Spans[i]
			break
		}
	}
	if turn == nil {
		t.Fatal("no turn span found")
	}
	if turn.Status != StatusError {
		t.Errorf("turn span status = %d, want StatusError", turn.Status)
	}
	if turn.StatusMsg != "Bad Request: context window exceeded" {
		t.Errorf("turn span status message = %q, want the mark's note", turn.StatusMsg)
	}
	if len(turn.Events) != 1 {
		t.Fatalf("turn span events = %d, want 1", len(turn.Events))
	}
	ev := turn.Events[0]
	if ev.Name != "exception" {
		t.Errorf("event name = %q, want \"exception\"", ev.Name)
	}
	if ev.Attributes["exception.message"] != "Bad Request: context window exceeded" {
		t.Errorf("event exception.message = %v, want the mark's note", ev.Attributes["exception.message"])
	}
	if !ev.Timestamp.Equal(parseEventTimestamp("2026-09-11T11:56:20Z")) {
		t.Errorf("event timestamp = %v, want the mark's timestamp", ev.Timestamp)
	}
}

// TestBuildTraceRootsOutOfTurnErrorMark checks the outage shape: an error mark
// with no open turn roots a standalone zero-length ERROR span rather than
// being dropped.
func TestBuildTraceRootsOutOfTurnErrorMark(t *testing.T) {
	session := tail.SessionMeta{ID: "s", Key: "k", Harness: tail.HarnessCrush, StartedAt: "2026-03-03T09:00:31Z"}
	marks := []classify.Mark{
		{Seq: 0, Timestamp: "2026-03-03T09:00:31Z", Type: "error", Note: "429 quota exhausted"},
	}

	trace := BuildTrace(session, nil, marks)
	if len(trace.Spans) != 1 {
		t.Fatalf("spans = %d, want 1 standalone error span", len(trace.Spans))
	}
	span := trace.Spans[0]
	if span.Name != "error" {
		t.Errorf("span name = %q, want \"error\"", span.Name)
	}
	if span.Status != StatusError {
		t.Errorf("span status = %d, want StatusError", span.Status)
	}
	if span.StatusMsg != "429 quota exhausted" {
		t.Errorf("span status message = %q, want the mark's note", span.StatusMsg)
	}
	if span.ParentSpanID != "" {
		t.Errorf("span parent = %q, want none", span.ParentSpanID)
	}
	if !span.EndTime.Equal(span.StartTime) {
		t.Errorf("span end = %v, start = %v; a rooted error span is zero-length", span.EndTime, span.StartTime)
	}
}
