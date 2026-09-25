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
//
// @joestump-agent 09/25/2026 - Added the Pi release (#103): any later user
// or assistant message on the chain — and the OpenCode release: a later
// assistant message in the session, where a queued prompt must not count.

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

// --- Pi ---

// piChain writes Pi session entries that each chain onto the one before, the
// way Pi appends a linear conversation: every entry's parentId is the
// previous entry's id.
type piChain struct {
	n    int
	prev string
}

// entry is one chained entry of the given type; fields is the rest of the
// record's JSON, without braces.
func (c *piChain) entry(typ, fields, ts string) string {
	c.n++
	id := fmt.Sprintf("e%d", c.n)
	line := `{"type":"` + typ + `","id":"` + id + `","parentId":"` + c.prev + `","timestamp":"` + ts + `",` + fields + `}` + "\n"
	c.prev = id
	return line
}

// user is a message the user typed.
func (c *piChain) user(text, ts string) string {
	return c.entry("message", `"message":{"role":"user","content":[{"type":"text","text":"`+text+`"}]}`, ts)
}

// calls is an assistant message issuing one bash call per id/command pair,
// ending with stopReason — "toolUse" for a response whose calls Pi runs,
// "aborted" for one the user interrupted mid-stream, whose calls it never
// runs.
func (c *piChain) calls(stopReason, ts string, idCmd ...string) string {
	var blocks []string
	for i := 0; i+1 < len(idCmd); i += 2 {
		blocks = append(blocks, `{"type":"toolCall","id":"`+idCmd[i]+`","name":"bash","arguments":{"command":"`+idCmd[i+1]+`"}}`)
	}
	return c.entry("message", `"message":{"role":"assistant","model":"m","stopReason":"`+stopReason+`","content":[`+strings.Join(blocks, ",")+`]}`, ts)
}

// text is an assistant message of plain text.
func (c *piChain) text(text, stopReason, ts string) string {
	return c.entry("message", `"message":{"role":"assistant","model":"m","stopReason":"`+stopReason+`","content":[{"type":"text","text":"`+text+`"}]}`, ts)
}

// result is the toolResult message answering call id.
func (c *piChain) result(id, ts string) string {
	return c.entry("message", `"message":{"role":"toolResult","toolCallId":"`+id+`","toolName":"bash","content":[{"type":"text","text":"ok"}],"isError":false}`, ts)
}

// TestPiParseSinceReleasesOrphanedCall is the Pi shape of #103: a response
// the user interrupted mid-stream keeps the calls it streamed, and Pi never
// runs them, so they get no result; the next prompt follows. Pi writes every
// result of a batch before the turn ends, and a user or assistant message
// only at a turn boundary, so the prompt proves the call dead: it must be
// emitted there, once, in position, and the watermark must move.
func TestPiParseSinceReleasesOrphanedCall(t *testing.T) {
	var c piChain
	path := writeTempJSONL(t, "s.jsonl", piHeaderLine("s1", "2026-01-01T10:00:00Z")+
		c.user("start", "2026-01-01T10:00:01Z")+
		c.calls("toolUse", "2026-01-01T10:00:02Z", "c1", "echo c1")+
		c.result("c1", "2026-01-01T10:00:03Z"))
	a := PiAdapter{}
	preOrphan := a.Watermark(t.Context(), path)

	appendLines(t, path, c.calls("aborted", "2026-01-01T10:00:04Z", "c2", "echo orphan"))
	events, marks, _, wm, err := a.ParseSince(t.Context(), path, preOrphan, 1)
	if err != nil {
		t.Fatalf("poll 1: ParseSince: %v", err)
	}
	if len(events) != 0 || len(marks) != 0 || wm != preOrphan {
		t.Fatalf("poll 1: %d events, %d marks, watermark %d; want 0, 0, %d — nothing yet says c2 is dead", len(events), len(marks), wm, preOrphan)
	}

	appendLines(t, path, c.user("resumed", "2026-01-01T11:00:00Z")+
		c.calls("toolUse", "2026-01-01T11:00:01Z", "c3", "echo c3")+
		c.result("c3", "2026-01-01T11:00:02Z"))
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

	events, marks, _, _, err = a.ParseSince(t.Context(), path, wm, 3)
	if err != nil {
		t.Fatalf("poll 3: ParseSince: %v", err)
	}
	if len(events) != 0 || len(marks) != 0 {
		t.Errorf("poll 3: %d events, %d marks; want none", len(events), len(marks))
	}

	full, _, _, err := a.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(full) != 3 || full[1].Summary != orphanSummary {
		t.Errorf("Parse = %+v, want the orphan at seq 1, between c1 and c3", full)
	}
}

// TestPiParseSinceHoldsSlowCall is the in-flight contract for Pi. Entries
// written while a call runs — a parallel sibling's result, and the entries
// that are not messages at all (an extension's custom message, a model or
// session-name change, a label) — say nothing about it, so it is held until
// its own result lands. Pi defers an extension's message and a user's own
// bash run to the end of the turn precisely so neither lands between a call
// and its result; they would not release it even if they did.
func TestPiParseSinceHoldsSlowCall(t *testing.T) {
	var c piChain
	path := writeTempJSONL(t, "s.jsonl", piHeaderLine("s1", "2026-01-01T10:00:00Z")+
		c.user("start", "2026-01-01T10:00:01Z"))
	a := PiAdapter{}
	start := a.Watermark(t.Context(), path)

	appendLines(t, path, c.calls("toolUse", "2026-01-01T10:00:02Z", "c1", "sleep 600", "c2", "echo c2")+
		c.result("c2", "2026-01-01T10:00:03Z")+
		c.entry("custom_message", `"customType":"ext","content":"context","display":false`, "2026-01-01T10:00:04Z")+
		c.entry("model_change", `"provider":"p","modelId":"m2"`, "2026-01-01T10:00:05Z")+
		c.entry("session_info", `"name":"renamed"`, "2026-01-01T10:00:06Z")+
		c.entry("label", `"targetId":"e2","label":"here"`, "2026-01-01T10:00:07Z"))
	events, _, _, wm, err := a.ParseSince(t.Context(), path, start, 0)
	if err != nil {
		t.Fatalf("poll 1: ParseSince: %v", err)
	}
	if len(events) != 0 || wm != start {
		t.Fatalf("poll 1: %d events, watermark %d; want 0 and %d — c1 is still running", len(events), wm, start)
	}

	appendLines(t, path, c.result("c1", "2026-01-01T10:10:00Z"))
	events, _, _, _, err = a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("poll 2: ParseSince: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("poll 2: got %d events, want c2 and c1", len(events))
	}
	assertOrphans(t, events)
}

// TestPiIncrementalMatchesFull runs the incremental==full oracle over Pi
// sessions containing orphaned calls, each released where a later message
// proves it dead — which used to hold the watermark forever, so this oracle
// failed outright — alongside calls that resolve across poll boundaries.
func TestPiIncrementalMatchesFull(t *testing.T) {
	tests := []struct {
		name    string
		steps   func(c *piChain) [][]string
		orphans []string
	}{
		{
			name: "parallel calls resolving in later polls",
			steps: func(c *piChain) [][]string {
				return [][]string{
					{c.calls("toolUse", "2026-01-01T10:00:02Z", "c1", "echo c1", "c2", "echo c2")},
					{c.result("c1", "2026-01-01T10:00:03Z")},
					{c.result("c2", "2026-01-01T10:00:04Z"), c.text("done", "stop", "2026-01-01T10:00:05Z")},
				}
			},
		},
		{
			name: "an interrupted response's calls, released by the next prompt",
			steps: func(c *piChain) [][]string {
				return [][]string{
					{c.calls("aborted", "2026-01-01T10:00:02Z", "c1", "echo orphan", "c2", "echo orphan")},
					{c.user("resumed", "2026-01-01T11:00:00Z")},
					{c.calls("toolUse", "2026-01-01T11:00:01Z", "c3", "echo c3")},
					{c.result("c3", "2026-01-01T11:00:02Z")},
				}
			},
			orphans: []string{"echo orphan"},
		},
		{
			// oh-my-pi, resuming a session whose process died mid-call,
			// writes an aborted assistant message to close the turn.
			name: "a call its process died running, released by the next response",
			steps: func(c *piChain) [][]string {
				return [][]string{
					{c.calls("toolUse", "2026-01-01T10:00:02Z", "c1", "echo c1", "c2", "echo orphan")},
					{c.result("c1", "2026-01-01T10:00:03Z")},
					{c.text("interrupted", "aborted", "2026-01-01T11:00:00Z")},
					{c.user("go on", "2026-01-01T11:00:01Z"), c.calls("toolUse", "2026-01-01T11:00:02Z", "c3", "echo c3")},
					{c.result("c3", "2026-01-01T11:00:03Z")},
				}
			},
			orphans: []string{"echo orphan"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c piChain
			path := writeTempJSONL(t, "s.jsonl", piHeaderLine("s1", "2026-01-01T10:00:00Z")+
				c.user("start", "2026-01-01T10:00:01Z"))
			var steps []func(t *testing.T)
			steps = append(steps, appendStep(path))
			for _, lines := range tt.steps(&c) {
				steps = append(steps, appendStep(path, lines...))
			}
			wm := assertIncrementalMatchesFull(t, &PiAdapter{}, path, steps)
			if end := jsonlCompleteOffset(path); wm != end {
				t.Errorf("watermark = %d, want %d: the session ends with nothing in flight", wm, end)
			}
			events, _, _, err := PiAdapter{}.Parse(t.Context(), path)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			assertOrphans(t, events, tt.orphans...)
		})
	}
}

// --- OpenCode ---

// ocTool is an OpenCode tool part running command in the given state.
func ocTool(callID, cmd, status string) string {
	extra := ""
	if status == "completed" {
		extra = `,"output":"ok"`
	}
	return `{"type":"tool","tool":"bash","callID":"` + callID + `","state":{"status":"` + status + `","input":{"command":"` + cmd + `"}` + extra + `}}`
}

// TestOpenCodeParseSinceReleasesOrphanedTool is the OpenCode shape of #103:
// a tool part left running by a process that died, then the session resumed.
// OpenCode runs one step at a time per session and, before its next
// assistant message is created, marks every tool part of the step it ends
// terminal ("Tool execution aborted" if nothing else) — so a later assistant
// message proves the part will never finish. The part is emitted in place
// with no result, where Parse has always put it, and everything after it is
// delivered.
func TestOpenCodeParseSinceReleasesOrphanedTool(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)
	db, dbPath := openTestOpenCodeDB(t)
	path := dbPath + "/ses_1"
	a := OpenCodeAdapter{DBPath: dbPath}

	steps := []func(t *testing.T){
		func(t *testing.T) {
			insertOpenCodeMessage(t, db, "m1", "ses_1", `{"role":"user","content":"start"}`, 1784148216000)
			insertOpenCodeMessage(t, db, "m2", "ses_1", `{"role":"assistant"}`, 1784148217000)
			insertOpenCodePart(t, db, "p1", "m2", "ses_1", ocTool("c1", "echo c1", "completed"), 1784148217100)
			insertOpenCodePart(t, db, "p2", "m2", "ses_1", ocTool("c2", "echo orphan", "running"), 1784148217200)
		},
		// The process is gone; nothing in the session changes for a while.
		func(t *testing.T) {},
		func(t *testing.T) {
			insertOpenCodeMessage(t, db, "m3", "ses_1", `{"role":"user","content":"resumed"}`, 1784150000000)
		},
		func(t *testing.T) {
			insertOpenCodeMessage(t, db, "m4", "ses_1", `{"role":"assistant"}`, 1784150001000)
			insertOpenCodePart(t, db, "p3", "m4", "ses_1", ocTool("c3", "echo c3", "completed"), 1784150001100)
		},
	}
	wm := assertIncrementalMatchesFull(t, &a, path, steps)
	if end := a.Watermark(t.Context(), path); wm != end {
		t.Errorf("watermark = %d, want %d: the session ends with nothing in flight", wm, end)
	}
	events, marks, _, err := a.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events) != 3 || events[1].Summary != orphanSummary {
		t.Fatalf("Parse = %+v, want the orphan at seq 1, between c1 and c3", events)
	}
	assertOrphans(t, events, "echo orphan")
	if len(marks) != 2 || marks[1].Note != "resumed" || marks[1].Seq != 2 {
		t.Errorf("marks = %+v, want the resumed turn's message at seq 2, after the orphan", marks)
	}
}

// TestOpenCodeParseSinceHoldsRunningToolThroughQueuedPrompt is the in-flight
// contract the release must not break. OpenCode writes a prompt sent while
// the session is busy as a user message straight away and runs it after the
// current step, and a subagent's assistant messages belong to its own child
// session: neither says anything about a part still running, so it is held
// until it turns terminal and then emitted once, with its result.
func TestOpenCodeParseSinceHoldsRunningToolThroughQueuedPrompt(t *testing.T) {
	resetDBCache()
	t.Cleanup(resetDBCache)
	db, dbPath := openTestOpenCodeDB(t)
	path := dbPath + "/ses_1"
	a := OpenCodeAdapter{DBPath: dbPath}

	insertOpenCodeMessage(t, db, "m1", "ses_1", `{"role":"assistant"}`, 1784148217000)
	insertOpenCodePart(t, db, "p1", "m1", "ses_1", ocTool("c1", "sleep 600", "running"), 1784148217100)
	insertOpenCodeMessage(t, db, "m2", "ses_1", `{"role":"user","content":"and another thing"}`, 1784148218000)
	insertOpenCodeMessage(t, db, "x1", "ses_2", `{"role":"assistant"}`, 1784148219000)
	resetDBCache()
	events, _, _, wm, err := a.ParseSince(t.Context(), path, 0, 0)
	if err != nil {
		t.Fatalf("poll 1: ParseSince: %v", err)
	}
	if len(events) != 0 || wm >= 1784148217100 {
		t.Fatalf("poll 1: %d events, watermark %d; want 0 and a watermark below the running part — it is still running", len(events), wm)
	}

	if _, err := db.Exec(`UPDATE part SET time_updated = ?, data = ? WHERE id = 'p1'`,
		1784148800000, ocTool("c1", "sleep 600", "completed")); err != nil {
		t.Fatal(err)
	}
	insertOpenCodeMessage(t, db, "m3", "ses_1", `{"role":"assistant"}`, 1784148800100)
	insertOpenCodePart(t, db, "p2", "m3", "ses_1", ocTool("c2", "echo c2", "completed"), 1784148800200)
	resetDBCache()
	events, _, _, _, err = a.ParseSince(t.Context(), path, wm, 0)
	if err != nil {
		t.Fatalf("poll 2: ParseSince: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("poll 2: got %d events, want c1 and c2", len(events))
	}
	assertOrphans(t, events)
}
