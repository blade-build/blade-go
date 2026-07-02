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
	"strings"
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

// cmakeOptions extracts a port's `cmake_options` (Blade's per-port configure
// flags, e.g. glog's -DGFLAGS_NOTHREADS=OFF). Returns nil for plain-string specs.
func cmakeOptions(v any) []string {
	spec, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	opts, ok := spec["cmake_options"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, o := range opts {
		if s, ok := o.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// writeOverlayTriplet materializes an overlay triplet that reuses the base
// triplet's settings and adds each port's cmake_options as a per-port
// VCPKG_CMAKE_CONFIGURE_OPTIONS branch (vcpkg triplets can switch on ${PORT}).
// This is how blade-go applies flare's cmake_options -- vcpkg.json has no
// per-port configure knob. Returns the overlay dir, or "" if nothing needs it.
func (r *Resolver) writeOverlayTriplet(packages map[string]any, dir string) (string, error) {
	perPort := map[string][]string{}
	for name, spec := range packages {
		if opts := cmakeOptions(spec); len(opts) > 0 {
			perPort[name] = opts
		}
	}
	if len(perPort) == 0 {
		return "", nil
	}
	base, err := r.baseTriplet()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n# blade-go: per-port cmake_options from vcpkg_config\n")
	names := make([]string, 0, len(perPort))
	for name := range perPort {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "if(PORT STREQUAL %q)\n", name)
		for _, opt := range perPort[name] {
			fmt.Fprintf(&b, "  list(APPEND VCPKG_CMAKE_CONFIGURE_OPTIONS %q)\n", opt)
		}
		b.WriteString("endif()\n")
	}
	overlay := filepath.Join(dir, "overlay-triplets")
	if err := os.MkdirAll(overlay, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(overlay, r.Triplet+".cmake"), []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return overlay, nil
}

// baseTriplet returns the built-in triplet file's contents (community/ falls
// back), so the overlay can extend rather than replace the standard settings.
func (r *Resolver) baseTriplet() (string, error) {
	if r.Root == "" {
		return "", fmt.Errorf("VCPKG_ROOT unset: cannot locate base triplet %s", r.Triplet)
	}
	for _, rel := range []string{
		filepath.Join("triplets", r.Triplet+".cmake"),
		filepath.Join("triplets", "community", r.Triplet+".cmake"),
	} {
		if data, err := os.ReadFile(filepath.Join(r.Root, rel)); err == nil {
			return string(data), nil
		}
	}
	return "", fmt.Errorf("base triplet %s not found under %s/triplets", r.Triplet, r.Root)
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
	args := []string{"install",
		"--triplet=" + r.Triplet,
		"--x-manifest-root=" + manifestDir,
		"--x-install-root=" + installRoot}
	if overlay, err := r.writeOverlayTriplet(packages, manifestDir); err != nil {
		return err
	} else if overlay != "" {
		args = append(args, "--overlay-triplets="+overlay)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stderr // progress on stderr; stdout stays clean for callers
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()

	// Use the pinned tree whenever it materialized, even on a partial failure:
	// the ports that installed (with their flare-pinned versions) must take
	// precedence over any stale global $VCPKG_ROOT/installed tree. Falling back
	// to that would silently undo the whole point of vcpkg_config.
	tree := filepath.Join(installRoot, r.Triplet)
	if _, statErr := os.Stat(filepath.Join(tree, "include")); statErr == nil {
		r.InstalledDir = tree
	}
	if runErr != nil {
		return fmt.Errorf("vcpkg install: %w", runErr)
	}
	return nil
}
