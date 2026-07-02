package config

import "testing"

func TestConfig(t *testing.T) {
	c := New()
	if len(c.Sections()) != 0 {
		t.Fatal("new config should be empty")
	}
	c.Record("cc_config", map[string]any{"cxxflags": []any{"-std=c++17"}}, "BLADE_ROOT:3")
	c.Record("cc_toolchain_config", map[string]any{"name": "msvc"}, "BLADE_ROOT:10")
	c.Record("cc_toolchain_config", map[string]any{"name": "gcc"}, "BLADE_ROOT:14")

	if got := len(c.Sections()); got != 3 {
		t.Fatalf("Sections=%d, want 3", got)
	}
	if got := c.Named("cc_config"); len(got) != 1 || got[0].Pos != "BLADE_ROOT:3" {
		t.Errorf("Named(cc_config)=%v", got)
	}
	tc := c.Named("cc_toolchain_config")
	if len(tc) != 2 {
		t.Fatalf("Named(cc_toolchain_config)=%d, want 2 (repeated calls)", len(tc))
	}
	if tc[0].Attrs["name"] != "msvc" || tc[1].Attrs["name"] != "gcc" {
		t.Errorf("toolchain order wrong: %v", tc)
	}
	if got := c.Named("absent"); got != nil {
		t.Errorf("Named(absent)=%v, want nil", got)
	}
}

func TestGetItem(t *testing.T) {
	c := New()
	c.Record("cc_config", map[string]any{"opt": "old"}, "p:1")
	c.Record("cc_config", map[string]any{"opt": "new", "flag": int64(2)}, "p:2")
	if v, ok := c.GetItem("cc_config", "opt"); !ok || v != "new" {
		t.Errorf("GetItem opt=%v,%v want new (last wins)", v, ok)
	}
	if v, ok := c.GetItem("cc_config", "flag"); !ok || v != int64(2) {
		t.Errorf("GetItem flag=%v,%v", v, ok)
	}
	if _, ok := c.GetItem("cc_config", "absent"); ok {
		t.Error("absent item should report ok=false")
	}
	if _, ok := c.GetItem("no_section", "x"); ok {
		t.Error("absent section should report ok=false")
	}
}
