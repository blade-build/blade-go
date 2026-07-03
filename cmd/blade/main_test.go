package main

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/blade-build/blade-go/internal/build"
)

func TestBuildFlagsNinja(t *testing.T) {
	// With no explicit -j, ninja() passes the cgroup-aware default so ninja does
	// not over-parallelize under a CFS-quota container.
	dj := strconv.Itoa(build.DefaultJobs())
	cases := []struct {
		name    string
		bf      buildFlags
		wantRun bool
		wantArg []string
	}{
		{"default", buildFlags{}, true, []string{"-j", dj}},
		{"no-build", buildFlags{noBuild: true}, false, []string{"-j", dj}},
		{"stop-after generate", buildFlags{stopAfter: "generate"}, false, []string{"-j", dj}},
		{"stop-after build still runs", buildFlags{stopAfter: "build"}, true, []string{"-j", dj}},
		{"explicit jobs override default", buildFlags{jobs: 8, keepGoing: true, dryRun: true}, true,
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
