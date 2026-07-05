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

	tc := toolchain.Detect()
	gen := New(tc)
	f, err := gen.Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	// Derive expected file names from the toolchain so the assertions hold under
	// MSVC naming on Windows (base.cc.obj / base.lib / app.exe) too.
	o := tc.ObjSuffix()
	baseLib := tc.StaticLib("base")
	appBin := tc.BinName("app")
	wants := []string{
		"build build_release/base/base.objs/base.cc" + o + ": cxx base/base.cc",
		"build build_release/base/" + baseLib + ": ar build_release/base/base.objs/base.cc" + o,
		"build build_release/app/" + appBin + ": link build_release/app/app.objs/main.cc" + o + " | build_release/base/" + baseLib,
		baseLib,     // the archive on the link line (in the libs var)
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

	tc := toolchain.Detect()
	f, err := New(tc).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	o := tc.ObjSuffix()
	wants := []string{
		"build build_release/pb/msg.pb.cc build_release/pb/msg.pb.h: protoc pb/msg.proto",
		"build build_release/pb/msg.pb.cc" + o + ": cxx build_release/pb/msg.pb.cc | build_release/pb/msg.pb.h",
		"build build_release/pb/" + tc.StaticLib("msg") + ": ar build_release/pb/msg.pb.cc" + o,
		"build_release/pb/msg.pb.h", // consumer compile gets the generated header as an implicit dep
		"-lprotobuf",                // proto pulls in the protobuf runtime for the link
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("generated ninja missing %q\n---\n%s", w, out)
		}
	}
	// The consumer's own compile lists the generated header as an implicit dep.
	if !strings.Contains(out, "build build_release/app/app.objs/main.cc"+o+": cxx app/main.cc | build_release/pb/msg.pb.h") {
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
	gen.Linkflags = strings.Fields(lflags) // protobuf runtime libs -> the ldflags var
	f, err := gen.Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	ninjaText := f.String()
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
	tc := toolchain.Detect()
	f, err := New(tc).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	wants := []string{
		"build build_release/g/out.cc: gen g/in.tmpl",
		"cmd = cp g/in.tmpl build_release/g/out.cc", // $SRCS/$OUTS substituted
		// the consumer compiles the generated source from its build-dir path:
		"build build_release/g/lib.objs/out.cc" + tc.ObjSuffix() + ": cxx build_release/g/out.cc",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q\n---\n%s", w, out)
		}
	}
}

// A gen_rule cmd referencing flare's single-overlay-triplet protoc glob is
// emitted with the concrete blade-go compat dir, not a shell glob -- otherwise,
// when the build dir is shared with Python Blade, "blade-*" expands to several
// protoc paths and protoc parses a protoc binary as a .proto.
func TestGenRuleResolvesProtocGlob(t *testing.T) {
	g, _ := buildGraph(t, map[string]string{
		"g/BUILD": `
gen_rule(name = 'mk', srcs = ['x.proto'], outs = ['x.pb.cc'],
         cmd = '$BUILD_DIR/.cache/vcpkg/installed/blade-*/tools/protobuf/protoc $SRCS', visibility = ['PUBLIC'])
`,
	}, "//g:mk")
	f, err := New(toolchain.Detect()).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	if !strings.Contains(out, "installed/blade-go/tools/protobuf/protoc") {
		t.Errorf("protoc glob not resolved to concrete blade-go path:\n%s", out)
	}
	if strings.Contains(out, "installed/blade-*/") {
		t.Errorf("shell glob blade-* left in cmd (ambiguous when build dir is shared):\n%s", out)
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

// cc_config warnings are emitted as a top-level ninja var applied to compiles;
// a `warning = 'no'` target overrides that var to empty for its own edges.
func TestWarningsWiring(t *testing.T) {
	g, _ := buildGraph(t, map[string]string{
		"a/BUILD": `cc_library(name = 'a', srcs = ['a.cc'], warning = 'no', visibility = ['PUBLIC'])`,
	}, "//a:a")
	gen := New(toolchain.Detect())
	gen.CxxWarnings = []string{"-Werror", "-Wall"}
	f, err := gen.Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	if !strings.Contains(out, "cxx_warnings = -Werror -Wall\n") {
		t.Errorf("top-level cxx_warnings not wired:\n%s", out)
	}
	if !strings.Contains(out, "  cxx_warnings = \n") {
		t.Errorf("warning='no' did not override cxx_warnings to empty:\n%s", out)
	}
}

// A prebuilt_cc_library's archive (lib${bits}/lib<name>.a in the source tree) is
// linked by a consumer; no build edge compiles it.
func TestEndToEndPrebuilt(t *testing.T) {
	if _, err := exec.LookPath("ninja"); err != nil {
		t.Skip("ninja not available")
	}
	tc := toolchain.Detect()
	if _, err := exec.LookPath(tc.CC); err != nil {
		t.Skip("C compiler not available")
	}
	g, root := buildGraph(t, map[string]string{
		"ext/greet.h": "const char* greet(void);\n",
		"ext/greet.c": "const char* greet(void){ return \"hi-prebuilt\"; }\n",
		"ext/BUILD":   `prebuilt_cc_library(name = 'greet', hdrs = ['greet.h'], visibility = ['PUBLIC'])`,
		"app/main.cc": `extern "C" {
#include "ext/greet.h"
}
#include <cstdio>
int main(){ printf("%s\n", greet()); return 0; }
`,
		"app/BUILD": `cc_binary(name = 'app', srcs = ['main.cc'], deps = ['//ext:greet'])`,
	}, "//app:app")

	// Produce the prebuilt archive at ext/lib64/libgreet.a from ext/greet.c.
	if err := os.MkdirAll(filepath.Join(root, "ext/lib64"), 0o755); err != nil {
		t.Fatal(err)
	}
	obj := filepath.Join(root, "ext/greet.o")
	if o, err := exec.Command(tc.CC, "-c", filepath.Join(root, "ext/greet.c"), "-o", obj).CombinedOutput(); err != nil {
		t.Fatalf("compile prebuilt: %v\n%s", err, o)
	}
	if o, err := exec.Command(tc.AR, "rcs", filepath.Join(root, "ext/lib64/libgreet.a"), obj).CombinedOutput(); err != nil {
		t.Fatalf("ar prebuilt: %v\n%s", err, o)
	}

	gen := New(tc)
	gen.Root = root
	f, err := gen.Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.String(), "ext/lib64/libgreet.a") {
		t.Fatalf("app link does not reference the prebuilt archive:\n%s", f.String())
	}
	if err := os.WriteFile(filepath.Join(root, "build.ninja"), []byte(f.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	nc := exec.Command("ninja", "-f", "build.ninja", "build_release/app/app")
	nc.Dir = root
	if o, err := nc.CombinedOutput(); err != nil {
		t.Fatalf("ninja: %v\n%s", err, o)
	}
	o, err := exec.Command(filepath.Join(root, "build_release/app/app")).CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, o)
	}
	if strings.TrimSpace(string(o)) != "hi-prebuilt" {
		t.Errorf("output=%q, want hi-prebuilt", strings.TrimSpace(string(o)))
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
	if !strings.Contains(out, "-isystem "+filepath.Join(installed, "include")) {
		t.Errorf("compile missing vcpkg include dir (as -isystem)\n%s", out)
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
	tc := toolchain.Detect()
	f, err := New(tc).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	if strings.Contains(out, tc.StaticLib("h")) {
		t.Errorf("header-only library should produce no archive:\n%s", out)
	}
	if !strings.Contains(out, "build build_release/app/"+tc.BinName("app")+": link") {
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

// --- Linux portability fixes (verified against flare on aarch64 Linux) ---

func TestLinkArgsELFGroupsVcpkgWithOwnLibs(t *testing.T) {
	// On ELF, the target's own archives AND the vcpkg archives must share one
	// --start-group so forward refs across them (curl -> ssl -> crypto) resolve.
	gen := New(&toolchain.Toolchain{OS: "linux"})
	got := gen.linkArgs(
		[]string{"libbuffer.a"},
		[]string{"pthread", "dl"},
		[]string{"/v/libcurl.a", "/v/libssl.a", "/v/libcrypto.a"},
	)
	want := "-Wl,--start-group libbuffer.a /v/libcurl.a /v/libssl.a /v/libcrypto.a " +
		"-Wl,--end-group -lpthread -ldl"
	if got != want {
		t.Errorf("ELF linkArgs\n got: %s\nwant: %s", got, want)
	}
}

func TestLinkArgsNonELFUngrouped(t *testing.T) {
	// macOS ld64 re-scans archives; keep the flat order (libs, -l syslibs, vcpkg
	// args last -- the last may be -framework flags).
	gen := New(&toolchain.Toolchain{OS: "darwin"})
	got := gen.linkArgs([]string{"libbuffer.a"}, []string{"pthread"},
		[]string{"/v/libssl.a", "-framework", "CoreFoundation"})
	want := "libbuffer.a -lpthread /v/libssl.a -framework CoreFoundation"
	if got != want {
		t.Errorf("non-ELF linkArgs\n got: %s\nwant: %s", got, want)
	}
}

func TestNormInc(t *testing.T) {
	cases := []struct{ pkg, inc, want string }{
		{"flare/base", ".", "flare/base"}, // package-relative
		{"thirdparty/gperftools", "//build_release/thirdparty/gperftools/include", // root-relative
			"build_release/thirdparty/gperftools/include"},
		{"flare/base", "include", "flare/base/include"},
		{"", ".", "."},
	}
	for _, c := range cases {
		if got := normInc(c.pkg, c.inc); got != c.want {
			t.Errorf("normInc(%q,%q)=%q, want %q", c.pkg, c.inc, got, c.want)
		}
	}
}

func TestPickForeignArchive(t *testing.T) {
	// gperftools_build emits many archives shared by sibling foreign_cc_library
	// targets; each must select lib<name>.a, not an arbitrary one.
	multi := map[string]string{
		"libtcmalloc.a":  "build/lib/libtcmalloc.a",
		"libprofiler.a":  "build/lib/libprofiler.a",
		"libtcmalloc.so": "build/lib/libtcmalloc.so", // non-.a ignored
	}
	if got := pickForeignArchive("libprofiler.a", multi); got != "build/lib/libprofiler.a" {
		t.Errorf("multi-archive: got %q, want libprofiler.a", got)
	}
	if got := pickForeignArchive("libtcmalloc.a", multi); got != "build/lib/libtcmalloc.a" {
		t.Errorf("multi-archive: got %q, want libtcmalloc.a", got)
	}
	// Single-archive build (jsoncpp): resolve even if the name doesn't match.
	single := map[string]string{"x.a": "build/lib/libjsoncpp.a"}
	if got := pickForeignArchive("libNOMATCH.a", single); got != "build/lib/libjsoncpp.a" {
		t.Errorf("single-archive fallback: got %q", got)
	}
	// No match among several: refuse to guess.
	if got := pickForeignArchive("libnope.a", multi); got != "" {
		t.Errorf("ambiguous no-match should be empty, got %q", got)
	}
}

func TestGenerateMSVCRules(t *testing.T) {
	// Drive the MSVC codepath on any host by constructing an MSVC toolchain
	// explicitly (IsMSVC keys on OS=="windows").
	g, _ := buildGraph(t, map[string]string{
		"base/BUILD": `cc_library(name = 'base', srcs = ['base.cc'], visibility = ['PUBLIC'])`,
		"app/BUILD":  `cc_binary(name = 'app', srcs = ['main.cc'], defs = ['FOO=1'], deps = ['//base:base'])`,
	}, "//app:app")
	tc := &toolchain.Toolchain{OS: "windows", CC: "cl.exe", CXX: "cl.exe", AR: "lib.exe", Link: "link.exe"}
	f, err := New(tc).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	wants := []string{
		"/nologo /c /showIncludes",                             // cl compile
		"deps = msvc",                                          // ninja MSVC dep parsing
		"msvc_deps_prefix = Note: including file:",             // /showIncludes prefix
		"/Fo${out} /Tp${in}",                                   // C++ object output + language
		"${ar} /nologo /OUT:${out} ${in}",                      // lib.exe archive
		"${link} /nologo ${in} ${libs} ${ldflags} /OUT:${out}", // link.exe
		"link = link.exe",                                      // linker var
		"build build_release/base/base.lib: ar",                // MSVC static-lib name
		"build build_release/app/app.exe: link",                // MSVC exe name
		"/DFOO=1",                                              // MSVC define flag
		`/I"."`,                                                // MSVC include flag (quoted for spaces)
		"/O2 /DNDEBUG",                                         // MSVC release optimize
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("MSVC ninja missing %q\n---\n%s", w, out)
		}
	}
}

func TestPrivateIncsOnCompile(t *testing.T) {
	// A target's own `incs` (private include dirs) must be added to its compiles,
	// package-relative, like Blade's attr['incs'].
	g, _ := buildGraph(t, map[string]string{
		"lib/BUILD": `cc_library(name = 'g', srcs = ['g.cc'], incs = ['include'])`,
	}, "//lib:g")
	f, err := New(toolchain.Detect()).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	if out := f.String(); !strings.Contains(out, "lib/include") {
		t.Errorf("private inc 'lib/include' not on the compile include path:\n%s", out)
	}
}

func TestGenerateMSVCAsmRule(t *testing.T) {
	// A .asm source on an MSVC toolchain with an assembler routes to the `as`
	// rule (armasm64 -> gnu-style -o), not cl.
	g, _ := buildGraph(t, map[string]string{
		"a/BUILD": `cc_library(name = 'a', srcs = ['add.asm'], visibility = ['PUBLIC'])`,
	}, "//a:a")
	tc := &toolchain.Toolchain{OS: "windows", CC: "cl.exe", AR: "lib.exe", Link: "link.exe", AS: "armasm64.exe"}
	f, err := New(tc).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	for _, w := range []string{
		"as = armasm64.exe",
		"${as} ${extra_compile_flags} -o ${out} ${in}",
		"build build_release/a/a.objs/add.asm.obj: as a/add.asm",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("MSVC asm ninja missing %q\n---\n%s", w, out)
		}
	}
}

func TestDllBasename(t *testing.T) {
	if got := dllBasename("suites/cc_basic", "hello"); got != "suites.cc_basic.hello.dll" {
		t.Errorf("dllBasename=%q", got)
	}
	if got := dllBasename("", "x"); got != "x.dll" {
		t.Errorf("dllBasename root=%q", got)
	}
}

func TestGenerateMSVCDynamicLink(t *testing.T) {
	// A dynamic_link binary builds its cc_library dep as a DLL (+ import lib via
	// windef/solink), links the import lib, and stages the DLL next to the exe.
	g, _ := buildGraph(t, map[string]string{
		"lib/BUILD": `cc_library(name = 'lib', srcs = ['l.cc'], visibility = ['PUBLIC'])`,
		"app/BUILD": `cc_binary(name = 'app', srcs = ['m.cc'], deps = ['//lib:lib'], dynamic_link = True)`,
	}, "//app:app")
	tc := &toolchain.Toolchain{OS: "windows", CC: "cl.exe", AR: "lib.exe", Link: "link.exe"}
	f, err := New(tc).Generate(g)
	if err != nil {
		t.Fatal(err)
	}
	out := f.String()
	wants := []string{
		"build build_release/lib/lib.def: windef",
		"build build_release/lib/lib.lib.dll | build_release/lib/lib.dll.lib: solink",
		"build build_release/app/lib.lib.dll: copy build_release/lib/lib.lib.dll", // staged next to exe
		"build_release/lib/lib.dll.lib",                                           // app links the import lib
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("dynamic-link ninja missing %q\n---\n%s", w, out)
		}
	}
	// The static archive of a DLL-ified lib must NOT be on the app link line.
	if strings.Contains(out, "build build_release/app/app.exe: link") &&
		strings.Contains(out, "libs = build_release/lib/lib.lib ") {
		t.Errorf("dynamic-link app should link the import lib, not the static archive\n%s", out)
	}
}
