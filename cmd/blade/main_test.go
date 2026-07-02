package main

import (
	"reflect"
	"testing"
)

func TestBuildFlagsNinja(t *testing.T) {
	cases := []struct {
		name    string
		bf      buildFlags
		wantRun bool
		wantArg []string
	}{
		{"default", buildFlags{}, true, nil},
		{"no-build", buildFlags{noBuild: true}, false, nil},
		{"stop-after generate", buildFlags{stopAfter: "generate"}, false, nil},
		{"stop-after build still runs", buildFlags{stopAfter: "build"}, true, nil},
		{"jobs+keep+dry", buildFlags{jobs: 8, keepGoing: true, dryRun: true}, true,
			[]string{"-j", "8", "-k", "0", "-n"}},
	}
	for _, c := range cases {
		run, args := c.bf.ninja()
		if run != c.wantRun {
			t.Errorf("%s: run=%v, want %v", c.name, run, c.wantRun)
		}
		if !reflect.DeepEqual(args, c.wantArg) {
			t.Errorf("%s: args=%v, want %v", c.name, args, c.wantArg)
		}
	}
}
