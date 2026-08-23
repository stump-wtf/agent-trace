package tail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ccSession writes a minimal Claude Code transcript whose title identifies it,
// then stamps a fixed mtime so tests control cache invalidation explicitly.
func ccSession(t *testing.T, dir, name, title string, mtime time.Time) string {
	t.Helper()
	proj := filepath.Join(dir, ".claude", "projects", "p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(proj, name+".jsonl")
	body := fmt.Sprintf(
		`{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":%q,"cwd":"/w","message":{"role":"user","content":"hi"}}`+"\n"+
			`{"type":"ai-title","timestamp":"2026-01-01T10:00:01Z","sessionId":%q,"aiTitle":%q}`+"\n",
		name, name, title)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return path
}

func titleOf(t *testing.T, a Adapter, id string) string {
	t.Helper()
	sessions, err := a.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, s := range sessions {
		if s.ID == id {
			return s.Title
		}
	}
	t.Fatalf("session %q not listed", id)
	return ""
}

// The cache must actually prevent the re-read, not merely produce equal
// results. Rewriting the file while holding size and mtime fixed is
// indistinguishable from no change, so a cached adapter keeps serving the old
// title and an uncached one picks up the new one. That difference is the proof.
func TestSummaryCacheServesUnchangedFileWithoutRereading(t *testing.T) {
	root := t.TempDir()
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	path := ccSession(t, root, "s1", "AAAA", stamp)

	adapter := &ClaudeCodeAdapter{}
	cached := adapter.WithRoot(root)
	cached.(SummaryCacheSetter).SetSummaryCache(NewSummaryCache())

	if got := titleOf(t, cached, "s1"); got != "AAAA" {
		t.Fatalf("first read title = %q, want AAAA", got)
	}

	// Same byte count, same mtime, different content.
	rewritten := ccSession(t, root, "s1", "BBBB", stamp)
	if rewritten != path {
		t.Fatalf("fixture moved: %s", rewritten)
	}

	if got := titleOf(t, cached, "s1"); got != "AAAA" {
		t.Errorf("cached read title = %q, want the cached AAAA — the file was re-read", got)
	}

	// A fresh adapter with no cache sees the new content, confirming the
	// rewrite really happened and the test is not proving a tautology.
	fresh := (&ClaudeCodeAdapter{}).WithRoot(root)
	if got := titleOf(t, fresh, "s1"); got != "BBBB" {
		t.Errorf("uncached read title = %q, want BBBB", got)
	}
}

func TestSummaryCacheInvalidatesOnModTimeChange(t *testing.T) {
	root := t.TempDir()
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	ccSession(t, root, "s1", "AAAA", stamp)

	cached := (&ClaudeCodeAdapter{}).WithRoot(root)
	cached.(SummaryCacheSetter).SetSummaryCache(NewSummaryCache())
	if got := titleOf(t, cached, "s1"); got != "AAAA" {
		t.Fatalf("first read title = %q", got)
	}

	ccSession(t, root, "s1", "BBBB", stamp.Add(time.Minute))
	if got := titleOf(t, cached, "s1"); got != "BBBB" {
		t.Errorf("title = %q, want BBBB after mtime moved", got)
	}
}

func TestSummaryCacheInvalidatesOnSizeChange(t *testing.T) {
	root := t.TempDir()
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	ccSession(t, root, "s1", "AAAA", stamp)

	cached := (&ClaudeCodeAdapter{}).WithRoot(root)
	cached.(SummaryCacheSetter).SetSummaryCache(NewSummaryCache())
	if got := titleOf(t, cached, "s1"); got != "AAAA" {
		t.Fatalf("first read title = %q", got)
	}

	// Longer title, same mtime: size alone must invalidate.
	ccSession(t, root, "s1", "AAAA-much-longer-title", stamp)
	if got := titleOf(t, cached, "s1"); got != "AAAA-much-longer-title" {
		t.Errorf("title = %q, want the longer title after size changed", got)
	}
}

// A .jsonl that is not a session for this harness fails to summarize on every
// scan. Caching that failure is the point: otherwise each foreign file is read
// in full, forever.
func TestSummaryCacheRemembersNonSessionFiles(t *testing.T) {
	root := t.TempDir()
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	ccSession(t, root, "s1", "AAAA", stamp)

	proj := filepath.Join(root, ".claude", "projects", "p")
	foreign := filepath.Join(proj, "foreign.jsonl")
	if err := os.WriteFile(foreign, []byte("{\"unrelated\":true}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(foreign, stamp, stamp); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	cache := NewSummaryCache()
	cached := (&ClaudeCodeAdapter{}).WithRoot(root)
	cached.(SummaryCacheSetter).SetSummaryCache(cache)

	for range 2 {
		sessions, err := cached.ListSessions(context.Background())
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("listed %d sessions, want 1 (the foreign file must stay excluded)", len(sessions))
		}
	}
	// Both the session and the rejected file are remembered.
	if cache.Len() != 2 {
		t.Errorf("cache.Len() = %d, want 2 (session + remembered rejection)", cache.Len())
	}
}

func TestSummaryCacheSweepDropsVanishedFiles(t *testing.T) {
	root := t.TempDir()
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	ccSession(t, root, "s1", "AAAA", stamp)
	path2 := ccSession(t, root, "s2", "BBBB", stamp)

	cache := NewSummaryCache()
	cached := (&ClaudeCodeAdapter{}).WithRoot(root)
	cached.(SummaryCacheSetter).SetSummaryCache(cache)

	if _, err := cached.ListSessions(context.Background()); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if cache.Len() != 2 {
		t.Fatalf("cache.Len() = %d, want 2", cache.Len())
	}
	cache.Sweep() // both were touched this generation, both survive
	if cache.Len() != 2 {
		t.Fatalf("after first sweep cache.Len() = %d, want 2", cache.Len())
	}

	if err := os.Remove(path2); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := cached.ListSessions(context.Background()); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	cache.Sweep() // s2 was not touched this generation
	if cache.Len() != 1 {
		t.Errorf("after sweep cache.Len() = %d, want 1 — the removed file should be evicted", cache.Len())
	}
}

// An adapter without a cache must behave exactly as before.
func TestSummaryCacheNilIsSafe(t *testing.T) {
	root := t.TempDir()
	ccSession(t, root, "s1", "AAAA", time.Now().Add(-time.Hour))

	uncached := (&ClaudeCodeAdapter{}).WithRoot(root)
	if got := titleOf(t, uncached, "s1"); got != "AAAA" {
		t.Errorf("title = %q, want AAAA", got)
	}

	var nilCache *SummaryCache
	if nilCache.Len() != 0 {
		t.Error("nil cache Len() != 0")
	}
	nilCache.Sweep() // must not panic
}

// The watcher installs a shared cache on every adapter that accepts one.
func TestWatcherInstallsSummaryCache(t *testing.T) {
	adapters := DefaultAdaptersIn(t.TempDir())
	w := NewWatcherWithConfig(WatchConfig{}, adapters)
	if w.summaries == nil {
		t.Fatal("watcher has no summary cache")
	}
	installed := 0
	for _, a := range adapters {
		switch v := a.(type) {
		case *ClaudeCodeAdapter:
			if v.cache == w.summaries {
				installed++
			}
		case *CodexAdapter:
			if v.cache == w.summaries {
				installed++
			}
		case *PiAdapter:
			if v.cache == w.summaries {
				installed++
			}
		}
	}
	if installed != 3 {
		t.Errorf("cache installed on %d adapters, want 3 (claude-code, codex, pi)", installed)
	}
}
