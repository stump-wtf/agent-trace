package tail

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stump-wtf/agent-trace/classify"
)

// dirDiagnostics checks whether a directory exists and is readable. Shared by
// JSONL-backed adapters (Claude Code, Codex, Pi) that all watch a filesystem
// directory.
func dirDiagnostics(dir string) []DiagnosticCheck {
	if dir == "" {
		return []DiagnosticCheck{{Name: "session-dir", Status: "error", Detail: "session directory is empty"}}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return []DiagnosticCheck{{Name: "session-dir", Status: "warn", Detail: "directory does not exist: " + dir}}
	}
	if !info.IsDir() {
		return []DiagnosticCheck{{Name: "session-dir", Status: "error", Detail: "path is not a directory: " + dir}}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []DiagnosticCheck{{Name: "session-dir", Status: "error", Detail: "directory not readable: " + err.Error()}}
	}
	return []DiagnosticCheck{{Name: "session-dir", Status: "ok", Detail: fmt.Sprintf("%d entries in %s", len(entries), dir)}}
}

// osClassifyOptions returns Options backed by the real filesystem — an
// os.Stat-based FileExists and real home/tmp dirs. Adapters pass this to
// classify.BuildEventWith so weak-target filtering and outside-scope
// detection work correctly end-to-end.
//
// verifyPatterns, when non-empty, is copied into Options.VerifyPatterns so
// custom verify commands (e.g. "just test") are classified correctly.
func osClassifyOptions(verifyPatterns []string) *classify.Options {
	home, _ := os.UserHomeDir()
	opts := &classify.Options{
		FileExists: func(cwd, rel string) bool {
			if cwd == "" || rel == "" {
				return false
			}
			abs := filepath.Join(cwd, filepath.FromSlash(rel))
			_, err := os.Stat(abs)
			return err == nil
		},
		HomeDir: home,
		TmpDir:  os.TempDir(),
	}
	if len(verifyPatterns) > 0 {
		opts.VerifyPatterns = verifyPatterns
	}
	return opts
}

// sessionKey produces a stable identifier for a session file, independent of
// the agent-level session ID. Codex resume rollouts can share an ID across
// multiple files, so IDs are display metadata rather than safe routing keys.
func sessionKey(harness, path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	sum := sha256.Sum256([]byte(harness + "\x00" + path))
	return fmt.Sprintf("%s-%x", harness, sum[:12])
}

// injectedUserMessage recognizes harness-injected text recorded as a user
// message but not written by the user. These are dropped before they become
// user-message marks — they would inflate turn stats and clutter timelines.
func injectedUserMessage(text string) bool {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "# AGENTS.md instructions") {
		return true
	}
	return strings.HasPrefix(text, "<") && strings.HasSuffix(text, ">")
}

// homeDir returns filepath.Join(home, paths...) or "" if the home directory
// cannot be determined.
func homeDir(paths ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home}, paths...)...)
}

// mtimeExcludes reports that a file was last written early enough that no
// session inside it can satisfy the filter's time bounds — so it can be skipped
// without being opened at all. This is the whole point of pushing a time filter
// into a file-walking adapter: the decision costs a stat, not a read and parse.
//
// Sound for both bounds, given that a file's contents were written at or before
// its modification time:
//
//   - ActiveSince asks when the session was last touched, and mtime is exactly
//     that signal.
//   - Since asks when it started. If mtime precedes Since then every timestamp
//     in the file precedes Since, so StartedAt does too and the session fails
//     the bound regardless.
//
// The assumption fails only if mtime is older than the timestamps recorded
// inside the file, which needs a backdated mtime or a clock that moved
// backwards. The opposite skew — a restored or copied file carrying an mtime
// newer than its contents — only over-selects, and filterSessions still applies
// the exact predicate afterwards, so it costs a wasted read and never a wrong
// answer.
func mtimeExcludes(info os.FileInfo, f SessionFilter) bool {
	if info == nil {
		return false
	}
	mod := info.ModTime()
	if !f.ActiveSince.IsZero() && mod.Before(f.ActiveSince) {
		return true
	}
	if !f.Since.IsZero() && mod.Before(f.Since) {
		return true
	}
	return false
}

// openJSONLSession opens a JSONL session file and returns a base SessionMeta
// with the harness, key, and file-derived ID pre-populated. The caller is
// responsible for closing the returned file.
func openJSONLSession(harness Harness, path string) (*os.File, SessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, SessionMeta{}, err
	}
	id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return f, SessionMeta{
		Key:     sessionKey(string(harness), path),
		ID:      id,
		Harness: harness,
		Path:    path,
	}, nil
}

// unmarshalContentList parses a JSON content list that may be a string,
// null, or an array of items. makeTextItem converts a bare string into
// a single-element slice with the harness's content item type.
func unmarshalContentList[T any](data []byte, makeTextItem func(string) T) ([]T, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, err
		}
		return []T{makeTextItem(s)}, nil
	}
	var items []T
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	return items, nil
}
