package classify

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestErrorExcerptUnderBudget(t *testing.T) {
	tests := []struct {
		name    string
		content string
		max     int
		want    string
	}{
		{"exact fit", "undefined: foo", 14, "undefined: foo"},
		{"room to spare", "undefined: foo", 200, "undefined: foo"},
		{"trims surrounding whitespace", "\n\n  401 Unauthorized \t\n", 200, "401 Unauthorized"},
		{"trim happens before the budget check", "   undefined: foo   ", 14, "undefined: foo"},
		{"keeps inner newlines", "Exit code 1\nundefined: foo", 200, "Exit code 1\nundefined: foo"},
		{"empty content", "", 200, ""},
		{"whitespace only", " \n\t ", 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorExcerpt(tt.content, tt.max, nil); got != tt.want {
				t.Errorf("errorExcerpt(%q, %d) = %q, want %q", tt.content, tt.max, got, tt.want)
			}
		})
	}
}

func TestErrorExcerptNonPositiveBudget(t *testing.T) {
	for _, max := range []int{0, -1} {
		if got := errorExcerpt("undefined: foo", max, nil); got != "" {
			t.Errorf("errorExcerpt(_, %d) = %q, want empty", max, got)
		}
	}
}

// TestErrorExcerptKeepsHeadAndTail pins the shape of an over-budget excerpt:
// the error at the top and the verdict at the bottom both survive, the noise
// between them does not, and the elision marker says so.
func TestErrorExcerptKeepsHeadAndTail(t *testing.T) {
	var b strings.Builder
	b.WriteString("./main.go:3:2: undefined: foo\n")
	for range 200 {
		b.WriteString("=== RUN   TestSomethingNoisy\n")
	}
	b.WriteString("FAIL\tgithub.com/example/pkg\t0.012s")
	content := b.String()

	const max = 120
	got := errorExcerpt(content, max, nil)

	if len(got) > max {
		t.Fatalf("len = %d, want <= %d: %q", len(got), max, got)
	}
	if !strings.HasPrefix(got, "./main.go:3:2: undefined: foo") {
		t.Errorf("excerpt lost the leading error: %q", got)
	}
	if !strings.HasSuffix(got, "FAIL\tgithub.com/example/pkg\t0.012s") {
		t.Errorf("excerpt lost the trailing verdict: %q", got)
	}
	if strings.Count(got, ErrorExcerptElision) != 1 {
		t.Errorf("want exactly one elision marker in %q", got)
	}
}

// TestErrorExcerptSnapsToLineBreaks checks that a cut near a line break lands
// on it, so a matcher sees whole lines, and that a cut far from one does not
// throw away most of the budget to find it.
func TestErrorExcerptSnapsToLineBreaks(t *testing.T) {
	lines := strings.Repeat("0123456789\n", 40) // 11-byte lines
	got := errorExcerpt(lines, 60, nil)
	head, tail, ok := strings.Cut(got, ErrorExcerptElision)
	if !ok {
		t.Fatalf("no elision marker in %q", got)
	}
	for _, part := range []string{head, tail} {
		for line := range strings.SplitSeq(part, "\n") {
			if line != "0123456789" {
				t.Errorf("partial line %q in excerpt %q", line, got)
			}
		}
	}

	// One long line: nothing to snap to, so the budget is used in full.
	long := strings.Repeat("x", 500)
	got = errorExcerpt(long, 60, nil)
	if len(got) != 60 {
		t.Errorf("len = %d, want 60 when there is no line break to snap to", len(got))
	}
}

// TestErrorExcerptNeverSplitsRunes walks every budget across text made of
// multi-byte runes, so each cut lands at every offset inside a rune at least
// once. A split rune would show up as invalid UTF-8.
func TestErrorExcerptNeverSplitsRunes(t *testing.T) {
	contents := map[string]string{
		"2-byte": strings.Repeat("é", 60),
		"3-byte": strings.Repeat("日本語", 20),
		"4-byte": strings.Repeat("🔥", 30),
		"mixed":  "错误: 未定义 foo 🔥 " + strings.Repeat("ñ日🔥a", 20) + " FAIL — 失败",
	}
	for name, content := range contents {
		for max := 1; max <= len(content)+1; max++ {
			got := errorExcerpt(content, max, nil)
			if len(got) > max {
				t.Fatalf("%s max=%d: len = %d", name, max, len(got))
			}
			if !utf8.ValidString(got) {
				t.Fatalf("%s max=%d: invalid UTF-8 %q", name, max, got)
			}
		}
	}
}

func TestErrorExcerptTinyBudgetKeepsHeadOnly(t *testing.T) {
	content := "undefined: foo and a great deal more text after it"
	for max := 1; max <= len(ErrorExcerptElision)+1; max++ {
		got := errorExcerpt(content, max, nil)
		if len(got) > max {
			t.Errorf("max=%d: len = %d", max, len(got))
		}
		if !strings.HasPrefix(content, got) {
			t.Errorf("max=%d: %q is not a head of the content", max, got)
		}
	}
}

func TestErrorExcerptStripsANSI(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"SGR colour", "\x1b[31;1merror:\x1b[0m undefined: foo", "error: undefined: foo"},
		{"cursor movement", "\x1b[2K\x1b[1Gdownloading\x1b[?25l", "downloading"},
		{"OSC 8 hyperlink with BEL", "see \x1b]8;;https://example.com\x07docs\x1b]8;;\x07", "see docs"},
		{"OSC title with ST", "\x1b]0;my title\x1b\\401 Unauthorized", "401 Unauthorized"},
		{"two-byte escape", "\x1bMFAIL", "FAIL"},
		{"lone escape", "FAIL\x1b", "FAIL"},
		{"unterminated OSC keeps the text after it", "\x1b]broken title FAIL", "broken title FAIL"},
		{"escapes around the trimmed edge", "\x1b[0m  undefined: foo  \x1b[0m", "undefined: foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorExcerpt(tt.content, 200, nil); got != tt.want {
				t.Errorf("errorExcerpt(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

// TestErrorExcerptStripsANSIBeforeTheBudget checks escapes do not eat into the
// budget: a coloured line that fits once stripped is kept whole.
func TestErrorExcerptStripsANSIBeforeTheBudget(t *testing.T) {
	content := "\x1b[31m" + "undefined: foo" + "\x1b[0m"
	if got := errorExcerpt(content, len("undefined: foo"), nil); got != "undefined: foo" {
		t.Errorf("got %q, want the whole stripped line", got)
	}
}

func TestErrorExcerptReplacesInvalidUTF8(t *testing.T) {
	got := errorExcerpt("bad \xff\xfe byte", 200, nil)
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8 survived: %q", got)
	}
	if got != "bad \uFFFD byte" {
		t.Errorf("got %q", got)
	}
}

// TestErrorExcerptRedactsBeforeTheCut places a secret across the elision
// point. Redacting the cut excerpt would hand the redactor half a token it
// cannot recognise; redacting first removes all of it.
func TestErrorExcerptRedactsBeforeTheCut(t *testing.T) {
	const secret = "sk-live-0123456789abcdefghijklmnopqrstuvwxyz"
	// A 60-byte budget keeps a 28-byte head, which ends 18 bytes into the
	// secret.
	content := strings.Repeat("a", 10) + secret + strings.Repeat("b", 70)
	redact := func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") }

	got := errorExcerpt(content, 60, redact)
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("want the redaction marker where the secret was: %q", got)
	}
	if strings.Contains(got, "sk-live") || strings.Contains(got, "0123456789") || strings.Contains(got, "uvwxyz") {
		t.Errorf("part of the secret survived redaction: %q", got)
	}
	if len(got) > 60 {
		t.Errorf("len = %d, want <= 60", len(got))
	}
}

// TestErrorExcerptBudgetHoldsWhenRedactLengthens guards the other half of
// the ordering: a redactor that grows the text is still held to the budget.
func TestErrorExcerptBudgetHoldsWhenRedactLengthens(t *testing.T) {
	redact := func(s string) string { return strings.ReplaceAll(s, "k", "[REDACTED-KEY]") }
	got := errorExcerpt(strings.Repeat("k", 30), 40, redact)
	if len(got) > 40 {
		t.Errorf("len = %d, want <= 40: %q", len(got), got)
	}
}

func TestErrorExcerptRedactSeesCleanedText(t *testing.T) {
	var seen string
	redact := func(s string) string { seen = s; return s }
	errorExcerpt("\x1b[31mtoken=abc\x1b[0m", 200, redact)
	if seen != "token=abc" {
		t.Errorf("redact saw %q, want the ANSI-stripped text", seen)
	}
}

func TestBuildEventWithErrorExcerpt(t *testing.T) {
	call := ToolCall{Name: "Bash", Input: map[string]any{"command": "go build ./..."}}
	failed := ToolResult{Content: "\x1b[31m./main.go:3:2: undefined: foo\x1b[0m\n", IsError: true}

	ev := BuildEventWith(&Options{ErrorExcerptBytes: 200}, 0, "/repo", call, failed)
	if ev.ErrorExcerpt != "./main.go:3:2: undefined: foo" {
		t.Errorf("ErrorExcerpt = %q", ev.ErrorExcerpt)
	}
	if ev.ResultBytes != len(failed.Content) {
		t.Errorf("ResultBytes = %d, want the raw content length %d", ev.ResultBytes, len(failed.Content))
	}

	ok := ToolResult{Content: "undefined: foo", IsError: false}
	if ev := BuildEventWith(&Options{ErrorExcerptBytes: 200}, 0, "/repo", call, ok); ev.ErrorExcerpt != "" {
		t.Errorf("a result with IsError false carried an excerpt: %q", ev.ErrorExcerpt)
	}
}

// TestBuildEventWithRedactsBeforeStorage checks the stored excerpt is the
// redacted one — the raw text never reaches the Event.
func TestBuildEventWithRedactsBeforeStorage(t *testing.T) {
	call := ToolCall{Name: "Bash", Input: map[string]any{"command": "gh api user"}}
	result := ToolResult{Content: "401 Unauthorized: token ghp_abc123 rejected", IsError: true}
	opts := &Options{
		ErrorExcerptBytes: 200,
		Redact:            func(s string) string { return strings.ReplaceAll(s, "ghp_abc123", "[REDACTED]") },
	}
	ev := BuildEventWith(opts, 0, "/repo", call, result)
	if ev.ErrorExcerpt != "401 Unauthorized: token [REDACTED] rejected" {
		t.Errorf("ErrorExcerpt = %q", ev.ErrorExcerpt)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ghp_abc123") {
		t.Errorf("serialized Event holds the raw secret: %s", raw)
	}
}

// TestErrorExcerptOffByDefault is the no-change promise to existing callers:
// without ErrorExcerptBytes an errored result carries no text, Redact is never
// called, and the serialized Event has no new key.
func TestErrorExcerptOffByDefault(t *testing.T) {
	call := ToolCall{Name: "Bash", Input: map[string]any{"command": "go test ./..."}}
	result := ToolResult{Content: "FAIL\tgithub.com/example/pkg", IsError: true}

	calls := 0
	redactOnly := &Options{Redact: func(s string) string { calls++; return s }}

	for name, ev := range map[string]Event{
		"BuildEvent":           BuildEvent(0, "/repo", call, result),
		"BuildEventWith nil":   BuildEventWith(nil, 0, "/repo", call, result),
		"zero Options":         BuildEventWith(&Options{}, 0, "/repo", call, result),
		"Redact without bytes": BuildEventWith(redactOnly, 0, "/repo", call, result),
	} {
		if ev.ErrorExcerpt != "" {
			t.Errorf("%s: ErrorExcerpt = %q, want empty", name, ev.ErrorExcerpt)
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "errorExcerpt") {
			t.Errorf("%s: serialized Event gained an errorExcerpt key: %s", name, raw)
		}
	}
	if calls != 0 {
		t.Errorf("Redact called %d times with ErrorExcerptBytes zero", calls)
	}
}
