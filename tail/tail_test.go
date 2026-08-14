package tail

import (
	"testing"

	"github.com/stump-wtf/agent-trace/internal/strutil"
)

func TestSessionKey(t *testing.T) {
	k1 := sessionKey("claude-code", "/home/user/.claude/projects/foo/bar.jsonl")
	k2 := sessionKey("claude-code", "/home/user/.claude/projects/foo/bar.jsonl")
	k3 := sessionKey("codex", "/home/user/.claude/projects/foo/bar.jsonl")
	if k1 != k2 {
		t.Errorf("same inputs should produce same key: %q vs %q", k1, k2)
	}
	if k1 == k3 {
		t.Error("different harnesses should produce different keys")
	}
}

func TestInjectedUserMessage(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"# AGENTS.md instructions\n...", true},
		{"<system-reminder>hello</system-reminder>", true},
		{"fix the login bug", false},
		{"<div>html</div>", true}, // complete markup envelope
		{"", false},
	}
	for _, tt := range tests {
		got := injectedUserMessage(tt.text)
		if got != tt.want {
			t.Errorf("injectedUserMessage(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		s      string
		max    int
		suffix string
		want   string
	}{
		{"hello", 10, "...", "hello"},
		{"hello world", 8, "...", "hello..."},
		{"hi", 2, "…", "hi"},
		{"abc", 2, "…", "a…"},
	}
	for _, tt := range tests {
		got := strutil.TruncateRunes(tt.s, tt.max, tt.suffix)
		if got != tt.want {
			t.Errorf("strutil.TruncateRunes(%q, %d, %q) = %q, want %q", tt.s, tt.max, tt.suffix, got, tt.want)
		}
	}
}

func TestDefaultIdleConfig(t *testing.T) {
	cfg := DefaultIdleConfig()
	if cfg.IdleAfter <= 0 {
		t.Error("IdleAfter should be positive")
	}
}

func TestWatcherIsIdleNoEvents(t *testing.T) {
	w := NewWatcher(DefaultIdleConfig(), nil)
	defer w.Stop()
	if !w.IsIdle("nonexistent") {
		t.Error("session with no events should be idle")
	}
}
