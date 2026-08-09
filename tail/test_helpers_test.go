package tail

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTempJSONL writes content to a temp file in the test's temp dir and
// returns the path. Used by adapter tests that need fixture files.
func writeTempJSONL(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}
