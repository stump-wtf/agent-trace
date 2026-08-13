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
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
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
	TraceID      string         `json:"traceId"`
	SpanID       string         `json:"spanId"`
	ParentSpanID string         `json:"parentSpanId,omitempty"`
	Name         string         `json:"name"`
	Kind         SpanKind       `json:"kind"`
	StartTime    time.Time      `json:"startTime"`
	EndTime      time.Time      `json:"endTime"`
	Attributes   map[string]any `json:"attributes,omitempty"`
	Status       StatusCode     `json:"status"`
	StatusMsg    string         `json:"statusMsg,omitempty"`
	Events       []SpanEvent    `json:"events,omitempty"`
}

// SpanEvent is a timestamped annotation within a span.
type SpanEvent struct {
	Name       string         `json:"name"`
	Timestamp  time.Time      `json:"timestamp"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Trace is a collection of spans forming one agent session's trace.
type Trace struct {
	TraceID string           `json:"traceId"`
	Session tail.SessionMeta `json:"session"`
	Spans   []Span           `json:"spans"`
}

// BuildTrace converts a session's classified events and marks into an OTel
// trace. Each user message starts a new parent span; tool calls become child
// spans under the most recent parent. Events without a preceding user message
// are grouped under no parent (orphan top-level).
//
// EndTime is finalized after building: tool spans end at the next timeline
// entry's start (or their own start if last), turn parent spans span from
// their first child's start to their last child's end.
//
// Timestamps are deterministic: missing timestamps fall back to the nearest
// event, then SessionMeta.StartedAt, then the zero time — never time.Now().
func BuildTrace(session tail.SessionMeta, events []classify.Event, marks []classify.Mark) Trace {
	traceID := deriveTraceID(session.Key)
	sessionStart := parseEventTimestamp(session.StartedAt)

	// Merge events and marks into a single timeline ordered by sequence.
	timeline := make([]timelineEntry, 0, len(events)+len(marks))
	for _, m := range marks {
		timeline = append(timeline, timelineEntry{seq: m.Seq, isMark: true, mark: m})
	}
	for _, e := range events {
		timeline = append(timeline, timelineEntry{seq: e.Seq, isMark: false, event: e})
	}
	sort.SliceStable(timeline, func(i, j int) bool {
		if timeline[i].seq != timeline[j].seq {
			return timeline[i].seq < timeline[j].seq
		}
		// Marks before events on equal seq — marks open/close a turn boundary.
		return timeline[i].isMark && !timeline[j].isMark
	})

	// Compute start times for each timeline entry.
	for i := range timeline {
		if timeline[i].isMark {
			timeline[i].startTime = resolveMarkTime(timeline[i].mark, events, sessionStart)
		} else {
			timeline[i].startTime = resolveEventTime(timeline[i].event, sessionStart)
		}
	}

	var spans []Span
	parentIdx := -1 // index into spans
	spanCounter := 0

	for _, entry := range timeline {
		if entry.isMark {
			switch entry.mark.Type {
			case "user-message":
				span := Span{
					TraceID:   traceID,
					SpanID:    deriveSpanID(traceID, spanCounter),
					Name:      truncate(entry.mark.Note, 128),
					Kind:      SpanKindInternal,
					StartTime: entry.startTime,
					Attributes: map[string]any{
						"agent.session.id":      session.ID,
						"agent.session.harness": string(session.Harness),
						"agent.turn.type":       "user-message",
					},
					Status: StatusOK,
				}
				spanCounter++
				parentIdx = len(spans)
				spans = append(spans, span)

			case "compaction":
				span := Span{
					TraceID:   traceID,
					SpanID:    deriveSpanID(traceID, spanCounter),
					Name:      "context-compaction",
					Kind:      SpanKindInternal,
					StartTime: entry.startTime,
					Attributes: map[string]any{
						"agent.session.id": session.ID,
						"agent.event.type": "compaction",
					},
					Status: StatusOK,
				}
				if parentIdx >= 0 && parentIdx < len(spans) {
					span.ParentSpanID = spans[parentIdx].SpanID
				}
				spanCounter++
				spans = append(spans, span)

			case "subagent":
				span := Span{
					TraceID:   traceID,
					SpanID:    deriveSpanID(traceID, spanCounter),
					Name:      fmt.Sprintf("subagent:%s", entry.mark.Note),
					Kind:      SpanKindClient,
					StartTime: entry.startTime,
					Attributes: map[string]any{
						"agent.session.id":    session.ID,
						"agent.event.type":    "subagent",
						"agent.subagent.name": entry.mark.Note,
					},
					Status: StatusOK,
				}
				if parentIdx >= 0 && parentIdx < len(spans) {
					span.ParentSpanID = spans[parentIdx].SpanID
				}
				spanCounter++
				spans = append(spans, span)
			}
			continue
		}

		// Tool call event → child span.
		ev := entry.event
		attrs := map[string]any{
			"agent.session.id":   session.ID,
			"agent.tool.name":    ev.Tool,
			"agent.tool.action":  ev.Action,
			"agent.result.bytes": int64(ev.ResultBytes),
		}
		if len(ev.Targets) > 0 {
			paths := make([]string, 0, len(ev.Targets))
			for _, t := range ev.Targets {
				paths = append(paths, t.Path)
			}
			attrs["agent.targets"] = strings.Join(paths, ",")
		}
		if len(ev.Outside) > 0 {
			attrs["agent.outside_count"] = int64(len(ev.Outside))
		}

		status := StatusOK
		statusMsg := ""
		if ev.IsError {
			status = StatusError
			statusMsg = ev.Summary
		}

		parentID := ""
		if parentIdx >= 0 && parentIdx < len(spans) {
			parentID = spans[parentIdx].SpanID
		}

		span := Span{
			TraceID:      traceID,
			SpanID:       deriveSpanID(traceID, spanCounter),
			ParentSpanID: parentID,
			Name:         ev.Summary,
			Kind:         SpanKindInternal,
			StartTime:    entry.startTime,
			Attributes:   attrs,
			Status:       status,
			StatusMsg:    statusMsg,
		}
		spanCounter++
		spans = append(spans, span)
	}

	// Finalize EndTimes: each span ends at the next timeline entry's start,
	// or its own start if it's the last entry. Turn parent spans span from
	// their first child's start to their last child's end.
	for i := range spans {
		spans[i].EndTime = computeEndTime(spans, timeline, i, sessionStart)
	}

	return Trace{
		TraceID: traceID,
		Session: session,
		Spans:   spans,
	}
}

// computeEndTime determines the end time for span at index spanIdx.
func computeEndTime(spans []Span, timeline []timelineEntry, spanIdx int, sessionStart time.Time) time.Time {
	if spanIdx < 0 || spanIdx >= len(spans) {
		return sessionStart
	}
	span := spans[spanIdx]

	// For turn parent spans, span from first child's start to last child's end.
	// We identify turn parents by the "agent.turn.type" attribute.
	if _, isTurn := span.Attributes["agent.turn.type"]; isTurn {
		var firstChild, lastChild time.Time
		for i := range spans {
			if spans[i].ParentSpanID == span.SpanID {
				if firstChild.IsZero() {
					firstChild = spans[i].StartTime
				}
				lastChild = spans[i].StartTime
			}
		}
		if !lastChild.IsZero() {
			return lastChild
		}
	}

	// For other spans, end at the next timeline entry's start after this span.
	spanStart := span.StartTime
	for _, entry := range timeline {
		if !entry.startTime.IsZero() && entry.startTime.After(spanStart) {
			return entry.startTime
		}
	}

	// Last span: end at sessionStart if it's later, else own start.
	if !sessionStart.IsZero() && sessionStart.After(spanStart) {
		return sessionStart
	}
	return spanStart
}

// deriveTraceID produces a deterministic 32-char hex trace ID from a session key.
func deriveTraceID(sessionKey string) string {
	sum := sha256.Sum256([]byte("trace:" + sessionKey))
	return hex.EncodeToString(sum[:16])
}

// deriveSpanID produces a deterministic 16-char hex span ID from trace ID + sequence.
func deriveSpanID(traceID string, seq int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "span:%s:%d", traceID, seq))
	return hex.EncodeToString(sum[:8])
}

func resolveMarkTime(mark classify.Mark, events []classify.Event, sessionStart time.Time) time.Time {
	if mark.Timestamp != "" {
		return parseEventTimestamp(mark.Timestamp)
	}
	for _, e := range events {
		if e.Seq == mark.Seq && e.Timestamp != "" {
			return parseEventTimestamp(e.Timestamp)
		}
	}
	if !sessionStart.IsZero() {
		return sessionStart
	}
	return time.Time{}
}

func resolveEventTime(event classify.Event, sessionStart time.Time) time.Time {
	if event.Timestamp != "" {
		return parseEventTimestamp(event.Timestamp)
	}
	if !sessionStart.IsZero() {
		return sessionStart
	}
	return time.Time{}
}

func parseEventTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

type timelineEntry struct {
	seq       int
	isMark    bool
	mark      classify.Mark
	event     classify.Event
	startTime time.Time
}

// WriteJSON serializes a Trace as OTLP-compatible JSON to the given writer.
// The output is a JSON object with traceId, session, and spans fields suitable
// for direct submission to an OTel collector's HTTP JSON endpoint or inspection.
func WriteJSON(trace Trace, w io.Writer) error {
	data, err := json.Marshal(trace)
	if err != nil {
		return fmt.Errorf("marshal trace: %w", err)
	}
	_, err = w.Write(data)
	return err
}
