package tail

import (
	"path/filepath"
	"testing"
)

func TestClaudeCodeAgentIDAndSidechain(t *testing.T) {
	path := writeTempJSONL(t, "cc_sidechain.jsonl",
		`{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"parent-session","agentId":"agent-abc123","isSidechain":true,"cwd":"/home/user/project","gitBranch":"main","message":{"role":"user","content":"do the thing"}}
{"type":"assistant","timestamp":"2026-01-01T10:00:01Z","sessionId":"parent-session","agentId":"agent-abc123","isSidechain":true,"message":{"role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"/home/user/project/src/main.go"}}]}}
{"type":"user","timestamp":"2026-01-01T10:00:02Z","sessionId":"parent-session","agentId":"agent-abc123","isSidechain":true,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"package main"}]}}`)
	adapter := ClaudeCodeAdapter{}

	t.Run("Parse populates AgentID and IsSidechain", func(t *testing.T) {
		_, _, meta, err := adapter.Parse(path)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if meta.AgentID != "agent-abc123" {
			t.Errorf("AgentID = %q, want %q", meta.AgentID, "agent-abc123")
		}
		if !meta.IsSidechain {
			t.Error("IsSidechain = false, want true")
		}
		if !meta.Auxiliary {
			t.Error("Auxiliary should be true when IsSidechain is true")
		}
	})

	t.Run("Summarize populates AgentID and IsSidechain", func(t *testing.T) {
		meta, err := adapter.Summarize(path)
		if err != nil {
			t.Fatalf("Summarize failed: %v", err)
		}
		if meta.AgentID != "agent-abc123" {
			t.Errorf("AgentID = %q, want %q", meta.AgentID, "agent-abc123")
		}
		if !meta.IsSidechain {
			t.Error("IsSidechain = false, want true")
		}
		if !meta.Auxiliary {
			t.Error("Auxiliary should be true when IsSidechain is true")
		}
	})
}

func TestClaudeCodeNoAgentIDOrSidechain(t *testing.T) {
	// The basic fixture has no agentId/isSidechain — both should be zero.
	adapter := ClaudeCodeAdapter{Dir: "testdata"}
	path := filepath.Join("testdata", "claudecode_basic.jsonl")

	t.Run("Parse leaves AgentID empty and IsSidechain false for top-level session", func(t *testing.T) {
		_, _, meta, err := adapter.Parse(path)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if meta.AgentID != "" {
			t.Errorf("AgentID = %q, want empty", meta.AgentID)
		}
		if meta.IsSidechain {
			t.Error("IsSidechain = true, want false for top-level session")
		}
		if meta.Auxiliary {
			t.Error("Auxiliary should be false for top-level session")
		}
	})

	t.Run("Summarize leaves AgentID empty and IsSidechain false for top-level session", func(t *testing.T) {
		meta, err := adapter.Summarize(path)
		if err != nil {
			t.Fatalf("Summarize failed: %v", err)
		}
		if meta.AgentID != "" {
			t.Errorf("AgentID = %q, want empty", meta.AgentID)
		}
		if meta.IsSidechain {
			t.Error("IsSidechain = true, want false for top-level session")
		}
	})
}

func TestClaudeCodeAgentIDWithoutSidechain(t *testing.T) {
	// A session can have an agentId without being a sidechain.
	path := writeTempJSONL(t, "cc_agentid_only.jsonl",
		`{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","agentId":"agent-xyz","cwd":"/tmp","message":{"role":"user","content":"hello"}}
{"type":"assistant","timestamp":"2026-01-01T10:00:01Z","sessionId":"s1","agentId":"agent-xyz","message":{"role":"assistant","model":"m","content":[{"type":"tool_use","id":"c1","name":"Read","input":{"file_path":"/tmp/f.go"}}]}}
{"type":"user","timestamp":"2026-01-01T10:00:02Z","sessionId":"s1","agentId":"agent-xyz","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"data"}]}}`)
	adapter := ClaudeCodeAdapter{}

	t.Run("Parse captures AgentID, IsSidechain stays false", func(t *testing.T) {
		_, _, meta, err := adapter.Parse(path)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if meta.AgentID != "agent-xyz" {
			t.Errorf("AgentID = %q, want %q", meta.AgentID, "agent-xyz")
		}
		if meta.IsSidechain {
			t.Error("IsSidechain = true, want false")
		}
		if meta.Auxiliary {
			t.Error("Auxiliary should be false when not a sidechain")
		}
	})
}

func TestClaudeCodeAgentIDFirstOccurrenceWins(t *testing.T) {
	// If the agentId changes mid-session, the first value should be kept.
	path := writeTempJSONL(t, "cc_agentid_change.jsonl",
		`{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"s1","agentId":"first-agent","cwd":"/tmp","message":{"role":"user","content":"hello"}}
{"type":"assistant","timestamp":"2026-01-01T10:00:01Z","sessionId":"s1","agentId":"second-agent","message":{"role":"assistant","model":"m","content":[{"type":"tool_use","id":"c1","name":"Read","input":{"file_path":"/tmp/f.go"}}]}}
{"type":"user","timestamp":"2026-01-01T10:00:02Z","sessionId":"s1","agentId":"third-agent","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"data"}]}}`)
	adapter := ClaudeCodeAdapter{}

	t.Run("Parse keeps first AgentID", func(t *testing.T) {
		_, _, meta, err := adapter.Parse(path)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		if meta.AgentID != "first-agent" {
			t.Errorf("AgentID = %q, want %q (first occurrence)", meta.AgentID, "first-agent")
		}
	})

	t.Run("Summarize keeps first AgentID", func(t *testing.T) {
		meta, err := adapter.Summarize(path)
		if err != nil {
			t.Fatalf("Summarize failed: %v", err)
		}
		if meta.AgentID != "first-agent" {
			t.Errorf("AgentID = %q, want %q (first occurrence)", meta.AgentID, "first-agent")
		}
	})
}
