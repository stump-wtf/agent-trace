# agent-trace

Agent session trace libraries extracted from [cosmtrek/mindwalk](https://github.com/cosmtrek/mindwalk). Classify tool actions, tail live session logs, and emit OpenTelemetry spans. Used by [Harness](https://gitea.stump.rocks/stump.wtf/harness) for idle detection and trajectory export to [Cairn](https://gitea.stump.rocks/stump.wtf/cairn).

## Scope

This module covers mindwalk's trace parsing, classification, and event/mark emission only. **Not extracted**: stats computation (`internal/model/stats.go`), the agent-graph builder (`internal/model/agent.go`), judge integration (`internal/judge/`), and the 3D citymap renderer.

## Packages

### `classify`

Pure classification of agent tool calls into semantic actions (`search`, `read`, `edit`, `exec`, `verify`) and file targets. No I/O — the `Options` struct lets callers inject a `FileExists` func and home/tmp dirs for weak-target filtering and outside-scope detection, `VerifyPatterns` to extend the built-in verify command list (`just test`, `bun test`, …), and `ErrorExcerptBytes` / `Redact` to keep the text of failed tool calls. Pass nil Options to keep all weak targets, the default verify patterns, and no result text.

```go
import "github.com/stump-wtf/agent-trace/classify"

event := classify.BuildEvent(seq, cwd, call, result)
// event.Action == "edit", event.Targets == [{Path: "foo.go", Touch: "edit"}]
```

For I/O-aware classification:

```go
opts := &classify.Options{
    FileExists: func(cwd, rel string) bool { _, err := os.Stat(filepath.Join(cwd, rel)); return err == nil },
    HomeDir:    home,
    TmpDir:     os.TempDir(),
}
event := classify.BuildEventWith(opts, seq, cwd, call, result)
```

An `Event` keeps a result's size and error flag but not its text. To keep the literal error an agent hit — `undefined: foo`, `401 Unauthorized` — opt in with `ErrorExcerptBytes`:

```go
opts := &classify.Options{
    ErrorExcerptBytes: 512,
    Redact:            redactSecrets, // optional func(string) string
}
event := classify.BuildEventWith(opts, seq, cwd, call, result)
// event.ErrorExcerpt == "./main.go:3:2: undefined: foo" when result.IsError
```

- **Off by default.** Zero keeps nothing, and a result with `IsError` false never carries an excerpt. Tool output can hold secrets and costs memory to keep, so nothing changes until you ask for it; the `errorExcerpt` JSON key is omitted when empty.
- **Shape.** Terminal escape sequences are stripped, surrounding whitespace is trimmed, and the excerpt is at most `ErrorExcerptBytes` bytes without splitting a UTF-8 rune. Longer text keeps its head and tail joined by `classify.ErrorExcerptElision`, because errors tend to lead and verdicts like `FAIL pkg` tend to close; each cut snaps to a nearby line break.
- **`Redact` runs before anything is stored**, over the whole cleaned text rather than the cut excerpt, so a secret straddling the cut is still whole when your redactor sees it, and a redactor that lengthens its input cannot push the excerpt past the budget.

In `tail`, set the same two fields on `WatchConfig` and the watcher hands them to every adapter. An excerpt only appears where the adapter knows the call failed: Claude Code and Pi record their own flag, Codex's is inferred from the exit code in the output, OpenCode's comes from a part in the `error` state, and Crush's from the `tool_result` part's `is_error`. Crush records a shell command that exits non-zero as an ordinary result whose text ends `Exit code N`, and the adapter keeps `is_error` alone rather than guessing from that text. OpenCode is narrower for a different reason: the adapter reads only a part's `state.error`, so a failed shell command — which lands in `state.output` under a `completed` status — carries no excerpt either.

An `Event` also keeps no arguments, and for a tool the classifier does not know — any MCP tool — `Summary` is only the tool name. `Event.InputDigest` (`inputDigest` in JSON) is the hex SHA-256 of the call's input encoded as JSON with sorted keys, so two calls with the same arguments share a digest whatever order the transcript stored them in, and a consumer can tell an agent repeating one call from an agent working through a list. It is always filled, it does not include the tool name (pair it with `Event.Tool`), and it is a fingerprint rather than a redaction: an input small enough to guess can be confirmed by hashing the guess.

### `tail`

Live session log discovery and per-agent JSONL parsing. Watches agent session directories, tails growing files, and emits classified `Event`s. Supports Claude Code, Codex, Crush, OpenCode, and Pi via the `Adapter` interface — `DefaultAdapters()` returns all five. The Pi adapter also reads oh-my-pi (OMP) sessions; set `PiAdapter.OMP` to label them `omp` and default to `~/.omp/agent/sessions`.

```go
import "github.com/stump-wtf/agent-trace/tail"

watcher := tail.NewWatcherWithConfig(tail.DefaultWatchConfig(), tail.DefaultAdapters())
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
go watcher.Start(ctx)

for ev := range watcher.Events() {
    fmt.Printf("%s: %s\n", ev.Session.Title, ev.Classified.Summary)
}
```

Idle detection uses the session's own event timestamps, not scan time:

```go
if watcher.IsIdle(sessionKey) {
    // session has been quiet longer than IdleAfter
}
```

Each adapter has a `Dir` field for testing with temp directories. The Codex adapter also has `IndexPath` for title resolution from `session_index.jsonl`.

Alongside events, `Parse` and `ParseSince` return `classify.Mark`s — timeline annotations that are not tool calls, each at the seq of the next tool event:

| `Type` | Meaning | Agents |
|---|---|---|
| `user-message` | a message the user typed (harness-injected text is filtered) | all |
| `compaction` | the context was compacted | Claude Code, Codex, OpenCode, Pi |
| `subagent` | a subagent was launched | Claude Code, Codex, OpenCode |
| `error` | a model call failed; `Note` says why | Claude Code, Crush |
| `turn-end` | the agent finished its turn, dated by the boundary; `Note` is the agent's reason where it records one | Claude Code, Codex, Crush |

`turn-end` comes from a Claude Code response's `stop_reason` (`end_turn`, `stop_sequence`, `max_tokens`, `refusal` — never `tool_use`), once per response even though Claude Code writes one line per content block; from Codex's `task_complete` event; and from a Crush assistant row's finish part (`end_turn`, `max_tokens`, `content_filter`). `ParseSince` delivers each exactly once across polls.
An adapter whose agent can write its run to stdout in a structured format implements `StreamParser`: `StreamFormat()` names the format and the argv flags that select it, and `ParseStream` reads that output from an `io.Reader` — the agent's stdout pipe — handing over events, marks and the session's metadata as the records arrive, then returns the run's `StreamResult` (outcome, turns, duration, cost, token usage). The result is nil when the process died before reporting one. Claude Code's `stream-json` is supported today:

```go
var cc tail.Adapter = &tail.ClaudeCodeAdapter{}
sp, ok := cc.(tail.StreamParser)
if !ok {
    return // no structured output mode
}
cmd := exec.Command("claude", append([]string{"-p", prompt}, sp.StreamFormat().Args...)...)
stdout, _ := cmd.StdoutPipe()
_ = cmd.Start()
meta, result, err := sp.ParseStream(ctx, stdout, tail.StreamHandler{
    Event: func(ev classify.Event) { fmt.Println(ev.Summary) },
})
_ = cmd.Wait()
```

### `otel`

Converts classified events and marks into OpenTelemetry span structs. Maps user messages to parent spans, tool calls to child spans, errors to status codes. Deterministic trace/span IDs enable idempotent re-submission.

```go
import "github.com/stump-wtf/agent-trace/otel"

trace := otel.BuildTrace(session, events, marks)
// trace.Spans[0].Name, .StartTime, .EndTime, .ParentSpanID

err := otel.WriteJSON(os.Stdout, trace)
```

`Span.Attributes` is `map[string]any` so numeric attributes stay numeric — `agent.result.bytes` and `agent.outside_count` serialize as JSON numbers, not quoted strings. Values must be one of the types OTel permits: string, bool, int64, float64, or a slice of those.

`WriteJSON` emits this package's own `{traceId, session, spans}` shape, not the OTLP/HTTP wire format — a collector-bound exporter has to translate it first.

No `time.Now()` — missing timestamps fall back to the nearest event, then `SessionMeta.StartedAt`, then zero. Building the same trace twice produces identical timings.

## Architecture

```
tail (parse JSONL)  →  classify (ToolCall+ToolResult → Event)  →  otel (Event+Mark → Span tree)
```

One-way data flow. `classify` is the pure core. `tail` does I/O and passes `Options` to `classify`. `otel` is a stateless transformation.

## Commands

```sh
make test    # go test ./...
make lint    # gofmt + go vet
make check   # lint + test
```

## Origin

Extracted from `cosmtrek/mindwalk` `internal/adapter` and `internal/model` packages. MIT license, preserving upstream's copyright.

## License

MIT — see [LICENSE](LICENSE).
