package tail

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
)

// fixtureLines reads a testdata JSONL fixture as its records, each keeping its
// newline: a record without one is incomplete, and ParseSince rightly leaves
// it for the next poll.
func fixtureLines(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	return lines[:len(lines)-1] // the empty string after the last newline
}

// pollJSONLGrowth writes lines to a fresh session file one at a time, the way
// a live harness appends, and reads it with ParseItemsSince after every
// write, carrying the watermark and next seq forward as the watcher does. It
// returns everything the polls delivered, concatenated, and the file's path.
//
// Polling after every record puts a watermark at every record boundary the
// adapter will accept, which is where a delta could be lost or repeated.
func pollJSONLGrowth(t *testing.T, a Adapter, lines []string) (Items, string) {
	t.Helper()
	path := writeTempJSONL(t, "s.jsonl", lines[0])
	var got Items
	var wm int64
	for i := range lines {
		if i > 0 {
			appendLines(t, path, lines[i])
		}
		items, _, next, err := ParseItemsSince(t.Context(), a, path, wm, len(got.Events))
		if err != nil {
			t.Fatalf("poll %d: ParseItemsSince: %v", i, err)
		}
		got.Events = append(got.Events, items.Events...)
		got.Marks = append(got.Marks, items.Marks...)
		got.Usage = append(got.Usage, items.Usage...)
		wm = next
	}
	return got, path
}

// assertUsageEqual compares two usage streams item by item.
func assertUsageEqual(t *testing.T, got, want []classify.Usage) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d usage items, want %d:\ngot  %+v\nwant %+v", len(got), len(want), got, want)
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("usage %d:\ngot  %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

// assertParseDropsOnlyUsage checks the contract ItemParser documents: Parse
// is ParseItems with the usage dropped, nothing else changed.
func assertParseDropsOnlyUsage(t *testing.T, a Adapter, path string) {
	t.Helper()
	items, itemsMeta, err := ParseItems(t.Context(), a, path)
	if err != nil {
		t.Fatalf("ParseItems: %v", err)
	}
	events, marks, meta, err := a.Parse(t.Context(), path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(events, items.Events) {
		t.Errorf("Parse events differ from ParseItems events")
	}
	if !reflect.DeepEqual(marks, items.Marks) {
		t.Errorf("Parse marks differ from ParseItems marks")
	}
	if !reflect.DeepEqual(meta, itemsMeta) {
		t.Errorf("Parse meta %+v differs from ParseItems meta %+v", meta, itemsMeta)
	}
}

// TestUsageReadersImplementItemParser keeps the usage readers from silently
// dropping out: ItemParser is optional, so an adapter that stopped
// implementing it would keep compiling and quietly report no usage.
func TestUsageReadersImplementItemParser(t *testing.T) {
	for _, a := range []Adapter{&ClaudeCodeAdapter{}, &CodexAdapter{}, &CrushAdapter{}} {
		if _, ok := a.(ItemParser); !ok {
			t.Errorf("%s adapter does not implement ItemParser", a.Harness())
		}
	}
}

// TestParseItemsFallsBackToParse: an adapter without usage support is still
// readable through the package-level helpers, with no usage items.
func TestParseItemsFallsBackToParse(t *testing.T) {
	path := writeTempJSONL(t, "s.jsonl",
		piHeaderLine("s1", "2026-01-01T10:00:00Z")+
			piEntryLine("e1", "", `{"role":"user","content":[{"type":"text","text":"hello"}]}`, "2026-01-01T10:00:01Z"))
	a := &PiAdapter{}
	if _, ok := any(a).(ItemParser); ok {
		t.Skip("the Pi adapter reads usage now; pick an adapter that does not")
	}
	items, _, err := ParseItems(t.Context(), a, path)
	if err != nil {
		t.Fatalf("ParseItems: %v", err)
	}
	if len(items.Marks) != 1 || items.Usage != nil {
		t.Errorf("ParseItems = %d marks, usage %v; want 1 mark and no usage", len(items.Marks), items.Usage)
	}
	items, _, _, err = ParseItemsSince(t.Context(), a, path, 0, 0)
	if err != nil {
		t.Fatalf("ParseItemsSince: %v", err)
	}
	if len(items.Marks) != 1 || items.Usage != nil {
		t.Errorf("ParseItemsSince = %d marks, usage %v; want 1 mark and no usage", len(items.Marks), items.Usage)
	}
}

// TestParseItemsSinceRejectsNonIncrementalAdapter: the helper reports an
// adapter it cannot read incrementally instead of returning nothing.
func TestParseItemsSinceRejectsNonIncrementalAdapter(t *testing.T) {
	if _, _, _, err := ParseItemsSince(t.Context(), staticAdapter{}, "x", 0, 0); err == nil {
		t.Error("ParseItemsSince on a non-incremental adapter returned no error")
	}
}
