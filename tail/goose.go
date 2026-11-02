package tail

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/internal/strutil"
)

// GooseAdapter discovers and parses AAIF Goose session data from SQLite
// databases at ~/.local/share/goose/sessions/sessions.db (Unix) or
// %APPDATA%\Block\goose\data\sessions\sessions.db (Windows).
// Goose is an open-source AI agent developed by AAIF (https://goose-docs.ai).
type GooseAdapter struct {
	// DBPath overrides the database path (default: ~/.local/share/goose/sessions/sessions.db).
	DBPath string
	// opts carries classify.Options from the watcher (verify patterns, etc).
	opts *classify.Options
}

// Harness reports the Goose harness identifier.
func (a GooseAdapter) Harness() Harness { return HarnessGoose }

// SetOptions injects classify.Options for verify patterns and error excerpts.
func (a *GooseAdapter) SetOptions(opts *classify.Options) { a.opts = opts }

// Diagnostics checks whether the Goose sessions database exists.
func (a GooseAdapter) Diagnostics() []DiagnosticCheck {
	dbPath := a.dbPath()
	if dbPath == "" {
		return []DiagnosticCheck{{Name: "database", Status: "warn", Detail: "database path is empty"}}
	}
	if _, err := os.Stat(dbPath); err != nil {
		return []DiagnosticCheck{{Name: "database", Status: "warn", Detail: "database not found: " + dbPath}}
	}
	return []DiagnosticCheck{{Name: "database", Status: "ok", Detail: dbPath}}
}

// WithRoot returns a copy of the adapter configured to discover sessions
// under root instead of the default location.
func (a GooseAdapter) WithRoot(root string) Adapter {
	if root == "" {
		a.DBPath = ""
		return &a
	}
	a.DBPath = filepath.Join(root, ".local", "share", "goose", "sessions", "sessions.db")
	return &a
}

// SessionDir returns the directory containing the Goose session database.
func (a GooseAdapter) SessionDir() string {
	if a.DBPath != "" {
		return filepath.Dir(a.DBPath)
	}
	return filepath.Dir(a.defaultDBPath())
}

func (a GooseAdapter) defaultDBPath() string {
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Block", "goose", "data", "sessions", "sessions.db")
		}
	}
	return homeDir(".local", "share", "goose", "sessions", "sessions.db")
}

func (a GooseAdapter) dbPath() string {
	if a.DBPath != "" {
		return a.DBPath
	}
	return a.defaultDBPath()
}

// ListSessions discovers Goose sessions from the database.
func (a GooseAdapter) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	return a.ListSessionsFiltered(ctx, SessionFilter{})
}

// ListSessionsFiltered implements FilteredLister, pushing time bounds into
// the SQL query and delegating exact predicate matching to filterSessions.
func (a GooseAdapter) ListSessionsFiltered(ctx context.Context, f SessionFilter) ([]SessionMeta, error) {
	dbPath := a.dbPath()
	if dbPath == "" {
		return filterSessions(nil, f), nil
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return filterSessions(nil, f), nil
	}
	sinceSec, hasSince := sinceLowerBoundSec(f)
	metas, err := a.listSessionsSince(ctx, db, dbPath, sinceSec, hasSince)
	if err != nil {
		return filterSessions(nil, f), nil
	}
	return filterSessions(metas, f), nil
}

func (a GooseAdapter) listSessionsSince(ctx context.Context, db *sql.DB, dbPath string, sinceSec int64, hasSince bool) ([]SessionMeta, error) {
	query := `SELECT id, name, working_dir, session_type, parent_session_id, provider_name, model_config_json, created_at, updated_at
		 FROM sessions`
	var args []any
	if hasSince {
		sinceStr := time.Unix(sinceSec, 0).UTC().Format("2006-01-02 15:04:05")
		query += ` WHERE (created_at >= ? OR strftime('%s', created_at) >= ?)`
		args = append(args, sinceStr, sinceSec)
	}
	query += ` ORDER BY updated_at DESC, id DESC`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var metas []SessionMeta
	for rows.Next() {
		var id, name, workingDir, sessionType string
		var parentID, providerName, modelConfig, createdAt, updatedAt sql.NullString
		if err := rows.Scan(&id, &name, &workingDir, &sessionType, &parentID, &providerName, &modelConfig, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scanning goose sessions in %s: %w", dbPath, err)
		}

		modelStr := parseGooseModel(providerName.String, modelConfig.String)
		startedAt := gooseTimeToRFC3339(createdAt.String)
		endedAt := gooseTimeToRFC3339(updatedAt.String)

		title := name
		if title == "" {
			if workingDir != "" {
				title = filepath.Base(workingDir) + " — " + id
			} else {
				title = id
			}
		}

		meta := SessionMeta{
			Key:       sessionKey(string(a.Harness()), dbPath+"/"+id),
			ID:        id,
			Harness:   a.Harness(),
			Path:      dbPath + "/" + id,
			Cwd:       workingDir,
			Model:     modelStr,
			Title:     title,
			StartedAt: startedAt,
			EndedAt:   endedAt,
		}

		if (parentID.Valid && parentID.String != "") || sessionType == "sub_agent" {
			meta.Auxiliary = true
		}
		if meta.Auxiliary {
			continue
		}
		metas = append(metas, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scanning goose sessions in %s: %w", dbPath, err)
	}
	return metas, nil
}

// Summarize extracts metadata for a single Goose session.
func (a GooseAdapter) Summarize(ctx context.Context, path string) (SessionMeta, error) {
	dbPath, sessionID := splitDBSessionPath(path)
	if dbPath == "" {
		return SessionMeta{}, fmt.Errorf("not a Goose session: %s", path)
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return SessionMeta{}, err
	}

	var name, workingDir, sessionType string
	var parentID, providerName, modelConfig, createdAt, updatedAt sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT name, working_dir, session_type, parent_session_id, provider_name, model_config_json, created_at, updated_at
		 FROM sessions WHERE id = ?`, sessionID).
		Scan(&name, &workingDir, &sessionType, &parentID, &providerName, &modelConfig, &createdAt, &updatedAt)
	if err != nil {
		return SessionMeta{}, fmt.Errorf("reading goose session %s: %w", sessionID, err)
	}

	modelStr := parseGooseModel(providerName.String, modelConfig.String)
	startedAt := gooseTimeToRFC3339(createdAt.String)
	endedAt := gooseTimeToRFC3339(updatedAt.String)

	title := name
	if title == "" {
		if workingDir != "" {
			title = filepath.Base(workingDir) + " — " + sessionID
		} else {
			title = sessionID
		}
	}

	meta := SessionMeta{
		Key:       sessionKey(string(a.Harness()), dbPath+"/"+sessionID),
		ID:        sessionID,
		Harness:   a.Harness(),
		Path:      dbPath + "/" + sessionID,
		Cwd:       workingDir,
		Model:     modelStr,
		Title:     title,
		StartedAt: startedAt,
		EndedAt:   endedAt,
	}
	if (parentID.Valid && parentID.String != "") || sessionType == "sub_agent" {
		meta.Auxiliary = true
	}
	return meta, nil
}

// Parse extracts events, marks, and metadata from a Goose session.
func (a GooseAdapter) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
	items, meta, err := a.ParseItems(ctx, path)
	return items.Events, items.Marks, meta, err
}

// ParseItems implements ItemParser for Goose.
func (a GooseAdapter) ParseItems(ctx context.Context, path string) (Items, SessionMeta, error) {
	items, meta, _, err := a.parseItemsInternal(ctx, path, 0, false, 0)
	return items, meta, err
}

// Watermark returns the current high-water mark for the session at path.
// For Goose, this is the maximum messages.id for sessionID.
func (a GooseAdapter) Watermark(ctx context.Context, path string) int64 {
	dbPath, sessionID := splitDBSessionPath(path)
	if dbPath == "" {
		return 0
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return 0
	}
	var maxID sql.NullInt64
	err = db.QueryRowContext(ctx, `SELECT MAX(id) FROM messages WHERE session_id = ?`, sessionID).Scan(&maxID)
	if err != nil || !maxID.Valid {
		return 0
	}
	return maxID.Int64
}

// goosePendingCall is a tool request whose response has not been read yet.
type goosePendingCall struct {
	call classify.ToolCall
}

// seedGooseOpenCalls fills pending with tool requests at or below watermark
// that have no response at or below watermark, so a tool call whose request
// landed in a previous read still pairs with its response in this one.
func seedGooseOpenCalls(ctx context.Context, db *sql.DB, sessionID string, watermark int64, pending map[string]goosePendingCall) error {
	reqRows, err := db.QueryContext(ctx,
		`SELECT created_timestamp, content_json FROM messages
		  WHERE session_id = ? AND id <= ? AND role = 'assistant' AND content_json LIKE '%"toolRequest"%'
		  ORDER BY id ASC`, sessionID, watermark)
	if err != nil {
		return fmt.Errorf("scanning goose tool requests for %s: %w", sessionID, err)
	}
	defer func() { _ = reqRows.Close() }()
	for reqRows.Next() {
		var createdSec sql.NullInt64
		var contentJSON sql.NullString
		if err := reqRows.Scan(&createdSec, &contentJSON); err != nil {
			return fmt.Errorf("scanning goose tool request: %w", err)
		}
		if !contentJSON.Valid {
			continue
		}
		ts := secToRFC3339(createdSec.Int64)
		for _, block := range decodeGooseContent(contentJSON.String) {
			if bType, _ := block["type"].(string); bType == "toolRequest" {
				callID, toolName, callArgs := parseGooseToolRequest(block)
				if callID != "" && toolName != "" {
					pending[callID] = goosePendingCall{call: classify.ToolCall{
						ID:        callID,
						Name:      toolName,
						Input:     callArgs,
						Timestamp: ts,
					}}
				}
			}
		}
	}
	if err := reqRows.Err(); err != nil {
		return fmt.Errorf("scanning goose tool requests for %s: %w", sessionID, err)
	}

	respRows, err := db.QueryContext(ctx,
		`SELECT content_json FROM messages
		  WHERE session_id = ? AND id <= ? AND role = 'user' AND content_json LIKE '%"toolResponse"%'`,
		sessionID, watermark)
	if err != nil {
		return fmt.Errorf("scanning goose tool responses for %s: %w", sessionID, err)
	}
	defer func() { _ = respRows.Close() }()
	for respRows.Next() {
		var contentJSON sql.NullString
		if err := respRows.Scan(&contentJSON); err != nil {
			return fmt.Errorf("scanning goose tool response: %w", err)
		}
		if !contentJSON.Valid {
			continue
		}
		for _, block := range decodeGooseContent(contentJSON.String) {
			if bType, _ := block["type"].(string); bType == "toolResponse" {
				if respID, _, _ := parseGooseToolResponse(block); respID != "" {
					delete(pending, respID)
				}
			}
		}
	}
	return respRows.Err()
}

// ParseSince extracts events, marks, and metadata incrementally starting after watermark.
func (a GooseAdapter) ParseSince(ctx context.Context, path string, watermark int64, startSeq int) ([]classify.Event, []classify.Mark, SessionMeta, int64, error) {
	items, meta, wm, err := a.ParseItemsSince(ctx, path, watermark, startSeq)
	return items.Events, items.Marks, meta, wm, err
}

// ParseItemsSince implements ItemParser incrementally.
func (a GooseAdapter) ParseItemsSince(ctx context.Context, path string, watermark int64, startSeq int) (Items, SessionMeta, int64, error) {
	return a.parseItemsInternal(ctx, path, watermark, true, startSeq)
}

func (a GooseAdapter) parseItemsInternal(ctx context.Context, path string, watermark int64, hasWatermark bool, startSeq int) (Items, SessionMeta, int64, error) {
	dbPath, sessionID := splitDBSessionPath(path)
	if dbPath == "" {
		return Items{}, SessionMeta{}, watermark, fmt.Errorf("not a Goose session: %s", path)
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return Items{}, SessionMeta{}, watermark, err
	}

	meta, err := a.Summarize(ctx, path)
	if err != nil {
		return Items{}, SessionMeta{}, watermark, err
	}

	msgQuery := `SELECT id, role, content_json, created_timestamp FROM messages WHERE session_id = ?`
	var args []any
	args = append(args, sessionID)
	if hasWatermark && watermark > 0 {
		msgQuery += ` AND id > ?`
		args = append(args, watermark)
	}
	msgQuery += ` ORDER BY id ASC`

	pending := make(map[string]goosePendingCall)

	// A poll can land between a toolRequest row and its toolResponse: neither
	// half of the pair would be inside the next read together, and the call
	// would be lost. Seed pending with open calls at or below the watermark -
	// before the message rows open, which otherwise hold the pooled connection.
	if hasWatermark && watermark > 0 {
		if err := seedGooseOpenCalls(ctx, db, sessionID, watermark, pending); err != nil {
			return Items{}, SessionMeta{}, watermark, err
		}
	}

	rows, err := db.QueryContext(ctx, msgQuery, args...)
	if err != nil {
		return Items{}, SessionMeta{}, watermark, fmt.Errorf("querying goose messages for %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var events []classify.Event
	var marks []classify.Mark
	seq := startSeq
	newWatermark := watermark
	newestMsgTS := int64(0)

	for rows.Next() {
		var id int64
		var role, contentJSON sql.NullString
		var createdSec sql.NullInt64
		if err := rows.Scan(&id, &role, &contentJSON, &createdSec); err != nil {
			return Items{}, SessionMeta{}, watermark, fmt.Errorf("scanning goose message: %w", err)
		}
		if id > newWatermark {
			newWatermark = id
		}
		if createdSec.Valid && createdSec.Int64 > newestMsgTS {
			newestMsgTS = createdSec.Int64
		}
		if !role.Valid || !contentJSON.Valid {
			continue
		}

		ts := secToRFC3339(createdSec.Int64)
		blocks := decodeGooseContent(contentJSON.String)

		for _, block := range blocks {
			bType, _ := block["type"].(string)
			switch {
			case role.String == "user" && bType == "text":
				if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
					if !injectedUserMessage(text) {
						marks = append(marks, classify.Mark{
							Seq:       seq,
							Timestamp: ts,
							Type:      "user-message",
							Note:      strutil.TruncateRunes(text, 2000, "…"),
						})
					}
				}

			case role.String == "assistant" && bType == "toolRequest":
				callID, toolName, callArgs := parseGooseToolRequest(block)
				if callID != "" && toolName != "" {
					pending[callID] = goosePendingCall{
						call: classify.ToolCall{
							ID:        callID,
							Name:      toolName,
							Input:     callArgs,
							Timestamp: ts,
						},
					}
				}

			case role.String == "user" && bType == "toolResponse":
				respID, content, isErr := parseGooseToolResponse(block)
				if p, ok := pending[respID]; ok {
					ev := classify.BuildEventWith(a.opts, seq, meta.Cwd, p.call, classify.ToolResult{
						Content: content,
						IsError: isErr,
					})
					events = append(events, ev)
					seq++
					delete(pending, respID)
				}

			case role.String == "assistant" && bType == "error":
				errMsg := firstString(block, "message", "error")
				if errMsg == "" {
					if k, ok := block["kind"].(string); ok {
						errMsg = k
					}
				}
				marks = append(marks, classify.Mark{
					Seq:       seq,
					Timestamp: ts,
					Type:      "error",
					Note:      strutil.TruncateRunes(errMsg, 2000, "…"),
				})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return Items{}, SessionMeta{}, watermark, fmt.Errorf("reading goose messages: %w", err)
	}

	// Flush remaining open tool calls if this is a full parse.
	if !hasWatermark && len(pending) > 0 {
		for _, p := range pending {
			ev := classify.BuildEventWith(a.opts, seq, meta.Cwd, p.call, classify.ToolResult{})
			events = append(events, ev)
			seq++
		}
	}

	// Read usage ledger entries. An incremental read delivers exactly the rows
	// in (lastMessageTSAtWatermark, newestMessageTSRead]: each usage row once,
	// the same way events and marks are withheld past the watermark.
	var usageItems []classify.Usage
	var compactions []classify.Mark
	ledgerErr := error(nil)
	if !hasWatermark || watermark == 0 {
		usageItems, compactions, ledgerErr = a.readUsageLedger(ctx, db, sessionID, seq, 0, 0)
	} else if newestMsgTS > 0 {
		var prevTS int64
		prevTS, ledgerErr = gooseLedgerCutoff(ctx, db, sessionID, watermark)
		if ledgerErr == nil {
			usageItems, compactions, ledgerErr = a.readUsageLedger(ctx, db, sessionID, seq, prevTS, newestMsgTS)
		}
	}
	if ledgerErr != nil && !strings.Contains(ledgerErr.Error(), "no such table") {
		return Items{}, SessionMeta{}, watermark, ledgerErr
	}
	marks = append(marks, compactions...)

	items := Items{
		Events: events,
		Marks:  marks,
		Usage:  usageItems,
	}
	return items, meta, newWatermark, nil
}

// gooseLedgerCutoff is the newest message timestamp at or below the watermark:
// the lower edge of the usage window for the next incremental read.
func gooseLedgerCutoff(ctx context.Context, db *sql.DB, sessionID string, watermark int64) (int64, error) {
	var cutoff int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(created_timestamp), 0) FROM messages WHERE session_id = ? AND id <= ?`,
		sessionID, watermark).Scan(&cutoff)
	if err != nil {
		return 0, fmt.Errorf("locating goose ledger cutoff for %s: %w", sessionID, err)
	}
	return cutoff, nil
}

// readUsageLedger reads usage rows; with throughTS > 0 only rows in
// (afterTS, throughTS] are returned, so incremental reads deliver each row once.
func (a GooseAdapter) readUsageLedger(ctx context.Context, db *sql.DB, sessionID string, seq int, afterTS, throughTS int64) ([]classify.Usage, []classify.Mark, error) {
	query := `SELECT created_timestamp, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost, is_compaction
		 FROM usage_ledger WHERE session_id = ?`
	args := []any{sessionID}
	if afterTS > 0 {
		query += ` AND created_timestamp > ?`
		args = append(args, afterTS)
	}
	if throughTS > 0 {
		query += ` AND created_timestamp <= ?`
		args = append(args, throughTS)
	}
	query += ` ORDER BY id ASC`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	var usages []classify.Usage
	var compactions []classify.Mark

	for rows.Next() {
		var createdSec int64
		var model sql.NullString
		var inputTokens, outputTokens, cacheRead, cacheWrite sql.NullInt64
		var cost sql.NullFloat64
		var isCompaction sql.NullInt64

		if err := rows.Scan(&createdSec, &model, &inputTokens, &outputTokens, &cacheRead, &cacheWrite, &cost, &isCompaction); err != nil {
			return nil, nil, err
		}

		if isCompaction.Valid && isCompaction.Int64 == 1 {
			compactions = append(compactions, classify.Mark{
				Seq:       seq,
				Timestamp: secToRFC3339(createdSec),
				Type:      "compaction",
			})
		}

		u := classify.Usage{
			Seq:          seq,
			At:           time.Unix(createdSec, 0).UTC(),
			Model:        model.String,
			InputTokens:  inputTokens.Int64,
			OutputTokens: outputTokens.Int64,
			CacheRead:    cacheRead.Int64,
			CacheWrite:   cacheWrite.Int64,
		}
		if cost.Valid {
			c := cost.Float64
			u.CostUSD = &c
		}
		usages = append(usages, u)
	}
	return usages, compactions, rows.Err()
}

// AgentGraph builds a parent-child tree of all Goose sessions including subagents.
func (a GooseAdapter) AgentGraph(ctx context.Context) (*AgentGraph, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dbPath := a.dbPath()
	if dbPath == "" {
		return &AgentGraph{Children: map[string][]AgentNode{}}, nil
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		return &AgentGraph{Children: map[string][]AgentNode{}}, nil
	}

	rows, err := db.QueryContext(ctx,
		`SELECT id, parent_session_id, name, created_at FROM sessions ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return &AgentGraph{Children: map[string][]AgentNode{}}, nil
	}
	defer func() { _ = rows.Close() }()

	graph := &AgentGraph{Children: map[string][]AgentNode{}}
	for rows.Next() {
		var id, name string
		var parentID, createdAt sql.NullString
		if err := rows.Scan(&id, &parentID, &name, &createdAt); err != nil {
			continue
		}
		node := AgentNode{
			SessionID: id,
			Harness:   a.Harness(),
			StartedAt: gooseTimeToRFC3339(createdAt.String),
			Title:     name,
		}
		if parentID.Valid && parentID.String != "" {
			node.ParentID = parentID.String
		}
		if node.ParentID == "" {
			graph.Roots = append(graph.Roots, node)
		} else {
			graph.Children[node.ParentID] = append(graph.Children[node.ParentID], node)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing goose sessions for agent graph: %w", err)
	}
	return graph, nil
}

func parseGooseModel(providerName, modelConfigJSON string) string {
	if modelConfigJSON != "" {
		var m struct {
			ModelName string `json:"model_name"`
		}
		if json.Unmarshal([]byte(modelConfigJSON), &m) == nil && m.ModelName != "" {
			return m.ModelName
		}
	}
	return providerName
}

func parseGooseTimestamp(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", raw, time.UTC); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), true
	}
	if sec, err := strconv.ParseInt(raw, 10, 64); err == nil && sec > 0 {
		return time.Unix(sec, 0).UTC(), true
	}
	return time.Time{}, false
}

func gooseTimeToRFC3339(raw string) string {
	t, ok := parseGooseTimestamp(raw)
	if !ok {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func decodeGooseContent(contentJSON string) []map[string]any {
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(contentJSON), &blocks); err == nil {
		return blocks
	}
	var single map[string]any
	if err := json.Unmarshal([]byte(contentJSON), &single); err == nil {
		return []map[string]any{single}
	}
	return nil
}

func parseGooseToolRequest(block map[string]any) (callID, toolName string, args map[string]any) {
	callID, _ = block["id"].(string)
	args = map[string]any{}

	if tc, ok := block["toolCall"].(map[string]any); ok {
		if val, ok := tc["value"].(map[string]any); ok {
			if n, ok := val["name"].(string); ok {
				toolName = n
			}
			if a, ok := val["arguments"].(map[string]any); ok {
				args = a
			}
		}
	}
	if toolName == "" {
		if n, ok := block["name"].(string); ok {
			toolName = n
		}
	}
	if len(args) == 0 {
		if a, ok := block["arguments"].(map[string]any); ok {
			args = a
		}
	}
	return callID, toolName, args
}

func parseGooseToolResponse(block map[string]any) (respID, content string, isErr bool) {
	respID, _ = block["id"].(string)

	if tr, ok := block["toolResult"].(map[string]any); ok {
		if val, ok := tr["value"].(map[string]any); ok {
			if ie, ok := val["isError"].(bool); ok {
				isErr = ie
			}
			if sc, ok := val["structuredContent"].(map[string]any); ok {
				if ec, ok := sc["exit_code"].(float64); ok && ec != 0 {
					isErr = true
				}
				if stdout, ok := sc["stdout"].(string); ok && stdout != "" {
					content = stdout
				}
				if stderr, ok := sc["stderr"].(string); ok && stderr != "" {
					if content != "" {
						content += "\n" + stderr
					} else {
						content = stderr
					}
				}
			}
			if content == "" {
				if c, ok := val["content"]; ok {
					content = classify.ContentToString(c)
				}
			}
		}
	}
	if content == "" {
		if c, ok := block["content"]; ok {
			content = classify.ContentToString(c)
		}
	}
	return respID, content, isErr
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
