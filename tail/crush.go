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

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/internal/strutil"
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

// Diagnostics checks the Crush backing store: projects.json existence and
// database readability.
func (a CrushAdapter) Diagnostics() []DiagnosticCheck {
	var checks []DiagnosticCheck
	if a.DBPath != "" {
		if _, err := os.Stat(a.DBPath); err != nil {
			checks = append(checks, DiagnosticCheck{Name: "database", Status: "warn", Detail: "DBPath does not exist: " + a.DBPath})
		} else {
			checks = append(checks, DiagnosticCheck{Name: "database", Status: "ok", Detail: a.DBPath})
		}
	} else {
		pp := a.projectsPath()
		if _, err := os.Stat(pp); err != nil {
			checks = append(checks, DiagnosticCheck{Name: "projects-json", Status: "warn", Detail: "projects.json not found: " + pp})
		} else {
			checks = append(checks, DiagnosticCheck{Name: "projects-json", Status: "ok", Detail: pp})
		}
	}
	return checks
}

// AgentGraph builds a parent-child tree of all sessions (including subagent
// sessions excluded from ListSessions). Queries parent_session_id from every
// project database and links children to their parents.
func (a CrushAdapter) AgentGraph(ctx context.Context) (*AgentGraph, error) {
	graph := &AgentGraph{Children: map[string][]AgentNode{}}
	for _, db := range a.dbPaths() {
		// Checked per database rather than once up front: the loop is the part
		// that scales with the number of projects, and each graphNodes opens
		// and queries a database. A cancelled caller gets context.Canceled
		// instead of a truncated tree that looks like "no subagents ran".
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		nodes, err := a.graphNodes(ctx, db.dbPath)
		if err != nil {
			continue
		}
		for _, node := range nodes {
			if node.ParentID == "" {
				graph.Roots = append(graph.Roots, node)
			} else {
				graph.Children[node.ParentID] = append(graph.Children[node.ParentID], node)
			}
		}
	}
	return graph, nil
}

func (a CrushAdapter) graphNodes(ctx context.Context, dbPath string) ([]AgentNode, error) {
	db, err := openSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, parent_session_id, title, created_at FROM sessions ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var nodes []AgentNode
	for rows.Next() {
		var id, title string
		var parentID sql.NullString
		var createdAt int64
		if err := rows.Scan(&id, &parentID, &title, &createdAt); err != nil {
			continue
		}
		node := AgentNode{
			SessionID: id,
			Harness:   a.Harness(),
			StartedAt: secToRFC3339(createdAt),
			Title:     title,
		}
		if parentID.Valid {
			node.ParentID = parentID.String
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing crush sessions for the agent graph: %w", err)
	}
	return nodes, nil
}

// WithRoot returns a copy of the adapter that discovers sessions under root,
// which is treated as a HOME-like base: the adapter appends its own layout
// (.local/share/crush/projects.json). Sibling fields — DBPath and Cwd, which a
// caller may have set for a single-project scan — are preserved; only the
// root-derived path changes. Pass an empty string to restore the default.
func (a CrushAdapter) WithRoot(root string) Adapter {
	if root == "" {
		a.ProjectsPath = ""
		return &a
	}
	a.ProjectsPath = filepath.Join(root, ".local", "share", "crush", "projects.json")
	return &a
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

// crushDB is a project database and the working directory that owns it.
type crushDB struct {
	dbPath string
	cwd    string
}

// dbPaths resolves projects.json into the set of databases to scan, one entry
// per database.
//
// Crush registers a project keyed on its working directory, not on its data
// directory, so two entries can name the same data_dir — the same crush.db
// reached from two spellings of a path, or two working directories configured
// to share one store. Every caller here loops over the result and appends what
// it finds, and nothing downstream dedupes: ListSessionsFiltered's sessions
// carry identical Keys through the sort, and AgentGraph gains a second copy of
// every node. So a registry with three entries pointing at one store listed
// each of its sessions three times.
//
// Deduping on the joined path rather than the raw data_dir string also folds
// together the spellings filepath.Join normalizes ("/p/.crush", "/p/./crush/.."
// and a trailing slash all clean to the same file). Symlinked stores are still
// two entries; resolving those would cost a syscall per registry entry to close
// a case Crush has no way to produce.
//
// The winner is the entry with the newest last_accessed, because its Path is
// the working directory the sessions in that store were most recently written
// from — and Cwd is what Parse hands classify as the base for resolving every
// relative path in the session. Entries whose timestamp does not parse sort
// oldest, so a malformed one never displaces a good one.
func (a CrushAdapter) dbPaths() []crushDB {
	if a.DBPath != "" {
		return []crushDB{{a.DBPath, a.Cwd}}
	}
	data, err := os.ReadFile(a.projectsPath())
	if err != nil {
		return nil
	}
	var pf crushProjectsFile
	if json.Unmarshal(data, &pf) != nil {
		return nil
	}
	var result []crushDB
	at := map[string]int{}         // dbPath -> index in result
	best := map[string]time.Time{} // dbPath -> last_accessed of the winning entry
	for _, p := range pf.Projects {
		dbPath := filepath.Join(p.DataDir, "crush.db")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		accessed, _ := time.Parse(time.RFC3339Nano, p.LastAccess)
		i, seen := at[dbPath]
		if !seen {
			at[dbPath] = len(result)
			best[dbPath] = accessed
			result = append(result, crushDB{dbPath, p.Path})
			continue
		}
		// Position is held by the first entry: the scan order of a registry
		// Crush keeps sorted by recency is worth preserving even when a later
		// duplicate wins the cwd.
		if accessed.After(best[dbPath]) {
			best[dbPath] = accessed
			result[i].cwd = p.Path
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
func (a CrushAdapter) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	return a.ListSessionsFiltered(ctx, SessionFilter{})
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
func (a CrushAdapter) ListSessionsFiltered(ctx context.Context, f SessionFilter) ([]SessionMeta, error) {
	sinceSec, hasSince := sinceLowerBoundSec(f)
	var metas []SessionMeta
	for _, db := range a.dbPaths() {
		if f.Cwd != "" && !cwdMatches(db.cwd, f.Cwd) {
			continue
		}
		sessions, err := a.listDBSessionsSince(ctx, db.dbPath, db.cwd, sinceSec, hasSince)
		if err != nil {
			continue
		}
		metas = append(metas, sessions...)
	}
	sortSessionsByRecency(metas)
	return filterSessions(metas, f), nil
}

func (a CrushAdapter) listDBSessions(ctx context.Context, dbPath, cwd string) ([]SessionMeta, error) {
	return a.listDBSessionsSince(ctx, dbPath, cwd, 0, false)
}

// listDBSessionsSince is listDBSessions with an optional epoch-second
// lower bound pushed into the query. The bound is a coarse prefilter (see
// sinceLowerBoundSec): created_at is the same column secToRFC3339 turns into
// StartedAt, so a row below the bound cannot satisfy the exact filter, while
// rows above it still face the in-memory pass.
func (a CrushAdapter) listDBSessionsSince(ctx context.Context, dbPath, cwd string, sinceSec int64, hasSince bool) ([]SessionMeta, error) {
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
		args = append(args, sinceSec)
	}
	query += ` ORDER BY updated_at DESC`
	rows, err := db.QueryContext(ctx, query, args...)
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
			StartedAt: secToRFC3339(createdAt),
			EndedAt:   secToRFC3339(updatedAt),
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scanning sessions in %s: %w", dbPath, err)
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

// Summarize extracts metadata from a single Crush session. The context is a
// cancellation bound on the underlying database query, matching Adapter.
func (a CrushAdapter) Summarize(ctx context.Context, path string) (SessionMeta, error) {
	// path is "dbPath/sessionID" from ListSessions
	dbPath, sessionID := splitDBSessionPath(path)
	if dbPath == "" {
		return SessionMeta{}, fmt.Errorf("not a Crush session: %s", path)
	}
	metas, err := a.listDBSessions(ctx, dbPath, a.cwdForDB(dbPath))
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
func (a CrushAdapter) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
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
	err = db.QueryRowContext(ctx,
		`SELECT title, parent_session_id, created_at, updated_at FROM sessions WHERE id = ?`, sessionID).
		Scan(&title, &parentID, &createdAt, &updatedAt)
	if err != nil {
		return nil, nil, SessionMeta{}, fmt.Errorf("reading crush session %s: %w", sessionID, err)
	}

	cwd := a.cwdForDB(dbPath)
	meta := SessionMeta{
		Key:       sessionKey(string(a.Harness()), dbPath+"/"+sessionID),
		ID:        sessionID,
		Harness:   a.Harness(),
		Path:      dbPath + "/" + sessionID,
		Cwd:       cwd,
		Title:     title,
		StartedAt: secToRFC3339(createdAt),
		EndedAt:   secToRFC3339(updatedAt),
	}
	if parentID.Valid && parentID.String != "" {
		meta.Auxiliary = true
	}

	// Read messages in insertion order.
	//
	// created_at is the wrong sort key and was the sort key: it holds Unix
	// SECONDS, and a tool call and its result are separate rows written well
	// inside one second. Across live Crush databases roughly half of all
	// messages sit in a same-second group, and in one 4086-message project 1787
	// of those groups hold both an assistant row and its tool row. SQLite leaves
	// the order of tied keys unspecified, so a tool_result iterated ahead of its
	// tool_call finds no pending call, hits the `continue` below, and the call
	// is flushed at the end with an empty result: the event survives, its output
	// does not.
	//
	// rowid is the insertion counter, so it is the order Crush wrote the rows —
	// the real conversational order that created_at only approximates. Verified
	// monotonic against created_at on live databases (zero inversions in 4086
	// messages). It requires messages to be an ordinary rowid table, which it
	// has been since Crush's initial migration.
	rows, err := db.QueryContext(ctx,
		`SELECT id, role, parts, model, created_at FROM messages WHERE session_id = ? ORDER BY rowid`, sessionID)
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
		ts := secToRFC3339(msgCreatedAt)
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
	if err := rows.Err(); err != nil {
		return nil, nil, meta, fmt.Errorf("reading messages for session %s: %w", sessionID, err)
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

// Watermark returns the rowid of the session's last message, the cursor
// ParseSince resumes from.
//
// It is a rowid and not MAX(created_at) for the reason Parse's query
// documents: created_at is second-resolution, so the value it returns names a
// second that is very often still being written to. The next poll's strict
// `created_at > watermark` then skipped every further message landing in that
// same second — permanently, since the watermark only moves forward. A rowid
// is unique and monotonic, so "the row after this one" is exact and a full
// Parse followed by Watermark can neither skip a row nor repeat one.
func (a CrushAdapter) Watermark(ctx context.Context, path string) int64 {
	dbPath, sessionID := splitDBSessionPath(path)
	if dbPath == "" {
		return 0
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return 0
	}
	var maxRow sql.NullInt64
	err = db.QueryRowContext(ctx,
		`SELECT MAX(rowid) FROM messages WHERE session_id = ?`, sessionID).Scan(&maxRow)
	if err != nil || !maxRow.Valid {
		return 0
	}
	return maxRow.Int64
}

// ParseSince reads only messages with rowid > watermark, returning events and
// marks with seq continuing from startSeq. This avoids re-querying and
// re-parsing the entire message history on every watcher poll.
func (a CrushAdapter) ParseSince(ctx context.Context, path string, watermark int64, startSeq int) ([]classify.Event, []classify.Mark, SessionMeta, int64, error) {
	dbPath, sessionID := splitDBSessionPath(path)
	if dbPath == "" {
		return nil, nil, SessionMeta{}, 0, fmt.Errorf("not a Crush session: %s", path)
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return nil, nil, SessionMeta{}, 0, err
	}

	var title string
	// parent_session_id is NULL for every top-level session, the same trap
	// Parse and listDBSessionsSince already document. Scanning it into a string
	// failed on exactly those rows, and the error is indistinguishable from a
	// missing session — so the watcher skipped the session on every poll after
	// the first and incremental parsing silently emitted nothing, forever, for
	// every session that was not a subagent branch.
	var parentID sql.NullString
	var createdAt, updatedAt int64
	err = db.QueryRowContext(ctx,
		`SELECT title, parent_session_id, created_at, updated_at FROM sessions WHERE id = ?`, sessionID).
		Scan(&title, &parentID, &createdAt, &updatedAt)
	if err != nil {
		return nil, nil, SessionMeta{}, 0, fmt.Errorf("reading crush session %s: %w", sessionID, err)
	}

	cwd := a.cwdForDB(dbPath)
	meta := SessionMeta{
		Key:       sessionKey(string(a.Harness()), dbPath+"/"+sessionID),
		ID:        sessionID,
		Harness:   a.Harness(),
		Path:      dbPath + "/" + sessionID,
		Cwd:       cwd,
		Title:     title,
		StartedAt: secToRFC3339(createdAt),
		EndedAt:   secToRFC3339(updatedAt),
	}
	if parentID.Valid && parentID.String != "" {
		meta.Auxiliary = true
	}
	if meta.Title == "" || meta.Title == "Untitled Session" {
		meta.Title = filepath.Base(cwd) + " — " + sessionID[:min(8, len(sessionID))]
	}

	query := `SELECT rowid, id, role, parts, model, created_at FROM messages WHERE session_id = ? AND rowid > ? ORDER BY rowid`
	rows, err := db.QueryContext(ctx, query, sessionID, watermark)
	if err != nil {
		return nil, nil, meta, 0, err
	}
	defer func() { _ = rows.Close() }()

	opts := a.opts
	if opts == nil {
		opts = osClassifyOptions(nil)
	}
	var events []classify.Event
	var marks []classify.Mark
	seq := startSeq
	pendingCalls := map[string]classify.ToolCall{}
	// callRows remembers the rowid of each still-unresolved call, so the safe
	// point can be kept strictly below it.
	callRows := map[string]int64{}

	// Cursor and result counts as of the last message that left no tool call
	// outstanding. A Crush tool_call and its tool_result are separate message
	// rows written seconds apart, so without this the poll that sees the call
	// advances past it and the next poll drops the orphaned result — see the
	// ClaudeCodeAdapter.ParseSince note for the full shape of that failure.
	//
	// Safe points are a stack, not three scalars, because an unresolved call
	// can be followed by rows that resolve their own calls: the stack is
	// rolled back to the last point strictly below the earliest unresolved
	// call before returning, cutting those later rows out of this poll and
	// leaving them to be re-read by the next one.
	type crushSafePoint struct {
		row    int64
		events int
		marks  int
	}
	safe := []crushSafePoint{{watermark, 0, 0}}

	for rows.Next() {
		var msgRow int64
		var msgID, role, partsJSON string
		var model sql.NullString
		var msgCreatedAt int64
		if err := rows.Scan(&msgRow, &msgID, &role, &partsJSON, &model, &msgCreatedAt); err != nil {
			continue
		}
		ts := secToRFC3339(msgCreatedAt)
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
				callRows[callID] = msgRow
			case "tool_result":
				callID := part.Data.ToolCallID
				call, ok := pendingCalls[callID]
				if !ok {
					continue
				}
				delete(pendingCalls, callID)
				delete(callRows, callID)
				result := classify.ToolResult{Content: part.Data.Content}
				events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, result))
				seq++
			}
		}
		if len(pendingCalls) == 0 {
			safe = append(safe, crushSafePoint{msgRow, len(events), len(marks)})
		}
	}

	// The watermark is a messages.rowid because that is the column the query
	// filters on. It was sessions.updated_at once, which mixed two clocks and
	// skipped messages written between them, and then messages.created_at,
	// which resumed past a second that was still being written to. A rowid is
	// the column the ORDER BY and the WHERE now agree on, and it admits no
	// ties, so "resume after this row" means exactly that.
	//
	// An iteration error is returned rather than swallowed: truncating here
	// would advance nothing — the watcher only stores newWatermark on success —
	// but a partial slice handed back as success would emit a subset of the
	// session's events and marks as though it were the whole truth.
	if err := rows.Err(); err != nil {
		return nil, nil, meta, watermark, fmt.Errorf("reading messages for session %s: %w", sessionID, err)
	}
	if len(callRows) > 0 {
		earliest := int64(0)
		for _, r := range callRows {
			if earliest == 0 || r < earliest {
				earliest = r
			}
		}
		for len(safe) > 1 && safe[len(safe)-1].row >= earliest {
			safe = safe[:len(safe)-1]
		}
	}
	sp := safe[len(safe)-1]
	return events[:sp.events], marks[:sp.marks], meta, sp.row, nil
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

// secToRFC3339 renders a Crush timestamp as RFC 3339. Crush stores Unix
// SECONDS in sessions.created_at/updated_at and messages.created_at, even
// though the schema's own comment claims milliseconds — the default trigger
// writes strftime('%s', 'now'), which is seconds. Verified against live
// databases; do not "fix" this back to time.UnixMilli without re-checking
// real data, because the schema comment is the lie.
func secToRFC3339(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339Nano)
}
