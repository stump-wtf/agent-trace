package classify

import "testing"

// TestVerifyCommandWithCustomPatterns verifies that callers can extend the
// verify command pattern list via Options (issue #14).
func TestVerifyCommandWithCustomPatterns(t *testing.T) {
	defaultVerifyTests := []struct {
		cmd  string
		want bool
	}{
		{"just test", false}, // not a default pattern
		{"bun test", false},  // not a default pattern
		{"go test ./...", true},
	}

	// Verify defaults are unchanged.
	for _, tt := range defaultVerifyTests {
		got := VerifyCommand(tt.cmd)
		if got != tt.want {
			t.Errorf("VerifyCommand(%q) = %v, want %v (default patterns)", tt.cmd, got, tt.want)
		}
	}

	// With custom patterns, the new patterns should be recognized.
	opts := &Options{
		VerifyPatterns: []string{"just test", "bun test", "deno test"},
	}

	customVerifyTests := []struct {
		cmd  string
		want bool
	}{
		{"just test", true},
		{"bun test", true},
		{"deno test --allow-read", true},
		{"go test ./...", true}, // default patterns still apply
		{"npm test", true},      // default patterns still apply
		{"echo hello", false},
		{"git status", false},
	}

	for _, tt := range customVerifyTests {
		got := VerifyCommandWith(opts, tt.cmd)
		if got != tt.want {
			t.Errorf("VerifyCommandWith(%q) = %v, want %v (custom patterns)", tt.cmd, got, tt.want)
		}
	}

	// Nil opts should behave identically to VerifyCommand.
	for _, tt := range defaultVerifyTests {
		got := VerifyCommandWith(nil, tt.cmd)
		if got != tt.want {
			t.Errorf("VerifyCommandWith(nil, %q) = %v, want %v (nil opts)", tt.cmd, got, tt.want)
		}
	}
}

// TestActionForWithCustomVerifyPatterns verifies that ActionForWith threads
// VerifyPatterns through to VerifyCommand (issue #14).
func TestActionForWithCustomVerifyPatterns(t *testing.T) {
	opts := &Options{
		VerifyPatterns: []string{"just test"},
	}

	// With custom pattern, "just test" should classify as verify.
	got := ActionForWith(opts, "Bash", map[string]any{"command": "just test"}, "")
	if got != ActionVerify {
		t.Errorf("ActionForWith custom verify: got %q, want %q", got, ActionVerify)
	}

	// Without opts, "just test" should NOT classify as verify.
	got = ActionFor("Bash", map[string]any{"command": "just test"}, "")
	if got != ActionExec {
		t.Errorf("ActionFor without opts: got %q, want %q", got, ActionExec)
	}
}
