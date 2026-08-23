package tail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// session writes a Claude Code transcript with explicit start/end timestamps in
// its content and an explicit mtime on the file, so a test can drive the
// content-based predicate and the mtime prefilter independently.
func session(t *testing.T, root, name string, started, ended, mtime time.Time) string {
	t.Helper()
	proj := filepath.Join(root, ".claude", "projects", "p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(proj, name+".jsonl")
	stamp := func(x time.Time) string { return x.UTC().Format("2006-01-02T15:04:05.000Z") }
	body := fmt.Sprintf(
		`{"type":"user","timestamp":%q,"sessionId":%q,"cwd":"/w","message":{"role":"user","content":"hi"}}`+"\n"+
			`{"type":"assistant","timestamp":%q,"sessionId":%q,"message":{"role":"assistant","model":"m","content":[]}}`+"\n",
		stamp(started), name, stamp(ended), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return path
}

func idSet(sessions []SessionMeta) map[string]bool {
	out := map[string]bool{}
	for _, s := range sessions {
		out[s.ID] = true
	}
	return out
}

// The distinction that motivates ActiveSince: a session opened well before the
// window but still being written to is current, and must survive a bound that
// Since would fail it on.
func TestActiveSinceKeepsLongRunningSessionThatSinceWouldDrop(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-5 * 24 * time.Hour)

	// Started 5 days ago, last active a minute ago.
	session(t, root, "long-running", old, now.Add(-time.Minute), now.Add(-time.Minute))
	// Started and finished 5 days ago.
	session(t, root, "stale", old, old, old)

	a := (&ClaudeCodeAdapter{}).WithRoot(root)
	cutoff := now.Add(-48 * time.Hour)

	active, err := ListSessionsFiltered(context.Background(), a, SessionFilter{ActiveSince: cutoff})
	if err != nil {
		t.Fatalf("ActiveSince list: %v", err)
	}
	got := idSet(active)
	if !got["long-running"] {
		t.Error("ActiveSince dropped a session that is still being written to")
	}
	if got["stale"] {
		t.Error("ActiveSince kept a session last touched 5 days ago")
	}

	// Since asks a different question and answers it differently — this is
	// why ActiveSince exists rather than reusing Since.
	started, err := ListSessionsFiltered(context.Background(), a, SessionFilter{Since: cutoff})
	if err != nil {
		t.Fatalf("Since list: %v", err)
	}
	if idSet(started)["long-running"] {
		t.Error("Since kept a session that began before the bound; the two filters are supposed to differ here")
	}
}

// The mtime prefilter must skip an excluded file before it is opened, not
// summarize it and discard the result afterwards. The cache is the witness: a
// file that was never summarized leaves no entry behind.
func TestActivityWindowSkipsExcludedFilesWithoutReading(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)

	session(t, root, "fresh", now.Add(-time.Hour), now.Add(-time.Minute), now.Add(-time.Minute))
	session(t, root, "ancient", old, old, old)

	cache := NewSummaryCache()
	a := (&ClaudeCodeAdapter{}).WithRoot(root)
	a.(SummaryCacheSetter).SetSummaryCache(cache)

	got, err := ListSessionsFiltered(context.Background(), a,
		SessionFilter{ActiveSince: now.Add(-48 * time.Hour)})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "fresh" {
		t.Fatalf("listed %v, want just the fresh session", idSet(got))
	}
	if cache.Len() != 1 {
		t.Errorf("cache holds %d entries, want 1 — the excluded file was opened and summarized anyway", cache.Len())
	}
}

// FilteredLister's contract: pushing the filter down must produce exactly what
// listing everything and filtering in memory produces.
func TestFilteredListerMatchesInMemoryFiltering(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	for i, age := range []time.Duration{
		30 * time.Minute, 6 * time.Hour, 47 * time.Hour, 49 * time.Hour, 30 * 24 * time.Hour,
	} {
		at := now.Add(-age)
		session(t, root, fmt.Sprintf("s%d", i), at.Add(-time.Minute), at, at)
	}

	a := (&ClaudeCodeAdapter{}).WithRoot(root)
	if _, ok := a.(FilteredLister); !ok {
		t.Fatal("ClaudeCodeAdapter does not implement FilteredLister")
	}

	for _, window := range []time.Duration{time.Hour, 48 * time.Hour, 365 * 24 * time.Hour} {
		f := SessionFilter{ActiveSince: now.Add(-window)}

		pushed, err := a.(FilteredLister).ListSessionsFiltered(context.Background(), f)
		if err != nil {
			t.Fatalf("pushed-down list: %v", err)
		}
		all, err := a.ListSessions(context.Background())
		if err != nil {
			t.Fatalf("full list: %v", err)
		}
		inMemory := filterSessions(all, f)

		if len(pushed) != len(inMemory) {
			t.Errorf("window %s: pushed down %d sessions, in-memory filter gives %d",
				window, len(pushed), len(inMemory))
			continue
		}
		for id := range idSet(inMemory) {
			if !idSet(pushed)[id] {
				t.Errorf("window %s: pushdown dropped %q", window, id)
			}
		}
	}
}

func TestWatcherAppliesDefaultActivityWindow(t *testing.T) {
	w := NewWatcherWithConfig(WatchConfig{}, nil)
	if w.cfg.MaxAge != DefaultMaxAge {
		t.Errorf("MaxAge = %s, want the %s default", w.cfg.MaxAge, DefaultMaxAge)
	}
	f := w.sessionFilter()
	if f.ActiveSince.IsZero() {
		t.Fatal("default watcher built an unbounded filter")
	}
	if d := time.Since(f.ActiveSince); d < DefaultMaxAge-time.Minute || d > DefaultMaxAge+time.Minute {
		t.Errorf("ActiveSince is %s ago, want ~%s", d, DefaultMaxAge)
	}
}

func TestWatcherNegativeMaxAgeDisablesTheWindow(t *testing.T) {
	w := NewWatcherWithConfig(WatchConfig{MaxAge: -1}, nil)
	if f := w.sessionFilter(); f != (SessionFilter{}) {
		t.Errorf("sessionFilter() = %+v, want the zero filter for MaxAge < 0", f)
	}
}

func TestWatcherHonorsExplicitMaxAge(t *testing.T) {
	w := NewWatcherWithConfig(WatchConfig{MaxAge: 90 * time.Minute}, nil)
	if w.cfg.MaxAge != 90*time.Minute {
		t.Fatalf("MaxAge = %s, want 90m", w.cfg.MaxAge)
	}
	if d := time.Since(w.sessionFilter().ActiveSince); d < 89*time.Minute || d > 91*time.Minute {
		t.Errorf("ActiveSince is %s ago, want ~90m", d)
	}
}

// The window moves with the clock rather than being pinned at construction, so
// a watcher left running for days keeps showing recent work.
func TestWatcherActivityWindowSlides(t *testing.T) {
	w := NewWatcherWithConfig(WatchConfig{MaxAge: time.Hour}, nil)
	first := w.sessionFilter().ActiveSince
	time.Sleep(10 * time.Millisecond)
	second := w.sessionFilter().ActiveSince
	if !second.After(first) {
		t.Errorf("window did not advance: %s then %s", first, second)
	}
}
