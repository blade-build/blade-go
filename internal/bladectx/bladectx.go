// Package bladectx builds the `blade` object exposed to BLADE_ROOT / BUILD
// files, mirroring Blade's safe `blade.*` DSL surface.
//
// This is the config-phase shape (enough to load flare's BLADE_ROOT): host
// info, environment access, and a curated `blade.path`. Build-phase-only
// handles (cc_toolchain, current_source_dir, ...) are added in a later phase;
// they are referenced only inside deferred `lambda blade: ...` config values,
// which are not evaluated at load time.
package bladectx

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

func errNoKwargs(b *starlark.Builtin) error {
	return fmt.Errorf("%s: unexpected keyword arguments", b.Name())
}

func argNotString(b *starlark.Builtin, i int) error {
	return fmt.Errorf("%s: argument %d is not a string", b.Name(), i+1)
}

// HostOS returns Blade's name for the host OS ("linux", "darwin", "windows").
func HostOS() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "darwin"
	default:
		return "linux"
	}
}

// HostArch returns Blade's name for the host architecture.
func HostArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

// commonMembers are the `blade.*` members available in both phases.
func commonMembers() starlark.StringDict {
	return starlark.StringDict{
		"host_os":   starlark.String(HostOS()),
		"host_arch": starlark.String(HostArch()),
		"getenv":    starlark.NewBuiltin("blade.getenv", getenv),
		"path":      pathModule(),
	}
}

// ConfigModule returns the `blade` value for the config phase (BLADE_ROOT /
// blade.conf). It deliberately omits cc_toolchain: like Blade, the toolchain
// isn't chosen yet, so config code must reach it through a deferred
// `lambda blade: ...`, whose body runs in the build phase.
func ConfigModule() starlark.Value {
	return starlarkstruct.FromStringDict(starlarkstruct.Default, commonMembers())
}

// ConfigGetter reads a configuration item, backing blade.config.get_item.
type ConfigGetter interface {
	GetItem(section, item string) (any, bool)
}

// BuildModule returns the `blade` value for the build phase (BUILD files and
// .bld extensions) in the given package. It adds a host-derived cc_toolchain
// proxy, the current source/target directories, and blade.config (backed by
// cfg, which may be nil).
func BuildModule(pkg, buildDir string, cfg ConfigGetter) starlark.Value {
	members := commonMembers()
	members["cc_toolchain"] = ccToolchain()
	// current_source_dir / current_target_dir must reflect the package of the
	// BUILD file being executed, not the one BuildModule was constructed with:
	// a .bld macro (foreign_build's cmake_build) runs on the consuming BUILD
	// file's thread. Read the package from the thread-local the loader sets,
	// falling back to the static pkg (blade.conf / tests that set no thread pkg).
	members["current_source_dir"] = pkgDirBuiltin("blade.current_source_dir", "", pkg)
	members["current_target_dir"] = pkgDirBuiltin("blade.current_target_dir", buildDir, pkg)
	members["config"] = configModule(cfg)
	members["build_type"] = starlark.String("release")
	members["build_type_is_debug"] = starlark.NewBuiltin("blade.build_type_is_debug", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		return starlark.False, nil
	})
	return starlarkstruct.FromStringDict(starlarkstruct.Default, members)
}

func configModule(cfg ConfigGetter) starlark.Value {
	getItem := starlark.NewBuiltin("blade.config.get_item", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var section, item string
		if err := starlark.UnpackArgs(b.Name(), args, kwargs, "section", &section, "item", &item); err != nil {
			return nil, err
		}
		if cfg != nil {
			if v, ok := cfg.GetItem(section, item); ok {
				return GoToStarlark(v), nil
			}
		}
		return starlark.None, nil
	})
	return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{"get_item": getItem})
}

// GoToStarlark converts a Go value (as produced by the loader's Starlark->Go
// conversion) back into a Starlark value.
func GoToStarlark(v any) starlark.Value {
	switch v := v.(type) {
	case nil:
		return starlark.None
	case bool:
		return starlark.Bool(v)
	case int64:
		return starlark.MakeInt64(v)
	case int:
		return starlark.MakeInt(v)
	case float64:
		return starlark.Float(v)
	case string:
		return starlark.String(v)
	case []any:
		elems := make([]starlark.Value, len(v))
		for i, e := range v {
			elems[i] = GoToStarlark(e)
		}
		return starlark.NewList(elems)
	case map[string]any:
		d := starlark.NewDict(len(v))
		for k, e := range v {
			_ = d.SetKey(starlark.String(k), GoToStarlark(e))
		}
		return d
	case starlark.Value:
		return v
	default:
		return starlark.None
	}
}

func ccToolchain() starlark.Value {
	dynSuffix := ".so"
	if HostOS() == "darwin" {
		dynSuffix = ".dylib"
	}
	return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"target_os":          starlark.String(HostOS()),
		"target_arch":        starlark.String(HostArch()),
		"dynamic_lib_suffix": starlark.String(dynSuffix),
		"tool":               toolBuiltin(),
	})
}

// toolBuiltin implements blade.cc_toolchain.tool(name): the resolved compiler /
// archiver the foreign-build macros pass to configure/cmake. Matches the
// env-or-default resolution the Go toolchain uses; unknown names return None
// (the macros fall back with `or 'cc'`).
func toolBuiltin() *starlark.Builtin {
	return starlark.NewBuiltin("blade.cc_toolchain.tool", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var name string
		if err := starlark.UnpackArgs(b.Name(), args, kwargs, "name", &name); err != nil {
			return nil, err
		}
		switch name {
		case "cc":
			return starlark.String(pickEnv("CC", "cc")), nil
		case "cxx":
			return starlark.String(pickEnv("CXX", "c++")), nil
		case "ar":
			return starlark.String(pickEnv("AR", "ar")), nil
		}
		return starlark.None, nil
	})
}

func pickEnv(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

// BuildTarget returns the `build_target` object BUILD files use for
// platform-conditional sources (flare uses build_target.arch).
func BuildTarget() starlark.Value {
	return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"arch": starlark.String(HostArch()),
		"bits": starlark.MakeInt(64),
	})
}

func dirBuiltin(name, result string) *starlark.Builtin {
	return starlark.NewBuiltin(name, func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
			return nil, err
		}
		return starlark.String(result), nil
	})
}

// threadPkgKey mirrors the loader's thread-local key for the executing BUILD
// file's package (kept as a literal to avoid a bladectx->loader import cycle).
const threadPkgKey = "blade.pkg"

// pkgDirBuiltin returns blade.current_{source,target}_dir: at call time it joins
// prefix ("" for source, the build dir for target) with the thread's current
// package, so the path tracks whichever BUILD file is executing.
func pkgDirBuiltin(name, prefix, staticPkg string) *starlark.Builtin {
	return starlark.NewBuiltin(name, func(t *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
			return nil, err
		}
		pkg := staticPkg
		if v, ok := t.Local(threadPkgKey).(string); ok {
			pkg = v
		}
		return starlark.String(path.Join(prefix, pkg)), nil
	})
}

// getenv(name, default=None) reads an environment variable.
func getenv(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	var def starlark.Value = starlark.None
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "name", &name, "default?", &def); err != nil {
		return nil, err
	}
	if v, ok := os.LookupEnv(name); ok {
		return starlark.String(v), nil
	}
	return def, nil
}

func pathModule() starlark.Value {
	str1 := func(name string, fn func(string) string) *starlark.Builtin {
		return starlark.NewBuiltin(name, func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var p string
			if err := starlark.UnpackArgs(b.Name(), args, kwargs, "path", &p); err != nil {
				return nil, err
			}
			return starlark.String(fn(p)), nil
		})
	}
	members := starlark.StringDict{
		"join":     starlark.NewBuiltin("path.join", pathJoin),
		"exists":   starlark.NewBuiltin("path.exists", pathExists),
		"relpath":  starlark.NewBuiltin("path.relpath", pathRelpath),
		"splitext": starlark.NewBuiltin("path.splitext", pathSplitext),
		"abspath":  str1("path.abspath", func(p string) string { a, _ := filepath.Abs(p); return a }),
		"dirname":  str1("path.dirname", path.Dir),
		"basename": str1("path.basename", path.Base),
		"normpath": str1("path.normpath", path.Clean),
	}
	return starlarkstruct.FromStringDict(starlarkstruct.Default, members)
}

func pathJoin(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(kwargs) > 0 {
		return nil, errNoKwargs(b)
	}
	parts := make([]string, 0, len(args))
	for i, a := range args {
		s, ok := starlark.AsString(a)
		if !ok {
			return nil, argNotString(b, i)
		}
		parts = append(parts, s)
	}
	return starlark.String(path.Join(parts...)), nil
}

func pathExists(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var p string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "path", &p); err != nil {
		return nil, err
	}
	_, err := os.Stat(p)
	return starlark.Bool(err == nil), nil
}

func pathRelpath(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var target, base string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "path", &target, "start?", &base); err != nil {
		return nil, err
	}
	if base == "" {
		base = "."
	}
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return nil, err
	}
	return starlark.String(filepath.ToSlash(rel)), nil
}

func pathSplitext(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var p string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "path", &p); err != nil {
		return nil, err
	}
	ext := path.Ext(p)
	root := strings.TrimSuffix(p, ext)
	return starlark.Tuple{starlark.String(root), starlark.String(ext)}, nil
}
