package ninjaparse

import (
	"reflect"
	"sort"
	"testing"
)

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestParse(t *testing.T) {
	src := `cxx = g++

rule cxx
  command = ${cxx} -c ${in} -o ${out}

build build64_release/base/a.cc.o: cxx base/a.cc | build64_release/pb/msg.pb.h
  includes = -I.
build build64_release/base/libbase.a: ar build64_release/base/a.cc.o
build build64_release/app/app: link build64_release/app/main.cc.o | build64_release/base/libbase.a
  libs = -lpthread
`
	edges := Parse(src)
	if len(edges) != 3 {
		t.Fatalf("got %d edges, want 3", len(edges))
	}
	// compile edge
	if edges[0].Rule != "cxx" || len(edges[0].Inputs) != 1 || edges[0].Inputs[0] != "base/a.cc" {
		t.Errorf("compile edge wrong: %+v", edges[0])
	}
	if len(edges[0].Implicit) != 1 || edges[0].Implicit[0] != "build64_release/pb/msg.pb.h" {
		t.Errorf("implicit dep wrong: %+v", edges[0].Implicit)
	}
	if got, want := keys(CompiledSources(edges)), []string{"a.cc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("CompiledSources=%v, want %v", got, want)
	}
	if got, want := keys(Archives(edges)), []string{"libbase.a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Archives=%v, want %v", got, want)
	}
}

func TestParseContinuationAndEscape(t *testing.T) {
	// A `$`-continued line and escaped spaces in a path.
	src := "build out.o: cxx a.cc $\n  b.cc\nbuild with$ space.o: cxx x.cc\n"
	edges := Parse(src)
	if len(edges) != 2 {
		t.Fatalf("got %d edges, want 2", len(edges))
	}
	if len(edges[0].Inputs) != 2 || edges[0].Inputs[1] != "b.cc" {
		t.Errorf("continuation not joined: %+v", edges[0].Inputs)
	}
	if edges[1].Outputs[0] != "with space.o" {
		t.Errorf("escape not decoded: %q", edges[1].Outputs[0])
	}
}
