package tail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

// StreamFormat reports Claude Code's stream-json mode. --verbose is part of
// it: `claude -p` rejects stream-json without it and exits before reading the
// prompt.
func (a ClaudeCodeAdapter) StreamFormat() StreamFormat {
	return StreamFormat{
		Name: "stream-json",
		Args: []string{"--output-format", "stream-json", "--verbose"},
	}
}

// ParseStream implements StreamParser for `claude -p --output-format
// stream-json --verbose`.
//
// A stream-json record wraps the same message a transcript line does, under
// snake_case field names, so each record is mapped onto a transcript line and
// run through the handler Parse uses. The same run therefore yields the same
// events, marks and seqs from its stream as from its transcript, with one
// difference in the input rather than the handling: -p does not echo the
// prompt onto stdout, so the prompt's user-message mark exists only in the
// transcript.
//
// A subagent writes onto its parent's stream, each record tagged with the
// Task call that launched it (parent_tool_use_id). Such a record is read as a
// sidechain line of that subagent, so its calls pair within its own
// conversation and its responses never settle the parent's open calls. Its
// events are handed over with the parent's; a transcript keeps them in a
// separate file.
//
// The metadata is what Parse would take from the same records: ID and Cwd
// from the init record, StartedAt and EndedAt from the records' timestamps,
// and Model from the first response. The init record's own model is not used:
// it is the model asked for, which can carry a suffix such as "[1m]" that no
// response does. So Model is still empty when Meta is called, and set on the
// metadata ParseStream returns. Key, Path and Title are empty — a stream has
// no file to name them after.
func (a ClaudeCodeAdapter) ParseStream(ctx context.Context, r io.Reader, h StreamHandler) (SessionMeta, *StreamResult, error) {
	opts := a.opts
	if opts == nil {
		opts = osClassifyOptions(nil)
	}
	meta := SessionMeta{Harness: a.Harness()}
	rec := newCCRecorder(opts, &meta, h.event, h.mark)
	var result *StreamResult
	read, recognized, announced := false, false, false

	err := scanJSONLines(r, func(data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		read = true
		var sl ccStreamLine
		if json.Unmarshal(data, &sl) != nil {
			return nil
		}
		if sl.Type == "result" {
			if sl.SessionID != "" {
				recognized = true
			}
			res := sl.result()
			result = &res
			return nil
		}
		line := sl.transcriptLine()
		if isCCLine(line) {
			recognized = true
		}
		if line.SessionID != "" {
			meta.ID = line.SessionID
		}
		if line.Cwd != "" && meta.Cwd == "" {
			meta.Cwd = line.Cwd
		}
		if line.Timestamp != "" {
			if meta.StartedAt == "" {
				meta.StartedAt = line.Timestamp
			}
			meta.EndedAt = line.Timestamp
		}
		if !announced && meta.ID != "" {
			announced = true
			h.meta(meta)
		}
		rec.record(line)
		return nil
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return meta, nil, err
	}
	rec.flush()
	if err == nil && read && !recognized {
		err = errors.New("not a Claude Code stream-json stream")
	}
	return meta, result, err
}

// ccStreamLine is one stream-json record: a message record (system,
// assistant, user) or the final result.
type ccStreamLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Timestamp string `json:"timestamp"`
	SessionID string `json:"session_id"`
	// ParentToolUseID is the Task call a subagent's record belongs to, and
	// null on the parent's own records.
	ParentToolUseID string          `json:"parent_tool_use_id"`
	Message         json.RawMessage `json:"message"`
	// IsMeta is the transcript's isMeta under its stream-json name: a user
	// record the loop wrote rather than one the user typed, such as a loaded
	// skill's body. IsSynthetic is a separate stamp on the same kind of
	// record. Either one keeps the record from settling an open call; see
	// ccSupersedes.
	IsMeta      bool `json:"is_meta"`
	IsSynthetic bool `json:"isSynthetic"`

	// Cwd is on the init record only.
	Cwd string `json:"cwd"`

	// A failed API call; see isCCAPIError.
	IsAPIErrorMessage bool            `json:"is_api_error_message"`
	APIError          json.RawMessage `json:"error"`
	APIErrorStatus    json.RawMessage `json:"api_error_status"`

	// Result record. Errors stays raw so a shape this reader does not
	// expect costs the error list, not the whole Result.
	IsError       bool            `json:"is_error"`
	NumTurns      int             `json:"num_turns"`
	DurationMS    int64           `json:"duration_ms"`
	DurationAPIMS int64           `json:"duration_api_ms"`
	TotalCostUSD  float64         `json:"total_cost_usd"`
	StopReason    string          `json:"stop_reason"`
	Result        string          `json:"result"`
	Errors        json.RawMessage `json:"errors"`
	Usage         struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// transcriptLine maps a message record onto the transcript line that carries
// the same message.
func (s ccStreamLine) transcriptLine() ccRawLine {
	return ccRawLine{
		Type:              s.Type,
		Timestamp:         s.Timestamp,
		SessionID:         s.SessionID,
		AgentID:           s.ParentToolUseID,
		IsSidechain:       s.ParentToolUseID != "",
		Cwd:               s.Cwd,
		Message:           s.Message,
		Subtype:           s.Subtype,
		IsMeta:            s.IsMeta || s.IsSynthetic,
		IsAPIErrorMessage: s.IsAPIErrorMessage,
		APIError:          s.APIError,
		APIErrorStatus:    s.APIErrorStatus,
	}
}

// result reads the final record into a StreamResult.
func (s ccStreamLine) result() StreamResult {
	var errs []string
	_ = json.Unmarshal(s.Errors, &errs)
	return StreamResult{
		Subtype:       s.Subtype,
		IsError:       s.IsError,
		Turns:         s.NumTurns,
		DurationMS:    s.DurationMS,
		APIDurationMS: s.DurationAPIMS,
		CostUSD:       s.TotalCostUSD,
		Usage: TokenUsage{
			InputTokens:              s.Usage.InputTokens,
			OutputTokens:             s.Usage.OutputTokens,
			CacheCreationInputTokens: s.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     s.Usage.CacheReadInputTokens,
		},
		StopReason: s.StopReason,
		Text:       s.Result,
		Errors:     errs,
	}
}
