package tail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLines writes lines (newline-terminated) to a temp file and opens it.
func writeLines(t *testing.T, lines []string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func collect(f *os.File, b summaryBudget) ([]string, bool, error) {
	var got []string
	complete, err := scanJSONLSummaryWith(f, b, func(data []byte) {
		got = append(got, string(data))
	})
	return got, complete, err
}

func TestScanJSONLSummaryConsumesSmallFileWhole(t *testing.T) {
	lines := []string{`{"n":1}`, `{"n":2}`, `{"n":3}`}
	f := writeLines(t, lines)

	got, complete, err := collect(f, summaryBudget{maxLines: 100, maxBytes: 1 << 20, tailBytes: 16})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !complete {
		t.Error("complete = false, want true for a file inside the head budget")
	}
	if len(got) != len(lines) {
		t.Fatalf("visited %d lines, want %d: %q", len(got), len(lines), got)
	}
	for i, want := range lines {
		if got[i] != want {
			t.Errorf("line %d = %q, want %q", i, got[i], want)
		}
	}
}

func TestScanJSONLSummaryElidesMiddleOfOversizedFile(t *testing.T) {
	// 200 lines of a fixed width, a 3-line head budget, and a tail window
	// sized to a couple of lines: the middle must drop out entirely.
	var lines []string
	for i := range 200 {
		lines = append(lines, fmt.Sprintf(`{"n":%03d}`, i))
	}
	f := writeLines(t, lines)

	got, complete, err := collect(f, summaryBudget{maxLines: 3, maxBytes: 1 << 20, tailBytes: 25})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if complete {
		t.Error("complete = true, want false for a file past the head budget")
	}

	// Head arrives first and in order.
	if len(got) < 3 {
		t.Fatalf("visited %d lines, want at least the 3-line head: %q", len(got), got)
	}
	for i := range 3 {
		if got[i] != lines[i] {
			t.Errorf("head line %d = %q, want %q", i, got[i], lines[i])
		}
	}

	// The final line of the file must be the final line delivered — this is
	// what Summarize reads EndedAt from.
	if last := got[len(got)-1]; last != lines[len(lines)-1] {
		t.Errorf("last visited = %q, want %q", last, lines[len(lines)-1])
	}

	// The middle is elided, not merely reordered.
	if len(got) > 20 {
		t.Errorf("visited %d lines, want only head + a short tail", len(got))
	}
	for _, line := range got {
		if line == lines[100] {
			t.Errorf("visited elided middle line %q", line)
		}
	}

	// Every delivered line is whole: no fragment from the mid-line seek.
	for _, line := range got {
		if !strings.HasPrefix(line, `{"n":`) || !strings.HasSuffix(line, `}`) {
			t.Errorf("visited a partial line %q", line)
		}
	}
}

func TestScanJSONLSummaryTailOverlappingHeadDeliversNoDuplicates(t *testing.T) {
	// A tail window large enough to reach back past where the head stopped.
	// The scan must resume at the head boundary rather than replaying lines.
	var lines []string
	for i := range 20 {
		lines = append(lines, fmt.Sprintf(`{"n":%03d}`, i))
	}
	f := writeLines(t, lines)

	got, _, err := collect(f, summaryBudget{maxLines: 5, maxBytes: 1 << 20, tailBytes: 1 << 20})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != len(lines) {
		t.Fatalf("visited %d lines, want %d with no duplicates: %q", len(got), len(lines), got)
	}
	seen := map[string]int{}
	for _, line := range got {
		seen[line]++
	}
	for line, n := range seen {
		if n != 1 {
			t.Errorf("line %q delivered %d times, want once", line, n)
		}
	}
	for i, want := range lines {
		if got[i] != want {
			t.Errorf("line %d = %q, want %q", i, got[i], want)
		}
	}
}

func TestScanJSONLSummaryByteBudgetBoundsHead(t *testing.T) {
	// A generous line budget must not let a few enormous lines blow the read.
	big := `{"pad":"` + strings.Repeat("x", 4096) + `"}`
	lines := []string{big, big, big, big, big, `{"n":"last"}`}
	f := writeLines(t, lines)

	got, complete, err := collect(f, summaryBudget{maxLines: 1000, maxBytes: 5000, tailBytes: 32})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if complete {
		t.Error("complete = true, want false once the byte budget stops the head")
	}
	if len(got) > 4 {
		t.Errorf("visited %d lines, want the head cut short by the byte budget", len(got))
	}
	if last := got[len(got)-1]; last != `{"n":"last"}` {
		t.Errorf("last visited = %q, want the file's final line", last)
	}
}

func TestScanJSONLSummaryTailWindowWithoutNewline(t *testing.T) {
	// One enormous final line: the tail window lands inside it and contains no
	// newline, so there is no whole line to recover. The scan must return the
	// head cleanly rather than emitting a fragment.
	lines := []string{`{"n":1}`, `{"n":2}`, `{"pad":"` + strings.Repeat("y", 8192) + `"}`}
	f := writeLines(t, lines)

	got, _, err := collect(f, summaryBudget{maxLines: 2, maxBytes: 1 << 20, tailBytes: 64})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("visited %d lines, want just the 2-line head: %q", len(got), got)
	}
	for _, line := range got {
		if strings.HasPrefix(line, "y") {
			t.Errorf("visited a fragment of the final line: %q", line)
		}
	}
}

func TestScanJSONLSummaryEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	got, complete, err := collect(f, defaultSummaryBudget())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !complete {
		t.Error("complete = false, want true for an empty file")
	}
	if len(got) != 0 {
		t.Errorf("visited %d lines, want 0: %q", len(got), got)
	}
}

// writeRaw writes exact bytes to a temp file and opens it, for fixtures whose
// point is what is *not* at a line boundary.
func writeRaw(t *testing.T, data []byte) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestScanSummaryHeadNeverDeliversMoreThanTheByteBudget pins the bound the
// budget is named for. A .jsonl with no newline in it is not hypothetical —
// the scan runs over whatever the trajectory directories contain, including
// foreign and corrupt files this library did not write — and before the head
// read through an io.LimitReader, bufio grew such a line to its full length
// before the budget was ever consulted.
func TestScanSummaryHeadNeverDeliversMoreThanTheByteBudget(t *testing.T) {
	const budget = 64 << 10
	f := writeRaw(t, []byte(`{"pad":"`+strings.Repeat("x", 8<<20)+`"}`))

	var largest int
	_, _, err := scanSummaryHead(f, summaryBudget{maxLines: 64, maxBytes: budget, tailBytes: 1 << 10},
		func(data []byte) {
			if len(data) > largest {
				largest = len(data)
			}
		})
	if err != nil {
		t.Fatalf("head scan: %v", err)
	}
	if largest > budget {
		t.Errorf("head delivered a %d-byte line against a %d-byte budget", largest, budget)
	}
}

// TestScanSummaryHeadStopsAtALineBoundary is the constraint the fragment drop
// exists to preserve: headEnd is where the tail scan resumes, so a headEnd
// that lands mid-line would make the tail either re-deliver a partial record
// or truncate the next one.
func TestScanSummaryHeadStopsAtALineBoundary(t *testing.T) {
	const budget = 1 << 10
	first := `{"n":0}` + "\n"
	f := writeRaw(t, []byte(first+`{"pad":"`+strings.Repeat("x", 4*budget)+`"}`+"\n"))

	var got []string
	headEnd, complete, err := scanSummaryHead(f, summaryBudget{maxLines: 64, maxBytes: budget, tailBytes: 1 << 10},
		func(data []byte) { got = append(got, string(data)) })
	if err != nil {
		t.Fatalf("head scan: %v", err)
	}
	if complete {
		t.Error("complete = true, want false — the file is larger than the budget")
	}
	if headEnd != int64(len(first)) {
		t.Errorf("headEnd = %d, want %d (the boundary after the only whole line)", headEnd, len(first))
	}
	if len(got) != 1 || got[0] != strings.TrimSuffix(first, "\n") {
		t.Errorf("visited %q, want only the whole first line", got)
	}
}

// TestScanJSONLSummaryKeepsALineThatEndsExactlyAtTheBudget guards the fidelity
// property the budget must not cost: dropping the truncated fragment must not
// drop a *whole* line whose end happens to coincide with the limit. The
// limiter's one byte of headroom is what separates the two cases, so the head
// delivers this line itself rather than leaning on the tail scan to recover
// it — which matters, because the tail window cannot always reach back far
// enough to do so. See TestScanJSONLSummaryKeepsAnExactBudgetLineTooLongForTheTail.
func TestScanJSONLSummaryKeepsALineThatEndsExactlyAtTheBudget(t *testing.T) {
	last := `{"n":1}`
	first := `{"n":0}` + "\n"
	f := writeRaw(t, []byte(first+last))

	got, _, err := collect(f, summaryBudget{
		maxLines:  64,
		maxBytes:  int64(len(first) + len(last)),
		tailBytes: 1 << 10,
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := []string{strings.TrimSuffix(first, "\n"), last}
	if len(got) != len(want) {
		t.Fatalf("visited %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestScanJSONLSummaryTailSurvivesAnOversizedHeadLine is the end-to-end shape
// the budget protects: a session whose opening line blows the byte budget must
// still report its closing timestamp, because the head stopping early is
// exactly what the tail scan exists to compensate for.
func TestScanJSONLSummaryTailSurvivesAnOversizedHeadLine(t *testing.T) {
	const budget = 1 << 10
	last := `{"n":"last"}`
	f := writeRaw(t, []byte(`{"pad":"`+strings.Repeat("x", 4*budget)+`"}`+"\n"+last+"\n"))

	got, complete, err := collect(f, summaryBudget{maxLines: 64, maxBytes: budget, tailBytes: 1 << 10})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if complete {
		t.Error("complete = true, want false")
	}
	if len(got) == 0 || got[len(got)-1] != last {
		t.Errorf("visited %q, want the final line %q delivered", got, last)
	}
}

// TestScanJSONLSummaryKeepsAnExactBudgetLineTooLongForTheTail pins the case the
// tail scan cannot rescue. When a file's real end lands exactly on maxBytes and
// its final line is longer than tailBytes, the tail window starts inside that
// line, drops it as a leading fragment, and the record is gone — taking EndedAt
// with it, silently, on a file that is not malformed at all.
//
// Recovering it is the reason scanSummaryHead reads through a limiter set one
// byte past the budget: without that headroom a genuine EOF at maxBytes is
// byte-for-byte indistinguishable from a line the limiter cut off.
//
// @joestump 08/23/2026 - Added while reviewing #87.
func TestScanJSONLSummaryKeepsAnExactBudgetLineTooLongForTheTail(t *testing.T) {
	const maxBytes, tailBytes = 1024, 64

	first := `{"n":0}` + "\n"
	// A final line with no trailing newline, sized so the file ends exactly on
	// the budget and the line itself overruns the tail window.
	last := `{"pad":"` + strings.Repeat("x", maxBytes-len(first)-len(`{"pad":""}`)) + `"}`
	if len(first)+len(last) != maxBytes {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(first)+len(last), maxBytes)
	}
	if len(last) <= tailBytes {
		t.Fatalf("final line is %d bytes, must exceed tailBytes=%d to exercise this path", len(last), tailBytes)
	}

	f := writeRaw(t, []byte(first+last))
	got, complete, err := collect(f, summaryBudget{maxLines: 64, maxBytes: maxBytes, tailBytes: tailBytes})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !complete {
		t.Errorf("complete = false, want true: the file ended at the budget, it was not truncated")
	}
	want := []string{strings.TrimSuffix(first, "\n"), last}
	if len(got) != len(want) {
		t.Fatalf("visited %d lines, want %d — the final line was dropped as a fragment", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %.40q..., want %.40q...", i, got[i], want[i])
		}
	}
}
