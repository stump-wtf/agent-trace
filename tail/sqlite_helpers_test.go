package tail

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
)

// resetDBCache closes and drops every cached DB handle. Test-only: callers
// must not hold a handle returned by openSQLite across a reset, since the
// underlying *sql.DB is closed here.
func resetDBCache() {
	dbCache.Range(func(key, value any) bool {
		if db, ok := value.(*sql.DB); ok {
			db.Close()
		}
		dbCache.Delete(key)
		return true
	})
}

// TestOpenSQLiteCaches verifies that repeated calls to openSQLite return the
// same *sql.DB handle for a given path (issue #10: DB connection pooling).
func TestOpenSQLiteCaches(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Create a minimal DB file.
	createTestCrushDB(t, dbPath)

	db1, err := openSQLite(dbPath)
	if err != nil {
		t.Fatalf("first openSQLite: %v", err)
	}
	db2, err := openSQLite(dbPath)
	if err != nil {
		t.Fatalf("second openSQLite: %v", err)
	}
	if db1 != db2 {
		t.Fatal("expected same *sql.DB handle from cache")
	}

	// Different path should produce a different handle.
	dbPath2 := filepath.Join(dir, "test2.db")
	createTestCrushDB(t, dbPath2)
	db3, err := openSQLite(dbPath2)
	if err != nil {
		t.Fatalf("openSQLite on second path: %v", err)
	}
	if db1 == db3 {
		t.Fatal("expected different *sql.DB handle for different path")
	}
}

// TestOpenSQLiteConcurrent verifies the cache is safe under concurrent access.
func TestOpenSQLiteConcurrent(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "concurrent.db")
	createTestCrushDB(t, dbPath)

	var wg sync.WaitGroup
	handles := make([]*sql.DB, 20)
	for i := range handles {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			db, err := openSQLite(dbPath)
			if err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
				return
			}
			handles[idx] = db
		}(i)
	}
	wg.Wait()

	for i := 1; i < len(handles); i++ {
		if handles[i] == nil || handles[0] == nil {
			t.Fatalf("goroutine %d got nil handle", i)
		}
		if handles[i] != handles[0] {
			t.Fatalf("goroutine %d got different handle", i)
		}
	}

	// The losers of the LoadOrStore race must be closed, not leaked: every
	// sql.Open starts a connectionOpener goroutine that lives until Close.
	// Only the cached handle should still be usable.
	if err := handles[0].Ping(); err != nil {
		t.Fatalf("cached handle should still be open: %v", err)
	}
}

// TestSplitDBSessionPath verifies path splitting for SQLite-backed adapters
// (issue #12: rename from splitCrushPath to splitDBSessionPath).
func TestSplitDBSessionPath(t *testing.T) {
	tests := []struct {
		input       string
		wantDBPath  string
		wantSession string
	}{
		{
			input:       "/home/user/.local/share/crush/projects/abc/crush.db/550e8400-e29b-41d4-a716-446655440000",
			wantDBPath:  "/home/user/.local/share/crush/projects/abc/crush.db",
			wantSession: "550e8400-e29b-41d4-a716-446655440000",
		},
		{
			input:       "/opt/data/opencode.db/session-xyz",
			wantDBPath:  "/opt/data/opencode.db",
			wantSession: "session-xyz",
		},
		{
			input:       "no-slash-here",
			wantDBPath:  "",
			wantSession: "",
		},
		{
			input:       "/trailing/slash/",
			wantDBPath:  "/trailing/slash",
			wantSession: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			dbPath, sessionID := splitDBSessionPath(tt.input)
			if dbPath != tt.wantDBPath {
				t.Errorf("dbPath = %q, want %q", dbPath, tt.wantDBPath)
			}
			if sessionID != tt.wantSession {
				t.Errorf("sessionID = %q, want %q", sessionID, tt.wantSession)
			}
		})
	}
}
