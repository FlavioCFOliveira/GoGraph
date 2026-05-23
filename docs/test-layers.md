# Test layers

GoGraph organises its test corpus into three layers ordered by runtime
budget. Every test belongs to exactly one layer; deeper layers are
strict supersets of the shallower ones.

| Layer | Budget | Selector | Default? |
|---|---|---|---|
| `short` | < 60 s per package | none (default) | yes — every PR |
| `soak` | minutes | `-tags=soak` or `SOAK_FULL=1` | no |
| `nightly` | hours | `-tags=nightly` or `GOGRAPH_NIGHTLY=1` | no |

The mapping is monotonic: the `soak` layer always includes the `short`
layer; the `nightly` layer always includes both `short` and `soak`.
There is no way to run a deeper layer alone, by design — a regression
in the short layer must surface before the longer suites are even
considered.

## How a test selects its layer

Two mechanisms are supported. Prefer the first whenever practical.

### 1. Compile-time build tag (preferred)

Place layer-specific tests in their own file with a build-tag header
on the first line:

```go
//go:build soak

package myfeature
```

```go
//go:build nightly

package myfeature
```

Tests in such files are not compiled when the tag is absent, so they
have **zero runtime cost** outside their layer. This is the canonical
mechanism; the existing `internal/stress/` package uses the same
pattern with the `stress` tag (which is part of the soak family — see
below).

### 2. Runtime gate via `testlayers` helpers

When splitting a test into its own file is impractical — for example,
when a single test function mixes short-layer assertions with optional
soak-only steps — call one of the helpers in `internal/testlayers`:

```go
import "gograph/internal/testlayers"

func TestSomething(t *testing.T) {
    testlayers.RequireSoak(t)
    // ... soak-only body ...
}

func TestSomethingHeavier(t *testing.T) {
    testlayers.RequireNightly(t)
    // ... nightly-only body ...
}
```

`RequireSoak` and `RequireNightly` call `t.Skip` with a descriptive
message when the corresponding layer is inactive. Skipped tests are
near-instant; the cost is the `t.Skip` call itself.

## Build tags and environment variables

| Variable / tag | Effect |
|---|---|
| `-tags=soak` | activates the soak layer at compile time |
| `-tags=nightly` | activates the nightly layer at compile time (and implies soak) |
| `SOAK_FULL=1` | activates the soak layer at runtime via the helpers |
| `GOGRAPH_NIGHTLY=1` | activates the nightly layer at runtime via the helpers (and implies soak) |

`SOAK_FULL` is preserved verbatim from the pre-existing toolchain so
existing CI workflows and developer aliases continue to work.
`GOGRAPH_NIGHTLY` is new; it pairs with the new `nightly` build tag.

The two mechanisms are independent: build tags gate compilation, env
vars gate runtime behaviour of helpers. A test guarded by
`//go:build soak` is invisible to `SOAK_FULL=1 go test ./...` because
the file is not compiled into the binary in the first place. Conversely,
a test guarded by `testlayers.RequireSoak(t)` compiles in every layer
and is admitted at runtime when either the `soak` tag is set or the
env var is set.

## Sample invocations

```bash
# Short layer only (PR-CI default).
go test ./... -count=1 -race

# Short + soak.
go test -tags=soak ./... -count=1 -race
SOAK_FULL=1 go test ./... -count=1 -race

# Short + soak + nightly.
go test -tags=nightly ./... -count=1 -race
GOGRAPH_NIGHTLY=1 go test ./... -count=1 -race
```

## Relationship to the existing `stress` tag

The `internal/stress/` package is gated by `//go:build stress` and was
introduced before the three-layer scheme. It runs a short concurrent
workload under `-race` to catch scheduler-dependent issues, and is
wired into the `stress` job of `.github/workflows/ci.yml`. The
`stress` tag is considered part of the **soak family**: anything
gated by `stress` belongs conceptually to the soak layer, even though
it uses a distinct tag for historical and CI-scheduling reasons.
There is no plan to rename it; new soak-layer work should use the
`soak` tag or `SOAK_FULL=1` instead.

The longer-running 4-hour Bolt soak in `bench/soak/` continues to use
its own `soakfull` tag (see `.github/workflows/soak.yml`). Like
`stress`, it is considered part of the soak family.

## Helpers reference

`gograph/internal/testlayers` exposes the runtime API. The package is
internal: it is consumed from elsewhere in this module and is not part
of GoGraph's public surface.

| Symbol | Kind | Description |
|---|---|---|
| `RequireSoak(tb testing.TB)` | function | skips `tb` unless the soak layer is active |
| `RequireNightly(tb testing.TB)` | function | skips `tb` unless the nightly layer is active |
| `IsSoak` | constant `bool` | compile-time flag, true under `-tags=soak` |
| `IsNightly` | constant `bool` | compile-time flag, true under `-tags=nightly` |

The two constants are useful when a test must branch its body on
layer membership rather than skip wholesale, for example to enlarge a
workload size from "hundreds of nodes" to "millions of nodes" under
the deeper layer.
