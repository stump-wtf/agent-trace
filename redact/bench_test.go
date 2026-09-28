package redact

import (
	"strings"
	"testing"
)

// Representative durable-log lines: mostly ordinary output, which is the real
// steady state for transcript rendering. The masked-lines benchmark carries
// the same content Harness's bench does, so the two engines are comparable.
var benchLines = []string{
	"2026/09/12 15:03:37 INFO state changed from=running to=stopping",
	"   ✓ internal/supervisor  1.492s",
	"cd /tmp/cairn-rv216 && go build ./... && go vet ./... && echo BUILD_VET_OK",
	"curl -s -H \"Authorization: token $GITEA_TOKEN\" https://gitea.stump.rocks/api/v1/repos/x/y",
	"git remote set-url origin https://user:hunter2@example.com/a/b.git",
	"        at foo.go:123 in packageName.functionName()",
}

func BenchmarkRedactPerLine(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Redact(benchLines[i%len(benchLines)])
	}
}

func BenchmarkRedactOrdinaryLineOnly(b *testing.B) {
	ln := "   ✓ internal/supervisor  1.492s"
	for i := 0; i < b.N; i++ {
		_ = Redact(ln)
	}
}

func BenchmarkLinesPerLine(b *testing.B) {
	var l Lines
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = l.String(benchLines[i%len(benchLines)])
	}
}

// BenchmarkRedactLargeFragment: transcript-sized input, where the betterleaks
// keyword prefilter earns its keep.
func BenchmarkRedactLargeFragment(b *testing.B) {
	in := strings.Repeat(strings.Join(benchLines, "\n")+"\n", 100)
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Redact(in)
	}
}
