package tail

import (
	"database/sql"
	"math"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// crushUsageDB builds a Crush store with the fixture helper's schema plus the
// messages.provider column real Crush has carried since mid-2025, and opens
// it for writing. The caller closes it.
func crushUsageDB(t *testing.T, withProvider bool) (*sql.DB, string) {
	t.Helper()
	resetDBCache()
	t.Cleanup(resetDBCache)
	dbPath := filepath.Join(t.TempDir(), "crush.db")
	createTestCrushDB(t, dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if withProvider {
		if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN provider TEXT`); err != nil {
			t.Fatal(err)
		}
	}
	return db, dbPath
}

// crushTurn writes one user row and one finished assistant row, the shape a
// Crush turn with no tool calls leaves, and records the session's usage the
// way Crush's updateSessionUsage does: cost accumulated, the token columns
// overwritten with the latest step's counts.
func crushTurn(t *testing.T, db *sql.DB, session string, n int, at int64, model, provider string, cost float64, prompt, completion int64) {
	t.Helper()
	id := func(role string) string { return session + "-" + role + "-" + string(rune('a'+n)) }
	if _, err := db.Exec(`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		id("user"), session, "user", `[{"type":"text","data":{"text":"next step"}}]`, at, at); err != nil {
		t.Fatal(err)
	}
	cols, args := `id, session_id, role, parts, model, created_at, updated_at, finished_at`,
		[]any{id("assistant"), session, "assistant", `[{"type":"text","data":{"text":"done"}},{"type":"finish","data":{"reason":"end_turn","time":` + strconv.FormatInt(at+4, 10) + `}}]`, model, at + 1, at + 4, at + 4}
	marks := `?,?,?,?,?,?,?,?`
	if provider != "" {
		cols, marks, args = cols+`, provider`, marks+`,?`, append(args, provider)
	}
	if _, err := db.Exec(`INSERT INTO messages (`+cols+`) VALUES (`+marks+`)`, args...); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET cost = ?, prompt_tokens = ?, completion_tokens = ?, updated_at = ? WHERE id = ?`,
		cost, prompt, completion, at+4, session); err != nil {
		t.Fatal(err)
	}
}

// TestCrushUsageIsTheSessionsRecordedCost: Crush keeps usage only on the
// session row, so its Usage is a cumulative snapshot: the cost Crush
// recorded, the model and provider of the latest assistant message, and no
// token counts — Crush's token columns hold the latest step's counts, not a
// total, and reporting them as one would make every consumer's difference
// wrong.
func TestCrushUsageIsTheSessionsRecordedCost(t *testing.T) {
	db, dbPath := crushUsageDB(t, true)
	const start = int64(1790000000)
	insertCrushRow(t, db, "s1", nil, start, start)
	crushTurn(t, db, "s1", 0, start+10, "glm-test", "zai", 0.40, 12000, 80)
	crushTurn(t, db, "s1", 1, start+60, "glm-test-air", "zai", 0.55, 15000, 120)
	_ = db.Close()

	a := &CrushAdapter{DBPath: dbPath, Cwd: "/test/project"}
	path := dbPath + "/s1"
	items, _, err := a.ParseItems(t.Context(), path)
	if err != nil {
		t.Fatalf("ParseItems: %v", err)
	}
	if len(items.Usage) != 1 {
		t.Fatalf("got %d usage items, want 1: %+v", len(items.Usage), items.Usage)
	}
	u := items.Usage[0]
	if !u.Cumulative || u.CostUSD == nil || *u.CostUSD != 0.55 {
		t.Errorf("usage = %+v (cost %v), want cumulative cost 0.55", u, u.CostUSD)
	}
	if u.Model != "glm-test-air" || u.Provider != "zai" {
		t.Errorf("model/provider = %q/%q, want the latest assistant message's glm-test-air/zai", u.Model, u.Provider)
	}
	if u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheRead != 0 || u.CacheWrite != 0 {
		t.Errorf("token counts = %+v; Crush records no token totals", u)
	}
	if want := time.Unix(start+64, 0).UTC(); !u.At.Equal(want) {
		t.Errorf("At = %v, want the session's updated_at %v", u.At, want)
	}
	if u.Seq != len(items.Events) {
		t.Errorf("Seq = %d, want %d (after everything the read delivered)", u.Seq, len(items.Events))
	}
	assertParseDropsOnlyUsage(t, a, path)
}

// TestCrushUsageAcrossTwoLifetimes: a resident Crush resumes one session
// across restarts and keeps adding to its cost, so the cumulative report a
// read after the second lifetime delivers, less the one the first lifetime's
// read delivered, is what the second lifetime spent.
func TestCrushUsageAcrossTwoLifetimes(t *testing.T) {
	db, dbPath := crushUsageDB(t, true)
	const start = int64(1790000000)
	insertCrushRow(t, db, "s1", nil, start, start)
	crushTurn(t, db, "s1", 0, start+10, "glm-test", "zai", 0.40, 12000, 80)

	a := &CrushAdapter{DBPath: dbPath, Cwd: "/test/project"}
	path := dbPath + "/s1"
	first, _, err := a.ParseItems(t.Context(), path)
	if err != nil {
		t.Fatalf("ParseItems: %v", err)
	}
	wm := a.Watermark(t.Context(), path)

	// The second lifetime: a restarted Crush resumes the session.
	crushTurn(t, db, "s1", 1, start+3600, "glm-test", "zai", 1.15, 9000, 40)
	crushTurn(t, db, "s1", 2, start+3700, "glm-test", "zai", 1.60, 11000, 60)
	_ = db.Close()

	second, _, _, err := a.ParseItemsSince(t.Context(), path, wm, len(first.Events))
	if err != nil {
		t.Fatalf("ParseItemsSince: %v", err)
	}
	if len(first.Usage) != 1 || len(second.Usage) != 1 {
		t.Fatalf("usage items: first lifetime %d, second %d; want 1 each", len(first.Usage), len(second.Usage))
	}
	before, after := first.Usage[0], second.Usage[0]
	if !before.Cumulative || !after.Cumulative {
		t.Fatal("Crush usage must be marked cumulative")
	}
	if delta := *after.CostUSD - *before.CostUSD; math.Abs(delta-1.20) > 1e-9 {
		t.Errorf("second lifetime's cost = %v - %v = %v, want 1.20", *after.CostUSD, *before.CostUSD, delta)
	}
}

// TestCrushParseSinceDeliversUsageOnce: a poll that reads no new rows reports
// no usage, so repeated polls of an unchanged session deliver the snapshot
// once; each poll that does advance delivers one snapshot, and the last one
// carries the session's current cost.
func TestCrushParseSinceDeliversUsageOnce(t *testing.T) {
	db, dbPath := crushUsageDB(t, true)
	defer func() { _ = db.Close() }()
	const start = int64(1790000000)
	insertCrushRow(t, db, "s1", nil, start, start)
	a := &CrushAdapter{DBPath: dbPath, Cwd: "/test/project"}
	path := dbPath + "/s1"

	var wm int64
	seq, reports := 0, 0
	var last float64
	poll := func() int {
		items, _, next, err := a.ParseItemsSince(t.Context(), path, wm, seq)
		if err != nil {
			t.Fatalf("ParseItemsSince: %v", err)
		}
		wm, seq = next, seq+len(items.Events)
		for _, u := range items.Usage {
			last = *u.CostUSD
		}
		reports += len(items.Usage)
		return len(items.Usage)
	}
	for turn, cost := range []float64{0.10, 0.25, 0.70} {
		crushTurn(t, db, "s1", turn, start+int64(turn)*100, "glm-test", "zai", cost, 1000, 10)
		if got := poll(); got != 1 {
			t.Errorf("turn %d: poll delivered %d usage items, want 1", turn, got)
		}
		for range 3 {
			if got := poll(); got != 0 {
				t.Errorf("turn %d: a poll with nothing new delivered %d usage items", turn, got)
			}
		}
	}
	if reports != 3 || last != 0.70 {
		t.Errorf("delivered %d reports ending at %v, want 3 ending at 0.70", reports, last)
	}
}

// TestCrushNoUsageRecordedYieldsNoItems: a session Crush has recorded no
// usage for yields no Usage items, and a store from before the provider
// column still reports its cost, with the provider unknown.
func TestCrushNoUsageRecordedYieldsNoItems(t *testing.T) {
	db, dbPath := crushUsageDB(t, false)
	const start = int64(1790000000)
	insertCrushRow(t, db, "empty", nil, start, start)
	if _, err := db.Exec(`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at) VALUES ('m1','empty','user','[{"type":"text","data":{"text":"hi"}}]',NULL,?,?)`, start, start); err != nil {
		t.Fatal(err)
	}
	insertCrushRow(t, db, "old", nil, start, start)
	crushTurn(t, db, "old", 0, start+10, "old-model", "", 0.05, 100, 5)
	_ = db.Close()

	a := &CrushAdapter{DBPath: dbPath, Cwd: "/test/project"}
	items, _, err := a.ParseItems(t.Context(), dbPath+"/empty")
	if err != nil {
		t.Fatalf("ParseItems: %v", err)
	}
	if items.Usage != nil {
		t.Errorf("a session with no recorded usage produced %+v", items.Usage)
	}
	items, _, _, err = a.ParseItemsSince(t.Context(), dbPath+"/empty", 0, 0)
	if err != nil {
		t.Fatalf("ParseItemsSince: %v", err)
	}
	if items.Usage != nil {
		t.Errorf("ParseItemsSince on a session with no recorded usage produced %+v", items.Usage)
	}

	items, _, err = a.ParseItems(t.Context(), dbPath+"/old")
	if err != nil {
		t.Fatalf("ParseItems on a store without messages.provider: %v", err)
	}
	if len(items.Usage) != 1 || items.Usage[0].Model != "old-model" || items.Usage[0].Provider != "" {
		t.Errorf("usage = %+v, want one report for old-model with no provider", items.Usage)
	}
}
