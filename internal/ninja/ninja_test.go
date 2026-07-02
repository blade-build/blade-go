package ninja

import (
	"strings"
	"testing"
)

func TestWrite(t *testing.T) {
	f := &File{}
	f.SetVar("cxx", "g++")
	f.AddRule(Rule{Name: "cxx", Command: "${cxx} -c ${in} -o ${out}", Description: "CXX ${out}", Depfile: "${out}.d", Deps: "gcc"})
	f.AddBuild(Build{
		Outputs: []string{"build/a.o"},
		Rule:    "cxx",
		Inputs:  []string{"a.cc"},
		Vars:    map[string]string{"includes": "-I."},
	})
	f.AddBuild(Build{
		Outputs:  []string{"build/app"},
		Rule:     "link",
		Inputs:   []string{"build/a.o"},
		Implicit: []string{"build/libx.a"},
	})
	got := f.String()
	for _, want := range []string{
		"cxx = g++\n",
		"rule cxx\n  command = ${cxx} -c ${in} -o ${out}\n  description = CXX ${out}\n  depfile = ${out}.d\n  deps = gcc\n",
		"build build/a.o: cxx a.cc\n  includes = -I.\n",
		"build build/app: link build/a.o | build/libx.a\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n---\n%s", want, got)
		}
	}
}

func TestEscapePath(t *testing.T) {
	cases := map[string]string{
		"a b":   "a$ b",
		"a:b":   "a$:b",
		"a$b":   "a$$b",
		"plain": "plain",
	}
	for in, want := range cases {
		if got := escapePath(in); got != want {
			t.Errorf("escapePath(%q)=%q, want %q", in, got, want)
		}
	}
}
