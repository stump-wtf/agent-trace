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
	_ "modernc.org/sqlite"
)

// HarnessCrush is the harness identifier for Crush sessions.
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
}

func (a CrushAdapter) Harness() Harness { return HarnessCrush }

func (a CrushAdapter) projectsPath() string {
	if a.ProjectsPath != "" {
		return a.ProjectsPath
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "crush", "projects.json")
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
func (a CrushAdapter) ListSessions() ([]SessionMeta, error) {
	var metas []SessionMeta
	for _, db := range a.dbPaths() {
		sessions, err := a.listDBSessions(db.dbPath, db.cwd)
		if err != nil {
			continue
		}
		metas = append(metas, sessions...)
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].EndedAt > metas[j].EndedAt
	})
	return metas, nil
}

func (a CrushAdapter) listDBSessions(dbPath, cwd string) ([]SessionMeta, error) {
	db, err := openCrushDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(),
		`SELECT id, title, parent_session_id, created_at, updated_at FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var metas []SessionMeta
	for rows.Next() {
		var id, title, parentID string
		var createdAt, updatedAt int64
		if err := rows.Scan(&id, &title, &parentID, &createdAt, &updatedAt); err != nil {
			continue
		}
		meta := SessionMeta{
			Key:       sessionKey(string(a.Harness()), dbPath+"/"+id),
			ID:        id,
			Harness:   a.Harness(),
			Path:      dbPath,
			Cwd:       cwd,
			Title:     title,
			StartedAt: msToRFC3339(createdAt),
			EndedAt:   msToRFC3339(updatedAt),
		}
		if parentID != "" {
			meta.Auxiliary = true
		}
		if meta.Title == "" || meta.Title == "Untitled Session" {
			meta.Title = filepath.Base(cwd) + " — " + id[:8]
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// Summarize extracts metadata from a single Crush session.
func (a CrushAdapter) Summarize(path string) (SessionMeta, error) {
	// path is "dbPath/sessionID" from ListSessions
	dbPath, sessionID := splitCrushPath(path)
	if dbPath == "" {
		return SessionMeta{}, fmt.Errorf("not a Crush session: %s", path)
	}
	metas, err := a.listDBSessions(dbPath, a.Cwd)
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
	dbPath, sessionID := splitCrushPath(path)
	if dbPath == "" {
		return nil, nil, SessionMeta{}, fmt.Errorf("not a Crush session: %s", path)
	}
	db, err := openCrushDB(dbPath)
	if err != nil {
		return nil, nil, SessionMeta{}, err
	}
	defer db.Close()

	// Get session metadata.
	var title, parentID string
	var createdAt, updatedAt int64
	err = db.QueryRowContext(context.Background(),
		`SELECT title, parent_session_id, created_at, updated_at FROM sessions WHERE id = ?`, sessionID).
		Scan(&title, &parentID, &createdAt, &updatedAt)
	if err != nil {
		return nil, nil, SessionMeta{}, fmt.Errorf("session not found: %s", sessionID)
	}

	cwd := a.Cwd
	meta := SessionMeta{
		Key:       sessionKey(string(a.Harness()), dbPath+"/"+sessionID),
		ID:        sessionID,
		Harness:   a.Harness(),
		Path:      dbPath,
		Cwd:       cwd,
		Title:     title,
		StartedAt: msToRFC3339(createdAt),
		EndedAt:   msToRFC3339(updatedAt),
	}
	if parentID != "" {
		meta.Auxiliary = true
	}

	// Read messages in order.
	rows, err := db.QueryContext(context.Background(),
		`SELECT id, role, parts, model, created_at FROM messages WHERE session_id = ? ORDER BY created_at`, sessionID)
	if err != nil {
		return nil, nil, meta, err
	}
	defer rows.Close()

	opts := osClassifyOptions()
	var events []classify.Event
	var marks []classify.Mark
	seq := 0
	pendingCalls := map[string]classify.ToolCall{}

	for rows.Next() {
		var msgID, role, partsJSON, model string
		var msgCreatedAt int64
		if err := rows.Scan(&msgID, &role, &partsJSON, &model, &msgCreatedAt); err != nil {
			continue
		}
		ts := msToRFC3339(msgCreatedAt)
		if model != "" && meta.Model == "" {
			meta.Model = model
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
							Note:      truncateRunes(part.Data.Text, 2000, "…"),
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
		meta.Title = filepath.Base(cwd) + " — " + sessionID[:8]
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

func openCrushDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	// Set a busy timeout so SQLITE_BUSY retries instead of failing immediately.
	db.SetMaxOpenConns(1)
	return db, nil
}

func msToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	t := time.UnixMilli(ms).UTC()
	return t.Format(time.RFC3339Nano)
}

func splitCrushPath(path string) (dbPath, sessionID string) {
	// path is "dbPath/sessionID" where sessionID is a UUID
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return "", ""
	}
	return path[:idx], path[idx+1:]
}
