# AGENTS.md

Go library for agent session trace parsing, classification, and OpenTelemetry export. Extracted from [cosmtrek/mindwalk](https://github.com/cosmtrek/mindwalk). Consumed by [Harness](https://gitea.stump.rocks/stump.wtf/harness) for idle detection and trajectory export to [Cairn](https://gitea.stump.rocks/stump.wtf/cairn).

## Commands

```sh
go test ./...          # run all tests
go test ./classify/... # test one package
go vet ./...           # vet (clean = no output)
```

No Makefile yet. No CI configured yet. Module path: `gitea.stump.rocks/stump.wtf/agent-trace`, Go 1.22.

## Architecture

Three packages with a one-way data flow: **tail → classify → otel**.

```
tail (parse JSONL)  →  classify (ToolCall+ToolResult → Event)  →  otel (Event+Mark → Span tree)
```

### `classify` — pure, stateless classification

Zero external deps beyond stdlib. No I/O, no filesystem access (except `repoPathExists` which calls `os.Stat` to filter weak/inferred paths). The core building block other packages depend on.

**Key types:** `ToolCall`, `ToolResult` (normalized input from any harness) → `Event` (classified output with `Action`, `Targets`, `Outside`, `Summary`), `Mark` (non-tool timeline annotations: user messages, compactions, subagent launches).

**Entry point:** `BuildEvent(seq, cwd, call, result) Event` — classifies a single tool call/result pair. Composes `ActionFor` + `TargetsFor` + `SummarizeTool`.

**Action classification** (`ActionFor`): maps tool names to semantic actions (`search`, `read`, `edit`, `exec`, `verify`, `other`). Handles multiple naming conventions per tool (e.g., `"Read"`/`"read"`, `"Bash"`/`"bash"`/`"exec_command"`). For shell tools, inspects command text to distinguish verify (test/build commands), search (grep/find/ls), read (cat/head/tail), and generic exec.

**Shell command heuristics** (`shell.go`): `SearchCommand`, `ReadCommand`, `VerifyCommand` are conservative — any unrecognized program in a pipeline returns false, keeping it as `ActionExec`. The `exec` tool (Octotree's JS-based wrapper) requires a mini JS parser (`exec.go`: `execToolArguments`, `matchingJSParen`, `skipJSIgnored`) to extract nested `tools.exec_command()` and `tools.apply_patch()` calls from source code.

**Path extraction** (`paths.go`): multiple regex-based extractors (`pathLineRe` for `file:line` hits, `pathOnlyRe` for bare paths, `commandPathRe` for command arguments, `patchFileRe` for apply_patch format). Paths are normalized relative to `cwd`, with out-of-repo files categorized as `home`, `tmp`, or `other`. `Weak` targets (inferred from command text rather than explicit tool input) are filtered through `repoPathExists` to avoid false positives.

### `tail` — session discovery and JSONL parsing

Discovers and parses live agent session logs from per-harness directories, emitting `classify.ToolCall`/`ToolResult` pairs that feed `classify.BuildEvent`.

**Adapter pattern** (`watcher.go`): `Adapter` interface with `Harness()`, `SessionDir()`, `ListSessions()`, `Parse()`. Three implementations:

| Adapter | Directory | Format |
|---|---|---|
| `ClaudeCodeAdapter` | `~/.claude/projects/` | One JSONL per session, `tool_use`/`tool_result` content items |
| `CodexAdapter` | `~/.codex/sessions/` | `response_item` lines with `function_call`/`function_call_output` |
| `PiAdapter` | `~/.pi/agent/sessions/` | Append-only tree linearized via `parentId` chain |

`DefaultAdapters()` returns all three. `NewWatcher` + `ScanOnce` for batch processing. Each adapter has a `Dir` field to override the default session directory (useful for testing).

**Session identification:** `sessionKey(harness, path)` produces a stable SHA-256-based key. This is intentionally NOT the agent-level session ID, because Codex resume rollouts can share an ID across multiple files. Use `sessionKey` for routing/routing keys, `SessionMeta.ID` for display only.

**Injected user messages** (`helpers.go`): `injectedUserMessage()` filters harness-injected text (e.g., `# AGENTS.md instructions`, anything wrapped in `<...>`) so it doesn't inflate turn stats. When adding new harness adapters, route user messages through this filter.

**Orphaned tool calls:** Both Claude Code and Pi adapters flush pending tool calls that never received a result, emitting them with zero-value `ToolResult`.

**Codex error inference:** Codex doesn't set an explicit error flag, so `commandOutputFailed()` pattern-matches output text for `exit code 1`, `error:`, `fatal:`, etc.

**Codex Parse gotcha:** `codex.go:200-218` has a dead first-pass event assembly loop (`events` local variable is shadowed and discarded via `_ = events`). The actual events are built in the second loop. This is a code smell but doesn't affect correctness.

### `otel` — OpenTelemetry span generation

Converts classified events + marks into a `Trace` of deterministic `Span` structs. No OTel SDK dependency — the `Span`/`SpanKind`/`StatusCode` types mirror the OTel model directly.

**Span hierarchy:** User messages → parent spans (turn boundaries). Tool calls → child spans under the most recent parent. Compaction → annotation span. Subagent → client-kind span. Events before the first user message are grouped under no parent (orphan top-level).

**Deterministic IDs:** `deriveTraceID(sessionKey)` and `deriveSpanID(traceID, seq)` are SHA-256-derived and stable across replays of the same session. This is by design — enables idempotent trace submission.

**Timestamp fallback:** `parseEventTimestamp` falls back to `time.Now()` for empty/invalid timestamps. This means trace timing is not fully deterministic when timestamps are missing.

## Conventions

- Pure functions where possible — `classify` is the stateless core, `tail` does I/O, `otel` is stateless transformation.
- Tool name matching is case-variant aware: both `"Read"` and `"read"` are handled in every switch statement. When adding a new tool, add ALL case variants.
- Touch ranking: `edit > read > hit` (see `RankTouch`). When merging targets for the same file, keep the deepest interaction.
- All exported types/functions have doc comments starting with the identifier name.
- Tests are table-driven with inline struct slices.
- No third-party dependencies — stdlib only. The `go.mod` has zero require directives.

## Gotchas

- **No Makefile or CI.** `make test` / `make lint` don't exist yet. Per the [CRUSH.md](https://gitea.stump.rocks/stump.wtf/agent-trace/../../CRUSH.md) convention, these should be added.
- **No README examples for tail/otel.** The README marks them as "planned" but they are fully implemented. The README is stale.
- **`classify.repoPathExists` does filesystem I/O** despite the package doc claiming "no filesystem access." The `Weak` target filtering path calls `os.Stat`. This is a minor doc inconsistency.
- **`codex.go` has a dead code block** (lines ~200-206) — a first event assembly loop whose result is discarded. The real assembly is the second loop starting at line 213.
- **`otel.sortTimeline` uses insertion sort** (O(n²)) — fine for typical session sizes but would degrade on very long sessions.
- **`truncateRunes` is duplicated** in both `classify/exec.go` and `tail/helpers.go` with identical implementation. Not shared via a common package.
- **Pi adapter `linearizePi`** walks `parentId` from the last entry backward. If the file is truncated mid-write (live tailing), the leaf may be incomplete.
