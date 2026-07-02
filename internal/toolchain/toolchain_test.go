package toolchain

import (
	"reflect"
	"testing"
)

func TestForceLoad(t *testing.T) {
	mac := &Toolchain{OS: "darwin"}
	if got, want := mac.ForceLoad("/x/libfoo.a"), []string{"-Wl,-force_load,/x/libfoo.a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("darwin ForceLoad=%v, want %v", got, want)
	}
	lin := &Toolchain{OS: "linux"}
	if got, want := lin.ForceLoad("/x/libfoo.a"),
		[]string{"-Wl,--whole-archive", "/x/libfoo.a", "-Wl,--no-whole-archive"}; !reflect.DeepEqual(got, want) {
		t.Errorf("linux ForceLoad=%v, want %v", got, want)
	}
}
