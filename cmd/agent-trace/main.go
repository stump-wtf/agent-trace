// Command agent-trace runs one agent transcript through the same
// normalization Harness uses — tail's adapters, classify, and otel — and
// writes the result to stdout, so a shell pipeline, a CI job or a person
// debugging a run by hand gets the records Harness would have.
//
//	agent-trace normalize --harness <name> <transcript>   # events and marks as JSONL
//	agent-trace otel      --harness <name> <transcript>   # otel.BuildTrace JSON
//
// Output is redacted by default; see redact.go.
//
// @joestump-agent 09/25/2026 - Added for #133. The stdin ("-") path reads a
// structured stream through tail.StreamParser (#132).
// @joestump-agent 09/27/2026 - Wired stdin to ParseStream now that #132 has
// landed, and switched the default redactor to the shared redact package
// (#134).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/otel"
	"github.com/stump-wtf/agent-trace/tail"
)

// Exit codes. A usage error is the caller's to fix before retrying; a runtime
// error (an unreadable transcript, a failed write) may not be.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usageText = `agent-trace normalizes an agent session transcript.

Usage:
  agent-trace normalize --harness <name> [flags] <transcript>
  agent-trace otel      --harness <name> [flags] <transcript>
  agent-trace help

Commands:
  normalize  write the session, its events and its marks as JSONL, one
             {"kind": "session"|"event"|"mark", ...} record per line
  otel       write the session as one otel.BuildTrace JSON object

Harnesses:
  ` + "%s" + `

The transcript is a session file for the JSONL harnesses, and
<database>/<session-id> for crush and opencode. "-" reads a structured
stream from standard input (claude-code's stream-json), for piping the
agent's stdout straight in: agent-trace normalize --harness claude-code -

Flags:
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is main without the process: it takes the arguments after the program
// name and returns the exit code, so tests drive the whole CLI in-process.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage(nil))
		return exitUsage
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		_, _ = fmt.Fprint(stdout, usage(nil))
		return exitOK
	case "normalize", "otel":
	default:
		_, _ = fmt.Fprintf(stderr, "agent-trace: unknown command %q\n\n%s", args[0], usage(nil))
		return exitUsage
	}

	cmd := args[0]
	var cfg config
	fs := newFlagSet(cmd, stderr, &cfg)
	positional, err := parseInterspersed(fs, args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		return exitUsage
	}
	if cfg.harness == "" {
		_, _ = fmt.Fprintf(stderr, "agent-trace %s: --harness is required (one of %s)\n", cmd, harnessList())
		return exitUsage
	}
	if len(positional) > 1 {
		_, _ = fmt.Fprintf(stderr, "agent-trace %s: want one transcript, got %d\n", cmd, len(positional))
		return exitUsage
	}
	if cfg.excerptBytes < 0 {
		_, _ = fmt.Fprintf(stderr, "agent-trace %s: --error-excerpt-bytes must not be negative\n", cmd)
		return exitUsage
	}
	src := "-"
	if len(positional) == 1 {
		src = positional[0]
	}

	s, err := load(ctx, cfg, src, stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agent-trace %s: %v\n", cmd, err)
		if errors.Is(err, errUnknownHarness) {
			return exitUsage
		}
		return exitError
	}

	switch cmd {
	case "normalize":
		err = writeNormalized(stdout, s)
	case "otel":
		err = writeTrace(stdout, s)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agent-trace %s: %v\n", cmd, err)
		return exitError
	}
	return exitOK
}

// config is what a subcommand's flags set.
type config struct {
	harness      string
	noRedact     bool
	excerptBytes int
}

func newFlagSet(name string, out io.Writer, cfg *config) *flag.FlagSet {
	fs := flag.NewFlagSet("agent-trace "+name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&cfg.harness, "harness", "", "the harness that wrote the transcript (required)")
	fs.BoolVar(&cfg.noRedact, "no-redact", false, "write summaries, notes, titles and error excerpts without redacting credentials")
	fs.IntVar(&cfg.excerptBytes, "error-excerpt-bytes", 0, "keep up to this many bytes of each errored result's text on the event (0 keeps none)")
	fs.Usage = func() { _, _ = fmt.Fprint(out, usage(fs)) }
	return fs
}

func usage(fs *flag.FlagSet) string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, usageText, harnessList())
	if fs == nil {
		fs = newFlagSet("", io.Discard, &config{})
	}
	fs.SetOutput(&b)
	fs.PrintDefaults()
	return b.String()
}

// parseInterspersed parses flags wherever they sit among the arguments, so
// "normalize session.jsonl --harness codex" works as well as the flags-first
// form the standard flag package stops at. A bare "--" ends flag parsing.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		// flag stops at the first non-flag, and consumes a "--" it stops at.
		if len(args) > len(rest) && args[len(args)-len(rest)-1] == "--" {
			return append(positional, rest...), nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// errUnknownHarness reports a --harness value no adapter answers to.
var errUnknownHarness = errors.New("unknown harness")

// adapters maps each --harness name to a constructor for its adapter. A fresh
// adapter per call, because SetOptions mutates it.
var adapters = map[tail.Harness]func() tail.Adapter{
	tail.HarnessClaudeCode: func() tail.Adapter { return &tail.ClaudeCodeAdapter{} },
	tail.HarnessCodex:      func() tail.Adapter { return &tail.CodexAdapter{} },
	tail.HarnessCrush:      func() tail.Adapter { return &tail.CrushAdapter{} },
	tail.HarnessOpenCode:   func() tail.Adapter { return &tail.OpenCodeAdapter{} },
	tail.HarnessPi:         func() tail.Adapter { return &tail.PiAdapter{} },
	tail.HarnessOMP:        func() tail.Adapter { return &tail.PiAdapter{OMP: true} },
}

func harnessList() string {
	names := make([]string, 0, len(adapters))
	for h := range adapters {
		names = append(names, string(h))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// session is one parsed, redacted transcript.
type session struct {
	meta   tail.SessionMeta
	events []classify.Event
	marks  []classify.Mark
	// result is the stream's final result; nil for a transcript read, and
	// for a stream that ended without one (a killed process).
	result *tail.StreamResult
}

// load parses src with the adapter for cfg.harness and applies redaction
// unless cfg.noRedact is set.
func load(ctx context.Context, cfg config, src string, stdin io.Reader) (session, error) {
	newAdapter, ok := adapters[tail.Harness(cfg.harness)]
	if !ok {
		return session{}, fmt.Errorf("%w %q (one of %s)", errUnknownHarness, cfg.harness, harnessList())
	}
	a := newAdapter()

	redact := defaultRedact
	if cfg.noRedact {
		redact = nil
	}
	if cfg.excerptBytes > 0 {
		// Only when excerpts are asked for: SetOptions replaces the
		// filesystem-backed Options an adapter otherwise builds for itself,
		// so the replacement has to carry the same FileExists, HomeDir and
		// TmpDir or weak-target filtering and home/tmp scoping would change.
		if setter, ok := a.(tail.OptionsSetter); ok {
			opts := fsClassifyOptions()
			opts.ErrorExcerptBytes = cfg.excerptBytes
			opts.Redact = redact
			setter.SetOptions(opts)
		}
	}

	if src == "-" {
		sp, ok := a.(tail.StreamParser)
		if !ok {
			return session{}, fmt.Errorf("harness %q has no structured stream format to read from standard input; pass a transcript path", cfg.harness)
		}
		return loadStream(ctx, sp, stdin, redact)
	}

	events, marks, meta, err := a.Parse(ctx, src)
	if err != nil {
		return session{}, err
	}
	// writeNormalized merges the two by seq, which needs each in seq order.
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	sort.SliceStable(marks, func(i, j int) bool { return marks[i].Seq < marks[j].Seq })
	if redact != nil {
		redactAll(redact, &meta, events, marks)
	}
	return session{meta: meta, events: events, marks: marks}, nil
}

// loadStream reads a structured stream with sp's ParseStream, redacting as
// the records arrive. The stream is the run as it happens, so nothing waits
// for EOF: each event and mark is redacted and collected in the handler, and
// the final metadata and Result arrive with ParseStream's return.
func loadStream(ctx context.Context, sp tail.StreamParser, r io.Reader, redact func(string) string) (session, error) {
	var s session
	h := tail.StreamHandler{
		Event: func(e classify.Event) {
			if redact != nil {
				e.Summary = redact(e.Summary)
				e.ErrorExcerpt = redact(e.ErrorExcerpt)
			}
			s.events = append(s.events, e)
		},
		Mark: func(m classify.Mark) {
			if redact != nil {
				m.Note = redact(m.Note)
			}
			s.marks = append(s.marks, m)
		},
	}
	meta, result, err := sp.ParseStream(ctx, r, h)
	if err != nil {
		return session{}, err
	}
	if redact != nil {
		meta.Title = redact(meta.Title)
	}
	s.meta = meta
	// A stream arrives in order; ParseStream hands events and marks out in
	// stream order, which is seq order, so there is nothing to sort.
	s.result = result
	return s, nil
}

// fsClassifyOptions mirrors the Options tail's adapters build for themselves
// when none are injected: an os.Stat-backed FileExists and the real home and
// temp directories.
func fsClassifyOptions() *classify.Options {
	home, _ := os.UserHomeDir()
	return &classify.Options{
		FileExists: func(cwd, rel string) bool {
			if cwd == "" || rel == "" {
				return false
			}
			_, err := os.Stat(filepath.Join(cwd, filepath.FromSlash(rel)))
			return err == nil
		},
		HomeDir: home,
		TmpDir:  os.TempDir(),
	}
}

// record is one line of normalize output. Exactly one of Session, Event,
// Mark and Result is set, named by Kind, so a consumer switches on kind
// without probing fields. Result only ever follows the records it belongs
// to: it is a stream's final summary, and never written for a transcript.
type record struct {
	Kind    string             `json:"kind"`
	Session *tail.SessionMeta  `json:"session,omitempty"`
	Event   *classify.Event    `json:"event,omitempty"`
	Mark    *classify.Mark     `json:"mark,omitempty"`
	Result  *tail.StreamResult `json:"result,omitempty"`
}

// writeNormalized writes the session record, then events and marks merged in
// seq order — a mark before an event that shares its seq, the order
// otel.BuildTrace reads them in, since a mark opens the turn its event
// belongs to.
func writeNormalized(w io.Writer, s session) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(record{Kind: "session", Session: &s.meta}); err != nil {
		return err
	}
	ei, mi := 0, 0
	for ei < len(s.events) || mi < len(s.marks) {
		var rec record
		if mi < len(s.marks) && (ei == len(s.events) || s.marks[mi].Seq <= s.events[ei].Seq) {
			rec = record{Kind: "mark", Mark: &s.marks[mi]}
			mi++
		} else {
			rec = record{Kind: "event", Event: &s.events[ei]}
			ei++
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	if s.result != nil {
		if err := enc.Encode(record{Kind: "result", Result: s.result}); err != nil {
			return err
		}
	}
	return nil
}

// writeTrace writes otel.BuildTrace's JSON for the session, newline
// terminated.
func writeTrace(w io.Writer, s session) error {
	if err := otel.WriteJSON(w, otel.BuildTrace(s.meta, s.events, s.marks)); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
