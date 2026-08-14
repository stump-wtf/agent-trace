package tail

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
)

// --- Test helpers ---

// staticAdapter returns a fixed set of sessions for testing filtering.
type staticAdapter struct {
	sessions []SessionMeta
	err      error
}

func (s staticAdapter) Harness() Harness        { return "static" }
func (s staticAdapter) SessionDir() string      { return "/fake" }
func (s staticAdapter) WithRoot(string) Adapter { return s }
func (s staticAdapter) ListSessions() ([]SessionMeta, error) {
	return s.sessions, s.err
}
func (s staticAdapter) Parse(string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
	return nil, nil, SessionMeta{}, nil
}

func makeSession(id, cwd, startedAt string) SessionMeta {
	return SessionMeta{
		Key:       id,
		ID:        id,
		Harness:   "static",
		Path:      "/fake/" + id,
		Cwd:       cwd,
		StartedAt: startedAt,
	}
}

// --- Zero-value filter ---

func TestFilterZeroValueMatchesAll(t *testing.T) {
	sessions := []SessionMeta{
		makeSession("s1", "/home/joe/project-a", "2026-01-01T10:00:00Z"),
		makeSession("s2", "/home/joe/project-b", "2026-06-01T10:00:00Z"),
	}
	a := staticAdapter{sessions: sessions}
	got, err := ListSessionsFiltered(a, SessionFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("zero-value filter should match all, got %d sessions", len(got))
	}
}

// --- Cwd filtering ---

func TestFilterCwdExactMatch(t *testing.T) {
	sessions := []SessionMeta{
		makeSession("s1", "/home/joe/project-a", "2026-01-01T10:00:00Z"),
		makeSession("s2", "/home/joe/project-b", "2026-01-01T10:00:00Z"),
		makeSession("s3", "", "2026-01-01T10:00:00Z"),
	}
	a := staticAdapter{sessions: sessions}
	got, err := ListSessionsFiltered(a, SessionFilter{Cwd: "/home/joe/project-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 session, got %d", len(got))
	}
	if got[0].ID != "s1" {
		t.Errorf("got session %s, want s1", got[0].ID)
	}
}

func TestFilterCwdPrefixMatch(t *testing.T) {
	sessions := []SessionMeta{
		makeSession("s1", "/home/joe/project-a", "2026-01-01T10:00:00Z"),
		makeSession("s2", "/home/joe/project-a/sub", "2026-01-01T10:00:00Z"),
		makeSession("s3", "/home/joe/project-b", "2026-01-01T10:00:00Z"),
	}
	a := staticAdapter{sessions: sessions}
	got, err := ListSessionsFiltered(a, SessionFilter{Cwd: "/home/joe/project-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions (exact + prefix), got %d", len(got))
	}
}

func TestFilterCwdNoFalsePositivePrefix(t *testing.T) {
	// "/home/joe" should NOT match "/home/joey" — path component, not string prefix.
	sessions := []SessionMeta{
		makeSession("s1", "/home/joey/project", "2026-01-01T10:00:00Z"),
		makeSession("s2", "/home/joe/project", "2026-01-01T10:00:00Z"),
	}
	a := staticAdapter{sessions: sessions}
	got, err := ListSessionsFiltered(a, SessionFilter{Cwd: "/home/joe"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 session, got %d (false positive prefix match?)", len(got))
	}
	if got[0].ID != "s2" {
		t.Errorf("got session %s, want s2", got[0].ID)
	}
}

func TestFilterCwdEmptySessionCwdDoesNotMatch(t *testing.T) {
	sessions := []SessionMeta{
		makeSession("s1", "", "2026-01-01T10:00:00Z"),
	}
	a := staticAdapter{sessions: sessions}
	got, err := ListSessionsFiltered(a, SessionFilter{Cwd: "/home/joe"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty Cwd session should not match, got %d", len(got))
	}
}

// --- Since filtering ---

func TestFilterSince(t *testing.T) {
	sessions := []SessionMeta{
		makeSession("old", "/proj", "2026-01-01T10:00:00Z"),
		makeSession("mid", "/proj", "2026-06-01T10:00:00Z"),
		makeSession("new", "/proj", "2026-12-01T10:00:00Z"),
	}
	since := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	a := staticAdapter{sessions: sessions}
	got, err := ListSessionsFiltered(a, SessionFilter{Since: since})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions (mid + new), got %d", len(got))
	}
	for _, s := range got {
		if s.ID == "old" {
			t.Error("old session should have been filtered out")
		}
	}
}

func TestFilterSinceBoundaryInclusive(t *testing.T) {
	// A session starting exactly at the Since boundary should pass.
	sessions := []SessionMeta{
		makeSession("exact", "/proj", "2026-06-01T10:00:00Z"),
	}
	since := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	a := staticAdapter{sessions: sessions}
	got, err := ListSessionsFiltered(a, SessionFilter{Since: since})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("boundary should be inclusive, got %d sessions", len(got))
	}
}

func TestFilterSinceExcludesMissingTimestamp(t *testing.T) {
	// A session with no StartedAt should NOT pass a Since filter.
	// "Unknown" is not within the time bound.
	sessions := []SessionMeta{
		makeSession("missing-ts", "/proj", ""),
		makeSession("has-ts", "/proj", "2026-06-01T10:00:00Z"),
	}
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := staticAdapter{sessions: sessions}
	got, err := ListSessionsFiltered(a, SessionFilter{Since: since})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 session (missing-ts excluded), got %d", len(got))
	}
	if got[0].ID != "has-ts" {
		t.Errorf("got %s, want has-ts", got[0].ID)
	}
}

func TestFilterSinceExcludesGarbageTimestamp(t *testing.T) {
	sessions := []SessionMeta{
		makeSession("garbage", "/proj", "not-a-date"),
		makeSession("valid", "/proj", "2026-06-01T10:00:00Z"),
	}
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := staticAdapter{sessions: sessions}
	got, err := ListSessionsFiltered(a, SessionFilter{Since: since})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 session, got %d", len(got))
	}
	if got[0].ID != "valid" {
		t.Errorf("got %s, want valid", got[0].ID)
	}
}

// --- Combined filters ---

func TestFilterCwdAndSince(t *testing.T) {
	sessions := []SessionMeta{
		makeSession("s1", "/proj-a", "2026-01-01T10:00:00Z"),
		makeSession("s2", "/proj-a", "2026-06-01T10:00:00Z"),
		makeSession("s3", "/proj-b", "2026-06-01T10:00:00Z"),
	}
	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	a := staticAdapter{sessions: sessions}
	got, err := ListSessionsFiltered(a, SessionFilter{
		Cwd:   "/proj-a",
		Since: since,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 session matching both filters, got %d", len(got))
	}
	if got[0].ID != "s2" {
		t.Errorf("got %s, want s2", got[0].ID)
	}
}

// --- Error propagation ---

func TestFilterPropagatesError(t *testing.T) {
	wantErr := errors.New("disk on fire")
	a := staticAdapter{err: wantErr}
	_, err := ListSessionsFiltered(a, SessionFilter{Cwd: "/proj"})
	if !errors.Is(err, wantErr) {
		t.Errorf("expected error %v, got %v", wantErr, err)
	}
}

// --- FilteredLister interface delegation ---

// filteredAdapter implements FilteredLister to verify that
// ListSessionsFiltered delegates to it instead of in-memory filtering.
type filteredAdapter struct {
	staticAdapter
	called   bool
	lastCall SessionFilter
}

func (f *filteredAdapter) ListSessionsFiltered(filter SessionFilter) ([]SessionMeta, error) {
	f.called = true
	f.lastCall = filter
	return []SessionMeta{makeSession("pushed-down", "/deep", "2026-01-01T00:00:00Z")}, nil
}

func TestListSessionsFilteredDelegatesToFilteredLister(t *testing.T) {
	fa := &filteredAdapter{}
	got, err := ListSessionsFiltered(Adapter(fa), SessionFilter{Cwd: "/test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fa.called {
		t.Error("expected FilteredLister.ListSessionsFiltered to be called")
	}
	if fa.lastCall.Cwd != "/test" {
		t.Errorf("filter passed to delegate = %+v, want Cwd=/test", fa.lastCall)
	}
	if len(got) != 1 || got[0].ID != "pushed-down" {
		t.Errorf("expected delegate result, got %+v", got)
	}
}

// --- Integration: real adapter with temp dir ---

func TestListSessionsFilteredClaudeCode(t *testing.T) {
	dir := t.TempDir()
	// Create two session files with different cwds.
	cc1 := `{"type":"user","timestamp":"2026-06-01T10:00:00Z","sessionId":"s1","cwd":"/home/joe/project-a","message":{"role":"user","content":"hello"}}`
	cc2 := `{"type":"user","timestamp":"2026-06-01T11:00:00Z","sessionId":"s2","cwd":"/home/joe/project-b","message":{"role":"user","content":"hello"}}`
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(cc1), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s2.jsonl"), []byte(cc2), 0644); err != nil {
		t.Fatal(err)
	}

	adapter := ClaudeCodeAdapter{Dir: dir}

	t.Run("Cwd filter", func(t *testing.T) {
		got, err := ListSessionsFiltered(adapter, SessionFilter{Cwd: "/home/joe/project-a"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 session, got %d", len(got))
		}
		if got[0].ID != "s1" {
			t.Errorf("got session %s, want s1", got[0].ID)
		}
	})

	t.Run("Since filter", func(t *testing.T) {
		since := time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC)
		got, err := ListSessionsFiltered(adapter, SessionFilter{Since: since})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 session after since, got %d", len(got))
		}
		if got[0].ID != "s2" {
			t.Errorf("got session %s, want s2", got[0].ID)
		}
	})

	t.Run("zero filter returns all", func(t *testing.T) {
		got, err := ListSessionsFiltered(adapter, SessionFilter{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("expected 2 sessions with no filter, got %d", len(got))
		}
	})
}

// --- Unit tests for cwdMatches ---

func TestCwdMatches(t *testing.T) {
	tests := []struct {
		name       string
		sessionCwd string
		filterCwd  string
		want       bool
	}{
		{"exact match", "/home/joe/proj", "/home/joe/proj", true},
		{"subdirectory", "/home/joe/proj/src", "/home/joe/proj", true},
		{"no false prefix /home/joe vs /home/joey", "/home/joey/proj", "/home/joe", false},
		{"empty session cwd", "", "/home/joe", false},
		{"empty filter cwd", "/home/joe", "", false},
		{"completely different", "/opt/other", "/home/joe", false},
		{"sibling", "/home/joe-sibling", "/home/joe", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cwdMatches(tt.sessionCwd, tt.filterCwd)
			if got != tt.want {
				t.Errorf("cwdMatches(%q, %q) = %v, want %v", tt.sessionCwd, tt.filterCwd, got, tt.want)
			}
		})
	}
}

// TestCwdMatchesNormalizesPaths guards the two ways the component comparison
// broke before cleaning: a trailing separator on the filter made every
// comparison fail, and the separator was hardcoded to '/'. Both matter because
// SessionFilter.Cwd is documented as exact rather than best-effort — a caller
// using it as a scoping boundary would have silently received zero sessions
// from a path that merely ended in a slash.
func TestCwdMatchesNormalizesPaths(t *testing.T) {
	sep := string(filepath.Separator)
	base := filepath.Join(sep+"home", "joe")
	tests := []struct {
		name    string
		session string
		filter  string
		want    bool
	}{
		{"exact", base, base, true},
		{"subdirectory", filepath.Join(base, "proj"), base, true},
		{"trailing separator on filter", filepath.Join(base, "proj"), base + sep, true},
		{"trailing separator, exact", base, base + sep, true},
		{"redundant dot element", filepath.Join(base, "proj"), filepath.Join(base, "."), true},
		{"sibling with shared prefix", filepath.Join(sep+"home", "joey", "proj"), base, false},
		{"parent is not a match", filepath.Join(sep + "home"), base, false},
		{"unrelated", filepath.Join(sep+"srv", "proj"), base, false},
		{"empty filter never matches", base, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cwdMatches(tt.session, tt.filter); got != tt.want {
				t.Errorf("cwdMatches(%q, %q) = %v, want %v", tt.session, tt.filter, got, tt.want)
			}
		})
	}
}
