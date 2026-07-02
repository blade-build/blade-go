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
| 4 | `proto_library` (protoc C++ codegen + ordering) | ⬜ |
| 5 | Custom rules (`define_rule`) + `cc_flare_library` | ⬜ |
| 6 | foreign / thirdparty (or vcpkg) | ⬜ |
| 7 | test execution + coverage | ⬜ |
| 8 | Full flare build + conformance capstone | ⬜ |

Each phase is one PR, merged after CI is green.

Phase 1 status: loads flare's real `BLADE_ROOT` (lambdas, `blade` context,
`load_value`) and **76 of 86** flare BUILD files (602 targets). The remaining 10
need `load()`/`include()` (custom-rule machinery, Phase 5) and `build_target`.

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
