// Package build orchestrates a build: find the workspace, load config + BUILD
// files, resolve the graph, generate ninja, and optionally run ninja.
package build

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/blade-build/blade-go/internal/cc"
	"github.com/blade-build/blade-go/internal/config"
	"github.com/blade-build/blade-go/internal/graph"
	"github.com/blade-build/blade-go/internal/loader"
	"github.com/blade-build/blade-go/internal/toolchain"
)

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

// Build loads the workspace, generates the ninja file for the given targets, and
// (optionally) runs ninja. It returns the path of the generated ninja file.
func Build(root string, targets []string, opt Options) (string, error) {
	l := loader.New(root)
	bladeRoot := filepath.Join(root, "BLADE_ROOT")
	if _, err := os.Stat(bladeRoot); err == nil {
		if err := l.LoadConfigFile(bladeRoot); err != nil {
			return "", fmt.Errorf("BLADE_ROOT: %w", err)
		}
	}
	g, err := graph.NewBuilder(l).Build(targets)
	if err != nil {
		return "", err
	}
	gen := cc.New(toolchain.Detect())
	configureProto(gen, l.Config)
	f, err := gen.Generate(g)
	if err != nil {
		return "", err
	}
	buildFile := filepath.Join(root, gen.BuildDir, "build.ninja")
	if err := os.MkdirAll(filepath.Dir(buildFile), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(buildFile, []byte(f.String()), 0o644); err != nil {
		return "", err
	}
	if opt.RunNinja {
		rel, _ := filepath.Rel(root, buildFile)
		cmd := exec.Command("ninja", "-f", rel)
		cmd.Dir = root
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return buildFile, fmt.Errorf("ninja: %w", err)
		}
	}
	return buildFile, nil
}
