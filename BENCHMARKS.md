# Benchmarks

This repo includes benchmarks for the extension shapes that matter to the Go +
Lua host-kernel model.

The goal is not to prove Lua is faster than Go. It is to make the cost of each
extension shape visible so future implementers can decide where Lua belongs.

## Benchmark groups

### `BenchmarkPolicyCheck`

Measures the core seam call path.

- `GoDirect`: direct Go implementation, no bridge.
- `GoThroughApp`: Go default called through the host registry.
- `LuaReplace`: Lua fully replaces `core.policy`.
- `LuaWrapGo`: Lua wraps the Go default and delegates on the common path.

Use this group to decide whether a seam is safe for hot paths.

### `BenchmarkLuaDispatch`

Measures feature-facing dispatch.

- `RunCommandNoop`: command registered by Lua, no-op handler.
- `EmitNoopEvent`: event emitted to one Lua no-op handler.
- `EmitEventWithPolicyCheck`: event handler calls back into the current policy.

Use this group to estimate user command/event overhead.

### `BenchmarkLuaLifecycle`

Measures operator lifecycle work.

- `ValidateDir`: build isolated Lua runtime and discard it.
- `Reload`: build isolated Lua runtime and swap it live.

Use this group to size reload/validate expectations. These are not request-path
operations.

## Quick run

```bash
./bench.sh
```

Equivalent to:

```bash
go test -run '^$' -bench=. -benchmem -count 1 ./pkg/host
```

Run a subset:

```bash
BENCH=BenchmarkPolicyCheck ./bench.sh
BENCH=BenchmarkLuaLifecycle BENCHTIME=100x ./bench.sh
```

Save output:

```bash
OUT=/tmp/extensible-go.before.txt COUNT=10 ./bench.sh
```

## Compare changes with benchstat

Install once:

```bash
go install golang.org/x/perf/cmd/benchstat@latest
```

Compare before/after:

```bash
OUT=/tmp/before.txt COUNT=10 ./bench.sh
# make code changes
OUT=/tmp/after.txt COUNT=10 ./bench.sh
benchstat /tmp/before.txt /tmp/after.txt
```

## Reading results

Treat these as order-of-magnitude signals, not universal constants.

Expected shape:

```text
GoDirect          fastest
GoThroughApp      small registry/interface overhead
LuaReplace        Go -> Lua call overhead
LuaWrapGo         Go -> Lua -> Go overhead
RunCommand        Lua handler dispatch + context creation
EmitEvent         event allocation + Lua handler dispatch
Validate/Reload   VM creation + script load cost
```

Rules of thumb:

- Startup, reload, validation, and human-triggered commands can spend
  microseconds to milliseconds.
- Request-time hooks need measurement against the real workload.
- Per-token, per-row, or tight-loop work should stay in Go unless a future
  LuaJIT/pool mode proves otherwise.

## Adding benchmarks for a new seam

When adding a seam, benchmark these four variants where possible:

1. Direct Go implementation.
2. Go implementation through the host/registry.
3. Lua replacement.
4. Lua wrapper around the Go implementation.

Keep benchmark Lua small and representative. Use `io.Discard` for output so the
benchmark measures the extension runtime, not terminal I/O.
