package classify

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Error Excerpts
//
// An Event keeps a tool result's size and error flag but none of its text, so
// the literal message an agent hit ("undefined: foo", "401 Unauthorized") never
// survives parsing. Options.ErrorExcerptBytes opts in to keeping a bounded
// piece of an errored result's text on Event.ErrorExcerpt. It is off by
// default because tool output can carry secrets and holding it costs memory.
//
// The excerpt keeps the head and the tail of the text around ErrorExcerptElision:
// compilers and HTTP clients print the error first, test runners print the
// verdict ("FAIL pkg") last, and the middle is usually the noise between them.
// A cut snaps to a line break when one sits in the quarter of the budget
// nearest the cut, so a matcher sees whole lines rather than a fragment.
//
// Options.Redact runs over the whole cleaned text before it is cut. Redacting
// after the cut would hand the redactor half a token at the elision point that
// its pattern no longer matches, and a redactor that lengthens its input could
// push the result past the budget.
//
// @joestump-agent 09/22/2026 - Added for Harness skill distillation, which
// matches skills to the error text agents hit.

// ErrorExcerptElision joins the head and the tail of an ErrorExcerpt whose
// source text did not fit the budget. Its presence is how a consumer tells a
// cut excerpt from a whole one.
const ErrorExcerptElision = "\n…\n"

// ansiEscapeRe matches the terminal escape sequences tools leave in their
// output: CSI sequences (colours, cursor movement), OSC sequences terminated
// by BEL or ST (window titles, hyperlinks), and two-byte escapes. An OSC with
// no terminator falls through to the two-byte arm, which removes only "ESC ]"
// rather than swallowing the text after it.
var ansiEscapeRe = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

// errorExcerpt shapes content for Event.ErrorExcerpt: escape sequences
// stripped, invalid UTF-8 replaced, redact applied, surrounding whitespace
// trimmed, and the result held to at most maxBytes bytes without splitting a
// rune. It returns "" when maxBytes is not positive.
func errorExcerpt(content string, maxBytes int, redact func(string) string) string {
	if maxBytes <= 0 {
		return ""
	}
	s := ansiEscapeRe.ReplaceAllString(content, "")
	s = strings.ReplaceAll(s, "\x1b", "")
	s = strings.ToValidUTF8(s, "\uFFFD")
	if redact != nil {
		s = redact(s)
	}
	s = strings.TrimSpace(s)
	if len(s) <= maxBytes {
		return s
	}
	// Too small to hold the marker and anything either side of it: keep the
	// head alone.
	if maxBytes <= len(ErrorExcerptElision)+1 {
		return strings.TrimSpace(s[:runeFloor(s, maxBytes)])
	}

	avail := maxBytes - len(ErrorExcerptElision)
	headBudget := (avail + 1) / 2
	tailBudget := avail - headBudget

	headEnd := runeFloor(s, headBudget)
	if nl := strings.LastIndexByte(s[:headEnd], '\n'); nl > 0 && nl >= headEnd-headBudget/4 {
		headEnd = nl
	}
	tailStart := runeCeil(s, len(s)-tailBudget)
	if nl := strings.IndexByte(s[tailStart:], '\n'); nl >= 0 && nl < tailBudget/4 {
		tailStart += nl + 1
	}

	head := strings.TrimRightFunc(s[:headEnd], unicode.IsSpace)
	tail := strings.TrimLeftFunc(s[tailStart:], unicode.IsSpace)
	return head + ErrorExcerptElision + tail
}

// runeFloor returns the largest index <= i that starts a rune in s, so s[:i]
// never ends inside a multi-byte sequence.
func runeFloor(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// runeCeil returns the smallest index >= i that starts a rune in s, so s[i:]
// never begins inside a multi-byte sequence.
func runeCeil(s string, i int) int {
	if i <= 0 {
		return 0
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
