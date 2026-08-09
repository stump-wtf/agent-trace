// Package classify provides pure, stateless classification of agent tool calls
// into semantic actions (search, read, edit, exec, verify) and file targets.
//
// Extracted from cosmtrek/mindwalk internal/adapter. All functions are pure —
// no I/O, no filesystem access, no external dependencies beyond the standard
// library. Callers provide normalized ToolCall + ToolResult pairs; this package
// returns classified Events with Action, Targets, Outside touches, and Summary.
package classify
