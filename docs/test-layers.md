# Test layers

GoGraph organises its test corpus into three layers ordered by runtime
budget. Every test belongs to exactly one layer; deeper layers are
strict supersets of the shallower ones.

| Layer | Budget | Selector | Default? |
|---|---|---|---|
| `short` | < 60 s per package | none (default) | yes — the default local run |
| `soak` | minutes | `-tags=soak` or `SOAK_FULL=1` | no |
| `nightly` | hours | `-tags=nightly` or `GOGRAPH_NIGHTLY=1` | no |

The mapping is monotonic: the `soak` layer always includes the `short`
layer; the `nightly` layer always includes both `short` and `soak`.
There is no way to run a deeper layer alone, by design — a regression
in the short layer must surface before the longer suites are even
considered.

### The `drivercompat` suite

One suite sits outside those three layers because it is not a duration
tier but a *capability* gate: the standing driver-compatibility suite
(`bolt/server/driver_compat_test.go`, task #2191). It drives the official
`neo4j-go-driver` against the in-process Bolt server and asserts 37
checks, reporting a pass/fail/degraded tally and **ratcheting** the
passing count — a check that regresses from pass to fail breaks the
build, exactly as the TCK execution baseline does for query results.

Run it explicitly:

```bash
go test -tags=drivercompat -run TestDriverCompatibility -v ./bolt/server/
```

It is tag-gated rather than layered because it stands up a server and a
real driver connection per check. It is excluded from the default run so
`make ci` keeps its short-layer budget; the current floor and the reason
each remaining check fails are recorded in the file's own header, which
is the authoritative list.

### Enforcing the short-layer budget

The `< 60 s per package` budget is enforced, not merely documented. The
`make test-short-timings` target runs the short layer once under `-race`
with `-json` and pipes it through `scripts/pkg_time_budget.sh`, which parses
per-package wall-clock and:

- emits a `::warning::` for any package over `SOFT_BUDGET` (60 s) so creep
  is visible in the job summary before it becomes a breach, and
- fails the job for any package over `HARD_BUDGET` (240 s, i.e. 4× the
  budget) — a genuine runaway, not a package merely near the line on a slow
  runner.

The measurement is taken under `-race`, which inflates wall-clock several-fold
over a plain run; the 240 s hard ceiling (4× the 60 s budget) is sized to
absorb that overhead and still catch a runaway.

Run it locally with `make test-short-timings` (override `SOFT_BUDGET` /
`HARD_BUDGET` to tighten the check). When a package approaches the budget,
split it or move its slow cases to the `soak` layer rather than relaxing
the threshold.

#### Documented per-package hard-ceiling overrides

A single package may carry a higher hard ceiling than the global 240 s when
it is legitimately heavy under `-race` and reducing it further would cost
required short-layer coverage. This is a documented, justified accommodation —
never a blanket relaxation — mirroring `cover_gate.sh`'s
`COVER_PKG_FLOOR_EXEMPT`. Overrides are supplied to `pkg_time_budget.sh` via
the `PKG_HARD_BUDGET_OVERRIDES` environment variable, a whitespace- or
comma-separated list of `path-substring=seconds` entries; a package whose
import path contains a key uses that key's ceiling instead of `HARD_BUDGET`.

The only current override, set in the `build + test + race` job:

| Package | Ceiling | Justification |
|---|---|---|
| `internal/sim` | 420 s | The deterministic-simulation (DST) integration harness. Its ACID/DST scenario battery is serial-dominated, so under `-race` it legitimately runs longer than a unit-test package. Its heaviest cases already run soak-only (the 9000-node index-diversity scenario and the seed-varied search scenarios); the remaining battery must still run under `-race` on **every** PR to preserve the ACID/DST guarantee, which the uniform 240 s ceiling would force out of the short layer. 420 s accommodates that cost with margin while staying well below the pre-reduction runtime (so a genuine regression still trips the gate) and clear of `go test`'s 10-minute timeout. |

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
import "github.com/FlavioCFOliveira/GoGraph/internal/testlayers"

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
existing scripts and developer aliases continue to work.
`GOGRAPH_NIGHTLY` is new; it pairs with the new `nightly` build tag.

### Diagnostic tags — not layers

Two further tags select *assertion* builds rather than test layers. They
do not admit or exclude tests by workload cost; they compile extra
run-time checks into the library itself, and the tests that pin those
checks are tagged to match.

| Tag | Effect |
|---|---|
| `-tags=gograph_debug` | compiles the development-only assertions into the library |
| `-race` | implies the barrier re-entrancy guard, in addition to the Go race detector |

Two assertions are currently gated this way:

- The **integer-distance overflow assertion** in `search/`
  (`overflow_assert_enabled.go` / `_disabled.go`), which panics when a
  Bellman-Ford or Johnson relaxation wraps around. Gated on
  `gograph_debug` alone.
- The **barrier re-entrancy guard** in `graph/lpg/`
  (`reentrancy_enabled.go` / `reentrancy_disabled.go`), which turns a
  same-goroutine nested `Graph.View` / `Graph.ApplyAtomically` — an
  engine-wide deadlock — into an immediate panic. Gated on
  `race || gograph_debug`, so the local gate (`go test -race ./...`,
  run by `make ci`) enforces it on every change. It is excluded from
  released binaries because identifying the calling goroutine needs a
  `runtime.Stack` call that measured **97–99 % of every `Graph.View`**
  (2088 ns against 4.0 ns for the RWMutex pair it guards, plus a 64 B
  allocation per call) and serialised readers on the Go runtime's
  process-global `debuglock`.

Reach for `-tags=gograph_debug` when diagnosing a suspected engine
freeze or a wrapped path distance in a build where `-race` is too slow.

The two mechanisms are independent: build tags gate compilation, env
vars gate runtime behaviour of helpers. A test guarded by
`//go:build soak` is invisible to `SOAK_FULL=1 go test ./...` because
the file is not compiled into the binary in the first place. Conversely,
a test guarded by `testlayers.RequireSoak(t)` compiles in every layer
and is admitted at runtime when either the `soak` tag is set or the
env var is set.

## Makefile targets

The three layers are wired into named `make` targets so the layering
discipline is enforced by tooling, not folklore.

| Target | Layer | Equivalent command |
|---|---|---|
| `make test-short` | short | `go test -race -count=1 ./...` |
| `make test-soak` | soak | `go test -race -count=1 -tags=soak ./...` |
| `make test-nightly` | nightly | `go test -race -count=1 -tags=nightly ./...` |

Three composite pipeline targets wrap these:

| Target | Purpose |
|---|---|
| `make ci` | Full local gate: tidy + fmt + vet + build + **test-short** + lint + cover-gate |
| `make ci-soak` | Like `ci` but runs **test-soak** instead of test-short |
| `make ci-nightly` | Like `ci` but runs **test-nightly** instead of test-short |

## Sample invocations

```bash
# Short layer only (the default local run) — via make or directly.
make test-short
go test ./... -count=1 -race

# Short + soak.
make test-soak
go test -tags=soak ./... -count=1 -race
SOAK_FULL=1 go test ./... -count=1 -race

# Short + soak + nightly.
make test-nightly
go test -tags=nightly ./... -count=1 -race
GOGRAPH_NIGHTLY=1 go test ./... -count=1 -race
```

## How each layer is run

There is no per-push or scheduled CI; the only GitHub Actions workflow is
`.github/workflows/release.yml` (tag-triggered). Each layer is run locally
by developer discipline before pushing or tagging:

| Layer | When to run | Command |
|---|---|---|
| **short** | before every push (also folded into `make ci`) | `make test-short` |
| **soak** | periodically and before a major release | dedicated soak harness / `SOAK_FULL=1 make soak` |
| **nightly** | periodically | `make test-nightly` |

The nightly target can also run benchmarks with `-cpuprofile` and
`-memprofile` so nightly regressions can be investigated with
`go tool pprof`.

## Relationship to the existing `stress` tag

The `internal/stress/` package is gated by `//go:build stress` and was
introduced before the three-layer scheme. It runs a short concurrent
workload under `-race` to catch scheduler-dependent issues, run locally
via its `stress` build tag. The `stress` tag is considered part of the
**soak family**: anything gated by `stress` belongs conceptually to the
soak layer, even though it uses a distinct tag for historical reasons.
There is no plan to rename it; new soak-layer work should use the
`soak` tag or `SOAK_FULL=1` instead.

The longer-running 4-hour Bolt soak in `bench/soak/` continues to use
its own `soakfull` tag. Like `stress`, it is considered part of the
soak family.

The `soakfull` tag also gates the two multi-hour DST endurance scenarios
in `internal/sim/phase4_long_running_soak_test.go` (2,000,000 and
1,000,000 ticks). Under the race detector these alone exceed the 600 s
`go test` default timeout, so they are excluded from the
`make test-nightly-ci` subset — which passes only `-tags=soak,nightly`
— while the full `make test-nightly` target passes
`-tags=soak,nightly,soakfull` and runs them. Excluding them loses no
scenario coverage: the `ScenarioLongRunning` run-path is exercised by the
short-layer `TestCatalogue_SmokeSubsetRunsClean` (part of `make ci`) and at
a 2,000-tick budget by the soak-layer `TestCatalogue_EachScenarioRunsClean`;
the endurance budget is a periodic heap/goroutine-stability watch, not a
release gate. This is the per-test analogue of the package-level
`search/extern` exclusion in `NIGHTLY_CI_PKGS`.

## Helpers reference

`github.com/FlavioCFOliveira/GoGraph/internal/testlayers` exposes the runtime API. The package is
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
