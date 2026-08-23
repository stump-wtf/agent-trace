package tail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Recovering EndedAt from a bounded tail
//
// #82 made Summarize read a bounded head plus a bounded tail. #83 promoted
// EndedAt from a display field to the criterion ActiveSince lists on. The
// interaction is what these tests pin: when the tail window holds no whole
// line — the ordinary shape of a transcript whose last record is a big tool
// result — EndedAt used to keep whatever the head last set, so a session being
// typed into right now reported a timestamp from the top of its own file and
// fell out of the activity window entirely.
//
// @joestump-agent 08/23/2026 - Added for #85.

// bigTailSession writes a Claude Code transcript whose content timestamps are
// all old, whose line count exceeds headLines, and whose final line is larger
// than tailWindow — then stamps the file as modified at mtime.
func bigTailSession(t *testing.T, root, name string, contentAt, mtime time.Time, headLines, tailWindow int) string {
	t.Helper()
	proj := filepath.Join(root, ".claude", "projects", "p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(proj, name+".jsonl")
	stamp := contentAt.UTC().Format("2006-01-02T15:04:05.000Z")

	var b strings.Builder
	fmt.Fprintf(&b, `{"type":"user","timestamp":%q,"sessionId":%q,"cwd":"/w","message":{"role":"user","content":"hi"}}`+"\n", stamp, name)
	for range headLines * 2 {
		fmt.Fprintf(&b, `{"type":"assistant","timestamp":%q,"sessionId":%q,"message":{"role":"assistant","model":"m","content":[]}}`+"\n", stamp, name)
	}
	// The final record: one tool result larger than the tail window, carrying
	// the timestamp that is the session's real last activity.
	fmt.Fprintf(&b, `{"type":"user","timestamp":%q,"sessionId":%q,"message":{"role":"user","content":%q}}`+"\n",
		mtime.UTC().Format("2006-01-02T15:04:05.000Z"), name, strings.Repeat("x", tailWindow*2))

	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return path
}

// TestScanSummaryTailReportsAWindowWithNoWholeLine is the primitive the fix
// rests on: "the tail delivered nothing" has to be distinguishable from "the
// tail delivered lines, none of them newer", because the caller's fields look
// identical in both cases and mean opposite things.
func TestScanSummaryTailReportsAWindowWithNoWholeLine(t *testing.T) {
	const window = 1 << 10
	f := writeRaw(t, []byte(`{"n":0}`+"\n"+`{"pad":"`+strings.Repeat("x", 4*window)+`"}`+"\n"))

	b := summaryBudget{maxLines: 1, maxBytes: 1 << 20, tailBytes: window}
	_, scan, err := collectScan(f, b)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scan.Complete {
		t.Error("Complete = true, want false")
	}
	if scan.SawFinalLine {
		t.Error("SawFinalLine = true, but the tail window holds no newline at all")
	}
}

// TestScanSummarySawFinalLineWhenTheTailIsReadable is the other half: the flag
// must not be a blanket "the file was elided", or every oversized session
// would be dated by mtime and the exact timestamp the tail read exists to
// recover would be thrown away.
func TestScanSummarySawFinalLineWhenTheTailIsReadable(t *testing.T) {
	var lines []string
	for i := range 100 {
		lines = append(lines, fmt.Sprintf(`{"n":%03d}`, i))
	}
	f := writeLines(t, lines)

	_, scan, err := collectScan(f, summaryBudget{maxLines: 3, maxBytes: 1 << 20, tailBytes: 64})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scan.Complete {
		t.Error("Complete = true, want false")
	}
	if !scan.SawFinalLine {
		t.Error("SawFinalLine = false, but the tail window holds several whole lines")
	}
}

// TestSummarizeDatesAnUnreadableTailByMtime is the defect from #85 stated as
// the caller sees it: without the fallback, EndedAt is whatever timestamp the
// head last set, which is a point near the *start* of a long session.
func TestSummarizeDatesAnUnreadableTailByMtime(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Truncate(time.Second)
	old := now.Add(-5 * 24 * time.Hour)

	path := bigTailSession(t, root, "s0", old, now, 64, 64<<10)
	a := (&ClaudeCodeAdapter{}).WithRoot(root)

	meta, err := a.(*ClaudeCodeAdapter).Summarize(context.Background(), path)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	ended, ok := meta.Ended()
	if !ok {
		t.Fatalf("EndedAt %q does not parse", meta.EndedAt)
	}
	if skew := ended.Sub(now); skew < -time.Second || skew > time.Second {
		t.Errorf("EndedAt = %s, want the file's mtime %s (off by %s)",
			ended.UTC(), now.UTC(), skew)
	}
}

// TestActivityWindowKeepsASessionWithAnUnreadableTail is the reproduction from
// the issue end to end. Every ingredient is ordinary — a session older than
// the window, more lines than the head budget, and a final line bigger than
// the tail window — and the failure was silent: the session simply was not in
// the listing.
func TestActivityWindowKeepsASessionWithAnUnreadableTail(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-5 * 24 * time.Hour)

	bigTailSession(t, root, "live", old, now, 64, 64<<10)
	a := (&ClaudeCodeAdapter{}).WithRoot(root)

	got, err := ListSessionsFiltered(context.Background(), a, SessionFilter{
		ActiveSince: now.Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("ListSessionsFiltered: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("a session being written to right now was dropped by a 48h activity window: got %d sessions", len(got))
	}
}

// TestSummarizeKeepsTheExactTimestampWhenTheTailIsReadable guards the
// fallback's blast radius. mtime is a substitute for a value that could not be
// read, never a replacement for one that could: a copied or restored file
// carries an mtime unrelated to its contents, so preferring it over a
// timestamp the tail actually delivered would make every summary worse.
func TestSummarizeKeepsTheExactTimestampWhenTheTailIsReadable(t *testing.T) {
	root := t.TempDir()
	contentEnd := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	mtime := time.Now()

	path := session(t, root, "s0", contentEnd.Add(-time.Hour), contentEnd, mtime)
	a := (&ClaudeCodeAdapter{}).WithRoot(root)

	meta, err := a.(*ClaudeCodeAdapter).Summarize(context.Background(), path)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	ended, ok := meta.Ended()
	if !ok {
		t.Fatalf("EndedAt %q does not parse", meta.EndedAt)
	}
	if !ended.Equal(contentEnd) {
		t.Errorf("EndedAt = %s, want the timestamp in the file %s", ended.UTC(), contentEnd.UTC())
	}
}

// TestSummarizeSeesAnAppendItCannotRead is the second consequence of a stale
// EndedAt, and the more damaging one: the watcher's change detection compares
// EndedAt against the previous scan's, so a session whose reported EndedAt is
// frozen at a head timestamp is judged "unchanged" on every poll and never
// re-parsed at all. Its events are not late — they never arrive.
//
// Back-to-back large tool results are the ordinary way a session stays in that
// state while genuinely running.
func TestSummarizeSeesAnAppendItCannotRead(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Truncate(time.Second)
	old := now.Add(-5 * 24 * time.Hour)

	path := bigTailSession(t, root, "s0", old, now.Add(-time.Minute), 64, 64<<10)
	a := (&ClaudeCodeAdapter{}).WithRoot(root).(*ClaudeCodeAdapter)

	before, err := a.Summarize(context.Background(), path)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	// Another large tool result lands, so the tail is still unreadable.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := fmt.Fprintf(f, `{"type":"user","timestamp":%q,"sessionId":"s0","message":{"role":"user","content":%q}}`+"\n",
		now.UTC().Format("2006-01-02T15:04:05.000Z"), strings.Repeat("y", 128<<10)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	after, err := a.Summarize(context.Background(), path)
	if err != nil {
		t.Fatalf("Summarize after append: %v", err)
	}
	if after.EndedAt == before.EndedAt {
		t.Errorf("EndedAt unchanged at %q across an append — the watcher reads this as "+
			"\"nothing happened\" and skips the session", before.EndedAt)
	}
}
