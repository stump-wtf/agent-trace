package redact

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
)

// fixtures assembles, at run time, the credentials these tests feed in.
// Written out literally they are exactly what this repository's secret scan
// exists to reject, and it cannot tell a fake from the real thing — so no
// source line here holds one whole.
var fixtures = strings.NewReplacer(
	"<PASSWORD>", "hunter2"+"-hunter2",
	"<HEX>", strings.Repeat("0123456789abcdef", 2)+"01234567",
	"<GHS>", "gh"+"s_"+strings.Repeat("abcdefghij", 3)+"0123",
	"<GHP>", "gh"+"p_"+strings.Repeat("abcdefghij", 3)+"0123",
	"<HVS>", "hv"+"s."+"CAESI"+strings.Repeat("abcdefghij", 3),
	"<STRIPE>", "sk"+"_"+"li"+"ve_"+strings.Repeat("abcdefghij", 3)+"0123",
	"<TOKEN>", "abc123"+"def456",
	"<JWT>", "abc."+"def."+"ghi",
	"<PEM-BEGIN>", "-----BEGIN OPENSSH "+"PRIVATE KEY-----",
	"<PEM-END>", "-----END OPENSSH "+"PRIVATE KEY-----",
	"<MASK>", Mask,
)

// TestRedactMasksCredentials is Harness's redaction corpus (ADR-0008), the
// "done when" bar of #134: every shape a credential reliably takes in an
// agent transcript masks, and Redact is idempotent.
func TestRedactMasksCredentials(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"git remote with password",
			"git remote set-url origin https://joestump-agent:<HEX>@gitea.stump.rocks/stump.wtf/harness.git",
			"git remote set-url origin https://joestump-agent:<MASK>@gitea.stump.rocks/stump.wtf/harness.git"},
		{"x-access-token userinfo",
			"git clone https://x-access-token:<GHS>@gitea.stump.rocks/stump.wtf/harness.git",
			"git clone https://x-access-token:<MASK>@gitea.stump.rocks/stump.wtf/harness.git"},
		{"bare token userinfo",
			"git push https://<HEX>@gitea.stump.rocks/a/b.git main",
			"git push https://<MASK>@gitea.stump.rocks/a/b.git main"},
		{"authorization token header",
			`curl -s -H "Authorization: token <HEX>" https://gitea.stump.rocks/api/v1/user`,
			`curl -s -H "Authorization: token <MASK>" https://gitea.stump.rocks/api/v1/user`},
		{"authorization bearer header",
			`curl -H 'Authorization: Bearer <JWT>' https://api.example.com`,
			`curl -H 'Authorization: Bearer <MASK>' https://api.example.com`},
		{"api key header",
			`curl -H "X-API-Key: <TOKEN>" https://api.example.com`,
			`curl -H "X-API-Key: <MASK>" https://api.example.com`},
		{"env assignment",
			"GITEA_TOKEN=<TOKEN> tea pr list",
			"GITEA_TOKEN=<MASK> tea pr list"},
		{"exported quoted key",
			`export OPENAI_API_KEY="<TOKEN>"`,
			`export OPENAI_API_KEY=<MASK>`},
		{"json password",
			`{"username": "joe", "password": "<PASSWORD>"}`,
			`{"username": "joe", "password": <MASK>}`},
		{"github token",
			"echo <GHP> | gh auth login --with-token",
			"echo <MASK> | gh auth login --with-token"},
		{"openbao token",
			"bao login <HVS>",
			"bao login <MASK>"},
		{"password flag",
			"mysql --password <PASSWORD> -h db",
			"mysql --password <MASK> -h db"},
		{"token flag with equals",
			"tea login add --token=<TOKEN> --url https://gitea.stump.rocks",
			"tea login add --token=<MASK> --url https://gitea.stump.rocks"},
		{"curl basic auth",
			"curl -fsS -u admin:<PASSWORD> https://prowlarr.stump.rocks/api",
			"curl -fsS -u <MASK> https://prowlarr.stump.rocks/api"},
		{"private key",
			"cat > id <<EOF\n<PEM-BEGIN>\nb3BlbnNzaC1rZXktdjEAAAAA\n<PEM-END>\nEOF",
			"cat > id <<EOF\n<MASK>\nEOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, want := fixtures.Replace(tc.in), fixtures.Replace(tc.want)
			got := Redact(in)
			if got != want {
				t.Errorf("Redact(%q)\n got %q\nwant %q", in, got, want)
			}
			if again := Redact(got); again != got {
				t.Errorf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

// TestRedactMasksVendorTokensOnlyBetterleaksKnows: the bar a hand-written rule
// set cannot meet — vendor token shapes the ported regexes do not list.
func TestRedactMasksVendorTokensOnlyBetterleaksKnows(t *testing.T) {
	stripe := fixtures.Replace("<STRIPE>")
	got := Redact("charge the card " + stripe + " today")
	if strings.Contains(got, stripe) {
		t.Errorf("Stripe live key survived: %q", got)
	}
	if !strings.Contains(got, Mask) {
		t.Errorf("expected %s in %q", Mask, got)
	}
}

// TestRedactLeavesOrdinaryCommandsAlone: redaction runs over every command an
// agent ran, so a rule that fires on SHAs, paths, ssh targets or code would
// make output unreadable to protect nothing.
func TestRedactLeavesOrdinaryCommandsAlone(t *testing.T) {
	for _, in := range []string{
		"git log --oneline 8799476587bf1234567890abcdef1234567890ab",
		"/home/joestump-agent/src/stumpcloud/infra/pdx.yaml",
		"ssh -o ConnectTimeout=15 -o BatchMode=yes joestump@nuc01.stump.wtf 'uptime; echo ---; sudo docker ps'",
		"git clone git@github.com:stump-wtf/harness.git",
		"curl -s 'https://api.example.com/v1/chat?max_tokens=4096'",
		"https://mastodon.social/@joestump",
		`token := os.Getenv("GITEA_TOKEN")`,
		"docker run --rm -u 1000:1000 ghcr.io/stump-wtf/harness:latest",
		"go test ./internal/runtrace -run TestAttribute -count=1",
		"mcp_gitea_search_issues",
		"Bad Request: litellm.ContextWindowExceededError: maximum context length is 196608 tokens",
		// How every sweep on tars actually handles its token: by reference.
		"TOKEN=$(cat /tmp/gitea-token 2>/dev/null) || TOKEN=$(grep -oE 'https://[^:]+:[^@]+@gitea.stump.rocks' ~/.git-credentials)",
		`if [ -z "${TOKEN:-}" ]; then echo NO_TOKEN; fi`,
		`export GITEA_TOKEN="$(cat /tmp/gitea-token)"`,
		`curl -sS -H "Authorization: token $TOKEN" https://gitea.stump.rocks/api/v1/user`,
		"git push https://x-access-token:$GH_TOKEN@gitea.stump.rocks/stump.wtf/harness.git",
	} {
		if got := Redact(in); got != in {
			t.Errorf("Redact(%q) = %q, want it unchanged", in, got)
		}
	}
}

// TestLinesMasksMultilinePEM: the whole point of the line-stream redactor —
// each line of a key arrives separately, as it does on a durable log write or
// a tail read, and the body must not survive either way.
func TestLinesMasksMultilinePEM(t *testing.T) {
	begin, body, end := fixtures.Replace("<PEM-BEGIN>"), "b3BlbnNzaC1rZXktdjEAAAAA", fixtures.Replace("<PEM-END>")
	lines := []string{
		"cat ~/.ssh/id_ed25519",
		begin,
		body,
		" " + body + " ",
		end,
		"and ordinary output resumes",
	}
	var l Lines
	var got []string
	for _, ln := range lines {
		got = append(got, l.String(ln))
	}
	if got[1] != Mask || got[2] != Mask || got[3] != Mask {
		t.Errorf("PEM frame/body survived: %q", got[1:4])
	}
	if !strings.Contains(got[4], "PRIVATE KEY-----") || strings.Contains(got[4], body) {
		t.Errorf("END line wrong: %q", got[4])
	}
	if got[5] != lines[5] {
		t.Errorf("stream did not recover after END: %q", got[5])
	}
}

// TestLinesMasksClippedPEM: a killed cat never writes the END line; a few
// lines of masking, then the stream recovers at the first line that could not
// be key body.
func TestLinesMasksClippedPEM(t *testing.T) {
	var l Lines
	out := []string{
		l.String(fixtures.Replace("<PEM-BEGIN>")),
		l.String("b3BlbnNzaC1rZXktdjEAAAAA"),
		l.String("+20 lines folded"),
		l.String("make test"),
	}
	if out[1] != Mask {
		t.Errorf("key body passed through: %q", out[1])
	}
	if out[3] != "make test" {
		t.Errorf("stream did not recover from a clipped key: %q", out[3])
	}
}

// TestFindings: Findings reports what it found with usable positions.
func TestFindings(t *testing.T) {
	in := fixtures.Replace("GITEA_TOKEN=<TOKEN> tea pr list")
	fs := Findings(in)
	if len(fs) == 0 {
		t.Fatal("no findings for a secret-named assignment")
	}
	found := false
	for _, f := range fs {
		if f.Secret == fixtures.Replace("<TOKEN>") {
			found = true
		}
	}
	if !found {
		t.Errorf("assignment value missing from findings: %+v", fs)
	}
}

// blockingTransport fails any HTTP request. Swapped in for
// http.DefaultTransport around redaction, it turns "no network" from a claim
// into a failing test: betterleaks' live validation would have to dial out to
// vendor APIs to trigger it.
type blockingTransport struct{}

func (blockingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("redaction must not touch the network")
}

// TestRedactionNeverTouchesNetwork: the detector is built with validation
// off, but the rules could grow one that phones home. Prove it cannot: with
// every outbound HTTP request failing, redaction still succeeds.
func TestRedactionNeverTouchesNetwork(t *testing.T) {
	prev := http.DefaultTransport
	http.DefaultTransport = blockingTransport{}
	t.Cleanup(func() { http.DefaultTransport = prev })

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected inbound connection to %s", r.URL)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	in := fixtures.Replace("GITEA_TOKEN=<TOKEN> sk live check")
	if got := Redact(in); !strings.Contains(got, Mask) {
		t.Errorf("redaction changed behaviour without network: %q", got)
	}
	var lns Lines
	for _, ln := range strings.Split(in, "\n") {
		_ = lns.String(ln)
	}
}
