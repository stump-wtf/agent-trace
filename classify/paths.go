package classify

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func normalizePath(opts *Options, cwd, base, path string) (string, *OutsideTouch, bool) {
	path = strings.TrimSpace(strings.Trim(path, `"'`))
	if path == "" || strings.Contains(path, "\n") {
		return "", nil, false
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return "", nil, false
	}
	if !filepath.IsAbs(path) {
		clean := filepath.Clean(path)
		if clean == "." || strings.HasPrefix(clean, "..") {
			return "", nil, false
		}
		if base != "" && filepath.IsAbs(base) {
			abs := filepath.Clean(filepath.Join(base, clean))
			if cwd != "" {
				root := filepath.Clean(cwd)
				if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
					return filepath.ToSlash(rel), nil, true
				}
			}
			return "", &OutsideTouch{Scope: outsideScope(opts, abs), Path: abs}, true
		}
		return filepath.ToSlash(clean), nil, true
	}
	abs := filepath.Clean(path)
	if cwd != "" {
		root := filepath.Clean(cwd)
		if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			return filepath.ToSlash(rel), nil, true
		}
	}
	return "", &OutsideTouch{Scope: outsideScope(opts, abs), Path: abs}, true
}

func outsideScope(opts *Options, path string) string {
	home := ""
	if opts != nil {
		home = opts.HomeDir
	}
	if home != "" {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return "home"
		}
	}
	tmp := "/tmp"
	if opts != nil && opts.TmpDir != "" {
		tmp = opts.TmpDir
	}
	if strings.HasPrefix(path, tmp) {
		return "tmp"
	}
	return "other"
}

type pathHit struct {
	path  string
	lines [][2]int
}

var pathLineRe = regexp.MustCompile(`(?:^|[\s"'([])([A-Za-z0-9_./@+-]*[A-Za-z0-9_/@+-]\.[A-Za-z0-9][A-Za-z0-9._-]*):([0-9]+)`)
var pathOnlyRe = regexp.MustCompile(`(?:^|[\s"'([])([./~A-Za-z0-9_@+-]*[/][A-Za-z0-9_./~@+-]*\.[A-Za-z0-9][A-Za-z0-9._-]*)(?:$|[\s"',)\]:;])`)
var commandPathRe = regexp.MustCompile(`(?:^|[\s"'=])([./~A-Za-z0-9_@+-]+\.[A-Za-z0-9][A-Za-z0-9._-]*)(?:$|[\s"',)\]:;])`)
var patchFileRe = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$|^\*\*\* Move to: (.+)$`)

func parsePathHits(text string) []pathHit {
	byPath := map[string][][2]int{}
	for _, m := range pathLineRe.FindAllStringSubmatch(text, -1) {
		line := 0
		fmt.Sscanf(m[2], "%d", &line)
		if line > 0 {
			if path, ok := cleanExtractedPath(m[1], true); ok {
				byPath[path] = append(byPath[path], [2]int{line, line})
			}
		}
	}
	for _, p := range extractPaths(text) {
		if _, ok := byPath[p]; !ok {
			byPath[p] = nil
		}
	}
	out := make([]pathHit, 0, len(byPath))
	for path, lines := range byPath {
		out = append(out, pathHit{path: path, lines: lines})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

func extractPaths(text string) []string {
	matches := pathOnlyRe.FindAllStringSubmatch(text, -1)
	seen := map[string]bool{}
	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		path, ok := cleanExtractedPath(m[1], false)
		if !ok {
			continue
		}
		if path == "" || seen[path] || strings.Contains(path, "://") {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	sortStrings(paths)
	return paths
}

func extractCommandPaths(command string) []string {
	matches := commandPathRe.FindAllStringSubmatch(command, -1)
	seen := map[string]bool{}
	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		path, ok := cleanExtractedPath(m[1], true)
		if !ok || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	sortStrings(paths)
	return paths
}

func parsePatchPaths(patch string) []string {
	matches := patchFileRe.FindAllStringSubmatch(patch, -1)
	seen := map[string]bool{}
	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		raw := m[1]
		if raw == "" {
			raw = m[2]
		}
		path, ok := cleanExtractedPath(raw, true)
		if !ok || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	sortStrings(paths)
	return paths
}

func cleanExtractedPath(path string, allowTopLevel bool) (string, bool) {
	path = strings.TrimSpace(strings.Trim(path, `"' ,;:()[]{}`))
	path = strings.TrimPrefix(path, "a/")
	path = strings.TrimPrefix(path, "b/")
	path = strings.TrimPrefix(path, "./")
	if path == "" || strings.Contains(path, "://") || strings.ContainsAny(path, "\n\r\t") {
		return "", false
	}
	if strings.HasPrefix(path, "--") || strings.HasPrefix(path, "++") {
		return "", false
	}
	if !allowTopLevel && !strings.Contains(path, "/") {
		return "", false
	}
	return path, true
}

func sortStrings(s []string) { sort.Strings(s) }
