# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the version is `v0.x`, the public API carries no compatibility promise and
breaking changes arrive in minor releases. See the note under
[Versioning](#versioning).

## [Unreleased]

### Changed

- `Summarize` and `AgentGraphBuilder.AgentGraph` take a `context.Context` (#72),
  closing the gap #67 left open. Both opened `context.Background()`, so a
  cancelled caller still waited on the SQLite query behind every Crush and
  OpenCode call — and `Summarize` sits on the hot path of every JSONL listing.
  The JSONL adapters check the context before opening the file; one file's read
  still runs to completion, per the Adapter cancellation contract.

## [0.1.0] - 2026-08-15

First tagged release. The library was extracted from `mindwalk` on 2026-08-09 and
has been usable from `main` since; this marks a point consumers can pin instead of
a commit SHA.

### Added

- **`classify`** — pure action classification for agent tool calls, extracted from
  `mindwalk`. No filesystem access except through an injectable `Options`, so the
  package is deterministic and testable.
  - Custom verify-command patterns via `Options.VerifyPatterns`.
- **`tail`** — session discovery and parsing across five harnesses, emitting
  `classify.ToolCall`/`ToolResult` pairs plus timeline marks.
  - JSONL-backed adapters: Claude Code, Codex, Pi.
  - SQLite-backed adapters: Crush, OpenCode.
  - `Watcher` with polling, per-session idle tracking, and seq-based dedup.
  - `IncrementalParser` — byte-offset tailing for JSONL, timestamp watermarks for
    SQLite, so long sessions are not re-read every poll. Implemented by Claude Code
    and Crush; the remaining three still full-parse (see issue #62).
  - `FilteredLister` — `SessionFilter` pushdown into SQL for the SQLite adapters,
    contractually identical to `ListSessions` plus in-memory filtering. The JSONL
    adapters do not implement it yet (see issue #37).
  - `DiagnosticsSource` — health checks that distinguish "no sessions" from
    "directory missing" or "database unreadable".
  - `AgentGraphBuilder` — parent/child session correlation for subagent traces.
  - `WithRoot` / `DefaultAdaptersIn` for retargeting adapters at a different root.
- **`otel`** — OpenTelemetry span construction from classified events, with a JSON
  exporter. Deterministic: no `time.Now()` in the build path.

### Fixed

Bugs worth calling out because they produced wrong output rather than errors:

- **Crush timestamps decoded as milliseconds when the column stores seconds**
  (#64). Every Crush timestamp rendered in January 1970. The schema comment claims
  milliseconds and is wrong; verified against live databases. OpenCode genuinely
  does store milliseconds, so the two converters are deliberately separate.
- **Marks silently dropped during live tailing** (#66). The watcher's emit path
  discarded every mark. OpenCode additionally numbered its user-message marks past
  every event, where the delivery mechanism could never reach them.
- **`rows.Err()` never checked after SQLite iteration** (#65). A mid-iteration
  failure returned a truncated result as a success.
- **Change detection advanced before the parse that could fail** (#70). A failed
  parse was never retried, losing a session's trailing events.
- **The watcher held its mutex across channel sends** (#71). A slow consumer
  blocked `LastActivity` and `IsIdle` — the API it needed to make progress.
- **Incremental parsing could emit an event twice** (#58), and optional interfaces
  were lost across `WithRoot` (#59).

### Changed

- `Adapter.ListSessions`, `Adapter.Parse`, `IncrementalParser.ParseSince` and
  `IncrementalParser.Watermark` take a `context.Context` (#67). `Watcher.ScanOnce`
  does too. `Summarize` and `AgentGraph` do not yet — see issue #72.
- The module path is `github.com/stump-wtf/agent-trace`. Development happens on
  [Gitea](https://gitea.stump.rocks/stump.wtf/agent-trace); GitHub is a read-only
  mirror that exists so Go tooling can resolve the module.

## Versioning

`v0.x` means the API is still moving. Two breaking changes landed in the run-up to
this release and issue #72 proposes another, so treat every minor bump as
potentially breaking and pin exactly.

The module path lives on the GitHub mirror because Go resolves versions there.
Tags are created on Gitea and reach GitHub through the push mirror — never tag the
mirror directly, as the next sync prunes refs the source does not have.

[Unreleased]: https://gitea.stump.rocks/stump.wtf/agent-trace/compare/v0.1.0...HEAD
[0.1.0]: https://gitea.stump.rocks/stump.wtf/agent-trace/releases/tag/v0.1.0
