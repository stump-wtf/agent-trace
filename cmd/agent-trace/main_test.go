package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/tail"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// fixture is a synthesized Claude Code transcript whose user message, command
// lines and errored result each carry a placeholder credential.
const fixture = "testdata/claude-code.jsonl"

// fixtureSecrets are the placeholder credentials in fixture, by the command
// whose output carries them: otel has no field for an error excerpt, so the
// one in the errored result reaches only normalize. Redacted output must
// contain none of them; --no-redact output must contain every one, which is
// what shows the redaction assertions can fail at all.
var fixtureSecrets = map[string][]string{
	"normalize": {"correct-horse-battery", "placeholder-bearer-value", "placeholder-token-value", "placeholder-pass"},
	"otel":      {"correct-horse-battery", "placeholder-bearer-value", "placeholder-pass"},
}

// cli runs the command in-process and returns its exit code and output.
func cli(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestNormalizeGolden(t *testing.T) {
	code, out, errOut := cli(t, "normalize", "--harness", "claude-code", "--error-excerpt-bytes", "200", fixture)
	if code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	compareGolden(t, "testdata/normalize.golden", stableJSONL(t, out))
}

func TestOtelGolden(t *testing.T) {
	code, out, errOut := cli(t, "otel", "--harness", "claude-code", "--error-excerpt-bytes", "200", fixture)
	if code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	if !strings.HasSuffix(out, "}\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("otel output is not one newline-terminated JSON object: %q", out)
	}
	compareGolden(t, "testdata/otel.golden", stableJSONL(t, out))
}

// TestNormalizeRecordOrder pins the line contract a consumer relies on: the
// session first, then marks and events in seq order with a mark ahead of an
// event sharing its seq, each line carrying exactly the field its kind names.
func TestNormalizeRecordOrder(t *testing.T) {
	_, out, _ := cli(t, "normalize", "--harness", "claude-code", fixture)
	var kinds []string
	lastSeq := -1
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var rec map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not JSON: %v", i+1, err)
		}
		var kind string
		if err := json.Unmarshal(rec["kind"], &kind); err != nil {
			t.Fatalf("line %d kind: %v", i+1, err)
		}
		if len(rec) != 2 || rec[kind] == nil {
			t.Fatalf("line %d: want exactly kind and %q, got %s", i+1, kind, line)
		}
		kinds = append(kinds, kind)
		if kind == "session" {
			continue
		}
		var body struct{ Seq int }
		if err := json.Unmarshal(rec[kind], &body); err != nil {
			t.Fatalf("line %d seq: %v", i+1, err)
		}
		if body.Seq < lastSeq {
			t.Fatalf("line %d: seq %d after %d", i+1, body.Seq, lastSeq)
		}
		lastSeq = body.Seq
	}
	want := "session mark event event event mark event"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("record kinds = %q, want %q", got, want)
	}
}

func TestRedactedByDefault(t *testing.T) {
	for _, cmd := range []string{"normalize", "otel"} {
		t.Run(cmd, func(t *testing.T) {
			_, out, _ := cli(t, cmd, "--harness", "claude-code", "--error-excerpt-bytes", "200", fixture)
			for _, s := range fixtureSecrets["normalize"] {
				if strings.Contains(out, s) {
					t.Errorf("output carries %q", s)
				}
			}
			if !strings.Contains(out, redacted) {
				t.Errorf("output has no %s marker", redacted)
			}
		})
	}
}

func TestNoRedactKeepsRawText(t *testing.T) {
	for _, cmd := range []string{"normalize", "otel"} {
		t.Run(cmd, func(t *testing.T) {
			_, out, _ := cli(t, cmd, "--harness", "claude-code", "--error-excerpt-bytes", "200", "--no-redact", fixture)
			for _, s := range fixtureSecrets[cmd] {
				if !strings.Contains(out, s) {
					t.Errorf("--no-redact output lacks %q", s)
				}
			}
			if strings.Contains(out, redacted) {
				t.Errorf("--no-redact output has a %s marker", redacted)
			}
		})
	}
}

// TestErrorExcerptOptIn checks --error-excerpt-bytes reaches the adapter: no
// excerpt without it, and the redacted one with it.
func TestErrorExcerptOptIn(t *testing.T) {
	_, out, _ := cli(t, "normalize", "--harness", "claude-code", fixture)
	if strings.Contains(out, "errorExcerpt") {
		t.Fatalf("excerpt written without --error-excerpt-bytes:\n%s", out)
	}
	_, out, _ = cli(t, "normalize", "--harness", "claude-code", "--error-excerpt-bytes", "200", fixture)
	if !strings.Contains(out, `"errorExcerpt":"401 Unauthorized: token=[REDACTED] was rejected"`) {
		t.Fatalf("want the redacted excerpt, got:\n%s", out)
	}
}

func TestStdinNotSupportedYet(t *testing.T) {
	for _, args := range [][]string{
		{"normalize", "--harness", "claude-code", "-"},
		{"normalize", "--harness", "claude-code"},
		{"otel", "--harness", "claude-code", "-"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, out, errOut := cli(t, args...)
			if code != exitError {
				t.Errorf("exit %d, want %d", code, exitError)
			}
			if out != "" {
				t.Errorf("wrote to stdout: %q", out)
			}
			if !strings.Contains(errOut, errStdinUnsupported.Error()) {
				t.Errorf("stderr = %q, want the not-supported error", errOut)
			}
		})
	}

	// The sentinel is what #132 replaces, so it must come back unwrapped
	// enough for errors.Is.
	_, err := load(context.Background(), config{harness: "claude-code"}, "-", strings.NewReader("{}\n"))
	if !errors.Is(err, errStdinUnsupported) {
		t.Fatalf("load(-) = %v, want errStdinUnsupported", err)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no command", nil, "Usage:"},
		{"unknown command", []string{"frobnicate"}, `unknown command "frobnicate"`},
		{"missing harness", []string{"normalize", fixture}, "--harness is required"},
		{"unknown harness", []string{"otel", "--harness", "vim", fixture}, `unknown harness "vim"`},
		{"two transcripts", []string{"normalize", "--harness", "codex", "a", "b"}, "want one transcript, got 2"},
		{"negative excerpt", []string{"normalize", "--harness", "codex", "--error-excerpt-bytes", "-1", fixture}, "must not be negative"},
		{"bad flag", []string{"normalize", "--nope"}, "flag provided but not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := cli(t, tc.args...)
			if code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
			if out != "" {
				t.Errorf("wrote to stdout: %q", out)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", errOut, tc.want)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	code, out, _ := cli(t, "help")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"normalize", "otel", "-harness", "-no-redact", harnessList()} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	if code, _, _ := cli(t, "normalize", "-h"); code != exitOK {
		t.Errorf("normalize -h: exit %d, want %d", code, exitOK)
	}
}

func TestMissingTranscript(t *testing.T) {
	code, out, errOut := cli(t, "normalize", "--harness", "claude-code", filepath.Join(t.TempDir(), "absent.jsonl"))
	if code != exitError || out != "" || errOut == "" {
		t.Fatalf("exit %d, stdout %q, stderr %q: want exit %d, an error and no output", code, out, errOut, exitError)
	}
}

// TestFlagsAfterTranscript covers the order the standard flag package would
// otherwise stop at: the path first, then the flags.
func TestFlagsAfterTranscript(t *testing.T) {
	code, first, errOut := cli(t, "normalize", fixture, "--harness", "claude-code")
	if code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	_, second, _ := cli(t, "normalize", "--harness", "claude-code", fixture)
	if first != second {
		t.Fatalf("flag order changed the output:\n%s\nvs\n%s", first, second)
	}
	// After "--" everything is a transcript, even something shaped like a flag.
	_, _, errOut = cli(t, "normalize", "--harness", "claude-code", "--", "--no-redact")
	if !strings.Contains(errOut, "--no-redact") {
		t.Fatalf("want --no-redact treated as a path after --, stderr: %s", errOut)
	}
}

// TestHarnessDispatch runs each JSONL harness's own fixture through the CLI
// and checks the session record names that harness, so --harness really
// selects the adapter rather than every name reaching the same one.
func TestHarnessDispatch(t *testing.T) {
	for _, tc := range []struct{ harness, path string }{
		{"claude-code", fixture},
		{"codex", "../../tail/testdata/codex_marks.jsonl"},
		{"omp", "../../tail/testdata/omp_session.jsonl"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			code, out, errOut := cli(t, "normalize", "--harness", tc.harness, tc.path)
			if code != exitOK {
				t.Fatalf("exit %d, stderr: %s", code, errOut)
			}
			var rec struct {
				Kind    string
				Session tail.SessionMeta
			}
			line, _, _ := strings.Cut(out, "\n")
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatal(err)
			}
			if rec.Kind != "session" || string(rec.Session.Harness) != tc.harness {
				t.Fatalf("first record = %s, want the %s session", line, tc.harness)
			}
			if strings.Count(out, "\n") < 2 {
				t.Fatalf("no events or marks for %s:\n%s", tc.harness, out)
			}
		})
	}
}

func TestAdaptersAnswerToTheirName(t *testing.T) {
	for name, newAdapter := range adapters {
		if got := newAdapter().Harness(); got != name {
			t.Errorf("--harness %s builds a %s adapter", name, got)
		}
	}
}

// stableJSONL rewrites the values that depend on where the checkout sits —
// the session key hashes the transcript's absolute path, and trace and span
// IDs derive from the key — to placeholders numbered by first appearance in
// sorted-key order, so the golden files hold on any machine and still pin
// which span parents which. Each line is re-encoded indented for a readable
// diff.
func stableJSONL(t *testing.T, out string) string {
	t.Helper()
	ids := map[string]string{}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				s, ok := v[k].(string)
				if ok && (k == "key" || k == "traceId" || k == "spanId" || k == "parentSpanId") {
					if _, seen := ids[s]; !seen {
						ids[s] = fmt.Sprintf("<id-%d>", len(ids))
					}
					v[k] = ids[s]
					continue
				}
				walk(v[k])
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		}
	}

	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var v any
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("output line is not JSON: %v\n%s", err, sc.Text())
		}
		walk(v)
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func compareGolden(t *testing.T, path, got string) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./cmd/agent-trace -update to create it)", err)
	}
	if got != string(want) {
		t.Fatalf("output differs from %s (run go test ./cmd/agent-trace -update after checking the change):\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// TestExcerptKeepsFilesystemOptions checks that asking for excerpts, which
// replaces an adapter's own classify.Options, classifies targets exactly as
// the adapter's default does: a weak target naming a missing file is still
// dropped, one naming a file that exists is still kept, and a path under the
// home or temp directory is still scoped "home" or "tmp" rather than "other".
func TestExcerptKeepsFilesystemOptions(t *testing.T) {
	cwd := t.TempDir()
	home, _ := os.UserHomeDir()
	homeFile := filepath.Join(home, ".agent-trace-cli-test", "notes.md")
	tmpFile := filepath.Join(os.TempDir(), "agent-trace-cli-test.log")
	command := "cat src/present.go src/absent.go " + tmpFile
	if home != "" {
		command += " " + homeFile
	}
	if err := os.Mkdir(filepath.Join(cwd, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "src", "present.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"s","cwd":` + strconv.Quote(cwd) + `,"message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:00:01Z","sessionId":"s","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":` + strconv.Quote(command) + `}}]}}`,
		`{"type":"user","timestamp":"2026-01-01T10:00:02Z","sessionId":"s","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","is_error":true,"content":"cat: src/absent.go: No such file or directory"}]}}`,
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	targets := func(excerptBytes int) string {
		t.Helper()
		s, err := load(context.Background(), config{harness: "claude-code", excerptBytes: excerptBytes}, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.events) != 1 {
			t.Fatalf("want one event, got %d", len(s.events))
		}
		b, _ := json.Marshal(struct {
			T any
			O any
		}{s.events[0].Targets, s.events[0].Outside})
		return string(b)
	}
	def, withExcerpt := targets(0), targets(200)
	if !strings.Contains(def, "present.go") || strings.Contains(def, "absent.go") {
		t.Fatalf("fixture does not exercise weak-target filtering: %s", def)
	}
	if !strings.Contains(def, `"scope":"tmp"`) || (home != "" && !strings.Contains(def, `"scope":"home"`)) {
		t.Fatalf("fixture does not exercise home/tmp scoping: %s", def)
	}
	if withExcerpt != def {
		t.Fatalf("--error-excerpt-bytes changed the targets:\n default %s\nexcerpt %s", def, withExcerpt)
	}
}
