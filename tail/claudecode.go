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

// SetOptions injects classify.Options for verify patterns and error excerpts.
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
// as classified events, plus timeline marks. It is ParseItems without the
// usage reports.
func (a ClaudeCodeAdapter) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, SessionMeta, error) {
	items, meta, err := a.ParseItems(ctx, path)
	return items.Events, items.Marks, meta, err
}

// ParseItems implements ItemParser: Parse, plus one classify.Usage per API
// response that recorded usage (see ccUsageTracker).
func (a ClaudeCodeAdapter) ParseItems(ctx context.Context, path string) (Items, SessionMeta, error) {
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
	var events []classify.Event
	var marks []classify.Mark
	var usage []classify.Usage
	usageSeen := newCCUsageTracker("", 0)
	rec := newCCRecorder(opts, &meta,
		func(e classify.Event) { events = append(events, e) },
		func(m classify.Mark) { marks = append(marks, m) },
		usageSeen, func(u classify.Usage) { usage = append(usage, u) })

	err = ReadJSONLines(f, func(data []byte) {
		var line ccRawLine
		if json.Unmarshal(data, &line) != nil {
			return
		}
		if isCCLine(line) {
			recognized = true
		}
		ccFoldMeta(&meta, line)
		rec.record(line)
	})
	// Flush the calls still open at the end of the session, last and in issue
	// order. Nothing after them proves they are dead, so they may yet be
	// answered — a live session's calls in flight look exactly like this.
	rec.flush()
	if meta.Title == "" {
		meta.Title = filepath.Base(path)
	}
	if !recognized {
		return Items{}, SessionMeta{}, fmt.Errorf("not a Claude Code session: %s", path)
	}
	return Items{Events: events, Marks: marks, Usage: usage}, meta, err
}

// ccFoldMeta folds the session metadata a record carries into meta.
func ccFoldMeta(meta *SessionMeta, line ccRawLine) {
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
}

// ccRecorder turns Claude Code records into classified events and marks, in
// order, one record at a time. Parse drives it from a transcript file and
// ParseStream from a stream-json pipe, so the two cannot drift apart: the same
// records yield the same events, the same marks and the same seqs.
//
// It reads meta (Cwd for classification, Model and Title to fill) but leaves
// the rest of the metadata to its caller, whose records are shaped
// differently.
type ccRecorder struct {
	opts    *classify.Options
	meta    *SessionMeta
	pending *pendingCalls[ccPendingCall]
	seq     int
	// endID is the message id of the previous record's turn-ending response,
	// so a line that continues it adds no second turn-end mark (#102). See
	// ccTurnEndMark.
	endID string
	event func(classify.Event)
	mark  func(classify.Mark)
	// usageSeen and usage are the optional usage plumbing: when usageSeen is
	// non-nil, every Usage it reports is handed to usage (see ccUsageTracker).
	// Parse/ParseItems set both; ParseStream leaves them nil — a stream pipe
	// carries no usage.
	usageSeen *ccUsageTracker
	usage     func(classify.Usage)
}

func newCCRecorder(opts *classify.Options, meta *SessionMeta, event func(classify.Event), mark func(classify.Mark), usageSeen *ccUsageTracker, usage func(classify.Usage)) *ccRecorder {
	return &ccRecorder{
		opts:      opts,
		meta:      meta,
		pending:   newPendingCalls[ccPendingCall](),
		event:     event,
		mark:      mark,
		usageSeen: usageSeen,
		usage:     usage,
	}
}

// emit classifies a call with its result and hands the event on.
func (r *ccRecorder) emit(call classify.ToolCall, result classify.ToolResult) {
	r.event(classify.BuildEventWith(r.opts, r.seq, r.meta.Cwd, call, result))
	r.seq++
}

// record handles one record's timeline content: marks, and the events of the
// calls it answers or proves dead.
func (r *ccRecorder) record(line ccRawLine) {
	// endID must reflect the record immediately before this one, and a record
	// that ends no turn (including one carrying no message at all) clears it —
	// otherwise a stale id would suppress a later turn-end mark.
	endID := r.endID
	r.endID = ""
	if line.Type == "ai-title" && line.AITitle != "" {
		r.meta.Title = line.AITitle
		return
	}
	if isCCCompaction(line) {
		r.mark(classify.Mark{Seq: r.seq, Type: "compaction"})
	}
	if isCCAPIError(line) {
		// A failed call is not a model turn: it holds no tool calls, and
		// its "<synthetic>" model must not become the session's model.
		r.mark(ccAPIErrorMark(line, r.seq))
		return
	}
	if len(line.Message) == 0 {
		return
	}
	var msg ccMessage
	if json.Unmarshal(line.Message, &msg) != nil {
		return
	}
	// A call this record proves can never be answered is emitted here,
	// ahead of anything the record itself contributes — see ccSupersedes.
	for _, p := range r.pending.release(func(p ccPendingCall) bool { return ccSupersedes(line, msg, p) }) {
		r.emit(p.call, classify.ToolResult{})
	}
	if r.usageSeen != nil {
		if u, ok := r.usageSeen.report(line, msg, r.seq); ok {
			r.usage(u)
		}
	}
	// The turn boundary this record ends, if it ends one (#102). A response
	// spans several lines sharing message.id and current versions copy the
	// stop_reason onto each, so a line continuing the previous record's
	// turn-ending response adds nothing — hence r.endID.
	if m, ok := ccTurnEndMark(line, msg, endID, r.seq); ok {
		r.mark(m)
	}
	if ccEndsTurn(line, msg) {
		r.endID = msg.ID
	}
	if line.Type == "user" && hasCCUserMessage(msg.Content) {
		text := ccUserMessageText(msg.Content)
		if !injectedUserMessage(text) {
			r.mark(classify.Mark{
				Seq:  r.seq,
				Type: "user-message",
				Note: strutil.TruncateRunes(text, 2000, "…"),
			})
		}
	}
	if msg.Model != "" && r.meta.Model == "" {
		r.meta.Model = msg.Model
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
				r.mark(classify.Mark{Seq: r.seq, Type: "subagent", Note: call.Name})
			}
			r.pending.put(call.ID, ccPendingCall{call: call, messageID: msg.ID, sidechain: line.IsSidechain, agentID: line.AgentID})
		case "tool_result":
			p, ok := r.pending.take(item.ToolUseID)
			if !ok {
				continue
			}
			r.emit(p.call, classify.ToolResult{
				Content: classify.ContentToString(item.Content),
				IsError: item.IsError,
			})
		}
	}
}

// flush emits every call still open, in issue order, with an empty result.
func (r *ccRecorder) flush() {
	for _, p := range r.pending.release(func(ccPendingCall) bool { return true }) {
		r.emit(p.call, classify.ToolResult{})
	}
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
// keeps the stream both complete and duplicate-free.
//
// A call that never gets a result — a session killed mid-tool, then resumed —
// held the watermark there forever, and nothing later in the session was ever
// delivered. Such a call is now released by the first record that proves no
// result can follow (see ccSupersedes) and emitted at that record with an
// empty result, exactly where Parse emits it. Until such a record arrives it
// is indistinguishable from a slow call, and holds the watermark like one.
func (a ClaudeCodeAdapter) ParseSince(ctx context.Context, path string, offset int64, startSeq int) ([]classify.Event, []classify.Mark, SessionMeta, int64, error) {
	items, meta, wm, err := a.ParseItemsSince(ctx, path, offset, startSeq)
	return items.Events, items.Marks, meta, wm, err
}

// ParseItemsSince implements ItemParser: ParseSince, plus the usage reports
// in the same window, withheld past the watermark with everything else. A
// read that resumes partway through an API response's records recognises
// the response from the records before the watermark (ccUsageTracker), so
// its usage is not reported a second time.
func (a ClaudeCodeAdapter) ParseItemsSince(ctx context.Context, path string, offset int64, startSeq int) (Items, SessionMeta, int64, error) {
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
	pending := newPendingCalls[ccPendingCall]()
	var events []classify.Event
	var marks []classify.Mark
	var usage []classify.Usage
	usageSeen := newCCUsageTracker(path, offset)
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
	safeOffset, safeEvents, safeMarks, safeUsage := offset, 0, 0, 0
	// ccTurnEndID of the previous record; see ccTurnEndMark. Until the first
	// record has been read it is not known: that record's predecessor sits
	// before the offset and is read only if the first record needs it.
	prevEndID, prevKnown := "", offset == 0

	err = readCompleteJSONLines(f, offset, func(data []byte, end int64) {
		defer func() {
			if pending.len() == 0 {
				safeOffset, safeEvents, safeMarks, safeUsage = end, len(events), len(marks), len(usage)
			}
		}()
		thisEndID := ""
		defer func() { prevEndID, prevKnown = thisEndID, true }()

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
		// A call this record proves can never be answered is emitted here,
		// ahead of anything the record itself contributes — see ccSupersedes.
		for _, p := range pending.release(func(p ccPendingCall) bool { return ccSupersedes(line, msg, p) }) {
			events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, p.call, classify.ToolResult{}))
			seq++
		}
		if u, ok := usageSeen.report(line, msg, seq); ok {
			usage = append(usage, u)
		}
		if !prevKnown && ccEndsTurn(line, msg) {
			// The first record read may continue a response whose earlier
			// lines precede the offset: look back at the one record before.
			prevEndID = ccTurnEndID(jsonlLastLineBefore(path, offset))
		}
		if m, ok := ccTurnEndMark(line, msg, prevEndID, seq); ok {
			marks = append(marks, m)
		}
		if ccEndsTurn(line, msg) {
			thisEndID = msg.ID
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
				pending.put(item.ID, ccPendingCall{call: call, messageID: msg.ID, sidechain: line.IsSidechain, agentID: line.AgentID})
			case "tool_result":
				p, ok := pending.take(item.ToolUseID)
				if !ok {
					continue
				}
				result := classify.ToolResult{
					Content: classify.ContentToString(item.Content),
					IsError: item.IsError,
				}
				events = append(events, classify.BuildEventWith(opts, seq, meta.Cwd, p.call, result))
				seq++
			}
		}
	})
	if meta.Title == "" {
		meta.Title = filepath.Base(path)
	}

	return Items{Events: events[:safeEvents], Marks: marks[:safeMarks], Usage: usage[:safeUsage]}, meta, safeOffset, err
}

// Tail-specific types for Claude Code JSONL format.

type ccRawLine struct {
	Type        string          `json:"type"`
	RequestID   string          `json:"requestId"`
	Timestamp   string          `json:"timestamp"`
	SessionID   string          `json:"sessionId"`
	AgentID     string          `json:"agentId"`
	IsSidechain bool            `json:"isSidechain"`
	Cwd         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	Message     json.RawMessage `json:"message"`
	AITitle     string          `json:"aiTitle"`
	Subtype     string          `json:"subtype"`
	// IsMeta marks a line Claude Code wrote into the conversation itself, such
	// as a loaded skill's body, rather than one the user typed.
	IsMeta bool `json:"isMeta"`

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
	// ID is the API response the line belongs to. Claude Code writes each
	// content block of a response on a line of its own, all sharing it.
	ID    string `json:"id"`
	Role  string `json:"role"`
	Model string `json:"model"`
	// StopReason is why the API response stopped; see ccEndsTurn. null
	// decodes to "".
	StopReason string        `json:"stop_reason"`
	Content    ccContentList `json:"content"`
	// Usage is the response's token usage. Every line of a response repeats
	// it; see ccUsageTracker.
	Usage *ccUsage `json:"usage"`
}

// ccUsage is the part of an Anthropic usage object a Usage reports. Its four
// counts are already disjoint: input_tokens excludes the cached prompt.
type ccUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// ccUsageTracker turns the usage Claude Code repeats on every record of an
// API response into one classify.Usage per response.
//
// Claude Code writes each content block of a response — thinking, text, each
// tool_use — on a line of its own, and every one of those lines carries the
// whole response's usage. Across 61,000 responses in local transcripts the
// copies agreed in all but two, both from one older build that wrote a
// partial output_tokens on a response's first line; the first line's numbers
// are the ones reported. A response's lines are not always adjacent: with
// tools run while the response streams, a tool_result lands between two
// tool_use lines of the same response. So a response is identified by its
// requestId (the message id when there is none), and a line repeats the
// response the same conversation's previous usage-bearing line belonged to.
// Conversations are kept apart because a subagent's lines can interleave with
// its parent's (see ccSupersedes).
//
// Parse starts with nothing seen. ParseSince starts mid-file, where the
// response its first records belong to may already have been reported by an
// earlier read, so for each conversation it meets it looks back past the
// watermark, once, for that conversation's last usage-bearing line — the
// same line a full read would have seen last. That keeps an incremental read
// reporting exactly what Parse reports, so a response is delivered once.
type ccUsageTracker struct {
	path   string
	offset int64
	last   map[string]string // conversation → response key of its last usage-bearing line
}

func newCCUsageTracker(path string, offset int64) *ccUsageTracker {
	return &ccUsageTracker{path: path, offset: offset, last: map[string]string{}}
}

// ccConversation names the conversation a line belongs to: the parent's, or
// one subagent's.
func ccConversation(line ccRawLine) string {
	if !line.IsSidechain {
		return ""
	}
	return "sidechain\x00" + line.AgentID
}

// ccUsageBearing reports whether a line is one of an API response's records
// carrying its usage. A failed call's synthetic record is not: no response
// was served, and it must not be taken for the response it interrupted.
func ccUsageBearing(line ccRawLine, msg ccMessage) bool {
	return line.Type == "assistant" && !isCCAPIError(line) && msg.Usage != nil
}

// ccResponseKey identifies the API response a line belongs to.
func ccResponseKey(line ccRawLine, msg ccMessage) string {
	if line.RequestID != "" {
		return line.RequestID
	}
	return msg.ID
}

// report returns the Usage for a line, at seq, when the line opens a response
// this tracker has not reported and records any usage at all. A line with no
// response key is its own response.
func (t *ccUsageTracker) report(line ccRawLine, msg ccMessage, seq int) (classify.Usage, bool) {
	if !ccUsageBearing(line, msg) {
		return classify.Usage{}, false
	}
	conv := ccConversation(line)
	prev, seen := t.last[conv]
	if !seen && t.offset > 0 {
		prev = t.lookBack(conv)
	}
	key := ccResponseKey(line, msg)
	t.last[conv] = key
	if key != "" && key == prev {
		return classify.Usage{}, false
	}
	u := msg.Usage
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
		return classify.Usage{}, false
	}
	at, _ := parseSessionTimeOk(line.Timestamp)
	return classify.Usage{
		Seq:          seq,
		At:           at,
		Model:        msg.Model,
		RequestID:    line.RequestID,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		CacheRead:    u.CacheReadInputTokens,
		CacheWrite:   u.CacheCreationInputTokens,
	}, true
}

// lookBack returns the response key of conv's last usage-bearing line before
// the watermark, or "" when there is none within reach.
func (t *ccUsageTracker) lookBack(conv string) string {
	var key string
	jsonlLastMatchBefore(t.path, t.offset, func(data []byte) bool {
		var line ccRawLine
		if json.Unmarshal(data, &line) != nil || line.Type != "assistant" || ccConversation(line) != conv {
			return false
		}
		var msg ccMessage
		if json.Unmarshal(line.Message, &msg) != nil || !ccUsageBearing(line, msg) {
			return false
		}
		key = ccResponseKey(line, msg)
		return true
	})
	return key
}

// ccPendingCall is a tool_use waiting on its tool_result, with what a later
// record needs to decide the result can no longer arrive.
type ccPendingCall struct {
	call      classify.ToolCall
	messageID string // the API response that issued the call
	sidechain bool
	agentID   string // the subagent whose conversation issued it, if any
}

// ccSupersedes reports whether line, read after a pending call, proves the
// call will never receive a result. Claude Code runs every call an API
// response issues and writes its tool_result before it sends the next
// request, and it writes the user's next message only once the turn is over.
// So two records settle it:
//
//   - An assistant line from a different API response. The next request went
//     out without this call's result, so the call was never run: the response
//     stopped short (a refusal, say), or the process died and the session was
//     resumed.
//   - A message the user typed, including the "[Request interrupted by user]"
//     marker Claude Code writes when a turn is cut short. Harness-injected
//     text and isMeta lines do not count: a loaded skill's body is an isMeta
//     user line written between the results of one batch of parallel calls.
//
// Both were checked against real transcripts: across 107,525 resolved calls
// in 1,774 local sessions, no tool_result was ever written after either
// record — only after an isMeta line, nine times. A user line that carries a
// tool_result never counts, even with text ahead of it: its own results are
// paired after the release runs, so counting it would drop them. Nor does one
// with no text, or whose text opens with a tag. Claude Code writes task
// notifications between the results of one parallel batch, and
// injectedUserMessage recognizes them only while nothing trails the closing
// tag; a typed message that opens with a system reminder is still followed by
// the response it asks for, and that response releases the call instead.
//
// Either record proves this only within its own conversation, so a line from
// another one never counts. The parent and its subagents are separate conversations —
// isSidechain tells the parent's lines from a subagent's, and agentId tells
// one subagent's from another's. Older transcripts interleave subagents'
// lines with the parent's, marked isSidechain but with no agentId; parallel
// subagents' lines then interleave with each other too, and nothing tells
// them apart. Such a line releases nothing, so a call it issued holds as it
// always did rather than being released by a sibling subagent's response.
func ccSupersedes(line ccRawLine, msg ccMessage, p ccPendingCall) bool {
	if line.IsSidechain != p.sidechain || line.AgentID != p.agentID {
		return false
	}
	if line.IsSidechain && line.AgentID == "" {
		return false
	}
	switch line.Type {
	case "assistant":
		return msg.ID != "" && p.messageID != "" && msg.ID != p.messageID
	case "user":
		if line.IsMeta || ccHasToolResult(msg.Content) || !hasCCUserMessage(msg.Content) {
			return false
		}
		text := strings.TrimSpace(ccUserMessageText(msg.Content))
		return text != "" && !strings.HasPrefix(text, "<") && !injectedUserMessage(text)
	}
	return false
}

// ccHasToolResult reports whether content carries any tool_result block.
func ccHasToolResult(content ccContentList) bool {
	for _, item := range content.Items {
		if item.Type == "tool_result" {
			return true
		}
	}
	return false
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

// ccEndsTurn reports whether a record is a model response that ends the
// agent's turn: an assistant line whose stop_reason is end_turn,
// stop_sequence, max_tokens or refusal. tool_use is not a boundary — the
// agent runs the calls and sends the next request — and neither is
// pause_turn, which the API returns to be resumed, nor a missing
// stop_reason.
//
// Only the model's own responses in this conversation count. A failed API
// call is a synthetic record with its own "error" mark, and the
// "<synthetic>" "No response requested." record is written without calling
// the model. An inline subagent line (isSidechain with no agentId, the older
// layout that interleaves subagents with the parent) ends the subagent's
// turn, not the session's; a subagent's own transcript carries its agentId
// on every line, and its turn ends count.
func ccEndsTurn(line ccRawLine, msg ccMessage) bool {
	if line.Type != "assistant" || line.IsAPIErrorMessage || msg.Model == "<synthetic>" {
		return false
	}
	if line.IsSidechain && line.AgentID == "" {
		return false
	}
	switch msg.StopReason {
	case "end_turn", "stop_sequence", "max_tokens", "refusal":
		return true
	}
	return false
}

// ccTurnEndID returns the message ID of a raw record that ends a turn (see
// ccEndsTurn), or "" when it does not, has no ID, or is nil.
func ccTurnEndID(data []byte) string {
	var line ccRawLine
	if len(data) == 0 || json.Unmarshal(data, &line) != nil || len(line.Message) == 0 {
		return ""
	}
	var msg ccMessage
	if json.Unmarshal(line.Message, &msg) != nil || !ccEndsTurn(line, msg) {
		return ""
	}
	return msg.ID
}

// ccTurnEndMark returns the "turn-end" mark a record contributes, if any: at
// the given seq, dated by the record, noted with its stop_reason. prevEndID is
// ccTurnEndID of the record immediately before this one.
//
// Claude Code writes each content block of a response on its own line, and
// current versions copy the final stop_reason onto every one of them, so a
// thinking-then-text response is two end_turn lines; older versions, and
// subagent transcripts, put null on all but the last. One response is one
// turn end: a line that continues the previous record's turn-ending response
// adds nothing, so the mark lands on the first line carrying the boundary.
//
// Deciding from the one record before, rather than every response ever
// marked, is what lets ParseSince resume mid-response with a single bounded
// read (jsonlLastLineBefore) and still agree with Parse. The cost is a
// response whose lines are split by another record — seen once in ~1,600
// multi-line responses across local transcripts — which marks twice, and a
// turn-ending line over jsonlLastLineCap, which ParseSince cannot look back
// at and so marks its continuation too.
func ccTurnEndMark(line ccRawLine, msg ccMessage, prevEndID string, seq int) (classify.Mark, bool) {
	if !ccEndsTurn(line, msg) || (msg.ID != "" && msg.ID == prevEndID) {
		return classify.Mark{}, false
	}
	return classify.Mark{Seq: seq, Timestamp: line.Timestamp, Type: "turn-end", Note: msg.StopReason}, true
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
// sorts error marks into quota, auth, timeout, transport and other, and the
// code and status are what it can classify a Claude Code failure on — a
// session-limit message, for one, names neither a rate limit nor a 429 — so
// change it only together with that classifier. The note is truncated to
// 2000 runes, as the Crush reader's error note is; the front is never what
// gets cut.
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
