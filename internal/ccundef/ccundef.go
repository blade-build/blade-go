// Package ccundef implements Blade's static undefined-symbol check
// (cc_check_undefined, blade-build#1225): for each cc_library, every undefined
// external symbol must be resolvable from the library's own archive, one of its
// declared deps' archives, the system libraries a final link pulls in, or a
// residual/allow-list regex. Anything left is a missing dependency.
//
// It works without a real link: one `nm -P -g` per archive, set arithmetic, and
// regex full-match. Cheap and precise.
package ccundef

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Severity controls reporting.
type Severity int

const (
	Off Severity = iota
	Warn
	Error
)

// SeverityFromBlade maps cc_library_config.check_undefined_severity: "error"
// fails; "debug" is silent (Off); everything else (incl. the default "warning")
// is Warn.
func SeverityFromBlade(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return Error
	case "debug":
		return Off
	default:
		return Warn
	}
}

// Issue is one cc_library with symbols not covered by its declared deps.
type Issue struct {
	Target    string   // "//pkg:name"
	TargetPos string   // BUILD location for the fix-it note
	Symbols   []string // the unresolved (sorted) symbols
	Sev       Severity
}

// residualBaseline: compiler-/linker-injected names not exported by any library
// (C++ ABI typeinfo/vtable/guard/operator-new, PIC/TLS bootstrap, coverage +
// sanitizer runtimes). Ported verbatim from Blade's builtin_tools.
var residualBaseline = compileAll(
	`_?_ZT[ISVTCWH]\w*`,
	`_?_ZG[VRr]\w*`,
	`_?_Z(?:nw|na|dl|da)[a-zA-Z0-9_]*`,
	`_?__dso_handle`,
	`_?__cxx_global_(?:array|var)_init\w*`,
	`_GLOBAL_OFFSET_TABLE_`,
	`_?_tlv_\w+`,
	`_?__tlv_\w+`,
	`__tls_get_addr`,
	`__tls_get_offset`,
	`_*llvm_gc(?:ov|da)_\w*`,
	`_*__gcov_\w*`,
	`_*__(?:a|ub|t|m|l)san_\w*`,
	`_*__sanitizer_\w*`,
	`_*Annotate\w*`,
	`_*RunningOnValgrind`,
	// -fstack-protector's guard/handler: __stack_chk_guard is a data symbol the
	// dynamic loader provides at runtime (it shows as U in libc.so.6 and the
	// non-shared helper alike), so no scannable library defines it.
	`_?__stack_chk_\w+`,
)

func compileAll(pats ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(pats))
	for i, p := range pats {
		out[i] = regexp.MustCompile("^(?:" + p + ")$")
	}
	return out
}

// CompileAllow compiles user allow_undefined patterns (full-match). A bad
// pattern is skipped (best-effort, like a lenient allowlist).
func CompileAllow(pats []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range pats {
		if re, err := regexp.Compile("^(?:" + p + ")$"); err == nil {
			out = append(out, re)
		}
	}
	return out
}

// NmExternals returns the (undefined, defined) external symbol sets of an
// archive/shared-lib via `nm -P -g` (POSIX, external-only). Type letters: U =
// undefined; lowercase w/v = weak-undefined (ambient, ignored); lowercase u =
// unique-global (defined); any uppercase = defined; other lowercase = local
// (ignored). ELF `.so` needs -D for the dynamic table. nm failure (no symbol
// table, missing file) yields empty sets, not an error.
func NmExternals(nm, lib string) (undef, defined map[string]bool) {
	undef, defined = map[string]bool{}, map[string]bool{}
	if nm == "" {
		nm = "nm"
	}
	flags := []string{"-P", "-g"}
	base := filepath.Base(lib)
	if strings.HasSuffix(base, ".so") || strings.Contains(base, ".so.") {
		flags = append([]string{"-D"}, flags...)
	}
	out, err := exec.Command(nm, append(flags, lib)...).Output()
	if err != nil {
		return undef, defined
	}
	for _, raw := range strings.Split(string(out), "\n") {
		f := strings.Fields(raw)
		if len(f) < 2 {
			continue
		}
		name, ty := f[0], f[1]
		if strings.HasSuffix(ty, ":") { // "archive[member]:" header
			continue
		}
		// Strip an ELF symbol-version suffix: a shared lib's dynamic table tags
		// defined symbols as `sym@@GLIBCXX_3.4` (default) or `sym@VER`, but the
		// undefined reference in a consumer object is the bare `sym`. Without this
		// the system baseline (libstdc++/libgcc are versioned) never matches the
		// std C++ symbols a C++ object references, flagging every C++ target. macOS
		// .tbd stubs are unversioned, so this only bit on Linux/ELF.
		if i := strings.IndexByte(name, '@'); i >= 0 {
			name = name[:i]
		}
		switch {
		case ty == "U":
			undef[name] = true
		case ty == "w" || ty == "v":
			// weak undefined: linker may leave unresolved. ambient.
		case ty == "u":
			defined[name] = true // unique global (defined)
		case ty == "i" || ty == "I":
			// GNU indirect function (IFUNC): glibc implements memcpy/memmove and
			// many str*/mem* routines this way; nm marks a defined IFUNC with a
			// lowercase `i` regardless of binding, which the uppercase=defined rule
			// below would otherwise miss, flagging every memcpy caller.
			defined[name] = true
		case ty == strings.ToUpper(ty) && ty != strings.ToLower(ty):
			defined[name] = true // any uppercase letter: defined external
		}
	}
	return undef, defined
}

// covered reports whether sym matches any baseline or allow pattern.
func covered(sym string, allow []*regexp.Regexp) bool {
	for _, re := range residualBaseline {
		if re.MatchString(sym) {
			return true
		}
	}
	for _, re := range allow {
		if re.MatchString(sym) {
			return true
		}
	}
	return false
}

// Unresolved returns the target's undefined symbols not covered by ownDefined,
// depDefined, system, the residual baseline, or the allow patterns (sorted).
func Unresolved(undef, ownDefined map[string]bool, depDefined, system map[string]bool, allow []*regexp.Regexp) []string {
	var out []string
	for s := range undef {
		if ownDefined[s] || depDefined[s] || system[s] || covered(s, allow) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Format renders an issue as a GCC-style diagnostic (severity word supplied).
func (i Issue) Format(severity string) string {
	loc := i.TargetPos
	if loc == "" {
		loc = i.Target
	}
	head := i.Target + ": " + plural(len(i.Symbols)) + " not covered by declared deps"
	var b strings.Builder
	b.WriteString(loc + ": " + severity + ": " + head + " [cc-check-undefined]")
	n := len(i.Symbols)
	if n > 20 {
		n = 20
	}
	for _, s := range i.Symbols[:n] {
		b.WriteString("\n    " + s)
	}
	if len(i.Symbols) > 20 {
		b.WriteString("\n    ... and more")
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return "1 undefined symbol"
	}
	return itoa(n) + " undefined symbols"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// SystemSymbols returns the union of defined symbols from the system libraries a
// final link implicitly pulls in (libc++/libSystem on macOS; libstdc++/libc/
// libm/libpthread/... on Linux). goos is the target OS, cc the C compiler driver
// (for `-print-file-name` on Linux). Best-effort and conservative: an over-broad
// system set only *reduces* findings, never causes a false positive.
func SystemSymbols(cc, goos, nm string) map[string]bool {
	syms := map[string]bool{}
	if goos == "darwin" {
		sdk := macSDK()
		if sdk == "" {
			return syms
		}
		for _, name := range []string{"libc++", "libSystem"} {
			for _, ext := range []string{".tbd", ".B.tbd"} {
				p := filepath.Join(sdk, "usr", "lib", name+ext)
				if data, err := os.ReadFile(p); err == nil {
					for _, s := range tbdSymRE.FindAllString(string(data), -1) {
						syms[s] = true
					}
					break
				}
			}
		}
		return syms
	}
	// Linux/ELF: resolve each system lib via the compiler driver, then nm it.
	if cc == "" {
		cc = "cc"
	}
	scan := func(p string) {
		_, def := NmExternals(nm, p)
		for s := range def {
			syms[s] = true
		}
	}
	resolve := func(cand string) string {
		out, err := exec.Command(cc, "-print-file-name="+cand).Output()
		if err != nil {
			return ""
		}
		p := strings.TrimSpace(string(out))
		if p == "" || p == cand { // driver echoes input when unknown
			return ""
		}
		if _, err := os.Stat(p); err != nil {
			return ""
		}
		return p
	}
	for _, alias := range []string{"stdc++", "c", "m", "pthread", "dl", "rt", "gcc_s"} {
		// Scan EVERY resolvable spelling, not just the first: the versioned
		// `.so.6`/`.so.1` is the real ELF (libgcc_s.so.1 defines _Unwind_Resume;
		// its unversioned `libgcc_s.so` is a script naming it by a *relative* path
		// we can't follow), while the unversioned `.so` is often a linker script
		// whose *absolute* members add symbols the shared object lacks -- glibc's
		// `libc.so` = GROUP ( libc.so.6 libc_nonshared.a AS_NEEDED ( ld.so ) ),
		// where libc_nonshared.a / ld.so hold wrappers and loader globals. Union
		// is safe: an over-broad system set only reduces findings.
		for _, cand := range []string{"lib" + alias + ".so", "lib" + alias + ".so.6", "lib" + alias + ".so.1"} {
			if p := resolve(cand); p != "" {
				for _, member := range followLinkerScript(p) {
					scan(member)
				}
			}
		}
	}
	// libgcc.a: the static compiler runtime -- 128-bit arithmetic (__divti3, ...)
	// and, on aarch64, the outline-atomics helpers (__aarch64_ldadd*/cas*/swp*)
	// that live here, not in the shared libgcc_s.so.
	if out, err := exec.Command(cc, "-print-libgcc-file-name").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			if _, err := os.Stat(p); err == nil {
				scan(p)
			}
		}
	}
	return syms
}

// followLinkerScript returns the real library files a GNU-ld linker script
// references, or [path] when path is a plain ELF object / symlink. `gcc
// -print-file-name=libc.so` yields such a script on glibc; its GROUP/INPUT names
// the shared object plus static helpers (and an AS_NEEDED loader) whose symbols
// belong in the system baseline. A real library starts with the ELF magic; a
// script is ASCII, so the magic check disambiguates cheaply without reading the
// (potentially huge) shared object in full.
func followLinkerScript(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{path}
	}
	defer f.Close()
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	buf = buf[:n]
	if n >= 4 && string(buf[:4]) == "\x7fELF" {
		return []string{path}
	}
	members := ldScriptLibRE.FindAllString(string(buf), -1)
	if len(members) == 0 {
		return []string{path}
	}
	return members
}

// ldScriptLibRE matches absolute paths to real libraries (.a or .so[.N...]) in a
// linker script. The AS_NEEDED loader path (ld-*.so.N) matches too, which is
// desirable: it defines loader-provided globals a normal link resolves.
var ldScriptLibRE = regexp.MustCompile(`/\S+\.(?:a|so(?:\.\d+)*)`)

// tbdSymRE matches Mach-O symbol names in an Apple .tbd stub (they carry a
// leading underscore). Over-matching non-symbol tokens is harmless -- they can't
// equal a real undefined symbol.
var tbdSymRE = regexp.MustCompile(`_[A-Za-z0-9_$.]+`)

var macSDKCache string

func macSDK() string {
	if macSDKCache == "" {
		out, err := exec.Command("xcrun", "--show-sdk-path").Output()
		if err != nil {
			macSDKCache = "-" // sentinel: resolved-to-none
			return ""
		}
		p := strings.TrimSpace(string(out))
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			macSDKCache = p
		} else {
			macSDKCache = "-"
		}
	}
	if macSDKCache == "-" {
		return ""
	}
	return macSDKCache
}
