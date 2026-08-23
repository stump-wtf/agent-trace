package tail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBigSession builds a Claude Code transcript whose opening lines are large
// enough to push the model-bearing line deep into the file by byte offset while
// keeping it shallow by line count — the real shape of a transcript, where the
// system prompt and injected context run to tens of kilobytes per line.
//
// firstModel appears near the top, lastModel at the very bottom, so a summary
// that reads only the tail is distinguishable from one that reads the head.
func writeBigSession(t *testing.T, padLines int, padWidth int, firstModel, lastModel string) string {
	t.Helper()
	pad := strings.Repeat("x", padWidth)

	var b strings.Builder
	fmt.Fprintf(&b, `{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"big-1","cwd":"/w","gitBranch":"main","message":{"role":"user","content":%q}}`+"\n", "start")
	for i := range padLines {
		fmt.Fprintf(&b, `{"type":"user","timestamp":"2026-01-01T10:00:%02dZ","sessionId":"big-1","message":{"role":"user","content":%q}}`+"\n", i+1, pad)
	}
	fmt.Fprintf(&b, `{"type":"assistant","timestamp":"2026-01-01T11:00:00Z","sessionId":"big-1","message":{"role":"assistant","model":%q,"content":[]}}`+"\n", firstModel)
	// Bulk of the session: lines that carry nothing the summary needs, and
	// enough of them that the head and tail windows cannot overlap. If they
	// overlap the tail scan re-reads the head's lines and the test cannot tell
	// a head-sourced field from a tail-sourced one.
	for i := range 400 {
		fmt.Fprintf(&b, `{"type":"user","timestamp":"2026-01-01T12:00:%02dZ","sessionId":"big-1","message":{"role":"user","content":%q}}`+"\n", i%60, pad)
	}
	fmt.Fprintf(&b, `{"type":"assistant","timestamp":"2026-01-01T23:59:59Z","sessionId":"big-1","message":{"role":"assistant","model":%q,"content":[]}}`+"\n", lastModel)

	path := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// The head budget must reach the first model line even when the lines ahead of
// it are huge. Capping the head by bytes alone truncates before that line, and
// Model then gets filled from the tail — reporting the model the session ended
// on instead of the one it started with. That was observed on 17 of 344 real
// sessions before the byte bound was widened.
func TestSummarizeReadsModelFromHeadNotTail(t *testing.T) {
	// 10 lines of 8 KiB each puts the model line past 80 KiB but at line 12.
	path := writeBigSession(t, 10, 8*1024, "claude-opus-4-7", "claude-opus-4-8")

	meta, err := ClaudeCodeAdapter{}.Summarize(t.Context(), path)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if meta.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q, want the model from the head (%q); reading it from the tail yields %q",
			meta.Model, "claude-opus-4-7", "claude-opus-4-8")
	}
}

// The closing timestamp comes from the tail, and the opening one from the head,
// on a file far larger than the head budget.
func TestSummarizeSpansHeadAndTailForTimestamps(t *testing.T) {
	path := writeBigSession(t, 10, 8*1024, "claude-opus-4-7", "claude-opus-4-8")

	meta, err := ClaudeCodeAdapter{}.Summarize(t.Context(), path)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if meta.StartedAt != "2026-01-01T10:00:00Z" {
		t.Errorf("StartedAt = %q, want the first line's timestamp", meta.StartedAt)
	}
	if meta.EndedAt != "2026-01-01T23:59:59Z" {
		t.Errorf("EndedAt = %q, want the last line's timestamp", meta.EndedAt)
	}
	if meta.Cwd != "/w" {
		t.Errorf("Cwd = %q, want /w from the head", meta.Cwd)
	}
	if meta.GitBranch != "main" {
		t.Errorf("GitBranch = %q, want main from the head", meta.GitBranch)
	}
}

// A file inside the head budget must be read whole, so small sessions keep
// exactly the behavior a full scan gave them.
func TestSummarizeSmallSessionUnchangedByBudget(t *testing.T) {
	path := filepath.Join("testdata", "claudecode_basic.jsonl")

	meta, err := ClaudeCodeAdapter{}.Summarize(t.Context(), path)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if meta.ID != "cc-session-1" {
		t.Errorf("ID = %q", meta.ID)
	}
	if meta.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %q", meta.Model)
	}
	if meta.Cwd != "/home/user/project" {
		t.Errorf("Cwd = %q", meta.Cwd)
	}
	if meta.GitBranch != "main" {
		t.Errorf("GitBranch = %q", meta.GitBranch)
	}
}

// A sidechain session is marked auxiliary from its first line, so the head
// always sees it and ListSessions keeps excluding it.
func TestSummarizeDetectsSidechainFromFirstLine(t *testing.T) {
	pad := strings.Repeat("z", 8*1024)
	var b strings.Builder
	fmt.Fprintf(&b, `{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"side-1","isSidechain":true,"message":{"role":"user","content":"go"}}`+"\n")
	for i := range 40 {
		fmt.Fprintf(&b, `{"type":"user","timestamp":"2026-01-01T10:%02d:00Z","sessionId":"side-1","message":{"role":"user","content":%q}}`+"\n", i, pad)
	}
	path := filepath.Join(t.TempDir(), "side.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	meta, err := ClaudeCodeAdapter{}.Summarize(t.Context(), path)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !meta.IsSidechain {
		t.Error("IsSidechain = false, want true")
	}
	if !meta.Auxiliary {
		t.Error("Auxiliary = false, want true — ListSessions relies on this to exclude sidechains")
	}
}
