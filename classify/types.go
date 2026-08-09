package classify

// ToolCall represents a normalized tool invocation from any agent harness.
// Adapters in the tail package convert agent-specific JSONL formats into this
// common shape before passing to Classify.
type ToolCall struct {
	ID        string
	Name      string
	Input     map[string]any
	Timestamp string
}

// ToolResult represents a normalized tool response.
type ToolResult struct {
	Content string
	IsError bool
}

// Event is a classified tool interaction: what the tool did (Action), which
// repo files it touched (Targets), any out-of-repo files (Outside), and a
// human-readable one-liner (Summary).
type Event struct {
	Seq         int
	Timestamp   string
	Tool        string
	Action      string
	Targets     []Target
	Outside     []OutsideTouch
	ResultBytes int
	IsError     bool
	Summary     string
}

// Target is a repo file touched by a tool call, with the deepest interaction
// type (edit > read > hit) and optional line ranges.
type Target struct {
	Path  string
	Touch string   // "edit", "read", or "hit"
	Lines [][2]int // optional line ranges
	Weak  bool     // inferred from command text rather than explicit tool use
}

// OutsideTouch is a file outside the primary repo scope that a tool call
// referenced. Scope is "home", "tmp", or "other".
type OutsideTouch struct {
	Scope string
	Path  string
}

// Mark is a non-tool timeline annotation: user messages, context compactions,
// subagent launches. Marks carry turn boundaries that Events do not.
type Mark struct {
	Seq  int
	Type string // "user-message", "compaction", "subagent"
	Note string
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
