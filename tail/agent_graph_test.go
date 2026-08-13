package tail

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestCrushAgentGraph verifies that CrushAdapter builds a correct parent-child
// tree from sessions with parent_session_id relationships.
func TestCrushAgentGraph(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UnixMilli()
	// Root session.
	_, err = db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
		"root-001", "Root Session", now, now+1000)
	if err != nil {
		t.Fatal(err)
	}
	// Child session (subagent).
	_, err = db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"child-001", "Child Session", "root-001", now+500, now+1500)
	if err != nil {
		t.Fatal(err)
	}
	// Another root.
	_, err = db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES (?, ?, '', ?, ?)`,
		"root-002", "Second Root", now+2000, now+3000)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	adapter := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	graph, err := adapter.AgentGraph()
	if err != nil {
		t.Fatalf("AgentGraph: %v", err)
	}

	if len(graph.Roots) != 2 {
		t.Errorf("expected 2 roots, got %d", len(graph.Roots))
	}
	if children, ok := graph.Children["root-001"]; !ok {
		t.Error("expected child for root-001")
	} else if len(children) != 1 {
		t.Errorf("expected 1 child for root-001, got %d", len(children))
	} else if children[0].SessionID != "child-001" {
		t.Errorf("child session ID = %q, want child-001", children[0].SessionID)
	}
}

// TestOpenCodeAgentGraph verifies that OpenCodeAdapter builds a correct
// parent-child tree from sessions with parent_id relationships.
func TestOpenCodeAgentGraph(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`INSERT INTO session (id, project_id, slug, directory, title, version, parent_id, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"root-001", "proj-1", "root-slug", "/test", "Root", "1.0", "", 1784148215000, 1784148215000)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO session (id, project_id, slug, directory, title, version, parent_id, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"child-001", "proj-1", "child-slug", "/test", "Child", "1.0", "root-001", 1784148216000, 1784148216000)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	adapter := OpenCodeAdapter{DBPath: dbPath}
	graph, err := adapter.AgentGraph()
	if err != nil {
		t.Fatalf("AgentGraph: %v", err)
	}

	if len(graph.Roots) != 1 {
		t.Errorf("expected 1 root, got %d", len(graph.Roots))
	}
	if children, ok := graph.Children["root-001"]; !ok {
		t.Error("expected child for root-001")
	} else if len(children) != 1 {
		t.Errorf("expected 1 child, got %d", len(children))
	}
}

// TestAgentGraphViaInterface verifies adapters are discoverable as
// AgentGraphBuilder via type assertion.
func TestAgentGraphViaInterface(t *testing.T) {
	adapters := []Adapter{
		&CrushAdapter{},
		&OpenCodeAdapter{},
	}
	for _, a := range adapters {
		if _, ok := a.(AgentGraphBuilder); !ok {
			t.Errorf("%T does not implement AgentGraphBuilder", a)
		}
	}
}

// TestAgentGraphEmptyDB verifies graceful handling when the DB has no sessions.
func TestAgentGraphEmptyDB(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)

	adapter := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	graph, err := adapter.AgentGraph()
	if err != nil {
		t.Fatalf("AgentGraph on empty DB: %v", err)
	}
	if len(graph.Roots) != 0 {
		t.Errorf("expected 0 roots, got %d", len(graph.Roots))
	}
}
