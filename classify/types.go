package classify

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

// Mark is a non-tool timeline annotation: user messages, context compactions,
// subagent launches. Marks carry turn boundaries that Events do not.
type Mark struct {
	Seq       int    `json:"seq"`
	Timestamp string `json:"ts,omitempty"`
	Type      string `json:"type"`
	Note      string `json:"note,omitempty"`
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
