package main

import (
	redactpkg "github.com/stump-wtf/agent-trace/redact"

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

// defaultRedact is the shared redactor (github.com/stump-wtf/agent-trace/redact,
// the one redactor Harness, agent-trace and Cairn use per ADR-0033). It is the
// redactor the CLI applies unless --no-redact is given.
func defaultRedact(s string) string {
	return redactpkg.Redact(s)
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
