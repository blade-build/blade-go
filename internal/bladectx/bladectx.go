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

// BuildModule returns the `blade` value for the build phase (BUILD files) in the
// given package. It adds a host-derived cc_toolchain proxy and the current
// source/target directories.
//
// Phase 1 note: cc_toolchain reports the host os/arch (real toolchain selection,
// incl. cross-compile and MSVC, arrives with the cc backend). That is enough for
// the platform conditionals BUILD files use at load time.
func BuildModule(pkg, buildDir string) starlark.Value {
	members := commonMembers()
	members["cc_toolchain"] = ccToolchain()
	members["current_source_dir"] = dirBuiltin("blade.current_source_dir", pkg)
	members["current_target_dir"] = dirBuiltin("blade.current_target_dir", path.Join(buildDir, pkg))
	return starlarkstruct.FromStringDict(starlarkstruct.Default, members)
}

func ccToolchain() starlark.Value {
	return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"target_os":   starlark.String(HostOS()),
		"target_arch": starlark.String(HostArch()),
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
