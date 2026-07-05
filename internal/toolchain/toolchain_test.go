package toolchain

import (
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestForceLoad(t *testing.T) {
	mac := &Toolchain{OS: "darwin"}
	if got, want := mac.ForceLoad("/x/libfoo.a"), []string{"-Wl,-force_load,/x/libfoo.a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("darwin ForceLoad=%v, want %v", got, want)
	}
	lin := &Toolchain{OS: "linux"}
	if got, want := lin.ForceLoad("/x/libfoo.a"),
		[]string{"-Wl,--whole-archive", "/x/libfoo.a", "-Wl,--no-whole-archive"}; !reflect.DeepEqual(got, want) {
		t.Errorf("linux ForceLoad=%v, want %v", got, want)
	}
}

func TestNaming(t *testing.T) {
	cases := []struct {
		os                 string
		obj, lib, dyn, exe string
	}{
		{"linux", ".o", "libfoo.a", "libfoo.so", "foo"},
		{"darwin", ".o", "libfoo.a", "libfoo.dylib", "foo"},
		{"windows", ".obj", "foo.lib", "foo.dll", "foo.exe"},
	}
	for _, c := range cases {
		tc := &Toolchain{OS: c.os}
		if got := tc.ObjSuffix(); got != c.obj {
			t.Errorf("%s ObjSuffix=%q, want %q", c.os, got, c.obj)
		}
		if got := tc.StaticLib("foo"); got != c.lib {
			t.Errorf("%s StaticLib=%q, want %q", c.os, got, c.lib)
		}
		if got := tc.DynamicLib("foo"); got != c.dyn {
			t.Errorf("%s DynamicLib=%q, want %q", c.os, got, c.dyn)
		}
		if got := tc.BinName("foo"); got != c.exe {
			t.Errorf("%s BinName=%q, want %q", c.os, got, c.exe)
		}
	}
	if !(&Toolchain{OS: "windows"}).IsMSVC() || (&Toolchain{OS: "linux"}).IsMSVC() {
		t.Error("IsMSVC should be true only on windows")
	}
}

func TestDetectMSVC(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("MSVC detection is Windows-only")
	}
	tc := Detect()
	if !strings.EqualFold(filepath.Base(tc.CC), "cl.exe") {
		t.Skipf("no MSVC on this box (CC=%q)", tc.CC)
	}
	if strings.ToLower(filepath.Base(tc.AR)) != "lib.exe" || strings.ToLower(filepath.Base(tc.Link)) != "link.exe" {
		t.Errorf("MSVC tools: AR=%q Link=%q, want lib.exe/link.exe", tc.AR, tc.Link)
	}
	if envValue(tc.Env, "INCLUDE") == "" {
		t.Error("MSVC dev env missing INCLUDE")
	}
	if envValue(tc.Env, "LIB") == "" {
		t.Error("MSVC dev env missing LIB")
	}
	t.Logf("cl=%s\n  INCLUDE dirs=%d  LIB dirs=%d", tc.CC,
		len(filepath.SplitList(envValue(tc.Env, "INCLUDE"))),
		len(filepath.SplitList(envValue(tc.Env, "LIB"))))
}
