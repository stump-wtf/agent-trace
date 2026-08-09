package classify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, root, path string) {
	t.Helper()
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsonString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func osOpts(root string) *Options {
	return &Options{
		FileExists: func(cwd, rel string) bool {
			_, err := os.Stat(filepath.Join(cwd, filepath.FromSlash(rel)))
			return err == nil
		},
	}
}

func buildExecEvent(cwd string, input map[string]any) Event {
	return BuildEventWith(osOpts(cwd), 0, cwd, ToolCall{Name: "exec", Input: input}, ToolResult{})
}

func TestTargetsForRead(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "src/main.go")
	abs := filepath.Join(root, "src/main.go")

	tests := []struct {
		name   string
		tool   string
		input  map[string]any
		action string
		path   string
		touch  string
		weak   bool
	}{
		{"read with offset/limit", "read", map[string]any{"path": abs, "offset": float64(10), "limit": float64(5)}, "read", "src/main.go", "read", false},
		{"edit with edits", "edit", map[string]any{"path": abs, "edits": []any{map[string]any{"oldText": "a", "newText": "b"}}}, "edit", "src/main.go", "edit", false},
		{"write with content", "write", map[string]any{"path": abs, "content": "x"}, "edit", "src/main.go", "edit", false},
		{"grep with path", "grep", map[string]any{"pattern": "TODO", "path": abs}, "search", "src/main.go", "hit", true},
		{"find with pattern", "find", map[string]any{"pattern": "*.go", "path": abs}, "search", "src/main.go", "hit", true},
		{"ls with path", "ls", map[string]any{"path": abs}, "search", "src/main.go", "hit", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := BuildEventWith(osOpts(root), 0, root, ToolCall{Name: tt.tool, Input: tt.input}, ToolResult{})
			if ev.Action != tt.action {
				t.Fatalf("action = %q, want %q", ev.Action, tt.action)
			}
			if tt.path == "" {
				return
			}
			if len(ev.Targets) != 1 {
				t.Fatalf("targets = %#v", ev.Targets)
			}
			target := ev.Targets[0]
			if target.Path != tt.path || target.Touch != tt.touch || target.Weak != tt.weak {
				t.Fatalf("target = %#v, want path=%q touch=%q weak=%v", target, tt.path, tt.touch, tt.weak)
			}
		})
	}
}

func TestReadLineRange(t *testing.T) {
	root := t.TempDir()
	input := map[string]any{"path": "/abs/elsewhere.go", "offset": float64(10), "limit": float64(5)}
	ev := BuildEventWith(osOpts(root), 0, root, ToolCall{Name: "read", Input: input}, ToolResult{})
	if ev.Action != ActionRead {
		t.Fatalf("action = %q", ev.Action)
	}
	if len(ev.Outside) != 1 {
		t.Fatalf("outside = %#v", ev.Outside)
	}
}

func TestExecAggregatedFindsSingleCommandTarget(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md")
	source := `const r = await tools.exec_command({cmd:"sed -n '1,20p' README.md",workdir:` + jsonString(t, root) + `});`
	ev := buildExecEvent(root, map[string]any{"_raw": source})
	if ev.Tool != "exec" || ev.Action != ActionRead {
		t.Fatalf("event = %#v", ev)
	}
	if len(ev.Targets) != 1 || ev.Targets[0].Path != "README.md" || !ev.Targets[0].Weak || ev.Targets[0].Touch != TouchRead {
		t.Fatalf("targets = %#v", ev.Targets)
	}
}

func TestExecExtractsApplyPatch(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "src/main.go")
	patch := "*** Begin Patch\n*** Update File: src/main.go\n@@\n-old\n+new\n*** End Patch"
	source := `const patch = ` + jsonString(t, patch) + `; text(await tools.apply_patch(patch));`
	ev := buildExecEvent(root, map[string]any{"_raw": source})
	if ev.Tool != "exec" || ev.Action != ActionEdit {
		t.Fatalf("event = %#v", ev)
	}
	if len(ev.Targets) != 1 || ev.Targets[0].Path != "src/main.go" || ev.Targets[0].Touch != TouchEdit || ev.Targets[0].Weak {
		t.Fatalf("targets = %#v", ev.Targets)
	}
}

func TestExecPromiseAllCommandTargets(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "first/main.go")
	writeTestFile(t, root, "second/main.go")
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	source := `const rs = await Promise.all([
  tools.exec_command({cmd:"sed -n '1,20p' main.go",workdir:` + jsonString(t, first) + `}),
  tools.exec_command({"cmd":"rg TODO main.go","workdir":` + jsonString(t, second) + `})
]);`
	ev := buildExecEvent(root, map[string]any{"code": source})
	if ev.Tool != "exec" || ev.Action != ActionExec {
		t.Fatalf("event = %#v", ev)
	}
	if len(ev.Targets) != 2 {
		t.Fatalf("targets = %#v", ev.Targets)
	}
	want := []struct {
		path  string
		touch string
	}{{"first/main.go", "read"}, {"second/main.go", "hit"}}
	for i, target := range ev.Targets {
		if target.Path != want[i].path || target.Touch != want[i].touch || !target.Weak {
			t.Fatalf("target %d = %#v", i, target)
		}
	}
}

func TestExecDecodesEscapedStrings(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, `quoted"dir`)
	writeTestFile(t, workdir, "src/main.go")
	command := `sed -n "1,20p" src/main.go`
	source := `tools.exec_command({cmd:` + jsonString(t, command) + `,workdir:` + jsonString(t, workdir) + `});`
	ev := buildExecEvent(root, map[string]any{"script": source})
	if len(ev.Targets) != 1 || ev.Targets[0].Path != `quoted"dir/src/main.go` || !ev.Targets[0].Weak {
		t.Fatalf("targets = %#v", ev.Targets)
	}
}

func TestExecAcceptsWorkdirBeforeCommand(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "sub")
	writeTestFile(t, workdir, "main.go")
	source := `tools.exec_command({workdir:` + jsonString(t, workdir) + `,cmd:"sed main.go"});`
	ev := buildExecEvent(root, map[string]any{"_raw": source})
	if len(ev.Targets) != 1 || ev.Targets[0].Path != "sub/main.go" {
		t.Fatalf("targets = %#v", ev.Targets)
	}
}

func TestExecActionRequiresEveryCommandToVerify(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{"all verification", `Promise.all([tools.exec_command({cmd:"go test ./..."}), tools.exec_command({cmd:"make test"})])`, "verify"},
		{"verification and ordinary", `Promise.all([tools.exec_command({cmd:"go test ./..."}), tools.exec_command({cmd:"sed -n '1,20p' README.md"})])`, "exec"},
		{"verification and another tool", `Promise.all([tools.exec_command({cmd:"go test ./..."}), tools.apply_patch("*** Begin Patch")])`, "exec"},
		{"dynamic command", `tools.exec_command({cmd,workdir:"/tmp"})`, "exec"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := buildExecEvent(t.TempDir(), map[string]any{"_raw": tt.source})
			if ev.Tool != "exec" || ev.Action != tt.want {
				t.Fatalf("event = %#v, want action %q", ev, tt.want)
			}
		})
	}
}

func TestExecIgnoresStringsAndComments(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md")
	source := "const quoted = 'tools.exec_command({cmd:\"go test ./...\"})';\n" +
		"const template = `tools.exec_command({cmd:\"sed README.md\"})`;\n" +
		"// tools.exec_command({cmd:\"sed README.md\"})\n" +
		"/* tools.exec_command({cmd:\"sed README.md\"}) */\n" +
		"text(quoted + template);"
	ev := buildExecEvent(root, map[string]any{"_raw": source})
	if ev.Action != ActionExec || len(ev.Targets) != 0 {
		t.Fatalf("event = %#v", ev)
	}
	if ev.Summary != "exec -> 0 targets, 0 outside" {
		t.Fatalf("summary = %q", ev.Summary)
	}
}

func TestExecDoesNotPairDistantWorkdir(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "README.md")
	source := `tools.exec_command({cmd:"sed README.md"}); const metadata = {workdir:"/tmp/not-the-command-workdir"};`
	ev := buildExecEvent(root, map[string]any{"_raw": source})
	if len(ev.Targets) != 1 || ev.Targets[0].Path != "README.md" || !ev.Targets[0].Weak {
		t.Fatalf("targets = %#v", ev.Targets)
	}
	if len(ev.Outside) != 0 {
		t.Fatalf("outside = %#v", ev.Outside)
	}
}

func TestJSReplTargets(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "packages/db/src/index.ts")
	code := `const db = await import("./packages/db/src/index.ts")`
	for _, key := range []string{"code", "_raw"} {
		t.Run(key, func(t *testing.T) {
			ev := BuildEventWith(osOpts(root), 0, root, ToolCall{Name: "js_repl", Input: map[string]any{key: code}}, ToolResult{})
			if ev.Tool != "js_repl" || ev.Action != ActionExec {
				t.Fatalf("event = %#v", ev)
			}
			if len(ev.Targets) != 1 || ev.Targets[0].Path != "packages/db/src/index.ts" || !ev.Targets[0].Weak {
				t.Fatalf("targets = %#v", ev.Targets)
			}
		})
	}
}

func TestBashSearchCommands(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{`grep -rn "Pair with AI" src --include="*.ts" | head -5`, "search"},
		{`find . -name "*.go" | wc -l`, "search"},
		{`cd /repo && rg TODO internal`, "search"},
		{`git grep -n hook`, "search"},
		{`FOO=1 grep -c x file.txt 2>/dev/null`, "search"},
		{`ls web/src`, "search"},
		{`grep x file > out.txt`, "exec"},
		{`find . -name "*.tmp" -delete`, "exec"},
		{`grep x file && rm file`, "exec"},
		{`npm install 2>&1 | tail -3`, "exec"},
		{`echo done`, "exec"},
		{`go test ./... | tail -5`, "verify"},
		{`cat internal/model/stats.go`, "read"},
		{`sed -n '1,240p' web/src/ui/Hud.tsx`, "read"},
		{`nl -ba main.go | head -50`, "read"},
		{`head -n 20 Makefile`, "read"},
		{`sed -i '' 's/a/b/g' main.go`, "exec"},
		{`cat notes.md > backup.md`, "exec"},
		{`cat main.go && rm main.go`, "exec"},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			ev := BuildEventWith(osOpts(t.TempDir()), 0, t.TempDir(), ToolCall{Name: "Bash", Input: map[string]any{"command": tt.command}}, ToolResult{})
			if ev.Action != tt.want {
				t.Fatalf("action(%q) = %q, want %q", tt.command, ev.Action, tt.want)
			}
		})
	}
}

func TestSummarizeToolTruncatesCommand(t *testing.T) {
	command := strings.Repeat("a", 92) + "界tail"
	summary := SummarizeTool("exec", map[string]any{"cmd": command}, nil, nil, false)
	want := strings.Repeat("a", 92) + "界... -> 0 targets, 0 outside"
	if summary != want {
		t.Fatalf("summary = %q, want %q", summary, want)
	}
}

func TestSummarizeToolExtractsExecWrapper(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{"single command", `const r = await tools.exec_command({cmd:"jq '.city | keys' snapshot.json",workdir:"/tmp"}); text(r.output)`, "jq '.city | keys' snapshot.json -> 0 targets, 0 outside"},
		{"multiple tool calls", `const rs = await Promise.all([tools.exec_command({cmd:"go test ./..."}), tools.exec_command({cmd:"go vet ./..."})]); text(rs)`, "go test ./... (+1 more tool call) -> 0 targets, 0 outside"},
		{"dynamic command", `const r = await tools.exec_command({cmd,workdir:"/tmp"}); text(r.output)`, "exec_command -> 0 targets, 0 outside"},
		{"non-command tool", `const p = await tools.update_plan({plan:[]}); text(p)`, "update_plan -> 0 targets, 0 outside"},
		{"mixed calls", `const r = await tools.exec_command({cmd:"go test ./..."}); const p = await tools.update_plan({plan:[]}); text(r.output); text(p)`, "go test ./... (+1 more tool call) -> 0 targets, 0 outside"},
		{"plain orchestration", `text("done")`, "exec -> 0 targets, 0 outside"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SummarizeTool("exec", map[string]any{"_raw": tt.source}, nil, nil, false)
			if got != tt.want {
				t.Fatalf("summary = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCommandReadPaths(t *testing.T) {
	tests := []struct {
		command string
		want    []string
	}{
		{`sed -n '1,240p' internal/adapter/adapter.go`, []string{"internal/adapter/adapter.go"}},
		{`cat a.go b.go`, []string{"a.go", "b.go"}},
		{`head -n 20 Makefile`, []string{"Makefile"}},
		{`tail -f logs/app.log`, []string{"logs/app.log"}},
		{`cat src/main.go | rg TODO`, []string{"src/main.go"}},
		{`sed -i '' 's/x/y/' a.go`, nil},
		{`cat file.go > copy.go`, nil},
		{`grep -rn TODO src`, nil},
		{`cat *.go`, nil},
		{`cat <<EOF > notes.md`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			got := CommandReadPaths(tt.command)
			if len(got) != len(tt.want) {
				t.Fatalf("CommandReadPaths(%q) = %#v, want %#v", tt.command, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("CommandReadPaths(%q) = %#v, want %#v", tt.command, got, tt.want)
				}
			}
		})
	}
}

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		cwd   string
		base  string
		path  string
		ok    bool
		isOut bool
	}{
		{"/repo", "", "src/main.go", true, false},
		{"/repo", "", "/repo/src/main.go", true, false},
		{"/repo", "", "/tmp/file.go", true, true},
		{"/repo", "", "/home/user/file.go", true, true},
		{"/repo", "", "http://example.com", false, false},
		{"/repo", "", "", false, false},
		{"/repo", "", "../escape.go", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			opts := &Options{HomeDir: "/home", TmpDir: "/tmp"}
			rel, out, ok := normalizePath(opts, tt.cwd, tt.base, tt.path)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v (rel=%q out=%v)", ok, tt.ok, rel, out)
			}
			if tt.isOut && out == nil {
				t.Fatalf("expected outside touch, got nil")
			}
			if !tt.isOut && out != nil && ok {
				t.Fatalf("expected in-repo, got outside: %v", out)
			}
		})
	}
}

func TestApplyPatchPaths(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: src/new.go\n@@\n+package src\n*** Update File: src/old.go\n@@\n-old\n+new\n*** Delete File: src/gone.go\n*** End Patch"
	paths := parsePatchPaths(patch)
	want := []string{"src/gone.go", "src/new.go", "src/old.go"}
	if len(paths) != len(want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
	for i, p := range paths {
		if p != want[i] {
			t.Fatalf("path %d = %q, want %q", i, p, want[i])
		}
	}
}

func TestParsePathHits(t *testing.T) {
	text := "src/foo.go:42: func foo() {\nsrc/bar.go:10: var x\nother/baz.go"
	hits := parsePathHits(text)
	if len(hits) == 0 {
		t.Fatal("expected path hits")
	}
	found := map[string]bool{}
	for _, h := range hits {
		found[h.path] = true
	}
	if !found["src/foo.go"] || !found["src/bar.go"] {
		t.Fatalf("missing expected paths in hits: %#v", hits)
	}
}

func TestExtractPaths(t *testing.T) {
	text := "see src/main.go and lib/util.ts for details"
	paths := extractPaths(text)
	if len(paths) != 2 {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestCleanExtractedPath(t *testing.T) {
	tests := []struct {
		path          string
		allowTopLevel bool
		ok            bool
	}{
		{"src/main.go", false, true},
		{"main.go", false, false},
		{"main.go", true, true},
		{"a/src/main.go", false, true},
		{"./src/main.go", false, true},
		{"http://example.com", false, false},
		{"--flag", false, false},
		{"", false, false},
	}
	for _, tt := range tests {
		_, ok := cleanExtractedPath(tt.path, tt.allowTopLevel)
		if ok != tt.ok {
			t.Errorf("cleanExtractedPath(%q, %v) ok = %v, want %v", tt.path, tt.allowTopLevel, ok, tt.ok)
		}
	}
}
