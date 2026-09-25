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
// @joestump-agent 09/25/2026 - Added for #133. Reading a structured stream
// from stdin ("-") waits on the io.Reader API in #132.
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
<database>/<session-id> for crush and opencode. "-" (standard input) is
reserved for reading a structured stream and is not supported yet.

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

// errStdinUnsupported is what reading a stream from standard input returns
// until the io.Reader API (#132) lands. It is a sentinel rather than a usage
// error so the stdin branch in load is the one place #132 plugs into.
var errStdinUnsupported = errors.New(`reading a stream from standard input ("-") is not supported yet (needs agent-trace#132); pass a transcript path`)

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
		// #132 plugs in here: an adapter with a stream format reads stdin
		// and returns the same events, marks and meta Parse does.
		_ = stdin
		return session{}, errStdinUnsupported
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

// record is one line of normalize output. Exactly one of Session, Event and
// Mark is set, named by Kind, so a consumer switches on kind without probing
// fields. A stream's final result (#132) becomes a fourth kind.
type record struct {
	Kind    string            `json:"kind"`
	Session *tail.SessionMeta `json:"session,omitempty"`
	Event   *classify.Event   `json:"event,omitempty"`
	Mark    *classify.Mark    `json:"mark,omitempty"`
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
