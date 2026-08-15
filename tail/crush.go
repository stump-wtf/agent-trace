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
func (a CrushAdapter) AgentGraph() (*AgentGraph, error) {
	graph := &AgentGraph{Children: map[string][]AgentNode{}}
	for _, db := range a.dbPaths() {
		nodes, err := a.graphNodes(db.dbPath)
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

func (a CrushAdapter) graphNodes(dbPath string) ([]AgentNode, error) {
	db, err := openSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(context.Background(),
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

// Summarize extracts metadata from a single Crush session.
func (a CrushAdapter) Summarize(path string) (SessionMeta, error) {
	// path is "dbPath/sessionID" from ListSessions
	dbPath, sessionID := splitDBSessionPath(path)
	if dbPath == "" {
		return SessionMeta{}, fmt.Errorf("not a Crush session: %s", path)
	}
	metas, err := a.listDBSessions(context.Background(), dbPath, a.cwdForDB(dbPath))
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

	// Read messages in order.
	rows, err := db.QueryContext(ctx,
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

// Watermark returns the latest message timestamp for the session, in the
// units messages.created_at natively stores (Unix seconds for Crush), used
// as an incremental watermark for SQLite-backed adapters.
func (a CrushAdapter) Watermark(ctx context.Context, path string) int64 {
	dbPath, sessionID := splitDBSessionPath(path)
	if dbPath == "" {
		return 0
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return 0
	}
	var maxTS sql.NullInt64
	err = db.QueryRowContext(ctx,
		`SELECT MAX(created_at) FROM messages WHERE session_id = ?`, sessionID).Scan(&maxTS)
	if err != nil || !maxTS.Valid {
		return 0
	}
	return maxTS.Int64
}

// ParseSince reads only messages with created_at > watermark, returning events
// and marks with seq continuing from startSeq. This avoids re-querying and
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

	query := `SELECT id, role, parts, model, created_at FROM messages WHERE session_id = ? AND created_at > ? ORDER BY created_at`
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

	// Watermark and result counts as of the last message that left no tool call
	// outstanding. A Crush tool_call and its tool_result are separate message
	// rows written seconds apart, so without this the poll that sees the call
	// advances past it and the next poll drops the orphaned result — see the
	// ClaudeCodeAdapter.ParseSince note for the full shape of that failure.
	safeWatermark, safeEvents, safeMarks := watermark, 0, 0

	for rows.Next() {
		var msgID, role, partsJSON string
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
				result := classify.ToolResult{Content: part.Data.Content}
				events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, result))
				seq++
			}
		}
		if len(pendingCalls) == 0 {
			safeWatermark, safeEvents, safeMarks = msgCreatedAt, len(events), len(marks)
		}
	}

	// The watermark is a messages.created_at value because that is the column
	// the query filters on. Returning sessions.updated_at, as this did, mixes
	// two clocks: the session row is touched after its messages are inserted,
	// so the next poll's `created_at > watermark` skips any message written in
	// the gap between them, and skips it permanently.
	//
	// An iteration error is returned rather than swallowed: truncating here
	// would advance nothing — the watcher only stores newWatermark on success —
	// but a partial slice handed back as success would emit a subset of the
	// session's events and marks as though it were the whole truth.
	if err := rows.Err(); err != nil {
		return nil, nil, meta, watermark, fmt.Errorf("reading messages for session %s: %w", sessionID, err)
	}
	return events[:safeEvents], marks[:safeMarks], meta, safeWatermark, nil
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
