package tail

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
)

// ClaudeCodeAdapter discovers and parses Claude Code session logs from
// ~/.claude/projects/. Each project subdirectory contains .jsonl session files.
type ClaudeCodeAdapter struct {
	Dir string // override default session directory
}

func (a ClaudeCodeAdapter) Harness() Harness { return HarnessClaudeCode }

func (a ClaudeCodeAdapter) SessionDir() string {
	if a.Dir != "" {
		return a.Dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
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
	f, err := os.Open(path)
	if err != nil {
		return SessionMeta{}, err
	}
	defer f.Close()

	id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	meta := SessionMeta{
		Key:     sessionKey(string(a.Harness()), path),
		ID:      id,
		Harness: a.Harness(),
		Path:    path,
	}
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
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, SessionMeta{}, err
	}
	defer f.Close()

	id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	meta := SessionMeta{
		Key:     sessionKey(string(a.Harness()), path),
		ID:      id,
		Harness: a.Harness(),
		Path:    path,
	}

	recognized := false
	opts := osClassifyOptions()
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
					Note: truncateRunes(text, 2000, "…"),
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
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		c.Items = []ccContentItem{{Type: "text", Text: s}}
		return nil
	}
	var items []ccContentItem
	if err := json.Unmarshal(data, &items); err != nil {
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
