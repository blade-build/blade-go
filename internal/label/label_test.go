package label

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		in, cur  string
		wantPkg  string
		wantName string
	}{
		{"//flare/base:string", "x", "flare/base", "string"},
		{"flare/base:string", "x", "flare/base", "string"}, // "//" optional
		{":message", "flare/rpc", "flare/rpc", "message"},
		{"//flare:init", "x", "flare", "init"},
		{"//flare/base", "x", "flare/base", "base"}, // name defaults to basename
		{"#pthread", "x", "#", "pthread"},
	}
	for _, tt := range tests {
		got, err := Parse(tt.in, tt.cur)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.in, err)
			continue
		}
		if got.Package != tt.wantPkg || got.Name != tt.wantName {
			t.Errorf("Parse(%q,%q)=%+v, want {%q %q}", tt.in, tt.cur, got, tt.wantPkg, tt.wantName)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"", "   ", "//pkg:"} {
		if _, err := Parse(in, ""); err == nil {
			t.Errorf("Parse(%q) expected error", in)
		}
	}
}

func TestStringAndSyslib(t *testing.T) {
	if got := (Label{Package: "flare/base", Name: "string"}).String(); got != "//flare/base:string" {
		t.Errorf("String=%q", got)
	}
	sys := Label{Package: SyslibPackage, Name: "pthread"}
	if !sys.IsSyslib() || sys.String() != "#pthread" {
		t.Errorf("syslib rendering wrong: %q", sys.String())
	}
}

func TestVisibleTo(t *testing.T) {
	lc := func(pkg, name string) Label { return Label{Package: pkg, Name: name} }
	tests := []struct {
		vis         []string
		definePkg   string
		consumer    Label
		wantVisible bool
	}{
		{nil, "flare/base", lc("flare/base", "other"), true},                        // same package
		{nil, "flare/base", lc("flare/rpc", "rpc"), false},                          // private, cross-package
		{[]string{"PUBLIC"}, "flare/base", lc("app", "x"), true},                    // PUBLIC
		{[]string{"//flare/rpc:rpc"}, "flare/base", lc("flare/rpc", "rpc"), true},   // exact
		{[]string{"//flare/rpc:rpc"}, "flare/base", lc("flare/rpc", "http"), false}, // exact, wrong name
		{[]string{"//flare/rpc:*"}, "flare/base", lc("flare/rpc", "http"), true},    // pkg wildcard
		{[]string{"//flare/rpc:*"}, "flare/base", lc("flare/net", "x"), false},      // wrong pkg
		{[]string{"//flare/..."}, "flare/base", lc("flare/rpc/detail", "x"), true},  // recursive
		{[]string{"//flare/..."}, "flare/base", lc("other", "x"), false},            // outside subtree
		{[]string{"//flare/..."}, "flare/base", lc("flare", "x"), true},             // the root itself
	}
	for i, tt := range tests {
		if got := VisibleTo(tt.vis, tt.definePkg, tt.consumer); got != tt.wantVisible {
			t.Errorf("case %d: VisibleTo(%v,%q,%v)=%v, want %v", i, tt.vis, tt.definePkg, tt.consumer, got, tt.wantVisible)
		}
	}
}

func TestVcpkg(t *testing.T) {
	if !IsVcpkg("vcpkg#gflags") || IsVcpkg("//x:y") {
		t.Fatal("IsVcpkg detection wrong")
	}
	if got := ParseVcpkg("vcpkg#gflags"); got.Port != "gflags" || got.Lib != "gflags" {
		t.Errorf("ParseVcpkg(vcpkg#gflags)=%+v", got)
	}
	if got := ParseVcpkg("vcpkg#protobuf:protobuf-lite"); got.Port != "protobuf" || got.Lib != "protobuf-lite" {
		t.Errorf("ParseVcpkg with lib=%+v", got)
	}
}
