package tail

import "testing"

func TestCommandOutputFailedStructuredEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr bool
	}{
		{
			name:    "exit_code 0 success",
			output:  `{"exit_code": 0}`,
			wantErr: false,
		},
		{
			name:    "exit_code 2 failure",
			output:  `{"exit_code": 2}`,
			wantErr: true,
		},
		{
			name:    "metadata.exit_code 2 with clean output",
			output:  `{"metadata": {"exit_code": 2}}`,
			wantErr: true,
		},
		{
			name:    "exit_code 10",
			output:  `{"exit_code": 10}`,
			wantErr: true,
		},
		{
			name:    "timed_out true",
			output:  `{"timed_out": true}`,
			wantErr: true,
		},
		{
			name:    "timed_out false",
			output:  `{"timed_out": false}`,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commandOutputFailed(tt.output)
			if got != tt.wantErr {
				t.Errorf("commandOutputFailed(%q) = %v, want %v", tt.output, got, tt.wantErr)
			}
		})
	}
}

func TestCommandOutputFailedNoFalsePositiveFromGrep(t *testing.T) {
	// A successful grep whose output merely contains "error:" must not
	// be flagged as an error. This was the false-positive from the issue.
	output := `src/errors.go:10: func handleError() error {`
	if commandOutputFailed(output) {
		t.Error("successful grep output containing 'error:' should not be flagged")
	}
}

func TestCommandOutputFailedNoFalseNegativeFromEnvelope(t *testing.T) {
	// A failed command with a structured exit_code envelope and clean
	// output text must be detected. This was the false-negative from the issue.
	output := `{"exit_code": 2}`
	if !commandOutputFailed(output) {
		t.Error("exit_code 2 envelope should be flagged as error")
	}
}

func TestCommandOutputFailedApplyPatch(t *testing.T) {
	if !commandOutputFailed("apply_patch verification failed: something") {
		t.Error("apply_patch verification failed should be error")
	}
}

func TestCommandOutputFailedScriptStatus(t *testing.T) {
	tests := []struct {
		firstLine string
		want      bool
	}{
		{"script completed successfully", false},
		{"script running...", false},
		{"script failed: timeout", true},
	}
	for _, tt := range tests {
		if got := commandOutputFailed(tt.firstLine); got != tt.want {
			t.Errorf("commandOutputFailed(%q) = %v, want %v", tt.firstLine, got, tt.want)
		}
	}
}

func TestCommandOutputFailedExitCodeLine(t *testing.T) {
	tests := []struct {
		output string
		want   bool
	}{
		{"Process exited with code 0\nOutput:\nstuff", false},
		{"Exit code: 1\nOutput:\nstuff", true},
		{"Exit code: 10", true},
		{"Exit code: 0", false},
	}
	for _, tt := range tests {
		if got := commandOutputFailed(tt.output); got != tt.want {
			t.Errorf("commandOutputFailed(%q) = %v, want %v", tt.output, got, tt.want)
		}
	}
}

func TestCommandOutputFailedAbortedByUser(t *testing.T) {
	output := "Aborted by user\nOutput:\nsomething"
	if !commandOutputFailed(output) {
		t.Error("'Aborted by user' in header should be error")
	}
}
