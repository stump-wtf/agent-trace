package redact

// The prefilter may only ever say yes too often. These tests hold it to never
// saying no to a line String would change.

import (
	"strings"
	"testing"
	"unicode"
)

// rulesOnly runs the ported rules with no prefilter: what mayMatch must
// never say no to.
func rulesOnly(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// prefilterCorpus is every shape the rules are written for, including the
// ones a lax fold would miss.
var prefilterCorpus = []string{
	"git remote set-url origin https://joestump-agent:<HEX>@gitea.stump.rocks/a.git",
	"git push https://<HEX>@gitea.stump.rocks/a/b.git main",
	`curl -s -H "Authorization: token <HEX>" https://x`,
	`curl -H 'AUTHORIZATION: Bearer <JWT>' https://x`,
	`curl -H "X-API-Key: <TOKEN>" https://x`,
	"GITEA_TOKEN=<TOKEN> tea pr list",
	`export OPENAI_API_KEY="<TOKEN>"`,
	`{"username": "joe", "password": "<PASSWORD>"}`,
	`{"passphrase" : "<PASSWORD>"}`,
	"echo <GHP> | gh auth login --with-token",
	"bao login <HVS>",
	"sent bearer " + "abcdefghij" + "0123456789xyz",
	`{"Authorization": "token <HEX>"}`,
	"wget --user=admin:<PASSWORD> https://x",
	"mysql --password <PASSWORD> -h db",
	"tea login add --token=<TOKEN> --url https://x",
	"curl -fsS -u admin:<PASSWORD> https://x",
	"<PEM-BEGIN>",
	"aws configure set aws_access_key_id AKIA" + strings.Repeat("A", 16),
	"eyJ" + strings.Repeat("a", 10) + "." + strings.Repeat("b", 10) + "." + strings.Repeat("c", 10),
	// Go's (?i) matches the Kelvin sign as k and the long s as s.
	"API_KEY=<TOKEN>",
	"ſecret: <TOKEN>",
}

func TestMayMatchCoversTheCorpus(t *testing.T) {
	for _, raw := range prefilterCorpus {
		in := fixtures.Replace(raw)
		if rulesOnly(in) == in {
			t.Errorf("corpus line %q is not masked by String; it proves nothing here", in)
			continue
		}
		if !mayMatch(in) {
			t.Errorf("mayMatch(%q) = false, but the ported rules mask it", in)
		}
	}
}

// Base64 (an image in a tool result) and ordinary text pass the prefilter,
// which is the point of it.
func TestMayMatchPassesBase64AndProse(t *testing.T) {
	b64 := strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg", 1000) + "=="
	for _, s := range []string{b64, "func main() { fmt.Println(\"hello\", 42) }", "the build passed in 1.4s"} {
		if mayMatch(s) {
			t.Errorf("mayMatch(%.60q…) = true", s)
		}
	}
}

// fold must map every non-ASCII character whose simple case-fold orbit holds
// an ASCII letter onto that letter, or a case-insensitive rule could match a
// line the prefilter passed. Checked over all of Unicode rather than trusted.
func TestFoldMatchesRegexpCaseFolding(t *testing.T) {
	for r := rune(utf8Self); r <= unicode.MaxRune; r++ {
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < utf8Self && unicode.IsLetter(f) {
				if got, want := fold(string(r)), string(unicode.ToLower(f)); got != want {
					t.Errorf("fold(%U) = %q; (?i) matches it as %q", r, got, want)
				}
				break
			}
		}
	}
}

const utf8Self = 0x80

// Whatever the ported rules change, mayMatch must have let through.
func FuzzMayMatchCoversRules(f *testing.F) {
	for _, raw := range prefilterCorpus {
		f.Add(fixtures.Replace(raw))
	}
	f.Add("the build passed")
	f.Fuzz(func(t *testing.T, s string) {
		if rulesOnly(s) != s && !mayMatch(s) {
			t.Errorf("the ported rules change %q but mayMatch passed it", s)
		}
	})
}
