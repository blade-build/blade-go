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
