package graph

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/blade-build/blade-go/internal/loader"
)

func workspace(t *testing.T, files map[string]string) *loader.Loader {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return loader.New(root)
}

func names(nodes []*Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Label()
	}
	return out
}

func TestBuildAndTopoSort(t *testing.T) {
	// app -> lib -> base; lib and base PUBLIC. base also uses #pthread.
	l := workspace(t, map[string]string{
		"app/BUILD":  `cc_binary(name = 'app', srcs = ['m.cc'], deps = ['//lib:lib'])`,
		"lib/BUILD":  `cc_library(name = 'lib', srcs = ['l.cc'], deps = ['//base:base'], visibility = ['PUBLIC'])`,
		"base/BUILD": `cc_library(name = 'base', srcs = ['b.cc'], deps = ['#pthread'], visibility = ['PUBLIC'])`,
	})
	g, err := NewBuilder(l).Build([]string{"//app:app"})
	if err != nil {
		t.Fatal(err)
	}
	if g.Len() != 3 {
		t.Fatalf("graph has %d nodes, want 3", g.Len())
	}
	base := g.Node("//base:base")
	if base == nil || len(base.Syslibs) != 1 || base.Syslibs[0].Name != "pthread" {
		t.Errorf("base syslibs wrong: %+v", base)
	}
	sorted, err := g.TopoSort()
	if err != nil {
		t.Fatal(err)
	}
	// Dependencies must come before dependents.
	pos := map[string]int{}
	for i, n := range sorted {
		pos[n.Label()] = i
	}
	if !(pos["//base:base"] < pos["//lib:lib"] && pos["//lib:lib"] < pos["//app:app"]) {
		t.Errorf("topo order wrong: %v", names(sorted))
	}
}

func TestMissingTarget(t *testing.T) {
	l := workspace(t, map[string]string{
		"app/BUILD": `cc_binary(name = 'app', srcs = ['m.cc'], deps = ['//lib:nope'])`,
		"lib/BUILD": `cc_library(name = 'lib', visibility = ['PUBLIC'])`,
	})
	if _, err := NewBuilder(l).Build([]string{"//app:app"}); err == nil {
		t.Fatal("expected a no-such-target error")
	}
}

func TestVisibilityEnforced(t *testing.T) {
	// base is private (no visibility) and app is in a different package.
	l := workspace(t, map[string]string{
		"app/BUILD":  `cc_binary(name = 'app', deps = ['//base:base'])`,
		"base/BUILD": `cc_library(name = 'base', srcs = ['b.cc'])`,
	})
	_, err := NewBuilder(l).Build([]string{"//app:app"})
	if err == nil {
		t.Fatal("expected a visibility error")
	}
}

func TestVisibilityRecursiveAllows(t *testing.T) {
	l := workspace(t, map[string]string{
		"flare/app/BUILD":  `cc_binary(name = 'app', deps = ['//flare/base:base'])`,
		"flare/base/BUILD": `cc_library(name = 'base', srcs = ['b.cc'], visibility = ['//flare/...'])`,
	})
	if _, err := NewBuilder(l).Build([]string{"//flare/app:app"}); err != nil {
		t.Fatalf("recursive visibility should allow: %v", err)
	}
}

func TestCycleDetected(t *testing.T) {
	l := workspace(t, map[string]string{
		"a/BUILD": `cc_library(name = 'a', deps = ['//b:b'], visibility = ['PUBLIC'])`,
		"b/BUILD": `cc_library(name = 'b', deps = ['//a:a'], visibility = ['PUBLIC'])`,
	})
	g, err := NewBuilder(l).Build([]string{"//a:a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.TopoSort(); err == nil {
		t.Fatal("expected a cycle error")
	}
}

func TestSharedDepLoadedOnce(t *testing.T) {
	// Diamond: app -> {left,right} -> base. base's package loads once.
	l := workspace(t, map[string]string{
		"app/BUILD": `cc_binary(name = 'app', deps = ['//x:left', '//x:right'])`,
		"x/BUILD": `
cc_library(name = 'left', deps = ['//base:base'], visibility = ['PUBLIC'])
cc_library(name = 'right', deps = ['//base:base'], visibility = ['PUBLIC'])
`,
		"base/BUILD": `cc_library(name = 'base', srcs = ['b.cc'], visibility = ['PUBLIC'])`,
	})
	g, err := NewBuilder(l).Build([]string{"//app:app"})
	if err != nil {
		t.Fatal(err)
	}
	if g.Len() != 4 { // app, left, right, base
		t.Fatalf("graph nodes=%d, want 4 (base shared once): %v", g.Len(), names(g.All()))
	}
}
