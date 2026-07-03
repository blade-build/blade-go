package hdrcheck

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/blade-build/blade-go/internal/graph"
	"github.com/blade-build/blade-go/internal/target"
)

// fakeClosure is a preset obj -> header-set map, standing in for `ninja -t deps`.
type fakeClosure map[string]map[string]bool

func (f fakeClosure) Closure(obj string) map[string]bool { return f[obj] }

func ccLib(pkg, name string, attrs map[string]any) *graph.Node {
	return &graph.Node{Target: &target.Target{Type: "cc_library", Name: name, Package: pkg, Attrs: attrs}}
}

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// A source that includes: its own header (ok), a dep's public header (ok), a
// non-dep's public header (missing dep), another target's private header
// (private), an unowned header (undeclared), and a system header (ignored).
func TestCheckClassifiesInclusions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a", "a.cc"), `
#include "a/a.h"
#include "b/b.h"
#include "d/d.h"
#include "c/c_priv.h"
#include "u/u.h"
#include <vector>
`)

	libB := ccLib("b", "b", map[string]any{"hdrs": []any{"b.h"}})
	libD := ccLib("d", "d", map[string]any{"hdrs": []any{"d.h"}})
	libC := ccLib("c", "c", map[string]any{"srcs": []any{"c.cc", "c_priv.h"}})
	libA := ccLib("a", "a", map[string]any{"srcs": []any{"a.cc"}, "hdrs": []any{"a.h"}})
	libA.Deps = []*graph.Node{libB} // A declares only B

	closure := fakeClosure{
		"build64_release/a/a.objs/a.cc.o": set(
			"a/a.cc", "a/a.h", "b/b.h", "d/d.h", "c/c_priv.h", "u/u.h",
			"/usr/include/c++/v1/vector", // system: absolute, spelling "vector" won't match
		),
	}

	issues := Check([]*graph.Node{libA, libB, libC, libD}, Options{
		Root: root, BuildDir: "build64_release", ObjSuffix: ".o",
		Severity: Warn, Closure: closure,
		Only: map[string]bool{"//a:a": true},
	})

	got := map[string]Kind{}
	line := map[string]int{}
	for _, is := range issues {
		got[is.Header] = is.Kind
		line[is.Header] = is.Line
	}
	want := map[string]Kind{
		"d/d.h":      MissingDep,
		"c/c_priv.h": PrivateHeader,
		"u/u.h":      Undeclared,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d issues %v, want %d %v", len(got), got, len(want), want)
	}
	for h, k := range want {
		if got[h] != k {
			t.Errorf("header %s: got kind %v, want %v", h, got[h], k)
		}
	}
	// Line numbers of the offending #include (source starts with a blank line).
	wantLine := map[string]int{"d/d.h": 4, "c/c_priv.h": 5, "u/u.h": 6}
	for h, ln := range wantLine {
		if line[h] != ln {
			t.Errorf("header %s: got line %d, want %d", h, line[h], ln)
		}
	}
	// The own header, the declared dep's header, and <vector> must NOT be flagged.
	for _, h := range []string{"a/a.h", "b/b.h", "vector"} {
		if _, bad := got[h]; bad {
			t.Errorf("header %s should not be flagged", h)
		}
	}
}

// A header only reachable via the compiler closure — but whose spelling is a
// same-directory relative include — resolves against the file's own package dir.
func TestCheckResolvesSameDirInclude(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a", "a.cc"), "#include \"helper.h\"\n")
	libHelper := ccLib("a", "helper", map[string]any{"hdrs": []any{"helper.h"}})
	libA := ccLib("a", "a", map[string]any{"srcs": []any{"a.cc"}}) // no deps -> missing

	closure := fakeClosure{"build64_release/a/a.objs/a.cc.o": set("a/a.cc", "a/helper.h")}
	issues := Check([]*graph.Node{libA, libHelper}, Options{
		Root: root, BuildDir: "build64_release", ObjSuffix: ".o", Severity: Warn,
		Closure: closure, Only: map[string]bool{"//a:a": true},
	})
	if len(issues) != 1 || issues[0].Header != "a/helper.h" || issues[0].Kind != MissingDep {
		t.Fatalf("want 1 missing-dep on a/helper.h, got %v", issues)
	}
}

// AllowUndec exempts a header from the Undeclared verdict.
func TestAllowUndeclared(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a", "a.cc"), "#include \"vendor/x.h\"\n")
	libA := ccLib("a", "a", map[string]any{"srcs": []any{"a.cc"}})
	closure := fakeClosure{"build64_release/a/a.objs/a.cc.o": set("a/a.cc", "vendor/x.h")}
	opt := Options{Root: root, BuildDir: "build64_release", ObjSuffix: ".o", Severity: Warn,
		Closure: closure, Only: map[string]bool{"//a:a": true}}

	if got := Check([]*graph.Node{libA}, opt); len(got) != 1 {
		t.Fatalf("want 1 undeclared, got %v", got)
	}
	opt.AllowUndec = map[string]bool{"vendor/x.h": true}
	if got := Check([]*graph.Node{libA}, opt); len(got) != 0 {
		t.Fatalf("allowed header should be exempt, got %v", got)
	}
}

func TestFormatGCC(t *testing.T) {
	i := Issue{
		Target: "//a:a", TargetPos: "a/BUILD:10:1",
		Source: "a/a.cc", Line: 4, Col: 1,
		Header: "d/d.h", Kind: MissingDep, Owners: []string{"//d:d"},
	}
	got := i.Format("error")
	want := "a/a.cc:4:1: error: 'd/d.h' is included here but //d:d is not in the deps of //a:a [hdr-check]\n" +
		"a/BUILD:10:1: note: add //d:d to deps"
	if got != want {
		t.Fatalf("Format:\n got: %q\nwant: %q", got, want)
	}
}

func TestParseNinjaDeps(t *testing.T) {
	out := "" +
		"build64_release/a/a.objs/a.cc.o: #deps 3, deps mtime 123 (VALID)\n" +
		"    a/a.cc\n" +
		"    a/a.h\n" +
		"    /usr/include/vector\n" +
		"\n" +
		"build64_release/b/b.objs/b.cc.o: #deps 1, deps mtime 456 (VALID)\n" +
		"    b/b.cc\n"
	n := &NinjaDepsClosure{}
	n.m = map[string]map[string]bool{}
	// Exercise the parser directly by feeding a preset (load() shells out to ninja).
	parseInto(n.m, out)
	c := n.m["build64_release/a/a.objs/a.cc.o"]
	if !c["a/a.h"] || !c["/usr/include/vector"] || len(c) != 3 {
		t.Fatalf("bad closure a: %v", c)
	}
	if !n.m["build64_release/b/b.objs/b.cc.o"]["b/b.cc"] {
		t.Fatalf("bad closure b")
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
