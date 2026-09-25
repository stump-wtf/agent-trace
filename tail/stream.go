package tail

import (
	"context"
	"io"

	"github.com/stump-wtf/agent-trace/classify"
)

// StreamFormat names an agent's structured stdout mode and the argv flags
// that turn it on. Args is the complete set: append it to the agent's own
// arguments as it stands.
type StreamFormat struct {
	Name string   // the agent's name for the format, such as "stream-json"
	Args []string // the flags that select it, such as --output-format stream-json
}

// StreamParser is an optional interface for an adapter whose agent can write
// its run to stdout in a structured format. Ask an adapter for it with a type
// assertion: an adapter that does not implement it has no such format, or one
// nobody has confirmed yet.
//
// A stream is the run as it happens, not a transcript read after the fact, so
// it is read from an io.Reader — typically the agent's stdout pipe — and its
// records reach the handler as they arrive.
type StreamParser interface {
	// StreamFormat reports the format ParseStream reads and how to ask the
	// agent for it.
	StreamFormat() StreamFormat
	// ParseStream reads r until EOF, handing h each event, mark and the
	// session's metadata as the records that produce them arrive, and
	// returns the final metadata and the run's Result.
	//
	// The Result is nil when the stream ends without one: the process was
	// killed, or crashed, before it reported how the run went. Everything
	// read up to that point has still been handed to h, and a half-written
	// last record is skipped. A tool call still open at EOF is handed over
	// last with an empty result, as Parse does for a transcript.
	//
	// ctx is checked between records. A read that blocks on a quiet pipe is
	// not interrupted by it; close the reader to stop one.
	ParseStream(ctx context.Context, r io.Reader, h StreamHandler) (SessionMeta, *StreamResult, error)
}

// StreamHandler receives what ParseStream reads, in stream order. Any field
// may be nil. The functions run on ParseStream's goroutine, and the stream is
// not read further until each returns.
type StreamHandler struct {
	// Meta is called once, when a record first identifies the session. For
	// a stream that opens with its init record, that is before any event.
	Meta func(SessionMeta)
	// Event is called for each classified tool call, once its result is
	// read or the stream proves none will follow.
	Event func(classify.Event)
	// Mark is called for each timeline mark.
	Mark func(classify.Mark)
}

func (h StreamHandler) meta(m SessionMeta) {
	if h.Meta != nil {
		h.Meta(m)
	}
}

func (h StreamHandler) event(e classify.Event) {
	if h.Event != nil {
		h.Event(e)
	}
}

func (h StreamHandler) mark(m classify.Mark) {
	if h.Mark != nil {
		h.Mark(m)
	}
}

// StreamResult is how a run ended, as the agent reported it on its final
// stream record. Fields are copied as the agent sent them, and are not
// reconciled with each other: Claude Code reports a run that failed on an
// expired login as subtype "success" with IsError set.
type StreamResult struct {
	// Subtype is the agent's outcome label: for Claude Code "success",
	// "error_max_turns", "error_during_execution" and the like.
	Subtype string `json:"subtype"`
	// IsError reports that the run failed.
	IsError bool `json:"isError"`
	// Turns is the number of model turns the run took.
	Turns int `json:"turns"`
	// DurationMS is the run's wall-clock time, and APIDurationMS the part
	// of it spent waiting on the model API, both in milliseconds.
	DurationMS    int64 `json:"durationMs"`
	APIDurationMS int64 `json:"apiDurationMs"`
	// CostUSD is the agent's own estimate of what the run cost.
	CostUSD float64 `json:"costUsd"`
	// Usage is the run's total token usage.
	Usage TokenUsage `json:"usage"`
	// StopReason is why the model stopped on its last turn, when reported.
	StopReason string `json:"stopReason,omitempty"`
	// Text is the run's final answer, when it produced one.
	Text string `json:"text,omitempty"`
	// Errors are the error messages an error subtype carries.
	Errors []string `json:"errors,omitempty"`
}

// TokenUsage counts the tokens a run used.
type TokenUsage struct {
	InputTokens              int64 `json:"inputTokens"`
	OutputTokens             int64 `json:"outputTokens"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     int64 `json:"cacheReadInputTokens"`
}
