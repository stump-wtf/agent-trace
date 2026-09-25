package tail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/internal/strutil"
)

// HarnessOMP identifies oh-my-pi (OMP) sessions: a PiAdapter with OMP set
// labels what it reads with it. OMP is a fork of Pi that keeps Pi's session
// format and adds to it — a leading title slot (parsePiTitleSlot), a
// "provider/model" model_change (piEntryModel), and entry types and message
// roles the reader passes over — so one reader serves both, and the label is
// the caller's to choose. No adapter in DefaultAdapters uses it.
const HarnessOMP Harness = "omp"

// PiAdapter discovers and parses Pi agent session logs from
// ~/.pi/agent/sessions/. Pi sessions are append-only trees that get
// linearized before parsing.
//
// It also reads oh-my-pi (OMP) session files, which open with a fixed-width
// title slot line before the session header. Set OMP to label those sessions
// HarnessOMP rather than HarnessPi and to default to OMP's directory.
type PiAdapter struct {
	Dir string // override default session directory
	// OMP makes the adapter an oh-my-pi reader: Harness() — and so every
	// SessionMeta.Harness and session key it produces — is HarnessOMP, and
	// the default directory is ~/.omp/agent/sessions. Parsing is identical
	// either way; a Pi reader also accepts OMP files and vice versa.
	OMP bool
	// opts carries classify.Options from the watcher (verify patterns, etc).
	opts *classify.Options
	// cache memoizes summaries across scans so an unchanged session file is
	// not re-read. When nil, every scan summarizes from scratch.
	cache *SummaryCache
}

// Harness returns HarnessPi, or HarnessOMP when OMP is set.
func (a PiAdapter) Harness() Harness {
	if a.OMP {
		return HarnessOMP
	}
	return HarnessPi
}

// agentDir is the adapter's agent directory relative to a HOME-like root.
func (a PiAdapter) agentDir() string {
	if a.OMP {
		return filepath.Join(".omp", "agent")
	}
	return filepath.Join(".pi", "agent")
}

// SetOptions injects classify.Options for verify patterns and error excerpts.
func (a *PiAdapter) SetOptions(opts *classify.Options) { a.opts = opts }

// SetSummaryCache injects the summary cache the watcher shares across scans.
func (a *PiAdapter) SetSummaryCache(c *SummaryCache) { a.cache = c }

// Diagnostics checks whether the Pi session directory exists and is readable.
func (a PiAdapter) Diagnostics() []DiagnosticCheck {
	return dirDiagnostics(a.SessionDir())
}

func (a PiAdapter) SessionDir() string {
	if a.Dir != "" {
		return a.Dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, a.agentDir(), "sessions")
}

// WithRoot returns a copy of the adapter that discovers sessions under root,
// which is treated as a HOME-like base: the adapter appends its own layout
// (.pi/agent/sessions, or .omp/agent/sessions when OMP is set). Sibling
// fields are preserved; only the root-derived path changes. Pass an empty
// string to restore the default.
func (a PiAdapter) WithRoot(root string) Adapter {
	if root == "" {
		a.Dir = ""
		return &a
	}
	a.Dir = filepath.Join(root, a.agentDir(), "sessions")
	return &a
}

// ListSessions walks the session directory and returns metadata for each
// recognized Pi session file, sorted newest-first.
// It delegates to ListSessionsFiltered with the zero filter, which is
// documented to match every session, so the two can never disagree.
func (a PiAdapter) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	return a.ListSessionsFiltered(ctx, SessionFilter{})
}

// ListSessionsFiltered implements FilteredLister. A file whose modification
// time already puts it outside the filter's time bounds is skipped on the stat
// alone — see ClaudeCodeAdapter.ListSessionsFiltered and mtimeExcludes for why
// that is exact rather than approximate.
func (a PiAdapter) ListSessionsFiltered(ctx context.Context, f SessionFilter) ([]SessionMeta, error) {
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
		if err == nil {
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

// Summarize extracts metadata from a Pi session file. The context is checked
// before the file is opened; the read itself runs to completion, bounded by
// that file's size, per the Adapter cancellation contract.
func (a PiAdapter) Summarize(ctx context.Context, path string) (SessionMeta, error) {
	if err := ctx.Err(); err != nil {
		return SessionMeta{}, err
	}
	head, entries, recognized, err := readPiSession(path)
	if err != nil && !recognized {
		return SessionMeta{}, err
	}
	if !recognized {
		return SessionMeta{}, fmt.Errorf("not a pi session: %s", path)
	}
	meta := a.piBaseMeta(path, head.header)
	for _, entry := range linearizePi(entries) {
		if entry.Timestamp != "" {
			if meta.StartedAt == "" {
				meta.StartedAt = entry.Timestamp
			}
			meta.EndedAt = entry.Timestamp
		}
		if entry.Type == "model_change" {
			if model := piEntryModel(entry); model != "" {
				meta.Model = model
			}
		}
		if entry.Type == "session_info" && entry.Name != "" {
			meta.Title = entry.Name
		}
		if entry.Type == "message" {
			var msg piMessage
			if json.Unmarshal(entry.Message, &msg) != nil {
				continue
			}
			if msg.Role == "assistant" && msg.Model != "" && meta.Model == "" {
				meta.Model = msg.Model
			}
		}
	}
	if meta.Title == "" {
		meta.Title = piSessionTitle(entries, head.storedTitle(), "", path)
	}
	return meta, err
}

// Parse reads a complete Pi session file and returns classified events.
func (a PiAdapter) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
	head, entries, recognized, err := readPiSession(path)
	if err != nil && !recognized {
		return nil, nil, SessionMeta{}, err
	}
	if !recognized {
		return nil, nil, SessionMeta{}, fmt.Errorf("not a pi session: %s", path)
	}
	meta := a.piBaseMeta(path, head.header)

	opts := a.opts
	if opts == nil {
		opts = osClassifyOptions(nil)
	}
	pending := newPendingCalls[classify.ToolCall]()
	var events []classify.Event
	var marks []classify.Mark
	firstUserText := ""
	seq := 0

	for _, entry := range linearizePi(entries) {
		if entry.Timestamp != "" {
			if meta.StartedAt == "" {
				meta.StartedAt = entry.Timestamp
			}
			meta.EndedAt = entry.Timestamp
		}
		switch entry.Type {
		case "model_change":
			if model := piEntryModel(entry); model != "" {
				meta.Model = model
			}
		case "compaction":
			marks = append(marks, classify.Mark{Seq: seq, Timestamp: entry.Timestamp, Type: "compaction"})
		case "branch_summary":
			marks = append(marks, classify.Mark{
				Seq:       seq,
				Timestamp: entry.Timestamp,
				Type:      "compaction",
				Note:      strutil.TruncateRunes("branch: "+entry.Summary, 2000, "…"),
			})
		case "custom_message":
			// Extension-injected context, not a real user turn.
		case "message":
			var msg piMessage
			if json.Unmarshal(entry.Message, &msg) != nil {
				continue
			}
			// A call this message proves can never be answered is emitted
			// here, ahead of anything the message itself contributes — see
			// piSupersedes.
			if piSupersedes(msg) {
				for _, call := range pending.release(func(classify.ToolCall) bool { return true }) {
					events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, classify.ToolResult{}))
					seq++
				}
			}
			switch msg.Role {
			case "user":
				text := piContentText(msg.Content)
				if !injectedUserMessage(text) {
					if firstUserText == "" {
						firstUserText = text
					}
					marks = append(marks, classify.Mark{
						Seq:       seq,
						Timestamp: entry.Timestamp,
						Type:      "user-message",
						Note:      strutil.TruncateRunes(text, 2000, "…"),
					})
				}
			case "assistant":
				if msg.Model != "" && meta.Model == "" {
					meta.Model = msg.Model
				}
				for _, block := range piContentBlocks(msg.Content) {
					if block.Type != "toolCall" || block.ID == "" {
						continue
					}
					call := classify.ToolCall{
						ID:        block.ID,
						Name:      block.Name,
						Input:     block.Arguments,
						Timestamp: entry.Timestamp,
					}
					pending.put(call.ID, call)
				}
			case "toolResult":
				call, ok := pending.take(msg.ToolCallID)
				if !ok {
					continue
				}
				events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, classify.ToolResult{
					Content: piContentText(msg.Content),
					IsError: msg.IsError,
				}))
				seq++
			case "bashExecution":
				call := classify.ToolCall{
					Name:      "bash",
					Input:     map[string]any{"command": msg.Command},
					Timestamp: entry.Timestamp,
				}
				isErr := msg.ExitCode != nil && *msg.ExitCode != 0
				events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, classify.ToolResult{
					Content: msg.Output,
					IsError: isErr,
				}))
				seq++
			}
		}
	}
	// Flush the calls still open at the end of the session, last and in issue
	// order. Nothing after them proves they are dead, so they may yet be
	// answered — a live session's calls in flight look exactly like this.
	for _, call := range pending.release(func(classify.ToolCall) bool { return true }) {
		events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, classify.ToolResult{}))
		seq++
	}
	meta.Title = piSessionTitle(entries, head.storedTitle(), firstUserText, path)
	return events, marks, meta, err
}

// Watermark returns the offset just past the last complete line, for use as
// an incremental watermark. It is deliberately not the file size: see
// readCompleteJSONLines for why a watermark must land on a record boundary.
func (a PiAdapter) Watermark(ctx context.Context, path string) int64 {
	return jsonlCompleteOffset(path)
}

// piSessionHead reads the session's header record — the first line, or the
// second when the first is an OMP title slot — which carries the session id,
// cwd and opening timestamp, along with the slot's title. ParseSince starts
// mid-file and needs it for the same reason ClaudeCodeAdapter needs
// sessionHeadCwd: the metadata before the watermark is what new events are
// attributed to. It reads those two lines and no further.
func piSessionHead(path string) (piHead, bool) {
	lines := jsonlLeadingLines(path, 2)
	if len(lines) == 0 {
		return piHead{}, false
	}
	var head piHead
	if title, ok := parsePiTitleSlot(lines[0]); ok {
		head.slotTitle, head.slotted = title, true
		lines = lines[1:]
	}
	if len(lines) == 0 || !isPiHeader(lines[0]) {
		return piHead{}, false
	}
	if json.Unmarshal(lines[0], &head.header) != nil {
		return piHead{}, false
	}
	return head, true
}

// ParseSince reads only records appended after the byte offset, returning
// events and marks with seq continuing from startSeq.
//
// Pi is not an append-only transcript: its records form a tree, and Parse
// linearizes by walking the parent chain from the last record back to the
// root. Appends therefore come in two shapes. A linear append — each new
// record's parent the previous leaf — extends the chain, and the appended
// records ARE the delta; they are processed directly, and the byte-offset
// watermark behaves like any other JSONL adapter's (withheld past the last
// record that left no tool call outstanding). A branch — a new record whose
// parent is some earlier node, which is what pi writes when a turn is edited
// and resubmitted — re-linearizes the whole chain, and the appended bytes are
// no longer the delta. That case is detected by comparing each new record's
// parent against the expected chain position, and it falls back to a full
// Parse whose result is trimmed to the seq the watcher has already emitted:
// byte-for-byte what the non-incremental path would have produced, at the
// cost of one full read on the rare poll that lands after a branch.
func (a PiAdapter) ParseSince(ctx context.Context, path string, offset int64, startSeq int) ([]classify.Event, []classify.Mark, SessionMeta, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, SessionMeta{}, 0, err
	}
	// If the file shrank (truncation/rotation), reset to full parse.
	if info.Size() < offset {
		return nil, nil, SessionMeta{}, 0, nil
	}

	var newEntries []piRawEntry
	var ends []int64
	err = func() error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if offset > 0 {
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				return err
			}
		}
		return readCompleteJSONLines(f, offset, func(data []byte, end int64) {
			var entry piRawEntry
			if json.Unmarshal(data, &entry) != nil {
				return
			}
			newEntries = append(newEntries, entry)
			ends = append(ends, end)
		})
	}()
	if err != nil {
		return nil, nil, SessionMeta{}, 0, err
	}
	if len(newEntries) == 0 {
		return nil, nil, SessionMeta{}, offset, nil
	}

	// Continuation check: the record consumed just before the watermark is
	// the previous leaf; a linear append chains onto it.
	prevID := ""
	if tail := jsonlLastLineBefore(path, offset); tail != nil {
		var prev piRawEntry
		if json.Unmarshal(tail, &prev) == nil {
			prevID = prev.ID
		}
	}
	linear := true
	for _, entry := range newEntries {
		if entry.ID == "" {
			continue // v1 records carry no ids and no tree structure
		}
		if entry.ParentID != prevID {
			linear = false
			break
		}
		prevID = entry.ID
	}
	if !linear {
		events, marks, meta, err := a.Parse(ctx, path)
		if err != nil {
			return nil, nil, SessionMeta{}, 0, err
		}
		if startSeq < len(events) {
			events = events[startSeq:]
		} else {
			events = nil
		}
		return events, marks, meta, jsonlCompleteOffset(path), nil
	}

	head, _ := piSessionHead(path)
	meta := a.piBaseMeta(path, head.header)

	opts := a.opts
	if opts == nil {
		opts = osClassifyOptions(nil)
	}
	pending := newPendingCalls[classify.ToolCall]()
	var events []classify.Event
	var marks []classify.Mark
	firstUserText := ""
	seq := startSeq

	// Offset and result counts as of the last record that left no tool call
	// outstanding — the ClaudeCodeAdapter.ParseSince withhold rule. A call a
	// later message proves dead is released in position, exactly as Parse
	// releases it (piSupersedes); a call still open at the end of the window
	// is deliberately NOT flushed here, unlike Parse: it is re-read from the
	// last safe offset next poll and emitted once its result lands, rather
	// than emitted empty and then again complete.
	safeIdx, safeEvents, safeMarks := -1, 0, 0

	for i, entry := range newEntries {
		if entry.Timestamp != "" {
			meta.EndedAt = entry.Timestamp
		}
		switch entry.Type {
		case "model_change":
			if model := piEntryModel(entry); model != "" {
				meta.Model = model
			}
		case "compaction":
			marks = append(marks, classify.Mark{Seq: seq, Timestamp: entry.Timestamp, Type: "compaction"})
		case "branch_summary":
			marks = append(marks, classify.Mark{
				Seq:       seq,
				Timestamp: entry.Timestamp,
				Type:      "compaction",
				Note:      strutil.TruncateRunes("branch: "+entry.Summary, 2000, "…"),
			})
		case "custom_message":
			// Extension-injected context, not a real user turn.
		case "session_info":
			if entry.Name != "" {
				meta.Title = entry.Name
			}
		case "message":
			var msg piMessage
			if json.Unmarshal(entry.Message, &msg) != nil {
				continue
			}
			// A call this message proves can never be answered is emitted
			// here, ahead of anything the message itself contributes — see
			// piSupersedes.
			if piSupersedes(msg) {
				for _, call := range pending.release(func(classify.ToolCall) bool { return true }) {
					events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, classify.ToolResult{}))
					seq++
				}
			}
			switch msg.Role {
			case "user":
				text := piContentText(msg.Content)
				if !injectedUserMessage(text) {
					if firstUserText == "" {
						firstUserText = text
					}
					marks = append(marks, classify.Mark{
						Seq:       seq,
						Timestamp: entry.Timestamp,
						Type:      "user-message",
						Note:      strutil.TruncateRunes(text, 2000, "…"),
					})
				}
			case "assistant":
				if msg.Model != "" && meta.Model == "" {
					meta.Model = msg.Model
				}
				for _, block := range piContentBlocks(msg.Content) {
					if block.Type != "toolCall" || block.ID == "" {
						continue
					}
					call := classify.ToolCall{
						ID:        block.ID,
						Name:      block.Name,
						Input:     block.Arguments,
						Timestamp: entry.Timestamp,
					}
					pending.put(call.ID, call)
				}
			case "toolResult":
				call, ok := pending.take(msg.ToolCallID)
				if !ok {
					continue
				}
				events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, classify.ToolResult{
					Content: piContentText(msg.Content),
					IsError: msg.IsError,
				}))
				seq++
			case "bashExecution":
				call := classify.ToolCall{
					Name:      "bash",
					Input:     map[string]any{"command": msg.Command},
					Timestamp: entry.Timestamp,
				}
				isErr := msg.ExitCode != nil && *msg.ExitCode != 0
				events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, classify.ToolResult{
					Content: msg.Output,
					IsError: isErr,
				}))
				seq++
			}
		}
		if pending.len() == 0 {
			safeIdx, safeEvents, safeMarks = i, len(events), len(marks)
		}
	}

	safeOffset := offset
	if safeIdx >= 0 {
		safeOffset = ends[safeIdx]
	}
	if meta.Title == "" {
		meta.Title = piSessionTitle(newEntries, head.storedTitle(), firstUserText, path)
	}
	return events[:safeEvents], marks[:safeMarks], meta, safeOffset, nil
}

// Pi-specific types.

type piRawEntry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  string          `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Cwd       string          `json:"cwd"`
	Message   json.RawMessage `json:"message"`
	ModelID   string          `json:"modelId"`
	Summary   string          `json:"summary"`
	Name      string          `json:"name"`
	// Title is the header's own title (OMP writes one on legacy, slot-less
	// files). It is read from the header only: an OMP title_change entry
	// carries a title too, but that is an audit record, and the title slot
	// already holds the current title.
	Title string `json:"title"`
	// Model and Role are OMP's model_change shape: "provider/modelId" and
	// the model role it selects. Pi writes ModelID instead.
	Model string `json:"model"`
	Role  string `json:"role"`
}

// piHead is what precedes a Pi session's entries: the header and, on an OMP
// file, the title slot before it.
type piHead struct {
	header    piRawEntry
	slotTitle string
	slotted   bool // the file opened with an OMP title slot
}

// storedTitle is the title the file itself records: the OMP slot's current
// title, else the header's. Empty on a Pi file.
func (h piHead) storedTitle() string {
	if h.slotted && h.slotTitle != "" {
		return h.slotTitle
	}
	return h.header.Title
}

// parsePiTitleSlot recognises OMP's title slot, the physical first line of
// every current OMP session file: {"type":"title","v":1,"title":…,
// "source"?:…,"updatedAt":…,"pad":"<spaces>"}, space-padded to 256 bytes
// with its newline (SESSION_TITLE_SLOT_BYTES) and rewritten in place at that
// width whenever the session is renamed. The fixed width is why a rename
// never moves a byte offset after the slot; the reader does not depend on
// it, and skips the slot as one line whatever its length.
//
// It accepts exactly what OMP's own parseTitleSlotObject accepts (oh-my-pi
// 62bc57be, packages/coding-agent/src/session/session-title-slot.ts): type
// "title", v 1, string title/updatedAt/pad, and source absent or
// "auto"/"user". Anything else is not a slot, and so is left to fail header
// recognition.
func parsePiTitleSlot(data []byte) (title string, ok bool) {
	var probe struct {
		Type      string          `json:"type"`
		V         json.RawMessage `json:"v"`
		Title     *string         `json:"title"`
		Source    json.RawMessage `json:"source"`
		UpdatedAt *string         `json:"updatedAt"`
		Pad       *string         `json:"pad"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return "", false
	}
	if probe.Type != "title" || string(probe.V) != "1" {
		return "", false
	}
	if probe.Title == nil || probe.UpdatedAt == nil || probe.Pad == nil {
		return "", false
	}
	if len(probe.Source) > 0 {
		var source string
		if json.Unmarshal(probe.Source, &source) != nil || (source != "auto" && source != "user") {
			return "", false
		}
	}
	return *probe.Title, true
}

// piEntryModel is the model a model_change entry selects, as a model id
// without its provider — the form Pi's modelId and an assistant message's
// model both take. Pi writes modelId; OMP writes model as "provider/modelId"
// plus an optional role, and only the default role (an absent role is
// "default") is the session's model — a change under another role ("smol",
// "slow", …) sets the model for that role instead (oh-my-pi 62bc57be,
// docs/session.md "model_change" and "Context Reconstruction",
// packages/coding-agent/src/session/session-entries.ts ModelChangeEntry).
func piEntryModel(entry piRawEntry) string {
	if entry.ModelID != "" {
		return entry.ModelID
	}
	if entry.Model == "" || (entry.Role != "" && entry.Role != "default") {
		return ""
	}
	if _, id, ok := strings.Cut(entry.Model, "/"); ok && id != "" {
		return id
	}
	return entry.Model
}

type piMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Model      string          `json:"model"`
	ToolCallID string          `json:"toolCallId"`
	IsError    bool            `json:"isError"`
	Command    string          `json:"command"`
	Output     string          `json:"output"`
	ExitCode   *int            `json:"exitCode"`
}

type piContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// piSupersedes reports whether msg, read after a pending tool call on the
// same linearized chain, proves the call will never receive a result. Any
// user or assistant message does. Pi's agent loop writes the results of a
// response's calls before the turn ends — a response cut off at the token
// limit gets an error result per call rather than none — and only then
// writes the next user or assistant message: steering the user typed
// mid-turn included. The one exception writes fewer results, never later
// ones: an abort mid-batch stops the executor (sequential or parallel) at
// the call it reached, and the calls after it are never run and never
// answered. It defers everything else that could land mid-turn (an
// extension's custom message, a user's own bash run, an oh-my-pi eval run)
// to the end of the turn for exactly this reason: providers reject a
// transcript with anything between a call and its result. So a call still
// pending at the next user or assistant message was never run — its
// response was aborted or errored mid-stream, and Pi runs no call from such
// a response, or the user aborted its batch before reaching it — or its
// process died running it. oh-my-pi, resuming a session whose process died,
// writes an aborted assistant message that closes the turn.
//
// Checked against pi-mono's packages/agent/src/agent-loop.ts (runLoop,
// executeToolCalls) and packages/coding-agent's AgentSession persistence,
// and oh-my-pi's fork of both (createInterruptedTurnAbortMessage). No local
// Pi store was available to replay, so unlike the Claude Code rule this one
// rests on the source, not on transcripts.
func piSupersedes(msg piMessage) bool {
	return msg.Role == "user" || msg.Role == "assistant"
}

func isPiHeader(data []byte) bool {
	var probe struct {
		Type string          `json:"type"`
		ID   json.RawMessage `json:"id"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return false
	}
	return probe.Type == "session" && len(probe.ID) > 0 && probe.ID[0] == '"'
}

// piBaseMeta builds the initial SessionMeta for a Pi session from the file
// path and parsed header entry.
func (a PiAdapter) piBaseMeta(path string, header piRawEntry) SessionMeta {
	id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if header.ID != "" {
		id = header.ID
	}
	return SessionMeta{
		Key:       sessionKey(string(a.Harness()), path),
		ID:        id,
		Harness:   a.Harness(),
		Path:      path,
		Cwd:       header.Cwd,
		StartedAt: header.Timestamp,
		EndedAt:   header.Timestamp,
	}
}

// readPiSession reads a whole Pi or OMP session file. The file is a session
// only if its first JSON record is the header — or, on an OMP file, if its
// first is a title slot and its second the header. One slot is skipped, and
// only in first position: anything else ahead of the header, a second slot
// included, means the file is not a session.
func readPiSession(path string) (head piHead, entries []piRawEntry, recognized bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return piHead{}, nil, false, err
	}
	defer func() { _ = f.Close() }()
	sawEntry := false
	err = ReadJSONLines(f, func(data []byte) {
		var entry piRawEntry
		if json.Unmarshal(data, &entry) != nil {
			if !json.Valid(data) {
				return
			}
			sawEntry = true
			return
		}
		if !sawEntry {
			if !head.slotted {
				if title, ok := parsePiTitleSlot(data); ok {
					head.slotTitle, head.slotted = title, true
					return
				}
			}
			sawEntry = true
			if isPiHeader(data) {
				head.header = entry
				recognized = true
			}
			return
		}
		if recognized && entry.Type != "" {
			entries = append(entries, entry)
		}
	})
	return head, entries, recognized, err
}

// linearizePi walks the parentId chain from the last entry back to root,
// producing chronological order. V1 files without IDs pass through as-is.
func linearizePi(entries []piRawEntry) []piRawEntry {
	leaf := -1
	index := map[string]int{}
	for i, entry := range entries {
		if entry.ID != "" {
			leaf = i
			index[entry.ID] = i
		}
	}
	if leaf < 0 {
		return entries
	}
	var path []int
	visited := map[int]bool{}
	cur := leaf
	for !visited[cur] {

		visited[cur] = true
		path = append(path, cur)
		parent := entries[cur].ParentID
		if parent == "" {
			break
		}
		next, ok := index[parent]
		if !ok {
			break
		}
		cur = next
	}
	ordered := make([]piRawEntry, 0, len(path))
	for i := len(path) - 1; i >= 0; i-- {
		ordered = append(ordered, entries[path[i]])
	}
	return ordered
}

func piContentBlocks(raw json.RawMessage) []piContentBlock {
	if len(raw) == 0 {
		return nil
	}
	var blocks []piContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return []piContentBlock{{Type: "text", Text: s}}
	}
	return nil
}

func piContentText(raw json.RawMessage) string {
	var parts []string
	for _, block := range piContentBlocks(raw) {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, strings.TrimSpace(block.Text))
		}
	}
	return strings.Join(parts, "\n")
}

// piSessionTitle mirrors pi's getSessionName: the latest session_info entry
// wins (an empty name explicitly clears), else the title the file stores (an
// OMP title slot or header — see piHead.storedTitle), else the first user
// message previewed to 240 runes, else filepath.Base(path). Pi files store
// no title, so for them stored is empty and the rule is pi's unchanged.
func piSessionTitle(entries []piRawEntry, stored, firstUserText, path string) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type != "session_info" {
			continue
		}
		if entries[i].Name != "" {
			return entries[i].Name
		}
		break
	}
	if stored != "" {
		return stored
	}
	if firstUserText != "" {
		preview := strings.Join(strings.Fields(firstUserText), " ")
		return strutil.TruncateRunes(preview, 240, "…")
	}
	return filepath.Base(path)
}
