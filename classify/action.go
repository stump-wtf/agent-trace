package classify

import (
	"os"
	"path/filepath"
)

// ActionFor classifies a tool call into a semantic action based on the tool
// name, its input parameters, and the result content. Shell commands are
// further classified by inspecting the command text (searchCommand,
// readCommand, verifyCommand).
func ActionFor(tool string, input map[string]any, result string) string {
	switch tool {
	case "Read", "read":
		return ActionRead
	case "Write", "Edit", "MultiEdit", "NotebookEdit", "apply_patch", "write", "edit":
		return ActionEdit
	case "Grep", "Glob", "LS", "view_image", "grep", "find", "ls":
		return ActionSearch
	case "Bash", "bash", "exec_command", "write_stdin", "js", "js_repl":
		command := firstString(input, "command", "cmd", "code", "chars", "script", "_raw")
		if VerifyCommand(command) {
			return ActionVerify
		}
		if SearchCommand(command) {
			return ActionSearch
		}
		if ReadCommand(command) {
			return ActionRead
		}
		return ActionExec
	case "exec":
		if len(execPatchPaths(input)) > 0 {
			return ActionEdit
		}
		commands := execCommands(input)
		if len(commands) == 0 || !execHasOnlyStaticCommands(input, len(commands)) {
			return ActionExec
		}
		allVerify, allSearch, allRead := true, true, true
		for _, c := range commands {
			if !VerifyCommand(c.command) {
				allVerify = false
			}
			if !SearchCommand(c.command) {
				allSearch = false
			}
			if !ReadCommand(c.command) {
				allRead = false
			}
		}
		if allVerify {
			return ActionVerify
		}
		if allSearch {
			return ActionSearch
		}
		if allRead {
			return ActionRead
		}
		return ActionExec
	default:
		_ = result
		return ActionOther
	}
}

// TargetsFor extracts repo file targets and out-of-repo touches from a tool
// call. cwd is the session's working directory for resolving relative paths.
func TargetsFor(cwd, tool string, input map[string]any, result string) ([]Target, []OutsideTouch) {
	var targets []Target
	var outside []OutsideTouch
	add := func(path, touch string, weak bool, lines [][2]int, base string) {
		rel, out, ok := normalizePath(cwd, base, path)
		if !ok {
			return
		}
		if out != nil {
			outside = append(outside, *out)
			return
		}
		if weak && !repoPathExists(cwd, rel) {
			return
		}
		for i := range targets {
			if targets[i].Path == rel {
				if RankTouch(touch) > RankTouch(targets[i].Touch) {
					targets[i].Touch = touch
				}
				targets[i].Lines = append(targets[i].Lines, lines...)
				return
			}
		}
		targets = append(targets, Target{Path: rel, Touch: touch, Lines: lines, Weak: weak})
	}

	switch tool {
	case "Read", "read":
		if path, ok := input["file_path"].(string); ok {
			add(path, TouchRead, false, readLines(input), "")
		}
		if path, ok := input["path"].(string); ok {
			add(path, TouchRead, false, readLines(input), "")
		}
	case "Write", "Edit", "MultiEdit", "NotebookEdit", "write", "edit":
		if path, ok := input["file_path"].(string); ok {
			add(path, TouchEdit, false, nil, "")
		}
		if path, ok := input["notebook_path"].(string); ok {
			add(path, TouchEdit, false, nil, "")
		}
		if path, ok := input["path"].(string); ok {
			add(path, TouchEdit, false, nil, "")
		}
	case "Grep", "grep":
		for _, hit := range parsePathHits(result) {
			add(hit.path, TouchHit, false, hit.lines, "")
		}
		if len(targets) == 0 {
			if path, ok := input["path"].(string); ok {
				add(path, TouchHit, true, nil, "")
			}
		}
	case "Glob", "LS", "find", "ls":
		for _, hit := range parsePathHits(result) {
			add(hit.path, TouchHit, false, nil, "")
		}
		if path, ok := input["path"].(string); ok && len(targets) == 0 {
			add(path, TouchHit, true, nil, "")
		}
	case "Bash", "bash":
		command := firstString(input, "command")
		for _, path := range CommandReadPaths(command) {
			add(path, TouchRead, true, nil, "")
		}
		for _, path := range extractCommandPaths(command) {
			add(path, TouchHit, true, nil, "")
		}
		for _, path := range extractPaths(command + "\n" + result) {
			add(path, TouchHit, true, nil, "")
		}
	case "exec_command":
		command := firstString(input, "cmd", "command")
		base := firstString(input, "workdir")
		for _, path := range CommandReadPaths(command) {
			add(path, TouchRead, true, nil, base)
		}
		for _, path := range extractCommandPaths(command) {
			add(path, TouchHit, true, nil, base)
		}
		for _, path := range extractPaths(command + "\n" + result) {
			add(path, TouchHit, true, nil, base)
		}
		for _, hit := range parsePathHits(result) {
			add(hit.path, TouchHit, true, hit.lines, base)
		}
	case "exec":
		for _, c := range execCommands(input) {
			for _, path := range CommandReadPaths(c.command) {
				add(path, TouchRead, true, nil, c.workdir)
			}
			for _, path := range extractCommandPaths(c.command) {
				add(path, TouchHit, true, nil, c.workdir)
			}
			for _, path := range extractPaths(c.command) {
				add(path, TouchHit, true, nil, c.workdir)
			}
		}
		for _, path := range extractPaths(result) {
			add(path, TouchHit, true, nil, "")
		}
		for _, hit := range parsePathHits(result) {
			add(hit.path, TouchHit, true, hit.lines, "")
		}
		for _, path := range execPatchPaths(input) {
			add(path, TouchEdit, false, nil, "")
		}
	case "apply_patch":
		patch := firstString(input, "patch", "input", "_raw")
		for _, path := range parsePatchPaths(patch) {
			add(path, TouchEdit, false, nil, "")
		}
	case "view_image":
		if path := firstString(input, "path"); path != "" {
			add(path, TouchRead, false, nil, "")
		}
	case "js", "js_repl":
		code := firstString(input, "code", "script", "_raw")
		for _, path := range extractPaths(code + "\n" + result) {
			add(path, TouchHit, true, nil, "")
		}
	}
	return targets, outside
}

// BuildEvent constructs a classified Event from a ToolCall + ToolResult pair.
// seq is the event sequence number within the trace; cwd is the session
// working directory for path resolution.
func BuildEvent(seq int, cwd string, call ToolCall, result ToolResult) Event {
	action := ActionFor(call.Name, call.Input, result.Content)
	targets, outside := TargetsFor(cwd, call.Name, call.Input, result.Content)
	if targets == nil {
		targets = []Target{}
	}
	return Event{
		Seq:         seq,
		Timestamp:   call.Timestamp,
		Tool:        call.Name,
		Action:      action,
		Targets:     targets,
		Outside:     outside,
		ResultBytes: len(result.Content),
		IsError:     result.IsError,
		Summary:     SummarizeTool(call.Name, call.Input, targets, outside, result.IsError),
	}
}

func firstString(input map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := input[key].(string); ok {
			return value
		}
	}
	return ""
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	default:
		return 0
	}
}

func readLines(input map[string]any) [][2]int {
	offset := intFromAny(input["offset"])
	limit := intFromAny(input["limit"])
	if offset <= 0 {
		return nil
	}
	if limit <= 0 {
		return [][2]int{{offset, offset}}
	}
	return [][2]int{{offset, offset + limit - 1}}
}

func repoPathExists(cwd, rel string) bool {
	if cwd == "" || rel == "" {
		return false
	}
	abs := filepath.Join(cwd, filepath.FromSlash(rel))
	_, err := os.Stat(abs)
	return err == nil
}
