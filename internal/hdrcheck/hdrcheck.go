// Package hdrcheck implements Blade's C/C++ header inclusion-dependency check:
// every project header a source directly #includes must belong to a cc target
// that the including target declares in `deps` (or to the target itself).
//
// This mirrors the *functionality* of Python Blade's inclusion_check.py, but not
// its mechanism. Python compiles every TU through a `-H` wrapper that records the
// inclusion stack to a `.incstk` file, then runs a per-target checker action.
// blade-go instead runs a single post-build pass that reuses the `.d` depfiles
// ninja already generates (Deps: "gcc") as the "headers the compiler actually
// used" closure, and a regex scan of each source's literal `#include` lines as
// the "directly included" set. A directly-included header is real iff its
// spelling resolves to a path in that closure -- which drops dead `#if 0`
// branches, commented includes, and (because they are absolute in the depfile)
// all system / vcpkg / thirdparty headers, leaving exactly the first-party
// headers worth checking.
//
// The closure source is abstracted behind ClosureSource so a native-MSVC backend
// (cl.exe has no `.d`; its header closure lives in ninja's binary deps log) can
// later plug in a `ninja -t deps` reader without touching the check algorithm.
package hdrcheck

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/blade-build/blade-go/internal/graph"
)

// Severity controls how issues are surfaced and whether they fail the build.
type Severity int

const (
	Off   Severity = iota // check disabled
	Warn                  // report, do not fail
	Error                 // report and fail the build
)

// ParseSeverity maps a CLI/config string to a Severity (default Warn on "").
func ParseSeverity(s string) (Severity, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "warn", "warning":
		return Warn, true
	case "off", "no", "none", "false":
		return Off, true
	case "error", "err":
		return Error, true
	}
	return Warn, false
}

// Kind classifies an inclusion problem.
type Kind int

const (
	// Undeclared: the header belongs to no known cc target.
	Undeclared Kind = iota
	// MissingDep: the header belongs to a cc target not in the includer's deps.
	MissingDep
	// PrivateHeader: the header is another target's private (srcs) header.
	PrivateHeader
)

// Issue is one inclusion-dependency violation.
type Issue struct {
	Target    string   // the including target's label ("//pkg:name")
	TargetPos string   // the target's BUILD location "pkg/BUILD:line:col" (for the fix-it note)
	Source    string   // the src/hdr file (workspace-relative) with the bad #include
	Line      int      // 1-based line of the offending #include in Source
	Col       int      // 1-based column of the '#' in Source
	Header    string   // the offending header (workspace-relative)
	Kind      Kind     // why it is bad
	Owners    []string // labels that own Header (for MissingDep / PrivateHeader)
}

// ccTypes are the target rules whose srcs/hdrs participate in the check.
var ccTypes = map[string]bool{
	"cc_library": true, "cc_binary": true, "cc_test": true, "cc_plugin": true,
}

// ClosureSource yields the set of paths the compiler actually traversed for a
// given object file -- the "did the compiler really use this header" gate.
//
// The default source is NinjaDepsClosure, because ninja's `deps = gcc` rule
// *consumes and deletes* each `.d` file after folding it into ninja's own binary
// dep log -- so raw depfiles do not survive a build. Reading them back via
// `ninja -t deps` also means the same source works for native MSVC (`deps = msvc`),
// whose header closure likewise lives only in that log.
type ClosureSource interface {
	Closure(obj string) map[string]bool
}

// Options configures a check run.
type Options struct {
	Root       string          // workspace root (abs)
	BuildDir   string          // e.g. "build_release"
	ObjSuffix  string          // e.g. ".o"
	Severity   Severity        // Off skips entirely
	Closure    ClosureSource   // nil => NinjaDepsClosure over BuildDir/build.ninja
	AllowUndec map[string]bool // headers exempt from the Undeclared verdict
	// Only, if non-nil, restricts which targets are *checked* to this label set
	// (ownership maps are still built from every node, so deps resolve). Mirrors
	// Python Blade checking only the requested targets, not their dependencies.
	Only map[string]bool
}

var includeRE = regexp.MustCompile(`(?m)^[ \t]*#[ \t]*include[ \t]*(?:"([^"]+)"|<([^>]+)>)`)

// Check runs the inclusion check over the given nodes and returns the issues
// found, sorted deterministically. nodes should be the built graph (the same set
// whose `.d` files exist); ownership maps are built from all of them, so a header
// owned by any loaded target resolves even if that target was pulled in only as a
// transitive dep.
func Check(nodes []*graph.Node, opt Options) []Issue {
	if opt.Severity == Off {
		return nil
	}
	if opt.Closure == nil {
		opt.Closure = &NinjaDepsClosure{Root: opt.Root, BuildFile: path.Join(opt.BuildDir, "build.ninja")}
	}
	genPrefix := opt.BuildDir + "/"

	// Global ownership: header (workspace-relative) -> set of owning labels.
	// hdrs are the target's public headers; header files listed in srcs are its
	// private headers (Blade: including another target's private header is an
	// error even for a declared dep). ownHdrs is a target's own public+private
	// set -- including any of those, or any of a direct dep's, is always fine.
	public := map[string]map[string]bool{}
	private := map[string]map[string]bool{}
	ownHdrs := map[string]map[string]bool{}
	add := func(m map[string]map[string]bool, hdr, lib string) {
		if m[hdr] == nil {
			m[hdr] = map[string]bool{}
		}
		m[hdr][lib] = true
	}
	for _, n := range nodes {
		if !ccTypes[n.Target.Type] {
			continue
		}
		lib := n.Target.Label()
		own := map[string]bool{}
		for _, h := range n.Target.AttrStrings("hdrs") {
			hp := path.Join(n.Target.Package, h)
			add(public, hp, lib)
			own[hp] = true
		}
		for _, s := range n.Target.AttrStrings("srcs") {
			if isHeader(s) {
				hp := path.Join(n.Target.Package, s)
				add(private, hp, lib)
				own[hp] = true
			}
		}
		ownHdrs[lib] = own
	}

	var issues []Issue
	for _, n := range nodes {
		if !ccTypes[n.Target.Type] {
			continue
		}
		lib := n.Target.Label()
		if opt.Only != nil && !opt.Only[lib] {
			continue
		}
		pkg := n.Target.Package

		// deps closure for membership: direct deps + self.
		deps := map[string]bool{lib: true}
		declared := map[string]bool{} // own + direct deps' own headers
		for h := range ownHdrs[lib] {
			declared[h] = true
		}
		for _, d := range n.Deps {
			deps[d.Target.Label()] = true
			for h := range ownHdrs[d.Target.Label()] {
				declared[h] = true
			}
		}

		for _, f := range n.Target.AttrStrings("srcs") {
			issues = checkFile(issues, n, f, pkg, lib, deps, declared, public, private, genPrefix, opt)
		}
		for _, f := range n.Target.AttrStrings("hdrs") {
			issues = checkFile(issues, n, f, pkg, lib, deps, declared, public, private, genPrefix, opt)
		}
	}

	sort.Slice(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Header < b.Header
	})
	return issues
}

func checkFile(issues []Issue, n *graph.Node, f, pkg, lib string,
	deps, declared map[string]bool,
	public, private map[string]map[string]bool,
	genPrefix string, opt Options) []Issue {

	srcPath := path.Join(pkg, f)
	// A src naming a generated file lives under the build dir; nothing to scan.
	if strings.HasPrefix(srcPath, genPrefix) {
		return issues
	}
	obj := path.Join(opt.BuildDir, pkg, n.Target.Name+".objs", f) + opt.ObjSuffix
	closure := opt.Closure.Closure(obj)
	if len(closure) == 0 {
		return issues // not built (no depfile) -> can't check this file
	}
	targetPos := relTo(opt.Root, n.Target.Pos)
	mk := func(hdr string, dh directHdr, kind Kind, owners []string) Issue {
		return Issue{
			Target: lib, TargetPos: targetPos,
			Source: srcPath, Line: dh.line, Col: dh.col,
			Header: hdr, Kind: kind, Owners: owners,
		}
	}
	for _, dh := range directHeaders(path.Join(opt.Root, srcPath), pkg, closure) {
		hdr := dh.header
		if declared[hdr] { // own or a direct dep's header
			continue
		}
		if strings.HasPrefix(hdr, genPrefix) { // generated header: generator ensures order
			continue
		}
		if owners := public[hdr]; len(owners) > 0 {
			if !anyIn(owners, deps) {
				issues = append(issues, mk(hdr, dh, MissingDep, sortedKeys(owners)))
			}
			continue
		}
		if powners := private[hdr]; len(powners) > 0 && !powners[lib] {
			issues = append(issues, mk(hdr, dh, PrivateHeader, sortedKeys(powners)))
			continue
		}
		if !opt.AllowUndec[hdr] {
			issues = append(issues, mk(hdr, dh, Undeclared, nil))
		}
	}
	return issues
}

// relTo strips a leading "root/" from an absolute location so it prints as a
// workspace-relative, clickable path (Blade's BUILD Pos is usually already
// relative; this is a no-op then).
func relTo(root, loc string) string {
	if root != "" && strings.HasPrefix(loc, root+"/") {
		return loc[len(root)+1:]
	}
	return loc
}

// directHdr is a directly-included header and the source line/column of its
// #include directive (column = the position of the '#').
type directHdr struct {
	header string
	line   int
	col    int
}

// directHeaders returns the workspace-relative headers that file (abs path)
// directly #includes AND the compiler actually used (present in closure), each
// with the 1-based line of its #include directive. Each literal include spelling
// is resolved the way the compiler would: against the workspace root (-I.) and
// against the including file's own directory (implicit for `"..."` includes).
// System/vcpkg headers are absolute in the closure, so a relative spelling never
// matches them and they drop out here.
func directHeaders(absFile, pkg string, closure map[string]bool) []directHdr {
	f, err := os.Open(absFile)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []directHdr
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		m := includeRE.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		spell := m[1] // quoted form
		if spell == "" {
			spell = m[2] // angle form (project headers are sometimes spelled <pkg/x.h>)
		}
		spell = path.Clean(spell)
		col := strings.IndexByte(text, '#') + 1 // 1-based column of the '#'
		for _, cand := range [2]string{spell, path.Join(pkg, spell)} {
			if closure[cand] && !seen[cand] {
				seen[cand] = true
				out = append(out, directHdr{cand, line, col})
			}
		}
	}
	return out
}

func anyIn(set, universe map[string]bool) bool {
	for k := range set {
		if universe[k] {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Format renders the issue as GCC-style diagnostics (no trailing newline): a
//
//	source:line:col: <severity>: <message> [hdr-check]
//
// line pointing at the offending #include, followed by a
//
//	BUILD:line:col: note: <fix-it>
//
// line pointing at the target's stanza to edit. Both locations are clickable in
// editors/terminals that recognize the GCC format. `severity` is "warning" or
// "error".
func (i Issue) Format(severity string) string {
	loc := fmt.Sprintf("%s:%d:%d", i.Source, i.Line, i.Col)
	owners := strings.Join(i.Owners, " or ")
	var msg, note string
	switch i.Kind {
	case MissingDep:
		msg = fmt.Sprintf("%s: %s: '%s' is included here but %s is not in the deps of %s [hdr-check]",
			loc, severity, i.Header, owners, i.Target)
		note = "note: add " + owners + " to deps"
	case PrivateHeader:
		msg = fmt.Sprintf("%s: %s: '%s' is a private header of %s [hdr-check]",
			loc, severity, i.Header, owners)
		note = "note: it is not part of " + owners + "'s public interface"
	default: // Undeclared
		msg = fmt.Sprintf("%s: %s: '%s' is not declared in any cc target [hdr-check]",
			loc, severity, i.Header)
		note = "note: declare it in the hdrs of its owning library, or in this target"
	}
	if i.TargetPos != "" {
		return msg + "\n" + i.TargetPos + ": " + note
	}
	return msg
}

// SeverityFromBlade maps Blade's cc_config.hdr_dep_missing_severity values to a
// Severity: "error" fails the build; "debug" is silenced (Off); everything else
// (info/notice/warning) is a non-fatal Warn. Unknown/empty falls back to Warn.
func SeverityFromBlade(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return Error
	case "debug":
		return Off
	default: // info, notice, warning, "" ...
		return Warn
	}
}

func isHeader(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".h", ".hpp", ".hh", ".hxx", ".inc", ".ipp", ".tcc":
		return true
	}
	return false
}

// NinjaDepsClosure reads header closures from ninja's dep log via `ninja -t deps`
// (run once, lazily, and cached). This is the default source: it works uniformly
// for `deps = gcc` and `deps = msvc`, both of which store the closure in ninja's
// binary log rather than leaving a `.d` file behind.
type NinjaDepsClosure struct {
	Root      string // workspace root (ninja cwd)
	BuildFile string // ninja file, relative to Root (e.g. "build_release/build.ninja")

	once sync.Once
	m    map[string]map[string]bool
}

// Closure returns the recorded dependency set for obj (nil if unknown).
func (n *NinjaDepsClosure) Closure(obj string) map[string]bool {
	n.once.Do(n.load)
	return n.m[obj]
}

func (n *NinjaDepsClosure) load() {
	n.m = map[string]map[string]bool{}
	cmd := exec.Command("ninja", "-f", n.BuildFile, "-t", "deps")
	cmd.Dir = n.Root
	out, err := cmd.Output()
	if err != nil {
		return
	}
	parseInto(n.m, string(out))
}

// parseInto fills m from `ninja -t deps` output. Records look like:
//
//	build_dir/pkg/name.objs/foo.cc.o: #deps 18, deps mtime ... (VALID)
//	    flare/base/foo.h
//	    /abs/system/header.h
//	<blank line>
func parseInto(m map[string]map[string]bool, out string) {
	var cur map[string]bool
	sc := bufio.NewScanner(bytes.NewReader([]byte(out)))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			cur = nil
			continue
		}
		if line[0] == ' ' || line[0] == '\t' { // a dependency line
			if cur != nil {
				if dep := strings.TrimSpace(line); dep != "" {
					cur[strings.TrimPrefix(dep, "./")] = true
				}
			}
			continue
		}
		if i := strings.Index(line, ": #deps"); i >= 0 { // a record header
			cur = map[string]bool{}
			m[line[:i]] = cur
		} else {
			cur = nil
		}
	}
}
