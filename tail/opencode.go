package tail

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
	"gitea.stump.rocks/stump.wtf/agent-trace/internal/strutil"
)

// HarnessOpenCode is the harness identifier for OpenCode sessions.
const HarnessOpenCode Harness = "opencode"

// OpenCodeAdapter discovers and parses OpenCode session data from SQLite
// databases at ~/.opencode/opencode.db. OpenCode is an open-source AI coding
// agent by SST (https://opencode.ai).
type OpenCodeAdapter struct {
	// DBPath overrides the database path (default: ~/.opencode/opencode.db).
	DBPath string
}

func (a OpenCodeAdapter) Harness() Harness { return HarnessOpenCode }

// WithRoot returns a copy of the adapter that discovers sessions under root,
// which is treated as a HOME-like base: the adapter appends its own layout
// (.opencode/opencode.db). Sibling fields are preserved; only the root-derived
// path changes. Pass an empty string to restore the default.
func (a OpenCodeAdapter) WithRoot(root string) Adapter {
	if root == "" {
		a.DBPath = ""
		return a
	}
	a.DBPath = filepath.Join(root, ".opencode", "opencode.db")
	return a
}

func (a OpenCodeAdapter) SessionDir() string {
	if a.DBPath != "" {
		return filepath.Dir(a.DBPath)
	}
	return homeDir(".opencode")
}

func (a OpenCodeAdapter) dbPath() string {
	if a.DBPath != "" {
		return a.DBPath
	}
	return homeDir(".opencode", "opencode.db")
}

// ListSessions discovers OpenCode sessions from the database.
//
// It delegates to ListSessionsFiltered with the zero filter, which matches
// every session and pushes nothing down, so the two entry points cannot drift
// on ordering, on the "a missing database is an empty result, not an error"
// policy, or on anything else — see the CrushAdapter.ListSessions note.
func (a OpenCodeAdapter) ListSessions() ([]SessionMeta, error) {
	return a.ListSessionsFiltered(SessionFilter{})
}

// ListSessionsFiltered implements FilteredLister, pushing the time bound into
// the SQL query and leaving the exact decision to filterSessions. A missing or
// unreadable database stays an empty result rather than an error.
//
// The empty results run through filterSessions rather than returning a bare
// nil so that they match the generic path in ListSessionsFiltered byte for
// byte: filterSessions yields a non-nil empty slice under a non-zero filter,
// which marshals as [] where nil marshals as null.
func (a OpenCodeAdapter) ListSessionsFiltered(f SessionFilter) ([]SessionMeta, error) {
	dbPath := a.dbPath()
	if dbPath == "" {
		return filterSessions(nil, f), nil
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return filterSessions(nil, f), nil
	}
	sinceMs, hasSince := sinceLowerBoundMs(f)
	metas, err := a.listSessionsSince(db, dbPath, sinceMs, hasSince)
	if err != nil {
		return filterSessions(nil, f), nil
	}
	return filterSessions(metas, f), nil
}

func (a OpenCodeAdapter) listSessions(db *sql.DB, dbPath string) ([]SessionMeta, error) {
	return a.listSessionsSince(db, dbPath, 0, false)
}

// listSessionsSince is listSessions with an optional epoch-millisecond lower
// bound pushed into the query. The bound is a coarse prefilter (see
// sinceLowerBoundMs): time_created is the same column msToRFC3339 turns into
// StartedAt, so a row below the bound cannot satisfy the exact filter.
//
// Only the time bound pushes down. directory holds the session cwd, but the
// filter's cwd rule is path-component matching over cleaned paths, and
// approximating that with SQL LIKE would risk disagreeing with cwdMatches on
// trailing separators or "." elements — the cheap win is not worth a pushdown
// that can diverge from the exact predicate.
func (a OpenCodeAdapter) listSessionsSince(db *sql.DB, dbPath string, sinceMs int64, hasSince bool) ([]SessionMeta, error) {
	// Composed rather than written out twice, so the column list exists once
	// and a schema change cannot update one copy and leave the other to fail
	// in rows.Scan (where the per-row `continue` would swallow it).
	//
	// The `, id DESC` tiebreak is what makes the pushdown safe. SQL leaves the
	// relative order of rows tied on time_updated unspecified, and adding a
	// WHERE clause can change the plan — an index scan on time_created walks
	// tied rows in a different order than a table scan. There is no Go-side
	// re-sort here, so that order reaches the caller directly and the filtered
	// listing could disagree with the unfiltered one on ties. A total order in
	// both queries removes the possibility.
	query := `SELECT id, title, directory, parent_id, model, time_created, time_updated
		 FROM session`
	var args []any
	if hasSince {
		query += ` WHERE time_created >= ?`
		args = append(args, sinceMs)
	}
	query += ` ORDER BY time_updated DESC, id DESC`
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var metas []SessionMeta
	for rows.Next() {
		var id, title, directory string
		var parentID, model sql.NullString
		var createdAt, updatedAt int64
		if err := rows.Scan(&id, &title, &directory, &parentID, &model, &createdAt, &updatedAt); err != nil {
			continue
		}
		var modelStr string
		if model.Valid {
			var m struct {
				ID         string `json:"id"`
				ProviderID string `json:"providerID"`
			}
			if json.Unmarshal([]byte(model.String), &m) == nil && m.ID != "" {
				modelStr = m.ID
			}
		}
		meta := SessionMeta{
			Key:       sessionKey(string(a.Harness()), dbPath+"/"+id),
			ID:        id,
			Harness:   a.Harness(),
			Path:      dbPath,
			Cwd:       directory,
			Model:     modelStr,
			Title:     title,
			StartedAt: msToRFC3339(createdAt),
			EndedAt:   msToRFC3339(updatedAt),
		}
		if parentID.Valid && parentID.String != "" {
			meta.Auxiliary = true
		}
		if meta.Title == "" {
			if directory != "" {
				meta.Title = filepath.Base(directory) + " — " + id[:min(8, len(id))]
			} else {
				meta.Title = id
			}
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// Summarize extracts metadata for a single OpenCode session.
func (a OpenCodeAdapter) Summarize(path string) (SessionMeta, error) {
	dbPath, sessionID := splitDBSessionPath(path)
	if dbPath == "" {
		return SessionMeta{}, fmt.Errorf("not an OpenCode session: %s", path)
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return SessionMeta{}, err
	}
	metas, err := a.listSessions(db, dbPath)
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

// Parse reads a complete OpenCode session and returns classified events.
func (a OpenCodeAdapter) Parse(path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
	dbPath, sessionID := splitDBSessionPath(path)
	if dbPath == "" {
		return nil, nil, SessionMeta{}, fmt.Errorf("not an OpenCode session: %s", path)
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return nil, nil, SessionMeta{}, err
	}

	// Get session metadata.
	var title, directory string
	var parentID, model sql.NullString
	var createdAt, updatedAt int64
	err = db.QueryRowContext(context.Background(),
		`SELECT title, directory, parent_id, model, time_created, time_updated
		 FROM session WHERE id = ?`, sessionID).
		Scan(&title, &directory, &parentID, &model, &createdAt, &updatedAt)
	if err != nil {
		return nil, nil, SessionMeta{}, fmt.Errorf("session not found: %s", sessionID)
	}

	var modelStr string
	if model.Valid {
		var m struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
		}
		if json.Unmarshal([]byte(model.String), &m) == nil && m.ID != "" {
			modelStr = m.ID
		}
	}

	meta := SessionMeta{
		Key:       sessionKey(string(a.Harness()), dbPath+"/"+sessionID),
		ID:        sessionID,
		Harness:   a.Harness(),
		Path:      dbPath,
		Cwd:       directory,
		Model:     modelStr,
		Title:     title,
		StartedAt: msToRFC3339(createdAt),
		EndedAt:   msToRFC3339(updatedAt),
	}
	if parentID.Valid && parentID.String != "" {
		meta.Auxiliary = true
	}

	// Read parts ordered by message time then part insertion.
	// OpenCode stores tool calls and results in a single ToolPart whose
	// state progresses: pending → running → completed/error.
	rows, err := db.QueryContext(context.Background(),
		`SELECT p.data, p.time_created
		 FROM part p
		 JOIN message m ON p.message_id = m.id
		 WHERE p.session_id = ?
		 ORDER BY m.time_created, p.time_created`, sessionID)
	if err != nil {
		return nil, nil, meta, err
	}
	defer func() { _ = rows.Close() }()

	opts := osClassifyOptions()
	var events []classify.Event
	var marks []classify.Mark
	seq := 0

	for rows.Next() {
		var dataJSON string
		var timeCreated int64
		if err := rows.Scan(&dataJSON, &timeCreated); err != nil {
			continue
		}
		ts := msToRFC3339(timeCreated)

		var part opencodePart
		if json.Unmarshal([]byte(dataJSON), &part) != nil {
			continue
		}

		switch part.Type {
		case "text":
			_ = strings.TrimSpace(part.Text)

		case "tool":
			if part.Tool == "" || part.CallID == "" {
				continue
			}
			input := map[string]any{}
			if part.State != nil {
				if part.State.Input != nil {
					input = *part.State.Input
				}
				if part.State.Raw != "" {
					var raw any
					if json.Unmarshal([]byte(part.State.Raw), &raw) == nil {
						if m, ok := raw.(map[string]any); ok {
							input = m
						}
					}
				}
			}
			call := classify.ToolCall{
				ID:        part.CallID,
				Name:      part.Tool,
				Input:     input,
				Timestamp: ts,
			}

			result := classify.ToolResult{}
			if part.State != nil {
				switch part.State.Status {
				case "completed":
					result.Content = part.State.Output
				case "error":
					result.Content = part.State.Error
					result.IsError = true
				}
			}

			events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, result))
			seq++

		case "compaction":
			marks = append(marks, classify.Mark{
				Seq:       seq,
				Timestamp: ts,
				Type:      "compaction",
			})

		case "subtask":
			marks = append(marks, classify.Mark{
				Seq:       seq,
				Timestamp: ts,
				Type:      "subagent",
				Note:      part.Agent,
			})
		}
	}

	// Read user messages separately for marks.
	msgRows, err := db.QueryContext(context.Background(),
		`SELECT data, time_created FROM message WHERE session_id = ? AND json_extract(data, '$.role') = 'user'
		 ORDER BY time_created`, sessionID)
	if err == nil {
		for msgRows.Next() {
			var dataJSON string
			var timeCreated int64
			if err := msgRows.Scan(&dataJSON, &timeCreated); err != nil {
				continue
			}
			ts := msToRFC3339(timeCreated)
			var msg struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			}
			if json.Unmarshal([]byte(dataJSON), &msg) != nil {
				continue
			}
			text := classify.ContentToString(msg.Content)
			if text != "" && !injectedUserMessage(text) {
				marks = append(marks, classify.Mark{
					Seq:       seq,
					Timestamp: ts,
					Type:      "user-message",
					Note:      strutil.TruncateRunes(text, 2000, "…"),
				})
			}
		}
		_ = msgRows.Close()
	}

	// Sort marks by timestamp for consistent ordering.
	sort.SliceStable(marks, func(i, j int) bool {
		return marks[i].Timestamp < marks[j].Timestamp
	})

	if meta.Title == "" {
		if directory != "" {
			meta.Title = filepath.Base(directory) + " — " + sessionID[:min(8, len(sessionID))]
		} else {
			meta.Title = sessionID
		}
	}

	return events, marks, meta, nil
}

// OpenCode part types (subset relevant to tracing).

type opencodePart struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// Tool fields
	Tool   string             `json:"tool"`
	CallID string             `json:"callID"`
	State  *opencodeToolState `json:"state"`

	// Subtask fields
	Agent string `json:"agent"`
}

type opencodeToolState struct {
	Status string          `json:"status"`
	Input  *map[string]any `json:"input"`
	Raw    string          `json:"raw"`
	Output string          `json:"output"`
	Error  string          `json:"error"`
}

// Ensure time is imported for potential future use.
var _ = time.Time{}
