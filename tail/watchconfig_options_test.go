package tail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// TestWatchConfigKeepsFilesystemClassifyOptions is #116: the Options the
// watcher injected for VerifyPatterns or ErrorExcerptBytes carried no
// FileExists, HomeDir or TmpDir, and replaced the filesystem-backed default an
// adapter falls back to. Turning either setting on silently kept weak targets
// that do not exist and demoted a home-dir file to "other".
//
// It drives the settings the way a consumer does — through WatchConfig and a
// real scan — rather than handing the adapter an Options, because the defect
// was in what the watcher built, not in what the adapter does with it.
func TestWatchConfigKeepsFilesystemClassifyOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // osClassifyOptions reads it via os.UserHomeDir
	notes := writeFile(t, filepath.Join(home, "notes.txt"), "n\n")

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "present.go"), "package p\n")
	dir := filepath.Join(root, ".claude", "projects", "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(ts, typ, content string) string {
		return `{"type":"` + typ + `","timestamp":"2026-01-01T10:00:` + ts + `Z","sessionId":"s1","cwd":"` + root +
			`","message":{"role":"` + typ + `","content":[` + content + `]}}`
	}
	writeFile(t, filepath.Join(dir, "s.jsonl"), strings.Join([]string{
		line("00", "assistant", `{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"cat does/not/exist.go"}}`),
		line("01", "user", `{"type":"tool_result","tool_use_id":"c1","is_error":true,"content":"cat: does/not/exist.go: No such file or directory"}`),
		line("02", "assistant", `{"type":"tool_use","id":"c2","name":"Bash","input":{"command":"cat present.go"}}`),
		line("03", "user", `{"type":"tool_result","tool_use_id":"c2","content":"package p"}`),
		line("04", "assistant", `{"type":"tool_use","id":"c3","name":"Read","input":{"file_path":"`+notes+`"}}`),
		line("05", "user", `{"type":"tool_result","tool_use_id":"c3","content":"n"}`),
	}, "\n")+"\n")

	for _, tc := range []struct {
		name string
		cfg  WatchConfig
	}{
		{"default", WatchConfig{}},
		{"VerifyPatterns", WatchConfig{VerifyPatterns: []string{"just test"}}},
		{"ErrorExcerptBytes", WatchConfig{ErrorExcerptBytes: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.MaxAge = -1 // the fixture carries fixed 2026-01-01 timestamps
			w := NewWatcherWithConfig(cfg, DefaultAdaptersIn(root))
			if err := w.ScanOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := drain(w.Events())
			if len(got) != 3 {
				t.Fatalf("got %d events, want 3", len(got))
			}

			if targets := got[0].Classified.Targets; len(targets) != 0 {
				t.Errorf("missing weak target kept: Targets = %+v, want none", targets)
			}
			// The existing file must survive, or "filtered" above could mean
			// every weak target is dropped rather than only missing ones.
			if targets := got[1].Classified.Targets; len(targets) != 1 || targets[0].Path != "present.go" {
				t.Errorf("existing weak target: Targets = %+v, want present.go", targets)
			}
			want := []classify.OutsideTouch{{Scope: "home", Path: notes}}
			if outside := got[2].Classified.Outside; len(outside) != 1 || outside[0].Scope != want[0].Scope || outside[0].Path != want[0].Path {
				t.Errorf("Outside = %+v, want %+v", outside, want)
			}
		})
	}
}

// TestWatchConfigKeepsFilesystemClassifyOptionsIncremental is #116 on the
// path a long-running watcher spends its life on. The first scan is a full
// Parse; every later one goes through ParseSince, which reads the injected
// Options separately. A fix that reached Parse alone would pass the test above
// and still classify every live update without the filesystem.
func TestWatchConfigKeepsFilesystemClassifyOptionsIncremental(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // osClassifyOptions reads it via os.UserHomeDir
	notes := writeFile(t, filepath.Join(home, "notes.txt"), "n\n")

	for _, tc := range []struct {
		name string
		cfg  WatchConfig
	}{
		{"default", WatchConfig{}},
		{"VerifyPatterns", WatchConfig{VerifyPatterns: []string{"just test"}}},
		{"ErrorExcerptBytes", WatchConfig{ErrorExcerptBytes: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fixture per subtest: the second scan appends to it.
			root := t.TempDir()
			dir := filepath.Join(root, ".claude", "projects", "p")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			line := func(ts, typ, content string) string {
				return `{"type":"` + typ + `","timestamp":"2026-01-01T10:00:` + ts + `Z","sessionId":"s1","cwd":"` + root +
					`","message":{"role":"` + typ + `","content":[` + content + `]}}` + "\n"
			}
			session := writeFile(t, filepath.Join(dir, "s.jsonl"),
				line("00", "assistant", `{"type":"tool_use","id":"c0","name":"Bash","input":{"command":"ls"}}`)+
					line("01", "user", `{"type":"tool_result","tool_use_id":"c0","content":"ok"}`))

			cfg := tc.cfg
			cfg.MaxAge = -1 // the fixture carries fixed 2026-01-01 timestamps
			w := NewWatcherWithConfig(cfg, DefaultAdaptersIn(root))
			if err := w.ScanOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := drain(w.Events()); len(got) != 1 {
				t.Fatalf("first scan: got %d events, want 1", len(got))
			}

			appendLines(t, session,
				line("02", "assistant", `{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"cat does/not/exist.go"}}`)+
					line("03", "user", `{"type":"tool_result","tool_use_id":"c1","is_error":true,"content":"cat: does/not/exist.go: No such file or directory"}`)+
					line("04", "assistant", `{"type":"tool_use","id":"c2","name":"Read","input":{"file_path":"`+notes+`"}}`)+
					line("05", "user", `{"type":"tool_result","tool_use_id":"c2","content":"n"}`))
			if err := w.ScanOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := drain(w.Events())
			if len(got) != 2 {
				t.Fatalf("second scan: got %d events, want 2", len(got))
			}
			if got[0].Classified.Seq != 1 || got[1].Classified.Seq != 2 {
				t.Errorf("second scan seqs = %d, %d, want 1, 2 (continuing after the first scan)", got[0].Classified.Seq, got[1].Classified.Seq)
			}
			if targets := got[0].Classified.Targets; len(targets) != 0 {
				t.Errorf("missing weak target kept: Targets = %+v, want none", targets)
			}
			if outside := got[1].Classified.Outside; len(outside) != 1 || outside[0].Scope != "home" || outside[0].Path != notes {
				t.Errorf("Outside = %+v, want [{Scope:home Path:%s}]", outside, notes)
			}
		})
	}
}
