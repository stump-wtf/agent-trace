package tail

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiagnosticsJSONLAdapter checks that JSONL adapters report session-dir
// status correctly: "ok" for existing dirs, "warn" for missing ones.
func TestDiagnosticsJSONLAdapter(t *testing.T) {
	dir := t.TempDir()

	// Claude Code with existing dir.
	adapter := ClaudeCodeAdapter{Dir: dir}
	checks := adapter.Diagnostics()
	if len(checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checks))
	}
	if checks[0].Status != "ok" {
		t.Errorf("status = %q, want ok", checks[0].Status)
	}

	// Claude Code with missing dir.
	adapter = ClaudeCodeAdapter{Dir: filepath.Join(dir, "nope")}
	checks = adapter.Diagnostics()
	if checks[0].Status != "warn" {
		t.Errorf("missing dir status = %q, want warn", checks[0].Status)
	}

	// Codex with existing dir.
	codexAdapter := CodexAdapter{Dir: dir}
	checks = codexAdapter.Diagnostics()
	if checks[0].Status != "ok" {
		t.Errorf("codex status = %q, want ok", checks[0].Status)
	}

	// Pi with existing dir.
	piAdapter := PiAdapter{Dir: dir}
	checks = piAdapter.Diagnostics()
	if checks[0].Status != "ok" {
		t.Errorf("pi status = %q, want ok", checks[0].Status)
	}
}

// TestDiagnosticsJSONLAdapterFileNotDir verifies that a path that exists but
// is not a directory is reported as an error.
func TestDiagnosticsJSONLAdapterFileNotDir(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "notadir")
	if err := os.WriteFile(filePath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	adapter := ClaudeCodeAdapter{Dir: filePath}
	checks := adapter.Diagnostics()
	if checks[0].Status != "error" {
		t.Errorf("status = %q, want error for non-directory", checks[0].Status)
	}
}

// TestDiagnosticsCrushAdapter checks Crush diagnostics for both DBPath and
// projects.json modes.
func TestDiagnosticsCrushAdapter(t *testing.T) {
	dir := t.TempDir()

	// With DBPath that exists.
	dbPath := filepath.Join(dir, "crush.db")
	createTestCrushDB(t, dbPath)
	adapter := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	checks := adapter.Diagnostics()
	if len(checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checks))
	}
	if checks[0].Status != "ok" {
		t.Errorf("status = %q, want ok", checks[0].Status)
	}

	// With DBPath that doesn't exist.
	adapter = CrushAdapter{DBPath: filepath.Join(dir, "missing.db")}
	checks = adapter.Diagnostics()
	if checks[0].Status != "warn" {
		t.Errorf("missing db status = %q, want warn", checks[0].Status)
	}
}

// TestDiagnosticsOpenCodeAdapter checks OpenCode diagnostics.
func TestDiagnosticsOpenCodeAdapter(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	createTestOpenCodeDB(t, dbPath)

	adapter := OpenCodeAdapter{DBPath: dbPath}
	checks := adapter.Diagnostics()
	if len(checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checks))
	}
	if checks[0].Status != "ok" {
		t.Errorf("status = %q, want ok", checks[0].Status)
	}

	// Missing DB.
	adapter = OpenCodeAdapter{DBPath: filepath.Join(dir, "missing.db")}
	checks = adapter.Diagnostics()
	if checks[0].Status != "warn" {
		t.Errorf("missing db status = %q, want warn", checks[0].Status)
	}
}

// TestDiagnosticsViaInterface verifies that adapters implementing
// DiagnosticsSource are discoverable via type assertion.
func TestDiagnosticsViaInterface(t *testing.T) {
	dir := t.TempDir()
	adapters := []Adapter{
		&ClaudeCodeAdapter{Dir: dir},
		&CodexAdapter{Dir: dir},
		&PiAdapter{Dir: dir},
		&CrushAdapter{DBPath: filepath.Join(dir, "crush.db")},
		&OpenCodeAdapter{DBPath: filepath.Join(dir, "opencode.db")},
	}
	for _, a := range adapters {
		if _, ok := a.(DiagnosticsSource); !ok {
			t.Errorf("%T does not implement DiagnosticsSource", a)
		}
	}
}
