package tail

import (
	"crypto/sha256"
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

// truncateRunes truncates s to max runes, appending suffix if truncated.
func truncateRunes(s string, max int, suffix string) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	suffixRunes := []rune(suffix)
	cut := max - len(suffixRunes)
	if cut < 0 {
		cut = 0
	}
	return string(runes[:cut]) + suffix
}
