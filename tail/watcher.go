package tail

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
)

// Watcher monitors agent session directories for new and growing .jsonl
// files, parses events through the appropriate adapter, and emits classified
// Events on a channel. It also tracks per-session idle state based on the
// session's own event timestamps (not scan time).
type Watcher struct {
	cfg          WatchConfig
	adapters     []Adapter
	events       chan Event
	lastActivity map[string]time.Time // session key → last event time from session data
	lastEmitted  map[string]int       // session key → highest event seq emitted
	fileState    map[string]string    // session key → last known EndedAt (change detection)
	parseState   map[string]int64     // session key → incremental parse watermark (byte offset or epoch-ms)
	mu           sync.Mutex
	done         chan struct{}
	stopOnce     sync.Once
}

// Adapter is the interface each agent-specific parser implements.
type Adapter interface {
	Harness() Harness
	SessionDir() string
	ListSessions() ([]SessionMeta, error)
	Parse(path string) ([]classify.Event, []classify.Mark, SessionMeta, error)
	// WithRoot returns a copy of the adapter configured to discover sessions
	// under the given root directory instead of the compiled-in default.
	// This lets a consumer holding []Adapter from DefaultAdapters retarget
	// them without type-switching over every concrete type. Passing an
	// empty string restores the default behaviour.
	//
	// Implementations must return a pointer to the copy. The optional
	// interfaces in this package — OptionsSetter above all — are satisfied
	// with pointer receivers, so an Adapter carrying a value silently fails
	// every one of those type assertions and loses the behaviour they carry.
	WithRoot(dir string) Adapter
}

// OptionsSetter is an optional interface adapters implement to accept
// classify.Options from the watcher. When WatchConfig.VerifyPatterns is
// non-empty, the watcher calls SetOptions on each adapter that implements
// this interface before scanning, so custom verify patterns reach
// BuildEventWith inside the adapter's Parse method.
//
// Adapters must implement this with a pointer receiver so the watcher's
// type assertion succeeds on &adapter.
type OptionsSetter interface {
	SetOptions(opts *classify.Options)
}

// DiagnosticCheck is a single health-check result from an adapter's backing
// store. Consumers use these to distinguish "no sessions" (healthy but empty)
// from "directory missing", "database corrupt", or "schema drifted".
type DiagnosticCheck struct {
	Name   string // human-readable check name (e.g. "session-dir", "database")
	Status string // "ok", "warn", "error"
	Detail string // additional context
}

// DiagnosticsSource is an optional interface adapters implement to expose
// health-check information about their backing store. Consumers discover it
// via type assertion: `if d, ok := a.(DiagnosticsSource); ok { ... }`.
//
// When an adapter returns zero sessions, these checks help distinguish a
// healthy-but-empty directory from a corrupt database or missing config file.
type DiagnosticsSource interface {
	Diagnostics() []DiagnosticCheck
}

// AgentNode is a session in an agent graph tree. It carries enough metadata
// to identify and order the session without parsing its full content.
type AgentNode struct {
	SessionID string
	ParentID  string // empty for root sessions
	Harness   Harness
	StartedAt string
	Title     string
}

// AgentGraph is a tree of parent-child session relationships. Roots are
// top-level sessions (no parent). Children maps parent session IDs to their
// subagent children.
type AgentGraph struct {
	Roots    []AgentNode
	Children map[string][]AgentNode // parent session ID → children
}

// AgentGraphBuilder is an optional interface for adapters that track
// parent-child session relationships (subagent correlation). Consumers use it
// to build a cross-session tree, linking a subagent's trace as a child of
// the parent session's trace.
//
// Only SQLite-backed adapters (Crush, OpenCode) implement this — they store
// parent_session_id / parent_id in their schema. JSONL adapters don't carry
// cross-session relationship data.
type AgentGraphBuilder interface {
	AgentGraph() (*AgentGraph, error)
}

// IncrementalParser is an optional interface adapters implement to avoid
// re-reading and re-parsing the entire session on every poll. The watcher
// tracks a per-session watermark (byte offset for JSONL, epoch-ms for SQLite)
// and passes it to ParseSince along with the seq to continue from.
//
// Implementations return only events/marks discovered after the watermark,
// with seq numbers continuing from startSeq. The returned newWatermark is
// stored for the next poll. Returning newWatermark=0 resets to full-parse
// on the next cycle (used when the file shrinks or rotates).
//
// A watermark always names a point at which no tool call was left
// outstanding, so an implementation withholds events past the last such
// point rather than advancing over a call whose result has not been written
// yet. That is what keeps the stream both complete and duplicate-free.
type IncrementalParser interface {
	// ParseSince reads only content discovered after the watermark and returns
	// events/marks with seq continuing from startSeq. The returned
	// newWatermark is stored for the next poll.
	ParseSince(path string, watermark int64, startSeq int) (events []classify.Event, marks []classify.Mark, meta SessionMeta, newWatermark int64, err error)
	// Watermark returns the current high-water mark for the session at path.
	// For JSONL adapters this is the offset past the last complete line — not
	// the file size, which can land inside a record a harness is still
	// writing. For SQLite adapters it is the latest message timestamp in
	// epoch milliseconds. Called after a full Parse to establish the initial
	// watermark.
	Watermark(path string) int64
}

// jsonlCompleteOffset returns the offset just past the last newline-terminated
// line, for use as a JSONL incremental watermark. Returns 0 if the file cannot
// be read (the watcher will do a full re-parse).
//
// The file size would be the obvious answer and is the wrong one: a harness
// appending to its log can be caught mid-record, and a watermark inside a
// record makes the next poll resume past a line it never parsed.
func jsonlCompleteOffset(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	var end int64
	if err := readCompleteJSONLines(f, 0, func(_ []byte, at int64) { end = at }); err != nil {
		return 0
	}
	return end
}

// DefaultAdapters returns adapters for all supported agent harnesses.
//
// This is every adapter the package ships. The SQLite-backed pair (Crush,
// OpenCode) is included: both treat a missing or unreadable database as an
// empty result rather than an error, so a machine without those tools
// installed simply contributes no sessions.
func DefaultAdapters() []Adapter {
	return []Adapter{
		&ClaudeCodeAdapter{},
		&CodexAdapter{},
		&CrushAdapter{},
		&OpenCodeAdapter{},
		&PiAdapter{},
	}
}

// DefaultAdaptersIn returns adapters for all supported agent harnesses, each
// configured to discover sessions under root. root is a HOME-like base, not a
// session directory: every adapter appends its own layout beneath it, so
// DefaultAdaptersIn("/tmp/fake-home") reads /tmp/fake-home/.claude/projects and
// friends. That is what makes it usable both for hermetic tests and for a
// relocated $HOME. Passing an empty string is equivalent to DefaultAdapters.
//
// The adapters it returns support the same optional interfaces as
// DefaultAdapters — see the WithRoot note on Adapter for why that is worth
// stating rather than assuming.
func DefaultAdaptersIn(root string) []Adapter {
	defaults := DefaultAdapters()
	out := make([]Adapter, 0, len(defaults))
	for _, a := range defaults {
		out = append(out, a.WithRoot(root))
	}
	return out
}

// NewWatcher creates a watcher with the given config and adapters.
// Call Start to begin monitoring. NewWatcher accepts IdleConfig for
// backward compatibility — it wraps it in WatchConfig with default polling.
func NewWatcher(cfg IdleConfig, adapters []Adapter) *Watcher {
	return NewWatcherWithConfig(WatchConfig{IdleConfig: cfg}, adapters)
}

// NewWatcherWithConfig creates a watcher with full WatchConfig control.
// If cfg.VerifyPatterns is non-empty, the watcher injects them into each
// adapter that implements OptionsSetter before the first scan.
func NewWatcherWithConfig(cfg WatchConfig, adapters []Adapter) *Watcher {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if len(cfg.VerifyPatterns) > 0 {
		for _, a := range adapters {
			if os, ok := a.(OptionsSetter); ok {
				os.SetOptions(&classify.Options{
					VerifyPatterns: cfg.VerifyPatterns,
				})
			}
		}
	}
	return &Watcher{
		cfg:          cfg,
		adapters:     adapters,
		events:       make(chan Event, 256),
		lastActivity: make(map[string]time.Time),
		lastEmitted:  make(map[string]int),
		fileState:    make(map[string]string),
		parseState:   make(map[string]int64),
		done:         make(chan struct{}),
	}
}

// Events returns the channel of classified events from all watched sessions.
// The channel closes when the watcher stops.
func (w *Watcher) Events() <-chan Event { return w.events }

// LastActivity returns the time of the last event for a session, or zero if
// no events have been observed. The time comes from the session's own data,
// not from when the scan ran.
func (w *Watcher) LastActivity(sessionKey string) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastActivity[sessionKey]
}

// IsIdle reports whether a session has been idle longer than the configured
// threshold. Returns true if no events have ever been observed for the key.
func (w *Watcher) IsIdle(sessionKey string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	last, ok := w.lastActivity[sessionKey]
	if !ok {
		return true
	}
	if last.IsZero() {
		return true
	}
	return time.Since(last) > w.cfg.IdleAfter
}

// Start begins the polling loop. It re-lists sessions, re-parses files
// whose size changed, and emits only events not yet emitted for that session
// key (tracked by last-emitted seq). The call blocks until ctx is cancelled
// or Stop is called. The events channel is closed on exit.
//
// v1 uses whole-file re-parse; byte-offset tailing is a future optimization.
func (w *Watcher) Start(ctx context.Context) {
	defer close(w.events)
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	for {
		w.scanOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-w.done:
			return
		case <-ticker.C:
		}
	}
}

// Stop signals the watcher to shut down. Safe to call multiple times.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.done)
	})
}

// ScanOnce performs a single scan of all adapters, parsing every discovered
// session and emitting events. Useful for batch processing without live
// tailing. Does not close the events channel.
func (w *Watcher) ScanOnce() error {
	w.scanOnce(context.Background())
	return nil
}

func (w *Watcher) scanOnce(ctx context.Context) {
	for _, a := range w.adapters {
		select {
		case <-w.done:
			return
		case <-ctx.Done():
			return
		default:
		}
		sessions, err := a.ListSessions()
		if err != nil {
			continue
		}
		for _, meta := range sessions {
			select {
			case <-w.done:
				return
			case <-ctx.Done():
				return
			default:
			}
			w.scanSession(a, meta)
		}
	}
}

func (w *Watcher) scanSession(a Adapter, meta SessionMeta) {
	// Change detection uses the session's own EndedAt timestamp instead of
	// os.Stat file size. This works for both JSONL and SQLite sessions:
	// SQLite databases in WAL mode don't grow the main file on writes (the
	// data lands in the -wal sidecar), so file-size stat misses updates.
	// EndedAt changes whenever a new event arrives, regardless of storage.
	w.mu.Lock()
	prevEndedAt := w.fileState[meta.Key]
	w.fileState[meta.Key] = meta.EndedAt
	lastSeq := w.lastEmitted[meta.Key]
	watermark := w.parseState[meta.Key]
	firstScan := prevEndedAt == ""
	if lastSeq == 0 && firstScan {
		lastSeq = -1 // first scan: emit everything including seq 0
	}
	w.mu.Unlock()

	if meta.EndedAt == prevEndedAt && prevEndedAt != "" {
		return // unchanged
	}

	// Use incremental parsing when the adapter supports it AND this isn't the
	// first scan. On the first scan, a full Parse is required to establish the
	// baseline. Subsequent scans only read new content since the watermark.
	var events []classify.Event
	var marks []classify.Mark
	var newWatermark int64

	if ip, ok := a.(IncrementalParser); ok && !firstScan {
		var err error
		events, marks, _, newWatermark, err = ip.ParseSince(meta.Path, watermark, lastSeq+1)
		if err != nil {
			return
		}
	} else {
		var err error
		events, marks, _, err = a.Parse(meta.Path)
		if err != nil {
			return
		}
		// After a full parse, record the watermark for the next incremental
		// scan. JSONL adapters use file size; SQLite adapters use the latest
		// message timestamp. ParseSince on the next poll will use this.
		if ip, ok := a.(IncrementalParser); ok {
			newWatermark = ip.Watermark(meta.Path)
		}
	}

	w.mu.Lock()
	w.parseState[meta.Key] = newWatermark
	w.mu.Unlock()

	w.emitEvents(events, marks, meta, lastSeq)
}

// emitEvents sends new events and marks through the watcher's event channel,
// updating lastEmitted and lastActivity. Events with seq <= lastSeq are
// skipped (dedup). The caller must not hold w.mu.
func (w *Watcher) emitEvents(events []classify.Event, marks []classify.Mark, meta SessionMeta, lastSeq int) {
	now := time.Now()
	lastEventTime := parseSessionTime(meta.EndedAt)

	w.mu.Lock()
	for _, ev := range events {
		if ev.Seq <= lastSeq {
			continue
		}
		lastSeq = ev.Seq
		if ev.Timestamp != "" {
			if t := parseSessionTime(ev.Timestamp); !t.IsZero() {
				lastEventTime = t
			}
		}
		select {
		case w.events <- Event{
			Session:    meta,
			Classified: ev,
			ReceivedAt: now,
		}:
		case <-w.done:
			w.mu.Unlock()
			return
		}
	}
	for _, m := range marks {
		if m.Seq <= w.lastEmitted[meta.Key] {
			continue
		}
		_ = m
	}
	w.lastEmitted[meta.Key] = lastSeq
	if !lastEventTime.IsZero() {
		w.lastActivity[meta.Key] = lastEventTime
	}
	w.mu.Unlock()
}

// parseSessionTime parses a session timestamp, returning the zero time when it
// is missing or unparseable. It delegates to parseSessionTimeOk and discards
// the ok flag: the watcher's own uses guard on IsZero anyway. Two independent
// copies of this parse is exactly the drift the SessionMeta accessors exist to
// prevent, so there is only one.
func parseSessionTime(ts string) time.Time {
	t, _ := parseSessionTimeOk(ts)
	return t
}
