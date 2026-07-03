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

// IsMSVC reports whether the toolchain uses MSVC naming/link conventions. On
// Windows blade-go targets MSVC (no MinGW), so the OS is the discriminant --
// mirroring Blade's cc_is('msvc'). File-naming below follows Blade's toolchain
// properties: obj '.obj', static lib '<name>.lib' (no 'lib' prefix), exe '.exe';
// gcc/clang use '.o', 'lib<name>.a', ”.
func (t *Toolchain) IsMSVC() bool { return t.OS == "windows" }

// ObjSuffix returns the object-file suffix ('.o' / '.obj').
func (t *Toolchain) ObjSuffix() string {
	if t.IsMSVC() {
		return ".obj"
	}
	return ".o"
}

// StaticLib returns the archive file name for a library target
// ('lib<name>.a' / '<name>.lib').
func (t *Toolchain) StaticLib(name string) string {
	if t.IsMSVC() {
		return name + ".lib"
	}
	return "lib" + name + ".a"
}

// DynamicLib returns the shared-library file name for a target
// ('lib<name>.so' / 'lib<name>.dylib' / '<name>.dll').
func (t *Toolchain) DynamicLib(name string) string {
	switch t.OS {
	case "windows":
		return name + ".dll"
	case "darwin":
		return "lib" + name + ".dylib"
	default:
		return "lib" + name + ".so"
	}
}

// BinName returns the executable file name for a binary target (” / '.exe').
func (t *Toolchain) BinName(name string) string {
	if t.OS == "windows" {
		return name + ".exe"
	}
	return name
}

// GroupsLibraries reports whether user archives must be wrapped in a link group
// to resolve inter-archive ordering (GNU ld). Apple ld64 re-scans, so no.
func (t *Toolchain) GroupsLibraries() bool { return t.OS == "linux" }

// ForceLoad returns the linker flags that force every object of an archive to be
// linked (blade's link_all_symbols) -- needed when a lib's static initializers
// (gflags/glog flag registration, protobuf descriptors) must run even though
// nothing references them. macOS uses -force_load; GNU ld uses --whole-archive.
func (t *Toolchain) ForceLoad(archive string) []string {
	if t.OS == "darwin" {
		return []string{"-Wl,-force_load," + archive}
	}
	return []string{"-Wl,--whole-archive", archive, "-Wl,--no-whole-archive"}
}
