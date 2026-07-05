package toolchain

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// msvcInfo is the resolved MSVC toolchain: the compiler/archiver/linker and the
// developer environment (INCLUDE/LIB/PATH/...) captured from vcvarsall.
type msvcInfo struct {
	cc, lib, link string
	env           []string
	ok            bool
}

var (
	msvcOnce   sync.Once
	msvcCached msvcInfo
)

// detectMSVC locates MSVC and captures its developer environment (cached; the
// probe runs vcvarsall which is ~1s). Rather than re-deriving every Windows SDK
// and MSVC include/lib directory the way Blade does, blade-go runs the stock
// `vcvarsall.bat <arch>` once and captures the resulting environment -- the same
// vars a "Developer Command Prompt" sets. They are injected into the ninja
// subprocess so cl/lib/link find their headers and libraries; the tools
// themselves are resolved from the captured PATH. ok=false => no VS found, so
// the caller keeps the gcc/clang defaults.
func detectMSVC(arch string) msvcInfo {
	msvcOnce.Do(func() { msvcCached = probeMSVC(arch) })
	return msvcCached
}

func probeMSVC(arch string) msvcInfo {
	vcvars := findVcvarsall()
	if vcvars == "" {
		return msvcInfo{}
	}
	env, ok := captureDevEnv(vcvars, arch)
	if !ok {
		return msvcInfo{}
	}
	path := envValue(env, "PATH")
	cc := lookInPath("cl.exe", path)
	lib := lookInPath("lib.exe", path)
	link := lookInPath("link.exe", path)
	if cc == "" || lib == "" || link == "" {
		return msvcInfo{}
	}
	// Force the English "/showIncludes" prefix so ninja's msvc_deps_prefix (and
	// blade-go's own inclusion parsing) match on a localized VS. See Blade #1154.
	env = append(env, "VSLANG=1033")
	return msvcInfo{cc: cc, lib: lib, link: link, env: env, ok: true}
}

// findVcvarsall locates VC\Auxiliary\Build\vcvarsall.bat via vswhere.
func findVcvarsall() string {
	pf := os.Getenv("ProgramFiles(x86)")
	if pf == "" {
		pf = os.Getenv("ProgramFiles")
	}
	vswhere := filepath.Join(pf, "Microsoft Visual Studio", "Installer", "vswhere.exe")
	if _, err := os.Stat(vswhere); err != nil {
		return ""
	}
	out, err := exec.Command(vswhere, "-latest", "-products", "*",
		"-requires", "Microsoft.VisualStudio.Component.VC.Tools.x86.x64",
		"-property", "installationPath").Output()
	if err != nil {
		return ""
	}
	inst := strings.TrimSpace(string(out))
	if inst == "" {
		return ""
	}
	vcvars := filepath.Join(inst, "VC", "Auxiliary", "Build", "vcvarsall.bat")
	if _, err := os.Stat(vcvars); err != nil {
		return ""
	}
	return vcvars
}

// captureDevEnv runs vcvarsall for arch and returns the resulting environment as
// KEY=VALUE lines. It writes a tiny temp .bat (call vcvarsall + `set`) and runs
// `cmd /c <bat>`, which sidesteps the nested-quoting hazard of passing a
// space-containing vcvarsall path inline to cmd.
func captureDevEnv(vcvars, arch string) ([]string, bool) {
	bat, err := os.CreateTemp("", "blade-msvc-*.bat")
	if err != nil {
		return nil, false
	}
	defer os.Remove(bat.Name())
	script := "@echo off\r\ncall \"" + vcvars + "\" " + arch + " >nul\r\nif errorlevel 1 exit /b 1\r\nset\r\n"
	if _, err := bat.WriteString(script); err != nil {
		bat.Close()
		return nil, false
	}
	bat.Close()

	out, err := exec.Command("cmd", "/c", bat.Name()).Output()
	if err != nil {
		return nil, false
	}
	var env []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.IndexByte(line, '='); i > 0 {
			env = append(env, line)
		}
	}
	if envValue(env, "INCLUDE") == "" || envValue(env, "LIB") == "" {
		return nil, false
	}
	return env, true
}

// envValue returns the value of key in a KEY=VALUE list (case-insensitive key,
// matching Windows env semantics).
func envValue(env []string, key string) string {
	prefix := strings.ToUpper(key) + "="
	for _, e := range env {
		if len(e) > len(key) && strings.EqualFold(e[:len(key)+1], prefix) {
			return e[len(key)+1:]
		}
	}
	return ""
}

// lookInPath finds an executable by name across the directories of a PATH string.
func lookInPath(name, path string) string {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return ""
}

// msvcArch maps Go's GOARCH to a vcvarsall arch argument (native host==target).
func msvcArch(goarch string) string {
	switch goarch {
	case "arm64":
		return "arm64"
	case "amd64":
		return "x64"
	case "386":
		return "x86"
	default:
		return "x64"
	}
}
