package sanitizer

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAndTag(t *testing.T) {
	// Aliases + canonicalization + sorting (order-independent).
	got, err := Parse("ubsan,asan")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"address", "undefined"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse=%v, want %v", got, want)
	}
	if tag := BuildTag(got); tag != "asan+ubsan" {
		t.Fatalf("BuildTag=%q, want asan+ubsan", tag)
	}
	if _, err := Parse("bogus"); err == nil {
		t.Error("expected error for unknown sanitizer")
	}
	if got, _ := Parse(""); got != nil {
		t.Errorf("empty should be nil, got %v", got)
	}
}

func TestCompat(t *testing.T) {
	if err := CheckCompat([]string{"address", "thread"}); err == nil {
		t.Error("address+thread should be incompatible")
	}
	if err := CheckCompat([]string{"address", "undefined"}); err != nil {
		t.Errorf("address+undefined should compose: %v", err)
	}
}

func TestFlags(t *testing.T) {
	cf := strings.Join(CompileFlags([]string{"undefined"}), " ")
	if !strings.Contains(cf, "-fsanitize=undefined") || !strings.Contains(cf, "-fno-sanitize-recover=undefined") {
		t.Errorf("ubsan compile flags: %q", cf)
	}
	if CompileFlags(nil) != nil {
		t.Error("no sanitizers should yield nil compile flags")
	}
	if lf := LinkFlags([]string{"address"}); len(lf) != 1 || lf[0] != "-fsanitize=address" {
		t.Errorf("asan link flags: %v", lf)
	}
}

func TestToolchainAndEnv(t *testing.T) {
	if err := CheckToolchain([]string{"memory"}, false, "linux"); err == nil {
		t.Error("msan on gcc should be rejected")
	}
	if err := CheckToolchain([]string{"memory"}, true, "darwin"); err == nil {
		t.Error("msan on non-linux should be rejected")
	}
	if err := CheckToolchain([]string{"address"}, false, "darwin"); err != nil {
		t.Errorf("asan should be fine anywhere: %v", err)
	}
	if env := RuntimeEnv([]string{"address"}); env["ASAN_OPTIONS"] != "abort_on_error=1" {
		t.Errorf("asan runtime env: %v", env)
	}
}
