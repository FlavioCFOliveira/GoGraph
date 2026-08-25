# DST parallel exercise — 2026-08-25

Three deterministic-simulation-testing (DST) swarm instances, distinct master seeds, run
simultaneously for 5 minutes, repeated for three cycles with fresh seeds each cycle, to
assess the correctness of the graph engine.

## Method

Binary built from a clean tree at HEAD `f1602db1` (`git status --short` empty before and
after). Each cycle launched three independent processes together:

```
simbin <master-seed> -swarm -duration=5m -runs=0 -bias -coverage-report -workers=3
```

| Cycle | Master seeds |
|---|---|
| 1 | 2026082511, 2026082512, 2026082513 |
| 2 | 2026082521, 2026082522, 2026082523 |
| 3 | 2026082531, 2026082532, 2026082533 |

Nine of ten cores were committed per cycle (3 instances x 3 workers). Exit codes were
written to per-instance files and read from there, never inferred from the wrapper.

`-runs=0` is mandatory alongside `-duration`: the count cap is dropped only when
`runs <= 0` is passed explicitly, so a wall-clock swarm is otherwise silently truncated
at the default 200 runs. The compiled binary is used rather than `go run`, which
swallows a panic's exit 2 and reports 1.

## Detection envelope

What the harness was capable of failing, stated before reading anything into the result:

- `SimDisk` has a durable shadow and `SimStore.Crash()` aliases `CrashHost()`, so every
  `.Crash()` site discards unsynced data and durability oracles can genuinely fail.
- Snapshot **index hydration is dead code**: `store/recovery.applySnapshotIndexes`
  requires an index registered on the graph's `index.Manager`, nothing calls
  `SetIndexManager` on the recovery-built graph, so `Result.SnapshotIndexes` is provably
  always 0. No run in this exercise deserialized an index payload.
- `long-running` is excluded from the swarm bias universe, so coverage is against 35
  scenarios, not the catalogue's 36.
- Zero-failure runs report fewer coverage buckets, because the `op-kind` and `violation`
  dimensions only materialise on failure. That is the result, not a regression.

## Results

| Cycle | Runs | Passes | Failures | Instances exit 0 |
|---|---|---|---|---|
| 1 | 3910 | 3897 | 13 | 0 of 3 |
| 2 | 2572 | 2547 | 25 | 0 of 3 |
| 3 | 4659 | 4659 | 0 | 3 of 3 |
| **Total** | **11141** | **11103** | **38** | |

Failure accounting is **exact** in every cycle: 38 declared failures = 38 reproduce
lines, zero unexplained.

**Zero graph-engine defects.** All 38 failures are harness artefacts with a single root
cause. No `ACID_ATOMICITY`, `ACID_CONSISTENCY`, `ACID_ISOLATION`, `ACID_DURABILITY`, or
`GRAPH_INTEGRITY` violation was observed in 11141 runs.

### Failure population

| Scenario | Failures | Clause |
|---|---|---|
| `bolt-decode-swarm` | 37 | `nv-swarm-rejections`, `nv-swarm-pressure-density` |
| `pagerank-ranker` | 1 | harness error, self-identified |

Per-scenario rates, with the sibling scenario as control:

| Cycle | `bolt-decode-swarm` | rate | `bolt-decode-pressure` | rate |
|---|---|---|---|---|
| 1 | 12 / 73 | 16.4% | 0 / 74 | 0% |
| 2 | 25 / 49 | 51.0% | 0 / 48 | 0% |
| 3 | 0 / 87 | 0% | 0 / 86 | 0% |

## Root cause

`runCPUStarvation` (`internal/sim/catalogue.go:221`) clamps process-global `GOMAXPROCS`
to 1 for the duration of its run:

```go
prev := runtime.GOMAXPROCS(cpuStarvationGOMAXPROCS) // = 1
defer runtime.GOMAXPROCS(prev)
```

Its own godoc states the constraint — *"The clamp is process-global, so the integration
test must not run in parallel"* — but `-swarm -workers=N` runs N scenarios in parallel in
one process, and `cpu-starvation` is in the swarm universe. A co-scheduled scenario
therefore loses the parallelism its claims rest on.

At `GOMAXPROCS=1` the PAIR rendezvous cannot hold two abusers in flight, so pool pressure
is never constructed, the 10 s start barrier expires, and the non-vacuity gates fire. The
clause text says so directly:

> 4 abusers produced NO refusal (start barrier satisfied: false): the pool was never
> actually pressured, so every clause about how it refuses passed without being tested

The `pagerank-ranker` failure is the same cause, caught by that scenario's both-sided
read-back guard and correctly reported as a harness error rather than a violation:

> GOMAXPROCS was 1 at the end of a window clamped to 4: a foreign clamp landed mid-window

### Evidence

A/B over the 37 failing seeds, holding the seeds fixed and varying **only** whether
`cpu-starvation` ran concurrently in the same process:

| Arm | Failed |
|---|---|
| No neighbour | **0 / 37** |
| 8 concurrent CPU spinners, 3 sweeps | **0 / 37** |
| `cpu-starvation` neighbour | **37 / 37** |

The spinner arm matters: it rules out generic CPU load as the trigger, which is the load
model rmp #2588 and #2611 searched with and found unable to reach the failing regime.

## Two structural observations

**The failures are not seed-reproducible.** Replaying all 37 seeds reproduced none. A
DST failure whose printed reproduce line does not reproduce breaks the harness's
load-bearing determinism invariant: whether a run fails depends on what a *neighbouring
worker* drew, which the seed does not capture.

**A vacuity gate is reported as a verdict.** The `nv-swarm-*` clauses emit
`ViolationOracleDeviation`, which reads as "the graph engine deviated from its oracle".
The run was genuinely vacuous, but the engine never deviated from anything — it was never
pressured. This is the defect shape rmp #2554 fixed elsewhere by making gates return
`[]string` rather than `[]Violation`, so the type forbids the promotion.

## A measurement error worth recording

The per-cycle failure rate tracked machine throughput inversely and perfectly
monotonically (16.4% at 3910 runs, 51.0% at 2572, 0% at 4659), which invited the reading
that concurrent load from the analysis tooling drove the failures. **That reading was
wrong**, and the A/B refuted it: 8 dedicated CPU spinners across 3 sweeps produced 0/37.
The real variable is whether `cpu-starvation` was drawn onto a neighbouring worker during
a `bolt-decode-swarm` run, which is probabilistic per master seed and unrelated to load.
A monotonic correlation across three points is not a mechanism.

## Filed

- **rmp #2613** (new) — the scheduling defect: the swarm co-schedules `cpu-starvation`
  with scenarios whose claims need real parallelism.
- **rmp #2606** — comment recording the first wild observation of the clamp hazard, which
  that task had filed on the mechanism alone.
- **rmp #2588** — comment recording the load model that reaches the failing regime.

## The fix

A scenario that writes process-global `GOMAXPROCS` now runs **alone**.
`internal/sim/gomaxprocs.go` adds one package-wide `sync.RWMutex`:

- the two clamping scenarios — `cpu-starvation` and `pagerank-ranker` — declare
  `Scenario.ClampsGOMAXPROCS` and take the **write** side across their clamped phase;
- every other scenario takes the **read** side inside `Scenario.Run`, so many run together
  but none can overlap a clamp.

The hold sits in `Scenario.Run`, not in `Swarm.runOne`, so the guarantee belongs to the
scenario API and covers every concurrent caller rather than only the swarm. Measured cost
on the package suite under `-race`: 525.9 s before, 531.8 s after — 1.1%, within noise.

Serialising the two clampers against each other also closes the interleaved save/restore
that could strand the process at the wrong value (rmp #2606). `runCPUStarvation`'s godoc
no longer says "the integration test must not run in parallel": the constraint is now
enforced rather than described.

`prWithClamp`'s both-sided read-back is deliberately kept. It should now never fire, and
it is the detector that caught the interference in the first place.

### Validation

Re-running the **identical master seeds** from cycle 2 — the 51% arm — with only the code
changed:

| | Runs | `bolt-decode-swarm` fails | rate | `cpu-starvation` runs | Instances exit 0 |
|---|---|---|---|---|---|
| Before | 2572 | 25 / 49 | 51.0% | 48 | 0 of 3 |
| After | 4516 | 0 / 85 | **0.0%** | 84 | **3 of 3** |

The result is not vacuous: the fixed run drew *more* of every scenario involved, so it had
strictly more opportunity to hit the defect and hit it zero times.

Throughput is **not** a controlled comparison — cycle 2 was contaminated by a concurrent
`go build` (see the measurement error above) and the two runs draw different scenario
mixes. Against the cleanest prior cycle (cycle 3, 4659 runs) the fixed run's 4516 is about
3% lower, which is the expected order for serialising a ~108 ms scenario drawn ~84 times,
but it is indicative rather than measured.

### Regression tests

`internal/sim/gomaxprocs_test.go`. Each carries a **red control**, so a green result cannot
mean "the two never overlapped":

- `TestBoltDecodeSwarm_SurvivesAConcurrentClamp` — the real production pairing: seeds drawn
  from the failing population run beside the real `cpu-starvation`, in one process.
- `TestSwarm_DoesNotCoScheduleAClampingScenario` — scheduler-level mutual exclusion. Its red
  control runs the two scenario bodies directly, bypassing the lock, because once
  `Scenario.Run` took the shared hold a sibling test holding the exclusive side could block
  the observer and manufacture a clean control. The control caught that itself and reported
  the arm vacuous rather than passing.
- `TestCPUStarvation_HoldsGOMAXPROCSExclusively` — the production path holds the exclusive
  lock for its whole run. The test holds the shared side itself, so the result is
  attributable to this goroutine rather than to some other test's clamper.
- `TestGOMAXPROCS_ConcurrentClampersRestoreTheOriginal` — the two real clamp paths driven
  against each other leave the process exactly as found (rmp #2606).
- `TestGOMAXPROCSWrites_AreAllDeclaredClampers` — structural tripwire: a new `GOMAXPROCS`
  write outside a declared clamping scenario fails at the moment it is written.

## Still open

The `nv-swarm-*` clauses report a vacuous run as `ViolationOracleDeviation`, which reads as
"the graph engine deviated from its oracle" when the engine was never exercised at all.
That mislabelling is why 37 failures pointed away from their cause. It was **not** changed
here: `ViolationKind` has no non-engine member, so adding one is a change to the violation
taxonomy and to the coverage report's `violation` bucket, and the shape of these particular
clauses is the open decision in rmp #2588. It needs a decision, not a unilateral edit.
