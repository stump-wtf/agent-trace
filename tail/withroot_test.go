package tail

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeCodeWithRoot(t *testing.T) {
	tests := []struct {
		name     string
		root     string
		wantDir  string
		wantZero bool
	}{
		{name: "non-empty root", root: "/tmp/fake-home", wantDir: "/tmp/fake-home/.claude/projects"},
		{name: "empty root restores default", root: "", wantDir: "", wantZero: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := ClaudeCodeAdapter{}.WithRoot(tt.root)
			cc, ok := a.(ClaudeCodeAdapter)
			if !ok {
				t.Fatalf("WithRoot did not return ClaudeCodeAdapter, got %T", a)
			}
			if tt.wantZero {
				if cc.Dir != "" {
					t.Errorf("Dir = %q, want empty for empty root", cc.Dir)
				}
			} else {
				if cc.Dir != tt.wantDir {
					t.Errorf("Dir = %q, want %q", cc.Dir, tt.wantDir)
				}
			}
			// Verify SessionDir returns the expected path.
			if !tt.wantZero && a.SessionDir() != tt.wantDir {
				t.Errorf("SessionDir() = %q, want %q", a.SessionDir(), tt.wantDir)
			}
		})
	}
}

func TestCodexWithRoot(t *testing.T) {
	t.Run("non-empty root sets Dir and IndexPath", func(t *testing.T) {
		a := CodexAdapter{}.WithRoot("/tmp/home")
		cd, ok := a.(CodexAdapter)
		if !ok {
			t.Fatalf("got %T", a)
		}
		wantDir := "/tmp/home/.codex/sessions"
		wantIndex := "/tmp/home/.codex/sessions/session_index.jsonl"
		if cd.Dir != wantDir {
			t.Errorf("Dir = %q, want %q", cd.Dir, wantDir)
		}
		if cd.IndexPath != wantIndex {
			t.Errorf("IndexPath = %q, want %q", cd.IndexPath, wantIndex)
		}
	})

	t.Run("empty root restores default", func(t *testing.T) {
		a := CodexAdapter{}.WithRoot("")
		cd, ok := a.(CodexAdapter)
		if !ok {
			t.Fatalf("got %T", a)
		}
		if cd.Dir != "" {
			t.Errorf("Dir = %q, want empty", cd.Dir)
		}
		if cd.IndexPath != "" {
			t.Errorf("IndexPath = %q, want empty", cd.IndexPath)
		}
	})
}

func TestPiWithRoot(t *testing.T) {
	a := PiAdapter{}.WithRoot("/tmp/home")
	pa, ok := a.(PiAdapter)
	if !ok {
		t.Fatalf("got %T", a)
	}
	want := "/tmp/home/.pi/agent/sessions"
	if pa.Dir != want {
		t.Errorf("Dir = %q, want %q", pa.Dir, want)
	}
	if a.SessionDir() != want {
		t.Errorf("SessionDir() = %q, want %q", a.SessionDir(), want)
	}
}

func TestCrushWithRoot(t *testing.T) {
	a := CrushAdapter{}.WithRoot("/tmp/home")
	ca, ok := a.(CrushAdapter)
	if !ok {
		t.Fatalf("got %T", a)
	}
	want := "/tmp/home/.local/share/crush/projects.json"
	if ca.ProjectsPath != want {
		t.Errorf("ProjectsPath = %q, want %q", ca.ProjectsPath, want)
	}
}

func TestOpenCodeWithRoot(t *testing.T) {
	a := OpenCodeAdapter{}.WithRoot("/tmp/home")
	oa, ok := a.(OpenCodeAdapter)
	if !ok {
		t.Fatalf("got %T", a)
	}
	want := "/tmp/home/.opencode/opencode.db"
	if oa.DBPath != want {
		t.Errorf("DBPath = %q, want %q", oa.DBPath, want)
	}
}

func TestDefaultAdaptersIn(t *testing.T) {
	t.Run("returns correct count", func(t *testing.T) {
		adapters := DefaultAdaptersIn("/tmp/home")
		if len(adapters) != 3 {
			t.Fatalf("expected 3 adapters, got %d", len(adapters))
		}
	})

	t.Run("each adapter is retargeted", func(t *testing.T) {
		adapters := DefaultAdaptersIn("/tmp/home")
		for _, a := range adapters {
			dir := a.SessionDir()
			if dir == "" {
				t.Errorf("%s SessionDir() is empty after WithRoot", a.Harness())
			}
			// None of the adapters should return the real home directory.
			// They should all point under /tmp/home.
			if dir != "" && !filepath.HasPrefix(dir, "/tmp/home") {
				t.Errorf("%s SessionDir() = %q, expected under /tmp/home", a.Harness(), dir)
			}
		}
	})

	t.Run("harness types preserved", func(t *testing.T) {
		adapters := DefaultAdaptersIn("/tmp/home")
		wantHarnesses := map[Harness]bool{
			HarnessClaudeCode: false,
			HarnessCodex:      false,
			HarnessPi:         false,
		}
		for _, a := range adapters {
			if _, ok := wantHarnesses[a.Harness()]; ok {
				wantHarnesses[a.Harness()] = true
			} else {
				t.Errorf("unexpected harness %q", a.Harness())
			}
		}
		for h, found := range wantHarnesses {
			if !found {
				t.Errorf("harness %q not present", h)
			}
		}
	})

	t.Run("empty root equivalent to DefaultAdapters", func(t *testing.T) {
		adapters := DefaultAdaptersIn("")
		if len(adapters) != 3 {
			t.Fatalf("expected 3 adapters, got %d", len(adapters))
		}
		// Each adapter should have empty override fields (same as default).
		for _, a := range adapters {
			switch v := a.(type) {
			case ClaudeCodeAdapter:
				if v.Dir != "" {
					t.Errorf("ClaudeCodeAdapter.Dir = %q, want empty for empty root", v.Dir)
				}
			case CodexAdapter:
				if v.Dir != "" || v.IndexPath != "" {
					t.Errorf("CodexAdapter has non-empty fields: Dir=%q IndexPath=%q", v.Dir, v.IndexPath)
				}
			case PiAdapter:
				if v.Dir != "" {
					t.Errorf("PiAdapter.Dir = %q, want empty", v.Dir)
				}
			}
		}
	})
}

func TestWithRootReturnsAdapterInterface(t *testing.T) {
	// Verify that WithRoot on each adapter returns something that still
	// satisfies the Adapter interface (compiles + no runtime panic).
	adapters := []Adapter{
		ClaudeCodeAdapter{},
		CodexAdapter{},
		PiAdapter{},
	}
	for _, a := range adapters {
		retargeted := a.WithRoot("/tmp/test-root")
		// Exercise all interface methods to make sure nothing panics.
		_ = retargeted.Harness()
		_ = retargeted.SessionDir()
		_, _ = retargeted.ListSessions() // empty dir, should return nil/nil
	}
}

func TestWithRootDoesNotMutateOriginal(t *testing.T) {
	original := ClaudeCodeAdapter{Dir: "/original"}
	retargeted := original.WithRoot("/new-root")

	oc := original
	if oc.Dir != "/original" {
		t.Errorf("original was mutated: Dir = %q", oc.Dir)
	}

	rc, ok := retargeted.(ClaudeCodeAdapter)
	if !ok {
		t.Fatalf("got %T", retargeted)
	}
	if rc.Dir != "/new-root/.claude/projects" {
		t.Errorf("retargeted Dir = %q", rc.Dir)
	}
}

// TestWithRootPreservesSiblingFields is the regression guard for WithRoot
// constructing a fresh zero adapter instead of copying the receiver. Crush is
// the case that actually broke: it carries DBPath and Cwd for single-project
// scans, and rebuilding the struct silently discarded both, so a caller that
// configured a scan and then retargeted the root lost the scan configuration
// with no error. Codex has the same shape (Dir + IndexPath).
func TestWithRootPreservesSiblingFields(t *testing.T) {
	t.Run("crush keeps DBPath and Cwd", func(t *testing.T) {
		a := CrushAdapter{ProjectsPath: "/old/projects.json", DBPath: "/db/crush.db", Cwd: "/work"}
		got := a.WithRoot("/fake-home").(CrushAdapter)
		if got.DBPath != "/db/crush.db" || got.Cwd != "/work" {
			t.Errorf("sibling fields dropped: DBPath=%q Cwd=%q", got.DBPath, got.Cwd)
		}
		want := filepath.Join("/fake-home", ".local", "share", "crush", "projects.json")
		if got.ProjectsPath != want {
			t.Errorf("ProjectsPath = %q, want %q", got.ProjectsPath, want)
		}
	})

	t.Run("codex retargets both paths together", func(t *testing.T) {
		got := CodexAdapter{}.WithRoot("/fake-home").(CodexAdapter)
		wantDir := filepath.Join("/fake-home", ".codex", "sessions")
		if got.Dir != wantDir {
			t.Errorf("Dir = %q, want %q", got.Dir, wantDir)
		}
		// The index must follow the root, or title resolution silently reads
		// the real home while sessions come from the fake one.
		if got.IndexPath != filepath.Join(wantDir, "session_index.jsonl") {
			t.Errorf("IndexPath = %q, want it under %q", got.IndexPath, wantDir)
		}
	})

	t.Run("empty root clears only the root-derived path", func(t *testing.T) {
		a := CrushAdapter{ProjectsPath: "/x/projects.json", DBPath: "/db/crush.db", Cwd: "/work"}
		got := a.WithRoot("").(CrushAdapter)
		if got.ProjectsPath != "" {
			t.Errorf("ProjectsPath = %q, want cleared back to the default", got.ProjectsPath)
		}
		if got.DBPath != "/db/crush.db" || got.Cwd != "/work" {
			t.Errorf("empty root dropped sibling fields: DBPath=%q Cwd=%q", got.DBPath, got.Cwd)
		}
	})

	t.Run("receiver is not mutated", func(t *testing.T) {
		a := ClaudeCodeAdapter{Dir: "/original"}
		_ = a.WithRoot("/fake-home")
		if a.Dir != "/original" {
			t.Errorf("WithRoot mutated its receiver: Dir = %q", a.Dir)
		}
	})
}

// TestDefaultAdaptersInCoversEveryDefaultAdapter guards the capacity constant
// that was hardcoded to 4 while DefaultAdapters returns 3 — a mismatch that is
// harmless today but is the kind of thing that silently truncates if someone
// later switches append for indexed assignment.
func TestDefaultAdaptersInCoversEveryDefaultAdapter(t *testing.T) {
	got := DefaultAdaptersIn("/fake-home")
	if len(got) != len(DefaultAdapters()) {
		t.Fatalf("DefaultAdaptersIn returned %d adapters, DefaultAdapters has %d", len(got), len(DefaultAdapters()))
	}
	for _, a := range got {
		dir := a.SessionDir()
		if dir == "" {
			continue // adapters that resolve lazily
		}
		if !strings.HasPrefix(dir, "/fake-home") {
			t.Errorf("%s SessionDir() = %q, want it under the supplied root", a.Harness(), dir)
		}
	}
}
