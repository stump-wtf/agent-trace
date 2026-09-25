package tail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/internal/strutil"
)

// CodexAdapter discovers and parses OpenAI Codex session logs from
// ~/.codex/sessions/. Each session is a .jsonl file with response_item lines.
type CodexAdapter struct {
	Dir       string // override default session directory
	IndexPath string // override session_index.jsonl location for title resolution
	// opts carries classify.Options from the watcher (verify patterns, etc).
	opts *classify.Options
	// cache memoizes summaries across scans so an unchanged session file is
	// not re-read. When nil, every scan summarizes from scratch.
	cache *SummaryCache
}

func (a CodexAdapter) Harness() Harness { return HarnessCodex }

// SetOptions injects classify.Options for verify patterns and error excerpts.
func (a *CodexAdapter) SetOptions(opts *classify.Options) { a.opts = opts }

// SetSummaryCache injects the summary cache the watcher shares across scans.
func (a *CodexAdapter) SetSummaryCache(c *SummaryCache) { a.cache = c }

// Diagnostics checks whether the Codex session directory exists and is readable.
func (a CodexAdapter) Diagnostics() []DiagnosticCheck {
	return dirDiagnostics(a.SessionDir())
}

func (a CodexAdapter) SessionDir() string {
	if a.Dir != "" {
		return a.Dir
	}
	return homeDir(".codex", "sessions")
}

// WithRoot returns a copy of the adapter that discovers sessions under root,
// which is treated as a HOME-like base: the adapter appends its own layout
// (.codex/sessions), including the session_index.jsonl used for title
// resolution. Sibling fields are preserved; only the root-derived paths
// change. Pass an empty string to restore the default locations.
func (a CodexAdapter) WithRoot(root string) Adapter {
	if root == "" {
		a.Dir, a.IndexPath = "", ""
		return &a
	}
	sessionsDir := filepath.Join(root, ".codex", "sessions")
	a.Dir = sessionsDir
	a.IndexPath = filepath.Join(sessionsDir, "session_index.jsonl")
	return &a
}

// ListSessions walks the session directory and returns metadata for each
// recognized Codex session file, sorted newest-first.
// It delegates to ListSessionsFiltered with the zero filter, which is
// documented to match every session, so the two can never disagree.
func (a CodexAdapter) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	return a.ListSessionsFiltered(ctx, SessionFilter{})
}

// ListSessionsFiltered implements FilteredLister. A file whose modification
// time already puts it outside the filter's time bounds is skipped on the stat
// alone — see ClaudeCodeAdapter.ListSessionsFiltered and mtimeExcludes for why
// that is exact rather than approximate.
func (a CodexAdapter) ListSessionsFiltered(ctx context.Context, f SessionFilter) ([]SessionMeta, error) {
	dir := a.SessionDir()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, nil
	}
	var metas []SessionMeta
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		// See ClaudeCodeAdapter.ListSessions: the per-entry check is what
		// makes a cancelled context stop the walk rather than a partial
		// listing masquerading as a complete one.
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if info, infoErr := entry.Info(); infoErr == nil && mtimeExcludes(info, f) {
			return nil
		}
		meta, err := summarizeCached(ctx, a.cache, a.Harness(), entry, path, a.Summarize)
		if err == nil && !meta.Auxiliary {
			metas = append(metas, meta)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	metas = filterSessions(metas, f)
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].EndedAt > metas[j].EndedAt
	})
	return metas, nil
}

// Summarize extracts metadata from a Codex session file without full parsing.
// The context is checked before the file is opened; the read itself runs to
// completion, bounded by that file's size, per the Adapter cancellation
// contract.
func (a CodexAdapter) Summarize(ctx context.Context, path string) (SessionMeta, error) {
	if err := ctx.Err(); err != nil {
		return SessionMeta{}, err
	}
	f, meta, err := openJSONLSession(a.Harness(), path)
	if err != nil {
		return SessionMeta{}, err
	}
	defer func() { _ = f.Close() }()

	recognized := false
	err = ReadJSONLines(f, func(data []byte) {
		var line codexRawLine
		if json.Unmarshal(data, &line) != nil {
			return
		}
		if line.Timestamp != "" {
			if meta.StartedAt == "" {
				meta.StartedAt = line.Timestamp
			}
			meta.EndedAt = line.Timestamp
		}
		switch line.Type {
		case "session_meta":
			recognized = true
			var payload codexSessionMeta
			if json.Unmarshal(line.Payload, &payload) == nil {
				if payload.ID != "" {
					meta.ID = payload.ID
				}
				if payload.Cwd != "" && meta.Cwd == "" {
					meta.Cwd = payload.Cwd
				}
				if payload.Git.Branch != "" {
					meta.GitBranch = payload.Git.Branch
				}
				if payload.isSubagent() {
					meta.Auxiliary = true
				}
			}
		case "turn_context":
			recognized = true
			var payload codexTurnContext
			if json.Unmarshal(line.Payload, &payload) == nil {
				if payload.Cwd != "" && meta.Cwd == "" {
					meta.Cwd = payload.Cwd
				}
				if payload.Model != "" && meta.Model == "" {
					meta.Model = payload.Model
				}
			}
		case "response_item", "event_msg":
			recognized = true
		case "message":
			if line.Role != "" {
				recognized = true
			}
		case "":
			if line.ID != "" {
				recognized = true
			}
		}
	})
	if !recognized {
		return SessionMeta{}, fmt.Errorf("not a Codex session: %s", path)
	}
	if meta.Title == "" {
		meta.Title = a.titleFor(meta.ID)
	}
	if meta.Title == "" {
		meta.Title = filepath.Base(path)
	}
	return meta, err
}

// Parse reads a complete Codex session file and returns classified events.
// It is ParseItems without the usage reports.
func (a CodexAdapter) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
	items, meta, err := a.ParseItems(ctx, path)
	return items.Events, items.Marks, meta, err
}

// ParseItems implements ItemParser: Parse, plus one classify.Usage per
// response Codex recorded a token_count for (see codexUsageTracker).
func (a CodexAdapter) ParseItems(ctx context.Context, path string) (Items, SessionMeta, error) {
	f, meta, err := openJSONLSession(a.Harness(), path)
	if err != nil {
		return Items{}, SessionMeta{}, err
	}
	defer func() { _ = f.Close() }()

	recognized := false
	opts := a.opts
	if opts == nil {
		opts = osClassifyOptions(nil)
	}
	calls := map[string]classify.ToolCall{}
	results := map[string]classify.ToolResult{}
	callOrder := []string{}
	directPatches := map[string]bool{}
	patchResults := map[string]codexEventMsg{}
	var marks []classify.Mark
	var usage []classify.Usage
	usageSeen := newCodexUsageTracker("", 0)

	err = ReadJSONLines(f, func(data []byte) {
		var line codexRawLine
		if json.Unmarshal(data, &line) != nil {
			return
		}
		if line.Timestamp != "" {
			if meta.StartedAt == "" {
				meta.StartedAt = line.Timestamp
			}
			meta.EndedAt = line.Timestamp
		}
		switch line.Type {
		case "session_meta":
			recognized = true
			var payload codexSessionMeta
			if json.Unmarshal(line.Payload, &payload) == nil {
				if payload.ID != "" {
					meta.ID = payload.ID
				}
				if payload.Cwd != "" && meta.Cwd == "" {
					meta.Cwd = payload.Cwd
				}
				if payload.Git.Branch != "" && meta.GitBranch == "" {
					meta.GitBranch = payload.Git.Branch
				}
				if payload.isSubagent() {
					meta.Auxiliary = true
				}
				usageSeen.provider = payload.ModelProvider
			}
		case "turn_context":
			recognized = true
			codexReleaseOpenCalls(calls, results, callOrder)
			var payload codexTurnContext
			if json.Unmarshal(line.Payload, &payload) == nil {
				usageSeen.turn(payload)
				if payload.Cwd != "" && meta.Cwd == "" {
					meta.Cwd = payload.Cwd
				}
				if payload.Model != "" && meta.Model == "" {
					meta.Model = payload.Model
				}
			}
		case "response_item":
			recognized = true
			var payload codexResponseItem
			if json.Unmarshal(line.Payload, &payload) != nil {
				return
			}
			if callID, name, input, ok := decodeCodexCall(payload); ok {
				if _, exists := calls[callID]; exists {
					return
				}
				if name == "spawn_agent" {
					marks = append(marks, classify.Mark{
						Seq:       len(callOrder),
						Timestamp: line.Timestamp,
						Type:      "subagent",
						Note:      name,
					})
				}
				calls[callID] = classify.ToolCall{
					ID:        callID,
					Name:      name,
					Input:     input,
					Timestamp: line.Timestamp,
				}
				callOrder = append(callOrder, callID)
				directPatches[callID] = payload.Type == "custom_tool_call" && name == "apply_patch"
				return
			}
			if callID, result, ok := decodeCodexOutput(payload); ok {
				if _, exists := calls[callID]; !exists {
					return
				}
				if _, exists := results[callID]; exists {
					return
				}
				results[callID] = result
				return
			}
			if payload.Type == "message" && payload.Role == "user" && payload.Content.HasText() {
				text := payload.Content.Text()
				if !injectedUserMessage(text) {
					codexReleaseOpenCalls(calls, results, callOrder)
					marks = append(marks, classify.Mark{
						Seq:       len(callOrder),
						Timestamp: line.Timestamp,
						Type:      "user-message",
						Note:      strutil.TruncateRunes(text, 2000, "…"),
					})
				}
			}
		case "message":
			if line.Role == "" {
				return
			}
			recognized = true
			if line.Role == "user" && line.Content.HasText() {
				text := line.Content.Text()
				if !injectedUserMessage(text) {
					codexReleaseOpenCalls(calls, results, callOrder)
					marks = append(marks, classify.Mark{
						Seq:       len(callOrder),
						Timestamp: line.Timestamp,
						Type:      "user-message",
						Note:      strutil.TruncateRunes(text, 2000, "…"),
					})
				}
			}
		case "event_msg":
			recognized = true
			var payload codexEventMsg
			if json.Unmarshal(line.Payload, &payload) != nil {
				return
			}
			if payload.Type == "task_started" || payload.Type == "turn_started" {
				codexReleaseOpenCalls(calls, results, callOrder)
			}
			if u, ok := usageSeen.report(payload, line.Timestamp, len(callOrder)); ok {
				usage = append(usage, u)
			}
			if payload.Type == "context_compacted" {
				marks = append(marks, classify.Mark{
					Seq:       len(callOrder),
					Timestamp: line.Timestamp,
					Type:      "compaction",
				})
			}
			if codexEndsTurn(payload.Type) {
				marks = append(marks, classify.Mark{
					Seq:       len(callOrder),
					Timestamp: line.Timestamp,
					Type:      "turn-end",
				})
			}
			if payload.Type == "patch_apply_end" && payload.CallID != "" && directPatches[payload.CallID] {
				if _, exists := patchResults[payload.CallID]; !exists {
					patchResults[payload.CallID] = payload
				}
			}
		case "":
			if line.ID != "" {
				recognized = true
				meta.ID = line.ID
			}
		}
	})

	// Assemble events in call order, enriching apply_patch calls with
	// authoritative per-file change lists from patch_apply_end events.
	events := make([]classify.Event, 0, len(callOrder))
	for seq, callID := range callOrder {
		call := calls[callID]
		result := results[callID]
		if patchResult, ok := patchResults[callID]; ok {
			call.Input = applyPatchChanges(call.Input, patchResult.Changes)
			if patchResult.Success != nil {
				result.IsError = !*patchResult.Success
			}
		}
		events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, result))
	}

	if meta.Title == "" {
		meta.Title = a.titleFor(meta.ID)
	}
	if meta.Title == "" {
		meta.Title = filepath.Base(path)
	}
	if !recognized {
		return Items{}, SessionMeta{}, fmt.Errorf("not a Codex session: %s", path)
	}
	return Items{Events: events, Marks: marks, Usage: usage}, meta, err
}

// Watermark returns the offset just past the last complete line, for use as
// an incremental watermark. It is deliberately not the file size: see
// readCompleteJSONLines for why a watermark must land on a record boundary.
func (a CodexAdapter) Watermark(ctx context.Context, path string) int64 {
	return jsonlCompleteOffset(path)
}

// codexSessionHeadCwd reads the session's opening record and returns the cwd
// it declares. ParseSince starts mid-file, so it cannot see the session_meta
// record — but cwd is the base every relative path in the session resolves
// against (see sessionHeadCwd for the Claude Code twin of this argument).
func codexSessionHeadCwd(path string) string {
	line := jsonlFirstLine(path)
	if line == nil {
		return ""
	}
	var head codexRawLine
	if json.Unmarshal(line, &head) != nil {
		return ""
	}
	switch head.Type {
	case "session_meta":
		var payload codexSessionMeta
		if json.Unmarshal(head.Payload, &payload) == nil {
			return payload.Cwd
		}
	case "turn_context":
		var payload codexTurnContext
		if json.Unmarshal(head.Payload, &payload) == nil {
			return payload.Cwd
		}
	}
	return ""
}

// ParseSince reads only lines appended after the byte offset, returning
// events and marks with seq continuing from startSeq. It follows the
// ClaudeCodeAdapter.ParseSince contract: the watermark advances only to the
// last record boundary at which no call was outstanding, and events and
// marks past that point are withheld for the next poll.
//
// One Codex-specific refinement. An apply_patch issued through the custom
// tool is enriched with its authoritative per-file change list from a
// patch_apply_end event_msg — and in real transcripts that event lands on the
// line AFTER the call's output (see testdata/codex_fidelity.jsonl). A poll
// that ends on the output would therefore advance past the pair and emit the
// event unenriched, where a full Parse of the same file enriches it. The
// watermark consequently refuses to land on a line that just resolved a
// direct patch call: the next line — the patch event, if one is coming — is
// read into the same window.
func (a CodexAdapter) ParseSince(ctx context.Context, path string, offset int64, startSeq int) ([]classify.Event, []classify.Mark, SessionMeta, int64, error) {
	items, meta, wm, err := a.ParseItemsSince(ctx, path, offset, startSeq)
	return items.Events, items.Marks, meta, wm, err
}

// ParseItemsSince implements ItemParser: ParseSince, plus the usage reports
// in the same window, withheld past the watermark with everything else. A
// read resuming mid-session recovers the turn's model and the previous
// token_count from the records before the watermark (codexUsageTracker), so
// it reports what a full read would.
func (a CodexAdapter) ParseItemsSince(ctx context.Context, path string, offset int64, startSeq int) (Items, SessionMeta, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Items{}, SessionMeta{}, 0, err
	}
	// If the file shrank (truncation/rotation), reset to full parse.
	if info.Size() < offset {
		return Items{}, SessionMeta{}, 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return Items{}, SessionMeta{}, 0, err
	}
	defer func() { _ = f.Close() }()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return Items{}, SessionMeta{}, 0, err
		}
	}

	opts := a.opts
	if opts == nil {
		opts = osClassifyOptions(nil)
	}
	calls := map[string]classify.ToolCall{}
	results := map[string]classify.ToolResult{}
	callOrder := []string{}
	directPatches := map[string]bool{}
	patchResults := map[string]codexEventMsg{}
	var marks []classify.Mark
	var usage []classify.Usage
	usageSeen := newCodexUsageTracker(path, offset)
	meta := SessionMeta{
		Key:     sessionKey(string(a.Harness()), path),
		ID:      strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Harness: a.Harness(),
		Path:    path,
		Cwd:     codexSessionHeadCwd(path),
	}

	// Watermark and result counts as of the last record that left no call
	// outstanding — and no patch event pending — the point it is safe to
	// resume from.
	safeOffset, safeEvents, safeMarks, safeUsage := offset, 0, 0, 0
	// True when the line just processed resolved a direct patch call, making
	// the next line the last place its patch_apply_end can appear.
	patchCheckPending := false

	err = readCompleteJSONLines(f, offset, func(data []byte, end int64) {
		defer func() {
			if len(calls) == len(results) && !patchCheckPending {
				safeOffset, safeEvents, safeMarks, safeUsage = end, len(callOrder), len(marks), len(usage)
			}
		}()

		patchCheckPending = false
		var line codexRawLine
		if json.Unmarshal(data, &line) != nil {
			return
		}
		if line.Timestamp != "" {
			meta.EndedAt = line.Timestamp
		}
		switch line.Type {
		case "session_meta":
			var payload codexSessionMeta
			if json.Unmarshal(line.Payload, &payload) == nil {
				if payload.ID != "" {
					meta.ID = payload.ID
				}
				if payload.Cwd != "" && meta.Cwd == "" {
					meta.Cwd = payload.Cwd
				}
				if payload.Git.Branch != "" && meta.GitBranch == "" {
					meta.GitBranch = payload.Git.Branch
				}
				usageSeen.provider = payload.ModelProvider
			}
		case "turn_context":
			codexReleaseOpenCalls(calls, results, callOrder)
			var payload codexTurnContext
			if json.Unmarshal(line.Payload, &payload) == nil {
				usageSeen.turn(payload)
				if payload.Cwd != "" && meta.Cwd == "" {
					meta.Cwd = payload.Cwd
				}
				if payload.Model != "" && meta.Model == "" {
					meta.Model = payload.Model
				}
			}
		case "response_item":
			var payload codexResponseItem
			if json.Unmarshal(line.Payload, &payload) != nil {
				return
			}
			if callID, name, input, ok := decodeCodexCall(payload); ok {
				if _, exists := calls[callID]; exists {
					return
				}
				if name == "spawn_agent" {
					marks = append(marks, classify.Mark{
						Seq:       startSeq + len(callOrder),
						Timestamp: line.Timestamp,
						Type:      "subagent",
						Note:      name,
					})
				}
				calls[callID] = classify.ToolCall{
					ID:        callID,
					Name:      name,
					Input:     input,
					Timestamp: line.Timestamp,
				}
				callOrder = append(callOrder, callID)
				directPatches[callID] = payload.Type == "custom_tool_call" && name == "apply_patch"
				return
			}
			if callID, result, ok := decodeCodexOutput(payload); ok {
				if _, exists := calls[callID]; !exists {
					return
				}
				if _, exists := results[callID]; exists {
					return
				}
				results[callID] = result
				if directPatches[callID] {
					patchCheckPending = true
				}
				return
			}
			if payload.Type == "message" && payload.Role == "user" && payload.Content.HasText() {
				text := payload.Content.Text()
				if !injectedUserMessage(text) {
					codexReleaseOpenCalls(calls, results, callOrder)
					marks = append(marks, classify.Mark{
						Seq:       startSeq + len(callOrder),
						Timestamp: line.Timestamp,
						Type:      "user-message",
						Note:      strutil.TruncateRunes(text, 2000, "…"),
					})
				}
			}
		case "message":
			if line.Role == "" {
				return
			}
			if line.Role == "user" && line.Content.HasText() {
				text := line.Content.Text()
				if !injectedUserMessage(text) {
					codexReleaseOpenCalls(calls, results, callOrder)
					marks = append(marks, classify.Mark{
						Seq:       startSeq + len(callOrder),
						Timestamp: line.Timestamp,
						Type:      "user-message",
						Note:      strutil.TruncateRunes(text, 2000, "…"),
					})
				}
			}
		case "event_msg":
			var payload codexEventMsg
			if json.Unmarshal(line.Payload, &payload) != nil {
				return
			}
			if payload.Type == "task_started" || payload.Type == "turn_started" {
				codexReleaseOpenCalls(calls, results, callOrder)
			}
			if u, ok := usageSeen.report(payload, line.Timestamp, startSeq+len(callOrder)); ok {
				usage = append(usage, u)
			}
			if payload.Type == "context_compacted" {
				marks = append(marks, classify.Mark{
					Seq:       startSeq + len(callOrder),
					Timestamp: line.Timestamp,
					Type:      "compaction",
				})
			}
			if codexEndsTurn(payload.Type) {
				marks = append(marks, classify.Mark{
					Seq:       startSeq + len(callOrder),
					Timestamp: line.Timestamp,
					Type:      "turn-end",
				})
			}
			if payload.Type == "patch_apply_end" && payload.CallID != "" && directPatches[payload.CallID] {
				if _, exists := patchResults[payload.CallID]; !exists {
					patchResults[payload.CallID] = payload
				}
			}
		case "":
			if line.ID != "" {
				meta.ID = line.ID
			}
		}
	})

	// Assemble events in call order, enriching apply_patch calls exactly as
	// Parse does — same enrichment, same order, so an incrementally-read
	// window is identical to the same lines read by a full Parse.
	events := make([]classify.Event, 0, len(callOrder))
	for _, callID := range callOrder {
		result, ok := results[callID]
		if !ok {
			continue // unresolved: withheld, re-read from the last safe offset
		}
		call := calls[callID]
		if patchResult, ok := patchResults[callID]; ok {
			call.Input = applyPatchChanges(call.Input, patchResult.Changes)
			if patchResult.Success != nil {
				result.IsError = !*patchResult.Success
			}
		}
		events = append(events, classify.BuildEventWith(opts, startSeq+len(events), meta.Cwd, call, result))
	}

	if meta.Title == "" {
		meta.Title = a.titleFor(meta.ID)
	}
	if meta.Title == "" {
		meta.Title = filepath.Base(path)
	}

	return Items{Events: events[:safeEvents], Marks: marks[:safeMarks], Usage: usage[:safeUsage]}, meta, safeOffset, err
}

// codexReleaseOpenCalls settles every call still waiting on its output with a
// zero ToolResult. It runs at a record that begins a new turn — a
// task_started event or a turn_context — or carries a message the user typed.
//
// Codex writes a sampling round's tool outputs before any of those records.
// A message the user sends mid-turn, and the turn_context a mid-turn
// compaction re-records, are written only after the round and its tools have
// finished. A new turn starts only once the old one has finished or been
// aborted, and an aborted turn's task is dropped, so nothing it was still
// running can write an output afterwards. A call still open at one of these
// records will never be answered — typically the process died mid-call and
// the session was resumed — and holding the watermark for it held the whole
// session forever (#103).
//
// Parse settles calls at the same records, so both paths emit an orphan with
// an empty result in call order, the position Parse always gave it, and both
// ignore an output that turns up after its call was settled. A call that is
// merely slow is untouched: none of these records is written while its turn
// is still running.
func codexReleaseOpenCalls(calls map[string]classify.ToolCall, results map[string]classify.ToolResult, order []string) {
	if len(results) == len(calls) {
		return
	}
	for _, id := range order {
		if _, ok := results[id]; !ok {
			results[id] = classify.ToolResult{}
		}
	}
}

// codexEndsTurn reports whether an event_msg type marks the end of a turn.
// Codex persists a TurnComplete event to every rollout, serialized as
// "task_complete" (with the turn's turn_id and its last agent message);
// "turn_complete" is the alias Codex's own deserializer also accepts, the
// twin of the task_started/turn_started pair above. An aborted turn is a
// separate event (turn_aborted) and is not a turn end here.
func codexEndsTurn(eventType string) bool {
	return eventType == "task_complete" || eventType == "turn_complete"
}

// Codex-specific types.

type codexRawLine struct {
	Type      string           `json:"type"`
	Timestamp string           `json:"timestamp"`
	Payload   json.RawMessage  `json:"payload"`
	ID        string           `json:"id"`
	Role      string           `json:"role"`
	Content   codexContentList `json:"content"`
}

type codexSessionMeta struct {
	ID string `json:"id"`
	// ModelProvider is the provider id the session was opened with
	// ("openai", or a configured one).
	ModelProvider string          `json:"model_provider"`
	Cwd           string          `json:"cwd"`
	ThreadSource  string          `json:"thread_source"`
	Source        json.RawMessage `json:"source"`
	Git           struct {
		Branch     string `json:"branch"`
		CommitHash string `json:"commit_hash"`
	} `json:"git"`
}

func (p codexSessionMeta) isSubagent() bool {
	if p.ThreadSource == "subagent" {
		return true
	}
	if len(p.Source) == 0 {
		return false
	}
	var source struct {
		Subagent json.RawMessage `json:"subagent"`
	}
	if json.Unmarshal(p.Source, &source) != nil {
		return false
	}
	subagent := strings.TrimSpace(string(source.Subagent))
	return subagent != "" && subagent != "null"
}

type codexResponseItem struct {
	Type      string           `json:"type"`
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Arguments json.RawMessage  `json:"arguments"`
	Input     json.RawMessage  `json:"input"`
	CallID    string           `json:"call_id"`
	Output    any              `json:"output"`
	Role      string           `json:"role"`
	Content   codexContentList `json:"content"`
}

type codexEventMsg struct {
	Type string `json:"type"`
	// Info is a token_count event's usage; null on one that only carries
	// rate limits.
	Info    *codexTokenInfo `json:"info"`
	CallID  string          `json:"call_id"`
	Success *bool           `json:"success"`
	Changes map[string]struct {
		Type string `json:"type"`
	} `json:"changes"`
}

type codexTurnContext struct {
	Cwd   string `json:"cwd"`
	Model string `json:"model"`
}

// codexTokenInfo is a token_count event's info: the last response's usage,
// and the running total for the session.
type codexTokenInfo struct {
	Total codexTokenUsage `json:"total_token_usage"`
	Last  codexTokenUsage `json:"last_token_usage"`
}

// codexTokenUsage is Codex's TokenUsage. cached_input_tokens and
// cache_write_input_tokens are parts of input_tokens, not additions to it,
// and reasoning_output_tokens is part of output_tokens.
type codexTokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

// codexUsageTracker turns a rollout's token_count events into one
// classify.Usage per model response.
//
// Codex writes a token_count after each response, carrying that response's
// usage (last_token_usage) and the session's running total
// (total_token_usage), and writes one again when only the rate limits
// changed — same info, repeated. A repeat is recognised by its total being
// the previous token_count's total, and skipped. A token_count with no info
// (rate limits only, before any response) and one whose last response spent
// nothing (Codex writes one after filling the context window on an overflow)
// report nothing. The names are from the codex-rs protocol (TokenCountEvent,
// TokenUsageInfo, TokenUsage) and its rollout persistence policy, which
// keeps every token_count event.
//
// The model is the one the current turn's turn_context names, the provider
// the one session_meta names. Codex records no cost and no response id.
//
// ParseSince starts mid-file, so the turn_context and the previous
// token_count may both sit before its watermark. At its first token_count
// with info it looks back for whichever of the two it has not seen yet in
// its own window, and takes the provider from the session's first record.
type codexUsageTracker struct {
	path     string
	offset   int64
	provider string

	model     string
	haveModel bool
	prev      codexTokenUsage
	havePrev  bool
	lookedUp  bool
}

func newCodexUsageTracker(path string, offset int64) *codexUsageTracker {
	t := &codexUsageTracker{path: path, offset: offset}
	if offset > 0 {
		if line := jsonlFirstLine(path); line != nil {
			var head codexRawLine
			var payload codexSessionMeta
			if json.Unmarshal(line, &head) == nil && head.Type == "session_meta" &&
				json.Unmarshal(head.Payload, &payload) == nil {
				t.provider = payload.ModelProvider
			}
		}
	}
	return t
}

// turn records the model a turn_context names.
func (t *codexUsageTracker) turn(tc codexTurnContext) {
	t.model, t.haveModel = tc.Model, true
}

// report returns the Usage for an event_msg payload, at seq, when it is a
// token_count for a response not yet reported that spent anything.
func (t *codexUsageTracker) report(ev codexEventMsg, ts string, seq int) (classify.Usage, bool) {
	if ev.Type != "token_count" || ev.Info == nil {
		return classify.Usage{}, false
	}
	if t.offset > 0 && !t.lookedUp {
		t.lookedUp = true
		t.lookBack()
	}
	repeat := t.havePrev && ev.Info.Total == t.prev
	t.prev, t.havePrev = ev.Info.Total, true
	last := ev.Info.Last
	if repeat || (last.InputTokens == 0 && last.OutputTokens == 0 &&
		last.CachedInputTokens == 0 && last.CacheWriteInputTokens == 0) {
		return classify.Usage{}, false
	}
	at, _ := parseSessionTimeOk(ts)
	return classify.Usage{
		Seq:      seq,
		At:       at,
		Model:    t.model,
		Provider: t.provider,
		// Codex counts cached prompt tokens inside input_tokens; Usage keeps
		// the buckets disjoint (TokenUsage.non_cached_input does the same).
		InputTokens:  max(last.InputTokens-last.CachedInputTokens-last.CacheWriteInputTokens, 0),
		OutputTokens: last.OutputTokens,
		CacheRead:    last.CachedInputTokens,
		CacheWrite:   last.CacheWriteInputTokens,
	}, true
}

// lookBack recovers, from the records before the watermark, the turn's model
// and the previous token_count's total — each only if this read has not
// already seen a newer one.
func (t *codexUsageTracker) lookBack() {
	needModel, needPrev := !t.haveModel, !t.havePrev
	if !needModel && !needPrev {
		return
	}
	jsonlLastMatchBefore(t.path, t.offset, func(data []byte) bool {
		var line codexRawLine
		if json.Unmarshal(data, &line) != nil {
			return false
		}
		switch line.Type {
		case "turn_context":
			var tc codexTurnContext
			if needModel && json.Unmarshal(line.Payload, &tc) == nil {
				t.model, t.haveModel, needModel = tc.Model, true, false
			}
		case "event_msg":
			var ev codexEventMsg
			if needPrev && json.Unmarshal(line.Payload, &ev) == nil && ev.Type == "token_count" && ev.Info != nil {
				t.prev, t.havePrev, needPrev = ev.Info.Total, true, false
			}
		}
		return !needModel && !needPrev
	})
}

type codexContentList struct {
	Items []codexContentItem
}

func (c *codexContentList) UnmarshalJSON(data []byte) error {
	items, err := unmarshalContentList(data, func(s string) codexContentItem {
		return codexContentItem{Type: "text", Text: s}
	})
	if err != nil {
		return err
	}
	c.Items = items
	return nil
}

type codexContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (c codexContentList) Text() string {
	var parts []string
	for _, item := range c.Items {
		if strings.TrimSpace(item.Text) != "" {
			parts = append(parts, strings.TrimSpace(item.Text))
		}
	}
	return strings.Join(parts, "\n")
}

func (c codexContentList) HasText() bool {
	for _, item := range c.Items {
		if strings.TrimSpace(item.Text) != "" {
			return true
		}
	}
	return false
}

func decodeCodexCall(payload codexResponseItem) (string, string, map[string]any, bool) {
	switch payload.Type {
	case "function_call", "custom_tool_call":
	default:
		return "", "", nil, false
	}
	callID := payload.CallID
	if callID == "" {
		callID = payload.ID
	}
	if callID == "" || payload.Name == "" {
		return "", "", nil, false
	}
	var raw json.RawMessage
	switch payload.Type {
	case "function_call":
		raw = payload.Arguments
	case "custom_tool_call":
		raw = payload.Input
	}
	return callID, payload.Name, parseCodexInput(raw), true
}

func decodeCodexOutput(payload codexResponseItem) (string, classify.ToolResult, bool) {
	switch payload.Type {
	case "function_call_output", "custom_tool_call_output":
	default:
		return "", classify.ToolResult{}, false
	}
	if payload.CallID == "" {
		return "", classify.ToolResult{}, false
	}
	output := classify.ContentToString(payload.Output)
	return payload.CallID, classify.ToolResult{
		Content: output,
		IsError: commandOutputFailed(output),
	}, true
}

func parseCodexInput(raw json.RawMessage) map[string]any {
	input := map[string]any{}
	if len(raw) == 0 || string(raw) == "null" {
		return input
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		input["_raw"] = string(raw)
		return input
	}
	switch v := value.(type) {
	case map[string]any:
		return v
	case string:
		return parseCodexInputText(v)
	default:
		encoded, _ := json.Marshal(v)
		input["_raw"] = string(encoded)
		return input
	}
}

func parseCodexInputText(text string) map[string]any {
	input := map[string]any{}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return input
	}
	if json.Unmarshal([]byte(trimmed), &input) == nil {
		return input
	}
	var nested string
	if json.Unmarshal([]byte(trimmed), &nested) == nil && nested != text {
		return parseCodexInputText(nested)
	}
	input["_raw"] = text
	return input
}

var exitCodeRe = regexp.MustCompile(`(?im)^(?:Process exited with code|Exit code:)\s*([0-9]+)\s*$`)

// commandOutputFailed infers failure from command output text. Codex doesn't
// set an explicit error flag like Claude Code does, so we estimate.
// Ported verbatim from mindwalk internal/adapter/codex/adapter.go.
func commandOutputFailed(output string) bool {
	trimmed := strings.TrimSpace(output)
	var envelope struct {
		ExitCode *int  `json:"exit_code"`
		TimedOut *bool `json:"timed_out"`
		Metadata struct {
			ExitCode *int `json:"exit_code"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(trimmed), &envelope) == nil {
		if envelope.ExitCode != nil {
			return *envelope.ExitCode != 0
		}
		if envelope.Metadata.ExitCode != nil {
			return *envelope.Metadata.ExitCode != 0
		}
		if envelope.TimedOut != nil && *envelope.TimedOut {
			return true
		}
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "apply_patch verification failed") {
		return true
	}
	firstLine := trimmed
	if newline := strings.IndexByte(firstLine, '\n'); newline >= 0 {
		firstLine = firstLine[:newline]
	}
	status := strings.ToLower(strings.TrimSpace(firstLine))
	switch {
	case strings.HasPrefix(status, "script completed"), strings.HasPrefix(status, "script running"):
		return false
	case strings.HasPrefix(status, "script failed"):
		return true
	}
	header := trimmed
	for _, marker := range []string{"\nOutput:\n", "\nFinal output:\n"} {
		if index := strings.Index(header, marker); index >= 0 {
			header = header[:index]
		}
	}
	for line := range strings.SplitSeq(header, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "aborted by user") {
			return true
		}
	}
	match := exitCodeRe.FindStringSubmatch(header)
	return len(match) == 2 && match[1] != "0"
}

func applyPatchChanges(input map[string]any, changes map[string]struct {
	Type string `json:"type"`
}) map[string]any {
	if len(changes) == 0 {
		return input
	}
	merged := make(map[string]any, len(input)+1)
	maps.Copy(merged, input)
	patch := ""
	for _, key := range []string{"patch", "input", "_raw"} {
		if value, ok := input[key].(string); ok {
			patch = value
			break
		}
	}
	if patch != "" && !strings.HasSuffix(patch, "\n") {
		patch += "\n"
	}
	paths := make([]string, 0, len(changes))
	for path := range changes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		operation := "Update"
		switch strings.ToLower(changes[path].Type) {
		case "add":
			operation = "Add"
		case "delete":
			operation = "Delete"
		}
		patch += fmt.Sprintf("*** %s File: %s\n", operation, path)
	}
	merged["patch"] = patch
	return merged
}

func (a CodexAdapter) indexPath() string {
	if a.IndexPath != "" {
		return a.IndexPath
	}
	if a.Dir != "" {
		return filepath.Join(filepath.Dir(a.Dir), "session_index.jsonl")
	}
	return homeDir(".codex", "session_index.jsonl")
}

func (a CodexAdapter) titleFor(id string) string {
	if id == "" {
		return ""
	}
	index := a.indexPath()
	if index == "" {
		return ""
	}
	return loadCodexTitleIndex(index)[id]
}

var codexTitleIndexCache struct {
	mu      sync.Mutex
	path    string
	size    int64
	modTime time.Time
	titles  map[string]string
}

func loadCodexTitleIndex(path string) map[string]string {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	codexTitleIndexCache.mu.Lock()
	defer codexTitleIndexCache.mu.Unlock()
	if codexTitleIndexCache.path == path && codexTitleIndexCache.size == info.Size() && codexTitleIndexCache.modTime.Equal(info.ModTime()) {
		return codexTitleIndexCache.titles
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	titles := map[string]string{}
	_ = ReadJSONLines(f, func(data []byte) {
		var row struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(data, &row) == nil && row.ID != "" && row.ThreadName != "" {
			titles[row.ID] = row.ThreadName
		}
	})
	codexTitleIndexCache.path = path
	codexTitleIndexCache.size = info.Size()
	codexTitleIndexCache.modTime = info.ModTime()
	codexTitleIndexCache.titles = titles
	return titles
}
