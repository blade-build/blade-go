package vcpkg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
