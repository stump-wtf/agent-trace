// Package classify provides classification of agent tool calls into semantic
// actions (search, read, edit, exec, verify) and file targets.
//
// Extracted from cosmtrek/mindwalk internal/adapter. All functions are pure —
// no I/O, no filesystem access, no external dependencies beyond the standard
// library. Callers provide normalized ToolCall + ToolResult pairs; this package
// returns classified Events with Action, Targets, Outside touches, and Summary.
//
// The FileExists and HomeDir/TmpDir fields on Options let callers that have a
// live filesystem (the tail package) inject I/O for weak-target filtering and
// outside-scope detection. Pass nil Options to keep all weak targets and use
// path heuristics only.
package classify
