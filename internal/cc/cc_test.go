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
	"github.com/blade-build/blade-go/internal/vcpkg"
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
		"build build_release/base/base.objs/base.cc.o: cxx base/base.cc",
		"build build_release/base/libbase.a: ar build_release/base/base.objs/base.cc.o",
		"build build_release/app/app: link build_release/app/app.objs/main.cc.o | build_release/base/libbase.a",
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

	binPath := "build_release/app/app"
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

func TestGenerateProtoStructure(t *testing.T) {
	g, _ := buildGraph(t, map[string]string{
		"pb/BUILD":  `proto_library(name = 'msg', srcs = ['msg.proto'], visibility = ['PUBLIC'])`,
		"app/BUILD": `cc_binary(name = 'app', srcs = ['main.cc'], deps = ['//pb:msg'])`,
	}, "//app:app")

	f, err := New(toolchain.Detect()).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	wants := []string{
		"build build_release/pb/msg.pb.cc build_release/pb/msg.pb.h: protoc pb/msg.proto",
		"build build_release/pb/msg.pb.cc.o: cxx build_release/pb/msg.pb.cc | build_release/pb/msg.pb.h",
		"build build_release/pb/libmsg.a: ar build_release/pb/msg.pb.cc.o",
		"build_release/pb/msg.pb.h", // consumer compile gets the generated header as an implicit dep
		"-lprotobuf",                // proto pulls in the protobuf runtime for the link
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("generated ninja missing %q\n---\n%s", w, out)
		}
	}
	// The consumer's own compile lists the generated header as an implicit dep.
	if !strings.Contains(out, "build build_release/app/app.objs/main.cc.o: cxx app/main.cc | build_release/pb/msg.pb.h") {
		t.Errorf("consumer compile missing generated-header implicit dep\n%s", out)
	}
}

func TestEndToEndProto(t *testing.T) {
	if _, err := exec.LookPath("ninja"); err != nil {
		t.Skip("ninja not available")
	}
	protoc, err := exec.LookPath("protoc")
	if err != nil {
		t.Skip("protoc not available")
	}
	tc := toolchain.Detect()

	g, root := buildGraph(t, map[string]string{
		"pb/item.proto": "syntax = \"proto3\";\npackage demo;\nmessage Item { string name = 1; int32 qty = 2; }\n",
		"pb/BUILD":      `proto_library(name = 'item', srcs = ['item.proto'], visibility = ['PUBLIC'])`,
		"app/main.cc": `#include "pb/item.pb.h"` + "\n#include <cstdio>\n" +
			"int main(){ demo::Item i; i.set_name(\"widget\"); i.set_qty(7);\n" +
			" printf(\"%s:%d\\n\", i.name().c_str(), i.qty()); return 0; }\n",
		"app/BUILD": `cc_binary(name = 'app', srcs = ['main.cc'], deps = ['//pb:item'])`,
	}, "//app:app")

	gen := New(tc)
	gen.Protoc = protoc
	// Discover protobuf include/lib flags via pkg-config when available, so the
	// generated code compiles/links against the installed runtime.
	if pc, err := exec.LookPath("pkg-config"); err == nil {
		if out, e := exec.Command(pc, "--exists", "protobuf").CombinedOutput(); e == nil {
			_ = out
		} else {
			t.Skip("protobuf not registered with pkg-config")
		}
	}
	// Compile/link against the installed protobuf runtime: C++17 + pkg-config
	// cflags go through the generator's Cxxflags; ldflags are prepended below.
	cflags, lflags := pkgConfig(t, "protobuf")
	gen.Cxxflags = append([]string{"-std=c++17"}, strings.Fields(cflags)...)
	f, err := gen.Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	ninjaText := "ldflags = " + lflags + "\n" + f.String()
	if err := os.WriteFile(filepath.Join(root, "build.ninja"), []byte(ninjaText), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ninja", "-f", "build.ninja", "build_release/app/app")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ninja build failed: %v\n%s", err, out)
	}
	out, err := exec.Command(filepath.Join(root, "build_release/app/app")).CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "widget:7" {
		t.Fatalf("binary output=%q, want widget:7", strings.TrimSpace(string(out)))
	}
}

func pkgConfig(t *testing.T, pkg string) (cflags, libs string) {
	t.Helper()
	pc, err := exec.LookPath("pkg-config")
	if err != nil {
		t.Skip("pkg-config not available")
	}
	c, err := exec.Command(pc, "--cflags", pkg).Output()
	if err != nil {
		t.Skipf("pkg-config --cflags %s: %v", pkg, err)
	}
	l, err := exec.Command(pc, "--libs", pkg).Output()
	if err != nil {
		t.Skipf("pkg-config --libs %s: %v", pkg, err)
	}
	return strings.TrimSpace(string(c)), strings.TrimSpace(string(l))
}

func TestGenerateGenRule(t *testing.T) {
	g, _ := buildGraph(t, map[string]string{
		"g/BUILD": `
gen_rule(name = 'mk', srcs = ['in.tmpl'], outs = ['out.cc'], cmd = 'cp $SRCS $OUTS', visibility = ['PUBLIC'])
cc_library(name = 'lib', srcs = ['out.cc'], deps = [':mk'])
`,
	}, "//g:lib")
	f, err := New(toolchain.Detect()).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	wants := []string{
		"build build_release/g/out.cc: gen g/in.tmpl",
		"cmd = cp g/in.tmpl build_release/g/out.cc", // $SRCS/$OUTS substituted
		// the consumer compiles the generated source from its build-dir path:
		"build build_release/g/lib.objs/out.cc.o: cxx build_release/g/out.cc",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q\n---\n%s", w, out)
		}
	}
}

func TestEndToEndGenRule(t *testing.T) {
	if _, err := exec.LookPath("ninja"); err != nil {
		t.Skip("ninja not available")
	}
	tc := toolchain.Detect()
	if _, err := exec.LookPath(tc.CXX); err != nil {
		t.Skip("C++ compiler not available")
	}
	g, root := buildGraph(t, map[string]string{
		"g/tmpl.cc": "#include <cstdio>\nint main(){ printf(\"gen-ok\\n\"); return 0; }\n",
		"g/BUILD": `
gen_rule(name = 'mk', srcs = ['tmpl.cc'], outs = ['hello.cc'], cmd = 'cp $SRCS $OUTS', visibility = ['PUBLIC'])
cc_binary(name = 'app', srcs = ['hello.cc'], deps = [':mk'])
`,
	}, "//g:app")
	f, err := New(tc).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "build.ninja"), []byte(f.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ninja", "-f", "build.ninja", "build_release/g/app")
	cmd.Dir = root
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ninja: %v\n%s\n%s", err, o, f.String())
	}
	o, err := exec.Command(filepath.Join(root, "build_release/g/app")).CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, o)
	}
	if strings.TrimSpace(string(o)) != "gen-ok" {
		t.Errorf("output=%q", strings.TrimSpace(string(o)))
	}
}

func TestVcpkgLinkFlags(t *testing.T) {
	vroot := t.TempDir()
	installed := filepath.Join(vroot, "installed", "x64-linux")
	if err := os.MkdirAll(filepath.Join(installed, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "lib", "libfoo.a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	g, _ := buildGraph(t, map[string]string{
		"app/BUILD": `cc_binary(name = 'app', srcs = ['main.cc'], deps = ['vcpkg#foo'])`,
	}, "//app:app")
	gen := New(toolchain.Detect())
	gen.Vcpkg = &vcpkg.Resolver{Root: vroot, Triplet: "x64-linux"}
	f, err := gen.Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	if !strings.Contains(out, "-I"+filepath.Join(installed, "include")) {
		t.Errorf("compile missing vcpkg include dir\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(installed, "lib", "libfoo.a")) {
		t.Errorf("link missing vcpkg archive\n%s", out)
	}
}

func TestEndToEndVcpkg(t *testing.T) {
	if _, err := exec.LookPath("ninja"); err != nil {
		t.Skip("ninja not available")
	}
	tc := toolchain.Detect()
	if _, err := exec.LookPath(tc.CXX); err != nil {
		t.Skip("C++ compiler not available")
	}
	if _, err := exec.LookPath(tc.AR); err != nil {
		t.Skip("ar not available")
	}

	// Build a fake vcpkg tree with a real static library.
	vroot := t.TempDir()
	installed := filepath.Join(vroot, "installed", "test-triplet")
	inc := filepath.Join(installed, "include")
	libdir := filepath.Join(installed, "lib")
	for _, d := range []string{inc, libdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inc, "greeter.h"), []byte("#pragma once\nconst char* greet();\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	greeterC := filepath.Join(src, "greeter.cc")
	os.WriteFile(greeterC, []byte(`#include "greeter.h"`+"\nconst char* greet(){ return \"vcpkg-ok\"; }\n"), 0o644)
	obj := filepath.Join(src, "greeter.o")
	if o, err := exec.Command(tc.CXX, "-I", inc, "-c", greeterC, "-o", obj).CombinedOutput(); err != nil {
		t.Fatalf("compiling fake vcpkg lib: %v\n%s", err, o)
	}
	if o, err := exec.Command(tc.AR, "rcs", filepath.Join(libdir, "libgreeter.a"), obj).CombinedOutput(); err != nil {
		t.Fatalf("archiving fake vcpkg lib: %v\n%s", err, o)
	}

	g, root := buildGraph(t, map[string]string{
		"app/main.cc": `#include "greeter.h"` + "\n#include <cstdio>\nint main(){ printf(\"%s\\n\", greet()); return 0; }\n",
		"app/BUILD":   `cc_binary(name = 'app', srcs = ['main.cc'], deps = ['vcpkg#greeter'])`,
	}, "//app:app")
	gen := New(tc)
	gen.Vcpkg = &vcpkg.Resolver{Root: vroot, Triplet: "test-triplet"}
	f, err := gen.Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "build.ninja"), []byte(f.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ninja", "-f", "build.ninja", "build_release/app/app")
	cmd.Dir = root
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ninja: %v\n%s\n%s", err, o, f.String())
	}
	o, err := exec.Command(filepath.Join(root, "build_release/app/app")).CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, o)
	}
	if strings.TrimSpace(string(o)) != "vcpkg-ok" {
		t.Errorf("output=%q, want vcpkg-ok", strings.TrimSpace(string(o)))
	}
}

func TestHeaderOnlyLibraryNoArchive(t *testing.T) {
	// A cc_library with only headers (no srcs) must not emit an `ar` edge with
	// zero object files (ar errors on that). Found compiling flare's //base:align.
	g, _ := buildGraph(t, map[string]string{
		"h/BUILD":   `cc_library(name = 'h', hdrs = ['h.h'], visibility = ['PUBLIC'])`,
		"app/BUILD": `cc_binary(name = 'app', srcs = ['main.cc'], deps = ['//h:h'])`,
	}, "//app:app")
	f, err := New(toolchain.Detect()).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	if strings.Contains(out, "libh.a") {
		t.Errorf("header-only library should produce no archive:\n%s", out)
	}
	if !strings.Contains(out, "build build_release/app/app: link") {
		t.Errorf("consumer link edge missing:\n%s", out)
	}
}

func TestIsHeaderCompilesAsCXX(t *testing.T) {
	// A header listed in srcs (blade's self-sufficiency check) must compile with
	// the C++ compiler -- it pulls in <memory> and other C++ headers.
	for _, h := range []string{"a.h", "b.hpp", "c.hh", "d.inc"} {
		if !isHeader(h) {
			t.Errorf("isHeader(%q)=false", h)
		}
	}
	for _, c := range []string{"a.c", "a.cc", "a.o"} {
		if isHeader(c) {
			t.Errorf("isHeader(%q)=true, want false", c)
		}
	}
}
