package tail

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
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

func (a OpenCodeAdapter) SessionDir() string {
	if a.DBPath != "" {
		return filepath.Dir(a.DBPath)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".opencode")
}

func (a OpenCodeAdapter) dbPath() string {
	if a.DBPath != "" {
		return a.DBPath
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".opencode", "opencode.db")
}

// ListSessions discovers OpenCode sessions from the database.
func (a OpenCodeAdapter) ListSessions() ([]SessionMeta, error) {
	dbPath := a.dbPath()
	if dbPath == "" {
		return nil, nil
	}
	db, err := openCrushDB(dbPath)
	if err != nil {
		return nil, nil // missing/unreadable DB is not an error
	}
	defer db.Close()
	metas, err := a.listSessions(db, dbPath)
	if err != nil {
		return nil, nil // unreadable/missing DB is not an error
	}
	return metas, nil
}

func (a OpenCodeAdapter) listSessions(db *sql.DB, dbPath string) ([]SessionMeta, error) {
	rows, err := db.QueryContext(context.Background(),
		`SELECT id, title, directory, parent_id, model, time_created, time_updated
		 FROM session ORDER BY time_updated DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	dbPath, sessionID := splitCrushPath(path)
	if dbPath == "" {
		return SessionMeta{}, fmt.Errorf("not an OpenCode session: %s", path)
	}
	db, err := openCrushDB(dbPath)
	if err != nil {
		return SessionMeta{}, err
	}
	defer db.Close()
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
	dbPath, sessionID := splitCrushPath(path)
	if dbPath == "" {
		return nil, nil, SessionMeta{}, fmt.Errorf("not an OpenCode session: %s", path)
	}
	db, err := openCrushDB(dbPath)
	if err != nil {
		return nil, nil, SessionMeta{}, err
	}
	defer db.Close()

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
	defer rows.Close()

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
			if strings.TrimSpace(part.Text) != "" {
				// Determine role from parent message — for now treat all text
				// as potential user messages; the message table has the role.
				// We check the message role separately below.
			}

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
					Note:      truncateRunes(text, 2000, "…"),
				})
			}
		}
		msgRows.Close()
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Ensure time is imported for potential future use.
var _ = time.Time{}
