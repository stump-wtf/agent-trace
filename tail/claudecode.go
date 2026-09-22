package tail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/internal/strutil"
)

// ClaudeCodeAdapter discovers and parses Claude Code session logs from
// ~/.claude/projects/. Each project subdirectory contains .jsonl session files.
type ClaudeCodeAdapter struct {
	Dir string // override default session directory
	// opts carries classify.Options from the watcher (verify patterns, etc).
	// When nil, Parse falls back to osClassifyOptions.
	opts *classify.Options
	// cache memoizes summaries across scans so an unchanged session file is
	// not re-read. When nil, every scan summarizes from scratch.
	cache *SummaryCache
}

func (a ClaudeCodeAdapter) Harness() Harness { return HarnessClaudeCode }

// SetOptions injects classify.Options for verify-pattern customization.
func (a *ClaudeCodeAdapter) SetOptions(opts *classify.Options) { a.opts = opts }

// SetSummaryCache injects the summary cache the watcher shares across scans.
func (a *ClaudeCodeAdapter) SetSummaryCache(c *SummaryCache) { a.cache = c }

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
		return &a
	}
	a.Dir = filepath.Join(root, ".claude", "projects")
	return &a
}

// ListSessions walks the session directory and returns metadata for each
// recognized Claude Code session file, sorted newest-first.
//
// It delegates to ListSessionsFiltered with the zero filter, which is
// documented to match every session, so the two can never disagree.
func (a ClaudeCodeAdapter) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	return a.ListSessionsFiltered(ctx, SessionFilter{})
}

// ListSessionsFiltered implements FilteredLister. A file whose modification
// time already puts it outside the filter's time bounds is skipped on the stat
// alone, so a bounded view never pays to open, read, or parse the history it is
// excluding — which on a large archive is nearly all of it.
//
// filterSessions still runs over the result: the mtime check only narrows what
// is read, and the exact predicate is applied in exactly one place, so this can
// never disagree with ListSessions followed by in-memory filtering.
func (a ClaudeCodeAdapter) ListSessionsFiltered(ctx context.Context, f SessionFilter) ([]SessionMeta, error) {
	dir := a.SessionDir()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, nil
	}
	var metas []SessionMeta
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		// Checked per entry rather than once up front: the walk is the part
		// that scales with the number of sessions, and Summarize opens and
		// reads each file. Returning the error stops the walk and surfaces
		// context.Canceled to the caller instead of a truncated listing that
		// looks like "this harness has fewer sessions than it does".
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" || strings.HasPrefix(filepath.Base(path), "agent-") {
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

// Summarize reads just enough of a session file to extract metadata without
// parsing every event: a bounded head plus, for a file that exceeds that
// budget, a bounded tail for the closing timestamp. A file inside the head
// budget is read whole, so its summary matches a full scan exactly.
//
// The elided middle costs at most a Title. An ai-title line past the head
// budget is not seen and the title falls back to the file name, which is the
// same fallback a session that never had one already gets. Fields that decide
// whether a session is listed at all are unaffected: the sidechain stamp that
// sets Auxiliary is written on the first line of every session that has one.
//
// EndedAt is the one field the tail read can fail to recover — a final line
// larger than the tail window leaves the window holding no whole line at all —
// and it is also the field ActiveSince decides listing on, so a stale one
// removes the session from discovery entirely. When the scan reports it never
// saw the final line, this dates the session by its mtime rather than by
// whatever timestamp the head happened to stop on, which would be the top of
// the file reported as its last activity. mtime is the right substitute
// because it is the same signal mtimeExcludes already trusts to skip a file
// unread: if it is sound enough to exclude a session without opening it, it is
// sound enough to date one whose tail could not be read.
//
// The context is checked before the file is opened; the read itself runs to
// completion, bounded by the budget, per the Adapter cancellation contract.
func (a ClaudeCodeAdapter) Summarize(ctx context.Context, path string) (SessionMeta, error) {
	if err := ctx.Err(); err != nil {
		return SessionMeta{}, err
	}
	f, meta, err := openJSONLSession(a.Harness(), path)
	if err != nil {
		return SessionMeta{}, err
	}
	defer func() { _ = f.Close() }()

	recognized := false
	scan, err := scanJSONLSummary(f, func(data []byte) {
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
		// A failed API call's "<synthetic>" model is not the session's.
		if len(line.Message) > 0 && !isCCAPIError(line) {
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
	if !scan.SawFinalLine {
		if info, statErr := f.Stat(); statErr == nil {
			meta.EndedAt = info.ModTime().UTC().Format(time.RFC3339Nano)
		}
	}
	if !recognized {
		return SessionMeta{}, fmt.Errorf("not a Claude Code session: %s", path)
	}
	return meta, err
}

// Parse reads a complete session file and returns all tool call/result pairs
// as classified events, plus timeline marks.
func (a ClaudeCodeAdapter) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
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
		if isCCAPIError(line) {
			// A failed call is not a model turn: it holds no tool calls, and
			// its "<synthetic>" model must not become the session's model.
			marks = append(marks, ccAPIErrorMark(line, seq))
			return
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
func (a ClaudeCodeAdapter) Watermark(ctx context.Context, path string) int64 {
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
func (a ClaudeCodeAdapter) ParseSince(ctx context.Context, path string, offset int64, startSeq int) ([]classify.Event, []classify.Mark, SessionMeta, int64, error) {
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
		if isCCAPIError(line) {
			// A failed call is not a model turn: it holds no tool calls, and
			// its "<synthetic>" model must not become the session's model.
			marks = append(marks, ccAPIErrorMark(line, seq))
			return
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

	// The three fields below describe a failed API call; see isCCAPIError.
	// APIError and APIErrorStatus stay raw because they are not one type
	// across record kinds: "error" is a string code on the synthetic
	// assistant record, but an object on the system records Claude Code
	// writes for each retry (subtype "api_error"). Typed as a string, every
	// one of those retry lines would fail to decode and be dropped whole —
	// its timestamp, session ID and all.
	IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
	APIError          json.RawMessage `json:"error"`
	APIErrorStatus    json.RawMessage `json:"apiErrorStatus"`
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

// isCCAPIError reports whether a record is the one Claude Code writes when an
// API call finally fails: a synthetic assistant message (model "<synthetic>")
// flagged isApiErrorMessage, carrying an error code, the HTTP status when
// there was one, and the text Claude Code shows the user. For a run that
// stalled on quota, auth or an outage it is the only record of why.
//
// Detection is on the flag alone, never on the text. The text is not a
// reliable signal — quota and auth messages do not start with "API Error:"
// at all — and matching it would also misfire on an agent that merely
// quotes one. Claude Code versions that predate the flag therefore produce
// no error marks, which is the honest result: they did not say.
//
// The per-attempt system records (subtype "api_error", with retryAttempt
// and maxRetries) are deliberately not errors here: a retry that later
// succeeds is not a failed call, and a call that exhausts its retries still
// ends in one of these assistant records.
func isCCAPIError(line ccRawLine) bool {
	return line.Type == "assistant" && line.IsAPIErrorMessage
}

// ccAPIErrorMark renders a failed API call as an "error" mark: at the given
// seq (the shared seq space every Claude Code mark uses — the seq of the
// next tool event), dated by the record's own timestamp, noted by
// ccAPIErrorNote.
func ccAPIErrorMark(line ccRawLine, seq int) classify.Mark {
	var msg ccMessage
	// A message that is missing or fails to decode still leaves the code and
	// status, which are the parts consumers classify on, so it is not a
	// reason to drop the mark.
	_ = json.Unmarshal(line.Message, &msg)
	var code string
	_ = json.Unmarshal(line.APIError, &code)
	var status int
	_ = json.Unmarshal(line.APIErrorStatus, &status)
	return classify.Mark{
		Seq:       seq,
		Timestamp: line.Timestamp,
		Type:      "error",
		// ccUserMessageText only joins the text items; nothing in it is
		// specific to user messages.
		Note: ccAPIErrorNote(code, status, ccUserMessageText(msg.Content)),
	}
}

// ccAPIErrorNote formats a failed API call's note so that its front is
// machine-readable and its tail is Claude Code's own message:
//
//	<code> (<status>): <text>    rate_limit (429): You've hit your session limit
//	<code>: <text>               server_error: API Error: Unable to connect to API
//
// <code> is the record's "error" field (server_error, rate_limit,
// authentication_failed, unknown, ...), or "unknown" when the record has
// none. It never contains whitespace, a colon or a parenthesis — any such
// rune is replaced with "_" — so the first space or colon always ends it,
// and it is capped at 64 runes so the front always fits in the note.
// " (<status>)" is present only when Claude Code got an HTTP response;
// connection failures have no status and omit it. ": <text>" is omitted when
// the record carries no text.
//
// This shape is a contract, not presentation: harness's Prometheus exporter
// sorts error marks into quota, auth, timeout, transport and other by
// matching the code and status at the front, so change it only together with
// that classifier. The note is truncated to 2000 runes, as the Crush reader's
// error note is; the front is never what gets cut.
func ccAPIErrorNote(code string, status int, text string) string {
	code = strings.Map(func(r rune) rune {
		switch r {
		case ':', '(', ')':
			return '_'
		}
		if unicode.IsSpace(r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(code))
	code = strutil.TruncateRunes(code, 64, "…")
	if code == "" {
		code = "unknown"
	}
	note := code
	if status > 0 {
		note += " (" + strconv.Itoa(status) + ")"
	}
	if text = strings.TrimSpace(text); text != "" {
		note += ": " + text
	}
	return strutil.TruncateRunes(note, 2000, "…")
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
