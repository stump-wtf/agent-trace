package classify

import (
	"path/filepath"
	"strings"
)

// SearchCommand reports whether a shell command only inspects the tree the
// way Grep/Glob/LS would: every pipeline segment runs a read-only program and
// at least one segment actually searches or lists. Conservative by design —
// anything unrecognized stays ActionExec.
func SearchCommand(command string) bool {
	cleaned := strings.NewReplacer("2>&1", " ", "2>/dev/null", " ", ">/dev/null", " ", "> /dev/null", " ").Replace(command)
	searched := false
	segments := strings.FieldsFunc(cleaned, func(r rune) bool {
		return r == '|' || r == ';' || r == '&' || r == '\n'
	})
	for _, segment := range segments {
		if strings.ContainsRune(segment, '>') {
			return false
		}
		fields := strings.Fields(segment)
		for len(fields) > 0 && envAssignRe.MatchString(fields[0]) {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		program := strings.ToLower(filepath.Base(fields[0]))
		if program == "git" && len(fields) > 1 && (fields[1] == "grep" || fields[1] == "ls-files") {
			searched = true
			continue
		}
		if searchPrograms[program] {
			if strings.Contains(segment, "-exec") || strings.Contains(segment, "-delete") {
				return false
			}
			searched = true
			continue
		}
		if !readOnlyPrograms[program] {
			return false
		}
	}
	return searched
}

// ReadCommand reports whether a shell command only pages file contents the
// way Read would: every pipeline segment runs a read-only program and at
// least one segment names a file to read.
func ReadCommand(command string) bool {
	if len(CommandReadPaths(command)) == 0 {
		return false
	}
	cleaned := strings.NewReplacer("2>&1", " ", "2>/dev/null", " ", ">/dev/null", " ", "> /dev/null", " ").Replace(command)
	segments := strings.FieldsFunc(cleaned, func(r rune) bool {
		return r == '|' || r == ';' || r == '&' || r == '\n'
	})
	for _, segment := range segments {
		if strings.ContainsRune(segment, '>') {
			return false
		}
		fields := strings.Fields(segment)
		for len(fields) > 0 && envAssignRe.MatchString(fields[0]) {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		program := strings.ToLower(filepath.Base(fields[0]))
		if program == "sed" && !sedReadsOnly(fields[1:]) {
			return false
		}
		if !readOnlyPrograms[program] && !readPrograms[program] {
			return false
		}
	}
	return true
}

// defaultVerifyPatterns are the built-in test/build verification patterns.
var defaultVerifyPatterns = []string{
	"go test", "go vet", "npm test", "npm run build",
	"pnpm test", "pnpm build", "pytest", "make test",
	"cargo test", "swift test",
}

// VerifyCommand reports whether a shell command runs a test/build verification
// step (go test, npm test, pytest, make test, cargo test, etc.).
func VerifyCommand(command string) bool {
	return VerifyCommandWith(nil, command)
}

// VerifyCommandWith is the Options-aware variant. When opts.VerifyPatterns is
// non-empty, those patterns are checked in addition to the defaults.
func VerifyCommandWith(opts *Options, command string) bool {
	c := strings.ToLower(command)
	for _, pattern := range defaultVerifyPatterns {
		if strings.Contains(c, pattern) {
			return true
		}
	}
	if opts != nil {
		for _, pattern := range opts.VerifyPatterns {
			if strings.Contains(c, strings.ToLower(pattern)) {
				return true
			}
		}
	}
	return false
}

// CommandReadPaths returns the file arguments of pager-style shell segments —
// cat/head/tail/nl and the `sed -n '…p' file` idiom — the shell equivalents
// of opening a file to read it.
func CommandReadPaths(command string) []string {
	seen := map[string]bool{}
	var paths []string
	segments := strings.FieldsFunc(command, func(r rune) bool {
		return r == '|' || r == ';' || r == '&' || r == '\n'
	})
	for _, segment := range segments {
		if strings.ContainsRune(segment, '>') {
			continue
		}
		fields := strings.Fields(segment)
		for len(fields) > 0 && envAssignRe.MatchString(fields[0]) {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		program := strings.ToLower(filepath.Base(fields[0]))
		if !readPrograms[program] {
			continue
		}
		args := fields[1:]
		scriptArgs := 0
		if program == "sed" {
			if !sedReadsOnly(args) {
				continue
			}
			scriptArgs = 1
		}
		expectValue := false
		for _, arg := range args {
			if expectValue {
				expectValue = false
				continue
			}
			if strings.HasPrefix(arg, "-") {
				expectValue = flagTakesValue(program, arg)
				continue
			}
			if scriptArgs > 0 {
				scriptArgs--
				continue
			}
			if strings.ContainsAny(arg, "<>*?$`") {
				continue
			}
			path, ok := cleanExtractedPath(arg, true)
			if !ok || seen[path] {
				continue
			}
			seen[path] = true
			paths = append(paths, path)
		}
	}
	sortStrings(paths)
	return paths
}

var searchPrograms = map[string]bool{
	"grep": true, "rg": true, "ag": true, "find": true, "fd": true,
	"ls": true, "tree": true,
}

var readOnlyPrograms = map[string]bool{
	"cd": true, "cat": true, "head": true, "tail": true, "wc": true,
	"sort": true, "uniq": true, "cut": true, "awk": true, "echo": true,
	"which": true, "file": true, "stat": true, "du": true, "pwd": true,
	"dirname": true, "basename": true, "true": true,
}

var readPrograms = map[string]bool{
	"cat": true, "head": true, "tail": true, "nl": true, "sed": true,
}

func sedReadsOnly(args []string) bool {
	hasN := false
	for _, arg := range args {
		if arg == "-n" {
			hasN = true
		}
		if strings.HasPrefix(arg, "-i") {
			return false
		}
	}
	return hasN
}

func flagTakesValue(program, flag string) bool {
	switch program {
	case "head", "tail":
		return flag == "-n" || flag == "-c"
	case "sed":
		return flag == "-e" || flag == "-f"
	}
	return false
}
