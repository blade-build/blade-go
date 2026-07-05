// Package cc generates ninja build statements for C/C++ targets (cc_library,
// cc_binary, cc_test, cc_benchmark) from the dependency graph.
package cc

import (
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/blade-build/blade-go/internal/graph"
	"github.com/blade-build/blade-go/internal/label"
	"github.com/blade-build/blade-go/internal/ninja"
	"github.com/blade-build/blade-go/internal/toolchain"
	"github.com/blade-build/blade-go/internal/vcpkg"
)

// Generator turns a resolved graph into a ninja file.
type Generator struct {
	Tc              *toolchain.Toolchain
	Root            string           // workspace root (abs), for locating prebuilt library files
	BuildDir        string           // build output dir (e.g. "build_release")
	Profile         string           // "release" or "debug" (drives optimize/NDEBUG flags)
	DebugInfo       string           // "" (project default) or no|low|mid|high (-g level override)
	Self            string           // path to the blade-go binary (resource_library codegen)
	Protoc          string           // protoc executable (proto_library codegen)
	ProtobufLibs    []string         // system libs a proto_library pulls in (bare names)
	ProtobufVcpkgs  []label.VcpkgDep // protobuf libs pinned in vcpkg (flare's idiom)
	Vcpkg           *vcpkg.Resolver  // resolves vcpkg#port:lib thirdparty deps
	Cppflags        []string         // cc_config flags for all C-family compiles
	Cxxflags        []string         // cc_config flags for C++ compiles only
	Cflags          []string         // cc_config flags for C compiles only
	CWarnings       []string         // cc_config warnings for C compiles (not generated code)
	CxxWarnings     []string         // cc_config warnings for C++ compiles (not generated code)
	Linkflags       []string         // cc_config link flags (ldflags)
	Optimize        []string         // cc_config optimize flags (override the release default)
	SanitizeCompile []string         // --sanitizer compile flags (all compiles, incl. generated)
	SanitizeLink    []string         // --sanitizer link flags
	CoverageFlags   []string         // --coverage flags (both compile and link: gcc/clang --coverage)

	foreignIncs map[*graph.Node][]string // include dirs exported by foreign_cc_library nodes
	libOf       map[*graph.Node]string   // node -> its archive path (populated by Generate; for LinkArchives)

	// ForceLoadPorts are vcpkg ports whose whole archive must be linked
	// (vcpkg_config link_all_symbols: gflags/glog/yaml-cpp).
	ForceLoadPorts map[string]bool
}

// New returns a Generator with the default build dir and protobuf settings.
func New(tc *toolchain.Toolchain) *Generator {
	return &Generator{
		Tc:           tc,
		BuildDir:     "build_release",
		Profile:      "release",
		Protoc:       "protoc",
		ProtobufLibs: []string{"protobuf", "pthread"},
		Vcpkg:        vcpkg.FromEnv(),
	}
}

// profileFlags returns the compile flags that differ by build profile, matching
// Python Blade: release optimizes (-O2) and defines NDEBUG; debug leaves asserts
// live and unoptimized (-O0) and adds the stack protector.
func (gen *Generator) profileFlags() []string {
	if gen.Tc.IsMSVC() {
		if gen.Profile == "debug" {
			return []string{"/Od"}
		}
		opt := gen.Optimize
		if len(opt) == 0 {
			opt = []string{"/O2"}
		}
		return append(append([]string{}, opt...), "/DNDEBUG")
	}
	if gen.Profile == "debug" {
		return []string{"-O0", "-fstack-protector"}
	}
	// Release: NDEBUG + optimize. cc_config.optimize overrides the -O2 default.
	opt := gen.Optimize
	if len(opt) == 0 {
		opt = []string{"-O2"}
	}
	return append(append([]string{}, opt...), "-DNDEBUG")
}

// debugInfoFlags maps --debug-info-level / global_config.debug_info_level to a
// -g level, appended after the project's own debug flags so it overrides them.
// Empty (unset) adds nothing -- debug info stays whatever the project's cc_config
// specifies (flare uses -gdwarf-2).
func (gen *Generator) debugInfoFlags() []string {
	if gen.Tc.IsMSVC() {
		// /Z7 embeds CodeView in each .obj (parallel-safe under ninja, unlike the
		// PDB-server /Zi). 'no'/unset -> none.
		switch gen.DebugInfo {
		case "low", "mid", "high":
			return []string{"/Z7"}
		}
		return nil
	}
	switch gen.DebugInfo {
	case "no":
		return []string{"-g0"}
	case "low":
		return []string{"-g1"}
	case "mid":
		return []string{"-g2"}
	case "high":
		return []string{"-g3"}
	}
	return nil
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
	if gen.Tc.Link != "" {
		f.SetVar("link", gen.Tc.Link) // MSVC link.exe; gcc/clang link via ${cxx}
	}
	f.SetVar("protoc", gen.Protoc)
	self := gen.Self
	if self == "" {
		self = "blade-go"
	}
	f.SetVar("self", self)
	f.SetVar("builddir", gen.BuildDir)
	// Profile flags mirror Python Blade: release optimizes and defines NDEBUG;
	// debug does neither (asserts stay live, unoptimized) and enables the stack
	// protector. Debug info (-gdwarf-2) comes from the project's cc_config in both.
	cpp := append(append([]string{}, gen.Cppflags...), gen.profileFlags()...)
	cpp = append(cpp, gen.debugInfoFlags()...)
	// Sanitizer + coverage flags go on *every* compile (generated code too): a
	// sanitized/instrumented binary needs all its translation units consistent.
	cpp = append(cpp, gen.SanitizeCompile...)
	cpp = append(cpp, gen.CoverageFlags...)
	f.SetVar("cppflags", strings.Join(cpp, " "))
	f.SetVar("cxxflags", strings.Join(gen.Cxxflags, " "))
	f.SetVar("cflags", strings.Join(gen.Cflags, " "))
	// Warnings are a separate var so generated code (proto/resource) can opt out
	// by overriding it to empty -- Blade applies warnings only to hand-written
	// sources, never to protoc/codegen output. ldflags carries cc_config.linkflags
	// plus the sanitizer runtime link flags.
	f.SetVar("c_warnings", strings.Join(gen.CWarnings, " "))
	f.SetVar("cxx_warnings", strings.Join(gen.CxxWarnings, " "))
	ld := append(append([]string{}, gen.Linkflags...), gen.SanitizeLink...)
	ld = append(ld, gen.CoverageFlags...)
	f.SetVar("ldflags", strings.Join(ld, " "))
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
				// The same gen_rule that builds the archive also writes the header
				// shims (autotools/cmake _EXPORT_HEADERS). Expose the archive as a
				// generated-header dep so a consumer's COMPILE waits for it --
				// otherwise the consumer can race ahead and miss "<pkg>/hdr.h".
				genHdrsOf[n] = []string{lib}
			}
			gen.foreignIncs[n] = incs
		case n.Target.Type == "prebuilt_cc_library":
			// A pre-built library referenced (not compiled) from the source tree.
			// Expose its archive to consumers' links via libOf; its export_incs are
			// picked up by includes() like any dep. No build edge -- the file exists.
			if lib := gen.prebuiltLib(n); lib != "" {
				libOf[n] = lib
			}
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
			// cc_test / cc_benchmark link their configured framework (gtest /
			// google-benchmark) via deps injected by the graph builder, so the
			// transitive lib/vcpkg/syslib collection above already covers them.

			// Extra link flags the pinned ports declare (macOS -framework for
			// curl's TLS backend). Only needed once vcpkg archives are linked.
			if len(vcpkgArgs) > 0 {
				// The same vcpkg archive can arrive from several sources (a
				// transitive dep, protobuf_libs, gtest_libs); dedup so ld doesn't
				// warn about "ignoring duplicate libraries".
				vcpkgArgs = uniqueStrings(vcpkgArgs)
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
	gen.libOf = libOf // expose archive paths for LinkArchives (undefined-symbol check)
	return f, nil
}

// LinkArchives returns the on-disk archive paths whose symbols are available to
// node n at link time: n's own archive and every transitive cc-family dep's
// archive + vcpkg archive. Raw paths (no -Wl,-force_load / -l wrapping). Used by
// the undefined-symbol check. Call after Generate.
func (gen *Generator) LinkArchives(n *graph.Node) (own string, deps []string) {
	own = gen.libOf[n]
	_, implicit := gen.transitiveLibs(n, gen.libOf) // raw dep archive paths
	deps = append(deps, implicit...)
	vcpkgs := gen.transitiveVcpkgs(n)
	// A proto in the closure pulls in the protobuf runtime archive, added the
	// same way the link edge does (not a regular transitive vcpkg dep).
	if gen.hasProtoInClosure(n) {
		vcpkgs = append(vcpkgs, gen.ProtobufVcpkgs...)
	}
	for _, v := range vcpkgs {
		if arg := gen.Vcpkg.LibArg(v.Lib); arg != "" && !strings.HasPrefix(arg, "-") {
			deps = append(deps, arg)
		}
	}
	return own, deps
}

func (gen *Generator) emitRules(f *ninja.File) {
	if gen.Tc.IsMSVC() {
		gen.emitMSVCRules(f)
	} else {
		gen.emitGNURules(f)
	}
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

// emitGNURules emits the gcc/clang compile/archive/link rules (depfile deps).
func (gen *Generator) emitGNURules(f *ninja.File) {
	f.AddRule(ninja.Rule{
		Name: "cc", Description: "CC ${out}", Depfile: "${out}.d", Deps: "gcc",
		Command: "${cc} -MMD -MF ${out}.d ${cppflags} ${cflags} ${c_warnings} ${defs} ${extra_compile_flags} ${includes} -c ${in} -o ${out}",
	})
	f.AddRule(ninja.Rule{
		Name: "cxx", Description: "CXX ${out}", Depfile: "${out}.d", Deps: "gcc",
		Command: "${cxx} -MMD -MF ${out}.d ${cppflags} ${cxxflags} ${cxx_warnings} ${defs} ${extra_compile_flags} ${includes} -c ${in} -o ${out}",
	})
	f.AddRule(ninja.Rule{
		// A .h compiled standalone (header self-sufficiency check): -x c++ tells
		// clang++ it's a C++ header, avoiding the deprecated "treating 'c-header'
		// input as 'c++-header'" warning.
		Name: "cxx_header", Description: "CXX ${out}", Depfile: "${out}.d", Deps: "gcc",
		Command: "${cxx} -MMD -MF ${out}.d ${cppflags} ${cxxflags} ${cxx_warnings} ${defs} ${extra_compile_flags} ${includes} -x c++ -c ${in} -o ${out}",
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

// emitMSVCRules emits the MSVC compile/archive/link rules. cl's /showIncludes is
// parsed natively by ninja (deps=msvc + msvc_deps_prefix), so no depfile. /Tc and
// /Tp force the C / C++ language (so a .h self-sufficiency check compiles as C++).
// /external:W0 keeps warnings out of /external:I (vcpkg/system) headers.
func (gen *Generator) emitMSVCRules(f *ninja.File) {
	const inc = "Note: including file:"
	f.AddRule(ninja.Rule{
		Name: "cc", Description: "CC ${out}", Deps: "msvc", MsvcDepsPrefix: inc,
		Command: "${cc} /nologo /c /showIncludes ${cppflags} ${cflags} ${c_warnings} /external:W0 ${defs} ${extra_compile_flags} ${includes} /Fo${out} /Tc${in}",
	})
	f.AddRule(ninja.Rule{
		Name: "cxx", Description: "CXX ${out}", Deps: "msvc", MsvcDepsPrefix: inc,
		Command: "${cxx} /nologo /c /showIncludes /EHsc ${cppflags} ${cxxflags} ${cxx_warnings} /external:W0 ${defs} ${extra_compile_flags} ${includes} /Fo${out} /Tp${in}",
	})
	f.AddRule(ninja.Rule{
		Name: "cxx_header", Description: "CXX ${out}", Deps: "msvc", MsvcDepsPrefix: inc,
		Command: "${cxx} /nologo /c /showIncludes /EHsc ${cppflags} ${cxxflags} ${cxx_warnings} /external:W0 ${defs} ${extra_compile_flags} ${includes} /Fo${out} /Tp${in}",
	})
	f.AddRule(ninja.Rule{
		Name: "ar", Description: "LIB ${out}",
		Command: "${ar} /nologo /OUT:${out} ${in}",
	})
	f.AddRule(ninja.Rule{
		Name: "link", Description: "LINK ${out}",
		Command: "${link} /nologo ${in} ${libs} ${ldflags} /OUT:${out}",
	})
}

// prebuiltLib resolves a prebuilt_cc_library's archive (workspace-relative). The
// library lives at <pkg>/<libpath>/lib<name><suffix>, where libpath is the
// target's libpath_pattern or the default "lib${bits}" (Blade's
// cc_library_config.prebuilt_libpath_pattern), with ${bits}/${arch}/${profile}
// substituted. Static (.a) is preferred over the dynamic library; "" when neither
// exists (Blade tolerates an unused, absent prebuilt).
func (gen *Generator) prebuiltLib(n *graph.Node) string {
	pattern := n.Target.AttrString("libpath_pattern")
	if pattern == "" {
		pattern = "lib${bits}"
	}
	libdir := strings.NewReplacer(
		"${bits}", "64",
		"${arch}", prebuiltArch(),
		"${profile}", gen.Profile,
	).Replace(pattern)
	base := path.Join(n.Target.Package, libdir)
	dyn := ".so"
	if gen.Tc.OS == "darwin" {
		dyn = ".dylib"
	}
	for _, suffix := range []string{".a", dyn} {
		rel := path.Join(base, "lib"+n.Target.Name+suffix)
		if _, err := os.Stat(filepath.Join(gen.Root, rel)); err == nil {
			return rel
		}
	}
	return ""
}

// prebuiltArch maps Go's GOARCH to Blade's ${arch} spelling.
func prebuiltArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return runtime.GOARCH
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
			// Generated resource C isn't warning-clean; suppress warnings.
			Implicit: []string{indexH}, Vars: map[string]string{"includes": inc, "c_warnings": ""},
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
		// Resolve flare's single-overlay-triplet protoc glob to blade-go's concrete
		// compat dir. flare/tools/build_rules.bld hardcodes a shell glob
		// ".cache/vcpkg/installed/blade-*/tools/protobuf/protoc", assuming exactly
		// one "blade-*" triplet. When blade-go shares the build dir with Python
		// Blade (build_release), that dir holds several blade-* triplets, so the
		// glob would expand to multiple protoc paths and protoc parses a protoc
		// binary as a .proto. Emitting the concrete blade-go path avoids the glob.
		".cache/vcpkg/installed/blade-*/", ".cache/vcpkg/installed/blade-go/",
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
			// protoc output isn't warning-clean; suppress warnings like Blade.
			Vars: map[string]string{"includes": inc, "cxx_warnings": ""},
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
	// Blade's ForeignCcLibrary picks its archive by convention -- lib<name>.a for
	// the target's own name -- or by an explicit `static_library` attr (#1262).
	// One autotools/cmake gen_rule can emit MANY archives (gperftools_build builds
	// libtcmalloc.a, libprofiler.a, ... shared by sibling foreign_cc_library
	// targets tcmalloc, profiler, ...), so selecting "any .a" links the wrong one
	// (or none) -- e.g. a consumer of :profiler missed libprofiler.a, leaving
	// ProfilerStart/Stop/Flush undefined. Match the target's archive by basename.
	want := gen.Tc.StaticLib(n.Target.Name)
	if sl := n.Target.AttrString("static_library"); sl != "" {
		want = path.Base(sl)
	}
	for _, dep := range n.Deps {
		if dep.Target.Type != "gen_rule" {
			continue
		}
		if a := pickForeignArchive(want, genFilesOf[dep]); a != "" {
			lib = a
		}
		pkg := dep.Target.Package
		incs = append(incs, path.Join(gen.BuildDir, path.Dir(pkg)))
		if sei := dep.Target.AttrString("system_export_incs"); sei != "" {
			incs = append(incs, path.Join(gen.BuildDir, pkg, sei))
		}
	}
	return lib, incs
}

// pickForeignArchive selects a foreign_cc_library's archive from a gen_rule's
// outputs (name->path). It prefers the one whose basename is `want`
// (lib<target>.a) so sibling foreign_cc_library targets sharing one multi-archive
// gen_rule each link their own; it falls back to the sole archive when there is
// exactly one and none matched (single-archive builds whose basename differs).
func pickForeignArchive(want string, files map[string]string) string {
	var only string
	nA := 0
	for _, p := range files {
		if !strings.HasSuffix(p, ".a") {
			continue
		}
		nA++
		only = p
		if path.Base(p) == want {
			return p
		}
	}
	if nA == 1 {
		return only
	}
	return ""
}

// emitCompiles emits one compile edge per source. It returns the linkable object
// paths and, separately, the header self-sufficiency-check objects: a header in
// srcs is compiled standalone (blade's check) but its object must NOT be archived
// or linked -- it is (often) an empty object ld rejects as "not a mach-o file".
// implicitHdrs (generated proto headers in the closure) order codegen before
// compilation; a src naming a generated file compiles from its build-dir path.
func (gen *Generator) emitCompiles(f *ninja.File, n *graph.Node, implicitHdrs []string, genFiles map[string]string) (objs, checkObjs []string) {
	inc := gen.includes(n)
	defs := gen.defineFlags(n) // per-target preprocessor defines (cc_test variants)
	// A target with `warning = 'no'` (flare's thirdparty source builds like blake3)
	// opts out of the project's cc_config warnings -- override the vars to empty.
	noWarn := n.Target.AttrString("warning") == "no"
	for _, src := range n.Target.AttrStrings("srcs") {
		srcPath := path.Join(n.Target.Package, src)
		if gp, ok := genFiles[src]; ok {
			srcPath = gp
		}
		obj := path.Join(gen.BuildDir, n.Target.Package, n.Target.Name+".objs", src) + gen.Tc.ObjSuffix()
		rule := "cc"
		// Headers compile standalone with the target's language, which for cc_*
		// is C++ (they pull in <memory>); only a bare .c stays on the C compiler.
		// A header uses the `cxx_header` rule (explicit -x c++) so clang++ doesn't
		// warn about "treating 'c-header' input as 'c++-header'".
		header := isHeader(src)
		switch {
		case header:
			rule = "cxx_header"
		case isCXXSource(src):
			rule = "cxx"
		}
		vars := map[string]string{"includes": inc}
		if defs != "" {
			vars["defs"] = defs
		}
		if ex := extraCompileFlags(n, src); ex != "" {
			vars["extra_compile_flags"] = ex
		}
		// Suppress warnings for `warning = 'no'` targets and for the standalone
		// header self-sufficiency check (a header may carry an intentional
		// `#warning` deprecation notice that -Werror would turn fatal; the real
		// .cc compile that includes it still gets the warnings).
		if noWarn || header {
			vars["c_warnings"] = ""
			vars["cxx_warnings"] = ""
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
	// The target's own private `incs` (Blade's attr['incs']): package-relative
	// include dirs for this target's compiles only, not propagated to consumers.
	for _, d := range n.Target.AttrStrings("incs") {
		d = normInc(n.Target.Package, d)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	visited := map[*graph.Node]bool{} // walk each node once, not once per DAG path
	var walk func(*graph.Node)
	walk = func(node *graph.Node) {
		if visited[node] {
			return
		}
		visited[node] = true
		for _, d := range node.Target.AttrStrings("export_incs") {
			d = normInc(node.Target.Package, d)
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
	// A cc_test/cc_benchmark's framework (gtest/google-benchmark) is a dep now, so
	// a vcpkg framework already shows up in transitiveVcpkgs.
	needVcpkg := len(gen.transitiveVcpkgs(n)) > 0
	if len(gen.ProtobufVcpkgs) > 0 && (n.Target.Type == "proto_library" || gen.hasProtoInClosure(n)) {
		needVcpkg = true // protobuf headers (for generated .pb.cc) live in the vcpkg tree
	}
	// vcpkg/thirdparty include dirs go through -isystem, not -I: they are external
	// headers, so their own warnings must not fire under the project's -Werror
	// (e.g. fmt's MSVC `#pragma warning` -> -Wunknown-pragmas). Matches Blade.
	var sysDirs []string
	if inc := gen.Vcpkg.IncludeDir(); inc != "" && needVcpkg {
		sysDirs = append(sysDirs, inc)
		// include_prefix ports (zlib, snappy, ...) resolve "prefix/hdr" here.
		if gen.Vcpkg.PrefixRoot != "" {
			sysDirs = append(sysDirs, gen.Vcpkg.PrefixRoot)
		}
	}
	// MSVC: -I -> /I, -isystem -> /external:I (silenced via the rule's
	// /external:W0). Quote so dirs with spaces (vcpkg trees) survive.
	reg, sys := "-I", "-isystem "
	quote := false
	if gen.Tc.IsMSVC() {
		reg, sys, quote = "/I", "/external:I ", true
	}
	var b strings.Builder
	emit := func(flag, d string) {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(flag)
		if quote {
			b.WriteByte('"')
			b.WriteString(d)
			b.WriteByte('"')
		} else {
			b.WriteString(d)
		}
	}
	for _, d := range dirs {
		emit(reg, d)
	}
	for _, d := range sysDirs {
		emit(sys, d)
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
// command.
//
// On ELF (GNU ld/gold/lld/mold), a --start-group is re-scanned to a fixpoint but
// anything outside it gets a single left-to-right pass, so a forward reference
// from one archive into a later one (curl -> ssl -> crypto, protobuf -> absl)
// only resolves if BOTH archives sit inside the same group. The target's own
// archives and the vcpkg archives therefore go into one group together -- this
// matches Blade, which collects vcpkg required_archives into usr_libs inside its
// single group. System `-l` libs follow the group (archives reference them, not
// the other way round, so one pass after the group suffices).
//
// Non-ELF linkers (Apple ld64) re-scan archives regardless, so grouping/order is
// moot there; that path is left exactly as it was (libs, then -l syslibs, then
// the vcpkg args, which may include macOS-only `-framework` flags).
func (gen *Generator) linkArgs(libs, syslibs, extra []string) string {
	var parts []string
	if gen.Tc.IsMSVC() {
		// MSVC link.exe resolves symbols across all inputs in one pass, so no
		// grouping/order dance. A `#name` syslib becomes `name.lib` (unless it
		// already carries an extension); .lib archives are positional inputs.
		parts = append(parts, libs...)
		parts = append(parts, extra...)
		for _, s := range syslibs {
			if strings.Contains(s, ".") {
				parts = append(parts, s)
			} else {
				parts = append(parts, s+".lib")
			}
		}
		return strings.Join(parts, " ")
	}
	if gen.Tc.GroupsLibraries() {
		grouped := append(append([]string{}, libs...), extra...)
		if len(grouped) > 0 {
			parts = append(parts, "-Wl,--start-group")
			parts = append(parts, grouped...)
			parts = append(parts, "-Wl,--end-group")
		}
		for _, s := range syslibs {
			parts = append(parts, "-l"+s)
		}
		return strings.Join(parts, " ")
	}
	parts = append(parts, libs...)
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

// normInc resolves an `incs`/`export_incs` entry to a workspace-relative include
// directory, mirroring Blade's _incs_to_fullpath: a leading `//` marks a
// root-relative "full path" (strip it -- flare's foreign_cc_library sets
// export_incs='//'+build_dir/.../include); anything else is relative to the
// declaring target's package. Without this a `//`-path reaches gcc as
// `-I//build_dir/...`, which the leading slash makes nonexistent -> a fatal
// -Wmissing-include-dirs under -Werror (clang silently ignores it, so it only
// bit on Linux/gcc).
func normInc(pkg, inc string) string {
	if strings.HasPrefix(inc, "//") {
		return path.Clean(inc[2:])
	}
	return path.Clean(path.Join(pkg, inc))
}

func isCXXSource(src string) bool {
	for _, ext := range []string{".cc", ".cpp", ".cxx", ".C", ".c++", ".mm"} {
		if strings.HasSuffix(src, ext) {
			return true
		}
	}
	return false
}

// extraCompileFlags returns a target's per-source extra compile flags (Blade
// issue #492). extra_cppflags apply to every C-family source; the
// language-specific set is added on top -- extra_cxxflags for C++/Objective-C++
// (.cc/.cpp/.cxx/.mm), extra_asflags for assembly (.s/.S/.asm), extra_cflags
// otherwise (C and Objective-C .m). They sit after the global cc_config flags in
// the compile command so a target can override them -- e.g. flare's crypto md5/
// sha targets pass -Wno-deprecated-declarations to silence OpenSSL 3.x's
// deprecation of the low-level HMAC/SHA APIs under the project-wide -Werror.
// Headers (compiled standalone as C++) take the C++ set, matching their rule.
func extraCompileFlags(n *graph.Node, src string) string {
	flags := append([]string{}, n.Target.AttrStrings("extra_cppflags")...)
	switch {
	case isCXXSource(src), isHeader(src):
		flags = append(flags, n.Target.AttrStrings("extra_cxxflags")...)
	case isAsmSource(src):
		flags = append(flags, n.Target.AttrStrings("extra_asflags")...)
	default:
		flags = append(flags, n.Target.AttrStrings("extra_cflags")...)
	}
	return strings.Join(flags, " ")
}

// isAsmSource reports whether src is an assembly source (the cc driver assembles
// .s/.S; .asm is MASM, routed elsewhere but classified here for extra_asflags).
func isAsmSource(src string) bool {
	for _, ext := range []string{".s", ".S", ".asm"} {
		if strings.HasSuffix(src, ext) {
			return true
		}
	}
	return false
}

// defineFlags returns a target's `defs` as -D/`/D` flags (flare's cc_test size
// variants pass e.g. defs = ['BUFFER_BLOCK_SIZE=\"4096\"']).
func (gen *Generator) defineFlags(n *graph.Node) string {
	flag := "-D"
	if gen.Tc.IsMSVC() {
		flag = "/D"
	}
	var b strings.Builder
	for i, d := range n.Target.AttrStrings("defs") {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(flag)
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
