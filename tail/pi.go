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

// PiAdapter discovers and parses Pi agent session logs from
// ~/.pi/agent/sessions/. Pi sessions are append-only trees that get
// linearized before parsing.
type PiAdapter struct {
	Dir string // override default session directory
	// opts carries classify.Options from the watcher (verify patterns, etc).
	opts *classify.Options
	// cache memoizes summaries across scans so an unchanged session file is
	// not re-read. When nil, every scan summarizes from scratch.
	cache *SummaryCache
}

func (a PiAdapter) Harness() Harness { return HarnessPi }

// SetOptions injects classify.Options for verify-pattern customization.
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
	return filepath.Join(home, ".pi", "agent", "sessions")
}

// WithRoot returns a copy of the adapter that discovers sessions under root,
// which is treated as a HOME-like base: the adapter appends its own layout
// (.pi/agent/sessions). Sibling fields are preserved; only the root-derived
// path changes. Pass an empty string to restore the default.
func (a PiAdapter) WithRoot(root string) Adapter {
	if root == "" {
		a.Dir = ""
		return &a
	}
	a.Dir = filepath.Join(root, ".pi", "agent", "sessions")
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
	header, entries, recognized, err := readPiSession(path)
	if err != nil && !recognized {
		return SessionMeta{}, err
	}
	if !recognized {
		return SessionMeta{}, fmt.Errorf("not a pi session: %s", path)
	}
	meta := a.piBaseMeta(path, header)
	for _, entry := range linearizePi(entries) {
		if entry.Timestamp != "" {
			if meta.StartedAt == "" {
				meta.StartedAt = entry.Timestamp
			}
			meta.EndedAt = entry.Timestamp
		}
		if entry.Type == "model_change" && entry.ModelID != "" {
			meta.Model = entry.ModelID
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
		meta.Title = piSessionTitle(entries, "", path)
	}
	return meta, err
}

// Parse reads a complete Pi session file and returns classified events.
func (a PiAdapter) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
	header, entries, recognized, err := readPiSession(path)
	if err != nil && !recognized {
		return nil, nil, SessionMeta{}, err
	}
	if !recognized {
		return nil, nil, SessionMeta{}, fmt.Errorf("not a pi session: %s", path)
	}
	meta := a.piBaseMeta(path, header)

	opts := a.opts
	if opts == nil {
		opts = osClassifyOptions(nil)
	}
	pending := map[string]classify.ToolCall{}
	pendingOrder := []string{}
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
			if entry.ModelID != "" {
				meta.Model = entry.ModelID
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
					if _, exists := pending[call.ID]; !exists {
						pendingOrder = append(pendingOrder, call.ID)
					}
					pending[call.ID] = call
				}
			case "toolResult":
				call, ok := pending[msg.ToolCallID]
				if !ok {
					continue
				}
				delete(pending, msg.ToolCallID)
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
	// Flush orphaned tool calls.
	for _, id := range pendingOrder {
		if call, ok := pending[id]; ok {
			events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, classify.ToolResult{}))
			seq++
		}
	}
	meta.Title = piSessionTitle(entries, firstUserText, path)
	return events, marks, meta, err
}

// Watermark returns the offset just past the last complete line, for use as
// an incremental watermark. It is deliberately not the file size: see
// readCompleteJSONLines for why a watermark must land on a record boundary.
func (a PiAdapter) Watermark(ctx context.Context, path string) int64 {
	return jsonlCompleteOffset(path)
}

// piSessionHead reads the session's header record — the first line, which
// carries the session id, cwd and opening timestamp. ParseSince starts
// mid-file and needs it for the same reason ClaudeCodeAdapter needs
// sessionHeadCwd: the metadata before the watermark is what new events are
// attributed to.
func piSessionHead(path string) (piRawEntry, bool) {
	line := jsonlFirstLine(path)
	if line == nil {
		return piRawEntry{}, false
	}
	if !isPiHeader(line) {
		return piRawEntry{}, false
	}
	var header piRawEntry
	if json.Unmarshal(line, &header) != nil {
		return piRawEntry{}, false
	}
	return header, true
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

	header, _ := piSessionHead(path)
	meta := a.piBaseMeta(path, header)

	opts := a.opts
	if opts == nil {
		opts = osClassifyOptions(nil)
	}
	pending := map[string]classify.ToolCall{}
	var events []classify.Event
	var marks []classify.Mark
	firstUserText := ""
	seq := startSeq

	// Offset and result counts as of the last record that left no tool call
	// outstanding — the ClaudeCodeAdapter.ParseSince withhold rule. Orphaned
	// calls are deliberately NOT flushed here, unlike Parse: an unresolved
	// call is re-read from the last safe offset next poll and emitted once
	// its result lands, rather than emitted empty and then again complete.
	safeIdx, safeEvents, safeMarks := -1, 0, 0

	for i, entry := range newEntries {
		if entry.Timestamp != "" {
			meta.EndedAt = entry.Timestamp
		}
		switch entry.Type {
		case "model_change":
			if entry.ModelID != "" {
				meta.Model = entry.ModelID
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
					pending[call.ID] = call
				}
			case "toolResult":
				call, ok := pending[msg.ToolCallID]
				if !ok {
					continue
				}
				delete(pending, msg.ToolCallID)
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
		if len(pending) == 0 {
			safeIdx, safeEvents, safeMarks = i, len(events), len(marks)
		}
	}

	safeOffset := offset
	if safeIdx >= 0 {
		safeOffset = ends[safeIdx]
	}
	if meta.Title == "" {
		meta.Title = piSessionTitle(newEntries, firstUserText, path)
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

func readPiSession(path string) (header piRawEntry, entries []piRawEntry, recognized bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return piRawEntry{}, nil, false, err
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
			sawEntry = true
			if isPiHeader(data) {
				header = entry
				recognized = true
			}
			return
		}
		if recognized && entry.Type != "" {
			entries = append(entries, entry)
		}
	})
	return header, entries, recognized, err
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
// wins (an empty name explicitly clears), else the first user message
// previewed to 240 runes, else filepath.Base(path).
func piSessionTitle(entries []piRawEntry, firstUserText, path string) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type != "session_info" {
			continue
		}
		if entries[i].Name != "" {
			return entries[i].Name
		}
		break
	}
	if firstUserText != "" {
		preview := strings.Join(strings.Fields(firstUserText), " ")
		return strutil.TruncateRunes(preview, 240, "…")
	}
	return filepath.Base(path)
}
