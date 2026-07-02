// Package loader evaluates Blade BUILD / BLADE_ROOT files, which are Starlark
// (a restricted Python dialect). It is the front-end of blade-go.
//
// Bootstrap state: only SmokeEval exists, to prove the Starlark toolchain is
// wired end to end. Phase 1 grows this into the real BUILD loader, where
// cc_library / cc_binary / proto_library / ... are Go-implemented builtins that
// register targets into a graph.
package loader

import "go.starlark.net/starlark"

// SmokeEval evaluates a single Starlark expression and returns its value.
//
// Temporary scaffolding, replaced by the BUILD loader in Phase 1.
func SmokeEval(expr string) (starlark.Value, error) {
	thread := &starlark.Thread{Name: "smoke"}
	return starlark.Eval(thread, "smoke.star", expr, nil)
}
