package ccundef

import (
	"reflect"
	"testing"
)

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func TestUnresolved(t *testing.T) {
	undef := set("symA", "symB", "symC", "printf", "_ZTV3Foo", "__asan_report")
	own := set("symA")         // resolved intra-archive
	dep := set("symB")         // provided by a declared dep
	system := set("printf")    // libc
	allow := CompileAllow(nil) // no user allowlist
	got := Unresolved(undef, own, dep, system, allow)
	// symC is the only genuinely-unresolved one; _ZTV (vtable) and __asan_ are
	// covered by the residual baseline.
	if want := []string{"symC"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Unresolved=%v, want %v", got, want)
	}
}

func TestAllowPattern(t *testing.T) {
	undef := set("myapp_internal_helper")
	allow := CompileAllow([]string{`myapp_.*`})
	if got := Unresolved(undef, nil, nil, nil, allow); len(got) != 0 {
		t.Fatalf("allow pattern should suppress, got %v", got)
	}
}

func TestSeverityFromBlade(t *testing.T) {
	for s, want := range map[string]Severity{"error": Error, "debug": Off, "warning": Warn, "": Warn, "notice": Warn} {
		if got := SeverityFromBlade(s); got != want {
			t.Errorf("SeverityFromBlade(%q)=%v, want %v", s, got, want)
		}
	}
}

func TestStackProtectorBaseline(t *testing.T) {
	// __stack_chk_guard/_fail are loader/runtime-provided; the residual baseline
	// must cover them so -fstack-protector targets don't flag on Linux.
	undef := set("__stack_chk_guard", "__stack_chk_fail", "realmissing")
	got := Unresolved(undef, nil, nil, nil, CompileAllow(nil))
	if len(got) != 1 || got[0] != "realmissing" {
		t.Fatalf("stack-protector baseline: got %v, want [realmissing]", got)
	}
}

func TestParseDumpbinSymbols(t *testing.T) {
	// Sample `dumpbin /SYMBOLS` lines (x64/ARM64 COFF): UNDEF External = undefined,
	// SECTn External = defined; Static and section headers are ignored.
	out := `
COFF SYMBOL TABLE
000 00000000 SECT1  notype       Static       | .text
008 00000000 UNDEF  notype ()    External     | ?missing_fn@@YAHH@Z (int __cdecl missing_fn(int))
009 00000000 SECT3  notype ()    External     | ?use_it@@YAHXZ (int __cdecl use_it(void))
00A 00000000 UNDEF  notype ()    External     | memcpy
00B 00000000 SECT2  notype       Static       | localthing
`
	undef, defined := parseDumpbinSymbols([]byte(out))
	for _, s := range []string{"?missing_fn@@YAHH@Z (int __cdecl missing_fn(int))", "memcpy"} {
		if !undef[s] {
			t.Errorf("expected undef %q", s)
		}
	}
	if !defined["?use_it@@YAHXZ (int __cdecl use_it(void))"] {
		t.Error("expected use_it defined")
	}
	if undef["localthing"] || defined["localthing"] {
		t.Error("Static symbol must be ignored")
	}
	if len(undef) != 2 || len(defined) != 1 {
		t.Errorf("counts: undef=%d defined=%d", len(undef), len(defined))
	}
}
