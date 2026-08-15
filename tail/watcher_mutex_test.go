package tail

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
)

// buildEvents returns n distinct events one second apart, enough to overrun
// the watcher's outbound buffer.
func buildEvents(n int) []classify.Event {
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	out := make([]classify.Event, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, classify.Event{
			Seq:       i,
			Timestamp: base.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			Tool:      "bash",
			Action:    classify.ActionExec,
			Summary:   fmt.Sprintf("step %d", i),
		})
	}
	return out
}

// waitForFullBuffer blocks until the watcher's event channel is saturated, so
// the scan goroutine is parked on a send rather than merely slow. Without
// this the test could pass vacuously by querying before the scan starts.
func waitForFullBuffer(t *testing.T, w *Watcher) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(w.Events()) >= eventBufferSize {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("event buffer never filled (len=%d, want %d) — cannot test the blocked-sender path",
		len(w.Events()), eventBufferSize)
}

// A consumer that falls behind must not be able to wedge the watcher's own
// status API. emitEvents used to hold w.mu across its channel sends, so once
// the buffer filled, the blocked send held the lock — and LastActivity and
// IsIdle, the two methods a consumer calls to decide whether to keep waiting,
// blocked behind it. The consumer stalled exactly the API it needed to
// unstall itself.
func TestLastActivityDoesNotBlockOnSlowConsumer(t *testing.T) {
	a := &scriptAdapter{path: "/mutex/s1", scans: []scriptScan{{
		events:  buildEvents(eventBufferSize + 64),
		endedAt: "2026-01-01T10:30:00Z",
	}}}
	w := NewWatcher(DefaultIdleConfig(), []Adapter{a})

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		_ = w.ScanOnce(context.Background())
	}()
	t.Cleanup(func() {
		w.Stop()
		<-scanDone
	})

	waitForFullBuffer(t, w)

	key := sessionKey("script", "/mutex/s1")
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		_ = w.LastActivity(key)
		_ = w.IsIdle(key)
	}()

	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("LastActivity/IsIdle blocked while a send was parked on a full buffer — emitEvents is holding w.mu across its sends")
	}
}

// The state a consumer reads must also be correct, not merely reachable:
// because the batch is committed before the first send, lastActivity already
// reflects the whole batch while it is still draining.
func TestLastActivityReflectsFullBatchWhileDraining(t *testing.T) {
	events := buildEvents(eventBufferSize + 64)
	last := events[len(events)-1].Timestamp

	a := &scriptAdapter{path: "/mutex/s2", scans: []scriptScan{{
		events:  events,
		endedAt: "2026-01-01T09:00:00Z", // deliberately older than the events
	}}}
	w := NewWatcher(DefaultIdleConfig(), []Adapter{a})

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		_ = w.ScanOnce(context.Background())
	}()
	t.Cleanup(func() {
		w.Stop()
		<-scanDone
	})

	waitForFullBuffer(t, w)

	want, err := time.Parse(time.RFC3339, last)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.LastActivity(sessionKey("script", "/mutex/s2")); !got.Equal(want) {
		t.Errorf("LastActivity = %v, want %v (the batch's last event, committed before draining)", got, want)
	}
}

// Draining the channel must still deliver every event in order, so the
// build-then-send split cannot be mistaken for a fix that drops the tail.
func TestSlowConsumerEventuallyReceivesEveryEvent(t *testing.T) {
	const n = eventBufferSize + 64
	a := &scriptAdapter{path: "/mutex/s3", scans: []scriptScan{{
		events:  buildEvents(n),
		endedAt: "2026-01-01T10:30:00Z",
	}}}
	w := NewWatcher(DefaultIdleConfig(), []Adapter{a})

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		_ = w.ScanOnce(context.Background())
	}()

	for i := 0; i < n; i++ {
		select {
		case ev := <-w.Events():
			if ev.Classified.Seq != i {
				t.Fatalf("event %d has seq %d — ordering broke", i, ev.Classified.Seq)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only received %d of %d events", i, n)
		}
	}
	select {
	case <-scanDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ScanOnce did not return after the buffer drained")
	}
	w.Stop()
}
