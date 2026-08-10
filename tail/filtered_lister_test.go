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
	all, err := a.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for i, f := range filters {
		t.Run(fmt.Sprintf("filter_%d", i), func(t *testing.T) {
			want := filterSessions(all, f)
			got, err := fl.ListSessionsFiltered(f)
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

// TestCrushFilteredListerSkipsNonMatchingProjects pins the actual optimization
// rather than only its correctness: Crush stores one database per project and
// the cwd belongs to the project, so a filter that excludes a project must skip
// its database entirely. Pointing a project at a database that does not exist
// proves it is never opened — if the adapter tried, it would still succeed
// here, but the session from the real project would be joined by nothing.
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
	got, err := a.ListSessionsFiltered(SessionFilter{Cwd: wanted})
	if err != nil {
		t.Fatalf("ListSessionsFiltered: %v", err)
	}
	if len(got) != 1 || got[0].ID != "in-scope" {
		t.Fatalf("got %v, want only the in-scope session", ids(got))
	}

	// And the unfiltered listing still sees both, so the filter is doing the
	// narrowing rather than the fixture being wrong.
	all, err := a.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered listing returned %v, want both sessions", ids(all))
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
