// Credential Redaction
//
// The one redactor Harness, agent-trace and Cairn share (Harness ADR-0033,
// Decision 7). Agents put credentials in transcripts constantly — a curl
// Authorization header, a git remote with a token in the userinfo, a
// GITEA_TOKEN=… on a command line — and those transcripts are displayed,
// exported and shared. Everything that renders agent output should pass it
// through here first.
//
// The engine is betterleaks (github.com/betterleaks/betterleaks) with its
// default rule set, pinned to an exact version: vendor token shapes (Stripe's
// sk_live_, AWS, Slack, GitHub, GitLab, JWTs and friends) that hand-written
// regexes chase badly. On top of it sit the rules ported from Harness's
// internal/redact — URL userinfo, Authorization-style headers, secret-named
// assignments and flags, curl -u, and multi-line PEM blocks — which match the
// shapes a credential reliably takes in an agent transcript and deliberately
// leave alone what only refers to one ($VAR, $(cat file), ${VAR:-}). A bare
// 40-hex string is a commit SHA far more often than a token, so it is not
// matched either. This is defence in depth for display, not a guarantee; a
// secret in an unrecognised shape passes through.
//
// This is the only package in the module that imports betterleaks. Its
// library API is still changing, so upgrades touch this package alone.
//
// No network: betterleaks' live validation of findings stays off — the
// detector is built without validation, and redaction never opens a socket.
//
// @joestump-agent 09/27/2026 - Added for stump.wtf/agent-trace#134, porting
// the rules from harness internal/redact (ADR-0008) onto the betterleaks
// engine.
package redact

import (
	"context"
	"regexp"
	"strings"
	"sync"

	blconfig "github.com/betterleaks/betterleaks/config"
	bldetect "github.com/betterleaks/betterleaks/detect"
	blreport "github.com/betterleaks/betterleaks/report"
)

// Mask is what a redacted value is replaced with, matching Harness and Cairn.
const Mask = "[REDACTED]"

// Finding reports one credential recognised in the input.
type Finding struct {
	RuleID      string
	Description string
	Secret      string
	StartLine   int
	EndLine     int
	StartColumn int
	EndColumn   int
}

// rules are the ported Harness rules, run in order. Each keeps the label that
// identified the secret and swaps only the value. No value rule starts on "["
// or "$", so a Mask already in place, or a reference to a secret, is never
// re-matched — Redact is idempotent.
var rules = []struct {
	re   *regexp.Regexp
	repl string
	id   string
}{
	// URL userinfo with a password: scheme://user:secret@host.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s/@:\[\]]+:)[^\s/@$\[][^\s/@]*@`), "${1}" + Mask + "@", "url-userinfo"},
	// A token as the whole http(s) userinfo: https://<token>@host.
	{regexp.MustCompile(`(?i)\b(https?://)[^\s/@:\[\]$]+@`), "${1}" + Mask + "@", "url-userinfo-token"},
	// Authorization-style headers, bare or as a quoted JSON or dict value:
	// with a scheme word, the value after it; with none, a bare value long
	// enough not to be the scheme word itself (the longest, bearer and digest,
	// are six letters).
	{regexp.MustCompile(`(?i)\b((?:proxy-)?authorization\s*["']?\s*[:=]\s*["']?\s*(?:bearer|token|basic|digest)\s+)[^\s"',;\[$][^\s"',;]*`), "${1}" + Mask, "authorization-header"},
	{regexp.MustCompile(`(?i)\b((?:proxy-)?authorization\s*["']?\s*[:=]\s*["']?\s*)[^\s"',;\[$][^\s"',;]{7,}`), "${1}" + Mask, "authorization-header"},
	{regexp.MustCompile(`(?i)\b((?:x-)?(?:api-?key|auth-?token|access-?token|private-token|gitea-token)\s*["']?\s*[:=]\s*["']?\s*)[^\s"',;\[$][^\s"',;@]*`), "${1}" + Mask, "token-header"},
	// A bearer credential outside a header: sent bearer <token>, where a
	// scheme-word label says what follows is a credential.
	{regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]{16,}`), "${1}" + Mask, "bearer"},
	// Vendor token shapes: GitHub, GitLab, Slack, OpenAI/Anthropic-style sk-,
	// AWS access key ids, OpenBao/Vault tokens, and JWTs. betterleaks' default
	// rules also carry vendor shapes, but its keyword and entropy gates let
	// short synthetic tokens through; this rule does not gate.
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|sk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|hv[sbr]\.[A-Za-z0-9_-]{20,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})\b`), Mask, "vendor-token"},
	// A PEM private key: the whole block, or to the end of a clipped one.
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?s:.*?)(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`), Mask, "pem-key"},
	// A secret-named assignment: GITEA_TOKEN=…, "password": "…", api_key: ….
	// The name must END in the secret word (max_tokens= is not a secret) and
	// not sit inside ${…} (a default, not a value); a value starting with "="
	// is Go's := and one starting with "-" is the next flag.
	{regexp.MustCompile(`(?i)(^|[^\w.${-])([a-z0-9_.-]*(?:token|secret|password|passwd|passphrase|api[_-]?key|access[_-]?key|private[_-]?key)"?\s*[=:]\s*)("[^"$][^"]*"|'[^'$][^']*'|[^\s"',;&=\[$-][^\s"',;&]*)`), "${1}${2}" + Mask, "secret-assignment"},
	// A secret-named flag given its value as the next word.
	{regexp.MustCompile(`(?i)(\s--?(?:password|passwd|token|api-key|secret|client-secret)\s+)("[^"$][^"]*"|'[^'$][^']*'|[^\s"'\[$-]\S*)`), "${1}" + Mask, "secret-flag"},
	// Basic auth handed to curl or wget through their -u and --user flags,
	// in both --user user:pass and --user=user:pass shapes. Scoped to those
	// tools, because docker's -u names a uid and gid.
	{regexp.MustCompile(`(\b(?:curl|wget)\b[^|;&\n]*?\s(?:-u|--user)[=\s][^\s:]*:)([^\s\[$]\S*)`), "${1}" + Mask, "curl-user"},
}

// PEM frame rules for the line-stream redactor.
var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// pemBody is what a line inside a key block looks like once the frame a
	// TUI draws around tool output is trimmed away: base64, or one of the
	// legacy RFC 1421 headers (Proc-Type, DEK-Info) an encrypted RSA key
	// carries before its body.
	pemBody = regexp.MustCompile(`^(?:[A-Za-z0-9+/=]+|[A-Za-z-]+: .*)$`)
)

var (
	detectOnce sync.Once
	detector   *bldetect.Detector
	detectErr  error
	// betterleaks' Detector is not documented as safe for concurrent Detect
	// calls; the shared instance is guarded, since the package exposes no
	// constructor and the cost is one build per process.
	detectMu sync.Mutex
)

// engine returns the process-wide betterleaks Detector, built once from the
// embedded default rule set with live validation disabled (no network).
func engine() (*bldetect.Detector, error) {
	detectOnce.Do(func() {
		cfg, err := blconfig.ParseTOMLString(blconfig.DefaultConfig, "betterleaks.toml")
		if err != nil {
			detectErr = err
			return
		}
		detector = bldetect.NewDetectorContext(context.Background(), cfg, bldetect.ValidationOptions{})
	})
	return detector, detectErr
}

// blFindings runs the betterleaks default rules over s.
func blFindings(s string) []blreport.Finding {
	d, err := engine()
	if err != nil {
		return nil
	}
	detectMu.Lock()
	defer detectMu.Unlock()
	return d.DetectString(s)
}

// Redact returns s with every recognised credential replaced by Mask. It is
// idempotent: redacting already-redacted text changes nothing.
func Redact(s string) string {
	if s == "" {
		return s
	}
	for _, f := range blFindings(s) {
		// A finding whose secret is already a Mask is the engine re-matching
		// previously redacted output (its generic-password rule reads
		// "[REDACTED]" as a hardcoded password literal); skipping it keeps
		// Redact idempotent.
		if f.Secret != "" && !strings.Contains(f.Secret, Mask) {
			s = strings.ReplaceAll(s, f.Secret, Mask)
		}
	}
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// Findings returns every credential recognised in s, from the betterleaks
// default rules and the ported Harness rules. Overlapping findings are not
// deduplicated; Redact masks them all regardless.
func Findings(s string) []Finding {
	var out []Finding
	for _, f := range blFindings(s) {
		out = append(out, Finding{
			RuleID:      f.RuleID,
			Description: f.Description,
			Secret:      f.Secret,
			StartLine:   f.StartLine,
			EndLine:     f.EndLine,
			StartColumn: f.StartColumn,
			EndColumn:   f.EndColumn,
		})
	}
	for _, r := range rules {
		for _, loc := range r.re.FindAllStringSubmatchIndex(s, -1) {
			// The secret is the last capture group the rule fills; for the
			// value-keeping rules it is the final group. A rule with no
			// capture group leaves that pair -1/-1, and the whole match is
			// then the only span there is to report.
			secretStart, secretEnd := loc[0], loc[1]
			secret := ""
			if n := len(loc); n >= 4 && loc[n-2] >= 0 {
				secretStart, secretEnd = loc[n-2], loc[n-1]
				secret = s[secretStart:secretEnd]
			}
			// Lines and columns count from 1, as betterleaks' findings do, so
			// a caller can put the two engines' positions side by side. The
			// column is offset from the start of the secret's own line, not
			// from the start of s.
			lineStart := strings.LastIndex(s[:secretStart], "\n") + 1
			line := 1 + strings.Count(s[:secretStart], "\n")
			out = append(out, Finding{
				RuleID:      r.id,
				Description: r.re.String(),
				Secret:      secret,
				StartLine:   line,
				EndLine:     line,
				StartColumn: secretStart - lineStart + 1,
				EndColumn:   secretEnd - lineStart + 1,
			})
		}
	}
	return out
}

// Lines masks a sequence of lines, one call per line, in order. The zero
// value is ready to use. It is not safe for concurrent use; give each stream
// its own.
//
// String returns ln masked as Redact would mask it, and additionally masks
// every line of a PEM private key body that follows a BEGIN line. A whole PEM
// block only matches Redact's engine when it is in one string; a
// line-at-a-time caller loses exactly that, and the body IS the key. A key
// block ends at its END line, or at the first line that could not be part of
// one — a clipped cat, a TUI's "+20 lines" fold — so a key whose END never
// arrives costs a few lines of masking, never the rest of the stream.
type Lines struct {
	inKey bool
}

// String masks one line, carrying PEM block state across calls.
func (l *Lines) String(ln string) string {
	if l.inKey {
		if pemEnd.MatchString(ln) {
			l.inKey = false
			return Redact(ln)
		}
		switch body := trimFrame(ln); {
		case body == "":
			return ln
		case pemBody.MatchString(body):
			return Mask
		}
		l.inKey = false
	}
	out := Redact(ln)
	if loc := pemBegin.FindStringIndex(ln); loc != nil && !pemEnd.MatchString(ln[loc[1]:]) {
		l.inKey = true
	}
	return out
}

// trimFrame strips the whitespace and box-drawing a TUI puts around a line of
// tool output ("│ … │", "⎿ …"), leaving the content an agent printed.
func trimFrame(ln string) string {
	return strings.TrimFunc(ln, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '|' || r > 0x7f
	})
}
