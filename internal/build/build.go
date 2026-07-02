// Package build orchestrates a build: find the workspace, load config + BUILD
// files, resolve the graph, generate ninja, and optionally run ninja.
package build

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.starlark.net/starlark"

	"github.com/blade-build/blade-go/internal/bladectx"
	"github.com/blade-build/blade-go/internal/cc"
	"github.com/blade-build/blade-go/internal/config"
	"github.com/blade-build/blade-go/internal/graph"
	"github.com/blade-build/blade-go/internal/label"
	"github.com/blade-build/blade-go/internal/loader"
	"github.com/blade-build/blade-go/internal/ninja"
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
	RunNinja  bool     // run ninja after generating build.ninja
	NinjaArgs []string // extra ninja flags (e.g. -j N, -k 0, -n) from the CLI
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
func loadGraph(root string, targets []string) (*loader.Loader, *graph.Graph, []string, error) {
	l := loader.New(root)
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
func plan(root string, targets []string) (*graph.Graph, *cc.Generator, *ninja.File, []string, error) {
	tm := newTiming()
	l, g, expanded, err := loadGraph(root, targets)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	tm.mark("load+graph")
	gen := cc.New(toolchain.Detect())
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
		return nil, nil, nil, nil, err
	}
	return g, gen, f, expanded, nil
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

func runNinja(root, buildFile string, extraArgs ...string) error {
	rel, _ := filepath.Rel(root, buildFile)
	cmd := exec.Command("ninja", append([]string{"-f", rel}, extraArgs...)...)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ninja: %w", err)
	}
	return nil
}

// Build loads the workspace, generates the ninja file for the given targets, and
// (optionally) runs ninja. It returns the path of the generated ninja file.
func Build(root string, targets []string, opt Options) (string, error) {
	_, gen, f, _, err := plan(root, targets)
	if err != nil {
		return "", err
	}
	buildFile, err := writeNinja(root, gen, f)
	if err != nil {
		return "", err
	}
	if opt.RunNinja {
		if err := runNinja(root, buildFile, opt.NinjaArgs...); err != nil {
			return buildFile, err
		}
	}
	return buildFile, nil
}

// TestResult is the outcome of running one cc_test target.
type TestResult struct {
	Label  string
	Passed bool
	Output string
}

// Test builds the given targets and runs each that is a cc_test, returning one
// result per test target (in request order).
func Test(root string, targets []string, opt Options) ([]TestResult, error) {
	g, gen, f, expanded, err := plan(root, targets)
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
	var results []TestResult
	for _, r := range expanded {
		lbl, err := label.Parse(r, "")
		if err != nil {
			return nil, err
		}
		node := g.Node(lbl.String())
		if node == nil || node.Target.Type != "cc_test" {
			continue
		}
		out, runErr := exec.Command(filepath.Join(root, gen.BinPath(node))).CombinedOutput()
		results = append(results, TestResult{Label: node.Label(), Passed: runErr == nil, Output: string(out)})
	}
	return results, nil
}

// Run builds a single runnable target (cc_binary or cc_test) and executes it,
// forwarding args and the current stdio; it returns the program's exit code.
func Run(root, target string, args []string, opt Options) (int, error) {
	g, gen, f, _, err := plan(root, []string{target})
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
	cmd := exec.Command(filepath.Join(root, gen.BinPath(node)), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), nil // propagate the program's exit code
		}
		return 1, err
	}
	return 0, nil
}

// Clean removes the build output directory.
func Clean(root string) error {
	return os.RemoveAll(filepath.Join(root, cc.New(toolchain.Detect()).BuildDir))
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
	_, g, _, err := loadGraph(root, []string{"//..."})
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
