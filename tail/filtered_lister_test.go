package tail

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// assertFilteredMatchesInMemory is the shared oracle for every FilteredLister
// implementation. FilteredLister's contract is that a pushdown must produce
// results *identical* to ListSessions followed by in-memory filtering — so the
// naive path is the specification, and every optimized path is checked against
// it rather than against hand-written expectations.
//
// This matters more than a normal equivalence test because the failure mode is
// silent: a pushdown that drops one row too many returns a plausible-looking
// shorter list, and a consumer using the filter as a scoping boundary would
// never know.
func assertFilteredMatchesInMemory(t *testing.T, a Adapter, filters []SessionFilter) {
	t.Helper()
	fl, ok := a.(FilteredLister)
	if !ok {
		t.Fatalf("%s does not implement FilteredLister", a.Harness())
	}
	all, err := a.ListSessions(t.Context())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for i, f := range filters {
		t.Run(fmt.Sprintf("filter_%d", i), func(t *testing.T) {
			want := filterSessions(all, f)
			got, err := fl.ListSessionsFiltered(t.Context(), f)
			if err != nil {
				t.Fatalf("ListSessionsFiltered: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("pushdown returned %d sessions, in-memory filtering returned %d\ngot:  %v\nwant: %v",
					len(got), len(want), ids(got), ids(want))
			}
			for j := range got {
				if got[j].ID != want[j].ID {
					t.Errorf("position %d: pushdown ID %q, in-memory ID %q (ordering must match too)",
						j, got[j].ID, want[j].ID)
				}
				if got[j].StartedAt != want[j].StartedAt || got[j].Cwd != want[j].Cwd {
					t.Errorf("position %d: metadata differs — pushdown %+v, in-memory %+v", j, got[j], want[j])
				}
			}
		})
	}
}

func ids(metas []SessionMeta) []string {
	out := make([]string, 0, len(metas))
	for _, m := range metas {
		out = append(out, m.ID)
	}
	return out
}

// filterMatrix returns filters that exercise each predicate alone, both
// together, and the boundary cases: the zero filter, a bound that matches
// everything, one that matches nothing, and a Since carrying sub-millisecond
// precision — the case where a truncating epoch-millisecond pushdown bound
// would over-select and must be corrected by the in-memory pass.
func filterMatrix(base time.Time, cwd string) []SessionFilter {
	return []SessionFilter{
		{},
		{Cwd: cwd},
		{Cwd: cwd + string(filepath.Separator)},
		{Cwd: filepath.Join(cwd, "nope")},
		{Since: base},
		{Since: base.Add(-time.Hour)},
		{Since: base.Add(24 * time.Hour)},
		{Since: base.Add(500 * time.Microsecond)},
		{Cwd: cwd, Since: base},
		{Cwd: cwd, Since: base.Add(24 * time.Hour)},
	}
}

func TestCrushFilteredListerMatchesInMemory(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "project")
	dbPath := filepath.Join(dir, "data", "crush.db")
	createTestCrushDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Hour).UnixMilli()
		if _, err := db.ExecContext(context.Background(),
			`INSERT INTO sessions (id, parent_session_id, title, created_at, updated_at) VALUES (?, '', ?, ?, ?)`,
			fmt.Sprintf("session-%d", i), fmt.Sprintf("title %d", i), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	a := CrushAdapter{DBPath: dbPath, Cwd: cwd}
	assertFilteredMatchesInMemory(t, a, filterMatrix(base, cwd))
}

func TestOpenCodeFilteredListerMatchesInMemory(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "project")
	dbPath := filepath.Join(dir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Hour).UnixMilli()
		if _, err := db.ExecContext(context.Background(),
			`INSERT INTO session (id, project_id, slug, directory, title, version, parent_id, model, time_created, time_updated)
			 VALUES (?, 'proj', 'slug', ?, ?, '1', NULL, NULL, ?, ?)`,
			fmt.Sprintf("session-%d", i), cwd, fmt.Sprintf("title %d", i), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	a := OpenCodeAdapter{DBPath: dbPath}
	assertFilteredMatchesInMemory(t, a, filterMatrix(base, cwd))
}

// TestCrushFilteredListerSkipsNonMatchingProjects covers the multi-project
// path: Crush stores one database per project and the cwd belongs to the
// project, so a cwd filter must narrow across databases and not merely within
// one.
//
// It pins the observable result, not the optimization. Deleting the per-project
// skip from ListSessionsFiltered leaves this test green, because the trailing
// filterSessions pass drops the out-of-scope sessions either way — the skip is
// only visible as I/O not performed, and every failure to open a database is
// swallowed by the same `continue` that makes a missing database a non-error.
// Pinning it would take an injectable opener; until then, treat the skip as an
// optimization this suite does not defend.
func TestCrushFilteredListerSkipsNonMatchingProjects(t *testing.T) {
	dir := t.TempDir()
	wanted := filepath.Join(dir, "wanted")
	other := filepath.Join(dir, "other")

	wantedDB := filepath.Join(dir, "wanted-data", "crush.db")
	otherDB := filepath.Join(dir, "other-data", "crush.db")
	createTestCrushDB(t, wantedDB)
	createTestCrushDB(t, otherDB)

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	for path, id := range map[string]string{wantedDB: "in-scope", otherDB: "out-of-scope"} {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(),
			`INSERT INTO sessions (id, parent_session_id, title, created_at, updated_at) VALUES (?, '', 'x', ?, ?)`,
			id, base, base); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}

	projects := filepath.Join(dir, "projects.json")
	writeProjectsJSON(t, projects, map[string]string{
		wanted: filepath.Dir(wantedDB),
		other:  filepath.Dir(otherDB),
	})

	a := CrushAdapter{ProjectsPath: projects}
	got, err := a.ListSessionsFiltered(t.Context(), SessionFilter{Cwd: wanted})
	if err != nil {
		t.Fatalf("ListSessionsFiltered: %v", err)
	}
	if len(got) != 1 || got[0].ID != "in-scope" {
		t.Fatalf("got %v, want only the in-scope session", ids(got))
	}

	// And the unfiltered listing still sees both, so the filter is doing the
	// narrowing rather than the fixture being wrong.
	all, err := a.ListSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered listing returned %v, want both sessions", ids(all))
	}
}

// TestCrushFilteredListerMatchesInMemoryOnTies is the case the single-project,
// strictly-increasing-timestamp fixtures above cannot reach, and the one the
// ordering half of the contract actually turns on.
//
// Two things have to be true at once for the pushdown to diverge: the slice fed
// to the sort must differ between the two paths (multiple projects, some of
// them skipped by the cwd pushdown or thinned by the SQL bound), and sessions
// must tie on the sort key. Ties are not exotic — updated_at is whole
// seconds, so a migration or a burst of activity produces them, and every row
// with updated_at <= 0 ties at "" because secToRFC3339 maps them all to the
// empty string.
//
// Before sortSessionsByRecency imposed a total order, this failed with roughly
// two thirds of the positions mismatched.
func TestCrushFilteredListerMatchesInMemoryOnTies(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	group := filepath.Join(dir, "group")
	cwds := []string{
		filepath.Join(group, "a"),
		filepath.Join(group, "b"),
		filepath.Join(group, "c"),
		filepath.Join(dir, "elsewhere"),
	}
	cwdToDataDir := map[string]string{}
	for p, cwd := range cwds {
		dbPath := filepath.Join(dir, fmt.Sprintf("data%d", p), "crush.db")
		createTestCrushDB(t, dbPath)
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 8 {
			// updated_at (the sort key) draws from a handful of values so rows
			// tie heavily; created_at (the Since key) spreads out so the bound
			// prunes a different subset than the cwd skip does.
			updated := base.Add(time.Duration(i%3) * time.Hour).Unix()
			created := base.Add(time.Duration(i*17) * time.Minute).Unix()
			if _, err := db.ExecContext(context.Background(),
				`INSERT INTO sessions (id, parent_session_id, title, created_at, updated_at) VALUES (?, '', 'x', ?, ?)`,
				fmt.Sprintf("p%d-s%d", p, i), created, updated); err != nil {
				t.Fatal(err)
			}
		}
		db.Close()
		cwdToDataDir[cwd] = filepath.Dir(dbPath)
	}
	projects := filepath.Join(dir, "projects.json")
	writeProjectsJSON(t, projects, cwdToDataDir)

	a := CrushAdapter{ProjectsPath: projects}
	assertFilteredMatchesInMemory(t, a, []SessionFilter{
		{},
		{Cwd: group},
		{Cwd: cwds[0]},
		{Since: base.Add(30 * time.Minute)},
		{Since: base.Add(90 * time.Minute)},
		{Cwd: group, Since: base.Add(30 * time.Minute)},
		{Cwd: group, Since: base.Add(500 * time.Microsecond)},
	})
}

// TestSortSessionsByRecencyOrdersByInstant pins the half of the ordering rule
// that a tie test cannot see: RFC3339Nano drops trailing fractional zeros, so
// comparing EndedAt as text ranks a whole second above a later fractional one
// in the same second ('Z' > '.'). The listing claims to be newest-first, and
// SQLite already returned these rows in the right order — a text comparison in
// Go put them back out of order.
func TestSortSessionsByRecencyOrdersByInstant(t *testing.T) {
	whole := msToRFC3339(1767268800000) // 2026-01-01T12:00:00Z
	frac := msToRFC3339(1767268800500)  // 2026-01-01T12:00:00.5Z — 500ms later
	if whole <= frac {
		t.Fatalf("fixture no longer exercises the trap: %q vs %q", whole, frac)
	}
	metas := []SessionMeta{
		{ID: "whole", Key: "k-whole", EndedAt: whole},
		{ID: "frac", Key: "k-frac", EndedAt: frac},
		{ID: "unknown", Key: "k-unknown"},
	}
	sortSessionsByRecency(metas)
	if got := ids(metas); got[0] != "frac" || got[1] != "whole" || got[2] != "unknown" {
		t.Errorf("got %v, want [frac whole unknown] (newest instant first, unknown last)", got)
	}
}

// TestSortSessionsByRecencyIsTotal pins the tiebreak itself: equal instants
// must resolve deterministically, or sorting a subset can order them
// differently than sorting the whole list.
func TestSortSessionsByRecencyIsTotal(t *testing.T) {
	ended := msToRFC3339(1767268800000)
	full := []SessionMeta{
		{ID: "c", Key: "k-c", EndedAt: ended},
		{ID: "a", Key: "k-a", EndedAt: ended},
		{ID: "d", Key: "k-d", EndedAt: ended},
		{ID: "b", Key: "k-b", EndedAt: ended},
	}
	sortSessionsByRecency(full)
	if got := ids(full); got[0] != "a" || got[3] != "d" {
		t.Fatalf("ties did not resolve by Key: %v", got)
	}
	subset := []SessionMeta{full[3], full[1]} // reversed relative order
	sortSessionsByRecency(subset)
	if ids(subset)[0] != full[1].ID {
		t.Errorf("subset order %v disagrees with the full order %v", ids(subset), ids(full))
	}
}

// TestCrushShortSessionIDTitleFallback covers a sessions row whose id is
// shorter than the 8 bytes the untitled-session fallback slices off. The
// listing runs from ListSessions, ListSessionsFiltered and Summarize, so an
// unguarded slice panicked all three — and, once Crush is in DefaultAdapters,
// the watcher's poll loop with them.
func TestCrushShortSessionIDTitleFallback(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "crush.db")
	createTestCrushDB(t, dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO sessions (id, parent_session_id, title, created_at, updated_at) VALUES ('abc', '', '', ?, ?)`,
		ts, ts); err != nil {
		t.Fatal(err)
	}
	db.Close()

	a := CrushAdapter{DBPath: dbPath, Cwd: filepath.Join(dir, "project")}
	metas, err := a.ListSessions(t.Context())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("got %d sessions, want 1", len(metas))
	}
	if metas[0].Title != "project — abc" {
		t.Errorf("title = %q, want %q", metas[0].Title, "project — abc")
	}
	if _, err := a.ListSessionsFiltered(t.Context(), SessionFilter{Cwd: filepath.Join(dir, "project")}); err != nil {
		t.Errorf("ListSessionsFiltered: %v", err)
	}
}

// TestOpenCodeFilteredListerEmptyResultShape pins the shape of an empty
// result, not just its length: the generic path in ListSessionsFiltered runs
// filterSessions, which returns a non-nil empty slice under a non-zero filter,
// and a pushdown that short-circuits to a bare nil would marshal as null where
// the generic path marshals as [].
func TestOpenCodeFilteredListerEmptyResultShape(t *testing.T) {
	a := OpenCodeAdapter{DBPath: filepath.Join(t.TempDir(), "absent.db")}
	got, err := a.ListSessionsFiltered(t.Context(), SessionFilter{Cwd: "/nowhere"})
	if err != nil {
		t.Fatalf("ListSessionsFiltered: %v", err)
	}
	if got == nil {
		t.Error("pushdown returned a nil slice where in-memory filtering returns an empty one")
	}
	if len(got) != 0 {
		t.Errorf("got %d sessions from an absent database", len(got))
	}
}

// TestSinceLowerBoundMsNeverExcludes is the property the SQL pushdown rests on:
// the coarse bound must be at or before Since, so `stored_ms >= bound` cannot
// drop a row the exact filter would keep. Sub-millisecond precision is the case
// that matters — UnixMilli truncates, and truncating the wrong way would turn
// an over-select into a silent under-select.
func TestSinceLowerBoundMsNeverExcludes(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, extra := range []time.Duration{0, time.Nanosecond, 500 * time.Microsecond, 999999 * time.Nanosecond} {
		since := base.Add(extra)
		bound, ok := sinceLowerBoundMs(SessionFilter{Since: since})
		if !ok {
			t.Fatalf("no bound for a non-zero Since")
		}
		if time.UnixMilli(bound).After(since) {
			t.Errorf("bound %v is after Since %v — the pushdown would drop qualifying rows", time.UnixMilli(bound), since)
		}
	}
	if _, ok := sinceLowerBoundMs(SessionFilter{}); ok {
		t.Error("zero Since reported a bound; the pushdown must not filter at all")
	}
}

// TestSinceLowerBoundSecNeverExcludes is TestSinceLowerBoundMsNeverExcludes
// for second-precision storage (Crush). The sub-second case matters just as
// much: Unix() truncates downward, so a Since mid-second must not round up and
// silently drop the session created in that second.
func TestSinceLowerBoundSecNeverExcludes(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, extra := range []time.Duration{0, time.Nanosecond, 500 * time.Millisecond, 999999999 * time.Nanosecond} {
		since := base.Add(extra)
		bound, ok := sinceLowerBoundSec(SessionFilter{Since: since})
		if !ok {
			t.Fatalf("no bound for a non-zero Since")
		}
		if time.Unix(bound, 0).After(since) {
			t.Errorf("bound %v is after Since %v — the pushdown would drop qualifying rows", time.Unix(bound, 0), since)
		}
	}
	if _, ok := sinceLowerBoundSec(SessionFilter{}); ok {
		t.Error("zero Since reported a bound; the pushdown must not filter at all")
	}
}

// writeProjectsJSON writes a Crush projects.json mapping working directories to
// their data directories, so a test can exercise the multi-project path without
// depending on a real Crush installation.
func writeProjectsJSON(t *testing.T, path string, cwdToDataDir map[string]string) {
	t.Helper()
	var pf crushProjectsFile
	for cwd, dataDir := range cwdToDataDir {
		pf.Projects = append(pf.Projects, crushProjectEntry{Path: cwd, DataDir: dataDir})
	}
	// Deterministic order: dbPaths preserves file order, and the assertions
	// compare against in-memory filtering of the same listing.
	sort.Slice(pf.Projects, func(i, j int) bool { return pf.Projects[i].Path < pf.Projects[j].Path })
	data, err := json.Marshal(pf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
