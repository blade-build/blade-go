package bladectx

import (
	"os"
	"testing"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// eval runs a Starlark expression with `blade` bound to m and returns the result.
func eval(t *testing.T, m starlark.Value, expr string) starlark.Value {
	t.Helper()
	thread := &starlark.Thread{Name: "test"}
	v, err := starlark.EvalOptions(&syntax.FileOptions{}, thread, "t.star", expr, starlark.StringDict{"blade": m})
	if err != nil {
		t.Fatalf("eval(%q): %v", expr, err)
	}
	return v
}

func TestHostInfo(t *testing.T) {
	if HostOS() == "" || HostArch() == "" {
		t.Fatal("HostOS/HostArch must be non-empty")
	}
	m := ConfigModule()
	if got := eval(t, m, "blade.host_os"); string(got.(starlark.String)) != HostOS() {
		t.Errorf("blade.host_os=%v", got)
	}
}

func TestPathModule(t *testing.T) {
	m := ConfigModule()
	cases := map[string]string{
		`blade.path.join('a', 'b', 'c')`:    "a/b/c",
		`blade.path.dirname('a/b/c.cc')`:    "a/b",
		`blade.path.basename('a/b/c.cc')`:   "c.cc",
		`blade.path.normpath('a/./b/../c')`: "a/c",
	}
	for expr, want := range cases {
		if got := eval(t, m, expr); string(got.(starlark.String)) != want {
			t.Errorf("%s = %v, want %q", expr, got, want)
		}
	}
	// splitext returns a (root, ext) tuple.
	tup := eval(t, m, `blade.path.splitext('a/b.proto')`).(starlark.Tuple)
	if string(tup[0].(starlark.String)) != "a/b" || string(tup[1].(starlark.String)) != ".proto" {
		t.Errorf("splitext=%v", tup)
	}
}

func TestGetenv(t *testing.T) {
	os.Setenv("BLADE_GO_TEST_ENV", "yes")
	defer os.Unsetenv("BLADE_GO_TEST_ENV")
	m := ConfigModule()
	if got := eval(t, m, `blade.getenv('BLADE_GO_TEST_ENV')`); string(got.(starlark.String)) != "yes" {
		t.Errorf("getenv set=%v", got)
	}
	if got := eval(t, m, `blade.getenv('BLADE_GO_NOPE', 'def')`); string(got.(starlark.String)) != "def" {
		t.Errorf("getenv default=%v", got)
	}
	if got := eval(t, m, `blade.getenv('BLADE_GO_NOPE')`); got != starlark.None {
		t.Errorf("getenv missing=%v, want None", got)
	}
}

func TestBuildModule(t *testing.T) {
	m := BuildModule("flare/base", "build64_release")
	if got := eval(t, m, `blade.cc_toolchain.target_os`); string(got.(starlark.String)) != HostOS() {
		t.Errorf("cc_toolchain.target_os=%v", got)
	}
	if got := eval(t, m, `blade.current_source_dir()`); string(got.(starlark.String)) != "flare/base" {
		t.Errorf("current_source_dir=%v", got)
	}
	if got := eval(t, m, `blade.current_target_dir()`); string(got.(starlark.String)) != "build64_release/flare/base" {
		t.Errorf("current_target_dir=%v", got)
	}
}

func TestPathExistsRelAbs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/f.txt", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m := ConfigModule()
	bind := starlark.StringDict{"blade": m, "d": starlark.String(dir)}
	thread := &starlark.Thread{Name: "t"}
	run := func(expr string) starlark.Value {
		v, err := starlark.EvalOptions(&syntax.FileOptions{}, thread, "t.star", expr, bind)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	if run(`blade.path.exists(d + '/f.txt')`) != starlark.True {
		t.Error("exists(existing) should be True")
	}
	if run(`blade.path.exists(d + '/nope')`) != starlark.False {
		t.Error("exists(missing) should be False")
	}
	if got := run(`blade.path.relpath('a/b/c', 'a')`); string(got.(starlark.String)) != "b/c" {
		t.Errorf("relpath=%v", got)
	}
	if got := run(`blade.path.abspath('x')`); !starlarkStrHasPrefix(got, "/") {
		t.Errorf("abspath not absolute: %v", got)
	}
}

func TestPathJoinRejectsNonString(t *testing.T) {
	thread := &starlark.Thread{Name: "t"}
	_, err := starlark.EvalOptions(&syntax.FileOptions{}, thread, "t.star",
		`blade.path.join('a', 3)`, starlark.StringDict{"blade": ConfigModule()})
	if err == nil {
		t.Fatal("expected join to reject a non-string argument")
	}
}

func starlarkStrHasPrefix(v starlark.Value, p string) bool {
	s, ok := v.(starlark.String)
	return ok && len(s) >= len(p) && string(s)[:len(p)] == p
}

func TestConfigModuleHasNoToolchain(t *testing.T) {
	// Mirrors Blade: cc_toolchain is not available in the config phase.
	thread := &starlark.Thread{Name: "test"}
	_, err := starlark.EvalOptions(&syntax.FileOptions{}, thread, "t.star",
		"blade.cc_toolchain", starlark.StringDict{"blade": ConfigModule()})
	if err == nil {
		t.Fatal("expected cc_toolchain to be absent in the config phase")
	}
}
