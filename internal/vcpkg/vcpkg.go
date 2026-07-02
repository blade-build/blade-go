// Package vcpkg resolves `vcpkg#<port>:<lib>` dependencies to include and link
// flags from an installed vcpkg tree.
//
// This is how blade-go routes thirdparty libraries (flare's 26 deps). When no
// vcpkg tree is configured it degrades to a plain `-l<lib>` system-library link,
// which keeps fixtures and simple setups working.
package vcpkg

import (
	"os"
	"path/filepath"
	"runtime"
)

// Resolver locates headers and archives in a vcpkg installation.
type Resolver struct {
	Root    string // VCPKG_ROOT
	Triplet string // e.g. "x64-linux"
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
func (r *Resolver) Configured() bool { return r != nil && r.Root != "" }

func (r *Resolver) installed() string {
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
