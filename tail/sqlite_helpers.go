package tail

import (
	"database/sql"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// dbCache caches *sql.DB handles keyed by database path to avoid repeated
// open/close churn during poll cycles (issue #10). SQLite open has non-trivial
// cost (mmap, schema parse, pragma setup) and the watcher polls every 2s.
var dbCache sync.Map

// openSQLite opens a read-only SQLite database, caching the handle so repeated
// calls for the same path avoid connection churn.
//
// The DSN uses a file: prefix so modernc.org/sqlite honors ?mode=ro for genuine
// read-only access (without it, the bare path DSN opens read-write).
// _busy_timeout=5000 makes SQLITE_BUSY retry for 5s instead of failing
// immediately when the live harness is writing to the same database.
func openSQLite(path string) (*sql.DB, error) {
	if cached, ok := dbCache.Load(path); ok {
		return cached.(*sql.DB), nil
	}
	dsn := "file:" + path + "?mode=ro&_busy_timeout=5000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(30 * time.Second)
	actual, loaded := dbCache.LoadOrStore(path, db)
	if loaded {
		db.Close()
	}
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
