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
	Key       string  `json:"key"`
	ID        string  `json:"id"`
	Harness   Harness `json:"harness"`
	Path      string  `json:"path"`
	Cwd       string  `json:"cwd,omitempty"`
	Model     string  `json:"model,omitempty"`
	Title     string  `json:"title,omitempty"`
	GitBranch string  `json:"gitBranch,omitempty"`
	StartedAt string  `json:"startedAt,omitempty"`
	EndedAt   string  `json:"endedAt,omitempty"`
	Auxiliary bool    `json:"-"`
}

// Event pairs a classified tool interaction with its source session metadata.
type Event struct {
	Session    SessionMeta     `json:"session"`
	Classified classify.Event  `json:"classified"`
	Marks      []classify.Mark `json:"marks,omitempty"`
	ReceivedAt time.Time       `json:"receivedAt"`
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

// WatchConfig controls the watcher's polling behavior.
type WatchConfig struct {
	IdleConfig
	// PollInterval is how often to re-scan session directories.
	// Default is 2 seconds.
	PollInterval time.Duration
}

// DefaultWatchConfig returns sensible defaults for live watching.
func DefaultWatchConfig() WatchConfig {
	return WatchConfig{
		IdleConfig:   DefaultIdleConfig(),
		PollInterval: 2 * time.Second,
	}
}
