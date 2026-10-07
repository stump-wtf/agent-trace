package redact

// Rule Prefilter
//
// Every ported rule (redact.go's rules) needs some literal to match: a scheme's "://", a header's
// "authorization", a vendor prefix, a secret word followed by "=" or ":".
// mayMatch checks for those literals, and a string with none of them is one no
// ported rule can change, so Redact skips the rules for it. The betterleaks
// pass is not gated here: the engine gates each of its rules on its own
// keywords, and costs tens of milliseconds over megabytes of text.
//
// That matters for a structured stream (jsonline.go). A tool result that
// carries an image is megabytes of base64 with no newline, and the rules over
// one string that long run Go's regexp NFA at about 1 MB/s while the agent
// waits on its stdout (1.3s for 4 MiB of base64 before this prefilter).
// Base64 contains none of the literals, so the prefilter passes it in
// milliseconds.
//
// The prefilter may say yes to a line no rule matches, never no to one a rule
// does. The case-insensitive rules are checked against a fold of the line
// that maps every character Go's (?i) matches to an ASCII letter onto that
// letter: ASCII upper case, the Kelvin sign and the long s.
// FuzzMayMatchCoversRules holds it to that.
//
// @joestump-agent 09/28/2026 - Added to Harness's internal/redact for the
// stream-json pipe spawn (https://github.com/stump-wtf/harness/issues/18).
// @joestump 10/07/2026 - Moved here with the bearer needle this package's
// rules add, so Harness can drop its own redactor
// (https://github.com/stump-wtf/harness/issues/912).

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// literal needles of the case-sensitive rules: the PEM header, the vendor
// token prefixes, and curl/wget basic auth.
var caseNeedles = []string{
	"-----BEGIN ",
	"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-",
	"xoxa-", "xoxb-", "xoxp-", "xoxr-", "xoxs-", "sk-", "AKIA", "hvs.", "hvb.", "hvr.",
	"curl", "wget",
}

// folded needles of the case-insensitive rules: URL userinfo, Authorization
// and token headers, bearer credentials, and secret-named flags.
var foldNeedles = []string{
	"://", "authorization", "bearer",
	"apikey", "api-key", "authtoken", "auth-token", "accesstoken", "access-token", "private-token", "gitea-token",
	"-password", "-passwd", "-token", "-api-key", "-secret",
}

// assignWords are the words a secret-named assignment's name ends in; each
// api/access/private key variant ends in "key".
var assignWords = []string{"token", "secret", "password", "passwd", "passphrase", "key"}

// mayMatch reports whether any ported rule could change s.
func mayMatch(s string) bool {
	for _, n := range caseNeedles {
		if strings.Contains(s, n) {
			return true
		}
	}
	// A JWT: eyJ, and the dots between its parts.
	if strings.Contains(s, "eyJ") && strings.Contains(s, ".") {
		return true
	}
	f := fold(s)
	for _, n := range foldNeedles {
		if strings.Contains(f, n) {
			return true
		}
	}
	for _, w := range assignWords {
		for i := 0; ; {
			j := strings.Index(f[i:], w)
			if j < 0 {
				break
			}
			i += j + len(w)
			if assignsAt(f, i) {
				return true
			}
		}
	}
	return false
}

// assignsAt reports whether f[i:] continues a name with the assignment
// rule's `"?\s*[=:]`.
func assignsAt(f string, i int) bool {
	if i < len(f) && f[i] == '"' {
		i++
	}
	for i < len(f) && (f[i] == ' ' || f[i] == '\t' || f[i] == '\n' || f[i] == '\f' || f[i] == '\r') {
		i++
	}
	return i < len(f) && (f[i] == '=' || f[i] == ':')
}

// fold lower-cases s the way the case-insensitive rules compare it: every
// character that simple-folds to an ASCII letter becomes that letter.
func fold(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s)
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case 'K': // KELVIN SIGN folds to k
			return 'k'
		case 'ſ': // LATIN SMALL LETTER LONG S folds to s
			return 's'
		}
		return unicode.ToLower(r)
	}, s)
}
