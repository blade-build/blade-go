package vcpkg

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolver(t *testing.T) {
	root := t.TempDir()
	installed := filepath.Join(root, "installed", "x64-linux")
	if err := os.MkdirAll(filepath.Join(installed, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "lib", "libfoo.a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Root: root, Triplet: "x64-linux"}
	if !r.Configured() {
		t.Fatal("should be configured")
	}
	if got, want := r.IncludeDir(), filepath.Join(installed, "include"); got != want {
		t.Errorf("IncludeDir=%q, want %q", got, want)
	}
	if got, want := r.LibArg("foo"), filepath.Join(installed, "lib", "libfoo.a"); got != want {
		t.Errorf("LibArg(foo)=%q, want the archive %q", got, want)
	}
	if got := r.LibArg("bar"); got != "-lbar" { // no archive -> -l fallback
		t.Errorf("LibArg(bar)=%q, want -lbar", got)
	}
}

func TestUnconfigured(t *testing.T) {
	r := &Resolver{}
	if r.Configured() {
		t.Fatal("empty resolver should be unconfigured")
	}
	if r.IncludeDir() != "" {
		t.Errorf("IncludeDir=%q, want empty", r.IncludeDir())
	}
	if got := r.LibArg("x"); got != "-lx" {
		t.Errorf("LibArg=%q, want -lx", got)
	}
	// nil resolver is safe.
	var nilR *Resolver
	if nilR.Configured() {
		t.Error("nil resolver should be unconfigured")
	}
}

func TestManifestJSON(t *testing.T) {
	// Mirrors flare's vcpkg_config: plain-string versions, {'version': ...}
	// dicts, and a features dict. The Blade-only keys (link_all_symbols,
	// cmake_options) must be ignored -- they aren't part of vcpkg.json.
	packages := map[string]any{
		"fmt":      "7.1.3",
		"protobuf": map[string]any{"version": "3.21.12"},
		"gflags":   map[string]any{"version": "2.2.2", "link_all_symbols": true},
		"openssl":  map[string]any{},
		"curl":     map[string]any{"features": []any{"openssl", "http2"}},
	}
	out, err := ManifestJSON("06a7fdd564234908731c59ac46a624f808e87b1c", packages)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v\n%s", err, out)
	}

	if m["builtin-baseline"] != "06a7fdd564234908731c59ac46a624f808e87b1c" {
		t.Errorf("baseline=%v", m["builtin-baseline"])
	}

	// dependencies: sorted; plain ports are strings, curl carries its features.
	deps := m["dependencies"].([]any)
	var names []string
	var curl map[string]any
	for _, d := range deps {
		switch v := d.(type) {
		case string:
			names = append(names, v)
		case map[string]any:
			names = append(names, v["name"].(string))
			if v["name"] == "curl" {
				curl = v
			}
		}
	}
	if want := []string{"curl", "fmt", "gflags", "openssl", "protobuf"}; !reflect.DeepEqual(names, want) {
		t.Errorf("dependency names=%v, want sorted %v", names, want)
	}
	if curl == nil || !reflect.DeepEqual(curl["features"], []any{"openssl", "http2"}) {
		t.Errorf("curl features not carried through: %v", curl)
	}

	// overrides: one per explicitly-versioned port (openssl/curl have none).
	got := map[string]string{}
	for _, o := range m["overrides"].([]any) {
		ov := o.(map[string]any)
		got[ov["name"].(string)] = ov["version"].(string)
	}
	want := map[string]string{"fmt": "7.1.3", "protobuf": "3.21.12", "gflags": "2.2.2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("overrides=%v, want %v", got, want)
	}
}

func TestManifestJSONNoBaseline(t *testing.T) {
	out, err := ManifestJSON("", map[string]any{"zlib": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if _, ok := m["builtin-baseline"]; ok {
		t.Error("no baseline should omit builtin-baseline")
	}
	if _, ok := m["overrides"]; ok {
		t.Error("no versions should omit overrides")
	}
	if !reflect.DeepEqual(m["dependencies"], []any{"zlib"}) {
		t.Errorf("dependencies=%v", m["dependencies"])
	}
}

func TestWriteOverlayTriplet(t *testing.T) {
	root := t.TempDir()
	// A stand-in built-in triplet file.
	if err := os.MkdirAll(filepath.Join(root, "triplets"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := "set(VCPKG_TARGET_ARCHITECTURE arm64)\nset(VCPKG_LIBRARY_LINKAGE static)\n"
	if err := os.WriteFile(filepath.Join(root, "triplets", "arm64-osx.cmake"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Root: root, Triplet: "arm64-osx"}
	packages := map[string]any{
		"glog":   map[string]any{"cmake_options": []any{"-DGFLAGS_NOTHREADS=OFF"}},
		"snappy": map[string]any{"cmake_options": []any{"-DSNAPPY_WITH_RTTI=ON"}},
		"fmt":    "7.1.3", // no cmake_options -> no branch
	}
	dir := t.TempDir()
	content, err := r.overlayTripletContent(packages)
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := r.writeOverlayTriplet(content, dir)
	if err != nil {
		t.Fatal(err)
	}
	if overlay == "" {
		t.Fatal("expected an overlay dir when cmake_options are present")
	}
	got, err := os.ReadFile(filepath.Join(overlay, "arm64-osx.cmake"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{
		base, // base triplet preserved
		`if(PORT STREQUAL "glog")`,
		`list(APPEND VCPKG_CMAKE_CONFIGURE_OPTIONS "-DGFLAGS_NOTHREADS=OFF")`,
		`if(PORT STREQUAL "snappy")`,
		`list(APPEND VCPKG_CMAKE_CONFIGURE_OPTIONS "-DSNAPPY_WITH_RTTI=ON")`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("overlay triplet missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, `STREQUAL "fmt"`) {
		t.Error("port without cmake_options should not get a branch")
	}
}

func TestWriteOverlayTripletNoOptions(t *testing.T) {
	r := &Resolver{Root: t.TempDir(), Triplet: "arm64-osx"}
	content, err := r.overlayTripletContent(map[string]any{"fmt": "7.1.3"})
	if err != nil {
		t.Fatal(err)
	}
	if content != "" {
		t.Errorf("no cmake_options should mean no overlay content, got %q", content)
	}
	overlay, err := r.writeOverlayTriplet(content, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if overlay != "" {
		t.Errorf("no content should mean no overlay dir, got %q", overlay)
	}
}

func TestInstalledDirOverride(t *testing.T) {
	dir := t.TempDir()
	r := &Resolver{Root: "/opt/vcpkg", Triplet: "arm64-osx", InstalledDir: dir}
	if got, want := r.IncludeDir(), filepath.Join(dir, "include"); got != want {
		t.Errorf("IncludeDir=%q, want manifest-tree %q", got, want)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("VCPKG_ROOT", "/opt/vcpkg")
	t.Setenv("VCPKG_DEFAULT_TRIPLET", "custom-triplet")
	if r := FromEnv(); r.Root != "/opt/vcpkg" || r.Triplet != "custom-triplet" {
		t.Errorf("FromEnv=%+v", r)
	}
	t.Setenv("VCPKG_DEFAULT_TRIPLET", "")
	if FromEnv().Triplet == "" {
		t.Error("default triplet should be non-empty")
	}
}

func TestBuildIncludePrefixes(t *testing.T) {
	// flare's include_prefix ports (zlib, snappy): vcpkg ships headers flat, but
	// flare includes "zlib/zlib.h". A per-prefix symlink to the include dir makes
	// "<prefix>/hdr" resolve via -I<PrefixRoot>.
	manifest := t.TempDir()
	installed := t.TempDir()
	if err := os.MkdirAll(filepath.Join(installed, "include"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "include", "zlib.h"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{InstalledDir: installed}
	packages := map[string]any{
		"zlib": map[string]any{"include_prefix": "zlib"},
		"fmt":  "7.1.3", // no include_prefix -> no symlink
	}
	if err := r.buildIncludePrefixes(packages, manifest); err != nil {
		t.Fatal(err)
	}
	if r.PrefixRoot == "" {
		t.Fatal("PrefixRoot not set")
	}
	// "zlib/zlib.h" resolves under PrefixRoot via the symlink.
	if _, err := os.Stat(filepath.Join(r.PrefixRoot, "zlib", "zlib.h")); err != nil {
		t.Errorf("zlib/zlib.h not resolvable via prefix symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(r.PrefixRoot, "fmt")); err == nil {
		t.Error("fmt has no include_prefix; should not get a symlink")
	}
}

func TestLinkExtrasFrameworks(t *testing.T) {
	// curl's .pc lists macOS frameworks in Libs: LinkExtras must surface them
	// (deduped) so a binary linking libcurl resolves CoreFoundation/Security.
	installed := t.TempDir()
	pc := filepath.Join(installed, "lib", "pkgconfig")
	if err := os.MkdirAll(pc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pc, "libcurl.pc"),
		[]byte("Libs: -lcurl -framework Security -framework CoreFoundation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pc, "other.pc"),
		[]byte("Libs: -lfoo -framework Security\n"), 0o644); err != nil { // dup Security
		t.Fatal(err)
	}
	r := &Resolver{InstalledDir: installed}
	got := r.LinkExtras()
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-framework Security") || !strings.Contains(joined, "-framework CoreFoundation") {
		t.Errorf("LinkExtras=%v, want the frameworks", got)
	}
	// Security appears once despite two .pc files.
	if n := strings.Count(joined, "Security"); n != 1 {
		t.Errorf("Security should be deduped, count=%d: %v", n, got)
	}
}

func TestLibArgManualLink(t *testing.T) {
	// vcpkg puts archives that define main() (gtest_main) under lib/manual-link/;
	// LibArg must find them there, not fall back to a broken -lgtest_main.
	installed := t.TempDir()
	ml := filepath.Join(installed, "lib", "manual-link")
	if err := os.MkdirAll(ml, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ml, "libgtest_main.a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{InstalledDir: installed}
	if got := r.LibArg("gtest_main"); got != filepath.Join(ml, "libgtest_main.a") {
		t.Errorf("LibArg(gtest_main)=%q, want the manual-link archive", got)
	}
}

func TestInstallFromConfigSkipsWhenStamped(t *testing.T) {
	// A present tree + a matching stamp must skip `vcpkg install` entirely --
	// no vcpkg executable is even consulted (mirrors blade's MD5-stamp skip).
	manifestDir := t.TempDir()
	triplet := "test-triplet"
	tree := filepath.Join(manifestDir, "vcpkg_installed", triplet)
	if err := os.MkdirAll(filepath.Join(tree, "include"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Triplet: triplet} // no Root/exe on purpose
	packages := map[string]any{"fmt": "7.1.3"}
	manifest, _ := ManifestJSON("", packages)
	overlay, _ := r.overlayTripletContent(packages)
	stamp := fmt.Sprintf("%x", md5.Sum([]byte(string(manifest)+overlay+triplet)))
	if err := os.WriteFile(filepath.Join(manifestDir, ".blade-go-vcpkg-stamp"), []byte(stamp), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.InstallFromConfig("", packages, manifestDir); err != nil {
		t.Fatalf("stamped install should skip cleanly: %v", err)
	}
	if r.InstalledDir != tree {
		t.Errorf("InstalledDir=%q, want the pinned tree %q", r.InstalledDir, tree)
	}

	// Without the stamp, it must NOT skip -- and with no vcpkg exe, that errors.
	os.Remove(filepath.Join(manifestDir, ".blade-go-vcpkg-stamp"))
	r2 := &Resolver{Triplet: triplet}
	if err := r2.InstallFromConfig("", packages, manifestDir); err == nil {
		t.Error("without a stamp it should try to install (and fail: no vcpkg exe)")
	}
}
