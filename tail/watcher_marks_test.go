package tail

import (
	"context"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// scriptAdapter is an Adapter whose Parse and ListSessions results are
// scripted per scan, so watcher mark-emission tests can drive exact cycles
// synchronously through ScanOnce instead of racing a polling loop. Each
// ListSessions call advances the session's EndedAt so the watcher's change
// detection re-parses on every ScanOnce.
//
// It is a full-parse adapter on purpose — it implements nothing beyond
// Adapter — so every cycle re-derives the whole session and exercises the
// watcher's seq-based dedup. incrScriptAdapter below is the incremental twin.
type scriptAdapter struct {
	path  string
	scans []scriptScan
	calls int
}

type scriptScan struct {
	events    []classify.Event
	marks     []classify.Mark
	endedAt   string
	watermark int64
}

func (s *scriptAdapter) Harness() Harness   { return "script" }
func (s *scriptAdapter) SessionDir() string { return s.path }
func (s *scriptAdapter) WithRoot(dir string) Adapter {
	return &scriptAdapter{path: dir, scans: s.scans}
}

func (s *scriptAdapter) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	// Each ScanOnce lists exactly once, so this is the cycle counter both the
	// meta and the parse results key off.
	s.calls++
	scan := s.scans[min(s.calls-1, len(s.scans)-1)]
	return []SessionMeta{{
		Key:     sessionKey("script", s.path),
		ID:      "script-session",
		Harness: "script",
		Path:    s.path,
		EndedAt: scan.endedAt,
	}}, nil
}

func (s *scriptAdapter) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
	scan := s.scans[min(s.calls-1, len(s.scans)-1)]
	meta := SessionMeta{
		Key:     sessionKey("script", s.path),
		ID:      "script-session",
		Harness: "script",
		Path:    s.path,
		EndedAt: scan.endedAt,
	}
	return scan.events, scan.marks, meta, nil
}

// drain pulls everything currently buffered on the events channel.
func drain(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

// incrScriptAdapter is scriptAdapter plus the IncrementalParser interface.
// The first scan uses the inherited full Parse from scans; every later scan
// returns the corresponding sinceScans entry, which must contain ONLY content
// after the watermark — that is the IncrementalParser contract, and the point
// of having a separate type.
type incrScriptAdapter struct {
	scriptAdapter
	sinceScans []scriptScan
}

func (s *incrScriptAdapter) ParseSince(ctx context.Context, path string, watermark int64, startSeq int) ([]classify.Event, []classify.Mark, SessionMeta, int64, error) {
	scan := s.sinceScans[min(s.calls-1, len(s.sinceScans)-1)]
	meta := SessionMeta{
		Key:     sessionKey("script", s.path),
		ID:      "script-session",
		Harness: "script",
		Path:    s.path,
		EndedAt: scan.endedAt,
	}
	return scan.events, scan.marks, meta, scan.watermark, nil
}

func (s *incrScriptAdapter) Watermark(ctx context.Context, path string) int64 { return 1 }

func TestWatcherMarksRideTheirSiblingEvents(t *testing.T) {
	// One cycle: a user message (seq 0, a mark) followed by a tool call (seq
	// 1, an event). The mark rides the event that follows it.
	a := &scriptAdapter{path: "/script/s1", scans: []scriptScan{{
		marks: []classify.Mark{
			{Seq: 0, Timestamp: "2026-01-01T10:00:00Z", Type: "user-message", Note: "fix the login bug"},
		},
		events: []classify.Event{
			{Seq: 1, Timestamp: "2026-01-01T10:00:02Z", Tool: "bash", Action: classify.ActionExec, Summary: "ran tests"},
		},
		endedAt: "2026-01-01T10:00:02Z",
	}}}
	w := NewWatcher(DefaultIdleConfig(), []Adapter{a})
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := drain(w.Events())
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if len(got[0].Marks) != 1 {
		t.Fatalf("event carries %d marks, want 1 — marks must not be dropped", len(got[0].Marks))
	}
	if got[0].Marks[0].Type != "user-message" || got[0].Marks[0].Note != "fix the login bug" {
		t.Errorf("riding mark = %+v", got[0].Marks[0])
	}
}

func TestWatcherTrailingMarksParkUntilTheNextEvent(t *testing.T) {
	// Cycle 1 ends on a user message — a mark with no event after it. Nothing
	// is lost: the mark parks, and cycle 2's event carries it. The second
	// cycle full-parses (the scripted adapter has no incremental path here),
	// so the mark reappears in the parse result too — the dedup by seq keeps
	// it from being emitted twice.
	a := &scriptAdapter{path: "/script/s2", scans: []scriptScan{
		{
			events: []classify.Event{
				{Seq: 1, Timestamp: "2026-01-01T10:00:02Z", Tool: "bash", Action: classify.ActionExec, Summary: "ran tests"},
			},
			marks: []classify.Mark{
				{Seq: 2, Timestamp: "2026-01-01T10:01:00Z", Type: "user-message", Note: "now fix the logout bug"},
			},
			endedAt: "2026-01-01T10:01:00Z",
		},
		{
			events: []classify.Event{
				{Seq: 1, Timestamp: "2026-01-01T10:00:02Z", Tool: "bash", Action: classify.ActionExec, Summary: "ran tests"},
				{Seq: 3, Timestamp: "2026-01-01T10:02:00Z", Tool: "bash", Action: classify.ActionExec, Summary: "ran more tests"},
			},
			marks: []classify.Mark{
				{Seq: 2, Timestamp: "2026-01-01T10:01:00Z", Type: "user-message", Note: "now fix the logout bug"},
			},
			endedAt: "2026-01-01T10:02:00Z",
		},
	}}
	w := NewWatcher(DefaultIdleConfig(), []Adapter{a})
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := drain(w.Events())
	if len(got) != 1 || len(got[0].Marks) != 0 {
		t.Fatalf("cycle 1: got %d events (marks %d on first), want 1 event with no marks — the trailing mark has nothing to ride yet", len(got), len(got[0].Marks))
	}
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = drain(w.Events())
	if len(got) != 1 {
		t.Fatalf("cycle 2: got %d events, want 1 (seq 1 already emitted)", len(got))
	}
	if len(got[0].Marks) != 1 || got[0].Marks[0].Note != "now fix the logout bug" {
		t.Fatalf("parked mark did not ride the next event: %+v", got[0].Marks)
	}
}

func TestWatcherMarksDedupAcrossFullReparses(t *testing.T) {
	// An adapter with no incremental path full-parses on every scan, so the
	// same marks come back every cycle. They must be emitted exactly once.
	a := &scriptAdapter{path: "/script/s3", scans: []scriptScan{
		{
			marks: []classify.Mark{
				{Seq: 0, Timestamp: "2026-01-01T10:00:00Z", Type: "user-message", Note: "hello"},
			},
			events: []classify.Event{
				{Seq: 1, Timestamp: "2026-01-01T10:00:02Z", Tool: "bash", Action: classify.ActionExec, Summary: "work"},
			},
			endedAt: "2026-01-01T10:00:02Z",
		},
		{
			marks: []classify.Mark{
				{Seq: 0, Timestamp: "2026-01-01T10:00:00Z", Type: "user-message", Note: "hello"},
			},
			events: []classify.Event{
				{Seq: 1, Timestamp: "2026-01-01T10:00:02Z", Tool: "bash", Action: classify.ActionExec, Summary: "work"},
			},
			endedAt: "2026-01-01T10:00:09Z",
		},
	}}
	w := NewWatcher(DefaultIdleConfig(), []Adapter{a})
	for range 2 {
		if err := w.ScanOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := drain(w.Events())
	if len(got) != 1 {
		t.Fatalf("got %d events across two full re-parses, want 1", len(got))
	}
	if len(got[0].Marks) != 1 {
		t.Fatalf("event carries %d marks, want 1", len(got[0].Marks))
	}
}

func TestWatcherIncrementalMarksSurviveTheWatermark(t *testing.T) {
	// Incremental parsing never repeats content, so a trailing mark parked in
	// one cycle only ever comes back via pendingMarks. Cycle 1 full-parses
	// (first scan) and parks the mark — nothing to ride. Cycle 2's ParseSince
	// returns ONLY the new event, per the IncrementalParser contract, so the
	// parked mark must survive via pendingMarks and ride it.
	base := scriptAdapter{path: "/script/s4", scans: []scriptScan{
		{
			marks: []classify.Mark{
				{Seq: 1, Timestamp: "2026-01-01T10:00:30Z", Type: "user-message", Note: "go"},
			},
			endedAt: "2026-01-01T10:00:30Z",
		},
		// Cycle 2 never full-parses, but ListSessions still needs a changed
		// EndedAt or the watcher's change detection skips the session.
		{endedAt: "2026-01-01T10:00:40Z"},
	}}
	a := &incrScriptAdapter{scriptAdapter: base, sinceScans: []scriptScan{
		{
			events: []classify.Event{
				{Seq: 2, Timestamp: "2026-01-01T10:00:40Z", Tool: "bash", Action: classify.ActionExec, Summary: "work"},
			},
			endedAt:   "2026-01-01T10:00:40Z",
			watermark: 40,
		},
	}}
	w := NewWatcher(DefaultIdleConfig(), []Adapter{a})
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := drain(w.Events()); len(got) != 0 {
		t.Fatalf("cycle 1: got %d events, want 0", len(got))
	}
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := drain(w.Events())
	if len(got) != 1 {
		t.Fatalf("cycle 2: got %d events, want 1", len(got))
	}
	if len(got[0].Marks) != 1 || got[0].Marks[0].Note != "go" {
		t.Fatalf("mark from before the watermark did not survive: %+v", got[0].Marks)
	}
}

func TestWatcherMarksFeedIdleDetection(t *testing.T) {
	// A session whose latest activity is a user message — a mark, not an
	// event — is not idle. The mark's timestamp feeds lastActivity even while
	// the mark itself is parked.
	a := &scriptAdapter{path: "/script/s5", scans: []scriptScan{{
		marks: []classify.Mark{
			{Seq: 1, Timestamp: "2026-01-01T10:00:30Z", Type: "user-message", Note: "waiting on you"},
		},
		endedAt: "2026-01-01T10:00:05Z", // EndedAt is older than the mark
	}}}
	w := NewWatcher(DefaultIdleConfig(), []Adapter{a})
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := sessionKey("script", "/script/s5")
	last := w.LastActivity(key)
	if last.IsZero() {
		t.Fatal("a mark-only scan left lastActivity zero — idle detection cannot see the session's latest activity")
	}
	if want := "2026-01-01T10:00:30Z"; last.Format("2006-01-02T15:04:05Z") != want {
		t.Errorf("lastActivity = %v, want %v (the mark's timestamp, not EndedAt)", last.Format("2006-01-02T15:04:05Z"), want)
	}
}

func TestWatcherMarkNewerThanLastEventStillCountsAsActivity(t *testing.T) {
	// The same rule as above, but with an event in the batch. The event loop
	// assigns lastActivity outright, so a mark that arrives after the batch's
	// last event must not be clobbered by it — otherwise a session sitting on
	// the user's turn is declared idle from the older event's timestamp and
	// goes idle early.
	a := &scriptAdapter{path: "/script/s6", scans: []scriptScan{{
		events: []classify.Event{
			{Seq: 1, Timestamp: "2026-01-01T10:00:10Z", Tool: "bash", Action: classify.ActionExec, Summary: "ran tests"},
		},
		marks: []classify.Mark{
			{Seq: 2, Timestamp: "2026-01-01T10:00:30Z", Type: "user-message", Note: "now do the other thing"},
		},
		endedAt: "2026-01-01T10:00:30Z",
	}}}
	w := NewWatcher(DefaultIdleConfig(), []Adapter{a})
	if err := w.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := sessionKey("script", "/script/s6")
	last := w.LastActivity(key)
	if want := "2026-01-01T10:00:30Z"; last.Format("2006-01-02T15:04:05Z") != want {
		t.Errorf("lastActivity = %v, want %v (the mark, which is newer than the last event)",
			last.Format("2006-01-02T15:04:05Z"), want)
	}
}
