// Package toolchain abstracts the host C/C++ toolchain (compiler, archiver, and
// the platform-specific naming/link conventions blade-go needs).
//
// Phase 3 scope: a gcc/clang-style toolchain discovered from the environment.
// MSVC and cross-compilation come later.
package toolchain

import (
	"os"
	"os/exec"
	"runtime"
)

// Toolchain describes the selected C/C++ tools and target platform.
type Toolchain struct {
	CC  string // C compiler driver
	CXX string // C++ compiler driver
	AR  string // static archiver
	OS  string // "linux", "darwin", "windows"
}

// Detect resolves the toolchain from $CC/$CXX/$AR or common defaults.
func Detect() *Toolchain {
	return &Toolchain{
		CC:  pick(os.Getenv("CC"), "cc", "gcc", "clang"),
		CXX: pick(os.Getenv("CXX"), "c++", "g++", "clang++"),
		AR:  pick(os.Getenv("AR"), "ar"),
		OS:  goos(),
	}
}

func goos() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "darwin"
	default:
		return "linux"
	}
}

// pick returns the first candidate that is non-empty and resolvable on PATH,
// falling back to the first candidate so generation never yields an empty tool.
func pick(candidates ...string) string {
	first := ""
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if first == "" {
			first = c
		}
		if _, err := exec.LookPath(c); err == nil {
			return c
		}
	}
	return first
}

// StaticLib returns the archive file name for a library target.
func (t *Toolchain) StaticLib(name string) string { return "lib" + name + ".a" }

// ObjSuffix returns the object-file suffix.
func (t *Toolchain) ObjSuffix() string { return ".o" }

// BinName returns the executable file name for a binary target.
func (t *Toolchain) BinName(name string) string {
	if t.OS == "windows" {
		return name + ".exe"
	}
	return name
}

// GroupsLibraries reports whether user archives must be wrapped in a link group
// to resolve inter-archive ordering (GNU ld). Apple ld64 re-scans, so no.
func (t *Toolchain) GroupsLibraries() bool { return t.OS == "linux" }
