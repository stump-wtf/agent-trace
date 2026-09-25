package tail

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
)

// ccUsageFixtureWant is what testdata/claudecode_usage.jsonl records: three
// API responses, one of them written across three records with a tool result
// between two of them, and a failed call that spent nothing.
var ccUsageFixtureWant = []classify.Usage{
	{
		Seq: 0, At: time.Date(2026, 9, 20, 10, 0, 4, 0, time.UTC),
		Model: "claude-test-model", RequestID: "req_01A",
		InputTokens: 2, OutputTokens: 557, CacheRead: 43155, CacheWrite: 3864,
	},
	{
		Seq: 2, At: time.Date(2026, 9, 20, 10, 0, 15, 0, time.UTC),
		Model: "claude-test-model", RequestID: "req_01B",
		InputTokens: 4, OutputTokens: 88, CacheRead: 47019, CacheWrite: 120,
	},
	{
		Seq: 2, At: time.Date(2026, 9, 20, 10, 2, 7, 0, time.UTC),
		Model: "claude-other-model", RequestID: "req_01C",
		InputTokens: 6, OutputTokens: 240, CacheRead: 51000,
	},
}

// TestClaudeCodeUsageFromFixture: one Usage per API response, not per record.
// Claude Code writes each content block of a response on its own line and
// repeats the response's usage on every one of them, so reporting per record
// would count a three-block response three times.
func TestClaudeCodeUsageFromFixture(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	path := filepath.Join("testdata", "claudecode_usage.jsonl")
	items, _, err := a.ParseItems(t.Context(), path)
	if err != nil {
		t.Fatalf("ParseItems: %v", err)
	}
	assertUsageEqual(t, items.Usage, ccUsageFixtureWant)
	assertParseDropsOnlyUsage(t, a, path)
}

// TestClaudeCodeParseSinceDeliversUsageOnce: read incrementally with a poll
// after every record, each response's usage arrives exactly once — including
// the response whose records straddle polls, where a read resuming mid-
// response must recognise the response it already reported.
func TestClaudeCodeParseSinceDeliversUsageOnce(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	for _, name := range []string{"claudecode_api_errors.jsonl", "claudecode_api_error_outage.jsonl", "claudecode_api_error_pending.jsonl"} {
		got, path := pollJSONLGrowth(t, a, fixtureLines(t, name))
		want, _, err := a.ParseItems(t.Context(), path)
		if err != nil {
			t.Fatalf("%s: ParseItems: %v", name, err)
		}
		assertUsageEqual(t, got.Usage, want.Usage)
	}

	got, path := pollJSONLGrowth(t, a, fixtureLines(t, "claudecode_usage.jsonl"))
	assertUsageEqual(t, got.Usage, ccUsageFixtureWant)

	// Polling again with nothing new delivers nothing again.
	wm := a.Watermark(t.Context(), path)
	again, _, _, err := a.ParseItemsSince(t.Context(), path, wm, len(got.Events))
	if err != nil {
		t.Fatalf("ParseItemsSince: %v", err)
	}
	if len(again.Usage) != 0 {
		t.Errorf("a poll with nothing new delivered %d usage items", len(again.Usage))
	}
}

// TestClaudeCodeNoUsageRecordedYieldsNoItems: a transcript with no usage
// objects yields no Usage items, and a usage object of all zeros — which is
// what a failed call's synthetic record carries — is not reported either.
func TestClaudeCodeNoUsageRecordedYieldsNoItems(t *testing.T) {
	a := &ClaudeCodeAdapter{}
	for _, name := range []string{"claudecode_basic.jsonl", "claudecode_api_error_outage.jsonl"} {
		items, _, err := a.ParseItems(t.Context(), filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("%s: ParseItems: %v", name, err)
		}
		for _, u := range items.Usage {
			if u.InputTokens+u.OutputTokens+u.CacheRead+u.CacheWrite == 0 {
				t.Errorf("%s: zero-valued usage item %+v", name, u)
			}
		}
		if name == "claudecode_basic.jsonl" && items.Usage != nil {
			t.Errorf("%s records no usage, got %+v", name, items.Usage)
		}
	}
	path := writeTempJSONL(t, "s.jsonl",
		`{"type":"assistant","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","requestId":"r1","message":{"id":"m1","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":0,"output_tokens":0}}}`+"\n")
	items, _, err := a.ParseItems(t.Context(), path)
	if err != nil {
		t.Fatalf("ParseItems: %v", err)
	}
	if items.Usage != nil {
		t.Errorf("an all-zero usage object produced %+v", items.Usage)
	}
}

// TestClaudeCodeUsageKeepsConversationsApart: a subagent's lines interleaved
// with the parent's are separate conversations, so a subagent response
// between two records of one parent response neither hides the subagent's
// usage nor makes the parent's count twice.
func TestClaudeCodeUsageKeepsConversationsApart(t *testing.T) {
	line := func(req, sidechain, ts string) string {
		return `{"type":"assistant","timestamp":"` + ts + `","sessionId":"s1","isSidechain":` + sidechain + `,"requestId":"` + req + `","message":{"id":"m-` + req + `","role":"assistant","model":"m","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":1,"output_tokens":2}}}` + "\n"
	}
	lines := []string{
		line("parent", "false", "2026-01-01T10:00:00Z"),
		line("child", "true", "2026-01-01T10:00:01Z"),
		line("parent", "false", "2026-01-01T10:00:02Z"),
	}
	a := &ClaudeCodeAdapter{}
	got, path := pollJSONLGrowth(t, a, lines)
	full, _, err := a.ParseItems(t.Context(), path)
	if err != nil {
		t.Fatalf("ParseItems: %v", err)
	}
	if len(full.Usage) != 2 || full.Usage[0].RequestID != "parent" || full.Usage[1].RequestID != "child" {
		t.Fatalf("ParseItems usage = %+v, want parent then child", full.Usage)
	}
	assertUsageEqual(t, got.Usage, full.Usage)
}
