# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

### Added

- **classify package**: pure, stateless action classification extracted from mindwalk (`BuildEvent`, `ActionFor`, `TargetsFor`, `SummarizeTool`)
- **tail package**: session discovery and JSONL parsing with adapter pattern for multiple harnesses
- **otel package**: deterministic OpenTelemetry span tree generation from classified events
- **Claude Code adapter**: parses `~/.claude/projects/` JSONL sessions with git branch and auxiliary metadata
- **Codex adapter**: parses `~/.codex/sessions/` rollout files with model, git branch, and error inference
- **Pi adapter**: parses `~/.pi/agent/sessions/` append-only trees with session titles and compaction marks
- **Crush adapter**: SQLite-backed session parsing (first non-JSONL harness)
- **OpenCode adapter**: SQLite-backed session parsing
- **Watcher**: polling-based live session tracking with idle detection and marks flow
- `SessionFilter` and `ListSessionsFiltered` for filtered session discovery
- `WithRoot` on `Adapter` interface and `DefaultAdaptersIn` helper
- `Started`/`Ended` time.Time accessors on `SessionMeta`
- `agentId` and `isSidechain` carried through `SessionMeta`
- Injectable `Options` making classify genuinely pure (no filesystem access in the hot path)
- `Renovate` configuration for automated dependency updates

### Changed

- Made OTel traces fully deterministic: `EndTime` assigned from events, `time.Now()` fallback eliminated
- Restored mindwalk-compatible JSON tags on all public types

### Fixed

- Codex adapter emitting zero marks (user messages, compaction, sequence interleaving)
- Codex error inference via `commandOutputFailed` pattern matching
