package tail

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// Error Excerpts Per Adapter
//
// classify.Options.ErrorExcerptBytes only produces an excerpt when an adapter
// hands BuildEventWith the real result text and a correct IsError. Each case
// below is one adapter's own record shape for a failed tool call next to a
// successful one, checked through Parse and ParseSince with the option off
// and on. The "off" half asserts IsError too, so an adapter that never sets it
// cannot pass by producing no excerpt for a reason unrelated to the option.
//
// @joestump-agent 09/22/2026 - Added with Options.ErrorExcerptBytes.

// excerptBudget is large enough that no fixture below is cut, so each case
// pins the text an adapter passed rather than the excerpt's cutting rule,
// which classify tests on its own.
const excerptBudget = 200

type excerptCase struct {
	name string
	// build writes a fresh fixture and returns an adapter reading it, so the
	// options one subtest sets never leak into another.
	build func(t *testing.T) (Adapter, string)
	// events is how many events the fixture yields.
	events int
	// want maps an event index to the excerpt it must carry when enabled.
	// Every other event is a success and must carry none.
	want map[int]string
}

func excerptCases() []excerptCase {
	return []excerptCase{
		{
			name: "claude-code",
			build: func(t *testing.T) (Adapter, string) {
				dir := t.TempDir()
				path := writeFile(t, filepath.Join(dir, "s.jsonl"), strings.Join([]string{
					`{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","cwd":"/repo","message":{"role":"user","content":"fix the build"}}`,
					`{"type":"assistant","timestamp":"2026-01-01T10:00:01Z","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"go build ./..."}}]}}`,
					`{"type":"user","timestamp":"2026-01-01T10:00:02Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","is_error":true,"content":"Exit code 1\n\u001b[31m./main.go:3:2: undefined: foo\u001b[0m\n"}]}}`,
					`{"type":"assistant","timestamp":"2026-01-01T10:00:03Z","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","id":"c2","name":"Read","input":{"file_path":"/repo/main.go"}}]}}`,
					`{"type":"user","timestamp":"2026-01-01T10:00:04Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c2","content":"package main"}]}}`,
				}, "\n")+"\n")
				return &ClaudeCodeAdapter{Dir: dir}, path
			},
			events: 2,
			want:   map[int]string{0: "Exit code 1\n./main.go:3:2: undefined: foo"},
		},
		{
			// Codex writes no error flag; the adapter infers one from the
			// exit code in the output header (commandOutputFailed).
			name: "codex",
			build: func(t *testing.T) (Adapter, string) {
				dir := t.TempDir()
				path := writeFile(t, filepath.Join(dir, "rollout.jsonl"), strings.Join([]string{
					`{"type":"session_meta","timestamp":"2026-01-01T10:00:00Z","payload":{"id":"sess-1","cwd":"/repo"}}`,
					`{"type":"response_item","timestamp":"2026-01-01T10:00:01Z","payload":{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":[\"go\",\"build\",\"./...\"]}"}}`,
					`{"type":"response_item","timestamp":"2026-01-01T10:00:02Z","payload":{"type":"function_call_output","call_id":"c1","output":"Exit code: 1\nWall time: 0.2 seconds\nOutput:\n./main.go:3:2: undefined: foo\n"}}`,
					`{"type":"response_item","timestamp":"2026-01-01T10:00:03Z","payload":{"type":"function_call","call_id":"c2","name":"shell","arguments":"{\"command\":[\"ls\"]}"}}`,
					`{"type":"response_item","timestamp":"2026-01-01T10:00:04Z","payload":{"type":"function_call_output","call_id":"c2","output":"Exit code: 0\nWall time: 0.1 seconds\nOutput:\nmain.go\n"}}`,
				}, "\n")+"\n")
				return &CodexAdapter{Dir: dir}, path
			},
			events: 2,
			want:   map[int]string{0: "Exit code: 1\nWall time: 0.2 seconds\nOutput:\n./main.go:3:2: undefined: foo"},
		},
		{
			// Crush flags a tool's error response with is_error on the
			// tool_result part.
			name: "crush",
			build: func(t *testing.T) (Adapter, string) {
				resetDBCache()
				t.Cleanup(resetDBCache)
				dbPath := newCrushSession(t, "s1", 1789120000)
				insertCrushMessages(t, dbPath, "s1", 1789120000,
					[]string{"user", "assistant", "tool", "assistant", "tool"},
					[]string{
						`[{"type":"text","data":{"text":"fix the build"}}]`,
						`[{"type":"tool_call","data":{"id":"c1","name":"view","input":"{\"file_path\":\"missing.go\"}","finished":true}}]`,
						`[{"type":"tool_result","data":{"tool_call_id":"c1","name":"view","content":"file not found: /repo/missing.go","is_error":true}}]`,
						`[{"type":"tool_call","data":{"id":"c2","name":"view","input":"{\"file_path\":\"main.go\"}","finished":true}}]`,
						`[{"type":"tool_result","data":{"tool_call_id":"c2","name":"view","content":"package main"}}]`,
					})
				return &CrushAdapter{DBPath: dbPath, Cwd: "/repo"}, dbPath + "/s1"
			},
			events: 2,
			want:   map[int]string{0: "file not found: /repo/missing.go"},
		},
		{
			// OpenCode keeps a failed call's text in state.error rather than
			// state.output.
			name: "opencode",
			build: func(t *testing.T) (Adapter, string) {
				resetDBCache()
				t.Cleanup(resetDBCache)
				dbPath := filepath.Join(t.TempDir(), "opencode.db")
				createTestOpenCodeDB(t, dbPath)
				db, err := sql.Open("sqlite", dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				exec := func(q string, args ...any) {
					t.Helper()
					if _, err := db.Exec(q, args...); err != nil {
						t.Fatal(err)
					}
				}
				exec(`INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated) VALUES ('s1', 'p1', 's1', '/repo', 'Run', '1.0.0', 1784148215000, 1784148220000)`)
				exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('m1', 's1', 1784148216000, 1784148216000, '{"role":"assistant"}')`)
				exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('p1', 'm1', 's1', 1784148217000, 1784148217000, ?)`,
					`{"type":"tool","tool":"bash","callID":"c1","state":{"status":"error","input":{"command":"go build ./..."},"error":"./main.go:3:2: undefined: foo"}}`)
				exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('p2', 'm1', 's1', 1784148218000, 1784148218000, ?)`,
					`{"type":"tool","tool":"read","callID":"c2","state":{"status":"completed","input":{"filePath":"/repo/main.go"},"output":"package main"}}`)
				return &OpenCodeAdapter{DBPath: dbPath}, dbPath + "/s1"
			},
			events: 2,
			want:   map[int]string{0: "./main.go:3:2: undefined: foo"},
		},
		{
			// Pi flags a tool result with isError, and a user-run shell
			// command (bashExecution) through its exit code.
			name: "pi",
			build: func(t *testing.T) (Adapter, string) {
				path := writeTempJSONL(t, "pi.jsonl", strings.Join([]string{
					`{"type":"session","id":"s1","timestamp":"2026-01-01T10:00:00Z","cwd":"/repo"}`,
					`{"type":"message","id":"m1","parentId":"s1","timestamp":"2026-01-01T10:00:01Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"read","arguments":{"path":"/repo/missing.go"}}]}}`,
					`{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-01-01T10:00:02Z","message":{"role":"toolResult","toolCallId":"tc1","content":[{"type":"text","text":"ENOENT: no such file or directory, open '/repo/missing.go'"}],"isError":true}}`,
					`{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-01-01T10:00:03Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc2","name":"read","arguments":{"path":"/repo/main.go"}}]}}`,
					`{"type":"message","id":"m4","parentId":"m3","timestamp":"2026-01-01T10:00:04Z","message":{"role":"toolResult","toolCallId":"tc2","content":[{"type":"text","text":"package main"}],"isError":false}}`,
					`{"type":"message","id":"m5","parentId":"m4","timestamp":"2026-01-01T10:00:05Z","message":{"role":"bashExecution","command":"go build ./...","output":"./main.go:3:2: undefined: foo\n","exitCode":1}}`,
				}, "\n")+"\n")
				return &PiAdapter{}, path
			},
			events: 3,
			want: map[int]string{
				0: "ENOENT: no such file or directory, open '/repo/missing.go'",
				2: "./main.go:3:2: undefined: foo",
			},
		},
	}
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func enableExcerpts(t *testing.T, a Adapter) {
	t.Helper()
	s, ok := a.(OptionsSetter)
	if !ok {
		t.Fatalf("%T does not implement OptionsSetter", a)
	}
	s.SetOptions(&classify.Options{ErrorExcerptBytes: excerptBudget})
}

// checkExcerpts asserts events against a case. With enabled false it checks
// the errored events are flagged but carry no text.
func checkExcerpts(t *testing.T, c excerptCase, events []classify.Event, enabled bool) {
	t.Helper()
	if len(events) != c.events {
		t.Fatalf("got %d events, want %d: %+v", len(events), c.events, events)
	}
	for i, ev := range events {
		want, errored := c.want[i]
		if ev.IsError != errored {
			t.Errorf("event %d (%s): IsError = %v, want %v", i, ev.Tool, ev.IsError, errored)
		}
		if !enabled {
			want = ""
		}
		if ev.ErrorExcerpt != want {
			t.Errorf("event %d (%s): ErrorExcerpt = %q, want %q", i, ev.Tool, ev.ErrorExcerpt, want)
		}
	}
}

func TestAdaptersErrorExcerpt(t *testing.T) {
	for _, c := range excerptCases() {
		t.Run(c.name+"/Parse disabled", func(t *testing.T) {
			a, path := c.build(t)
			events, _, _, err := a.Parse(t.Context(), path)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			checkExcerpts(t, c, events, false)
		})
		t.Run(c.name+"/Parse enabled", func(t *testing.T) {
			a, path := c.build(t)
			enableExcerpts(t, a)
			events, _, _, err := a.Parse(t.Context(), path)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			checkExcerpts(t, c, events, true)
		})
		t.Run(c.name+"/ParseSince enabled", func(t *testing.T) {
			a, path := c.build(t)
			enableExcerpts(t, a)
			ip, ok := a.(IncrementalParser)
			if !ok {
				t.Fatalf("%T does not implement IncrementalParser", a)
			}
			events, _, _, _, err := ip.ParseSince(t.Context(), path, 0, 0)
			if err != nil {
				t.Fatalf("ParseSince: %v", err)
			}
			checkExcerpts(t, c, events, true)
		})
	}
}

// TestWatcherThreadsErrorExcerptAndRedact drives the setting the way a
// consumer does, through WatchConfig, and checks the excerpt that reaches the
// events channel is the redacted one.
func TestWatcherThreadsErrorExcerptAndRedact(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".claude", "projects", "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "s.jsonl"), strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","cwd":"` + root + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"gh api user"}}]}}`,
		`{"type":"user","timestamp":"2026-01-01T10:00:01Z","sessionId":"s1","cwd":"` + root + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","is_error":true,"content":"HTTP 401: Bad credentials (token ghp_secret123)"}]}}`,
	}, "\n")+"\n")

	scan := func(cfg WatchConfig) []Event {
		t.Helper()
		cfg.MaxAge = -1 // the fixture carries fixed 2026-01-01 timestamps
		w := NewWatcherWithConfig(cfg, DefaultAdaptersIn(root))
		if err := w.ScanOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		return drain(w.Events())
	}

	got := scan(WatchConfig{
		ErrorExcerptBytes: excerptBudget,
		Redact:            func(s string) string { return strings.ReplaceAll(s, "ghp_secret123", "[REDACTED]") },
	})
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if want := "HTTP 401: Bad credentials (token [REDACTED])"; got[0].Classified.ErrorExcerpt != want {
		t.Errorf("ErrorExcerpt = %q, want %q", got[0].Classified.ErrorExcerpt, want)
	}

	got = scan(WatchConfig{})
	if len(got) != 1 {
		t.Fatalf("default config: got %d events, want 1", len(got))
	}
	if !got[0].Classified.IsError || got[0].Classified.ErrorExcerpt != "" {
		t.Errorf("default config: IsError = %v, ErrorExcerpt = %q; want an errored event with no text",
			got[0].Classified.IsError, got[0].Classified.ErrorExcerpt)
	}
}

// TestWatcherErrorExcerptAloneSetsOptions checks the watcher injects Options
// when only ErrorExcerptBytes is set, and still does not when nothing is —
// the no-change path TestVerifyPatternsEmptyLeavesDefaultsUnchanged covers
// for VerifyPatterns.
func TestWatcherErrorExcerptAloneSetsOptions(t *testing.T) {
	a := &ClaudeCodeAdapter{Dir: t.TempDir()}
	NewWatcherWithConfig(WatchConfig{ErrorExcerptBytes: 64}, []Adapter{a})
	if a.opts == nil || a.opts.ErrorExcerptBytes != 64 {
		t.Fatalf("opts = %+v, want ErrorExcerptBytes 64", a.opts)
	}

	b := &ClaudeCodeAdapter{Dir: t.TempDir()}
	NewWatcherWithConfig(WatchConfig{Redact: func(s string) string { return s }}, []Adapter{b})
	if b.opts != nil {
		t.Errorf("Redact alone set opts = %+v; it has no effect without ErrorExcerptBytes", b.opts)
	}
}
