// Package label parses and represents Blade target labels and visibility
// patterns.
//
// A label identifies a target within the workspace: "//pkg/sub:name". Blade also
// accepts shorthands in deps: ":name" (same package), "pkg:name" (the leading
// "//" is optional), "//pkg" (name defaults to the last path component), and
// "#name" for a system library.
package label

import (
	"fmt"
	"strings"
)

// SyslibPackage is the sentinel package for a system-library dep ("#pthread").
const SyslibPackage = "#"

// Label is a parsed target reference.
type Label struct {
	Package string // slash path relative to the workspace root ("" == root)
	Name    string
}

// String renders the canonical form.
func (l Label) String() string {
	if l.Package == SyslibPackage {
		return "#" + l.Name
	}
	return "//" + l.Package + ":" + l.Name
}

// IsSyslib reports whether l refers to a system library ("#name").
func (l Label) IsSyslib() bool { return l.Package == SyslibPackage }

// VcpkgDep is a parsed "vcpkg#<port>:<lib>" dependency.
type VcpkgDep struct {
	Port string
	Lib  string
}

// IsVcpkg reports whether a dep string uses the vcpkg scheme.
func IsVcpkg(s string) bool { return strings.HasPrefix(s, "vcpkg#") }

// ParseVcpkg parses "vcpkg#<port>" (lib defaults to port) or
// "vcpkg#<port>:<lib>".
func ParseVcpkg(s string) VcpkgDep {
	body := strings.TrimPrefix(s, "vcpkg#")
	if i := strings.LastIndex(body, ":"); i >= 0 {
		return VcpkgDep{Port: body[:i], Lib: body[i+1:]}
	}
	return VcpkgDep{Port: body, Lib: body}
}

// Parse resolves a dep string relative to currentPkg.
func Parse(s, currentPkg string) (Label, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Label{}, fmt.Errorf("empty label")
	}
	if strings.HasPrefix(s, "#") {
		return Label{Package: SyslibPackage, Name: s[1:]}, nil
	}
	if strings.HasPrefix(s, ":") {
		return Label{Package: currentPkg, Name: s[1:]}, nil
	}
	body := strings.TrimPrefix(s, "//")
	if i := strings.LastIndex(body, ":"); i >= 0 {
		pkg := body[:i]
		if pkg == "" {
			pkg = currentPkg
		}
		name := body[i+1:]
		if name == "" {
			return Label{}, fmt.Errorf("label %q has an empty target name", s)
		}
		return Label{Package: pkg, Name: name}, nil
	}
	// No colon: "//pkg" -> target named after the last path component.
	pkg := body
	name := pkg
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		name = pkg[i+1:]
	}
	return Label{Package: pkg, Name: name}, nil
}

// VisibleTo reports whether a target declared in definingPkg with the given
// visibility patterns is visible to consumer. A target is always visible within
// its own package; otherwise a pattern must match.
//
// Supported patterns: "PUBLIC", "//pkg:name" (exact), "//pkg:*" (any target in
// pkg), and "//pkg/..." / "//pkg:..." (pkg and all sub-packages -- flare uses
// the colon form, e.g. '//flare/fiber:...').
func VisibleTo(visibility []string, definingPkg string, consumer Label) bool {
	if consumer.Package == definingPkg {
		return true
	}
	for _, v := range visibility {
		if v == "PUBLIC" || matchVisibility(v, consumer) {
			return true
		}
	}
	return false
}

func matchVisibility(pattern string, consumer Label) bool {
	body := strings.TrimPrefix(pattern, "//")
	// Recursive subtree: both '//pkg/...' and '//pkg:...' mean pkg and all its
	// sub-packages (flare writes the colon form).
	for _, suffix := range []string{"/...", ":..."} {
		if rec, ok := strings.CutSuffix(body, suffix); ok {
			return consumer.Package == rec || strings.HasPrefix(consumer.Package, rec+"/")
		}
	}
	if body == "..." {
		return true
	}
	if i := strings.LastIndex(body, ":"); i >= 0 {
		pkg, name := body[:i], body[i+1:]
		if name == "*" {
			return consumer.Package == pkg
		}
		return consumer.Package == pkg && consumer.Name == name
	}
	return false
}
