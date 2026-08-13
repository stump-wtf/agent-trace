# AGENTS.md

Go library for agent session trace parsing, classification, and OpenTelemetry export. Extracted from [cosmtrek/mindwalk](https://github.com/cosmtrek/mindwalk). Consumed by [Harness](https://gitea.stump.rocks/stump.wtf/harness) for idle detection and trajectory export to [Cairn](https://gitea.stump.rocks/stump.wtf/cairn).

## Commands

```sh
make test    # go test ./...
make lint    # gofmt + go vet
make check   # lint + test
```

Module path: `gitea.stump.rocks/stump.wtf/agent-trace`, Go 1.25. CI runs on Gitea Actions (`.gitea/workflows/ci.yaml`) with separate `lint` and `test` jobs.

## Architecture

Three packages with a one-way data flow: **tail → classify → otel**.

```
tail (parse JSONL/SQLite)  →  classify (ToolCall+ToolResult → Event)  →  otel (Event+Mark → Span tree)
```

### `classify` — classification with optional I/O

Stateless core with one opt-in I/O surface: the `Options` struct supplies a `FileExists` func and home/tmp dirs for weak-target filtering and outside-scope detection. Pass nil Options to keep all weak targets.

**Key types:** `ToolCall`, `ToolResult` (normalized input from any harness) → `Event` (classified output with `Action`, `Targets`, `Outside`, `Summary`), `Mark` (non-tool timeline annotations: user messages, compactions, subagent launches).

**Entry point:** `BuildEvent(seq, cwd, call, result) Event` — classifies a single tool call/result pair. Composes `ActionFor` + `TargetsFor` + `SummarizeTool`. `BuildEventWith(opts, …)` threads `Options` for I/O-aware classification and custom verify patterns.

**Action classification** (`ActionFor` / `ActionForWith`): maps tool names to semantic actions (`search`, `read`, `edit`, `exec`, `verify`, `other`). Handles multiple naming conventions per tool (e.g., `"Read"`/`"read"`, `"Bash"`/`"bash"`/`"exec_command"`). For shell tools, inspects command text to distinguish verify (test/build commands), search (grep/find/ls), read (cat/head/tail), and generic exec. `Options.VerifyPatterns` extends the built-in verify command list for projects using non-standard tools (`just test`, `bun test`, etc.).

**Shell command heuristics** (`shell.go`): `SearchCommand`, `ReadCommand`, `VerifyCommand` / `VerifyCommandWith` are conservative — any unrecognized program in a pipeline returns false, keeping it as `ActionExec`. The `exec` tool (Octotree's JS-based wrapper) requires a mini JS parser (`exec.go`: `execToolArguments`, `matchingJSParen`, `skipJSIgnored`) to extract nested `tools.exec_command()` and `tools.apply_patch()` calls from source code.

**Path extraction** (`paths.go`): multiple regex-based extractors (`pathLineRe` for `file:line` hits, `pathOnlyRe` for bare paths, `commandPathRe` for command arguments, `patchFileRe` for apply_patch format). Paths are normalized relative to `cwd`, with out-of-repo files categorized as `home`, `tmp`, or `other`. `Weak` targets (inferred from command text rather than explicit tool input) are filtered through `repoPathExists` to avoid false positives.

### `tail` — session discovery and parsing

Discovers and parses live agent session logs from per-harness directories, emitting `classify.ToolCall`/`ToolResult` pairs that feed `classify.BuildEvent`. Supports JSONL-backed adapters (Claude Code, Codex, Pi) and SQLite-backed adapters (Crush, OpenCode).

**Adapter pattern** (`watcher.go`): `Adapter` interface with `Harness()`, `SessionDir()`, `ListSessions()`, `Parse()`. Five implementations:

| Adapter | Directory | Format |
|---|---|---|
| `ClaudeCodeAdapter` | `~/.claude/projects/` | One JSONL per session, `tool_use`/`tool_result` content items |
| `CodexAdapter` | `~/.codex/sessions/` | `response_item` lines with `function_call`/`function_call_output` |
| `PiAdapter` | `~/.pi/agent/sessions/` | Append-only tree linearized via `parentId` chain |
| `CrushAdapter` | `~/.local/share/crush/projects.json` | SQLite (`crush.db`), messages stored as JSON parts |
| `OpenCodeAdapter` | `~/.opencode/opencode.db` | SQLite, parts stored as JSON |

`DefaultAdapters()` returns Claude Code, Codex, and Pi. `NewWatcher` + `ScanOnce` for batch processing. Each adapter has a `Dir` field (JSONL) or `DBPath` field (SQLite) to override the default session directory (useful for testing).

**SQLite helpers** (`sqlite_helpers.go`): `openSQLite` opens a read-only SQLite database with a `sync.Map`-based connection cache to avoid per-poll open/close churn. `splitDBSessionPath` splits composite `"dbPath/sessionID"` paths. Both are generic — shared by Crush and OpenCode adapters.

**Session identification:** `sessionKey(harness, path)` produces a stable SHA-256-based key. This is intentionally NOT the agent-level session ID, because Codex resume rollouts can share an ID across multiple files. Use `sessionKey` for routing/routing keys, `SessionMeta.ID` for display only.

**Injected user messages** (`helpers.go`): `injectedUserMessage()` filters harness-injected text (e.g., `# AGENTS.md instructions`, anything wrapped in `<...>`) so it doesn't inflate turn stats. When adding new harness adapters, route user messages through this filter.

**Orphaned tool calls:** Both Claude Code and Pi adapters flush pending tool calls that never received a result, emitting them with zero-value `ToolResult`.

**Codex error inference:** Codex doesn't set an explicit error flag, so `commandOutputFailed()` pattern-matches output text for `exit code 1`, `error:`, `fatal:`, etc.

### `otel` — OpenTelemetry span generation

Converts classified events + marks into a `Trace` of deterministic `Span` structs. No OTel SDK dependency — the `Span`/`SpanKind`/`StatusCode` types mirror the OTel model directly. `Span.Attributes` uses `map[string]any` to support the full range of OTel attribute value types (string, int64, float64, bool).

**Span hierarchy:** User messages → parent spans (turn boundaries). Tool calls → child spans under the most recent parent. Compaction → annotation span. Subagent → client-kind span. Events before the first user message are grouped under no parent (orphan top-level).

**Deterministic IDs:** `deriveTraceID(sessionKey)` and `deriveSpanID(traceID, seq)` are SHA-256-derived and stable across replays of the same session. This is by design — enables idempotent trace submission.

**Timestamp handling:** Missing timestamps fall back to the nearest event, then `SessionMeta.StartedAt`, then the zero time — never `time.Now()`. Building the same trace twice produces identical timings.

**Export:** `WriteJSON(trace, w io.Writer)` serializes a Trace as OTLP-compatible JSON.

### `internal/strutil` — shared string utilities

Contains `TruncateRunes(s, max, suffix)` used by classify, tail, and otel packages. Eliminates the former duplication of `truncateRunes`.

## Conventions

- Pure functions where possible — `classify` is the stateless core, `tail` does I/O, `otel` is a stateless transformation.
- Tool name matching is case-variant aware: both `"Read"` and `"read"` are handled in every switch statement. When adding a new tool, add ALL case variants.
- Touch ranking: `edit > read > hit` (see `RankTouch`). When merging targets for the same file, keep the deepest interaction.
- All exported types/functions have doc comments starting with the identifier name.
- Tests are table-driven with inline struct slices.
- Third-party dependency: `modernc.org/sqlite` (pure-Go SQLite driver for Crush and OpenCode adapters).

## Gotchas

- **`classify.repoPathExists` does filesystem I/O** despite the package doc claiming "no filesystem access." The `Weak` target filtering path calls `os.Stat`. This is a minor doc inconsistency. Use `Options{FileExists: nil}` to disable.
- **Pi adapter `linearizePi`** walks `parentId` from the last entry backward. If the file is truncated mid-write (live tailing), the leaf may be incomplete.
- **Watcher re-parses entire files** on each poll cycle. For long-running sessions this means re-reading the full file every 2s. Byte-offset tailing is a future optimization.
