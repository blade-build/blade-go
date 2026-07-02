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

func TestBuildAppliesProtoConfig(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"BLADE_ROOT": `proto_library_config(protoc = '/custom/protoc', protobuf_libs = ['#protobuf', '#pthread'])`,
		"pb/BUILD":   `proto_library(name = 'msg', srcs = ['msg.proto'], visibility = ['PUBLIC'])`,
		"app/BUILD":  `cc_binary(name = 'app', srcs = ['main.cc'], deps = ['//pb:msg'])`,
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
	data, _ := os.ReadFile(ninjaFile)
	out := string(data)
	if !strings.Contains(out, "protoc = /custom/protoc") {
		t.Errorf("configured protoc not applied:\n%s", out)
	}
	if !strings.Contains(out, "-lprotobuf") || !strings.Contains(out, "-lpthread") {
		t.Errorf("configured protobuf_libs not applied:\n%s", out)
	}
}

func TestRunsCcTests(t *testing.T) {
	if _, err := exec.LookPath("ninja"); err != nil {
		t.Skip("ninja not available")
	}
	if _, err := exec.LookPath("c++"); err != nil {
		t.Skip("c++ not available")
	}
	root := t.TempDir()
	files := map[string]string{
		"BLADE_ROOT": `cc_config()`,
		"t/pass.cc":  "int main(){ return 0; }\n",
		"t/fail.cc":  "int main(){ return 1; }\n",
		"t/BUILD": `
cc_test(name = 'pass', srcs = ['pass.cc'])
cc_test(name = 'fail', srcs = ['fail.cc'])
`,
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
	results, err := Test(root, []string{"//t:pass", "//t:fail"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	byLabel := map[string]bool{}
	for _, r := range results {
		byLabel[r.Label] = r.Passed
	}
	if !byLabel["//t:pass"] {
		t.Error("//t:pass should pass")
	}
	if byLabel["//t:fail"] {
		t.Error("//t:fail should fail (exit 1)")
	}
}
