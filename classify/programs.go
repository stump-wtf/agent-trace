package classify

import (
	"regexp"
	"strings"
)

// maxPrograms bounds Event.Programs: a generated script can name hundreds of
// commands, and a consumer keying a node per program needs a ceiling.
const maxPrograms = 32

// maxShellDepth bounds how far `bash -c '…'` nesting is followed.
const maxShellDepth = 3

var programNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// Programs returns the programs a shell tool call runs, as executable
// basenames in first-seen order without repeats: `cd x && go test ./... |
// tee out` runs go and tee. It covers the tools ActionFor treats as a shell
// (Bash, bash, exec_command) and the static commands of an exec wrapper.
//
// The parse is conservative. A program whose name is computed when the
// command runs (`$TOOL`, `$(which go)`) is left out, and so is anything inside
// a command substitution, so the list is a lower bound: an empty result means
// "none that could be named", not "none ran". Heredoc bodies and redirect
// targets are never mistaken for commands. Wrappers that only run the next
// word (sudo, env, timeout, xargs, nohup, nice, exec, command) are replaced by
// what they run, a shell given `-c '…'` is replaced by the programs in its
// script, shell builtins and keywords are left out, and at most 32 programs
// are returned. Other tools return nil.
func Programs(tool string, input map[string]any) []string {
	var scripts []string
	switch tool {
	case "Bash", "bash", "exec_command":
		for _, key := range []string{"command", "cmd"} {
			if s, ok := input[key].(string); ok {
				scripts = append(scripts, s)
				break
			}
			if argv, ok := input[key].([]any); ok {
				scripts = append(scripts, argvScript(argv))
				break
			}
		}
	case "exec":
		for _, c := range execCommands(input) {
			scripts = append(scripts, c.command)
		}
	default:
		return nil
	}
	var p programSet
	for _, script := range scripts {
		p.addScript(script, 0)
	}
	return p.names
}

// argvScript rebuilds a command line from an argv array (Codex's older shell
// call shape), single-quoting every element so no element is re-split.
func argvScript(argv []any) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		s, ok := a.(string)
		if !ok {
			return ""
		}
		parts = append(parts, "'"+strings.ReplaceAll(s, "'", `'\''`)+"'")
	}
	return strings.Join(parts, " ")
}

type programSet struct {
	names []string
	seen  map[string]bool
}

func (p *programSet) add(name string) {
	if len(p.names) >= maxPrograms || p.seen[name] {
		return
	}
	if p.seen == nil {
		p.seen = map[string]bool{}
	}
	p.seen[name] = true
	p.names = append(p.names, name)
}

func (p *programSet) addScript(script string, depth int) {
	for _, cmd := range shellCommands(script) {
		p.addCommand(cmd, depth)
	}
}

// addCommand resolves one simple command's words to the program it runs.
func (p *programSet) addCommand(words []shellWord, depth int) {
	for len(words) > 0 {
		w := words[0]
		if w.dynamic {
			return
		}
		if envAssignRe.MatchString(w.text) {
			words = words[1:]
			continue
		}
		name := w.text
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		switch {
		case shellCommandPrefixes[name]:
			// `if go test; then …`: the keyword prefixes the real command.
			words = words[1:]
			continue
		case shellSkipCommand[name]:
			// `for f in …`, `case`, `function`: the words are not a command.
			return
		case shellBuiltins[name]:
			return
		case name == "command" && len(words) > 1 && (words[1].text == "-v" || words[1].text == "-V"):
			return // a lookup, not a run
		}
		if skip, ok := wrapperArgs(name, words[1:]); ok {
			words = words[1+skip:]
			continue
		}
		if shellInterpreters[name] {
			if script, ok := dashCScript(words[1:]); ok {
				if depth < maxShellDepth {
					p.addScript(script, depth+1)
				}
				return
			}
		}
		if programNameRe.MatchString(name) {
			p.add(name)
		}
		return
	}
}

// wrapperArgs reports whether name only runs the command that follows its own
// options, and how many of args are those options.
func wrapperArgs(name string, args []shellWord) (int, bool) {
	valueFlags, ok := wrapperValueFlags[name]
	if !ok {
		return 0, false
	}
	i := 0
	for i < len(args) {
		a := args[i].text
		if args[i].dynamic {
			return i, true
		}
		if name == "env" && envAssignRe.MatchString(a) {
			i++
			continue
		}
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		i++
		if valueFlags[a] && i < len(args) {
			i++
		}
	}
	if name == "timeout" && i < len(args) {
		i++ // the duration
	}
	return i, true
}

// dashCScript finds the script a shell was given with -c (or a combined flag
// such as -lc or -ec).
func dashCScript(args []shellWord) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a.dynamic || (!strings.HasPrefix(a.text, "-") && !strings.HasPrefix(a.text, "+")) {
			return "", false
		}
		if a.text == "-o" || a.text == "+o" {
			i++ // -o pipefail
			continue
		}
		if a.text != "--" && strings.Contains(a.text[1:], "c") && !strings.HasPrefix(a.text, "--") {
			if i+1 < len(args) && !args[i+1].dynamic {
				return args[i+1].text, true
			}
			return "", false
		}
	}
	return "", false
}

var wrapperValueFlags = map[string]map[string]bool{
	"sudo":    {"-u": true, "-g": true, "-C": true, "-D": true, "-h": true, "-p": true, "-U": true},
	"env":     {"-u": true, "-C": true, "-S": true},
	"nice":    {"-n": true},
	"nohup":   {},
	"exec":    {"-a": true},
	"command": {},
	"timeout": {"-s": true, "-k": true, "--signal": true, "--kill-after": true},
	"xargs": {"-I": true, "-n": true, "-P": true, "-L": true, "-s": true, "-d": true,
		"-E": true, "-a": true, "--max-args": true, "--max-procs": true,
		"--delimiter": true, "--arg-file": true, "--replace": true},
	"stdbuf": {"-i": true, "-o": true, "-e": true},
}

var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
}

// shellCommandPrefixes are keywords followed by a command on the same
// segment.
var shellCommandPrefixes = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "do": true,
	"while": true, "until": true, "!": true, "time": true, "{": true,
	"fi": true, "done": true, "esac": true, "}": true,
}

// shellSkipCommand are keywords whose words name no command.
var shellSkipCommand = map[string]bool{
	"for": true, "case": true, "select": true, "function": true, "in": true,
	"[[": true, "]]": true,
}

// shellBuiltins run inside the shell rather than as a program. echo, printf
// and test are builtins in every shell the agents use, even where a binary of
// the same name exists.
var shellBuiltins = map[string]bool{
	"cd": true, "pushd": true, "popd": true, "export": true, "set": true,
	"unset": true, "source": true, ".": true, ":": true, "true": true,
	"false": true, "exit": true, "return": true, "local": true, "shift": true,
	"alias": true, "unalias": true, "trap": true, "wait": true, "read": true,
	"eval": true, "readonly": true, "declare": true, "typeset": true,
	"break": true, "continue": true, "umask": true, "ulimit": true,
	"hash": true, "type": true, "builtin": true, "test": true, "[": true,
	"echo": true, "printf": true, "jobs": true, "fg": true, "bg": true,
	"disown": true, "let": true, "getopts": true,
}

// shellWord is one word of a simple command after quote removal. dynamic
// marks a word whose text depends on an expansion ($VAR, $(…), `…`).
type shellWord struct {
	text    string
	dynamic bool
}

// shellCommands splits a script into simple commands, each a list of words.
// It honors quotes and backslashes, splits on unquoted | || & && ; newline
// and parentheses, drops redirect operators with their targets, and skips
// heredoc bodies.
func shellCommands(script string) [][]shellWord {
	var (
		cmds     [][]shellWord
		cur      []shellWord
		word     strings.Builder
		inWord   bool
		dynamic  bool
		redirect bool // the next word is a redirect target
		heredocs []heredoc
	)
	endWord := func() {
		if !inWord {
			return
		}
		if redirect {
			redirect = false
		} else {
			cur = append(cur, shellWord{text: word.String(), dynamic: dynamic})
		}
		word.Reset()
		inWord, dynamic = false, false
	}
	endCmd := func() {
		endWord()
		if len(cur) > 0 {
			cmds = append(cmds, cur)
		}
		cur = nil
	}
	rs := []rune(script)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\\' && i+1 < len(rs):
			if rs[i+1] != '\n' {
				word.WriteRune(rs[i+1])
				inWord = true
			}
			i++
		case r == '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			word.WriteString(string(rs[i+1 : min(j, len(rs))]))
			inWord = true
			i = j
		case r == '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '\\' && j+1 < len(rs) {
					word.WriteRune(rs[j+1])
					j += 2
					continue
				}
				if rs[j] == '$' || rs[j] == '`' {
					dynamic = true
				}
				word.WriteRune(rs[j])
				j++
			}
			inWord = true
			i = j
		case r == '$' || r == '`':
			dynamic, inWord = true, true
			i = skipExpansion(rs, i)
		case r == '#' && !inWord:
			for i+1 < len(rs) && rs[i+1] != '\n' {
				i++
			}
		case r == '<' && i+1 < len(rs) && rs[i+1] == '<' && (i+2 >= len(rs) || rs[i+2] != '<'):
			endWord()
			j := i + 2
			strip := j < len(rs) && rs[j] == '-'
			if strip {
				j++
			}
			for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
				j++
			}
			delim, next := heredocDelimiter(rs, j)
			if delim != "" {
				heredocs = append(heredocs, heredoc{delim: delim, strip: strip})
			}
			i = next - 1
		case r == '>' || r == '<':
			if inWord && isDigits(word.String()) {
				word.Reset()
				inWord, dynamic = false, false
			}
			endWord()
			for i+1 < len(rs) && (rs[i+1] == '>' || rs[i+1] == '&' || rs[i+1] == '|' || rs[i+1] == '<') {
				i++
			}
			redirect = true
		case r == '&' && i+1 < len(rs) && rs[i+1] == '>':
			endWord()
			i++
			for i+1 < len(rs) && rs[i+1] == '>' {
				i++
			}
			redirect = true
		case r == '\n':
			endCmd()
			if len(heredocs) > 0 {
				i = skipHeredocs(rs, i+1, heredocs) - 1
				heredocs = nil
			}
		case r == '|' || r == '&' || r == ';' || r == '(' || r == ')':
			endCmd()
		case r == ' ' || r == '\t' || r == '\r':
			endWord()
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	endCmd()
	return cmds
}

type heredoc struct {
	delim string
	strip bool
}

// heredocDelimiter reads a heredoc delimiter word, removing its quotes, and
// returns it with the index after it.
func heredocDelimiter(rs []rune, i int) (string, int) {
	var b strings.Builder
	for i < len(rs) {
		r := rs[i]
		if r == '\'' || r == '"' {
			j := i + 1
			for j < len(rs) && rs[j] != r {
				b.WriteRune(rs[j])
				j++
			}
			i = j + 1
			continue
		}
		if r == '\\' && i+1 < len(rs) {
			b.WriteRune(rs[i+1])
			i += 2
			continue
		}
		if strings.ContainsRune(" \t\n;|&<>()", r) {
			break
		}
		b.WriteRune(r)
		i++
	}
	return b.String(), i
}

// skipHeredocs skips the bodies of the pending heredocs, which start at i,
// and returns the index of the first line after the last terminator.
func skipHeredocs(rs []rune, i int, docs []heredoc) int {
	for _, d := range docs {
		for i < len(rs) {
			end := i
			for end < len(rs) && rs[end] != '\n' {
				end++
			}
			line := string(rs[i:end])
			if d.strip {
				line = strings.TrimLeft(line, "\t")
			}
			i = end + 1
			if line == d.delim {
				break
			}
		}
	}
	return min(i, len(rs))
}

// skipExpansion returns the index of the last rune of the expansion that
// starts at i: $(…), ${…}, `…`, or $NAME.
func skipExpansion(rs []rune, i int) int {
	if rs[i] == '`' {
		j := i + 1
		for j < len(rs) && rs[j] != '`' {
			if rs[j] == '\\' {
				j++
			}
			j++
		}
		return min(j, len(rs)-1)
	}
	if i+1 >= len(rs) {
		return i
	}
	open, close := rs[i+1], rune(0)
	switch open {
	case '(':
		close = ')'
	case '{':
		close = '}'
	default:
		j := i + 1
		for j < len(rs) && (rs[j] == '_' || rs[j] >= 'a' && rs[j] <= 'z' || rs[j] >= 'A' && rs[j] <= 'Z' || rs[j] >= '0' && rs[j] <= '9') {
			j++
		}
		if j == i+1 && j < len(rs) && strings.ContainsRune("?!#@*$-", rs[j]) {
			j++
		}
		return j - 1
	}
	depth := 0
	for j := i + 1; j < len(rs); j++ {
		switch rs[j] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return j
			}
		case '\'':
			for j++; j < len(rs) && rs[j] != '\''; j++ {
			}
		}
	}
	return len(rs) - 1
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
