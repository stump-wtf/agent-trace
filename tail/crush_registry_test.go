package tail

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCrushRegistry creates one project (data dir + crush.db holding one
// top-level session) and returns the projects.json body that registers it.
func writeCrushRegistry(t *testing.T, root string) (projectDir string, body []byte) {
	t.Helper()
	projectDir = filepath.Join(root, "project")
	dbPath := filepath.Join(projectDir, ".crush", "crush.db")
	createTestCrushDB(t, dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO sessions (id, title, parent_session_id, created_at, updated_at) VALUES ('sess-1', 'one', NULL, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	body, err = json.MarshalIndent(crushProjectsFile{Projects: []crushProjectEntry{{
		Path:       projectDir,
		DataDir:    filepath.Join(projectDir, ".crush"),
		LastAccess: time.Now().UTC().Format(time.RFC3339Nano),
	}}}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return projectDir, body
}

// TestCrushRegistryTrailingBytesStillDiscovers reproduces a registry found on
// a live host: a complete document followed by a stray "}", the residue of two
// Crush processes truncating and rewriting projects.json concurrently. A strict
// decode rejected the whole file and discovery returned zero sessions.
func TestCrushRegistryTrailingBytesStillDiscovers(t *testing.T) {
	root := t.TempDir()
	projectDir, body := writeCrushRegistry(t, root)
	pp := filepath.Join(root, "projects.json")
	if err := os.WriteFile(pp, append(body, '}'), 0o600); err != nil {
		t.Fatal(err)
	}

	a := CrushAdapter{ProjectsPath: pp}
	metas, err := a.ListSessions(t.Context())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("got %d sessions from a registry with trailing bytes, want 1", len(metas))
	}
	if metas[0].Cwd != projectDir {
		t.Errorf("Cwd = %q, want %q", metas[0].Cwd, projectDir)
	}
}

// TestCrushDiagnosticsRegistryHealth checks that the projects-json check
// reports what discovery will actually be able to do, not merely that a file
// exists.
func TestCrushDiagnosticsRegistryHealth(t *testing.T) {
	root := t.TempDir()
	_, body := writeCrushRegistry(t, root)

	cases := []struct {
		name       string
		content    []byte
		wantStatus string
		wantDetail string
	}{
		{"clean", body, "ok", ""},
		{"trailing whitespace is clean", append(append([]byte{}, body...), "\n\n"...), "ok", ""},
		{"trailing brace", append(append([]byte{}, body...), '}'), "warn", "trailing bytes"},
		{"undecodable", []byte("{\"projects\": ["), "warn", "does not parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pp := filepath.Join(t.TempDir(), "projects.json")
			if err := os.WriteFile(pp, tc.content, 0o600); err != nil {
				t.Fatal(err)
			}
			checks := CrushAdapter{ProjectsPath: pp}.Diagnostics()
			if len(checks) != 1 {
				t.Fatalf("expected 1 check, got %d", len(checks))
			}
			if checks[0].Status != tc.wantStatus {
				t.Errorf("status = %q (%s), want %q", checks[0].Status, checks[0].Detail, tc.wantStatus)
			}
			if tc.wantDetail != "" && !strings.Contains(checks[0].Detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", checks[0].Detail, tc.wantDetail)
			}
		})
	}

	// A missing registry is still a warning, as before.
	checks := CrushAdapter{ProjectsPath: filepath.Join(root, "absent.json")}.Diagnostics()
	if checks[0].Status != "warn" {
		t.Errorf("missing registry status = %q, want warn", checks[0].Status)
	}
}
