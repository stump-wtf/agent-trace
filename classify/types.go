package classify

import "time"

// ToolCall represents a normalized tool invocation from any agent harness.
// Adapters in the tail package convert agent-specific JSONL formats into this
// common shape before passing to Classify.
type ToolCall struct {
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name"`
	Input     map[string]any `json:"input"`
	Timestamp string         `json:"ts,omitempty"`
}

// ToolResult represents a normalized tool response.
type ToolResult struct {
	Content string `json:"content"`
	IsError bool   `json:"isError"`
}

// Event is a classified tool interaction: what the tool did (Action), which
// repo files it touched (Targets), any out-of-repo files (Outside), and a
// human-readable one-liner (Summary).
type Event struct {
	Seq         int            `json:"seq"`
	Timestamp   string         `json:"ts,omitempty"`
	Tool        string         `json:"tool"`
	Action      string         `json:"action"`
	Targets     []Target       `json:"targets"`
	Outside     []OutsideTouch `json:"outside,omitempty"`
	ResultBytes int            `json:"resultBytes"`
	IsError     bool           `json:"isError"`
	Summary     string         `json:"summary"`
	// ErrorExcerpt is a bounded, cleaned piece of an errored result's text.
	// It is empty unless Options.ErrorExcerptBytes is positive and IsError is
	// true; see Options.ErrorExcerptBytes for its shape.
	ErrorExcerpt string `json:"errorExcerpt,omitempty"`
	// InputDigest is the hex SHA-256 of the call's input encoded as JSON
	// with sorted keys: equal for two calls with the same arguments, whatever
	// order the transcript stored them in, and different otherwise. It does
	// not include the tool name. See digest.go.
	InputDigest string `json:"inputDigest,omitempty"`
}

// Target is a repo file touched by a tool call, with the deepest interaction
// type (edit > read > hit) and optional line ranges.
type Target struct {
	Path  string   `json:"path"`
	Touch string   `json:"touch"`
	Lines [][2]int `json:"lines,omitempty"`
	Weak  bool     `json:"weak,omitempty"`
}

// OutsideTouch is a file outside the primary repo scope that a tool call
// referenced. Scope is "home", "tmp", or "other".
type OutsideTouch struct {
	Scope string `json:"scope"`
	Path  string `json:"path"`
}

// Mark is a non-tool timeline annotation. Type is one of "user-message",
// "compaction", "subagent", "error" (a failed model call) or "turn-end" (the
// agent finished its turn; Note carries the agent's own reason where it
// records one). Marks carry turn boundaries that Events do not.
type Mark struct {
	Seq       int    `json:"seq"`
	Timestamp string `json:"ts,omitempty"`
	Type      string `json:"type"`
	Note      string `json:"note,omitempty"`
}

// Usage is one usage report a transcript recorded: the tokens a model call
// spent, what it cost where the agent records a cost, and which model and
// provider served it. It is a third item kind beside Event and Mark, and it
// shares their seq space the way a Mark does: Seq is the seq of the next
// event, so a Usage precedes the event with that seq and consumes none.
//
// Everything here is what the transcript says, never an estimate. A field
// the transcript does not record stays at its zero value — an empty Model,
// Provider or RequestID means "not recorded", which a consumer comparing a
// served model against a pin must treat as unknown rather than as a match —
// and CostUSD stays nil unless the agent itself wrote a cost down. Pricing
// belongs to the consumer.
//
// The four token counts are disjoint: InputTokens excludes the prompt tokens
// served from or written to a cache, which CacheRead and CacheWrite carry, so
// their sum is the whole prompt. OutputTokens includes any reasoning tokens.
// A reader whose agent reports cached tokens as a subset of its input total
// (Codex does) subtracts them to keep that true.
type Usage struct {
	Seq int `json:"seq"`
	// At is when the transcript dated the report; zero when it did not.
	At time.Time `json:"at,omitzero"`
	// Model is the model as the transcript names it, and Provider the
	// provider when it names one.
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	// RequestID is the provider's request or response id, when recorded.
	RequestID    string `json:"requestId,omitempty"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	CacheRead    int64  `json:"cacheRead"`
	CacheWrite   int64  `json:"cacheWrite"`
	// CostUSD is the cost the agent recorded, in US dollars; nil when it
	// recorded none. It is never computed here.
	CostUSD *float64 `json:"costUsd,omitempty"`
	// Cumulative reports that the numbers are running totals for the
	// session rather than one call's usage. A consumer differences
	// consecutive cumulative reports to get the usage between them; see the
	// reader that sets it for what the total covers.
	Cumulative bool `json:"cumulative,omitempty"`
}

// Action constants returned by ActionFor.
const (
	ActionSearch = "search"
	ActionRead   = "read"
	ActionEdit   = "edit"
	ActionExec   = "exec"
	ActionVerify = "verify"
	ActionOther  = "other"
)

// Options controls I/O-dependent behavior in an otherwise pure package.
// Pass nil to keep all weak targets and use path heuristics only for
// outside-scope classification. The tail package supplies an Options with
// real os.Stat-backed FileExists and real home/tmp dirs.
type Options struct {
	// FileExists reports whether a repo-relative path exists on disk.
	// nil keeps all weak targets (no filtering).
	FileExists func(cwd, rel string) bool
	// HomeDir overrides the home directory for outside-scope classification.
	// Empty falls back to path heuristics only (/tmp prefix → "tmp", else "other").
	HomeDir string
	// TmpDir overrides the temp directory for outside-scope classification.
	// Empty falls back to "/tmp".
	TmpDir string
	// VerifyPatterns extends the built-in verify command patterns (go test,
	// npm test, etc.). Patterns are matched case-insensitively as substrings.
	// A project using "just test" or "bun test" can add those here so they
	// classify as ActionVerify instead of ActionExec. Empty or whitespace-only
	// entries are ignored — they would match every command.
	VerifyPatterns []string
	// ErrorExcerptBytes, when positive, keeps up to that many bytes of an
	// errored result's text on Event.ErrorExcerpt, so a consumer can match on
	// the literal error an agent hit. Zero (the default) keeps none. The text
	// has terminal escape sequences stripped and surrounding whitespace
	// trimmed, and is never cut inside a UTF-8 rune. Text over the budget
	// keeps its head and tail joined by ErrorExcerptElision, because errors
	// tend to lead and summaries like "FAIL pkg" tend to close. Results with
	// IsError false never carry an excerpt.
	ErrorExcerptBytes int
	// Redact, when set, rewrites the error text before an excerpt is cut
	// from it and stored, so a consumer that supplies one never holds the raw
	// text on an Event. It sees the whole cleaned result rather than the cut
	// excerpt, so a secret straddling the elision point is still whole when
	// it is matched. It is ignored while ErrorExcerptBytes is zero.
	Redact func(string) string
}

// Touch constants used in Target.Touch. Ranked by RankTouch: edit > read > hit.
const (
	TouchEdit = "edit"
	TouchRead = "read"
	TouchHit  = "hit"
)

// RankTouch returns a numeric rank for touch types so callers can keep the
// deepest interaction when merging targets for the same file.
func RankTouch(touch string) int {
	switch touch {
	case TouchEdit:
		return 3
	case TouchRead:
		return 2
	case TouchHit:
		return 1
	default:
		return 0
	}
}
