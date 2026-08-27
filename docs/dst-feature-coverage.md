# DST Feature Coverage

This document records how the Deterministic Simulation Testing harness
(`internal/sim/`) exercises the GoGraph feature surface, and the coverage work
that makes the DST drive **every implemented feature**.

The goal: for every implemented GoGraph feature, the DST has a scenario that
drives it during simulation and validates it against an **independent** oracle
or reference — including, wherever applicable, across crash and recovery.

## Method

The coverage work has run in two passes, and this document carries both.

**The first pass (2026-07-13).** Three domain audits (Cypher language,
graph/search algorithms, storage/durability) enumerated the implemented feature
surface and cross-referenced it against the scenarios the DST actually ran.
Every "implemented but unexercised" feature became a tracked task.

**The second pass (the 2026-08-14 audit, closed across sprints 346–349).** A
second audit re-enumerated the surface against the DST as it then stood, and
found both the gaps the first pass had left and everything the module had grown
since. Its findings were closed one domain per sprint:

| Sprint | Domain the audit named |
|---|---|
| 346 | the Cypher language surface — read, write, DDL and functions |
| 347 | storage, durability and the MVCC substrate |
| 348 | the Bolt wire surface |
| 349 | the graph API, `search/` and the bulk-load surface, and the audit's own closure |

Sprint 345, which closed the same day the audit was taken, is the immediately
preceding cycle rather than one of its four: it made the simulator exercise MVCC
and genuinely concurrent multi-client access, and is recorded in
[MVCC multi-session and concurrency coverage](#mvcc-multi-session-and-concurrency-coverage-sprint-345)
below.

Sections below are tagged with the `rmp` task that produced them, so a claim can
be traced to the change that made it true. Where a task's finding was later
fixed, the section says so and the tense follows the code, not the finding.

Each new check validates the engine against an independent computation — never
the code under test — and every deterministic scenario is bit-reproducible from
its seed.

Three classes of verification vehicle are used:

- **Oracle-computed checks**: an invariant computed independently from the
  shadow model (`GraphOracle`, or a scenario-private model) is compared to the
  engine's result — the strongest form (catches absolute wrongness).
- **Absolute-literal checks**: a self-contained `RETURN <expr>` whose answer is
  a known constant, compared to the engine's canonical rendering.
- **Independent naive references**: for the search algorithms, a from-scratch
  reference (naive BFS/Bellman-Ford/power-iteration/degree-parity/…) computed on
  a shaped fixture with known ground truth.

## Whole-module coverage driven from the simulator

This section records what the DST alone covers: **whole-module statement
coverage with the simulator as the only driver**. It is not the coverage of
`make ci`, which additionally runs every package's own unit tests and is a
different and much larger number.

**Method, so the measurement is repeatable.**

```bash
go test -count=1 -covermode=atomic -coverpkg=./... ./internal/sim/...
```

Run twice: once at `ccdca3e6` — the last sprint-345 commit, which is the
2026-08-14 audit's baseline — in a detached worktree, and once at `21b8364f`,
HEAD with sprint 349 complete.

### The headline, and the caveat that travels with it

**Excluding `internal/*`, over a near-stable denominator (60,246 → 61,405
statements): 50.7% → 65.1%, +14.4 pp.** That is the figure that answers the
sprint's question, because its denominator is the product surface and barely
moved.

Raw, over every package `-coverpkg=./...` admits: **54.5% → 71.4%, +16.9 pp**
(37,611/69,005 → 65,903/92,350 statements). The raw number is the larger one and
the weaker one, for a reason that has to travel with it: `-coverpkg=./...`
includes `internal/sim` **itself**, and the harness grew from **8,619 to 30,726
statements** across sprints 346–349 while sitting at ~84% covered. A substantial
part of the raw gain is therefore the test harness entering its own denominator,
not the product surface being better exercised. Excluding only `internal/sim`
and keeping the other `internal/*` packages gives 50.7% → 65.0%, +14.3 pp —
which shows the difference between the two exclusions is immaterial, and that
`internal/sim` is the whole of the distortion.

**The baseline re-measurement reproduced the inherited 54.5% exactly.** That is
worth stating on its own: it validates both the 2026-08-14 audit's figure and
this method, and it is the reason the delta above can be trusted rather than
merely reported.

### The two acceptance-criteria packages, both demonstrably off zero

| Package | 2026-08-14 baseline | after sprint 349 |
|---|---:|---:|
| `graph/lpg/schema` | **0.0%** | **61.0%** (47/77) |
| `graph/index/stats` | **0.0%** | **56.2%** (127/226) |

**No package is now at genuine zero.** `internal/crashpoint` reports 0/0: it has
**no statements** in a build without the `gograph_crashinject` tag, because
`crashpoint.go` declares only constants and documentation and
`crashpoint_disabled.go` is a single empty function body (`func Breakpoint(string) {}`).
That is by design — the released binary must not link the self-kill path — and
not a coverage gap.

### Five packages were not linked into the sim test binary at all at baseline

They appear in no baseline profile block, which is zero coverage in the
strongest sense: the simulator never referenced them, even transitively.

| Package | after sprint 349 |
|---|---:|
| `graph/generation` | 87.5% |
| `store/bulkimport` | 78.0% |
| `graph/query` | 75.5% |
| `graph/io/dot` | 74.5% |
| `internal/goldens` | 25.3% |

Together they are 660 statements, **0.7% of the new denominator**, so they do
**not** explain the delta. They are a qualitative result — four product packages
and one test-support package went from unreferenced to driven — and should not
be read as a quantitative one.

### Per-package movement

Baseline versus HEAD, sorted by movement.

| Package | 2026-08-14 baseline | after sprint 349 | movement |
|---|---:|---:|---:|
| `graph/index/label` | 25.7% | 88.0% | +62.3 pp |
| `graph/lpg/schema` | 0.0% | 61.0% | +61.0 pp |
| `store/bulk` | 11.1% | 69.3% | +58.2 pp |
| `graph/index/stats` | 0.0% | 56.2% | +56.2 pp |
| `graph/io/jsonl` | 23.9% | 79.7% | +55.8 pp |
| `graph/index/btree` | 14.0% | 59.7% | +45.7 pp |
| `graph/index/hash` | 22.3% | 55.8% | +33.6 pp |
| `cypher/procs` | 47.1% | 80.4% | +33.3 pp |
| `cypher/funcs` | 24.6% | 55.1% | +30.6 pp |
| `bolt/server` | 54.5% | 81.0% | +26.5 pp |
| `graph` | 58.6% | 84.3% | +25.7 pp |
| `graph/index` | 61.3% | 86.8% | +25.5 pp |
| `graph/index/count` | 57.1% | 80.5% | +23.4 pp |
| `search/centrality` | 67.1% | 90.3% | +23.2 pp |
| `cypher/expr` | 28.5% | 51.7% | +23.1 pp |
| `graph/io/graphml` | 49.7% | 72.5% | +22.8 pp |
| `store/snapshot` | 44.9% | 65.1% | +20.2 pp |
| `store/txn` | 43.4% | 63.4% | +19.9 pp |
| `graph/csr` | 49.2% | 68.1% | +18.8 pp |
| `graph/io/csv` | 59.5% | 77.9% | +18.5 pp |
| `cypher` | 51.5% | 68.9% | +17.5 pp |
| `store/wal` | 52.2% | 69.6% | +17.4 pp |
| `store/csrfile` | 62.0% | 78.1% | +16.0 pp |
| `cypher/exec` | 40.9% | 56.5% | +15.6 pp |
| `store/checkpoint` | 73.6% | 86.8% | +13.2 pp |
| `store/recovery` | 53.0% | 66.0% | +13.0 pp |
| `bolt/packstream` | 51.0% | 63.7% | +12.7 pp |
| `search/flow` | 81.4% | 92.8% | +11.4 pp |
| `cypher/ir` | 51.9% | 62.9% | +11.1 pp |
| `bolt/proto` | 67.2% | 78.0% | +10.8 pp |
| `graph/lpg` | 60.3% | 71.1% | +10.8 pp |
| `ds` | 34.5% | 39.7% | +5.2 pp |
| `search` | 82.1% | 87.0% | +4.9 pp |
| `cypher/parser/gen` | 52.5% | 57.4% | +4.9 pp |
| `cypher/ast` | 22.5% | 26.5% | +4.0 pp |
| `graph/adjlist` | 56.4% | 60.3% | +3.9 pp |
| `search/extern` | 80.4% | 84.1% | +3.6 pp |
| `cypher/parser` | 57.7% | 61.0% | +3.4 pp |
| `cypher/sema` | 50.9% | 54.2% | +3.3 pp |
| `internal/sim` | 80.8% | 84.0% | +3.2 pp |
| `store` | 93.9% | 97.0% | +3.0 pp |
| `internal/clock` | 84.4% | 87.0% | +2.6 pp |
| `graph/mvcc` | 69.1% | 71.2% | +2.1 pp |
| `search/community` | 93.1% | 93.9% | +0.8 pp |
| `internal/memlimit` | 50.0% | 50.0% | +0.0 pp |
| `internal/metrics` | 94.1% | 94.1% | +0.0 pp |
| `internal/testlayers` | 29.2% | 29.2% | +0.0 pp |
| `graph/generation` | not linked | 87.5% | newly linked |
| `graph/io/dot` | not linked | 74.5% | newly linked |
| `graph/query` | not linked | 75.5% | newly linked |
| `internal/goldens` | not linked | 25.3% | newly linked |
| `store/bulkimport` | not linked | 78.0% | newly linked |
| `internal/crashpoint` | not linked | 0/0 statements | n/a — see above |

**Module total: 54.5% (37,611/69,005) → 71.4% (65,903/92,350) = +16.9 pp.**

Two rows deserve a note rather than a cheer. `ds` moves only +5.2 pp because
`search/` calls three of its methods and nothing calls the rest — see
[Documented debt](#documented-debt--out-of-scope). `internal/sim` rising 3.2 pp
while more than tripling in size is what a growing harness that stays
well-covered looks like, and is exactly the term the excluding-`internal/*`
figure removes.

### The soak layer, and the red that came with it

```bash
go test -tags=soak -count=1 -covermode=atomic -coverpkg=./... ./internal/sim/...
```

**71.6% of statements**, in 2073.7 s. Reported here with its verdict stated
plainly rather than as a clean run: **the soak run exited 1.**

`TestBoltDecodeSwarm_SoakSeedSweep` failed at seed `0x2487a018`
(`internal/sim/bolt_decode_pressure_soak_test.go:76`): the
`nv-swarm-pressure-density` oracle deviated with `RejectionsDuringHonest == 0`
and per-segment `[0 0 0 0]`, the start barrier satisfied, the fleet having drawn
3 refusals in total, and honest service 24 of 24 correct. **No engine
misbehaviour.** It is the third iteration of the harness-defect family of
rmp #2587 and #2596: the clause adjudicates a temporal coincidence the harness
cannot force, and #2587's residual "nonzero" floor is still scheduler-dependent
— under load the pressure can be entirely spent before honest service begins.
Filed as **rmp #2611** (see
[Harness and gate defects](#harness-and-gate-defects-surfaced-by-this-coverage-work)).

**The 55.2% soak baseline is inherited from the 2026-08-14 audit and was NOT
re-measured**, so no soak delta is claimed here. Only the short-layer baseline
was reproduced.

### Reading the delta

Two cautions, both established elsewhere in this document. First, a
statement-coverage percentage measures what was **executed**, not what was
**adjudicated**: several sections here record clauses that execute an entry
point and cannot fail (see
[Documented debt](#documented-debt--out-of-scope)), and those raise coverage
without raising assurance. Second, a package can fall while its assurance rises,
because a scenario that stops driving a path through the engine and drives it
through an independent model instead is a stronger check and a smaller number.

## Cypher language coverage

### Mutation clauses (`schema-mutation`, `merge-rel` scenarios)

| Feature | Scenario | Independent check |
|---|---|---|
| `REMOVE n.prop`, `REMOVE n:Label` | schema-mutation | oracle read-back: property reads NULL / label dropped, across crash+checkpoint recovery |
| `SET n:Label`, `SET n += $map`, `SET n = $map` | schema-mutation | oracle labels/properties after each op, across recovery |
| multi-label match `(n:A:B)` | schema-mutation | oracle count of dual-labelled nodes |
| `MERGE (a)-[r]->(b) ON CREATE/ON MATCH SET` | merge-rel | idempotent edge count + `r.n` counter round-trips across recovery |

Map-valued parameters are bound by the harness adapter (`toExprValue`) so
`SET n += $map` / `MERGE (n $map)` can be driven.

### Read clauses, expressions, functions (`cypher-surface` scenario)

`CheckCypherSurfaceExtended` (oracle-computed over the Person/KNOWS graph):
`count(DISTINCT)`; 3VL `AND`/`OR`/`XOR`/`NOT`, `IN`, `IS NULL`, `<>`;
`STARTS WITH`/`ENDS WITH`/`CONTAINS`/`=~`; `ORDER BY … SKIP … LIMIT`;
`avg`/`min`/`max`/`sum` and `percentileCont`/`percentileDisc` invariants;
`EXISTS { }` / `COUNT { }` / pattern-comprehension subqueries;
`CALL db.labels`/`relationshipTypes`/`propertyKeys` vs the modelled schema.

`CheckExprLiterals` (absolute-literal battery, ~40 probes): `UNION`/`UNION ALL`;
`CASE`, list comprehension, `reduce`, `all`/`any`/`none`/`single`; the
scalar/list/string/math function surface; list subscript, list slice, map
projection; temporal constructors (`date`, `duration`) and component access.

## Search algorithm coverage

Every `search/` algorithm the DST did not previously exercise (or exercised
only in a degenerate regime) is now cross-checked against an independent naive
reference on shaped, seed-deterministic fixtures, folded into the
`search` / `search-crash` battery (so each is validated post-crash-recovery):

negative weights + negative-cycle detection (Bellman-Ford / Floyd-Warshall /
Johnson); `MinCostMaxFlow` in both its cost regimes; `PushRelabelMaxFlow`;
`Closeness` / `Harmonic` / `Eigenvector` / `Katz` / `PersonalisedPushPageRank`,
each under both its literal options and its shipped `Default…Options`;
serial-vs-parallel `Betweenness` (unweighted and weighted); parallel-edge
k-shortest; `TopologicalSort` DAG success; `Diameter`; triangle counting
(serial == parallel); `WCCParallel` vs serial; undirected Euler
(`HierholzerUndirected` beside the directed form); `BiBFS` / `BiBFSOn`;
`BidirectionalDijkstra` / `BidirectionalDijkstraOn`; direction-optimised BFS on
a hub fixture; the `*Into` / `NewSSSP` buffer-reuse APIs; external-memory
`extern.BFS` / `extern.PageRank`.

Beside the correctness battery, every public **context-accepting** entry point
of `search`, `search/centrality`, `search/community`, `search/flow` and
`search/extern` — 58 of them, enumerated from source rather than from a name
pattern — is driven under `context.Background()` and under a pre-cancelled
context on the same cadence (rmp #2489, below).

### The stateful PageRanker: bit-identity across a reused object, and the aliasing contract (rmp #2495)

`centrality.PageRanker` publishes two promises in its godoc, and until this task
both were carried by that godoc alone.

**What was and was not already reached.** `NewPageRanker`/`Run` was not undriven.
`internal/sim/search_ctx_cancel.go` carries a `PageRanker.Run` row, and it is the
one row in that table whose identity arm compares two genuinely independent
implementations (`Run` against `PageRankCtx`) rather than a delegation, through a
lossless hex-float digest. But every property that makes a PageRanker a
PageRanker is outside what that row can see: it builds a **fresh** ranker per
call, Runs **once**, with the default options, on a fixture of a few tens of
nodes — which is below `pageRankParallelThreshold` (2048), so the reverse-CSR
transpose is never built at all and the arm compares two **serial** runs. The
in-package tests reach further and still stop short:
`TestPageRanker_BitIdenticalToOneShot` does compare bit patterns over three runs
on a 7-node and a 10 000-node graph, but all three runs use **identical**
options, it never touches `GOMAXPROCS` (so which regime the large fixture takes
is a property of the host and is asserted nowhere), and the transpose, if built,
is built on the **first** run. `TestPageRank_ParallelBitIdentical` does clamp
`GOMAXPROCS` and does compare serial against parallel — but only for the
**one-shot** `PageRank`. And nothing anywhere pinned the aliasing contract: the
only mention of it in the whole package was a comment in
`TestPageRanker_ConcurrentIndependent` saying the test "mirrors the documented
contract", above code that neither copies the result nor Runs again.

**A premise in the task was wrong.** The brief asked for the sequence to
interleave the regimes "with varying options". `PageRankOptions` has no
parallelism knob — VERIFIED in source, its only fields are `Damping`,
`MaxIterations` and `Tolerance` — and the regime is decided inside
`pageRankState.run` as `runtime.GOMAXPROCS(0) > 1 && live >=
pageRankParallelThreshold`. A PageRanker is bound to one immutable CSR, so `live`
is fixed for its whole lifetime and options can never move the regime. The only
reachable lever is the process-global `GOMAXPROCS` over a fixture above the
threshold, which is also the only way to make the lazy transpose build land
**mid-sequence**.

**How the regime is established rather than assumed.** Two instruments, because
the task refuses timing. A **derivation**: `live` is recomputed from the
fixture's own edge list using the same definition `newPageRankState` uses,
`GOMAXPROCS` is read back inside the clamped window, and the documented predicate
is re-evaluated — labelled as a derivation, not an observation. And an
**observation**, exact and deterministic: every worker of the parallel engine
starts inside `pprof.Do(e.ctx, ...)`, and `pprof.Do` consults the parent label
set through `Context.Value`, on the context the caller handed to `Run`. A
counting context therefore reads **zero** lookups on the serial path (no engine,
no worker) and one per worker at spawn on the parallel path, with the spawn
provably preceding the first `iterate` return because `iterate` sends on each
worker's unbuffered start channel and that receive happens inside the function
`pprof.Do` wraps. Each worker performs a **second** lookup on the way out and
`pageRankEngine.close` does not join its workers, so the clause is a **band** —
0 for serial, `[workers, 2*workers]` for parallel. MEASURED: 0 for every serial
window, and 4, 8, 9 or 11 for parallel windows at clamps of 4 and 8; the counts
above the worker total are workers of the *previous* window's pool exiting, which
is exactly why an equality would have flaked. Only band membership, never the raw
count, enters the reproducible digest.

**The two claims need different instruments.** Bit-identity is compared on the
**bit pattern** (`math.Float64bits`), not with `==`, so a ±0 divergence or a NaN
cannot read as agreement. The package's existing PageRank oracle compares within
`pagerankEpsilon` = 1e-4, which is right for *its* claim (the
library-versus-reference convergence gap) and would make a bit-identity clause
unfalsifiable — and is the wrong **scale** here anyway: MEASURED on the
catalogue seed's 3 069-node fixture the median rank is 2.03e-4 and the smallest
1.69e-4, so an absolute 1e-4 tolerance is half a typical rank and would accept a
rank wrong by 50%. The
aliasing claim is pinned in both directions: the previous window's returned
**slice** must read the new run's values (MEASURED: every element of 3 069
changed, at all five transitions), a **copy** of it must still hash to what it
hashed to at copy time (the control, checked against a recorded hash rather than
against itself), and both are gated on the two consecutive results actually
**differing** — which is why the plan gives every window its own damping.

**The whole-sequence shape is "at most two", and that was a finding.** Which
backing array a Run returns is `start XOR (iterations mod 2)`, because `run`
swaps `cur` and `next` once per iteration and returns `cur`. MEASURED, seed
`0x8d10afeecdf8dcf` gave all six windows an **even** iteration count and
therefore returned the same array six times. An "exactly two backing arrays"
assertion would have been a parity coincidence dressed up as an invariant, and it
failed on the first 32-seed sweep; the asserted shape is at most two with at
least one repeat.

**What is not claimed.** The transpose **cache** is evidence, not a clause: it is
observable only through allocation, and the allocated-byte counter is
process-global, so an upper bound on a later window would flake in a swarm. Only
the lower bound on the first parallel window is asserted — noise can only add —
against the floor the structure-only transpose provably needs (`revVerts`
(n+1)·8 + `revEdges` |E|·8 + the scatter cursor n·8). MEASURED at the catalogue
seed: 0 and 16 bytes for the serial windows, 117 456 for the first parallel one
against a 104 328-byte floor, and 11 144 then 2 288 for the two parallel windows
after it — a 10.5x drop reported as a number rather than dressed up as a
tripwire. The allocated-byte counter is read through `runtime.ReadMemStats`
rather than the cheaper non-stop-the-world `runtime/metrics` counter for a
measured reason: that counter is fed from per-P caches flushed at a GC or an
mcache refill, and MEASURED through it every serial window read exactly 0 bytes
while two of three parallel windows did too. There is also **no crash/recovery
arm** in the scenario: both claims are pure functions of an immutable CSR
snapshot, so repeating them after a recovery would cost time and detect nothing.
The per-tick half of this coverage, which does run after every recovery on the
recovered graph's own CSR, is `pagerankerStatefulViolations` in
`internal/sim/search_pagerank.go` — two Runs on the small per-tick fixture,
bit-identity against the one-shot plus the aliasing pin, in the serial regime.

**A finding recorded rather than fixed.** `centrality.PageRank`'s godoc claims
the parallel pull path is "bit-for-bit identical to the serial path regardless of
GOMAXPROCS or worker scheduling", and that "the per-worker partial L1 deltas are
reduced in fixed worker-id order, so the returned delta is likewise deterministic
across worker counts". The second sentence is true for a **given** worker count
and not across worker counts: reducing per-range partial sums is not the same
float operation as one sequential sum. MEASURED over one pair of consecutive
iterate vectors from a 2 400-node probe graph of the same family, a sequential L1
reduction gave `0x3fb51ef43754e4b2` while equal-range partitioned reductions gave
five **different** values for 2, 3, 4, 8 and 10 ranges — a spread of 73 ULP,
about 1e-14 relative. The result stays bit-identical only because that difference
has never straddled the convergence threshold, which would change the iteration
count and with it the answer. How unlikely that is was measured, not assumed: 40
seeds x 4 dampings x 9 tolerances x 12 worker counts — 17 280
serial-versus-parallel comparisons — found **zero** bit divergences and zero
iteration-count differences, and a 400-seed sweep of the scenario itself found
zero across its own 1 600, as the arithmetic predicts (the stopping delta is
spread over a log-width of about `ln(1/d)`, 0.11 to 0.60 across the damping band,
so the chance of landing in a 1e-14 relative window is on the order of 1e-13 per
run). The `cross-regime`
clause is therefore **not** offered as a detector of that coincidence: what it
detects is a structural change — a pull formulation that stopped summing each
vertex's in-edges in the reverse-CSR's increasing-source order, a partition that
stopped being contiguous, a reduction reordered — because those diverge
systematically. The godoc's determinism sentence is over-stated as written and is
left for the user to decide on.

**`GOMAXPROCS` is process-global, and that is handled explicitly.** The scenario
holds a package-level mutex across its whole clamped phase, so two instances in
one swarm cannot decide each other's regimes nor corrupt the value permanently
through interleaved save/restore, and `prWithClamp` reads the value back on both
sides of every window: a **foreign** clamp (`runCPUStarvation` is the only other
one in this package) is reported as a harness error rather than as a false
violation. The pre-existing hazard that `runCPUStarvation` does not participate
in that mutex is reported, not changed — making an existing scenario serialise
against a new one is a behaviour change for the user to sanction.

### Min-cost flow's negative-cost regime, the hoisted-reverse Dijkstra, and the shipped default option regimes (rmp #2497)

Three residues the 2026-08-14 audit left in `search/`. Two are regimes: the DST
called the entry point, but only ever in one of the configurations it ships with,
so a whole branch of production code went undriven while the checker looked
green. The third is an entry point the DST did not call at all.

**A. `MinCostMaxFlow` had only ever seen strictly positive costs.** The cost
flavour is now chosen by fixture **INDEX**, never by a seed draw
(`flowMCMFNegFrom = 2`, `internal/sim/search_flow.go:65`): of the four
min-cost fixtures a tick builds, 0–1 stay all-positive and 2–3 force a negative
arc, so both the zero-potential fast path (`hasNegativeCost` false, the
Bellman-Ford bootstrap skipped entirely) and the bootstrap itself run on **every**
tick rather than when a tick draws luckily. Forcing is not cosmetic and the
number is measured: under an unforced symmetric draw, **80 of 20,000** fixtures
carried no negative arc at all, so the bootstrap would not have run. Costs come
from `[-flowMaxCost, +flowMaxCost]` = `[-10, +10]` through exactly one `IntN`
draw per cost (`flowDrawCost`, `:901`), so the per-fixture draw count is
unchanged and the all-positive fixtures stay bit-identical to what they were
before the flavour existed.

**The check was also swallowing errors, which is a checker defect and not merely
a coverage gap.** It called the non-context `flow.MinCostMaxFlow`, whose whole
body is `out, c, _ := MinCostMaxFlowCtx(...)` (`search/flow/min_cost.go:68-72`) —
the error is discarded. So the checker could not see `flow.ErrNegativeCycle`,
`flow.ErrCapacityOverflow`, or the internal `rc < 0` invariant violation at
`search/flow/min_cost.go:148-154`, which returns a **partial** flow beside its
error. It now drives `flow.MinCostMaxFlowCtx` and asserts `err == nil` **by
name**, with a parallel non-ctx call pinning the wrapper's own contract
(`internal/sim/search_flow.go:573`, `:585`).

**Two guards make the oracle's precondition structural rather than lucky.** The
SPFA reference is a sound min-cost oracle only if the zero flow is min-cost of
value 0, which (Ahuja–Magnanti–Orlin Thm 9.1) requires the `cap >= 1` arc set to
hold no negative-cost cycle. The generator's low-index→high-index normalisation
yields no directed cycle of **any** sign, so the condition holds structurally.
That normalisation had been commented as a convenience; it is in fact both a
**soundness** and a **liveness** precondition — SPFA has no negative-cycle
detector, so on a negative cycle the reference did not fail, it **hung**. It now
carries a relaxation budget that converts the hang into a named violation
(`errFlowRefRelaxBudget`, `:491`), and `flowFirstNonDAGArc` (`:638`) asserts the
DAG invariant before the reference is ever called.

Two **planted** negative-cycle fixtures assert the `ErrNegativeCycle` refusal
contract. They run on a separate path that never reaches the reference, are
hand-built constants that consume **zero** draws (so they perturb neither the
per-tick stream nor each other), and each asserts a non-zero plain-Dinic max
flow on the same capacities — which is what makes the `(0, 0)` assertions
evidential rather than vacuous (`flowCheckNegCycleFixture`, `:735`). Finally, a
negative-cycle **optimality certificate** is run on the reference's own final
residual (`:621`): it catches a misconception **shared** by production and
reference, which a flow-value comparison structurally cannot.

**B. `BidirectionalDijkstraOn` had no correctness coverage**, although
`BiBFSOn` had already established the pattern. The reverse CSR is now built once
per tick in `ssspViolations` (`internal/sim/search_sssp.go:42`) and threaded into
the point-to-point checks — which is the hoist the `On` variant exists for — and
the variant is judged **twice, by two deliberately separate assertions**: against
the independent naive reference exactly as its two siblings are, and for **cost
parity** against `BidirectionalDijkstra` on the same pair (`bidijkstraOnParity`,
`:286`). Keeping them apart is the point: agreeing with the reference is a
property each entry point can hold alone, whereas parity is the assertion that
pins the caller-supplied reverse CSR to the internally built one, which is the
only thing that differs between the two calls. Parity is asserted as **exact**
equality, not within a tolerance: every edge weight is an integer in `[1, 16]`
and the fixtures are small, so a correct implementation cannot round.

**C. The shipped `Default…Options` regimes were entirely undriven.**
`Eigenvector`, `Katz` and `PersonalisedPushPageRank` ran only under the
hand-written literal options this file also drives, leaving the regime the
library actually ships — including Katz's auto-alpha branch and the
100-iteration eigenvector cap — unexercised. Each now runs in **both** regimes
against the **same** independent reference, with the op labels kept distinct so a
divergence names which regime diverged.

The tolerances were **measured before being chosen**, and each constant records
the observation, the headroom, and the size of defect it still catches
(`internal/sim/search_centrality_measures.go:61-122`):

| Measure | Worst observed \|diff\| | Sweep | Tolerance | Headroom |
|---|---|---|---|---|
| `Eigenvector` under `DefaultEigenvectorOptions` | 5.75e-06 | 25,000 runs (5,000 ticks × the 5 applicable fixtures) | `1e-4` | 17.4× |
| `Katz` under `DefaultKatzOptions` | 1.00e-07 | 40,000 runs (5,000 ticks × all 8 fixtures) | `1e-6` | 9.96× |
| `PersonalisedPushPageRank` under `DefaultPPRPushOptions` | 3.06e-06 | 5,000 runs | `1e-4` | 32.7× on the observation |

The two spectral sweeps are **exhaustive**, not samples, and the reason is
structural: eigenvector and Katz read only the adjacency (arc weights are
ignored), and `centralityFixtures` yields exactly **23** distinct shapes — 7
fixed plus the 16 `(a, b)` size combinations of `centralityRandomBridged`, whose
two clique sizes are each drawn from `[2, 5]`. Twenty of the 23 are undirected
and non-empty, which is exactly the subset `centralitySpectralApplicable`
admits, and the sweep covered every one.

The PPR sweep is a genuine **sample** — its fixture varies in order and in its
seed-chosen extra arcs — so `pprDefaultEps` is placed above the **structural**
residue bound rather than above the observation: local push leaves at most
`epsilon * deg(v)` of residue un-pushed per node, hence at most `epsilon * |E|`
overall, and `|E|` never exceeded 17 across the sweep, giving
`1e-6 * 17 = 1.7e-05`. `1e-4` sits 5.9× above that worst case the algorithm is
permitted to leave behind. Katz's auto-alpha, `0.85 / (1 + maxInDegree)`, is
re-derived independently from the fixture, so the documented formula is pinned
rather than trusted.

Falsifiability was observed rather than asserted: ten controlled reverts were
each driven and each turned a gate red, three of them proving the tolerances sit
above a genuinely non-zero divergence (rmp #2497).

**What this task does NOT claim.** Two clauses have **no falsifying witness**,
and are kept because dropping them would drop the assertion, not because they
have been shown to fire: the ctx-versus-non-ctx agreement clause and the
`(flow, cost)`-versus-reference clause, because either would need a real engine
defect to fire. The `rc < 0` guard is proved reachable by construction — the
negative-cost flavour puts reduced costs on the boundary of it, with 4,743
zero-cost arcs measured across the 10,000-fixture sweep the tests run — but it
has never been **observed** firing. And the `inf → 0` potential substitution at
`search/flow/min_cost.go:275-279` remains **dead code on this generator**: the
spine gives every node an in-path of capacity ≥ 1, so every node is reachable
from the source and no potential is ever left at `inf`. Reaching those lines
needs an isolated-source fixture, which was deliberately left outside this
task's scope and is recorded under
[Documented debt](#documented-debt--out-of-scope).

### Every public context-accepting search entry point, under cancellation (rmp #2489)

`search`, `search/centrality`, `search/community`, `search/flow` and
`search/extern` expose **58** entry points whose first parameter is a
`context.Context`. Before this task the DST called exactly **one** of them —
`search.FloydWarshallCtx`, from `negWeightCycleCheck`, and only because the
non-context form cannot surface `search.ErrNegativeCycle` in its signature.
Nothing at the DST layer asserted that a cancelled context is honoured, that the
honouring is visible in the result, or that the cancellable form computes what
the plain form computes. The family lives in `internal/sim/search_ctx_cancel.go`
and runs on `CheckSearch`'s cadence, so it is re-driven after every crash and
recovery.

**The inventory is enumerated from the parameter type, not from a name.** The
task was filed against "about 35 …Ctx entry points"; the truth is 58, and a
`...Ctx` suffix search finds only 53.
`TestSearchCtxCancel_TableCoversEveryEntryPoint`
(`internal/sim/search_ctx_cancel_test.go:167`) parses the five packages with
`go/ast` and requires the set of exported functions and exported-receiver
methods taking a leading `context.Context` to **equal** the driven table, in both
directions. The five a name filter loses are not marginal:
`search.KShortestPathsLooplessCtxWithOpts` (THE implementation both other
k-shortest forms delegate to, and the only one that can return a partial result
beside its error), `centrality.PageRanker.Run` (a **method**, and one of only two
genuinely independent implementations in the whole surface), and the three
zero-allocation primitives `search.AStarInto`, `search.BellmanFordInto` and
`search.DijkstraInto`, each of which takes a context and has no non-`Ctx`
sibling. The test additionally names two rows explicitly, so a **narrowing of
the enumerator itself** fails rather than silently shrinking the table it is
compared against.

**Three regimes, all in the short layer.** Every row runs under
`context.Background()` and under a context cancelled **before** the call, with
five clauses (`bg-err`, `twin-err`, `identity`, `cancel-err`, `cancel-val`); a
third arm, `ctxCancelPrecedenceViolations`, drives **7** rows whose input makes
the entry point return a terminal sentinel, and asserts both that the sentinel
really is reached under a live context (`prec-setup`) and that a pre-cancelled
context outranks it (`prec-order`). Two mechanics are load-bearing: the clauses
use `errors.Is`, never `==`, because four centrality entry points wrap the
context error while the other 54 return it raw; and they test **error identity**,
never non-nilness, because `ErrCycle`, `ErrNegativeCycle`, `ErrNoPath` and
`ErrInvalidInput` are all non-nil and none of them is a cancellation.
`cancel-val` is the non-vacuity gate that makes `cancel-err` mean something, and
it is why every fixture in `ctxFixtures` is constructed to have a **non-zero**
answer.

**What the identity arm proves, and where it is vacuous — stated rather than
counted as coverage.** The task was written on the premise that the `Ctx` form
might be a divergent code path; reading the five packages inverts it. Of the 58
rows, **54** have a counterpart to compare against at all (the three `*Into`
primitives and `KShortestPathsLooplessCtxWithOpts` have none), and for **52** of
those 54 it is the NON-`Ctx` form that is the wrapper — its whole body is a
tail-call to the context-aware form with `context.Background()` — so a bitwise
difference is impossible by construction and the arm is a structural tripwire,
not a correctness oracle. The file labels it as such, and
`TestSearchCtxCancel_TwinIsAStructuralDelegation` (`:331`) checks the label in
both directions against the source, so neither a twin that stops delegating nor
one that starts can leave it stale. **Two** rows carry a real identity arm:
`centrality.PageRanker.Run` against `centrality.PageRankCtx` — two genuinely
separate implementations, duplicated on purpose because extracting `PageRankCtx`
behind the method boundary was measured to regress the parallel SpMV by ~3% —
and `search.BFSDirectionOptCtx` against `search.BFSDirectionOpt`, which does not
delegate to the `Ctx` form. Six further rows (the `*Parallel*Ctx` set) have teeth
in a weaker sense: delegation makes the code identical but two invocations are
two independent work partitions, so for those the comparison is a determinism
check. Twenty-seven of the 54 counterparts cannot report an error at all — the
swallowing itself is pinned by
`TestSearchCtxCancel_TwinErrorArityMatchesSource` (`:412`).

**The battery found an engine defect, and it found it on the path its own main
loop could not see.** Eight of the 58 rows could not honour a pre-cancelled
context at all — seven through one shared increment-then-mask shape, and
`search.TopologicalSortCtx` by a different mechanism entirely. This battery never
saw them fail: the concurrency audit written for rmp #2489 found them first and
they were corrected under rmp #2593 before the battery shipped, so those rows
assert the mandate rather than the shortfall (see
[Defects surfaced by this coverage work](#defects-surfaced-by-this-coverage-work),
finding 19). One further site survived that first sweep, and it is the one this
battery found rather than inherited: cancellation did not outrank
`search.ErrNegativeCycle` in `search.JohnsonAPSPCtx`
or `search.JohnsonAPSPParallelCtx`, because the shared prologue
`bellmanFordVirtualSource` (`search/johnson.go`) polled on the same
increment-then-mask shape, and Johnson runs that reweighting prologue **before**
its per-source poll. The main row loop could not see it, and the reason will
recur: that loop's fixtures are valid inputs by construction — they must be, or
`bg-err` would mean nothing — and Johnson does poll before its per-source loop,
so on a valid graph it looked compliant. The defect lived only where a prologue
decides the input is unusable before the entry point consults its context, which
is exactly the shape the precedence arm exists for; both Johnson rows are now in
it as the regression guard.

**Regimes this family does NOT reach.** Read these before assuming the context
surface is certified:

- **No mid-run cancellation, and therefore no promptness claim.** Both regimes
  cancel before the call. Neither says where inside a running algorithm the poll
  happens, nor how much work can still be done after cancellation is signalled.
  The obvious deterministic mechanism does not work here either: the in-package
  fakes `cancelAfterFirstCheck` and `cancelAfterNCalls` override only `Err()`
  over an embedded `context.Background()`, and four of the parallel entry points
  derive their own child with `context.WithCancel` and poll the **child** —
  because `Background().Done()` is nil the child is never linked to the parent
  and a `cancelCtx`'s `Err()` never consults its parent, so the fake's `Err()` is
  never called at all. Measured: those four return a complete result with a nil
  error under that fake, so a counted mid-run arm built on it would have been a
  clause that cannot fail.
- **The teardown arm proves no leak, not prompt joining.** Nine rows start
  goroutines — including `search.DiameterCtx`, which does so without carrying
  "Parallel" in its name, so scoping the arm by name would have missed it.
  `TestSearchCtxCancel_NoGoroutineLeak` (`:666`) drives all nine under both
  regimes inside `goleak`, which proves the pools do not leak indefinitely. It
  does not prove they are joined before the call returns, and a goroutine-COUNT
  comparison is not the fix: `pageRankEngine.close()` does not join its workers
  and was measured returning with up to `GOMAXPROCS` still-live goroutines on 39
  of 40 runs, which `goleak` correctly tolerates, while a count delta was
  measured to flake in **both** directions on the same code.
- **Several entry points have no inner poll at all, and this family cannot see
  it.** `HopcroftKarpCtx` polls per phase and `HopcroftTarjanBCCCtx` per DFS
  root, so on a connected graph the whole traversal is one uninterruptible
  window; `WCCCtx` and `WCCParallelCtx` are checkable at two points only, because
  `wccUnionEdgeRange` takes no context; `ClosenessCtx` and `HarmonicCtx` poll
  every 1024 **sources** rather than at every source as their godoc says. All six
  honour a pre-call cancellation, so all six are green here. That, and nothing
  more, is what this family claims about them.
- **Cancellation is not delivered through `Done()` alone.** Both regimes use a
  real `context.WithCancel`, so `Done()` closes and `Err()` reports. An entry
  point that selected on `Done()` and one that polls `Err()` are
  indistinguishable to this family.
- **The fixtures are small by design.** Each is a few tens of nodes, so the
  whole 58-way sweep costs ~10–25 ms (rmp #2489 records 12.9 ms per invocation,
  about 0.4% of the package's run time and inside its run-to-run spread), so it
  can run on the search battery's cadence. Sizing up is not free: `FloydWarshallCtx`
  alone costs ~9.6 s at `MaxNodeID` 4352.

One caution the godoc of these packages does not carry: roughly forty clauses
across the five packages claim a "wrapped `ctx.Err()`" and do **not** wrap.
Assertions here are built from the poll site, never from the doc.

## Storage / durability coverage

| Feature | Scenario / vehicle | Invariant |
|---|---|---|
| Concurrent durable commits + crash recovery | `durable-commit-crash` | acked ⊆ recovered ⊆ issued; failures absent; no torn CREATE |
| Background checkpointer + crash-safe `store.DB` teardown | `checkpoint-teardown` | no `ErrWriterClosed` into an acked commit; recovered ⊇ acked; `Stop()` joins |
| Read-transaction behaviour under concurrency + crash | `readtx-isolation` | no dirty/partial reads; whole-batch atomicity on recovery |
| Atomic csrfile publish under fault/ENOSPC, across every weight kind and access pattern | `csrfile-publish-fault` (`internal/sim/csrfile_access_matrix.go`) | a failed publish leaves either no file or the complete prior csrfile — never torn, now also at a SEED-DRAWN weight kind; and the whole `WeightKind` x `AccessPattern` grid round-trips exactly, with the weights decoded independently of the package's typed accessors, an advisory access hint proven to change no byte, `WeightAbsent` distinguished from a weighted file on four independent signals, and a truncated file refused with a typed sentinel while `Reinterpret` refuses to build a view — see below |
| Recovery genuine-corruption fail-stop | `wal-corruption-failstop` | a corrupted interior WAL frame is detected (CRC), recovery reconstructs exactly the clean prefix and refuses to append; a benign torn tail is not treated as corruption |
| Post-rename dir-fsync fail-stop (WAL prefix reclaim) | `checkpoint-dirfsync-fault` | a post-rename parent-dir fsync failure poisons the writer, yet reopen recovers the exact committed state |
| DDL (index + UNIQUE constraint) across the checkpoint/snapshot boundary | `ddl-checkpoint-crash`; `constraint-enforce` and `index-diversity` now checkpoint too | the checkpoint's reclaimed WAL prefix COVERS the DDL frames (measured on the SimDisk image), the pure-snapshot phase replays ZERO WAL ops, and the recovered schema still enforces UNIQUE, answers every index seek, and matches `SHOW`/`db.*` |
| `graph/io` export→import: CSV, JSONL, GraphML and DOT | `io-roundtrip-fault` (`internal/sim/storage_fault_scenarios.go` + `internal/sim/graph_io_surface.go`) | a clean round-trip reproduces the modelled edge set exactly and an export under ENOSPC fails with a typed error leaving no partial artefact a re-import would accept; the DOT writer — which has no reader — is adjudicated by CROSS-FORMAT AGREEMENT with CSV and JSONL over a model built to force quoting, weight labels and a bare node statement, with the one legitimate disagreement (an edge-list CSV cannot carry an isolated vertex) asserted in shape rather than waived; the JSONL property path round-trips every property KIND; the `csv.Options` delimiter / comment / header / weight-column / formula-sanitisation space is driven beyond `DefaultOptions`; every export is checked for byte-reproducibility; and a seed-mutated export sweep requires no panic and an effective mutation per format. The sweep's bounded-allocation bound is adjudicated in a SERIALISED test arm rather than in the scenario, because it is measured with a process-global counter that bills a concurrently scheduled scenario for its neighbours (rmp #2553). Every defensive cap in `graph/io` is provoked, and every `*Ctx` reader is cancelled mid-parse, by `RunGraphIOGuards` — see below |
| Offline bulk-import publication (`store/bulkimport`) — **parity only, NOT fault coverage** | `bulkimport-parity` | a published snapshot reopens through real recovery equal to the harness model exactly (node set two-sided, labels, properties by **kind and value**, per-handle edge multisets including parallel twins), `SnapshotHit` with **zero** replayed WAL ops on two successive opens, and the measured lifecycle contract (`ErrNotFinished` / `ErrFinished` / `ErrStoreNotEmpty`, their precedence, and `PublishResult.Stats`); plus the publish's byte-reproducibility boundary. **No fault regime is reachable** — see the note below |
| Offline bulk LOADER full contract (`store/bulk`) — content, streaming, caps and fault-injected publication | `bulk-load-oracle` (`internal/sim/bulk_load_oracle.go`) | every arm adjudicates the LOADED GRAPH against a harness model that reimplements the documented ingest rules in plain Go and calls neither `graph/adjlist` nor `csr.OrderRuns`, so a defect shared by every builder is still visible: `Drain` clean and ctx-cancelled mid-stream, `AddBatch` including the partial-ingest-then-`ErrTooManyRows` contract, `Parallel` true vs false asserting a BYTE-IDENTICAL csrfile and identical CSR slices, the `MaxRows` crossing on all three ingest entry points, all four Directed × Multigraph configurations, publication onto a `SimDisk` under `FaultRate` / `ArmSyncFaultAt`, reopen verified against the model, corruption fail-stop, and a host-crash differential across the publish rename → parent-fsync window. The post-fault oracle is THREE-way (absent, complete, or rejected), never two |
| Crash **during** the snapshot publish, at each step of the crash-atomic swap | `checkpoint-crash-storm` | acked ⊆ recovered ⊆ issued across a crash inside the publish window; a stranded backup is promoted by recovery (measured on the durable image and on `store.recovery.snapshot.promoteParentFsync`), never a half-published snapshot |
| Node-key and edge-weight CODEC matrix across crash and upgrade | `codec-matrix` (soak; `internal/sim/codec_matrix.go`) | seven `(key codec, weight codec)` arms each survive the three snapshot-publish crash windows AND the upgrade + snapshot boundaries with acked ⊆ recovered ⊆ issued adjudicated BY KEY; the durable `mapper.bin` carries the layout the key type selects (v1 for the string control, v2 for the other six) and the snapshot-only reopen replays ZERO WAL ops, so every recovered key came through the mapper; `txn.ErrNoWeightCodec` is provoked and its actual behaviour pinned. One measured gap is pinned rather than tolerated: a struct weight is dropped by the snapshot CSR writer — see below |
| Corruption of a published snapshot COMPONENT | `snapshot-corruption-failstop` | a byte flipped in any of the nine components fail-stops recovery with that component's typed sentinel; recovery returns no store, mutates nothing on disk and leaves `db/wal` byte-identical; the restored image still recovers the exact committed model. One documented non-fail-stop is pinned in the same run, as a PAIRED oracle (rmp #2490): an intact `indexes/<name>.bin` is HYDRATED and a corrupt one is REBUILT, asserted on the engine-scoped population counter in both directions, with both reopens verified against a full scan and against the committed key set. The manifest's key-name region was the second non-fail-stop until rmp #2520 checksummed it — see below |
| Cross-release compatibility of the on-disk SNAPSHOT format | `TestCrossRelease_*` (soak: prior-tag subprocess) + `TestCrossReleaseCompat_*` (short: frozen fixtures) | a PRIOR release publishes a **checkpoint** — snapshot directory plus truncated WAL — and the current code reopens it through the FULL-STACK `recovery.OpenCtx`, so that release's `manifest.json`, `csr.bin`, `labels.bin`, `properties.bin` and `mapper.bin` are parsed by current code. "The snapshot was opened" is adjudicated from two INDEPENDENT observations — the filesystem read with the current manifest reader, and recovery's own `SnapshotHit` — so a directory present but skipped is a `SnapshotProvenanceGap` that fails parity, not an unfalsifiable false. On the short layer a frozen pre-#2520/#2526 snapshot directory asserts both directions of the documented contract: an older artefact still opens (manifest loads reporting `IntegrityVerified` **false**; the dense width-8 weights column parses with its exact `float64` values), and a newer artefact is refused by the older reader's documented rule **deterministically**, by both of that release's guards. Fixture digests are pinned in the test source rather than in a golden, so `-update` cannot regenerate the old format away — see below |
| Per-transaction op caps (CWE-770), producer **and** replay | `txn-oversize` (`internal/sim/txn_oversize.go`) | an over-cap commit is refused with `txn.ErrTransactionTooLarge` **before any frame is written** — proved by the durable WAL image being BYTE-identical across the refusal and the live graph unmutated, not by the error alone — and the surviving file recovers clean with every refused key absent; a hand-built WAL whose marker-less run exceeds the replay cap fail-stops with `recovery.ErrTransactionTooLarge`, keeps exactly the committed prefix, and is refused by the store-open rather than appended onto. The boundary is MEASURED on both sides and the two caps agree exactly (cap ops passes, cap+1 fails both). Until this task the cap reached only the replayer, so neither sentinel was reachable under simulation at any setting — see below |
| Lock-free CSR publisher refcount lifecycle (`graph/generation`) | `generation-swap` (`internal/sim/generation_swap.go`) | every acquisition's traversal matches the model's INDEPENDENTLY computed adjacency for the generation the artefact's own content declares, so a torn swap is caught by IDENTITY rather than by well-formedness (the package's own rotation test asserts a constant edge count and discriminates no generation from any other); every generation's refcount is audited AT REST, after every reader is joined and the publisher stopped — the only quiescent point — plus a structural floor (>=1 while held) and ceiling (<=readers+1) that hold at every instant; `PublishWithDrain` with a reference held by the publisher itself returns `ErrDrainTimeout` at ANY positive timeout (1ns..20ms measured) without corrupting `Current`, while the forced unbounded drain beside it must complete, so neither direction passes vacuously; `Close` drains a LIVE reader fleet racing a publisher with none left wedged, and the post-close contract (`Acquire`->nil, `Current`->nil, `Publish`/`PublishWithDrain`->`ErrClosed`) is pinned. The plan and every sub-seed are drawn up front so the plan digest is seed-reproducible while the interleaving is not — stated, not glossed. **USE-AFTER-FREE is out of reach** without a poisoned allocator and is not claimed; use-after-RECYCLE is what the sentinel clauses cover. **Eleven** library mutations were applied and reverted to prove the clauses fire, and the table in the file header is the authoritative count: five of them were REPRODUCED in a later validation pass and are stamped there with the seed and reader count each was measured at, while the other six are marked inherited-and-unverified because their sighting counts carry no conditions and are not reproducible as written. One (dropping `Acquire`'s re-check) is caught only probabilistically and, on the inherited measurements, needs both `-race` and a fleet wider than the core count; no detection RATE is claimed for any width set, because the only width those measurements share with the default fleet detected nothing in 5 runs. The wide 64/256/1024 fleets and the 64-seed geometry sweep run in the SHORT layer: they were gated behind the soak tag on an estimate of "a million Acquire/Release pairs per seed … minutes under the race detector", measurement put the whole set at **0.46 s** under `-race` on 10 cores, and they were promoted so the default gate exercises the published concurrency levels instead of skipping them |
| Fluent pattern-query engine as an INDEPENDENT second read path (`graph/query`) | `fluent-query` (`internal/sim/fluent_query.go`) | every probe is adjudicated THREE ways against a model-computed arbiter, with the three comparisons as SEPARABLE clauses so a red run names the wrong path: `fluent-vs-oracle`, `cypher-vs-oracle` and `fluent-vs-cypher` (a shared-substrate defect moves both engines together and leaves the third clause silent, which is the correct attribution rather than a fluent-vs-Cypher divergence). This is deliberately NOT the stance `differential.go` takes — that facility compares the engine with itself, which is sound only because the engine guarantees its two planner variants are result-equivalent. A FOURTH channel, neither engine, walks the Mapper directly and is held to the model BEFORE any probe, so a probe failure cannot be explained away by an already-diverged substrate. `Out()` must answer identically over the live-filtered AND the tombstone-agnostic CSR build — a theorem of the two prunes together, not a coincidence — and the CSR is read by nothing except `Out()`, so the label / property / range probes are provably CSR-independent. The tombstone gate is on `seedAllLive`'s Mapper walk, which never forgets a slot and is therefore DETERMINISTIC; the label-bitmap corpse count is swept by lpg's background vacuum (MEASURED at 3 and 2 for the same seed in the same process) and is telemetry, gated on nothing. `Out()`'s ghost-arc prune is unreachable on the live graph (MEASURED: `DETACH DELETE` strips arcs, raw and live CSR `Size()` equal on all 24 sweep seeds) and is driven by a constructed fixture that asserts its own precondition. Seek and scan are separated BY CONSTRUCTION (`Vertex(label, pred)` vs `Vertex(label).Vertex(pred)`, since `labelsInPreds` of a label-free predicate list is empty and both seek helpers refuse it); served-ness is established by ENUMERATING the guard's conditions, which is stated as such rather than presented as observation. The one divergence this scenario FOUND — the seek and scan arms disagreeing for mixed INTEGER/FLOAT range bounds — became the asserted `range-mixed` clause (rmp #2600), and closing it exposed a SECOND asymmetry between two PREDICATES rather than two arms: the unified range matched where the still-shared-kind equality did not, so the same data answered differently depending on how the predicate was written. rmp #2601 unified `equalValue` through the same exact comparator, removed the `hashLookuper[int64]`/`[float64]` arms as unsound SUBSETS of a unified equality, and moved a numeric equality onto the companion btree as the degenerate range `[v, v]` with `equalValue` as the residual — so an equality and a degenerate range now agree by CONSTRUCTION over one comparator, one index and one residual. The identity is scoped to the ORDERABLE kinds: openCypher's equatability is wider than its comparability, so for BOOLEAN, BYTES and TIME the two predicates legitimately differ, and that divergence is pinned rather than closed. Not claimed: resurrection, concurrency, multi-hop |
| Typed-schema validator as a runtime ENFORCEMENT hook (`lpg.Graph.SetValidator`, `lpg.Graph.ValidateNode`, `graph/lpg/schema`) | `typed-schema` (`internal/sim/typed_schema.go`) | the accept/reject verdict is adjudicated against a DECLARATION-TABLE oracle — the same table the schema is built from, never the schema itself — on all **five** validator-consulting write paths (VERIFIED in source; the task brief named only the edge-property ones), and classified by SENTINEL rather than by non-nilness, because `ErrTypeMismatch`, `ErrUnknownProperty` and `ErrMissingRequired` are three different refusals. Coverage is CONSTRUCTED: the fifteen (path, verdict) cells are SWEPT once per epoch in a seed-shuffled order, so the seed decides the order and never the coverage. Every refusal is proven side-effect-free on five separate clauses — the value through the path's own accessor, the value through the columnar store's SECOND accessor, the node and edge population, whether the property key was INTERNED (MEASURED: the hook runs BEFORE the intern, so it is not), and on the fused path that neither the edge nor its fresh endpoint node appeared. `Graph.ValidateNode` **had no caller outside `graph/lpg`** before this task (MEASURED: only the package's own internal `NodeValidator` dispatch and its own tests), so required-property existence is embedder-invoked and gets its own arm: the mid-build rejection paired with the finalised acceptance, an unlabelled control, the never-interned benign exit, and a pre-installation fixture that is the only route (short of the recovery bypass) to ValidateNode's kind re-check over already-present properties. The recovery asymmetry is pinned as FIVE clauses from one constructed probe, not documented away. Two defects surfaced: a bit-packed BOOL column PANICKED on the fused append path (**fixed**, reachable from public `graph/lpg` on a three-node fixture with no DST involved), and `txn.Tx.Commit`'s fsync-BEFORE-validate ordering let a REFUSED value become durable and be resurrected by recovery (**FIXED 2026-08-25, rmp #2602, `fd7159d6`** — the validator now runs before the durable write, and the scenario's arm inverted with it; see below) |

### Snapshot component corruption is now covered; the manifest is checksummed (rmp #2467, #2520)

`store/snapshot` declares **nine** typed corruption sentinels — one per durable
component — and the manifest carries a CRC32C for each. Before this task the
only corruption the simulator injected was a byte flip inside a WAL frame
(`wal-corruption-failstop`), `SimDisk.CorruptRange` appeared nowhere else
outside the disk's own unit tests, and **none of the nine sentinels had ever
been reached under simulation**. `snapshot-corruption-failstop` now flips a byte
in each component of a published snapshot and adjudicates the reopen that
follows, over a fixture whose checkpoint has folded the **whole** WAL — so the
snapshot is the only durable source of the committed graph and a refusal is a
genuine fail-stop rather than a fallback.

The sentinel a flip produces is a function of **where** it lands, which the
scenario measures rather than assumes:

| Component | flip at byte 0 (the header) | flip anywhere else |
|---|---|---|
| `manifest.json` | `ErrManifestCorrupted` (JSON parse) | `ErrManifestCorrupted` (CRC32C trailer) |
| `csr.bin` | `ErrCSRCorrupted` + `ErrCorrupted` | `ErrCorrupted` (CRC) |
| `labels.bin` | `ErrLabelsCorrupted` + `ErrCorrupted` | `ErrCorrupted`, or both |
| `properties.bin` | `ErrPropertiesCorrupted` + `ErrCorrupted` | `ErrCorrupted` |
| `mapper.bin` | `ErrMapperCorrupted` **alone** | `ErrCorrupted`, or both |
| `tombstones.bin` | `ErrTombstonesCorrupted` + `ErrCorrupted` | `ErrCorrupted`, or both |
| `edgehandles.bin` | `ErrEdgeHandlesCorrupted` + `ErrCorrupted` | `ErrCorrupted` |
| `constraints.bin` | `ErrConstraintsCorrupted` + `ErrCorrupted` | `ErrCorrupted`, or both |
| `indexdefs.bin` | `ErrIndexDefsCorrupted` + `ErrCorrupted` | `ErrCorrupted` |
| `indexes/<name>.bin` | *(no error — see below)* | *(no error)* |

Two asymmetries in that table are worth naming. First, a component's **own**
sentinel is raised only when the flip breaks the structural parse; a flip in a
payload region parses cleanly and is caught later, when the CRC32C is compared
against the manifest entry, which surfaces as the directory-level
`snapshot.ErrCorrupted` **without** the component's own sentinel. Second,
`mapper.bin` is the one component whose header failure is **not** wrapped in
`ErrCorrupted`: `LoadSnapshotFull` peeks the mapper's format version before
handing it to the verified reader, and that peek returns its error unwrapped.
Everything else in the load path wraps.

`indexes/<name>.bin` is **tolerated by design**, not a gap, and it is the
battery's **one documented exception to the fail-stop rule**: `snapshot.LoadIndexes`
reports a CRC-failing payload as nil bytes, recovery classifies it as
`recovery.ErrIndexPayloadUnreadable`, and the engine rebuilds that index from the
recovered graph — per index, never a fail-stop. An index is derived data over an
already-recovered, independently integrity-checked graph, so a rebuild restores
byte-identical content and loses nothing; refusing to open the database over it
would deny service for a fault with a complete local repair. Every reference
engine reaches the same conclusion: PostgreSQL discards and rebuilds
`pg_internal.init`, Memgraph rebuilds its label indexes unconditionally on
recovery, and Neo4j coerces an unreadable index header to `POPULATING` and
repopulates.

The one index-related condition that **is** fail-stop stays fail-stop: a manifest
index name that would escape the `indexes/` directory raises
`snapshot.ErrManifestCorrupted` from `validateIndexName` and refuses the open.
That is a path-traversal security event driven by attacker-controlled manifest
bytes, not benign corruption, and it is deliberately unreachable through the
per-payload reason codes.

**The arm is PAIRED (rmp #2490).** Requiring only that the reopen succeeds was
*vacuous on the hydration axis*: before #2490 the simulator built its engine
through a constructor that carries no snapshot payloads, so every reopen rebuilt
every index whatever the payload said — "recovery survived and the indexes are
consistent" held identically with a valid payload, a damaged one, and no payload
at all. The arm now drives two reopens over the same fixture and asserts the
engine-scoped population counter
(`cypher.Engine.RecoveredIndexPopulation`, surfaced to the harness as
`SimStore.RecoveredIndexPopulation`) in **both** directions:

| half | payloads | required population | measured (seed `0x1`) |
|---|---|---|---|
| control | intact | every registered index **HYDRATED**, none rebuilt | `registered=5 hydrated=5 rebuilt=0` |
| corrupt | one byte flipped in each | every registered index **REBUILT**, none hydrated, and **every** payload reported unreadable | `registered=5 flipped=5 hydrated=0 rebuilt=5 unreadable=5` |

Both halves go through one shared verification body — the same
`CheckIndexConsistency` cross-check against a full label scan and the same
committed-key-set re-read — because the whole claim under test is that a hydrated
index and a rebuilt one are indistinguishable in their answers; two near-identical
bodies could drift and check one half more weakly than the other. The counts are
anchored to `index.Manager.ListIndexes()` rather than to a constant, so the
assertion survives the engine adding or removing an internal numeric companion,
and `hydrated + rebuilt == registered` is asserted on its own: an index that is
registered without being populated would be seekable while empty.

`unreadable == flipped` is what attributes the rebuilds to the injected
corruption rather than to a coincidence, and the terminal non-vacuity gate
refuses evidence in which the control half hydrated nothing — precisely the state
this harness was in before #2490, when it passed.

**The finding (rmp #2467): `manifest.json` carried the CRC of every other
component and none of its own.** A byte flipped inside a JSON **key name** left a
syntactically valid document whose key `encoding/json` no longer recognised, so
the field was dropped and decoded to its zero value with no error anywhere. A
byte-by-byte sweep of this scenario's published manifest (1399 bytes) found **360
bytes, 25.7% of it, whose corruption recovery accepted silently** — the key names
(`"version"`, `"order"`, `"size"`, `"commit_ts"`, `"crc32c"`, `"name"`), the
index-name string values, and the trailing newline. The consequence measured on
the worst of them: flipping one character of the `"commit_ts"` **key** dropped the
MVCC clock floor recovery derives (`recovery.Result.MaxCommitTS`) from 20 to 0, so
`RestoreMVCCClock` was never called and the reopened graph re-minted instants the
image already contained — the loss rmp #2309 exists to prevent, reachable through
an undetected corruption. No committed node was lost; the damage was to the clock,
not to the data, which is why no test that counts recovered nodes could see it.

**The fix (rmp #2520): a CRC32C trailer.** `snapshot.WriteManifest` now appends a
fixed 16-byte trailer after the JSON document — an 8-byte magic, a 4-byte
algorithm identifier, and a CRC32C over every preceding byte. `LoadManifest`
verifies it **before it reads a single field**, the version check included, since
a manifest is the first thing a store open reads and nothing in it may be trusted
until its bytes are established.

The checksum lives *outside* the structure it protects, which is the property
that closes the gap. A `crc32c` field *inside* the JSON would have to be excluded
from its own computation, leaving its own key name unprotected — the same defect,
merely relocated. The trailer covers the document whole: every key name, every
value, every byte of indentation.

Two rules make the coverage total rather than nearly total:

- **A non-empty tail MUST verify.** The reader splits the file at
  `json.Decoder.InputOffset()`. A tail of nothing but whitespace means "written
  before the trailer existed" and is accepted unverified; anything else must be a
  well-formed, verifying trailer. Without this a flip in the *magic* would demote
  a protected manifest to "legacy, accepted" — the silent acceptance moved into
  the trailer instead of removed.
- **A manifest that declares a trailer must carry one.** `Manifest.Integrity`
  records the framing scheme, so a manifest whose trailer was lost entirely
  (a zeroed tail block, a truncating copy) is refused rather than mistaken for a
  legacy file. Losing the protection needs two independent damages, not one.

Re-measured after the fix: **0 of 1450 bytes** of the published manifest can be
flipped without recovery refusing (`TestSnapshotCorruption_ManifestUncheckedByteCensus`,
which now requires zero). `TestSnapshotCorruption_ManifestKeyRegionIsChecksummed`
pins the `commit_ts` key specifically, and
`TestSnapshotCorruption_MVCCClockFloorIsNotSilentlyZeroable` states the same
guarantee in engine terms: sweeping all 1449 manifest bytes, the restored clock
floor of 20 was never moved — 1449 flips refused, 0 accepted.

**Forward compatibility is preserved in both directions, and `ManifestVersion` is
deliberately NOT bumped.** The trailer is a change to the file *framing*, not to
the JSON *schema*, and keeping those layers apart is what lets integrity and
`encoding/json`'s ignore-unknown-fields policy hold together. An older snapshot
has no trailer, is accepted unverified, and reports `Manifest.IntegrityVerified`
false — the frozen v1 fixture in `store/snapshot/testdata` still loads byte-for-byte
unchanged, with no migration. A newer snapshot read by an older build is
unaffected too: `json.Decoder` stops at the end of the first complete value, so a
build that predates the trailer never reads past the closing brace. Bumping the
schema version would have made those readers reject the file with
`ErrManifestUnsupported` for no reason. The "absent means no value" policy
`Manifest.CommitTS` documents is therefore unchanged — but it is now only ever
evaluated over bytes the trailer has established, so "absent" can only mean the
writer omitted the field, never that corruption renamed its key.

The guarantee is scoped to *accidental* corruption. CRC32C is not a MAC: an
attacker who can rewrite `manifest.json` can recompute the trailer. The defence
against a hostile store directory remains the surrounding controls (`O_NOFOLLOW`
component opens, `DefaultMaxManifestBytes`, the per-component allocation bounds).

Closing the gap removed the substrate the battery's primary oracle stood on.
Every arm asserts recovery REFUSED, and an assertion that can only ever hold
proves nothing, so the reachability control moved rather than being dropped:
`TestSnapshotCorruption_OracleFiresWhenRecoveryWronglySucceeds` now aims its arm
at an `indexes/<name>.bin` payload — the one durable file in a published snapshot
a reopen accepts damaged — and still requires the run to REPORT the acceptance.

A third observation, measured by the scenario's own determinism check rather
than assumed: two identical fixtures built in the same process publish snapshots
whose components differ in LENGTH in two places. `manifest.json` embeds
`created_at`, whose RFC3339 fraction trims trailing zeros; and `mapper.bin` holds
the engine-minted natural keys, which are `__cx_<hex>` drawn from a
**process-global** counter (`cypher/exec/create_node.go`, where the counter is
deliberately seeded from the largest existing suffix so a new process cannot
collide with keys an earlier one persisted). The keys' hex width — and with it
the component's size — therefore grows as a process creates more nodes: a
measured 303-byte `mapper.bin` on a process's first fixture against 318 bytes on
every later one. This is documented, intentional behaviour, not a defect, but it
means a snapshot's component sizes are a function of process history, so
`TestSnapshotCorruption_Deterministic` compares offsets where the sizes match and
REPORTS a size difference rather than hiding it. It is the same class of
observation as the bulk-import byte-reproducibility note below.

A second, smaller finding, measured while probing the manifest guards:
`ErrManifestTooLarge` bounded what the decoder **consumed**, not the file's
length. `json.Decoder` stops at the end of the first complete value, so
whitespace appended after the closing brace was never read and a `manifest.json`
of any size on disk was accepted. That was defensible for a guard whose stated
purpose is to bound the transient decode allocation, but it meant the ceiling
could only be reached by bytes inside the JSON value — which is how the scenario
drives it. **Closed by rmp #2520**: verifying the trailer requires reading to the
end of the file, so the ceiling now bounds the bytes read. The scenario still
pads *inside* the object, because that probe reaches the guard under both
readings and so remains the stricter one.

### Cross-release testing reached the WAL and nothing else (rmp #2477)

The cross-release harness (`internal/sim/crossrelease*.go`,
`cmd/sim-xrelease-helper`) imported `store/wal`, `store/recovery` and
`store/txn` — grepping that path for `snapshot` returned nothing. A prior
release therefore only ever handed the current code a **WAL**: no prior
release's snapshot directory had ever been opened by current code, and no v1 or
v2 manifest had ever been loaded by the current reader. The entire on-disk
snapshot format family — `manifest.json`, `csr.bin`, `labels.bin`,
`properties.bin`, `mapper.bin`, `edgehandles.bin` — sat outside cross-version
testing.

Two surfaces created in this same sprint sat exactly in that gap, and both were
shipped **without a schema-version step** precisely so old and new artefacts
would keep interoperating:

- **rmp #2520** appended a CRC32C integrity trailer after the manifest JSON.
  Older snapshots have no trailer and must still open, reporting themselves
  unverified rather than claiming an integrity they do not have.
- **rmp #2526** introduced the `0xFF` sentinel in `csr.bin`'s weight-width
  header byte to select a variable-width, codec-encoded weights section. Older
  files keep the dense native layout byte for byte, and an older reader meeting
  a sentinel-bearing file must fail loudly rather than mis-slice the column.

Because neither bumped a version, no version gate would have caught a regression
in either direction, and neither had a cross-release test.

**What now covers it.** The helper publishes a checkpoint before it exits and
the reopen routes through the full-stack recovery path; frozen fixtures pin the
old shapes on the short layer and hold the legacy reader's documented rule to
both directions. The design — the two-stage helper build that degrades one
capability instead of skipping a whole tag, the two independent observations
behind the snapshot-provenance verdict, and the three guards against a silently
regenerated fixture — is described in
[docs/dst.md](dst.md#cross-release-compatibility-beyond-the-wal-rmp-2477).

**The checkpoint half did not build at any real tag (rmp #2531).** As shipped,
that coverage reached `HEAD`-as-prior only. `Checkpointer.RunCheckpoint` was
exported only from **v0.6.0**, so at `v0.2.0` and `v0.3.0` the helper's checkpoint
file failed to compile, the two-stage build dropped it, and both tags fell back to
a WAL-only image — reporting the degradation loudly, which is how it was found.
Driving the checkpoint through `Start`/`TriggerCtx`/`Stop` instead — exported
unchanged since **v0.1.0** — makes every one of the fourteen release tags publish
a manifest-v3 snapshot that current code opens with snapshot-only recovery, so
there is no snapshot floor. The snapshot facts are now asserted per tag rather
than logged. Because a published snapshot truncates the WAL and thereby hides any
prior defect living in WAL replay — `v0.1.0` and `v0.2.0` have one, reading their
own 32-node image back as 79 nodes — a deliberately WAL-only arm
(`ForceWALOnly`) retains that path and doubles as the negative control proving
`SnapshotOpened` can read false. Both are described in
[docs/dst.md](dst.md#the-fallback-fired-at-every-real-tag-rmp-2531).

**Measured on arrival.** No cross-version defect was found: a pre-#2520 manifest
loads and reports `IntegrityVerified` false, a pre-#2526 dense `csr.bin` returns
its exact weights, a sentinel-bearing `csr.bin` is refused by both legacy guards
(over-determined, not by luck), and the full-stack reopen recovers the whole
graph from a directory whose WAL had shrunk to zero bytes with zero replayed WAL
ops. What changed is that all of that is now falsifiable.

### Bulk-import publication is covered for PARITY, not for faults (rmp #2466)

The `bulkimport-parity` row above is deliberately narrower than every other row
in the table, and the gap is structural rather than an omission.

Every other durability scenario injects its faults through `SimDisk`, which
reaches the persistence packages via their filesystem seams (`wal.OpenFS`,
`recovery.OpenFS`, `snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS`
and siblings). `bulkimport.Publish` has **no such seam**: it calls `os.MkdirAll`
and `os.ReadDir` directly and writes through the **non-seamed**
`snapshot.WriteSnapshotFullCtx`, while `ImportInto` takes a `storeDir string`
plus an `Options` that carries no filesystem. A `SimDisk` therefore cannot be
placed underneath a bulk-import publish without changing the production API —
**filed for a user decision as rmp #2518**, and deliberately not done under
#2466.

So for bulk-import publication the following remain **uncovered**: `ENOSPC`
mid-write; a failing `fsync` on a component, on the staging directory, or on the
parent directory; a failing or crash-interrupted `snapshot.tmp` → `snapshot`
rename; and a crash landing inside the publish window. The scenario's
crashed-import arm reconstructs the *outcome* state of such a crash — a complete
snapshot moved to the assembly name, which recovery must ignore and clean up —
which measures recovery's treatment of that state, **not** the writer's
behaviour while reaching it.

A second finding from the same task, measured rather than assumed: a bulk-import
publish is **not byte-reproducible** once items carry two or more properties.
Republishing the identical record slices twice yields data components of the same
names and sizes but different bytes, and stripping properties — or reducing each
item to exactly one — makes it byte-identical, which isolates the cause to Go map
iteration over the `Properties` maps. The *logical* result is identical every run
(the parity pass proves it), so this is not a correctness defect and matches what
`bulkimport.Node` documents; but two imports of identical data cannot be compared
by checksum, and bulk-import snapshots will not deduplicate in content-addressed
storage. `TestBulkImportParity_ByteBoundary` pins the boundary.

The `DiskConfig.FaultRate` knob, the direct `CorruptRange` mutator and ten
one-shot arms back these scenarios. All default to inert and none draws from the
[Seed], so arming one never perturbs the fault stream and existing scenarios stay
byte-identical.

**Faults** — an operation fails: `CorruptRange` (applied directly rather than
armed), `ArmSyncFaultAt`,
`ArmDirSyncFaultForPath` (rmp #2537), `ArmParentDirSyncFaultForPath` and
`ArmRenameFaultForPath`. `ArmSyncGateAt` parks a caller inside its fsync instead
of failing it, which is how a crash is made to land in the phantom-commit window.

**Crash-window selectors** — an operation succeeds, and the arm pins *which* of
its legal crash outcomes the run takes, because a metadata mutation is not
crash-survivable until the containing directory is fsynced.
`ArmRenameWritebackForPath` pins "the rename had reached stable storage";
`ArmRenameRollbackForPath` (rmp #2514) pins the other legal branch; and
`ArmRemoveWritebackForPath` / `ArmRemoveRollbackForPath` (rmp #2536) do the same
for an unlink, whose two outcomes are "the removal stuck" and "the file I deleted
is back after the crash". A removal has no illegal outcome, so it has no
counterpart to `ArmRenameRevokeBothForPath`, which exists purely to reproduce the
physically impossible both-names-lost state on purpose so the harness's own gates
can be shown to reject it.

Because a journalling filesystem commits metadata **in order**, renames and
unlinks share ONE ordered log with a single durable-prefix draw
(`SimDisk.direntUndos`, rmp #2536): the crash keeps a prefix of the issued
mutations and reverses the suffix. Two independent draws would let a crash keep
an unlink while reversing a rename issued before it, which is an interleaving no
filesystem can produce.

Every arm has a reachability observable, and a scenario that arms one is expected
to assert it: `RenameFaultCount`, `RenameWritebackCount`, `RenameRollbackCount`,
`RenameRevokeBothCount`, `DirSyncFaultCount`, `RemoveWritebackCount`,
`RemoveRollbackCount`, plus the window and shape observables `SyncCount`,
`PendingRenameCount`, `PendingRemoveCount`, `LastCrashRenameOutcome`,
`LastCrashRemoveOutcome`, `LastCrashDiscardedBytes` and the removal-hit counters
`RemoveHitCount` / `RemoveHitCountForPath`. An arm that never fires is not an
assertion, so the counter is what distinguishes "the primitive fired" from "the
arm was silently ignored".

### Crash during the snapshot publish (rmp #2465, closing #1827)

Until sprint 347 a checkpoint could never be *interrupted*: `SimStore.Checkpoint`
is synchronous and always ran to completion, and `Simulator.maybeCheckpoint`
treats any checkpoint error as a hard run failure. The whole interrupted-publish
half of the durability contract was therefore unexercised, and recovery's
snapshot-promote repair — the block in `store/recovery` that promotes a stranded
`snapshot.bak` back to the live name, marked by the
`recovery.snapshot-promote-post-rename-pre-fsync` crash point — was **dead code
under simulation**.

Two things had to change before the window could be reached at all, and both
were found by measurement rather than assumed:

* **The renames could not fail.** Every other step of the publish
  (`write+fsync components → fsync staging dir → archive rename → publish rename
  → fsync parent`) could already be faulted, but the two renames could not, so
  the publish path's own archive-restore branch was unreachable.
  `ArmRenameFaultForPath` closes that gap. The task's premise that the
  *parent fsync* also could not fail was **wrong**: the pre-existing
  path-keyed `ArmParentDirSyncFaultForPath` already targets it, which a probe
  confirmed.
* **A crash in the publish window manufactured a false total loss.**
  `SimDisk.Crash` revokes *every* not-yet-fsync'd dirent, and the publish issues
  its two renames back to back with no fsync between them — so the crash dropped
  both the archived backup and the newly published snapshot, an outcome no real
  filesystem produces (a lost rename leaves the *old* name). That single
  modelled outcome is also the reason the promote repair was unreachable: it
  exists precisely for "the archive rename reached disk, the publish rename did
  not". `ArmRenameWritebackForPath` selects that other, equally legal branch of
  the crash-window non-determinism, one rename at a time and opt-in.

`checkpoint-crash-storm` then crashes at three points of the swap
(`stranded-backup`, `publish-rename`, `archive-rename`) while concurrent Bolt
committers are still writing — the publish is checkpoint phase 2 and holds no
commit lock, so the window is genuinely raced, which the run measures as durable
commits landing *during* the interrupted checkpoint.

The DST does not observe crash points directly (see
`CoverageTracker.UnobservableSignals`): `crashpoint.Breakpoint` is compiled out
without the `gograph_crashinject` tag and SIGKILLs the process with it, which
would kill the test binary instead of producing the harness's in-process crash.
Bridging it would mean adding a pluggable handler to a production-callable
package. The scenario instead reproduces the *window* the site marks and observes
the *branch* it guards through surfaces that already exist — the durable image
(backup-only before the reopen, live-only after it) and store/recovery's own
exported `store.recovery.snapshot.promoteParentFsync` counter.

### The codec surface is now covered; struct weights do not survive a checkpoint (rmp #2473)

Before sprint 347 the simulator drove exactly **one** codec pair. That was
structural rather than an omission: every Cypher-driven scenario reaches the
graph through `cypher.Engine`, whose constructors take a
`*txn.Store[string, float64]` and nothing else, so `OpenSimStore` hardcoded
`txn.NewStringCodec` / `txn.NewFloat64WeightCodec`. `NewIntCodec`,
`NewInt32Codec`, `NewInt64Codec`, `NewUint64Codec`, `NewUUIDCodec`,
`NewBinaryMarshalerCodec`, `NewInt64WeightCodec` and
`NewBinaryMarshalerWeightCodec` had never appeared in a simulated crash, and
neither had the **version-2 byte-mapper**: `snapshot.WriteMapper` delegates to
`WriteMapperString` for `N == string`, so the codec-framed layout (and its read
side, `ReadMapperBytes` → `snapshot.ApplyMapperToGraphWithCodec`) was
unreachable from this harness.

`OpenSimStore` is now the string/float64 specialisation of a codec-generic core,
and `codec-matrix` runs seven arms through the crash-storm and upgrade
scenarios. Six of the seven hold completely.

**The seventh measured a real gap.** For a `BinaryMarshaler` weight the numbers
separate the two durable paths cleanly: at the WAL-only boundary **95 of 95**
acknowledged edges came back with their weight; after one folding checkpoint
over the same image — WAL measured to 0 bytes, 0 WAL ops replayed — **191 of
191** came back with the ZERO weight.

The cause is that `store/snapshot`'s CSR component never consults
`txn.WeightCodec`. It sizes a weight with the fixed table in `csrWeightSize`
(`store/snapshot/writer.go`), which returns 0 for every type outside the Go
primitives — including any struct, the case
`txn.NewBinaryMarshalerWeightCodec` exists for. `WriteCSR` then emits
`hasWeights=0`, which is **the same on-disk encoding a deliberately weightless
graph produces**, and the checkpoint truncates the WAL prefix holding the true
values. So an embedder using a non-primitive weight with checkpointing enabled
loses those weights silently and permanently, with no error at any layer.
Measured: `csrWeightSize[float64]` = 8, `csrWeightSize[int64]` = 8,
`csrWeightSize` of a struct with one `int64` field = 0,
`csrWeightSize[struct{}]` = 0.

Fixing it means changing `store/snapshot`, which was outside this task's scope,
so the behaviour is **pinned** instead: the affected arm must come back with the
zero weight on a snapshot-only recovery, and a separate non-vacuity check
requires that outcome to have actually been observed. Both fire the day the
engine changes, in either direction.

**Still uncovered on the codec dimension**: the codec arms are single-writer,
because every concurrency, Bolt and Cypher oracle in `internal/sim` is bound to
string keys through `cypher.Engine`. Concurrent-writer coverage for a non-string
key codec would require the engine itself to be generic, and is not reachable
from the harness as it stands.

### DDL across the snapshot boundary (rmp #2464)

Until sprint 347 every DDL-issuing scenario (`schema-chaos`, `constraint-enforce`,
`index-diversity`) ran **WAL-only**, so recovery always replayed the
`CREATE INDEX` / `CREATE CONSTRAINT` frames and the snapshot's schema components
(`store/snapshot/constraints.go`, `indexdefs.go`, `indexes.go`) were never the
source of a recovered index or constraint. The loss mode the checkpointer's
phase-3 self-sufficiency re-verification exists to prevent — truncating the WAL
prefix that first *declared* a constraint or an index (#1334 / #1464 / #1755) —
was therefore never exercised.

`ddl-checkpoint-crash` occupies that intersection directly, and
`constraint-enforce` and `index-diversity` now enable in-loop checkpointing so
their existing post-recovery oracles adjudicate a **snapshot-loaded** schema.
A `CheckpointConfig` is INERT unless the run loop calls `maybeCheckpoint`, which
only the default `Simulator.Run` does automatically; each custom loop wires the
call and each scenario carries a terminal gate asserting a **non-zero checkpoint
count**, so a configuration that stops taking effect fails the run rather than
passing quietly.

### Index CONTENTS across the snapshot boundary: hydrate or rebuild (rmp #2490)

#2464 brought the index **definitions** across the boundary. Their **contents**
stayed out of reach: a recovered index is populated either by DESERIALIZING the
snapshot's `indexes/<name>.bin` payload or by rebuilding from the recovered graph,
and the simulator built its engine through `cypher.NewEngineWithStoreAndSchema`,
which carries no payloads and therefore **always rebuilt**. Every
`indexes/<name>.bin` the DST published was written and never read. The
`index-diversity` scenario's own comment asserted recovery
"re-registers and re-backfills" as unconditional; it is now conditional, and the
comment is corrected.

The decision is per index, by name, and hydration requires all three of:

1. the snapshot was **self-sufficient** (it carried `mapper.bin`, so the payload's
   raw node ids still name the same nodes);
2. the payload is **readable and CRC-valid**;
3. **nothing the replayed WAL suffix committed touched that index's
   `(label, property)`** — the only precondition the engine, rather than recovery,
   can evaluate, because only the engine knows which pair a name covers.

Anything else is a rebuild. That is what makes the surface hard to test: a
hydrated index and a rebuilt one must, by contract, answer **identically**, so no
result-level oracle can tell them apart. The only sound instrument is the
engine-scoped population counter `cypher.Engine.RecoveredIndexPopulation`
(process-global `store.recovery.indexes.*` metrics cannot attribute a decision to
one reopen, and the swarm runner builds engines concurrently), asserted in **both**
directions with the answers verified independently on top.

`index-diversity` therefore drives **two constructed arms**, not two hoped-for
coincidences. Under its churn the WAL suffix carries a `:Person` write almost
every tick, so the refused branch is the outcome of nearly any crash and the
hydrate branch would need a crash landing on the very tick a checkpoint published,
before that tick's write. Waiting for that would let the crash schedule, not the
engine, decide whether the arm ran.

| arm | construction | required population | measured (seed `0x10DE5`, 3000-node short slice) |
|---|---|---|---|
| **hydrate** | forced crossing of the snapshot boundary (`crossSnapshotBoundaryOn`): checkpoint, measure the WAL emptied, crash, reopen — nothing committed in between | every registered index **HYDRATED**, none rebuilt, **zero** node references backfilled | `hydrated=6 rebuilt=0 registered=6 backfilled=0`, with the crossing measuring `WAL 853468 → 0 bytes, replayed 0 WAL ops, snapshot published=true` |
| **stale** | commit one `:Person {name, age, city}` on top of that snapshot, crash, reopen | every registered index **REBUILT**, none hydrated, and the post-checkpoint write reachable **through** those indexes | `hydrated=0 rebuilt=6 registered=6 backfilled=18006 walOps=5 probeVisible=true` |

Six, not three, because each of the three user indexes carries an internal
numeric companion; the assertion is anchored to `ListIndexes()` rather than to a
constant so it survives that set changing.

Two independent witnesses back each arm rather than one flag reporting on itself.
The hydrate arm reuses the #2468 forced crossing and adjudicates it with
`checkSnapshotSourcedRecovery`, which MEASURES the WAL going to zero and the
recovery replaying zero ops — the substrate-level reason hydration is permitted.
The stale arm requires `walOps > 0`, the substrate-level reason it is refused;
without a replayed op the payload would have been hydratable and "rebuilt" would
be the wrong expectation.

The stale arm's second half is the DST analogue of the staleness gate, and it is
what makes the first half matter. "Rebuilt" is the right answer only because the
payload describes a state the graph has left; the consequence of getting it wrong
is an index that silently omits every write committed after the checkpoint. So
each indexed property is queried through a predicate the planner serves **from
the index** — the leaf operator is asserted, because a query the planner scanned
instead would answer correctly even from a stale index and prove nothing — and the
answer must both equal an independent full-scan reference and contain the node the
payload cannot know about.

The in-loop crashes record which branch they took (`loop[reopens=3 hydrated=0
rebuilt=18]` on that slice) but assert neither: under churn the rebuild branch is
the correct answer for almost every one of them, and requiring either would put
the crash schedule back in charge of the verdict. The terminal gate requires both
CONSTRUCTED arms to have run, and is silent on a WAL-only configuration, which has
no snapshot to hydrate from — a coverage clause may only fail a run whose
precondition was constructed.

Neither arm touches `Simulator.checkpointCount` or `crashCount`, which is why
they call the store-level `crossSnapshotBoundaryOn` rather than the `Simulator`
method that wraps it. Those counters are what the #2457/#2464 checkpoint gates
assert on, and an arm that ran unconditionally and incremented them would satisfy
those gates by itself and silence exactly the defect they were written to catch.

### The label index's scoped and range surface, and what its CRC can reach (rmp #2496)

`graph/index/label/index.go` is 514 lines and all of it is public. The parts lpg
drives — `Add`, `Remove`, `Count`, `Has`, `Intersect`, `IntersectCardinality` —
are exercised by every DST run that touches a label. The rest was carried by its
godoc alone. VERIFIED by an `os.walk`+`re` sweep of every production
(non-`_test.go`) file in the tree (not by `grep`, which on the reference host
can return a silent empty result — an empty match is not evidence of absence):
`NewNodeIndex`, `NewEdgeIndex` and `Scope` have **no production caller**, and
`AddRange`, `RemoveRange`, `Scan`, `Union`, `Serialize` and `Deserialize` have
**no non-test caller anywhere**.

**The constructor rationale, established rather than assumed.** The task asked
why lpg builds both its indexes with the unscoped `NewIndex()`, and the answer
is sharper than "the scope does not matter here". `Scope` is read in exactly one
place, inside `Index.Apply`; `Apply` runs only through the `index.Manager`
fan-out (`graph/index/manager.go:254` and `:266`); and no `label.Index` is ever
registered with a manager. `Manager.CreateIndex` (`manager.go:166`) is the sole
writer of the subscriber registry, and every one of its twelve production call
sites — `cypher/index_binding.go:711,714,731,734,788,890,900`,
`cypher/api.go:1728,3273,3289`, `cypher/exec/create_index.go:107`,
`cypher/exec/create_constraint.go:125` — registers a `hash` or `btree` index.
Only three production packages import `graph/index/label` at all (`cypher`,
`cypher/exec`, `graph/lpg`, VERIFIED through `go list`), and two of them only
read an index lpg owns. A second, structural check agrees without reading a
single return type: only **four** production files name the package —
`graph/lpg/lpg.go`, which constructs both indexes, `cypher/exec/scan_label.go`
and `cypher/stats_estimate.go`, which take one as a read source, and
`cypher/api.go`, whose two mentions are doc comments — and not one of the twelve
`CreateIndex` call sites is in any of them. So `NewIndex()` is correct for both
because the field the other constructors set is never read on a directly-driven
index.

The sharper half is that `NewEdgeIndex()` would not merely be inert — it would
be **wrong** if it ever took effect. `index.OpAddEdgeLabel` **is** constructed
in production (`cypher/api.go:17890` and `:18804`) and **is** delivered
(`exec.IndexBuffer.Commit` calls `Manager.ApplyBatch` at
`cypher/exec/index_writeback.go:45`); it is simply discarded by every registered
subscriber, since `hash` and `btree` handle only the four node ops. Its partner
`index.OpRemoveEdgeLabel` is constructed **nowhere** — all four production
mentions are the enum declaration, the `IsEdgeChange` switch, one doc comment
and the `Apply` case that consumes it. A registered `ScopeEdge` label index
would therefore take every edge-label addition and never one removal,
accumulating stale postings for as long as the process ran. That is now recorded
in the `graph/index/label` package documentation, on `NewEdgeIndex` and on
`Scope`.

**Overlap and adjacency are CONSTRUCTED, not drawn.** The inclusive-to-exclusive
`+1` conversions at `graph/index/nodeset.go:339` and `:378` are where an
off-by-one would live, and a drawn pair of endpoints reaches an exact adjacency
(`[a,b]` then `[b+1,c]`) only by luck. Thirteen relationships — disjoint,
adjacent on each side, partial overlap on each side, contained, containing,
identical, single-element inside and outside, inverted, and the off-by-one empty
range — are swept in BOTH directions, twenty-six cells per epoch, in a
seed-shuffled order, each cell driven twice: once on a fresh label carrying only
the base band (so a mismatch is attributable to the relationship) and once on a
label from a small reused pool that already holds a long history. The seed
decides the order and the band; it never decides the coverage, and a gate names
any cell that went undriven. `TestLabelIndexScoped_RelBoundsMatchTheirNames`
asserts each relationship really has the geometry its name claims, because a
mislabelled "adjacent" that quietly overlapped would delete the coverage while
every clause still passed.

**The model is naive, independent, and itself tested.** A plain
`map[uint32]map[uint64]bool` recomputed from the op stream, whose range methods
walk the closed interval one id at a time. It knows nothing about roaring and
nothing about `NodeSet`'s tier machine, and it never asks the index what the
answer should be. It deliberately does **not** model when the index deletes a
label's map entry: that would be a second copy of `nodeset.go` agreeing with the
original by construction. The one entry-population clause is therefore tier-free
and one-sided — the image must declare at least as many labels as the model says
carry a member — and the excess is reported as a measured number.
`TestLabelIndexScoped_ModelIsIndependent` pins the model's own answers on
hand-computed adjacency, overlap and inverted cases, because an unchecked oracle
is an assumption with extra steps.

**What the corruption arm can and cannot reach.** The serialized form ends in a
CRC32C over every preceding byte, so a raw single-byte flip is caught by the
checksum wherever it lands — MEASURED across all seven layout regions in the
short layer, and under soak across **every one of the 143 byte offsets** of a
small image, all 143 answered by the CRC and every one leaving a populated
receiver byte-for-byte unchanged. That is the whole detectable population for a
bad sector, and it means the four structural guards inside `Deserialize` are
**unreachable by corruption alone**. To reach them the image must be damaged
*and* its trailer recomputed, so a second family does exactly that and requires
each trial to reach its NAMED guard: bad magic, unsupported version, implausible
bitmap length, bitmap parse, and the truncated-entry read a too-high
`labelCount` produces. A third family reads the image short, which reaches a
different branch again — below four bytes the reader answers "short payload"
before any CRC arithmetic. Guards are classified by WHOLE distinctive phrases
rather than single words, because "bitmap" appears in two of them, and a message
matching two needles comes back unclassified rather than approximated
(`TestLabelIndexScoped_GuardClassifierIsExact`).

The re-stamped family also records where the format has no redundancy: a
re-stamped `labelCount` of **0** is ACCEPTED and yields an empty index, and the
body's remaining bytes are never examined. That is not a defect against CRC32C's
contract — an error-detecting code is not a MAC, and an adversary who can
recompute the trailer is outside its threat model — but it does mean nothing
ties `labelCount` to the payload length, and a structural "the body was fully
consumed" check would cost nothing and does not exist.

**Two clauses are TRIPWIRES on their production path, and say so rather than
posing as detectors.** `corrupt-restore` cannot fire against the current
`Deserialize`: it reads the whole payload, validates the trailer, parses into a
FRESH map and only then takes the write lock and swaps, so every error path
returns before the receiver is touched — untouched by construction, not by good
behaviour. It is kept because the swap is exactly what a plausible optimisation
would remove (parsing straight into the live map to save an allocation), which
would leave a refused image half-applied; its logic is proved fireable by a
perturbation. And `corrupt-refusal` on the raw family does not detect a CRC
collision — CRC32C detects every single-byte error — it detects the CRC CHECK
being weakened, reordered after the parse, or removed. Both are real regressions
worth catching; neither is the thing the clause name suggests, and a clause that
looks like a detector and cannot fail is worse than no clause.

**An over-strong assertion of the harness's own, corrected before it shipped.**
The tier arm's first draft compared an `Add`-built index with an
`AddRange`-built one and required byte-identity. It FAILED at widths 4 and
above, and the failure was the harness's fault: `Serialize`'s godoc promises the
form is deterministic "for a given in-memory state", never that it is a function
of the logical contents. What the module actually claims
(`graph/index/label/index.go:407-415`) is that the INLINE tier serializes like a
bitmap holding the same ids — the claim that made #1585 a zero-migration change
— and that HOLDS, verified at six widths with the bitmap side reached by
growth-then-trim (individual `Add`s past `smallSetMax`, then `Remove`s back
down, exploiting the documented one-way promotion). The `Add`-versus-`AddRange`
difference is kept as a MEASUREMENT with no clause on it, because the
consequence is easy to assume away and is real: two indexes that answer every
query identically can have different images, so byte-comparing two snapshots is
not a valid way to ask whether two graphs carry the same labels.

The same mistake nearly reached the round-trip clause, and finding it is what
produced defect #17 above. `image == round-tripped image` is FALSE for a
run-encoded label of 4 to 8 ids, and the sweep can reach exactly that shape — an
`AddRange` followed by a `RemoveRange` that trims the label to a handful — so
the clause would have passed on the catalogue seed and flaked on another. What
is asserted instead is that the form is a **fixpoint after at most one cycle**,
which is true at every width probed; whether the FIRST cycle was stable is
recorded as a number, and the instability is pinned separately with a control
one id above the threshold.

**Coverage this adds.** Thirteen relationships x two directions x two label
shapes adjudicated against the model on `Scan`, `Count` and `Has` after every
operation (2 496 range operations and 1 875 comparisons at the catalogue seed,
41 600 and 31 203 under the long soak arm); `Union` against a per-label naive
union across multi-label, unknown-label, duplicate-label and empty subsets, all
four gated as reached; the serialized form round-tripped twice and compared
against the model at both, its emission proved deterministic, and its
tier-independence claim verified at six widths; three damage families on a
`SimDisk`; the three constructors' scopes and the twelve-row `Apply` routing
table that is the only place a scope is observable; and three latent defects
pinned to their measured behaviour. Twenty-five perturbations each fire their
named clause with the unperturbed control silent, and every one of the fifteen
non-vacuity gates is proved fireable by a knockout — including four that no
configuration knob can reach, which would otherwise have been gates nobody had
shown could fail.

### Conjunctive index intersection and its budgeted gate (rmp #2490)

The planner composes two single-property indexes on the same label into one
access path by ANDing their range bitmaps (`cypher/index_intersect_plan.go`,
#2134), reaching that decision through a **budgeted** cardinality count per
conjunct (#2266): `RangeCountFrom` for a string conjunct with no upper bound,
`RangeCount` for one with both. None of it was reachable under the DST, because
no scenario ever issued a two-predicate conjunction over two btree-indexed
properties of one label — every indexed probe the simulator drove constrained a
**single** property, which is the shipped single-property range seek and a
different code path.

`index-diversity` is the scenario that can reach it: it declares three indexes on
`:Person`, two of them BTREE (`age` numeric, `city` string). The hash index on
`name` deliberately cannot participate, since the recogniser requires a bound
BTREE per conjunct.

| arm | predicate | drives | order claim |
|---|---|---|---|
| `intersect-range-count-from` | `n.age >= A AND n.city >= 'c9k'` | the unbounded-above string branch (`RangeCountFrom`) | **yes** — see below |
| `intersect-range-count` | `n.age >= A AND n.city STARTS WITH 'cNN'` | the bounded string branch (`RangeCount`) | no |
| `solo-control` | `n.city >= 'c9k'` | the single-property range seek | n/a |

Each composed arm runs in its literal and its parameterised spelling, is
result-verified as an id-multiset against one plain label scan filtered
client-side, and must retain its residual `Filter` — each part is only a SUPERSET
of its conjunct (#F-EXEC1), so the exact predicate must still be re-applied per
row.

Three details are load-bearing:

* **The composed marker is specific.** A composition renders one `∩ range=` per
  index beyond the primary; the multi-label conjunction joins its labels with a
  bare `∩` and no `range=`. The composition reuses
  `exec.NodeByIndexRangeScan`, so the operator *name* cannot distinguish it —
  which is why the `solo-control` arm exists. It must seek through that very
  operator and still render no `∩ range=`, or nothing establishes that the marker
  discriminates a composition rather than "some index was used".
* **The rendered string bound identifies the SHAPE, not the function body.**
  `"c95"..+inf` is rendered only when the extracted upper bound is nil, which is
  exactly the condition `budgetedStringRangeCount` switches on to call
  `RangeCountFrom`. So the fragment proves the arm drove that branch of the gate.
  It does **not** pin the branch's body: rewriting `RangeCountFrom(lo, budget)` as
  `RangeCount(lo, "\xff\xff\xff\xff", budget)` was measured to change nothing
  observable, because every key in this fixture sorts below that sentinel. Such a
  rewrite is an *equivalent mutant* and no result- or plan-level oracle can see
  it; the counting functions themselves are pinned by
  `graph/index/btree/range_from_test.go`.
* **The AND order is the only observable a counted VALUE has.** The parts are
  ANDed in ascending exact count, ties broken on the property key, and nothing
  else in the plan or the answers depends on what the gate counted. The
  unbounded-above arm therefore predicts the order from the two cardinalities it
  derived client-side — both of its bounds are closed below and unbounded above,
  so neither count is a superset — and requires the plan to agree. Counting one
  city bucket instead of the tail (`RangeCount(lo, lo, budget)`) is invisible to
  every result and every bound, and inverts this order. The bounded arm makes no
  order claim: the planner counts a prefix over the CLOSED interval
  `[prefix, prefix+1]`, which on this data spans the next city value too, so
  predicting it client-side would mean re-implementing the operator's superset
  semantics instead of observing it.

The windows are drawn from the checker's own sub-seed (`intersectSeedMix`) so the
workload, crash, parity, seek-result and statistics streams stay byte-identical,
and sized to keep the string side comfortably the larger conjunct (at least
1.67×), which is what keeps an under-count detectable rather than seed-dependent.
`NewIndexIntersectProbes` **panics** on a fixture below the planner's 1024-node
label-population floor plus margin, rather than producing arms that assert a
composition which cannot happen.

Measured on the 3000-node short slice: `composed=18 withRows=18 soloSeeks=9`. The
terminal gate requires all three to be non-zero.

Half-open `>= lo` **single-property** range probes are deliberately NOT duplicated
here: rmp #2450 already ships them in `IndexSeekResults`, with the floor sized at
~9% selectivity precisely so `RangeFrom` / `RangeCountFrom` engage.

### Group-commit coalescing and fail-all (rmp #2471)

This section previously stated that every write commit — including its WAL
`fsync` — is serialised under a single `visMu` lock, so `SyncGroup` was "always a
solo leader with zero followers" and multi-member coalescing was unreachable
through the engine. **That is false, and has been since sprint 334 made MVCC the
module's concurrency control.** An ordinary write takes the barrier SHARED
(`cypher/api.go`, `Engine.schemaGate.WeakLockAuto`), so two commits run
concurrently by design and their fsyncs coalesce.

The correction is measured, not argued. Driving 12 concurrent Bolt writer
connections × 40 commits through a real WAL-backed store on a `SimDisk`
(`RunGroupCommitCoalescing`, `internal/sim/group_commit.go`):

| committers | SyncGroup rounds | leaders | followers | acked commits |
|---|---|---|---|---|
| 12 | 483 | 422 | **61** | 480 |
| 1 (control) | 43 | 43 | **0** | 40 |

Both properties are now gated in the DST rather than recorded in a comment:

- **Coalescing** is a coverage precondition. `checkGroupCommitCoverage` fails the
  run if `store.wal.SyncGroup.coalesced` is zero under ≥ 8 concurrent
  committers — the signature of a regression to solo-leader commits, which halves
  write throughput and un-covers the fail-all branch entirely while leaving every
  other scenario green. `checkGroupCommitNonVacuity` is a **separate** gate, so a
  run that simply failed to commit is reported as uninformative rather than as a
  writer regression. The single-committer control arm is retained as a permanent
  sensitivity proof: it must read zero followers, which also keeps the
  `coalesced` counter honest (it is shared with `SyncBuffered`'s
  durable-already path).
- **Fail-all** is asserted end to end by `RunGroupCommitFailAll`. It builds a
  genuine 8-member group — `SimDisk.ArmSyncGateAt` holds the leader inside its
  fsync while the followers arrive, which is the only way the group is
  deterministic rather than lucky — fails that one shared fsync, and asserts every
  member receives the durability fail-stop (`wal.ErrDurabilityFailed`), that
  **none** is acknowledged, that exactly **one** round was poisoned, and that
  recovery keeps the commit acknowledged before the group while discarding the
  whole failed group.

The store-layer unit tests (`store/wal/syncgroup_test.go`,
`store/txn/group_commit_durability_test.go`) remain the arithmetic gate. What the
DST adds is a group whose membership is constructed rather than assumed: the WAL
unit test fails *every* fsync, so it cannot distinguish one shared round from N
serialised ones.

### The WAL writer surface: watermark, frame contiguity, truncate, guards (rmp #2472)

Most of what `store/wal.Writer` **exports** was invisible to the simulator.
`Stats`, `DurableOffset`, `Poisoned` and `SyncBuffered` were never read by any
scenario, and the whole-file `Truncate` — as distinct from `TruncatePrefix`, which
the checkpoint path drives — was never called. Each states a contract the rest of
the store depends on: the checkpointer picks its WAL cut point from
`DurableOffset` and aborts on `Poisoned`, and the txn layer's empty commit resolves
through `SyncBuffered`. `internal/sim/wal_writer_surface.go` adjudicates all of
them.

**The durability watermark** is observed after *every* acknowledged commit, and
what "expected" means is split in two, because only one form is available to every
caller:

- **Exact**, when the harness chose the payloads (`RunWALWatermarkDirect`): the
  durable offset must equal the watermark `AppendRun` returned for that commit, and
  `wal.Stats` must equal `SUM(wal.HeaderSize + len(payload))` over every frame
  emitted. Measured: 12 commits, 24 frames, `durable == appended == boundary ==
  imageLen == 656`.
- **Relative**, when the payloads are the engine's (`RunWALWatermarkEngine`): every
  counter is monotonic, the durable offset never exceeds the bytes accepted, and it
  lands on a **frame boundary** of the durable image — the accumulation over some
  whole number of leading complete frames. Measured: 8 commits, 32 frames, final
  offset 1456.

The relative form deliberately asserts **no absolute size**. rmp #2521 measured
that the durable image varies with process wall-clock time, because a commit marker
encodes the instant it was written; an oracle pinning a byte count would be pinning
the clock. The frame-boundary relation is derived from the frames actually on disk,
so it is invariant under that variation and is still exactly the invariant
`DurableOffset` documents.

**Per-transaction frame contiguity** is the claim `AppendRun` makes *by
construction* — it holds `w.mu` across a whole transaction's frames — and that
claim is only testable under concurrency. Before commit `9eee3b18` the contiguity
came from the store's single-writer semaphore two layers up, and `store/recovery`
discards a transaction's buffered prefix as orphaned on the stated ground that
frames never interleave, so an interleaved image makes recovery drop **committed**
ops. `RunWALContiguity` drives 8 concurrent committers × 12 transactions × 4 frames
through the real writer, then partitions the durable image by payload tag — what is
physically on disk, not what any counter claims:

| append path | frames | transactions | maximal runs | split transactions | worst fragments |
|---|---|---|---|---|---|
| `AppendRun` (one call per transaction) | 384 | 96 | **96** | **0** | 1 |

**The claim holds**, and it is asserted **unconditionally** — a split is a defect
however little concurrency produced it.

**The evidence that attributes contiguity to `AppendRun` is CONSTRUCTED, not
raced.** The first version of the control drove eight committers concurrently and
asserted that per-frame `Append` produced at least one split. It did on an idle
machine — 31 of 96 transactions — and then failed under `make ci`'s coverage step
with the suite running in parallel, where all four retries measured
`committerSwitches=7, split=0`: the scheduler simply never overlapped the
committers. **An assertion on a scheduling outcome measures the machine, not the
module** (the defect class filed as rmp #2517), and raising the retry count would
have traded a red gate for a slow one while still measuring the scheduler.

`RunWALContiguityAlternating` replaces it with a handoff protocol. Two committers
pass a token, so exactly one is eligible to append at any instant and the durable
image is determined by the protocol. Both modes run the **same** protocol; only the
append API changes, so the difference is attributable to the API alone:

| mode | frames | transactions | maximal runs | split | worst fragments | committer switches |
|---|---|---|---|---|---|---|
| per-frame `Append`, strict alternation | 8 | 2 | **8** | **2** | 4 | 7 |
| `AppendRun`, same handoff | 8 | 2 | **2** | **0** | 1 | 1 |

Per-frame `Append` releases the writer mutex between frames, so the partner's frame
really does land in the middle and each transaction ends up in four fragments —
a genuinely interleaved image from the real writer, reproducible byte for byte.
Under `AppendRun` the partner signals from *inside* the run and still cannot get
in, because the run holds the mutex throughout; that ordering is decided by the
**mutex, not the scheduler**, so it is equally deterministic. The signal is one-way
on purpose: a full ping-pong would deadlock there, and that deadlock is the
mechanism under test. The **whole layout** is asserted, not just "at least one
split", so a lost frame or a skipped handoff changes a number and is caught.

Verified in the condition that broke the original: under `GOMAXPROCS=1` with
coverage instrumentation — where the concurrent arm reproduces
`committerSwitches=7` exactly — five consecutive runs produced **byte-identical**
layouts for both alternating modes and exited 0.

Three gates are kept apart:

- **Non-vacuity** rejects shapes that would make the census worthless: fewer than 8
  committers, a **single frame per transaction** (contiguous by definition), or a
  transaction missing or short of frames.
- **The verdict** (`checkWALContiguity`) is asserted unconditionally.
- **The concurrency witness** (`checkWALContiguityConcurrencyWitness`) reports
  whether the machine actually granted concurrency. A shortfall is logged as
  **UNINFORMATIVE, never as a failure**, because it measures the scheduler; it
  never excuses a split, and the deterministic proof does not depend on it.

**`Truncate` and the poisoned writer** are pinned by `RunWALLifecycle`. The
documented parts hold: `Truncate` returns the bytes that were in the file, zeroes
the watermark, leaves the **lifetime** counters untouched, and the next append
restarts at offset 0 of the empty file; a poisoned writer reports a stable sticky
error carrying `wal.ErrDurabilityFailed`, rejects every append with that same value,
fails a `SyncGroup` for a watermark the poison discarded, and — per rmp #2322 —
still returns nil for a watermark that was already durable. Two members of the
contract were **undocumented and are pinned as measured**:

- **`SyncBuffered` on a poisoned writer returns nil.** The poison rewinds the
  accepted offset to the durable one, so "make everything accepted durable" is
  already satisfied and the durable-already fast path fires. It is correct —
  nothing accepted is un-durable — but it means `SyncBuffered` is **not** a health
  probe; `Poisoned` is.
- **`Truncate` on a poisoned writer succeeds** and empties the file, while the
  writer stays poisoned. `Truncate` is the one mutator that does not consult the
  sticky error. It is not a durability hole — the writer still refuses every
  append, so nothing can be written after the emptied file — and `Truncate` is
  documented as a maintenance helper off the production checkpoint path, which cuts
  the WAL with `TruncatePrefix` instead. It is pinned so a change putting `Truncate`
  on a live path is caught rather than absorbed.

Also measured: after `Close`, `Append` and `Truncate` return `wal.ErrWriterClosed`
rather than the poison (the closed check precedes the poison check), while
`Poisoned` still reports why the handle died.

**`ErrWALLocked` and the `O_NOFOLLOW` refusal remain unreachable through
`SimDisk`, and that is stated rather than papered over.** `SimDisk` is a flat
in-memory key table with no inodes, links or advisory locks, so a flock and a
symlink cannot exist in it. Of the two honest routes — grow `SimDisk` a
lock-and-symlink model, or drive the two opens against a real directory —
`RunWALRealFSGuards` takes the second, on the ground that a modelled flock would
prove the model while a real second `wal.Open` exercises the syscall the guard is
made of. (`os.MkdirTemp` where the SimDisk seam does not exist is already
precedent, rmp #2466.) It asserts a second `wal.Open` of a locked path returns
`wal.ErrWALLocked` — `flock(2)` binds to the open file description, so the two
opens conflict within one process and no subprocess is needed — that closing the
first releases it, and that a symlinked final component is refused at **both**
`O_NOFOLLOW` sites: the WAL data file and the `LOCK` sentinel, which is opened
before any WAL data is touched. The victim file outside the directory is verified
byte-unchanged, which is the property that actually matters (CWE-59, security
finding #1843). The consequence to record: **these two guards sit outside every
seeded, crash-injecting scenario in this package, and will while `SimDisk` has no
link or lock semantics.** The adjudicator makes no claim at all on a platform that
cannot express them, so a skip never reads as a pass.

Every gate here is proved falsifiable as well as satisfied — a doctored record for
each clause, plus the live per-frame control for contiguity.

### Transaction-size caps: producer refusal and replay fail-stop (rmp #2474)

The store bounds a single transaction on **both** sides, and for one reason:
recovery buffers a whole transaction's ops in memory before applying them on its
`OpCommit` marker, so a producer able to commit an arbitrarily large transaction
could write a WAL that recovery cannot replay without allocating in proportion to
it. That is the CWE-770 shape of the persistence layer. Two typed sentinels
answer it:

| Bound | Default | Refusal |
|---|---|---|
| Producer (`txn.Tx.appendOnly`) | `txn.DefaultMaxTxnOps` = 16 000 000 | `txn.ErrTransactionTooLarge`, **before** a sequence is minted or a frame written |
| Replay (`recovery.Options.MaxTxnOps`) | the same value | `recovery.ErrTransactionTooLarge`, classified by `tailErrIsCorruption` as genuine corruption |

**Neither sentinel had ever been produced under simulation, and the workloads
were not the reason.** `simStoreConfig.maxTxnOps` was plumbed carefully — through
`recovery.OpenFS` on the full-stack path and `recovery.ReplayWAL` on the WAL-only
one — and reached **recovery and nothing else**: the store itself was built with
the uncapped `txn.NewStoreWithOptions`, so the replay bound was configurable and
the commit bound was not. Lowering the cap could never make the producer refuse,
and reaching 16 000 000 ops by workload is not a test but an out-of-memory. The
`overload` actor's own comment records the gap: it pushes "toward"
`DefaultMaxTxnOps`, and nothing ever arrived. `simstore.go` now passes the cap to
`txn.NewStoreWithOptionsCapped` as well; the change is behaviour-neutral for
every existing caller, all of which carry `0` and resolve to the same default as
before.

**A typed error is not the assertion.** A refusal that appended frames and then
truncated them is how a bug becomes permanent loss (rmp #2526), so the producer
arm reads the whole durable WAL image off the `SimDisk` before and after each
refused attempt and requires them **byte-identical**, and reads the live graph's
node count across the same boundary. Measured at a deliberately small cap of 32
ops:

| attempt | ops | outcome | WAL bytes | live order |
|---|---|---|---|---|
| `warmup` | 8 | committed | 0 → 436 | 0 → 4 |
| `one-over` | 33 | **refused**, `txn.ErrTransactionTooLarge` | 436 → 436, **identical** | 4 → 4 |
| `far-over` | 128 | **refused**, same sentinel | 436 → 436, **identical** | 4 → 4 |
| `at-cap` | 32 | committed | 436 → 2084 | 4 → 20 |

The at-cap transaction is driven **after** the refusals deliberately: a refusal
that had silently poisoned the writer fails there rather than passing as a clean
refusal. The reopen then recovers clean and equal to the model, with all 80 keys
of the two refused transactions absent.

**The boundary is measured, not inferred from the two comparisons.** The producer
refuses when `len(ops) > cap`; recovery stops when, before appending another
frame, `len(pending) >= cap`. The operators differ because the counts are taken
at different moments, and the arms pin what that actually yields: **32 ops
commits and replays, 33 does neither** — the two caps agree exactly, which is the
`producer <= replay` invariant `txn.DefaultMaxTxnOps` documents.

**The oversize WAL is built by hand, because the engine cannot produce one.**
The producer cap is `<=` the replay cap by construction, so any transaction large
enough to trouble recovery is refused before a frame is written — that is the
whole point of the pairing. The file is therefore constructed one v3 op payload
at a time and written through the real `wal.Writer`, so only the op stream is
hand-made and the framing, CRC and fsync are the production ones. Measured with a
replay cap of 16 over a 4-op committed prefix:

| arm | run ops | cap | outcome | ops applied |
|---|---|---|---|---|
| `at-cap` | 16 | 16 | clean | 20 (prefix + run) |
| `over-cap` | 17 | 16 | **fail-stop**, `recovery.ErrTransactionTooLarge` | 4 — exactly the committed prefix |
| `over-cap-unlimited` | 17 | unlimited | clean | 21 |

The harness store-open is checked alongside recovery's own report: an embedder
that swallowed the fail-stop would append onto the corruption and embed it
permanently, so the `over-cap` image must be **refused** by `openSimTypedStore`
with the sentinel intact, and the two clean arms must not be.

**Three sensitivity seams pin the oracles, and two drive the real defect rather
than fabricated evidence.**

- `txn.MaxTxnOpsUnlimited` runs the byte-identical producer plan and commits all
  four transactions, so the capped arm's refusals are attributable to the cap and
  not to those op counts being rejected for some reason of their own.
- The `over-cap-unlimited` replay arm replays the **byte-identical** 954-byte file
  with the cap disabled and recovers all 21 ops, which rules out a file the
  harness simply built wrong. It doubles as the standing proof that the
  hand-written v3 payload layout matches `store/txn`'s unexported encoder: a wrong
  version tag, kind, sequence width or body layout could not decode into exactly
  the nodes the frames name.
- `simStoreConfig.uncappedProducerSeam` restores the pre-#2474 plumbing — the cap
  reaching the replayer and not the producer — and **measures the hazard the
  invariant exists to prevent**, which is worse than a missed refusal: the 33-op
  transaction is acknowledged durable, and recovery then refuses to replay the
  file at all. The store does not lose that transaction; it **fails to reopen**,
  and every committed transaction in the WAL becomes unreachable behind a
  fail-stop.

The non-vacuity gate is **separate and shape-only**, so an uninformative run never
reads as a faulty one: it requires an attempt genuinely larger than the reference
cap, a **non-empty** WAL underneath it — a byte-unchanged assertion over an absent
file is satisfied by definition — and at least one transaction actually
committed. Every clause of both verdicts and both non-vacuity gates is proved
falsifiable by perturbing a hand-built control one field at a time, with the
unperturbed control silent.


### csrfile access patterns, weight kinds, and what truncation does NOT reach (rmp #2478)

The csrfile arm published **one** fixture before this task: a `float64`-weighted
CSR read back through the default access pattern. Four of the five
`csrfile.WeightKind` values were therefore never written by the simulator,
`csrfile.AccessPattern` and `Reader.SetHint` were never called at all, and
`csrfile.Reinterpret`'s alignment precondition was never probed. The arm now
enumerates the whole 5 x 5 grid. Five things it measured are worth recording,
because three of them contradict what the surface suggests.

**There are five access patterns, not three.** `AccessDefault`,
`AccessSequential`, `AccessRandom`, `AccessWillNeed` and `AccessDontNeed`.
`store/csrfile`'s own `TestReader_SetHint` drives four of them; `AccessDontNeed`
was reached by no test in the repository before this task, and it is the one that
makes "a hint does not change the data" a real question rather than a formality —
on a live mapping it tells the kernel to drop the resident pages, so the read that
follows must fault them back in and yield the same bytes.

**The in-memory disk cannot reach madvise, and a green matrix over it is not
evidence that it did.** `Reader.SetHint` short-circuits when there is no mapping
to advise, and the DST filesystem seam produces exactly that: `csrfile.OpenWith`
over a non-OS backend reads the whole image into a heap buffer and leaves the
Reader's `mm` nil, so `SetHint` returns nil **before** the platform call. Every
cell of the in-memory grid therefore proves the CONTRACT — a hint is accepted on a
live reader, refused with `ErrReaderClosed` on a closed one, and alters no byte —
and none of them proves the syscall ran. The madvise path is reachable only
through `csrfile.Open`, which always mmaps; one test
(`TestCSRFileMatrix_MadviseOverRealMapping`) drives all five patterns against a
real temp directory for that reason. A second measured fact belongs with it: an
**out-of-range** `AccessPattern` is not rejected — the switch falls through to
`MADV_NORMAL` and `SetHint` returns nil — so `SetHint` validates nothing.

**An aligned truncation and a misaligned one behave identically, and the reason is
structural.** `Header.validate` compares the file's length against the ONE
canonical layout for its counts and demands EXACT equality, so alignment never
enters the decision. Measured on a 964-byte file: a cut at 128 (a multiple of
`Alignment` = 64), a cut at 277 (a multiple of neither 64 nor 8), a cut at the
edges offset, a cut at the CRC offset and a cut one byte short all produce the
same `ErrHeaderInconsistent`, which wraps `ErrFileCorrupted`. The only threshold
that changes the answer is `HeaderSize+4`: **67 bytes gives `ErrHeaderTooShort`
and 68 gives `ErrHeaderInconsistent`**, because below 68 the length gate fires
before the header is decoded. The two backends diverge at exactly one length —
zero. The byte-backed reader reports `ErrHeaderTooShort`; the mmap path fails
earlier, because `mmap(2)` refuses a zero-length mapping, and surfaces an
**untyped** wrapped syscall error that no `errors.Is` against a package sentinel
will match.

**`Reinterpret` refuses by PANIC, and truncation cannot reach its alignment
half.** There is no error return: a short buffer, a negative `n` and an `n` whose
byte requirement overflows all panic, and the refusal must be caught with
`recover`. More importantly, its alignment precondition is on the **base address**
of the buffer, which truncating a file cannot change — truncation only ever trips
the LENGTH half. The alignment gate is therefore probed the only way it can be:
by sweeping all eight byte phases of a buffer, of which exactly one is 8-byte
aligned. The measurement is 1 accepted, 7 refused with "base address not aligned
to 8 bytes". `n == 0` is the documented non-refusal — it returns nil without
inspecting the buffer at all — so an alignment probe written at `n = 0` would
prove nothing.

**`WeightAbsent` is distinguishable from a weighted file on four independent
signals, and collapses in exactly one case.** Over one shared topology the
unweighted file differs in its header kind, in `WeightsRaw()` being nil, in
`WeightsOffset` being 0, and — the signal that owes nothing to any header field —
in being strictly smaller (measured: 580 bytes unweighted, 708 with a 4-byte
weight, 836 with an 8-byte one). The collapse is the csrfile-side shape of what
rmp #2526 fixed in the snapshot: a CSR declared at a weighted Go type but
carrying an **empty weights slice at runtime** is downgraded by the writer and
lands on disk **byte-identical** to a graph that never had weights. That is
pinned rather than tolerated silently, and it is also what makes the coverage
gate honest — the gate counts which kinds were OBSERVED in the published headers,
so driving the `float64` arm with an empty weights slice registers as `float64`
**unreached**, not as `float64` covered.

One thing found here is out of this task's scope and filed as **rmp #2529**:
`weightKindOf` advertises `int`, `uint` and `uintptr` as `WeightUint64`, but
`binary.Write` refuses them ("some values are not fixed-sized in type `[]int`"),
so a publish at those types always fails — cleanly, leaving neither the file nor
the temp behind, but failing. The arm table stays on the four widths that
round-trip, plus `struct{}`.

Every verdict here is proved falsifiable by a control that drives it red: the
round-trip check is run against a DIFFERENT topology and against the same
topology with one weight value changed (so it cannot be passing on length
alone), the truncation check is run against a file that was not truncated (which
makes all three of its clauses fire at once), the size-discrimination check is
fed collapsed sizes, and the alignment sweep is fed both a gate that refuses
nothing and one whose refusals come from the length check.


### Which counter proves the fault fired (rmp #2479)

A fault scenario that asserts the **effect** of a fault without confirming the
faulted code path was **entered** proves less than it appears to. rmp #2465 had
to establish that the mid-publish crash window was genuinely entered before its
durability verdict meant anything; rmp #2471 gated group-commit coalescing on a
metric and then found the metric itself could be satisfied by an unrelated path;
rmp #2478 found the in-memory backend silently skips `madvise`, so a green matrix
would have been evidence of nothing.

The simulator's metrics oracle read **four** counters, all from the Cypher layer:
`cypher.Run`, `cypher.RunInTx` and their paired `.errors`. Every storage- and
Bolt-layer metric emitted across the module was unasserted — so the counter that
would prove a fault fired was precisely the one nothing read.
`internal/sim/metrics_required.go` closes that. Each fault scenario now
**declares** the counters its faults must move, and failing to move a declared
counter is a violation: a coverage precondition in the shape rmp #2470/#2471
established, kept apart from the scenario's own verdict because a declaration
that did not fire means the **run** proved nothing, not that the **engine** is
broken.

**Every name was driven out, not read off a list.** `docs/metrics.md` carries an
inventory of wired metric names and nothing here was taken from it. Each
scenario was run with a recording sink installed and the arriving names were
read; the floors are the structural counts the scenario fixes (one per publish
window, one per corrupted component), not the total one run happened to produce.
Two of the declarations would have been wrong if written from the obvious guess:
the cadence arm never moves `store.checkpoint.RunCheckpoint.errors` at all,
because the cadence environment drives the checkpointer through its own fold
callback, and the csrfile arm moves nothing whatsoever.

**`store.wal.Decode.errors` is the trap, and it is not hypothetical.**
`store/wal/format.go` increments it on **every** decode failure class, including
the `io.EOF` / `io.ErrUnexpectedEOF` path that yields `wal.ErrTornFrame` — the
ordinary shape of a WAL whose writer was killed mid-write, with no corruption
anywhere. It is the only fault counter the WAL-corruption arm emits, so declaring
it alone would be satisfied by a benign crash tail. The discriminator is a
**control**, the standing guard rmp #2471 kept for the same reason:
`runWALCorruptionFailStopWith` runs the identical scenario with the interior byte
flip withheld — same commits, same clean close, same clean and prefix replays,
same reopen — and the control requires the counter to stay at **0**. Measured: 2
with the flip, 0 without.

**The csrfile publish arm is metrics-blind, and the blindness is now asserted.**
`store/csrfile` contains no reference to `internal/metrics`, and a full driven run
of the scenario emitted **zero** metric names of any kind. The atomic publish
protocol (tmp write -> fsync -> rename -> parent-dir fsync) is entirely
unobserved: no counter can witness the ENOSPC bound or the armed `Sync` fault,
and borrowing one from a neighbouring layer would be exactly the non-unique
declaration this task exists to prevent. So the declaration states the blindness
and pins it — no name under `store.` may be emitted while the arm runs — which
turns "we declared nothing" from a vacuous pass into a falsifiable claim. The day
csrfile gains a counter, the declaration fails and must be replaced by the real
name.

**Eight of the nine snapshot components carry a unique witness; the mapper does
not.** The aggregates (`store.recovery.OpenCtxFS.errors`,
`store.recovery.openCodec.errors`, `store.snapshot.LoadSnapshotFull.errors`) move
for **any** reopen failure, WAL-side included, so none of them can say the damage
was seen where it was done. The per-component decoder counters can, and
`ReadCSR`, `ReadLabels`, `ReadProperties`, `ReadTombstones`, `ReadEdgeHandles`,
`ReadConstraints`, `ReadIndexDefs` and `ReadManifestFile` all move on their own
arm. `store.snapshot.ReadMapperString.errors` does not: the mapper arm's damage
is caught before that decoder is reached, so the mapper is witnessed only by the
aggregates. That is recorded rather than papered over, and logged as a witness on
every run.

The declared arms, with the counters as **observed**:

| Scenario / arm | Required counters | Uniqueness |
|---|---|---|
| `csrfile-publish-fault` | *(none — metrics-blind; `store.` asserted silent)* | n/a |
| `wal-corruption-failstop` | `store.wal.Decode.errors` >= 2 | shared with the benign torn tail — control arm discriminates |
| `checkpoint-dirfsync-fault` | `store.wal.TruncatePrefix.errors`, `store.wal.Close.errors` >= 1 (unique); `store.checkpoint.RunCheckpoint.errors`, `store.wal.Append.errors`, `store.wal.Sync.errors` >= 1 | the truncate failure is unique; the append/sync/close triple is the **poison signature** downstream of it |
| `checkpoint-crash-storm` | `store.recovery.snapshot.promoteParentFsync` >= 1 (unique); `store.checkpoint.RunCheckpoint.errors`, `store.snapshot.WriteSnapshotFullCtx.errors` >= 3 | the promote counter has one emission site in the module; the other two are required at the **window count** |
| `snapshot-corruption-failstop` | three aggregates >= 9; eight per-component `Read*.errors` >= 1 | aggregates shared, per-component decoders unique |
| `db-teardown[fault-on-close]` | `store.DB.Close.errors`, `store.wal.Close.errors` >= 1 | both unique: the teardown failed **and** it failed at step 3, where the fault was armed |
| `checkpoint-cadence[transient-fault]` | `store.snapshot.WriteSnapshotFullCtx.errors`, `store.snapshot.WriteCapture.errors` >= 1 | shared — the clean cadence arm is the standing control |

**Three disable-a-fault proofs, on three different scenarios**, show the
declarations are load-bearing rather than incidentally satisfied. Withdrawing the
WAL byte flip, withdrawing `FaultOnClose` from the teardown, and running the
clean cadence arm each leave **every** declared counter at zero, and each proof
asserts that the specific declared counters are the ones reported missing — not
merely that something failed. A separate, shape-only gate
(`CheckCounterDeclShape`) reads no run at all: it rejects a declaration that
names nothing, that claims blindness and counters at once, that sets a floor of
zero, or that admits a shared counter with no discriminator — every form that
would pass by saying nothing.


### graph/io completeness: DOT, the property path, the caps, and cancellation (rmp #2480)

`io-roundtrip-fault` drove two edge-list formats and one property format before
this task. Four things were therefore untouched, and each was invisible to a
green suite for the same reason: nothing referenced them at all.

**The DOT writer has no reader, so a round-trip cannot adjudicate it.**
`graph/io/dot` exports and never imports, which is why it was imported nowhere in
the simulator. It is now adjudicated by **cross-format agreement**: the same
model is written as DOT, as CSV and as JSONL, and the three must describe the
same graph. The DOT text is read back by a character scanner in
`internal/sim/graph_io_surface.go` rather than by a line split, because the
writer quotes an identifier containing the edge operator or a statement
terminator and a line-oriented parser would mis-split exactly the identifiers the
arm exists to drive. The model is built to force every branch at once: a DOT
reserved keyword (`graph`), identifiers carrying a space, a quote, a backslash,
`->`, a comma and a leading `-`, the empty identifier the engine accepts (rmp
#2043), zero and non-zero weights, and one isolated vertex. The measured census
at the pinned seed is 36 quoted identifiers, 13 weight labels and 1 bare node
statement over 26 edges.

**The one legitimate format disagreement is asserted in SHAPE, not waived.** An
edge-list CSV cannot encode a vertex with no incident edge. Rather than compare
only the edges, the arm asserts that the CSV vertex set is **exactly** the model
minus the isolated vertex and that DOT and JSONL both carry it — so a format that
began losing ordinary vertices would fail, where "the formats differ, never mind"
would not. The non-vacuity gate refuses a run in which CSV carried as many
vertices as the model, because the disagreement the verdict adjudicates could
then not arise.

**Three of the eight caps in the audit list are WRITER-side and unreachable from
a mutated export.** `ErrPropertyValueTooLarge`, `ErrPropertyNestingTooDeep` (both
packages) and `graphml.ErrInvalidXMLChar` are raised by the encoders
(`graph/io/jsonl/writer.go`, `graph/io/graphml/writer_props.go`), so they are
provoked by handing the writer a hostile GRAPH, not by feeding a corrupted file
to a reader. `graph/io/csv` also has no `*CappedCtx` variant at all — its ceiling
is the `Options.MaxBytes` field — and it carries a sentinel the list omitted,
`ErrTooManyFields`, as `jsonl` does with `ErrUnknownType`. The verified surface
is 14 sentinels, declared with their side in `GraphIOGuardDecls`; **13 are
provoked and matched with `errors.Is`** on every run, and the verdict fails when
a cap declared reachable was not reached, so deleting a probe cannot quietly
reduce the coverage.

**The '#'-leading id was excluded from the hostile-name set on a claim that had
stopped being true — rmp #2533.** The set carried a note saying a `#`-leading id
"does not survive a CSV round-trip", because the CSV reader treats a leading
comment character as a comment line, and the id was left out on that basis.
Re-validating the claim REFUTED it: rmp #2042 had already made the CSV writer
force-quote any cell whose first rune is the ACTIVE comment rune, so the id
round-trips intact. The exclusion was preserving a defect record that the code
had outgrown, which is worse than no record at all — it discourages exactly the
assertion that would have caught a regression. `"#hash"` is now in
`graphIOHostileNames`, where it drives the force-quoting path end to end.

The class it belongs to — a node identifier whose FIRST character is significant
to the format's own syntax — is now asserted across all four formats in
`graph/io/prefix_significant_id_roundtrip_test.go`, and the answer per format is
recorded rather than assumed. **CSV** is the only one exposed, and it is closed by
force-quoting; the test also drives a NON-DEFAULT comment rune (`/` and `-`), so
the writer is held to keying on the configured rune rather than a hard-coded `#`
— coverage nothing had before, and it fails on different ids than the default arm
does. **JSONL** has no comment convention at all and every id travels inside a
JSON string that `encoding/json` escapes, so the class does not arise. **GraphML**
puts ids in XML attribute values, where the hazard is markup rather than a line
prefix; the probe ids are `<xml>`, which would break the document if emitted raw,
and `&amp;`, which must come back as the five characters it is and not be decoded
to `&`, a double-unescape being the silent-corruption shape of this class in XML.
**DOT** is write-only, so there is no importer to lose anything; what is asserted
instead is the property a third-party parser depends on — every hazardous id is
emitted inside double quotes and no emitted line begins with `#`, `//` or `/*`.

Non-vacuity was established by disabling the writer's force-quote branch: all
four CSV arms fail, the two configured-comment-rune arms failing on `-danger` and
`/*block*/` rather than on `#hash`, and the DST cross-format verdict fails with
them (`<io-csv>`: the CSV round-trip did not reproduce the modelled edge
multiset), which is what putting the id INTO the battery bought.

**The two size caps and the two depth caps need DIFFERENT payloads, and a single
one would reach only one of them.** The encoders check depth on the way DOWN and
size after serialising on the way back UP, and the nested-list wire grows ~2x per
level, so a nested list carrying data trips the 64 MiB **size** cap at depth ~24
and never reaches the depth ceiling of 128. The size probe is therefore a single
oversized value and the depth probe is 130 nested EMPTY lists, which costs
0.3 MiB and no serialisation at all.

**One cap is structurally unreachable, and the reason is asserted rather than
assumed.** `jsonl.ErrListTooDeep` fires at nesting depth 64. The wire embeds each
level as a re-escaped JSON string, so the encoded size roughly doubles per level
— measured 112 bytes at depth 1 and 2,097,401 bytes at depth 18, a ratio of
**2.00x per level** — which puts an input reaching depth 64 at order 2^67 bytes.
The declaration carries that reason, and the run still adjudicates it: the
measured growth ratio must stay at or above 1.9x, the extrapolated size at the
guard's depth must stay above 2^62 bytes, and the deepest nesting that still
round-trips must stay well above the trivial, so a change to the encoding or to
the depth ceiling that made the guard reachable fails the run instead of leaving
a declaration quietly stale.

**Bounded allocation is measured, not assumed — and what each bound proves
differs by probe.** Every `ErrInputTooLarge` probe is fed an **unbounded**
generator with a 64 MiB safety ceiling, so without the cap the reader would not
stop; reaching the ceiling is itself a violation ("the cap did not stop the
reader"), which is what makes those bounds decisive rather than decorative. The
crafted probes (`ErrTooManyKeys`, `ErrTooManyData`, `ErrTooManyFields`) bound the
per-element amplification the guard exists to refuse. The two writer size caps
are checked **after** the value is serialised, so their bound is a ceiling on the
encoder's blow-up and is documented as such rather than presented as a
zero-copy claim. Measured under `-race`: 5.8-13.5 MiB for the endless probes,
107.6 MiB for the `<key>` flood, 320.3 MiB for each 64 MiB size cap.

**Mid-parse cancellation is now driven on all five `*Ctx` readers.** Every one
checks `ctx.Err()` once per 4096 units — rows for the CSV and JSON Lines readers,
**edges** for both GraphML readers — so a short document runs to completion and
proves nothing. The arms import a 12,000-edge chain and cancel from inside the
`io.Reader`, at the byte offset of the 5,000th unit, so the cancellation is
observed at the check at unit 8,192 with thousands of units already folded in.
All five return an error wrapping `context.Canceled` and a **nil** graph, so no
partial graph escapes; each is paired with an uncancelled control over the same
bytes that must reproduce the model exactly, because "the reader returned nil" is
otherwise satisfied by a reader that always returns nil.

**The caps are crafted, not seeded, and the split is recorded as an
assertion.** No byte flip or truncation of an ordinary export produces 65,537
`<key>` declarations, so the caps are driven deterministically once rather than
on every seed. The seeded sweep does what a seed sweep can: four corruptions
(byte flip, truncation, spliced prefix, delimiter run) against four importers,
requiring no panic, a genuinely changed artefact, at least one semantically
effective mutation per format, at least one rejection overall, and allocation
bounded at 64x the bytes fed in. A test asserts that not every mutation reaches a
typed cap — if it did, the crafted battery would be redundant, which contradicts
its stated purpose.

Every gate here is proved falsifiable by a synthetic result that drives it red:
ten for the surface non-vacuity gate (each removing one piece of evidence the
verdict rests on), eleven for the cap and cancellation verdict (an unprobed cap,
a wrong sentinel, a panic, an overrun ceiling, a blown heap bound, a wire that
stopped re-escaping, a depth guard firing early, an untyped cancellation, an
escaped partial graph, a cancellation landing before parsing began, and a control
that did not reproduce the model), and six for the declaration shape gate.

### The bulk loader's full contract: content, streaming, caps and a faulted publish (rmp #2488)

`store/bulk.Loader` is the module's non-transactional ingest path: it streams
`(src, dst, weight)` records into an in-memory adjacency, builds an immutable
CSR, and publishes it as a Tier 2 csrfile. Until this scenario the DST touched
three of its methods — `New`, `Add`, `Finalise` — and threw the result away. The
pre-existing `bulk-vs-online` scenario drives 20,000 `Add` calls beside
transactional writes and then compares ONE number, the row count, against the
constant it just used to generate them; the returned CSR is discarded, the
csrfile goes to a real OS temporary directory that no fault can reach, and the
loader is only ever configured Directed+Multigraph. That scenario is a
concurrency and resource-stability watch, it remains one, and it was left intact.
`bulk-load-oracle` (`internal/sim/bulk_load_oracle.go`) occupies the content and
fault gap beside it: every arm adjudicates the **loaded graph**, and every
publication goes through a `SimDisk` so the atomicity of the publish can be
attacked.

**The model is independent of both builders.** `bulkOracleModel` reimplements
the documented ingest contract in plain Go — per-edge interning order,
simple-graph first-occurrence dedup, undirected mirroring with the self-loop
exception, and the stable order-by-destination each row ends in — and calls
neither `graph/adjlist` nor `csr.OrderRuns`; its sort is `slices.SortStableFunc`.
That matters because `store/bulk`'s own identity tests are differentials between
two ENGINE code paths (`TestCSRDirect_IdenticalToBuildFromAdjList` builds an
`adjlist.AdjList` itself and compares `csr.BuildFromAdjList` against
`buildCSRDirect`), and a differential cannot see a defect the two sides share.

**One primitive is explicitly NOT certified here.** The NodeID a key receives is
assigned by `graph.Mapper` first-seen-per-shard from a hash of the key, which the
harness cannot derive without reimplementing the hash. The model therefore
interns through its OWN `graph.Mapper`, Src then Dst per edge in input order,
which is the rule `buildCSRDirect`'s doc comment states. What is certified is the
edge multiset, the dedup, the mirroring, the within-row ordering, the row cap,
the streaming contracts and the publication's atomicity — all of it **indexed by
whatever ids the Mapper chose**. A Mapper that assigned ids differently but
consistently would pass; a loader that lost, duplicated, reordered or
mis-weighted an edge would not.

**The arms.** `Drain` to completion and `Drain` cancelled mid-stream (the
cancellation point is exact by construction: the producer sends into an
UNBUFFERED channel and then cancels without sending again, so `Drain` has
necessarily completed the `Add` for every send that returned); `AddBatch`,
including the PARTIAL contract its godoc documents — the error identity, the
exact surviving row count, and the CONTENT of the accepted prefix against the
model, rather than merely that an error appeared; the `MaxRows` crossing on all
three ingest entry points; all four Directed × Multigraph configurations, each
built twice and required to produce a byte-identical csrfile and identical CSR
slices under `Parallel` true and false; publication onto a `SimDisk` under armed
sync, rename, ENOSPC and parent-fsync faults; a media-fault disk at a 0.30
per-sector probability; a corruption arm; and a host-crash differential.

**The post-fault oracle had to be THREE-way, not absent-or-complete.** A reader
of the published path must observe exactly one of: ABSENT; present and
reconstructing the expected graph EXACTLY; or present and REJECTED by the
reader. The third is legal and reachable — with a non-zero fault rate a written
sector is silently corrupted, so the writer can return nil over an image whose
stored CRC no longer matches its bytes, and media corruption after a correct
publication is not an atomicity failure but what the checksum exists to catch.
The forbidden fourth state, and the one `bulkOracleAdjudicateImage` exists to
catch, is present, accepted and DIFFERENT. Nor is a rejection required to be a
CRC failure: `csrfile.DecodeHeader` validates magic, version, byte order and
weight kind before the tail CRC is computed, and a TRUNCATED image is caught by
neither — the `total == fileLen` equality in `Header.validate` yields
`csrfile.ErrHeaderInconsistent` deterministically, so a torn publication is
caught by SIZE, not by checksum. Pinning the oracle to `ErrFileCorrupted` alone
would report false failures. The measurement that forced this: at the catalogue
seed the media arm's eight attempts gave 2 absent, 6 rejected and 0 complete
(rmp #2488), so a two-way oracle would have reported six false defects. The
shipped gate does not pin that distribution — it requires at least one REJECTED
outcome, and says in its own failure message why.

**The crash window is a pinned differential, not a sample.** Three sub-arms
publish generation 1, then generation 2, then crash the HOST: a *control* where
generation 2 published cleanly (the crash must leave generation 2), a
*treatment* where the parent-directory fsync is faulted and the rename pinned to
its rolled-back branch (the crash must restore generation 1 — the publication is
lost, whole, never torn), and a *writeback* arm pinning the other legal outcome
of the same window (the crash must leave generation 2). Both branches are pinned
rather than drawn, so each side is deterministic for every seed. Every verdict is
read off the DURABLE image, never off the publisher's return value: real power
loss ends the process, so what a still-running publisher would have returned is a
harness artefact. `SimDisk.CrashProcess` is deliberately not used for this —
a SIGKILL discards nothing, so it could only ever confirm "never torn"; only
`CrashHost` can revoke a dirent. And the published path is a SUBDIRECTORY key on
purpose: `SimDisk` treats a path whose parent is `.` or `/` as durably linked
from creation, so publishing at root level would make the rename un-rollbackable
and every "the name survived the crash" assertion would pass while proving
nothing.

**What this scenario CANNOT reach, stated rather than implied.**

- **The goroutine fan-out is unreachable from outside the package.**
  `Loader.buildParallel` — the phase-1-intern / phase-2-partition-by-shard
  fan-out — is gated by `parallelEligible()`, which requires Directed AND at
  least 50,000 buffered edges; but `Finalise` matches `Parallel &&
  csrDirectEligible()` FIRST, and `csrDirectEligible()` is
  `Directed && MaxShardCapacity == 0`, which no public `bulk.Options` can
  falsify. So the byte-identity arm certifies the PRODUCTION parallel path —
  `buildCSRDirect` against `BuildFromAdjList`, which is the code a caller setting
  `Parallel: true` actually runs — and says nothing about multi-goroutine build
  determinism. `TestBulkLoadOracle_ParallelFanOutStillUnreachable` fails if a
  future revision adds a shard-capacity knob to `bulk.Options`, so this paragraph
  cannot go stale silently. The eight (Directed × Multigraph × Parallel)
  combinations reach exactly three builders, and `Parallel && !Directed` is the
  only externally reachable route into the buffered-replay branch.
- **`Finalise`'s own publication cannot be faulted.** It publishes through
  `csrfile.WriteToFile`, which binds the OS backend at its entry point. The fault
  arms call `csrfile.WriteToFileWith` over a `SimDisk` instead — the same writer
  core, both forms tail-calling `writeToFileWith` and differing only in the `fs`
  value, so the write/fsync/rename/parent-fsync protocol under test is the one
  `Finalise` runs. What is NOT covered is the `Finalise` → `WriteToFile` call
  edge under a fault; the closest reachable substitute, an unwritable
  `OutputPath`, is driven by `bulkOracleArmRealFS` and pins the error wrapping
  and the fact that the built CSR is still returned.
- **A crash cannot land inside the build.** The build completes wholly in memory
  before the single publication, so there is no partial-CSR state to crash into.
- **A leftover temp file is an observation, not a violation.** Nothing in the
  module enumerates the publish directory, so a stranded `<path>.tmp` is
  invisible to every reader and never reclaimed. It is counted into the evidence
  and reported, never raised as a violation. Measured on the default seed: zero
  of the five armed publish faults strands one, because every failure path in
  `writeToFileWith` removes the temp file before returning.

**One harness defect was found before the tests were written and fixed there:**
an atomicity clause guarded on a `lastGood` value that a zero-complete seed never
assigned, so it could never fire. Falsifiability is observed rather than
asserted: 16 of 16 scenario seams and 8 of 8 checker dimensions were driven RED
(rmp #2488). No engine defect was found across all four configurations and 200
seeds.

### The lock-free CSR publisher's generation lifecycle (rmp #2491)

`graph/generation` publishes an immutable `csr.CSR` under an `atomic.Pointer` and
keeps every superseded generation alive until its refcount drains to zero. It had
**zero** DST coverage, and its failure modes are exactly the class the DST exists
to find: a refcount that leaks, a generation recycled while a reader still holds
it, a drain that never completes, a `Close` that wedges. None of them is visible
in the ANSWER a reader gets — a reader handed the wrong generation still returns
a well-formed graph, it simply returns the wrong one — which is why the oracle in
`internal/sim/generation_swap.go` is an **identity** oracle and not a
well-formedness one.

**The identity oracle is self-locating.** Every generation the plan builds is a
different graph and carries its own publish sequence number INSIDE its content:
node 0 has exactly one out-neighbour, and that neighbour is `1 + seq`. A reader
can therefore decode from the artefact alone which generation it believes it
holds, and then compare the WHOLE traversal against the model's independently
computed fingerprint for that sequence number. Three identity channels must
agree: the content's marker, the model's plan row for that sequence, and the
generation POINTER's recorded sequence (checked terminally). The fingerprint is
computed by the model from its own adjacency map and by the reader from the
engine's read path (`csr.CSR.NeighboursByID`); neither side asks the engine what
the answer should be. The package's own `csr_rotation_consistency_test.go`
cannot reach any of this: the shared `makeCSR` helper it uses
(`graph/generation/generation_test.go:14`) adds exactly ONE edge,
`seed → seed+1`, whatever the seed, so its "`Size()` must equal 1" oracle holds
for every generation that ever existed and cannot distinguish one from another —
a well-formedness check wearing a torn-swap name.

**No refcount clause can be flaky, because none of them samples.**
`Generation.Refcount` is documented as an observability counter that races with
concurrent `Acquire`/`Release`, so "the refcount is N" is never a sound
assertion. Every clause is therefore a structural bound that holds at every
instant, or is taken where nothing can move the counter: a FLOOR (a reader inside
its own access window holds one reference, so a value below 1 is a lost increment
or a double decrement, never a scheduling artefact); a CEILING (a reader holds at
most one outstanding increment, since `Acquire`'s retry loop rolls its increment
back before retrying, and the publisher holds at most one hostage reference, so
the count can never exceed `readers + 1`); and AT REST, after every reader has
been JOINED and the publisher stopped, where "every generation ever published has
refcount 0" is a total, exact assertion. That last one — not a poll during the
run — is the sound place for the task's "every superseded generation's refcount
returns to zero".

**The drain-timeout arm is structural, not timing-thresholded.** It does not race
a short duration against a drain: the PUBLISHER acquires the generation it is
about to supersede and holds that reference across the whole `PublishWithDrain`
call, so the wait loop's condition is permanently true and the only exit is the
timeout branch. `ErrDrainTimeout` is therefore guaranteed for any positive
timeout — 1 ns or 1 s — and the timeout value changes only how long the call
takes, never its verdict. The arm also asserts the hostage really was the
captured predecessor, so a spurious timeout cannot be mistaken for the contract.
The paired CONTROL is what makes it mean something: the plan forces the very next
publish to be an unbounded `PublishWithDrain(c, 0)`, which must return nil, so
timeout-when-held and drain-when-free are both measured and neither direction can
pass vacuously. The post-`Close` contract is pinned alongside.

**Determinism, exactly.** `ExecMode.Reproducible` is false for `ModeConcurrent`
and this scenario does not pretend otherwise. The whole plan and every
per-goroutine sub-seed are drawn up front on the calling goroutine, in a fixed
order, before any goroutine spawns. Reproducible from the seed alone: the
generation count, every generation's shape and fingerprint, the
publish/drain/drain-timeout op for each publish, the index of the drain-timeout
arm, and the plan digest the report pins. Reproducible from (seed, reader count):
the reader sub-seeds — drawn AFTER the plan precisely so varying the reader count
cannot perturb it. NOT reproducible, and recorded as telemetry rather than
asserted: how many acquisitions happened, which generation each landed on, and
the observed refcount values. Every non-vacuity clause is structural rather than
a rate: each reader's first acquisition is taken before the publisher may publish
and its last after the publisher has finished, so "every reader straddled at
least one swap" is a fact of the construction rather than a hope about
scheduling.

**What this scenario CANNOT detect.** USE-AFTER-FREE is not reachable: Go's
garbage collector keeps a `*csr.CSR` alive for exactly as long as a reader holds
the pointer, so there is no freed memory to touch and no observable fault to
catch; detecting a true use-after-free would need a poisoned allocator or unsafe
reinterpretation of released storage, and this scenario has neither. What IS
reachable is USE-AFTER-RECYCLE: the modelled decision to reclaim a generation's
backing storage, which the publisher may only take once an unbounded
`PublishWithDrain` has returned nil. A reader that ever finds that flag set on a
generation `Acquire` just handed it has caught a premature reclamation.
CONCURRENT PUBLISHERS are out of scope here deliberately — the readers'
monotonicity clause is only sound under a single publisher, because the plan
allocates sequence numbers before the swap rather than under the library's
`publishMu`; the package's own
`TestPublisher_ConcurrentPublishWithDrain_NoLostDrain` covers that through an
unexported seam this scenario cannot see.

**Falsifiability is tabulated in the file, with its provenance split.** Eleven
library mutations are recorded against the clauses they fire, and the file names
that table as authoritative over any prose. Five were reproduced in rmp #2491's
validation pass and carry the host, seed and reader count they were measured
under; the other six are inherited from the implementing session, were not re-run,
and their sighting counts are marked unattributed — the split is kept because a
count with no seed and no reader count behind it is not evidence. One mutation is
caught only PROBABILISTICALLY (dropping `Acquire`'s re-check, whose window is a
few instructions wide), and the inherited figures for it are recorded with an
explicit statement of what they do and do not license: detection needed both the
race detector's added preemption and a fleet wider than the host's core count,
and no detection rate is claimed for any width set.

**Two harness defects were found in validation and fixed.** `testing.AllocsPerRun`
panics when called while any parallel test is in flight, so the
fingerprint-allocation gate must stay in the sequential pass and its
`t.Parallel()` is intentionally absent. And a shape forgery that dropped the
highest node id from the SOURCES only is not encodable at all —
`csr.Validate` rejects a destination that is not strictly below the node count —
so it was refused for **120 of 200** seeds and passed only because the catalogue
default is one of the other 80; dropping the destinations too makes it
seed-independent and changes neither expected clause.

**The wide fleets are in the SHORT layer, and the reason is a measurement that
refuted the original justification by three orders of magnitude.** The
64/256/1024-reader arms and a 64-seed geometry sweep were first gated behind
`soak || nightly` on the estimate that 1024 readers would be "minutes of work
under the race detector".
Measured on darwin/arm64, 10 logical cores, under `-race`: one run at 1024
readers is 10,247 acquisitions in 37 ms, the whole wide-fleet test is 0.28–0.29 s
and the 64-seed sweep 0.17–0.18 s — 0.46 s together. The reason the estimate was
so far out is a property of the scenario, not of the machine: a reader performs a
minimum number of acquisitions and then stops as soon as the publisher is done,
and the publisher does not pace itself, so per-reader work FALLS as the fleet
widens (about 199 acquisitions each at 8 readers, 13 at 256, 10 at 1024) and
total work grows sublinearly with width. The arms are therefore kept for
FLEET-WIDTH and GEOMETRY diversity rather than for cost, and the direction is
stated honestly: at 1024 readers the ceiling clause's bound is LOOSER, not
tighter, so the wide arms add contention DEPTH rather than a sharper clause. The
64-seed sweep is what varies the drain-timeout arm's POSITION in the publish
sequence — measured at 31 distinct positions.

### Read-transaction isolation

`cypher.Engine.BeginReadTx` provides **snapshot isolation across the whole
transaction** since rmp #2307: one read instant is pinned at `BEGIN` and every
statement of the handle executes at it. It was per-statement read-committed when
this section was first written, and the `readtx-isolation` scenario — which
asserts only that no dirty or partial read is ever observed — certifies a
property both levels satisfy, so it remains valid under the stronger contract.

### MVCC multi-session and concurrency coverage (sprint 345)

The MVCC machinery is exercised end to end by four dedicated modes (see
[docs/dst.md](dst.md#mvcc-multi-session-and-concurrency-coverage) for the full
description): the deterministic multi-session mode with in-transaction
isolation checkers (`RunMVCCSessions` + `mvcc_isolation.go`, rmp #2436), the
contended lost-update scenario (`RunMVCCContention`, rmp #2437), crashes with
open transactions and transaction-granular recovery adjudication (rmp #2438),
Bolt-wire transactional roles with typed conflict accounting and during-run
isolation oracles (rmp #2439/#2440), and the `production-profile` catalogue
scenario combining all of it over the durable store in crash cycles
(rmp #2441), which since rmp #2469 checkpoints inside its traffic and
adjudicates the MVCC clock and the transaction sequence across the snapshot
boundary. The checkers found four engine isolation defects on arrival
(rmp #2445, #2446), all fixed and regression-pinned.

## Read-path coverage beyond Cypher

### The fluent pattern-query engine as an independent second read path (rmp #2492)

`graph/query` is a full reader of the same `lpg.Graph` the Cypher engine reads,
and until this task **nothing under `internal/sim` imported it**. Its only
exercisers were `examples/02_property_graph`, `examples/19_pattern_query` and its
own in-package tests over hand-built fixtures, and the knowledge graph recorded
no DST scenario touching it. It is not a thin wrapper: it has its own working-set
representation (a `roaring64` bitmap of NodeIDs), its own label seeding
(`NodeIndex().Intersect` when a label is constrained, a `Mapper.Walk` when none
is), its own tombstone pruning (`pruneTombstones`) and its own index-seek
decision logic (`index_seek.go`), and none of that is shared with the Cypher
planner. Two independent readers of one substrate can disagree.

**The arbiter is the model, and this is a different stance from `differential.go`.**
This package already had a differential facility, and it deliberately validates
the engine *against itself*: `DefaultVariantPair` compares the default planner
with the same planner with the disconnected-equi-join hash join disabled. That is
sound there, because `EngineOptions.DisableHashJoin` exists precisely so the two
plans can be proven result-equivalent — any divergence is by construction a
regression and it does not matter which side is "right", because both sides must
be the same side. The fluent engine and the Cypher engine carry no such
guarantee, so "they agree" is a **weak** claim (both read the same label bitmaps
and the same property shards, and can be wrong the same way) and "they disagree"
would name a divergence but no culprit. Every probe therefore carries three
**separable** clauses:

| Clause | What a red result means |
|---|---|
| `fluent-vs-oracle` | the fluent engine's working-set logic is wrong |
| `cypher-vs-oracle` | the Cypher path is wrong |
| `fluent-vs-cypher` | the two engines disagree — and its SILENCE alongside two red oracle clauses says both engines agree and the MODEL is wrong, i.e. look at the harness first |

A defect in the shared substrate (a property store that loses a value) moves both
engines together and shows up as two red oracle clauses with a silent
`fluent-vs-cypher`. That is the correct attribution: it is not a fluent-vs-Cypher
divergence. A **fourth** channel, neither engine, walks `graph.Mapper.Walk`
directly, skips `IsTombstoned` ids and reads each survivor's `name`; its live-name
set is held to the model *before* any probe runs, so a probe failure downstream
cannot be explained away by a substrate that had already diverged.

**Which CSR generation the engine is handed is answered by a measured
invariant, not by a preference.** `query.New` takes both a graph and a CSR, so the
scenario builds a FRESH pair at every probe point, on the single simulation
goroutine, at a quiescent instant between two ticks — the topology the fluent
engine expands over and the state the model describes are the same instant. It
builds two: `BuildFromAdjListLive` with `LiveNodeFilter`, and the
tombstone-agnostic `BuildFromAdjList`. `Out()` must answer **identically** over
both. That is a theorem of the two prunes acting together — an arc whose source is
tombstoned cannot contribute because the seed step already dropped that source,
and an arc whose target is tombstoned is dropped by `Out`'s own prune, so between
them they remove exactly the arcs the live filter removes — which makes the clause
a detector rather than a tautology. The CSR is read by **nothing** except
`Pattern.Out` (verified in `graph/query/query.go`: `Vertex`, `filterByPreds`,
`seekIndexablePreds`, `Cardinality`, `Collect` and `NodeIDs` never touch
`Engine.csr`), so the invariance clause is scoped to the `Out` probes and the
label / property / range probes are provably CSR-independent.

**The tombstone gate is on the DETERMINISTIC seed path, and the reason is a
measurement.** `pruneTombstones` is the only thing between the fluent engine and a
deleted node, and the two seeding paths differ in a way that decides where a
non-vacuity gate can soundly go:

- The **label-seeded** path intersects `NodeIndex()`'s bitmaps. A deleted node's
  label-bitmap entry is not removed synchronously while MVCC is armed: lpg
  DEFERS it (`graph/lpg/mvcc_index.go`) and the **background vacuum** applies it
  in `applyDeferredIndexRemovals` once the reclamation watermark passes.
  MEASURED: two runs of the *same seed in the same process* observed **3** and
  **2** label-advertised corpses at the same tick. That count is therefore
  recorded as telemetry, gated on nothing, and excluded from the
  reproducible-evidence comparison. A non-vacuity gate on a scheduler-dependent
  count is exactly the flake this sprint spent two tasks (#2587, #2596) removing
  from other scenarios.
- The **no-predicate** path (`Vertex()` with no predicates) seeds from
  `seedAllLive`, which walks the Mapper. The Mapper **never forgets a slot** —
  NodeID stability is a hard contract, restated in `lpg.RemoveNode`'s own godoc —
  so every id ever interned is yielded on every call and every tombstoned one
  must be removed by the prune for as long as the run lasts. MEASURED: the
  tombstone count is monotonic and identical across runs of the same seed. The
  gate lives here, and the `all-live` probe is the one whose answer the prune has
  to earn.

The **detector** for a prune regression is the `unknown-id` clause, not any
name-set comparison, and the distinction is load-bearing. A corpse is *unnamed*
in the substrate view by construction, so a corpse that leaks into a working set
contributes no name and cannot change a name set. MEASURED by reproducing the
broken output: with the prune omitted, `fluent-vs-oracle` stays **silent** and
`unknown-id` fires alone. The name-set clauses detect a working set that gained
or lost a LIVE node; the identity clause detects a corpse; neither substitutes
for the other.

**`Out()`'s ghost-arc prune is unreachable on the live graph, so it is driven by a
constructed fixture.** MEASURED: `DETACH DELETE` strips the deleted node's
incident arcs, so the raw and live-filtered CSR builds report the **same**
`Size()` — on the catalogue seed and on all 24 seeds of the soak sweep. Letting
the invariance clause pass vacuously there would have been the easy mistake, so a
side fixture builds a path graph with the plain Go API and uses `lpg.RemoveNode`
to tombstone interior nodes *without* stripping their arcs — the one documented
way to produce a ghost arc. It **asserts its own precondition** (a raw arc into a
tombstoned target exists, and `cRaw.Size() > cLive.Size()`) before asserting any
answer, and because the removed nodes are drawn from the INTERIOR of a path — where
an incoming arc always exists — the precondition holds for every seed. A 400-seed
sweep proves that rather than asserting it, which is the lesson of #2491's
fixture that was unbuildable for 120 of 200 seeds.

**Seek and scan are separated by construction, and served-ness is claimed only as
far as it can be established.** `Vertex(label, pred)` is seek-eligible;
`Vertex(label).Vertex(pred)` is not, because `labelsInPreds` of a predicate list
holding no `WithLabel` is empty and both `trySeekProperty` and `trySeekRange`
return false immediately on an empty label list. So the two arms are genuinely
different code paths with no DDL churn and no instrumentation, and
`seek-vs-scan` is a real clause. Served-ness itself cannot be *observed* from
another package — `graph/query/index_seek_spy_test.go` and
`graph/query/equal_numeric_order_internal_test.go` are where the path is
observed — so the scenario **enumerates the guard's conditions** instead (a
non-nil `IndexManager`, a constrained label, an index of the right `Kind()`, a
`BoundNode()` matching the pair, and a concrete index satisfying the typed read
interface for the bound value's kind) and asserts them as
`precondition:seek-eligibility`. That is stated as an enumeration, not dressed up
as observation.

**The equality/degenerate-range asymmetry, and the one place it must NOT close
(rmp #2601).** Unifying the range order in #2600 and leaving the equality alone
made the two predicates disagree with each other over the same data:
`WithRange("age", Float64Value(age), Float64Value(age))` matched a stored
`Int64Value` and `WithProperty("age", Float64Value(age))` did not. #2601 routed
`equalValue` through the *same* exact comparator, so the scenario now drives an
`eq-mixed-point` window under the `eq-mixed` clause and, beside it, an
`eq-mixed:equality-vs-degenerate-range` clause that holds the equality answer
directly against `WithRange(v, v)`. The three-way adjudication of the two probes
already implies the identity, but only while both answers are non-empty; the
direct clause makes it independent of that, and it has its own perturbation
(`fqPerturbDegenerateRangeDrop`) because the degenerate-range arm is read nowhere
else in the battery and its silence would otherwise mean nothing.

The identity is **scoped to the orderable kinds**, deliberately. openCypher's
equatability is wider than its comparability: BOOLEAN, BYTES and TIME values are
equal to themselves but are not ordered scalars, so over them `WithProperty`
matches where the degenerate `WithRange` cannot. `age` is numeric, so this
scenario only ever exercises the orderable case; the deliberate divergence for
the other kinds is pinned in `graph/query`'s own tests so that a later reader
cannot mistake it for #2601 regressing.

MEASURED, the reachable arms against engine-created indexes are exactly (a `WithProperty` on a
BOOLEAN property is hash-served exactly too, but no engine-created index is bool-keyed, so it
is absent from this table and covered only by `graph/query`'s own tests):

| Predicate | Bound kind | Served by | Verdict |
|---|---|---|---|
| `WithProperty` on a string property | `PropString` | `hash.Index[string]` from `indexType:'hash'` | seek-served, agrees with the scan |
| `WithProperty` on a numeric property | `PropInt64` or `PropFloat64` | the same `<label>_<prop>_btree_num` companion, seeked as the DEGENERATE range `[v, v]` | seek-served as a **superset**, with `query.equalValue` as the exact residual filter, and agrees with the scan. Since **rmp #2601** a numeric equality is served here and NOT from a hash index: `hashLookuper[int64]` and `hashLookuper[float64]` were removed because a single-kind hash index is a **subset** of a unified equality and a subset cannot be repaired by a residual filter — the same reason #2600 removed `btreeRanger[int64]` one task earlier. No engine-created hash index is numeric (`CREATE INDEX` always builds a `hash.Index[string]`), so nothing an engine builds lost a seek |
| `WithRange` on a string property | `PropString` | `btree.Index[string]` from `indexType:'btree'` | seek-served, agrees with the scan |
| `WithRange` on a numeric property | `PropInt64`, `PropFloat64`, or one of each | the internal `<label>_<prop>_btree_num` companion, a `btree.Index[float64]` a btree `CREATE INDEX` registers alongside the user-named `btree.Index[string]` (`cypher/index_binding.go`) | seek-served as a **superset**, with `query.valueInRange` as the exact residual filter, and agrees with the scan. Since **rmp #2600** `query.seekRangeInto` routes every numeric bound pair to this index; before it, `seekRangeInto` asserted a `btreeRanger[int64]` that no engine-created index satisfies and had no float64 route for int64 bounds, so an `Int64Value`-bounded range was never served at all. MEASURED: the numeric companion was eligible at **every** battery of all 24 soak seeds (`numeric=16/0 … 21/0`) |

**What this scenario does not claim.** Resurrection is out of reach: Cypher
`CREATE` mints a fresh synthetic node key (`__cx_<hex>`), so the re-created
Person is a new NodeID and `lpg.AddNode`'s revive path — which needs the same
mapper KEY — is never entered. The churn phase drives `pruneTombstones` against a
GROWING tombstone set, which is a different property, and the file header says so
rather than letting the task's "delete-then-recreate" wording imply revive
coverage. Concurrent use of the fluent engine is also out of scope: the package
documents an `Engine` as safe for concurrent use only while the graph and CSR are
quiescent and a `Pattern` as owned by one goroutine, so a concurrent arm would
need that contract relaxed first, which is a design question rather than a test.
Only a single `Out()` hop is probed, and the one-hop equivalence rests on an
ASSERTED precondition (`precondition:model-shape`: every modelled node is
`:Person` and every modelled edge is `:KNOWS`, because `Out()` expands a CSR that
carries no relationship type at all) — so a future workload that adds a second
edge type fires that clause instead of silently comparing the wrong things.

## Write-path and schema-enforcement coverage

### The typed schema as a runtime enforcement hook (rmp #2493)

`graph/lpg/schema` is not advisory. A `*schema.Schema` installed through
`lpg.Graph.SetValidator` is consulted on the write path and refuses a value whose
kind disagrees with its declaration, before the mutation. Until this task
**nothing under `internal/sim` called `SetValidator`, and nothing imported
`graph/lpg/schema`** — every DST run, in every scenario, drove a graph with no
validator installed. Three claims were therefore unfalsifiable under simulation:
the accept/reject verdict on any write path, the all-or-nothing contract of a
refused write, and the behaviour of a graph rebuilt by recovery.

The gap was invisible to a green suite for the ordinary reason — nothing
referenced the surface. `graph/lpg` and `graph/lpg/schema` do have in-package
tests over hand-built fixtures (`validator_bypass_test.go`,
`validate_node_finalise_test.go`, `schema/enforce_writes_test.go`) and
`store/recovery` has one regression gate for a single op
(`edge_property_recovery_test.go`, task #1418), but none runs under crash
injection, none drives the paths side by side against one declaration, and none
asks what a *recovered* graph does.

**Five hook sites, verified in source.** The task's functional requirement said
the hook "sits inside the edge-property write paths". It is consulted on five,
node-side included:

| Site | File |
|---|---|
| `setNodePropertyInfo` | `graph/lpg/property.go` |
| `setEdgePropertyInfo` (columnar, per pair) | `graph/lpg/edge_property.go` |
| `setEdgePropertyByHandleInfo` (per stable handle) | `graph/lpg/edge_handle.go` |
| `setEdgePropertyAtInfo` (per CREATE ordinal) | `graph/lpg/edge_instance_props.go` |
| `AddEdgeLabeledWithProperty` (fused create+property) | `graph/lpg/lpg.go` |

Each is its own arm because the four property stores behind them are genuinely
different stores. MEASURED, on one pair `(a,b)` carrying one write through each
path: `EdgeProperties(a,b)` returned only the columnar value,
`EdgePropertiesByHandle` only the per-handle one, and `EdgePropertiesAt` only the
per-instance one. A single arm would have proved nothing about the other three.

**The oracle is a declaration table, not the schema.** `typedSchemaModel` is built
from the same `[]tsDecl` and `label -> required` map that are fed to
`Schema.RegisterProperty` and `Schema.RequireProperty`, and it never calls
`Validate` or `ValidateNode` to decide what the answer should be — the
internal/sim rule against validating the engine with the engine. A model that
asked the schema would agree with it by construction. The observed side is
classified by SENTINEL: `schema.ErrTypeMismatch`, `schema.ErrUnknownProperty` and
`schema.ErrMissingRequired` are three different refusals, an arm that accepted any
error would pass while the wrong one was raised, and an error matching none of
them is itself a violation.

**Coverage is constructed, not drawn.** Five paths x three verdict classes is
fifteen cells, and a non-vacuity gate on fifteen randomly drawn cells is a gate
that fails a run whose draws were unlucky. The battery SWEEPS: each epoch visits
every cell exactly once, in a seed-shuffled order. The seed decides the order and
the values; it does not decide the coverage.
`TestTypedSchema_SweepVisitsEveryCellExactlyOnce` pins that claim directly, and
`TestTypedSchema_CoverageGateFires` proves the gate is wired by running a
two-tick budget: MEASURED, that run reports exactly 13 coverage violations (34
violations in total, since the node battery, the checkpoint and every no-mutation
sub-clause are unreached too), one per cell the two ticks could not visit.

**What a refusal must not do.** Every rejected write is followed by a five-clause
battery, each clause separate because they fail for different reasons:

| Clause | What it reads |
|---|---|
| `no-mutation:value` | the target slot through the path's own accessor |
| `no-mutation:cross-accessor` | the columnar per-pair store through its SECOND public reader (`EdgeProperties` beside `GetEdgeProperty`) |
| `no-mutation:population` | `AdjList().Order()` and `AdjList().Size()` |
| `no-mutation:key-interning` | whether the unregistered key entered `Graph.PropertyKeys()` |
| `no-mutation:fused-edge` / `fused-endpoint` | whether the refused fused write inserted the edge or interned its fresh endpoint |

The interning clause is the one that distinguishes a hook running BEFORE the
intern from one running after it. MEASURED: after a refused write under an
unregistered key, `PropertyKeys().Lookup` still reports the key absent, and the
refused fused write interned no endpoint node and added no edge. Those are
asserted rather than assumed, because "the write returned an error" and "the write
changed nothing" are different claims and only the second is the contract.

The Cypher engine adds a sixth, coarser observation the direct API cannot make.
MEASURED: a refused `CREATE (n:Person {name:$name, age:$age})` INTERNS a mapper
slot and then TOMBSTONES it — the statement's undo runs — so the live node count
is unchanged and the name is unfindable, while the mapper slot leaks. The leak is
the documented NodeID-stability contract, not a defect, so the arm asserts the
count and the unfindability and deliberately does not assert reclamation.

**`Graph.ValidateNode` had no caller outside `graph/lpg`.** Required-property
existence is enforced only where an embedder invokes it, never by the engine.
MEASURED across the tree before this task, the only call was `graph/lpg`'s own
internal dispatch (`lpg.go`, `nv.ValidateNode(labels, props)` on the
`NodeValidator` interface) plus that package's own tests; every hit in
`graph/lpg/schema` is `Schema.ValidateNode`, a different receiver. This scenario
is now the first caller outside the package, and it invokes it at the
node-finalisation boundary itself. Five clauses, each with a CONSTRUCTED
precondition and, separately, the model's prediction over the node's actual
labels and properties (so a perturbation that changes the fixture fires the
literal clause while leaving the model clause silent, which is the correct
attribution):

| Clause | Fixture | Required verdict |
|---|---|---|
| `validate:mid-build` | label set, required property not yet written | `ErrMissingRequired` |
| `validate:finalised` | the same node, one write later | clean |
| `validate:unlabelled` | a node with no label | clean |
| `validate:ghost` | a name never interned | clean (the documented nothing-to-check exit) |
| `validate:pre-install` | a node whose forbidden value was written BEFORE `SetValidator` | `ErrTypeMismatch` |

The last fixture exists because `ValidateNode` re-checks the kinds of properties
that are already PRESENT — a branch `Validate` structurally cannot reach, since it
runs before the write. With the validator installed no write can produce a
forbidden stored value, so the branch is unreachable unless the value predates
installation or arrives through the recovery bypass.
`TestTypedSchema_PreInstallFixtureIsTheOnlyRouteToTheKindRecheck` asserts that
unreachability, so a future reader cannot mistake the fixture for redundant
scaffolding.

**Why the direct-API arms run on a side graph.** A direct `lpg` write does not go
through the WAL. Running these arms on the durable store's own graph would put
nodes in the engine that the `GraphOracle` does not model — breaking the
harness's node/edge count parity — and a checkpoint would then make those
unmodelled nodes durable. So they run on a graph the scenario owns, and the
durable store is driven only through modelled Cypher templates, which is what
keeps `InvariantChecker.Check` and `CheckDurability` meaningful here instead of
disabled.

**The two durable paths order the validator differently.** This is the finding.

| Path | Order | Consequence |
|---|---|---|
| Cypher engine (`walMutatorAdapter.SetNodeProperty`, `cypher/api.go`) | the validated `WriteView` write, **then** buffer the WAL op | a refused value never reaches the log |
| `store/txn` (`txn.Tx.Commit`) | append + **fsync** every buffered op, **then** apply through the `WriteView` | a refused value is already durable when it is refused |

The Cypher side is asserted across a real crash. Every `typedSchemaWitnessEvery`
ticks the loop arms a WITNESS: a Person created with an accepted `age`,
immediately followed by a refused string `age` on the same node. After every
recovery — and at the end — every armed witness is read through TWO independent
channels, a Cypher projection and a `Mapper.Walk` of the native store, and must
carry the accepted INTEGER and nothing else. The pair is what makes the clause
non-vacuous: "the refused value is absent" is satisfied by a recovery that
replayed nothing, so the accepted value has to come back too.

The `store/txn` side is measured by `typedSchemaPureStoreArm`, on its own
`SimDisk`, through `openSimTypedStore` rather than `OpenSimStore` because it needs
the `txn.Store` itself — the Cypher adapter cannot reach that ordering at all.
MEASURED 2026-08-24: the refused commit returned `txn.ErrCommittedNotApplied`
wrapping `schema.ErrTypeMismatch`, the LIVE graph was correctly left without the
property, and after a host crash and reopen the recovered graph carried `age`
**as a STRING**. See item 12 under
[Defects surfaced by this coverage work](#defects-surfaced-by-this-coverage-work).

**FIXED 2026-08-25 (rmp #2602), and the arm inverted with it.** `txn.Tx.SetNodeProperty`
and its value-bearing siblings now validate BEFORE buffering, so a refused op
never reaches the log and there is nothing for a validator-less replay to
materialise. The arm asserts the contract instead of pinning its breach: the
refusal is expected at BUFFER time (`pure-store:precondition`), a second
rejection from `Commit` is itself a violation (`pure-store:refused-twice`,
because the op should never have been buffered), and resurrection after the crash
is now an `ACID_CONSISTENCY` violation (`pure-store:resurrection`) where it used
to be the pinned behaviour.

This is the pin doing its job. It was a PIN on a measurement rather than prose,
and its message named exactly what to update when the ordering changed — the pin,
the file header, and this document. All three were, which is the argument for
pinning a measurement: prose would have gone quietly stale instead.

**The reopened graph carries no validator — asserted, not documented away.**
`SimStore.Graph()` returns a graph rebuilt by recovery, and the schema is not
among the snapshot's components, so nothing re-installs it. The pin is five
clauses from one constructed probe on a dedicated pin node:

| Clause | Required outcome |
|---|---|
| `pin:no-validator` | a write the live validator REFUSED is ACCEPTED on the freshly recovered graph |
| `pin:node-clean` | `ValidateNode` reports clean there too, so the whole-node hook is absent as well |
| `pin:reinstalled` | with a FRESH schema bound to the RECOVERED registries, the identical write is refused |
| `pin:validate-detected` | `ValidateNode` now reports the planted value as a type mismatch |
| `pin:validate-repaired` | once the value is repaired, `ValidateNode` reports clean |

Clause 3 is what makes clause 1 mean something: without it, "the write was
accepted" could hold because the write was never forbidden. The schema is REBUILT
rather than re-installed because `schema.New` mints property-key and label ids
through the registries it is handed, and a recovered graph has fresh ones. The
plant is a direct `lpg` write, so it never reaches the WAL and cannot contaminate
the durable image; the repair happens before control returns to the tick loop, so
no checkpoint can capture it either.

**Documented limitation.** *The schema is not persisted with snapshots, so a
graph reopened by recovery carries no validator until an embedder re-installs
one, and the durable replay path deliberately does not consult one.* The
consequence is asymmetric enforcement across a restart: writes made through a
live validated graph are checked, and writes replayed by recovery are not.

Since rmp #2602 that asymmetry no longer lets a REFUSED value through, because
both write paths now validate before the write-ahead log, so the log cannot
contain one (item 12). What remains open is narrower and stated as such: a log
written by an older build, before that fix, is outside the guarantee, and replay
still enforces nothing of its own. Persisting the schema alongside the snapshot components, and
validating on replay, would close it — and would be a change to the durable
format and to the recovery contract, so it is recorded here for adjudication
rather than decided by a test. What this task does is make the limitation
FALSIFIABLE: the five pin clauses above fail loudly the moment the behaviour
changes in either direction, so the documentation cannot drift away from the code.

**What this scenario does not claim.** Concurrency: the validator is installed and
read through `atomicValidator`, which is lock-free, but the scenario drives one
goroutine and asserts nothing about concurrent installation or about a swap racing
a write. Per-edge-instance required properties: `NodeValidator` is a whole-NODE
hook and `lpg` exposes no edge equivalent, so `RequireProperty` is exercised on
nodes only. Property kinds beyond the four scalars: BYTES, LIST, TIME and the
internal date kind go through the same `Validate` comparison, but the declaration
table names only STRING, INTEGER, FLOAT and BOOLEAN, which is what makes every
type mismatch constructible from another *declared* kind rather than from an
exotic one. And the `Schema.RegisterProperty` conflict path (a
second registration of one key under a different kind, which returns
`ErrTypeMismatch` at DECLARATION time) is a schema-construction error rather than
a write-path verdict, and is left to `graph/lpg/schema`'s own tests.

## Planner-statistics coverage

### The derived count-store, and the reopen that heals it (rmp #2494)

The relationship count-store (`graph/index/count`) is the planner's exact
cardinality source: `E(relType)`, `D(label, relType, dir)` and
`T(labelA, relType, labelB)`. Before this task the DST never looked at it.

- **`count.Store.Snapshot` had no production caller.** It exists "for
  observability and differential testing" and only the count package's own tests
  ever called it, so nothing outside `cypher` could read a cell.
- **No `CountE` / `CountT` / `TDirty` query shape was in the repertoire.** Every
  count the sim issued named a bound variable (`count(n)`, `count(r)`), which the
  planner does not serve from this store.
- **So the reopen-time recompute was never asserted.** It *runs* on every
  recovery already — `OpenSimStore` builds a fresh `cypher.Engine` per reopen and
  `NewEngineWithOptions` calls `recomputeCountStore` at construction — but nothing
  compared its result to anything.

This is the fail-silent class the DST exists for. A wrong count store raises no
error and fails no test: it yields a *plausible-but-wrong plan*. The rows are
still correct, just reached by the wrong access path, so the whole suite stays
green.

The gap was invisible for the ordinary reason. `cypher` has good in-package
coverage — `count_maintenance_test.go`'s `diffCounts` against an O(V+E) recount,
`count_rapid_test.go`'s rapid property test and its same-seed determinism gate —
but all of it runs on hand-built graphs inside the package with no crash
injection, no WAL, no snapshot and no reopen. Its recount oracle compares the
store with the **graph**, which is exactly what `recomputeCountStore` itself
does, so it structurally cannot witness the recompute.

**One accessor was added.** `cypher.Engine.CountSnapshot() count.Snapshot`
returns a copy of the cells and dirty markings. It deliberately does not return
the `*count.Store`: a store handle would hand every caller `Apply`, `MarkDirty`
and `RecomputeReset` over the engine's own derived state. `Engine.CountStoreCells`
already existed but is a size indicator, and `lpgLabelResolver.Counts()` sits on
an unexported type.

**The model is the shadow model, keyed by name.** `GraphOracle.countStoreModel`
recomputes E/D/T from the oracle's own node and edge maps — the op stream alone.
A model that recounted the *graph* would be a second copy of
`recomputeCountStore` and would agree with it by construction on exactly the
reopen this scenario checks. The store's keys are interned ids, so whether they
survive a reopen is an empirical question: MEASURED, they did — a pre-crash
registry of `0=Person 1=KNOWS 2=Vip 3=Gold` came back identical after a WAL-only
replay *and* after a snapshot+WAL reopen, and `Vip` kept id 2 in a run where no
live node carried it any more. Nothing documents that, so the model keys by NAME
and resolves through the recovered graph's own registry at comparison time.

**A dirty cell is not a wrong cell, and a `DETACH DELETE` makes one.**
`countRelabel` cannot enumerate a node's in-edges in O(delta), so a relabel marks
`D(X,*,IN)` and `T(*,*,X)` non-exact instead of writing a wrong exact (design
§3.3.1). What was not obvious is how easily that is reached: MEASURED, one
`DETACH DELETE` of a `Person` at tick 5 of a run left `DIn:[Person Vip]
TB:[Person Vip]`, and **the dirty sets only ever grow until the next
`RecomputeReset`**. So on any graph that has ever deleted a labelled node, the
planner's IN-side degree and triple statistics for that label are vetoed for the
rest of the session. That is what makes the reopen's heal consequential rather
than cosmetic — and it is why the scenario carries a never-relabelled,
never-deleted `Hub` label: without it the live `DIn` and `T` clauses would have
compared two empty maps. The `ComparedLive` / `ComparedRecovered` counters, and
the gate on them, are what turn that from an intention into an assertion.

**A negative cell is legitimate, and it is constructed on purpose.** `Store.add`
retains a cell driven negative rather than clamping it, because that is what makes
the aggregate order-insensitive (rmp #2303). MEASURED, that is reachable from
plain Cypher with no concurrency: with `a -> b` and both nodes plain Persons,
`SET a:X`, `SET b:X`, `REMOVE a:X` leaves `T(X, KNOWS, X)` at **-1** — the `+1`
was never applied (b has no out-edge, so the relabel's OUT recount returns early
and the IN side is covered by the `TB(X)` marking) while the `-1` did land,
because by then b carried X and the OUT recount reads the endpoint's *current*
labels. The scenario builds exactly that, under a label (`Neg`) it owns
exclusively so the cell cannot be cancelled by unrelated churn, and asserts two
things about it: the negative must be dirty-**covered** (an uncovered negative is
a lost decrement offered to the planner as exact) and it must be **gone** after
the reopen.

This also corrected a documentation defect. `Store.Snapshot`'s godoc said it
returns "every live cell (value > 0)"; the code has always used `v != 0`, and
must, for the reason above. The doc is now accurate and says why.

**The sharpest claim: the reopen heals.** The recovered phase skips *nothing* and
requires all four dirty sets empty. MEASURED on the constructed fixture:
pre-recovery `dirty{DIn:[Neg] TB:[Neg]}`, `negatives=[T[Neg,KNOWS,Neg]=-1]` and two
cells the dirty markings excused where ground truth says non-zero; post-recovery
`dirty{}` with 11 cells, `DIn[Neg,KNOWS]=1`, `T[Person,KNOWS,Neg]=1` and no
negative cell at all. The
non-vacuity gate is built on the *transition*: `HealedFromDirty` and
`HealedNegative` are credited only when the immediately preceding live
observation was itself dirty (respectively held a negative cell), and the fixture
is re-armed after every recovery — MEASURED before re-arming was added, a
1500-tick soak run had 19 recoveries and healed exactly **one** negative cell;
with the re-arm the same run reports 19 and 19, and all 152 of its live
observations hold a negative cell to classify.

**The query shapes.** `MATCH ()-[:KNOWS]->() RETURN count(*)` (the `E` shape) and
`MATCH (:Person)-[:KNOWS]->(:Person) RETURN count(*)` (the `T` shape) are added to
the shared surface battery, with references from
`GraphOracle.knowsPatternCount`, a derivation that consults both endpoints'
labels rather than counting edge-map keys like `knowsCount`. One honest
limitation: in a population where every node carries `Person` the two references
are the same number, so the labelled clause cannot fail where the unlabelled one
passes — what it adds there is the plan shape, not a second oracle. The
count-store scenario therefore probes four more shapes whose references genuinely
differ (`Vip`- and `Hub`-constrained), and each is checked **three ways**: the
query rows, the model, and the count-store `T` cell that serves it.

**`Cells()` boundedness** is asserted in soak, where |E| has grown far enough for
the claim to be distinguishable from "both numbers are small". MEASURED on a
1500-tick run: `Cells()` peaked at 20 against a combinatorial ceiling of 25 for a
four-label, one-type vocabulary, while the modelled edge count reached 227 — a
footprint of 8.8% of |E|, and a function of the schema, exactly as design §2.3
states.

#### The defect this scenario surfaced: the anchor swap drops rows for an anonymous labelled source

The scenario's first run failed immediately, on a post-recovery probe. Isolated,
it reproduces with **no store, no recovery and no simulator** — a plain
`cypher.NewEngine` over one `(:Person)-[:KNOWS]->(:Person:Vip)` edge plus forty
bare Persons:

| Query | Result | Correct |
|---|---|---|
| `MATCH (:Person)-[:KNOWS]->(:Vip) RETURN count(*)` | **0** | 1 |
| `MATCH (:Person)-[:KNOWS]->(b:Vip) RETURN count(*)` | **0** | 1 |
| `MATCH (a:Person)-[:KNOWS]->(:Vip) RETURN count(*)` | 1 | 1 |
| `MATCH (a:Person)-[:KNOWS]->(b:Vip) RETURN count(*)` | 1 | 1 |

The discriminator is the **source** node's anonymity, not the destination's. All
four render the identical `EXPLAIN` tree
(`NodeByLabelScan [Vip] -> Expand -> Filter`), so the plan text cannot tell them
apart; `PROFILE` localises the loss exactly — the `Filter` above the re-rooted
`Expand` receives one row and emits zero.

**Which answer is wrong, per the TCK.** The openCypher TCK covers this exact
pattern shape:
`cypher/tck/features/clauses/match/Match2.feature`, scenario **[2] "Matching a
relationship pattern using a label predicate on both sides"** — fixture
`CREATE (:A)-[:T1]->(:B), (:B)-[:T2]->(:A), (:B)-[:T3]->(:B), (:A)-[:T4]->(:A)`,
query `MATCH (:A)-[r]->(:B) RETURN r`, expected result one row `[:T1]`. So the
anonymous-both-sides spelling is required to return the matching rows: **the 0 is
wrong and the 1 is right.**

**Why the TCK does not catch it, MEASURED on the TCK's own fixture.** Two
independent reasons, each sufficient:

1. That scenario's relationship is **untyped** (`[r]`), and `matchAnchorSite`
   requires `len(exp.RelTypes) == 1`, so the pattern is not a swap candidate at
   all. MEASURED: adding 40 further `(:A)` nodes to the TCK fixture leaves the
   plan anchored on `[A]` and the answer at 1.
2. The fixture is **balanced** (2 `:A`, 2 `:B`), so the 2x cost-win margin can
   never be met even for a typed pattern.

Give the TCK's own scenario a relationship type and label skew, and it breaks —
same fixture, same shape:

| Query on the TCK Match2 fixture + 40 extra `(:A)` | Result | TCK expects |
|---|---|---|
| `MATCH (:A)-[r]->(:B) RETURN count(r)` (verbatim, untyped) | 1 | 1 |
| `MATCH (:A)-[r:T1]->(:B) RETURN count(r)` | **0** | 1 |
| `MATCH (:A)-[:T1]->(:B) RETURN count(*)` | **0** | 1 |
| `MATCH (:A)-[r:T1]->(b:B) RETURN count(r)` | **0** | 1 |
| `MATCH (a:A)-[r:T1]->(:B) RETURN count(r)` | 1 | 1 |

The discriminator is the SOURCE node's anonymity, isolated one variable at a time
across three fixtures: naming the source always fixes it, naming the destination
never does, the projection (`RETURN r` / `count(r)` / `count(*)`) is irrelevant,
and a single-label destination behaves exactly like a multi-label one. Every wrong
row is one where the plan anchored on the right-hand label; every balanced-graph
row is correct because no swap fired.

Why the swap's own suite missed it: MEASURED, `cypher/anchor_swap_diff_test.go`
and `cypher/anchor_swap_symmetric_test.go` contain 24 `MATCH` patterns between
them, and the only anonymous labelled nodes in either file appear in `CREATE`
clauses that build the fixture (`CREATE (:Other {i:i})-[:R]->(h)`). Every read
probe the two differential suites issue names both endpoints, so the spelling
that breaks was never driven.

The full suite is green with the defect present: MEASURED,
`go test -race ./cypher/` passes (190.6 s) and the TCK gate reports
**3897 scenarios, 3897 passed, 0 failed, 0 undefined (baseline=3897)**.

Attributed by A/B: `EngineOptions{DisableAnchorSwap: true}` makes all four return
1. The culprit is the single-edge anchor-swap peephole
(`cypher/anchor_swap_plan.go`, rmp #2090/#2150). `matchNodeScan`
(`cypher/ir/match.go`) leaves an anonymous node's variable name as the **empty
string**, so `matchAnchorSite` records `fromVar == ""` and `mirrorAnchorSite`
re-checks the from-label as `Selection{LabelPredicate{Receiver:
Variable{Name: ""}}}` above the re-rooted expand — a receiver that does not
resolve to that expand's destination binding, so the re-check is unsatisfiable and
every row is dropped.

The count store is what makes it reachable, and the direction of that is the
interesting part. The swap is admitted only when every cost input is
`EstExact ∧ ¬dirty`, so while a relabel keeps a label's IN families dirty the
swap is **vetoed** and the anonymous spelling answers correctly. The moment a
reopen clears the dirty flags — this scenario's own central claim — the swap
becomes admissible and the same query starts answering 0. That is the task's
fail-silent thesis reached from the other side: not a wrong count store producing
a bad plan, but a *correct* one unlocking a broken plan.

**FIXED in rmp #2603.** The reversal moved the from-label off the access path
(`NodeByLabelScan`, which enforces a label with no variable name) and onto a
predicate above the re-rooted expand (which can only reach a node through its
name) — and an anonymous pattern *head* has no name, since the IR translator names
every non-head node with a synthetic `__anon_N` but leaves the head's empty.
`matchAnchorSite` now declines any site with an empty endpoint name, so these
patterns keep the written order. The scenario's `Vip` shapes are back to the
anonymous spelling, and
`TestCountStore_AnchorSwapRetainsAnonymousSourceRows` is the regression gate that
replaced the pin: it asserts all four spellings answer 1 with the swap ENABLED
(the shipped default) and with it disabled.

#### What is not claimed

- **The OUT-side dirty branch.** `countRelabel` dirties `D(X,*,OUT)` and
  `T(X,*,*)` only when the relabelled node's out-degree exceeds
  `EngineOptions.MaxLabelRecountEdges` (default 4096), and that option does not
  reach the durable store: `OpenSimStore` builds its engine with `Store` plus the
  three recovered-schema fields and nothing else, while `Config.EngineOpts` is
  applied only on the plain in-memory path. So `DirtyDOut` and `DirtyTA` are empty
  for every observation this scenario takes, `parity:DOut` is always compared
  live, and the over-budget branch stays with `cypher`'s in-package
  `TestCountStore_BudgetTripDirtiesOut`. Threading the budget through would mean
  widening `simStoreConfig` and `OpenSimStore` — a harness API change outside this
  task.
- **Multiple relationship types.** One type (`KNOWS`) is driven, so `E` has a
  single term and the multi-type `T` fan-out is not exercised here; `cypher`'s
  rapid property test drives three types over four labels.
- **Concurrent readers of the snapshot.** `CountSnapshot` documents itself as safe
  alongside writers, and the store's own package tests cover concurrent deltas;
  this scenario drives a single goroutine, so it adds nothing there.
- **The planner's use of the cells.** The scenario asserts the cells are right and
  that the query answer matches them. Which plan the estimate selects is the
  anchor-swap and join-reorder machinery's own coverage — and the defect above is
  what happens when that machinery is wrong on cells that are right.
- **A dirty cell's value.** A dirty cell is documented non-exact, so nothing here
  asserts what it holds — only that a *negative* one is dirty-covered, and that
  the reopen leaves none dirty at all.

## Bolt wire-surface coverage

### The authentication surface, and why the WAL is the witness (rmp #2481)

Every SimServer in `internal/sim` was constructed with
`server.NoAuthHandler` — a handler that returns success for any scheme, any
principal and any credential. The consequence is stronger than "the credential
path was untested": an assertion made against that handler is **vacuous**,
because it asserts the absence of a check nobody installed. A probe that sent a
wrong password and then observed a successful write would have been observing
correct behaviour. So the gap could not be closed by adding arms to the existing
servers; the harness first needed a server that genuinely refuses, which is what
`NewSimServerAuth` provides (`BasicAuthHandler` over `ConstantTimeValidate`).

Four facts about the surface came out of reading the server rather than
describing it, and each one shaped an arm.

**The credentials arrive on two different messages, handled by two different
functions.** On Bolt >= 5.1 `handleLogon` authenticates and `HELLO` carries only
driver metadata; on <= 5.0 `handleHello` authenticates inline. Covering one
leaves the other untested, and the harness's `WireClient.Handshake` always leads
with 5.6 — the server correctly picks the highest version it is offered — so the
inline path was unreachable until `HandshakeOffering` was added to withhold the
newer versions. The wrong-password arm therefore runs twice, at 5.6 and at 4.4.

**A first authentication failure and a re-authentication failure are different
branches.** A failed first `LOGON` (or a failed inline `HELLO`) sets
`StateDefunct` and the connection closes; a failed `LOGON` from `READY`/`TX_READY`
calls `enterFailed`, which reclaims any open transaction and leaves the session
recoverable through `RESET`. Both are driven, and the second is the one that can
leave an explicit transaction open, so it is also where a reclaim defect would
show.

**`LOGOFF` from `TX_READY` leaves the session in `TX_READY`.** The state machine
does not close the transaction; only `s.authenticated` changes. That is precisely
why `handleCommit` and `handleRollback` carry their own authentication gate
(CWE-306, audit 2026-07-13 security F5) rather than relying on the state machine
— and it is what makes the `commit-after-logoff` arm the sharpest one in the set:
the write is already staged in the engine, and one boolean stands between it and
the durable log.

**A FAILURE reply proves the server SAID no, not that nothing happened.** This is
the reason the scenario is backed by a real WAL (`SimStore` over `SimDisk`) rather
than the in-memory engine every other wire scenario uses. Each arm is bracketed
between two readings of `wal.Writer.Stats`, and a refused arm must leave both the
frame and the byte counter exactly where it found them. The sentinel node its
statement would have created is then censused twice — in the live engine, and in a
graph reopened through real recovery after a crash — because a frame appended but
not yet visible would hide from the live census alone.

The exact failure **code** is pinned per arm rather than "some failure", since
mapping one onto another changes what a driver is told. Measured:
`Neo.ClientError.Security.Unauthorized` for a bad credential (both entry points,
and both branches),
`Neo.ClientError.Security.AuthProviderFailed` for an unknown scheme, and
`Neo.ClientError.Request.Invalid` for every de-authorised transition — the
illegal-transition code, not a security code, because the session reaches those
gates through `failTransition`.

**A shared failure code cannot attribute a refusal, so the ORIGIN STATE does.**
The authentication gate and the state-machine gate both answer
`Neo.ClientError.Request.Invalid`, because both go through `failTransition`. A
code-only assertion is therefore blind to the case that matters: if `LOGOFF`'s
target state regressed, `commit-after-logoff` would be refused by the *state*
check one line above the auth check, the arm would still see the expected code,
and the CWE-306 gate would be untested behind a green scenario. `failTransition`
names the state the session was in, which is exactly the discriminator — a refusal
by the auth gate names a LEGAL state, one by the state machine names `FAILED` —
so every gate arm pins it. Measured: `... in state AUTHENTICATION`,
`... in state READY`, `... in state TX_READY`, `... in state TX_STREAMING`,
`... in state NEGOTIATION`, `... in state FAILED`.

**Four arms exist because a security review asked what the roster still could not
see.** `route-after-logoff` completes the five auth-gated verbs and is the only
one whose violation neither the WAL counter nor the census could ever catch — a
leaked `ROUTE` writes nothing, so it needs its own assertion or none.
`logoff-in-tx-streaming` asserts the guard that lets `handlePull` and
`handleDiscard` run with NO authentication gate of their own: the flag can only be
cleared by `LOGOFF`, and `LOGOFF` is illegal in the streaming states, so a session
cannot become de-authorised mid-stream. That edge — load-bearing for two ungated
handlers — was driven by no test at any level. `reset-after-logoff-open-tx`
reaches the reclamation limb of `handleReset`, which every existing RESET test
misses because they all run with no transaction open; it asserts that RESET
discards the staged write and returns an unauthenticated connection to
NEGOTIATION, where a bare `LOGON` is illegal. And
`second-message-after-refusal` pins the scoping of the soft-IGNORE: `dispatch`
softens a request in FAILED to `IGNORED` only when the session is still
authenticated, so a de-authorised client must get a hard FAILURE — dropping the
`&& s.authenticated` half would have broken no other arm, because every one of
them stops at its first refusal.

**The instrument is shown moving in the same run.** Two arms are ADMIT arms: an
honest authenticated write, and a write after re-authenticating following
`LOGOFF`. Both must ADVANCE the counters (measured +4 frames, +183 and +188 bytes)
and both nodes must survive recovery. The second carries a second duty: a server
that refused every post-`LOGOFF` write — including the legitimate one — would
satisfy every refusal clause in the scenario and fail only here.

Non-vacuity is adjudicated by a separate shape-only gate (rmp #2470): the full
arm roster must have run, refusals and admissions must both have occurred, the
frame counter must have been observed moving, and all three failure codes must
have been seen. The **control** is a real alternative configuration rather than a
perturbed value — the identical wrong-password exchange against a
`NoAuthHandler` server must be ADMITTED — which is what attributes the refusals
to the `AuthHandler` and not to the state machine, the framing, or a mistake in
the harness. Every clause of both adjudicators is additionally falsified by
injection into the pure checker: 22 single-field perturbations, each required to
produce a violation naming its own defect.

The three new abuse families are classified by what they need, not by what they
are. `AbuseLogoffThenRun` and `AbuseCommitAfterLogoff` are refused by ANY server,
because the gate they reach is the session's own `authenticated` flag, so they
join the randomly-drawn set that runs against the NoAuth `bad-actors` server.
`AbuseBadCredentials` is admitted by `NoAuthHandler` — correctly — so
`BoltAbuser.PickFamily` must never draw it there, and a test drives it against
both server kinds to prove the distinction is real rather than decorative.

### TLS certificate rotation under fault (rmp #2481)

`server.CertReloader` had unit tests for its happy path, a parse failure and
missing files; what it had never been driven through is the sequence an operator
actually produces. The scenario runs ten steps — initial load, clean rotation,
torn key, garbled key, absent key, mismatched pair, completed rotation,
preserved-mtime rotation, expired leaf, not-yet-valid leaf — plus the
background-poller arm, and five things about it are worth recording.

**The oracle is a real TLS handshake, because the dangerous failure mode parses
cleanly.** A cert rotated without its key leaves two files that both decode
perfectly and no longer belong together. `crypto/tls` is the independent
reference that can see it: the verifier completes a genuine TLS 1.3 handshake
over a loopback TCP pair against whatever `GetCertificate` currently serves, with
the client trusting exactly that certificate and verifying the SAN. A successful
handshake proves the certificate parses, the name matches, and the private key
corresponds to the certificate's public key. It deliberately does NOT trust a
pre-agreed root, so it cannot by itself detect that the WRONG pair went live —
that is settled separately by the served leaf's Common Name, and crediting the
handshake with it would overstate what it checks. Measured across all four
faults:
`rotation-B` stayed in service and kept handshaking, and `rotation-C` took over
the moment its key landed. The verifier's own falsifiability is proved by pointing
it at a certificate issued for a different name, which `crypto/tls` rejects.

**Three documented contract halves were untested and are now pinned.** An
unloaded `CertReloader` refuses to serve rather than returning a nil certificate
the TLS stack would dereference; `NewCertReloader` over a torn key fails closed —
the initial load is mandatory, and a reloader that started on unparseable material
would put a server into service with no certificate at all; and the `Watch`
poller's `onError` callback is now asserted to FIRE over a broken pair. That last
one was reachable by nothing in the module: `onError` appeared only in
`tls_reload.go` itself, every caller passed a discarding closure, and deleting
`r.onError(err)` from `Watch` would have broken no test — even though it is the
only operator-visible signal that an unattended rotation failed and a stale
certificate is still in service. It is the same defect class as the
`Options.Logger` bypass this sprint fixed, and it is now evidence: measured 2
deliveries per broken-pair arm.

**A pair can parse, pair, and still be unable to serve — rmp #2557.** Every arm
above breaks the *material*: bytes missing, bytes corrupted, or the wrong key
beside the certificate. The commonest real rotation incident breaks none of them.
A renewal that produced an already-expired leaf, or a clock skew on the renewing
host that produced a not-yet-valid one, yields two files that decode perfectly and
belong to each other — and `tls.LoadX509KeyPair`, which inspects neither
`NotBefore` nor `NotAfter`, accepted them. `Reload` then swapped the doomed leaf
into service and the previous, working certificate was gone: a recoverable
condition (stale but functioning) converted into a total outage of the Bolt
listener, against a godoc that promised the previous certificate stays in service
"until the new pair is fully validated". Two arms now install exactly that
material and require the swap to be REFUSED.

The refusal is adjudicated by **cause**, not by failure. Each step records
`errors.Is(err, server.ErrCertOutsideValidity)` where the error is returned — not
a substring of a message — and the oracle fails in both directions: a validity arm
refused for any other reason, and a parse fault reported as a validity refusal.
Without that, "the reload failed" would have been satisfied by a torn key, and the
arms would have proved nothing about the window. A non-vacuity clause requires at
least one validity refusal to have been observed anywhere in the run, and a
distinctness clause requires both arms to have installed INTACT key material —
otherwise they would silently duplicate the torn and absent arms. `CertReloader`
also grew a module-internal `SetClock` seam, pinned here to the same instant
`tls.Config.Time` uses: a leaf's window is now read by the ENGINE as well as by
`crypto/tls`, so leaving the engine on the real clock would have made the fixed
fixture bounds a time bomb on both sides rather than one.

**The handshake oracle could not report the failure it exists to report.** Finding
the #2557 arms meant running them against the unfixed engine, and that run did not
fail — it HUNG, to the 40s test timeout, with both TLS peers parked in
`crypto/tls.(*Conn).flush`. `net.Pipe` is synchronous and unbuffered, so a failing
handshake deadlocks it: the server is still flushing its flight while the client,
having already rejected the certificate, flushes its alert, and neither side is
reading. Every earlier arm hid this, because in every earlier arm the reload was
refused and the handshake therefore SUCCEEDED; the one condition the oracle exists
to detect was the one condition that hung the run. The verifier now uses a
loopback TCP pair, whose kernel buffer absorbs the flight, plus a deliberately
generous 60s deadline as a deadlock breaker rather than a latency gate. The same
broken-engine state is now reported in 0.03s as nine attributed violations.

One latent defect in the scenario itself was found the same way and fixed. The
fixtures carry fixed validity bounds so their bytes are seed-reproducible, but the
verifying handshake originally evaluated them against the real clock — which made
the whole scenario a TIME BOMB that would start failing on 2036-01-01 for a reason
having nothing to do with the code under test. Both sides of the handshake now
pin `tls.Config.Time` to a fixed instant inside the window.

**The torn key is produced by an actual crash, not by writing a short file.** The
first draft of this section claimed the opposite — that `SimDisk.CrashHost` never
discards un-synced data, so the truncation had to be faked. That was a
restatement of the model rmp #2535 *replaced*: #2535 is the fix that made fsync
load-bearing, and since it landed each file carries a durable image advanced only
by a `Sync` that returned nil, with `CrashHost` reverting to that image (power
failure) and `CrashProcess` keeping the bytes (SIGKILL). The claim was corrected
by reading `internal/sim/disk.go` rather than by trusting the note, and the arm
now uses the real mechanism: write the prefix, `Sync` it, write the remainder,
leave it un-synced, `CrashHost`. Measured: 85 of 119 bytes survive, and the arm
fails loudly if a crash ever discards nothing. `SimDisk` is likewise the image
authority for the garbled arm (`SimDisk.CorruptRange`). Only the projection onto
a real temporary directory leaves the simulated disk, and it must: `CertReloader`
reads through `os.Stat` and `tls.LoadX509KeyPair` and exposes no filesystem seam,
so growing one purely for this scenario would change a production API rather than
test it. The precedent is `wal_writer_surface.go`.

Two details make the run reproducible and the roster honest. The fixtures are
**Ed25519 with fixed validity bounds**, so the PEM bytes are a pure function of
the seed; an ECDSA pair, whose signature draws randomness, would regenerate
differently every run. And because the torn and garbled arms produce the
IDENTICAL parse error (`tls: failed to find any PEM data in key input`), the
non-vacuity gate compares the key sizes each step left on disk — a torn key must
be strictly shorter than the key it truncates (measured 85 of 119 bytes), a
garbled one exactly as long (119 of 119), an absent one zero — so the scenario
cannot claim two faults where only one was ever applied. The gate also requires at
least one reload to have succeeded, at least one to have failed, and the
certificate in service to have genuinely changed, since a reloader that ignored
every rotation would pass every retention clause.

**A rotation that does not advance the mtime must still take effect — rmp
#2558.** `Reload` short-circuited on "neither file's mtime is After the last
successful load", which is a cheap heuristic for "nothing changed" and an unsound
one: mtime is not a content hash. Every rotation that replaces content without
advancing the timestamp — a rename from another directory, `cp -p`, a restore
from an archive, two rotations inside one filesystem timestamp tick — returned
nil having loaded NOTHING, and because the call reported success `onError` never
fired. A rotation performed to REVOKE a certificate could therefore be ignored in
silence while the operator believed the material had been replaced, which is the
worst shape this component can fail in. The skip is now keyed on a SHA-256 digest
of each file's bytes, recorded only on a load that succeeded, so a skip is
provably a no-op and a refused pair is always re-examined.

The skip was measured rather than assumed, and the measurement contradicted the
intuition that motivated it: 20.6 µs and 2.0 kB per call against 37.8 µs and
9.4 kB for the full load (best of 5, darwin/arm64,
`BenchmarkCertReloader_ReloadUnchanged` against
`BenchmarkCertReloader_ParseCostAvoidedBySkip`). The two file reads dominate BOTH
paths, so the skip is a 1.8x saving on a call that happens once per poll interval
— it is kept because it does not recompute and re-publish a certificate that has
not changed, not because it is fast.

The DST arm carries two structural guards, because its fault is a timestamp and
nothing else. The mtimes it leaves on disk are recorded in the evidence and may
not exceed the previous step's, or the rotation advanced the clock after all. And
it must FOLLOW a step whose reload SUCCEEDED: the fault is a timestamp
indistinguishable from "nothing has changed since the last successful load", so
placed after a refused step the same rotation would carry a newer timestamp than
the reloader's bookkeeping and even the defective code would have loaded it — the
arm would pass everywhere and prove nothing. Measured against the pre-fix code it
reports exactly the bug's shape: `preserved-mtime-rotation reload-ok
serving=rotation-C`, where `rotation-preserved` was expected.

One projection detail remains load-bearing and easy to lose: the mtimes are
stamped **explicitly**, one second apart, so the timestamp is a deterministic
property of the run rather than a property of the filesystem's granularity. Until
#2558 that mattered because an honest rotation could otherwise be skipped; it now
matters because it is what makes the preserved-mtime arm expressible at all.

### The transaction registry, the idle reaper and the per-principal cap (rmp #2482)

`Server.Transactions`, `Server.TerminateTransaction`, `Options.MaxTxIdleTime` and
`Options.MaxOpenTxPerPrincipal` were all added after the round-3 comparative
audit demonstrated a whole-server stall from one abandoned `BEGIN` (rmp #2175,
#2176). Six tests in `bolt/server` cover them, and between them they establish
what follows.

**What the six pre-existing tests already covered.**
`tx_introspection_test.go` covers the listing's FIELDS (id, principal, mode,
remote, query, state, a non-zero `StartedAt` and a positive `Elapsed`), that a
termination rolls back atomically, that a NEVER-SEEN id returns
`ErrNoSuchTransaction`, that an idle offender blocks neither a reader nor a
writer, and that both read and write transactions are listed oldest-first.
`abandoned_tx_test.go` covers the idle reaper bounding a reader stall, a BUSY
transaction NOT being reaped, the per-principal cap refusing with a typed error,
one slot being returned by a client `ROLLBACK`, and the cap being per-principal
rather than per-server.

**What a wall clock cannot reach, and what the fake clock adds.** All six run on
real time, and that is not a stylistic difference — it is a ceiling on what they
can assert:

- **Exact instants.** A test that does not know what the server's clock read when
  an entry was registered can assert no more than `Elapsed > 0`. Driving the
  server's clock through `Server.SetClock` makes the harness the sole author of
  every instant, so `StartedAt` is EXACTLY the fake instant it opened at and
  `Elapsed` is the listing instant minus that, to the nanosecond. A registry that
  stamped every entry at once, or that computed `Elapsed` against wall time while
  the clock was injected, passes `Elapsed > 0` and fails this.
- **An ordinal instead of a timescale.** `TestAbandonedTx_IdleReaperBoundsTheReaderStall`
  asserts an order of magnitude — the reader unblocked nearer the 300 ms idle
  bound than the 20 s total bound — because a real-time test cannot do better
  without becoming flaky. On virtual time the reap lands on a specific ADVANCE,
  predicted before the run by an independent model of the rule and compared
  exactly. A reaper one advance early or late is then a failure rather than
  noise, which is precisely the deviation the arm's live control produces on
  purpose by shortening the server's bound by one step.
- **Quiet ordinals.** Real time cannot assert that the reaper DECLINED to reap at
  a particular moment. The staggered plan gives five advances at the front of the
  measured sequence at which nothing may be reclaimed, so a reaper that emptied
  the registry on its first fire is caught.
- **Reaper-free attribution.** A termination test on real time cannot rule out
  that a bound fired instead. Arm 2 installs both bounds at ten minutes of FAKE
  time and makes ZERO advances; `clock.Fake` delivers only from `Advance`
  (`internal/clock/fake.go`), so no timer it armed can possibly fire and every
  departure from the registry is provably the operator call's doing. That is
  asserted as a non-vacuity clause rather than argued in a comment.
- **Minutes of transaction lifetime in milliseconds of wall time.** The scale arm
  drives 64 transactions through 70 advances — 13.3 s of simulated time — in
  412 ms of real time under `-race`, and the churn arm drives 3m20s of simulated
  time in 97 ms.

**Three properties the pre-existing tests do not reach at all.**

1. **Successor immunity.** `txRegistry` documents that a stale id can never
   terminate whatever transaction the same connection opened next, and nothing
   tested it. Two mechanisms implement it: `txRegistry.nextID` mints
   `"<sessionID>-<seq>"` from a server-wide counter that only ever increases, and
   `Session.unregisterTx` drains any terminate request that arrived for the
   transaction just ended. The arm exercises the first directly — the stale id is
   refused by the registry lookup, which sends no signal at all — and asserts the
   OBSERVABLE property for the second across a settle window, because the
   interleaving the drain exists for (a signal queued while the session is inside
   `HandleMessage`) is a scheduler outcome the harness cannot construct. The
   `ErrNoSuchTransaction` case the pre-existing test covers uses a hand-written
   id the server never minted; the two here were WATCHED live and then watched
   finish, one by termination and one by a client `COMMIT`.
2. **Who was refused, and at what number.** The cap test checks the failure CODE
   and that the message is non-empty. A code-only assertion is equally satisfied
   by refusing the wrong principal, or the right one at the wrong count. The
   quota arm RECOMPUTES the text from the principal and the limit it configured
   and requires an exact match, which is sound because `handleBegin` returns the
   quota error VERBATIM rather than through `Session.sanitiseErr`
   (`session.go:1604`) — unlike every neighbouring failure in that handler.
3. **The other three ways a slot comes back.** `abandoned_tx_test.go` covers a
   client `ROLLBACK`. The arm drives the idle reaper, `TerminateTransaction`, and
   a DE-AUTHORISED session's refused `COMMIT`, each ending in a `BEGIN` the cap
   must now allow. The third is the clause rmp #2482 carries over from the #2481
   security review, and it exists because no WAL or census oracle can see it: a
   refusal that left the transaction OPEN with its slot held and its registry
   entry live would write nothing either, so only the registry and the quota can
   distinguish "declined the message" from "reclaimed the transaction".

**One measurement worth recording.** `txRegistry.list` ranges a Go map — whose
iteration order is randomised — and insertion-sorts the result by `StartedAt`,
swapping only on a strict `Before`. Its cost therefore depends entirely on
whether the instants are DISTINCT, and the first version of the measurement in
the soak arm missed that: a fake-clock harness that never advances registers
every entry at one instant, the sort makes ZERO swaps, and the per-entry cost
came out FLAT with `n` — which looked like evidence the sort was cheap and was
evidence it had nothing to do. Measured with both arrangements, one
`Transactions()` call, no `-race`:

| open | same instant | distinct instants |
|---|---|---|
| 8 | 326 ns (40 ns/entry) | 314 ns (39 ns/entry) |
| 64 | 2.954 µs (46 ns/entry) | 11.839 µs (184 ns/entry) |
| 256 | 8.628 µs (33 ns/entry) | 143.153 µs (559 ns/entry) |
| 512 | 16.74 µs (32 ns/entry) | 599.715 µs (1.171 µs/entry) |

A production server's clock is real, so it is always in the second column: the
call is QUADRATIC in the number of open transactions, and 256 → 512 costs 4.19×
for twice the input. The comment above the sort justifies it by saying open
transactions are kept small, which was written when a writing transaction
serialised every other writer; rmp #2305/#2306 retired that, so the bound on
"small" is now `Options.MaxOpenTxPerPrincipal` (default 2048) times the
principals in play. Recorded, not fixed — it is outside this task.


### The graceful teardown: drain, Closer ordering, and what a RUN reply means (rmp #2483)

`Options.Closer` — the store-level teardown owner a Bolt server closes after its
connections drain — was passed by nothing in the module outside `bolt/server`'s own
tests. `SimServer.Close` only cancels the serve context, and the durable scenarios
close their store directly, so the ordering `store.DB` documents a Bolt server as
relying on was exercised end to end nowhere. Four deterministic arms and one
concurrent arm now drive it.

**The ordering is asserted on two observables, neither of them a timing guess.** A
`net.Conn` decorator counts accepted-and-not-yet-closed server connections; it is
one-sided by construction, because the connection handler's `conn.Close` runs
strictly before its `wg.Done`, so the count can lag but cannot claim a connection
finished before it did. And a rendezvous is CONSTRUCTED rather than waited for: a
commit is parked inside its WAL fsync with `SimDisk.ArmSyncGateAt`, and the closer
must have run zero times across a window in which the listener is already closed —
the listener flag being positive evidence that `Shutdown` has entered its drain
wait, rather than not yet started.

**Three measurements refute the obvious model of `Shutdown`.** Neither of its
failure branches closes the owned store: at the instant an expiring `Shutdown`
returned, the closer had run zero times (12/12 with a deadline, 12/12 with a
cancel), and the store is closed afterwards by `Serve`'s deferred exit path, once
the abandoned connections finish. It is never left unclosed in any reachable case
found. On a CLEAN drain, though, *who* closes is a genuine race: `Shutdown` cancels
the accept context before draining, so `Serve`'s exit path and `Shutdown`'s
drain-success branch wait on the same `WaitGroup` — measured **22 `Serve` / 3
`Shutdown`** over 25 successful drains. A `Shutdown` returning nil therefore does
not mean `Shutdown` closed the store, which is worth knowing for anyone reasoning
about teardown ordering from its return value.

**The third is a lesson about assertions, not about the server.** Which error a
DEADLINE-bounded `Shutdown` reports is also a race: it clamps its drain timeout to
`time.Until(deadline)` and then selects over both that clamped `time.After` and
`ctx.Done()`, which come due at nearly the same instant, and Go's select is uniform
when both are ready. The distribution is heavily skewed to the drain timeout —
measured 12 of 12 when the arm was written, and 8 of 8 in a later sitting — and the
arm PINNED that branch on the strength of it. Once the other branch surfaced under
`-race`, that pin and its siblings made **5 of 6 `-race` runs of the file red**,
each time on a different test. Both branches are now legal, the distribution is
reported rather than asserted, and the deadline arm is excluded from the
determinism clause because that field is not a function of its seed. An assertion
that holds twenty times and then fails is worse than one that never held, because
by the time it breaks it is trusted.

**One clause could not be written as the task posed it.** "No `wal.ErrWriterClosed`
reaches a client" is unfalsifiable as stated: it never can reach one, because
`Session.sanitiseErr` replaces the text of any error that is not client-fault and
`FailureCode` maps it to the catch-all — measured, a client whose store is closed
under it receives `Neo.DatabaseError.General.UnknownError` and a message naming
only a crypto-random session id. The oracle is therefore split in two: on the wire,
no statement on an undrained connection may receive a DatabaseError-class code; at
the store, `errors.Is(err, wal.ErrWriterClosed)` is checked on a commit attempted
after the teardown — which is simultaneously the proof that the WAL really closed
and the proof that the detector is not blind.

**A RUN SUCCESS is not the durability acknowledgement for an auto-commit write.**
This is the most transferable thing the task produced, and it arrived as two
harness defects rather than one engine defect. The concurrent arm reported an
`ACID_DURABILITY` violation in 4 of 25 runs; the lost row always had the same
signature — `RUN` answered SUCCESS, the terminal never arrived, the connection cut
— and the name was in neither the live engine nor the raw WAL bytes, with the WAL
image fully durable. Nothing had been made durable, so nothing acknowledged had
been lost. `handleRun` replies SUCCESS whenever the engine returns no error and
never consults `Result.Err()`; its metadata is `fields`, `qid` and `db` — statement
accepted, here are your columns. The BOOKMARK, which is what a driver uses to
establish that a write landed, rides on the terminal `PULL`/`DISCARD` SUCCESS and
on `COMMIT`, and is absent from `RUN`. When a graceful shutdown cancels an
in-flight statement, `commitUnderBarrier` early-returns on the materialise error,
appends no WAL frame, and the client that already holds its RUN SUCCESS is told
nothing further.

The same file had already made the same class of mistake once, in a way worth
recording beside it: it counted a `*proto.Ignored` — the reply a session in FAILED
gives every request-phase message until it is RESET — as an acknowledgement,
manufacturing the identical violation in 8 of 30 runs. Both are one rule: **an
acknowledgement is an explicit terminal SUCCESS and nothing else.** The RUN reply
and any IGNORED are kept as witnesses, because they are what separates "never
dispatched" from "dispatched, outcome unknown to the client", and the parked
in-flight commit is adjudicated on the invariant that does hold for it — the
statement the drain found executing must have RUN and must be DURABLE, read from a
reopen through real recovery rather than from any reply. That also closes the escape
hatch an absence-only oracle would leave: a drain that abandoned every in-flight
write would satisfy "nothing acknowledged was lost" by acknowledging nothing.

**A write cut short by a graceful shutdown can be reported as non-retryable.** The
session's own checks answer `Neo.TransientError.General.RequestInterrupted`, but a
cancellation surfacing from the engine is mapped to
`Neo.ClientError.Transaction.Terminated` — and this module already documents that
`neo4j-go-driver` v5.28.4's `reclassify()` demotes `Transaction.Terminated` out of
the retryable family. Both codes are pinned as named constants so a correction fails
the arm deliberately.

### Streaming semantics: PULL n paging, DISCARD, and the qid that routes nothing (rmp #2484)

Every result stream this package had ever opened was drained with a single
`PULL {n:-1, qid:-1}`. The consequence was not simply that paging was untested: no
`PULL` had ever carried a finite `n`, so `has_more` had been false on every reading
the harness ever took and never once observed true; `DISCARD` did not appear
anywhere in `internal/sim`; and no arm had ever addressed a stream by an explicit
qid. Three server paths — `handlePull`'s `n` limit and its look-ahead peek,
`handleDiscard`'s own `n` accounting, and the qid validation both share — were
reachable only from `bolt/server`'s unit tests.

**The task's premise was half wrong, and the refutation is worth more than the
scenario would have been.** It asked for "QID multiplexing" and "QID routing":
several open result streams on one session, addressed by qid. This server has
neither, and cannot, and each limb was verified in the code rather than inferred
from a passing test:

- `handlePull` refuses any `qid >= 0` outright, answering
  `Neo.ClientError.Request.Invalid` with `no such query: qid %d`
  (`bolt/server/session.go:1240-1243`). `handleDiscard` carries the identical guard
  (`:1421-1424`).
- RUN's SUCCESS always reports `"qid": int64(-1)` (`:1223`), so no positive qid is
  ever minted for a client to send back.
- A second RUN while a stream is open is refused by the state machine: `handleRun`
  requires READY or TX_READY (`:1075`), and a live stream leaves the session in
  STREAMING or TX_STREAMING (`bolt/server/state.go:181-228`, `:230-277`).

There is therefore exactly ONE open stream per session at any instant, and
"routing" is a property to REFUTE rather than to test. The scenario asserts the
refutation instead of arguing it: every RUN reply in the run is inspected and must
report `qid = -1` (26 readings at the catalogue seed, distinct set exactly `[-1]`),
and both refusals are pinned to their exact code AND exact message text.

What DOES exist — and is the honest reading of the objective — is that cursors
ACCUMULATE across SEQUENTIAL RUNs inside one explicit transaction. Each RUN appends
a cursor to `tx.results` (`bolt/server/tx.go:135` and `:140`), the slice is cleared
only by `Tx.closeCursors` on COMMIT or ROLLBACK, and
`Options.MaxInFlightPerConnection` is the bound (`session.go:518-526` counts it,
`:1086` refuses past it). Nothing in the harness had ever passed that option, so the
only cap the DST could have reached was the server's own default of 1024 cursors
deep inside one transaction — neither a short-layer budget nor a legible report.
`NewSimServerInFlight` now sets it, and REFUSES a non-positive value rather than
defaulting it, because passing zero through would silently hand a cap-driving
scenario a cap of 1024 and the refusal it then failed to observe would read as a
pass.

**The load-bearing oracle is an independent reference drain.** The same query is
drained twice, on two connections: once with a single `PULL -1`, which is the
reference record set, and once with a seed-drawn sequence of `PULL n` pages. The
concatenation must equal the reference ELEMENT BY ELEMENT and IN ORDER, compared
through the package's existing `compareWireRow`, which compares the decoded value
AND its concrete Go type because the dynamic type IS the wire encoding. That
distinction is load-bearing rather than decorative, and the falsifiability table
proves it: replacing `int64(41)` with `float64(41)` — three identical characters
under any `String()` rendering — fires the equivalence clause. The reference query
spans five PackStream encodings (Integer, String, Boolean, List, Float) and touches
no node, so every value it yields is a pure function of the query text with no
created-node internal key anywhere in the rows.

The partial-DISCARD arm sharpens equivalence into an exact statement about WHICH
rows were skipped. A seed-drawn prefix is paged, a seed-drawn window is DISCARDed,
the remainder is pulled, and prefix++remainder must equal the reference with exactly
that window cut out of it. A DISCARD that dropped one row too many, or one too few,
shifts the suffix and fails here; "the session still works afterwards" could see
neither. A controlled revert confirms it end to end: changing `handleDiscard`'s loop
bound from `discarded < n` to `discarded <= n` takes the scenario to exit 1 with
`prefix(21 rows)++suffix after DISCARD n=7 delivered 21 row(s), want 90`.

Measured at the catalogue seed: 12 pages from the plan
`[12 5 3 11 5 5 8 16 11 3 16 8]`, `has_more` 11 true / 1 false, the bookmark present
on exactly the terminal page and on no other, a window of 21 rows paged over 2 pages
with `DISCARD n=7` and a 69-row suffix, and the `qid = -1` control served all 97
rows.

**DISCARD abandons delivery, not the statement.** An autocommit write commits during
the DRAIN rather than at RUN (`session.go:1144-1148` explains why the statement's
deadline is held across the drain for exactly that reason), and `handleDiscard`
drains the cursor with the same `s.result.Next()` loop PULL uses (`:1453-1458`). So
the interesting question was never whether DISCARD is safe but whether it silently
drops the write along with the rows. Measured: it does not. The DISCARD delivers
ZERO records, its terminal SUCCESS still reports
`nodes-created=1 labels-added=1 properties-set=1 contains-updates=1` — the write
counters being the only route by which the effect can reach a client that took no
rows — and the node is present both in the live engine and in a graph reopened
through real WAL recovery after a crash.

**Two gates share one failure code, so the refusal is attributed by ORIGIN STATE.**
`handleRun`'s authentication gate (`:1072`) and its state gate (`:1075`) both return
`failTransition`'s `Neo.ClientError.Request.Invalid`, so a code match cannot say
which one refused — the discipline rmp #2481 established. `failTransition` reports
the ORIGIN state (`:1885`), which is the discriminator, and the needle is the whole
`in state X` phrase rather than the bare state name: **`TX_STREAMING` contains
`STREAMING` as a substring**, so a containment check on the name alone would let a
TX_STREAMING refusal satisfy the STREAMING clause, which is precisely the confusion
the attribution exists to prevent. A controlled revert moving `origin := s.state` to
after `s.enterFailed()` — a plausible refactoring mistake — takes the scenario to
exit 1 on `refusal-origin-state` and `refusal-message` alone.

**Both refusals POISON the session, and that was measured rather than assumed.** A
qid refusal routes through `failWith` → `enterFailed`, and the next request-phase
message on that connection draws `*proto.Ignored`, not a FAILURE and not a SUCCESS.
Only RESET restores it, after which a RUN+PULL is acknowledged again. Both facts are
asserted, which matters twice over: an IGNORED is a refusal, so a helper that
treated "not a FAILURE" as an acknowledgement would let every "the session is still
usable" clause in the file pass on a poisoned connection. A dedicated test drives a
real server into that state to prove the helper refuses it.

The in-flight arm runs two transactions against a WAL-backed store, each bracketed
by two readings of `wal.Writer.Stats`:

- **Under the cap.** Exactly `MaxInFlightPerConnection` RUN+PULL cycles accumulate
  and the transaction COMMITs. Measured frames +10, three nodes live and three
  recovered. This half is the non-vacuity witness in two senses: it proves the cap
  admits accumulation up to its bound, and it is the run in which the frame counter
  is observed MOVING, without which "the doomed transaction appended no frame" would
  be a statement about a dead instrument.
- **Over the cap.** The same cycles, then one more RUN, which must draw
  `Neo.TransientError.Transaction.MaximumTransactionLimitReached` naming `cap=3, open=3`.
  The `open=` figure
  is parsed back out of the message and cross-checked against the harness's own
  cycle count, so two independent accountings of the same quantity must agree. The
  decisive RUN runs under an armed read deadline, so a stall becomes a harness error
  instead of a silent pass — the "backpressure or a typed error, never a block"
  mandate stated as an observation rather than as a hope. Measured frames +0 and
  bytes +0 across the whole arm, with the staged nodes absent both live and after
  recovery: the cap breach moves the session to FAILED, which rolls the transaction
  back.

**Frame counts are seed-pure; byte totals are not, and the rendering respects the
difference.** A created node's hidden internal key is minted by `cypher/exec` as
`"__cx_"+hex(n)` from a PROCESS-GLOBAL counter, so the same seed yields frames of
different widths depending on how many nodes every other test in the process created
first — the limitation already documented for `bolt-auth` and `schema-mutation`. The
evidence rendering therefore carries frame counts for both halves but a byte total
only where the expected value is ZERO, since zero is zero at any width, and every
map it walks is walked in sorted key order. Two runs of one seed render
byte-identically.

**The controls are real alternative configurations, not doctored values.** The
identical `PULL`/`DISCARD` message with `qid = -1` must be SERVED the whole record
set, which is what pins the refusals on the qid rather than on the message type, the
framing, or a typo in the harness. And the identical cap script with only
`MaxInFlightPerConnection` raised must stop being refused, which pins the refusal on
the cap rather than on explicit transactions, sequential RUNs, or the `CREATE`
statement — every one of which would refuse the raised-cap run too.

**The concurrent arm reuses the existing slow-consumer actor**, and one of its
oracles had to be demoted after reading the harness's own code. `halfPipe.write`
chunks every write to the space remaining (`internal/sim/simconn.go:96-124`), so the
queue can NEVER exceed `simConnBufferSize`: "the server did not buffer past the
bound" is an invariant of the pipe, not a property of the server, and a clause
asserting it cannot fail against a real server. It is kept as a labelled guard on
the harness itself, and the server-side HEAP bound — that a page is not materialised
into a second in-memory copy ahead of the wire — is left where it can actually be
measured, the live-heap gate in `bolt/server/streaming_backpressure_test.go`, rather
than restated where it cannot be. The reading also had to be CONSTRUCTED rather than
sampled: a single `ReadBuffered` call at the instant the consumer stalls was MEASURED
at 0 bytes on 2 of 3 seeds, and a bound asserted against 0 is a bound asserted
against nothing, so the arm polls until the queue is full and reports the peak
(65536 of 65536 on 9 of 9 runs under `-race`). What the arm then asserts is only what
every interleaving shares: a non-empty PROPER prefix reached the consumer, the writer
was provably blocked when the connection was torn down, the teardown leaked no
goroutine, and a FRESH connection's paged drain still matched plain range arithmetic
with `has_more` true on exactly its non-final pages.

**The coverage gate returns `Violation`s rather than the `[]string` rmp #2554
demoted the MERGE and FOREACH gates to**, and the distinction is deliberate. Those
gates reported a shortfall when a SEEDED WORKLOAD happened not to drive a branch,
which is an uninformative run and not a defect. Every precondition here is
CONSTRUCTED: each arm runs by rule on its own connection, and the draws are bounded
so that a discard window is always strictly interior (at most four prefix pages of at
most 16 rows leaves at least 33 of the 97 behind, against a window of at most 16) and
a paged drain always takes at least `ceil(97/16) = 7` pages. A shortfall therefore
means the harness itself stopped exercising the surface, which must fail loudly, as
it does for the constructed battery of rmp #2483. The soak sweep runs 400 seeds and
was clean on all 400.

### BEGIN extras: bookmarks, tx_timeout, metadata, mode, db and ROUTE (rmp #2485)

The harness had exactly one way to open an explicit transaction — `WireClient.Begin`,
which sends BEGIN with an EMPTY extras map. rmp #2482 added `WireClient.BeginMode`
for the single key `mode`, and the three scenarios that used it sent only the two
canonical spellings `"r"` and `"w"` (`internal/sim/bolt_tx_quota.go:696`,
`bolt_tx_registry.go:1058-1070`, `bolt_tx_terminate.go:486-496`). Everything else a
real driver puts in those extras was driven by nothing at all: no BEGIN or RUN
anywhere in `internal/sim` had carried a `bookmarks` list, a `tx_timeout`, a
`tx_metadata` map or a `db` name. rmp #2484 reads the `bookmark` key back OFF a
terminal SUCCESS and pins its presence to exactly the terminal page
(`internal/sim/bolt_stream_semantics.go:1150`), but nothing had ever SENT one, so
nothing in the module distinguished a server that honours the token from one that
ignores it. ROUTE had one call site in the package, rmp #2481's `route-after-logoff` arm
(`internal/sim/bolt_auth_surface.go:671`), which sends the ZERO message on a
DE-AUTHORISED session and requires it to be REFUSED — so `handleRoute` had never once
produced a routing table under simulation, and its payload was reached by nothing.
`WireClient.BeginExtras` (`internal/sim/wireclient.go:374`) is the new primitive, and
`Begin` and `BeginMode` now both route through it.

**This server does not honour an incoming bookmark, so the arm that looks like a
causality test proves nothing on its own.** `server.ExtractBookmarks` has exactly two
non-test call sites in the module — `bolt/server/session.go:1099` (RUN) and `:1529`
(BEGIN) — and neither does anything with the result but write it to a Debug log. The
RUN site says so in as many words: "Log any incoming bookmarks for observability;
single-host server ignores them for causal consistency but they should not be
silently dropped" (`:1097-1098`); the BEGIN site carries the shorter "Log incoming
bookmarks for observability" (`:1528`). The extractor validates nothing — it reads
the `bookmarks` key, keeps whichever elements assert to `string`, and returns nil
when none do (`bolt/server/bookmark.go:28-47`).

A reader on a second connection that presents the writer's bookmark and then sees the
write has therefore seen it for a reason that has nothing to do with the token it
sent. A single-host server has ONE store, and a committed write is already visible to
every later read of it: the property a bookmark exists to provide holds here
unconditionally. "The causal read observed the write" is a TRUE assertion that proves
nothing, and an arm that stopped there would be exactly the vacuous shape this sprint
has hit repeatedly.

The scenario makes the assertion honest by driving the SAME causal read five ways, on
five separate connections in a seed-drawn order, and requiring the five to be
indistinguishable:

- with the writer's REAL bookmark, which is what a driver actually depends on;
- with a FABRICATED far-future token, `FB:kffffffff`, whose counter is separately
  asserted to be strictly above every bookmark the server did issue, so "the server
  never minted this" is an observation rather than an assumption;
- with a token that is not of the shape this server mints at all
  (`not-a-gograph-bookmark`), which the extractor nevertheless keeps, and which is
  therefore logged like any other, because it validates nothing;
- with a `bookmarks` list whose single element is an `int64`, which the extractor
  filters out, so the server sees ZERO tokens rather than one it cannot parse;
- with no `bookmarks` key at all, the baseline.

All five must be ACCEPTED, must observe the identical count, and must reply inside a
real-time bound. That a token the server never issued is accepted is the evidence
that the token is IGNORED rather than honoured, and it is what makes the first arm's
meaning honest: a server that honoured bookmarks and was handed a far-future one
could only block until its own counter reached it — caught by `bookmark-does-not-wait`
— or refuse it — caught by `bookmark-accepted`. Either way the pin fires deliberately
and this section has to be rewritten, which is the point of the pin.

This is pinned INTENDED behaviour, not a defect, and it belongs here rather than in
the defect list below. What is new is not the behaviour but that anything in the
module now asserts it: before this task, nothing distinguished "ignored" from
"honoured", so a change in either direction would have gone unobserved.

**What a bookmark IS here, and where it arrives, was measured rather than described.**
`server.NextBookmark` (`bolt/server/bookmark.go:20-23`) returns `"FB:k"` followed by a
process-global atomic counter (`:13`) as eight zero-padded hexadecimal digits. It is
assigned in exactly ONE place — `s.bookmark = NextBookmark()` in `handleCommit`
(`bolt/server/session.go:1694`) — and delivered in three: the COMMIT SUCCESS, whose
metadata is that bookmark and nothing else (`:1696`); the terminal PULL SUCCESS, the
one with `has_more` false (`:1397`); and the terminal DISCARD SUCCESS (`:1500`). Since
rmp #2484 established that the terminal reply is also the durability acknowledgement,
the bookmark rides on the ack.

Two consequences follow from "assigned only in `handleCommit`", and both are pinned
because both are what a driver sees:

- On a session that has never committed an explicit transaction, an AUTOCOMMIT
  write's terminal PULL SUCCESS carries the EMPTY string in its `bookmark` field —
  measured, on a reply whose own `stats` map in that same SUCCESS reports
  `contains-updates`. The stats reading is what stops the empty bookmark being
  explained away as "the statement wrote nothing".
- On a session that HAS committed one, a later autocommit write's terminal PULL
  SUCCESS carries that EARLIER transaction's bookmark — measured EQUAL to it, not
  merely similar. A driver chaining causality off an autocommit `ResultSummary` is
  therefore chaining a strictly earlier transaction's token.

Neither is asserted anywhere in `bolt/server`: the only bookmark-key assertion in that
package is on a COMMIT SUCCESS and checks existence and non-emptiness alone
(`bolt/server/tx_test.go:82-88`).

**The bookmark VALUE is not reachable from the seed, and the rendering respects the
difference.** `bookmarkCounter` is process-global (`bolt/server/bookmark.go:13`),
exactly like the `"__cx_"+hex(n)` node key that bounds the authentication surface's
byte oracle, so the literal text of an issued bookmark depends on how many
transactions every other test in the process committed first. Every clause is
consequently written over a DERIVED relation — equality between two observed
bookmarks, a strict advance between two successive ones, an ordering against the
fabricated counter — and never over a literal value; and the evidence rendering emits
an issued bookmark purely positionally (`#0=<issued>`, `#1=<issued>`), so two runs of
one seed render byte-identically.

That rendering originally carried the ADVANCE between consecutive counters
(`#1=<issued,+1>`), on the reasoning that the advance, unlike the absolute value, is
seed-determined. It is not. The advance is one only while nothing else commits in
between, and `sim -swarm -workers N` runs N scenarios concurrently in ONE process
(`internal/sim/swarm.go:271-278`), so a concurrent COMMIT inflates it. MEASURED over
six fixed seeds, the advance read `+1` at `workers=1` and `+5`, `+6` or `+7` at
`workers=6` — six of six seeds rendering differently at the two worker counts — and
with the advance dropped, sixteen of sixteen seeds render identically at `workers=16`.
`TestBoltBeginExtras_Deterministic` could not have caught it, because it compares two
SERIAL runs. The property was never in the rendering's gift anyway:
`bookmark-strictly-advances` adjudicates the relation `n > prev` between two OBSERVED
counters, which survives any interleaving.

**`tx_timeout` is attributed by its CONTROL, not by its subject.** Four arms run in a
FIXED order, each against its own server on its own fake clock
(`internal/sim/bolt_begin_extras.go:1265-1306`); every advance is virtual, and no arm
depends on wall time for its outcome, only for its bound. The order is fixed rather
than drawn because an arm and its control are comparable only as the same script.

- `client-tx-timeout`, the subject, asks for a 100 ms `tx_timeout` with the idle bound
  and the server's default total bound both lifted to 10 virtual minutes, and a single
  advance of exactly that bound must reap it. Lifting both is load-bearing: the serve
  loop reaps at the EARLIER of the two bounds (`effectiveTxDeadline`,
  `bolt/server/serve.go:1155-1167`, established by rmp #2482), so an arm that left the
  idle bound at its default would be timing the wrong reaper. The non-vacuity gate
  re-derives that separation from the arm's own recorded bounds rather than trusting
  the plan.
- `no-tx-timeout-control` is THE attribution. It is the byte-identical script with the
  `tx_timeout` key removed, given the identical advance, and it must both survive and
  COMMIT. Without it, "advance and the transaction died" is satisfiable by any timer
  at all; with it, the single difference between the two arms is the extra.
- `idle-bound-control` is a CONSTRUCTED collision: the same reap reached through the
  IDLE bound instead. It differs from the subject in TWO fields, not one — the idle
  bound is the small one AND `tx_timeout` is not sent — and the second is forced by the
  first, since leaving the client's bound in place would arm a total-lifetime deadline
  at the same instant and the arm could no longer attribute its reap to the idle
  reaper.
  The checker requires its code AND its message to be byte-identical to the subject's,
  which is how the shared-failure finding is ASSERTED rather than restated. Reading
  `bolt/server` widened rmp #2560 from two paths to three — the idle reaper, the
  total-lifetime reaper and `Server.TerminateTransaction` all funnelled through one
  teardown that armed a single `pendingTermErr`, so a client could tell none of the
  three apart.
  **rmp #2560 split that PARTLY, and the surviving half is what these arms now pin.**
  The operator path was given its own reason (`Session.terminateTxByOperator`, pinned
  separately as `txTerminateFailureCode`/`txTerminateFailureMessage` and adjudicated by
  the terminate arm); the two DEADLINE bounds still share one, deliberately, because a
  client's correct response to either is identical whereas an operator's is not. The
  idle-bound control therefore still asserts a byte-identical answer — but it now
  asserts a two-way collision that is intended, rather than recording a three-way one
  that was not.
- `overflow-tx-timeout` is the hostile arm, and its mechanism is worth stating exactly
  because the obvious reading of it is wrong. It sends `tx_timeout = 1<<62`
  milliseconds, and that value never reaches a multiplication: `clientMillisToDuration`
  (`bolt/server/session.go:460-465`) returns `(0, false)` for
  `ms <= 0 || ms > maxClientTimeoutMillis`, and `maxClientTimeoutMillis` is
  `math.MaxInt64 / int64(time.Millisecond)` = 9,223,372,036,854 ms, about 2,562,047
  hours (`:452`), which 1<<62 = 4,611,686,018,427,387,904 exceeds by a factor of
  500,000. `handleBegin` treats `(0, false)` as "unset" and leaves the server default
  in force (`:1543-1555`), so an out-of-range client bound is SILENTLY IGNORED. The
  guard is what makes it silent rather than catastrophic: had the multiplication
  happened, 2^62 x 10^6 is 2^68 x 5^6, a multiple of 2^64, so the int64 product would
  be exactly ZERO — a non-positive duration that would leave `txDeadline` unset and
  DISABLE the reaper altogether. The arm asserts the outcome in BOTH directions with
  two advances of half the 100 ms server default each: the first must not reap, and
  the second must. An arm that only advanced past the default could not tell "the
  default is in force" from "a shorter bound is".

**The abort is typed, delivered ONCE, and then the session ignores.** Every reaped arm
is adjudicated by one shared checker, so the three cannot drift apart, and the checker
is skipped entirely for an arm that was not reaped — an arm with nothing to report
must not be able to satisfy a clause about a report. It pins the exact code
`Neo.ClientError.Transaction.TransactionTimedOut` and the exact message "the
transaction has been terminated because it exceeded its timeout" against the named
constants `txReapFailureCode`/`txReapFailureMessage` (`internal/sim/bolt_tx_registry.go`)
— the message shed its trailing "; the writer lock was released" in rmp #2560, since
rmp #2305/#2306 had retired the hold that clause named; it requires the SECOND request-phase
message after the abort to draw `*proto.Ignored`, because `pendingTermErr` is
delivered on the first such message and cleared there
(`bolt/server/session.go:594-597`), after which the switch falls through to
`&proto.Ignored{}` (`:599`); it brackets, in REAL time, the interval from the reaping
advance to the abort reaching the client, so a stall reads as a failure rather than as
a pass; and it requires the injected clock to have registered at least one timer,
because a reap is attributable to the reaper only if the reaper was armed.

**The `mode` coercion FAILED OPEN. This scenario measured it, and rmp #2564 fixed
it.** `handleBegin` selected read-only for the exact string `"r"` and for nothing
else: a non-string value, a misspelling, the uppercase `"R"` — every one of them
silently yielded a WRITE transaction, so a client that asked for read-only received
write authority and was told nothing. That is a fail-open coercion on a field this
server treats as a capability restriction, which the project's
fail-stop-never-fail-silent rule forbids.

**The contract since rmp #2564:** the two canonical spellings and the ABSENT key
behave as before, and any other value is REFUSED at the BEGIN with
`Neo.ClientError.Request.Invalid`, in a message that NAMES the offending value. The
decision was taken on evidence rather than preference — the Bolt specification is
silent on invalid `mode` values and frames the field as a routing hint ("what kind of
server the RUN message is targeting") rather than as authorisation, so the contract is
GoGraph's to choose, and it chose the fail-stop direction its own rules require.
Compatibility risk is low by construction: the official drivers send exactly `"r"` or
`"w"`, so a refusal can only reach a client that was previously being granted write
authority it did not ask for.

Five arms drive `"r"`, `"w"`, `"R"`, `"bogus"` and no key at all, in a seed-drawn
order. Each accepted arm is adjudicated on two observables — the server's own
`server.TransactionInfo.Mode`, read off `Server.Transactions()` while the transaction
is open, and whether a `CREATE` inside it is accepted — because one alone could not
tell a mis-recorded mode from a mis-enforced one. For a REFUSED arm the second
observable is that no transaction reached the registry at all. The read-only arm's
refusal is pinned to the exact code `Neo.ClientError.Request.Invalid`, while its
message is required only to CONTAIN "read-only transaction": that text comes from
`cypher`, not from `bolt/server`, so pinning it verbatim would couple the scenario to
a message the engine owns. No earlier scenario had ever attempted a write inside a
Bolt read-only transaction, so that refusal is new coverage too.

The clause that pinned the fail-open said, in its own failure message, that a refusal
would mean the coercion had been hardened and that it and this document must be
rewritten. Both were. The clause is INVERTED rather than deleted: accepting an
unrecognised mode is now an `ACID_CONSISTENCY` violation, and a non-vacuity clause
requires at least one arm to have been refused, so the new contract cannot go
unexercised.

**`db` is echoed unvalidated, agrees across both replies, and is never empty.**
`selectDatabaseFrom` (`bolt/server/session.go:322-324`) records the extra verbatim,
and `databaseName` (`:309-317`) reports it, falling back to the server's own name and
then to `DefaultDatabaseName`, which is `"neo4j"` (`bolt/server/serve.go:195`). A name
this server does not serve is therefore ECHOED rather than refused with
`Neo.ClientError.Database.DatabaseNotFound`, which `Options.DatabaseName`'s own godoc
states deliberately (`bolt/server/serve.go:308-322`): GoGraph serves one graph per
server, so the name is a label and not a selector. Four arms drive `"neo4j"`, a foreign
`"not-this-server"`, `"system"` — a name that in Neo4j is a real and distinct database
and here is echoed like any other label — and no key at all. Each pins the echo; pins
that the RUN SUCCESS and the terminal PULL SUCCESS report the SAME name, because a
driver reads it off whichever one it consumes; pins that the reported name is never
empty, which is the rmp #2172 guard (the official driver returns a nil `DatabaseInfo`
for an absent or empty `db`, so the idiomatic `summary.Database().Name()` panics inside
the driver); and pins that the COMMIT SUCCESS carries the bookmark and NOTHING else, so
a widening of that reply is noticed rather than absorbed. The name is sent on BEGIN and
not on the following RUN, which is what a real driver does inside an explicit
transaction and what `handleRun`'s `if !s.txActive` guard
(`bolt/server/session.go:1134-1142`) makes safe: were that guard absent, the RUN's
empty extras would CLEAR the selection BEGIN recorded, and this arm is what would see
it.

**`tx_metadata` is accepted and echoed nowhere, which is asserted instead of a round
trip that does not happen.** The key is read in no file under `bolt/`: a sweep of every
`.go` file in the module finds it only in this task's own files, the catalogue entry
and `WireClient.BeginExtras`'s godoc, so unlike `bookmarks` it is not even logged.
`docs/bolt.md:225-226` already claimed "accepted in `BEGIN`/`RUN` extras and silently
ignored; the server stores and echoes no transaction metadata", and nothing drove it.
The arm sends two keys on BEGIN and then requires the BEGIN SUCCESS, the terminal PULL
SUCCESS and the COMMIT SUCCESS each to carry none of them.

**The ROUTE payload is compared against an INDEPENDENT reference, not against a
constant restated.** `handleRoute` (`bolt/server/session.go:1728-1753`) reads nothing
whatever from the message — not its `Routing` map, not its `Bookmarks`, not its `DB`.
Past the authentication gate (`:1745-1747`) and the state gate (`:1748-1750`) it
answers `RoutingTable(s.localAddr)` (`:1751`), a table whose TTL is a hardcoded 300
seconds, whose own `db` is the EMPTY string, and whose three roles WRITE, READ and
ROUTE all point at the one address (`bolt/server/route.go:11-33`). Two ROUTE messages
are sent on one connection, one carrying a routing context, a bookmark and a database
name, and one the zero message. Their rendered tables must be IDENTICAL, which is the
assertion that all three fields are dropped; and the populated request's table `db`
must be empty, which pins that a ROUTE naming a database is answered by a table
labelled with nothing. ROUTE's bookmarks are dropped without even the Debug line RUN
and BEGIN give theirs. rmp #2481 covered ROUTE's authentication gate and deliberately
left the payload here.

The address clause is where the independence matters. The table is built from
`s.localAddr`, which the accept loop copied off the listener —
`localAddr = s.ln.Addr().String()` at `bolt/server/serve.go:1000-1005`, handed to
`newSession` at `:1006` — and the checker compares every advertised address against
`SimServer.ListenerAddr()` (`internal/sim/simserver.go:474`), which reads
`s.ln.Addr().String()` on the harness side. The two reach the same source of truth by
different routes, so "the table names THIS server" is a comparison of two independently
obtained values. A checker that compared the reply against `server.RoutingTable`'s own
output would be comparing that function with itself. The non-vacuity gate additionally
fails the run when the listener reports an empty address, because every advertised
address would then be compared against `""`.

**The non-vacuity family is a separate function because it answers a different
question.** `checkBoltBeginExtras` asks whether the server misbehaved;
`checkBoltBeginExtrasNonVacuity` asks whether the run was in a position to notice.
Between them they carry 36 named contract clauses and 22 `nv-` ones. Both feed the SAME
report, so a coverage shortfall FAILS the scenario exactly as a contract violation does
— the `Violation`-returning discipline rmp #2484 adopted, rather than the advisory
`[]string` rmp #2554 demoted the MERGE and FOREACH gates to — but every `nv-` message
names what the run failed to construct instead of accusing the server. Every
precondition here is CONSTRUCTED rather than left to a seeded workload, so a shortfall
means the harness itself stopped exercising the surface. What the gate refuses to let
pass: a causal read of ZERO nodes, which could not distinguish "the reader saw the
write" from "the write never happened" (the writer's node count is drawn from [2, 6],
so a positive count is guaranteed by construction); fewer than one real and two
fabricated tokens, without which "the token changed nothing" compares nothing; fewer
than two arms that completed a read, which would compare a value with itself; fewer
than two issued bookmarks, because one event is not a sequence a strict-advance clause
can falsify; an EMPTY reference bookmark, which would collapse the
not-stale autocommit comparison into "both are empty"; a timeout family with no reaped
arm or no surviving
one; any timeout arm whose injected clock registered no timer, which is what separates
"the reaper declined" from "there was no reaper"; a `client-tx-timeout` whose server
bounds are not strictly beyond its advance; a mode family missing either the read-only
or the write side, or in which no write was ever accepted; a database family with no
foreign name or no absent-key arm, without which an echo and a fallback are the same
observation; an empty listener address, an unnamed database in the ROUTE request, an
undecoded routing table, or an empty table rendering; and a `tx_metadata` arm that sent
no keys, which would make "no reply echoed one" vacuously true.

Measured at the catalogue seed 612741132: 3 nodes under `:BeginCausal` committed across
2 transactions; all five causal arms accepted and each observing 3 of 3, with
`ExtractBookmarks` keeping one token on three of them and zero on the other two; the
autocommit bookmark `FB:k00000003` on a fresh session and `FB:k00000005` on a session
that had already committed `FB:k00000004` — a freshly minted token in both cases, on a
reply reporting `contains-updates`. **Re-measured after rmp #2563**, which fixed this:
the same seed previously read EMPTY on the fresh session and equal to the prior
COMMIT's token on the other; the four
timeout arms reaped after advance ordinals 0, never, 0 and 1 respectively, each with
exactly one timer armed on its injected clock, and all three reaped arms answering the
byte-identical code and message with IGNORED on the message after it; the registry
reporting mode `"w"` for `"w"` and for the absent key with the write ACCEPTED in both,
`"r"` for `"r"` with the write refused `Neo.ClientError.Request.Invalid` / "cypher:
write or DDL statement not allowed in a read-only transaction", and `"R"` and `"bogus"`
REFUSED at the BEGIN with `Neo.ClientError.Request.Invalid`, opening no transaction at
all. **Re-measured after rmp #2564**, which fixed this: the same seed previously read
mode `"w"` with the write ACCEPTED for `"R"` and `"bogus"` too; `db` reported as `neo4j` for the arm that sent no key and as
`not-this-server`, `neo4j` and `system` verbatim for the three that named one, with
the COMMIT SUCCESS carrying exactly `[bookmark]` in all four; the
routing table advertising `[WRITE READ ROUTE]` over three entries at the listener's own
address with `ttl=300` and `db=""`, the populated and zero ROUTE answered identically;
and the `tx_metadata` BEGIN SUCCESS carrying no metadata keys at all, its terminal PULL
carrying `[db has_more]` and its COMMIT `[bookmark]`, none of them a key the client
sent. **Re-measured after rmp #2563**: that terminal PULL is INSIDE an explicit
transaction, where the specification puts the bookmark on the COMMIT SUCCESS and not on
the stream's terminal SUCCESS, so the field is now absent there; the same seed
previously read `[bookmark db has_more]`, carrying the stale token. A serial sweep of seeds 1 to 100 was clean on all 100.

### The protocol version matrix: 4.4, 5.0 and 5.x side by side (rmp #2486)

**Every DST connection before this task negotiated 5.6.** `WireClient.Handshake` offers
5.6 with a minor range down to 5.0 in slot 0 and 4.4 in slot 1, and the server picks the
highest version it supports inside any offered range, so 5.6 is what every arm of every
scenario got. rmp #2481 added `HandshakeOffering` to reach a specific version and used it
only to reach 5.0, only to check that a credential-bearing HELLO is accepted there. Bolt
4.4 had never been negotiated by anything in `internal/sim`, and no two versions had ever
been compared against each other.

**Two whole axes of the server were undriven, and they are different axes.** The entity
and temporal encodings branch on the MAJOR version: a Node is three fields at Bolt 4 and
four at Bolt 5 (`bolt/server/entity_struct.go:96-98`), a Relationship five and eight
(`:112-118`), an UnboundRelationship three and four (`:130-132`), and a Path inherits all
of them by recursion (`:144-152`); a zoned DateTime switches both its struct tag and the
MEANING of its seconds field — `0x49`/`0x69` carrying a true UTC epoch second at major
≥ 5, `0x46`/`0x66` carrying the wall clock expressed as if UTC at 4.4
(`dateTimeToPackstream`, `bolt/server/session.go:2222-2243`). Authentication branches on
the MINOR version at a different place: `authDeferredToLogon` compares against
`proto.Version{5, 1}` (`bolt/server/state.go:294-305`), so ≥ 5.1 sends a credential-less
HELLO and authenticates on a separate LOGON while ≤ 5.0 carries the credentials on HELLO
itself. **The task text calls the second axis "4.4 (no LOGON)"; reading the code says
more than that** — 5.0 is on the same side of the auth split as 4.4 and the other side of
the encoding split, which is exactly why it is called out separately as never entered,
and it is what makes the matrix a CROSSED design rather than a list. 4.4 against 5.0
moves the encoding axis with auth held fixed; 5.0 against 5.1 moves the auth axis with
the encoding held fixed, so either difference is attributable to ONE axis, which a
4.4-versus-5.6 comparison alone could never be.
`TestBoltVersionMatrix_TableIsCrossed` pins that the 5.0 row exists, because dropping it
would silently collapse the design while every clause still passed.

**The load-bearing shape is that semantics are INVARIANT while encodings DIFFER, and both
halves are asserted**, because either alone is satisfiable by a broken server. A run that
only required the decoded values to agree across versions would pass against a server
that ignored the negotiated version entirely and emitted Bolt 5 structures to a 4.4
client — the values would agree perfectly and a real 4.4 driver would fail to hydrate
them, since its hydrator asserts the field count. So
`encoding-differs-across-majors` is written deliberately as the guard on the other
clauses: the SAME query's record captured at 4.4 and at 5.6 must not produce the same
struct census or the same byte length. Measured at the catalogue seed, `[N/3 R/5 P/3 N/3
N/3 r/3]` against `[N/4 R/8 P/3 N/4 N/4 r/4]`, and 144 bytes against 168 — the census is
stable run to run, the two lengths are not (see the instrument trap below), but they
always differ, because the Bolt 5 layout adds seven decimal element_id strings to this
record and cannot encode to the same size. **A controlled
revert confirmed it end to end**: making `boltVersionExpectedWidths` version-blind turned
the live 4.4 arm red on `encoding-struct-layout`, and declaring 5.0 deferred-auth turned
the live 5.0 arm red on five auth clauses at once.

**The oracle is an independent PackStream reader, not the codec it adjudicates.**
`decodeBoltWire` is a minimal reader written in `internal/sim/bolt_version_matrix.go`
from the marker table — derived first from real hex captures of this server and then
confirmed against the published constants at `bolt/packstream/encoder.go:24-59` — and it
is what produces each record's struct census from the raw chunked bytes.
`TestDecodeBoltWire_ReadsHandBuiltBytes` pins it against hand-written byte strings whose
content is known independently of any encoder, and `TestDecodeBoltWire_RejectsMalformed`
pins its refusals, because a reader that tolerated a truncation could report a short
census as a correct one. `encoding-walker-agrees-with-codec` then runs the module's own
decoder over the identical bytes and requires the two censuses to match, so a bug in the
independent reader surfaces as a disagreement rather than as a confident wrong verdict.
The value-level oracles are computed by the harness: the entity ids and property maps are
what the scenario itself created, each element_id must be `strconv.FormatInt` of the id
the same structure reports, and every temporal field is computed with Go's `time` package
from the literal the query carries.

**The no-LOGON contract is measured against a CREDENTIALED server**, never
`NoAuthHandler`: against a handler that admits everyone, "the credentials were accepted"
is true at every version and proves nothing. With `BasicAuthHandler` the same bytes
produce opposite outcomes on the two sides of 5.1, which is what makes the contract
falsifiable. A WRONG password on HELLO draws `Neo.ClientError.Security.Unauthorized` and
the connection is torn down at 4.4 and 5.0, and draws SUCCESS with the connection intact
at 5.1 and 5.6. A credential-less HELLO is refused at 4.4 and 5.0 and succeeds at 5.1 and
5.6. A RUN sent straight after a successful HELLO is SERVED at 4.4 and 5.0 and refused at
5.1 and 5.6 by the state gate, which names the state it refused from. And a RESET on that
pre-LOGON session returns it to NEGOTIATION rather than to READY — the deliberate
pre-authentication RESET gate of task #1345 (`bolt/server/state.go:124-133` and
`Session.handleReset`'s `!s.authenticated` branch, `bolt/server/session.go:1038-1041`) —
so the following RUN is refused naming NEGOTIATION, while the same RESET on an inline-auth
session leaves it usable. Every refusal clause pins the whole `in state X` phrase rather
than the bare state name, following rmp #2484.

**Negotiation is adjudicated by a literal expectation table over raw 20-byte preambles**,
written directly on `SimConn` rather than through `WireClient`, because
`HandshakeOfferingSlots` collapses a rejection into an error and telling a rejection apart
from a transport failure by matching that error's text would be a fragile oracle. Fifteen
cases: the four exact versions; four range offers, including one whose top is ABOVE
everything the server supports and which still resolves, to 5.6; the legacy version
offered FIRST and losing anyway, which shows the choice is driven by the server's
preference and not by slot order; a supported version with an unsupported decoy alongside
it; and four ways to have nothing in common, all refused. Because every expectation
follows from `proto.SupportedVersions`, `negotiate-supported-list` is a TRIPWIRE that
compares that list against a literal copy, so adding 5.7 upstream is a loud failure at the
one clause whose job is to notice rather than silent staleness across the other fourteen.

**The offer SPELLING is seed-chosen and its invariance is the claim.** Each arm negotiates
its target twice — once canonically (exact version, slot 0, no range) and once with a
seed-drawn slot, minor range and optional unsupported decoy — and the seeded spelling is
the one the working connection uses, so every observation the arm makes was produced over
it. This needed one new primitive, `WireClient.HandshakeOfferingSlots`, because nothing in
the harness could send a range offer other than the one `Handshake` hard-codes or place an
offer in a chosen slot. `Handshake` and `HandshakeOffering` were both refactored onto it,
and `TestWireClientHandshake_PreambleBytesAreUnchanged` pins the exact 20 bytes each still
writes, read off a bare `SimListener` with no server behind it — comparing negotiated
versions could not have caught the change, because a range offer and an exact offer of the
same top version negotiate the same result.

**One trap was found in this scenario's own instrument rather than in the server.** An
early draft rendered node ids and the record's byte length, on the stated belief that they
were assigned in fixture-creation order and therefore a function of the seed. The
determinism test refuted it: two runs of the same seed produced node ids 38/215 and then
227/48, and records of 138 and then 140 bytes, because the id derives from a node key
minted from a process-global counter and the byte length follows it through the decimal
element_id strings. Both are now rendered POSITIONALLY — `n0`, `n1`, `e0` in
first-encounter order, with each element_id shown as the token of the id whose decimal it
is — which keeps every structural fact (which entity appears where, which element_id
belongs to which id) while dropping the process-dependent value. The CHECKERS still read
the raw values, and are entitled to: every clause over them is a derived relation, never a
literal. This is the same class of defect as the rmp #2485 report field that rendered `+1`
at one worker and `+5` at six.

The remaining families are the version-invariant half. The parameter round trip drives
seven kinds (null, boolean, integer, float, string, a mixed list, a map) and compares the
decoded value AND its concrete Go type across versions, following rmp #2484 — the dynamic
type IS the wire encoding, so an Integer re-encoded as the identically-rendered Float must
fail. The zone-less temporals (`date` `0x44`, `localtime` `0x74`, `time` `0x54`,
`localdatetime` `0x64`, `duration` `0x45`) are required to be BYTE-identical at every
version, which is the control proving the version knob is narrow rather than global. Both
zoned datetimes are asked at a NON-ZERO offset (`+02:00`, and Europe/Athens on 2 January,
also `+02:00`) because at a zone offset of zero the legacy and UTC conventions encode the
identical seconds field and the clause degenerates to a tag-only check — Europe/Lisbon in
January is exactly that trap and was the first zone tried;
`TestBoltVersionMatrix_TemporalReferenceOffsetIsNonZero` pins it. A bad-actor battery
(garbage opcode, COMMIT with no transaction, PULL with no RUN) must draw the identical
typed refusal at every version, pinned to the literal code and message so that two
versions agreeing on a wrong answer does not pass. And each arm commits a marker node in an
explicit transaction: the census must advance by exactly one per arm, and every marker must
be present both live and in a graph reopened through real WAL recovery after a crash, so
the protocol version a write arrived over provably does not reach the durable state.

The non-vacuity gate is a separate function answering a different question — was the run in
a position to notice — and its shortfalls fail the scenario just as a contract violation
does. It censuses which versions were actually negotiated (the trap being an arm that
silently landed on 5.6 while believing it negotiated 4.4), requires the crossed design to
have been constructed, requires the same entity tag to have been SEEN at two different
arities and both zoned-datetime conventions to have been seen, requires the negotiation
table to have produced at least one refusal and one range resolution, requires the seeded
spelling to have differed from the canonical one for at least one arm, and reports a
missing zone database as a shortfall rather than letting the named-zone clause pass
unexercised. 38 named contract clauses and 10 `nv-` ones; 53 falsifiability subtests each
perturb one field and assert the clause that must catch it.

### Aggregate inbound-decode backpressure, and two nesting caps that are not one (rmp #2487)

The harness had exactly one arm anywhere near inbound-memory abuse: the `BoltAbuser`'s
oversized frame, which drives the PER-MESSAGE framing cap on ONE connection. Three bounds
that matter more were driven by nothing. The **engine-wide inbound-decode pool**
(`packstream.InboundBudget`) is created ONCE PER SERVER — `bolt/server/serve.go:654`,
`NewInboundBudget(resolveMaxInboundDecodeBytes(opts.MaxInboundDecodeBytes))` — and that
single pointer is what makes it the cross-connection vector, because the per-message cap
times the connection limit is unbounded and pre-authentication-reachable, which is the
CWE-770 the pool exists to close. The **wire nesting cap** (`packstream maxValueDepth =
128`, `value.go:21`) is a hard security boundary rather than a convenience: without it a
crafted message can request millions of stack frames and kill the process, and it is
reachable during the FIRST HELLO decode. And a third that this task found by measurement
rather than by reading: the engine's **own parameter nesting cap** (`cypher
maxParamBindDepth = 32`, in `cypher/api.go`), a second, lower, independent cap on the
same axis, which nothing had documented as Bolt-visible.

**The load-bearing oracle is a closed-form model of the pool, not the server's word.** The
harness re-derives what a RUN's decode holds from the shared pool out of packstream's
published per-slot costs:

```
held(query, key, n) = 32 + 3*48    the RUN struct: container + 3 fields
                    + 512 + 112    the one-entry parameters map
                    + 32           the parameter list's container
                    + 512          the empty extra map
                    + len(query)   String payloads are charged 1:1 (decoder.go:712)
                    + len(key)     and so is a map key
                    + 48*n         one 48-byte slot per list element
                    = 1344 + len(query) + len(key) + 48*n
```

The two string terms are not obvious and were found by measurement: `ReadString` charges
its raw payload against the SAME shared pool that `chargeDecoded` draws on, so a longer
query text moves the admission boundary. The model was calibrated against the real decoder
by binary search on the smallest budget that admits a payload — `48n + 1353` for every `n`
from 0 to 174,734 with an 8-byte query and a one-byte key, exactly what the closed form
says — and it then named the last admitted element count EXACTLY at nine ceilings from
2 MiB to 32 MiB (the soak sweep). The scenario does not trust that: it SCANS a window
around the prediction and requires the measured boundary to be one element wide, monotone,
and equal to the model's. That makes `pool-boundary-matches-model` a tripwire on five
packstream constants; if one changes, this scenario names the divergence instead of
drifting.

**A one-element-wide boundary is the strongest available refutation of "the per-message cap
did it."** At the 4 MiB ceiling, `n=87353` (modelled hold 4,194,304 B, slack +0 B) is
SERVED and `n=87354` (slack -48 B) is REFUSED: the two differ by 48 charged bytes out of
~4 MiB, both are ~200x under the 16 MiB framing cap and 32x under the 128 MiB per-message
decoded-collection cap. The **control** closes it: a second server differing in the
ceiling (64 MiB) and in NOTHING else serves the identical 165,796-element bytes the
pressured one refused. The two write arms carry the same query shape and differ only in
their parameter's element count, so the census is attributable: the accepted write's node
is present live AND after real WAL recovery, the refused write's node is present nowhere,
and the same bytes wrote a node into the control server's own engine.

**Three abuse vectors, three DIFFERENT answers — measured, not inferred.** A server that
collapsed any two would be indistinguishable, from a client's side, from one with no
aggregate pool and no depth cap at all, and every other clause here could still pass
against it:

| vector | code | session afterwards |
|---|---|---|
| aggregate pool breach | `Neo.TransientError.General.OutOfMemoryError` | READY (usable with NO RESET) |
| wire nesting cap (>= 128) | `Neo.ClientError.Request.Invalid` / `malformed Bolt message` | READY (usable with NO RESET) |
| engine parameter cap (> 32) | `Neo.ClientError.Statement.ArgumentError` / `cypher: ArgumentError.ParameterNestedTooDeep: parameter "d" is nested deeper than the supported limit of 32 levels` | FAILED (next message IGNORED) |

The third row read `Neo.DatabaseError.General.UnknownError`, sanitised to "An internal
error occurred", when this scenario first measured it — and **reporting that is what got it
fixed**: rmp #2570 reclassified it as the client fault it always was, since the payload is
entirely client-supplied. It is the **`Statement`** family rather than the `Request` family
because the message decoded correctly and the statement was dispatched: what is invalid is
the statement's ARGUMENT, not the form of the request, and `Request.Invalid` is what the
row above answers for a frame that will not decode at all. The classification is reached
through the module's own TCK-pinned convention — an engine error whose message carries
`cypher: ArgumentError.` — so no `bolt/server` change was needed. The arms of this family
were what pinned the old answer, so the correction failed them on purpose and they were
updated with the ticket rather than deleted.

The first two are answered ABOVE the session state machine — the serve loop rejects them
between the read and `sess.HandleMessage` — which is why the session survives them intact;
the third travels through it into `cypher.BindParams`, so it fails the session. That
state-after difference is a **third discriminator, independent of the codes**, and it is
asserted separately. The three answers do NOT disagree about the session, which is worth
saying because it reads like an inconsistency: staying READY is reserved for
back-pressure, where retrying the same request can succeed (`bolt/server/state.go`), and a
depth refusal is deterministic — retrying the identical RUN fails identically forever.

One consequence of the fix landed inside this scenario's own instruments. The parameter cap
was the ONLY arm here whose failure went through the sanitiser, so
`boltDecodeRedactSession` — which strips the per-session id that would otherwise make the
rendering vary run to run — became a no-op on every arm. The determinism test's clause
requiring that the redaction had really fired was therefore **unsatisfiable, and was
retired rather than left to fail**; the redaction itself is kept as insurance for a future
arm that does draw a sanitised message, and every message this family records is now a pure
function of the seed by construction. The classification segment is asserted on its
own too, read out of the OBSERVED code rather than compared to the literal this file
declares: neo4j-go-driver's `IsRetriableTransient` tests `classification ==
"TransientError"` (`bolt/server/errors.go:129-131`), so "typed RETRYABLE backpressure" is a
checked property of the code the server actually sent. Testing the classification only
after the whole code matched the literal would have made that guard unreachable, and an
earlier revision did exactly that.

**The harness reads that segment at the driver's arity, not a laxer one (rmp #2575).**
`boltDecodeClassification` accepts a code of EXACTLY four dot-separated segments and
returns `""` for anything else, mirroring
`github.com/neo4j/neo4j-go-driver/v5` v5.28.4, whose `(*Neo4jError).parse`
(`neo4j/db/errors.go:114-127`) abandons a code on `len(parts) != 4` and so leaves the
classification empty, making `IsRetriableTransient` (`neo4j/db/errors.go:156-159`) report
false. An earlier revision split on dots and took `parts[1]` from any code with two or more
segments, so a regression emitting the three-segment `Neo.TransientError.OutOfMemoryError`
would have SATISFIED `pool-refusal-retryable` while no real driver would retry it. The gap
was latent — every `Neo.*` code the server can emit is four-part, verified by sweeping the
tree's Go AST rather than by assuming it, and every dynamic `Code:` assignment on the Bolt
path resolves through `FailureCode`/`authErrorCode`/`evalErrorCode`, which return only
four-part literals — so closing it changed no verdict. The mirroring is deliberate and is a
third-party contract that can drift, so the arity must be re-derived from the pinned
dependency rather than remembered.

**The nesting family is bracketed at every boundary and is deliberately tiny on the wire.**
32 accepted / 33 refused, and 127 refused-by-the-engine / 128 refused-by-the-decoder,
identically for LIST chains and MAP chains — the bound is on composite depth, not on lists.
Every payload is far under the 64 KiB anti-confound ceiling that is asserted — 55 to
4046 wire bytes at the catalogue seed, and at most 6166 on any seed, since the
deliberately excessive arm's chain length is drawn from `[2048, 6144)` — because a
message refused for its SIZE proves nothing about a DEPTH cap. The chains are
hand-built from the marker table rather than through `packstream.Encoder`, and
not for authenticity: the encoder CANNOT express them, because `writeValue` carries the
same `maxValueDepth` bound as `readValue` (`value.go:68-69`). An abuse the module's own
encoder refuses to encode is exactly the abuse a hostile peer hand-rolls, and building it
here keeps the harness from validating the decoder with the encoder that shares its bound.
The **pre-authentication** arm is the one that isolates the wire cap cleanly, since no
parameter is bound and the engine's cap is not in the way: a 127-deep HELLO succeeds, a
128-deep one is refused, and the connection survives with the session still
UNauthenticated — a following plain HELLO succeeds, which it could only do from
NEGOTIATION.

**No-leak is proved through the wire, not by reading the pool.** `InboundBudget` exposes
`Enabled`, `TryReserve` and `Release` but no `Remaining`, and the Server's pool is
unexported. Rather than reach for an accessor, the run repeats the calibrated
boundary-sized message after every abuse arm: a message whose modelled hold is within one
element's charge of the whole ceiling can only be admitted by a pool restored to within
that many bytes of full. Measured slack at the 4 MiB ceiling is **+0 B**, so a leak of a
single byte is detectable, and the gate asserts that slack stays tight — a probe that had
gone slack would pass whether or not the pool came back short. The soak layer adds the
statement the short layer structurally cannot make: 4000 alternating served and refused
decodes against ONE long-lived server (2000 each, so both release paths run equally), after
which the boundary probe is still admitted.

**The concurrent sibling exists because the aggregate vector is unreachable without it.**
Every charge is released before its reply is written — the reassembly reader releases on
every return path from `ReadMessage` (`bolt/proto/chunking.go:160-165`), the decoder's hold
by the deferred `ReleaseInboundBudget` (`serve.go:1419-1423`) — so a single-threaded
lock-step script can never observe two charges outstanding at once, whatever it sends. The
`bolt-decode-swarm` scenario runs four abusers at 55% of an 8 MiB pool (one fits, two
cannot) against an honest client on its own connection.

**Its overlap oracle had to be CONSTRUCTED rather than raced for, and every attempt to
threshold it instead was measured and abandoned.** Started together with fixed counts, the
abusers finished in ~38 ms while the honest client was still pausing between exchanges:
exactly ONE of 24 honest exchanges straddled a refusal, a coverage clause one scheduling
decision from failing for no reason and one from PASSING while the run showed honest
traffic working before and after the pressure rather than during it. Pinning the window's
ENDS — the honest client waits for the first refusal, the abusers push until it finishes —
took it to 9 of 24, and those two pins remain the arm's construction. Controlling the
WIDTH came next: every sixth honest exchange holds its open stream for 50 ms between RUN
and PULL, genuinely in flight with the server holding a cursor for it, which is ~13x the
measured inter-refusal interval and ~79x the median narrow exchange (633 us). That hold is
deliberately NOT a wait for a refusal to be counted, which would make the clause true by
construction of the HARNESS instead of by behaviour of the SERVER.

**What the width did not buy was a threshold, and rmp #2596 measured the cost of pretending
otherwise.** Requiring half of the four wide exchanges to have straddled a refusal compares
a FIXED 50 ms window against a refusal cadence the machine controls. Under 32 concurrent
coverage-instrumented test binaries, 9 to 13 of every 96 swarm runs put only 1 of the 4 over
a refusal and failed that clause on a clean engine, with nothing else in the gate firing; the
same shape had already reddened `make ci` from the coverage run while passing in the same
invocation's race run. Widening it to "any refusal observed inside any exchange" was measured
too and rejected: in the whole-module coverage regime the gate actually runs in, that
quantity came back as 1 on 2 of 4 runs, which is no margin at all.

**The clause is now an interval intersection measured by the fleet itself.** Each abuser
samples the honest client's in-flight state immediately before it writes and again at the
moment it counts a refusal, and the refusal is attributed to honest work only when those
samples bracket a honest exchange's own flight; nv-swarm-overlap requires that count to be
nonzero. That replaces asking WHEN a refusal was counted, which is a question the harness
cannot answer: the counter is incremented by an abuser goroutine after its reply has been
decoded, so a refusal is recorded a load-dependent time after the server issued it. Driving
the harness so that no refusal could possibly be issued during a honest exchange left the
old event-in-window instrument reporting 16 to 25 in-flight refusals per run and the whole
gate green, while the present one goes to zero and fires on all three catalogue seeds. It
also has the margin the old one lacked: fewest overlapping refusals in a run was 4 under 32
concurrent coverage-instrumented binaries, 386 across a 100-seed soak sweep, and the
overlapping share of a run's window refusals is around 97%.

**Nothing in the arm thresholds a rate any more.** The wide and narrow straddle counts, the
narrowest wide window, the in-flight and gap splits and the per-segment distribution are all
reported on every failing run and adjudicated nowhere. The density clause alongside it went
the same way in rmp #2587: it once required 8 refusals across the honest run, then one per
segment, and now requires only that the window's total is nonzero. Overlap counts a subset
of the same events and is gated on that total being nonzero, so the two can never report one
cause as two findings, and a fleet that took turns with the honest client — pressure present
throughout, never once concurrent — fails overlap while satisfying density.

**The liveness bound is a wall clock, and this is the case rmp #2567 left standing.** That
task removed deadlines used as oracles over BOUNDED payloads, where a deadline can only
misattribute a slow machine. This wait is bounded by nothing: a server that starved honest
traffic under aggregate pressure would never serve it, so the honest client would wait for
ever and only a clock can tell. The bound is set against a MEASUREMENT rather than a
guess — 30 s against a measured 633 us median and 902 us worst honest exchange with the
fleet pushing under `-race`, about 33,000x the worst observed service time — the
message that fires says STARVED rather than "slow", and it is paired with the claim that
matters and involves no clock at all — every honest exchange must return the value the
harness chose for it, so a reply belonging to a different exchange is a failure and not a
pass.

The swarm's sizing also has to keep the honest client clear of the REASSEMBLY reader, whose
budget breach is **not** the connection-preserving refusal the decode layer's is: it
returns `packstream.ErrInboundBudgetExceeded` as a READ error (`chunking.go:223-227`) and
the serve loop tears the connection down on every read error (`serve.go:1237-1247`). The
pool floor is therefore computed rather than hoped for — one abuser's charge is 4.4 MiB of
an 8 MiB pool, leaving 3.6 MiB, and three refused abusers transiently holding one 1 MiB
reservation chunk each leaves a worst floor of ~0.6 MiB against ~100 bytes of honest
reassembly — and
`swarm-no-transport-loss` reports it by name if the sizing ever stops holding.

The coverage gate is a separate family, as elsewhere. It requires the boundary window to
have BRACKETED the transition (window counts and run-wide counts are kept separate, after
an earlier revision summed them and a window that had gone entirely green still satisfied
the bracketing clause because the breach arm's single refusal was being counted as if the
scan had produced it), requires both a refusal and an ACCEPT (a pool stuck at zero refuses
everything and satisfies "every refusal is typed" perfectly), requires the leak probe's
slack to stay tight, requires the nesting family to be complete and to have produced all
THREE outcomes (with fewer, the distinctness clause returns without adjudicating anything),
requires an over-nested HELLO to have been refused so the pre-authentication path was
actually visited, requires the control to be a genuinely different configuration that
genuinely disagreed, and requires the live census to be non-empty so "the refused write
left nothing behind" is not trivially true of an empty graph. 29 named contract clauses and
17 `nv-` ones; 52 falsifiability subtests each perturb one field and assert the clause that
must catch it, and a further 6 subtests drive every pairwise collapse direction (see below).

**Four of those checks are guards on the harness, and say so (rmp #2576, #2579).**
`nesting-not-by-size`, `pool-control-identical-payload`, the `ControlBudget <= Budget`
branch of `nv-control-differs` and `nv-nesting-family-complete` read quantities that no
server behaviour can move: the nesting payloads are built by the harness and top out at
6166 wire bytes against a 64 KiB ceiling, the control's element count is copied from the
breach arm inside the same run, both ceilings are compile-time constants, and a missing
nesting arm aborts the run before adjudication. Each is kept — a harness that has been
re-wired is worth catching — and each carries the label "A guard on the HARNESS, not on the
server", so a reader does not count it as evidence about the subject. The deterministic
`nv-leak-probe-tight` is deliberately NOT in that list: it probes at the MEASURED boundary,
so a server that admitted fewer elements widens its slack and fires it.

**`nv-swarm-leak-probe-tight` was the fifth, and is now a real clause about the server
(rmp #2579).** It shares its checker with the deterministic `nv-leak-probe-tight` and
differed from it only in which probe it was handed: the swarm's was sent at the MODELLED
boundary, so its slack was the constant 16 B and neither a server nor anything else could
move it. `RunBoltDecodeSwarm` now MEASURES the boundary first, with a seven-probe scan on
one connection against a pristine pool, and sizes the post-swarm leak probe to the largest
element count the server actually admitted. Measuring it DURING the swarm would have been
unsound — under aggregate pressure the boundary is whatever the other connections happen to
be holding at that instant — so the scan runs before `boltDecodeSwarm.run` starts a single
goroutine and cannot race the abusers. That the clause is now server-falsifiable was
demonstrated rather than asserted: shrinking the real server's pool by one element's charge
(48 B) while leaving the harness's declared ceiling alone moved the measured boundary from
174734 to 174733, widened the slack from 16 B to 64 B, and fired the clause; 96 B moved it
two elements and gave 112 B. Measured across 13 seeds including the catalogue default, the
scan brackets the transition every time (four probes accepted, three refused), the measured
boundary equals the model's, and the slack stays 16 B, so no verdict changed. An A/B with
the scan disabled showed it does not disturb the fleet's dynamics either — refusals across
the honest run were 59-65 without it and 59-64 with it, and every wide exchange still
straddled. It costs about 0.35 s per swarm run. Because the probe now depends on a
measurement, the new `nv-swarm-window-spans-boundary` reports a scan that failed to bracket
the transition, so the "no calibrated size" fallback to the model's boundary can never be
taken silently — which is the only way this clause could quietly become a harness guard
again.

**The collapse-detection claim is pinned by a test rather than by a deleted probe
(rmp #2579).** `checkBoltDecodeCapsAnswerDifferently`'s godoc says the clause is not the
only thing standing between the run and a server that answers every abuse vector with one
code, because the literal-pinning clauses catch such a collapse first. rmp #2576 justified
that wording with a throwaway probe and then deleted it, leaving the claim resting on a
measurement no longer in the tree: narrowing `nesting-answer` later would silently make the
distinctness clause the sole detector and the godoc quietly false.
`TestBoltDecodePressure_DistinctnessIsNeverTheSoleCollapseDetector` is that probe, kept. It
enumerates the ordered pairs of the three vectors rather than listing them — six directions
over three vectors, so a fourth vector cannot be added without the table growing — and for
each one requires both that `caps-answer-differently` fires and that a literal-pinning
clause fires with it. Re-derived independently, a literal-pinning clause co-fires in 6 of 6:
`pool-refusal-typed` with `pool-refusal-retryable` on the two directions that move the pool
arm, `nesting-answer` on all four that move a nesting arm, joined by
`nesting-is-not-backpressure` on the two that collapse onto the budget code. The two
directions between the wire cap and the parameter cap rest on `nesting-answer` ALONE, which
is what gives the test its teeth: narrowing that clause to stop pinning the code turns
exactly those two red.

**One collision was found in the harness itself.** Generalising rmp #2485's single-scenario
seed-mix guard into a table over every Bolt scenario immediately went red:
`txQuotaSeedMix` and `boltTxQuotaDefaultSeed` were both `0x2482_9074`, so `bolt-tx-quota`
built its `SimDisk` from `NewSeed(0)` on the one run every report starts from — precisely
the defect the original guard was written to prevent, unnoticed because the guard had been
copied per surface instead of iterating. Fixed to `0x2482_5EED`, and the table now fails if
a Bolt scenario is added to the catalogue without an entry.

## Defects surfaced by this coverage work

The coverage work exercised the engine against these scenarios and found:

1. **`MERGE … ON CREATE/ON MATCH SET` dropped an expression right-hand side**
   (fail-silent): `ON MATCH SET n.n = n.n + 1` committed but never applied.
   **Fixed** — the merge operators now evaluate a non-literal RHS per-row
   (openCypher TCK unchanged at 3897/3897). The `merge-rel` scenario is the
   regression guard.
2. **`CALL … YIELD … WHERE <pred>`** silently ignored the `WHERE` filter
   (read-only). **Fixed** (#1966) — the visitor now captures `Call.Where` and
   the translator lifts it as a `Selection` over the `ProcedureCall`. The
   engine-level guard is `cypher/call_yield_where_test.go`; since rmp #2462 the
   DST also holds the procedure form to the harness's DDL model on every
   schema-introspection check (`checkCallYieldWhere`, see
   [DST language-surface gaps](dst.md#language-surface-gaps-rmp-2462)).
3. **k-shortest multigraph semantics** diverge between `YenKShortest` (dedups by
   node sequence, cheapest parallel edge) and `Loopless`/`Eppstein` (parallel
   edges as distinct paths). Recorded for adjudication (#1967).
4. **A list-valued column was stringified on the Bolt wire.**
   `bolt/server/session.go`'s `exprValueToPackstream` documented an
   `expr.ListValue` case but its switch had none, so a list column fell through
   to the `default` arm and was emitted as a PackStream **String**
   (`"[1, 2, 3]"`) instead of a PackStream **List**. A literal list
   (`RETURN [1,2,3]`) was stringified identically, so this was the RECORD encoder
   rather than parameter binding — a bound list evaluated correctly (indexing,
   `size`, and equality against a literal list all agreed). It affected every
   list-producing construct: `collect()`, `labels()`, `keys()`, `nodes(p)`,
   `relationships(p)`, list literals, and list-valued properties; `nodes(p)`
   additionally lost all node structure. Found by the wire parameter matrix
   (rmp #2462), which PINNED the rendering so a fix would flip the probe
   deliberately. **Fixed** (#2513) — the arm encodes element-wise with the
   negotiated Bolt major threaded through, so nested containers and entity or
   temporal elements encode structurally on both 4.4 and 5.x. The pin is now the
   live assertion, and `internal/sim/wire_list_encoding_test.go` carries the
   end-to-end matrix.

5. **`manifest.json` was not checksummed.** It carried a CRC32C for every other
   snapshot component and none of its own, so a single byte flipped inside a
   JSON **key name** decoded as an unknown field, was dropped by
   `encoding/json`, and zeroed the corresponding `Manifest` field with no error
   anywhere — 360 bytes, 25.7% of a published 1399-byte manifest, were
   silently accepted. Measured consequence on the worst field: flipping one character of
   the `"commit_ts"` key dropped the recovered MVCC clock floor from 20 to 0, so
   `RestoreMVCCClock` was skipped and the reopened graph re-minted instants the
   image already contained (the loss #2309 prevents). No committed node was lost,
   which is why a green suite could not see it. Found by
   `snapshot-corruption-failstop` (rmp #2467), which changed nothing outside
   `internal/sim` and `docs/`. **Fixed** (#2520): `WriteManifest` appends a
   CRC32C trailer over the whole document and `LoadManifest` verifies it before
   reading any field. Re-measured census: **0 of 1450 bytes**. `ManifestVersion`
   is unchanged, so old snapshots open and new ones stay readable by older
   builds; see the section above for why the framing and schema layers are kept
   apart.
6. **A struct edge weight is silently dropped by the snapshot CSR writer**
   (fail-silent, permanent data loss). `store/snapshot` never consulted
   `txn.WeightCodec`: `csrWeightSize` (`store/snapshot/writer.go`) returns 0 for
   every weight type outside the Go primitives — every struct, and every NAMED
   integer type such as `time.Duration` — so `WriteCSR` emitted `hasWeights=0`,
   the same encoding a deliberately weightless graph produces, and the
   checkpoint then truncated the WAL prefix holding the true values. Measured by
   `codec-matrix` (rmp #2473): the same image returned **95 of 95** weights at
   the WAL-only boundary and **0 of 191** after one folding checkpoint.
   **FIXED (rmp #2526).** The snapshot persists weights of any type through the
   store's own codec (`checkpoint.WithWeightCodec` /
   `recovery.Options.WeightCodec`), and a weight that still cannot be encoded
   fails the snapshot write with `snapshot.ErrWeightNotPersistable` instead of
   publishing a weightless image, so the WAL prefix holding the surviving copy
   is never truncated. The matrix assertion is inverted: every arm must now
   confirm its weights after a SNAPSHOT-ONLY recovery, and a non-vacuity gate
   refuses a run in which any arm never crossed a checkpoint.
7. **`jsonl.WriteWithProps` is not byte-reproducible** (fail-silent; no data
   loss). It emits one `"property"` record per entry of the node's property
   MAP, iterating it directly (`graph/io/jsonl/writer.go`), so two exports of
   the same graph carry the same records in a different order. Measured by the
   graph/io surface arm (rmp #2480) over a four-node, seven-kind fixture:
   **7 of 7** repeat exports differed byte for byte from the first, while
   `graphml.WriteWithProps` — which emits in a fixed key order — differed in
   **0 of 7**, as did `dot.Write`, `csv.Write`, `jsonl.Write` and
   `graphml.Write`. Nothing is lost (the reader is order-insensitive within a
   node, and the round-trip is exact), but the artefact cannot be compared by
   digest, diffs spuriously, and defeats content-addressing — and it made a
   seed-derived byte offset into the export non-reproducible, which is how the
   simulator found it. The mutation sweep canonicalises the JSONL property
   suffix for its own use and the verdict asserts byte-reproducibility for every
   OTHER encoder, so this one is recorded rather than papered over. Not fixed:
   the fix is in `graph/io`, outside that task's scope.
8. **`Options.Logger` never reached the Bolt session** (fail-silent; no data
   loss). `Options.Logger` documents itself as "the structured logger for server
   events", and `NewServer` threads it into the `Server`. `newSession`
   nevertheless hard-coded `slog.Default()` and the bootstrap never overrode it,
   so all eleven session-level log sites — every refused credential, every failed
   query, every failed `BEGIN` and `COMMIT`, the transaction-quota refusal, and
   the received-bookmark debug records — bypassed the configured logger. That is
   the majority of what a Bolt server logs: an embedder who routed the server's
   output to a file, a collector, or `io.Discard` still had its
   security-relevant events written to the process default. Found while building
   the auth scenario (rmp #2481), which discards its server's log precisely
   because it provokes dozens of refused credentials on purpose — and saw them on
   stderr anyway. Measured on the unfixed build: a capturing handler received 4
   records and NEITHER the authentication failure nor the query failure was among
   them. **Fixed** — the bootstrap calls `sess.setLogger(s.log)`;
   `bolt/server/session_logger_test.go` is the regression guard and was verified
   to fail without the fix.
9. **An OPERATOR termination tells the client its transaction timed out and that
   a writer lock was released** (fail-misleading; no data loss).
   `Server.TerminateTransaction` routed through `Session.reapTimedOutTx`, which
   was shared with the idle/total reaper, so the client was answered
   `Neo.ClientError.Transaction.TransactionTimedOut` with "the transaction has
   been terminated because it exceeded its timeout; the writer lock was
   released". BOTH halves are false for a termination on demand: it exceeded no
   timeout, and no writer lock has been held for a transaction's lifetime since
   rmp #2305/#2306 retired it (`Engine.beginTxSession` acquires no writer
   serialisation, `cypher/exectx.go`). A driver cannot distinguish an operator
   ending a transaction from a transaction that ran too long, which is exactly
   the distinction an operator needs the client's logs to preserve. Both strings
   were PINNED against named constants so the eventual correction would fail those
   arms deliberately instead of slipping through.
   **Fixed (rmp #2560), and the pin did its job — it fired, and was updated rather
   than deleted.** The teardown now takes the reason from its caller: the operator
   path arms `Neo.ClientError.Transaction.Terminated` / "the transaction has been
   terminated by an operator request", and the two deadline bounds keep the timeout
   code with the stale writer-lock clause dropped. The DST constants split into a
   deadline pair and a terminate pair, and the operator-terminate arm adjudicates
   against the latter with a named case for the specific regression of borrowing the
   deadline reason back.
   The code is `ClientError`, not `TransientError`, on primary evidence:
   `neo4j-go-driver` v5.28.4's `reclassify()` (`neo4j/db/errors.go:132-139`) rewrites
   `Neo.TransientError.Transaction.Terminated` to exactly this code, and its stated
   job is mapping "errors coming from pre-5.x servers into their 5.x
   classifications" — so this IS the modern classification and the Transient
   spelling is the legacy one. Because `reclassify()` runs BEFORE the classification
   is parsed, emitting the Transient form would not make a driver retry either; it
   would only claim a retriability the driver does not honour.
   **The same teardown also carried a metric defect the filed report called
   correct.** `incCounter(metricTxTimedOut)` was unconditional, so an idle reap and
   an operator termination were each counted as a total-lifetime timeout too, making
   that counter a superset of all three events. `metricTxIdleReaped`'s own godoc
   promises the opposite — that an abandoned `BEGIN` "shows up here, whereas a
   legitimately long transaction shows up there (rmp #2175)" — so the separation the
   idle counter exists to draw was nominal. Measured on the unfixed build:
   `tx.terminated=1` **and** `tx.timedout=1` for one operator termination;
   `tx.idlereaped=1` **and** `tx.timedout=1` for one idle reap. The counter now
   belongs to the total-bound branch of the serve loop alone, beside its log line.
   `bolt/server/terminate_reason_test.go` is the regression guard and was verified to
   fail on the unfixed build in all four shapes.
10. **A quota-refused `BEGIN` leaves the session READY** (behavioural
    inconsistency; no data loss). `handleBegin`'s per-principal-cap branch
    returns before `Transition` and never calls `enterFailed`
    (`bolt/server/session.go:1597-1606`), unlike the `newTx` failure path
    directly above it, which does (`:1583`). The two failures are a step apart in
    one handler and leave the session in different states, so a client whose
    `BEGIN` was refused by the cap is served normally on its next message while
    one refused by `newTx` must `RESET` first. Which behaviour is correct is a
    contract question, not an obvious bug — the Bolt state machine arguably
    should not fail a session for a resource refusal — so it was filed as **rmp
    #2561** and the OBSERVED behaviour was pinned rather than judged:
    `internal/sim/bolt_tx_quota.go` drives a statement down the refused connection
    and requires it to be served.
    **FIXED (rmp #2561, `f77df07e`) — and READY is now the CHOSEN answer, not the
    accidental one.** A cap is back-pressure, not a protocol error: the slot frees
    when another of the principal's transactions closes, so retrying the same
    `BEGIN` is the right response and a `RESET` round trip would charge the client
    for the server being busy. The reason is now written in `handleBegin`, in
    `Transition`'s godoc as its one documented exception, and in `docs/bolt.md`.
    The `newTx` neighbour keeps `FAILED` on purpose, and a second test pins it there
    so the difference cannot become a side effect of whichever branch is edited
    next. **The ticket's second half turned out to be wrong twice over:**
    `Neo.ClientError.General.LimitExceeded` does not appear in Neo4j's status codes
    at all — GoGraph invented it — and its `ClientError` class instructs a driver
    NOT to retry, the opposite of what a self-freeing cap wants. The code is now
    `Neo.TransientError.Transaction.MaximumTransactionLimitReached`, which is real
    and means exactly this. The in-flight CURSOR cap keeps `LimitExceeded`: it is a
    different limit, reached inside one transaction. The pin now asserts that
    staying `READY` is worth having — it frees the slot and re-issues the `BEGIN`
    with no `RESET` between.
11. ~~**`graph/query`'s index seek and its scan fallback disagree for mixed-kind
    range bounds**~~ **Closed by rmp #2600.** (Silent wrong answer on one of the
    two paths; read-only, no data loss.) With `Float64Value` bounds over an
    **int64-valued** property, the seek arm was served by the internal numeric
    companion btree — which keys `PropInt64` and `PropFloat64` under one float64
    order (`cypher/index_binding.go:projectNumericPropValue`) — and returned the
    numeric matches, agreeing with the Cypher engine and with the model. The scan
    arm returned **nothing**, because `query.valueInRange` required `v`, `lo` and
    `hi` to be the same `PropertyValue` kind. MEASURED and reproducible before the
    fix: seed `0x24920003` (short layer) logged seek=1 scan=0 cypher=1 oracle=1,
    and `TestFluentQuery_SoakSeedSweep` (`-tags=soak`, 24 derived seeds, 500 ticks
    each) recorded 14–21 mixed-kind probes per seed with **every one** of them
    diverging on **every** seed.

    The semantics were then settled from the primary sources rather than from
    either implementation: openCypher orders INTEGER and FLOAT in a **single**
    numeric order — the sole off-diagonal entry of the comparability matrix in the
    normative CIP "Comparability and Orderability", pinned by the TCK in
    `expressions/comparison/Comparison2.feature` ("Comparing across types yields
    null, except numbers", whose 90-pair cross-type sweep keeps exactly the four
    INTEGER/FLOAT rows) and `Comparison1.feature` (`1 = 1.0` is true). The **scan**
    was therefore the defective side.

    #2600 unified the comparison in `query.valueInRange`, comparing INTEGER
    against FLOAT **exactly** rather than by widening the integer (the TCK pins
    `4611686018427387905 != 4611686018427387900` although both round to 2^62);
    made every numeric bound pair — including a mixed INTEGER/FLOAT pair, whose
    two bound tests the CIP makes independent — routable to the float64 companion
    as a **superset**, with `valueInRange` kept as the exact **residual filter**
    over the seek's output; and removed the `btreeRanger[int64]` arm, because once
    the comparison unified the two numeric kinds an int64-keyed index became a
    *subset* of the answer (it cannot hold the float-valued nodes a numeric range
    must also match) and a subset cannot be repaired by a residual filter. The
    probe was promoted from telemetry to the asserted `range-mixed` clause, with
    a `vacuity:mixed-kind` / `vacuity:mixed-kind-non-empty` pair in
    `Finish` and a `vacuity:numeric-seek` gate so the seek arm cannot silently
    degrade to a scan. TWO windows run under that clause, one per half of the
    fix: `range-mixed-point` (FLOAT64 bounds over an INT64 property — the
    divergence itself) and `range-mixed-bounds` (bounds of DIFFERENT kinds, the
    shape `trySeekRange` used to refuse outright when `lo.Kind() != hi.Kind()`).
    MEASURED after the fix, same host and same 24-seed soak sweep: **32–42**
    mixed-kind probes per seed, **every one with a non-empty answer and every one
    agreeing**, with the numeric companion eligible at **every** battery
    (`numeric=16/0 … 21/0`).

    MEASURED cost, `graph/query` benchmarks at 200 000 nodes with ages drawn
    uniformly from [21, 65] (Apple M4, go1.26.6, `-benchmem -count=6`,
    `benchstat`): against the path this query took BEFORE the fix — a full scan,
    because an `Int64Value`-bounded range was never index-served — the new
    seek-plus-residual path is **31.8× faster** on a selective window
    (44.19 ms → 1.39 ms for `age ∈ [30, 31]`) and **1.4% slower** on a
    100%-selectivity window (47.91 ms → 48.56 ms for `age ∈ [21, 65]`), with
    B/op and allocs/op **identical** — the residual filter reads properties, it
    allocates nothing. Against the pre-#2600 *float64-bounded* behaviour, which
    took the seek and skipped the comparison, the residual costs **11.1×**
    (125.7 µs → 1390 µs) selective and **18.7×** (2.59 ms → 48.56 ms) broad; that
    arm returned a different, over-returning answer, so the comparison prices
    EXACTNESS rather than a lost optimisation. A follow-up that would recover most
    of it is noted in the debt section below.

    A second finding from the same measurement was **partly wrong** and is
    corrected here: the `hashLookuper[int64]`/`[float64]`/`[bool]` arms of
    `index_seek.go` are dead against every *engine-created* index — a `hash`
    `CREATE INDEX` builds a `hash.Index[string]` — but they are NOT dead code.
    They are reachable through `hash.NewBound` from `graph/query`'s own public
    API, and `TestSeek_EqualityMatchesScan_AllKinds`
    (`graph/query/index_seek_test.go`) exercises all four. They also remain sound,
    because equality is *not* unified across kinds: `query.equalValue` still
    requires both sides to share a kind, so a single-kind index is an exact mirror
    of the equality it serves. Only the *range* arm had to go.


12. **A bit-packed BOOL edge-property column PANICKED on the fused append
    path.** `edgePropColumn.grownWithValue` and `edgePropColumn.grownAbsentShared`
    — the two column-level halves of the fused build fast path
    `edgePropCols.GrowSlotWithValue` drives — converted a DENSE column to the
    sparse (COO) representation **unconditionally**. A bit-packed bool column has
    no sparse representation, and the file says so in three places:
    `allocSparseBacking`'s bool arm is commented "Unreachable: bool never goes
    sparse", `appendSparseValueFromDense` has no bool case at all (so a bool
    column falls through to the `boxed` backing, which is `nil` for bool), and
    `edgePropColumn.slotValue`'s bool arm reads the bit at the backing index on
    the assumption that "bool is never sparse, so i == slot", which holds only
    while the column is dense. The result was
    `panic: runtime error: index out of range [n] with length 0` inside
    `edgePropColumn.toSparse`, reachable from the public
    `Graph.AddEdgeLabeledWithProperty` on a **three-node, two-call fixture** with
    no concurrency, no store, no schema and no DST:

    ```go
    g.AddEdge("a", "b", 1)
    g.SetEdgeProperty("a", "b", "flag", BoolValue(true)) // general path -> DENSE bool column
    g.AddEdgeLabeledWithProperty("a", "c", 1, "REL", "n", Int64Value(7)) // PANIC
    ```

    Two sibling functions already carried the guard — `newSparseSingleSlot`
    builds a bool single-slot column dense, and `edgePropColumn.reshaped` never
    demotes bool because `demoteThreshold` returns `-1` for it — which is exactly
    what made the two fused paths' omission invisible: every *other* route into
    the representation change was already correct, so the invariant held
    everywhere it was checked. Nothing had ever combined a bool edge property with
    a fused append, and the caller census says why: MEASURED across the tree, the
    ONLY non-test caller of `Graph.AddEdgeLabeledWithProperty` is
    `examples/26_social_scale_bench`, which passes a `lpg.DateValue`. Every other
    call site is a test, and each carries an INTEGER or a STRING. The method's own
    godoc describes it as "the simple single-edge-per-pair case the bulk builders
    use", which is what it is FOR; what it is actually called by, today, is one
    example and a handful of fixtures — none of them with a BOOLEAN.

    Found by `typed-schema` (rmp #2493) on its first run: the scenario sweeps five
    write paths x four declared kinds, so the (fused append, BOOLEAN) combination
    is reached by CONSTRUCTION rather than by a draw. **Fixed** — both functions
    now keep a bool column dense, delegating to the existing
    `edgePropColumn.grown`, which has always handled `PropBool` correctly. The
    dense absent-grow costs `O(length/64)` bitmap words instead of the `O(1)`
    amortised tail push the sparse path buys for every other kind; that is what
    the general mutation path already pays for bool, it is 64x cheaper than the
    same copy on an 8-byte value, and correctness outranks the constant.
    `graph/lpg/edge_prop_bool_fused_test.go` is the regression gate, and each half
    of the fix was verified load-bearing by a controlled revert: reverting the
    `grownWithValue` guard alone panics `TestFusedAppend_BoolColumnAsTarget` and
    `…AcrossManySlots` while `…AsNonTarget` still passes, and reverting the
    `grownAbsentShared` guard alone panics `…AsNonTarget` and `…AcrossManySlots`
    while `…AsTarget` still passes.

13. **A validator-REFUSED value becomes durable, and recovery resurrects it, on
    the pure `store/txn` path.** The two durable paths order the validator
    differently, and only one of them is safe:

    | Path | Order | Consequence |
    |---|---|---|
    | Cypher engine (`walMutatorAdapter.SetNodeProperty`) | validated write, **then** buffer the WAL op | a refused value never reaches the log |
    | `store/txn` (`txn.Tx.Commit`) | append + **fsync** every buffered op, **then** apply through the `WriteView` | a refused value is already durable when it is refused |

    MEASURED. A transaction buffering `AddNode` + `SetNodeProperty("age",
    StringValue(...))` against a graph whose installed schema declares `age` as
    `PropInt64` returns `txn.ErrCommittedNotApplied` wrapping
    `schema.ErrTypeMismatch`, and leaves the LIVE graph correctly without the
    property. After a host crash and a reopen, the recovered graph carries `age`
    **as a STRING**: four WAL ops replay, and the replay path installs no
    validator — by design, since `Graph.SetEdgePropertyByHandleID`'s godoc states
    that values replayed there "were validated at the time of the original write
    and must not fail during recovery". On this path they were not. The promise in
    `ErrCommittedNotApplied`'s own godoc — "recovery will reconcile" — is what
    materialises the value.

    The exposure is confined to an embedder that drives `store/txn` directly with
    a validator installed; `store.DB` and every Cypher-driven path validate first.
    It was **pinned rather than fixed** at first, because changing the commit
    ordering is a durability-contract decision and not a test fix:
    `checkTypedSchemaPureStore` asserted the behaviour as measured, in both
    directions, and its message named exactly what to update when the ordering
    changed — the pin, the file header and this document.
    **FIXED (rmp #2602, `fd7159d6`), and the arm inverted with it.** The contract
    was put to the user with three options and validating at BUFFER time was
    chosen: `Tx.SetNodeProperty`, `SetEdgeProperty` and `SetEdgePropertyByHandle`
    now reject before the op reaches the buffer, so the frame is never appended and
    never fsynced, and `lpg.Graph.ValidateProperty` exists for that call. This also
    makes `store/txn` agree with the Cypher path, which has always validated before
    buffering. Measured after: `refusedAtBuffer=true`, `notApplied=false`,
    `resurrected=false`, `storedAfterRecovery=absent`, and `walOps` down from 4 to
    3 — the refused frame is gone from the log. **The cost flagged when
    recommending it did bite:** the Cypher adapter validated and then buffered, so
    every engine write validated TWICE — and a `SchemaValidator` may be stateful, so
    a counting validator saw the wrong write refused; worse, the adapter DISCARDED
    that call's error (`_ =`, annotated "ErrTxFinished impossible here", a premise
    this change falsified), so the new guard fired on the engine path and was
    swallowed. Both are closed by explicit pre-validated entry points
    (`SetNodePropertyPreValidated` and its two siblings) for the one caller that has
    already validated the same value against the same graph. The narrative section
    above records the same outcome; the two statements agree.
    See [The typed schema as a runtime enforcement hook (rmp #2493)](#the-typed-schema-as-a-runtime-enforcement-hook-rmp-2493).

14. **The single-edge anchor swap drops every row when the pattern's SOURCE node
    is anonymous** (fail-silent, wrong answer). With the swap admissible,
    `MATCH (:Person)-[:KNOWS]->(:Vip) RETURN count(*)` returns **0** where
    `MATCH (a:Person)-[:KNOWS]->(b:Vip) RETURN count(*)` returns 1 over the same
    data. Reproducible with **no store, no recovery and no simulator** — a plain
    `cypher.NewEngine` over one `(:Person)-[:KNOWS]->(:Person:Vip)` edge plus
    forty bare Persons. Naming the source fixes it; naming the destination does
    not. All four spellings render the identical `EXPLAIN` tree
    (`NodeByLabelScan [Vip] -> Expand -> Filter`), so the plan text cannot tell
    them apart; `PROFILE` localises the loss to the `Filter` above the re-rooted
    `Expand`, which receives one row and emits zero. Attributed by A/B:
    `EngineOptions{DisableAnchorSwap: true}` makes all four return 1. Cause:
    `matchNodeScan` (`cypher/ir/match.go`) leaves an anonymous node's variable
    name as the **empty string**, so `matchAnchorSite` records `fromVar == ""` and
    `mirrorAnchorSite` (`cypher/anchor_swap_plan.go`) re-checks the from-label as
    `Selection{LabelPredicate{Receiver: Variable{Name: ""}}}` above the re-rooted
    expand — a receiver that does not resolve to that expand's destination
    binding. The count store is what makes it reachable: the swap needs every cost
    input `EstExact ∧ ¬dirty`, so a relabel's dirty marking vetoes it and the
    anonymous spelling answers correctly *until a reopen clears the flags*. The
    swap's own differential suites missed it because, MEASURED, all 24 of their
    `MATCH` patterns name both endpoints — the only anonymous labelled nodes in
    either file are in fixture-building `CREATE` clauses. The openCypher TCK does
    cover the shape (`clauses/match/Match2.feature` scenario [2], which requires
    one row from `MATCH (:A)-[r]->(:B) RETURN r`) but is immune twice over: its
    relationship is untyped, so the pattern is not a swap candidate, and its graph
    is balanced, so the 2x margin is unreachable. MEASURED: adding a relationship
    type and 40 extra `(:A)` nodes to that same fixture makes it return 0. The
    whole suite is green with the defect present — `go test -race ./cypher/` passes
    and the TCK gate reports 3897/3897, 0 failed, 0 undefined. Found by
    `count-store` (rmp #2494) on its first run. **FIXED in rmp #2603**:
    `matchAnchorSite` declines a site whose endpoint variable name is empty, which
    is what an anonymous pattern head has, so the written order stands for these
    patterns. Gated by
    `TestCountStore_AnchorSwapRetainsAnonymousSourceRows` here and by
    `cypher/anchor_swap_anonymous_test.go` in the planner's own package.
15. **`count.Store.Snapshot`'s godoc contradicted its code** (documentation).
    It said it returns "every live cell (value > 0)"; the predicate has always
    been `v != 0`, and must be, because `Store.add` deliberately RETAINS a cell
    driven negative (rmp #2303) — that retention is what makes the aggregate
    order-insensitive. MEASURED, a negative cell is reachable from ordinary Cypher
    with no concurrency: `SET a:X`, `SET b:X`, `REMOVE a:X` over an `a -> b` edge
    leaves `T(X, KNOWS, X)` at -1. A consumer trusting the doc would assume every
    present value is positive. **Fixed** in this task: the doc now states the
    real predicate and why negative cells exist.

16. **`AddRange`/`RemoveRange` silently drop the WHOLE range when it ends at
    `math.MaxUint64`** (fail-silent, latent). `index.NodeSet.AddRange` converts
    the inclusive upper endpoint to roaring's exclusive one with `to+1`
    (`graph/index/nodeset.go:339`), and `RemoveRange` does the same in its
    **bitmap branch** (`:378`). At `to == math.MaxUint64` that wraps to zero and
    roaring treats `start >= end` as a no-op. The loss is total, not off-by-one:
    MEASURED, `AddRange(max-5, max)` yields cardinality **0** where the closed
    interval names 6, and `RemoveRange(max-3, max)` over a five-element
    **bitmap-tier** set removes **nothing**. The control one id lower is exact
    (`AddRange(max-5, max-1)` yields 5), so the loss is attributable to the final
    id and not to the top of the id space. **The two tiers disagree, and only for
    `RemoveRange`**: its singleton and small branches filter on the closed
    interval directly (`v < from || v > to`, `nodeset.go:349-370`) with no `+1`
    and so no wrap, and MEASURED the identical call over a five-element
    **inline-tier** set leaves 1 — correct. The same logical operation on the
    same membership therefore answers differently depending on a tier the public
    surface does not expose. `AddRange` has no such split, promoting before it
    reads the interval, so it is uniformly wrong at the boundary.
    It was **latent** — neither range method has a production caller, and no graph
    in this module mints a NodeID there — and reported rather than fixed, because
    the repair is a design choice (split the call, saturate, or refuse) that changes
    behaviour at the boundary. It was pinned to the measured behaviour by
    `label-index-scoped`'s `boundary-pin`.
    **FIXED (rmp #2607, `ab0e9832`).** The choice is **split**, because it is the
    only one of the three that keeps the documented closed-interval contract:
    saturating silently drops the top id and refusing changes the public surface.
    The dependency's own source settles that a split is required rather than
    avoidable — roaring64's `AddRange`/`RemoveRange` are half-open over `uint64`
    with no closed variant (`roaring/v2@v2.18.2/roaring64/roaring64.go:1054,1079`),
    while the 32-bit sibling escapes the same problem only by widening its bound to
    `uint64` so it can name `MaxUint32+1` (`roaring.go:1958`); at 64 bits no wider
    type exists, so the top element is handled separately. Both directions now
    route through `addRangeClosed` / `removeRangeClosed`, which add or remove
    `[from, max)` and then the top element itself; an inverted interval is a no-op,
    matching roaring's own semantics. The pin is **inverted, not deleted**:
    `liPerturbBoundaryFixed` (which simulated the fix) becomes
    `liPerturbBoundaryWraps` (which reproduces the overflow), so the arm still has a
    demonstrated way to fail, and the arm now also drives `RemoveRange` over an
    INLINE-tier label of identical membership and asserts the two tiers agree —
    tier agreement being the half a single-tier arm cannot see.
17. **An inverted or empty `AddRange` creates a permanent, serialized entry for a
    label with nothing in it** (unbounded growth, latent).
    `label.Index.AddRange` stores the `NodeSet` back unconditionally
    (`graph/index/label/index.go:151-153`) and `NodeSet.AddRange` promotes to the
    bitmap tier BEFORE it looks at the interval (`nodeset.go:326-339`), so an
    interval naming no ids still leaves a bitmap behind. The entry is invisible
    to every query path — `Count` 0, `Scan` nil, `Has` false — and permanent.
    MEASURED: 1 000 inverted `AddRange` calls on distinct labels turn a 16-byte
    empty image into a **20 016-byte** one declaring 1 000 labels, 20 bytes
    apiece, none carrying an id. `RemoveRange`'s own godoc promises the opposite
    for its direction ("Empty bitmaps are deleted so the map does not grow
    unboundedly") and MEASURED keeps that promise; `AddRange` makes no such claim
    and does not behave that way. It was **latent** for the same reason as #15, and
    reported rather than fixed; it was pinned by `label-index-scoped`'s
    `phantom-pin`, which recorded the defective numbers deliberately.
    **FIXED (rmp #2608, `064ed6ce`).** `AddRange` now mirrors `RemoveRange`'s
    delete-on-empty. **BOTH** prescribed repairs are applied, because they fix
    different halves and neither alone suffices — verified rather than assumed:
    with only the store-back guard, `NodeSet.AddRange` would still promote an
    EXISTING inline label to the bitmap tier on an inverted range, and promotion is
    one-way, so the waste would be permanent; with only the early return,
    `label.Index.AddRange` would still read the zero-value `NodeSet`, call a no-op
    and store it back, so the entry would still be minted and `labelCount` would
    still count it. So `NodeSet.AddRange` returns before promoting when
    `from > to`, and `label.Index.AddRange` deletes rather than stores a set that is
    empty after the call — a branch reachable only when the label had no entry,
    since `AddRange` cannot empty a set that already held ids. **One probe was
    VACUOUS and was replaced:** a label-level test comparing serialized bytes before
    and after an inverted `AddRange` on an existing label CANNOT detect the
    promotion, because an inline set and a bitmap holding the same ids serialize
    byte-identically — which is exactly the #1585 zero-migration guarantee. The tier
    tag is the only honest probe, so that half moved into `graph/index`. The pin is
    inverted into a regression arm — `liPerturbPhantomGone` becomes
    `liPerturbPhantomKept` — gains a CONTROL (a range naming five ids, which must
    still be recorded), and `gate:phantom-armed` is re-pointed at that control,
    since "no entry was created" is otherwise satisfied by a harness that measured
    nothing. **Recorded, not fixed, and out of scope:** `Deserialize` still
    re-materialises an empty-bitmap entry from a legacy image written by the
    defective writer, because dropping them in the reader would change round-trip
    byte semantics for existing images.
18. **The serialized label index is not idempotent for a run-encoded label small
    enough to be down-converted** (encoding instability, latent). A label built
    by `AddRange` holds a roaring RUN container; if its cardinality is at most
    `smallSetMax` (8), `index.NodeSetFromBitmap` moves it to the inline tier when
    the image is read back, and the inline tier re-materialises through `AddMany`
    as an ARRAY container. So the image the reader emits is not the image it was
    handed. MEASURED, the window is exactly **[4, 8]**: at widths 1-3 roaring
    keeps an array container and nothing changes; at 4-8 the image goes 55 bytes
    in, 64/66/68/70/72 out; at 9 and above the down-convert does not run and 55
    bytes come back unchanged. Every Add-built label is stable at every width, so
    the instability is attributable to the run encoding. It converges after
    exactly one cycle, and no content is ever lost — but a checkpoint, reload and
    re-checkpoint produces different bytes for the same logical state, which is
    what a fixture diff, a content-addressed store, or an incremental backup's
    deduplication relies on not happening. It was **latent** — `AddRange` has no
    production caller, so no production label is ever a run container — and
    reported rather than fixed; it was pinned by `label-index-scoped`'s
    `dense-small-pin`, with the exact window swept under soak.
    **FIXED (rmp #2609, `f917b9c1`), and the ACCEPTANCE CRITERIA WERE AMENDED with
    the user's decision recorded.** As filed they asked for the image to be a
    function of the logical contents at EVERY width. Measured, that costs more than
    the report knew: `Serialize` holds only an `RLock` and `NodeSet.Bitmap` hands
    back the LIVE bitmap, so normalising an arbitrary set needs a full `Clone`
    first, and roaring's run optimisation rewrites containers in place so
    copy-on-write does not help — on a sparse 100k-id label that measured
    **6.55 → 90 µs/op and 1 289 → 218 065 B/op** to produce a byte-identical image.
    The user chose to bound the normalisation at `smallSetMax`, which is exactly the
    band the reader down-converts and therefore the only band where a cycle can
    change the encoding; the AC's second clause is now bounded to widths 1..8 while
    the idempotence clause still covers 1..16. Within that bound, normalise **DOWN
    rather than UP**: re-materialising from the sorted ids reaches the same canonical
    form as `RunOptimize` with strictly less work, and leaves every image an existing
    caller can produce byte-identical. The public seam is
    `NodeSet.CanonicalBitmap`. **Allocations are IDENTICAL, every sample equal**, for
    a 100k dense label, a 100k sparse one and an `Add`-built label of 8 ids; only an
    `AddRange`-built label at or below the bound moves, 16 → 28 allocs/op, which is
    what the `Add`-built label of the same ids already cost. A first, SEQUENTIAL A/B
    reported a −8.57 % speedup on the dense label; **interleaving REFUTED it
    (p=0.841)**, so it was an artefact and is not claimed. All three defects this
    scenario witnessed are now fixed, so the pin is inverted into a regression arm
    (`liPerturbDenseSmallStable` becomes `liPerturbDenseSmallUnstable`, reproducing
    the 55→72 re-encoding), and `gate:range-tier-crossover` is KEPT, as the user
    chose: the crossover moved from width 4 to `smallSetMax` rather than
    disappearing, so `liRangeTierWidths` gains 9, 12 and 16 and the gate still
    brackets both answers.

19. **Exported `search` entry points ran to completion under an already-cancelled
    context, one of them returning the true, complete answer with a nil error.**
    Found by the concurrency audit that preceded the DST cancellation battery
    (rmp #2489), filed and fixed as **rmp #2593** before the battery was written,
    so the battery asserts the mandate rather than the shortfall. WORST CASE:
    `flow.PushRelabelMaxFlowCtx` returned the correct maximum flow with a **nil**
    error under a dead context, measured at `MaxNodeID` up to 1536 — a
    request-scoped deadline fires, the caller's context is dead, the library does
    the full work and reports success, and nothing downstream can tell. One root
    cause behind five of the six sites: **increment-then-mask**
    (`c++; if c&0xFFF == 0`), so the counter is 1 on the first iteration and the
    mask first trips at 4096 units of work — any input below that stride returned
    a complete answer under a dead context. Measured with a pre-cancelled context
    at 8 nodes, all of these returned nil with a complete result:
    `BellmanFordCtx`, `BellmanFordInto`, `KCoreCtx`, `KShortestPathsLooplessCtx`,
    `KShortestPathsLooplessCtxWithOpts`, the deprecated `EppsteinKShortestCtx`
    that delegates to it, and `PushRelabelMaxFlowCtx`. The correct idiom already
    existed in the same codebase (`search/dijkstra.go`, `search/prim.go` poll on
    iteration 0), and all five sites are check-then-increment now; the k-shortest
    family additionally gained an entry poll, placed in the `WithOpts` form so one
    poll covers all three callers. `TopologicalSortCtx` was a different mechanism:
    on a fully cyclic graph every vertex has indegree ≥ 1, so the polled Kahn loop
    never runs and `ErrCycle` outranked cancellation at every input size. A
    **sixth site** was missed by that first sweep and found by the battery it was
    meant to unblock: `bellmanFordVirtualSource` (`search/johnson.go`) is a shared
    prologue, so neither `JohnsonAPSPCtx` nor `JohnsonAPSPParallelCtx` contained
    the defect in its own file — see
    [the cancellation battery](#every-public-context-accepting-search-entry-point-under-cancellation-rmp-2489)
    for why the battery's main row loop could not see it either. `discharge`
    (`search/flow/push_relabel.go`) gained its own poll, decided by measurement:
    one discharge reaches 600,004 inner steps at 200k vertices, 146× the whole
    stride, so counting discharges left the inter-poll interval unbounded in input
    size; the poll costs +3.3% to +4.6% on one adversarial fan-out shape, accepted
    under correct → secure → fast and recorded as the only measurable throughput
    cost. The count is closed by SCAN rather than by trust: a detector for the
    shape, run over all 47 stride-gated polls in the module, leaves exactly two
    increment-then-mask sites, both benign because their callers poll
    unconditionally first (`search/flow/dinic.go:152` and
    `cypher/exec/hash_join.go:171`; the latter is outside the change's scope and
    untouched). **No live-context result changed, and that is measured**: 3,622
    byte-identical signatures before and after, including 540 Johnson ones at two
    resolutions, one hashing `johnsonPrepare`'s potentials directly so a prologue
    change would show even if it cancelled out downstream. Godoc was corrected
    throughout — `bellman_ford.go` and both Johnson entry points had documented a
    check "at every relaxation-round boundary" over what is SPFA on a deque, and
    all of them claimed a wrapped `ctx.Err()` where none wraps. Fixed in
    `ac16a8c9`; the DST regression guard is the cancellation battery
    (`internal/sim/search_ctx_cancel.go`), which re-drives all 58 entry points
    after every crash and recovery.

20. **A refused Bolt re-authentication left the connection running as the PREVIOUS
    principal** (security; unauthorised write capability). `handleLogon`'s
    non-`firstAuth` failure branch was the only exit that set neither `s.identity`
    nor `s.authenticated` — the assignments sit after the error return — so a refused
    identity switch changed nothing, and `handleReset` then took the authenticated
    path back to `READY` with full write capability. MEASURED end to end over a real
    socket: `LOGON(alice, ok)`, `LOGON(bob, WRONG)` → FAILURE, `RUN` → IGNORED,
    `RESET` → SUCCESS, `CREATE (:Ghost)` → SUCCESS, nodes-created **1**. The identity
    is security-relevant even without roles: it keys the per-principal transaction
    quota and the `SHOW TRANSACTIONS` attribution, so a refused switch left both
    pointing at the wrong principal.
    **FIXED (rmp #2556, `372c7520`).** The contract comes from the SPECIFICATION, not
    from preference: the Bolt `LOGON` section states that a failed authentication
    makes the server respond FAILURE and close the connection, and carves out no
    exception for re-authentication; the user chose to terminate on ANY failed
    `LOGON`. `enterFailed` runs before `DEFUNCT` rather than a raw state set, because
    `LOGON` is legal in `TX_READY` and `enterFailed` is the audited reclaim (#1312).
    **WHY NOTHING CAUGHT IT:** every existing test and DST arm sends `LOGOFF` first,
    which pre-clears the flag, so the branch was exercised nowhere — and the official
    `neo4j-go-driver` always emits `LOGOFF` before `LOGON`, so the branch was
    reachable only by a non-conforming client, exactly an attacker's shape, because
    skipping the `LOGOFF` was how a failed credential guess cost nothing. The new arm
    `reauth-wrong-password-no-logoff` omits the `LOGOFF` and attempts the write AFTER
    a `RESET`, because `RESET` is where the recovery happened — an arm that only
    checked the reply to the `LOGON` would have passed against the defect. It is
    registered in `boltAuthExpectedArms` so it cannot silently stop running. The first
    version of the new unit test was VACUOUS and said so: `newReadySession` installs
    `NoAuthHandler{}`, which accepts every credential, so the run duly reported that a
    wrong-credential `LOGON` had been accepted.
21. **`store/csrfile` followed a symlinked final component on both the write and the
    read path** (security, CWE-59 link following; arbitrary-file overwrite). Both
    sibling publish paths were hardened against exactly this class under rmp #1843 —
    `store/wal` has `walNoFollow`, `store/snapshot` has `openSnapshotComponent` — and
    `csrfile` alone was missing it. The write side is the sharper half because the temp
    name is FULLY PREDICTABLE: the writer forms it as `OutputPath + ".tmp"`, so a local
    principal who can write the store directory pre-plants a symlink there aimed at any
    file this process may write. REPRODUCED, with all three parts of the audit's claim
    MEASURED rather than argued — with the guard disabled the regression test reports
    that the publish **SUCCEEDED** through the symlinked temp, that the victim file went
    from 60 bytes to **2 244 bytes of CSR data**, and that the published path itself
    **became a SYMLINK**, because `rename(2)` moved the planted link onto the output
    name, so every later write through it lands on the victim. The read path followed a
    symlink too.
    **FIXED (rmp #2580, `7a4bbfcc`).** The guard mirrors the existing pattern rather
    than inventing one: a build-tagged `csrNoFollow`, `syscall.O_NOFOLLOW` on unix and
    zero elsewhere, applied in `osFS.Create` and in the reader's open. **THE TOCTOU IS
    CLOSED SEPARATELY, AND IT HAD TO BE:** the writer resized the temp with a
    PATH-based `os.Truncate` moments after creating it, which re-resolves a predictable
    name and is a second window even with `O_NOFOLLOW` on the create; `Truncate` moves
    onto the already-open descriptor, so there is no second name resolution to race.
    That changes the `csrfile` FS seam — `Truncate` leaves the `fs` interface and joins
    `File`, whose only two implementations already satisfy it (`*os.File` natively, and
    `*sim.SimFileHandle` because it grew the same method) — and the two now-orphaned
    seam methods are removed rather than left dangling, verified to have no callers
    first. The publish ordering (tmp, fsync, rename, parent-fsync) is untouched. The
    negatives are paired with a POSITIVE CONTROL, without which a guard that refused
    every open would pass both: an ordinary publish and open still work, and so does a
    publish through a symlinked PARENT directory, which pins the guard's scope to the
    final component instead of leaving it to the comment.
22. **`MERGE` decided its match from the RAW graph, not the transaction's view, and
    created a DUPLICATE** (fail-silent, wrong data; ACID Isolation).
    `GraphMutator.NodeLabels` and `NodeProperties` are bare shard reads returning the
    NEWEST stored value, which includes other in-flight transactions' eager,
    uncommitted writes. Reachable for the same reason as #2353 and #2355: conflicts are
    per SUBSTORE, so a transaction writing the label never collides with one reading it
    to make a match decision. REPRODUCED FIRST, and the result is not what the report
    predicted: the predicted direction — a peer's uncommitted ADD making `MERGE` match
    — does NOT reproduce, because the enumeration feeding the filter is already
    view-resolved; the MIRROR does, a peer's uncommitted label REMOVAL hiding a node
    that carries the label in every committed state. Diagnosing rather than assuming
    located the fix: asking `MATCH` the same question in the same transaction settled
    it — `MATCH (n:Target {k:'y'}) RETURN count(n)` answered **1** while
    `MERGE (n:Target {k:'y'})` duplicated, so `MERGE` CONTRADICTED `MATCH` inside one
    transaction and the candidate was enumerated correctly and then dropped by the
    comparison. The defect is at the decision site, which is why the fix is there and
    not in the enumeration.
    **FIXED (rmp #2365, `56359836`).** Labels go through `exec.labelsInTx`, which
    #2355 built for exactly this; properties through a new
    `nodeMatchesAllPropertiesInTx`, which reads per KEY via the transaction view's
    `NodePropertyInTx` rather than fetching the whole bag. Probing after the label half
    was fixed found the PROPERTY half has the identical exposure at the same three call
    sites, and it is fixed here too — leaving it would have shipped a `MERGE` still
    reproducing the user-visible symptom through the adjacent read on the same line.
    **A BETTER OUTCOME THAN A DUPLICATE, and worth stating:** where the peer's write
    and `MERGE`'s `ON MATCH` action touch the same substore, `MERGE` now correctly
    takes `ON MATCH` and the action then meets a real serialization conflict — refused,
    not duplicated. Refusing is what the ACID contract asks for; duplicating is what it
    forbids. Four of the seven regression tests would pass on a `MERGE` that was broken
    outright, so each negative is paired with a positive control. **Left alone and
    recorded rather than swept in:** `nodeMatchesAllProperties` still serves the
    EDGE-property reads in `merge_pattern.go`, the same class on a different surface,
    which this ticket did not reproduce.
23. **`UNION` in a subquery body silently dropped every branch but the first**
    (fail-silent, wrong answer). A `UNION` inside `EXISTS { }` or `COUNT { }` parsed
    cleanly and the second branch was discarded: the grammar admits `regularQuery` in
    both positions, but `ast.ExistsSubquery.Query` and `ast.CountSubquery.Query` are
    typed `*ast.SingleQuery` and cannot hold one, so the visitor kept `Parts[0]` and
    discarded the rest — "multi-union inside EXISTS is unusual" — with no error and no
    notification. MEASURED on a node with a `:W` edge and no `:Z` edge:
    `EXISTS { MATCH (x)-[:Z]->() RETURN 1 UNION MATCH (x)-[:W]->() RETURN 1 }` returned
    **false**, where the second branch matches. **THE REPORT'S OWN `COUNT` EXAMPLE
    COULD NOT HAVE CAUGHT THIS**, and the regression test does not use it: in
    `COUNT { … RETURN 1 UNION … RETURN 1 }` both branches return the same row, `UNION`
    de-duplicates, and the correct answer is 1 — exactly what dropping a branch also
    gives. The tests use branches returning DIFFERENT values.
    **REFUSED RATHER THAN SUPPORTED (rmp #2615, `9103d5e5`), decided with the user, and
    the divergence is stated in the code rather than hidden.** BOTH REFERENCE ENGINES
    ANSWER this query, read from their grammar source: Neo4j's `existsExpression` and
    `countExpression` admit `regularQuery` (`Cypher5Parser.g4:33-35, 671-677`), and
    Memgraph's `existsSubquery` and `countSubquery` admit `cypherQuery`, which carries
    `cypherUnion` (`Cypher.g4:73-81, 317-323`). So GoGraph now refuses what both answer
    — a stopgap, because a silent wrong answer is a DEFECT and reference parity is a
    FEATURE. Support is filed as **#2627** with that evidence and with the
    UNION-versus-UNION-ALL semantics the refusal sidesteps. **The openCypher 9 TCK does
    not cover subquery expressions at all** — zero occurrences of `EXISTS {` or
    `COUNT {` across every feature file, searched brace-aware and multiline — so
    neither refusing nor supporting can move the conformance count, and no TCK-covered
    semantics constrain the choice. Both visitors refuse through ONE helper so they
    cannot drift, which mattered here: the `COUNT` twin had the identical silent drop
    with no comment at all and no entry in the acceptance matrix, while the `EXISTS` one
    at least carried a comment. The matrix now has both, and its exists-union row is
    INVERTED from accepted to rejected with the reason recorded rather than deleted.
24. **An unreproducible `NodeID` was attributed to the SNAPSHOT rather than to the KEY
    TYPE** (misattribution; no data loss, and no silent loss either).
    `mapperShardFor` hashes an uncovered comparable key through
    `fmt.Fprintf(h, "%v", v)`, which renders a pointer as an ADDRESS, so a key carrying
    one hashes to a different shard on every run — while the function's godoc promised
    the hash was stable across processes, and FNV was chosen precisely to guarantee
    that. REPRODUCED DETERMINISTICALLY IN PROCESS, which is stronger than the subprocess
    reopen the task prescribed: two allocations of the same logical pointer-bearing key
    already hash to different shards, so no second address space is needed and, unlike a
    subprocess run, this cannot agree by coincidence.
    **Two findings changed what the fix should be, and neither was in the report.** THE
    LOSS IS NOT SILENT: `Mapper.LoadFrom` already enforces that an entry's packed shard
    equals `mapperShardFor(key)` and returns `ErrMapperEntryCorrupted`, and
    `store/snapshot/apply.go` propagates it and aborts before any label or property is
    applied — the reported drift does not happen, the restore fails loudly. AND NO
    BETTER HASH COULD FIX SUCH A KEY: for every comparable Go type, "formats as an
    address" and "compares by address" coincide, so a key decoded into a fresh
    allocation is not the key that was written even with identical data — its IDENTITY,
    not merely its shard, is unreproducible, and an address-independent hash would have
    made the hash agree while the map still created a second entry, which looks fixed
    and is not.
    **FIXED as a MISATTRIBUTION defect (rmp #2528, `8be7afcb`).** "Entries corrupted"
    blames the snapshot writer or the disk for a key-type defect, sending diagnosis to
    re-reading a file that no re-read can fix. `LoadFrom` now inspects the key on that
    failure path — free, since it is already failing — and when it finds a pointer,
    `unsafe.Pointer` or channel anywhere in the value it reports
    `ErrMapperKeyNotPortable` wrapped inside `ErrMapperEntryCorrupted`, naming the
    offending field path (`key.P`, `key.Inner.P`, `key.A[0]`). It inspects the VALUE,
    not only the static type, because an interface field's dynamic type is what decides.
    A portable key with a genuinely wrong recorded shard is still reported as
    corruption, which a test asserts in that direction too. The audit for other
    `%v`-based hashing or ordering is recorded: `mapperShardFor` is the only `%v` hash
    in the tree; the two cross-release record renderers compare through `%v` but fail
    loudly if ever handed a pointer; and the graphml and Bolt `%v` arms are default
    branches of switches that already cover every case their input can hold
    (`lpg.PropertyValue` has exactly seven kinds and all seven are handled).
25. **Recovery derived the transaction sequence on every open and EVERY consumer
    discarded it** (fail-silent; sequence reuse over live WAL entries). Its godoc said
    the value exists to seed the store so a sequence is never minted twice. MEASURED: a
    store reopened on a non-empty WAL **re-minted sequences 1, 2 and 3** while
    transactions carrying those values were still in that same WAL. The census over the
    tree was total: **no shipped embedder wired it.**
    **FIXED (rmp #2522, `4df36ef5`).** A contract that every caller violates is
    MIS-PLACED, not universally mis-implemented — and this codebase had already reached
    that conclusion one field over, where recovery restores the MVCC clock itself and
    its comment names this very field as the negative example: a restoration that every
    reopen path must remember is one some reopen path will forget. So the recovery
    result now BUILDS the transactional store (`Result.NewStore`,
    `Result.NewStoreCapped`), binding its own recovered graph and derived sequence to a
    caller-supplied WAL writer; the floor never leaves the package and the omission
    becomes inexpressible. `recovery.Options` is unchanged, so every existing caller
    compiles untouched. Returning a fully-open store was REJECTED — deciding whether to
    append onto an unclean recovery must stay with the caller — and a caller-set field
    was rejected too, because Go cannot make one mandatory and a positional argument
    still accepts zero. The sequence is RATCHETED, never assigned. **Three shipped
    examples were wrong and are fixed**, one of them a long-lived HTTP API serving
    writes off a reopened store; both exposed sites already fail-stopped on an unclean
    result before appending and still dropped the sequence, which is the clearest
    evidence the field sat in the wrong place. The MVCC clock floor belonged in the
    library for the same reason and moves into the WAL-only replay path, with the
    harness workaround deleted and its redundancy proven by the suite rather than
    assumed. Both regression tests read sequences back off disk and assert a PROPERTY,
    not fixed numbers, and both are mutation-proved by breaking the library: the
    disabled ratchet reproduces the reported collision exactly. The architectural root
    cause — that `store.Close` exists and no composed `store.Open` does — is filed as
    **#2523**.
26. **Two planner cost floors were set by assumption, and one of them inverted the
    plan-choice rule** (performance; results identical, no data loss).
    `MATCH (n:Common:Rare) RETURN n.k` over 100 000 `:Common` of which 1 000 also carry
    `:Rare` planned a morsel-parallel scan of `:Common` instead of the serial scan
    re-anchored on `:Rare` — **4.611 ms against 0.186 ms** for the plan the same engine
    already knew how to build, and worse than the 4.280 ms legacy full-`:Common` scan
    the re-anchor was written to replace. Separately, an equality lookup on an indexed
    property cost **68.7 µs over a label of 1023 nodes and 5.5 µs over one of 1024** —
    a 12.6× cliff at a constant, with the SMALLER graph the slower one.
    **FIXED (rmp #2431, `84ab4123`; rmp #2367, `db02f8c3`).** For #2431 the root cause
    was that the columnar chain's yield was INERT: `tryBuildColumnarFilterChain`
    already declines when `pickMinLabel` would fire and states the rule — the re-anchor
    reduces the rows SCANNED, while columnar execution only removes a constant factor
    from each scanned row — and `tryBuildParallelScanProject` was tried IMMEDIATELY
    AFTER without making the same argument, so it claimed every shape the yield gave
    up, anchored on `Labels[0]`. WHICH LABEL THE GATE JUDGED WAS PROVED, NOT READ:
    holding `|Rare|` at 1 000 and sweeping `|Common|`, the plan flips at exactly
    `|Common| > 50 000` — the parallel threshold — while the re-anchored label never
    moves; the bitmap intersection is confirmed NOT implicated, since disabling it
    changed neither plan nor time. The fix ADOPTS the anchor rather than merely
    declining, which makes the case where the smallest label is itself above the
    threshold FASTER: **−96.08 % sec/op, −99.28 % B/op, −98.54 % allocs/op
    (106 293 → 1 551), p=0.008** over 5 interleaved pairs, with **no shape regressing**.
    The regression gate pins the precedence STRUCTURALLY, comparing plans and never
    wall-clock. **One control was invalid and the replacement is the point:** the
    obvious non-vacuity control — disable the re-anchor, check the fixture still plans a
    parallel scan — FAILS, because disabling the re-anchor also unblocks the columnar
    chain, which then claims the shape; the gate therefore drives `RETURN n.k + 1`,
    which the columnar chain declines at every setting, leaving the parallel tier as the
    only operator deciding.
    For #2367 the floor moves 1024 → 64, and **the mechanism was established rather
    than inferred**: an earlier version of the ticket asserted a mis-calibrated
    cost-based crossover and that claim was WITHDRAWN when no such threshold was found,
    and four further explanations for the surrounding anomaly were each refuted by
    measurement. The key KIND decides which index path is reachable at all — a Cypher
    `CREATE INDEX` builds a STRING-keyed hash index, so an INTEGER key cannot reach the
    hash path and falls to the range path, which is the population-gated one, which is
    why a string-keyed fixture cannot see this. A floor is KEPT rather than removed
    because below it no count can change the verdict, so `rangeSeekBudget` refuses to
    take one. Allocations say the same and cannot be moved by machine load: 889 per
    lookup at 1023 nodes against 127 at 4096, flat at ~123 after. PRIOR ART, READ FROM
    SOURCE: neither reference has an analogue — PostgreSQL's
    `cost_seqscan`/`cost_index` carry no minimum table size, and Neo4j's
    `NodeIndexLeafPlanner.findIndexMatches` yields an index match for every compatible
    predicate — both delegate to a cost model, and GoGraph has none, so this constant IS
    the decision and it is now set from measurement. **Not fixed, and out of scope:** an
    integer-keyed property still cannot use the hash path.
27. **`count(*)` built one row per input row to carry a CONSTANT** (performance; no data
    loss). A group-by-less, non-`DISTINCT` `count(*)` has a constant aggregate argument
    — `aggArgItem` emits `expr.BoolValue(true)` for an empty argument, so `CountAgg`
    ticks on every row and can never reject one — yet the serial pipeline still built a
    pre-projection materialising one fresh single-column row per input row, **seven
    million rows** on the count this was filed against, so a counter could null-check a
    value that is never null.
    **FIXED (rmp #2625, `91daab81`).** `exec.CountRows` counts the child's rows instead,
    tried AFTER every existing count pushdown, because those answer in `O(1)` from a
    maintained counter while this still visits every row: it only stops BUILDING rows
    nobody reads. `count(v)` is excluded deliberately — it counts non-null bindings, so
    its argument must still be evaluated per row. On a bare typed expansion the
    pre-projection was 4.06 ms of an 11.30 ms query and removing it cut the query
    **4.52 ms → 2.71 ms (~40 %)**; once an endpoint label is added the per-row `Filter`
    checking the far endpoint costs ~18.2 ms of 26.1 ms and this operator's own cost is
    ~2.6 ms. At 40 000 users over an interleaved five-run A/B the median gain is modest
    and the VARIANCE result is large — `count_friend` 8.040 s → 7.504 s with spread
    **52 % → 4 %**, `count_like` 6.331 s → 6.156 s with spread **59 % → 9 %** — because
    removing seven million per-row allocations takes the GC out of the tail. One early
    pairing showed 35 %; five rounds identify it as an outlier in the BEFORE arm and
    **it is not claimed**. **THE FIRST IMPLEMENTATION SHIPPED A WRONG ANSWER**, recorded
    because the mechanism generalises: it installed the post-aggregation schema through
    the UNGUARDED installer the leaf pushdowns use — safe there only because nothing is
    built below a leaf — and over an arbitrary child whose `Selection` operators hold
    closures over `bopts.scalarCols`, tagging the output column scalar when its alias
    SHADOWS a pattern variable made those `Selection`s read the bound node's column as a
    scalar and drop every row:
    `MATCH (n {name:'A'})-[:LIKES]->(m {name:'B'}) RETURN count(*) AS n` returned **0**
    while the same query aliased `AS c` returned 1. Three existing
    `TestEdgeTypeFilterCache_*` tests caught it, and `installAggOutputSchema` — which
    carries the alias-shadow guard — is what it uses now. `cypher/exec`'s own
    `PlanChildren` completeness gate caught a second omission. **The count-store fast
    path the task prescribed is REFUTED and not built:** `count.Store.CountE` takes no
    snapshot and a read transaction pins its view — a scan answered 2000 before and
    after 100 edges committed outside it, while a fresh query answered 2100 — so
    answering from the store would violate snapshot isolation.
28. **Closing a write window never released its shard builders, so `Compact` DOUBLED
    resident memory** (resource retention; no data loss). The contract on
    `adjShard.building` says "the window end freezes it by clearing this field". Nothing
    did: neither `EndExclusiveBuild` nor `EndCommit` touched it, so the field was
    released only lazily by `storeEntry`, when a write presenting a DIFFERENT owner
    happened to touch the same shard — a shard no later transaction touched pinned its
    builder for the lifetime of the graph. The cost is not the builder, it is what the
    builder BLOCKS: it keeps the shard's pre-window slot array alive after `slotsRef`
    has moved on, so anything replacing `slotsRef` leaves both arrays live. `Compact`
    does exactly that, which **inverted its own purpose**. MEASURED at 3 M edges, build
    then `Compact`, live heap after `runtime.GC()`:

    | Arm | Live heap | Allocated |
    |---|---:|---:|
    | no window | 159.9 MiB | 3.25 GiB |
    | exclusive-build window, before | **361.9 MiB** | 1.79 GiB |
    | exclusive-build window, after | **159.9 MiB** | 1.79 GiB |

    Bracketing a bulk build — the documented way to make it cheaper — roughly DOUBLED
    resident adjacency once the graph was compacted. It is now free.
    **FIXED (rmp #2628, `6b57bf00`).** Released where it costs nothing:
    `releaseBuilders` on the outermost `EndExclusiveBuild`, and in `compactShard`, which
    already holds the shard mutex. `EndCommit` is deliberately unchanged — that is the
    per-transaction serving path, and an `O(shards)` walk per commit would be a real
    regression; the comment there is right that correctness does not need it. Clearing
    loses nothing, because `storeEntry` publishes the builder into `slotsRef` on the
    shard's first touch, so every write the window made is already reachable through the
    published pointer. **A second, sharper hypothesis was REFUTED and is not claimed:** a
    write made after `Compact` inside the same open window is NOT lost — a test asserting
    the edge survives passes on the unfixed code. Both new retention tests fail before
    the change with **256 of 256 shards retaining a builder**. Found while bracketing
    `examples/26_social_scale_bench`'s build (rmp #2624), which without this fix would
    have traded 3.4× allocation for 2× residency.

### Harness and gate defects surfaced by this coverage work

These are defects in the test harness and the local gate, not in the engine.
They are listed apart from the numbered findings above because a green engine and
a green gate are different claims, and conflating them is how a gate defect gets
reported as an engine defect. Two earlier members of the same family are
documented where their subject is — the Bolt swarm density clause (rmp #2587)
and the `nv-swarm-overlap` straddle count (rmp #2596), both in
[Aggregate inbound-decode backpressure](#aggregate-inbound-decode-backpressure-and-two-nesting-caps-that-are-not-one-rmp-2487)
— and H5 below is the third time that same clause has had to be revisited.

- **H1. `make ci` had no explicit `-timeout`, and a package sat 10.5 s from Go's
  600 s default** (rmp #2584). One `make ci` run recorded `cypher 589.466s` — a
  margin of 1.8% — while a concurrent second run on the same tree and commit
  recorded `panic: test timed out after 10m0s` for the same package. Same code,
  same commit; the only variable was available CPU, so a loaded machine was being
  reported as a failed subject. `SHORT_TIMEOUT ?= 30m` is now passed by
  `test-short`, `test-short-timings` and `make race`, chosen as 3.05× the slowest
  completing package and 1.5× the `-timeout=20m` that `scripts/cover_gate.sh`
  already applied — which is how it emerged that the `-race` pass was the only
  whole-suite run in `make ci` with no explicit limit. Validated by outcome:
  `internal/sim` now completes in-suite at 602.866 s, which the 600 s default had
  killed twice.
- **H2. The hypothesis that capping package parallelism would make the gate
  faster AND more reliable was MEASURED AND REFUTED; no change was made**
  (rmp #2590). A six-run sweep on the 10-core host (uncapped ×2, `-p` 6/4/3/2)
  showed wall-clock is monotonic in the cap — 761 s uncapped rising to 1171 s at
  `-p 2`, +54% — so the cores idle instead of working. Per-package inflation does
  fall with the cap (`cypher` 417.2 s → 272.8 s), which confirms the contention
  diagnosis, but the wall-clock penalty outweighs it and the reliability gain
  measured NONE: all six runs green, zero failed tests, including both uncapped
  runs. Recorded here because a refuted hypothesis is evidence too: the gate's
  parallelism is not the cause of its timing-gate failures, and #2584's explicit
  timeout is what was retained.
- **H3. An alloc sensitivity meta-test demanded EXACT additivity, so 0.0076%
  run-to-run variance failed it on correct code** (rmp #2591). The mutation-alloc
  oracle under test was fine and its 64× bound is untouched — it correctly
  condemned the injected run at 101.5× — but the meta-assertion required
  `inflated >= control + injected` with zero tolerance, and netting out the
  injection the same workload measured 14,804,360 B against the control's
  14,805,480 B. The tolerance is now derived from a measured DISTRIBUTION rather
  than from the single failing figure: 40 sampled pairs put the non-injected
  portion between 31,728 B below the control and 8,336 B above, so the gate's
  1,120 B shortfall understated the real noise by ~28× and anchoring to it would
  have shipped a window still too tight. 1% of the injected 16 MiB is 167,772 B —
  5.3× above the worst measured shortfall and 6.25× below the smallest real miss
  — and it is expressed against the INJECTED amount so it does not scale with
  unrelated allocation growth. The family was then surveyed with the
  discriminator that a threshold is safe when it bounds what the HARNESS
  determines and fragile when it bounds what the RUNTIME decides; ~15 candidates
  were deliberately safe with the reasoning already in code, and two were filed
  rather than guessed at (rmp #2592, #2588).
- **H4. The cancellation battery's precedence override was package-level mutable
  state, so two parallel tests raced and reddened the gate** (rmp #2597).
  `var ctxCancelPrecedenceOverride` was declared in the non-test file
  `search_ctx_cancel.go` and written by the falsifiability helper while the
  checker test read it — exactly what CLAUDE.md forbids. ONE race failed SIXTEEN
  tests, because Go's `testing` marks every parallel test in flight when the
  detector fires. It was introduced by rmp #2489 itself and survived that task's
  own validation because an isolated `go test -race ./internal/sim/` passed twice
  at 480 s: the window opens only under the gate's concurrent whole-tree load.
  Fixed by REMOVING the shared state — the precedence table is now a parameter,
  so the helper supplies its own — rather than by dropping `t.Parallel()` or
  adding a mutex, either of which would have preserved the forbidden variable.
  An AST guard now fails if any package-level `var` in a non-test file is written
  by a test, with nine non-vacuity fixtures; an audit of all 73 package-level
  vars found exactly one offender. Fixed in `ab048fa6`.
- **H5. The swarm density clause STILL has a scheduling-dependent floor, and it
  reddened the closing coverage run** (rmp #2611, open). Found on 2026-08-24 by
  the rmp #2498 coverage re-measurement itself: a coverage-instrumented soak run
  of the whole package exited 1 because `TestBoltDecodeSwarm_SoakSeedSweep`
  failed at seed `0x2487a018`
  (`internal/sim/bolt_decode_pressure_soak_test.go:76`). The
  `nv-swarm-pressure-density` oracle deviated with `RejectionsDuringHonest == 0`
  and per-segment `[0 0 0 0]`, with the start barrier satisfied, 3 refusals drawn
  by the fleet in total, and honest service 24 of 24 correct — **no engine
  misbehaviour**. This is the THIRD iteration of the family that already produced
  rmp #2587 and #2596: the clause adjudicates a TEMPORAL COINCIDENCE the harness
  cannot force. #2587 removed that clause's numeric floor and left only a nonzero
  floor; the residual floor is still scheduler-dependent, because whether any of
  the fleet's refusals lands between the first honest exchange starting and the
  last one finishing is decided by the machine and not by the construction —
  under load the pressure can be spent entirely before honest service begins. It
  is recorded here rather than among the engine findings for the same reason as
  H1–H4, and it is the standing counter-example to treating a green soak run as
  evidence about the engine. Open at the time of writing; the coverage figure it
  accompanies is reported with this red stated, in
  [The soak layer](#the-soak-layer-and-the-red-that-came-with-it).

## Documented debt / out of scope

- **`ds` (union-find) is not named by any DST scenario, and is exercised only
  TRANSITIVELY.** The task that closes this cycle records the package as "covered
  at the unit layer and intentionally out of DST scope"; what is actually true is
  more precise, and the distinction matters because "unit-tested" and
  "DST-covered" are different claims. VERIFIED by an `os.walk`+`re` sweep of every
  `.go` file in the tree (not by `grep`, which on the reference host can return a
  silent empty result — an empty match is not evidence of absence): exactly four
  files import `github.com/FlavioCFOliveira/GoGraph/ds`, namely `search/wcc.go`,
  `search/wcc_parallel.go`, `search/kruskal.go` and `ds/example_test.go`. Nothing
  under `internal/sim/` imports it or names either of its types, so **no scenario
  drives it directly**.
  - It IS driven transitively, on the DST's own cadence: `ds.UnionFindSlice` is
    the working structure inside `search.KruskalMST` (`search/kruskal.go:119`),
    serial `search.WCC` (`search/wcc.go:58`) and `search.WCCParallel`
    (`search/wcc_parallel.go:95-116`), and the sim drives all three —
    `internal/sim/search_mst.go:180`, `internal/sim/search_check.go:145` and
    `:157`, plus the `Ctx` twins of all of them in the cancellation battery. Both
    of those algorithm families are adjudicated against references that share no
    code with `ds`: the MST arm compares total weight and spanning-forest validity
    against `naiveKruskalTotal` over the sim's own private parent-array union-find
    (`internal/sim/search_mst.go`), and the WCC arm compares the partition up to
    relabelling against `nameGraph.naiveWCC`, whose `find`/`union` are inline
    closures (`internal/sim/search_oracle.go`). So a defect in `Find`/`Union`
    would surface as an MST-weight or WCC-partition divergence, not silently.
  - What is genuinely NOT reached at all: the generic map-backed
    `ds.UnionFind[T comparable]` has **no production caller anywhere in the
    module** — its only callers are `ds/example_test.go`'s three runnable
    examples — and `UnionFindSlice`'s `Connected`, `Len` and `Reset` are likewise
    called by no production code, since `search/` uses only `NewSlice`, `Find` and
    `Union`. Those are carried entirely by the package's own tests
    (`ds/unionfind_test.go`, `ds/unionfind_len_test.go`,
    `ds/unionfind_reset_test.go`, `ds/security_unionfind_int32_test.go` — the
    last being the regression battery for the historical int32-truncation defect
    rmp #1476), including a randomised cross-check of the slice variant against
    the map variant and a property-style check against a naive relabel-array
    model. That is the appropriate layer for them: a DST scenario cannot reach an
    API no production path calls, and adding one would be the simulator testing
    the simulator's own call.
  - Both exported types state their concurrency contract explicitly ("not safe
    for concurrent use; callers that need concurrent access must guard it
    externally"), so the CLAUDE.md rule that silence about a contract is a defect
    is satisfied without a DST arm.

- **The `inf → 0` potential substitution in min-cost flow is DEAD CODE on the
  DST's generator.** `search/flow/min_cost.go:275-279` replaces an unreachable
  node's Bellman-Ford potential with 0 before the Dijkstra phase. The min-cost
  fixtures give every node an in-path of capacity ≥ 1 through the connected
  forward spine, so every node is reachable from the source and no potential is
  ever left at `inf`. Reaching those lines needs an isolated-source fixture,
  which was deliberately left outside rmp #2497's scope: adding a disconnected
  component changes what the SPFA reference must assert about unreachable sinks,
  and that is a separate, separately-reviewed change rather than a line added to
  the generator. Open, and the only unreached branch left in the min-cost path.

- **Two clauses in the min-cost-flow arm have no falsifying witness**, and are
  kept as assertions of a construction rather than counted as coverage: the
  ctx-versus-non-ctx agreement clause and the `(flow, cost)`-versus-reference
  clause, either of which would need a real engine defect to fire. The `rc < 0`
  invariant guard is proved REACHABLE by construction — the negative-cost flavour
  puts reduced costs on its exact boundary — but has never been observed firing.
  See
  [Min-cost flow's negative-cost regime](#min-cost-flows-negative-cost-regime-the-hoisted-reverse-dijkstra-and-the-shipped-default-option-regimes-rmp-2497).

- **The search cancellation battery makes NO promptness claim, and cannot.** Both
  its regimes cancel before the call, so nothing is asserted about where inside a
  running algorithm the poll happens or how much work can still be done after
  cancellation is signalled. The obvious deterministic mechanism is unavailable:
  the in-tree counting fakes override only `Err()` over an embedded
  `context.Background()`, and four parallel entry points derive their own child
  context and poll the child — which, because `Background().Done()` is nil, is
  never linked to the parent — so the fake's `Err()` is never called at all and a
  counted mid-run arm built on it would be a clause that cannot fail. Its
  goroutine arm likewise proves NO LEAK, not prompt joining. See
  [the cancellation battery](#every-public-context-accepting-search-entry-point-under-cancellation-rmp-2489)
  for the full list of unreached regimes and the measurements behind each.

- **`bulk.Loader`'s goroutine fan-out is unreachable from outside its own
  package, so the byte-identity claim covers `buildCSRDirect` and not
  `buildParallel`.** `Finalise` matches `Parallel && csrDirectEligible()` first,
  and no public `bulk.Options` can falsify `csrDirectEligible()`, so a directed
  parallel load never reaches the fan-out. The DST certifies the code a caller
  setting `Parallel: true` actually runs; multi-goroutine build determinism is
  covered only by `store/bulk`'s own in-package tests, which can inject a shard
  capacity. `TestBulkLoadOracle_ParallelFanOutStillUnreachable` fails if a
  shard-capacity knob is ever added to `bulk.Options`, so this bullet cannot go
  stale silently. Likewise, `Finalise` → `csrfile.WriteToFile` binds the OS
  backend at its entry point, so that ONE call edge cannot be faulted; the fault
  arms drive the identical writer core through `WriteToFileWith` over a
  `SimDisk`, and an unwritable `OutputPath` is the closest reachable substitute
  for the edge itself.

- **`graph/generation` use-after-FREE is unreachable, and concurrent publishers
  are out of scope.** Go's garbage collector keeps a `*csr.CSR` alive for as long
  as any reader holds the pointer, so there is no freed memory to touch and no
  observable fault to catch; the `generation-swap` scenario claims
  use-after-RECYCLE (the modelled reclamation decision) and says so. Concurrent
  publishers are excluded deliberately: the readers' monotonicity clause is sound
  only under a single publisher, because the plan allocates sequence numbers
  before the swap rather than under the library's own `publishMu`. The package's
  own `TestPublisher_ConcurrentPublishWithDrain_NoLostDrain` covers that case
  through an unexported seam no scenario can see.

- **The numeric range residual filter runs unconditionally, and need not.** rmp
  #2600 makes `query.valueInRange` a residual filter over every numeric range
  seek, because the float64 companion's keys round above 2^53. MEASURED cost:
  11.1× selective and 18.7× broad against a seek that skips it (see finding 11).
  It is provably unnecessary whenever BOTH bounds lie strictly inside
  (−2^53, 2^53): every value that can satisfy such a predicate is itself inside
  that interval, where `int64 → float64` is lossless, so the companion's key
  equals the exact value and the seek is EXACT. The bound must be strict — with
  `hi = 2^53` a node valued 2^53+1 keys to 2^53 and would be admitted — and any
  implementation of it needs that exact case as its falsifier. Not done under
  #2600: the fix that closed the divergence was deliberately kept to the simplest
  provably-correct shape, and making a correctness-critical filter conditional is
  a separate, separately-reviewed decision.

- ~~**GraphML round-trip under fault** is not yet covered.~~ **Closed** (verified
  rmp #2471): `internal/sim/storage_fault_scenarios.go` carries the
  property-graph fixture this bullet said was missing (`graphmlModel`, labelled
  and propertied) and drives it through both halves of the ST8 contract —
  `graphmlRoundTripClean` (exact round-trip via `graphml.WriteWithProps` /
  `ReadWithProps`) and `graphmlExportFaultFailsClean` (a clean typed failure
  under a sub-full ENOSPC bound, with no silently-accepted partial). CSV, JSONL
  and GraphML are all covered. **Extended** (rmp #2480): DOT was still
  uncovered when that was written — `graph/io/dot` was imported nowhere in the
  simulator — and is now adjudicated by cross-format agreement, alongside the
  JSONL property path, the whole `csv.Options` space, every defensive cap and
  mid-parse cancellation on every `*Ctx` reader. See
  [graph/io completeness](#graphio-completeness-dot-the-property-path-the-caps-and-cancellation-rmp-2480)
  above.
- **Snapshot isolation for read transactions** shipped in rmp #2307 (sprint
  334) via MVCC version chains rather than the copy-on-write epic (#1671) once
  considered for it. The DST scenarios assert no-dirty-read, which the stronger
  contract still satisfies; the isolation level itself is gated by
  `cypher/readtx_snapshot_test.go` and `bolt/server/e2e_readtx_snapshot_test.go`.
- ~~**Multi-member WAL group-commit coalescing / fail-all** is engine-unreachable
  (serialised under `visMu`).~~ **Closed** (rmp #2471): the premise was false —
  an ordinary write has taken the barrier SHARED since sprint 334, and multi-member
  coalescing is measured at 61 followers in 483 rounds through the engine. Both
  coalescing and fail-all are now gated in the DST; see
  [Group-commit coalescing and fail-all](#group-commit-coalescing-and-fail-all-rmp-2471)
  above.
- **`wal.ErrWALLocked` and the WAL `O_NOFOLLOW` refusal are STRUCTURALLY
  unreachable through `SimDisk`** — it is a flat in-memory key table with no
  inodes, links or advisory locks — so they are **not** covered by any seeded,
  crash-injecting scenario, and will not be unless `SimDisk` grows a
  lock-and-symlink model. Since rmp #2472 both are driven against a real
  temporary directory by `RunWALRealFSGuards`, alongside the store-layer unit
  tests (`store/wal/lock_test.go`, `store/wal/symlink_escape_test.go`); that arm
  is their only representation in the simulator, it leaves the simulated disk to
  do it, and it makes no claim on a platform that cannot express the guards. See
  [The WAL writer surface](#the-wal-writer-surface-watermark-frame-contiguity-truncate-guards-rmp-2472)
  above for why a real directory was chosen over a model.
