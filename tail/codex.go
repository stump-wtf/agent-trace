package tail

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
)

// CodexAdapter discovers and parses OpenAI Codex session logs from
// ~/.codex/sessions/. Each session is a .jsonl file with response_item lines.
type CodexAdapter struct {
	Dir       string // override default session directory
	IndexPath string // override session_index.jsonl location for title resolution
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

// WithRoot returns a copy of the adapter with Dir and IndexPath set to
// their respective locations under root/.codex/sessions. Pass an empty
// string to restore the default.
func (a CodexAdapter) WithRoot(root string) Adapter {
	if root == "" {
		return CodexAdapter{}
	}
	sessionsDir := filepath.Join(root, ".codex", "sessions")
	return CodexAdapter{
		Dir:       sessionsDir,
		IndexPath: filepath.Join(sessionsDir, "session_index.jsonl"),
	}
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
	opts := osClassifyOptions()
	calls := map[string]classify.ToolCall{}
	results := map[string]classify.ToolResult{}
	callOrder := []string{}
	directPatches := map[string]bool{}
	patchResults := map[string]codexEventMsg{}
	var marks []classify.Mark

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
					marks = append(marks, classify.Mark{
						Seq:       len(callOrder),
						Timestamp: line.Timestamp,
						Type:      "user-message",
						Note:      truncateRunes(text, 2000, "…"),
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
					marks = append(marks, classify.Mark{
						Seq:       len(callOrder),
						Timestamp: line.Timestamp,
						Type:      "user-message",
						Note:      truncateRunes(text, 2000, "…"),
					})
				}
			}
		case "event_msg":
			recognized = true
			var payload codexEventMsg
			if json.Unmarshal(line.Payload, &payload) != nil {
				return
			}
			if payload.Type == "context_compacted" {
				marks = append(marks, classify.Mark{
					Seq:       len(callOrder),
					Timestamp: line.Timestamp,
					Type:      "compaction",
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
		return nil, nil, SessionMeta{}, fmt.Errorf("not a Codex session: %s", path)
	}
	return events, marks, meta, err
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
	ID           string          `json:"id"`
	Cwd          string          `json:"cwd"`
	ThreadSource string          `json:"thread_source"`
	Source       json.RawMessage `json:"source"`
	Git          struct {
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
	Type    string `json:"type"`
	CallID  string `json:"call_id"`
	Success *bool  `json:"success"`
	Changes map[string]struct {
		Type string `json:"type"`
	} `json:"changes"`
}

type codexTurnContext struct {
	Cwd   string `json:"cwd"`
	Model string `json:"model"`
}

type codexContentList struct {
	Items []codexContentItem
}

func (c *codexContentList) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		c.Items = []codexContentItem{{Type: "text", Text: s}}
		return nil
	}
	var items []codexContentItem
	if err := json.Unmarshal(data, &items); err != nil {
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
	for _, line := range strings.Split(header, "\n") {
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
	for key, value := range input {
		merged[key] = value
	}
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
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "session_index.jsonl")
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
	defer f.Close()
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
