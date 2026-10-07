package redact

// JSON-Lines Redaction
//
// An agent's structured stream (Claude Code's stream-json, a transcript's
// JSONL) is stored and shown as JSON lines, masked line by line. Masking such
// a line as plain text breaks it in two ways.
//
//   - The line stops being JSON. The rules end a value at a quote, and inside
//     a JSON string a quote is escaped: `Authorization: token abc\"` masks
//     `abc\`, the escape goes with it, and the string ends early.
//   - It is slow. Over a multi-megabyte line Go's regexp runs its NFA, about
//     1 MB/s for the whole rule set (3.5s for a 4 MiB tool result), and the
//     agent blocks on its stdout for all of it.
//
// So JSONLine masks each string literal of the line on its own, decoded: the
// text the agent printed, split at its real newlines and masked a line at a
// time through Lines, which keeps a PEM body masked and skips a line no rule
// can match (prefilter.go). A 4 MiB tool result masks in tens of
// milliseconds this way, base64 images included. A literal
// that carried nothing is copied byte for byte. One that did is re-encoded,
// so the line still parses. A string value under a secret-named key
// ("password", "api_key", "GITEA_TOKEN") is masked whole, which is the
// structural twin of the assignment rule. A line that is not JSON, or does not
// scan as JSON, is masked as text.
//
// @joestump-agent 09/28/2026 - Added to Harness's internal/redact for the
// stream-json pipe spawn (https://github.com/stump-wtf/harness/issues/18).
// @joestump 10/07/2026 - Moved here so Harness can drop its own redactor
// (https://github.com/stump-wtf/harness/issues/912).

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// secretKey matches a JSON object key that names a credential. It uses the
// assignment rule's word list and, like that rule, requires the name to END in
// the word, so "max_tokens" and "input_tokens" are not secrets.
var secretKey = regexp.MustCompile(`(?i)^[a-z0-9_.-]*(?:` + secretWords + `)$`)

// JSONLine returns one line of a JSON-lines stream with every recognised
// credential masked. A JSON line comes back as JSON: byte-identical except in
// the string literals that carried a secret. Any other line is masked as
// Redact would mask it, with the PEM state Lines keeps across lines.
func (l *Lines) JSONLine(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return l.String(line)
	}
	out, ok := maskJSONStrings(line)
	if !ok {
		return l.String(line)
	}
	// A JSON line cannot be the body of a key block a text line opened.
	l.inKey = false
	return out
}

// maskJSONStrings masks every string literal in line, returning false when the
// line does not scan: an unterminated string, or a literal that does not
// decode.
func maskJSONStrings(line string) (string, bool) {
	var b strings.Builder
	copied := 0     // line[:copied] is already in b (or unchanged)
	prev := byte(0) // the last significant byte outside a string
	secret := false // the key before the pending value names a credential
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c != '"' {
			if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
				prev = c
			}
			continue
		}
		end, ok := stringEnd(line, i)
		if !ok {
			return "", false
		}
		lit := line[i : end+1]
		text, ok := decodeString(lit)
		if !ok {
			return "", false
		}
		isKey := nextSignificant(line, end+1) == ':'
		var masked string
		changed := false
		switch {
		case isKey:
			masked, changed = maskText(text)
			secret = secretKey.MatchString(text)
		case prev == ':' && secret && text != "" && text[0] != '$' && text[0] != '[':
			// The value of "password": "…" — masked whole, whatever its shape.
			masked, changed = Mask, true
			secret = false
		default:
			masked, changed = maskText(text)
			secret = false
		}
		if changed {
			b.Grow(len(line))
			b.WriteString(line[copied:i])
			b.WriteString(quoteString(masked))
			copied = end + 1
		}
		prev = '"'
		i = end
	}
	if copied == 0 {
		return line, true
	}
	b.WriteString(line[copied:])
	return b.String(), true
}

// stringEnd returns the index of the quote closing the literal that opens at
// line[start].
func stringEnd(line string, start int) (int, bool) {
	for j := start + 1; j < len(line); j++ {
		switch line[j] {
		case '\\':
			j++
		case '"':
			return j, true
		}
	}
	return 0, false
}

// nextSignificant is the first byte at or after i that is not JSON
// whitespace, or 0.
func nextSignificant(line string, i int) byte {
	for ; i < len(line); i++ {
		switch line[i] {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return line[i]
	}
	return 0
}

// decodeString decodes one JSON string literal, quotes included. A literal
// with no escapes is its own content, which spares the decoder the common
// case.
func decodeString(lit string) (string, bool) {
	body := lit[1 : len(lit)-1]
	if !strings.ContainsRune(body, '\\') && utf8.ValidString(body) {
		return body, true
	}
	var s string
	if err := json.Unmarshal([]byte(lit), &s); err != nil {
		return "", false
	}
	return s, true
}

// maskText masks decoded string text one line at a time, so the rules see the
// lines they were written for and a PEM body inside the string is masked by
// Lines' state. It reports whether anything changed.
func maskText(text string) (string, bool) {
	parts := strings.Split(text, "\n")
	var l Lines
	changed := false
	for i, p := range parts {
		if m := l.String(p); m != p {
			parts[i] = m
			changed = true
		}
	}
	if !changed {
		return text, false
	}
	return strings.Join(parts, "\n"), true
}

// quoteString encodes s as a JSON string literal, leaving <, > and & as they
// are.
func quoteString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}
