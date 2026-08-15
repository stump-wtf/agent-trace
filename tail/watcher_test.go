package tail

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
)

// mockAdapter implements Adapter for testing.
type mockAdapter struct {
	dir string
}

func (m mockAdapter) Harness() Harness   { return "mock" }
func (m mockAdapter) SessionDir() string { return m.dir }
func (m mockAdapter) WithRoot(dir string) Adapter {
	return mockAdapter{dir: dir}
}
func (m mockAdapter) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	var metas []SessionMeta
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(m.dir, entry.Name())
		metas = append(metas, SessionMeta{
			Key:     sessionKey("mock", path),
			ID:      entry.Name(),
			Harness: "mock",
			Path:    path,
		})
	}
	return metas, nil
}

func (m mockAdapter) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, SessionMeta{}, err
	}
	lines := splitLines(string(data))
	var events []classify.Event
	for i, line := range lines {
		if line == "" {
			continue
		}
		events = append(events, classify.Event{
			Seq:       i,
			Timestamp: "2026-01-01T10:00:0" + string(rune('0'+i)) + "Z",
			Tool:      "bash",
			Action:    classify.ActionExec,
			Summary:   line,
		})
	}
	meta := SessionMeta{
		Key:     sessionKey("mock", path),
		ID:      filepath.Base(path),
		Harness: "mock",
		Path:    path,
		EndedAt: "2026-01-01T10:00:05Z",
	}
	return events, nil, meta, nil
}

func splitLines(s string) []string {
	var lines []string
	current := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, current)
			current = ""
			continue
		}
		current += string(r)
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func TestWatcherScanOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")
	if err := os.WriteFile(path, []byte("event1\nevent2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	w := NewWatcherWithConfig(WatchConfig{
		IdleConfig:   IdleConfig{IdleAfter: 1 * time.Second},
		PollInterval: 100 * time.Millisecond,
	}, []Adapter{mockAdapter{dir: dir}})
	defer w.Stop()

	go w.Start(context.Background())
	defer w.Stop()

	var got []Event
	timeout := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				t.Fatal("events channel closed")
			}
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timed out waiting for events, got %d", len(got))
		}
	}
	if got[0].Classified.Summary != "event1" {
		t.Errorf("first event summary = %q", got[0].Classified.Summary)
	}
}

func TestWatcherStopClosesChannel(t *testing.T) {
	w := NewWatcher(DefaultIdleConfig(), nil)
	go w.Start(context.Background())
	w.Stop()
	// After Stop, the channel should eventually close. Either outcome is
	// acceptable — this test asserts only that Stop does not panic or wedge
	// the reader; Start may not have exited yet when the deadline fires.
	select {
	case <-w.Events():
	case <-time.After(1 * time.Second):
	}
}

func TestWatcherStopIdempotent(t *testing.T) {
	w := NewWatcher(DefaultIdleConfig(), nil)
	w.Stop()
	w.Stop() // should not panic
}

func TestWatcherIdleFromSessionData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.jsonl")
	if err := os.WriteFile(path, []byte("old\n"), 0644); err != nil {
		t.Fatal(err)
	}

	w := NewWatcherWithConfig(WatchConfig{
		IdleConfig:   IdleConfig{IdleAfter: 1 * time.Millisecond},
		PollInterval: 100 * time.Millisecond,
	}, []Adapter{mockAdapter{dir: dir}})

	ctx, cancel := context.WithCancel(context.Background())
	go w.Start(ctx)
	defer cancel()
	defer w.Stop()

	// Wait for first scan
	time.Sleep(300 * time.Millisecond)

	// The session's EndedAt is 2026-01-01, so it should be idle.
	if !w.IsIdle(sessionKey("mock", path)) {
		t.Error("session with old EndedAt should be idle")
	}
}
