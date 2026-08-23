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
