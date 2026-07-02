package loader

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// workspace writes files (relative path -> content) under a temp dir and returns
// its root.
func workspace(t *testing.T, files map[string]string) string {
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
	return root
}

func TestLoadBuildFile_CcLibrary(t *testing.T) {
	root := workspace(t, map[string]string{
		"flare/base/BUILD": `
cc_library(
    name = 'base',
    srcs = ['a.cc', 'b.cc'],
    hdrs = ['base.h'],
    deps = ['//flare/logging:logging', '#pthread'],
    visibility = ['PUBLIC'],
)
`,
	})
	l := New(root)
	if err := l.LoadBuildFile(filepath.Join(root, "flare/base/BUILD")); err != nil {
		t.Fatal(err)
	}
	if l.Targets.Len() != 1 {
		t.Fatalf("got %d targets, want 1", l.Targets.Len())
	}
	tgt := l.Targets.Get("//flare/base:base")
	if tgt == nil {
		t.Fatalf("target //flare/base:base not found; labels=%v", l.Targets.Labels())
	}
	if tgt.Type != "cc_library" {
		t.Errorf("Type=%q, want cc_library", tgt.Type)
	}
	if got := tgt.AttrStrings("srcs"); !reflect.DeepEqual(got, []string{"a.cc", "b.cc"}) {
		t.Errorf("srcs=%v", got)
	}
	if got := tgt.AttrStrings("deps"); !reflect.DeepEqual(got, []string{"//flare/logging:logging", "#pthread"}) {
		t.Errorf("deps=%v", got)
	}
}

func TestStrOrList(t *testing.T) {
	root := workspace(t, map[string]string{
		"p/BUILD": `cc_binary(name = 'app', srcs = 'main.cc')`,
	})
	l := New(root)
	if err := l.LoadBuildFile(filepath.Join(root, "p/BUILD")); err != nil {
		t.Fatal(err)
	}
	tgt := l.Targets.Get("//p:app")
	if got := tgt.AttrStrings("srcs"); !reflect.DeepEqual(got, []string{"main.cc"}) {
		t.Errorf("srcs=%v, want [main.cc] (bare string accepted)", got)
	}
}

func TestGlob(t *testing.T) {
	root := workspace(t, map[string]string{
		"p/a.cc":        "",
		"p/b.cc":        "",
		"p/a_test.cc":   "",
		"p/sub/deep.cc": "",
		"p/.hidden.cc":  "",
		"p/BUILD": `cc_library(
    name = 'p',
    srcs = glob(['*.cc', '**/*.cc'], exclude = ['*_test.cc']),
)`,
	})
	l := New(root)
	if err := l.LoadBuildFile(filepath.Join(root, "p/BUILD")); err != nil {
		t.Fatal(err)
	}
	got := l.Targets.Get("//p:p").AttrStrings("srcs")
	want := []string{"a.cc", "b.cc", "sub/deep.cc"} // a_test.cc excluded, .hidden.cc skipped
	if !reflect.DeepEqual(got, want) {
		t.Errorf("glob srcs=%v, want %v", got, want)
	}
}

func TestGlobEmptyIsError(t *testing.T) {
	root := workspace(t, map[string]string{
		"p/BUILD": `cc_library(name = 'p', srcs = glob(['*.nope']))`,
	})
	l := New(root)
	if err := l.LoadBuildFile(filepath.Join(root, "p/BUILD")); err == nil {
		t.Fatal("expected an error for an empty glob without allow_empty")
	}
}

func TestEnableIf(t *testing.T) {
	root := workspace(t, map[string]string{
		"p/BUILD": `cc_library(
    name = 'p',
    srcs = ['base.cc'] + enable_if(blade.host_os == 'linux', ['linux.cc'], ['other.cc']),
)`,
	})
	l := New(root)
	if err := l.LoadBuildFile(filepath.Join(root, "p/BUILD")); err != nil {
		t.Fatal(err)
	}
	srcs := l.Targets.Get("//p:p").AttrStrings("srcs")
	if len(srcs) != 2 || srcs[0] != "base.cc" {
		t.Errorf("enable_if srcs=%v", srcs)
	}
}

func TestFail(t *testing.T) {
	root := workspace(t, map[string]string{
		"p/BUILD": `fail('boom', 42)`,
	})
	l := New(root)
	err := l.LoadBuildFile(filepath.Join(root, "p/BUILD"))
	if err == nil {
		t.Fatal("expected fail() to error")
	}
}

func TestDuplicateTarget(t *testing.T) {
	root := workspace(t, map[string]string{
		"p/BUILD": `
cc_library(name = 'x', srcs = ['a.cc'])
cc_library(name = 'x', srcs = ['b.cc'])
`,
	})
	l := New(root)
	if err := l.LoadBuildFile(filepath.Join(root, "p/BUILD")); err == nil {
		t.Fatal("expected a duplicate-target error")
	}
}

func TestMissingName(t *testing.T) {
	root := workspace(t, map[string]string{
		"p/BUILD": `cc_library(srcs = ['a.cc'])`,
	})
	l := New(root)
	if err := l.LoadBuildFile(filepath.Join(root, "p/BUILD")); err == nil {
		t.Fatal("expected a missing-name error")
	}
}

func TestLoadConfigFile_LambdaAndBlade(t *testing.T) {
	// Exercises the two things flare's BLADE_ROOT needs: the `blade` context
	// (host_os / conditionals) and lambda deferred config (stored, not called).
	root := workspace(t, map[string]string{
		"BLADE_ROOT": `
_WIN = blade.host_os == 'windows'
cc_config(
    cxxflags = ['-std=c++17'],
    optimize = lambda blade: ['-O2'] if not blade.build_type_is_debug() else [],
)
proto_library_config(protoc = 'protoc')
`,
	})
	l := New(root)
	if err := l.LoadConfigFile(filepath.Join(root, "BLADE_ROOT")); err != nil {
		t.Fatal(err)
	}
	cc := l.Config.Named("cc_config")
	if len(cc) != 1 {
		t.Fatalf("cc_config sections=%d, want 1", len(cc))
	}
	if _, ok := cc[0].Attrs["cxxflags"]; !ok {
		t.Errorf("cxxflags not captured: %v", cc[0].Attrs)
	}
	// The lambda is preserved as a callable, not converted.
	if cc[0].Attrs["optimize"] == nil {
		t.Errorf("lambda config value 'optimize' not preserved")
	}
	if len(l.Config.Named("proto_library_config")) != 1 {
		t.Errorf("proto_library_config not captured")
	}
}

func TestLoadValue(t *testing.T) {
	root := workspace(t, map[string]string{
		"build/public.conf": `['//a:x', '//b:y']`,
		"BLADE_ROOT": `
global_config(legacy_public_targets = load_value('build/public.conf'))
`,
	})
	l := New(root)
	if err := l.LoadConfigFile(filepath.Join(root, "BLADE_ROOT")); err != nil {
		t.Fatal(err)
	}
	gc := l.Config.Named("global_config")
	if len(gc) != 1 {
		t.Fatalf("global_config sections=%d", len(gc))
	}
	got, ok := gc[0].Attrs["legacy_public_targets"].([]any)
	if !ok || len(got) != 2 || got[0] != "//a:x" {
		t.Errorf("load_value result=%v (%T)", gc[0].Attrs["legacy_public_targets"], gc[0].Attrs["legacy_public_targets"])
	}
}

func TestBuildPhaseToolchainConditional(t *testing.T) {
	// A top-level platform conditional using blade.cc_toolchain must evaluate at
	// load time (the host-derived proxy).
	root := workspace(t, map[string]string{
		"p/BUILD": `
_extra = ['posix.cc'] if blade.cc_toolchain.target_os != 'windows' else ['win.cc']
cc_library(name = 'p', srcs = ['base.cc'] + _extra)
`,
	})
	l := New(root)
	if err := l.LoadBuildFile(filepath.Join(root, "p/BUILD")); err != nil {
		t.Fatal(err)
	}
	if l.Targets.Get("//p:p") == nil {
		t.Fatal("target not loaded")
	}
}

func TestConfigRejectsPositionalArgs(t *testing.T) {
	root := workspace(t, map[string]string{
		"BLADE_ROOT": `cc_config('oops')`,
	})
	l := New(root)
	if err := l.LoadConfigFile(filepath.Join(root, "BLADE_ROOT")); err == nil {
		t.Fatal("expected config to reject positional args")
	}
}
