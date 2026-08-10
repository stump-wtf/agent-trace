package tail

import (
	"testing"
	"time"
)

func TestSessionMetaStarted(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantOk  bool
		wantSec int64 // Unix seconds, only checked when wantOk
	}{
		{
			name:    "RFC3339Nano",
			input:   "2026-01-15T10:30:00.123456789Z",
			wantOk:  true,
			wantSec: 1768473000,
		},
		{
			name:    "RFC3339",
			input:   "2026-01-15T10:30:00Z",
			wantOk:  true,
			wantSec: 1768473000,
		},
		{
			name:    "timezone offset",
			input:   "2026-01-15T05:30:00-05:00",
			wantOk:  true,
			wantSec: 1768473000,
		},
		{
			name:   "empty string",
			input:  "",
			wantOk: false,
		},
		{
			name:   "garbage",
			input:  "not a timestamp",
			wantOk: false,
		},
		{
			name:   "partial date",
			input:  "2026-01-15",
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := SessionMeta{StartedAt: tt.input}
			got, ok := m.Started()
			if ok != tt.wantOk {
				t.Fatalf("Started() ok = %v, want %v (input %q)", ok, tt.wantOk, tt.input)
			}
			if ok {
				if got.Unix() != tt.wantSec {
					t.Errorf("Started() time = %v (Unix %d), want Unix %d", got, got.Unix(), tt.wantSec)
				}
			} else {
				if !got.IsZero() {
					t.Errorf("Started() returned non-zero time %v when ok=false", got)
				}
			}
		})
	}
}

func TestSessionMetaEnded(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantOk  bool
		wantSec int64
	}{
		{
			name:    "RFC3339Nano",
			input:   "2026-06-01T12:00:00.5Z",
			wantOk:  true,
			wantSec: 1780315200,
		},
		{
			name:   "empty string",
			input:  "",
			wantOk: false,
		},
		{
			name:   "garbage",
			input:  "???",
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := SessionMeta{EndedAt: tt.input}
			got, ok := m.Ended()
			if ok != tt.wantOk {
				t.Fatalf("Ended() ok = %v, want %v (input %q)", ok, tt.wantOk, tt.input)
			}
			if ok && got.Unix() != tt.wantSec {
				t.Errorf("Ended() time Unix = %d, want %d", got.Unix(), tt.wantSec)
			}
			if !ok && !got.IsZero() {
				t.Errorf("Ended() returned non-zero time %v when ok=false", got)
			}
		})
	}
}

func TestSessionMetaStartedEndedRoundTrip(t *testing.T) {
	// Verify both accessors work on the same SessionMeta and the
	// boolean distinguishes missing from present.
	m := SessionMeta{
		StartedAt: "2026-03-15T08:00:00Z",
		// EndedAt intentionally left empty.
	}

	started, startedOk := m.Started()
	if !startedOk {
		t.Fatal("Started() should be ok")
	}
	ended, endedOk := m.Ended()
	if endedOk {
		t.Fatal("Ended() should not be ok for empty EndedAt")
	}
	if !ended.IsZero() {
		t.Fatal("Ended() should be zero time when not ok")
	}
	if !started.After(ended) {
		t.Error("started should be after zero-time ended")
	}

	// Verify the time value matches what parseSessionTime would produce.
	expected := time.Date(2026, 3, 15, 8, 0, 0, 0, time.UTC)
	if !started.Equal(expected) {
		t.Errorf("Started() = %v, want %v", started, expected)
	}
}

func TestSessionMetaStartedMissingDoesNotEqualZeroTime(t *testing.T) {
	// The critical correctness property from the issue: a missing timestamp
	// must be distinguishable from the zero time. A caller comparing
	// "did this session start after X" must not treat missing as
	// "1 January year 1, therefore before everything."
	m := SessionMeta{StartedAt: ""}

	_, ok := m.Started()
	if ok {
		t.Error("Started() should return ok=false for empty timestamp")
	}

	// A caller can now branch on ok before comparing:
	if ok {
		t.Error("should not reach here — ok is false")
	}

	// Contrast with the zero-value time.Time: if a caller ignored the bool
	// and used the zero time directly, they'd wrongly conclude the session
	// started at 0001-01-01. The bool prevents that.
}

// TestParseSessionTimeDelegates guards the deduplication: the watcher's
// parseSessionTime and SessionMeta.Started must never disagree about what a
// timestamp means. They were two independent copies of the same parse, which
// is precisely the drift these accessors exist to prevent — if someone
// reintroduces a second parser, this fails.
func TestParseSessionTimeDelegates(t *testing.T) {
	inputs := []string{
		"",
		"2026-01-01T10:00:00Z",
		"2026-01-01T10:00:00.123456789Z",
		"2026-01-01T10:00:00+05:30",
		"not-a-timestamp",
		"1735725600", // epoch seconds: not RFC 3339, must not parse
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			got := parseSessionTime(in)
			want, ok := SessionMeta{StartedAt: in}.Started()
			if !got.Equal(want) {
				t.Errorf("parseSessionTime(%q) = %v, Started() = %v — the two parsers disagree", in, got, want)
			}
			// The bool is the only thing that distinguishes them: a failed
			// parse and a zero time are the same time.Time but not the same
			// answer, which is the whole point of the accessor.
			if ok && got.IsZero() {
				t.Errorf("Started() reported ok for %q but the time is zero", in)
			}
		})
	}
}
