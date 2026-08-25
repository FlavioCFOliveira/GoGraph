# Test layers

GoGraph organises its test corpus into three layers ordered by runtime
budget. Every test belongs to exactly one layer; deeper layers are
strict supersets of the shallower ones.

| Layer | Budget | Selector | Default? |
|---|---|---|---|
| `short` | < 60 s per package soft, 240 s hard (**enforced on `make ci`** — see below) | none (default) | yes — the default local run |
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

### The short-layer budget, and what enforces it (rmp #2566, #2577, #2599)

The per-package cost budget is **enforced on the routine gate**: `test-short`
pipes its output through `scripts/pkg_time_budget.sh`, so every `make ci` reads
it. `SOFT_BUDGET` (60 s) warns; `HARD_BUDGET` (240 s) fails the run.

The script echoes the `go test` output through **verbatim** and reads the plain
`ok<TAB>pkg<TAB>0.330s` summary lines, so **what you see on `make test-short` is
unchanged**. It deliberately does not use `-json` on that path: `-json` implies
`-v` and would bury the routine run in per-test noise to obtain the identical
per-package numbers.

| Target | What it is |
|---|---|
| `make test-short` | the short layer **and** the budget check — one run, both outcomes |
| `make test-short-timings` | an alias for `test-short`, kept as the named entry point for ad-hoc exploration (`SOFT_BUDGET=30 make test-short-timings`) |
| `make test-timing` | a different gate entirely: the serial wall-clock phase (rmp #2517), not this |

`test-short-timings` is now an alias because its own copy of the command had
**drifted**: it omitted `GOGRAPH_PARALLEL_SUITE`, making it the one target that
ran the whole parallel suite with the quiet-machine gates *asserting* — exactly
the contention rmp #2517 removed everywhere else. Delegation makes that class of
drift impossible.

#### Measured cost, 2026-08-25

Whole suite, `go test -race -count=1 ./...`, darwin/arm64, 10 cores, **load
average 1.08 before / 5.00 after**, 11 m 34 s wall. Ten packages exceed the 60 s
soft budget:

| Package | In-suite |
|---|---|
| `internal/sim` | **564.0 s** |
| `cypher` | **276.4 s** |
| `examples/26_social_scale_bench` | 146.3 s |
| `internal/anomaly` | 115.8 s |
| `bench/csrorder` | 86.0 s |
| `bench/cyclicjoin` | 80.3 s |
| `bench/cypher_scale` | 72.4 s |
| `search` | 65.9 s |
| `examples/24_social_network_cli` | 65.6 s |
| `store/recovery` | 62.0 s |

Only the first two exceed the 240 s hard ceiling. **The global ceiling was not
relaxed to accommodate them**; each gets a named, measured override instead, so
the accommodation is visible per package and cannot silently cover a third.

#### The override rule

`PKG_HARD_BUDGET_OVERRIDES` entries are **one stated rule, not a number fitted
per package**: the **worst** in-suite figure ever recorded for that package in
this document × 1.25, rounded up to the whole minute.

| Package | Worst in-suite | × 1.25 | Ceiling |
|---|---|---|---|
| `internal/sim` | 602.9 s | 753.6 s | **780 s** |
| `cypher` | 276.4 s | 345.5 s | **360 s** |

**Worst-observed, not last-measured.** `internal/sim` has been recorded in-suite
on this hardware at 545.8 s, 557.4 s, 564.0 s and 602.9 s — a **10.5 % spread** —
and twice more at 600.7 s and 601.7 s, both of which were `panic: test timed out`
against the old 600 s default and so are *lower bounds* on the real cost. A
ceiling fitted to whichever run happened to be measured would false-red on a
busier day; 780 s leaves **29 %** headroom over the worst of them while still
tripping on a genuine 25 % cost regression, and stays far clear of
`SHORT_TIMEOUT` (30 m).

The `cypher` figure rests on a **single** observation. It carries less evidential
weight than the `internal/sim` one and should be re-derived once a second is
recorded.

Keys match as a **suffix** of the import path, not as a substring. This matters:
substring matching would have let `/cypher` also cover `cypher/tck`, `cypher/ir`
and `bench/cypher_scale`, handing an unrelated package a ceiling nobody measured
for it. `internal/scriptgate` asserts every key still names an existing package
directory, because a stale suffix left behind after a rename is not inert — it
starts covering whatever package later ends with it.

#### Where `internal/sim` spends its time

Recorded so the next person does not re-derive it. Whole package,
`go test -race -count=1 -v`, quiet host, 2026-08-25: **974 top-level tests** (22
skipped), **570.1 s** summed against a ~530 s wall.

| Test | Cost | Share |
|---|---|---|
| `TestSchemaMutation_NonVacuityGatesAreNotVerdicts` | 97.20 s | 17.1 % |
| `TestCypherSurface_Scenario_Passes` | 34.09 s | 6.0 % |
| `TestMVCCSessionsCrash_Deterministic` | 25.58 s | 4.5 % |
| `TestMVCCSessions_IsolationGreen20Seeds` | 25.56 s | 4.5 % |
| `TestMergeRel_MultiSeed` | 19.82 s | 3.5 % |
| *top 20 combined* | *331.4 s* | *58.1 %* |

**The package was re-baselined rather than split, and that was a coverage
decision, not an arithmetic one.** Removing even the single largest test leaves
~433 s — still above the old 420 s figure — so no surgical cut reaches the
ceiling; the remaining 954 tests hold the other 41.9 %. Moving the battery to
`soak` was rejected because this document requires it to run under `-race` on
**every** push to preserve the ACID/DST guarantee, and trading that for a cost
target inverts the correct-before-fast order.

The drift is real and is why the gate now runs: rmp #2577 measured **818 tests /
460.9 s** at commit `147e28e4` on 2026-08-20. Five days later the same package is
**974 tests / 570.1 s** — +19 % tests, +24 % cost, unremarked, because nothing on
the routine path was reading the number.

#### What rmp #2566 found, kept as the record

Until rmp #2566 this section claimed the budget was "enforced, not merely
documented". All three of its supporting claims were untrue at HEAD, and each was
verified individually: `make ci` did not invoke the gate; `HARD_BUDGET` defaulted
to `0`, which the script documents as *disabled*; and `PKG_HARD_BUDGET_OVERRIDES`
was set nowhere, so the `internal/sim` override attributed to "the `build + test +
race` job" existed no more than that job did — the only workflow is `release.yml`.

That paragraph is kept because it names the failure mode this section now exists
to prevent: **a ceiling nothing reads is decoration, and it reports success in
exactly the same way as a passing one.** `internal/scriptgate` holds the
regression tests, which fail on the tree described above.

When a package approaches the budget, split it or move its slow cases to the
`soak` layer rather than relaxing the threshold — unless, as with `internal/sim`,
moving it would cost coverage the layer exists to provide. Then re-baseline
deliberately, with the measurement recorded here.

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
| `GOGRAPH_PARALLEL_SUITE=1` | declares that packages are being tested **in parallel**, so `testlayers.RequireQuietMachine` skips wall-clock/throughput/CPU-time assertions. Set by `test-short` and `cover-gate`; deliberately NOT set by `test-timing`. Detected by PRESENCE, not value — an empty expansion still counts as set, so a Makefile slip cannot silently re-enable a timing gate under load (rmp #2517) |

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
| `make test-short` | short | `GOGRAPH_PARALLEL_SUITE=1 go test -race -count=1 -timeout=$(SHORT_TIMEOUT) ./... \| bash scripts/pkg_time_budget.sh` |
| `make test-timing` | short (serial phase) | `go test -race -count=1 -p 1 -timeout=$(TIMING_TIMEOUT) -run '$(TIMING_RUN)' $(TIMING_PKGS)` |
| `make test-soak` | soak | `go test -race -count=1 -timeout=$(SOAK_TIMEOUT) -tags=soak ./...` |
| `make test-nightly` | nightly | `go test -race -count=1 -timeout=$(NIGHTLY_TIMEOUT) -tags=nightly ./...` |

`test-timing` is not a fourth layer. It is the **same** short layer re-run
serially for the subset of tests whose assertion is a duration, a rate, or a
ratio of them — the phase in which that measurement is valid. `make ci`,
`make ci-soak` and `make ci-nightly` all invoke it, so those assertions gate
every push. See [`RequireQuietMachine` and the `test-timing` phase](#requirequietmachine-and-the-test-timing-phase).

### Why every layer passes an explicit `-timeout`

Go applies a default of **10 minutes per package** when no `-timeout` is given.
Every layer target therefore sets one explicitly, through an overridable
variable, so the timeout is a **backstop against a hung package** and never a
budget the suite is expected to approach.

| Variable | Default | Applied by |
|---|---|---|
| `SHORT_TIMEOUT` | `30m` | `test-short`, `test-short-timings`, `race` |
| `SOAK_TIMEOUT` | `4h` | `test-soak` |
| `NIGHTLY_TIMEOUT` | `12h` | `test-nightly` |
| `NIGHTLY_CI_TIMEOUT` | `4h` | `test-nightly-ci` |

All four are overridable on the command line, so a slower or faster host needs
no edit to the Makefile. `scripts/cover_gate.sh` — the *second* whole-suite pass
inside `make ci` — carries its own hard-coded `-timeout=20m` for the same
reason.

**A `-timeout` is not a cost budget.** The per-package *cost* budget is a
separate concern with its own instrument, `scripts/pkg_time_budget.sh`, which
`test-short` pipes through on every run (60 s soft, 240 s hard, plus the two
measured overrides above). Raising a timeout must never be read as raising that
budget: a 30 m timeout catches a package that HUNG, the budget catches one that
merely grew.

#### The short layer: `SHORT_TIMEOUT` (rmp #2584)

`SHORT_TIMEOUT` defaults to **30m**. Before it existed, `test-short` passed no
`-timeout` at all, so Go's 600 s default applied and two packages ran close
enough to it that ordinary machine load turned a green gate red.

Measured on the reference host (Apple M4, 10 cores, `darwin/arm64`, go1.26.6)
on **2026-08-20**:

| Observation | Result |
|---|---|
| Commit `147e28e4`, isolated worktree, `go test -race -count=1 ./...` | Whole suite green, exit 0 — but `ok internal/sim 545.794s`, i.e. only **~9% headroom** under the 600 s default, and `ok cypher 433.236s` |
| With the rmp #2488 work in the tree, two consecutive `make ci` runs | `FAIL internal/sim 600.705s` and `FAIL internal/sim 601.675s`, both `panic: test timed out after 10m0s` |
| Was it hung? | **No.** The second run was 3 s into `TestTypeCoverage_NonVacuous` when the alarm fired — cumulative budget exhaustion, not a deadlock |
| Run-to-run variance | `cypher`, which #2488 does not touch, measured **433.2 s / 589.5 s / 576.8 s** across three runs (**+33%**) |
| Parallel-package contention | `internal/sim` takes **487.6 s** in isolation against **545.8 s** inside `go test ./...`, which runs package binaries concurrently |

The variance alone (+33% on an untouched package) exceeds the headroom, so this
is a **headroom** problem, not an attribution problem: no amount of care in
apportioning the cost would make a 600 s ceiling survive it.

`make race` carries the same variable. It is not a gate — `ci` is
`tidy fmt vet build test-short lint cover-gate` — but it runs the same corpus
under the same detector, so it has the same exposure.

**Why 30m.** It is **3.05×** the slowest package measured that actually
completed (`cypher`, 589.5 s) and **1.5×** the `-timeout=20m` the coverage pass
of the same `make ci` gate already applies. That margin is what makes a machine
under sustained competing load reach the same verdict as an idle one, while
still failing a genuinely hung package in bounded time. Check growth against
the measurements above: a package approaching 30m is a cost regression to
investigate, not a reason to raise the value.

#### The deferred layers: `SOAK_TIMEOUT`, `NIGHTLY_TIMEOUT`, `NIGHTLY_CI_TIMEOUT`

The soak layer cannot satisfy the 10-minute default at all.

Measured on an Apple M4 (10 cores, `darwin/arm64`, go1.26.5), in an isolated
worktree so machine contention could not be the explanation:

| Package | Under `-race`, `-tags=soak` | Verdict |
|---|---|---|
| `graph/io/csv` | **800.8 s — passes** | 1.33× the 10-minute default: could never finish inside it, on any machine |
| `cypher` | did not complete at **45 min** | slow, not hung — see below |
| `internal/shapegen` | did not complete at **45 min** | `TestStructured_Hypercube_Soak` still running at 40m19s |
| `internal/sim` | did not complete at **45 min** | `TestSimulator_Soak` still running at 30m57s |

**These packages are slow, not deadlocked.** The decisive check:
`cypher/TestDetachDelete_Hub1M_Soak` builds 1 000 000 nodes and 1 000 000 edges
and then issues one `DETACH DELETE`. With the race detector **off** it passes in
**724.2 s**; with it **on** it had not finished after 44m24s. The goroutine dumps
at timeout show the test bodies executing rather than blocked.

The defaults above are therefore chosen so that the timeout is **not the binding
constraint** — not to encode a known total runtime, because the three long
packages have not been measured to completion under `-race`.

> **Open question, deliberately not decided here.** This layer is specified as
> "minutes-long workloads", yet three packages exceed 45 minutes under `-race`,
> which is nightly-class by that definition. Either those specific tests belong
> in the nightly layer, or the soak target should not use `-race`, or the layer
> definition should change. Moving a test between layers changes what each gate
> means, so it is recorded here rather than settled unilaterally.

Three composite pipeline targets wrap these:

| Target | Purpose |
|---|---|
| `make ci` | Full local gate: tidy + fmt + vet + build + **test-short** + lint + cover-gate |
| `make ci-soak` | Like `ci` but runs **test-soak** instead of test-short |
| `make ci-nightly` | Like `ci` but runs **test-nightly** instead of test-short |

## What the soak layer asserts — and what it does not

**Running `-tags=soak` does not, on its own, certify no-growth or latency
stability.** The reliability instruments in `bench/soak/` fit a linear
regression over a measurement window, and the short layer's window is far too
small to hold one. This is a property of the layer's 60-second budget, not a
defect — but it must never again be read as evidence, because the 2026-08-10
certification did exactly that (rmp #2396): the layer reported `ok` while
neither slope check had evaluated anything.

| Instrument | Asserts in the short layer | Needs a longer window |
|---|---|---|
| `TestGCPause_Stable` | **max GC pause < 200 ms** — a ceiling needs no regression, so it asserts from a single sample | pause-slope regression |
| `TestNoGrowth_HeapFDGoroutine` | nothing; it exercises the workload and writes the CSV | heap / goroutine / fd slopes |
| `TestLatencyP99_Stable` | that round-trips succeeded at all (zero successes is an error) | p99-over-time slope |
| `TestBoltSoak_60s`, `TestBoltCypherMixed_Smoke`, `TestCypherRW_Analytics_Smoke` | success/failure counts, cap errors, goroutine deltas | — |

A slope check that cannot run **skips** rather than passing, so `go test -v`
shows `--- SKIP` and names the setting that would make it assert. Run the soak
layer with **`-v`**: without it `go test` prints only `ok` for the package, and
Go shows neither the skip nor its reason — so a non-verbose soak run still
cannot be read as evidence that anything was asserted. Skipping is
allowed *only* in the short layer at its default window: under `SOAK_FULL=1`,
`GOGRAPH_NIGHTLY=1`, or any explicit `SOAK_*` window override, too few samples
is a **failure**, because those configurations assert that the criterion will be
evaluated. The floor is `minRegressionPoints` = 6 samples
(`bench/soak/soakenv_test.go`): ordinary least squares spends two points on the
fit itself, so two points regress with zero residual degrees of freedom and
three leave one, while six leave four — the smallest count at which one
anomalous sample cannot dictate the slope.

Intermediate windows that make each slope check assert without paying for the
full multi-hour variant:

```bash
# Heap / fd / goroutine growth — 9 samples in ~95 s.
SOAK_NOGROWTH_MEASURE=90s go test -tags=soak -count=1 -v \
  -run TestNoGrowth_HeapFDGoroutine ./bench/soak/

# p99 stability — 10 windows, 8 post-warm-up, in ~100 s.
SOAK_P99_DURATION=100s SOAK_P99_WINDOW=10s go test -tags=soak -count=1 -v \
  -run TestLatencyP99_Stable ./bench/soak/

# GC pause slope — 12 samples in ~60 s.
SOAK_GCPAUSE_MEASURE=60s go test -tags=soak -count=1 -v \
  -run TestGCPause_Stable ./bench/soak/
```

The sample *interval* is deliberately not overridable. Shrinking it is the one
adjustment that manufactures samples without lengthening the observation, which
would convert the regression into a noise detector over a handful of adjacent
points. The window is what gets extended.

The release-grade variants remain `SOAK_FULL=1` (5 min warm-up + 55 min
measurement for no-growth, ~330 samples; 4 h at 64 goroutines for p99).

## Wall clock is not a short-layer instrument

The short layer is not a quiet machine. `make ci` runs `go test -race ./...`,
which executes package binaries concurrently, so a wall-clock measurement taken
there reads machine load as much as it reads the code.

**A last/first ratio is not a defence against this.** It cancels load that is
*constant* across the measurement, and `make ci` load is not constant — sibling
packages start and finish during a run. Measured on a 10-core `darwin/arm64`
host under `-race`, against the *flat* (post-fix) delete path of
`cypher/delete_scaling_test.go`, with `yes > /dev/null` workers as load:

| Load regime | Wall per cycle, first … last | Wall last/first | CPU last/first |
|---|---|---|---|
| idle | 239 ms … 240 ms | 1.00× | 1.03× |
| 300 workers, constant | 7.80 s … 9.10 s | 1.17× | 0.99× |
| 300 workers, ramping up during the run | 620 ms … 9.91 s | **15.98×** | 1.00× |
| idle at the first cycle, saturated at the last | 239 ms … 8.59 s | **35.88×** | 1.50× |

The defect these gates gate against moves the same statistic by 5.2×. Wall
clock in the short layer therefore carries up to 35.9× of noise against 5.2× of
signal, and it duly failed `make ci` on a flat engine.

An absolute **ceiling** — the shape recommended above, because it needs no
window — does not rescue the measurement. To survive constant saturation it
would have to sit above 9.10 s, while the regression it guards costs about
1.2 s on an idle machine: it would be blinder than the ratio, not safer. A
ceiling is the right short-layer shape only for a quantity the machine's load
cannot inflate.

### Resolution (rmp #2589): allocation volume is the instrument these gates needed

The delete-scaling gates no longer measure time at all, and therefore no longer
need `RequireQuietMachine`. They assert on **allocation volume**, which returned
all three to asserting in `test-short` on every push — strictly more coverage than
the serial phase gave them, and `cypher` left `TIMING_PKGS` entirely.

Allocation was previously rejected for these gates on a precise ground, recorded in
#2589: it is load-invariant here (mallocs differed 0.5% across a 35.9× wall
inflation) but *"nothing establishes that the pre-fix O(k·n) Mapper.Walk allocated
in proportion to the nodes it scanned. An oracle whose power against the actual
defect is unknown is cover, not a gate."* Invariance was established; **power was
not**.

The missing experiment was then run, using the method #2572 had supplied — model
the defect and measure whether the instrument sees it:

| workload | per-cycle allocated bytes, last/first |
|---|---|
| flat (healthy, fixed work per cycle) | **0.87×** |
| degrading (6× more work in the last cycle) | **8.66×** |

A 10× separation, with the existing 2.5× threshold sitting between them — 2.9× of
headroom below and 3.5× of margin above. Against the CPU ratio, which read 2.90× on
the *flat* workload (a false red) and 1.52× on the *degrading* one (a false pass).
The threshold did not change; only the instrument did.

**The general lesson.** When a load-invariant instrument is rejected for unknown
power, that is a missing measurement, not a dead end. Build a control that models
the defect and measure the separation.

### Correction (rmp #2517, 2026-08-25): process CPU time is NOT load-invariant

The rule below used to read "in the short layer, assert on an instrument load
cannot move — process CPU time is the load-invariant analogue of wall time",
and quoted a worst-case CPU noise of 1.50× from the table above. **That rule was
wrong, and following it is what produced rmp #2517 and #2589.**

Measured on the same flat engine under `make ci`:

| Test | Under `make ci` | In isolation |
|---|---|---|
| `TestDetachDeleteDoesNotDegradeAcrossCycles` | **2.90×** CPU ratio against a 2.5× limit; per-cycle 852 ms → 2.48 s | **0.94×**; per-cycle 19.3, 17.8, 18.5, 18.9, 18.2, 18.1 ms |

The cycles ran 45–130× slower under the gate, and 2.90× is well past the 1.50×
the table records as CPU's worst case — that figure was measured against
synthetic `yes` workers, not against sibling Go test binaries under `-race`,
which contend for cache, TLB, and the scheduler in ways that charge real CPU to
the measured goroutine. Its sibling power control fails the mirror way: contention
*compresses* the ratio it must exceed, so the control stops firing (#2589).

Contention inflates CPU time. There is no cheap load-invariant proxy for a
duration; the honest options are a genuinely invariant *different* quantity
(allocation counts, operation counts, a fitted complexity exponent) or a quiet
machine.

The rules this yields:

- **Assert on a duration, a rate, or a ratio of them only where the machine is
  quiet.** Two places qualify: the soak layer, and the `test-timing` phase
  described below.
- **In the short layer, prefer a quantity that is invariant by construction** —
  allocation counts, operation counts, fitted exponents — and only where its
  power against the actual defect is established. `bench/cyclicjoin` is the
  worked example: its per-point win is asserted in allocations (2.7×–27× margin,
  run-to-run spread below 0.01%) while its wall-clock arm is only a coarse guard.
- **Where no invariant quantity exists, guard the timing assertion with
  `testlayers.RequireQuietMachine`** rather than widening its tolerance. For
  roughly a third of the audited assertions — the plain "elapsed vs a constant"
  hang detectors — the duration *is* the claim, and there is nothing to substitute.

### `RequireQuietMachine` and the `test-timing` phase

`testlayers.RequireQuietMachine(tb, detail)` skips a timing assertion when
`GOGRAPH_PARALLEL_SUITE` is set. `test-short` and `cover-gate` set it, because
both run packages concurrently; `test-timing` deliberately does not.

**The default is to ASSERT.** The variable's presence is what causes a skip, so a
single-package run — `go test ./cypher/`, or a `-run` filter while investigating —
still checks the gate. No gate silently disappears from a developer's own runs.

**Nothing moves out of the pre-push gate.** `make ci` invokes `test-timing` as its
own phase, which re-runs exactly the guarded gates serially (`-p 1`) and without
the variable, so every one of them still gates every push — in the phase where the
measurement means something. Measured cost: **59 s** for the gates guarded so far,
against 193 s for the `cypher` package alone under `-race` in the same `make ci`.

`TIMING_PKGS` and `TIMING_RUN` in the `Makefile` list only the gates actually
guarded today. The complete inventory of affected assertions — 39 across 12
packages, with the measurement that motivated each — is
[`short-layer-wallclock-audit.md`](short-layer-wallclock-audit.md).

The skip is **loud by contract**: every caller passes the quantity it was about to
assert on, and the message names `test-timing` as the place the assertion still
runs, so a skipped gate can never be mistaken for a passing one. This is the same
contract `RequireUninstrumented` carries for coverage instrumentation, and the two
compose — `bench/mvccwrite`'s instrument controls call both, because the same
precondition is defeated by two different causes.

- **A test that measures process CPU must not call `t.Parallel()`.** `getrusage`
  is process-scoped, so a concurrent sibling is charged to the measurement. Go
  runs non-parallel top-level tests to completion before resuming any test that
  called `t.Parallel()`, which is what makes the measurement attributable. Load
  from *other packages* is irrelevant: that is a different process.
- **`runtime/metrics` `/cpu/classes/*` is not CPU time** for this purpose. It is
  derived from `GOMAXPROCS` × wall-clock accounting, so a goroutine descheduled
  by the OS still accrues it. In the very run where `getrusage` moved 1.50×,
  `/cpu/classes/user:cpu-seconds` moved 50.2× — it is wall clock wearing a CPU
  name, and substituting it would look like a fix while changing nothing.
- **A load-invariant instrument still needs proven power.** Allocation counts
  are perfectly invariant here (0.5% apart across a 35.9× wall inflation) but
  nothing establishes that the regression allocated in proportion to the work it
  did, so they are not asserted on. Where the defective build cannot be rebuilt,
  ship a control that drives a deliberately degrading workload and requires the
  gate to *fire*.

The delete-scaling gates are split accordingly:

| Test | Layer | Asserts |
|---|---|---|
| `TestDeleteDoesNotDegradeAcrossCycles`, `TestDetachDeleteDoesNotDegradeAcrossCycles` | short | last/first **ALLOCATION** ratio ≤ 2.5× (rmp #2400 / #2418, instrument changed by #2589). Measures 0.97× and 0.88×; **no guard needed** — see below |
| `TestDeleteCycleGateDetectsDegradation` | short | that the 2.5× gate still **fires** on a workload engineered to degrade: allocation ratio **8.84×** against the 2.5× limit |
| `TestDeleteWallTimeDoesNotDegradeAcrossCycles`, `TestDetachDeleteWallTimeDoesNotDegradeAcrossCycles` | soak | last/first **wall** ratio ≤ 2.5×, on a quiet machine |
| `TestSingleStatementDeleteOfNinetyThousandNodes` | soak | an absolute 10 s budget for a 90 000-node single-statement delete |

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
| `RequireUninstrumented(tb, detail)` | function | skips `tb` when built with coverage instrumentation, which compresses the arms a concurrency-**effect control** compares. For controls only (rmp #2319) |
| `Instrumented() bool` | function | reports whether coverage instrumentation is active, for a caller that adjusts rather than skips |
| `RequireQuietMachine(tb, detail)` | function | skips `tb` when `GOGRAPH_PARALLEL_SUITE` is set, i.e. when packages are being tested in parallel and a **timing** assertion would measure machine load. Defaults to ASSERTING; the assertion still runs in `make test-timing` (rmp #2517) |
| `InParallelSuite() bool` | function | reports whether this is a parallel whole-suite run, for a test with BOTH load-independent and timing arms that must guard only the timing one — `bench/cyclicjoin` is the worked example |

`detail` is mandatory on both `Require*` guards above and must name the quantity
the caller was about to assert on. That is what makes a skip self-explaining, so a
skipped gate can never be mistaken for a passing one.

The two constants are useful when a test must branch its body on
layer membership rather than skip wholesale, for example to enlarge a
workload size from "hundreds of nodes" to "millions of nodes" under
the deeper layer.
