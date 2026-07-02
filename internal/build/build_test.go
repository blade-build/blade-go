package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/blade-build/blade-go/internal/ninjaparse"
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

func TestBuildRoutesVcpkgProtobuf(t *testing.T) {
	// flare pins protobuf in vcpkg: protobuf_libs = ['vcpkg#protobuf:protobuf'].
	// The proto compile must see the vcpkg include dir, and the link must use the
	// vcpkg archive -- not a bare -lprotobuf.
	vroot := t.TempDir()
	t.Setenv("VCPKG_ROOT", vroot)
	t.Setenv("VCPKG_DEFAULT_TRIPLET", "test-triplet")
	inst := filepath.Join(vroot, "installed", "test-triplet")
	if err := os.MkdirAll(filepath.Join(inst, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(inst, "include"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst, "lib", "libprotobuf.a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	files := map[string]string{
		"BLADE_ROOT": `proto_library_config(protoc = 'protoc', protobuf_libs = ['vcpkg#protobuf:protobuf'])`,
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
	if want := filepath.Join(inst, "include"); !strings.Contains(out, want) {
		t.Errorf("proto compile missing vcpkg include %q:\n%s", want, out)
	}
	if want := filepath.Join(inst, "lib", "libprotobuf.a"); !strings.Contains(out, want) {
		t.Errorf("link missing vcpkg protobuf archive %q:\n%s", want, out)
	}
	if strings.Contains(out, "-lprotobuf") {
		t.Errorf("protobuf should route via vcpkg archive, not -lprotobuf:\n%s", out)
	}
}

func TestBuildForeignCcLibrary(t *testing.T) {
	// A source-built thirdparty library (jsoncpp-style): a gen_rule chain
	// (unpack -> build) produces an archive under thirdparty/, wrapped by a
	// foreign_cc_library. A consumer must (1) resolve the chain -- the build
	// gen_rule's `foo.stamp` src points at the unpack gen_rule's output, not the
	// source tree; (2) see $OUT_DIR expanded; (3) link the archive; (4) get the
	// exported include dirs. The thirdparty/ package has a real BUILD, so its
	// intra-package deps must NOT be misrouted to vcpkg.
	root := t.TempDir()
	files := map[string]string{
		"BLADE_ROOT": `cc_config()`,
		"thirdparty/foo/BUILD": `
gen_rule(name = 'foo_unpack', outs = ['foo.stamp'], cmd = 'touch $OUTS')
gen_rule(name = 'foo_build', srcs = ['foo.stamp'], outs = ['lib/libfoo.a'],
         deps = [':foo_unpack'], cmd = 'build @ $OUT_DIR', system_export_incs = 'include')
foreign_cc_library(name = 'foo', deps = [':foo_build'], visibility = ['PUBLIC'])
`,
		"app/BUILD": `cc_binary(name = 'app', srcs = ['m.cc'], deps = ['//thirdparty/foo:foo'])`,
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
		t.Fatalf("foreign_cc_library build failed to plan: %v", err)
	}
	out := string(mustRead(t, ninjaFile))

	// (1) chain: foo_build's stamp src resolves to the unpack's build-dir output.
	if !strings.Contains(out, "build64_release/thirdparty/foo/foo.stamp") {
		t.Errorf("gen_rule chain src not resolved to the dep's output:\n%s", out)
	}
	// (2) $OUT_DIR expanded to the target's output dir.
	if !strings.Contains(out, "build @ build64_release/thirdparty/foo") {
		t.Errorf("$OUT_DIR not expanded:\n%s", out)
	}
	// (3) consumer links the built archive.
	if !strings.Contains(out, "build64_release/thirdparty/foo/lib/libfoo.a") {
		t.Errorf("consumer does not link the foreign archive:\n%s", out)
	}
	// (4) consumer gets the exported include dirs.
	if !strings.Contains(out, "-Ibuild64_release/thirdparty ") && !strings.Contains(out, "-Ibuild64_release/thirdparty\n") {
		t.Errorf("consumer missing the pkg-parent include dir:\n%s", out)
	}
	if !strings.Contains(out, "-Ibuild64_release/thirdparty/foo/include") {
		t.Errorf("consumer missing system_export_incs dir:\n%s", out)
	}
	// (5) the consumer's COMPILE waits for the foreign build (its archive is an
	// implicit dep) so the header shims exist -- else it races and misses them.
	if !strings.Contains(out, "app/m.cc | ") || !strings.Contains(out, "app/m.cc | build64_release/thirdparty/foo/lib/libfoo.a") {
		t.Errorf("consumer compile not ordered after the foreign build:\n%s", out)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBuildResourceLibrary(t *testing.T) {
	// A resource_library embeds files: emit the index (.h/.c) + a .c per
	// resource, compile all, archive, and let a consumer link it.
	root := t.TempDir()
	files := map[string]string{
		"BLADE_ROOT": `cc_config()`,
		"res/a.txt":  "hello",
		"res/BUILD":  `resource_library(name = 'r', srcs = ['a.txt'], visibility = ['PUBLIC'])`,
		"app/BUILD":  `cc_binary(name = 'app', srcs = ['m.cc'], deps = ['//res:r'])`,
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
	out := string(mustRead(t, ninjaFile))
	// index edge produces the .h and .c
	if !strings.Contains(out, "build64_release/res/r.h build64_release/res/r.c: resource_index") {
		t.Errorf("resource_index edge missing:\n%s", out)
	}
	// per-resource embed edge
	if !strings.Contains(out, "build64_release/res/a.txt.c: resource res/a.txt") {
		t.Errorf("resource embed edge missing:\n%s", out)
	}
	// archived and linked by the consumer
	if !strings.Contains(out, "build64_release/res/libr.a") {
		t.Errorf("resource archive missing:\n%s", out)
	}
}

func TestBuildLinkAllSymbols(t *testing.T) {
	// A cc_library with link_all_symbols must be force-loaded (whole archive)
	// into a consumer binary, so its static initializers survive.
	root := t.TempDir()
	files := map[string]string{
		"BLADE_ROOT": `cc_config()`,
		"reg/BUILD":  `cc_library(name = 'reg', srcs = ['r.cc'], link_all_symbols = True, visibility = ['PUBLIC'])`,
		"app/BUILD":  `cc_binary(name = 'app', srcs = ['m.cc'], deps = ['//reg:reg'])`,
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
	out := string(mustRead(t, ninjaFile))
	archive := "build64_release/reg/libreg.a"
	// Platform-specific whole-archive wrapping around the archive.
	if !strings.Contains(out, "-force_load,"+archive) &&
		!strings.Contains(out, "--whole-archive "+archive) {
		t.Errorf("link_all_symbols archive not force-loaded:\n%s", out)
	}
}

func TestBuildBenchmarkLinksBenchmarkLib(t *testing.T) {
	// A cc_benchmark links cc_config's benchmark_libs (google-benchmark from
	// vcpkg), not the gtest test framework.
	vroot := t.TempDir()
	t.Setenv("VCPKG_ROOT", vroot)
	t.Setenv("VCPKG_DEFAULT_TRIPLET", "test-triplet")
	inst := filepath.Join(vroot, "installed", "test-triplet")
	if err := os.MkdirAll(filepath.Join(inst, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst, "lib", "libbenchmark.a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	files := map[string]string{
		"BLADE_ROOT": `cc_config(benchmark_libs = ['//thirdparty/benchmark:benchmark'])`,
		"p/BUILD":    `cc_benchmark(name = 'b', srcs = ['b.cc'])`,
	}
	for rel, content := range files {
		fp := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ninjaFile, err := Build(root, []string{"//p:b"}, Options{RunNinja: false})
	if err != nil {
		t.Fatal(err)
	}
	out := string(mustRead(t, ninjaFile))
	if !strings.Contains(out, filepath.Join(inst, "lib", "libbenchmark.a")) {
		t.Errorf("cc_benchmark does not link the benchmark archive:\n%s", out)
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
	// onResult must be called once per test as it finishes (streaming), before
	// Test returns -- so a large suite shows progress instead of looking hung.
	var streamed []string
	results, err := Test(root, []string{"//t:pass", "//t:fail"}, Options{},
		func(r TestResult) { streamed = append(streamed, r.Label) })
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if len(streamed) != 2 {
		t.Errorf("onResult called %d times, want 2 (results must stream)", len(streamed))
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

func writeWorkspace(t *testing.T, files map[string]string) string {
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

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestDifferentialVsPythonBlade checks that blade-go's generated build graph
// matches Python Blade's (the oracle) on the same workspace: it compiles the
// same source files and produces the same static archives. Set BLADE_PY to the
// Python Blade launcher to run it.
func TestDifferentialVsPythonBlade(t *testing.T) {
	pyBlade := os.Getenv("BLADE_PY")
	if pyBlade == "" {
		t.Skip("set BLADE_PY to the Python Blade launcher to run the differential test")
	}
	files := map[string]string{
		"BLADE_ROOT":   "cc_config()\n",
		"base/base.h":  "#pragma once\nint add(int, int);\n",
		"base/base.cc": "#include \"base/base.h\"\nint add(int a, int b){ return a + b; }\n",
		"base/BUILD":   "cc_library(name = 'base', srcs = ['base.cc'], hdrs = ['base.h'], visibility = ['PUBLIC'])\n",
		"app/main.cc":  "#include \"base/base.h\"\nint main(){ return add(1, 2) == 3 ? 0 : 1; }\n",
		"app/BUILD":    "cc_binary(name = 'app', srcs = ['main.cc'], deps = ['//base:base'])\n",
	}
	pyRoot := writeWorkspace(t, files)
	goRoot := writeWorkspace(t, files)

	// Python Blade (the oracle): generate ninja only.
	cmd := exec.Command(pyBlade, "build", "//app:app", "--stop-after", "generate")
	cmd.Dir = pyRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		// The oracle couldn't run here (toolchain/env) -- skip rather than fail;
		// a genuine divergence is only meaningful once both generate.
		t.Skipf("python blade did not run: %v\n%s", err, out)
	}
	pyNinja := findNinja(t, pyRoot)

	// blade-go.
	if _, err := Build(goRoot, []string{"//app:app"}, Options{RunNinja: false}); err != nil {
		t.Fatal(err)
	}
	goNinja := findNinja(t, goRoot)

	pyE := ninjaparse.Parse(pyNinja)
	goE := ninjaparse.Parse(goNinja)
	if a, b := normSet(ninjaparse.CompiledSources(pyE)), normSet(ninjaparse.CompiledSources(goE)); !reflect.DeepEqual(a, b) {
		t.Errorf("compiled sources differ:\n  python=%v\n  blade-go=%v", sortedKeys(a), sortedKeys(b))
	}
	if a, b := ninjaparse.Archives(pyE), ninjaparse.Archives(goE); !reflect.DeepEqual(a, b) {
		t.Errorf("archives differ:\n  python=%v\n  blade-go=%v", sortedKeys(a), sortedKeys(b))
	}
}

// normSet drops blade-internal sources blade-go doesn't emit (the SCM version
// stamp), so the comparison is about the user's sources.
func normSet(m map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		if k != "scm.cc" {
			out[k] = true
		}
	}
	return out
}

// findNinja concatenates every .ninja file under the build dir (Python Blade
// splits edges into per-target subninjas; blade-go writes a single file).
func findNinja(t *testing.T, root string) string {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(root, "build*"))
	var sb strings.Builder
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() && strings.HasSuffix(p, ".ninja") {
				if data, rerr := os.ReadFile(p); rerr == nil {
					sb.Write(data)
					sb.WriteByte('\n')
				}
			}
			return nil
		})
	}
	if sb.Len() == 0 {
		t.Fatalf("no ninja files generated under %s", root)
	}
	return sb.String()
}

func TestBuildAppliesCcConfigFlags(t *testing.T) {
	root := writeWorkspace(t, map[string]string{
		"BLADE_ROOT": `
cc_config(
    cxxflags = ['-std=c++17'],
    cppflags = lambda blade: ['-DFOO', '-mx' if blade.cc_toolchain.target_arch == 'nope' else ''],
    extra_incs = ['/opt/x/include'],
)`,
		"p/BUILD": `cc_library(name = 'p', srcs = ['p.cc'])`,
	})
	ninjaFile, err := Build(root, []string{"//p:p"}, Options{RunNinja: false})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(ninjaFile)
	out := string(data)
	for _, want := range []string{
		"cxxflags = -std=c++17",
		"cppflags = -DFOO -I/opt/x/include", // lambda evaluated, '' dropped, extra_incs -> -I
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestBuildExtraIncsUsesWorkspaceBuildDir(t *testing.T) {
	// flare's extra_incs lambda references blade.workspace.build_dir; without it
	// the lambda throws and every extra_incs (incl. the global -Ithirdparty) is
	// silently dropped.
	root := writeWorkspace(t, map[string]string{
		"BLADE_ROOT": `
cc_config(
    extra_incs = lambda blade: ['thirdparty/', '%s/thirdparty/' % blade.workspace.build_dir],
)`,
		"p/BUILD": `cc_library(name = 'p', srcs = ['p.cc'])`,
	})
	ninjaFile, err := Build(root, []string{"//p:p"}, Options{RunNinja: false})
	if err != nil {
		t.Fatal(err)
	}
	out := string(mustRead(t, ninjaFile))
	if !strings.Contains(out, "-Ithirdparty/") || !strings.Contains(out, "-Ibuild64_release/thirdparty/") {
		t.Errorf("extra_incs lambda with blade.workspace.build_dir not applied:\n%s", out)
	}
}

func TestBuildCcTestLinksGtest(t *testing.T) {
	root := writeWorkspace(t, map[string]string{
		"BLADE_ROOT": `
cc_test_config(
    gtest_libs = ['//thirdparty/googletest:gtest', '#pthread'],
    gtest_main_libs = ['//thirdparty/googletest:gtest_main'],
)`,
		"t/BUILD": `cc_test(name = 't', srcs = ['t.cc'])`,
	})
	ninjaFile, err := Build(root, []string{"//t:t"}, Options{RunNinja: false})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(ninjaFile)
	out := string(data)
	// gtest/gtest_main routed to vcpkg (archive path or -lgtest); pthread as syslib.
	for _, want := range []string{"gtest", "gtest_main", "-lpthread"} {
		if !strings.Contains(out, want) {
			t.Errorf("cc_test link missing %q:\n%s", want, out)
		}
	}
}

func TestBuildDefines(t *testing.T) {
	// A target's `defs` become -D flags on its compiles (flare's cc_test size
	// variants: defs = ['BUFFER_BLOCK_SIZE=4096']).
	root := t.TempDir()
	files := map[string]string{
		"BLADE_ROOT": `cc_config()`,
		"p/BUILD":    `cc_binary(name = 'app', srcs = ['m.cc'], defs = ['FOO=1', 'BAR'])`,
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
	ninjaFile, err := Build(root, []string{"//p:app"}, Options{RunNinja: false})
	if err != nil {
		t.Fatal(err)
	}
	out := string(mustRead(t, ninjaFile))
	if !strings.Contains(out, "defs = -DFOO=1 -DBAR") {
		t.Errorf("per-target defs not applied:\n%s", out)
	}
}

func TestClean(t *testing.T) {
	root := t.TempDir()
	bd := filepath.Join(root, "build64_release")
	if err := os.MkdirAll(filepath.Join(bd, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Clean(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bd); !os.IsNotExist(err) {
		t.Errorf("build dir still present after clean: %v", err)
	}
}

func TestQuery(t *testing.T) {
	// app -> lib -> base (base also //thirdparty/x -> vcpkg). Query deps and
	// dependents over the whole-repo graph.
	root := t.TempDir()
	files := map[string]string{
		"BLADE_ROOT": `cc_config()`,
		"base/BUILD": `cc_library(name = 'base', srcs = ['b.cc'], visibility = ['PUBLIC'])`,
		"lib/BUILD":  `cc_library(name = 'lib', srcs = ['l.cc'], deps = ['//base:base'], visibility = ['PUBLIC'])`,
		"app/BUILD":  `cc_binary(name = 'app', srcs = ['m.cc'], deps = ['//lib:lib'])`,
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	// deps of app = lib + base.
	res, err := Query(root, []string{"//app:app"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"//base:base", "//lib:lib"}; !reflect.DeepEqual(res[0].Related, want) {
		t.Errorf("--deps //app:app = %v, want %v", res[0].Related, want)
	}
	// dependents of base = lib + app.
	res, err = Query(root, []string{"//base:base"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"//app:app", "//lib:lib"}; !reflect.DeepEqual(res[0].Related, want) {
		t.Errorf("--dependents //base:base = %v, want %v", res[0].Related, want)
	}
}
