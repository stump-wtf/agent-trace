package tail

import (
	"database/sql"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// dbCache caches *sql.DB handles keyed by database path to avoid repeated
// open/close churn during poll cycles (issue #10). SQLite open has non-trivial
// cost (mmap, schema parse, pragma setup) and the watcher polls every 2s.
var dbCache sync.Map

// openSQLite opens a read-only SQLite database, caching the handle so repeated
// calls for the same path avoid connection churn.
func openSQLite(path string) (*sql.DB, error) {
	if cached, ok := dbCache.Load(path); ok {
		return cached.(*sql.DB), nil
	}
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	actual, _ := dbCache.LoadOrStore(path, db)
	return actual.(*sql.DB), nil
}

// splitDBSessionPath splits a "dbPath/sessionID" composite path into its
// components. Used by SQLite-backed adapters (Crush, OpenCode) to route
// Parse/Summarize calls to the right database row.
func splitDBSessionPath(path string) (dbPath, sessionID string) {
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return "", ""
	}
	return path[:idx], path[idx+1:]
}

// resetDBCache clears all cached DB handles. Test-only — callers must close
// handles themselves if they need a fresh connection.
func resetDBCache() {
	dbCache.Range(func(key, value any) bool {
		if db, ok := value.(*sql.DB); ok {
			db.Close()
		}
		dbCache.Delete(key)
		return true
	})
}
