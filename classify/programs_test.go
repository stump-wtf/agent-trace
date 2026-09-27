package classify

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestProgramsShellCommands(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{"single", "go test ./...", []string{"go"}},
		{"pipeline and list", "cd internal && go test ./... 2>&1 | tee out.log; make lint", []string{"go", "tee", "make"}},
		{"repeats collapse", "go build ./... && go vet ./...", []string{"go"}},
		{"path is reduced to basename", "./bin/harness doctor && /usr/local/bin/golangci-lint run", []string{"harness", "golangci-lint"}},
		{"env assignments", `GOFLAGS=-count=1 CGO_ENABLED=0 go test ./...`, []string{"go"}},
		{"quoted assignment value", `MSG="a b; c" git commit -m "$MSG"`, []string{"git"}},
		{"quoted separators are not splits", `grep -E "a|b;c && d" file.go`, []string{"grep"}},
		{"redirect targets are not commands", "go test > /tmp/out 2>/dev/null < input.txt", []string{"go"}},
		{"fd redirect digits", "make test 2>&1", []string{"make"}},
		{"ampersand redirect", "npm run build &> build.log", []string{"npm"}},
		{"sudo and env unwrap", "sudo -u deploy env FOO=1 systemctl restart harness", []string{"systemctl"}},
		{"timeout unwraps with its duration", "timeout 30s go test -run TestX", []string{"go"}},
		{"xargs unwraps", "find . -name '*.go' | xargs -n 1 -P 4 gofmt -l", []string{"find", "gofmt"}},
		{"bash -lc script", `bash -lc 'cd /repo && cargo test'`, []string{"cargo"}},
		{"bash -c with -o pipefail", `bash -e -o pipefail -c "go vet ./... | tee v"`, []string{"go", "tee"}},
		{"shell running a file is the shell", "bash scripts/release.sh", []string{"bash"}},
		{"heredoc body is not commands", "cat > f.txt <<'EOF'\nrm -rf /\nnot a command\nEOF\ngo build", []string{"cat", "go"}},
		{"stripped heredoc", "python3 - <<-END\n\tprint('x')\n\tEND\nuv sync", []string{"python3", "uv"}},
		{"here-string is data", "wc -l <<< 'rm -rf /'", []string{"wc"}},
		{"builtins and keywords", "set -e; export X=1; if go test; then echo ok; fi", []string{"go"}},
		{"loops", "for f in *.go; do gofmt -l \"$f\"; done", []string{"gofmt"}},
		{"case arms", "case $x in a) go build;; esac", []string{"go"}},
		{"conditional test", "[[ -f go.mod ]] && go mod tidy", []string{"go"}},
		{"subshell", "(cd web && npm ci) && git status", []string{"npm", "git"}},
		{"computed program is left out", `$TOOL --version; "$(which go)" version; go env`, []string{"go"}},
		{"substitution contents are left out", `echo "$(rm -rf x)" && kubectl get pods`, []string{"kubectl"}},
		{"command -v is a lookup", "command -v jq && command go version", []string{"go"}},
		{"comments", "# run rm -rf here\ngo test # and tee", []string{"go"}},
		{"line continuation", "go test \\\n  ./internal/...", []string{"go"}},
		{"backtick substitution", "for p in `ls`; do echo $p; done; ls", []string{"ls"}},
		{"empty", "", nil},
		{"only builtins", "cd /tmp && echo hi", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Programs("Bash", map[string]any{"command": tt.command})
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Programs(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

func TestProgramsToolShapes(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		input map[string]any
		want  []string
	}{
		{"crush bash", "bash", map[string]any{"command": "make build"}, []string{"make"}},
		{"codex exec_command string", "exec_command", map[string]any{"cmd": "rg -n foo"}, []string{"rg"}},
		{"codex argv array", "exec_command", map[string]any{"command": []any{"bash", "-lc", "go test ./..."}}, []string{"go"}},
		{"argv with a quote", "exec_command", map[string]any{"command": []any{"git", "commit", "-m", "it's done"}}, []string{"git"}},
		{"exec wrapper static commands", "exec", map[string]any{"code": `await tools.exec_command({cmd: "go test ./..."}); await tools.exec_command({cmd: "npm run build"});`}, []string{"go", "npm"}},
		{"not a shell tool", "Read", map[string]any{"file_path": "go.mod"}, nil},
		{"mcp tool with a command field", "mcp_run", map[string]any{"command": "go test"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Programs(tt.tool, tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Programs(%s, %v) = %q, want %q", tt.tool, tt.input, got, tt.want)
			}
		})
	}
}

func TestProgramsCapAndDepth(t *testing.T) {
	var cmds []string
	for i := 0; i < 50; i++ {
		cmds = append(cmds, "tool"+strings.Repeat("x", i))
	}
	got := Programs("Bash", map[string]any{"command": strings.Join(cmds, "; ")})
	if len(got) != maxPrograms {
		t.Errorf("got %d programs, want the cap %d", len(got), maxPrograms)
	}

	nested := `sh -c "sh -c 'sh -c \"sh -c go\"'"`
	if got := Programs("Bash", map[string]any{"command": nested}); len(got) != 0 {
		t.Errorf("nesting past maxShellDepth still yielded %q", got)
	}
	shallow := `sh -c "sh -c 'go test'"`
	if got := Programs("Bash", map[string]any{"command": shallow}); !reflect.DeepEqual(got, []string{"go"}) {
		t.Errorf("Programs(%q) = %q, want [go]", shallow, got)
	}
}

func TestBuildEventFillsPrograms(t *testing.T) {
	ev := BuildEvent(1, "/repo", ToolCall{Name: "Bash", Input: map[string]any{"command": "go test ./... | tee out"}}, ToolResult{})
	if !reflect.DeepEqual(ev.Programs, []string{"go", "tee"}) {
		t.Fatalf("Event.Programs = %q, want [go tee]", ev.Programs)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"programs":["go","tee"]`) {
		t.Errorf("JSON %s lacks programs", b)
	}

	read := BuildEvent(2, "/repo", ToolCall{Name: "Read", Input: map[string]any{"file_path": "go.mod"}}, ToolResult{})
	b, _ = json.Marshal(read)
	if strings.Contains(string(b), "programs") {
		t.Errorf("a non-shell event serialized programs: %s", b)
	}
}
