# DST Determinism Audit — 2026-08-28

**Question.** Is the DST exercising GoGraph deterministically — especially since MVCC
became the module's sole concurrency-control mechanism — and if not, why?

**Scope.** `internal/sim` + `cmd/sim` at HEAD `53c91512` (branch
`feature/352-deep-profiling-and-optimization`), the engine surfaces the deterministic
modes drive (`cypher`, `graph/lpg`, `graph/mvcc`, `store`). The in-flight sprint and its
tasks were deliberately ignored, as instructed. Every claim below is anchored to a
file:line read during the audit or to a measurement run for the audit; nothing is
asserted from memory.

---

## Verdict

**The deterministic modes are genuinely deterministic — verified empirically across
every axis this audit varied — but the determinism is a property of the *driver*, not
of the *engine under test*, and MVCC is precisely where that distinction bites.**

Three structural facts explain why the DST "may not be acting deterministically":

1. **Since MVCC was armed by default (rmp #2288), the engine is never single-threaded
   again — even under the single-goroutine simulator.** Every `lpg.Graph` owns a
   demand-started background vacuum goroutine whose schedule the seed does not reach.
   The harness has already *measured* seed-identical runs observing different
   vacuum-visible state (3 vs 2 label-advertised corpses,
   `internal/sim/fluent_query_test.go:520-529`) and responded by **excluding those
   observables from its determinism gates** (rmp #2587/#2596) rather than controlling
   the source. "Deterministic" therefore means *deterministic modulo vacuum-visible
   state*.

2. **The concurrency MVCC exists to guarantee is exercised only non-deterministically.**
   The deterministic MVCC modes interleave logical sessions at *statement* granularity
   on one goroutine (`internal/sim/mvcc_sessions.go:17-30`); they can never explore a
   race *inside* a statement — the claim-then-cross-check windows, shard-lock
   orderings, and endpoint stamping that make up the MVCC implementation. Those are
   reached only by the real-goroutine modes (`ModeConcurrent`/`ModeLiveness`/Bolt
   fleet scenarios), which the package honestly documents as **not bit-reproducible**
   (`internal/sim/scenario.go:18-24,64`). A failure there has no seed-replay.

3. **Process-global engine state sits outside the seed.** This is the one mechanism
   that has *actually produced* non-reproducible DST failures in this project's
   history, twice: the `GOMAXPROCS` clamp (rmp #2613 — all 37 failing swarm seeds of
   2026-08-25 replayed clean standalone) and the `cypher/exec` synthetic-key counter
   (`globalNodeCounter`, acknowledged at `internal/sim/merge_surface.go:249-262`:
   "the scenario's seed does not reach it", ~0.4 % of process histories flip a
   fixture's shard placement). The class — not just the two instances — is the
   standing threat to seed-reproducibility.

No new engine defect and no current determinism breach was found. The findings are
structural: where determinism is narrower than its name, and where the exercise of
MVCC is real but unreproducible.

---

## How the DST defines determinism (the hybrid model)

The package is explicit that only part of the DST is bit-reproducible:

- `ExecMode.Reproducible()` returns true **only** for `ModeDeterministic`
  (`internal/sim/scenario.go:64`). Trace recording, scripted replay, and shrinking
  apply to that mode alone.
- Catalogue composition at HEAD: **43 deterministic, 10 concurrent, 1 liveness,
  1 bulk-vs-online** (55 scenarios, `cmd/sim -list-scenarios`).
- The deterministic substrate is sound by construction: one PCG stream per run
  (`internal/sim/seed.go`), sub-seeds isolating the checker, disk-fault, and crash
  streams from the workload stream (`internal/sim/sim.go:36-39,237-255`), a virtual
  clock with no `time.Now` (`internal/sim/clock.go`), draw-consumption kept stable on
  branches not taken (e.g. `internal/sim/mvcc_sessions.go:875-905`), and map-iteration
  order quarantined behind sorted accessors (`internal/sim/oracle.go:650-657`).

This is a legitimate design (a single-node VOPR adaptation), but it must be read
precisely: **"the DST is deterministic" is true of 43/55 scenarios' drivers, and of
none of the modes that put real parallelism on the MVCC machinery.**

## Empirical verification

All runs on this host under pre-existing load (a foreign `a352.test` benchmark at
~136 % CPU; loadavg 3.8–5.3 throughout — noted per the measurement-discipline rule;
bit-equality claims are load-independent, and holding under load *strengthens* them).
Outputs compared after stripping logger wall-clock timestamps; hashes are SHA-256 of
the full stdout+stderr or of the printed fingerprint lines.

| Probe | Config | Axes varied | Result |
|---|---|---|---|
| E1a | `cmd/sim 987654321 --ticks=20000` | 2 fresh processes | identical (only the `WARN` logger timestamp differs) |
| E1b | same + `--verbose` (20 000-op stream) | 2 processes; `GOMAXPROCS=1` | 3× identical hash |
| E1c | `424242 --ticks=30000 --crashes --checkpoint --verbose` (durable path, crash+recovery+checkpoints) | 2 processes; `GOMAXPROCS=1` | 3× identical hash |
| E1d | scenarios `fluent-query`, `typed-schema`, `label-index-scoped`, `count-store`, fixed seed 777 | 2 processes each; `GOMAXPROCS=1` | identical per scenario |
| E2 | `RunMVCCSessions` (5 000 ticks, 8 sessions, 25 conflicts) + `RunMVCCContention` (6 sessions × 3 counters, 1 198 typed conflicts), full per-step trace FNV-64 + rendered result | 2 fresh processes; `GOMAXPROCS=1`; **polluted process** (a prior full run advancing `globalNodeCounter` first); **under 10 CPU spinners**; **`-race` build** | identical fingerprint in all six runs |
| E2-crash | same sessions config with `Crash{Prob:1/200}` armed | 2 processes; under load | identical fingerprint |

Protocol notes: the E2 probe was a temporary test file (deleted after the audit)
hashing the `OnStep` trace and printing `%+v` of the result; the CPU load came from a
self-terminating generator named `claude-dst-audit-cpuload` (per the process-naming
rule). One false alarm during E1d — divergent hashes — was my own protocol error:
`cmd/sim` **draws a random seed when none is given**, so unseeded scenario runs are
never comparable. With explicit seeds everything converged.

**Conclusion of the empirical phase:** at HEAD, the deterministic modes are
bit-reproducible across processes, GOMAXPROCS, process history, host load, and race
instrumentation, at configurations larger than the shipped gates (which run 600 ticks
in-process only). The nondeterminism risks below are structural, not currently
manifest on these probes.

---

## Findings

### F1 — The MVCC vacuum makes the engine scheduler-dependent under a deterministic driver (the central answer)

- MVCC is armed unconditionally at graph construction (`graph/lpg/lpg.go:1744-1751`);
  the vacuum goroutine is demand-started by write debt and exits when idle
  (`graph/lpg/mvcc_vacuum.go:416-429,583`). The harness's own metrics oracle documents
  the inventory: *"The engine itself spawns no goroutines; its GRAPH spawns the MVCC
  vacuum"* (`internal/sim/metrics_oracle.go:294-299`) — and must join it before its
  goroutine-baseline snapshot (`:339-346`) because a run once measured 13 → 14.
- Vacuum-visible state is *measurably* nondeterministic on a fixed seed:
  `MaxTombstonedInLabelIndexObserved` came out 3 and 2 on the same seed in the same
  process, because lpg defers a deleted node's label-bitmap removal to the vacuum
  (`internal/sim/fluent_query_test.go:520-529`). rmp #2587/#2596 removed such
  assertions from other scenarios.
- **Consequences.** (a) Any future checker clause that touches vacuum-visible state
  (label-bitmap population, version-chain depth, `VacuumStats`, reclamation debt) is a
  latent seed-unreproducible flake; the protection today is reviewer vigilance, not a
  mechanism. (b) An engine defect whose *trigger* is vacuum timing (a sweep landing
  between two statements) is outside the deterministic modes' reach entirely: the seed
  does not schedule the vacuum, so the DST can neither systematically explore nor
  replay such an interleaving. The E2 `-race`/load runs show the *observed surfaces*
  are currently insensitive — they do not show the class is empty.

### F2 — MVCC's physically-concurrent regime has no deterministic exercise

- The deterministic MVCC modes (`mvcc_sessions.go`, `mvcc_contention.go`) interleave
  at statement granularity on one goroutine — by design and honestly documented
  (`mvcc_sessions.go:17-30`: exercises "overlapping ExplicitTx handles, per-object
  conflict detection, session read-your-own-writes"; `:273-277`: "never parallel").
  Every statement runs to completion before the next begins, so intra-statement race
  windows (the rmp #2444 claim-then-cross-check ordering, dual-endpoint stamping,
  `noteExclusive` adjacency claims) are structurally unreachable from these modes.
- Physical concurrency on MVCC is exercised by `concurrent-tx`/`concurrent-iso`/
  `readtx-isolation`/fleet scenarios over the real Bolt wire with real goroutines —
  with exact quiescence adjudication (lost-update accounting) and during-run oracles
  (monotonic reads, RYOW, atomic-batch multiples) — **but none of it is
  bit-reproducible**, `AMBIGUOUS` outcomes are tolerated by necessity
  (`concurrent_tx.go:20-23`), and a failure yields no seed-replay.
- **Consequence.** The property the user asked about — "MVCC deverá garantir os
  comportamentos concorrentes" — is validated by (i) deterministic *logical*
  interleavings, (ii) non-reproducible physical stress plus `-race`, and (iii) unit
  batteries. What is missing is the middle rung: *controlled, replayable* exploration
  of physical interleavings (loom/shuttle-style yield-point scheduling at MVCC
  decision points). In Go this needs an explicit seam (failpoint hooks at
  claim/cross-check boundaries); no such seam exists today.

### F3 — Process-global engine state the seed does not capture (the proven flake mechanism)

- `cypher/exec/create_node.go:83-111`: `globalNodeCounter` (plus a `sync.Once` seeding
  scan) names every synthetic node key `__cx_<hex>` process-wide. Node→shard placement
  hashes that key, so *engine-internal identity* is a function of process history.
  The harness itself documents the impact (`merge_surface.go:249-262`) and carries a
  workaround (`mergeHandleDecoyAltName`, rmp #2515/#2524) for the ~0.4 % of process
  histories where a fixture's decoy lands on id 0.
- The same class produced the 2026-08-25 incident: 38 swarm failures, **all 37
  seed-replays clean**, root cause a *neighbouring worker's* `runtime.GOMAXPROCS(1)`
  clamp — state the seed cannot capture (fixed for that one variable by
  `internal/sim/gomaxprocs.go`; the scenario API now takes the shared side in
  `Scenario.Run`, `scenario.go:199-201`).
- Beyond those two: the engine keeps ~15 process-global atomic plan/operator counters
  (`degreeRewriteCount`, `parallelCountScanBuildCount`, `sortKeyEvalCount`, … in
  `cypher/*.go`). They are metrics-only, but any DST clause asserting on their deltas
  would be coupled to whatever else runs in the same process (swarm workers, parallel
  package tests).
- **Consequence.** In-process gates (run-twice `DeepEqual`) and this audit's polluted-
  process probe show current *outcomes* are insensitive. But the invariant that
  actually matters — *"a DST failure always reproduces from its seed alone"* — is not
  guaranteed by construction while engine state exists that the seed does not reach;
  it has failed twice historically and was patched per-instance both times.

### F4 — The deterministic MVCC modes are invisible to the swarm

`DefaultRegistry` (`internal/sim/catalogue.go:131-188`) contains no MVCC-sessions or
MVCC-contention scenario; `readtx-isolation` in the catalogue is a **concurrent** mode.
The deterministic MVCC modes run only as Go tests at fixed configs (600 ticks,
6 sessions, seeds 1/7/42 plus pinned regression seeds). The seed-exploration machine
(`cmd/sim -swarm`, the coverage-biased fleet) therefore never searches the MVCC
interleaving space — the audit's 5 000-tick/8-session runs were already larger than
anything the gates execute. The mode that *can* find new MVCC isolation defects
deterministically (it found three in sprint 345) is not hooked to the machinery that
hunts new seeds.

### F5 — The goroutine-parallel operators are effectively outside the deterministic DST

Parallel scan/aggregate engage only above `DefaultParallelScanThreshold = 50 000` live
nodes (`cypher/api.go:1006`), while deterministic workloads hold the graph near
`churnHighWater = 200` (`internal/sim/actor.go:170`) — even the 2 M-tick long-running
scenario is bounded-churn. The only deterministic exercise is the differential family
(`differential.go:70,97`), which lowers the threshold to 1 over a ~9-node trace and —
necessarily — compares canonicalised, order-independent signatures, restricted to
tie-free aggregates because the engine's scan order is not stable across builds and
min/max tie representatives are scan-order-dependent (documented at
`differential.go:83-95`). The morsel-racy result paths at scale are thus covered by
unit tests and non-reproducible modes only.

### F6 — Minor wall-clock hygiene

The engine's default logger stamps `WARN` lines with wall-clock time, so raw
`cmd/sim` output is never byte-identical across runs (E1a). Harmless — the op stream
and verdict are identical — but replay tooling that diffs raw output must strip
timestamps, and it is one more reason a future "compare two runs' output" gate could
flake for a non-reason.

### Verified non-issues

- Checker probes and diffs are order-insensitive (count queries; sorted name/edge
  diffs, `mvcc_sessions.go:469-560`; sorted oracle accessors).
- Temporal functions never compare against wall-clock constants; statement-`now`
  stability is asserted relatively (`cypher_expr_literals.go:238-245`).
- `ParallelGovernor` is per-engine, not process-global (`cypher/api.go:1629`).
- WAL group commit is wall-clock-free; no store background goroutines on the sim's
  durable path (checkpoints are driven synchronously in-loop, `sim.go:612-624`).
- `ViolationVacuousRun` now exists (`checker.go:63`), so a vacuous run no longer has
  to be reported as an engine-shaped `ORACLE_DEVIATION` (the 2026-08-25 #2614 gap).
- The sim package has a structural tripwire against test-mutated package globals
  (`global_state_guard_test.go`, rmp #2597).

---

## Recommendations (ranked)

1. **Give the vacuum a deterministic seam.** Either a synchronous `SweepOnce()`-style
   step the simulator can drive on its own tick cadence (with the background goroutine
   disabled under the sim), or at minimum an exported barrier ("run vacuum to
   quiescence now") the harness calls before any observation of vacuum-visible state.
   This converts F1 from "excluded observables + reviewer vigilance" into controlled,
   seed-scheduled reclamation — and makes vacuum-interleaving defects *findable and
   replayable* instead of flaky.
2. **Register the MVCC deterministic modes in the catalogue** so the swarm explores
   their seed/interleaving space (sessions, contention, and the crash arm), and add a
   **cross-process determinism gate** (subprocess re-run comparing a full-trace hash,
   as this audit's E2 probe did) alongside the in-process `DeepEqual` gates — it is
   the only gate shape that can see process-history and hash-seed sensitivity.
3. **Retire the process-global `globalNodeCounter`** in favour of a per-engine (or
   per-graph) counter, making engine identity a pure function of the op stream. This
   closes the acknowledged 0.4 % fixture-flip class at the root instead of
   per-fixture, and removes the largest remaining "state the seed cannot reach".
4. **Add yield-point hooks at MVCC decision boundaries** (claim, cross-check, publish,
   abort-reclaim) behind a build tag or unexported seam, and a small
   deterministic-schedule explorer over them (loom-style, bounded interleavings of
   2–3 sessions). This is the long-term answer to F2 — replayable *physical*
   interleavings — and is the piece neither the deterministic modes nor `-race` can
   provide today.
5. **Silence or restamp the logger in `cmd/sim`** so raw output is byte-diffable.

## Evidence inventory

- Empirical matrix above (all hashes reproduced twice or more; false-alarm E1d
  documented with cause).
- Code anchors: `internal/sim/scenario.go:18-24,64,199-201` ·
  `internal/sim/seed.go:8-21` · `internal/sim/sim.go:36-39,150-154,612-624` ·
  `internal/sim/mvcc_sessions.go:17-30,273-277,875-905` ·
  `internal/sim/mvcc_contention.go:1-28` · `internal/sim/concurrent_tx.go:1-28` ·
  `internal/sim/concurrent_iso.go:1-26` · `internal/sim/fluent_query_test.go:520-529` ·
  `internal/sim/metrics_oracle.go:294-299,339-346` ·
  `internal/sim/merge_surface.go:249-262` · `internal/sim/actor.go:166-170` ·
  `internal/sim/differential.go:58-99` · `internal/sim/catalogue.go:131-188` ·
  `internal/sim/gomaxprocs.go` · `cypher/exec/create_node.go:83-111,358-394` ·
  `cypher/api.go:999-1006,1629` · `graph/lpg/lpg.go:1739-1751` ·
  `graph/lpg/mvcc_vacuum.go:348-429,583` · `internal/sim/checker.go:39-63`.
- Historical record: `docs/dst-parallel-exercise-2026-08-25.md` (37/37 clean replays,
  rmp #2613) · rmp #2515/#2524 (decoy id-0) · rmp #2587/#2596 (scheduler-outcome
  assertions removed) · rmp #2435-#2444 (MVCC session modes and the defects they
  found).

*Audit performed on 2026-08-28 against HEAD `53c91512`; host under foreign benchmark
load throughout (recorded above). The temporary E2 probe file and the load generator
were removed after the runs; no repository file was left modified by this audit.*
