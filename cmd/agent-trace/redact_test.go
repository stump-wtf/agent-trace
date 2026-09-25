package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"
)

// fake assembles a token-shaped string at run time, so no literal in this file
// looks like a credential to a secret scanner.
func fake(prefix string, n int) string { return prefix + strings.Repeat("x", n) }

func TestDefaultRedact(t *testing.T) {
	pem := "-----BEGIN RSA " + "PRIVATE KEY-----\nabc\ndef\n-----END RSA " + "PRIVATE KEY-----"
	for _, tc := range []struct {
		name, in, want string
	}{
		{"authorization header", `wget --header "Authorization: Bearer abcdefgh12345678" x`, `wget --header "Authorization: Bearer [REDACTED]" x`},
		{"authorization token scheme", `-H 'authorization: token abc123'`, `-H 'authorization: token [REDACTED]'`},
		{"authorization json value", `{"Authorization":"Basic dXNlcjpwYXNz"}`, `{"Authorization":"Basic [REDACTED]"}`},
		{"authorization dict value", `headers={'authorization': 'Bearer short'}`, `headers={'authorization': 'Bearer [REDACTED]'}`},
		{"bare bearer", "sent bearer " + fake("", 20), "sent bearer [REDACTED]"},
		{"url userinfo", "git clone https://ci:hunter2@example.com/r.git", "git clone https://ci:[REDACTED]@example.com/r.git"},
		{"env assignment", "GITEA_TOKEN=abc123 make deploy", "GITEA_TOKEN=[REDACTED] make deploy"},
		{"password colon", "the password: correct-horse", "the password: [REDACTED]"},
		{"quoted json value", `{"api_key": "abc def"}`, `{"api_key": "[REDACTED]"}`},
		{"flag style", "--client-secret=s3cr3t --verbose", "--client-secret=[REDACTED] --verbose"},
		{"flag with a spaced value", "mysql --password hunter2 -h db", "mysql --password [REDACTED] -h db"},
		{"quoted flag value", `login --api-key "abc123" --yes`, `login --api-key "[REDACTED]" --yes`},
		{"curl user", "cur" + "l -s -u admin:" + fake("", 12) + " https://x", "cur" + "l -s -u admin:[REDACTED] https://x"},
		{"curl long user", "cur" + "l --user=admin:" + fake("", 12) + " https://x", "cur" + "l --user=admin:[REDACTED] https://x"},
		{"pem block", "key:\n" + pem + "\ndone", "key:\n[REDACTED]\ndone"},
		{"github token", "push with " + fake("gh"+"p_", 36), "push with [REDACTED]"},
		{"github fine-grained", fake("github"+"_pat_", 40), "[REDACTED]"},
		{"openai style key", "key " + fake("sk"+"-", 32), "key [REDACTED]"},
		{"slack token", fake("xo"+"xb-", 24), "[REDACTED]"},
		{"aws access key", "AKIA" + strings.Repeat("X", 16), "[REDACTED]"},
		{"jwt", "eyJ" + fake("", 10) + "." + fake("eyJ", 10) + "." + fake("", 10), "[REDACTED]"},

		// Left alone: counts and prose that merely mention a keyword.
		{"token counts", "max_tokens=4096 input_tokens: 12", "max_tokens=4096 input_tokens: 12"},
		{"flag naming a file", "--token-file ./t --max-tokens 4096", "--token-file ./t --max-tokens 4096"},
		{"-u without curl", "git push -u origin feat:main", "git push -u origin feat:main"},
		{"prose", "rotate the token before the secret expires", "rotate the token before the secret expires"},
		{"plain url", "https://example.com/a:b", "https://example.com/a:b"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultRedact(tc.in); got != tc.want {
				t.Errorf("defaultRedact(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDefaultRedactIdempotent checks that redacting twice changes nothing
// further: an already-redacted excerpt goes through redactAll again.
func TestDefaultRedactIdempotent(t *testing.T) {
	in := `Authorization: Bearer abcdefgh12345678 password=x --password y https://u:p@h ` + fake("gh"+"p_", 36) + " cur" + "l -u a:b"
	once := defaultRedact(in)
	if twice := defaultRedact(once); twice != once {
		t.Fatalf("second pass changed the text:\n once %q\ntwice %q", once, twice)
	}
}

func TestRedactAllCoversEveryTextField(t *testing.T) {
	const secret = "password=hunter2"
	meta := tail.SessionMeta{Title: secret, Cwd: "/work/" + secret, Path: "/p/" + secret}
	events := []classify.Event{{Summary: secret, ErrorExcerpt: secret, Targets: []classify.Target{{Path: secret}}}}
	marks := []classify.Mark{{Note: secret}}

	redactAll(defaultRedact, &meta, events, marks)

	for field, got := range map[string]string{
		"SessionMeta.Title":  meta.Title,
		"Event.Summary":      events[0].Summary,
		"Event.ErrorExcerpt": events[0].ErrorExcerpt,
		"Mark.Note":          marks[0].Note,
	} {
		if got != "password=[REDACTED]" {
			t.Errorf("%s = %q, want it redacted", field, got)
		}
	}
	// Paths are the record's substance and are deliberately untouched.
	if meta.Cwd != "/work/"+secret || meta.Path != "/p/"+secret || events[0].Targets[0].Path != secret {
		t.Errorf("paths were rewritten: cwd %q, path %q, target %q", meta.Cwd, meta.Path, events[0].Targets[0].Path)
	}
}

// TestExcerptRedactedBeforeTheCut checks the CLI hands its redactor to the
// adapter's classify.Options rather than only redacting afterwards. The error
// text opens with a token and the excerpt budget cuts through it: redacted
// after the cut, the fragment left is too short for the token pattern and
// survives; redacted before it, the whole token is gone.
func TestExcerptRedactedBeforeTheCut(t *testing.T) {
	token := fake("gh"+"p_", 36)
	content := token + "\n" + strings.Repeat("filler line\n", 20) + "FAIL"
	lines := []string{
		`{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"s","cwd":"/w","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:00:01Z","sessionId":"s","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"make"}}]}}`,
		`{"type":"user","timestamp":"2026-01-01T10:00:02Z","sessionId":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","is_error":true,"content":` + strconv.Quote(content) + `}]}}`,
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := load(context.Background(), config{harness: "claude-code", excerptBytes: 25}, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 1 {
		t.Fatalf("want one event, got %d", len(s.events))
	}
	excerpt := s.events[0].ErrorExcerpt
	if excerpt == "" || !strings.Contains(excerpt, classify.ErrorExcerptElision) {
		t.Fatalf("want a cut excerpt, got %q", excerpt)
	}
	if strings.Contains(excerpt, "gh"+"p_") {
		t.Fatalf("excerpt keeps part of the token: %q", excerpt)
	}
}
