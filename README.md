# agent-trace

Agent session trace libraries extracted from [cosmtrek/mindwalk](https://github.com/cosmtrek/mindwalk). Classify tool actions, tail live session logs, and emit OpenTelemetry spans. Used by [Harness](https://gitea.stump.rocks/stump.wtf/harness) for idle detection and trajectory export to [Cairn](https://gitea.stump.rocks/stump.wtf/cairn).

## Scope

This module covers mindwalk's trace parsing, classification, and event/mark emission only. **Not extracted**: stats computation (`internal/model/stats.go`), the agent-graph builder (`internal/model/agent.go`), judge integration (`internal/judge/`), and the 3D citymap renderer.

## Packages

### `classify`

Pure classification of agent tool calls into semantic actions (`search`, `read`, `edit`, `exec`, `verify`) and file targets. No I/O — the `Options` struct lets callers inject a `FileExists` func and home/tmp dirs for weak-target filtering and outside-scope detection, plus `VerifyPatterns` to extend the built-in verify command list (`just test`, `bun test`, …). Pass nil Options to keep all weak targets and the default verify patterns.

```go
import "gitea.stump.rocks/stump.wtf/agent-trace/classify"

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

### `tail`

Live session log discovery and per-agent JSONL parsing. Watches agent session directories, tails growing files, and emits classified `Event`s. Supports Claude Code, Codex, and Pi via the `Adapter` interface.

```go
import "gitea.stump.rocks/stump.wtf/agent-trace/tail"

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

### `otel`

Converts classified events and marks into OpenTelemetry span structs. Maps user messages to parent spans, tool calls to child spans, errors to status codes. Deterministic trace/span IDs enable idempotent re-submission.

```go
import "gitea.stump.rocks/stump.wtf/agent-trace/otel"

trace := otel.BuildTrace(session, events, marks)
// trace.Spans[0].Name, .StartTime, .EndTime, .ParentSpanID
```

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
