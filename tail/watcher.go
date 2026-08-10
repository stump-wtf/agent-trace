package tail

import (
	"context"
	"os"
	"sync"
	"time"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
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
	fileState    map[string]int64     // session path → last known file size
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
	WithRoot(dir string) Adapter
}

// DefaultAdapters returns adapters for all supported agent harnesses.
func DefaultAdapters() []Adapter {
	return []Adapter{
		ClaudeCodeAdapter{},
		CodexAdapter{},
		PiAdapter{},
	}
}

// DefaultAdaptersIn returns adapters for all supported agent harnesses,
// each configured to discover sessions under root. It is shorthand for
// calling WithRoot on every adapter returned by DefaultAdapters. Passing
// an empty string is equivalent to DefaultAdapters.
func DefaultAdaptersIn(root string) []Adapter {
	out := make([]Adapter, 0, 4)
	for _, a := range DefaultAdapters() {
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
func NewWatcherWithConfig(cfg WatchConfig, adapters []Adapter) *Watcher {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return &Watcher{
		cfg:          cfg,
		adapters:     adapters,
		events:       make(chan Event, 256),
		lastActivity: make(map[string]time.Time),
		lastEmitted:  make(map[string]int),
		fileState:    make(map[string]int64),
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
	// Check if file changed since last scan.
	info, err := os.Stat(meta.Path)
	if err != nil {
		return
	}
	w.mu.Lock()
	lastSize := w.fileState[meta.Path]
	w.fileState[meta.Path] = info.Size()
	lastSeq := w.lastEmitted[meta.Key]
	if lastSeq == 0 && lastSize == 0 {
		lastSeq = -1 // first scan: emit everything including seq 0
	}
	w.mu.Unlock()

	if info.Size() == lastSize && lastSize > 0 {
		return // unchanged
	}

	events, marks, _, err := a.Parse(meta.Path)
	if err != nil {
		return
	}

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
	// Emit marks that are new (seq > lastEmitted).
	for _, m := range marks {
		if m.Seq <= w.lastEmitted[meta.Key] {
			continue
		}
		// Marks ride along as metadata; the Watcher emits them as part of
		// a dedicated mark channel or the next event. For now they're
		// available via the Event.Marks field when the adapter bundles them.
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
