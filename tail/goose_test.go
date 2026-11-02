package tail

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	_ "modernc.org/sqlite"
)

func createTestGooseDB(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	schema := []string{
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			user_set_name BOOLEAN DEFAULT FALSE,
			session_type TEXT NOT NULL DEFAULT 'user',
			working_dir TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			extension_data TEXT DEFAULT '{}',
			total_tokens INTEGER,
			input_tokens INTEGER,
			output_tokens INTEGER,
			cache_read_tokens INTEGER,
			cache_write_tokens INTEGER,
			accumulated_total_tokens INTEGER,
			accumulated_input_tokens INTEGER,
			accumulated_output_tokens INTEGER,
			accumulated_cache_read_tokens INTEGER,
			accumulated_cache_write_tokens INTEGER,
			accumulated_cost REAL,
			schedule_id TEXT,
			recipe_json TEXT,
			user_recipe_values_json TEXT,
			provider_name TEXT,
			model_config_json TEXT,
			goose_mode TEXT NOT NULL DEFAULT 'auto',
			archived_at TIMESTAMP,
			project_id TEXT,
			parent_session_id TEXT
		)`,
		`CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id TEXT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			role TEXT NOT NULL,
			content_json TEXT NOT NULL,
			created_timestamp INTEGER NOT NULL,
			timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			tokens INTEGER,
			metadata_json TEXT
		)`,
		`CREATE TABLE usage_ledger (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			created_timestamp INTEGER NOT NULL,
			model TEXT,
			input_tokens INTEGER,
			output_tokens INTEGER,
			total_tokens INTEGER,
			cache_read_tokens INTEGER,
			cache_write_tokens INTEGER,
			cost REAL,
			cost_source TEXT,
			is_compaction INTEGER DEFAULT 0
		)`,
	}
	for _, q := range schema {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("creating test goose schema: %v", err)
		}
	}
}

func TestGooseDiagnostics(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")

	a := GooseAdapter{DBPath: dbPath}
	diag := a.Diagnostics()
	if len(diag) != 1 || diag[0].Status != "warn" {
		t.Fatalf("expected warn for non-existent database, got %v", diag)
	}

	createTestGooseDB(t, dbPath)
	diag = a.Diagnostics()
	if len(diag) != 1 || diag[0].Status != "ok" {
		t.Fatalf("expected ok for existing database, got %v", diag)
	}
}

func TestGooseWithRoot(t *testing.T) {
	a := GooseAdapter{}
	retargeted := a.WithRoot("/custom/root").(*GooseAdapter)
	want := filepath.Join("/custom/root", ".local", "share", "goose", "sessions", "sessions.db")
	if retargeted.DBPath != want {
		t.Errorf("DBPath = %q, want %q", retargeted.DBPath, want)
	}

	cleared := retargeted.WithRoot("").(*GooseAdapter)
	if cleared.DBPath != "" {
		t.Errorf("cleared DBPath = %q, want empty", cleared.DBPath)
	}
}

func TestGooseFilteredListerMatchesInMemory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	cwd := filepath.Join(dir, "workdir")
	createTestGooseDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		sid := fmt.Sprintf("20260801_%d", i+1)
		title := fmt.Sprintf("Session %d", i+1)
		tStr := base.Add(time.Duration(i) * time.Hour).Format("2006-01-02 15:04:05")
		_, err := db.Exec(`INSERT INTO sessions (id, name, working_dir, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			sid, title, cwd, tStr, tStr)
		if err != nil {
			t.Fatal(err)
		}
	}

	a := GooseAdapter{DBPath: dbPath}
	assertFilteredMatchesInMemory(t, a, filterMatrix(base, cwd))
}

func TestGooseParse(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	cwd := filepath.Join(dir, "repo")
	createTestGooseDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	sessionID := "20260802_1"
	_, err = db.Exec(`INSERT INTO sessions (id, name, working_dir, session_type, provider_name, model_config_json, created_at, updated_at)
		VALUES (?, ?, ?, 'user', 'openai', '{"model_name":"gpt-5.6-sol"}', '2026-08-02 12:00:00', '2026-08-02 12:05:00')`,
		sessionID, "Test Session", cwd)
	if err != nil {
		t.Fatal(err)
	}

	// 1. User message
	userContent := `[{"type":"text","text":"List the files in the directory"}]`
	_, err = db.Exec(`INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp)
		VALUES (1, 'm1', ?, 'user', ?, 1785710000)`, sessionID, userContent)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Assistant tool request
	asstReq := `[{"type":"toolRequest","id":"call_123","toolCall":{"status":"success","value":{"name":"shell","arguments":{"command":"ls -la"}}}}]`
	_, err = db.Exec(`INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp)
		VALUES (2, 'm2', ?, 'assistant', ?, 1785710002)`, sessionID, asstReq)
	if err != nil {
		t.Fatal(err)
	}

	// 3. User tool response
	userResp := `[{"type":"toolResponse","id":"call_123","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"file1.go\nfile2.go"}],"structuredContent":{"stdout":"file1.go\nfile2.go","exit_code":0},"isError":false}}}]`
	_, err = db.Exec(`INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp)
		VALUES (3, 'm3', ?, 'user', ?, 1785710004)`, sessionID, userResp)
	if err != nil {
		t.Fatal(err)
	}

	// 4. Assistant error block
	asstErr := `[{"type":"error","kind":"rate_limit","message":"Too many requests"}]`
	_, err = db.Exec(`INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp)
		VALUES (4, 'm4', ?, 'assistant', ?, 1785710006)`, sessionID, asstErr)
	if err != nil {
		t.Fatal(err)
	}

	// 5. Usage ledger entry
	_, err = db.Exec(`INSERT INTO usage_ledger (id, session_id, created_timestamp, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost, is_compaction)
		VALUES (1, ?, 1785710005, 'gpt-5.6-sol', 100, 50, 20, 10, 0.005, 0)`, sessionID)
	if err != nil {
		t.Fatal(err)
	}

	// 6. Compaction entry in usage ledger
	_, err = db.Exec(`INSERT INTO usage_ledger (id, session_id, created_timestamp, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost, is_compaction)
		VALUES (2, ?, 1785710007, 'gpt-5.6-sol', 0, 0, 0, 0, 0.0, 1)`, sessionID)
	if err != nil {
		t.Fatal(err)
	}

	a := GooseAdapter{DBPath: dbPath}
	path := dbPath + "/" + sessionID

	items, meta, err := a.ParseItems(context.Background(), path)
	if err != nil {
		t.Fatalf("ParseItems failed: %v", err)
	}

	if meta.ID != sessionID || meta.Model != "gpt-5.6-sol" || meta.Title != "Test Session" {
		t.Errorf("unexpected meta: %+v", meta)
	}

	// Check events
	if len(items.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(items.Events))
	}
	ev := items.Events[0]
	if ev.Tool != "shell" || ev.Action != classify.ActionSearch {
		t.Errorf("event mismatch: tool=%q action=%q", ev.Tool, ev.Action)
	}

	// Check marks: user-message, error, compaction
	if len(items.Marks) != 3 {
		t.Fatalf("expected 3 marks, got %d: %+v", len(items.Marks), items.Marks)
	}
	if items.Marks[0].Type != "user-message" || items.Marks[0].Note != "List the files in the directory" {
		t.Errorf("mark 0 mismatch: %+v", items.Marks[0])
	}
	if items.Marks[1].Type != "error" || items.Marks[1].Note != "Too many requests" {
		t.Errorf("mark 1 mismatch: %+v", items.Marks[1])
	}
	if items.Marks[2].Type != "compaction" {
		t.Errorf("mark 2 mismatch: %+v", items.Marks[2])
	}

	// Check usage
	if len(items.Usage) != 2 {
		t.Fatalf("expected 2 usage items, got %d", len(items.Usage))
	}
	u := items.Usage[0]
	if u.InputTokens != 100 || u.OutputTokens != 50 || u.CostUSD == nil || *u.CostUSD != 0.005 {
		t.Errorf("usage mismatch: %+v", u)
	}
}

func TestGooseParseSinceIncrementalParity(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	cwd := filepath.Join(dir, "repo")
	createTestGooseDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	sessionID := "20260802_2"
	_, err = db.Exec(`INSERT INTO sessions (id, name, working_dir, session_type, created_at, updated_at)
		VALUES (?, 'Incremental Parity', ?, 'user', '2026-08-02 12:00:00', '2026-08-02 12:05:00')`,
		sessionID, cwd)
	if err != nil {
		t.Fatal(err)
	}

	// Turn 1
	_, _ = db.Exec(`INSERT INTO messages (id, session_id, role, content_json, created_timestamp)
		VALUES (1, ?, 'user', '[{"type":"text","text":"first turn"}]', 1785710000)`, sessionID)
	_, _ = db.Exec(`INSERT INTO messages (id, session_id, role, content_json, created_timestamp)
		VALUES (2, ?, 'assistant', '[{"type":"toolRequest","id":"c1","toolCall":{"value":{"name":"tree","arguments":{"path":"."}}}}]', 1785710001)`, sessionID)
	_, _ = db.Exec(`INSERT INTO messages (id, session_id, role, content_json, created_timestamp)
		VALUES (3, ?, 'user', '[{"type":"toolResponse","id":"c1","toolResult":{"value":{"content":"ok"}}}]', 1785710002)`, sessionID)

	a := GooseAdapter{DBPath: dbPath}
	path := dbPath + "/" + sessionID

	// First incremental parse
	items1, _, wm1, err := a.ParseItemsSince(context.Background(), path, 0, 0)
	if err != nil {
		t.Fatalf("first ParseItemsSince failed: %v", err)
	}
	if wm1 != 3 {
		t.Fatalf("expected watermark 3, got %d", wm1)
	}
	if len(items1.Events) != 1 || len(items1.Marks) != 1 {
		t.Fatalf("items1: events=%d marks=%d", len(items1.Events), len(items1.Marks))
	}

	// Turn 2
	_, _ = db.Exec(`INSERT INTO messages (id, session_id, role, content_json, created_timestamp)
		VALUES (4, ?, 'user', '[{"type":"text","text":"second turn"}]', 1785710010)`, sessionID)
	_, _ = db.Exec(`INSERT INTO messages (id, session_id, role, content_json, created_timestamp)
		VALUES (5, ?, 'assistant', '[{"type":"toolRequest","id":"c2","toolCall":{"value":{"name":"write","arguments":{"path":"foo.txt","content":"hello"}}}}]', 1785710011)`, sessionID)
	_, _ = db.Exec(`INSERT INTO messages (id, session_id, role, content_json, created_timestamp)
		VALUES (6, ?, 'user', '[{"type":"toolResponse","id":"c2","toolResult":{"value":{"content":"written"}}}]', 1785710012)`, sessionID)

	// Second incremental parse
	items2, _, wm2, err := a.ParseItemsSince(context.Background(), path, wm1, len(items1.Events))
	if err != nil {
		t.Fatalf("second ParseItemsSince failed: %v", err)
	}
	if wm2 != 6 {
		t.Fatalf("expected watermark 6, got %d", wm2)
	}
	if len(items2.Events) != 1 || len(items2.Marks) != 1 {
		t.Fatalf("items2: events=%d marks=%d", len(items2.Events), len(items2.Marks))
	}

	// Full parse comparison
	fullItems, _, err := a.ParseItems(context.Background(), path)
	if err != nil {
		t.Fatalf("full ParseItems failed: %v", err)
	}
	if len(fullItems.Events) != len(items1.Events)+len(items2.Events) {
		t.Errorf("event count mismatch: full=%d, combined=%d", len(fullItems.Events), len(items1.Events)+len(items2.Events))
	}
	if len(fullItems.Marks) != len(items1.Marks)+len(items2.Marks) {
		t.Errorf("marks count mismatch: full=%d, combined=%d", len(fullItems.Marks), len(items1.Marks)+len(items2.Marks))
	}
}

func TestGooseAgentGraph(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	createTestGooseDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	// Root session
	_, _ = db.ExecContext(ctx, `INSERT INTO sessions (id, name, working_dir, session_type, created_at)
		VALUES ('root-1', 'Root Session', '/repo', 'user', '2026-08-01 10:00:00')`)
	// Child subagent sessions
	_, _ = db.ExecContext(ctx, `INSERT INTO sessions (id, parent_session_id, name, working_dir, session_type, created_at)
		VALUES ('sub-1', 'root-1', 'Subagent 1', '/repo', 'sub_agent', '2026-08-01 10:05:00')`)
	_, _ = db.ExecContext(ctx, `INSERT INTO sessions (id, parent_session_id, name, working_dir, session_type, created_at)
		VALUES ('sub-2', 'root-1', 'Subagent 2', '/repo', 'sub_agent', '2026-08-01 10:10:00')`)

	a := GooseAdapter{DBPath: dbPath}
	graph, err := a.AgentGraph(ctx)
	if err != nil {
		t.Fatalf("AgentGraph failed: %v", err)
	}

	if len(graph.Roots) != 1 || graph.Roots[0].SessionID != "root-1" {
		t.Fatalf("unexpected roots: %+v", graph.Roots)
	}
	children := graph.Children["root-1"]
	if len(children) != 2 {
		t.Fatalf("expected 2 children for root-1, got %d", len(children))
	}
	if children[0].SessionID != "sub-1" || children[1].SessionID != "sub-2" {
		t.Errorf("children order mismatch: %+v", children)
	}
}

func TestGooseOrphanedToolCalls(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	cwd := filepath.Join(dir, "repo")
	createTestGooseDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	sessionID := "20260802_orphaned"
	_, _ = db.Exec(`INSERT INTO sessions (id, name, working_dir, session_type, created_at)
		VALUES (?, 'Orphaned Call', ?, 'user', '2026-08-02 12:00:00')`, sessionID, cwd)

	// Assistant issues toolRequest, session dies without response
	_, _ = db.Exec(`INSERT INTO messages (id, session_id, role, content_json, created_timestamp)
		VALUES (1, ?, 'assistant', '[{"type":"toolRequest","id":"orphan_1","toolCall":{"value":{"name":"shell","arguments":{"command":"cargo test"}}}}]', 1785710000)`, sessionID)

	a := GooseAdapter{DBPath: dbPath}
	events, _, _, err := a.Parse(context.Background(), dbPath+"/"+sessionID)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event from flushed orphaned call, got %d", len(events))
	}
	if events[0].Tool != "shell" || events[0].Action != classify.ActionVerify {
		t.Errorf("orphaned event mismatch: tool=%q action=%q", events[0].Tool, events[0].Action)
	}
}
