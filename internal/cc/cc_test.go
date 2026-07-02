package cc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blade-build/blade-go/internal/graph"
	"github.com/blade-build/blade-go/internal/loader"
	"github.com/blade-build/blade-go/internal/toolchain"
)

func buildGraph(t *testing.T, files map[string]string, roots ...string) (*graph.Graph, string) {
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
	g, err := graph.NewBuilder(loader.New(root)).Build(roots)
	if err != nil {
		t.Fatal(err)
	}
	return g, root
}

func TestGenerateStructure(t *testing.T) {
	g, _ := buildGraph(t, map[string]string{
		"base/BUILD": `cc_library(name = 'base', srcs = ['base.cc'], deps = ['#pthread'], visibility = ['PUBLIC'])`,
		"app/BUILD":  `cc_binary(name = 'app', srcs = ['main.cc'], deps = ['//base:base'])`,
	}, "//app:app")

	gen := New(toolchain.Detect())
	f, err := gen.Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	wants := []string{
		"build build64_release/base/base.objs/base.cc.o: cxx base/base.cc",
		"build build64_release/base/libbase.a: ar build64_release/base/base.objs/base.cc.o",
		"build build64_release/app/app: link build64_release/app/app.objs/main.cc.o | build64_release/base/libbase.a",
		"libbase.a", // the archive on the link line (in the libs var)
		"-lpthread", // transitive syslib
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("generated ninja missing %q\n---\n%s", w, out)
		}
	}
}

// TestEndToEndBuild generates ninja for a real cc_library + cc_binary, runs
// ninja, and executes the resulting binary.
func TestEndToEndBuild(t *testing.T) {
	ninjaBin, err := exec.LookPath("ninja")
	if err != nil {
		t.Skip("ninja not available")
	}
	tc := toolchain.Detect()
	if _, err := exec.LookPath(tc.CXX); err != nil {
		t.Skipf("C++ compiler %q not available", tc.CXX)
	}

	g, root := buildGraph(t, map[string]string{
		"base/greeter.h":  "#pragma once\nconst char* greet();\n",
		"base/greeter.cc": `#include "base/greeter.h"` + "\nconst char* greet() { return \"hi-from-cc\"; }\n",
		"base/BUILD":      `cc_library(name = 'greeter', srcs = ['greeter.cc'], hdrs = ['greeter.h'], visibility = ['PUBLIC'])`,
		"app/main.cc":     `#include "base/greeter.h"` + "\n#include <cstdio>\nint main() { printf(\"%s\\n\", greet()); return 0; }\n",
		"app/BUILD":       `cc_binary(name = 'app', srcs = ['main.cc'], deps = ['//base:greeter'])`,
	}, "//app:app")

	f, err := New(tc).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	ninjaFile := filepath.Join(root, "build.ninja")
	if err := os.WriteFile(ninjaFile, []byte(f.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	binPath := "build64_release/app/app"
	cmd := exec.Command(ninjaBin, "-f", "build.ninja", binPath)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ninja build failed: %v\n%s\n--- build.ninja ---\n%s", err, out, f.String())
	}

	run := exec.Command(filepath.Join(root, binPath))
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("running binary: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "hi-from-cc" {
		t.Fatalf("binary output = %q, want hi-from-cc", strings.TrimSpace(string(out)))
	}
}
