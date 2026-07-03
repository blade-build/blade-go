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
	"regexp"
	"sort"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"

	"github.com/blade-build/blade-go/internal/bladectx"
	"github.com/blade-build/blade-go/internal/config"
	"github.com/blade-build/blade-go/internal/target"
)

// ruleNames are the rule functions a BUILD file may call. Each records a target;
// rule-specific attribute validation is deferred to later phases.
var ruleNames = []string{
	"cc_library", "cc_binary", "cc_test", "cc_benchmark",
	"proto_library", "resource_library", "gen_rule",
	"foreign_cc_library", "prebuilt_cc_library",
}

// thread-local keys for the currently-executing BUILD file's context.
const (
	threadPkg = "blade.pkg"
	threadDir = "blade.dir"
)

func pkgOf(thread *starlark.Thread) string {
	if v, ok := thread.Local(threadPkg).(string); ok {
		return v
	}
	return ""
}

func dirOf(thread *starlark.Thread) string {
	if v, ok := thread.Local(threadDir).(string); ok {
		return v
	}
	return ""
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
	BuildDir string // build output dir mirror (e.g. "build_release")
	Targets  *target.Registry
	Config   *config.Config

	extCache map[string]starlark.StringDict // load()'ed extensions by path
	incCache map[string]starlark.StringDict // include()'ed .bld globals by path
}

// New returns a Loader rooted at the given workspace directory.
func New(root string) *Loader {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return &Loader{
		Root:     abs,
		BuildDir: "build_release",
		Targets:  target.NewRegistry(),
		Config:   config.New(),
		extCache: map[string]starlark.StringDict{},
		incCache: map[string]starlark.StringDict{},
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
	pre["enable_if"] = enableIfBuiltin()
	pre["load_value"] = l.loadValueBuiltin()
	return l.exec(pathname, "", "", pre)
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
	pre := l.buildEnv()
	return l.exec(pathname, pkg, dir, pre)
}

// buildEnv is the predeclared environment for BUILD files: rules, native, the
// blade context, and helpers. Rules read their package from the thread, so the
// same builtins work when invoked from a macro loaded via load().
func (l *Loader) buildEnv() starlark.StringDict {
	pre := starlark.StringDict{
		"blade":        bladectx.BuildModule("", l.BuildDir, l.Config),
		"native":       l.nativeModule(),
		"build_target": bladectx.BuildTarget(),
	}
	l.addRuleBuiltins(pre)
	l.addHelperBuiltins(pre)
	return pre
}

func (l *Loader) exec(pathname, pkg, dir string, pre starlark.StringDict) error {
	src, err := os.ReadFile(pathname)
	if err != nil {
		return err
	}
	if err := l.applyIncludes(string(src), filepath.Dir(pathname), pre); err != nil {
		return fmt.Errorf("%s: %w", pathname, err)
	}
	thread := &starlark.Thread{Name: pathname, Load: l.loadExtension}
	thread.SetLocal(threadPkg, pkg)
	thread.SetLocal(threadDir, dir)
	_, err = starlark.ExecFileOptions(fileOptions(), thread, pathname, src, pre)
	return err
}

// nativeModule exposes the rule builtins as `native.<rule>` for use inside .bld
// macros (e.g. cc_flare_library calling native.gen_rule / native.cc_library).
func (l *Loader) nativeModule() starlark.Value {
	m := starlark.StringDict{}
	for _, name := range ruleNames {
		m[name] = l.ruleBuiltin(name)
	}
	return starlarkstruct.FromStringDict(starlarkstruct.Default, m)
}

// loadExtension implements Starlark load(): evaluate a "//path.bld" extension
// once and return its globals. Extensions see `native`, `blade`, and helpers.
func (l *Loader) loadExtension(_ *starlark.Thread, module string) (starlark.StringDict, error) {
	rel := strings.TrimPrefix(module, "//")
	p := filepath.Join(l.Root, filepath.FromSlash(rel))
	if g, ok := l.extCache[p]; ok {
		return g, nil
	}
	src, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("load(%q): %w", module, err)
	}
	pre := starlark.StringDict{
		"blade":      bladectx.BuildModule("", l.BuildDir, l.Config),
		"native":     l.nativeModule(),
		"enable_if":  enableIfBuiltin(),
		"isinstance": isinstanceBuiltin(),
	}
	et := &starlark.Thread{Name: p, Load: l.loadExtension}
	globals, err := starlark.ExecFileOptions(fileOptions(), et, p, src, pre)
	if err != nil {
		return nil, fmt.Errorf("load(%q): %w", module, err)
	}
	l.extCache[p] = globals
	return globals, nil
}

// includeRe matches blade's include('//path.bld') directive. Blade's include()
// splices a .bld's top-level defs into the calling file's namespace (unlike the
// selective load()), so we pre-scan for it and merge those globals into the
// predeclared environment before the file is compiled.
var includeRe = regexp.MustCompile(`(?m)^\s*include\(\s*['"]([^'"]+)['"]\s*\)`)

// applyIncludes finds include() directives in src, evaluates each referenced
// .bld with the same predeclared environment (so its macros see bare gen_rule,
// blade, isinstance, ...), and merges the resulting globals into pre. A no-op
// include builtin satisfies the runtime call itself (the work is done here).
func (l *Loader) applyIncludes(src, baseDir string, pre starlark.StringDict) error {
	pre["include"] = noopInclude()
	for _, m := range includeRe.FindAllStringSubmatch(src, -1) {
		globals, err := l.includeGlobals(m[1], baseDir)
		if err != nil {
			return err
		}
		for k, v := range globals {
			pre[k] = v
		}
	}
	return nil
}

// resolveInclude maps an include() argument to an absolute path: a "//path" is
// workspace-root-relative, anything else (e.g. "../foreign_build.bld") is
// relative to the including file's directory.
func (l *Loader) resolveInclude(module, baseDir string) string {
	if rest, ok := strings.CutPrefix(module, "//"); ok {
		return filepath.Join(l.Root, filepath.FromSlash(rest))
	}
	return filepath.Join(baseDir, filepath.FromSlash(module))
}

// includeGlobals evaluates an included .bld (recursively resolving its own
// includes, relative to its own directory) with the full BUILD predeclared
// environment and returns its globals.
func (l *Loader) includeGlobals(module, baseDir string) (starlark.StringDict, error) {
	p := l.resolveInclude(module, baseDir)
	if g, ok := l.incCache[p]; ok {
		return g, nil
	}
	src, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("include(%q): %w", module, err)
	}
	pre := l.buildEnv()
	if err := l.applyIncludes(string(src), filepath.Dir(p), pre); err != nil {
		return nil, fmt.Errorf("include(%q): %w", module, err)
	}
	t := &starlark.Thread{Name: p, Load: l.loadExtension}
	globals, err := starlark.ExecFileOptions(fileOptions(), t, p, src, pre)
	if err != nil {
		return nil, fmt.Errorf("include(%q): %w", module, err)
	}
	l.incCache[p] = globals
	return globals, nil
}

func noopInclude() *starlark.Builtin {
	return starlark.NewBuiltin("include", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		return starlark.None, nil
	})
}

func (l *Loader) addRuleBuiltins(pre starlark.StringDict) {
	for _, name := range ruleNames {
		pre[name] = l.ruleBuiltin(name)
	}
}

func (l *Loader) addConfigBuiltins(pre starlark.StringDict) {
	for _, name := range configNames {
		pre[name] = l.configBuiltin(name)
	}
}

// addHelperBuiltins registers glob/enable_if. glob reads the current package's
// on-disk directory from the thread. (`fail`/`print` come from the universe.)
func (l *Loader) addHelperBuiltins(pre starlark.StringDict) {
	pre["enable_if"] = enableIfBuiltin()
	pre["glob"] = globBuiltin()
	pre["isinstance"] = isinstanceBuiltin()
}

func (l *Loader) ruleBuiltin(ruleType string) *starlark.Builtin {
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
		t := &target.Target{Type: ruleType, Name: name, Package: pkgOf(thread), Attrs: attrs, Pos: callerPos(thread)}
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

// isinstanceBuiltin provides Python's isinstance(obj, type) for BUILD/.bld code
// (Starlark has no isinstance). The type argument is a universe type constructor
// (str/list/dict/int/bool/tuple/float), matching how BUILD authors write it.
func isinstanceBuiltin() *starlark.Builtin {
	typeName := map[string]string{
		"str": "string", "list": "list", "dict": "dict",
		"int": "int", "bool": "bool", "tuple": "tuple", "float": "float",
	}
	return starlark.NewBuiltin("isinstance", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var obj, typ starlark.Value
		if err := starlark.UnpackArgs(b.Name(), args, kwargs, "obj", &obj, "type", &typ); err != nil {
			return nil, err
		}
		tb, ok := typ.(*starlark.Builtin)
		if !ok {
			return nil, fmt.Errorf("isinstance: type must be one of str/list/dict/int/bool/tuple/float")
		}
		want, ok := typeName[tb.Name()]
		if !ok {
			return nil, fmt.Errorf("isinstance: unsupported type %q", tb.Name())
		}
		return starlark.Bool(obj.Type() == want), nil
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

func globBuiltin() *starlark.Builtin {
	return starlark.NewBuiltin("glob", func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		dir := dirOf(thread)
		if dir == "" {
			return nil, fmt.Errorf("glob is only available in a BUILD file")
		}
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

// callerPos returns the "file:line:col" of the BUILD/config call site. The
// innermost frame while a rule runs is the Go builtin itself (Pos filename
// "<builtin>"), so walk outward to the first frame with a real source file --
// the BUILD line where the rule was invoked.
func callerPos(thread *starlark.Thread) string {
	cs := thread.CallStack()
	for i := len(cs) - 1; i >= 0; i-- {
		if fn := cs[i].Pos.Filename(); fn != "" && fn != "<builtin>" {
			return cs[i].Pos.String()
		}
	}
	return ""
}
