// Package ninja is a small writer for ninja build files.
package ninja

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Rule is a ninja rule definition.
type Rule struct {
	Name        string
	Command     string
	Description string
	Depfile     string
	Deps        string // e.g. "gcc"
}

// Build is a ninja build statement: outputs = rule(inputs) with implicit deps.
type Build struct {
	Outputs  []string
	Rule     string
	Inputs   []string
	Implicit []string          // order-only-independent implicit deps ("| ...")
	Vars     map[string]string // per-build variable bindings
}

// File is an in-memory ninja file.
type File struct {
	vars   [][2]string
	rules  []Rule
	builds []Build
}

// SetVar adds a top-level variable binding, in insertion order.
func (f *File) SetVar(name, value string) { f.vars = append(f.vars, [2]string{name, value}) }

// AddRule appends a rule.
func (f *File) AddRule(r Rule) { f.rules = append(f.rules, r) }

// AddBuild appends a build statement.
func (f *File) AddBuild(b Build) { f.builds = append(f.builds, b) }

// String renders the file.
func (f *File) String() string {
	var sb strings.Builder
	_ = f.Write(&sb)
	return sb.String()
}

// Write renders the file to w.
func (f *File) Write(w io.Writer) error {
	bw := bufio.NewWriter(w)
	for _, kv := range f.vars {
		fmt.Fprintf(bw, "%s = %s\n", kv[0], kv[1])
	}
	if len(f.vars) > 0 {
		fmt.Fprintln(bw)
	}
	for _, r := range f.rules {
		fmt.Fprintf(bw, "rule %s\n  command = %s\n", r.Name, r.Command)
		if r.Description != "" {
			fmt.Fprintf(bw, "  description = %s\n", r.Description)
		}
		if r.Depfile != "" {
			fmt.Fprintf(bw, "  depfile = %s\n", r.Depfile)
		}
		if r.Deps != "" {
			fmt.Fprintf(bw, "  deps = %s\n", r.Deps)
		}
		fmt.Fprintln(bw)
	}
	for _, b := range f.builds {
		fmt.Fprintf(bw, "build %s: %s", strings.Join(escapeAll(b.Outputs), " "), b.Rule)
		if len(b.Inputs) > 0 {
			fmt.Fprintf(bw, " %s", strings.Join(escapeAll(b.Inputs), " "))
		}
		if len(b.Implicit) > 0 {
			fmt.Fprintf(bw, " | %s", strings.Join(escapeAll(b.Implicit), " "))
		}
		fmt.Fprintln(bw)
		for _, k := range sortedKeys(b.Vars) {
			fmt.Fprintf(bw, "  %s = %s\n", k, b.Vars[k])
		}
	}
	return bw.Flush()
}

// escapePath escapes the characters ninja treats specially in a path token.
func escapePath(s string) string {
	s = strings.ReplaceAll(s, "$", "$$")
	s = strings.ReplaceAll(s, " ", "$ ")
	s = strings.ReplaceAll(s, ":", "$:")
	return s
}

func escapeAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = escapePath(s)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
