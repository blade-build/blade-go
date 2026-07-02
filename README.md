# blade-go

A ground-up **Go reimplementation of the [Blade](https://github.com/blade-build/blade-build) build system**.

Goal: build the C++ RPC framework [flare](https://github.com/Tencent/flare) end to
end, with sufficient tests and coverage. This is a fresh implementation, not a
port — the Python Blade stays the reference oracle (see *Testing*).

## Why a rewrite

Blade is mature Python. A Go implementation buys a **single static binary** (no
Python/venv version hell), speed, concurrency (no GIL), and — with a Starlark
front-end — determinism/hermeticity. blade-go replaces the front-end + dependency
graph + ninja generation; it keeps **ninja** as the backend and the real
toolchains (gcc/clang, protoc) unchanged.

## Scope

Only what flare needs:

- **cc**: `cc_library`, `cc_binary`, `cc_test`, `cc_benchmark`
- **proto**: `proto_library`
- **custom rules**: `define_rule` / `load()` (flare's `cc_flare_library`)
- **foreign / thirdparty**: `foreign_cc_library` / `autotools_build` / `cmake_build`
  (or vcpkg — decision pending)
- **config**: `cc_config`, `cc_library_config`, `cc_binary_config`,
  `cc_test_config`, `proto_library_config`, `global_config`, `vcpkg_config`,
  plus the lambda deferred-config model
- `resource_library`

**Non-goals** (flare doesn't use them): java / scala / py / go / cu / swig /
thrift / lex-yacc.

BUILD / BLADE_ROOT files are evaluated as **Starlark** (`go.starlark.net`). Blade's
BUILD files are already a restricted-Python subset ~96% Starlark-clean; the few
gaps (`assert`→`fail`, generator→list comprehension, config lambdas) are small and
known.

## Phased plan

| Phase | What | Status |
| --- | --- | --- |
| 0 | Scaffold: repo, CI+coverage, Starlark toolchain wired | ✅ |
| 1 | Load & config: BUILD/BLADE_ROOT eval, `blade` context, config capture, lambdas, `glob`/`fail`/`enable_if`/`load_value` | ✅ |
| 2 | Graph & analysis: dep expansion, visibility, topo sort | ✅ |
| 3 | cc core → ninja: compile/ar/link, includes, syslibs, toolchain; `blade build` CLI | ✅ |
| 4 | `proto_library` (protoc C++ codegen + ordering) | ✅ |
| 5 | Custom-rule extensions: `load()` + `native.*` macros + `blade.config.get_item` (the `cc_flare_library` pattern) | ✅ |
| 6 | `gen_rule` ninja backend + generated-source resolution + `build_target` | ✅ |
| 7 | `cc_test` execution + `blade test` CLI | ✅ |
| 8a | vcpkg resolver (`vcpkg#port:lib` → include/lib flags) | ✅ |
| 8b | //thirdparty→vcpkg mapping (flare graph resolves) | ✅ |
| 8c | flare `.bld` loads (isinstance builtin + flare assert/`is`/str-concat fixes) | ✅ |
| 8d | differential harness vs Python Blade (ninja parser + CI-wired) | ✅ |
| 8e | full flare compile (needs a vcpkg+flare env) | ⬜ |

Each phase is one PR, merged after CI is green.

Phase 1 status: loads flare's real `BLADE_ROOT` (lambdas, `blade` context,
`load_value`) and **76 of 86** flare BUILD files (602 targets).

Phase 5 adds the custom-rule machinery `cc_flare_library` uses: `load()` of a
`.bld` extension, a `native.*` object whose rules register in the *calling*
package (thread-local context), and `blade.config.get_item`. Still needed to load
the last flare BUILD files: the `gen_rule` ninja backend, `build_target`, and
`include()` (tracked for the flare capstone; flare's `.bld` also needs its
`assert`→`fail` tweak since Starlark has no `assert`).

## Testing

1. **Unit tests** per package (Starlark eval, config, graph, cc-flag computation,
   ninja emission).
2. **Differential testing (the linchpin)** — generate `build.ninja` with both
   blade-go and Python Blade on the same BUILD, normalize, and diff. The Python
   impl is a free, exhaustive oracle.
3. **Conformance** — run the existing [blade-test](https://github.com/blade-build/blade-test)
   suites end to end through blade-go.
4. **The flare build itself** as the top integration test.

CI runs `go test -race -coverprofile` on every PR and reports coverage.

## Build

```sh
go build ./...
go test ./...
```

Requires Go 1.25+ (and, for the cc end-to-end test, `ninja` + a C++ compiler).

As of Phase 3, `blade build //pkg:target` works for cc targets: it finds
BLADE_ROOT, resolves the graph, generates `build64_release/build.ninja`, and runs
ninja to produce the binary/archive.
