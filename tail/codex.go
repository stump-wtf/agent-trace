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

// CodexAdapter discovers and parses OpenAI Codex session logs from
// ~/.codex/sessions/. Each session is a .jsonl file with response_item lines.
type CodexAdapter struct {
	Dir string // override default session directory
}

func (a CodexAdapter) Harness() Harness { return HarnessCodex }

func (a CodexAdapter) SessionDir() string {
	if a.Dir != "" {
		return a.Dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

// ListSessions walks the session directory and returns metadata for each
// recognized Codex session file, sorted newest-first.
func (a CodexAdapter) ListSessions() ([]SessionMeta, error) {
	dir := a.SessionDir()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, nil
	}
	var metas []SessionMeta
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		meta, err := a.Summarize(path)
		if err == nil {
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

// Summarize extracts metadata from a Codex session file without full parsing.
func (a CodexAdapter) Summarize(path string) (SessionMeta, error) {
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
		var line codexRawLine
		if json.Unmarshal(data, &line) != nil {
			return
		}
		if line.Type == "session_meta" {
			recognized = true
			var payload codexSessionMeta
			if json.Unmarshal(line.Payload, &payload) == nil {
				if payload.ID != "" {
					meta.ID = payload.ID
				}
				if payload.Cwd != "" {
					meta.Cwd = payload.Cwd
				}
			}
		}
		if line.Timestamp != "" {
			if meta.StartedAt == "" {
				meta.StartedAt = line.Timestamp
			}
			meta.EndedAt = line.Timestamp
		}
	})
	if !recognized {
		return SessionMeta{}, fmt.Errorf("not a Codex session: %s", path)
	}
	return meta, err
}

// Parse reads a complete Codex session file and returns classified events.
func (a CodexAdapter) Parse(path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
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
	calls := map[string]classify.ToolCall{}
	results := map[string]classify.ToolResult{}
	callOrder := []string{}
	var marks []classify.Mark
	seq := 0

	err = ReadJSONLines(f, func(data []byte) {
		var line codexRawLine
		if json.Unmarshal(data, &line) != nil {
			return
		}
		if line.Type == "session_meta" {
			recognized = true
			var payload codexSessionMeta
			if json.Unmarshal(line.Payload, &payload) == nil {
				if payload.ID != "" {
					meta.ID = payload.ID
				}
				if payload.Cwd != "" {
					meta.Cwd = payload.Cwd
				}
			}
			return
		}
		if line.Timestamp != "" {
			if meta.StartedAt == "" {
				meta.StartedAt = line.Timestamp
			}
			meta.EndedAt = line.Timestamp
		}
		if line.Type == "response_item" {
			var payload codexResponseItem
			if json.Unmarshal(line.Payload, &payload) != nil {
				return
			}
			switch payload.Type {
			case "function_call", "custom_tool_call":
				callID, name, input := decodeCodexCall(payload)
				if callID == "" {
					return
				}
				call := classify.ToolCall{
					ID:        callID,
					Name:      name,
					Input:     input,
					Timestamp: line.Timestamp,
				}
				if _, exists := calls[callID]; !exists {
					callOrder = append(callOrder, callID)
				}
				calls[callID] = call
			case "function_call_output", "custom_tool_call_output":
				callID, result := decodeCodexOutput(payload)
				if callID == "" {
					return
				}
				results[callID] = result
			}
		}
		if line.Type == "message" {
			var msg codexMessage
			if json.Unmarshal(line.Payload, &msg) == nil && msg.Role == "user" {
				text := strings.TrimSpace(msg.Content)
				if text != "" && !injectedUserMessage(text) {
					marks = append(marks, classify.Mark{
						Seq:  seq,
						Type: "user-message",
						Note: truncateRunes(text, 2000, "…"),
					})
				}
			}
		}
	})
	// Assemble events in call order.
	for _, callID := range callOrder {
		call := calls[callID]
		result := results[callID] // zero value if no result
		events := append(([]classify.Event)(nil), classify.BuildEvent(seq, meta.Cwd, call, result))
		_ = events
		seq++
	}
	if !recognized {
		return nil, nil, SessionMeta{}, fmt.Errorf("not a Codex session: %s", path)
	}
	// Rebuild events properly.
	var allEvents []classify.Event
	seq = 0
	for _, callID := range callOrder {
		call := calls[callID]
		result := results[callID]
		allEvents = append(allEvents, classify.BuildEvent(seq, meta.Cwd, call, result))
		seq++
	}
	return allEvents, marks, meta, err
}

// Codex-specific types.

type codexRawLine struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type codexSessionMeta struct {
	ID  string `json:"id"`
	Cwd string `json:"cwd"`
}

type codexResponseItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	Output    any             `json:"output"`
}

type codexMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func decodeCodexCall(payload codexResponseItem) (string, string, map[string]any) {
	callID := payload.CallID
	if callID == "" {
		callID = payload.ID
	}
	if callID == "" {
		return "", "", nil
	}
	name := payload.Name
	var raw json.RawMessage
	switch payload.Type {
	case "function_call":
		raw = payload.Arguments
	case "custom_tool_call":
		raw = payload.Input
	}
	input := parseCodexInput(raw)
	return callID, name, input
}

func decodeCodexOutput(payload codexResponseItem) (string, classify.ToolResult) {
	callID := payload.CallID
	if callID == "" {
		return "", classify.ToolResult{}
	}
	output := classify.ContentToString(payload.Output)
	return callID, classify.ToolResult{
		Content: output,
		IsError: commandOutputFailed(output),
	}
}

func parseCodexInput(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	// Try as string (some Codex calls serialize args as JSON string).
	var s string
	if json.Unmarshal(raw, &s) == nil {
		var inner map[string]any
		if json.Unmarshal([]byte(s), &inner) == nil {
			return inner
		}
		return map[string]any{"_raw": s}
	}
	return nil
}

// commandOutputFailed infers failure from command output text. Codex doesn't
// set an explicit error flag like Claude Code does, so we estimate.
func commandOutputFailed(output string) bool {
	lower := strings.ToLower(output)
	patterns := []string{
		"exit code 1", "exit code 2", "command failed",
		"error:", "fatal:", "panic:", "traceback",
		"script failed", "timed out",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}
