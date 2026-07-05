package graph

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestLegacyPublicTargets(t *testing.T) {
	// A private target (no visibility) listed in global_config's
	// legacy_public_targets is visible everywhere (flare's grandfather list, how
	// //thirdparty/protobuf:protobuf is reachable).
	l := workspace(t, map[string]string{
		"BLADE_ROOT":                `global_config(legacy_public_targets = ['thirdparty/protobuf:protobuf'])`,
		"app/BUILD":                 `cc_binary(name = 'app', deps = ['//thirdparty/protobuf:protobuf'])`,
		"thirdparty/protobuf/BUILD": `cc_library(name = 'protobuf', srcs = ['p.cc'])`,
	})
	if err := l.LoadConfigFile(filepath.Join(l.Root, "BLADE_ROOT")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBuilder(l).Build([]string{"//app:app"}); err != nil {
		t.Fatalf("legacy_public_targets should make the dep visible: %v", err)
	}

	// Without the legacy entry, the same private cross-package dep is an error.
	l2 := workspace(t, map[string]string{
		"BLADE_ROOT":                `global_config()`,
		"app/BUILD":                 `cc_binary(name = 'app', deps = ['//thirdparty/protobuf:protobuf'])`,
		"thirdparty/protobuf/BUILD": `cc_library(name = 'protobuf', srcs = ['p.cc'])`,
	})
	if err := l2.LoadConfigFile(filepath.Join(l2.Root, "BLADE_ROOT")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBuilder(l2).Build([]string{"//app:app"}); err == nil {
		t.Fatal("expected a visibility error without legacy_public_targets")
	}
}

func TestExpandPatterns(t *testing.T) {
	l := workspace(t, map[string]string{
		"flare/base/BUILD": `
cc_library(name = 'a', visibility = ['PUBLIC'])
cc_library(name = 'b', visibility = ['PUBLIC'])
`,
		"flare/base/sub/BUILD": `cc_library(name = 'c', visibility = ['PUBLIC'])`,
		"other/BUILD":          `cc_library(name = 'x')`,
	})
	b := NewBuilder(l)

	// Package wildcard: only flare/base's own targets.
	got, err := b.Expand([]string{"//flare/base:*"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"//flare/base:a", "//flare/base:b"}; !reflect.DeepEqual(got, want) {
		t.Errorf(":* expand=%v, want %v", got, want)
	}

	// Recursive: flare/base and its sub-packages.
	got, err = b.Expand([]string{"//flare/base/..."})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"//flare/base:a", "//flare/base:b", "//flare/base/sub:c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("/... expand=%v, want %v", got, want)
	}

	// Colon-ellipsis is the same recursive form.
	got, _ = b.Expand([]string{"//flare/base:..."})
	if len(got) != 3 {
		t.Errorf(":... expand=%v, want 3 targets", got)
	}

	// A concrete label passes through unchanged.
	got, _ = b.Expand([]string{"//other:x"})
	if !reflect.DeepEqual(got, []string{"//other:x"}) {
		t.Errorf("concrete label expand=%v", got)
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

func TestThirdpartyRoutesToVcpkg(t *testing.T) {
	// A //thirdparty/... dep resolves to a vcpkg dep without loading its BUILD.
	l := workspace(t, map[string]string{
		"app/BUILD": `cc_binary(name = 'app', srcs = ['m.cc'], deps = ['//thirdparty/googletest:gtest', '//thirdparty/gflags:gflags'])`,
		// note: no thirdparty/*/BUILD files exist -- they must not be loaded.
	})
	g, err := NewBuilder(l).Build([]string{"//app:app"})
	if err != nil {
		t.Fatal(err)
	}
	app := g.Node("//app:app")
	if len(app.Vcpkgs) != 2 {
		t.Fatalf("app.Vcpkgs=%+v, want 2", app.Vcpkgs)
	}
	got := map[string]string{}
	for _, v := range app.Vcpkgs {
		got[v.Port] = v.Lib
	}
	if got["googletest"] != "gtest" || got["gflags"] != "gflags" {
		t.Errorf("vcpkg mapping wrong: %+v", app.Vcpkgs)
	}
	if g.Len() != 1 { // only //app:app; thirdparty packages were not loaded
		t.Errorf("graph loaded thirdparty BUILDs: %v", names(g.All()))
	}
}

func TestVcpkgPrefixDisabled(t *testing.T) {
	l := workspace(t, map[string]string{
		"app/BUILD":               `cc_binary(name = 'app', deps = ['//thirdparty/gflags:gflags'])`,
		"thirdparty/gflags/BUILD": `cc_library(name = 'gflags', srcs = ['g.cc'], visibility = ['PUBLIC'])`,
	})
	b := NewBuilder(l)
	b.VcpkgPrefix = "" // disable mapping -> load the real BUILD
	g, err := b.Build([]string{"//app:app"})
	if err != nil {
		t.Fatal(err)
	}
	if g.Node("//thirdparty/gflags:gflags") == nil {
		t.Error("with mapping disabled, the thirdparty target should load normally")
	}
}

func TestFrameworkLibsInjectedAsCcTestDeps(t *testing.T) {
	// A cc_test's framework libs (SetFrameworkLibs) are injected as deps and
	// classified: a `//`-target is a real dep, a `#name` a syslib, a thirdparty
	// entry a vcpkg dep. A cc_library gets none.
	l := workspace(t, map[string]string{
		"tf/BUILD":  `cc_library(name = 'main', srcs = ['main.cc'], visibility = ['PUBLIC'])`,
		"t/BUILD":   `cc_test(name = 't', srcs = ['t.cc'])`,
		"lib/BUILD": `cc_library(name = 'lib', srcs = ['l.cc'])`,
	})
	b := NewBuilder(l)
	b.SetFrameworkLibs([]string{"//tf:main", "#pthread"}, nil)
	g, err := b.Build([]string{"//t:t", "//lib:lib"})
	if err != nil {
		t.Fatal(err)
	}
	tst := g.Node("//t:t")
	if got := names(tst.Deps); len(got) != 1 || got[0] != "//tf:main" {
		t.Errorf("cc_test deps=%v, want [//tf:main]", got)
	}
	if len(tst.Syslibs) != 1 || tst.Syslibs[0].Name != "pthread" {
		t.Errorf("cc_test syslibs=%v, want [pthread]", tst.Syslibs)
	}
	if lib := g.Node("//lib:lib"); len(lib.Deps) != 0 {
		t.Errorf("cc_library should get no framework deps, got %v", names(lib.Deps))
	}
}
