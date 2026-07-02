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
| 8e | cc_config compile flags via config-lambda evaluation | ✅ |
| 8f | cc_test links cc_test_config test framework (gtest via vcpkg) | ✅ |
| 8g | read BLADE_ROOT `vcpkg_config`: install flare-pinned ports (fmt 7.1.3, protobuf 3.21.12, ...) via a generated vcpkg manifest | ✅ |
| 8h | source-built thirdparty: `include()`, `cmake_build` (jsoncpp) + `autotools_build` (ctemplate) via `foreign_cc_library` | ✅ |
| 8i | RPC graph: `legacy_public_targets`, header self-sufficiency (C++), vcpkg `include_prefix`, `resource_library` codegen | ✅ |
| 8j | **`//flare/rpc:rpc` — the full flare RPC library — compiles end-to-end** | ✅ |
| 8k | **`//flare/example/rpc:server` — a full RPC server binary links & runs** (cc_flare_library codegen, header-check separation, pkg-config -framework) | ✅ |
| 8l | **`blade test` runs flare's cc_tests** (gtest via vcpkg manual-link) — endian/chrono/enum/string tests PASS | ✅ |

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
go build ./...     # build all packages
go test ./...      # run the test suite
```

Requires **Go 1.25+** (and, for the cc end-to-end test, `ninja` + a C++
compiler). If your system `go` is older, keep `GOTOOLCHAIN=auto` (the Go default)
so the right toolchain is fetched automatically:

```sh
go env -w GOTOOLCHAIN=auto   # once; restores Go's default if you set it to 'local'
# or per-command: GOTOOLCHAIN=auto go build ./...
```

### The `blade` executable

The CLI lives in `./cmd/blade`:

```sh
go build -o blade ./cmd/blade   # produce ./blade
# or install onto your PATH (GOBIN, else GOPATH/bin):
go install ./cmd/blade
```

As of Phase 3, `blade build //pkg:target` works for cc targets: it finds
BLADE_ROOT, resolves the graph, generates `build64_release/build.ninja`, and runs
ninja to produce the binary/archive. `blade test //pkg/...` builds and runs every
cc_test in the pattern.

## Performance

Front-end cost (load BUILD files → resolve graph → generate ninja; *not* the
compile/link, which ninja does) on flare's **whole repo** (`//...`, 685 targets),
warm, measured with `blade build --no-build` on both:

| phase | Python Blade | blade-go |
| --- | --- | --- |
| load + resolve graph | ~0.19s | ~0.12s |
| vcpkg install (idempotent) | skipped (stamped) | skipped (stamped) |
| generate ninja | ~0.8s | ~0.05s |
| **front-end total** | **~1.0s** | **~0.17s** |

Getting there was mostly removing accidental O(bad) work, each found by
profiling (`BLADE_TIMING=1` for per-phase timing, `BLADE_CPUPROFILE=<path>` for a
pprof profile): dedup DAG traversals by node (not path), an MD5 stamp to skip an
unchanged `vcpkg install` (as Blade does), caching the pkg-config `-framework`
scan instead of re-reading it per link, and pruning the build-output/hidden dirs
from the whole-repo walk. From ~42s to ~0.17s.
