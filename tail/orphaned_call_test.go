package tail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
//
// @joestump-agent 09/22/2026 - Review: pinned that the Claude Code release
// stays inside one conversation — parallel inline subagents, and a
// subagent's own file — and that a line carrying a tool_result, no text, or
// text opening with a tag never counts as a message the user typed.

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

// sidechain marks a Claude Code line as a subagent's, with agentID naming the
// subagent when it is not empty. Older transcripts wrote subagents' lines
// inline in the parent's file with no agentId; newer ones give each subagent
// a file of its own, every line carrying its agentId.
func sidechain(agentID, line string) string {
	fields := `"isSidechain":true,`
	if agentID != "" {
		fields += `"agentId":"` + agentID + `",`
	}
	return strings.Replace(line, `"timestamp"`, fields+`"timestamp"`, 1)
}

// TestClaudeCodeParallelInlineSubagentsReleaseNothing: two subagents launched
// in parallel, their lines interleaved inline with no agentId to tell them
// apart. Subagent B's response is a different API response from subagent A's
// call, on the same side of the sidechain boundary — and says nothing about
// A's call, which is still running. It must hold, then pair with its result.
func TestClaudeCodeParallelInlineSubagentsReleaseNothing(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl", ccUserText("start", "2026-01-01T10:00:00Z"))
	steps := []func(t *testing.T){
		appendStep(path),
		appendStep(path,
			ccCall("m1", "task1", "echo task1", "2026-01-01T10:00:01Z"),
			ccCall("m1", "task2", "echo task2", "2026-01-01T10:00:01Z"),
			sidechain("", ccUserText("subagent A prompt", "2026-01-01T10:00:02Z")),
			sidechain("", ccCall("sa1", "a1", "echo a1", "2026-01-01T10:00:03Z")),
			sidechain("", ccUserText("subagent B prompt", "2026-01-01T10:00:03Z")),
			sidechain("", ccCall("sb1", "b1", "echo b1", "2026-01-01T10:00:04Z"))),
		appendStep(path,
			sidechain("", ccToolResult("b1", "2026-01-01T10:00:05Z")),
			sidechain("", ccText("sb2", "B done", "2026-01-01T10:00:06Z"))),
		appendStep(path,
			sidechain("", ccToolResult("a1", "2026-01-01T10:00:07Z")),
			sidechain("", ccText("sa2", "A done", "2026-01-01T10:00:08Z")),
			ccToolResult("task1", "2026-01-01T10:00:09Z"),
			ccToolResult("task2", "2026-01-01T10:00:10Z"),
			ccText("m2", "all done", "2026-01-01T10:00:11Z")),
	}
	wm := assertIncrementalMatchesFull(t, &ClaudeCodeAdapter{}, path, steps)
	if end := jsonlCompleteOffset(path); wm != end {
		t.Errorf("watermark = %d, want %d: the session ends with nothing in flight", wm, end)
	}
	events, _, _, err := ClaudeCodeAdapter{}.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assertOrphans(t, events)
}

// TestClaudeCodeSubagentFileReleasesOrphanedCall: a subagent's own file, every
// line carrying its agentId, is one conversation, so a response that stopped
// short of running its call is released there exactly as in the parent — by
// the same subagent's next response, and by nothing another subagent wrote.
func TestClaudeCodeSubagentFileReleasesOrphanedCall(t *testing.T) {
	path := writeTempJSONL(t, "agent-a.jsonl", sidechain("a", ccUserText("subagent prompt", "2026-01-01T10:00:00Z")))
	steps := []func(t *testing.T){
		appendStep(path),
		appendStep(path, sidechain("a", ccCall("s1", "c1", "sleep 60", "2026-01-01T10:00:01Z"))),
		// Lines another subagent wrote prove nothing about c1, still running.
		appendStep(path, sidechain("b", ccText("x1", "elsewhere", "2026-01-01T10:00:02Z")),
			sidechain("b", ccUserText("another prompt", "2026-01-01T10:00:02Z"))),
		appendStep(path, sidechain("a", ccToolResult("c1", "2026-01-01T10:01:01Z"))),
		appendStep(path, sidechain("a", ccCall("s2", "c2", "echo orphan", "2026-01-01T10:01:02Z"))),
		appendStep(path, sidechain("a", ccText("s3", "Let me try that differently.", "2026-01-01T10:01:03Z")),
			sidechain("a", ccCall("s3", "c3", "echo c3", "2026-01-01T10:01:03Z"))),
		appendStep(path, sidechain("a", ccToolResult("c3", "2026-01-01T10:01:04Z"))),
	}
	wm := assertIncrementalMatchesFull(t, &ClaudeCodeAdapter{}, path, steps)
	if end := jsonlCompleteOffset(path); wm != end {
		t.Errorf("watermark = %d, want %d: the session ends with nothing in flight", wm, end)
	}
	events, _, _, err := ClaudeCodeAdapter{}.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	assertOrphans(t, events, "echo orphan")
}

// TestClaudeCodeResultLineDoesNotSupersede: a user line carrying a
// tool_result is never a message the user typed, even with text ahead of the
// result. The release runs before a line's own results are paired, so
// counting it would drop the very result it carries.
func TestClaudeCodeResultLineDoesNotSupersede(t *testing.T) {
	line := `{"type":"user","timestamp":"2026-01-01T10:00:02Z","sessionId":"s1","cwd":"/tmp","message":{"role":"user","content":[{"type":"text","text":"note"},{"type":"tool_result","tool_use_id":"c1","content":"package main"}]}}` + "\n"
	path := writeTempJSONL(t, "s.jsonl", ccUserText("start", "2026-01-01T10:00:00Z")+
		ccCall("m1", "c1", "echo c1", "2026-01-01T10:00:01Z")+line)
	events, _, _, err := ClaudeCodeAdapter{}.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	assertOrphans(t, events)
}

// TestClaudeCodeTaggedUserTextDoesNotSupersede: text that opens with a tag is
// Claude Code's or a harness's, not the user's, even when something trails the
// closing tag — which injectedUserMessage alone does not recognize. Claude
// Code writes task notifications between the results of one parallel batch,
// so such a line must not release a sibling still running; nor must a user
// line with no text at all.
func TestClaudeCodeTaggedUserTextDoesNotSupersede(t *testing.T) {
	imageOnly := `{"type":"user","timestamp":"2026-01-01T10:00:04Z","sessionId":"s1","cwd":"/tmp","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":""}}]}}` + "\n"
	path := writeTempJSONL(t, "s.jsonl", ccUserText("start", "2026-01-01T10:00:00Z")+
		ccCall("m1", "c1", "sleep 60", "2026-01-01T10:00:01Z")+
		ccCall("m1", "c2", "echo c2", "2026-01-01T10:00:01Z")+
		ccToolResult("c2", "2026-01-01T10:00:02Z")+
		ccUserText(`<task-notification><status>completed</status></task-notification>\nRead the output file to retrieve the result.`, "2026-01-01T10:00:03Z")+
		imageOnly)
	a := ClaudeCodeAdapter{}
	events, _, _, wm, err := a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("poll 1: ParseSince: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("poll 1: got %d events, want 0 — c1 is still running", len(events))
	}
	appendLines(t, path, ccToolResult("c1", "2026-01-01T10:01:01Z"))
	events, _, _, _, err = a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("poll 2: ParseSince: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("poll 2: got %d events, want c2 and c1", len(events))
	}
	assertOrphans(t, events)
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

// TestCrushParseSinceReleasesCallItsStepAbandoned is the Crush release: a
// step that finished without answering one of its calls — here one cut short
// at max tokens, which never dispatches the calls it streamed. Once a later
// turn row confirms the step is over, the call is emitted at the end of its
// row with no result, and the resumed turn behind it is delivered.
func TestCrushParseSinceReleasesCallItsStepAbandoned(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)
	now := time.Now().Unix()
	dbPath := newCrushSession(t, "s1", now)
	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	path := dbPath + "/s1"
	batch := int64(0)
	poll := func(wm int64, startSeq int, writes ...crushStep) ([]classify.Event, []classify.Mark, int64) {
		t.Helper()
		batch++
		crushSteps(dbPath, "s1", now+batch*10, writes)[0](t)
		resetDBCache()
		events, marks, _, next, err := a.ParseSince(t.Context(), path, wm, startSeq)
		if err != nil {
			t.Fatalf("ParseSince: %v", err)
		}
		return events, marks, next
	}

	events, marks, wm := poll(0, 0,
		crushStep{id: "u1", role: "user", parts: crushUserRow("start")},
		crushStep{id: "a1", role: "assistant", parts: `[` + crushCallPart("c1", "echo orphan") + `]`})
	if len(events) != 0 || len(marks) != 1 || wm != 1 {
		t.Fatalf("poll 1: %d events, %d marks, watermark %d; want 0, the user message, and 1 — c1 is running", len(events), len(marks), wm)
	}

	// The step ends on max tokens: c1 is never run. Nothing after the row yet,
	// so it is held — Crush before fantasy wrote this same row while the call
	// was still about to run.
	events, marks, wm = poll(wm, 0, crushStep{id: "a1", update: true, parts: `[` + crushCallPart("c1", "echo orphan") + `,` + crushFinish("max_tokens") + `]`})
	if len(events) != 0 || len(marks) != 0 || wm != 1 {
		t.Fatalf("poll 2: %d events, %d marks, watermark %d; want 0, 0, 1", len(events), len(marks), wm)
	}

	events, marks, wm = poll(wm, 0,
		crushStep{id: "u2", role: "user", parts: crushUserRow("resumed")},
		crushStep{id: "a2", role: "assistant", parts: `[` + crushCallPart("c2", "echo c2") + `]`},
		crushStep{id: "t2", role: "tool", parts: crushResultRow("c2")},
		crushStep{id: "a2", update: true, parts: `[` + crushCallPart("c2", "echo c2") + `,` + crushFinish("tool_use") + `]`},
		crushStep{id: "a3", role: "assistant", parts: finishErrorParts})
	if len(events) != 2 {
		t.Fatalf("poll 3: got %d events, want the orphan and c2", len(events))
	}
	if events[0].Seq != 0 || events[0].ResultBytes != 0 || events[0].Summary != orphanSummary {
		t.Errorf("poll 3: event 0 = %+v, want the orphan at seq 0 with no result", events[0])
	}
	if events[1].Seq != 1 || events[1].ResultBytes == 0 {
		t.Errorf("poll 3: event 1 = %+v, want c2 at seq 1 with its result", events[1])
	}
	if len(marks) != 2 || marks[0].Note != "resumed" || marks[0].Seq != 1 || marks[1].Type != "error" || marks[1].Seq != 2 {
		t.Errorf("poll 3: marks = %+v, want the resumed message at seq 1 and the error at seq 2", marks)
	}
	if end := a.Watermark(t.Context(), path); wm != end {
		t.Errorf("poll 3: watermark = %d, want %d", wm, end)
	}

	events, marks, _, _, err := a.ParseSince(t.Context(), path, wm, 2)
	if err != nil {
		t.Fatalf("poll 4: ParseSince: %v", err)
	}
	if len(events) != 0 || len(marks) != 0 {
		t.Errorf("poll 4: %d events, %d marks; want none", len(events), len(marks))
	}
}

// TestCrushParseSinceHoldsCallThroughConcurrentTurn pins why nothing weaker
// than a finish part releases a Crush call. Crush runs turns concurrently in
// one session, so a call can still be running while a later user message
// starts another turn — and its result then lands after that turn's rows, as
// it did 39 times in live stores. The call must be held until it does.
//
// The row a Crush killed mid-call leaves behind looks exactly like c1 at the
// first poll, and is held the same way, for good: the store has no record
// that tells a dead call from one in a concurrent turn.
func TestCrushParseSinceHoldsCallThroughConcurrentTurn(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)
	now := time.Now().Unix()
	dbPath := newCrushSession(t, "s1", now)
	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}
	path := dbPath + "/s1"

	crushSteps(dbPath, "s1", now, []crushStep{
		{id: "u1", role: "user", parts: crushUserRow("start")},
		{id: "a1", role: "assistant", parts: `[` + crushCallPart("c1", "sleep 600") + `]`},
		{id: "u2", role: "user", parts: crushUserRow("and another thing")},
		{id: "a2", role: "assistant", parts: `[` + crushCallPart("c2", "echo c2") + `]`},
		{id: "t2", role: "tool", parts: crushResultRow("c2")},
		{id: "a2", update: true, parts: `[` + crushCallPart("c2", "echo c2") + `,` + crushFinish("tool_use") + `]`},
		{id: "a3", role: "assistant", parts: `[` + crushFinish("end_turn") + `]`},
	})[0](t)
	resetDBCache()
	events, _, _, wm, err := a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("poll 1: ParseSince: %v", err)
	}
	if len(events) != 0 || wm != 1 {
		t.Fatalf("poll 1: %d events, watermark %d; want 0 and 1 — c1 is still running in the first turn", len(events), wm)
	}

	crushSteps(dbPath, "s1", now+100, []crushStep{
		{id: "t1", role: "tool", parts: crushResultRow("c1")},
		{id: "a1", update: true, parts: `[` + crushCallPart("c1", "sleep 600") + `,` + crushFinish("tool_use") + `]`},
	})[0](t)
	resetDBCache()
	events, _, _, _, err = a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("poll 2: ParseSince: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("poll 2: got %d events, want c2 and c1", len(events))
	}
	for _, ev := range events {
		if ev.ResultBytes == 0 {
			t.Errorf("event %d (%s) has no result: released while its turn was still running", ev.Seq, ev.Summary)
		}
	}
}

// TestCrushParseFlushesOpenCallsInIssueOrder: a full Parse flushes the calls
// still open at the end of a session last, and in the order they were issued.
// It ranged over a map, so several open calls came out in a different order,
// with different seqs, from one Parse to the next.
func TestCrushParseFlushesOpenCallsInIssueOrder(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)
	now := time.Now().Unix()
	dbPath := newCrushSession(t, "s1", now)
	var calls, want []string
	for i := range 6 {
		cmd := fmt.Sprintf("echo c%d", i)
		calls = append(calls, crushCallPart(fmt.Sprintf("c%d", i), cmd))
		want = append(want, cmd+" -> 0 targets, 0 outside")
	}
	crushSteps(dbPath, "s1", now, []crushStep{
		{id: "u1", role: "user", parts: crushUserRow("start")},
		{id: "a1", role: "assistant", parts: `[` + strings.Join(calls, ",") + `]`},
	})[0](t)
	a := CrushAdapter{DBPath: dbPath, Cwd: "/test"}

	for run := range 20 {
		events, _, _, err := a.Parse(t.Context(), dbPath+"/s1")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		var got []string
		for _, ev := range events {
			got = append(got, ev.Summary)
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("run %d: open calls flushed as %q, want issue order %q", run, got, want)
		}
	}
}
