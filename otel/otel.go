// Package otel converts classified agent trace events into OpenTelemetry span
// structures suitable for export to any OTel collector or direct submission
// to Cairn's trace API.
//
// The mapping follows agent tracing conventions:
//   - User messages → parent spans (turn boundaries)
//   - Tool calls → child spans under the enclosing turn
//   - Compactions/subagents → annotation spans
//   - Errors → span status codes
package otel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
	"gitea.stump.rocks/stump.wtf/agent-trace/tail"
)

// SpanKind mirrors OTel span kinds without importing the full SDK.
type SpanKind int

const (
	SpanKindInternal SpanKind = 0
	SpanKindServer   SpanKind = 1
	SpanKindClient   SpanKind = 2
)

// StatusCode mirrors OTel status codes.
type StatusCode int

const (
	StatusUnset StatusCode = 0
	StatusOK    StatusCode = 1
	StatusError StatusCode = 2
)

// Span is a lightweight OTel-compatible span structure. It carries enough
// information for any OTel exporter to serialize without depending on the
// go.opentelemetry.io/otel SDK at compile time.
type Span struct {
	TraceID      string            // 32-char hex
	SpanID       string            // 16-char hex
	ParentSpanID string            // empty for root spans
	Name         string
	Kind         SpanKind
	StartTime    time.Time
	EndTime      time.Time
	Attributes   map[string]string
	Status       StatusCode
	StatusMsg    string
	Events       []SpanEvent
}

// SpanEvent is a timestamped annotation within a span.
type SpanEvent struct {
	Name       string
	Timestamp  time.Time
	Attributes map[string]string
}

// Trace is a collection of spans forming one agent session's trace.
type Trace struct {
	TraceID string
	Session tail.SessionMeta
	Spans   []Span
}

// BuildTrace converts a session's classified events and marks into an OTel
// trace. Each user message starts a new parent span; tool calls become child
// spans under the most recent parent. Events without a preceding user message
// are grouped under a synthetic "session-start" root span.
func BuildTrace(session tail.SessionMeta, events []classify.Event, marks []classify.Mark) Trace {
	traceID := deriveTraceID(session.Key)
	trace := Trace{
		TraceID: traceID,
		Session: session,
	}

	// Merge events and marks into a single timeline ordered by sequence.
	timeline := make([]timelineEntry, 0, len(events)+len(marks))
	for _, m := range marks {
		timeline = append(timeline, timelineEntry{seq: m.Seq, isMark: true, mark: m})
	}
	for _, e := range events {
		timeline = append(timeline, timelineEntry{seq: e.Seq, isMark: false, event: e})
	}
	sortTimeline(timeline)

	var currentParent Span
	hasParent := false
	spanCounter := 0

	for _, entry := range timeline {
		if entry.isMark {
			switch entry.mark.Type {
			case "user-message":
				// Start a new parent span for this turn.
				currentParent = Span{
					TraceID:   traceID,
					SpanID:    deriveSpanID(traceID, spanCounter),
					Name:      truncate(entry.mark.Note, 128),
					Kind:      SpanKindInternal,
					StartTime: parseTimestamp(events, entry.seq),
					Attributes: map[string]string{
						"agent.session.id":    session.ID,
						"agent.session.harness": string(session.Harness),
						"agent.turn.type":     "user-message",
					},
					Status: StatusOK,
				}
				if hasParent {
					currentParent.ParentSpanID = "" // top-level turn
				}
				hasParent = true
				spanCounter++
				trace.Spans = append(trace.Spans, currentParent)

			case "compaction":
				span := Span{
					TraceID:   traceID,
					SpanID:    deriveSpanID(traceID, spanCounter),
					Name:      "context-compaction",
					Kind:      SpanKindInternal,
					StartTime: parseTimestamp(events, entry.seq),
					Attributes: map[string]string{
						"agent.session.id": session.ID,
						"agent.event.type": "compaction",
					},
					Status: StatusOK,
				}
				if hasParent {
					span.ParentSpanID = currentParent.SpanID
				}
				spanCounter++
				trace.Spans = append(trace.Spans, span)

			case "subagent":
				span := Span{
					TraceID:   traceID,
					SpanID:    deriveSpanID(traceID, spanCounter),
					Name:      fmt.Sprintf("subagent:%s", entry.mark.Note),
					Kind:      SpanKindClient,
					StartTime: parseTimestamp(events, entry.seq),
					Attributes: map[string]string{
						"agent.session.id":    session.ID,
						"agent.event.type":    "subagent",
						"agent.subagent.name": entry.mark.Note,
					},
					Status: StatusOK,
				}
				if hasParent {
					span.ParentSpanID = currentParent.SpanID
				}
				spanCounter++
				trace.Spans = append(trace.Spans, span)
			}
			continue
		}

		// Tool call event → child span.
		ev := entry.event
		attrs := map[string]string{
			"agent.session.id":   session.ID,
			"agent.tool.name":    ev.Tool,
			"agent.tool.action":  ev.Action,
			"agent.result.bytes": fmt.Sprintf("%d", ev.ResultBytes),
		}
		if len(ev.Targets) > 0 {
			paths := make([]string, 0, len(ev.Targets))
			for _, t := range ev.Targets {
				paths = append(paths, t.Path)
			}
			attrs["agent.targets"] = joinStrings(paths, ",")
		}
		if len(ev.Outside) > 0 {
			attrs["agent.outside_count"] = fmt.Sprintf("%d", len(ev.Outside))
		}

		status := StatusOK
		statusMsg := ""
		if ev.IsError {
			status = StatusError
			statusMsg = ev.Summary
		}

		parentID := ""
		if hasParent {
			parentID = currentParent.SpanID
		}

		span := Span{
			TraceID:      traceID,
			SpanID:       deriveSpanID(traceID, spanCounter),
			ParentSpanID: parentID,
			Name:         ev.Summary,
			Kind:         SpanKindInternal,
			StartTime:    parseEventTimestamp(ev.Timestamp),
			Attributes:   attrs,
			Status:       status,
			StatusMsg:    statusMsg,
		}
		spanCounter++
		trace.Spans = append(trace.Spans, span)
	}

	return trace
}

// deriveTraceID produces a deterministic 32-char hex trace ID from a session key.
func deriveTraceID(sessionKey string) string {
	sum := sha256.Sum256([]byte("trace:" + sessionKey))
	return hex.EncodeToString(sum[:16])
}

// deriveSpanID produces a deterministic 16-char hex span ID from trace ID + sequence.
func deriveSpanID(traceID string, seq int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("span:%s:%d", traceID, seq)))
	return hex.EncodeToString(sum[:8])
}

func parseTimestamp(events []classify.Event, seq int) time.Time {
	for _, e := range events {
		if e.Seq == seq && e.Timestamp != "" {
			return parseEventTimestamp(e.Timestamp)
		}
	}
	return time.Now()
}

func parseEventTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Now()
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return time.Now()
		}
	}
	return t
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

func joinStrings(ss []string, sep string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += sep
		}
		result += s
	}
	return result
}

type timelineEntry struct {
	seq    int
	isMark bool
	mark   classify.Mark
	event  classify.Event
}

func sortTimeline(entries []timelineEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].seq < entries[j-1].seq; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}
