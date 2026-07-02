// Package ninjaparse parses a ninja build file into its build edges. It exists
// to compare blade-go's generated build graph against Python Blade's (the
// oracle) in the differential harness.
package ninjaparse

import (
	"path"
	"strings"
)

// Edge is one `build` statement: outputs = rule(inputs) with implicit deps.
type Edge struct {
	Outputs  []string
	Rule     string
	Inputs   []string
	Implicit []string
}

// Parse extracts the build edges from a ninja file. It joins `$`-continued
// lines, unescapes ninja path escapes, and ignores rules/vars/comments.
func Parse(src string) []Edge {
	var edges []Edge
	for _, line := range logicalLines(src) {
		if !strings.HasPrefix(line, "build ") {
			continue
		}
		rest := strings.TrimPrefix(line, "build ")
		colon := findUnescaped(rest, ':')
		if colon < 0 {
			continue
		}
		outs := splitPaths(rest[:colon])
		after := splitFields(rest[colon+1:])
		if len(after) == 0 {
			continue
		}
		e := Edge{Outputs: outs, Rule: after[0]}
		dst := &e.Inputs
		for _, tok := range after[1:] {
			switch tok {
			case "|":
				dst = &e.Implicit
			case "||":
				dst = new([]string) // order-only: ignore
			default:
				*dst = append(*dst, unescape(tok))
			}
		}
		edges = append(edges, e)
	}
	return edges
}

// logicalLines joins `$`-continued physical lines into logical ones.
func logicalLines(src string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if strings.HasSuffix(line, "$") {
			cur.WriteString(strings.TrimSuffix(line, "$"))
			continue
		}
		cur.WriteString(line)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func findUnescaped(s string, ch byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '$' {
			i++ // skip escaped char
			continue
		}
		if s[i] == ch {
			return i
		}
	}
	return -1
}

func splitPaths(s string) []string {
	var out []string
	for _, f := range splitFields(s) {
		out = append(out, unescape(f))
	}
	return out
}

// splitFields splits on unescaped whitespace, keeping `$`-escaped chars (e.g. a
// `$ ` escaped space) attached to their token.
func splitFields(s string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '$' && i+1 < len(s) {
			cur.WriteByte(c)
			cur.WriteByte(s[i+1])
			i++
			continue
		}
		if c == ' ' || c == '\t' {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func unescape(s string) string {
	s = strings.ReplaceAll(s, "$ ", " ")
	s = strings.ReplaceAll(s, "$:", ":")
	s = strings.ReplaceAll(s, "$$", "$")
	return s
}

// CompiledSources returns the set of C/C++ source-file basenames that any edge
// compiles (inputs ending in a C/C++ source extension), across the file.
func CompiledSources(edges []Edge) map[string]bool {
	set := map[string]bool{}
	for _, e := range edges {
		for _, in := range e.Inputs {
			if isCXXSource(in) || strings.HasSuffix(in, ".c") {
				set[path.Base(in)] = true
			}
		}
	}
	return set
}

// Archives returns the set of static-archive basenames produced by any edge.
func Archives(edges []Edge) map[string]bool {
	set := map[string]bool{}
	for _, e := range edges {
		for _, o := range e.Outputs {
			if strings.HasSuffix(o, ".a") {
				set[path.Base(o)] = true
			}
		}
	}
	return set
}

func isCXXSource(s string) bool {
	for _, ext := range []string{".cc", ".cpp", ".cxx", ".C", ".c++", ".mm"} {
		if strings.HasSuffix(s, ext) {
			return true
		}
	}
	return false
}
