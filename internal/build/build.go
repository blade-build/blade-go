// Package build orchestrates a build: find the workspace, load config + BUILD
// files, resolve the graph, generate ninja, and optionally run ninja.
package build

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

// configureProto applies proto_library_config (protoc path, protobuf_libs) to
// the generator. Non-string / lambda values are ignored, keeping the defaults.
func configureProto(gen *cc.Generator, cfg *config.Config) {
	for _, s := range cfg.Named("proto_library_config") {
		if p, ok := s.Attrs["protoc"].(string); ok && p != "" {
			gen.Protoc = p
		}
		if libs, ok := s.Attrs["protobuf_libs"].([]any); ok {
			var names []string
			for _, l := range libs {
				if str, ok := l.(string); ok {
					names = append(names, strings.TrimPrefix(str, "#"))
				}
			}
			if len(names) > 0 {
				gen.ProtobufLibs = names
			}
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
	RunNinja bool
}

// plan loads the workspace and produces the graph, generator, and ninja file for
// the given targets.
func plan(root string, targets []string) (*graph.Graph, *cc.Generator, *ninja.File, error) {
	l := loader.New(root)
	bladeRoot := filepath.Join(root, "BLADE_ROOT")
	if _, err := os.Stat(bladeRoot); err == nil {
		if err := l.LoadConfigFile(bladeRoot); err != nil {
			return nil, nil, nil, fmt.Errorf("BLADE_ROOT: %w", err)
		}
	}
	g, err := graph.NewBuilder(l).Build(targets)
	if err != nil {
		return nil, nil, nil, err
	}
	gen := cc.New(toolchain.Detect())
	configureProto(gen, l.Config)
	configureCcFlags(gen, l.Config)
	f, err := gen.Generate(g)
	if err != nil {
		return nil, nil, nil, err
	}
	return g, gen, f, nil
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
	_, gen, f, err := plan(root, targets)
	if err != nil {
		return "", err
	}
	buildFile, err := writeNinja(root, gen, f)
	if err != nil {
		return "", err
	}
	if opt.RunNinja {
		if err := runNinja(root, buildFile); err != nil {
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
func Test(root string, targets []string) ([]TestResult, error) {
	g, gen, f, err := plan(root, targets)
	if err != nil {
		return nil, err
	}
	buildFile, err := writeNinja(root, gen, f)
	if err != nil {
		return nil, err
	}
	if err := runNinja(root, buildFile); err != nil {
		return nil, err
	}
	var results []TestResult
	for _, r := range targets {
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
