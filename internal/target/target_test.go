package target

import (
	"reflect"
	"testing"
)

func TestLabel(t *testing.T) {
	tests := []struct {
		pkg, name, want string
	}{
		{"flare/base", "base", "//flare/base:base"},
		{"", "root", "//:root"},
	}
	for _, tt := range tests {
		tg := &Target{Package: tt.pkg, Name: tt.name}
		if got := tg.Label(); got != tt.want {
			t.Errorf("Label(%q,%q)=%q, want %q", tt.pkg, tt.name, got, tt.want)
		}
	}
}

func TestAttrStrings(t *testing.T) {
	tg := &Target{Attrs: map[string]any{
		"one":   "single",
		"many":  []any{"a", "b"},
		"mixed": []any{"a", int64(3), "b"}, // non-strings dropped
		"num":   int64(5),
	}}
	cases := map[string][]string{
		"one":     {"single"},
		"many":    {"a", "b"},
		"mixed":   {"a", "b"},
		"num":     nil,
		"missing": nil,
	}
	for attr, want := range cases {
		if got := tg.AttrStrings(attr); !reflect.DeepEqual(got, want) {
			t.Errorf("AttrStrings(%q)=%v, want %v", attr, got, want)
		}
	}
}

func TestAttrString(t *testing.T) {
	tg := &Target{Attrs: map[string]any{"s": "v", "n": int64(1)}}
	if got := tg.AttrString("s"); got != "v" {
		t.Errorf("AttrString(s)=%q", got)
	}
	if got := tg.AttrString("n"); got != "" {
		t.Errorf("AttrString(n)=%q, want empty (not a string)", got)
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	a := &Target{Type: "cc_library", Name: "a", Package: "p"}
	b := &Target{Type: "cc_binary", Name: "b", Package: "p"}
	if err := r.Add(a); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(b); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(&Target{Name: "a", Package: "p", Pos: "BUILD:9"}); err == nil {
		t.Fatal("expected duplicate error")
	}
	if r.Len() != 2 {
		t.Errorf("Len=%d, want 2", r.Len())
	}
	if r.Get("//p:a") != a {
		t.Error("Get(//p:a) wrong")
	}
	if r.Get("//p:missing") != nil {
		t.Error("Get(missing) should be nil")
	}
	if got := r.Labels(); !reflect.DeepEqual(got, []string{"//p:a", "//p:b"}) {
		t.Errorf("Labels=%v", got)
	}
	if got := r.All(); len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("All order wrong: %v", got)
	}
}
