// Package sanitizer parses the --sanitizer flag into -fsanitize flags, a
// build-dir tag, and the test runtime environment. Mirrors Python Blade's
// sanitizer module (blade-build#1038): Address / Undefined / Leak / Thread /
// Memory. A sanitizer is a per-run choice, not project config. The parsed set is
// canonical (deduped + sorted) so it drives the flags and the build-dir tag
// identically regardless of argument order.
package sanitizer

import (
	"fmt"
	"sort"
	"strings"
)

// canonical -fsanitize= name -> short build-dir tag (also its `--sanitizer`
// alias). Single source of truth for the set.
var tags = map[string]string{
	"address":   "asan",
	"undefined": "ubsan",
	"leak":      "lsan",
	"thread":    "tsan",
	"memory":    "msan",
}

// aliases: both the canonical name and the short tag map to the canonical name.
var aliases = func() map[string]string {
	m := map[string]string{}
	for canonical, tag := range tags {
		m[canonical] = canonical
		m[tag] = canonical
	}
	return m
}()

// incompatible: each uses a different shadow-memory/runtime model, so they are
// mutually exclusive; `undefined` is pure instrumentation and composes freely.
var incompatible = map[string]map[string]bool{
	"address": {"thread": true, "memory": true},
	"leak":    {"thread": true, "memory": true},
	"thread":  {"address": true, "leak": true, "memory": true},
	"memory":  {"address": true, "leak": true, "thread": true},
}

var optionsVar = map[string]string{
	"address": "ASAN_OPTIONS", "undefined": "UBSAN_OPTIONS", "leak": "LSAN_OPTIONS",
	"thread": "TSAN_OPTIONS", "memory": "MSAN_OPTIONS",
}

// Parse turns a --sanitizer value into a sorted list of canonical names. Empty
// yields nil (off); an unknown name is an error.
func Parse(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	set := map[string]bool{}
	for _, name := range strings.Split(value, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		c, ok := aliases[name]
		if !ok {
			return nil, fmt.Errorf("unknown --sanitizer %q (supported: %s)", name, known())
		}
		set[c] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

func known() string {
	ks := make([]string, 0, len(aliases))
	for k := range aliases {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

// CompileFlags returns the compile flags for the active set (nil if empty).
func CompileFlags(sanitizers []string) []string {
	if len(sanitizers) == 0 {
		return nil
	}
	// Frame pointers + debug info for readable, symbolized reports.
	flags := []string{"-fsanitize=" + strings.Join(sanitizers, ","), "-fno-omit-frame-pointer", "-g"}
	if has(sanitizers, "undefined") {
		// Make UBSan findings fatal so a test actually fails on them.
		flags = append(flags, "-fno-sanitize-recover=undefined")
	}
	if has(sanitizers, "memory") {
		// Track where an uninitialized value was allocated (MSan is nearly
		// unusable without origins).
		flags = append(flags, "-fsanitize-memory-track-origins=2")
	}
	return flags
}

// LinkFlags returns the link flags (pull in the runtime); nil if empty.
func LinkFlags(sanitizers []string) []string {
	if len(sanitizers) == 0 {
		return nil
	}
	return []string{"-fsanitize=" + strings.Join(sanitizers, ",")}
}

// BuildTag returns the build-dir tag for the set (e.g. "asan", "asan+ubsan").
func BuildTag(sanitizers []string) string {
	ts := make([]string, len(sanitizers))
	for i, s := range sanitizers {
		ts[i] = tags[s]
	}
	return strings.Join(ts, "+")
}

// CheckCompat errors if the requested sanitizers cannot be combined.
func CheckCompat(sanitizers []string) error {
	set := map[string]bool{}
	for _, s := range sanitizers {
		set[s] = true
	}
	for _, s := range sanitizers {
		var conflicts []string
		for c := range incompatible[s] {
			if set[c] {
				conflicts = append(conflicts, c)
			}
		}
		if len(conflicts) > 0 {
			sort.Strings(conflicts)
			return fmt.Errorf("--sanitizer: %q cannot be combined with %s", s, strings.Join(conflicts, ", "))
		}
	}
	return nil
}

// CheckToolchain errors if the toolchain can't provide a requested sanitizer.
// isClang reports whether the C++ compiler is clang (MSan needs Clang); os is
// the target OS (MSan is Linux-only).
func CheckToolchain(sanitizers []string, isClang bool, os string) error {
	if has(sanitizers, "memory") {
		if !isClang {
			return fmt.Errorf(`the "memory" sanitizer (MSan) requires Clang; GCC has no MemorySanitizer`)
		}
		if os != "linux" {
			return fmt.Errorf(`the "memory" sanitizer (MSan) is only supported on Linux, not %s`, os)
		}
	}
	return nil
}

// RuntimeEnv returns the default *_OPTIONS env so a detection reliably fails the
// test. Defaults only -- the test runner should not override a value the user
// already set.
func RuntimeEnv(sanitizers []string) map[string]string {
	env := map[string]string{}
	for _, s := range sanitizers {
		switch s {
		case "address":
			env["ASAN_OPTIONS"] = "abort_on_error=1"
		case "thread":
			env["TSAN_OPTIONS"] = "halt_on_error=1"
		case "undefined":
			env["UBSAN_OPTIONS"] = "halt_on_error=1:print_stacktrace=1"
		case "leak":
			env["LSAN_OPTIONS"] = "exitcode=1"
		case "memory":
			env["MSAN_OPTIONS"] = "halt_on_error=1"
		}
	}
	_ = optionsVar // reserved for sanitizer_config.options (follow-up)
	return env
}

func has(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
