// Package tail discovers and tails live agent session logs, emitting
// normalized ToolCall/ToolResult pairs for classification. It supports
// multiple agent harnesses (Claude Code, Codex, Pi) via per-adapter parsers
// and provides idle detection based on event activity.
package tail

import (
	"time"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
)

// Harness identifies which agent CLI produced a session log.
type Harness string

const (
	HarnessClaudeCode Harness = "claude-code"
	HarnessCodex      Harness = "codex"
	HarnessPi         Harness = "pi"
)

// SessionMeta is lightweight metadata for a discovered session file.
type SessionMeta struct {
	Key       string // stable hash of harness + path
	ID        string // agent-level session ID
	Harness   Harness
	Path      string // absolute path to the .jsonl file
	Cwd       string // working directory the session ran in
	Model     string // model identifier (if available)
	Title     string // session title or first user message
	StartedAt string // ISO 8601 timestamp
	EndedAt   string // ISO 8601 timestamp
}

// Event pairs a classified tool interaction with its source session metadata.
type Event struct {
	Session    SessionMeta
	Classified classify.Event
	RawCall    classify.ToolCall
	RawResult  classify.ToolResult
	ReceivedAt time.Time // when the tailer observed this event
}

// IdleConfig controls idle detection thresholds.
type IdleConfig struct {
	// IdleAfter is the duration of no events before a session is considered idle.
	IdleAfter time.Duration
}

// DefaultIdleConfig returns sensible defaults for idle detection.
func DefaultIdleConfig() IdleConfig {
	return IdleConfig{
		IdleAfter: 30 * time.Second,
	}
}
