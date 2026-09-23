package classify

import (
	"encoding/json"
	"testing"
)

// decode is how every adapter produces an input: JSON text unmarshalled into
// a map, so key order in the text is lost before the digest ever sees it.
func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInputDigestKeysOnArguments(t *testing.T) {
	comment := `{"method":"add_comment","owner":"stump.wtf","repo":"harness","index":383,"body":"."}`
	tests := []struct {
		name string
		a, b string
		same bool
	}{
		{"identical", comment, comment, true},
		{"key order in the transcript", comment, `{"body":".","index":383,"repo":"harness","owner":"stump.wtf","method":"add_comment"}`, true},
		{"nested key order", `{"q":{"a":1,"b":[{"x":1,"y":2}]}}`, `{"q":{"b":[{"y":2,"x":1}],"a":1}}`, true},
		{"different body", comment, `{"method":"add_comment","owner":"stump.wtf","repo":"harness","index":383,"body":".."}`, false},
		{"different issue", comment, `{"method":"add_comment","owner":"stump.wtf","repo":"harness","index":384,"body":"."}`, false},
		{"number is not string", `{"index":383}`, `{"index":"383"}`, false},
		{"array order matters", `{"cmd":["a","b"]}`, `{"cmd":["b","a"]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := inputDigest(decode(t, tt.a)), inputDigest(decode(t, tt.b))
			if len(a) != 64 || len(b) != 64 {
				t.Fatalf("digests %q, %q: want 64 hex chars", a, b)
			}
			if (a == b) != tt.same {
				t.Errorf("same = %v, want %v (%s vs %s)", a == b, tt.same, tt.a, tt.b)
			}
		})
	}
}

func TestInputDigestEmptyInput(t *testing.T) {
	if nilD, emptyD := inputDigest(nil), inputDigest(map[string]any{}); nilD != emptyD || nilD == "" {
		t.Fatalf("nil %q and empty %q inputs must share one non-empty digest", nilD, emptyD)
	}
}

func TestInputDigestUnencodable(t *testing.T) {
	if d := inputDigest(map[string]any{"ch": make(chan int)}); d != "" {
		t.Fatalf("unencodable input: digest %q, want empty", d)
	}
}

// TestBuildEventInputDigest is the case that motivated the field: two MCP
// calls the classifier knows nothing about have the same Summary whatever
// their arguments, and only InputDigest tells a repeat from new work.
func TestBuildEventInputDigest(t *testing.T) {
	build := func(body string) Event {
		in := map[string]any{"method": "add_comment", "index": float64(383), "body": body}
		return BuildEvent(0, "/repo", ToolCall{Name: "mcp_gitea_issue_write", Input: in}, ToolResult{Content: `{"id":1}`})
	}
	a, again, other := build("."), build("."), build("a real comment")
	if a.Summary != other.Summary {
		t.Fatalf("precondition: an unknown tool's Summary should ignore its arguments, got %q vs %q", a.Summary, other.Summary)
	}
	if a.InputDigest == "" || a.InputDigest != again.InputDigest {
		t.Errorf("identical calls: digests %q and %q, want equal and non-empty", a.InputDigest, again.InputDigest)
	}
	if a.InputDigest == other.InputDigest {
		t.Errorf("different bodies share digest %q", a.InputDigest)
	}
	if nilOpts := BuildEventWith(nil, 0, "/repo", ToolCall{Name: "x"}, ToolResult{}); nilOpts.InputDigest == "" {
		t.Error("BuildEventWith(nil opts) left InputDigest empty; it is not opt-in")
	}
}
