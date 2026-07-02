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
	Tc               *toolchain.Toolchain
	BuildDir         string           // build output dir (e.g. "build64_release")
	Self             string           // path to the blade-go binary (resource_library codegen)
	Protoc           string           // protoc executable (proto_library codegen)
	ProtobufLibs     []string         // system libs a proto_library pulls in (bare names)
	ProtobufVcpkgs   []label.VcpkgDep // protobuf libs pinned in vcpkg (flare's idiom)
	Vcpkg            *vcpkg.Resolver  // resolves vcpkg#port:lib thirdparty deps
	Cppflags         []string         // cc_config flags for all C-family compiles
	Cxxflags         []string         // cc_config flags for C++ compiles only
	Cflags           []string         // cc_config flags for C compiles only
	TestVcpkgs       []label.VcpkgDep // cc_test_config gtest libs that resolve to vcpkg
	TestSyslibs      []string         // cc_test_config gtest libs that are #-syslibs
	BenchmarkVcpkgs  []label.VcpkgDep // cc_config benchmark libs that resolve to vcpkg
	BenchmarkSyslibs []string         // cc_config benchmark libs that are #-syslibs

	foreignIncs map[*graph.Node][]string // include dirs exported by foreign_cc_library nodes

	// ForceLoadPorts are vcpkg ports whose whole archive must be linked
	// (vcpkg_config link_all_symbols: gflags/glog/yaml-cpp).
	ForceLoadPorts map[string]bool
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
	self := gen.Self
	if self == "" {
		self = "blade-go"
	}
	f.SetVar("self", self)
	f.SetVar("builddir", gen.BuildDir)
	f.SetVar("cppflags", strings.Join(gen.Cppflags, " "))
	f.SetVar("cxxflags", strings.Join(gen.Cxxflags, " "))
	f.SetVar("cflags", strings.Join(gen.Cflags, " "))
	gen.emitRules(f)

	libOf := map[*graph.Node]string{}
	genHdrsOf := map[*graph.Node][]string{}
	genFilesOf := map[*graph.Node]map[string]string{}
	gen.foreignIncs = map[*graph.Node][]string{}
	for _, n := range sorted {
		switch {
		case n.Target.Type == "gen_rule":
			gen.emitGenRule(f, n, genFilesOf, libOf)
			// A gen_rule that emits headers (flare's cc_flare_library codegen
			// produces .flare.pb.h) exposes them as generated headers so a
			// consumer's compile waits for the codegen.
			for name, p := range genFilesOf[n] {
				if isHeader(name) {
					genHdrsOf[n] = append(genHdrsOf[n], p)
				}
			}
		case n.Target.Type == "foreign_cc_library":
			// A source-built thirdparty library (jsoncpp): its gen_rule dep
			// produced the archive + header shims. Contribute the archive to
			// consumers' links (via libOf) and the include dirs to their compiles.
			lib, incs := gen.foreignInfo(n, genFilesOf)
			if lib != "" {
				libOf[n] = lib
			}
			gen.foreignIncs[n] = incs
		case n.Target.Type == "proto_library":
			lib, hdrs := gen.emitProto(f, n)
			libOf[n] = lib
			genHdrsOf[n] = hdrs
		case n.Target.Type == "resource_library":
			lib, hdrs := gen.emitResourceLibrary(f, n)
			libOf[n] = lib
			genHdrsOf[n] = hdrs
		case IsCC(n.Target.Type):
			objs, checkObjs := gen.emitCompiles(f, n, gen.transitiveGenHdrs(n, genHdrsOf), gen.transitiveGenFiles(n, genFilesOf))
			if n.Target.Type == "cc_library" {
				if len(objs) == 0 {
					// Header-only library (only headers in srcs, or none): no
					// archive to link. The self-sufficiency check objects are not
					// linked anywhere, so nothing references them -- skip.
					continue
				}
				lib := gen.libPath(n)
				// checkObjs (compiled headers) are implicit so the check runs, but
				// are NOT archived -- ld rejects an empty header object.
				f.AddBuild(ninja.Build{Outputs: []string{lib}, Rule: "ar", Inputs: objs, Implicit: checkObjs})
				libOf[n] = lib
				continue
			}
			libs, implicit := gen.transitiveLibs(n, libOf)
			implicit = append(implicit, checkObjs...)
			syslibs := gen.transitiveSyslibs(n)
			vcpkgArgs := gen.vcpkgLinkArgs(n)
			if gen.hasProtoInClosure(n) {
				syslibs = uniqueStrings(append(syslibs, gen.ProtobufLibs...))
				for _, v := range gen.ProtobufVcpkgs {
					vcpkgArgs = append(vcpkgArgs, gen.Vcpkg.LibArg(v.Lib))
				}
			}
			// cc_test / cc_benchmark link their configured framework: gtest
			// (cc_test_config) for tests, google-benchmark (cc_config) for benches.
			switch n.Target.Type {
			case "cc_test":
				for _, v := range gen.TestVcpkgs {
					vcpkgArgs = append(vcpkgArgs, gen.Vcpkg.LibArg(v.Lib))
				}
				syslibs = uniqueStrings(append(syslibs, gen.TestSyslibs...))
			case "cc_benchmark":
				for _, v := range gen.BenchmarkVcpkgs {
					vcpkgArgs = append(vcpkgArgs, gen.Vcpkg.LibArg(v.Lib))
				}
				syslibs = uniqueStrings(append(syslibs, gen.BenchmarkSyslibs...))
			}
			// Extra link flags the pinned ports declare (macOS -framework for
			// curl's TLS backend). Only needed once vcpkg archives are linked.
			if len(vcpkgArgs) > 0 {
				vcpkgArgs = append(vcpkgArgs, gen.Vcpkg.LinkExtras()...)
			}
			f.AddBuild(ninja.Build{
				Outputs:  []string{gen.binPath(n)},
				Rule:     "link",
				Inputs:   objs,
				Implicit: implicit,
				Vars:     map[string]string{"libs": gen.linkArgs(libs, syslibs, vcpkgArgs)},
			})
		}
	}
	return f, nil
}

func (gen *Generator) emitRules(f *ninja.File) {
	f.AddRule(ninja.Rule{
		Name: "cc", Description: "CC ${out}", Depfile: "${out}.d", Deps: "gcc",
		Command: "${cc} -MMD -MF ${out}.d ${cppflags} ${cflags} ${defs} ${includes} -c ${in} -o ${out}",
	})
	f.AddRule(ninja.Rule{
		Name: "cxx", Description: "CXX ${out}", Depfile: "${out}.d", Deps: "gcc",
		Command: "${cxx} -MMD -MF ${out}.d ${cppflags} ${cxxflags} ${defs} ${includes} -c ${in} -o ${out}",
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
	f.AddRule(ninja.Rule{
		Name: "resource_index", Description: "RESOURCE INDEX ${out}",
		Command: "${self} __gen-resource-index ${name} ${path} ${out} ${in}",
	})
	f.AddRule(ninja.Rule{
		Name: "resource", Description: "RESOURCE ${in}",
		Command: "${self} __gen-resource ${out} ${in}",
	})
}

// emitResourceLibrary emits blade's resource_library: an index (.h/.c) plus one
// embedded-bytes .c per resource, all compiled and archived. Returns the archive
// and the generated index header (a consumer include).
func (gen *Generator) emitResourceLibrary(f *ninja.File, n *graph.Node) (lib string, genHdrs []string) {
	pkg := n.Target.Package
	name := n.Target.Name
	resources := n.Target.AttrStrings("srcs")
	indexH := path.Join(gen.BuildDir, pkg, name+".h")
	indexC := path.Join(gen.BuildDir, pkg, name+".c")

	var srcPaths []string
	for _, r := range resources {
		srcPaths = append(srcPaths, path.Join(pkg, r))
	}
	f.AddBuild(ninja.Build{
		Outputs: []string{indexH, indexC}, Rule: "resource_index", Inputs: srcPaths,
		Vars: map[string]string{"name": name, "path": pkg},
	})

	inc := gen.includes(n)
	cFiles := []string{indexC}
	for _, r := range resources {
		rc := path.Join(gen.BuildDir, pkg, r+".c")
		f.AddBuild(ninja.Build{Outputs: []string{rc}, Rule: "resource", Inputs: []string{path.Join(pkg, r)}})
		cFiles = append(cFiles, rc)
	}

	var objs []string
	for _, cf := range cFiles {
		obj := cf + gen.Tc.ObjSuffix()
		f.AddBuild(ninja.Build{
			Outputs: []string{obj}, Rule: "cc", Inputs: []string{cf},
			Implicit: []string{indexH}, Vars: map[string]string{"includes": inc},
		})
		objs = append(objs, obj)
	}
	lib = gen.libPath(n)
	f.AddBuild(ninja.Build{Outputs: []string{lib}, Rule: "ar", Inputs: objs})
	return lib, []string{indexH}
}

// emitGenRule emits a custom-command edge for a gen_rule. It records the mapping
// from each declared `out` name to its build-dir path so a consumer that lists a
// generated file in `srcs` resolves to the generated path. A src that is itself
// a generated file (an out of a dep gen_rule -- the unpack->generate->build
// chains in foreign_build) resolves to that build path and becomes an implicit
// dep, so ninja orders the chain. The command sees the blade gen_rule variables
// $SRCS/$OUTS/$OUT_DIR/$FIRST_SRC/$SRC_DIR/$BUILD_DIR.
func (gen *Generator) emitGenRule(f *ninja.File, n *graph.Node, genFilesOf map[*graph.Node]map[string]string, libOf map[*graph.Node]string) {
	pkg := n.Target.Package
	fromDeps := gen.transitiveGenFiles(n, genFilesOf)
	var srcs []string
	var implicit []string
	for _, s := range n.Target.AttrStrings("srcs") {
		if gp, ok := fromDeps[s]; ok {
			srcs = append(srcs, gp)
			implicit = append(implicit, gp)
		} else {
			srcs = append(srcs, path.Join(pkg, s))
		}
	}
	// Order after each dep's primary output, even deps wired only through the
	// `deps` attr (not srcs): a gen_rule dep's generated files (autotools make
	// after configure), a cc_binary dep's binary (flare's cc_flare_library codegen
	// runs the built v2_plugin), or a cc_library dep's archive.
	for _, dep := range n.Deps {
		for _, op := range genFilesOf[dep] {
			implicit = append(implicit, op)
		}
		if IsCC(dep.Target.Type) && dep.Target.Type != "cc_library" {
			implicit = append(implicit, gen.binPath(dep))
		} else if lib, ok := libOf[dep]; ok {
			implicit = append(implicit, lib)
		}
	}
	outMap := map[string]string{}
	var outs []string
	for _, o := range n.Target.AttrStrings("outs") {
		op := path.Join(gen.BuildDir, pkg, o)
		outs = append(outs, op)
		outMap[o] = op
	}
	genFilesOf[n] = outMap
	firstSrc := ""
	if len(srcs) > 0 {
		firstSrc = srcs[0]
	}
	cmd := strings.NewReplacer(
		"$SRCS", strings.Join(srcs, " "),
		"$OUTS", strings.Join(outs, " "),
		"$OUT_DIR", path.Join(gen.BuildDir, pkg),
		"$FIRST_SRC", firstSrc,
		"$SRC_DIR", pkg,
		"$BUILD_DIR", gen.BuildDir,
	).Replace(n.Target.AttrString("cmd"))
	f.AddBuild(ninja.Build{Outputs: outs, Rule: "gen", Inputs: srcs, Implicit: implicit, Vars: map[string]string{"cmd": cmd}})
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

// foreignInfo returns the static archive and consumer include dirs for a
// foreign_cc_library, read from its gen_rule dependency (the cmake_build /
// autotools_build that produced them). The header shims the build writes live at
// <build>/<pkg>/<leaf>.h, so consumers that include "<pkg-leaf>/hdr" resolve via
// -I<build>/<dirname(pkg)>; system_export_incs adds the raw install include dir.
func (gen *Generator) foreignInfo(n *graph.Node, genFilesOf map[*graph.Node]map[string]string) (lib string, incs []string) {
	for _, dep := range n.Deps {
		if dep.Target.Type != "gen_rule" {
			continue
		}
		for name, p := range genFilesOf[dep] {
			if strings.HasSuffix(name, ".a") {
				lib = p
			}
		}
		pkg := dep.Target.Package
		incs = append(incs, path.Join(gen.BuildDir, path.Dir(pkg)))
		if sei := dep.Target.AttrString("system_export_incs"); sei != "" {
			incs = append(incs, path.Join(gen.BuildDir, pkg, sei))
		}
	}
	return lib, incs
}

// emitCompiles emits one compile edge per source. It returns the linkable object
// paths and, separately, the header self-sufficiency-check objects: a header in
// srcs is compiled standalone (blade's check) but its object must NOT be archived
// or linked -- it is (often) an empty object ld rejects as "not a mach-o file".
// implicitHdrs (generated proto headers in the closure) order codegen before
// compilation; a src naming a generated file compiles from its build-dir path.
func (gen *Generator) emitCompiles(f *ninja.File, n *graph.Node, implicitHdrs []string, genFiles map[string]string) (objs, checkObjs []string) {
	inc := gen.includes(n)
	defs := defineFlags(n) // per-target preprocessor defines (cc_test variants)
	for _, src := range n.Target.AttrStrings("srcs") {
		srcPath := path.Join(n.Target.Package, src)
		if gp, ok := genFiles[src]; ok {
			srcPath = gp
		}
		obj := path.Join(gen.BuildDir, n.Target.Package, n.Target.Name+".objs", src) + gen.Tc.ObjSuffix()
		rule := "cc"
		// Headers compile standalone with the target's language, which for cc_*
		// is C++ (they pull in <memory>); only a bare .c stays on the C compiler.
		header := isHeader(src)
		if isCXXSource(src) || header {
			rule = "cxx"
		}
		vars := map[string]string{"includes": inc}
		if defs != "" {
			vars["defs"] = defs
		}
		f.AddBuild(ninja.Build{
			Outputs: []string{obj}, Rule: rule, Inputs: []string{srcPath},
			Implicit: implicitHdrs,
			Vars:     vars,
		})
		if header {
			checkObjs = append(checkObjs, obj) // built as a check, never linked
		} else {
			objs = append(objs, obj)
		}
	}
	return objs, checkObjs
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
	visited := map[*graph.Node]bool{} // walk each node once, not once per DAG path
	var walk func(*graph.Node)
	walk = func(node *graph.Node) {
		if visited[node] {
			return
		}
		visited[node] = true
		for _, d := range node.Target.AttrStrings("export_incs") {
			if !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
		}
		for _, d := range gen.foreignIncs[node] {
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
	needVcpkg := len(gen.transitiveVcpkgs(n)) > 0
	if n.Target.Type == "cc_test" && len(gen.TestVcpkgs) > 0 {
		needVcpkg = true // gtest headers live in the vcpkg tree
	}
	if n.Target.Type == "cc_benchmark" && len(gen.BenchmarkVcpkgs) > 0 {
		needVcpkg = true // google-benchmark headers live in the vcpkg tree
	}
	if len(gen.ProtobufVcpkgs) > 0 && (n.Target.Type == "proto_library" || gen.hasProtoInClosure(n)) {
		needVcpkg = true // protobuf headers (for generated .pb.cc) live in the vcpkg tree
	}
	if inc := gen.Vcpkg.IncludeDir(); inc != "" && needVcpkg {
		dirs = append(dirs, inc)
		// include_prefix ports (zlib, snappy, ...) resolve "prefix/hdr" here.
		if gen.Vcpkg.PrefixRoot != "" {
			dirs = append(dirs, gen.Vcpkg.PrefixRoot)
		}
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
	visited := map[*graph.Node]bool{} // walk each node once, not once per DAG path
	var walk func(*graph.Node)
	walk = func(node *graph.Node) {
		if visited[node] {
			return
		}
		visited[node] = true
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
		arg := gen.Vcpkg.LibArg(v.Lib)
		// Force-load a link_all_symbols port's archive (gflags/glog registries).
		// Only when it resolved to a real archive, not a -l fallback.
		if gen.ForceLoadPorts[v.Port] && !strings.HasPrefix(arg, "-") {
			out = append(out, gen.Tc.ForceLoad(arg)...)
		} else {
			out = append(out, arg)
		}
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
				implicit = append(implicit, lib)
				if dep.Target.AttrBool("link_all_symbols") {
					libs = append(libs, gen.Tc.ForceLoad(lib)...)
				} else {
					libs = append(libs, lib)
				}
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
	visited := map[*graph.Node]bool{} // walk each node once, not once per DAG path
	var walk func(*graph.Node)
	walk = func(node *graph.Node) {
		if visited[node] {
			return
		}
		visited[node] = true
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

// defineFlags returns a target's `defs` as -D flags (flare's cc_test size
// variants pass e.g. defs = ['BUFFER_BLOCK_SIZE=\"4096\"']).
func defineFlags(n *graph.Node) string {
	var b strings.Builder
	for i, d := range n.Target.AttrStrings("defs") {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("-D")
		b.WriteString(d)
	}
	return b.String()
}

// isHeader reports whether src is a C/C++ header (compiled standalone for
// self-sufficiency checking). Treated as C++ by emitCompiles.
func isHeader(src string) bool {
	for _, ext := range []string{".h", ".hpp", ".hh", ".hxx", ".h++", ".inc"} {
		if strings.HasSuffix(src, ext) {
			return true
		}
	}
	return false
}
