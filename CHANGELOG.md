# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the version is `v0.x`, the public API carries no compatibility promise and
breaking changes arrive in minor releases. See the note under
[Versioning](#versioning).

## [Unreleased]

### Changed

- `tail`: the Claude Code usage lookback a `ParseSince` performs for each
  conversation it first meets is now capped at 4 MiB instead of the general
  16 MiB record-walk default. The cap bounds the backwards read a
  latency-sensitive poll pays the first time a conversation appears. When a
  conversation's last pre-watermark usage record lies farther back than the
  cap, the first post-watermark record of the response an earlier read
  already reported is reported a second time — a once-per-conversation
  double count that inflates usage, never drops it (see #143).

## [0.6.0] - 2026-09-25

### Fixed

- Setting `WatchConfig.VerifyPatterns` or `WatchConfig.ErrorExcerptBytes`
  no longer drops the filesystem-backed classification every adapter uses by
  default. `NewWatcherWithConfig` injected a `classify.Options` holding only
  those fields, which replaced the adapter's default and so carried no
  `FileExists`, `HomeDir` or `TmpDir`: a weak target inferred from command
  text (`cat does/not/exist.go`) was kept even when the file did not exist,
  and a file under the home directory landed in `Outside` as `other` instead
  of `home`. The watcher now starts from the same default and adds the
  settings to it (#116). A consumer that sets either field will see fewer
  weak `Targets` and different `Outside` scopes than before — the values an
  unconfigured watcher already produced.
- The Crush adapter flags a failed shell command as `IsError`, from both
  `Parse` and `ParseSince` (#115). Crush returns a non-zero exit as an
  ordinary text response, so the part's `is_error` stays false and a failed
  build or test read as a success in `Summary`, the span status and
  `ErrorExcerpt`. A `bash` or `job_output` result whose last line is
  `Exit code N` (N non-zero) or `Command was aborted before completion` —
  before the `<cwd>…</cwd>` block `bash` appends — now sets `IsError`. The
  rule is anchored to the end of the text, so an exit line earlier in the
  output, or any other tool's text, is not a failure. This changes `IsError`,
  `Summary` and span status for existing consumers of Crush events. The
  OpenCode adapter is unchanged.
- The Pi adapter's incremental cursor no longer stalls for good behind a
  tool call that never gets a result (#103). A response the user interrupts
  mid-stream keeps the calls it streamed, and Pi never runs them; an abort
  mid-batch leaves the calls after the one it reached unrun; a process that
  dies mid-call leaves its call unanswered too. In each case `ParseSince`
  held its watermark below the call from then on, and nothing later in the
  session — the next prompt, its tool calls — reached `ParseSince` or a
  `Watcher`. Pi writes every result a response's calls will ever get before
  the turn ends, and the next user or assistant message only after that (it
  defers extension messages and the user's own bash runs to the end of the
  turn so nothing lands between a call and its result), so such a message now
  releases every call still open before it. The call is emitted there with a
  zero-value `ToolResult`, by `Parse` and `ParseSince` alike, including on
  oh-my-pi files, whose resume writes an aborted assistant message after a
  process that died mid-turn. A call with no later message is still held, and
  `Parse` still flushes it last.

- The OpenCode adapter's incremental cursor no longer stalls for good behind
  a tool part left `pending` or `running` by a process that died (#103).
  `ParseSince` withholds such a part until it turns terminal, which it never
  does, so nothing later in the session was delivered. OpenCode runs one step
  at a time per session and marks every tool part a step has not finished as
  an error before it creates the next step's assistant message, so a later
  assistant message in the session now releases the part: it is emitted in
  place with a zero-value `ToolResult`, exactly as `Parse` has always emitted
  it, and seqs are unchanged. A later user message does not release it —
  OpenCode writes a prompt sent while the session is busy at once and runs it
  after the current step.

### Changed

- **Breaking for consumers that key on sequence numbers:** a Pi tool call
  released as an orphan now takes its seq at the message that proved it dead,
  rather than last, where `Parse` used to place it — the change v0.3.0 made
  for Claude Code and Crush. Every Pi event and mark after such a call moves
  up by one, per orphan.
### Added

- `turn-end` marks: `tail` now reports where an agent finished its turn,
  dated by the boundary record, as the closing counterpart of
  `user-message`. No reader reported one before, so a consumer could only
  guess a turn was over from silence — harness's graceful shutdown waited out
  a quiet period, and its metrics could not count a turn that ended in text
  alone. Claude Code marks a response whose `stop_reason` is `end_turn`,
  `stop_sequence`, `max_tokens` or `refusal` (never `tool_use`), noted with
  that reason, once per response although Claude Code writes one line per
  content block and current versions repeat the stop_reason on each;
  failed-call, `<synthetic>` and inline-subagent records never count. Codex
  marks its `task_complete` event (alias `turn_complete`). Crush marks an
  assistant row's finish part with reason `end_turn`, `max_tokens` or
  `content_filter`, dated by the part's own time; its `error` finish mark is
  unchanged. `ParseSince` delivers each exactly once, including when a poll
  stops between two lines of one Claude Code response. Pi and OpenCode emit
  none yet. `otel` makes no span for the mark, but a turn's last tool span
  now ends at the turn end instead of stretching to the next user message.
- `tail.StreamParser`, an optional adapter interface for an agent's
  structured stdout, read from an `io.Reader` as it arrives rather than from
  a transcript file afterwards. `StreamFormat()` names the format and the
  argv flags that select it; `ParseStream` hands each `classify.Event`,
  `classify.Mark` and the session's `SessionMeta` to a `StreamHandler` as the
  records that produce them are read, and returns a `StreamResult` — subtype,
  `IsError`, turns, wall and API duration, cost and `TokenUsage`. The result
  is nil when the stream ends without one, as it does when the process is
  killed; everything read before that is still delivered. Nothing normalized
  a run's stdout before, so a one-shot run that pipes it could not treat the
  stream as its record.
- `ClaudeCodeAdapter` implements it for `claude -p --output-format
  stream-json --verbose`. Each record is read through the same handler as
  `Parse`, so a run's stream and its transcript yield the same events and
  marks; only the prompt's user-message mark is transcript-only, because -p
  does not echo the prompt. A subagent's records, which share the parent's
  stream, pair within their own conversation.
- `cmd/agent-trace`, a CLI over the library. `agent-trace normalize --harness
  <name> <transcript>` writes the session, its marks and its events as JSONL,
  one `{"kind": "session"|"mark"|"event", …}` record per line in seq order;
  `agent-trace otel --harness <name> <transcript>` writes `otel.BuildTrace`'s
  JSON. Every adapter is reachable (`claude-code`, `codex`, `crush`, `omp`,
  `opencode`, `pi`), so a shell pipeline or CI job gets the records Harness
  would build. Output is redacted by default: summaries, mark notes, session
  titles and error excerpts pass through a pattern-based credential redactor,
  the excerpt through `classify.Options.Redact` before it is cut;
  `--no-redact` turns it off and `--error-excerpt-bytes` opts in to excerpts.
  Reading a stream from standard input (`-`) is reserved for #132 and returns
  an error until then.

- `classify.Usage`: token usage, recorded cost, and the served model and
  provider, as a third item kind beside events and marks (#105). A `Usage`
  shares their seq space the way a `Mark` does and follows the same
  watermark rules, so repeated incremental reads deliver each report once. A
  transcript with no usage yields no `Usage` items; a field the transcript
  does not record stays empty, and `CostUSD` is nil unless the agent wrote a
  cost down — agent-trace never prices tokens.
- `tail.ItemParser`, `tail.Items`, and the package-level `tail.ParseItems` /
  `tail.ParseItemsSince`: `Parse` and `ParseSince` with the usage kept. The
  interface is optional, so `Adapter`, `IncrementalParser` and their callers
  are unchanged; the helpers fall back to `Parse` / `ParseSince` for an
  adapter that does not implement it.
- Usage from three readers. Claude Code: one report per API response —
  every record of a response repeats its usage, so it is reported once, keyed
  by `requestId`, including when an incremental read resumes mid-response.
  Codex: one report per response from `event_msg` `token_count`
  (`info.last_token_usage`), skipping the repeat Codex writes on a rate-limit
  update, with the turn's model and the session's `model_provider`; cached
  prompt tokens are moved out of `input_tokens` so the four token counts are
  disjoint. Crush: the session's recorded cost as a cumulative report —
  Crush's token columns hold the latest step's counts rather than a total, so
  they are not reported. OpenCode and Pi are not read yet; the README's table
  lists which fields each reader fills.

## [0.5.0] - 2026-09-24

### Added

- `classify.Event.InputDigest` (`inputDigest` in JSON): the hex SHA-256 of the
  call's input encoded as JSON, whose map keys encoding/json sorts at every
  depth. Two calls with the same arguments share it whatever order the
  transcript stored them in. An `Event` kept no arguments, and for a tool the
  classifier does not know — every MCP tool — `Summary` is only the tool name,
  so a consumer could not tell 608 identical `add_comment` calls from 608
  different ones. It is always filled by
  `BuildEvent` and `BuildEventWith`, excludes the tool name, and is a
  fingerprint, not a redaction.
- `tail.PiAdapter` reads oh-my-pi (OMP) session files. OMP opens every
  session with a fixed-width, 256-byte `{"type":"title","v":1,…}` slot line
  that it rewrites in place on rename, then the Pi session header, and the
  reader recognised a session only by a header on its first line — so it
  rejected every OMP session as "not a pi session". It now skips exactly one
  leading slot, in `Parse`, `Summarize` and `ParseSince`'s header lookup;
  byte-offset watermarks are unaffected, and a rename moves none of them.
  The slot's title becomes `SessionMeta.Title` (a Pi `session_info` name
  still wins), OMP's `model_change` shape (`model: "provider/modelId"` with a
  `role`, only the default role counting) sets `SessionMeta.Model`, and
  OMP-only entry types and message roles are passed over. A file with a slot
  and no header straight after it is still rejected.
- `tail.HarnessOMP` and `PiAdapter.OMP`. Setting `OMP` labels the sessions
  an adapter reads `omp` instead of `pi` — `Harness()`, `SessionMeta.Harness`
  and session keys — and defaults its directory to `~/.omp/agent/sessions`
  (`WithRoot` likewise). The zero value is unchanged, and `DefaultAdapters`
  does not include an OMP adapter.

## [0.4.0] - 2026-09-22

A correctness release for `tail` and `otel`: a failed model call reaches the
exported trace, an errored tool result can keep its text, and two cursor bugs
stop losing events on a session that runs turns concurrently or goes quiet
mid-call.

`otel` now renders an `error` mark on the trace instead of dropping it. Inside a
turn the mark lands on that turn's span as an exception event plus an ERROR
status; outside any turn it roots a zero-length standalone span. Before this the
reason a turn failed — a provider error, a quota, an auth failure, an overload —
was classified by `tail` and then discarded at export, so a trace showed a turn
that simply stopped.

`ParseSince` in the Crush adapter holds its watermark below **every** unfinished
assistant row, not only the last one. Crush rewrites an assistant row in place as
a turn streams, so a turn's finish part — including one recording a provider
error — can land in a row a poll already read. Holding below the last unfinished
row was enough while turns ran one at a time; with turns running concurrently in
one session, turn B finishing says nothing about turn A, so A's error mark was
lost. A row a killed Crush never finishes now holds the cursor on its own, which
the watcher's stalled-session reconciliation bounds.

The watcher also reconciles a stalled session rather than re-reading it forever.
A transcript whose writer died mid-call could not be proved dead or finished by
anything in the file, so every poll returned the same offset; after `StallScans`
consecutive unchanged scans the watcher re-reads it in full and delivers
everything the incremental polls had withheld.

### Fixed
- A transcript whose writer died mid-call no longer stalls the watcher's
  cursor forever. `ParseSince` holds an unresolved call below its watermark by
  design, and a daemon restart's trailing records — a `bridge-session`, a
  `last-prompt`, a `queue-operation` — can neither answer the call nor prove
  it dead, so on a transcript that will never change again every poll re-read
  the same bytes and returned the same offset: #110's real case settled at
  3,168,969 of 3,176,944 bytes with 400 consecutive empty polls. Nothing in
  the file can tell a dead call from a slow one, so the proof arrives as
  silence: after `StallScans` consecutive scans that find a session unchanged,
  the watcher reconciles it with a full `Parse` — whose orphan flush is what
  a completed transcript deserves — and delivers everything the incremental
  polls had withheld, deduplicated by seq, with the watermark set to the end
  of the file. The trade is explicit: a tool call that genuinely outlasts the
  window is emitted early with an empty result, and its result, landing
  afterwards, is dropped. Below the threshold the in-flight hold is untouched.

- The Crush adapter reads the `is_error` flag Crush writes on a `tool_result`
  part, from both `Parse` and `ParseSince`. Every Crush event previously had
  `IsError` false, so a failed view or edit read as a success in its summary
  and in the span `otel` builds from it. This changes `IsError`, `Summary` and
  span status for Crush tool calls that Crush itself recorded as errors. A
  shell command that exits non-zero is still not an error: Crush records it as
  an ordinary result whose text ends `Exit code N`.

- `ParseSince` in the Crush adapter holds its watermark below every unfinished
  assistant row read in a poll, not only the last. Crush inserts an assistant
  row when a turn begins and rewrites its parts in place, so the turn's tool
  calls and its finish part — including one recording a provider error — can
  land in a row a poll already read. Holding below only the last unfinished row
  assumed that anything written after a turn means that turn is over, which
  stops being true when Crush runs turns concurrently in one session: turn B
  finishing says nothing about turn A, so A's finish was written before the
  cursor and its error mark was lost. A row a killed Crush never finishes holds
  the cursor on its own; the watcher's stalled-session reconciliation bounds it.

### Added

- `otel.BuildTrace` renders an `error` mark on the exported trace. A mark
  inside a turn sets that turn's span to an ERROR status, records the note as
  its status message, and adds an `exception` span event carrying
  `exception.message`; a mark outside any turn roots a zero-length standalone
  span. The reason a turn failed was already classified by `tail` and was
  dropped at export, so a trace showed a turn that simply stopped.

- `classify.Options.ErrorExcerptBytes` keeps up to that many bytes of an
  errored tool result's text on the new `classify.Event.ErrorExcerpt`
  (`errorExcerpt` in JSON, omitted when empty). Until now an `Event` kept a
  result's size and error flag but none of its text, so the literal error an
  agent hit — `undefined: foo`, `401 Unauthorized` — never survived parsing,
  and a consumer matching on symptoms had nothing to match. The excerpt has
  terminal escape sequences stripped and surrounding whitespace trimmed, never
  splits a UTF-8 rune, and keeps the head and tail of longer text joined by
  `classify.ErrorExcerptElision`, each cut snapping to a nearby line break.
  It is off by default: zero keeps nothing, and a result whose `IsError` is
  false never carries one.

- `classify.Options.Redact` rewrites the text before an excerpt is cut from it
  and stored, so a consumer that supplies one never holds raw text on an
  `Event`. It runs over the whole cleaned result rather than the cut excerpt,
  so a secret straddling the cut is still whole when it is matched.

- `tail.WatchConfig.ErrorExcerptBytes` and `tail.WatchConfig.Redact` carry the
  two settings to every adapter through `OptionsSetter`, the path
  `VerifyPatterns` already takes, so events from both `Parse` and `ParseSince`
  carry the excerpt. The watcher injects `Options` only when `VerifyPatterns`
  or `ErrorExcerptBytes` is set; with neither, adapters keep their defaults.

## [0.3.0] - 2026-09-22

A correctness release for `tail`: the incremental readers stop going silent,
and a failed model call becomes something an operator can see and classify.

`ParseSince` no longer pins its watermark forever behind a tool call that will
never get a result, so a session killed mid-call and resumed keeps being
delivered instead of stalling. Claude Code joins Crush in emitting an `error`
mark for a failed model call — the signal harness classifies into quota, auth,
timeout and transport, which stayed empty for every claude-code agent until now.

One change shifts sequence numbers for consumers that key on them, and is
called out under Changed.

### Added

- The Crush adapter emits a `classify.Mark` of type `error` for a turn that
  ended in a `finish` part with reason `error`, from both `Parse` and
  `ParseSince`. The note carries the provider's message and details — for a
  run that died of a context-window overflow or a rejected request, the one
  fact that says why, which previously never left the database. `otel` has no
  span for the type and passes it over. The mark is dated by the finish part's
  own `time`, not by its message row, which Crush creates when the turn begins
  — a live store held a turn that timed out 300 seconds after its row was
  written.

- The Claude Code adapter emits a `classify.Mark` of type `error` for a failed
  API call, from both `Parse` and `ParseSince`. Claude Code records the
  failure as a synthetic assistant message flagged `isApiErrorMessage`, and
  until now the adapter read past it, so an agent stalled on a rate limit, a
  rejected key or an outage looked merely quiet. The mark is dated by that
  record and shares the seq space of the other marks. Its note leads with the
  error code and, when Claude Code got an HTTP response, the status —
  `rate_limit (429): You've hit your session limit`, or
  `server_error: API Error: Unable to connect to API` for a connection failure
  with no status — because the message text alone does not name the failure:
  quota messages do not start with `API Error:` at all. Detection is on the
  flag only; versions of Claude Code that predate it stay silent rather than
  being guessed at from text. The failed call's `<synthetic>` model no longer
  becomes the session's model, which it did for a session whose first
  assistant record was a failure. Retries (the `system` records with subtype
  `api_error`) are not marks: a retry that succeeds is not a failed call. As
  with the Crush mark, `otel` has no span for the type and passes it over.

### Fixed

- The Claude Code adapter's incremental cursor no longer stalls for good
  behind a tool call that never gets a result. `ParseSince` held its
  watermark below any call still waiting on its result, which is right for a
  call in flight and permanent for one whose agent was killed mid-call and
  then resumed: every later poll re-read the same point, and nothing after the
  orphan — the resumed turn's user message, its tool calls — ever reached
  `ParseSince` or a `Watcher`. A pending call is now released by the first
  record that proves no result can follow: an assistant line from a different
  API response, or a message the user typed, including the interrupt marker.
  A loaded skill's `isMeta` body, harness-injected text and any text that
  opens with a tag (a task notification, a system reminder) do not count; they
  land between the results of one parallel batch. Nor does a line from
  another conversation: the parent and each subagent are told apart by
  `isSidechain` and `agentId`, and an older transcript's inline subagent line,
  which has no `agentId` and so cannot be told from a parallel sibling's,
  releases nothing. Across 107,525 resolved
  calls in 1,774 local transcripts, no result was ever written after either
  record. The released call is emitted at that record with a zero-value
  `ToolResult`, by `Parse` and `ParseSince` alike, where `Parse` used to
  append it after every other event — so every event and mark after it now
  carries a seq one higher, per orphan, than `Parse` gave it before. A call
  with nothing after it that proves it dead is still held, and `Parse` still
  flushes it last.

- The Codex adapter's incremental cursor no longer stalls for good behind a
  call that never gets an output, the same failure in a rollout. A call still
  open when a new turn begins (a `task_started` event or a `turn_context`
  record) or when the user sends a message is now settled with a zero-value
  `ToolResult`: Codex writes a round's tool outputs before any of those, and
  drops an aborted turn's task, so nothing can answer the call afterwards.
  `Parse` settles it at the same records, so its seq, which follows call
  order, is unchanged, and both paths now ignore an output that arrives after
  its call was settled.

- The Crush adapter's incremental cursor no longer stalls for good behind a
  call its step finished without answering. A step cut short (max tokens, an
  unknown finish) never runs the calls it streamed, and a live store held
  such a call with 528 rows behind it, all withheld from `ParseSince`. Crush
  writes a step's finish part only after every tool row the step will write,
  so a call on a finished row that no row answers is released once a user or
  assistant row follows, and emitted at the end of its row with a zero-value
  `ToolResult`, by `Parse` and `ParseSince` alike. A later row alone proves
  nothing: Crush runs turns concurrently within one session, and in live
  stores 39 of 42,341 results landed after a later user or assistant row. A
  call on a row that never finishes, which is what a Crush killed mid-call
  leaves, therefore still holds the cursor. `Parse` also flushes the calls
  still open at the end of a session in the order they were issued, where it
  ranged over a map and could number several of them differently each time.

- The Crush adapter's incremental cursor no longer skips a turn that is still
  being streamed. Crush inserts an assistant row when a turn starts and writes
  its tool calls and its `finish` part into that same row as the stream
  progresses, so a poll that read the row mid-stream moved `ParseSince` (and,
  on a watcher's first scan, `Watermark`) past it, and the finish error and
  any tool call recorded afterwards were never read. The cursor now holds
  strictly below a last row that is an assistant message with no `finish`
  part. Only the last row is held: a Crush killed mid-stream leaves a row that
  never finishes, and anything written after it means that turn is over.
  `Watermark` holds only while that row carries no tool call yet, because
  `Parse` has already flushed such a call as an event, and re-reading it would
  emit it twice.

- The Crush adapter no longer discovers zero sessions when `projects.json`
  carries trailing bytes after its JSON document. Crush writes the registry
  with `os.WriteFile` under an in-process lock only, so two Crush processes
  registering at once leave a complete document followed by the tail of a
  longer one; a live host's registry ended in `}}`. A strict decode rejected
  the whole file, silently. Discovery now reads the first document, and
  `Diagnostics` warns about a registry that has trailing bytes or does not
  parse instead of reporting it `ok` because it exists.

### Changed

- **Breaking for consumers that key on sequence numbers:** a tool call released
  as an orphan now takes its seq where a later record proved it dead, rather
  than last as `Parse` used to place it. Every event and mark after such a call
  therefore moves up by one, per orphan, for Claude Code and Crush; Codex
  already numbered calls in order and is unchanged. Callers that persist seqs
  across a poll boundary — harness's telemetry item IDs, for one — see a
  one-time shift on any session containing an orphan. Reiterating a call's own
  seq as it arrives is unaffected.

- `CrushAdapter` and `DefaultAdapters` now document that `CRUSH_GLOBAL_DATA`
  relocates `projects.json`, so a supervised Crush needs its own adapter with
  `ProjectsPath`, and that `DBPath` plus `Cwd` is a supported single-project
  mode rather than a test hook.

Both reported on the GitHub mirror by [@LarsArtmann](https://github.com/LarsArtmann),
in [issue #22](https://github.com/stump-wtf/agent-trace/issues/22).

- The Crush adapter no longer double-counts a database that `projects.json`
  registers more than once. Crush keys a project on its working
  directory, so several entries can resolve to one `crush.db`; every scan loop
  appended what each entry found and nothing downstream deduped, so those
  sessions were listed once per entry and `AgentGraph` gained a copy of every
  node per entry. Discovery now folds entries by their resolved database path,
  with the newest `last_accessed` supplying the working directory.

- Crush incremental parsing no longer loses messages that share the watermark's
  second. `messages.created_at` is second-resolution and the watermark
  was a value from it, so once a poll resumed at second T the strict
  `created_at > T` excluded every later message stamped T — permanently, since
  the watermark only moves forward. `Watermark` and `ParseSince` now use
  `messages.rowid`, which is unique, monotonic, and the insertion order Crush
  actually wrote the rows in. `Parse` orders by it too: a tool call and its
  result routinely tie on `created_at`, and SQLite leaves tied rows in an
  unspecified order, which would drop the result and flush the call with no
  output.

## [0.2.0] - 2026-08-24

A performance and correctness release for `tail`. Every adapter now parses
incrementally, discovery is bounded by an activity window and memoized across
scans, and `Summarize` reads a bounded head and tail instead of the whole file:
one pass over a 215-session corpus drops from 2.3s to 0.4s, and the steady state
to ~4ms.

One change is breaking for library consumers — `WatchConfig.MaxAge` now defaults
to 48h — and is called out under Changed.

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
  a full Parse would have used. Pi's per-poll continuation check reads backwards
  from the watermark under a 4 MiB cap and falls back to a full parse rather
  than growing to fit one enormous record.

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

`v0.x` means the API is still moving, and this project's minor bumps carry
breaking changes rather than deferring them to a major: v0.1.0 shipped two,
v0.2.0 another, and v0.3.0 a change to how orphans are sequenced. Treat every
minor bump as potentially breaking and pin exactly.

The module path lives on the GitHub mirror because Go resolves versions there.
Tags are created on Gitea and reach GitHub through the push mirror — never tag the
mirror directly, as the next sync prunes refs the source does not have.

[Unreleased]: https://github.com/stump-wtf/agent-trace/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/stump-wtf/agent-trace/releases/tag/v0.6.0
[0.5.0]: https://github.com/stump-wtf/agent-trace/releases/tag/v0.5.0
[0.4.0]: https://github.com/stump-wtf/agent-trace/releases/tag/v0.4.0
[0.3.0]: https://github.com/stump-wtf/agent-trace/releases/tag/v0.3.0
[0.2.0]: https://github.com/stump-wtf/agent-trace/releases/tag/v0.2.0
[0.1.0]: https://github.com/stump-wtf/agent-trace/releases/tag/v0.1.0
