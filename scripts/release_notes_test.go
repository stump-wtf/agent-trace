// Package scripts holds tests for the repo's shell tooling. The scripts run in
// CI where a mistake is expensive and invisible — release-notes.sh feeds the
// release pages, so a bad extraction publishes wrong notes under a tag that
// cannot be recut — and `go test ./...` is what actually runs here, so they are
// exercised from Go rather than from a shell harness the repo does not have.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sampleChangelog = `# Changelog

Preamble that must never appear in any release's notes.

## [Unreleased]

## [0.2.0] - 2026-09-01

### Added

- The newer thing.

## [0.1.0] - 2026-08-15

First release.

### Fixed

- The older thing.

## Versioning

This trailing section is a plain level-2 heading, not a bracketed version.
Stopping only at "## [" runs straight through it.

[Unreleased]: https://example.invalid/compare/v0.2.0...HEAD
[0.1.0]: https://example.invalid/releases/tag/v0.1.0
`

func runScript(t *testing.T, changelog string, args ...string) (string, string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(path, []byte(changelog), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{"release-notes.sh"}, append(args, path)...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// The regression: the last version section is bounded by the trailing plain
// "## Versioning" heading, so neither it nor the link definitions may leak in.
func TestReleaseNotesStopsAtAPlainHeading(t *testing.T) {
	out, _, err := runScript(t, sampleChangelog, "v0.1.0")
	if err != nil {
		t.Fatalf("script failed: %v", err)
	}
	for _, forbidden := range []string{"## Versioning", "[Unreleased]:", "runs straight through it"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("notes leaked past the section: contains %q\n---\n%s", forbidden, out)
		}
	}
	if !strings.Contains(out, "The older thing.") {
		t.Errorf("notes missing the section's own content:\n%s", out)
	}
}

// A middle section is bounded by the next version heading on both sides.
func TestReleaseNotesExtractsOnlyTheRequestedVersion(t *testing.T) {
	out, _, err := runScript(t, sampleChangelog, "v0.2.0")
	if err != nil {
		t.Fatalf("script failed: %v", err)
	}
	if !strings.Contains(out, "The newer thing.") {
		t.Errorf("missing 0.2.0 content:\n%s", out)
	}
	for _, forbidden := range []string{"The older thing.", "First release.", "Preamble"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("0.2.0 notes contain %q from another section:\n%s", forbidden, out)
		}
	}
}

// The leading "v" is optional, since tags carry it and CHANGELOG headings do not.
func TestReleaseNotesAcceptsVersionWithOrWithoutV(t *testing.T) {
	withV, _, err := runScript(t, sampleChangelog, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	withoutV, _, err := runScript(t, sampleChangelog, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if withV != withoutV {
		t.Errorf("v-prefixed and bare versions disagree:\n--- v0.1.0 ---\n%s\n--- 0.1.0 ---\n%s", withV, withoutV)
	}
}

// An unknown version must fail rather than publish empty notes under a tag
// that cannot be recut once the Go module proxy has cached it.
func TestReleaseNotesFailsOnMissingVersion(t *testing.T) {
	out, stderr, err := runScript(t, sampleChangelog, "v9.9.9")
	if err == nil {
		t.Fatalf("expected a non-zero exit for a version with no section, got success with:\n%s", out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected no stdout on failure, got:\n%s", out)
	}
	if !strings.Contains(stderr, "no section for") {
		t.Errorf("expected an explanatory error, got: %q", stderr)
	}
}

// The section must not begin or end with the blank lines that surround it in
// the file, so the release body starts at the prose.
func TestReleaseNotesTrimsSurroundingBlankLines(t *testing.T) {
	out, _, err := runScript(t, sampleChangelog, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(out, "\n") {
		t.Errorf("notes start with a blank line:\n%q", out)
	}
	if strings.HasSuffix(out, "\n\n") {
		t.Errorf("notes end with a blank line:\n%q", out)
	}
	if !strings.HasPrefix(out, "First release.") {
		t.Errorf("notes should open with the section's first line, got:\n%q", out)
	}
}
