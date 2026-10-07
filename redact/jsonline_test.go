package redact

// Governing tests: Harness ADR-0033 "Structured one-shots run on pipes" and
// "Confirmation" (a tool call carrying `Authorization: token abc123` is stored
// masked, and a scan of the stream file finds no abc123); ADR-0008.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// jsonLine masks one line with a fresh Lines.
func jsonLine(ln string) string {
	var l Lines
	return l.JSONLine(ln)
}

// mustParse fails the test when s is not one JSON value.
func mustParse(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("masked line is not JSON (%v):\n%s", err, s)
	}
	return v
}

// The ADR's own confirmation: a Bash tool call whose command carries an
// Authorization header. Masked as text, the rule eats the backslash escaping
// the closing quote and the line stops parsing; JSONLine keeps it JSON.
func TestJSONLineMasksACommandInsideAToolCall(t *testing.T) {
	ln := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"curl -H \"Authorization: token abc123\" https://gitea.example/api","description":"List repos"}}]}}`
	if text := Redact(ln); strings.Contains(text, "abc123") || json.Valid([]byte(text)) {
		t.Logf("text masking: %s", text)
	}
	got := jsonLine(ln)
	if strings.Contains(got, "abc123") {
		t.Fatalf("the token survived:\n%s", got)
	}
	v := mustParse(t, got)
	cmd := v["message"].(map[string]any)["content"].([]any)[0].(map[string]any)["input"].(map[string]any)["command"]
	if want := `curl -H "Authorization: token [REDACTED]" https://gitea.example/api`; cmd != want {
		t.Errorf("command = %q, want %q", cmd, want)
	}
}

// A line with nothing to mask comes back byte for byte, escapes and all: the
// file is the raw stream, not a re-encoding of it.
func TestJSONLineLeavesACleanLineByteIdentical(t *testing.T) {
	for _, ln := range []string{
		`{"type":"system","subtype":"init","session_id":"0199","tools":["Bash","Read"],"model":"claude-opus-5"}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"café \/tmp\nline two\t\"quoted\""}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":0.0123,"usage":{"input_tokens":10,"max_tokens":4096}}`,
		`  [1, 2, "three"]`,
	} {
		if got := jsonLine(ln); got != ln {
			t.Errorf("a clean line changed:\n got %s\nwant %s", got, ln)
		}
	}
}

// A string value under a secret-named key is masked whole, and the line stays
// JSON: the structural form of `password: hunter2`.
func TestJSONLineMasksSecretNamedKeys(t *testing.T) {
	ln := `{"type":"tool_use","input":{"password":"hunter2","GITEA_TOKEN": "tok-9f8e7d","max_tokens":"4096","api_key":"$API_KEY","user":"joe"}}`
	got := jsonLine(ln)
	for _, secret := range []string{"hunter2", "tok-9f8e7d"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q survived:\n%s", secret, got)
		}
	}
	in := mustParse(t, got)["input"].(map[string]any)
	if in["max_tokens"] != "4096" {
		t.Errorf("max_tokens = %v; a plural is a count, not a secret", in["max_tokens"])
	}
	if in["api_key"] != "$API_KEY" {
		t.Errorf("api_key = %v; a reference to a secret is not one", in["api_key"])
	}
	if in["user"] != "joe" {
		t.Errorf("user = %v, want it untouched", in["user"])
	}
}

// A PEM key inside a tool result arrives as one JSON string with escaped
// newlines. Decoded, it is lines again, and Lines masks the body.
func TestJSONLineMasksAKeyBlockInsideAString(t *testing.T) {
	body := strings.Join([]string{pemBeginLine, pemBodyLine, pemBodyLine, pemEndLine}, "\n")
	enc, _ := json.Marshal(map[string]any{"type": "user", "content": "$ cat id_ed25519\n" + body + "\n$ ls"})
	got := jsonLine(string(enc))
	if strings.Contains(got, pemBodyLine) {
		t.Fatalf("key material survived:\n%s", got)
	}
	if c := mustParse(t, got)["content"].(string); !strings.HasPrefix(c, "$ cat id_ed25519\n") || !strings.HasSuffix(c, "\n$ ls") {
		t.Errorf("text around the key changed: %q", c)
	}
}

// A line that is not JSON, or does not scan as JSON, is masked as text rather
// than passed through.
func TestJSONLineFallsBackToText(t *testing.T) {
	for _, ln := range []string{
		`warning: GITEA_TOKEN=abc123 is set`,
		// Assembled at run time, like redact_test.go's fixtures, so no source
		// line holds a whole credential shape for the secret scan to match.
		`{"type":"broken","command":"curl -u joe:` + "abc123" + ` https://x`,
	} {
		if got := jsonLine(ln); strings.Contains(got, "abc123") {
			t.Errorf("%q passed unmasked: %q", ln, got)
		}
	}
}

// A multi-megabyte line, the size a real tool result reaches, is masked whole
// (the secret sits at its far end) and stays JSON. The time is logged, not
// asserted: text masking of the same line takes seconds, and the point of
// masking per string is that this one does not.
func TestJSONLineMultiMegabyteLine(t *testing.T) {
	var sb strings.Builder
	for sb.Len() < 4<<20 {
		sb.WriteString("func main() { fmt.Println(\"hello\", 42) } // a line of the file being read\n")
	}
	sb.WriteString("git remote set-url origin https://joe:hunter2@example.com/a/b.git\n")
	enc, _ := json.Marshal(map[string]any{"type": "user", "content": sb.String()})
	start := time.Now()
	got := jsonLine(string(enc))
	t.Logf("masked a %d-byte line in %v", len(enc), time.Since(start))
	if strings.Contains(got, "hunter2") {
		t.Fatal("the credential at the end of the line survived")
	}
	c := mustParse(t, got)["content"].(string)
	if len(c) < 4<<20 || !strings.Contains(c, "https://joe:[REDACTED]@example.com") {
		t.Errorf("masked content is wrong: %d bytes, tail %q", len(c), c[len(c)-80:])
	}
}

func BenchmarkJSONLineMultiMegabyte(b *testing.B) {
	var sb strings.Builder
	for sb.Len() < 4<<20 {
		sb.WriteString("func main() { fmt.Println(\"hello\", 42) } // a line of the file being read\n")
	}
	enc, _ := json.Marshal(map[string]any{"type": "user", "content": sb.String()})
	ln := string(enc)
	b.SetBytes(int64(len(ln)))
	for i := 0; i < b.N; i++ {
		_ = jsonLine(ln)
	}
}
