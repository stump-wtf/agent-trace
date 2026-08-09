# agent-trace

Agent session trace libraries extracted from [cosmtrek/mindwalk](https://github.com/cosmtrek/mindwalk). Classify tool actions, tail live session logs, and emit OpenTelemetry spans. Used by [Harness](https://gitea.stump.rocks/stump.wtf/harness) for idle detection and trajectory export to [Cairn](https://gitea.stump.rocks/stump.wtf/cairn).

## Packages

### `classify`

Pure, stateless classification of agent tool calls into semantic actions (`search`, `read`, `edit`, `exec`, `verify`) and file targets. No I/O, no filesystem access, no external dependencies.

```go
import "gitea.stump.rocks/stump.wtf/agent-trace/classify"

event := classify.BuildEvent(seq, cwd, call, result)
// event.Action == "edit", event.Targets == [{Path: "foo.go", Touch: "edit"}]
```

### `tail` *(planned)*

Live session log discovery and per-agent JSONL parsing. Watches agent session directories (`~/.claude/projects/`, `~/.codex/sessions/`, etc.), tails growing files, and emits normalized `ToolCall`/`ToolResult` pairs. Includes idle detection (no events for N seconds).

### `otel` *(planned)*

Converts classified event streams into OpenTelemetry span structs. Maps user messages to parent spans, tool calls to child spans, errors to status codes. Ready for export to any OTel collector or direct submission to Cairn's trace API.

## Origin

Extracted from `cosmtrek/mindwalk` `internal/adapter` and `internal/model` packages. The mindwalk project visualizes coding-agent sessions on a 3D codebase map; these libraries provide the underlying trace parsing and classification without the rendering layer.
