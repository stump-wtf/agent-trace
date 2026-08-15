package tail

import (
	"context"
	"errors"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// flakyAdapter fails its first Parse and succeeds afterwards, always reporting
// the same EndedAt. That combination is the one that matters: change
// detection keys off EndedAt, so if a failed parse is allowed to advance
// fileState, the retry looks like "nothing changed" and never happens.
type flakyAdapter struct {
	path       string
	endedAt    string
	events     []classify.Event
	parseCalls int
	failFirst  bool
}

func (f *flakyAdapter) Harness() Harness   { return "flaky" }
func (f *flakyAdapter) SessionDir() string { return f.path }
func (f *flakyAdapter) WithRoot(dir string) Adapter {
	return &flakyAdapter{path: dir, endedAt: f.endedAt, events: f.events, failFirst: f.failFirst}
}

func (f *flakyAdapter) meta() SessionMeta {
	return SessionMeta{
		Key:     sessionKey("flaky", f.path),
		ID:      "flaky-session",
		Harness: "flaky",
		Path:    f.path,
		EndedAt: f.endedAt,
	}
}

func (f *flakyAdapter) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	return []SessionMeta{f.meta()}, nil
}

func (f *flakyAdapter) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
	f.parseCalls++
	if f.failFirst && f.parseCalls == 1 {
		return nil, nil, SessionMeta{}, errors.New("database is locked")
	}
	return f.events, nil, f.meta(), nil
}

// A parse that fails must not advance change detection. Before this was
// fixed, scanSession wrote fileState before calling Parse, so a transient
// SQLite failure on the last write of a session meant its trailing events
// were never emitted — the next poll saw an unchanged EndedAt and returned
// early forever.
func TestWatcherRetriesAfterParseFailure(t *testing.T) {
	a := &flakyAdapter{
		path:      "/flaky/s1",
		endedAt:   "2026-01-01T10:00:05Z",
		failFirst: true,
		events: []classify.Event{
			{Seq: 0, Timestamp: "2026-01-01T10:00:05Z", Tool: "bash", Action: classify.ActionExec, Summary: "ran tests"},
		},
	}
	w := NewWatcher(DefaultIdleConfig(), []Adapter{a})

	// Cycle 1: Parse fails. Nothing emitted, and nothing about this scan may
	// be recorded as done.
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := drain(w.Events()); len(got) != 0 {
		t.Fatalf("failed parse emitted %d events, want 0", len(got))
	}

	// Cycle 2: same EndedAt, Parse now succeeds. The event must arrive.
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := drain(w.Events())
	if len(got) != 1 {
		t.Fatalf("after the retry got %d events, want 1 — a failed parse advanced change detection and the session was skipped", len(got))
	}
	if got[0].Classified.Summary != "ran tests" {
		t.Errorf("event = %+v", got[0].Classified)
	}
	if a.parseCalls != 2 {
		t.Errorf("Parse called %d times, want 2 (the failure must be retried)", a.parseCalls)
	}
}

// The complement: a scan that succeeds must still short-circuit on the next
// poll when EndedAt has not moved. Without this the fix would trade a missed
// retry for re-parsing every session on every cycle.
func TestWatcherStillSkipsUnchangedSessions(t *testing.T) {
	a := &flakyAdapter{
		path:    "/flaky/s2",
		endedAt: "2026-01-01T10:00:05Z",
		events: []classify.Event{
			{Seq: 0, Timestamp: "2026-01-01T10:00:05Z", Tool: "bash", Action: classify.ActionExec, Summary: "ran tests"},
		},
	}
	w := NewWatcher(DefaultIdleConfig(), []Adapter{a})

	for i := 0; i < 3; i++ {
		if err := w.ScanOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if a.parseCalls != 1 {
		t.Errorf("Parse called %d times over 3 scans, want 1 — unchanged sessions must still short-circuit", a.parseCalls)
	}
	if got := drain(w.Events()); len(got) != 1 {
		t.Errorf("got %d events over 3 scans, want 1", len(got))
	}
}
