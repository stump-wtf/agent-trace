package tail

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
	"gitea.stump.rocks/stump.wtf/agent-trace/internal/strutil"
)

// ClaudeCodeAdapter discovers and parses Claude Code session logs from
// ~/.claude/projects/. Each project subdirectory contains .jsonl session files.
type ClaudeCodeAdapter struct {
	Dir string // override default session directory
	// opts carries classify.Options from the watcher (verify patterns, etc).
	// When nil, Parse falls back to osClassifyOptions.
	opts *classify.Options
}

func (a ClaudeCodeAdapter) Harness() Harness { return HarnessClaudeCode }

// SetOptions injects classify.Options for verify-pattern customization.
func (a *ClaudeCodeAdapter) SetOptions(opts *classify.Options) { a.opts = opts }

// Diagnostics checks whether the Claude Code session directory exists and is readable.
func (a ClaudeCodeAdapter) Diagnostics() []DiagnosticCheck {
	dir := a.SessionDir()
	return dirDiagnostics(dir)
}

func (a ClaudeCodeAdapter) SessionDir() string {
	if a.Dir != "" {
		return a.Dir
	}
	return homeDir(".claude", "projects")
}

// WithRoot returns a copy of the adapter that discovers sessions under root,
// which is treated as a HOME-like base: the adapter appends its own layout
// (.claude/projects). Sibling fields are preserved; only the root-derived path
// changes. Pass an empty string to restore the default location.
func (a ClaudeCodeAdapter) WithRoot(root string) Adapter {
	if root == "" {
		a.Dir = ""
		return a
	}
	a.Dir = filepath.Join(root, ".claude", "projects")
	return a
}

// ListSessions walks the session directory and returns metadata for each
// recognized Claude Code session file, sorted newest-first.
func (a ClaudeCodeAdapter) ListSessions() ([]SessionMeta, error) {
	dir := a.SessionDir()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, nil
	}
	var metas []SessionMeta
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" || strings.HasPrefix(filepath.Base(path), "agent-") {
			return nil
		}
		meta, err := a.Summarize(path)
		if err == nil && !meta.Auxiliary {
			metas = append(metas, meta)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].EndedAt > metas[j].EndedAt
	})
	return metas, nil
}

// Summarize reads just enough of a session file to extract metadata without
// parsing every event.
func (a ClaudeCodeAdapter) Summarize(path string) (SessionMeta, error) {
	f, meta, err := openJSONLSession(a.Harness(), path)
	if err != nil {
		return SessionMeta{}, err
	}
	defer func() { _ = f.Close() }()

	recognized := false
	err = ReadJSONLines(f, func(data []byte) {
		var line ccRawLine
		if json.Unmarshal(data, &line) != nil {
			return
		}
		if isCCLine(line) {
			recognized = true
		}
		if line.SessionID != "" {
			meta.ID = line.SessionID
		}
		if line.Timestamp != "" {
			if meta.StartedAt == "" {
				meta.StartedAt = line.Timestamp
			}
			meta.EndedAt = line.Timestamp
		}
		if line.Type == "ai-title" && line.AITitle != "" {
			meta.Title = line.AITitle
		}
		if line.Cwd != "" && meta.Cwd == "" {
			meta.Cwd = line.Cwd
		}
		if line.GitBranch != "" && meta.GitBranch == "" {
			meta.GitBranch = line.GitBranch
		}
		if line.IsSidechain {
			meta.IsSidechain = true
			meta.Auxiliary = true
		}
		if line.AgentID != "" && meta.AgentID == "" {
			meta.AgentID = line.AgentID
		}
		if len(line.Message) > 0 {
			var msg ccMessage
			if json.Unmarshal(line.Message, &msg) == nil {
				if msg.Model != "" && meta.Model == "" {
					meta.Model = msg.Model
				}
			}
		}
	})
	if meta.Title == "" {
		meta.Title = filepath.Base(path)
	}
	if !recognized {
		return SessionMeta{}, fmt.Errorf("not a Claude Code session: %s", path)
	}
	return meta, err
}

// Parse reads a complete session file and returns all tool call/result pairs
// as classified events, plus timeline marks.
func (a ClaudeCodeAdapter) Parse(path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
	f, meta, err := openJSONLSession(a.Harness(), path)
	if err != nil {
		return nil, nil, SessionMeta{}, err
	}
	defer func() { _ = f.Close() }()

	recognized := false
	opts := a.opts
	if opts == nil {
		opts = osClassifyOptions(nil)
	}
	pending := map[string]classify.ToolCall{}
	pendingOrder := []string{}
	var events []classify.Event
	var marks []classify.Mark
	seq := 0

	err = ReadJSONLines(f, func(data []byte) {
		var line ccRawLine
		if json.Unmarshal(data, &line) != nil {
			return
		}
		if isCCLine(line) {
			recognized = true
		}
		if line.SessionID != "" {
			meta.ID = line.SessionID
		}
		if line.Cwd != "" && meta.Cwd == "" {
			meta.Cwd = line.Cwd
		}
		if line.GitBranch != "" && meta.GitBranch == "" {
			meta.GitBranch = line.GitBranch
		}
		if line.IsSidechain {
			meta.IsSidechain = true
			meta.Auxiliary = true
		}
		if line.AgentID != "" && meta.AgentID == "" {
			meta.AgentID = line.AgentID
		}
		if line.Timestamp != "" {
			if meta.StartedAt == "" {
				meta.StartedAt = line.Timestamp
			}
			meta.EndedAt = line.Timestamp
		}
		if line.Type == "ai-title" && line.AITitle != "" {
			meta.Title = line.AITitle
			return
		}
		if isCCCompaction(line) {
			marks = append(marks, classify.Mark{Seq: seq, Type: "compaction"})
		}
		if len(line.Message) == 0 {
			return
		}
		var msg ccMessage
		if json.Unmarshal(line.Message, &msg) != nil {
			return
		}
		if line.Type == "user" && hasCCUserMessage(msg.Content) {
			text := ccUserMessageText(msg.Content)
			if !injectedUserMessage(text) {
				marks = append(marks, classify.Mark{
					Seq:  seq,
					Type: "user-message",
					Note: strutil.TruncateRunes(text, 2000, "…"),
				})
			}
		}
		if msg.Model != "" && meta.Model == "" {
			meta.Model = msg.Model
		}
		for _, item := range msg.Content.Items {
			switch item.Type {
			case "tool_use":
				call := classify.ToolCall{
					ID:        item.ID,
					Name:      item.Name,
					Input:     item.Input,
					Timestamp: line.Timestamp,
				}
				if call.Name == "Task" || call.Name == "Agent" {
					marks = append(marks, classify.Mark{Seq: seq, Type: "subagent", Note: call.Name})
				}
				if _, exists := pending[call.ID]; !exists {
					pendingOrder = append(pendingOrder, call.ID)
				}
				pending[call.ID] = call
			case "tool_result":
				call, ok := pending[item.ToolUseID]
				if !ok {
					continue
				}
				delete(pending, item.ToolUseID)
				result := classify.ToolResult{
					Content: classify.ContentToString(item.Content),
					IsError: item.IsError,
				}
				events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, result))
				seq++
			}
		}
	})
	// Flush orphaned tool calls (no result received).
	for _, id := range pendingOrder {
		if call, ok := pending[id]; ok {
			events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, classify.ToolResult{}))
			seq++
		}
	}
	if meta.Title == "" {
		meta.Title = filepath.Base(path)
	}
	if !recognized {
		return nil, nil, SessionMeta{}, fmt.Errorf("not a Claude Code session: %s", path)
	}
	return events, marks, meta, err
}

// Watermark returns the offset just past the last complete line, for use as an
// incremental watermark. It is deliberately not the file size: see
// readCompleteJSONLines for why a watermark must land on a record boundary.
func (a ClaudeCodeAdapter) Watermark(path string) int64 {
	return jsonlCompleteOffset(path)
}

// sessionHeadCwd reads the first record of a JSONL session and returns the cwd
// it declares.
//
// ParseSince starts mid-file, so it cannot see the session's opening record —
// but cwd is the base every relative path in the session resolves against, and
// classify.BuildEventWith silently classifies everything against "" without it.
// The head record is one small bounded read and every harness writes cwd there,
// so seeding from it keeps an incrementally-parsed event classified identically
// to the same event from a full Parse.
func sessionHeadCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	cwd := ""
	_ = readCompleteJSONLines(f, 0, func(data []byte, _ int64) {
		if cwd != "" {
			return
		}
		var line ccRawLine
		if json.Unmarshal(data, &line) == nil {
			cwd = line.Cwd
		}
	})
	return cwd
}

// ParseSince reads only lines appended after the byte offset, returning events
// and marks with seq continuing from startSeq. Used by the watcher to avoid
// re-reading the entire file on every poll.
//
// The watermark it returns is not simply "where reading stopped". A tool_use
// and its tool_result are separate records written seconds apart, so the poll
// that sees the call routinely ends before the result exists. Advancing past
// the call would drop it permanently: the next poll starts after it, finds a
// tool_result with no matching call in its fresh pending map, and discards it —
// so exactly the long-running commands worth watching produced no event at all.
//
// Instead the watermark advances only to the last record boundary at which no
// call was outstanding, and events and marks past that point are withheld. The
// unresolved tail is re-read next poll and emitted once the result lands, which
// keeps the stream both complete and duplicate-free. A call that never gets a
// result — a session killed mid-tool — parks the watermark on a short tail that
// is cheap to re-read, rather than corrupting everything after it.
func (a ClaudeCodeAdapter) ParseSince(path string, offset int64, startSeq int) ([]classify.Event, []classify.Mark, SessionMeta, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, SessionMeta{}, 0, err
	}
	// If the file shrank (truncation/rotation), reset to full parse.
	if info.Size() < offset {
		return nil, nil, SessionMeta{}, 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, SessionMeta{}, 0, err
	}
	defer func() { _ = f.Close() }()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, nil, SessionMeta{}, 0, err
		}
	}

	opts := a.opts
	if opts == nil {
		opts = osClassifyOptions(nil)
	}
	pending := map[string]classify.ToolCall{}
	var events []classify.Event
	var marks []classify.Mark
	seq := startSeq
	meta := SessionMeta{
		Key:     sessionKey(string(a.Harness()), path),
		ID:      strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Harness: a.Harness(),
		Path:    path,
		Cwd:     sessionHeadCwd(path),
	}

	// Watermark and result counts as of the last record that left no call
	// outstanding — the point it is safe to resume from.
	safeOffset, safeEvents, safeMarks := offset, 0, 0

	err = readCompleteJSONLines(f, offset, func(data []byte, end int64) {
		defer func() {
			if len(pending) == 0 {
				safeOffset, safeEvents, safeMarks = end, len(events), len(marks)
			}
		}()

		var line ccRawLine
		if json.Unmarshal(data, &line) != nil {
			return
		}
		if line.SessionID != "" {
			meta.ID = line.SessionID
		}
		if line.Cwd != "" && meta.Cwd == "" {
			meta.Cwd = line.Cwd
		}
		if line.GitBranch != "" && meta.GitBranch == "" {
			meta.GitBranch = line.GitBranch
		}
		if line.IsSidechain {
			meta.IsSidechain = true
			meta.Auxiliary = true
		}
		if line.AgentID != "" && meta.AgentID == "" {
			meta.AgentID = line.AgentID
		}
		if line.Timestamp != "" {
			meta.EndedAt = line.Timestamp
		}
		if line.Type == "ai-title" && line.AITitle != "" {
			meta.Title = line.AITitle
			return
		}
		if isCCCompaction(line) {
			marks = append(marks, classify.Mark{Seq: seq, Type: "compaction"})
		}
		if len(line.Message) == 0 {
			return
		}
		var msg ccMessage
		if json.Unmarshal(line.Message, &msg) != nil {
			return
		}
		if line.Type == "user" && hasCCUserMessage(msg.Content) {
			text := ccUserMessageText(msg.Content)
			if !injectedUserMessage(text) {
				marks = append(marks, classify.Mark{
					Seq:       seq,
					Timestamp: line.Timestamp,
					Type:      "user-message",
					Note:      strutil.TruncateRunes(text, 2000, "…"),
				})
			}
		}
		if msg.Model != "" && meta.Model == "" {
			meta.Model = msg.Model
		}
		for _, item := range msg.Content.Items {
			switch item.Type {
			case "tool_use":
				call := classify.ToolCall{
					ID:        item.ID,
					Name:      item.Name,
					Input:     item.Input,
					Timestamp: line.Timestamp,
				}
				if call.Name == "Task" || call.Name == "Agent" {
					marks = append(marks, classify.Mark{Seq: seq, Type: "subagent", Note: call.Name})
				}
				pending[item.ID] = call
			case "tool_result":
				call, ok := pending[item.ToolUseID]
				if !ok {
					continue
				}
				delete(pending, item.ToolUseID)
				result := classify.ToolResult{
					Content: classify.ContentToString(item.Content),
					IsError: item.IsError,
				}
				events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, call, result))
				seq++
			}
		}
	})
	if meta.Title == "" {
		meta.Title = filepath.Base(path)
	}

	return events[:safeEvents], marks[:safeMarks], meta, safeOffset, err
}

// Tail-specific types for Claude Code JSONL format.

type ccRawLine struct {
	Type        string          `json:"type"`
	Timestamp   string          `json:"timestamp"`
	SessionID   string          `json:"sessionId"`
	AgentID     string          `json:"agentId"`
	IsSidechain bool            `json:"isSidechain"`
	Cwd         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	Message     json.RawMessage `json:"message"`
	AITitle     string          `json:"aiTitle"`
	Subtype     string          `json:"subtype"`
}

type ccMessage struct {
	Role    string        `json:"role"`
	Model   string        `json:"model"`
	Content ccContentList `json:"content"`
}

type ccContentList struct {
	Items []ccContentItem
}

func (c *ccContentList) UnmarshalJSON(data []byte) error {
	items, err := unmarshalContentList(data, func(s string) ccContentItem {
		return ccContentItem{Type: "text", Text: s}
	})
	if err != nil {
		return err
	}
	c.Items = items
	return nil
}

type ccContentItem struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Input     map[string]any `json:"input"`
	ToolUseID string         `json:"tool_use_id"`
	Content   any            `json:"content"`
	IsError   bool           `json:"is_error"`
	Text      string         `json:"text"`
}

func isCCLine(line ccRawLine) bool {
	if line.SessionID != "" {
		return true
	}
	switch line.Type {
	case "user", "assistant", "system", "ai-title":
		return line.Timestamp != "" || len(line.Message) > 0
	default:
		return false
	}
}

func isCCCompaction(line ccRawLine) bool {
	return line.Type == "system" && strings.Contains(strings.ToLower(line.Subtype), "compact")
}

func hasCCUserMessage(content ccContentList) bool {
	if len(content.Items) == 0 {
		return false
	}
	for _, item := range content.Items {
		if item.Type == "tool_result" {
			return false
		}
		if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
			return true
		}
	}
	return true
}

func ccUserMessageText(content ccContentList) string {
	var parts []string
	for _, item := range content.Items {
		if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
			parts = append(parts, strings.TrimSpace(item.Text))
		}
	}
	return strings.Join(parts, "\n")
}
