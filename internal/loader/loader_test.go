package loader

import (
	"testing"

	"go.starlark.net/starlark"
)

func TestSmokeEval(t *testing.T) {
	v, err := SmokeEval("1 + 2")
	if err != nil {
		t.Fatalf("SmokeEval: %v", err)
	}
	got, ok := v.(starlark.Int)
	if !ok {
		t.Fatalf("got %T, want starlark.Int", v)
	}
	if n, _ := got.Int64(); n != 3 {
		t.Fatalf("1 + 2 = %d, want 3", n)
	}
}

func TestSmokeEvalError(t *testing.T) {
	if _, err := SmokeEval("1 +"); err == nil {
		t.Fatal("expected a parse error for an incomplete expression")
	}
}
