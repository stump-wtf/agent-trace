// Package strutil provides shared string utilities used across classify and tail.
package strutil

// TruncateRunes truncates s to max runes, appending suffix if truncated.
func TruncateRunes(s string, max int, suffix string) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	suffixRunes := []rune(suffix)
	cut := max - len(suffixRunes)
	if cut < 0 {
		cut = 0
	}
	return string(runes[:cut]) + suffix
}
