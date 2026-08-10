// Package tail discovers and tails live agent session logs, emitting
// normalized ToolCall/ToolResult pairs for classification. It supports
// multiple agent harnesses (Claude Code, Codex, Pi) via per-adapter parsers
// and provides idle detection based on event activity.
package tail

import (
	"time"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
)

// parseSessionTimeOk parses an RFC 3339 / RFC 3339Nano timestamp and returns
// the parsed time plus true on success, or zero plus false when the string is
// empty or unparseable. It is the single timestamp parser in this package —
// SessionMeta.Started/.Ended surface the ok flag so callers can distinguish a
// missing timestamp from a genuine zero-time value, and parseSessionTime wraps
// it for the watcher, which guards on IsZero instead.
//
// Every adapter normalizes to RFC 3339 before the value reaches SessionMeta
// (the SQLite-backed ones via msToRFC3339), so one parser covers all of them.
func parseSessionTimeOk(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return time.Time{}, false
		}
	}
	return t, true
}

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
	// AgentID is the sub-session agent identifier the transcript records, when
	// it has one. Claude Code only; other adapters leave it empty.
	AgentID string `json:"agentId,omitempty"`
	// IsSidechain reports that this session is a subagent branch of another
	// session rather than a top-level session. Claude Code only.
	//
	// NOTE: a sidechain session also sets Auxiliary, and ListSessions skips
	// auxiliary sessions — so IsSidechain is effectively always false on
	// metadata obtained from ListSessions. It is observable through Parse and
	// Summarize against a specific path. Enumerating sidechains would need a
	// listing that opts into auxiliary sessions, which does not exist yet.
	IsSidechain bool `json:"isSidechain,omitempty"`
	// Auxiliary marks a session ListSessions omits from its results: a Claude
	// Code sidechain, or each other adapter's own equivalent. Not serialized.
	Auxiliary bool `json:"-"`
}

// Started parses StartedAt into a time.Time. The boolean is false when the
// field is missing or unparseable, so callers can distinguish "unknown" from
// the zero time and avoid treating a missing timestamp as "1 January year 1".
func (m SessionMeta) Started() (time.Time, bool) {
	return parseSessionTimeOk(m.StartedAt)
}

// Ended parses EndedAt into a time.Time. The boolean is false when the field
// is missing or unparseable, so callers can distinguish "unknown" from the
// zero time.
func (m SessionMeta) Ended() (time.Time, bool) {
	return parseSessionTimeOk(m.EndedAt)
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

// SessionFilter narrows the sessions returned by ListSessionsFiltered.
// The zero value matches every session — existing callers (including the
// watcher) are unaffected.
//
// Filtering is exact, not a hint: every session that does not satisfy all
// non-zero fields is excluded from the result. An adapter that implements
// FilteredLister may push the predicate down to its storage layer for
// efficiency, but the observable result is identical to in-memory filtering.
type SessionFilter struct {
	// Cwd filters sessions whose working directory matches. When non-empty,
	// a session passes if its Cwd equals this value exactly or starts with
	// this value followed by a path separator (prefix match on path
	// components, not a raw string prefix).
	Cwd string
	// Since filters sessions started at or after this instant. A session
	// with a missing or unparseable StartedAt timestamp does not pass this
	// criterion — the caller asked for a time bound and "unknown" is not
	// within it.
	Since time.Time
}

// FilteredLister is an optional interface an adapter may implement to push
// a SessionFilter into its storage layer instead of accepting in-memory
// filtering. The result must be identical to what ListSessions followed by
// in-memory filtering would produce.
type FilteredLister interface {
	ListSessionsFiltered(SessionFilter) ([]SessionMeta, error)
}

// ListSessionsFiltered returns sessions from the adapter that satisfy the
// given filter. If the adapter implements FilteredLister, that method is
// used directly; otherwise the result of ListSessions is filtered in memory.
//
// The zero-value filter matches every session, making this equivalent to
// ListSessions. Filtering is exact — see the SessionFilter docs.
func ListSessionsFiltered(a Adapter, f SessionFilter) ([]SessionMeta, error) {
	if fl, ok := a.(FilteredLister); ok {
		return fl.ListSessionsFiltered(f)
	}
	sessions, err := a.ListSessions()
	if err != nil {
		return nil, err
	}
	return filterSessions(sessions, f), nil
}

// filterSessions applies f to sessions in memory. See SessionFilter for the
// matching rules.
func filterSessions(sessions []SessionMeta, f SessionFilter) []SessionMeta {
	if f == (SessionFilter{}) {
		return sessions
	}
	out := make([]SessionMeta, 0, len(sessions))
	for _, s := range sessions {
		if f.Cwd != "" && !cwdMatches(s.Cwd, f.Cwd) {
			continue
		}
		if !f.Since.IsZero() {
			started := parseSessionTime(s.StartedAt)
			if started.IsZero() || started.Before(f.Since) {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// cwdMatches reports whether sessionCwd equals or is a subdirectory of
// filterCwd. It compares path components, not raw string prefixes, so
// "/home/joe" does not match sessions in "/home/joey".
func cwdMatches(sessionCwd, filterCwd string) bool {
	if filterCwd == "" {
		return false
	}
	if sessionCwd == filterCwd {
		return true
	}
	if len(sessionCwd) <= len(filterCwd) {
		return false
	}
	return sessionCwd[len(filterCwd)] == '/' &&
		sessionCwd[:len(filterCwd)] == filterCwd
}
