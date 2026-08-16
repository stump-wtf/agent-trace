# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the version is `v0.x`, the public API carries no compatibility promise and
breaking changes arrive in minor releases. See the note under
[Versioning](#versioning).

## [Unreleased]

### Added

- `SessionFilter.ActiveSince` bounds discovery by when a session was last
  touched rather than when it began (#81). `Since` asks the other question, and
  a session opened days ago but still being typed into fails a short `Since`
  bound while actively in use — which is the wrong answer for a live view. The
  three JSONL adapters implement `FilteredLister` and skip an excluded file on
  its mtime without opening it; `filterSessions` still applies the exact
  predicate afterwards, so the pushdown can only ever over-select.
- Incremental parsing for the remaining three adapters (#62). Codex and Pi join
  Claude Code on byte-offset watermarks; OpenCode joins Crush on timestamp
  watermarks (milliseconds, per its schema). Two harness-specific behaviours:
  a Pi branch — an edited turn re-linearizes the append-only tree — is detected
  and falls back to a full parse trimmed to the seq the watcher already emitted,
  and an OpenCode tool part is withheld until its row turns terminal, because
  OpenCode stores the call and its result as one row that mutates in place.
  Codex additionally holds its watermark for one line after an apply_patch
  output, so the patch_apply_end that enriches it is read into the same window
  a full Parse would have used.

### Changed

- **Breaking for library consumers:** `WatchConfig.MaxAge` bounds a watcher's
  discovery to sessions active within a window, defaulting to `DefaultMaxAge`
  (48h) (#81). A watcher built from a zero `WatchConfig` previously discovered
  every session that had ever existed and now applies that default. Set
  `MaxAge: -1` for the old unbounded behavior; `MaxAge: 0` means "use the
  default", following `PollInterval`'s convention in the same struct.
- `Summarize` reads a bounded head, plus a bounded tail for the closing
  timestamp on files that exceed it, instead of parsing every event (#79). One
  discovery pass over a 215-session corpus drops from 2.3s to 0.4s. A file
  inside the head budget is still read whole, so its summary is unchanged. The
  one field the budget can miss is `Title`, when a session titles itself past
  the head: it falls back to the file name, the same fallback an untitled
  session already gets.
- Watchers memoize session summaries across scans on `(harness, size, mtime)`,
  so discovery re-reads only files that actually changed (#80). Steady-state
  `ListSessions` over the same corpus drops from 2.3s to ~4ms. Adapters
  constructed directly, without a `SummaryCache`, are unaffected.
- Every `FilteredLister` implementation is now checked against the shared
  oracle its contract names — identical results to `ListSessions` followed by
  in-memory filtering (#37). Codex and Pi had no equivalence test at all, and
  the matrix predated `ActiveSince`, so the bound the JSONL adapters actually
  push down was untested. Test-only; no behavior change.
- `Summarize` and `AgentGraphBuilder.AgentGraph` take a `context.Context` (#72),
  closing the gap #67 left open. Both opened `context.Background()`, so a
  cancelled caller still waited on the SQLite query behind every Crush and
  OpenCode call — and `Summarize` sits on the hot path of every JSONL listing.
  The JSONL adapters check the context before opening the file; one file's read
  still runs to completion, per the Adapter cancellation contract.

### Fixed

- `Summarize` no longer reports a timestamp from the top of a file as a
  session's last activity (#85). `EndedAt` is recovered from a bounded tail
  read, and a final line larger than that window leaves the window holding no
  whole line at all — after which `EndedAt` silently kept whatever the head had
  last set. That is the normal shape of a transcript whose last record is a big
  tool result, and it had two consequences once `ActiveSince` began listing on
  `EndedAt`: a session being typed into right now fell outside a 48h activity
  window, and the watcher's change detection saw a frozen `EndedAt` and skipped
  the session as unchanged on every poll. When the tail delivers no whole line,
  the session is now dated by the file's mtime — the same signal `mtimeExcludes`
  already trusts to skip a file unread. A tail that *is* readable still supplies
  the exact timestamp.
- The summary head's byte budget is now a hard bound on what is read, not a
  check applied after the fact (#84). Both bounds were tested before each line,
  so a single line longer than `maxBytes` was still pulled into memory whole —
  measured at 8 MiB against a 2 MiB budget. The scan runs over whatever
  `.jsonl` files are in the trajectory directories, including foreign ones this
  library did not write, so one newline-free file defeated the budget that
  exists to bound exactly that. The head now reads through an
  `io.LimitReader` and drops the truncated fragment, the same way the tail scan
  already drops the fragment its seek lands in.
- An incremental watermark could land ON the timestamp of a withheld row, and
  the next poll's strict `>` filter then excluded that row forever (#62). Crush
  could hit this whenever a resolved and an unresolved call shared a second —
  routine at its schema's second resolution. Safe points are now kept strictly
  below the earliest outstanding row's timestamp.

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
