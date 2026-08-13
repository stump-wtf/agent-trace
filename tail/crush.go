package tail

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
	"gitea.stump.rocks/stump.wtf/agent-trace/internal/strutil"
) // HarnessCrush is the harness identifier for Crush sessions.
const HarnessCrush Harness = "crush"

// CrushAdapter discovers and parses Crush session data from SQLite databases.
// Unlike the JSONL adapters, Crush stores sessions in crush.db files discovered
// via ~/.local/share/crush/projects.json.
type CrushAdapter struct {
	// ProjectsPath overrides the projects.json location (default: ~/.local/share/crush/projects.json).
	ProjectsPath string
	// DBPath overrides the database path for a single-project scan (testing).
	DBPath string
	// Cwd overrides the working directory (testing).
	Cwd string
	// opts carries classify.Options from the watcher (verify patterns, etc).
	opts *classify.Options
}

func (a CrushAdapter) Harness() Harness { return HarnessCrush }

// SetOptions injects classify.Options for verify-pattern customization.
func (a *CrushAdapter) SetOptions(opts *classify.Options) { a.opts = opts }

// WithRoot returns a copy of the adapter that discovers sessions under root,
// which is treated as a HOME-like base: the adapter appends its own layout
// (.local/share/crush/projects.json). Sibling fields — DBPath and Cwd, which a
// caller may have set for a single-project scan — are preserved; only the
// root-derived path changes. Pass an empty string to restore the default.
func (a CrushAdapter) WithRoot(root string) Adapter {
	if root == "" {
		a.ProjectsPath = ""
		return a
	}
	a.ProjectsPath = filepath.Join(root, ".local", "share", "crush", "projects.json")
	return a
}

func (a CrushAdapter) projectsPath() string {
	if a.ProjectsPath != "" {
		return a.ProjectsPath
	}
	return homeDir(".local", "share", "crush", "projects.json")
}

// SessionDir returns the directory containing project databases. For Crush
// this is not a single directory — we use projects.json for discovery.
func (a CrushAdapter) SessionDir() string {
	if a.DBPath != "" {
		return filepath.Dir(a.DBPath)
	}
	return a.projectsPath()
}

type crushProjectEntry struct {
	Path       string `json:"path"`
	DataDir    string `json:"data_dir"`
	LastAccess string `json:"last_accessed"`
}

type crushProjectsFile struct {
	Projects []crushProjectEntry `json:"projects"`
}

func (a CrushAdapter) dbPaths() []struct {
	dbPath string
	cwd    string
} {
	if a.DBPath != "" {
		return []struct {
			dbPath string
			cwd    string
		}{{a.DBPath, a.Cwd}}
	}
	data, err := os.ReadFile(a.projectsPath())
	if err != nil {
		return nil
	}
	var pf crushProjectsFile
	if json.Unmarshal(data, &pf) != nil {
		return nil
	}
	var result []struct {
		dbPath string
		cwd    string
	}
	for _, p := range pf.Projects {
		dbPath := filepath.Join(p.DataDir, "crush.db")
		if _, err := os.Stat(dbPath); err == nil {
			result = append(result, struct {
				dbPath string
				cwd    string
			}{dbPath, p.Path})
		}
	}
	return result
}

// ListSessions discovers Crush sessions from all project databases.
//
// It delegates to ListSessionsFiltered with the zero filter, which is
// documented to match every session: the pushdown skips no project, adds no
// WHERE clause, and filterSessions returns its input untouched. Keeping one
// body rather than two makes FilteredLister's "identical to ListSessions
// followed by in-memory filtering" contract true by construction instead of by
// two hand-synchronized copies of the same loop, sort, and error policy.
func (a CrushAdapter) ListSessions() ([]SessionMeta, error) {
	return a.ListSessionsFiltered(SessionFilter{})
}

// ListSessionsFiltered implements FilteredLister. Crush stores one database
// per project and the working directory is a property of that project, not of
// individual rows — so a project whose cwd cannot match is skipped without
// opening its database at all, which is the largest win available here. The
// time bound rides along as a coarse WHERE on created_at.
//
// filterSessions still runs over the result: the pushdown only narrows what is
// read, and the exact predicate is applied in exactly one place, so this can
// never disagree with ListSessions followed by in-memory filtering.
func (a CrushAdapter) ListSessionsFiltered(f SessionFilter) ([]SessionMeta, error) {
	sinceMs, hasSince := sinceLowerBoundMs(f)
	var metas []SessionMeta
	for _, db := range a.dbPaths() {
		if f.Cwd != "" && !cwdMatches(db.cwd, f.Cwd) {
			continue
		}
		sessions, err := a.listDBSessionsSince(db.dbPath, db.cwd, sinceMs, hasSince)
		if err != nil {
			continue
		}
		metas = append(metas, sessions...)
	}
	sortSessionsByRecency(metas)
	return filterSessions(metas, f), nil
}

func (a CrushAdapter) listDBSessions(dbPath, cwd string) ([]SessionMeta, error) {
	return a.listDBSessionsSince(dbPath, cwd, 0, false)
}

// listDBSessionsSince is listDBSessions with an optional epoch-millisecond
// lower bound pushed into the query. The bound is a coarse prefilter (see
// sinceLowerBoundMs): created_at is the same column msToRFC3339 turns into
// StartedAt, so a row below the bound cannot satisfy the exact filter, while
// rows above it still face the in-memory pass.
func (a CrushAdapter) listDBSessionsSince(dbPath, cwd string, sinceMs int64, hasSince bool) ([]SessionMeta, error) {
	db, err := openSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	// Composed rather than written out twice: the column list and the ORDER BY
	// exist once, so a schema change cannot update the bounded query and leave
	// the unbounded one behind (rows.Scan would then fail below and the
	// per-row `continue` would silently return zero sessions).
	query := `SELECT id, title, parent_session_id, created_at, updated_at FROM sessions`
	var args []any
	if hasSince {
		query += ` WHERE created_at >= ?`
		args = append(args, sinceMs)
	}
	query += ` ORDER BY updated_at DESC`
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var metas []SessionMeta
	for rows.Next() {
		var id, title string
		// parent_session_id is nullable — the schema says so, and Crush writes
		// NULL for every top-level session. Scanning it into a string fails on
		// those rows, and the `continue` below turns that into a silent drop:
		// listing returned only subagent sessions, never a real one. The
		// OpenCode adapter already scans its equivalent as a NullString.
		var parentID sql.NullString
		var createdAt, updatedAt int64
		if err := rows.Scan(&id, &title, &parentID, &createdAt, &updatedAt); err != nil {
			continue
		}
		meta := SessionMeta{
			Key:       sessionKey(string(a.Harness()), dbPath+"/"+id),
			ID:        id,
			Harness:   a.Harness(),
			Path:      dbPath + "/" + id,
			Cwd:       cwd,
			Title:     title,
			StartedAt: msToRFC3339(createdAt),
			EndedAt:   msToRFC3339(updatedAt),
		}
		if parentID.Valid && parentID.String != "" {
			meta.Auxiliary = true
		}
		if meta.Auxiliary {
			continue
		}
		if meta.Title == "" || meta.Title == "Untitled Session" {
			// Bounded like the OpenCode twin: a row whose id is shorter than 8
			// bytes would otherwise panic the whole listing, and this listing
			// now runs from ListSessions, ListSessionsFiltered and Summarize.
			meta.Title = filepath.Base(cwd) + " — " + id[:min(8, len(id))]
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// cwdForDB resolves the working directory that owns a database.
//
// ListSessions takes each session's Cwd from the projects.json entry, but
// Summarize and Parse used a.Cwd, which is only set in the single-database
// testing mode — so under the discovery path DefaultAdapters uses, they
// disagreed with the listing and reported an empty cwd. That empty cwd then
// reaches classify.BuildEventWith as the base for resolving every relative
// path in the session.
func (a CrushAdapter) cwdForDB(dbPath string) string {
	if a.Cwd != "" {
		return a.Cwd
	}
	for _, db := range a.dbPaths() {
		if db.dbPath == dbPath {
			return db.cwd
		}
	}
	return ""
}

// Summarize extracts metadata from a single Crush session.
func (a CrushAdapter) Summarize(path string) (SessionMeta, error) {
	// path is "dbPath/sessionID" from ListSessions
	dbPath, sessionID := splitDBSessionPath(path)
	if dbPath == "" {
		return SessionMeta{}, fmt.Errorf("not a Crush session: %s", path)
	}
	metas, err := a.listDBSessions(dbPath, a.cwdForDB(dbPath))
	if err != nil {
		return SessionMeta{}, err
	}
	for _, m := range metas {
		if m.ID == sessionID {
			return m, nil
		}
	}
	return SessionMeta{}, fmt.Errorf("session not found: %s", sessionID)
}

// Parse reads a complete Crush session and returns classified events.
func (a CrushAdapter) Parse(path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
	dbPath, sessionID := splitDBSessionPath(path)
	if dbPath == "" {
		return nil, nil, SessionMeta{}, fmt.Errorf("not a Crush session: %s", path)
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return nil, nil, SessionMeta{}, err
	}

	// Get session metadata. parent_session_id is NULL for top-level sessions;
	// scanning it into a string made Parse report "session not found" for every
	// session that was not a subagent branch.
	var title string
	var parentID sql.NullString
	var createdAt, updatedAt int64
	err = db.QueryRowContext(context.Background(),
		`SELECT title, parent_session_id, created_at, updated_at FROM sessions WHERE id = ?`, sessionID).
		Scan(&title, &parentID, &createdAt, &updatedAt)
	if err != nil {
		return nil, nil, SessionMeta{}, fmt.Errorf("session not found: %s", sessionID)
	}

	cwd := a.cwdForDB(dbPath)
	meta := SessionMeta{
		Key:       sessionKey(string(a.Harness()), dbPath+"/"+sessionID),
		ID:        sessionID,
		Harness:   a.Harness(),
		Path:      dbPath + "/" + sessionID,
		Cwd:       cwd,
		Title:     title,
		StartedAt: msToRFC3339(createdAt),
		EndedAt:   msToRFC3339(updatedAt),
	}
	if parentID.Valid && parentID.String != "" {
		meta.Auxiliary = true
	}

	// Read messages in order.
	rows, err := db.QueryContext(context.Background(),
		`SELECT id, role, parts, model, created_at FROM messages WHERE session_id = ? ORDER BY created_at`, sessionID)
	if err != nil {
		return nil, nil, meta, err
	}
	defer func() { _ = rows.Close() }()

	opts := a.opts
	if opts == nil {
		opts = osClassifyOptions(nil)
	}
	var events []classify.Event
	var marks []classify.Mark
	seq := 0
	pendingCalls := map[string]classify.ToolCall{}

	for rows.Next() {
		var msgID, role, partsJSON string
		// messages.model is nullable, and Crush leaves it NULL on user
		// messages. Scanning it into a string failed on exactly those rows and
		// the `continue` dropped them, so every user message vanished from the
		// marks a session produced.
		var model sql.NullString
		var msgCreatedAt int64
		if err := rows.Scan(&msgID, &role, &partsJSON, &model, &msgCreatedAt); err != nil {
			continue
		}
		ts := msToRFC3339(msgCreatedAt)
		if model.Valid && model.String != "" && meta.Model == "" {
			meta.Model = model.String
		}

		var parts []crushPart
		if json.Unmarshal([]byte(partsJSON), &parts) != nil {
			continue
		}

		for _, part := range parts {
			switch part.Type {
			case "text":
				if role == "user" && strings.TrimSpace(part.Data.Text) != "" {
					if !injectedUserMessage(part.Data.Text) {
						marks = append(marks, classify.Mark{
							Seq:       seq,
							Timestamp: ts,
							Type:      "user-message",
							Note:      strutil.TruncateRunes(part.Data.Text, 2000, "…"),
						})
					}
				}
			case "tool_call":
				callID := part.Data.ID
				name := part.Data.Name
				input := map[string]any{}
				if part.Data.Input != "" {
					var raw any
					if json.Unmarshal([]byte(part.Data.Input), &raw) == nil {
						if m, ok := raw.(map[string]any); ok {
							input = m
						} else {
							input = map[string]any{"_raw": part.Data.Input}
						}
					} else {
						input = map[string]any{"_raw": part.Data.Input}
					}
				}
				call := classify.ToolCall{
					ID:        callID,
					Name:      name,
					Input:     input,
					Timestamp: ts,
				}
				pendingCalls[callID] = call

			case "tool_result":
				callID := part.Data.ToolCallID
				call, ok := pendingCalls[callID]
				if !ok {
					continue
				}
				delete(pendingCalls, callID)
				result := classify.ToolResult{
					Content: part.Data.Content,
				}
				events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, result))
				seq++
			}
		}
	}

	// Flush orphaned tool calls.
	for _, call := range pendingCalls {
		events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, classify.ToolResult{}))
		seq++
	}

	if meta.Title == "" || meta.Title == "Untitled Session" {
		meta.Title = filepath.Base(cwd) + " — " + sessionID[:min(8, len(sessionID))]
	}

	return events, marks, meta, nil
}

type crushPart struct {
	Type string        `json:"type"`
	Data crushPartData `json:"data"`
}

type crushPartData struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Input      string `json:"input"`
	Finished   bool   `json:"finished"`
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	Text       string `json:"text"`
}

func msToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	t := time.UnixMilli(ms).UTC()
	return t.Format(time.RFC3339Nano)
}
