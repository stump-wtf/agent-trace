package classify

import "testing"

func TestActionFor(t *testing.T) {
	tests := []struct {
		tool   string
		input  map[string]any
		result string
		want   string
	}{
		{"Read", map[string]any{"file_path": "foo.go"}, "", ActionRead},
		{"read", map[string]any{"path": "bar.go"}, "", ActionRead},
		{"Write", map[string]any{"file_path": "foo.go"}, "", ActionEdit},
		{"Edit", map[string]any{"file_path": "foo.go"}, "", ActionEdit},
		{"edit", map[string]any{"path": "foo.go"}, "", ActionEdit},
		{"Grep", map[string]any{"pattern": "foo"}, "", ActionSearch},
		{"grep", map[string]any{"pattern": "foo"}, "", ActionSearch},
		{"Glob", map[string]any{"pattern": "*.go"}, "", ActionSearch},
		{"Bash", map[string]any{"command": "go test ./..."}, "", ActionVerify},
		{"Bash", map[string]any{"command": "grep -r foo ."}, "", ActionSearch},
		{"Bash", map[string]any{"command": "cat foo.go"}, "", ActionRead},
		{"Bash", map[string]any{"command": "echo hello"}, "", ActionExec},
		{"bash", map[string]any{"command": "npm test"}, "", ActionVerify},
		{"bash", map[string]any{"command": "make build"}, "", ActionExec},
		{"shell", map[string]any{"command": "go test ./..."}, "", ActionVerify},
		{"shell", map[string]any{"command": "cat foo.go"}, "", ActionRead},
		{"shell", map[string]any{"command": "find . -name '*.go'"}, "", ActionSearch},
		{"shell", map[string]any{"command": "python3 main.py"}, "", ActionExec},
		{"Shell", map[string]any{"command": "npm test"}, "", ActionVerify},
		{"tree", map[string]any{"path": "src"}, "", ActionSearch},
		{"Tree", map[string]any{"path": "src"}, "", ActionSearch},
		{"read_image", map[string]any{"source": "img.png"}, "", ActionRead},
		{"ReadImage", map[string]any{"path": "diagram.jpg"}, "", ActionRead},
		{"unknown_tool", nil, "", ActionOther},
	}
	for _, tt := range tests {
		got := ActionFor(tt.tool, tt.input, tt.result)
		if got != tt.want {
			t.Errorf("ActionFor(%q) = %q, want %q", tt.tool, got, tt.want)
		}
	}
}

func TestVerifyCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"go test ./...", true},
		{"go vet ./...", true},
		{"npm test", true},
		{"pytest -v", true},
		{"make test", true},
		{"cargo test --release", true},
		{"echo hello", false},
		{"git status", false},
		{"", false},
	}
	for _, tt := range tests {
		got := VerifyCommand(tt.cmd)
		if got != tt.want {
			t.Errorf("VerifyCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

func TestSearchCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"grep -r foo .", true},
		{"rg pattern", true},
		{"find . -name '*.go'", true},
		{"ls -la", true},
		{"cat foo.go", false},
		{"echo hello", false},
		{"go build", false},
		{"", false},
	}
	for _, tt := range tests {
		got := SearchCommand(tt.cmd)
		if got != tt.want {
			t.Errorf("SearchCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

func TestReadCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"cat foo.go", true},
		{"head -n 10 bar.go", true},
		{"tail baz.go", true},
		{"echo hello", false},
		{"grep foo bar.go", false},
		{"", false},
	}
	for _, tt := range tests {
		got := ReadCommand(tt.cmd)
		if got != tt.want {
			t.Errorf("ReadCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
}

func TestRankTouch(t *testing.T) {
	if RankTouch(TouchEdit) <= RankTouch(TouchRead) {
		t.Error("edit should rank higher than read")
	}
	if RankTouch(TouchRead) <= RankTouch(TouchHit) {
		t.Error("read should rank higher than hit")
	}
	if RankTouch("unknown") != 0 {
		t.Error("unknown touch should rank 0")
	}
}

func TestContentToString(t *testing.T) {
	tests := []struct {
		input any
		want  string
	}{
		{nil, ""},
		{"hello", "hello"},
		{[]any{map[string]any{"text": "a"}, map[string]any{"content": "b"}}, "a\nb"},
		{42, "42"},
	}
	for _, tt := range tests {
		got := ContentToString(tt.input)
		if got != tt.want {
			t.Errorf("ContentToString(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTargetsForGooseTools(t *testing.T) {
	cwd := "/workspace/repo"

	// shell tool extracts read paths and command paths
	targets, _ := TargetsFor(cwd, "shell", map[string]any{"command": "cat internal/util.go"}, "")
	if len(targets) == 0 || targets[0].Path != "internal/util.go" {
		t.Fatalf("TargetsFor(shell) targets = %v, want internal/util.go", targets)
	}

	// tree tool extracts path as TouchHit
	targets, _ = TargetsFor(cwd, "tree", map[string]any{"path": "pkg/api"}, "")
	if len(targets) != 1 || targets[0].Path != "pkg/api" || targets[0].Touch != TouchHit {
		t.Fatalf("TargetsFor(tree) targets = %v, want pkg/api (hit)", targets)
	}

	// read_image extracts local file path as TouchRead
	targets, _ = TargetsFor(cwd, "read_image", map[string]any{"source": "docs/architecture.png"}, "")
	if len(targets) != 1 || targets[0].Path != "docs/architecture.png" || targets[0].Touch != TouchRead {
		t.Fatalf("TargetsFor(read_image local) targets = %v, want docs/architecture.png (read)", targets)
	}

	// read_image ignores remote URLs
	targets, outside := TargetsFor(cwd, "read_image", map[string]any{"source": "https://example.com/logo.png"}, "")
	if len(targets) != 0 || len(outside) != 0 {
		t.Fatalf("TargetsFor(read_image remote) returned targets=%v outside=%v, want none", targets, outside)
	}
}
