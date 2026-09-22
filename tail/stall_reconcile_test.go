package tail

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Stalled-watermark reconciliation (#110).
//
// A transcript whose writer died mid-call never changes again, so its
// orphaned call holds ParseSince's watermark below it forever: every poll
// re-reads the same bytes, withholds the same tail, and returns the same
// offset — 400 consecutive empty polls on the transcript #110 names. Nothing
// in the file ever proves the call dead, and neither rejected direction
// (flushing at end-of-poll; releasing on message-less records) can, so the
// proof arrives as silence: after StallScans consecutive unchanged scans the
// watcher reconciles the session with a full Parse and delivers the withheld
// tail, deduplicated by seq.
//
// The tests here pin the convergence the issue asks for — a completed
// transcript settles on the adapter's own end-of-file watermark, with the
// orphan delivered exactly once, where Parse puts it — across the three JSONL
// adapters that share the safe-offset shape, and the guard the in-flight
// contract demands: below the threshold, a held call is still held.
//
// @joestump-agent 09/22/2026 - Added for #110.

// watcherCollector drains a watcher's event channel, counting the summaries
// it has seen so a test can assert on delivery after each synchronous scan.
type watcherCollector struct {
	mu        sync.Mutex
	summaries []string
	bySummary map[string]int
}

func newWatcherCollector(w *Watcher) *watcherCollector {
	c := &watcherCollector{bySummary: map[string]int{}}
	go func() {
		for ev := range w.Events() {
			c.mu.Lock()
			c.summaries = append(c.summaries, ev.Classified.Summary)
			c.bySummary[ev.Classified.Summary]++
			c.mu.Unlock()
		}
	}()
	return c
}

// settled waits out the emit goroutine before an assertion of "nothing was
// delivered": waiting longer only makes a false zero less likely.
func (c *watcherCollector) settled() int {
	time.Sleep(100 * time.Millisecond)
	return c.total()
}

// waitFor polls until at least n events have been delivered or two seconds
// pass, and returns the total seen — a positive assertion must not race the
// emit goroutine's channel hop.
func (c *watcherCollector) waitFor(n int) int {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if total := c.total(); total >= n || time.Now().After(deadline) {
			return total
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (c *watcherCollector) count(summary string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bySummary[summary]
}

func (c *watcherCollector) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.summaries)
}

// ccTailRecords is what the daemon writes around a restart and never follows:
// records that carry no message, so they can neither answer a pending call
// nor supersede it — the stalled transcript's trailing silence, in miniature.
var ccTailRecords = `{"type":"bridge-session","timestamp":"2026-01-01T11:00:00Z","sessionId":"s1"}` + "\n" +
	`{"type":"last-prompt","timestamp":"2026-01-01T11:00:01Z","sessionId":"s1"}` + "\n" +
	`{"type":"queue-operation","timestamp":"2026-01-01T11:00:02Z","sessionId":"s1"}` + "\n"

// TestWatcherReconcilesStalledOrphanClaudeCode is #110's shape end to end: a
// resolved session the watcher has baselined, an orphaned call followed by
// message-less daemon records, then silence. The incremental polls withhold
// everything and stall; the reconciliation delivers the orphan where Parse
// puts it and the watermark reaches the end of the file.
func TestWatcherReconcilesStalledOrphanClaudeCode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(
		ccUserText("start", "2026-01-01T10:00:00Z")+
			ccCall("m1", "c1", "echo c1", "2026-01-01T10:00:01Z")+
			ccToolResult("c1", "2026-01-01T10:00:02Z")), 0o644); err != nil {
		t.Fatal(err)
	}
	w := NewWatcherWithConfig(WatchConfig{MaxAge: -1, StallScans: 2}, []Adapter{&ClaudeCodeAdapter{Dir: dir}})
	c := newWatcherCollector(w)

	w.scanOnce(context.Background()) // baseline: a full Parse
	if got := c.waitFor(1); got != 1 {
		t.Fatalf("baseline: %d events delivered, want 1", got)
	}

	// The daemon dies mid-call and writes its restart records; the transcript
	// is now complete and will never change again.
	appendLines(t, path, ccCall("m2", "c2", "echo orphan", "2026-01-01T10:00:03Z")+ccTailRecords)
	w.scanOnce(context.Background()) // progress scan: the orphan is withheld
	w.scanOnce(context.Background()) // silent scan 1 of 2
	if got := c.settled(); got != 1 {
		t.Fatalf("below the threshold %d events delivered, want 1", got)
	}

	w.scanOnce(context.Background()) // silent scan 2 of 2: reconcile
	if got := c.waitFor(2); got != 2 {
		t.Fatalf("after the threshold %d events delivered, want the withheld orphan too", got)
	}
	if got := c.count("echo orphan -> 0 targets, 0 outside"); got != 1 {
		t.Errorf("the orphan was delivered %d times, want once", got)
	}
	if got := c.count("echo c1 -> 0 targets, 0 outside"); got != 1 {
		t.Errorf("echo c1 delivered %d times after reconciliation, want still 1", got)
	}

	w.scanOnce(context.Background()) // settled: nothing repeats
	if got := c.settled(); got != 2 {
		t.Errorf("total events delivered = %d, want 2", got)
	}
	key := sessionKey(string(ClaudeCodeAdapter{}.Harness()), path)
	if end := jsonlCompleteOffset(path); w.parseState[key] != end {
		t.Errorf("watermark = %d, want %d: a completed transcript must settle on the file end", w.parseState[key], end)
	}
	close(w.events)
}

// TestWatcherReconcilesStalledOrphanCodex is the Codex shape: an exec whose
// output never lands, trailed by reasoning, a token count and the model's own
// message — records that prove nothing and release nothing, so the
// incremental watermark stalls below the call for good.
func TestWatcherReconcilesStalledOrphanCodex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(
		codexSessionMetaLine("2026-01-01T10:00:00Z")+
			codexTurnStart("t1", "2026-01-01T10:00:01Z")+
			codexUserLine("run it", "2026-01-01T10:00:01Z")+
			codexExec("c1", "echo c1", "2026-01-01T10:00:02Z")+
			codexOutputLine("c1", `"ok"`, "2026-01-01T10:00:03Z")), 0o644); err != nil {
		t.Fatal(err)
	}
	w := NewWatcherWithConfig(WatchConfig{MaxAge: -1, StallScans: 2}, []Adapter{&CodexAdapter{Dir: dir}})
	c := newWatcherCollector(w)

	w.scanOnce(context.Background())
	if got := c.waitFor(1); got != 1 {
		t.Fatalf("baseline: %d events delivered, want 1", got)
	}

	appendLines(t, path,
		codexExec("c2", "echo orphan", "2026-01-01T10:00:04Z")+
			`{"type":"response_item","timestamp":"2026-01-01T10:00:05Z","payload":{"type":"reasoning","summary":[]}}`+"\n"+
			`{"type":"event_msg","timestamp":"2026-01-01T10:00:06Z","payload":{"type":"token_count","info":null}}`+"\n"+
			`{"type":"response_item","timestamp":"2026-01-01T10:00:07Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"waiting on the build"}]}}`+"\n")
	w.scanOnce(context.Background())
	w.scanOnce(context.Background())
	if got := c.settled(); got != 1 {
		t.Fatalf("below the threshold %d events delivered, want 1", got)
	}

	w.scanOnce(context.Background())
	if got := c.waitFor(2); got != 2 {
		t.Fatalf("after the threshold %d events delivered, want the withheld orphan too", got)
	}
	if got := c.count("echo orphan -> 0 targets, 0 outside"); got != 1 {
		t.Errorf("the orphan was delivered %d times, want once", got)
	}
	w.scanOnce(context.Background())
	if got := c.count("echo c1 -> 0 targets, 0 outside"); got != 1 {
		t.Errorf("echo c1 delivered %d times after reconciliation, want still 1", got)
	}
	key := sessionKey(string(CodexAdapter{}.Harness()), path)
	if end := jsonlCompleteOffset(path); w.parseState[key] != end {
		t.Errorf("watermark = %d, want %d", w.parseState[key], end)
	}
	close(w.events)
}

// TestWatcherReconcilesStalledOrphanPi is the Pi shape: a toolCall with no
// toolResult, trailed by model_change records, stalled and then reconciled.
func TestWatcherReconcilesStalledOrphanPi(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(
		`{"type":"session","id":"s1","timestamp":"2026-01-01T10:00:00Z","cwd":"/tmp"}`+"\n"+
			`{"type":"message","id":"m1","parentId":"s1","timestamp":"2026-01-01T10:00:01Z","message":{"role":"user","content":"run it"}}`+"\n"+
			`{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-01-01T10:00:02Z","message":{"role":"assistant","model":"m","content":[{"type":"toolCall","id":"tc1","name":"read","arguments":{"path":"/tmp/foo.go"}}]}}`+"\n"+
			`{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-01-01T10:00:03Z","message":{"role":"toolResult","toolCallId":"tc1","content":"package main","isError":false}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := NewWatcherWithConfig(WatchConfig{MaxAge: -1, StallScans: 2}, []Adapter{&PiAdapter{Dir: dir}})
	c := newWatcherCollector(w)

	w.scanOnce(context.Background())
	if got := c.waitFor(1); got != 1 {
		t.Fatalf("baseline: %d events delivered, want 1", got)
	}

	appendLines(t, path,
		`{"type":"message","id":"m4","parentId":"m3","timestamp":"2026-01-01T10:00:04Z","message":{"role":"assistant","model":"m","content":[{"type":"toolCall","id":"tc2","name":"bash","arguments":{"command":"sleep 60"}}]}}`+"\n"+
			`{"type":"model_change","id":"mc1","parentId":"m4","timestamp":"2026-01-01T11:00:00Z","modelId":"other"}`+"\n"+
			`{"type":"model_change","id":"mc2","parentId":"mc1","timestamp":"2026-01-01T11:00:01Z","modelId":"other"}`+"\n")
	w.scanOnce(context.Background())
	w.scanOnce(context.Background())
	if got := c.settled(); got != 1 {
		t.Fatalf("below the threshold %d events delivered, want still 1", got)
	}

	w.scanOnce(context.Background())
	if got := c.waitFor(2); got != 2 {
		t.Fatalf("after the threshold %d events delivered, want the withheld orphan too", got)
	}
	key := sessionKey(string(PiAdapter{}.Harness()), path)
	if end := jsonlCompleteOffset(path); w.parseState[key] != end {
		t.Errorf("watermark = %d, want %d", w.parseState[key], end)
	}
	close(w.events)
}

// TestWatcherHoldsSlowCallThroughSilence pins the guard the threshold must
// keep: a call that is merely slow holds through scans of silence until the
// threshold is reached — one silent scan below a threshold of two delivers
// nothing, exactly as the in-flight contract requires.
func TestWatcherHoldsSlowCallThroughSilence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(
		ccUserText("start", "2026-01-01T10:00:00Z")+
			ccCall("m1", "c1", "echo c1", "2026-01-01T10:00:01Z")+
			ccToolResult("c1", "2026-01-01T10:00:02Z")), 0o644); err != nil {
		t.Fatal(err)
	}
	w := NewWatcherWithConfig(WatchConfig{MaxAge: -1, StallScans: 2}, []Adapter{&ClaudeCodeAdapter{Dir: dir}})
	c := newWatcherCollector(w)

	w.scanOnce(context.Background())
	if got := c.waitFor(1); got != 1 {
		t.Fatalf("baseline: %d events delivered, want 1", got)
	}

	appendLines(t, path, ccCall("m2", "c2", "sleep 600", "2026-01-01T10:00:03Z"))
	w.scanOnce(context.Background()) // progress scan: the slow call is withheld
	w.scanOnce(context.Background()) // silent scan 1 of 2: still held
	if got := c.settled(); got != 1 {
		t.Fatalf("%d events delivered below the threshold, want still 1", got)
	}
	close(w.events)
}
