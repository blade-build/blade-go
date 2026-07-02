// Package vcpkg resolves `vcpkg#<port>:<lib>` dependencies to include and link
// flags from an installed vcpkg tree.
//
// This is how blade-go routes thirdparty libraries (flare's 26 deps). When no
// vcpkg tree is configured it degrades to a plain `-l<lib>` system-library link,
// which keeps fixtures and simple setups working.
package vcpkg

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
)

// Resolver locates headers and archives in a vcpkg installation.
type Resolver struct {
	Root         string // VCPKG_ROOT
	Triplet      string // e.g. "x64-linux"
	InstalledDir string // overrides Root/installed/Triplet (manifest-mode install)
}

// FromEnv builds a Resolver from $VCPKG_ROOT and $VCPKG_DEFAULT_TRIPLET (falling
// back to a host-derived triplet).
func FromEnv() *Resolver {
	triplet := os.Getenv("VCPKG_DEFAULT_TRIPLET")
	if triplet == "" {
		triplet = defaultTriplet()
	}
	return &Resolver{Root: os.Getenv("VCPKG_ROOT"), Triplet: triplet}
}

func defaultTriplet() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "x64-linux"
	case "linux/arm64":
		return "arm64-linux"
	case "darwin/amd64":
		return "x64-osx"
	case "darwin/arm64":
		return "arm64-osx"
	case "windows/amd64":
		return "x64-windows"
	default:
		return "x64-linux"
	}
}

// Configured reports whether a vcpkg tree is available.
func (r *Resolver) Configured() bool { return r != nil && (r.Root != "" || r.InstalledDir != "") }

func (r *Resolver) installed() string {
	if r.InstalledDir != "" {
		return r.InstalledDir
	}
	return filepath.Join(r.Root, "installed", r.Triplet)
}

// IncludeDir returns the vcpkg include directory (empty when unconfigured).
func (r *Resolver) IncludeDir() string {
	if !r.Configured() {
		return ""
	}
	return filepath.Join(r.installed(), "include")
}

// LibArg returns the linker argument for a library: the static archive path if
// it exists in the vcpkg tree, otherwise a plain `-l<lib>`.
func (r *Resolver) LibArg(lib string) string {
	if r.Configured() {
		archive := filepath.Join(r.installed(), "lib", "lib"+lib+".a")
		if _, err := os.Stat(archive); err == nil {
			return archive
		}
	}
	return "-l" + lib
}

// ManifestJSON turns a BLADE_ROOT vcpkg_config into a vcpkg.json manifest so the
// exact same ports/versions flare pins are installed. `baseline` is the vcpkg
// builtin baseline commit; `packages` maps a port name to either a version
// string ('fmt': '7.1.3') or a dict ({'version': ..., 'features': [...]}).
//
// Ports with an explicit version become `overrides` entries (pinning the exact
// version against the baseline's version database); ports with `features` become
// object dependencies. Blade-specific keys (link_all_symbols, include_prefix,
// cmake_options, linkage) are not part of vcpkg.json and are ignored here -- they
// affect how a port is built/linked, which is a separate concern from installing
// the right version.
func ManifestJSON(baseline string, packages map[string]any) ([]byte, error) {
	names := make([]string, 0, len(packages))
	for name := range packages {
		names = append(names, name)
	}
	sort.Strings(names)

	type override struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	type featureDep struct {
		Name     string   `json:"name"`
		Features []string `json:"features"`
	}
	deps := make([]any, 0, len(names))
	var overrides []override
	for _, name := range names {
		version, features := parsePackage(packages[name])
		if len(features) > 0 {
			deps = append(deps, featureDep{Name: name, Features: features})
		} else {
			deps = append(deps, name)
		}
		if version != "" {
			overrides = append(overrides, override{Name: name, Version: version})
		}
	}

	m := map[string]any{"dependencies": deps}
	if baseline != "" {
		m["builtin-baseline"] = baseline
	}
	if len(overrides) > 0 {
		m["overrides"] = overrides
	}
	return json.MarshalIndent(m, "", "  ")
}

func parsePackage(v any) (version string, features []string) {
	switch spec := v.(type) {
	case string:
		return spec, nil
	case map[string]any:
		if s, ok := spec["version"].(string); ok {
			version = s
		}
		if fs, ok := spec["features"].([]any); ok {
			for _, f := range fs {
				if s, ok := f.(string); ok {
					features = append(features, s)
				}
			}
		}
	}
	return version, features
}

// FindExe locates the vcpkg executable: $VCPKG_ROOT/vcpkg if present, else
// `vcpkg` on PATH (mirroring blade's own resolution -- CI relies on the runner's
// preinstalled vcpkg). Returns "" when none is found.
func (r *Resolver) FindExe() string {
	if r != nil && r.Root != "" {
		exe := filepath.Join(r.Root, "vcpkg")
		if _, err := os.Stat(exe); err == nil {
			return exe
		}
	}
	if p, err := exec.LookPath("vcpkg"); err == nil {
		return p
	}
	return ""
}

// InstallFromConfig writes a vcpkg.json under manifestDir from the vcpkg_config
// and runs vcpkg in manifest mode, installing the pinned ports into
// manifestDir/vcpkg_installed. On success it points the Resolver at that tree
// (via InstalledDir) so headers/archives resolve to the flare-pinned versions.
//
// It is idempotent: vcpkg skips ports already installed for the manifest.
func (r *Resolver) InstallFromConfig(baseline string, packages map[string]any, manifestDir string) error {
	exe := r.FindExe()
	if exe == "" {
		return fmt.Errorf("vcpkg executable not found (set VCPKG_ROOT or put vcpkg on PATH)")
	}
	manifest, err := ManifestJSON(baseline, packages)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "vcpkg.json"), manifest, 0o644); err != nil {
		return err
	}
	installRoot := filepath.Join(manifestDir, "vcpkg_installed")
	cmd := exec.Command(exe, "install",
		"--triplet="+r.Triplet,
		"--x-manifest-root="+manifestDir,
		"--x-install-root="+installRoot)
	cmd.Stdout = os.Stderr // progress on stderr; stdout stays clean for callers
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("vcpkg install: %w", err)
	}
	r.InstalledDir = filepath.Join(installRoot, r.Triplet)
	return nil
}
