package vcpkg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolver(t *testing.T) {
	root := t.TempDir()
	installed := filepath.Join(root, "installed", "x64-linux")
	if err := os.MkdirAll(filepath.Join(installed, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "lib", "libfoo.a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Root: root, Triplet: "x64-linux"}
	if !r.Configured() {
		t.Fatal("should be configured")
	}
	if got, want := r.IncludeDir(), filepath.Join(installed, "include"); got != want {
		t.Errorf("IncludeDir=%q, want %q", got, want)
	}
	if got, want := r.LibArg("foo"), filepath.Join(installed, "lib", "libfoo.a"); got != want {
		t.Errorf("LibArg(foo)=%q, want the archive %q", got, want)
	}
	if got := r.LibArg("bar"); got != "-lbar" { // no archive -> -l fallback
		t.Errorf("LibArg(bar)=%q, want -lbar", got)
	}
}

func TestUnconfigured(t *testing.T) {
	r := &Resolver{}
	if r.Configured() {
		t.Fatal("empty resolver should be unconfigured")
	}
	if r.IncludeDir() != "" {
		t.Errorf("IncludeDir=%q, want empty", r.IncludeDir())
	}
	if got := r.LibArg("x"); got != "-lx" {
		t.Errorf("LibArg=%q, want -lx", got)
	}
	// nil resolver is safe.
	var nilR *Resolver
	if nilR.Configured() {
		t.Error("nil resolver should be unconfigured")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("VCPKG_ROOT", "/opt/vcpkg")
	t.Setenv("VCPKG_DEFAULT_TRIPLET", "custom-triplet")
	if r := FromEnv(); r.Root != "/opt/vcpkg" || r.Triplet != "custom-triplet" {
		t.Errorf("FromEnv=%+v", r)
	}
	t.Setenv("VCPKG_DEFAULT_TRIPLET", "")
	if FromEnv().Triplet == "" {
		t.Error("default triplet should be non-empty")
	}
}
