package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "BLADE_ROOT"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := FindRoot(sub)
	if err != nil {
		t.Fatal(err)
	}
	// t.TempDir may live under a symlink (/var -> /private/var on macOS); compare
	// resolved paths.
	gotResolved, _ := filepath.EvalSymlinks(got)
	rootResolved, _ := filepath.EvalSymlinks(root)
	if gotResolved != rootResolved {
		t.Errorf("FindRoot=%q, want %q", gotResolved, rootResolved)
	}
}

func TestFindRootMissing(t *testing.T) {
	if _, err := FindRoot(t.TempDir()); err == nil {
		t.Fatal("expected an error when no BLADE_ROOT exists")
	}
}

func TestBuildGeneratesNinja(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"BLADE_ROOT": `cc_config()`,
		"base/BUILD": `cc_library(name = 'base', srcs = ['base.cc'], visibility = ['PUBLIC'])`,
		"app/BUILD":  `cc_binary(name = 'app', srcs = ['main.cc'], deps = ['//base:base'])`,
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ninjaFile, err := Build(root, []string{"//app:app"}, Options{RunNinja: false})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ninjaFile)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{
		"build build64_release/base/libbase.a: ar",
		"build build64_release/app/app: link",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated ninja missing %q\n%s", want, out)
		}
	}
}

func TestBuildRunsNinja(t *testing.T) {
	if _, err := exec.LookPath("ninja"); err != nil {
		t.Skip("ninja not available")
	}
	if _, err := exec.LookPath("c++"); err != nil {
		t.Skip("c++ not available")
	}
	root := t.TempDir()
	files := map[string]string{
		"BLADE_ROOT": `cc_config()`,
		"hi/hi.cc":   "#include <cstdio>\nint main(){printf(\"built\\n\");}\n",
		"hi/BUILD":   `cc_binary(name = 'hi', srcs = ['hi.cc'])`,
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Build(root, []string{"//hi:hi"}, Options{RunNinja: true}); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(root, "build64_release/hi/hi")).CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "built" {
		t.Errorf("binary output=%q", strings.TrimSpace(string(out)))
	}
}
