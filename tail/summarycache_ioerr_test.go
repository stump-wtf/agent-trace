package tail

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A session file is append-only and, once finished, frozen — its (size, mtime)
// never changes again. So an I/O failure cached against that identity is served
// forever: the session disappears from discovery for the life of the process,
// and Sweep cannot rescue it because a live file is looked up every scan.
//
// A "not a <harness> session" verdict is different. It is a durable statement
// about the file's content, and caching it is the whole reason the error path
// is cached at all.
//
// @joestump-agent 08/23/2026 - Review fix for #82.
func TestSummarizeCachedDoesNotCacheIOErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := dirEntryFor(t, dir, "session.jsonl")

	c := NewSummaryCache()
	calls := 0
	ioFail := func(context.Context, string) (SessionMeta, error) {
		calls++
		return SessionMeta{}, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
	}

	for i := 0; i < 3; i++ {
		if _, err := summarizeCached(context.Background(), c, entry, path, ioFail); err == nil {
			t.Fatal("expected the I/O error to be returned")
		}
	}
	if calls != 3 {
		t.Errorf("summarize called %d times; a transient I/O error must not be memoized "+
			"against a frozen file's identity", calls)
	}
	if c.Len() != 0 {
		t.Errorf("cache holds %d entries; expected an I/O failure to leave nothing behind", c.Len())
	}
}

// The verdict the cache exists for must still be memoized.
func TestSummarizeCachedStillCachesNotASessionVerdict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foreign.jsonl")
	if err := os.WriteFile(path, []byte(`{"unrelated":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := dirEntryFor(t, dir, "foreign.jsonl")

	c := NewSummaryCache()
	calls := 0
	verdict := func(context.Context, string) (SessionMeta, error) {
		calls++
		return SessionMeta{}, errors.New("not a Claude Code session: " + path)
	}

	for i := 0; i < 3; i++ {
		if _, err := summarizeCached(context.Background(), c, entry, path, verdict); err == nil {
			t.Fatal("expected the verdict to be returned")
		}
	}
	if calls != 1 {
		t.Errorf("summarize called %d times; a content verdict must be memoized (that is "+
			"what stops every foreign .jsonl being re-read on every scan)", calls)
	}
}

func dirEntryFor(t *testing.T, dir, name string) os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == name {
			return e
		}
	}
	t.Fatalf("no dir entry for %s", name)
	return nil
}
