package tail

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// Orphaned Calls
//
// A tool call that never receives a result used to hold ParseSince's
// watermark below it forever, so nothing later in the session was delivered:
// not the next user message, not the next tool call, not the error marks that
// matter most during a provider outage (#103). The tests here pin the release
// rule per adapter — a call is released only once a later record proves no
// result can follow — and the in-flight contract it must not break: a call
// that is merely slow is still held.
//
// @joestump-agent 09/22/2026 - Added for #103.

// TestClaudeCodeParseSinceReleasesOrphanedCall is the shape #103 reports: an
// agent killed mid-call and then resumed. The resumed turn must be delivered,
// the orphan with it — once, in position, with no result — and the watermark
// must move.
func TestClaudeCodeParseSinceReleasesOrphanedCall(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", ccUserText("start", "2026-01-01T10:00:00Z")+
		ccCall("m1", "c1", "echo c1", "2026-01-01T10:00:01Z")+
		ccToolResult("c1", "2026-01-01T10:00:02Z"))
	a := ClaudeCodeAdapter{}
	preOrphan := a.Watermark(t.Context(), path)

	// The agent is killed while c2 runs: its result is never written.
	appendLines(t, path, ccCall("m2", "c2", "echo orphan", "2026-01-01T10:00:03Z"))
	events, marks, _, wm, err := a.ParseSince(t.Context(), path, preOrphan, 1)
	if err != nil {
		t.Fatalf("poll 1: ParseSince: %v", err)
	}
	if len(events) != 0 || len(marks) != 0 || wm != preOrphan {
		t.Fatalf("poll 1: %d events, watermark %d; want 0 and %d — nothing yet says c2 is dead", len(events), wm, preOrphan)
	}

	// The session resumes.
	appendLines(t, path, ccUserText("resumed", "2026-01-01T11:00:00Z")+
		ccCall("m3", "c3", "echo c3", "2026-01-01T11:00:01Z")+
		ccToolResult("c3", "2026-01-01T11:00:02Z"))
	events, marks, _, wm, err = a.ParseSince(t.Context(), path, wm, 1)
	if err != nil {
		t.Fatalf("poll 2: ParseSince: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("poll 2: got %d events, want the orphan and c3", len(events))
	}
	if events[0].Seq != 1 || events[0].ResultBytes != 0 || events[0].Summary != orphanSummary {
		t.Errorf("poll 2: event 0 = %+v, want the orphan at seq 1 with no result", events[0])
	}
	if events[1].Seq != 2 || events[1].ResultBytes == 0 {
		t.Errorf("poll 2: event 1 = %+v, want c3 at seq 2 with its result", events[1])
	}
	if len(marks) != 1 || marks[0].Note != "resumed" || marks[0].Seq != 2 {
		t.Errorf("poll 2: marks = %+v, want the resumed turn's message at seq 2, after the orphan it superseded", marks)
	}
	if end := jsonlCompleteOffset(path); wm != end {
		t.Errorf("poll 2: watermark = %d, want %d", wm, end)
	}

	// Nothing is repeated.
	events, marks, _, _, err = a.ParseSince(t.Context(), path, wm, 3)
	if err != nil {
		t.Fatalf("poll 3: ParseSince: %v", err)
	}
	if len(events) != 0 || len(marks) != 0 {
		t.Errorf("poll 3: %d events, %d marks; want none", len(events), len(marks))
	}

	// And a full Parse agrees on where the orphan sits.
	full, _, _, err := a.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(full) != 3 || full[1].Summary != orphanSummary {
		t.Errorf("Parse = %+v, want the orphan at seq 1, between c1 and c3", full)
	}
}

// orphanSummary is the summary of the call these tests orphan.
const orphanSummary = "echo orphan -> 0 targets, 0 outside"

// TestClaudeCodeParseSinceHoldsSlowCall is the in-flight contract the release
// rule must not break. Records that land while a call runs — its siblings'
// lines and results, a loaded skill's body, an injected notification, a
// compaction — prove nothing about it, so the call is held until its own
// result arrives, and then emitted with that result.
func TestClaudeCodeParseSinceHoldsSlowCall(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", ccUserText("start", "2026-01-01T10:00:00Z"))
	a := ClaudeCodeAdapter{}
	start := a.Watermark(t.Context(), path)

	appendLines(t, path, ccCall("m1", "c1", "sleep 600", "2026-01-01T10:00:01Z")+
		ccCall("m1", "c2", "echo c2", "2026-01-01T10:00:01Z")+
		ccToolResult("c2", "2026-01-01T10:00:02Z")+
		ccMetaText("Base directory for this skill", "2026-01-01T10:00:03Z")+
		ccUserText("<task-notification>b1 done</task-notification>", "2026-01-01T10:00:04Z")+
		`{"type":"system","subtype":"compact_boundary","timestamp":"2026-01-01T10:00:05Z","sessionId":"s1"}`+"\n")
	events, _, _, wm, err := a.ParseSince(t.Context(), path, start, 0)
	if err != nil {
		t.Fatalf("poll 1: ParseSince: %v", err)
	}
	if len(events) != 0 || wm != start {
		t.Fatalf("poll 1: %d events, watermark %d; want 0 and %d — c1 is still running", len(events), wm, start)
	}

	appendLines(t, path, ccToolResult("c1", "2026-01-01T10:10:01Z"))
	events, _, _, _, err = a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("poll 2: ParseSince: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("poll 2: got %d events, want c2 and c1", len(events))
	}
	for _, ev := range events {
		if ev.ResultBytes == 0 {
			t.Errorf("event %d (%s) has no result: released while it was still running", ev.Seq, ev.Summary)
		}
	}
}

// TestClaudeCodeSidechainLineDoesNotSupersede keeps the rule inside one
// conversation. Older transcripts interleave a subagent's lines, marked
// isSidechain, with the parent's; the subagent's responses carry their own
// message ids and must not release the parent's Task call.
func TestClaudeCodeSidechainLineDoesNotSupersede(t *testing.T) {
	sidechain := `{"type":"assistant","isSidechain":true,"timestamp":"2026-01-01T10:00:02Z","sessionId":"s1","cwd":"/tmp","message":{"id":"sub1","role":"assistant","model":"m","content":[{"type":"text","text":"working"}]}}` + "\n" +
		`{"type":"user","isSidechain":true,"timestamp":"2026-01-01T10:00:03Z","sessionId":"s1","cwd":"/tmp","message":{"role":"user","content":[{"type":"text","text":"subagent prompt"}]}}` + "\n"
	path := writeTempJSONL(t, "s.jsonl", ccUserText("start", "2026-01-01T10:00:00Z")+
		ccCall("m1", "c1", "echo task", "2026-01-01T10:00:01Z")+sidechain)
	a := ClaudeCodeAdapter{}

	events, _, _, wm, err := a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %d events, want 0: a sidechain line released the parent's call", len(events))
	}
	appendLines(t, path, ccToolResult("c1", "2026-01-01T10:05:00Z"))
	events, _, _, _, err = a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(events) != 1 || events[0].ResultBytes == 0 {
		t.Errorf("events = %+v, want the Task call once, with its result", events)
	}
}

// TestWatcherResumesAfterOrphanedCall drives the watcher, which inherits the
// release through ParseSince: after an orphan, a resumed session's events and
// its user message must reach the channel, each exactly once.
func TestWatcherResumesAfterOrphanedCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(ccUserText("start", "2026-01-01T10:00:00Z")+
		ccCall("m1", "c1", "echo c1", "2026-01-01T10:00:01Z")+
		ccToolResult("c1", "2026-01-01T10:00:02Z")), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fixed fixture timestamps; discovery scope is not what this tests.
	w := NewWatcherWithConfig(WatchConfig{MaxAge: -1}, []Adapter{&ClaudeCodeAdapter{Dir: dir}})
	var got []Event
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range w.Events() {
			got = append(got, ev)
		}
	}()

	w.scanOnce(context.Background()) // baseline: a full Parse
	appendLines(t, path, ccCall("m2", "c2", "echo orphan", "2026-01-01T10:00:03Z"))
	w.scanOnce(context.Background()) // the orphan, not yet known to be one
	appendLines(t, path, ccUserText("resumed", "2026-01-01T11:00:00Z")+
		ccCall("m3", "c3", "echo c3", "2026-01-01T11:00:01Z")+
		ccToolResult("c3", "2026-01-01T11:00:02Z"))
	w.scanOnce(context.Background())
	appendLines(t, path, ccCall("m4", "c4", "echo c4", "2026-01-01T11:00:03Z")+
		ccToolResult("c4", "2026-01-01T11:00:04Z"))
	w.scanOnce(context.Background())

	close(w.events)
	<-drained

	var summaries []string
	var resumed []classify.Mark
	for _, ev := range got {
		summaries = append(summaries, ev.Classified.Summary)
		for _, m := range ev.Marks {
			if m.Note == "resumed" {
				resumed = append(resumed, m)
			}
		}
		if ev.Classified.Seq != len(summaries)-1 {
			t.Errorf("event %q has seq %d, want %d", ev.Classified.Summary, ev.Classified.Seq, len(summaries)-1)
		}
	}
	want := []string{"echo c1", "echo orphan", "echo c3", "echo c4"}
	if len(summaries) != len(want) {
		t.Fatalf("watcher delivered %d events %q, want %d: %q", len(summaries), summaries, len(want), want)
	}
	for i, cmd := range want {
		if summaries[i] != cmd+" -> 0 targets, 0 outside" {
			t.Errorf("event %d = %q, want %q", i, summaries[i], cmd)
		}
	}
	if len(resumed) != 1 {
		t.Errorf("the resumed turn's user message was delivered %d times, want once", len(resumed))
	}
}

// TestCodexParseSinceReleasesOrphanedCall is the Codex shape of #103: a
// rollout whose process died mid-call, then resumed with a new turn. The
// orphan keeps its call-order seq — where Parse has always put it — with no
// output, and everything after it is delivered.
func TestCodexParseSinceReleasesOrphanedCall(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", codexSessionMetaLine("2026-01-01T10:00:00Z")+
		codexTurnStart("t1", "2026-01-01T10:00:01Z")+
		codexUserLine("run it", "2026-01-01T10:00:01Z")+
		codexExec("c1", "echo c1", "2026-01-01T10:00:02Z")+
		codexOutputLine("c1", `"ok"`, "2026-01-01T10:00:03Z"))
	a := CodexAdapter{}
	preOrphan := a.Watermark(t.Context(), path)

	appendLines(t, path, codexExec("c2", "echo orphan", "2026-01-01T10:00:04Z"))
	events, marks, _, wm, err := a.ParseSince(t.Context(), path, preOrphan, 1)
	if err != nil {
		t.Fatalf("poll 1: ParseSince: %v", err)
	}
	if len(events) != 0 || len(marks) != 0 || wm != preOrphan {
		t.Fatalf("poll 1: %d events, %d marks, watermark %d; want 0, 0, %d — nothing yet says c2 is dead", len(events), len(marks), wm, preOrphan)
	}

	appendLines(t, path, codexTurnStart("t2", "2026-01-01T11:00:00Z")+
		codexUserLine("resumed", "2026-01-01T11:00:01Z")+
		codexExec("c3", "echo c3", "2026-01-01T11:00:02Z")+
		codexOutputLine("c3", `"ok"`, "2026-01-01T11:00:03Z"))
	events, marks, _, wm, err = a.ParseSince(t.Context(), path, wm, 1)
	if err != nil {
		t.Fatalf("poll 2: ParseSince: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("poll 2: got %d events, want the orphan and c3", len(events))
	}
	if events[0].Seq != 1 || events[0].ResultBytes != 0 || events[0].Summary != orphanSummary {
		t.Errorf("poll 2: event 0 = %+v, want the orphan at seq 1 with no output", events[0])
	}
	if events[1].Seq != 2 || events[1].ResultBytes == 0 {
		t.Errorf("poll 2: event 1 = %+v, want c3 at seq 2 with its output", events[1])
	}
	if len(marks) != 1 || marks[0].Note != "resumed" || marks[0].Seq != 2 {
		t.Errorf("poll 2: marks = %+v, want the resumed turn's message at seq 2", marks)
	}
	if end := jsonlCompleteOffset(path); wm != end {
		t.Errorf("poll 2: watermark = %d, want %d", wm, end)
	}

	events, marks, _, _, err = a.ParseSince(t.Context(), path, wm, 3)
	if err != nil {
		t.Fatalf("poll 3: ParseSince: %v", err)
	}
	if len(events) != 0 || len(marks) != 0 {
		t.Errorf("poll 3: %d events, %d marks; want none", len(events), len(marks))
	}
}

// TestCodexParseSinceHoldsSlowCall is the in-flight contract for Codex.
// Records written while a call runs — its sibling's output, reasoning, the
// model's own message, token counts, injected context — do not release it.
func TestCodexParseSinceHoldsSlowCall(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", codexSessionMetaLine("2026-01-01T10:00:00Z")+
		codexTurnStart("t1", "2026-01-01T10:00:01Z")+
		codexUserLine("build it", "2026-01-01T10:00:01Z"))
	a := CodexAdapter{}
	start := a.Watermark(t.Context(), path)

	appendLines(t, path, codexExec("c1", "sleep 600", "2026-01-01T10:00:02Z")+
		codexExec("c2", "echo c2", "2026-01-01T10:00:02Z")+
		codexOutputLine("c2", `"ok"`, "2026-01-01T10:00:03Z")+
		`{"type":"response_item","timestamp":"2026-01-01T10:00:04Z","payload":{"type":"reasoning","summary":[]}}`+"\n"+
		`{"type":"response_item","timestamp":"2026-01-01T10:00:05Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"waiting on the build"}]}}`+"\n"+
		`{"type":"event_msg","timestamp":"2026-01-01T10:00:06Z","payload":{"type":"token_count","info":null}}`+"\n"+
		codexUserLine("<environment_context>cwd</environment_context>", "2026-01-01T10:00:07Z"))
	events, _, _, wm, err := a.ParseSince(t.Context(), path, start, 0)
	if err != nil {
		t.Fatalf("poll 1: ParseSince: %v", err)
	}
	if len(events) != 0 || wm != start {
		t.Fatalf("poll 1: %d events, watermark %d; want 0 and %d — c1 is still running", len(events), wm, start)
	}

	appendLines(t, path, codexOutputLine("c1", `"built"`, "2026-01-01T10:10:00Z"))
	events, _, _, _, err = a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("poll 2: ParseSince: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("poll 2: got %d events, want c1 and c2", len(events))
	}
	for _, ev := range events {
		if ev.ResultBytes == 0 {
			t.Errorf("event %d (%s) has no output: released while it was still running", ev.Seq, ev.Summary)
		}
	}
}
