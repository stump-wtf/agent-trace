package tail

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// writeCCLookBackFile lays out the fixture the lookback tests share: one
// usage-bearing record for response req_A, then non-usage pad putting req_A
// roughly lookBackPast bytes behind the watermark, then — after the
// watermark — req_A's second record (the response straddles the watermark,
// the way Claude Code's interleaved tool results make response records
// non-adjacent) and response req_B's two records. It returns the path and
// the watermark: the byte offset a read resumes at.
func writeCCLookBackFile(t *testing.T, lookBackPast int) (path string, watermark int64) {
	t.Helper()
	usageLine := func(requestID string, outputTokens int64) string {
		return `{"type":"assistant","requestId":"` + requestID + `","message":{"id":"msg_01","role":"assistant","model":"claude-test-model","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"output_tokens":` + itoa64(outputTokens) + `}}}`
	}
	padLine := `{"type":"user","message":{"role":"user","content":"pad"}}` + "\n"
	pad := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(padLine)
		}
		return b.String()
	}

	var b strings.Builder
	b.WriteString(usageLine("req_A", 41) + "\n")
	b.WriteString(pad((lookBackPast / len(padLine)) + 1))
	watermark = int64(b.Len())
	// The response straddles the watermark: its second record sits after it.
	b.WriteString(usageLine("req_A", 42) + "\n")
	b.WriteString(usageLine("req_B", 91) + "\n")
	b.WriteString(usageLine("req_B", 92) + "\n")
	b.WriteString(pad(3))

	path = filepath.Join(t.TempDir(), "lookback.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, watermark
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ccFeedReport feeds one raw JSONL line to the tracker and returns the Usage
// it reports, mirroring ParseItemsSince's per-record call.
func ccFeedReport(t *testing.T, tr *ccUsageTracker, data []byte, seq int) (classify.Usage, bool) {
	t.Helper()
	var line ccRawLine
	if err := json.Unmarshal(data, &line); err != nil {
		t.Fatalf("unmarshal ccRawLine: %v", err)
	}
	var msg ccMessage
	if len(line.Message) > 0 {
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			t.Fatalf("unmarshal ccMessage: %v", err)
		}
	}
	return tr.report(line, msg, seq)
}

// ccFeedBefore replays the records before the watermark into a tracker, the
// state an earlier read left behind.
func ccFeedBefore(t *testing.T, tr *ccUsageTracker, path string, watermark int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range strings.SplitAfter(string(data[:watermark]), "\n") {
		if trimmed := strings.TrimSuffix(ln, "\n"); strings.TrimSpace(trimmed) != "" {
			ccFeedReport(t, tr, []byte(trimmed), 0)
		}
	}
}

// TestCCUsageLookBackPinnedBound: the usage lookback is bounded by
// ccUsageLookBackCap — 4 MiB, tighter than jsonlLastMatchCap's 16 MiB
// default (#143). Within the bound the resuming read still recognises the
// response it already reported; beyond it the walk gives up.
func TestCCUsageLookBackPinnedBound(t *testing.T) {
	if ccUsageLookBackCap >= jsonlLastMatchCap {
		t.Fatalf("ccUsageLookBackCap (%d) is not tighter than jsonlLastMatchCap (%d)", ccUsageLookBackCap, jsonlLastMatchCap)
	}
	if ccUsageLookBackCap != 4<<20 {
		t.Fatalf("ccUsageLookBackCap moved off its documented 4 MiB: %d", ccUsageLookBackCap)
	}
	if got := newCCUsageTracker("", 0).lookBackCap; got != ccUsageLookBackCap {
		t.Fatalf("production tracker cap %d, want the documented default %d", got, ccUsageLookBackCap)
	}

	// req_A's pre-watermark record sits ~2 KiB behind the watermark: within
	// a 4 KiB cap, beyond a 1 KiB one.
	path, watermark := writeCCLookBackFile(t, 2<<10)

	big := newCCUsageTracker(path, watermark)
	big.lookBackCap = 4 << 10
	if got := big.lookBack(""); got != "req_A" {
		t.Fatalf("lookBack within the cap returned %q, want \"req_A\"", got)
	}
	tiny := newCCUsageTracker(path, watermark)
	tiny.lookBackCap = 1 << 10
	if got := tiny.lookBack(""); got != "" {
		t.Fatalf("lookBack beyond the cap returned %q, want \"\"", got)
	}
}

// TestCCUsageLookBackMissDoubleCountsOnce: when the lookback misses, the
// first post-watermark record of the response the earlier read reported
// reports again — once — and its remaining records still dedup against it.
// Nothing is dropped. With the record within the cap, nothing duplicates.
func TestCCUsageLookBackMissDoubleCountsOnce(t *testing.T) {
	path, watermark := writeCCLookBackFile(t, 2<<10)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var linesAfter []string
	for _, ln := range strings.SplitAfter(string(data[watermark:]), "\n") {
		if trimmed := strings.TrimSuffix(ln, "\n"); strings.TrimSpace(trimmed) != "" {
			linesAfter = append(linesAfter, trimmed)
		}
	}

	// Cap too small to reach req_A's pre-watermark record: the straddling
	// record reports req_A a second time — the one documented duplicate —
	// and req_B reports as new.
	miss := newCCUsageTracker(path, watermark)
	miss.lookBackCap = 1 << 10
	var dup, reqB int
	for _, ln := range linesAfter {
		u, ok := ccFeedReport(t, miss, []byte(ln), 1)
		if !ok {
			continue
		}
		switch u.RequestID {
		case "req_A":
			dup++
		case "req_B":
			reqB++
		}
	}
	if dup != 1 || reqB != 1 {
		t.Fatalf("after a lookback miss: req_A duplicated %d times, req_B reported %d; want 1/1", dup, reqB)
	}

	// Cap covering req_A's record: the straddling response is recognised,
	// req_B is reported once, and req_A is not reported again.
	hit := newCCUsageTracker(path, watermark)
	hit.lookBackCap = 4 << 10
	ccFeedBefore(t, hit, path, watermark)
	var hitReqA, hitReqB int
	for _, ln := range linesAfter {
		u, ok := ccFeedReport(t, hit, []byte(ln), 1)
		if !ok {
			continue
		}
		switch u.RequestID {
		case "req_A":
			hitReqA++
		case "req_B":
			hitReqB++
		}
	}
	if hitReqA != 0 {
		t.Fatalf("with the lookback hit, req_A reported %d more times; want 0", hitReqA)
	}
	if hitReqB != 1 {
		t.Fatalf("req_B reported %d times; want 1", hitReqB)
	}
}
