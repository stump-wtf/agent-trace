package tail

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
)

// codexUsageFixtureWant is what testdata/codex_usage.jsonl records. The
// fixture follows the rollout shape Codex persists (codex-rs protocol
// TokenCountEvent: event_msg "token_count" with info.last_token_usage and
// info.total_token_usage), including a token_count with no info, which Codex
// writes for a rate-limit update before any response, and a token_count that
// repeats the previous one's numbers, which it writes when only the rate
// limits changed.
var codexUsageFixtureWant = []classify.Usage{
	{
		Seq: 1, At: time.Date(2026, 9, 20, 9, 0, 4, 100e6, time.UTC),
		Model: "gpt-test-codex", Provider: "openai",
		InputTokens: 3000, OutputTokens: 300, CacheRead: 6000,
	},
	{
		Seq: 1, At: time.Date(2026, 9, 20, 9, 0, 12, 100e6, time.UTC),
		Model: "gpt-test-codex", Provider: "openai",
		InputTokens: 300, OutputTokens: 150, CacheRead: 8800, CacheWrite: 400,
	},
	{
		Seq: 1, At: time.Date(2026, 9, 20, 9, 1, 3, 100e6, time.UTC),
		Model: "gpt-test-mini", Provider: "openai",
		InputTokens: 600, OutputTokens: 90, CacheRead: 9600,
	},
}

// TestCodexUsageFromFixture: one Usage per response, from last_token_usage,
// with cached prompt tokens moved out of InputTokens (Codex counts them
// inside input_tokens), the model of the turn the response belongs to, and
// the provider the session was opened with.
func TestCodexUsageFromFixture(t *testing.T) {
	a := &CodexAdapter{}
	path := filepath.Join("testdata", "codex_usage.jsonl")
	items, _, err := a.ParseItems(t.Context(), path)
	if err != nil {
		t.Fatalf("ParseItems: %v", err)
	}
	assertUsageEqual(t, items.Usage, codexUsageFixtureWant)
	assertParseDropsOnlyUsage(t, a, path)
}

// TestCodexParseSinceDeliversUsageOnce: polled after every record, each
// response is reported once — the rate-limit repeat is recognised even when
// it is the first record a read sees, and a read resuming mid-turn still
// names the turn's model.
func TestCodexParseSinceDeliversUsageOnce(t *testing.T) {
	a := &CodexAdapter{}
	got, path := pollJSONLGrowth(t, a, fixtureLines(t, "codex_usage.jsonl"))
	assertUsageEqual(t, got.Usage, codexUsageFixtureWant)

	wm := a.Watermark(t.Context(), path)
	again, _, _, err := a.ParseItemsSince(t.Context(), path, wm, len(got.Events))
	if err != nil {
		t.Fatalf("ParseItemsSince: %v", err)
	}
	if len(again.Usage) != 0 {
		t.Errorf("a poll with nothing new delivered %d usage items", len(again.Usage))
	}
}

// TestCodexNoUsageRecordedYieldsNoItems: rollouts with no token_count, or
// only ones without info, yield no Usage items; nor does a token_count whose
// last response spent nothing (Codex writes one when it fills the context
// window after an overflow).
func TestCodexNoUsageRecordedYieldsNoItems(t *testing.T) {
	a := &CodexAdapter{}
	for _, name := range []string{"codex_fidelity.jsonl", "codex_marks.jsonl"} {
		items, _, err := a.ParseItems(t.Context(), filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("%s: ParseItems: %v", name, err)
		}
		if items.Usage != nil {
			t.Errorf("%s records no usage, got %+v", name, items.Usage)
		}
	}
	path := writeTempJSONL(t, "s.jsonl",
		codexSessionMetaLine("2026-01-01T10:00:00Z")+
			`{"type":"event_msg","timestamp":"2026-01-01T10:00:01Z","payload":{"type":"token_count","info":null}}`+"\n"+
			`{"type":"event_msg","timestamp":"2026-01-01T10:00:02Z","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":272000},"last_token_usage":{"total_tokens":1000},"model_context_window":272000}}}`+"\n")
	items, _, err := a.ParseItems(t.Context(), path)
	if err != nil {
		t.Fatalf("ParseItems: %v", err)
	}
	if items.Usage != nil {
		t.Errorf("token_counts with no spend produced %+v", items.Usage)
	}
}
