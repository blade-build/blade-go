// Package build orchestrates a build: find the workspace, load config + BUILD
// files, resolve the graph, generate ninja, and optionally run ninja.
package build

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"go.starlark.net/starlark"

	"github.com/blade-build/blade-go/internal/bladectx"
	"github.com/blade-build/blade-go/internal/cc"
	"github.com/blade-build/blade-go/internal/ccundef"
	"github.com/blade-build/blade-go/internal/config"
	"github.com/blade-build/blade-go/internal/graph"
	"github.com/blade-build/blade-go/internal/hdrcheck"
	"github.com/blade-build/blade-go/internal/label"
	"github.com/blade-build/blade-go/internal/loader"
	"github.com/blade-build/blade-go/internal/ninja"
	"github.com/blade-build/blade-go/internal/sanitizer"
	"github.com/blade-build/blade-go/internal/toolchain"
)

// configureCcFlags resolves cc_config's compile flags (cppflags/cxxflags/cflags/
// extra_incs) and applies them to the generator. Config values may be
// `lambda blade: [...]` callables (flare's idiom); they are evaluated here with a
// build-phase blade context. Evaluation is best-effort -- a lambda that needs
// something we don't model is skipped rather than failing the build.
func configureCcFlags(gen *cc.Generator, cfg *config.Config) {
	blade := bladectx.BuildModule("", gen.BuildDir, cfg)
	cpp := evalConfigList(cfg, "cc_config", "cppflags", blade)
	for _, d := range evalConfigList(cfg, "cc_config", "extra_incs", blade) {
		cpp = append(cpp, "-I"+d)
	}
	gen.Cppflags = cpp
	gen.Cxxflags = evalConfigList(cfg, "cc_config", "cxxflags", blade)
	gen.Cflags = evalConfigList(cfg, "cc_config", "cflags", blade)

	// warnings apply to all C-family compiles; c_warnings/cxx_warnings add the
	// per-language ones. Kept in a separate ninja var so generated code (protoc /
	// resource) opts out, matching Blade.
	warn := evalConfigList(cfg, "cc_config", "warnings", blade)
	gen.CWarnings = append(append([]string{}, warn...), evalConfigList(cfg, "cc_config", "c_warnings", blade)...)
	gen.CxxWarnings = append(append([]string{}, warn...), evalConfigList(cfg, "cc_config", "cxx_warnings", blade)...)
	gen.Linkflags = evalConfigList(cfg, "cc_config", "linkflags", blade)
	gen.Optimize = evalConfigList(cfg, "cc_config", "optimize", blade)
}

// evalConfigList returns a config list item as []string, evaluating a lambda
// (called with `blade`) if that's how it was written. Empty strings are dropped
// (flare emits `'-flag' if cond else ”`).
func evalConfigList(cfg *config.Config, section, item string, blade starlark.Value) []string {
	v, ok := cfg.GetItem(section, item)
	if !ok {
		return nil
	}
	if callable, ok := v.(starlark.Callable); ok {
		res, err := starlark.Call(&starlark.Thread{Name: "config"}, callable, starlark.Tuple{blade}, nil)
		if err != nil {
			return nil
		}
		return starlarkStrings(res)
	}
	return goStrings(v)
}

func starlarkStrings(v starlark.Value) []string {
	it, ok := v.(starlark.Iterable)
	if !ok {
		return nil
	}
	iter := it.Iterate()
	defer iter.Done()
	var out []string
	var e starlark.Value
	for iter.Next(&e) {
		if s, ok := starlark.AsString(e); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func goStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range list {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// configureTestLibs resolves the framework libs a cc_test / cc_benchmark links
// from config: cc_test_config's gtest_libs / gtest_main_libs for tests, and
// cc_config's benchmark_libs / benchmark_main_libs for benchmarks. Each entry is
// classified -- a "#name" is a syslib, a "thirdparty/<port>:<lib>" a vcpkg lib.
func configureTestLibs(gen *cc.Generator, cfg *config.Config) {
	blade := bladectx.BuildModule("", gen.BuildDir, cfg)
	var gtest []string
	gtest = append(gtest, evalConfigList(cfg, "cc_test_config", "gtest_libs", blade)...)
	gtest = append(gtest, evalConfigList(cfg, "cc_test_config", "gtest_main_libs", blade)...)
	gen.TestVcpkgs, gen.TestSyslibs = classifyFrameworkLibs(gtest)

	var bench []string
	bench = append(bench, evalConfigList(cfg, "cc_config", "benchmark_libs", blade)...)
	bench = append(bench, evalConfigList(cfg, "cc_config", "benchmark_main_libs", blade)...)
	gen.BenchmarkVcpkgs, gen.BenchmarkSyslibs = classifyFrameworkLibs(bench)
}

// classifyFrameworkLibs splits framework lib labels into vcpkg deps
// ("thirdparty/<port>:<lib>") and syslibs ("#name").
func classifyFrameworkLibs(labels []string) (vcpkgs []label.VcpkgDep, syslibs []string) {
	const prefix = "thirdparty/"
	for _, s := range labels {
		if strings.HasPrefix(s, "#") {
			syslibs = append(syslibs, strings.TrimPrefix(s, "#"))
			continue
		}
		lbl, err := label.Parse(s, "")
		if err != nil {
			continue
		}
		if strings.HasPrefix(lbl.Package, prefix) {
			rest := strings.TrimPrefix(lbl.Package, prefix)
			port := rest
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				port = rest[:i]
			}
			vcpkgs = append(vcpkgs, label.VcpkgDep{Port: port, Lib: lbl.Name})
		}
	}
	return vcpkgs, syslibs
}

// configureVcpkg reads BLADE_ROOT's vcpkg_config (baseline + pinned packages)
// and installs those exact ports via vcpkg manifest mode, pointing the resolver
// at the resulting tree. This is what lets blade-go build flare against the same
// thirdparty versions its Python-Blade build uses (fmt 7.1.3, protobuf 3.21.12,
// ...) rather than whatever a bare `vcpkg install <port>` gives today.
//
// Best-effort: with no vcpkg_config, or no vcpkg executable, it leaves the
// env-derived resolver untouched (so fixtures and simple setups are unaffected).
func configureVcpkg(gen *cc.Generator, cfg *config.Config, root string) {
	pkgsV, ok := cfg.GetItem("vcpkg_config", "packages")
	if !ok {
		return
	}
	packages, ok := pkgsV.(map[string]any)
	if !ok || len(packages) == 0 {
		return
	}
	// Ports whose whole archive must be linked (link_all_symbols: gflags/glog/
	// yaml-cpp -- their flag/registry static initializers must survive --gc).
	gen.ForceLoadPorts = map[string]bool{}
	for name, spec := range packages {
		if m, ok := spec.(map[string]any); ok {
			if b, ok := m["link_all_symbols"].(bool); ok && b {
				gen.ForceLoadPorts[name] = true
			}
		}
	}
	baseline, _ := cfg.GetItem("vcpkg_config", "baseline")
	baselineStr, _ := baseline.(string)
	manifestDir := filepath.Join(root, gen.BuildDir, ".blade-go-vcpkg")
	if err := gen.Vcpkg.InstallFromConfig(baselineStr, packages, manifestDir); err != nil {
		fmt.Fprintf(os.Stderr, "blade-go: vcpkg_config install skipped: %v\n", err)
	}
}

// configureProto applies proto_library_config (protoc path, protobuf_libs) to
// the generator. Non-string / lambda values are ignored, keeping the defaults.
func configureProto(gen *cc.Generator, cfg *config.Config) {
	for _, s := range cfg.Named("proto_library_config") {
		if p, ok := s.Attrs["protoc"].(string); ok && p != "" {
			// `vcpkg#<port>` resolves to the protoc the pinned vcpkg tree
			// installs (matching the linked libprotobuf), mirroring blade's own
			// vcpkg# scheme. configureVcpkg has already set InstalledDir.
			if port, ok := strings.CutPrefix(p, "vcpkg#"); ok {
				if tp := gen.Vcpkg.ToolPath(port, "protoc"); tp != "" {
					p = tp
				}
			}
			gen.Protoc = p
		}
		if libs, ok := s.Attrs["protobuf_libs"].([]any); ok {
			var syslibs []string
			var vpkgs []label.VcpkgDep
			for _, l := range libs {
				str, ok := l.(string)
				if !ok {
					continue
				}
				// flare pins protobuf itself in vcpkg ('vcpkg#protobuf:protobuf');
				// route those through the resolver, not as a bare -l syslib.
				if rest, ok := strings.CutPrefix(str, "vcpkg#"); ok {
					port, lib := rest, rest
					if i := strings.IndexByte(rest, ':'); i >= 0 {
						port, lib = rest[:i], rest[i+1:]
					}
					vpkgs = append(vpkgs, label.VcpkgDep{Port: port, Lib: lib})
				} else {
					syslibs = append(syslibs, strings.TrimPrefix(str, "#"))
				}
			}
			// The config is authoritative once protobuf_libs is set: replace the
			// defaults rather than merging (flare doesn't want -lprotobuf on top).
			gen.ProtobufLibs = syslibs
			gen.ProtobufVcpkgs = vpkgs
		}
	}
}

// FindRoot walks up from start to the nearest directory containing BLADE_ROOT.
func FindRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "BLADE_ROOT")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no BLADE_ROOT found at or above %s", start)
		}
		dir = parent
	}
}

// Options controls a build.
type Options struct {
	RunNinja   bool     // run ninja after generating build.ninja
	NinjaArgs  []string // extra ninja flags (e.g. -j N, -k 0, -n) from the CLI
	FullTest   bool     // re-run every test, ignoring the incremental cache
	TestJobs   int      // parallel test workers (0 = CPUs available to the process, cgroup-aware)
	Profile    string   // "release" (default) or "debug"; selects build_<profile> + flags
	DebugInfo  string   // "" (project default) or no|low|mid|high (-g level)
	Sanitizers []string // canonical sanitizer set (nil = off); adds a build-dir tag + flags
	Coverage   bool     // --coverage: instrument for gcov coverage; adds a build-dir tag + flags
}

// buildDirFor is the output directory for a profile + variant set:
// build_<profile>[_coverage][_<sanitizer-tag>] (e.g. build_release_asan,
// build_release_coverage). Each variant is codegen-incompatible with a plain
// build, so it gets its own tree. An empty/unknown profile falls back to release.
func buildDirFor(profile string, sanitizers []string, coverage bool) string {
	dir := "build_release"
	if profile == "debug" {
		dir = "build_debug"
	}
	if coverage { // coverage before the sanitizer tag, matching Blade's variant suffix
		dir += "_coverage"
	}
	if tag := sanitizer.BuildTag(sanitizers); tag != "" {
		dir += "_" + tag
	}
	return dir
}

// normProfile normalizes a profile string, defaulting to release.
func normProfile(profile string) string {
	if profile == "debug" {
		return "debug"
	}
	return "release"
}

// timing reports per-phase front-end durations to stderr when BLADE_TIMING is
// set -- load config, load BUILD files + resolve the graph, generate ninja.
type timing struct {
	on    bool
	start time.Time
	last  time.Time
	marks [][2]any // (name, duration)
}

func newTiming() *timing {
	now := time.Now()
	return &timing{on: os.Getenv("BLADE_TIMING") != "", start: now, last: now}
}

func (t *timing) mark(name string) {
	if !t.on {
		return
	}
	now := time.Now()
	t.marks = append(t.marks, [2]any{name, now.Sub(t.last)})
	t.last = now
}

func (t *timing) report(nodes int) {
	if !t.on {
		return
	}
	fmt.Fprintf(os.Stderr, "blade-go front-end (%d graph nodes):\n", nodes)
	for _, m := range t.marks {
		fmt.Fprintf(os.Stderr, "  %-12s %v\n", m[0], m[1].(time.Duration).Round(time.Millisecond))
	}
	fmt.Fprintf(os.Stderr, "  %-12s %v\n", "TOTAL", time.Since(t.start).Round(time.Millisecond))
}

// loadGraph loads BLADE_ROOT config and resolves the dependency graph for the
// given target patterns (no ninja generation, no vcpkg install). Shared by plan
// (which then generates) and Query (which only inspects the graph).
func loadGraph(root string, targets []string, buildDir string) (*loader.Loader, *graph.Graph, []string, error) {
	l := loader.New(root)
	l.BuildDir = buildDir // the build-dir mirror must match the actual output tree
	bladeRoot := filepath.Join(root, "BLADE_ROOT")
	if _, err := os.Stat(bladeRoot); err == nil {
		if err := l.LoadConfigFile(bladeRoot); err != nil {
			return nil, nil, nil, fmt.Errorf("BLADE_ROOT: %w", err)
		}
	}
	b := graph.NewBuilder(l)
	expanded, err := b.Expand(targets)
	if err != nil {
		return nil, nil, nil, err
	}
	g, err := b.Build(expanded)
	if err != nil {
		return nil, nil, nil, err
	}
	return l, g, expanded, nil
}

// plan loads the workspace and produces the graph, generator, and ninja file for
// the given targets (patterns are expanded to concrete labels, also returned).
func plan(root string, targets []string, profile, debugInfo string, sanitizers []string, coverage bool) (*graph.Graph, *cc.Generator, *ninja.File, []string, *loader.Loader, error) {
	tm := newTiming()
	buildDir := buildDirFor(profile, sanitizers, coverage)
	l, g, expanded, err := loadGraph(root, targets, buildDir)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	tm.mark("load+graph")
	gen := cc.New(toolchain.Detect())
	gen.Root = root
	gen.BuildDir = buildDir
	gen.Profile = normProfile(profile)
	gen.SanitizeCompile = sanitizer.CompileFlags(sanitizers)
	gen.SanitizeLink = sanitizer.LinkFlags(sanitizers)
	if coverage {
		// `--coverage` is the gcc/clang driver flag for both compile
		// (-fprofile-arcs -ftest-coverage) and link (-lgcov); .gcda land next to
		// the objects for gcovr to read after the tests run.
		gen.CoverageFlags = []string{"--coverage"}
	}
	// Debug-info level: --debug-info-level override, else global_config, else the
	// project's own -g flags (empty here).
	gen.DebugInfo = debugInfo
	if gen.DebugInfo == "" {
		if v, ok := l.Config.GetItem("global_config", "debug_info_level"); ok {
			if s, ok := v.(string); ok {
				gen.DebugInfo = s
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		gen.Self = exe // resource_library codegen re-invokes this binary
	}
	configureVcpkg(gen, l.Config, root)
	tm.mark("vcpkg-install") // one-time provisioning (idempotent), not analysis
	configureProto(gen, l.Config)
	configureCcFlags(gen, l.Config)
	configureTestLibs(gen, l.Config)
	f, err := gen.Generate(g)
	tm.mark("generate")
	tm.report(g.Len())
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	return g, gen, f, expanded, l, nil
}

func writeNinja(root string, gen *cc.Generator, f *ninja.File) (string, error) {
	buildFile := filepath.Join(root, gen.BuildDir, "build.ninja")
	if err := os.MkdirAll(filepath.Dir(buildFile), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(buildFile, []byte(f.String()), 0o644); err != nil {
		return "", err
	}
	return buildFile, nil
}

// precreateBuildDirs materializes every build-dir include directory before ninja
// runs. Blade does this implicitly: each target's per-target ninja file creation
// os.makedirs(build_dir/<package>) (build_manager.py), so a globally-configured
// -Ibuild_dir/<pkg> (flare's cc_config.extra_incs lists build_dir/thirdparty) has
// its directory on disk before the first compile. blade-go writes one build.ninja
// and would otherwise rely on ninja's lazy per-edge mkdir -- which races the -I:
// gcc's -Wmissing-include-dirs (under flare's -Werror) errors on a not-yet-created
// -I dir, and the first compile can precede the edge that would create it. (clang
// silently ignores missing -I dirs, so this only bites on gcc/Linux.) We create:
//   - build_dir/<package> for every target (matches Blade's per-target makedirs),
//   - any build-dir-rooted -I from the global cc_config flags (e.g. the
//     build_dir/thirdparty mirror), so it exists even with no target under it.
func precreateBuildDirs(root string, gen *cc.Generator, g *graph.Graph) error {
	seen := map[string]bool{}
	mk := func(rel string) error {
		if rel == "" || seen[rel] {
			return nil
		}
		seen[rel] = true
		return os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755)
	}
	for _, n := range g.All() {
		if err := mk(path.Join(gen.BuildDir, n.Target.Package)); err != nil {
			return err
		}
	}
	for _, fl := range gen.Cppflags {
		d, ok := strings.CutPrefix(fl, "-I")
		if !ok {
			continue
		}
		d = path.Clean(d)
		if d == gen.BuildDir || strings.HasPrefix(d, gen.BuildDir+"/") {
			if err := mk(d); err != nil {
				return err
			}
		}
	}
	return nil
}

func runNinja(root, buildFile string, extraArgs ...string) error {
	rel, _ := filepath.Rel(root, buildFile)
	cmd := exec.Command("ninja", append([]string{"-f", rel}, extraArgs...)...)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// On MSVC, hand ninja the captured VS developer environment (INCLUDE/LIB/PATH/
	// VSLANG) so cl/lib/link resolve their headers, libraries, and helper tools
	// without a vcvars prompt. Appended last so it wins over the inherited env.
	// Empty (gcc/clang) leaves the environment untouched.
	if env := toolchain.Detect().Env; len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ninja: %w", err)
	}
	return nil
}

// Build loads the workspace, generates the ninja file for the given targets, and
// (optionally) runs ninja. It returns the path of the generated ninja file.
func Build(root string, targets []string, opt Options) (string, error) {
	g, gen, f, _, _, err := plan(root, targets, opt.Profile, opt.DebugInfo, opt.Sanitizers, opt.Coverage)
	if err != nil {
		return "", err
	}
	buildFile, err := writeNinja(root, gen, f)
	if err != nil {
		return "", err
	}
	if err := precreateBuildDirs(root, gen, g); err != nil {
		return "", err
	}
	if opt.RunNinja {
		if err := runNinja(root, buildFile, opt.NinjaArgs...); err != nil {
			return buildFile, err
		}
	}
	return buildFile, nil
}

// CheckHdrs runs the header inclusion-dependency check over the requested
// targets (whose header closures must already exist in ninja's dep log from a
// build). Ownership is resolved across the whole loaded graph, but only the
// requested targets' closure -- expanded from patterns like //pkg/... -- is
// checked. The effective severity is `override` if non-empty, else the project's
// cc_config.hdr_dep_missing_severity (Blade parity), else Warn. Returns the
// sorted issues and the effective severity so the caller can report and decide
// whether to fail.
func CheckHdrs(root string, targets []string, override, profile string, sanitizers []string, coverage bool) ([]hdrcheck.Issue, error) {
	bd := buildDirFor(profile, sanitizers, coverage)
	l, g, expanded, err := loadGraph(root, targets, bd)
	if err != nil {
		return nil, err
	}
	// Inclusion check severity: --hdr-check override, else cc_config, else warn.
	sev := hdrcheck.Warn
	if override != "" {
		s, ok := hdrcheck.ParseSeverity(override)
		if !ok {
			return nil, fmt.Errorf("invalid --hdr-check value %q (want off|warn|error)", override)
		}
		sev = s
	} else if v, ok := l.Config.GetItem("cc_config", "hdr_dep_missing_severity"); ok {
		if s, ok := v.(string); ok {
			sev = hdrcheck.SeverityFromBlade(s)
		}
	}
	// Unused-deps severity: cc_config only (Blade default 'debug' == Off). An
	// explicit --hdr-check=off disables both checks.
	unusedSev := hdrcheck.Off
	if override != "off" {
		if v, ok := l.Config.GetItem("cc_config", "unused_deps_severity"); ok {
			if s, ok := v.(string); ok {
				unusedSev = hdrcheck.SeverityFromBlade(s)
			}
		}
	}
	if sev == hdrcheck.Off && unusedSev == hdrcheck.Off {
		return nil, nil
	}
	only := make(map[string]bool, len(expanded))
	for _, lbl := range expanded {
		only[lbl] = true
	}
	// Extra -I roots (cc_config.extra_incs) + their build-dir mirror, so a header
	// included via one of them (flare's `#include "blake3/blake3.h"` under
	// `-Ithirdparty`) resolves to its owning target's full workspace-relative hdr.
	var incDirs []string
	blade := bladectx.BuildModule("", bd, l.Config)
	for _, d := range evalConfigList(l.Config, "cc_config", "extra_incs", blade) {
		incDirs = append(incDirs, d, bd+"/"+d)
	}
	// Headers the project allows to be included without a declared dep.
	var allowUndec map[string]bool
	if hs := evalConfigList(l.Config, "cc_config", "allowed_undeclared_hdrs", blade); len(hs) > 0 {
		allowUndec = make(map[string]bool, len(hs))
		for _, h := range hs {
			allowUndec[h] = true
		}
	}
	tc := toolchain.Detect()
	issues := hdrcheck.Check(g.All(), hdrcheck.Options{
		Root:           root,
		BuildDir:       bd,
		ObjSuffix:      tc.ObjSuffix(),
		Severity:       sev,
		UnusedSeverity: unusedSev,
		IncludeDirs:    incDirs,
		AllowUndec:     allowUndec,
		Only:           only,
	})
	return issues, nil
}

// CheckUndefined runs the static undefined-symbol check over the requested
// cc_library targets (their archives must already be built): every undefined
// external symbol must be resolvable from the library's own archive, a declared
// dep's archive, the system libraries, or an allow/baseline regex. `override` is
// "true" (--cc-check-undefined), "false" (--no-cc-check-undefined), or "" (use
// cc_library_config). Returns the issues (sorted); caller reports + decides fail.
func CheckUndefined(root string, targets []string, override, profile string, sanitizers []string, coverage bool) ([]ccundef.Issue, error) {
	g, gen, _, expanded, l, err := plan(root, targets, profile, "", sanitizers, coverage)
	if err != nil {
		return nil, err
	}
	// Enabled = cc_library_config.check_undefined (default true), overridable.
	enabled := true
	if v, ok := l.Config.GetItem("cc_library_config", "check_undefined"); ok {
		if b, ok := v.(bool); ok {
			enabled = b
		}
	}
	switch override {
	case "true":
		enabled = true
	case "false":
		enabled = false
	}
	if !enabled {
		return nil, nil
	}
	sev := ccundef.Warn // cc_library_config default severity is 'warning'
	if v, ok := l.Config.GetItem("cc_library_config", "check_undefined_severity"); ok {
		if s, ok := v.(string); ok {
			sev = ccundef.SeverityFromBlade(s)
		}
	}
	if override == "true" && sev == ccundef.Off {
		sev = ccundef.Warn // forced on: don't let a 'debug' config silence it
	}
	if sev == ccundef.Off {
		return nil, nil
	}

	tc := toolchain.Detect()
	system := ccundef.SystemSymbols(tc.CC, tc.OS, "nm")
	blade := bladectx.BuildModule("", gen.BuildDir, l.Config)
	allowGlobal := evalConfigList(l.Config, "cc_library_config", "allow_undefined", blade)

	only := make(map[string]bool, len(expanded))
	for _, lbl := range expanded {
		only[lbl] = true
	}
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(root, p)
	}
	definedCache := map[string]map[string]bool{}
	getDefined := func(archive string) map[string]bool {
		if d, ok := definedCache[archive]; ok {
			return d
		}
		_, d := ccundef.NmExternals("nm", abs(archive))
		definedCache[archive] = d
		return d
	}

	var issues []ccundef.Issue
	for _, n := range g.All() {
		if n.Target.Type != "cc_library" || !only[n.Label()] {
			continue
		}
		// Per-target allow_undefined: a bare True exempts the whole target; a list
		// adds patterns on top of the global allowlist.
		if b, ok := n.Target.Attrs["allow_undefined"].(bool); ok && b {
			continue
		}
		allow := append(append([]string{}, allowGlobal...), n.Target.AttrStrings("allow_undefined")...)
		own, deps := gen.LinkArchives(n)
		if own == "" {
			continue // header-only / no archive
		}
		undef, ownDef := ccundef.NmExternals("nm", abs(own))
		depDefined := map[string]bool{}
		for _, d := range deps {
			for s := range getDefined(d) {
				depDefined[s] = true
			}
		}
		unresolved := ccundef.Unresolved(undef, ownDef, depDefined, system, ccundef.CompileAllow(allow))
		if len(unresolved) > 0 {
			sort.Strings(unresolved)
			issues = append(issues, ccundef.Issue{
				Target: n.Label(), TargetPos: relTo(root, n.Target.Pos), Symbols: unresolved, Sev: sev,
			})
		}
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Target < issues[j].Target })
	return issues, nil
}

// relTo strips a leading root prefix for a workspace-relative, clickable path.
func relTo(root, loc string) string {
	if root != "" && strings.HasPrefix(loc, root+"/") {
		return loc[len(root)+1:]
	}
	return loc
}

// DefaultJobs is the default parallelism for both building and testing when the
// user gives no explicit count: the number of CPUs effectively available to this
// process. runtime.GOMAXPROCS(0) is used rather than runtime.NumCPU() because on
// Go 1.25+ GOMAXPROCS defaults to the cgroup CPU limit on Linux -- so a container
// capped at N CPUs on a many-core host gets N, not the host count.
//
// This matters most for the build: ninja's own default (`sched_getaffinity` + 2)
// sees a cpuset limit but NOT a CFS quota (`docker --cpus=N`, the common case),
// so it would launch host-many parallel compilers in such a container and risk
// OOM. Passing DefaultJobs() as the default -j closes that gap. GOMAXPROCS covers
// both cpuset and CFS quota. It is the logical (hyperthread) count, not physical;
// physical-core detection needs per-OS probing / a dependency and isn't worth it.
//
// Build and test parallelism stay independent (separate flags / code paths): a
// distributed build can crank `-j` far past local cores without touching test
// concurrency, which must stay bounded by the local machine.
func DefaultJobs() int {
	if n := runtime.GOMAXPROCS(0); n > 0 {
		return n
	}
	return 1
}

// CompileDB generates a clang-style compilation database (compile_commands.json)
// for the requested targets. It plans + writes the ninja file, then asks ninja
// itself (`ninja -t compdb`) to emit the JSON for the compile rules -- reusing
// the exact commands ninja would run, so the database always matches the build.
func CompileDB(root string, targets []string, profile string, sanitizers []string, coverage bool) ([]byte, error) {
	_, gen, f, _, _, err := plan(root, targets, profile, "", sanitizers, coverage)
	if err != nil {
		return nil, err
	}
	buildFile, err := writeNinja(root, gen, f)
	if err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(root, buildFile)
	// Only the compile rules belong in a compilation database (not ar/link/gen/
	// protoc). These are the rule names emitted by the cc generator.
	cmd := exec.Command("ninja", "-f", rel, "-t", "compdb", "cc", "cxx", "cxx_header")
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ninja -t compdb: %w", err)
	}
	return out, nil
}

// CoverageReport runs gcovr over a coverage build's .gcno/.gcda to print a
// C/C++ coverage summary, honoring coverage_config.exclude. Best-effort: it
// warns (never fails) when gcovr is unavailable, since the raw coverage data is
// still on disk. Clang's gcov data is read via `llvm-cov gcov`.
func CoverageReport(root, profile string, sanitizers []string) error {
	if _, err := exec.LookPath("gcovr"); err != nil {
		fmt.Fprintln(os.Stderr, "blade: --coverage: gcovr not found; skipping report (raw .gcda are in the build dir)")
		return nil
	}
	buildDir := buildDirFor(profile, sanitizers, true)
	// coverage_config.exclude: globstar patterns of files to drop from the report.
	var excludes []string
	l := loader.New(root)
	l.BuildDir = buildDir
	if _, err := os.Stat(filepath.Join(root, "BLADE_ROOT")); err == nil {
		if err := l.LoadConfigFile(filepath.Join(root, "BLADE_ROOT")); err == nil {
			blade := bladectx.BuildModule("", buildDir, l.Config)
			excludes = evalConfigList(l.Config, "coverage_config", "exclude", blade)
		}
	}
	args := []string{"-r", root, buildDir}
	tc := toolchain.Detect()
	if ccIsClang(tc.CXX) {
		// clang's gcov data is read via `llvm-cov gcov`. On macOS the default
		// compiler is Apple clang; its gcov data must be read by the *matching*
		// llvm-cov, reachable via xcrun -- a homebrew llvm-cov on PATH is often a
		// different LLVM version that mis-parses Apple's output. Prefer xcrun there;
		// elsewhere use llvm-cov from PATH.
		gcovExec := "llvm-cov gcov"
		if tc.OS == "darwin" {
			if _, err := exec.LookPath("xcrun"); err == nil {
				gcovExec = "xcrun llvm-cov gcov"
			}
		}
		// llvm-cov's gcov output drifts from gcovr's parser (gcc bug 68080): it
		// emits absurd "suspicious" hit counts. Raise the threshold above them so
		// they aren't flagged (a clean exit), and warn-not-fail on negative hits.
		args = append(args, "--gcov-executable", gcovExec,
			"--gcov-suspicious-hits-threshold=999999999999999",
			"--gcov-ignore-parse-errors=negative_hits.warn_once_per_file")
	}
	for _, e := range excludes {
		args = append(args, "-e", globToRegex(e))
	}
	cmd := exec.Command("gcovr", args...)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "blade: --coverage: gcovr failed: %v\n", err)
	}
	return nil
}

// ccIsClang reports whether the C++ compiler is clang -- robust to it being
// invoked under a generic name (macOS `c++`/`cc` are Apple clang). Falls back to
// `<cxx> --version` when the path name doesn't already say "clang".
func ccIsClang(cxx string) bool {
	if strings.Contains(cxx, "clang") {
		return true
	}
	out, err := exec.Command(cxx, "--version").Output()
	return err == nil && strings.Contains(strings.ToLower(string(out)), "clang")
}

// globToRegex converts a coverage-exclude globstar pattern to a gcovr regex:
// ** -> .* (across dirs), * -> [^/]*, ? -> ., and dots are escaped.
func globToRegex(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		switch c := g[i]; c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '.':
			b.WriteString(`\.`)
		case '?':
			b.WriteByte('.')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// TestResult is the outcome of running one cc_test target.
type TestResult struct {
	Label  string
	Passed bool
	Output string
	Cached bool // reported without re-running: passed last time, inputs unchanged
}

// Test builds the given targets and runs each that is a cc_test, returning one
// result per test target (in request order). onResult, if non-nil, is called as
// each test finishes -- so a caller can stream progress instead of waiting for
// the whole (possibly slow) suite, which otherwise looks hung.
//
// Incremental: a test that passed last time and whose inputs (its binary and
// testdata) are unchanged is reported cached without re-running, unless
// opt.FullTest. (Environment variables are deliberately not part of the
// fingerprint -- blade-go doesn't set test env yet; revisit if it ever does.)
func Test(root string, targets []string, opt Options, onResult func(TestResult)) ([]TestResult, error) {
	g, gen, f, expanded, _, err := plan(root, targets, opt.Profile, opt.DebugInfo, opt.Sanitizers, opt.Coverage)
	if err != nil {
		return nil, err
	}
	buildFile, err := writeNinja(root, gen, f)
	if err != nil {
		return nil, err
	}
	// Keep going past a failed build so every runnable test still gets built and
	// run -- one target that won't compile shouldn't hide the rest of a sweep.
	// (Errors are surfaced; a test whose binary is missing is reported failed.)
	runNinja(root, buildFile, append([]string{"-k", "0"}, opt.NinjaArgs...)...)
	hist := loadTestHistory(root, gen.BuildDir)

	// Split into the normal tests (run concurrently) and the exclusive ones
	// (`exclusive = True`: CPU-heavy / timing-sensitive -- run serially and never
	// alongside others, or they go flaky). Non-cc_test targets are ignored.
	var normal, exclusive []*graph.Node
	for _, r := range expanded {
		lbl, err := label.Parse(r, "")
		if err != nil {
			return nil, err
		}
		node := g.Node(lbl.String())
		if node == nil || node.Target.Type != "cc_test" {
			continue
		}
		if node.Target.AttrBool("exclusive") {
			exclusive = append(exclusive, node)
		} else {
			normal = append(normal, node)
		}
	}

	// Sanitizer *_OPTIONS defaults so a detection reliably fails the test -- but
	// only for keys the user hasn't already set in the environment.
	var sanExtra []string
	for k, v := range sanitizer.RuntimeEnv(opt.Sanitizers) {
		if _, set := os.LookupEnv(k); !set {
			sanExtra = append(sanExtra, k+"="+v)
		}
	}

	var mu sync.Mutex // guards results, hist, and onResult ordering
	var results []TestResult
	runOne := func(node *graph.Node) {
		binRel := gen.BinPath(node)
		fp := testFingerprint(root, binRel, node) // stat-only, safe concurrently

		mu.Lock()
		rec, ok := hist[node.Label()]
		cached := !opt.FullTest && ok && rec.Passed && rec.Fingerprint == fp
		mu.Unlock()
		if cached {
			res := TestResult{Label: node.Label(), Passed: true, Cached: true}
			mu.Lock()
			if onResult != nil {
				onResult(res)
			}
			results = append(results, res)
			mu.Unlock()
			return
		}

		// Each test gets its own runfiles dir (unique path -> concurrency-safe
		// without a lock), so relative I/O like ./dump.txt doesn't collide.
		runDir := prepareRunDir(root, node, binRel)
		cmd := exec.Command(filepath.Join(root, binRel))
		if runDir != "" {
			cmd.Dir = runDir // isolated cwd + staged testdata
		}
		if len(sanExtra) > 0 {
			cmd.Env = append(os.Environ(), sanExtra...)
		}
		out, runErr := cmd.CombinedOutput() // the slow part -- runs unlocked
		res := TestResult{Label: node.Label(), Passed: runErr == nil, Output: string(out)}
		mu.Lock()
		hist[node.Label()] = testRecord{Passed: res.Passed, Fingerprint: fp}
		if onResult != nil {
			onResult(res)
		}
		results = append(results, res)
		mu.Unlock()
	}

	jobs := opt.TestJobs
	if jobs <= 0 {
		jobs = DefaultJobs()
	}
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for _, node := range normal {
		wg.Add(1)
		sem <- struct{}{}
		go func(n *graph.Node) {
			defer wg.Done()
			defer func() { <-sem }()
			runOne(n)
		}(node)
	}
	wg.Wait()
	// Exclusive tests run one at a time, after the concurrent batch drained.
	for _, node := range exclusive {
		runOne(node)
	}

	saveTestHistory(root, gen.BuildDir, hist)
	return results, nil
}

// testRecord is one test's last outcome + input fingerprint, persisted across
// runs for incremental testing.
type testRecord struct {
	Passed      bool   `json:"passed"`
	Fingerprint string `json:"fp"`
}

type testHistory map[string]testRecord

func testHistoryPath(root, buildDir string) string {
	return filepath.Join(root, buildDir, ".blade-go-test-history.json")
}

func loadTestHistory(root, buildDir string) testHistory {
	h := testHistory{}
	if b, err := os.ReadFile(testHistoryPath(root, buildDir)); err == nil {
		_ = json.Unmarshal(b, &h)
	}
	return h
}

func saveTestHistory(root, buildDir string, h testHistory) {
	if b, err := json.MarshalIndent(h, "", "  "); err == nil {
		_ = os.WriteFile(testHistoryPath(root, buildDir), b, 0o644)
	}
}

// testFingerprint hashes a test's inputs: its binary (mtime+size) and every
// testdata file (path+mtime+size). Changing a source recompiles the binary (new
// mtime) and editing testdata changes its stat -- either invalidates the cache.
func testFingerprint(root, binRel string, n *graph.Node) string {
	h := md5.New()
	if st, err := os.Stat(filepath.Join(root, binRel)); err == nil {
		fmt.Fprintf(h, "bin\x00%d\x00%d\n", st.ModTime().UnixNano(), st.Size())
	}
	for _, e := range testdataEntries(root, n) {
		_ = filepath.WalkDir(e.srcPath, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if info, e := d.Info(); e == nil {
				fmt.Fprintf(h, "td\x00%s\x00%d\x00%d\n", p, info.ModTime().UnixNano(), info.Size())
			}
			return nil
		})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Run builds a single runnable target (cc_binary or cc_test) and executes it,
// forwarding args and the current stdio; it returns the program's exit code.
func Run(root, target string, args []string, opt Options) (int, error) {
	g, gen, f, _, _, err := plan(root, []string{target}, opt.Profile, opt.DebugInfo, opt.Sanitizers, opt.Coverage)
	if err != nil {
		return 1, err
	}
	buildFile, err := writeNinja(root, gen, f)
	if err != nil {
		return 1, err
	}
	if err := runNinja(root, buildFile, opt.NinjaArgs...); err != nil {
		return 1, err
	}
	lbl, err := label.Parse(target, "")
	if err != nil {
		return 1, err
	}
	node := g.Node(lbl.String())
	if node == nil {
		return 1, fmt.Errorf("no such target %s", target)
	}
	if t := node.Target.Type; t != "cc_binary" && t != "cc_test" {
		return 1, fmt.Errorf("%s is a %s, not a runnable binary", target, t)
	}
	binRel := gen.BinPath(node)
	cmd := exec.Command(filepath.Join(root, binRel), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if dir := prepareRunDir(root, node, binRel); dir != "" {
		cmd.Dir = dir
	}
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), nil // propagate the program's exit code
		}
		return 1, err
	}
	return 0, nil
}

// stageTestdata materializes a cc_test's `testdata` next to the test binary and
// returns that dir (the test's runtime cwd), or "" if the target has no
// testdata. Each entry is a path staged at the same relative name, or a
// (src, dst) tuple staging src as dst -- e.g. ('testdata', 'conf') makes the
// package's testdata/ dir available as conf/ so the test reads "conf/x.yaml".
// blade runs tests from this dir; mirroring that lets data-driven tests find
// their files.
// prepareRunDir gives a test its OWN runfiles directory and stages its testdata
// there; the test runs with this dir as cwd. This isolates relative-path I/O --
// e.g. flare's binlog tests all write "./dump.txt" -- so parallel siblings don't
// clobber each other. Mirrors blade, which runs each test from a per-target
// runfiles dir. Returns "" only if the dir can't be created (fall back to no cwd
// change).
func prepareRunDir(root string, n *graph.Node, binRel string) string {
	runDir := filepath.Join(root, binRel+".runfiles") // build_dir/<pkg>/<name>.runfiles
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return ""
	}
	for _, e := range testdataEntries(root, n) {
		dstPath := filepath.Join(runDir, filepath.FromSlash(e.dst))
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			continue
		}
		os.RemoveAll(dstPath)              // replace any stale staging
		_ = os.Symlink(e.srcPath, dstPath) // absolute symlink to the source data
	}
	return runDir
}

type testdataEntry struct {
	srcPath string // absolute source path
	dst     string // runtime-relative destination
}

// testdataEntries resolves a target's `testdata` attribute to (source, dest)
// pairs. Each entry is a path (staged at the same name) or a (src, dst) tuple.
func testdataEntries(root string, n *graph.Node) []testdataEntry {
	raw, ok := n.Target.Attrs["testdata"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	pkg := filepath.FromSlash(n.Target.Package)
	var out []testdataEntry
	for _, e := range raw {
		src, dst := "", ""
		switch v := e.(type) {
		case string:
			src, dst = v, v
		case []any:
			if len(v) >= 1 {
				src, _ = v[0].(string)
				dst = src
			}
			if len(v) >= 2 {
				dst, _ = v[1].(string)
			}
		}
		if src == "" || strings.Contains(src, "..") {
			continue
		}
		var srcPath string
		if rest, ok := strings.CutPrefix(src, "//"); ok {
			srcPath = filepath.Join(root, filepath.FromSlash(rest))
		} else {
			srcPath = filepath.Join(root, pkg, filepath.FromSlash(src))
		}
		out = append(out, testdataEntry{srcPath: srcPath, dst: dst})
	}
	return out
}

// Clean removes the build output directory.
// Clean removes the build outputs of the requested targets (their closure) via
// `ninja -t clean`, leaving the ninja file and -- crucially -- the vcpkg tree in
// place, so a subsequent build doesn't re-provision thirdparty deps. This mirrors
// Python Blade's clean (per-target output removal), not a `rm -rf` of the whole
// build dir. targets accepts the same patterns as build (e.g. //... for all).
//
// The ninja graph is regenerated for the requested targets so `ninja -t clean`
// removes exactly their outputs. If nothing has been built yet (no build dir),
// it is a no-op.
func Clean(root string, targets []string, profile string, sanitizers []string, coverage bool) error {
	if _, err := os.Stat(filepath.Join(root, buildDirFor(profile, sanitizers, coverage))); os.IsNotExist(err) {
		return nil // nothing built, nothing to clean
	}
	_, gen, f, _, _, err := plan(root, targets, profile, "", sanitizers, coverage)
	if err != nil {
		return err
	}
	buildFile, err := writeNinja(root, gen, f)
	if err != nil {
		return err
	}
	return runNinja(root, buildFile, "-t", "clean")
}

// QueryResult is one queried target and its related targets (sorted).
type QueryResult struct {
	Target  string
	Related []string
}

// Query answers dependency questions over the whole-repo graph: for each target,
// its transitive dependencies (dependents=false) or transitive dependents
// (dependents=true, the reverse closure).
func Query(root string, targets []string, dependents bool) ([]QueryResult, error) {
	_, g, _, err := loadGraph(root, []string{"//..."}, buildDirFor("release", nil, false)) // deps are profile-independent
	if err != nil {
		return nil, err
	}
	var reverse map[*graph.Node][]*graph.Node
	if dependents {
		reverse = map[*graph.Node][]*graph.Node{}
		for _, n := range g.All() {
			for _, d := range n.Deps {
				reverse[d] = append(reverse[d], n)
			}
		}
	}
	var out []QueryResult
	for _, t := range targets {
		lbl, err := label.Parse(t, "")
		if err != nil {
			return nil, err
		}
		node := g.Node(lbl.String())
		if node == nil {
			return nil, fmt.Errorf("no such target %s", t)
		}
		var related []string
		seen := map[*graph.Node]bool{}
		var walk func(*graph.Node)
		walk = func(n *graph.Node) {
			next := n.Deps
			if dependents {
				next = reverse[n]
			}
			for _, d := range next {
				if !seen[d] {
					seen[d] = true
					related = append(related, d.Label())
					walk(d)
				}
			}
		}
		walk(node)
		sort.Strings(related)
		out = append(out, QueryResult{Target: node.Label(), Related: related})
	}
	return out, nil
}
