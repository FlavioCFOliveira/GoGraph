# DST parallel exercise — 2026-08-18

Three deterministic-simulation-testing (DST) swarm instances, distinct master seeds,
run simultaneously for 30 minutes to search for undesired behaviour and assess the
correctness of the module.

## Method

Binary built from a clean tree at `sprint-346` (`git status --short` empty before and
after). Three independent processes, each with its own master seed, launched together:

```
simbin <master-seed> -swarm -duration=30m -runs=0 -bias -coverage-report -workers=3
```

| Instance | Master seed | Workers | Outcome |
|---|---|---|---|
| inst1 | 20260818001 | 3 | **died at ~19 min** (process panic, exit 2) |
| inst2 | 20260818002 | 3 | completed 30m02s, exit 1 |
| inst3 | 20260818003 | 3 | completed 30m04s, exit 1 |

`-runs=0` is mandatory alongside `-duration`: `cmd/sim/main.go:560` drops the count cap
only when `runs <= 0` is passed explicitly, so a wall-clock swarm is otherwise silently
truncated at the default 200 runs.

Nine of ten cores were committed (3 instances x 3 workers). Exit codes were read from
**inside** each log, not from the launching wrapper.

## Detection envelope

Establishing what the harness is capable of failing, before reading anything into a
clean result:

- `18fe98dd` gave `SimDisk` a durable shadow (`durableData` / `durableLen`), and
  `SimStore.Crash()` is now an alias for `CrashHost()` (`internal/sim/simstore.go`).
  All 90 `.Crash()` call sites across the scenarios therefore discard unsynced data.
- Before that fix (rmp #2535), the disk model retained every unsynced byte across a
  crash, so every "commit acked therefore bytes recovered" oracle held regardless of
  what the engine did with `fsync`.

This run is consequently the first large-scale swarm in which the durability oracles
can actually fail. A clean durability result here carries evidence that the same result
did not carry a week ago.

## Results

| Metric | Value |
|---|---|
| Runs completed (inst2 + inst3) | 7813 |
| Passes | 7700 |
| Failures | 113 |
| Process-killing panics | 1 |
| Coverage buckets exercised | 43 of 43 (0 unexplored) |
| Scenarios exercised | all 35 in the swarm universe |

One caveat on that coverage figure: "0 unexplored" is relative to the **swarm universe of
35 scenarios**, not the 36-scenario catalogue. `swarmBiasUniverse` (`cmd/sim/main.go:594`)
deliberately excludes the soak-scale `long-running` scenario, whose millions-of-ticks
budget would stall an interactive swarm; it stays runnable via `-scenario=long-running`.
The exclusion is legitimate, but it means the bucket cannot appear as unexplored — so a
clean coverage report does not by itself attest that the whole catalogue ran. `long-running`
was **not** exercised by this run.

Non-vacuity gate: **PASS** — the exercise was informative (run count, coverage breadth,
and all three exit codes present in-log).

Failure accounting is exact: 113 declared failures = 113 `FAIL` lines = 72
`io-roundtrip-fault` + 41 `schema-mutation`. **Zero unexplained failures.**

## Verdict

**No engine correctness violation was observed in 7813 runs.** Every one of the 113
failures, and the panic, is a defect in the DST harness itself rather than in GoGraph.
No ACID, isolation, durability, recovery, or Cypher-semantics deviation was reported by
any oracle.

The three defects below are nonetheless consequential, because each one degrades the
harness's ability to find real defects.

---

## F1 — `production-profile` panics and kills the whole process

**Severity: high (destroys an entire swarm instance).**

```
panic: runtime error: index out of range [0] with length 0
  internal/sim/production_profile.go:290
```

`expectedCtr` is a slice allocated once at `production_profile.go:194` with length
`size.counters` (always 2). The loop at line 289 therefore always iterates `k = 0, 1`
and indexes `res.ContendedFinal[k]` at line 290.

But `ContendedFinal` is populated only under two *different* predicates
(`internal/sim/concurrent.go:344-352`):

- `res.TxIssued > 0`, and
- `haveContended`, which is true only if the random role draw assigned at least one
  `roleTxContended` connection (`concurrent.go:239-245`).

Meanwhile `ContendedCounters` is defaulted to 2 **unconditionally** (`concurrent.go:255`),
so `ContendedAcked` and `expectedCtr` are always length 2 while `finals` can be length 0.

With 24 connections at `TxContendedWeight: 0.25`, P(no contended connection) = 0.75^24
~= 0.10% per cycle, and 2 cycles per run gives **~0.2% of `production-profile` runs**.

**Reproduced deterministically:** `simbin -scenario=production-profile 17477768168859964485`
panics at the same line, exit 2, on every attempt.

Consequence: the panic is not contained to the run. It killed instance 1 outright at
~19 minutes, discarding its swarm summary, its coverage report, and roughly a third of
the intended exercise.

Suggested fix: size `ContendedFinal` consistently with `ContendedAcked` (or bound the
loop by `len(res.ContendedFinal)`), so the two slices cannot disagree.

---

## F2 — the io allocation oracle measures the process, not the code under test

**Severity: high (72 of 113 failures; pollutes every concurrent swarm).**

```
[ORACLE_DEVIATION] <io-mutation-alloc>: the mutation sweep allocated 36925424 bytes
over 312100 bytes of input (ratio 118.3), above the bound of 64x
```

The measurement is at `internal/sim/graph_io_surface.go:982-1004`:

```go
runtime.ReadMemStats(&m0)
...
runtime.ReadMemStats(&m1)
return out, m1.TotalAlloc - m0.TotalAlloc, inputBytes, nil
```

`runtime.MemStats.TotalAlloc` is a **process-global** cumulative counter. Its delta
attributes to the mutation sweep every byte allocated by *every other goroutine* during
the window. The swarm runs three scenarios concurrently in one process, so the other two
workers' allocations are billed to this oracle.

**Demonstrated by varying only the worker count**, identical master seed and identical
per-run seeds:

| Workers | Passes | Failures |
|---|---|---|
| 1 | 120 / 120 | 0 |
| 3 | 4 / 120 | 116 |

The scenario is declared `[deterministic]`, yet its outcome depends on concurrent
execution. The failing seed also passes 12/12 when run standalone, and the scenario uses
`SimDisk` only (no real filesystem), so cross-process interference is excluded.

A second site has the same pattern: `graphIOMeasure` at `graph_io_surface.go:1917-1922`.

Consequence beyond the false failures: the swarm's failure signal is polluted. A genuine
engine defect surfacing once would sit among ~72 false positives, and the swarm's
reproduce line does not reproduce the failure, so triage leads nowhere.

Suggested fix: attribute allocations to the goroutine under measurement, or measure this
property in a dedicated single-threaded arm rather than inside the concurrent swarm.

---

## F3 — a non-vacuity gate is reported as a correctness verdict

**Severity: medium (41 of 113 failures).**

```
[ORACLE_DEVIATION] op="merge non-vacuity": the node-only outer-relationship MERGE
never took its ON CREATE branch, so the misdirection-prone write never ran down the
create path (rmp #2515)
```

`checkMergeSurfaceNonVacuity` (`internal/sim/merge_surface.go:1675`) is documented in its
own godoc as *"the terminal assert-something-was-seen gate"* — a non-vacuity gate. It
nevertheless emits `Violation{Kind: ViolationOracleDeviation, ...}` at line 1678, which
fails the run and exits 1.

This contradicts the project's own three-gate pattern (settled by rmp #2472): the verdict
is unconditional, the non-vacuity gate is separate, and the witness is `Logf` only —
*an uninformative run must never read as a faulty one*.

Measured rate: **51 failures in 400 runs (12.75%)** at `workers=1`, so this is
seed-dependent, not concurrency-dependent — a different defect from F2. All sampled
failures fire the same single clause.

This is workload reachability, not an engine property: 87% of seeds do reach the branch,
and the dedicated `TestSchemaMutation_Scenario_Passes` is green, so the engine plainly
can and does take the `ON CREATE` path. For roughly one seed in eight the random workload
simply never drives it there.

Suggested fix: demote the clause to a witness, or make the workload construct the
precondition rather than hoping the draw supplies it.

---

## Resource stability

RSS sampled every 60 s (`ps`), per instance:

| Instance | First | Max | Last |
|---|---|---|---|
| 20260818001 | 1202 MB | 1207 MB | 1207 MB (died early) |
| 20260818002 | 517 MB | 1102 MB | 1102 MB |
| 20260818003 | 948 MB | 1341 MB | 1341 MB |

No unbounded growth was observed, and no OOM, deadlock, or goroutine-explosion symptom
appeared. The modest rise is consistent with the heterogeneous scenario mix (which
includes `long-running`, `mem-pressure`, and `overload`) and with Go not returning heap
to the OS. This instrument is too coarse to certify the absence of a leak, and no such
claim is made here.

## What this exercise does and does not establish

Establishes:

- No engine correctness violation across 7813 runs spanning every scenario in the
  catalogue, with `fsync` load-bearing in the disk model for the first time.
- Three reproducible defects in the DST harness, each with a measured rate and a
  minimal reproduction.

Does not establish:

- That the module is free of defects. 7813 runs at 30 minutes is a sample, not a proof.
- Anything about the ~1/3 of the exercise lost when F1 killed instance 1.
- Anything about the soak-scale `long-running` scenario, which the swarm universe excludes.
- Any claim about leaks, per the caveat above.

The most important operational consequence is that **F1 and F2 together degrade the DST
as an instrument**: F1 discards whole instances, and F2 injects a large, systematic false-
positive population into precisely the concurrent configuration the swarm is meant to be
run in. Both should be fixed before the next swarm is used as evidence.

## Re-run after the fixes

All three findings were fixed (#2552 `69d03662`, #2553 `fb30434e`, #2554 `0b9eb1e2`) and
the exercise was repeated with the SAME three master seeds, the same 30-minute budget and
the same 3x3 worker layout, against the fixed binary.

| | Before | After |
|---|---|---|
| Instances completing 30 minutes | 2 of 3 | **3 of 3** |
| Total runs | 7813 | **10953** |
| Passes | 7700 | **10953** |
| Failures | 113 | **0** |
| Process-killing panics | 1 | **0** |
| Exit codes | 2, 1, 1 | **0, 0, 0** |
| Scenarios exercised | 35 of 35 | 35 of 35 |
| Non-vacuity gate | PASS | PASS |

The 40% rise in run count is itself part of the result: instance 1 previously died at ~19
minutes of its 30, so roughly a third of that instance's work never ran.

The coverage dimensions drop from 43 buckets to 36, which is not lost coverage. `op-kind`
and `violation` are failure-derived dimensions that only materialise once something fails,
and `outcome` falls from two buckets to one. Their absence IS the zero-failure result;
scenario coverage is unchanged at 35 of 35.

Targeted replays, run separately because a swarm comparison is not exact (with `-bias`,
scenario selection depends on a round-robin tie-break driven by call order across three
concurrent workers, so run counts are close but not identical):

* `production-profile` seed 17477768168859964485 — exit 0, no panic (was a deterministic
  process kill).
* `io-roundtrip-fault` — 120 of 120 at workers 1, 3 AND 8, seeds held fixed (was 4 of 120
  at workers=3). Note a SERIAL replay of these seeds would have passed on the unfixed code
  too, so it would have proved nothing; they must be replayed concurrently.
* `schema-mutation` — 400 of 400 on master 20260818002 and 400 of 400 on master 25541818
  (were 51 and 54 failures respectively). The second master is what makes the zero
  falsifiable rather than a sweep that could not fail.

`make ci` green on the final tree, exit read from inside the log: 124 packages, 0 FAIL,
0 races, `cover_gate: OK (aggregate 86.9%)`, lint 0 issues.

### What the clean re-run does and does not mean

It establishes that the harness has stopped manufacturing failures, and therefore that its
failure signal is now worth acting on: previously 72 false positives would have buried a
genuine engine defect, and a panic could erase a third of the evidence before it was read.

It does NOT establish that the engine is defect-free. 10953 runs is a larger sample than
before, and it is still a sample.

One residual is deliberately left open as **#2555**: #2553 moved the allocation BOUND to a
serialised arm, but that arm's own sensitivity assertion still differences two
process-global `TotalAlloc` counters, and it was observed to flake once by 360 bytes out of
31.3 MB. It is the same root cause one layer up, and it is a test-stability defect rather
than an engine one.

## Reproductions

```
# F1 — panics deterministically, exit 2
go run ./cmd/sim -scenario=production-profile 17477768168859964485

# F2 — passes at workers=1, fails at workers=3, identical seeds
go run ./cmd/sim 20260818002 -swarm -scenario=io-roundtrip-fault -runs=120 -workers=1
go run ./cmd/sim 20260818002 -swarm -scenario=io-roundtrip-fault -runs=120 -workers=3

# F3 — seed-dependent, 51/400 at workers=1
go run ./cmd/sim -scenario=schema-mutation 2630239485482423300
go run ./cmd/sim 20260818002 -swarm -scenario=schema-mutation -runs=400 -workers=1
```
