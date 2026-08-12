package tail

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
)

// osClassifyOptions returns Options backed by the real filesystem — an
// os.Stat-based FileExists and real home/tmp dirs. Adapters pass this to
// classify.BuildEventWith so weak-target filtering and outside-scope
// detection work correctly end-to-end.
func osClassifyOptions() *classify.Options {
	home, _ := os.UserHomeDir()
	return &classify.Options{
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

