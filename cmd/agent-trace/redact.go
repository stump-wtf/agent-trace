package main

import (
	"regexp"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"
)

// Redaction
//
// The CLI's output leaves the machine: it is piped into other tools, pasted
// into issues and posted to Cairn. Every free-text field it writes can carry
// what an agent typed or saw — a token on a command line, a password in a
// user message, an Authorization header echoed in an error. So redaction is
// on by default and --no-redact is the explicit opt-out.
//
// The redactor is the same hook the library already takes
// (classify.Options.Redact): the CLI hands it to the adapters, so an error
// excerpt is redacted whole, before it is cut. The fields the library does not
// run it over — Event.Summary, Mark.Note and SessionMeta.Title — are redacted
// here after parsing, before anything is written. otel output is built from
// the redacted values, so a span name or status message never sees the raw
// text either.
//
// It is pattern-based and best-effort. A summary is truncated by classify
// before the CLI sees it, so a token cut short by that truncation can fall
// below a pattern's minimum length and survive in part. Paths (Targets,
// Outside, SessionMeta.Path and Cwd) are left alone: they are what the
// normalized record is for.
//
// @joestump-agent 09/25/2026 - Added with the agent-trace CLI (#133).

// redacted replaces every secret the redactor finds.
const redacted = "[REDACTED]"

// credentialLabel opens capture group 1 on a key that names a credential and
// its separator, up to where the value starts; each pattern using it closes
// the group itself. The keyword must end the key or be followed by a
// separator, so max_tokens and input_tokens — counts, not credentials — are
// left alone.
const credentialLabel = `(?i)(\b[A-Za-z0-9_.-]*(?:token|secret|passw(?:or)?d|pwd|api[_-]?key|access[_-]?key|private[_-]?key|credentials?)(?:[_.-][A-Za-z0-9_.-]*)?["']?\s*[:=]\s*`

// secretPatterns are applied in order. Each replaces its match with repl, a
// regexp.Expand template: a pattern that names a credential by its label —
// "Bearer ", "password=" — keeps the label in ${1}, so the output still says
// what kind of value was removed.
var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// A PEM private key block, header to footer.
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), redacted},
	// An Authorization header, whatever its scheme, bare or as a quoted JSON
	// or dict value.
	{regexp.MustCompile(`(?i)(\bauthorization["']?\s*[:=]\s*["']?(?:(?:bearer|basic|token)\s+)?)[^\s"',;]+`), "${1}" + redacted},
	// A bearer credential outside a header.
	{regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]{16,}`), "${1}" + redacted},
	// The password in URL userinfo: scheme://user:password@host.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s/:@]+:)[^\s/@]+@`), "${1}" + redacted + "@"},
	// key=value and key: value where the key names a credential, quoted
	// values first so the quotes survive around the marker.
	{regexp.MustCompile(credentialLabel + `")[^"\n]*"`), "${1}" + redacted + `"`},
	{regexp.MustCompile(credentialLabel + `')[^'\n]*'`), "${1}" + redacted + `'`},
	{regexp.MustCompile(credentialLabel + `)[^\s"',;&|)]+`), "${1}" + redacted},
	// A command-line flag naming a credential, with its value as the next
	// argument: --password hunter2, --api-key "abc". The flag name must end
	// on the keyword, so --token-file and --max-tokens are left alone.
	{regexp.MustCompile(`(?i)((?:^|\s)--?[A-Za-z0-9-]*(?:token|secret|passw(?:or)?d|api[_-]?key|access[_-]?key|private[_-]?key)\s+["']?)[^\s"',;&|)]+`), "${1}" + redacted},
	// curl's user:password credential: curl -u user:pass, --user user:pass.
	{regexp.MustCompile(`(\bcurl\b[^\n]*?\s(?:-u\s*|--user[=\s]\s*)["']?[^\s:"']+:)[^\s"']+`), "${1}" + redacted},
	// Well-known token shapes, wherever they appear.
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{8,}|github_pat_[A-Za-z0-9_]{8,})`), redacted},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`), redacted},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{8,}`), redacted},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), redacted},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), redacted},
}

// defaultRedact removes the credentials secretPatterns recognise from s.
// It is the redactor the CLI applies unless --no-redact is given.
func defaultRedact(s string) string {
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// redactAll runs redact over every free-text field of a parsed session that
// the adapters did not already redact: each event's Summary, each mark's Note
// and the session's Title. ErrorExcerpt is redacted again too, which is a
// no-op for text the adapter's hook already cleaned and keeps the guarantee
// whole if an adapter ever fills it some other way. It rewrites the slices in
// place.
func redactAll(redact func(string) string, meta *tail.SessionMeta, events []classify.Event, marks []classify.Mark) {
	meta.Title = redact(meta.Title)
	for i := range events {
		events[i].Summary = redact(events[i].Summary)
		events[i].ErrorExcerpt = redact(events[i].ErrorExcerpt)
	}
	for i := range marks {
		marks[i].Note = redact(marks[i].Note)
	}
}
