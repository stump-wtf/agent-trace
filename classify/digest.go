package classify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Input Digests
//
// An Event keeps what a tool call did but not the arguments it was called
// with, and for a tool the classifier knows nothing about — every MCP tool —
// Summary is only the tool name. So two calls with different arguments look
// the same, and a consumer cannot tell a runaway loop repeating one call from
// an agent working through a list. Event.InputDigest keeps a fingerprint of
// the arguments: the SHA-256 of the call's input encoded as JSON, which
// encoding/json writes with map keys sorted at every depth, so the same
// arguments always give the same digest whatever order the transcript stored
// them in.
//
// It is a fingerprint and not a redaction boundary. It does not reveal the
// arguments, but an input small enough to guess (a one-word command) can be
// confirmed by hashing the guess. The tool name is not part of it; pair it
// with Event.Tool.
//
// @joestump-agent 09/23/2026 - Added for Harness's runaway tool-loop guard:
// 608 identical MCP calls in one run were indistinguishable from 608
// different ones.

// inputDigest returns the hex SHA-256 of input's JSON encoding. A nil and an
// empty input are the same call and share the digest of "{}". It returns ""
// only when the input cannot be encoded, which no adapter's decoded JSON can
// produce.
func inputDigest(input map[string]any) string {
	if len(input) == 0 {
		input = map[string]any{}
	}
	b, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
