package tail

import (
	"context"
	"os"
	"sync"
	"time"
)

// Session Summary Caching
//
// Discovery re-summarized every session on every scan. The watcher already
// skipped unchanged files before the expensive Parse, but ListSessions runs
// first and summarized everything unconditionally, so the cheap check sat one
// layer below the cost it was meant to avoid. On a real corpus that meant 406
// files re-read every 2s to notice that 2 of them had changed.
//
// A session file is append-only and, once its session ends, permanently frozen.
// Keying on (size, mtime) is therefore enough to know a prior summary still
// stands, and is the same signal the watcher's own Parse gate already trusts.
//
// @joestump-agent 08/23/2026 - Added for #80.

// SummaryCache memoizes session summaries so a file that has not changed since
// it was last summarized is not read again. It is safe for concurrent use.
//
// The zero value is not usable — call NewSummaryCache. A nil *SummaryCache is
// valid and simply caches nothing, which is what an adapter constructed without
// one does.
type SummaryCache struct {
	mu      sync.Mutex
	entries map[string]summaryEntry
	// generation advances on every Sweep. An entry touched during the current
	// generation survives the next sweep; one that is not has no live file
	// behind it any more and is dropped.
	generation uint64
}

type summaryEntry struct {
	size    int64
	modTime time.Time
	meta    SessionMeta
	// err records a summarize failure — most often "not a session file for
	// this harness". Caching it matters: without it, every foreign or
	// malformed .jsonl in the directory is re-read in full on every scan.
	err        error
	generation uint64
}

// NewSummaryCache returns an empty cache ready for use.
func NewSummaryCache() *SummaryCache {
	return &SummaryCache{entries: make(map[string]summaryEntry)}
}

// SummaryCacheSetter is an optional interface adapters implement to accept a
// SummaryCache from the watcher, mirroring OptionsSetter. The watcher installs
// one on every adapter that implements it before the first scan.
//
// Adapters must implement this with a pointer receiver so the watcher's type
// assertion succeeds on &adapter.
type SummaryCacheSetter interface {
	SetSummaryCache(c *SummaryCache)
}

// lookup returns a cached entry when one was taken from a file with the same
// size and modification time. A nil cache never hits.
func (c *SummaryCache) lookup(path string, info os.FileInfo) (summaryEntry, bool) {
	if c == nil || info == nil {
		return summaryEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[path]
	if !ok || e.size != info.Size() || !e.modTime.Equal(info.ModTime()) {
		return summaryEntry{}, false
	}
	e.generation = c.generation
	c.entries[path] = e
	return e, true
}

// store records a summary against the file identity it was taken from. A nil
// cache discards it.
func (c *SummaryCache) store(path string, info os.FileInfo, meta SessionMeta, err error) {
	if c == nil || info == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[path] = summaryEntry{
		size:       info.Size(),
		modTime:    info.ModTime(),
		meta:       meta,
		err:        err,
		generation: c.generation,
	}
}

// Sweep drops entries that were not used since the previous Sweep, bounding the
// cache to files that still exist. The watcher calls it at the end of a scan,
// by which point every live session file has been looked up.
func (c *SummaryCache) Sweep() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for path, e := range c.entries {
		if e.generation != c.generation {
			delete(c.entries, path)
		}
	}
	c.generation++
}

// Len reports how many summaries are currently cached.
func (c *SummaryCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// summarizeCached returns a summary for path, reusing the cached one when the
// file is unchanged. It is the shared body of the file-walking adapters'
// ListSessions callbacks, which are otherwise identical.
//
// A file whose info cannot be read is summarized without caching rather than
// skipped: losing the cache is better than losing the session.
func summarizeCached(
	ctx context.Context,
	c *SummaryCache,
	entry os.DirEntry,
	path string,
	summarize func(context.Context, string) (SessionMeta, error),
) (SessionMeta, error) {
	info, infoErr := entry.Info()
	if infoErr == nil {
		if e, ok := c.lookup(path, info); ok {
			return e.meta, e.err
		}
	}
	meta, err := summarize(ctx, path)
	if infoErr == nil {
		// A cancelled scan says nothing about the file, so caching that
		// result would poison the entry for every later scan.
		if ctx.Err() == nil {
			c.store(path, info, meta, err)
		}
	}
	return meta, err
}
