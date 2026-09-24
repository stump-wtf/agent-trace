package tail

// oh-my-pi (OMP) session files, read by PiAdapter.
//
// Every fixture here is derived from OMP's source, not recorded from a run:
// oh-my-pi v18.3.0 (github.com/can1357/oh-my-pi at 62bc57be),
// packages/coding-agent/src/session/session-title-slot.ts (the slot),
// session-entries.ts (entry and header shapes) and docs/session.md. OMP was
// not installed or run. testdata/omp_session.jsonl is byte-identical to
// harness's internal/adapter/testdata/omp-session.jsonl (stump.wtf/harness
// PR #663), which harness's TestOMPSessionsAreNotReadByPiReader reads, so
// the two repositories agree on the same bytes.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ompTitleSlot builds an OMP title slot line the way serializeTitleSlot
// does: the JSON object, space-padded in "pad" to exactly 256 bytes
// including the newline.
func ompTitleSlot(t *testing.T, title, source, updatedAt string) string {
	t.Helper()
	type slot struct {
		Type      string `json:"type"`
		V         int    `json:"v"`
		Title     string `json:"title"`
		Source    string `json:"source,omitempty"`
		UpdatedAt string `json:"updatedAt"`
		Pad       string `json:"pad"`
	}
	s := slot{Type: "title", V: 1, Title: title, Source: source, UpdatedAt: updatedAt}
	unpadded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	pad := 256 - (len(unpadded) + 1)
	if pad < 0 {
		t.Fatalf("title %q does not fit a 256-byte slot", title)
	}
	s.Pad = strings.Repeat(" ", pad)
	line, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	out := string(line) + "\n"
	if len(out) != 256 {
		t.Fatalf("slot is %d bytes, want 256", len(out))
	}
	return out
}

// ompBody is an OMP session after its title slot: the v3 header, an OMP
// model_change ("provider/modelId"), one user turn, one read call answered,
// and a closing assistant message.
const ompBody = `{"type":"session","version":3,"id":"omp-sess-1","timestamp":"2026-09-24T08:00:00.000Z","cwd":"/work/omp"}` + "\n" +
	`{"type":"model_change","id":"e1","parentId":null,"timestamp":"2026-09-24T08:00:01.000Z","model":"anthropic/claude-sonnet-4-5"}` + "\n" +
	`{"type":"message","id":"e2","parentId":"e1","timestamp":"2026-09-24T08:00:02.000Z","message":{"role":"user","content":[{"type":"text","text":"fix the build"}]}}` + "\n" +
	`{"type":"message","id":"e3","parentId":"e2","timestamp":"2026-09-24T08:00:03.000Z","message":{"role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"Makefile"}}]}}` + "\n" +
	`{"type":"message","id":"e4","parentId":"e3","timestamp":"2026-09-24T08:00:04.000Z","message":{"role":"toolResult","toolCallId":"call_1","toolName":"read","content":[{"type":"text","text":"all: build"}],"isError":false}}` + "\n" +
	`{"type":"message","id":"e5","parentId":"e4","timestamp":"2026-09-24T08:00:05.000Z","message":{"role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"Found it."}]}}` + "\n"

// TestOMPFixtureParses reads harness's OMP fixture, the bytes harness will
// hand this reader once it observes omp.
func TestOMPFixtureParses(t *testing.T) {
	path := filepath.Join("testdata", "omp_session.jsonl")
	events, marks, meta, err := PiAdapter{}.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events) != 1 || events[0].Tool != "read" || events[0].IsError {
		t.Fatalf("events = %+v, want one successful read", events)
	}
	if len(marks) != 1 || marks[0].Type != "user-message" || marks[0].Note != "fix the build" {
		t.Errorf("marks = %+v, want the one user message", marks)
	}
	if meta.ID != "omp-fixture-1" || meta.Cwd != "/work/omp" {
		t.Errorf("meta ID/Cwd = %q/%q, want the header's", meta.ID, meta.Cwd)
	}
	if meta.Model != "z-ai/glm-5.3-flash" {
		t.Errorf("meta.Model = %q", meta.Model)
	}
	if meta.Title != "fix the build" {
		t.Errorf("meta.Title = %q, want the slot's", meta.Title)
	}
}

// TestOMPTitleSlotParsesLikeSlotless: a title slot changes nothing but the
// title. The same body with and without the slot yields identical events,
// marks and metadata, apart from the title (which the slot supplies) and the
// path-derived fields.
func TestOMPTitleSlotParsesLikeSlotless(t *testing.T) {
	const title = "Repair the Makefile build"
	slotted := writeTempJSONL(t, "omp.jsonl", ompTitleSlot(t, title, "auto", "2026-09-24T08:00:05.000Z")+ompBody)
	plain := writeTempJSONL(t, "pi.jsonl", ompBody)
	a := PiAdapter{}

	evS, mkS, metaS, err := a.Parse(t.Context(), slotted)
	if err != nil {
		t.Fatalf("Parse slotted: %v", err)
	}
	evP, mkP, metaP, err := a.Parse(t.Context(), plain)
	if err != nil {
		t.Fatalf("Parse slot-less: %v", err)
	}
	if len(evS) != 1 || len(mkS) != 1 {
		t.Fatalf("slotted: %d events, %d marks; want 1 and 1", len(evS), len(mkS))
	}
	if !reflect.DeepEqual(evS, evP) {
		t.Errorf("events differ:\n slotted %+v\n plain   %+v", evS, evP)
	}
	if !reflect.DeepEqual(mkS, mkP) {
		t.Errorf("marks differ:\n slotted %+v\n plain   %+v", mkS, mkP)
	}
	if metaS.Title != title {
		t.Errorf("slotted Title = %q, want the slot's %q", metaS.Title, title)
	}
	if metaP.Title != "fix the build" {
		t.Errorf("slot-less Title = %q, want the first user message", metaP.Title)
	}
	if metaS.ID != "omp-sess-1" || metaS.Model != "claude-sonnet-4-5" {
		t.Errorf("slotted ID/Model = %q/%q", metaS.ID, metaS.Model)
	}
	sameMeta := func(m SessionMeta) SessionMeta {
		m.Key, m.Path, m.Title = "", "", ""
		return m
	}
	if sameMeta(metaS) != sameMeta(metaP) {
		t.Errorf("meta differs:\n slotted %+v\n plain   %+v", metaS, metaP)
	}

	sumS, err := a.Summarize(t.Context(), slotted)
	if err != nil {
		t.Fatalf("Summarize slotted: %v", err)
	}
	sumP, err := a.Summarize(t.Context(), plain)
	if err != nil {
		t.Fatalf("Summarize slot-less: %v", err)
	}
	if sumS.Title != title {
		t.Errorf("Summarize slotted Title = %q, want %q", sumS.Title, title)
	}
	if sameMeta(sumS) != sameMeta(sumP) {
		t.Errorf("Summarize meta differs:\n slotted %+v\n plain   %+v", sumS, sumP)
	}
}

// TestOMPParseSinceAfterSlot: ParseSince from a watermark past the slot
// reads only what was appended, attributes it to the header behind the slot,
// and a rename — the slot rewritten in place at the same width — neither
// moves the watermark nor re-emits anything.
func TestOMPParseSinceAfterSlot(t *testing.T) {
	path := writeTempJSONL(t, "omp.jsonl", ompTitleSlot(t, "first title", "auto", "2026-09-24T08:00:05.000Z")+ompBody)
	a := PiAdapter{}

	events, _, _, err := a.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	offset := a.Watermark(t.Context(), path)
	if want := int64(256 + len(ompBody)); offset != want {
		t.Fatalf("Watermark = %d, want %d", offset, want)
	}

	// A linear append: a second user turn and a read call answered.
	appendLines(t, path, strings.Join([]string{
		`{"type":"message","id":"e6","parentId":"e5","timestamp":"2026-09-24T08:01:00.000Z","message":{"role":"user","content":[{"type":"text","text":"and the tests"}]}}`,
		`{"type":"message","id":"e7","parentId":"e6","timestamp":"2026-09-24T08:01:01.000Z","message":{"role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"toolCall","id":"call_2","name":"read","arguments":{"path":"main_test.go"}}]}}`,
		`{"type":"message","id":"e8","parentId":"e7","timestamp":"2026-09-24T08:01:02.000Z","message":{"role":"toolResult","toolCallId":"call_2","toolName":"read","content":[{"type":"text","text":"package main"}],"isError":false}}`,
	}, "\n")+"\n")
	// Rename before the poll lands: the slot is rewritten in place.
	rewriteSlot(t, path, ompTitleSlot(t, "Renamed by the user", "user", "2026-09-24T08:01:03.000Z"))

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	newEvents, newMarks, meta, next, err := a.ParseSince(t.Context(), path, offset, len(events))
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if len(newEvents) != 1 || newEvents[0].Seq != len(events) {
		t.Fatalf("new events = %+v, want only the appended call at seq %d", newEvents, len(events))
	}
	if len(newMarks) != 1 || newMarks[0].Note != "and the tests" {
		t.Errorf("new marks = %+v, want only the appended user message", newMarks)
	}
	if next != info.Size() {
		t.Errorf("watermark = %d, want end of file %d", next, info.Size())
	}
	// The header behind the slot, not the file name: this is what reading
	// the first line alone gets wrong.
	if meta.ID != "omp-sess-1" || meta.Cwd != "/work/omp" {
		t.Errorf("meta ID/Cwd = %q/%q, want the header's omp-sess-1 and /work/omp", meta.ID, meta.Cwd)
	}
	if meta.Title != "Renamed by the user" {
		t.Errorf("meta.Title = %q, want the rewritten slot's", meta.Title)
	}

	// Another rename with nothing appended: same size, same watermark,
	// nothing re-emitted.
	rewriteSlot(t, path, ompTitleSlot(t, "Renamed again", "user", "2026-09-24T08:02:00.000Z"))
	if got := a.Watermark(t.Context(), path); got != next {
		t.Errorf("Watermark after rename = %d, want unchanged %d", got, next)
	}
	again, againMarks, _, after, err := a.ParseSince(t.Context(), path, next, len(events)+1)
	if err != nil {
		t.Fatalf("ParseSince after rename: %v", err)
	}
	if len(again) != 0 || len(againMarks) != 0 || after != next {
		t.Errorf("after rename: %d events, %d marks, watermark %d; want 0, 0, %d", len(again), len(againMarks), after, next)
	}
}

// TestOMPTitleSlotFailsClosed: a slot is skipped only as the first line and
// only once, and only a slot OMP itself would accept is skipped. Without a
// session header straight after it, the file is not a session.
func TestOMPTitleSlotFailsClosed(t *testing.T) {
	header := `{"type":"session","version":3,"id":"s1","timestamp":"2026-09-24T08:00:00.000Z","cwd":"/work"}` + "\n"
	message := `{"type":"message","id":"e1","parentId":null,"timestamp":"2026-09-24T08:00:01.000Z","message":{"role":"user","content":"hi"}}` + "\n"
	slot := ompTitleSlot(t, "t", "auto", "2026-09-24T08:00:00.000Z")
	tests := []struct {
		name    string
		content string
	}{
		{"slot alone", slot},
		{"slot then an entry", slot + message},
		{"two slots then header", slot + slot + header},
		{"an entry before the slot and header", message + slot + header},
		{"slot of an unknown version", strings.Replace(slot, `"v":1`, `"v":2`, 1) + header},
		{"slot with an unknown source", `{"type":"title","v":1,"title":"t","source":"bot","updatedAt":"x","pad":""}` + "\n" + header},
		{"slot missing updatedAt", `{"type":"title","v":1,"title":"t","pad":""}` + "\n" + header},
		{"slot with a numeric title", `{"type":"title","v":1,"title":7,"updatedAt":"x","pad":""}` + "\n" + header},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "s.jsonl")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			a := PiAdapter{Dir: dir}
			if _, _, _, err := a.Parse(t.Context(), path); err == nil {
				t.Error("Parse accepted it")
			}
			if _, err := a.Summarize(t.Context(), path); err == nil {
				t.Error("Summarize accepted it")
			}
			if _, ok := piSessionHead(path); ok {
				t.Error("piSessionHead found a header")
			}
			metas, err := a.ListSessions(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(metas) != 0 {
				t.Errorf("ListSessions listed it: %+v", metas)
			}
		})
	}

	// The control: the same slot with a header straight after it is a session.
	path := writeTempJSONL(t, "ok.jsonl", slot+header+message)
	if _, _, _, err := (PiAdapter{}).Parse(t.Context(), path); err != nil {
		t.Errorf("slot + header rejected: %v", err)
	}
	if _, ok := piSessionHead(path); !ok {
		t.Error("piSessionHead missed the header behind the slot")
	}
}

// TestOMPOnlyEntriesTolerated: OMP's entry types Pi does not have, and its
// pythonExecution message role, sit in the parent chain without breaking it
// or adding anything. The session with them parses to the same events and
// marks as the session without them.
func TestOMPOnlyEntriesTolerated(t *testing.T) {
	head := `{"type":"session","version":3,"id":"s1","timestamp":"2026-09-24T08:00:00.000Z","cwd":"/work"}` + "\n"
	user := `{"type":"message","id":"u1","parentId":%q,"timestamp":"2026-09-24T08:00:01.000Z","message":{"role":"user","content":"go"}}`
	call := `{"type":"message","id":"a1","parentId":%q,"timestamp":"2026-09-24T08:00:02.000Z","message":{"role":"assistant","model":"m","content":[{"type":"toolCall","id":"c1","name":"read","arguments":{"path":"x.go"}}]}}`
	result := `{"type":"message","id":"r1","parentId":%q,"timestamp":"2026-09-24T08:00:03.000Z","message":{"role":"toolResult","toolCallId":"c1","content":"ok","isError":false}}`
	done := `{"type":"message","id":"d1","parentId":%q,"timestamp":"2026-09-24T08:00:09.000Z","message":{"role":"assistant","model":"m","content":"done"}}`
	// Each OMP-only record, chained, as session-entries.ts declares them.
	extras := []string{
		`{"type":"session_init","id":"x1","parentId":"r1","timestamp":"2026-09-24T08:00:04.000Z","systemPrompt":"sys","task":"t","tools":["read"]}`,
		`{"type":"mode_change","id":"x2","parentId":"x1","timestamp":"2026-09-24T08:00:04.000Z","mode":"plan","data":{"planFile":"p.md"}}`,
		`{"type":"title_change","id":"x3","parentId":"x2","timestamp":"2026-09-24T08:00:05.000Z","title":"new","previousTitle":"old","source":"user"}`,
		`{"type":"model_usage","id":"x4","parentId":"x3","timestamp":"2026-09-24T08:00:05.000Z","purpose":"title","role":"tiny","api":"a","provider":"p","model":"tiny-model","usage":{},"stopReason":"stop"}`,
		`{"type":"thinking_level_change","id":"x5","parentId":"x4","timestamp":"2026-09-24T08:00:06.000Z","thinkingLevel":"high"}`,
		`{"type":"service_tier_change","id":"x6","parentId":"x5","timestamp":"2026-09-24T08:00:06.000Z","serviceTier":null}`,
		`{"type":"ttsr_injection","id":"x7","parentId":"x6","timestamp":"2026-09-24T08:00:07.000Z","injectedRules":["r"]}`,
		`{"type":"credential_pin","id":"x8","parentId":"x7","timestamp":"2026-09-24T08:00:07.000Z","provider":"anthropic","hash":"h"}`,
		`{"type":"message","id":"x9","parentId":"x8","timestamp":"2026-09-24T08:00:08.000Z","message":{"role":"pythonExecution","code":"print(1)","output":"1","exitCode":0}}`,
		`{"type":"reset_boundary","id":"x10","parentId":"x9","timestamp":"2026-09-24T08:00:08.000Z"}`,
	}
	sprintf := func(format, parent string) string {
		return strings.Replace(format, "%q", `"`+parent+`"`, 1)
	}
	base := head + sprintf(user, "") + "\n" + sprintf(call, "u1") + "\n" + sprintf(result, "a1") + "\n"
	with := writeTempJSONL(t, "with.jsonl", base+strings.Join(extras, "\n")+"\n"+sprintf(done, "x10")+"\n")
	without := writeTempJSONL(t, "without.jsonl", base+sprintf(done, "r1")+"\n")

	evW, mkW, metaW, err := PiAdapter{}.Parse(t.Context(), with)
	if err != nil {
		t.Fatalf("Parse with OMP entries: %v", err)
	}
	evO, mkO, _, err := PiAdapter{}.Parse(t.Context(), without)
	if err != nil {
		t.Fatalf("Parse without: %v", err)
	}
	if len(evW) != 1 || len(mkW) != 1 {
		t.Fatalf("with OMP entries: %d events, %d marks; want 1 and 1", len(evW), len(mkW))
	}
	if !reflect.DeepEqual(evW, evO) || !reflect.DeepEqual(mkW, mkO) {
		t.Errorf("OMP-only entries changed the parse:\n with    %+v %+v\n without %+v %+v", evW, mkW, evO, mkO)
	}
	// The chain ran through them to the last record, rather than stopping.
	if metaW.EndedAt != "2026-09-24T08:00:09.000Z" {
		t.Errorf("EndedAt = %q, want the final record's", metaW.EndedAt)
	}
	// model_usage names a model too, but it is not a model_change.
	if metaW.Model != "m" {
		t.Errorf("Model = %q, want the assistant's m", metaW.Model)
	}
}

// TestOMPModelChange: OMP's model_change records "provider/modelId" under
// model, with an optional role; only the default role is the session's
// model. Pi's modelId is read as before.
func TestOMPModelChange(t *testing.T) {
	header := `{"type":"session","version":3,"id":"s1","timestamp":"2026-09-24T08:00:00.000Z","cwd":"/work"}` + "\n"
	tests := []struct {
		name    string
		entries []string
		want    string
	}{
		{"omp provider/model", []string{`{"type":"model_change","id":"e1","parentId":null,"model":"anthropic/claude-sonnet-4-5"}`}, "claude-sonnet-4-5"},
		{"omp model id with a slash keeps it", []string{`{"type":"model_change","id":"e1","parentId":null,"model":"openrouter/z-ai/glm-5.3"}`}, "z-ai/glm-5.3"},
		{"omp explicit default role", []string{`{"type":"model_change","id":"e1","parentId":null,"model":"openai/gpt-4o","role":"default"}`}, "gpt-4o"},
		{"omp smol role does not replace the session model", []string{
			`{"type":"model_change","id":"e1","parentId":null,"model":"anthropic/claude-sonnet-4-5"}`,
			`{"type":"model_change","id":"e2","parentId":"e1","model":"anthropic/claude-haiku-4-5","role":"smol"}`,
		}, "claude-sonnet-4-5"},
		{"pi modelId", []string{`{"type":"model_change","id":"e1","parentId":null,"provider":"anthropic","modelId":"claude-opus-4"}`}, "claude-opus-4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempJSONL(t, "s.jsonl", header+strings.Join(tt.entries, "\n")+"\n")
			meta, err := PiAdapter{}.Summarize(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			if meta.Model != tt.want {
				t.Errorf("Summarize Model = %q, want %q", meta.Model, tt.want)
			}
			_, _, pmeta, err := PiAdapter{}.Parse(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			if pmeta.Model != tt.want {
				t.Errorf("Parse Model = %q, want %q", pmeta.Model, tt.want)
			}
		})
	}
}

// TestPiAdapterOMPLabel: OMP set labels sessions HarnessOMP — their keys
// included — and moves the default layout to .omp; the zero value is
// unchanged.
func TestPiAdapterOMPLabel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(ompTitleSlot(t, "t", "", "x")+ompBody), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		adapter PiAdapter
		want    Harness
	}{
		{PiAdapter{Dir: dir}, HarnessPi},
		{PiAdapter{Dir: dir, OMP: true}, HarnessOMP},
	} {
		if got := tt.adapter.Harness(); got != tt.want {
			t.Errorf("Harness() = %q, want %q", got, tt.want)
		}
		metas, err := tt.adapter.ListSessions(t.Context())
		if err != nil || len(metas) != 1 {
			t.Fatalf("%s ListSessions = %v, %v; want one session", tt.want, metas, err)
		}
		if metas[0].Harness != tt.want || metas[0].Key != sessionKey(string(tt.want), path) {
			t.Errorf("%s session labelled %q with key %q", tt.want, metas[0].Harness, metas[0].Key)
		}
		_, _, meta, err := tt.adapter.Parse(t.Context(), path)
		if err != nil || meta.Harness != tt.want {
			t.Errorf("%s Parse labelled %q (err %v)", tt.want, meta.Harness, err)
		}
	}

	rooted := PiAdapter{OMP: true}.WithRoot("/tmp/home")
	if got := rooted.SessionDir(); got != filepath.Join("/tmp/home", ".omp", "agent", "sessions") {
		t.Errorf("OMP WithRoot SessionDir = %q", got)
	}
	if rooted.Harness() != HarnessOMP {
		t.Errorf("WithRoot dropped OMP: %q", rooted.Harness())
	}
	if got := (PiAdapter{}).WithRoot("/tmp/home").SessionDir(); got != filepath.Join("/tmp/home", ".pi", "agent", "sessions") {
		t.Errorf("Pi WithRoot SessionDir = %q", got)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := (PiAdapter{OMP: true}).SessionDir(); got != filepath.Join(home, ".omp", "agent", "sessions") {
		t.Errorf("OMP default SessionDir = %q", got)
	}
}

// rewriteSlot overwrites the first 256 bytes in place, as OMP renames a
// session (overlayTitleSlotPrefix).
func rewriteSlot(t *testing.T, path, slot string) {
	t.Helper()
	if len(slot) != 256 {
		t.Fatalf("slot is %d bytes", len(slot))
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt([]byte(slot), 0); err != nil {
		t.Fatal(err)
	}
}
