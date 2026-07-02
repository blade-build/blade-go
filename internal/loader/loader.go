// Package loader evaluates Blade BUILD / BLADE_ROOT files, which are Starlark
// (a restricted Python dialect), and collects the declared targets and
// configuration. It is the front-end of blade-go.
package loader

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/blade-build/blade-go/internal/bladectx"
	"github.com/blade-build/blade-go/internal/config"
	"github.com/blade-build/blade-go/internal/target"
)

// ruleNames are the rule functions a BUILD file may call. Each records a target;
// rule-specific attribute validation is deferred to later phases.
var ruleNames = []string{
	"cc_library", "cc_binary", "cc_test", "cc_benchmark",
	"proto_library", "resource_library",
}

// configNames are the configuration functions a BLADE_ROOT / blade.conf may call.
var configNames = []string{
	"global_config",
	"cc_config", "cc_library_config", "cc_binary_config", "cc_test_config",
	"cc_toolchain_config", "msvc_config",
	"proto_library_config", "thrift_library_config", "vcpkg_config",
}

// Loader accumulates targets and configuration across the files it loads.
type Loader struct {
	Root     string // workspace root (absolute)
	BuildDir string // build output dir mirror (e.g. "build64_release")
	Targets  *target.Registry
	Config   *config.Config
}

// New returns a Loader rooted at the given workspace directory.
func New(root string) *Loader {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return &Loader{
		Root:     abs,
		BuildDir: "build64_release",
		Targets:  target.NewRegistry(),
		Config:   config.New(),
	}
}

func fileOptions() *syntax.FileOptions {
	// Permissive enough for real BLADE_ROOT files: top-level if/for, global
	// reassignment (`_WIN = ...; _WIN = ...`), and lambda deferred config.
	return &syntax.FileOptions{
		Set:             true,
		TopLevelControl: true,
		GlobalReassign:  true,
	}
}

// LoadConfigFile evaluates a BLADE_ROOT / blade.conf, recording its
// configuration calls. No targets are expected.
func (l *Loader) LoadConfigFile(pathname string) error {
	pre := starlark.StringDict{"blade": bladectx.ConfigModule()}
	l.addConfigBuiltins(pre)
	l.addHelperBuiltins(pre, "")
	pre["load_value"] = l.loadValueBuiltin()
	return l.exec(pathname, pre)
}

// loadValueBuiltin implements load_value(filepath): read a file (relative to the
// workspace root) holding a single Starlark literal and return its value. Used
// by BLADE_ROOT to pull large config lists out into side files.
func (l *Loader) loadValueBuiltin() *starlark.Builtin {
	return starlark.NewBuiltin("load_value", func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var rel string
		if err := starlark.UnpackArgs(b.Name(), args, kwargs, "filepath", &rel); err != nil {
			return nil, err
		}
		p := rel
		if !filepath.IsAbs(p) {
			p = filepath.Join(l.Root, rel)
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("load_value: %w", err)
		}
		v, err := starlark.EvalOptions(fileOptions(), thread, p, string(src), nil)
		if err != nil {
			return nil, fmt.Errorf("load_value(%s): %w", rel, err)
		}
		return v, nil
	})
}

// LoadBuildFile evaluates a BUILD file; its package is derived from the file's
// directory relative to the workspace root.
func (l *Loader) LoadBuildFile(pathname string) error {
	abs, err := filepath.Abs(pathname)
	if err != nil {
		return err
	}
	dir := filepath.Dir(abs)
	pkg, err := filepath.Rel(l.Root, dir)
	if err != nil {
		return err
	}
	pkg = filepath.ToSlash(pkg)
	if pkg == "." {
		pkg = ""
	}
	pre := starlark.StringDict{"blade": bladectx.BuildModule(pkg, l.BuildDir)}
	l.addRuleBuiltins(pre, pkg)
	l.addHelperBuiltins(pre, dir)
	return l.exec(pathname, pre)
}

func (l *Loader) exec(pathname string, pre starlark.StringDict) error {
	src, err := os.ReadFile(pathname)
	if err != nil {
		return err
	}
	thread := &starlark.Thread{Name: pathname}
	_, err = starlark.ExecFileOptions(fileOptions(), thread, pathname, src, pre)
	return err
}

func (l *Loader) addRuleBuiltins(pre starlark.StringDict, pkg string) {
	for _, name := range ruleNames {
		pre[name] = l.ruleBuiltin(name, pkg)
	}
}

func (l *Loader) addConfigBuiltins(pre starlark.StringDict) {
	for _, name := range configNames {
		pre[name] = l.configBuiltin(name)
	}
}

// addHelperBuiltins registers glob/enable_if. `dir` is the package's on-disk
// directory (for glob); pass "" when globbing is not applicable.
func (l *Loader) addHelperBuiltins(pre starlark.StringDict, dir string) {
	pre["enable_if"] = enableIfBuiltin()
	if dir != "" {
		pre["glob"] = globBuiltin(dir)
	}
	// `fail` and `print` come from Starlark's universe.
}

func (l *Loader) ruleBuiltin(ruleType, pkg string) *starlark.Builtin {
	return starlark.NewBuiltin(ruleType, func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		attrs := map[string]any{}
		name := ""
		for _, kv := range kwargs {
			k, _ := starlark.AsString(kv[0])
			if k == "name" {
				s, ok := starlark.AsString(kv[1])
				if !ok {
					return nil, fmt.Errorf("%s: 'name' must be a string", ruleType)
				}
				name = s
				continue
			}
			attrs[k] = toGo(kv[1])
		}
		if name == "" && len(args) > 0 {
			if s, ok := starlark.AsString(args[0]); ok {
				name = s
			}
		}
		if name == "" {
			return nil, fmt.Errorf("%s: missing required 'name'", ruleType)
		}
		t := &target.Target{Type: ruleType, Name: name, Package: pkg, Attrs: attrs, Pos: callerPos(thread)}
		if err := l.Targets.Add(t); err != nil {
			return nil, err
		}
		return starlark.None, nil
	})
}

func (l *Loader) configBuiltin(name string) *starlark.Builtin {
	return starlark.NewBuiltin(name, func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) > 0 {
			return nil, fmt.Errorf("%s: only keyword arguments are allowed", name)
		}
		attrs := map[string]any{}
		for _, kv := range kwargs {
			k, _ := starlark.AsString(kv[0])
			attrs[k] = toGo(kv[1])
		}
		l.Config.Record(name, attrs, callerPos(thread))
		return starlark.None, nil
	})
}

func enableIfBuiltin() *starlark.Builtin {
	return starlark.NewBuiltin("enable_if", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var cond, trueV starlark.Value
		var falseV starlark.Value = starlark.None
		if err := starlark.UnpackArgs(b.Name(), args, kwargs, "cond", &cond, "true_value", &trueV, "false_value?", &falseV); err != nil {
			return nil, err
		}
		if cond.Truth() {
			return trueV, nil
		}
		if falseV == starlark.None {
			return starlark.NewList(nil), nil
		}
		return falseV, nil
	})
}

func globBuiltin(dir string) *starlark.Builtin {
	return starlark.NewBuiltin("glob", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var includeV starlark.Value
		var excludeV starlark.Value = starlark.NewList(nil)
		allowEmpty := false
		if err := starlark.UnpackArgs(b.Name(), args, kwargs, "include", &includeV, "exclude?", &excludeV, "allow_empty?", &allowEmpty); err != nil {
			return nil, err
		}
		files, err := globFiles(dir, toStringSlice(includeV), toStringSlice(excludeV), allowEmpty)
		if err != nil {
			return nil, err
		}
		elems := make([]starlark.Value, len(files))
		for i, f := range files {
			elems[i] = starlark.String(f)
		}
		return starlark.NewList(elems), nil
	})
}

// globFiles returns the files under dir matching any include pattern and no
// exclude pattern, with "**" matching zero or more path components. Hidden
// files (dot-prefixed) are skipped, matching Blade.
func globFiles(dir string, includes, excludes []string, allowEmpty bool) ([]string, error) {
	set := map[string]bool{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(path.Base(rel), ".") {
			return nil
		}
		if !matchAny(rel, includes) || matchAny(rel, excludes) {
			return nil
		}
		set[rel] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	if len(out) == 0 && !allowEmpty {
		return nil, fmt.Errorf("glob(%v) returned an empty result; set allow_empty=True if intended", includes)
	}
	return out, nil
}

func matchAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if matchGlobstar(name, p) {
			return true
		}
	}
	return false
}

func matchGlobstar(name, pattern string) bool {
	return matchParts(strings.Split(name, "/"), strings.Split(pattern, "/"))
}

func matchParts(parts, pat []string) bool {
	if len(pat) == 0 {
		return len(parts) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(parts); i++ {
			if matchParts(parts[i:], pat[1:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	if ok, _ := path.Match(pat[0], parts[0]); !ok {
		return false
	}
	return matchParts(parts[1:], pat[1:])
}

// toGo converts a Starlark value to a Go value: None->nil, Bool->bool,
// Int->int64, Float->float64, String->string, List/Tuple->[]any, Dict->
// map[string]any. Other values (notably callables, i.e. config lambdas) are
// preserved as-is for later deferred evaluation.
func toGo(v starlark.Value) any {
	switch v := v.(type) {
	case starlark.NoneType:
		return nil
	case starlark.Bool:
		return bool(v)
	case starlark.Int:
		n, _ := v.Int64()
		return n
	case starlark.Float:
		return float64(v)
	case starlark.String:
		return string(v)
	case *starlark.List:
		return iterToGo(v)
	case starlark.Tuple:
		out := make([]any, 0, len(v))
		for _, e := range v {
			out = append(out, toGo(e))
		}
		return out
	case *starlark.Dict:
		m := map[string]any{}
		for _, item := range v.Items() {
			k, _ := starlark.AsString(item[0])
			m[k] = toGo(item[1])
		}
		return m
	default:
		return v
	}
}

func iterToGo(it starlark.Iterable) []any {
	iter := it.Iterate()
	defer iter.Done()
	var out []any
	var e starlark.Value
	for iter.Next(&e) {
		out = append(out, toGo(e))
	}
	return out
}

// toStringSlice normalizes a Starlark value to a string slice, accepting a bare
// string (Blade's str-or-list convention) or an iterable of strings.
func toStringSlice(v starlark.Value) []string {
	if s, ok := starlark.AsString(v); ok {
		return []string{s}
	}
	it, ok := v.(starlark.Iterable)
	if !ok {
		return nil
	}
	iter := it.Iterate()
	defer iter.Done()
	var out []string
	var e starlark.Value
	for iter.Next(&e) {
		if s, ok := starlark.AsString(e); ok {
			out = append(out, s)
		}
	}
	return out
}

func callerPos(thread *starlark.Thread) string {
	cs := thread.CallStack()
	if len(cs) > 0 {
		return cs[len(cs)-1].Pos.String()
	}
	return ""
}
