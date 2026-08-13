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
func openSQLite(path string) (*sql.DB, error) {
	if cached, ok := dbCache.Load(path); ok {
		return cached.(*sql.DB), nil
	}
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// Retire idle connections periodically. Without this the pooled connection
	// lives forever and keeps reading the inode it first opened, so a database
	// that is deleted and recreated (a project reset) would serve stale rows
	// until the process restarts. The window is long enough that the 2s poll
	// still reuses a warm connection.
	db.SetConnMaxIdleTime(30 * time.Second)
	actual, loaded := dbCache.LoadOrStore(path, db)
	if loaded {
		// Another goroutine won the race; close ours rather than leaking it.
		// sql.Open starts a connectionOpener goroutine per handle, so an
		// unclosed loser leaks a goroutine and any pooled file descriptor.
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
