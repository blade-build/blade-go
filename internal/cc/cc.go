// Package cc generates ninja build statements for C/C++ targets (cc_library,
// cc_binary, cc_test, cc_benchmark) from the dependency graph.
package cc

import (
	"path"
	"strings"

	"github.com/blade-build/blade-go/internal/graph"
	"github.com/blade-build/blade-go/internal/ninja"
	"github.com/blade-build/blade-go/internal/toolchain"
)

// Generator turns a resolved graph into a ninja file.
type Generator struct {
	Tc       *toolchain.Toolchain
	BuildDir string // build output dir (e.g. "build64_release")
}

// New returns a Generator with the default build dir.
func New(tc *toolchain.Toolchain) *Generator {
	return &Generator{Tc: tc, BuildDir: "build64_release"}
}

// IsCC reports whether a rule type is one this generator handles.
func IsCC(ruleType string) bool {
	switch ruleType {
	case "cc_library", "cc_binary", "cc_test", "cc_benchmark":
		return true
	}
	return false
}

// Generate produces the ninja file for all cc targets in g.
func (gen *Generator) Generate(g *graph.Graph) (*ninja.File, error) {
	sorted, err := g.TopoSort()
	if err != nil {
		return nil, err
	}
	f := &ninja.File{}
	f.SetVar("cc", gen.Tc.CC)
	f.SetVar("cxx", gen.Tc.CXX)
	f.SetVar("ar", gen.Tc.AR)
	gen.emitRules(f)

	libOf := map[*graph.Node]string{}
	for _, n := range sorted {
		if !IsCC(n.Target.Type) {
			continue
		}
		objs := gen.emitCompiles(f, n)
		switch n.Target.Type {
		case "cc_library":
			lib := gen.libPath(n)
			f.AddBuild(ninja.Build{Outputs: []string{lib}, Rule: "ar", Inputs: objs})
			libOf[n] = lib
		default: // cc_binary / cc_test / cc_benchmark
			libs, implicit := gen.transitiveLibs(n, libOf)
			f.AddBuild(ninja.Build{
				Outputs:  []string{gen.binPath(n)},
				Rule:     "link",
				Inputs:   objs,
				Implicit: implicit,
				Vars:     map[string]string{"libs": gen.linkArgs(libs, gen.transitiveSyslibs(n))},
			})
		}
	}
	return f, nil
}

func (gen *Generator) emitRules(f *ninja.File) {
	f.AddRule(ninja.Rule{
		Name: "cc", Description: "CC ${out}", Depfile: "${out}.d", Deps: "gcc",
		Command: "${cc} -MMD -MF ${out}.d ${cflags} ${includes} -c ${in} -o ${out}",
	})
	f.AddRule(ninja.Rule{
		Name: "cxx", Description: "CXX ${out}", Depfile: "${out}.d", Deps: "gcc",
		Command: "${cxx} -MMD -MF ${out}.d ${cxxflags} ${includes} -c ${in} -o ${out}",
	})
	f.AddRule(ninja.Rule{
		Name: "ar", Description: "AR ${out}",
		Command: "rm -f ${out} && ${ar} rcs ${out} ${in}",
	})
	f.AddRule(ninja.Rule{
		Name: "link", Description: "LINK ${out}",
		Command: "${cxx} ${in} ${libs} ${ldflags} -o ${out}",
	})
}

// emitCompiles emits one compile edge per source and returns the object paths.
func (gen *Generator) emitCompiles(f *ninja.File, n *graph.Node) []string {
	inc := gen.includes(n)
	var objs []string
	for _, src := range n.Target.AttrStrings("srcs") {
		srcPath := path.Join(n.Target.Package, src)
		obj := path.Join(gen.BuildDir, n.Target.Package, n.Target.Name+".objs", src) + gen.Tc.ObjSuffix()
		rule := "cc"
		if isCXXSource(src) {
			rule = "cxx"
		}
		f.AddBuild(ninja.Build{
			Outputs: []string{obj}, Rule: rule, Inputs: []string{srcPath},
			Vars: map[string]string{"includes": inc},
		})
		objs = append(objs, obj)
	}
	return objs
}

// includes builds the -I flags: workspace root and build dir (so both source and
// generated headers resolve by workspace-relative path), plus the transitive
// exported include dirs.
func (gen *Generator) includes(n *graph.Node) string {
	dirs := []string{".", gen.BuildDir}
	seen := map[string]bool{".": true, gen.BuildDir: true}
	var walk func(*graph.Node)
	walk = func(node *graph.Node) {
		for _, d := range node.Target.AttrStrings("export_incs") {
			if !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
		}
		for _, dep := range node.Deps {
			walk(dep)
		}
	}
	walk(n)
	var b strings.Builder
	for i, d := range dirs {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("-I")
		b.WriteString(d)
	}
	return b.String()
}

// transitiveLibs returns the archive paths of all cc_library deps (dependents
// before their own deps, so the link order is correct) and the same set as
// implicit deps.
func (gen *Generator) transitiveLibs(n *graph.Node, libOf map[*graph.Node]string) (libs, implicit []string) {
	seen := map[*graph.Node]bool{}
	var walk func(*graph.Node)
	walk = func(node *graph.Node) {
		for _, dep := range node.Deps {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			if lib, ok := libOf[dep]; ok {
				libs = append(libs, lib)
				implicit = append(implicit, lib)
			}
			walk(dep)
		}
	}
	walk(n)
	return libs, implicit
}

func (gen *Generator) transitiveSyslibs(n *graph.Node) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(*graph.Node)
	walk = func(node *graph.Node) {
		for _, s := range node.Syslibs {
			if !seen[s.Name] {
				seen[s.Name] = true
				out = append(out, s.Name)
			}
		}
		for _, dep := range node.Deps {
			walk(dep)
		}
	}
	walk(n)
	return out
}

// linkArgs renders the archive + system-library portion of the link command.
func (gen *Generator) linkArgs(libs, syslibs []string) string {
	var parts []string
	if len(libs) > 0 && gen.Tc.GroupsLibraries() {
		parts = append(parts, "-Wl,--start-group")
		parts = append(parts, libs...)
		parts = append(parts, "-Wl,--end-group")
	} else {
		parts = append(parts, libs...)
	}
	for _, s := range syslibs {
		parts = append(parts, "-l"+s)
	}
	return strings.Join(parts, " ")
}

func (gen *Generator) libPath(n *graph.Node) string {
	return path.Join(gen.BuildDir, n.Target.Package, gen.Tc.StaticLib(n.Target.Name))
}

func (gen *Generator) binPath(n *graph.Node) string {
	return path.Join(gen.BuildDir, n.Target.Package, gen.Tc.BinName(n.Target.Name))
}

func isCXXSource(src string) bool {
	for _, ext := range []string{".cc", ".cpp", ".cxx", ".C", ".c++", ".mm"} {
		if strings.HasSuffix(src, ext) {
			return true
		}
	}
	return false
}
