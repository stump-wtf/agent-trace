package classify

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const toolSummaryVerbLimit = 96

// SummarizeTool produces a human-readable one-liner for a tool call, suitable
// for display in trace viewers and dashboards.
func SummarizeTool(tool string, input map[string]any, targets []Target, outside []OutsideTouch, isError bool) string {
	verb := tool
	if desc, ok := input["description"].(string); ok && desc != "" {
		verb = desc
	}
	if command := firstString(input, "command", "cmd"); command != "" {
		verb = truncateRunes(command, toolSummaryVerbLimit, "...")
	} else if tool == "exec" {
		if summary := summarizeExecWrapper(input); summary != "" {
			verb = summary
		}
	}
	status := ""
	if isError {
		status = " error"
	}
	return fmt.Sprintf("%s -> %d targets, %d outside%s", verb, len(targets), len(outside), status)
}

// ContentToString coerces an agent tool result's content field to a plain
// string. Handles nil, string, []any (text/content blocks), and falls back to
// JSON marshaling for anything else.
func ContentToString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
				if text, ok := m["content"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

var envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

type execCommand struct {
	command string
	workdir string
}

var execStringFieldRe = regexp.MustCompile(`(?:^|[[:space:],{])(?:"(cmd|workdir)"|(cmd|workdir))\s*:\s*("(?:\\.|[^"\\])*")`)
var execPatchAssignmentRe = regexp.MustCompile(`(?m)^[\t ]*(?:const|let|var)[\t ]+patch[\t ]*=[\t ]*("(?:\\.|[^"\\])*")[\t ]*;`)

func execSource(input map[string]any) string {
	for _, key := range []string{"_raw", "code", "script"} {
		if candidate, ok := input[key].(string); ok && candidate != "" {
			return candidate
		}
	}
	return ""
}

func execHasOnlyStaticCommands(input map[string]any, commandCount int) bool {
	source := execSource(input)
	if source == "" {
		return firstString(input, "cmd", "command") != ""
	}
	tools := execToolNames(source)
	if len(tools) != commandCount {
		return false
	}
	for _, tool := range tools {
		if tool != "exec_command" {
			return false
		}
	}
	return true
}

func execCommands(input map[string]any) []execCommand {
	source := execSource(input)
	if source == "" {
		if command := firstString(input, "cmd", "command"); command != "" {
			return []execCommand{{command: command, workdir: firstString(input, "workdir")}}
		}
		return nil
	}
	arguments := execToolArguments(source, "exec_command")
	commands := make([]execCommand, 0, len(arguments))
	for _, argument := range arguments {
		if c, ok := parseStaticExecCommand(argument); ok {
			commands = append(commands, c)
		}
	}
	return commands
}

func execPatchPaths(input map[string]any) []string {
	source := execSource(input)
	if source == "" {
		return nil
	}
	match := execPatchAssignmentRe.FindStringSubmatch(source)
	if len(match) != 2 {
		return nil
	}
	var patch string
	if json.Unmarshal([]byte(match[1]), &patch) != nil {
		return nil
	}
	for _, argument := range execToolArguments(source, "apply_patch") {
		if strings.TrimSpace(argument) == "patch" {
			return parsePatchPaths(patch)
		}
	}
	return nil
}

func parseStaticExecCommand(argument string) (execCommand, bool) {
	var c execCommand
	ambiguousWorkdir := false
	for _, match := range execStringFieldRe.FindAllStringSubmatchIndex(argument, -1) {
		keyStart, keyEnd := match[2], match[3]
		if keyStart < 0 {
			keyStart, keyEnd = match[4], match[5]
		}
		key := argument[keyStart:keyEnd]
		var value string
		if err := json.Unmarshal([]byte(argument[match[6]:match[7]]), &value); err != nil {
			continue
		}
		if key == "cmd" {
			if c.command != "" {
				return execCommand{}, false
			}
			c.command = value
			continue
		}
		if c.workdir != "" {
			ambiguousWorkdir = true
			c.workdir = ""
			continue
		}
		if !ambiguousWorkdir {
			c.workdir = value
		}
	}
	return c, c.command != ""
}

func execToolArguments(source, tool string) []string {
	call := "tools." + tool
	var arguments []string
	for i := 0; i < len(source); {
		if next, ok := skipJSIgnored(source, i); ok {
			i = next
			continue
		}
		if !strings.HasPrefix(source[i:], call) || (i > 0 && isJSIdentifierByte(source[i-1])) {
			i++
			continue
		}
		open := i + len(call)
		for open < len(source) && isJSSpace(source[open]) {
			open++
		}
		if open >= len(source) || source[open] != '(' {
			i++
			continue
		}
		close, ok := matchingJSParen(source, open)
		if !ok {
			break
		}
		arguments = append(arguments, source[open+1:close])
		i = close + 1
	}
	return arguments
}

func execToolNames(source string) []string {
	const prefix = "tools."
	var names []string
	for i := 0; i < len(source); {
		if next, ok := skipJSIgnored(source, i); ok {
			i = next
			continue
		}
		if !strings.HasPrefix(source[i:], prefix) || (i > 0 && isJSIdentifierByte(source[i-1])) {
			i++
			continue
		}
		nameStart := i + len(prefix)
		nameEnd := nameStart
		for nameEnd < len(source) && isJSIdentifierByte(source[nameEnd]) {
			nameEnd++
		}
		open := nameEnd
		for open < len(source) && isJSSpace(source[open]) {
			open++
		}
		if nameEnd == nameStart || open >= len(source) || source[open] != '(' {
			i++
			continue
		}
		names = append(names, source[nameStart:nameEnd])
		i = open + 1
	}
	return names
}

func summarizeExecWrapper(input map[string]any) string {
	commands := execCommands(input)
	source := execSource(input)
	nestedTools := execToolNames(source)
	if len(commands) == 0 && len(nestedTools) == 0 {
		return ""
	}
	primary := ""
	if len(commands) > 0 {
		primary = commands[0].command
	} else {
		primary = nestedTools[0]
	}
	additionalCalls := len(commands) - 1
	if len(nestedTools) > 0 {
		additionalCalls = len(nestedTools) - 1
	}
	suffix := ""
	if additionalCalls == 1 {
		suffix = " (+1 more tool call)"
	} else if additionalCalls > 1 {
		suffix = fmt.Sprintf(" (+%d more tool calls)", additionalCalls)
	}
	commandLimit := toolSummaryVerbLimit - len([]rune(suffix))
	return truncateRunes(primary, commandLimit, "...") + suffix
}

func matchingJSParen(source string, open int) (int, bool) {
	depth := 1
	for i := open + 1; i < len(source); {
		if next, ok := skipJSIgnored(source, i); ok {
			i = next
			continue
		}
		switch source[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
		i++
	}
	return 0, false
}

func skipJSIgnored(source string, start int) (int, bool) {
	if start >= len(source) {
		return start, false
	}
	if quote := source[start]; quote == '\'' || quote == '"' || quote == '`' {
		for i := start + 1; i < len(source); i++ {
			if source[i] == '\\' {
				i++
				continue
			}
			if source[i] == quote {
				return i + 1, true
			}
		}
		return len(source), true
	}
	if source[start] != '/' || start+1 >= len(source) {
		return start, false
	}
	switch source[start+1] {
	case '/':
		if end := strings.IndexByte(source[start+2:], '\n'); end >= 0 {
			return start + 2 + end + 1, true
		}
		return len(source), true
	case '*':
		if end := strings.Index(source[start+2:], "*/"); end >= 0 {
			return start + 2 + end + 2, true
		}
		return len(source), true
	default:
		return start, false
	}
}

func isJSSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

func isJSIdentifierByte(b byte) bool {
	return b == '_' || b == '$' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func truncateRunes(s string, max int, suffix string) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	suffixRunes := []rune(suffix)
	cut := max - len(suffixRunes)
	if cut < 0 {
		cut = 0
	}
	return string(runes[:cut]) + suffix
}
