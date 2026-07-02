// Package cc generates ninja build statements for C/C++ targets (cc_library,
// cc_binary, cc_test, cc_benchmark) from the dependency graph.
package cc

import (
	"path"
	"strings"

	"github.com/blade-build/blade-go/internal/graph"
	"github.com/blade-build/blade-go/internal/label"
	"github.com/blade-build/blade-go/internal/ninja"
	"github.com/blade-build/blade-go/internal/toolchain"
	"github.com/blade-build/blade-go/internal/vcpkg"
)

// Generator turns a resolved graph into a ninja file.
type Generator struct {
	Tc           *toolchain.Toolchain
	BuildDir     string          // build output dir (e.g. "build64_release")
	Protoc       string          // protoc executable (proto_library codegen)
	ProtobufLibs []string        // system libs a proto_library pulls in (bare names)
	Vcpkg        *vcpkg.Resolver // resolves vcpkg#port:lib thirdparty deps
}

// New returns a Generator with the default build dir and protobuf settings.
func New(tc *toolchain.Toolchain) *Generator {
	return &Generator{
		Tc:           tc,
		BuildDir:     "build64_release",
		Protoc:       "protoc",
		ProtobufLibs: []string{"protobuf", "pthread"},
		Vcpkg:        vcpkg.FromEnv(),
	}
}

// IsCC reports whether a rule type is one this generator handles.
func IsCC(ruleType string) bool {
	switch ruleType {
	case "cc_library", "cc_binary", "cc_test", "cc_benchmark":
		return true
	}
	return false
}

// Generate produces the ninja file for all cc / proto targets in g.
func (gen *Generator) Generate(g *graph.Graph) (*ninja.File, error) {
	sorted, err := g.TopoSort()
	if err != nil {
		return nil, err
	}
	f := &ninja.File{}
	f.SetVar("cc", gen.Tc.CC)
	f.SetVar("cxx", gen.Tc.CXX)
	f.SetVar("ar", gen.Tc.AR)
	f.SetVar("protoc", gen.Protoc)
	f.SetVar("builddir", gen.BuildDir)
	gen.emitRules(f)

	libOf := map[*graph.Node]string{}
	genHdrsOf := map[*graph.Node][]string{}
	genFilesOf := map[*graph.Node]map[string]string{}
	for _, n := range sorted {
		switch {
		case n.Target.Type == "gen_rule":
			gen.emitGenRule(f, n, genFilesOf)
		case n.Target.Type == "proto_library":
			lib, hdrs := gen.emitProto(f, n)
			libOf[n] = lib
			genHdrsOf[n] = hdrs
		case IsCC(n.Target.Type):
			objs := gen.emitCompiles(f, n, gen.transitiveGenHdrs(n, genHdrsOf), gen.transitiveGenFiles(n, genFilesOf))
			if n.Target.Type == "cc_library" {
				lib := gen.libPath(n)
				f.AddBuild(ninja.Build{Outputs: []string{lib}, Rule: "ar", Inputs: objs})
				libOf[n] = lib
				continue
			}
			libs, implicit := gen.transitiveLibs(n, libOf)
			syslibs := gen.transitiveSyslibs(n)
			if gen.hasProtoInClosure(n) {
				syslibs = uniqueStrings(append(syslibs, gen.ProtobufLibs...))
			}
			f.AddBuild(ninja.Build{
				Outputs:  []string{gen.binPath(n)},
				Rule:     "link",
				Inputs:   objs,
				Implicit: implicit,
				Vars:     map[string]string{"libs": gen.linkArgs(libs, syslibs, gen.vcpkgLinkArgs(n))},
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
	f.AddRule(ninja.Rule{
		Name: "protoc", Description: "PROTOC ${in}",
		Command: "${protoc} --proto_path=. --cpp_out=${builddir} ${in}",
	})
	f.AddRule(ninja.Rule{
		Name: "gen", Description: "GEN ${out}",
		Command: "${cmd}",
	})
}

// emitGenRule emits a custom-command edge for a gen_rule. It records the mapping
// from each declared `out` name to its build-dir path so a consumer that lists a
// generated file in `srcs` resolves to the generated path.
func (gen *Generator) emitGenRule(f *ninja.File, n *graph.Node, genFilesOf map[*graph.Node]map[string]string) {
	pkg := n.Target.Package
	var srcs, outs []string
	for _, s := range n.Target.AttrStrings("srcs") {
		srcs = append(srcs, path.Join(pkg, s))
	}
	outMap := map[string]string{}
	for _, o := range n.Target.AttrStrings("outs") {
		op := path.Join(gen.BuildDir, pkg, o)
		outs = append(outs, op)
		outMap[o] = op
	}
	genFilesOf[n] = outMap
	cmd := strings.NewReplacer(
		"$SRCS", strings.Join(srcs, " "),
		"$OUTS", strings.Join(outs, " "),
		"$BUILD_DIR", gen.BuildDir,
	).Replace(n.Target.AttrString("cmd"))
	f.AddBuild(ninja.Build{Outputs: outs, Rule: "gen", Inputs: srcs, Vars: map[string]string{"cmd": cmd}})
}

// transitiveGenFiles merges the out-name->path maps of all gen_rule deps, so a
// consumer's generated sources can be resolved to their build-dir paths.
func (gen *Generator) transitiveGenFiles(n *graph.Node, genFilesOf map[*graph.Node]map[string]string) map[string]string {
	out := map[string]string{}
	seen := map[*graph.Node]bool{}
	var walk func(*graph.Node)
	walk = func(node *graph.Node) {
		for _, dep := range node.Deps {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			for k, v := range genFilesOf[dep] {
				out[k] = v
			}
			walk(dep)
		}
	}
	walk(n)
	return out
}

// emitProto runs protoc for each .proto (generating .pb.cc/.pb.h under the build
// dir), compiles the generated C++, and archives it into the target's library.
// Returns the archive path and the generated header paths (for consumers).
func (gen *Generator) emitProto(f *ninja.File, n *graph.Node) (lib string, genHdrs []string) {
	inc := gen.includes(n)
	var objs []string
	for _, src := range n.Target.AttrStrings("srcs") {
		proto := path.Join(n.Target.Package, src)
		stem := path.Join(gen.BuildDir, strings.TrimSuffix(proto, ".proto"))
		pbcc, pbh := stem+".pb.cc", stem+".pb.h"
		f.AddBuild(ninja.Build{Outputs: []string{pbcc, pbh}, Rule: "protoc", Inputs: []string{proto}})
		obj := pbcc + gen.Tc.ObjSuffix()
		f.AddBuild(ninja.Build{
			Outputs: []string{obj}, Rule: "cxx", Inputs: []string{pbcc},
			Implicit: []string{pbh},
			Vars:     map[string]string{"includes": inc},
		})
		objs = append(objs, obj)
		genHdrs = append(genHdrs, pbh)
	}
	lib = gen.libPath(n)
	f.AddBuild(ninja.Build{Outputs: []string{lib}, Rule: "ar", Inputs: objs})
	return lib, genHdrs
}

// emitCompiles emits one compile edge per source and returns the object paths.
// implicitHdrs (generated proto headers in the closure) are added as implicit
// deps so codegen is ordered before compilation. A source that names a generated
// file (in genFiles) compiles from its build-dir path instead of the source tree.
func (gen *Generator) emitCompiles(f *ninja.File, n *graph.Node, implicitHdrs []string, genFiles map[string]string) []string {
	inc := gen.includes(n)
	var objs []string
	for _, src := range n.Target.AttrStrings("srcs") {
		srcPath := path.Join(n.Target.Package, src)
		if gp, ok := genFiles[src]; ok {
			srcPath = gp
		}
		obj := path.Join(gen.BuildDir, n.Target.Package, n.Target.Name+".objs", src) + gen.Tc.ObjSuffix()
		rule := "cc"
		if isCXXSource(src) {
			rule = "cxx"
		}
		f.AddBuild(ninja.Build{
			Outputs: []string{obj}, Rule: rule, Inputs: []string{srcPath},
			Implicit: implicitHdrs,
			Vars:     map[string]string{"includes": inc},
		})
		objs = append(objs, obj)
	}
	return objs
}

// transitiveGenHdrs collects the generated headers of all proto_library deps.
func (gen *Generator) transitiveGenHdrs(n *graph.Node, genHdrsOf map[*graph.Node][]string) []string {
	var out []string
	seen := map[*graph.Node]bool{}
	var walk func(*graph.Node)
	walk = func(node *graph.Node) {
		for _, dep := range node.Deps {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			out = append(out, genHdrsOf[dep]...)
			walk(dep)
		}
	}
	walk(n)
	return out
}

// hasProtoInClosure reports whether any transitive dep is a proto_library.
func (gen *Generator) hasProtoInClosure(n *graph.Node) bool {
	seen := map[*graph.Node]bool{}
	var walk func(*graph.Node) bool
	walk = func(node *graph.Node) bool {
		for _, dep := range node.Deps {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			if dep.Target.Type == "proto_library" || walk(dep) {
				return true
			}
		}
		return false
	}
	return walk(n)
}

func uniqueStrings(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
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
	if inc := gen.Vcpkg.IncludeDir(); inc != "" && len(gen.transitiveVcpkgs(n)) > 0 {
		dirs = append(dirs, inc)
	}
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

// transitiveVcpkgs collects the unique vcpkg deps of a node and its closure.
func (gen *Generator) transitiveVcpkgs(n *graph.Node) []label.VcpkgDep {
	var out []label.VcpkgDep
	seen := map[string]bool{}
	var walk func(*graph.Node)
	walk = func(node *graph.Node) {
		for _, v := range node.Vcpkgs {
			key := v.Port + ":" + v.Lib
			if !seen[key] {
				seen[key] = true
				out = append(out, v)
			}
		}
		for _, dep := range node.Deps {
			walk(dep)
		}
	}
	walk(n)
	return out
}

// vcpkgLinkArgs returns the linker arguments for a node's transitive vcpkg deps.
func (gen *Generator) vcpkgLinkArgs(n *graph.Node) []string {
	var out []string
	for _, v := range gen.transitiveVcpkgs(n) {
		out = append(out, gen.Vcpkg.LibArg(v.Lib))
	}
	return out
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

// linkArgs renders the archive + system-library + vcpkg portion of the link
// command. `extra` (vcpkg archive paths / -l flags) is appended after the group.
func (gen *Generator) linkArgs(libs, syslibs, extra []string) string {
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
	parts = append(parts, extra...)
	return strings.Join(parts, " ")
}

func (gen *Generator) libPath(n *graph.Node) string {
	return path.Join(gen.BuildDir, n.Target.Package, gen.Tc.StaticLib(n.Target.Name))
}

func (gen *Generator) binPath(n *graph.Node) string {
	return path.Join(gen.BuildDir, n.Target.Package, gen.Tc.BinName(n.Target.Name))
}

// BinPath returns the (workspace-relative) executable path for a cc_binary /
// cc_test / cc_benchmark node.
func (gen *Generator) BinPath(n *graph.Node) string { return gen.binPath(n) }

func isCXXSource(src string) bool {
	for _, ext := range []string{".cc", ".cpp", ".cxx", ".C", ".c++", ".mm"} {
		if strings.HasSuffix(src, ext) {
			return true
		}
	}
	return false
}
