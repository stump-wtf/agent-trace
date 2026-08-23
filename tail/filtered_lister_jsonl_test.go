package tail

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Oracle coverage for the JSONL pushdowns
//
// #31 asked for the filter to be pushed into the adapters; #36 landed the
// interface and pinned its contract — a pushdown must produce results
// IDENTICAL to ListSessions followed by in-memory filtering — and #37 tracked
// the coverage. Crush and OpenCode were checked against that oracle from the
// start. The three JSONL adapters gained an mtime pushdown in #81 and were
// not, and Codex and Pi had no equivalence test of any kind.
//
// That is the coverage gap worth closing rather than the missing
// implementations, because the failure this oracle catches is silent: a
// pushdown that skips one file too many returns a plausible shorter list, and
// a consumer using the filter as a scoping boundary cannot tell.
//
// @joestump-agent 08/23/2026 - Added for #37.

// codexSessionFile writes a Codex rollout with explicit content timestamps and
// an explicit mtime, so the content predicate and the mtime prefilter can be
// driven apart — the same split the Claude Code `session` helper makes.
func codexSessionFile(t *testing.T, root, name, cwd string, started, ended, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(root, ".codex", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stamp := func(x time.Time) string { return x.UTC().Format(time.RFC3339Nano) }
	path := filepath.Join(dir, name+".jsonl")
	body := fmt.Sprintf(
		`{"type":"session_meta","timestamp":%q,"payload":{"id":%q,"cwd":%q}}`+"\n"+
			`{"type":"event_msg","timestamp":%q,"payload":{"type":"agent_message"}}`+"\n",
		stamp(started), name, cwd, stamp(ended))
	writeWithMtime(t, path, body, mtime)
	return path
}

// piSessionFile writes a Pi session tree with the same split.
func piSessionFile(t *testing.T, root, name, cwd string, started, ended, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(root, ".pi", "agent", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stamp := func(x time.Time) string { return x.UTC().Format(time.RFC3339Nano) }
	path := filepath.Join(dir, name+".jsonl")
	body := fmt.Sprintf(
		`{"type":"session","id":%q,"timestamp":%q,"cwd":%q}`+"\n"+
			`{"type":"message","id":"m1","parentId":%q,"timestamp":%q,"message":{"role":"user","content":"hi"}}`+"\n",
		name, stamp(started), cwd, name, stamp(ended))
	writeWithMtime(t, path, body, mtime)
	return path
}

func writeWithMtime(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// jsonlFilterMatrix extends filterMatrix with the bound the JSONL adapters
// actually push down. filterMatrix predates ActiveSince and exercises Cwd and
// Since only, which for an mtime pushdown leaves the interesting half untested:
// mtimeExcludes decides on ActiveSince first.
//
// The spread of instants matters as much as the fields. A bound between two
// sessions is the only one that can catch an off-by-one skip; bounds that
// match everything or nothing pass against almost any broken implementation.
// The ages that land EXACTLY on a session's mtime are load-bearing for the
// same reason: both bounds are documented inclusive ("at or after this
// instant"), and an mtime prefilter that reached for `!After` instead of
// `Before` would drop precisely those sessions and nothing else. Without a
// bound sitting on a fixture, that mutation passes the whole matrix.
func jsonlFilterMatrix(base time.Time, cwd string) []SessionFilter {
	out := filterMatrix(base, cwd)
	for _, d := range []time.Duration{
		-8 * 24 * time.Hour, -25 * time.Hour, -24 * time.Hour, -3 * time.Hour,
		-2 * time.Hour, -90 * time.Minute, -time.Hour, -45 * time.Minute,
		-30 * time.Minute, -15 * time.Minute, time.Hour,
	} {
		out = append(out,
			SessionFilter{ActiveSince: base.Add(d)},
			SessionFilter{Cwd: cwd, ActiveSince: base.Add(d)},
			SessionFilter{Since: base.Add(-24 * time.Hour), ActiveSince: base.Add(d)},
			// Since gets the same treatment: mtimeExcludes applies it to the
			// file's mtime, and filterMatrix's bounds are all relative to
			// `base` rather than to a fixture, so none of them can land on
			// one.
			SessionFilter{Since: base.Add(d)},
			SessionFilter{Cwd: cwd, Since: base.Add(d)},
		)
	}
	return out
}

// jsonlOracleCorpus writes one session per adapter-specific writer at a spread
// of ages, with content timestamps and mtimes that agree — the ordinary case —
// plus one session whose mtime is NEWER than its content. That last one is the
// direction the pushdown is allowed to be wrong in: mtimeExcludes may
// over-select, and filterSessions must then be what actually decides.
func jsonlOracleCorpus(t *testing.T, root, cwd string, write func(*testing.T, string, string, string, time.Time, time.Time, time.Time) string) time.Time {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	for i, age := range []time.Duration{
		15 * time.Minute, 90 * time.Minute, 3 * time.Hour, 25 * time.Hour, 8 * 24 * time.Hour,
	} {
		at := now.Add(-age)
		write(t, root, fmt.Sprintf("s%d", i), cwd, at.Add(-time.Minute), at, at)
	}
	// Restored from a backup: mtime says "now", the contents say last week.
	old := now.Add(-7 * 24 * time.Hour)
	write(t, root, "restored", cwd, old.Add(-time.Minute), old, now)
	// A session whose StartedAt, EndedAt and mtime are the same instant. It is
	// the only shape that can catch a `Since` prefilter reaching for `!After`
	// instead of `Before`: everywhere else StartedAt precedes mtime, so the
	// exact predicate excludes the session anyway and the two agree by
	// accident.
	inst := now.Add(-2 * time.Hour)
	write(t, root, "instant", cwd, inst, inst, inst)
	// A different project, so Cwd has something to exclude.
	other := now.Add(-45 * time.Minute)
	write(t, root, "elsewhere", filepath.Join(root, "other"), other.Add(-time.Minute), other, other)
	return now
}

func TestCodexFilteredListerMatchesInMemory(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	now := jsonlOracleCorpus(t, root, cwd, codexSessionFile)

	assertFilteredMatchesInMemory(t, (&CodexAdapter{}).WithRoot(root), jsonlFilterMatrix(now, cwd))
}

func TestPiFilteredListerMatchesInMemory(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	now := jsonlOracleCorpus(t, root, cwd, piSessionFile)

	assertFilteredMatchesInMemory(t, (&PiAdapter{}).WithRoot(root), jsonlFilterMatrix(now, cwd))
}

// TestClaudeCodeFilteredListerMatchesInMemory puts Claude Code on the shared
// oracle too. TestFilteredListerMatchesInMemoryFiltering already checks it, but
// only over ActiveSince windows and with a hand-rolled comparison that ignores
// ordering — and ordering is part of the contract, since the pushdown sorts.
func TestClaudeCodeFilteredListerMatchesInMemory(t *testing.T) {
	root := t.TempDir()
	cwd := "/w"
	now := time.Now().Truncate(time.Second)
	for i, age := range []time.Duration{
		15 * time.Minute, 90 * time.Minute, 3 * time.Hour, 25 * time.Hour, 8 * 24 * time.Hour,
	} {
		at := now.Add(-age)
		session(t, root, fmt.Sprintf("s%d", i), at.Add(-time.Minute), at, at)
	}
	old := now.Add(-7 * 24 * time.Hour)
	session(t, root, "restored", old.Add(-time.Minute), old, now)
	inst := now.Add(-2 * time.Hour)
	session(t, root, "instant", inst, inst, inst)

	assertFilteredMatchesInMemory(t, (&ClaudeCodeAdapter{}).WithRoot(root), jsonlFilterMatrix(now, cwd))
}

// TestEveryDefaultAdapterImplementsFilteredLister is the guard #37 was filed
// for. The interface is optional, so an adapter that quietly stops
// implementing it keeps compiling and keeps returning correct — just slow —
// results, which is exactly how the gap went unnoticed the first time.
func TestEveryDefaultAdapterImplementsFilteredLister(t *testing.T) {
	for _, a := range DefaultAdapters() {
		if _, ok := a.(FilteredLister); !ok {
			t.Errorf("%T does not implement FilteredLister", a)
		}
	}
}
