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
