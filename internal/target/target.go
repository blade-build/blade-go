// Package target holds the in-memory model of a Blade target and the registry
// that collects them during BUILD-file loading.
package target

import (
	"fmt"
	"sort"
)

// Target is one declared build target (a cc_library, proto_library, ...).
//
// Attrs holds every keyword argument the rule was called with, converted from
// Starlark to Go values (string, int, bool, []any, map[string]any). Downstream
// phases interpret them per rule type; the loader deliberately does not validate
// rule-specific attributes yet.
type Target struct {
	Type    string         // rule name, e.g. "cc_library"
	Name    string         // target name within its package
	Package string         // slash path relative to the workspace root ("" == root)
	Attrs   map[string]any // all keyword arguments, Starlark converted to Go
	Pos     string         // source location "pkg/BUILD:12"
}

// Label returns the canonical "//package:name" identifier.
func (t *Target) Label() string {
	return "//" + t.Package + ":" + t.Name
}

// AttrStrings returns attr `name` normalized to a string slice, accepting the
// str-or-list convention Blade uses (a bare string becomes a one-element slice).
// A missing attr yields nil.
func (t *Target) AttrStrings(name string) []string {
	switch v := t.Attrs[name].(type) {
	case nil:
		return nil
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// AttrString returns attr `name` as a string, or "" if absent / not a string.
func (t *Target) AttrString(name string) string {
	s, _ := t.Attrs[name].(string)
	return s
}

// AttrBool returns a boolean attribute (false when absent or not a bool).
func (t *Target) AttrBool(name string) bool {
	b, _ := t.Attrs[name].(bool)
	return b
}

// Registry collects targets keyed by label, rejecting duplicates.
type Registry struct {
	byLabel map[string]*Target
	order   []*Target
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byLabel: map[string]*Target{}}
}

// Add registers t, returning an error if its label already exists.
func (r *Registry) Add(t *Target) error {
	label := t.Label()
	if prev, ok := r.byLabel[label]; ok {
		return fmt.Errorf("duplicate target %s (already defined at %s)", label, prev.Pos)
	}
	r.byLabel[label] = t
	r.order = append(r.order, t)
	return nil
}

// Get returns the target for a label, or nil.
func (r *Registry) Get(label string) *Target { return r.byLabel[label] }

// Len returns the number of registered targets.
func (r *Registry) Len() int { return len(r.order) }

// All returns the targets in declaration order.
func (r *Registry) All() []*Target {
	out := make([]*Target, len(r.order))
	copy(out, r.order)
	return out
}

// Labels returns all registered labels, sorted.
func (r *Registry) Labels() []string {
	out := make([]string, 0, len(r.byLabel))
	for l := range r.byLabel {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}
