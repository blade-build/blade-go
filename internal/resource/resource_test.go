package resource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegularVarName(t *testing.T) {
	if got := RegularVarName("a/b-c.d+e"); got != "a_b_c_d_e" {
		t.Errorf("RegularVarName=%q", got)
	}
}

func TestGenerateResource(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(in, []byte("Hi!"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.c")
	if err := GenerateResource(in, out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	s := string(got)
	varName := RegularVarName(in)
	// blade's format: const char RESOURCE_<var>[] = { 0x.. }; + _len.
	if !strings.Contains(s, "const char RESOURCE_"+varName+"[] = {") {
		t.Errorf("byte-array decl missing:\n%s", s)
	}
	if !strings.Contains(s, "0x48, 0x69, 0x21") { // "Hi!"
		t.Errorf("bytes not emitted:\n%s", s)
	}
	if !strings.Contains(s, "const unsigned int RESOURCE_"+varName+"_len = 3;") {
		t.Errorf("length symbol wrong:\n%s", s)
	}
}

func TestGenerateIndex(t *testing.T) {
	dir := t.TempDir()
	pkg := "pkg"
	if err := os.MkdirAll(filepath.Join(dir, pkg), 0o755); err != nil {
		t.Fatal(err)
	}
	res := filepath.Join(dir, pkg, "a.txt")
	if err := os.WriteFile(res, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	hdr := filepath.Join(dir, "res.h")
	src := filepath.Join(dir, "res.c")
	if err := GenerateIndex("res", filepath.Join(dir, pkg), hdr, src, []string{res}); err != nil {
		t.Fatal(err)
	}
	h, _ := os.ReadFile(hdr)
	c, _ := os.ReadFile(src)
	hs, cs := string(h), string(c)

	entryVar := RegularVarName(res)
	// Header: type struct, extern decl (with size 4), index externs, extern "C".
	for _, want := range []string{
		"struct BladeResourceEntry {",
		"extern const char RESOURCE_" + entryVar + "[4];",
		`extern "C" {`,
		"extern const struct BladeResourceEntry RESOURCE_INDEX_",
	} {
		if !strings.Contains(hs, want) {
			t.Errorf("header missing %q:\n%s", want, hs)
		}
	}
	// Source: includes header, the index array with the entry (name "a.txt"),
	// and BLADE_RESOURCE_KEEP.
	for _, want := range []string{
		"BLADE_RESOURCE_KEEP",
		`{ "a.txt", RESOURCE_` + entryVar + ", 4 },",
		"_len = 1;",
	} {
		if !strings.Contains(cs, want) {
			t.Errorf("source missing %q:\n%s", want, cs)
		}
	}
}
