package tail

import (
	"sync"
	"time"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
)

// Watcher monitors agent session directories for new and growing .jsonl
// files, parses events through the appropriate adapter, and emits classified
// Events on a channel. It also tracks per-session idle state.
type Watcher struct {
	cfg      IdleConfig
	adapters []Adapter
	events   chan Event
	idle     map[string]time.Time // session key → last event time
	mu       sync.Mutex
	done     chan struct{}
}

// Adapter is the interface each agent-specific parser implements.
type Adapter interface {
	Harness() Harness
	SessionDir() string
	ListSessions() ([]SessionMeta, error)
	Parse(path string) ([]classify.Event, []classify.Mark, SessionMeta, error)
}

// DefaultAdapters returns adapters for all supported agent harnesses.
func DefaultAdapters() []Adapter {
	return []Adapter{
		ClaudeCodeAdapter{},
		CodexAdapter{},
		PiAdapter{},
	}
}

// NewWatcher creates a watcher with the given idle config and adapters.
// Call Start to begin monitoring.
func NewWatcher(cfg IdleConfig, adapters []Adapter) *Watcher {
	return &Watcher{
		cfg:      cfg,
		adapters: adapters,
		events:   make(chan Event, 256),
		idle:     make(map[string]time.Time),
		done:     make(chan struct{}),
	}
}

// Events returns the channel of classified events from all watched sessions.
func (w *Watcher) Events() <-chan Event { return w.events }

// LastActivity returns the time of the last event for a session, or zero if
// no events have been observed.
func (w *Watcher) LastActivity(sessionKey string) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.idle[sessionKey]
}

// IsIdle reports whether a session has been idle longer than the configured
// threshold. Returns true if no events have ever been observed for the key.
func (w *Watcher) IsIdle(sessionKey string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	last, ok := w.idle[sessionKey]
	if !ok {
		return true
	}
	return time.Since(last) > w.cfg.IdleAfter
}

// Stop shuts down the watcher and closes the events channel.
func (w *Watcher) Stop() {
	close(w.done)
}

// ScanOnce performs a single scan of all adapters, parsing every discovered
// session and emitting events. Useful for batch processing without live
// tailing.
func (w *Watcher) ScanOnce() error {
	for _, a := range w.adapters {
		sessions, err := a.ListSessions()
		if err != nil {
			continue
		}
		for _, meta := range sessions {
			events, _, _, err := a.Parse(meta.Path)
			if err != nil {
				continue
			}
			now := time.Now()
			for _, ev := range events {
				select {
				case w.events <- Event{
					Session:    meta,
					Classified: ev,
					ReceivedAt: now,
				}:
				case <-w.done:
					return nil
				}
			}
			w.mu.Lock()
			w.idle[meta.Key] = now
			w.mu.Unlock()
		}
	}
	return nil
}
