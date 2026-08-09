package tail

import "testing"

func TestPiSessionTitle(t *testing.T) {
	tests := []struct {
		name          string
		entries       []piRawEntry
		firstUserText string
		path          string
		want          string
	}{
		{
			name: "session_info wins",
			entries: []piRawEntry{
				{Type: "session_info", Name: "my session"},
			},
			firstUserText: "fix the bug",
			path:          "/tmp/sess.jsonl",
			want:          "my session",
		},
		{
			name: "latest session_info wins",
			entries: []piRawEntry{
				{Type: "session_info", Name: "old name"},
				{Type: "session_info", Name: "new name"},
			},
			path: "/tmp/sess.jsonl",
			want: "new name",
		},
		{
			name: "empty session_info clears, falls back",
			entries: []piRawEntry{
				{Type: "session_info", Name: "named"},
				{Type: "session_info", Name: ""},
			},
			firstUserText: "fix the bug",
			path:          "/tmp/sess.jsonl",
			want:          "fix the bug",
		},
		{
			name:          "first user message fallback",
			entries:       nil,
			firstUserText: "add a login page",
			path:          "/tmp/sess.jsonl",
			want:          "add a login page",
		},
		{
			name:          "filepath.Base fallback",
			entries:       nil,
			firstUserText: "",
			path:          "/tmp/abc123.jsonl",
			want:          "abc123.jsonl",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := piSessionTitle(tt.entries, tt.firstUserText, tt.path)
			if got != tt.want {
				t.Errorf("piSessionTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}
