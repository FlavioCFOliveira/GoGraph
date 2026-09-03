# Contention inventory — round 2, 2026-09-02

Ranked, measured inventory of every contention site found across the GoGraph
module surface, re-baselined after round 1's six fixes landed. Produced by rmp
task #2690 in sprint 353, "GoGraph Optimization Laboratory".

Round 1's inventory (`docs/contention-inventory-2026-09-01.md`) is superseded by
this one, for the reason that motivated this task: **contention relocates when
it is removed.** Every fix promoted a different site to the top, so a ranking
survives only until the next fix lands.

## How this was measured

| | |
|---|---|
| Instrument | `bench/contention`, extended by this task with five workloads and thirteen ceiling arms |
| HEAD | `69b496b6`, branch `feature/353-gograph-optimization-laboratory` |
| Module code measured | byte-identical to `69b496b6`; the working tree differs only inside `bench/contention`, additively |
| Host | Apple M4, **10 cores** (4 performance + 6 efficiency), 32 GiB, `go1.27.0 darwin/arm64` |
| Sweep | 25 workloads x 5 levels x 2 windows = **250 fresh child processes**, 1026 s wall, exit 0, 125 rows |
| Sweep host load | loadavg **1.63** before, **10.48** after; nothing else ran on the machine |
| Ceiling probe | 14 pairs x 3 levels x 5 interleaved rounds = **420 windows**, 2796 s wall, exit 0 |
| Ceiling host load | loadavg **2.18** before, **8.10** after |
| Ranking | `go tool pprof -top -cum -lines`, cumulative and line-granular, restricted to GoGraph frames |

Every measurement runs **two windows in two separate processes**: an unprofiled
window supplying throughput and latency, and a profiled window supplying lock
attribution. The profiles and the heap are both cumulative per process with no
reset API, so sharing a process lets the first window contaminate the second.

### Freshness audit — which numbers are proved to come from a real run (rmp #2713)

`go test` caches a passing package result and, on a later invocation with the
same binary, the same cacheable flags and the same environment variables, prints
that stored result instead of running anything. A replay reprints the previous
run's throughput, its latency and even its reported test duration; the only tell
is `(cached)` in place of the elapsed time on the `ok` line. This document was
written before that hazard was known, so nothing warned about it at the time and
every number here had to be re-checked against surviving evidence.

**The hazard is now closed at the source.** `bench/contention` refuses to
measure whenever cmd/go has signalled that it intends to store the result, and
Go stores only *passing* results — so a run that publishes numbers is never
cacheable and a cached measurement can no longer exist. See
`bench/contention/cachegate_test.go`.

**What was audited.** Every surviving file on this host carrying a
`bench/contention` package-result line: **312 files, 327 result lines**. Exactly
**one** historical campaign `(cached)` result exists and it belongs to a
different document — the Bolt evaluation's noise-floor retake. None of this
document's campaigns is among them.

| Published numbers | Verdict | Evidence |
|---|---|---|
| The scaling table (25 workloads x 5 levels, and `ops/s at 1`); the round-2 column of "Round 1 verified" | **Verified fresh** | Driven by a script that passes `-count=1`. Its log ends `ok … 1025.498s`, not `(cached)`, and the wrapper's own clock brackets the run 16:40:48 -> 16:57:54 UTC = **1026 s**, matching the reported 1025.26 s; a replay would have shown a bracket of about a second against the same reported duration. Its 966 artefact files span 1024.5 s. `summary.tsv` reproduces every cell checked — 8 workloads x 5 scaling cells plus 8 level-1 throughputs, **0 mismatches**. |
| The ranked inventory's ceilings and spreads; the level-1 handicap table; the `cypher-write-mem` validity control | **Verified fresh** | Same construction: `-count=1`, log ends `ok … 2795.218s`, wrapper bracket 17:04:05 -> 17:50:41 UTC = **2796 s** against a reported 2794.85 s, artefacts spanning 2794.4 s. `ceiling.tsv` reproduces all 15 ranked ratios checked, all 6 handicap ratios and all 3 validity-control rows, spreads included. |
| Every blocked-time and share figure; the CPU-profile shares; the rmp #2697 `execUnderBarrier` correction table | **Verified fresh** | Re-derived from the surviving profiles rather than from any console line: `generation-publish-read@1024` yields **2337.55 s** and `index-hash-rw@1024` yields **273.74 s**, both exactly as published, and the profiles' own embedded capture times (17:56:41 and 17:43:30 WEST) fall inside the logged sweep window. |
| The noise floor: eighteen A-vs-A ratios, the +/-2.4% rule, the 38.88% and 19.23% spreads | **Verified fresh** | Two probe logs ending `ok … 206.583s` and `ok … 197.816s`, each bracketed by loadavg lines whose own clock (17:14 -> 17:18 and 17:21 -> 17:25) matches the reported duration. Their `ceiling.tsv` files carry the published `0.3888` and `0.1923` spreads. Unrelated to caching: the first probe's directory also holds 12 files from an earlier partial run at 17:07–17:08; the published `ceiling.tsv` is written wholesale from the final run's rows, so those leftovers did not enter the figures. |
| The `dst-mvcc-sessions` row and its `loadavg 1.74 before / 2.60 after` | **Verified fresh** | Driven with `-count=1`; the log records `loadavg_before: { 1,74 … }` and `loadavg_after: { 2,60 … }` — the published pair — and ends `ok … 12.968s`. |
| The fault-rate probe table (5000 / 559 / 4441 / 561); the error-class probe (9 / 30720 staggered, **2163 / 30720** with the barrier, 1934 + 229 classes); the seed 29 / 102 / 215 replays; the `Manager` cross-reference counts (113 and 88); the **649% / 669% / 506%** core-utilisation figures | **Cannot be verified** | No surviving log reproduces any of them. The only files on this host containing these figures are this document and the working notes written from it. The 312-file scan bounds the risk — no cached campaign result exists anywhere in this document's lineage — but that is a statement about the population, not evidence about these particular numbers. |

Nothing in this table is a claim that an unverified number is wrong. It is a
claim that the evidence needed to prove it fresh no longer exists, which is a
different and weaker statement — and the honest one. Replacing the unverified
figures is not part of rmp #2713.

### The noise floor is not one number

Measured first and twice, A-vs-A — the same code on both sides, driven by exactly
the machinery that later produced every ratio here, arms interleaved and the
order alternating between rounds. Eighteen ratios that should all read 1.000:
sixteen landed inside **+/-2.4%**. The two that did not were both
`index-manager-fanout`@1024, whose own five-round spread is 38.88% and 19.23%
against 0.4-5.6% everywhere else.

**Working rule applied to every ratio in this document: +/-2.4% is the floor
where an arm's own spread is under ~6%, and no ratio is called a result unless it
clears that arm's own measured spread.** Every ceiling number below carries its
arm spreads beside it.

### Reading the numbers honestly

- **The host has 10 cores.** Only the 1 -> 8 region is a scaling signal. At 64 and
  above the question is not "does it go faster" but "does it degrade gracefully".
  Oversubscription is not reported here as a contention defect.
- **Anti-scaling at 8 is damning.** With spare cores available, a curve below
  1.000x means the sharing costs more than the work it protects.
- **A share is not a prize.** Round 1 measured a site holding 98.67% of all mutex
  delay with about 4% of achievable throughput behind it. This round therefore
  refuses to rank a site without a ceiling number, and the refusal earned its
  keep twice over — see [Big share, small ceiling](#big-share-small-ceiling).
- `scaling_vs_1` is throughput at level *N* over throughput at level 1, both from
  the unprofiled window.
- **A ceiling is read against its own level-1 cell, never on its own.** A ceiling
  arm must read 1.000x where there is nothing yet to unshare; whatever it reads
  instead is its construction bias, and the bias runs in BOTH directions across
  the thirteen arms here. A ceiling quoted without that cell has no established
  direction — see [the corrected section](#the-ceiling-arms-bias-runs-both-ways--corrected-rmp-2712).

## Round 1 verified: every fix holds

`scaling_vs_1` at 8 / 1024, round 1 measured at `3733f514` against round 2 at
`69b496b6`. **Bold** marks anti-scaling.

| workload | round 1 | round 2 | fix |
|---|---|---|---|
| `cypher-mixed-rw` | 1.602 / **0.096** | 1.789 / 1.334 | #2686 |
| `index-count-spread` | **0.917** / **0.538** | 2.470 / 2.713 | #2682 |
| `index-hash-rw` | 1.329 / **0.916** | 1.898 / 1.742 | #2692 |
| `index-btree-rw` | not separately reported | 5.750 / 5.651 | #2683 |
| `cypher-read-label-small` | 1.467 / 1.477 | 1.555 / 1.528 | #2691, #2693 |

**No workload that round 1 addressed anti-scales any more.** The five worst
curves in this round are all surfaces round 1 never reached.

## Scaling table

`scaling_vs_1`, higher is better. **Bold** marks anti-scaling.

| workload | 1 | 8 | 64 | 256 | 1024 | ops/s at 1 |
|---|---|---|---|---|---|---:|
| `search-bfs-csr` | 1.000 | 5.819 | 5.781 | 6.564 | 7.212 | 195,664 |
| `index-btree-rw` | 1.000 | 5.750 | 6.180 | 5.915 | 5.651 | 23,795,433 |
| `search-sssp-shared` | 1.000 | 5.403 | 6.976 | 7.165 | 7.383 | 2,403 |
| `cypher-write-wal` | 1.000 | 3.986 | 29.802 | 97.034 | 207.813 | 258 |
| `store-checkpoint-write` | 1.000 | 3.706 | 21.698 | 63.581 | 93.313 | 195 |
| `cypher-read-scan-large` | 1.000 | 3.168 | 3.452 | 3.454 | 3.412 | 240 |
| `cypher-read-project` | 1.000 | 3.075 | 3.680 | 3.643 | 3.591 | 4,099 |
| `lpg-neighbours-read` | 1.000 | 3.004 | 3.379 | 3.392 | 2.748 | 5,886,798 |
| `index-count-spread` | 1.000 | 2.470 | 2.633 | 2.652 | 2.713 | 118,285,913 |
| `mvcc-explicit-tx` | 1.000 | 2.242 | 2.365 | 1.875 | 1.266 | 123,072 |
| `centrality-pagerank` | 1.000 | 2.217 | 3.683 | 3.863 | 3.544 | 10,428 |
| ~~`dst-concurrent-bolt`~~ † | ~~1.000~~ | ~~2.206~~ | ~~0.470~~ | ~~0.274~~ | ~~0.127~~ | ~~595~~ |
| `mvcc-session-write` | 1.000 | 2.182 | 2.203 | 1.869 | 1.385 | 370,156 |
| `dst-disk-wal` | 1.000 | 2.174 | 10.423 | 28.116 | 44.583 | 1,168 |
| `cypher-write-mem` | 1.000 | 2.104 | 2.080 | 1.744 | 1.388 | 363,503 |
| `index-hash-rw` | 1.000 | 1.898 | 1.795 | 1.747 | 1.742 | 54,082,029 |
| `cypher-mixed-rw` | 1.000 | 1.789 | 1.740 | 1.494 | 1.334 | 492,737 |
| `bolt-connect-churn` | 1.000 | 1.566 | 2.078 | 2.523 | 2.733 | 27,382 |
| `cypher-read-label-small` | 1.000 | 1.555 | 1.619 | 1.576 | 1.528 | 1,012,488 |
| `bolt-wire-read` | 1.000 | 1.484 | 1.606 | 1.705 | 1.711 | 75,572 |
| `dst-disk-fault-wal` | 1.000 | **0.989** | **0.862** | **0.768** | **0.585** | 177,585 |
| `index-manager-fanout` | 1.000 | **0.773** | **0.650** | **0.505** | **0.089** | 7,381,596 |
| `metrics-emit` | 1.000 | **0.445** | **0.446** | **0.440** | **0.465** | 44,385,276 |
| `index-count-hot` | 1.000 | **0.328** | **0.331** | **0.331** | **0.330** | 242,816,796 |
| `generation-publish-read` | 1.000 | **0.094** | **0.084** | **0.084** | **0.084** | 141,137,201 |
| `dst-mvcc-sessions` * | 1.000 | 2.169 | 2.839 | 2.861 | 2.814 | 127 |

\* `dst-mvcc-sessions` was added after the main sweep and swept on its own
(5 levels x 2 windows, exit 0, loadavg 1.74 before / 2.60 after). It is not
comparable to the rows above as a contention ranking and is not ranked below —
see [dst-mvcc-sessions shares nothing, and that is the point](#dst-mvcc-sessions-shares-nothing-and-that-is-the-point).

### † dst-concurrent-bolt is SUPERSEDED: this row measured the HARNESS (rmp #2728)

Every `RunConcurrent` call runs `probeWireParamTypes` before it spawns a
connection, and that probe's fixture used a **fixed label and a fixed id
against the one shared `SimServer`** all `level` workers drove. With no
uniqueness constraint, many probe nodes matched the same
`MATCH (n:WireParam ...)` at once, so each probe's `SET n.s = $nul` and
`DETACH DELETE n` **fanned out over every other probe's node**. The row's
scaling column is that fan-out, not engine write scaling.

**Reproduced at HEAD by counting the engine calls, not by inference.** A
temporary counter on `graph/lpg.delNodePropertyInfo`, driving the arm's own
operation at 1/2/4/8/16 workers x 8 operations each:

| workers | per operation, private fixture | per operation, shared fixture |
|---:|---:|---:|
| 1 | 6.0 | 6.0 |
| 2 | 6.0 | 6.1 |
| 4 | 6.0 | 7.2 |
| 8 | 6.0 | 8.8 |
| 16 | 6.0 | 14.1 |

**6.0 is the whole cost of one probe** — one property removed by the null `SET`
plus the five that survive to the `DETACH DELETE` — and with a private fixture
it is **flat at every level**, which is what a fixture that does not share must
be. The shared fixture instead grows with the level, and it compounds with the
number of operations in flight as well: the original attribution measured
**1022.5 calls per operation at level 64** over a 1984-operation window, of
which **97.9% removed nothing**.

**The wasted work was not the worst of it.** The probe's own `count(*) == 1` and
`count(*) == 0` assertions are false whenever a neighbour's node is live, so the
arm's parameter-matrix oracle was **corrupted from level 2 upward** — 2,003
spurious divergences at level 2, 7,835 at level 8 — and sixteen concurrent
probes leaked **64 nodes** while filling the log with
`mvcc: serialization conflict`, because their cleanup transactions conflicted
over the shared node set and aborted. So the caption's "levels 1 and 8 are
unaffected" was true of the *work* only.

**Fixed and gated at both levels.** Each probe now holds a private fixture slot
(`internal/sim.wireParamSlotPool`), and slots are recycled rather than minted
per probe so the engine's label cardinality stays bounded by peak concurrency.
Three gates hold it: `internal/sim.TestWireParamTypes_ConcurrentProbesDoNotCrossTalk`,
`TestWireParamFixture_IdentifiersCarryTheSlot`, and — at the arm itself —
`bench/contention.TestDstConcurrentBoltSharesOneServerCleanly`, which drives the
workload the way the sweep drives it. All three are mutation-proven: restoring
the fixed label and id fails every one of them, while the pre-existing
single-goroutine smoke test `TestRound2WorkloadsDrive` still **passes** — which
is precisely the blindness the arm-level gate closes. `dstConcurrentOp` now
also asserts the parameter matrix, an oracle it had to discard while the
fixture was shared.

**No replacement figure is published here.** The cell must be re-swept on HEAD
in a quiet window (rmp #2730) before any number is put back in the table.

**And judge it against the right floor when that happens.** The A-vs-A floor for
this cell is **bimodal**: 43 of 46 interleaved runs sit within ±4% of the median
while three fall 24-32% below it. So a **single run carries ±32%** — which is
what #2710 measured and reported as ±32.8% — while a **median of ten interleaved
runs carries ±4%**. The ±11.5% previously published describes neither regime.

## Ranked inventory

Ranked by **ceiling**, not by share: the number that says how much throughput
partitioning could actually buy. Blocked time is cumulative mutex delay over all
goroutines, so the absolute figures at 1024 are large by construction; the share
is what locates the site inside its own profile.

| # | site | blocked / share | provoked by | ceiling @8 (raw) | ceiling @1024 (raw) | on an engine path? |
|---:|---|---|---|---|---|---|
| 1 | `graph/generation/generation.go:152` `releaseRef`, `:162` `Release`, `:185` `Publish` | 64.34 ms @8; 2337.55 s @1024 (`Publish` 82.89%, `releaseRef` 17.11%) | `generation-publish-read`@8 | **47.619x** (2.42% / 7.17%) | 23.878x (0.37% / 13.08%) | **no — and deliberately so** |
| 2 | `graph/index/manager.go:254` `Manager.Apply` -> `graph/index/label/index.go:327` `Index.Add` | 11484 s = **43.88%** of 26172 s @1024; `CreateIndex` 28.34%, `DropIndex` 27.65% | `index-manager-fanout`@1024 | 3.396x (20.65% / 3.40%) | **32.223x** (48.32% / 5.14%) | yes — see the correction below |
| 3 | ~~`bolt/server/serve.go:802` -> `graph/lpg/lpg.go:1388` `applyVersionedInstant`~~ **CORRECTED — see below** | 9792 s = **98.55%** of 9936 s @1024 | `dst-concurrent-bolt`@1024 | 1.627x (1.43% / 2.15%) | **16.307x** (49.41% / 34.78%) | yes |
| 4 | `internal/metrics/metrics.go:105` `IncCounter` | 5.23 ms @1024; **82.01% of CPU** @8 | `metrics-emit`@8 | 3.300x (1.90% / 13.48%) | 2.138x (2.15% / 28.86%) | only with a real backend installed |
| 5 | `store/wal` durable commit, reached via `cypher/api.go:18379` `execUnderBarrier` | 1425.59 s = 99.03% of 1439.51 s @1024 | `dst-disk-wal`@1024 | 3.860x (0.28% / 0.79%) | 1.353x (1.91% / 2.07%) | yes |
| 6 | `graph/index/count/count.go:346` `Store.Apply` | 4.90 ms @1024; `atomic.Int32.Add` **44.85% of CPU** @8 | `index-count-hot`@8 | 2.916x (0.45% / 26.25%) | 2.392x (0.16% / 24.10%) | yes |
| 7 | `graph/index/hash/index.go:1191` `Index.Insert` | 268.01 s = **97.91%** of 273.74 s @1024 | `index-hash-rw`@1024 | 1.960x (1.38% / 9.57%) | 1.531x (4.95% / 9.08%) | yes |
| 8 | `cypher/api.go:18379` `execUnderBarrier` (in-memory write barrier) | 189.16 s = 88.13% of 214.63 s @1024 | `cypher-write-mem`@1024 | 1.262x (2.44% / 2.17%) | 1.702x (2.63% / 9.24%) | yes |
| 9 | `cypher/api.go:18379` via `cypher/session.go:109` `Session.RunInTx` | 134.41 s = 72.13% of 186.33 s @1024 | `mvcc-session-write`@1024 | 1.308x (1.79% / 1.88%) | 1.712x (2.04% / 5.96%) | yes |
| 10 | `cypher/api.go:4933` `parseAndAnalyse` | 121.10 s = 53.29% of 227.26 s @1024 | `cypher-read-label-small`@1024 | 1.152x (2.47% / 3.26%) | 1.116x (3.42% / 1.82%) | yes |
| 11 | `cypher/plan_cache.go:85` `planCache.get` | 144.56 s = 61.48% of 235.13 s @1024 | `cypher-mixed-rw`@1024 | 1.111x (1.87% / 5.53%) | 1.272x (8.44% / 5.79%) | yes |
| 12 | `graph/index/btree` (round 1's #2683 target) | — | `index-btree-rw` | 0.948x (2.38% / 5.53%) | 1.005x (7.47% / 6.45%) | yes — **exhausted** |
| 13 | `graph/lpg` / `graph/adjlist` neighbour read | — | `lpg-neighbours-read` | 1.004x (3.97% / 2.25%) | 1.001x (3.08% / 3.04%) | yes — **nothing there** |

Spreads in parentheses are (base arm, ceiling arm) five-round min/max as a
fraction of the median. Every ratio called a result above clears both.

**The two ceiling columns above are RAW.** They are not normalised by each arm's
own level-1 cell, and three of the thirteen arms are *faster* than their base
before anything has been unshared — so not every ceiling here is a lower bound.
The corrected figures, and the direction of each arm's bias, are in the next
section; where a raw and a normalised figure disagree, the normalised one is the
better estimate.

### The ceiling arms' bias runs BOTH ways — corrected (rmp #2712)

A ceiling arm does not delete a lock — module code is not the harness's to
change, and a deleted lock measures a program that does not exist. It **removes
the sharing**: it builds `GOMAXPROCS` independent copies of the fixture the base
workload shares and routes each worker to one. The path through the module is
byte-identical; only the number of goroutines meeting on one object changes.

That construction carries a bias, and the level-1 cell measures it: at level 1
there is one worker meeting one replica, so nothing has been unshared and the
pair must read 1.000x.

**What this section published before, and why it was wrong.** It listed six arms,
every one of them below 1.00, and concluded: *"Every ceiling above 1.00x is
therefore a lower bound."* The table it drew that conclusion from had been
filtered to the arms that sat below 1.00 — **seven of the thirteen arms were
omitted entirely** — and three of those seven sit **above** 1.00 by more than the
tolerance, with two more above it inside the tolerance. For an arm above 1.00 the
arm is faster than its base before anything has been unshared, so its ceiling is
an **upper** bound to be discounted, not a lower bound to be trusted.
The worst case is the headline of ranked row 3: `dst-concurrent-bolt`'s
**16.307x** was published un-normalised against an arm that reads **1.078x** at
level 1, and the corrected figure is **15.127x**.

#### Every arm's level-1 cell, and the direction it implies

Direction is decided against the same working rule the rest of this document
uses: the departure from 1.000 must clear the wider of the ±2.4% floor and the
anchor row's own two arm spreads.

| arm | cell @1 | base spread | arm spread | tolerance | direction of the bias |
|---|---:|---:|---:|---:|---|
| `metrics-emit` | 0.627x | 0.89% | 1.25% | ±2.40% | arm handicapped → its ceilings are **lower** bounds |
| `index-count-hot` | 0.864x | 1.30% | 14.76% | ±14.76% | **not a result** by this document's own rule — see the note below |
| `generation-publish-read` | 0.893x | 1.83% | 4.11% | ±4.11% | arm handicapped → **lower** bounds |
| `index-hash-rw` | 0.949x | 1.89% | 0.76% | ±2.40% | arm handicapped → **lower** bounds |
| `index-btree-rw` | 0.950x | 1.78% | 1.28% | ±2.40% | arm handicapped → **lower** bounds |
| `cypher-read-label-small` | 0.960x | 0.67% | 1.16% | ±2.40% | arm handicapped → **lower** bounds |
| `index-manager-fanout` | 0.976x | 1.81% | 3.31% | ±3.31% | inside the tolerance — no direction |
| `lpg-neighbours-read` | 0.987x | 1.99% | 0.70% | ±2.40% | inside the tolerance — no direction |
| `cypher-write-mem` | 1.004x | 1.73% | 1.07% | ±2.40% | inside the tolerance — no direction |
| `mvcc-session-write` | 1.012x | 0.60% | 0.93% | ±2.40% | inside the tolerance — no direction |
| `dst-disk-wal` | 1.025x | 0.46% | 0.16% | ±2.40% | **arm favoured** → **upper** bounds (marginal: 0.1 pp clear) |
| `cypher-mixed-rw` | 1.034x | 1.75% | 0.92% | ±2.40% | **arm favoured** → **upper** bounds |
| `dst-concurrent-bolt` | 1.078x | 0.95% | 0.62% | ±2.40% | **arm favoured** → **upper** bounds |

Five arms understate their ceilings, three flatter them, and five establish no
direction from this campaign — `index-count-hot` among them, whose cause is
nonetheless settled by a dedicated experiment further down. There is no single
direction, and there never was: the old table found one only because it had
dropped every arm that pointed the other way.

**Where these cells come from.** All thirteen are rows of the round-2 campaign's
own `ceiling.tsv` (14 pairs × 3 levels × 5 interleaved rounds, loadavg **2.18
before / 8.10 after**, exit 0) — the same 42-row file the freshness audit above
reproduced this document's ranked ceilings, handicap ratios and validity-control
rows from. Six of the thirteen were already published here and are covered
directly by that audit's *verified fresh* verdict. The other seven were never
published, so the audit never checked them one by one: they are as fresh as the
file they share and the audit established that file's provenance, but that is a
statement about the file rather than a per-figure check, and it is recorded here
as the weaker claim it is. **No figure corrected in this section derives from an
input the audit marked "cannot be verified".**

#### What dividing by the level-1 cell removes, and what it does not

Normalising removes the part of the construction bias that is **constant across
the ladder** — the arm's per-operation routing wrapper, its extra resident state,
its allocator and cache effects — because that part is present at level 1 too.

It does **not** remove a bias that **grows with the level**, and this package has
a large one. `drive` (`bench/contention/observatory.go:291`) holds the **total**
operation count fixed and splits it across the workers, so at level *N* > 1 each
of an arm's replicas accumulates roughly 1/min(*N*, replicas) of the writes the
base's single fixture accumulates — and a smaller fixture answers a cheaper
query. That effect is exactly **zero** at level 1 (one worker, one replica, the
same writes), so the anchor cannot see it and the division cannot remove it.

**For every arm whose per-operation cost depends on state its fixture
accumulates, the normalised figure is therefore still an upper bound**, by an
amount this task did not measure. The clear cases are the arms that write a graph
or a log on every operation: `cypher-write-mem`, `mvcc-session-write`,
`cypher-mixed-rw`, `dst-concurrent-bolt` and `dst-disk-wal`. The two read-only
arms — `cypher-read-label-small` and `lpg-neighbours-read` — are exempt, because
their fixtures do not grow.

#### Corrected ceilings: raw, and normalised by the arm's own level-1 cell

Row numbers are the ranked inventory's. **Bold** marks a correction large enough
to change how the row should be read.

| # | arm | cell @1 | ceiling @8: raw → normalised | ceiling @1024: raw → normalised |
|---:|---|---:|---:|---:|
| 1 | `generation-publish-read` | 0.893x | 47.619x → **53.348x** | 23.878x → **26.751x** |
| 2 | `index-manager-fanout` | 0.976x | 3.396x → 3.479x | 32.223x → 33.012x |
| 3 | `dst-concurrent-bolt` | **1.078x** | 1.627x → **1.509x** | **16.307x → 15.127x** |
| 4 | `metrics-emit` | 0.627x | 3.300x → **5.263x** | 2.138x → **3.410x** |
| 5 | `dst-disk-wal` | 1.025x | 3.860x → 3.765x | 1.353x → 1.320x |
| 6 | `index-count-hot` | 0.864x | 2.916x → 3.374x | 2.392x → 2.767x |
| 7 | `index-hash-rw` | 0.949x | 1.960x → 2.066x | 1.531x → 1.614x |
| 8 | `cypher-write-mem` | 1.004x | 1.262x → 1.257x | 1.702x → 1.695x |
| 9 | `mvcc-session-write` | 1.012x | 1.308x → 1.292x | 1.712x → 1.691x |
| 10 | `cypher-read-label-small` | 0.960x | 1.152x → 1.200x | 1.116x → 1.163x |
| 11 | `cypher-mixed-rw` | **1.034x** | 1.111x → 1.074x | 1.272x → 1.230x |
| 12 | `index-btree-rw` | 0.950x | 0.948x → **0.998x** | 1.005x → 1.058x |
| 13 | `lpg-neighbours-read` | 0.987x | 1.004x → 1.017x | 1.001x → 1.014x |

Two readings change materially. `metrics-emit` is the largest understatement in
the document: its 3.300x at 8 is really **5.263x**, and its 2.138x at 1024 is
**3.410x**. `dst-concurrent-bolt`'s 16.307x headline is really **15.127x**, and
it is an upper bound rather than a lower one, so 15.127x is the ceiling's
optimistic end and not its pessimistic one.

**A caveat that must travel with this table.** For the five arms whose level-1
cell does not clear its tolerance — rows 2, 6, 8, 9 and 13 — the correction the
division applies is itself smaller than the instrument can resolve, so for those
rows the raw and the normalised figure are equally good estimates and the
normalised one carries no extra authority. Row 6 is the awkward case: its
campaign cell (0.864x) fails its own 14.76% spread, while the dedicated
experiment reported below puts the same arm at 0.879x with a 1.75% base spread.
The direction of its correction is settled; its magnitude is not, and 3.374x
would be 3.314x if the experiment's cell were used instead.

One reading is repaired rather than changed. Row 12's raw 0.948x at 8 reads like
a ceiling arm running *slower* than the base with eight cores available; it is
not, it is 0.998x with the arm's own 5% handicap taken out. The conclusion —
`graph/index/btree` is exhausted, there is nothing behind its sharing — is
unaffected, but the number that supported it was an artefact.

#### The cause of each departure, and how firmly it is established

The technical requirement behind rmp #2712 is that a cell which departs from
1.000 by more than the noise be given a **cause**, not merely a number. What
follows separates what was measured for this task from what is still an
assertion, because the previous version of this section stated causes it had not
measured — and two of them are now known to be wrong.

**`metrics-emit`, 0.627x — established, and the cause published here before was
wrong.** This section blamed "10 Prometheus registries instead of 1: worse
locality, more resident state". The arm installs exactly **one** registry, and
its own godoc (`bench/contention/ceiling_arms.go:408`) says so: what it
replicates is the metric **name**, not the backend, deliberately, so that the
counter's cache-line cost is separated from the registry's lookup cost. The real
cause is an allocation asymmetry that replicating the name introduces.
`metricsOp` (`bench/contention/workloads_unreached.go:261`) builds every metric
name by concatenating a suffix; the base workload passes `""`, the arm passes
`.rN`. Measured on this host (`go test -bench -benchmem -count=3`, Apple M4,
go1.27.0):

| concatenation | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `"bench.contention.ops" + ""` (base) | 3.18 | 0 | **0** |
| `"bench.contention.ops" + ".r3"` (arm) | 12.24 | 24 | **1** |

The empty-suffix case takes the Go runtime's `concatstrings` fast path and
returns the operand without allocating. At 1.3125 concatenations per operation
(a counter always, a latency every 4, a gauge every 16) the arm pays about 11.9
ns/op and 1.31 allocations the base never pays. The in-situ level-1 gap is
**13.46 ns/op** in this campaign (22.62 vs 36.08 ns/op, from the 44.20M/s and
27.71M/s cells above) and **13.25 ns/op** in the rmp #2698 re-measurement (23.25
vs 36.50). The concatenation accounts for the great majority of it either way.
The allocation counts reproduced exactly; the ns/op figures above were taken on a
host that was not quiet, and rmp #2698's reading of the same benchmark gave 3.174
and 16.17 ns/op — the mechanism is identical, the arithmetic is tighter with the
numbers measured here.

**`index-hash-rw` 0.949x, `index-count-hot` 0.864x, `generation-publish-read`
0.893x — the cause is the arm's ROUTING WRAPPER, not the replica count.** This
section blamed "10 hash indexes instead of 1", "10 count stores instead of 1",
"10 CSR publishers instead of 1". At level 1 the single worker meets replica 0
and the other nine are never touched, so the replica count has no obvious way to
cost anything — and a single-variable experiment says it does not.
`ceilingReplicas()` returns `GOMAXPROCS(0)`, so a child run at `GOMAXPROCS=1`
builds **exactly one replica** and then differs from its base workload only by
the wrapper: `run(ctx, set.pick(worker), worker, iter)`, which costs a modulo, a
bounds-checked slice load (`bench/contention/ceiling_arms.go:107`) and an
indirect call through a func value that the base workload's directly-captured
closure does not make.

Level 1, five interleaved rounds per cell, ten times the declared operation
count, one window per child process, loadavg **4.86 before / 6.54 after**. The
ns/op columns are the one-replica condition, where the wrapper is the only
remaining difference:

| arm | cell @1, replicas = 10 | cell @1, replicas = 1 | base ns/op | arm ns/op | delta |
|---|---|---|---:|---:|---:|
| `index-hash-rw` | 0.903x (34.91% / 25.73%) | **0.946x (2.93% / 1.65%)** | 14.49 | 15.32 | **0.83 ns** |
| `index-count-hot` | 0.880x (1.75% / 7.01%) | 0.879x (13.42% / 0.21%) | 4.86 | 5.53 | **0.67 ns** |
| `generation-publish-read` | 0.879x (16.96% / 4.69%) | 0.862x (37.96% / 48.18%) | 7.31 | 8.49 | **1.18 ns** |

Dropping the replica count from ten to one leaves each handicap where it was, so
the handicap is not the replica count. And the three deltas — 0.83, 0.67 and
1.18 ns/op across three unrelated data structures — are a roughly **constant
per-operation cost**, which is what a fixed wrapper predicts and what "ten copies
instead of one" does not. Confidence: **established** for `index-hash-rw`, whose
one-replica cell carries 2.93% / 1.65% spreads and lands on the campaign's own
0.949x; **established** for `index-count-hot`, whose two point estimates differ
by 0.1% and whose ten-replica cell is tight; **supported but not established**
for `generation-publish-read`, whose one-replica cells carry 38% and 48% spreads.
The host was not quiet throughout: another agent's test suites were running.

**`index-count-hot`, a note on its 0.864x.** The campaign's own level-1 row does
not clear its ceiling arm's five-round spread of 14.76%, so by this document's
working rule that cell is not a result and the old table should not have listed
it as one. The dedicated experiment above does clear its spreads and puts the
same arm at 0.879x with a 1.75% base spread, so the arm really is handicapped;
the direction rests on that experiment, not on the campaign row.

**`cypher-read-label-small` 0.960x and `index-btree-rw` 0.950x — NOT
established.** Their level-1 operations cost 971 ns and 40.9 ns, so the ~0.8 ns
wrapper is 0.08% of the first operation and 2.0% of the second, against
deficits of 4.0% and 5.0% — so it explains essentially none of
`cypher-read-label-small`'s and under half of `index-btree-rw`'s. Something else
carries the rest, and this task did not measure it. The causes this section gave
for them are assertions, not measurements, and are retained only as hypotheses:
ten engines and ten graphs, and ten btree indexes, in place of one.

**`dst-concurrent-bolt`, 1.078x — mechanism established, and the cause recorded
in rmp #2712 is refuted at level 1.** rmp #2712 and
`docs/bolt-evaluation-2026-09-03.md` both give the cause as "its ten replicas
each accumulate a tenth of the writes and so answer cheaper queries against a
smaller graph". That cannot operate at level 1. `drive` splits a **fixed** total
of 2000 operations across the workers and `replicaSet.pick` routes worker *i* to
replica *i* mod *N*, so at level 1 the one worker meets replica 0 and that single
replica accumulates **all** 2000 operations — exactly what the base workload's
single server accumulates. Nine replicas stay empty. The effect is real, but it
is a property of levels above 1 and is precisely the level-dependent bias the
anchor cannot remove; it is not what the level-1 cell measures.

What does differ at level 1 is the live heap, and through it the garbage
collector. Measured with `GODEBUG=gctrace=1`, one window per child process, level
1, the identical 2000 operations, five interleaved pairs:

| | base | ceiling arm |
|---|---:|---:|
| live heap after GC (median) | 3 MB | 7 MB |
| next-GC goal (median) | 7 MB | 15 MB |
| GC cycles in the window | 1643 – 1656 | 777 – 815 |

Ten Bolt servers where the base has one raise the live heap about 2.3×; Go's
pacer sets the next heap goal proportional to the live heap, so the goal is about
2.1× larger and the identical workload finishes in **about half** the GC cycles.
Across the five independent process pairs the base's cycle count varies by 0.8%
and the arm's by 4.9%, and their ratio stays between **2.03 and 2.12** — the
effect dwarfs its own scatter. The heap figures corroborate the refutation on
their own: if the writes
really were spread across ten replicas the live heap would be about the base's,
not 2.3× it. 7 MB is one written server plus nine idle ones.

Raising `GOGC` to 800 for both arms — a single variable applied identically —
moved the paired median from 1.069x to 1.033x and lifted the base arm's
throughput by about 37% against the ceiling arm's 13%, both in the direction the
mechanism predicts. That is consistent, but it does not fix a magnitude: the host
was not quiet (loadavg 4.48 → 6.91) and the per-round ratios ranged 0.60x to
1.38x. **The mechanism is established; how much of the 7.8% it accounts for is
not.**

The *direction*, by contrast, is not in doubt. Three independent campaigns put
this cell above 1.00 with tight spreads: **1.078x** here (0.95% / 0.62%), and
**1.0959x** and **1.0954x** in the two Bolt-evaluation probes at HEAD `d4f49b85`
(`docs/bolt-evaluation-2026-09-03.md`, which normalises its own 10.353x at 64 to
9.46x). A fourth reading taken for this task on a loaded host gives the same
point estimate, **1.098x**, but with 23.26% and 42.56% spreads — and the probe's
own tolerance rule therefore declined to call a direction from it, which is the
rule working correctly. It corroborates the value; it establishes nothing by
itself.

**`cypher-mixed-rw` 1.034x and `dst-disk-wal` 1.025x — direction established
marginally, cause NOT established.** Both clear the ±2.4% floor, by 1.0 and 0.1
percentage points. `cypher-mixed-rw` was re-measured for this task and
reproduces: **1.036x** (spreads 1.44% / 0.33%, three interleaved rounds, loadavg
2.88 → 2.81), which clears the floor on its own and puts the arm's raw 1.098x at
8 in that run at 1.060x normalised. `dst-disk-wal` was not re-measured, and at a
tenth of a point clear of the floor it should be read as provisional. Neither cause was measured for this task. Both arms' fixtures
accumulate state on every operation, so the GC-pacing mechanism established above
for `dst-concurrent-bolt` is the obvious candidate; `dstDiskCeiling` additionally
unshares the harness's own simulated disk, a confound this document already
records against row 5.

#### This is now enforced, not merely documented

`bench/contention` no longer lets a ceiling be published without its level-1
cell. `TestCeilingProbe` refuses a ladder that omits level 1 before any window
runs; `normaliseByAnchor` refuses to normalise a pair whose level-1 cell is
missing or unusable, and that pair's rows are dropped rather than published raw;
and `writeProbeSummary` refuses to write `ceiling.tsv` at all if a row reaches it
without an anchor. `ceiling.tsv` now carries `ratio_at_1`, `ratio_normalised`,
`anchor_tolerance` and `direction` beside every raw ratio, and the probe's log
prints the correction as well as its result.

All three refusals are proved non-vacuous in `bench/contention/normalise_test.go`:
each one was mutated and the test that covers it fails, then passes again on
restore. The outermost guard is covered twice over, because a pure-predicate test
would not have been enough — neutering the `if` while leaving the predicate
correct keeps every in-process test green, so the probe is also driven as a child
process on a ladder of `8` alone, and that arm asserts both that it refuses and
that the artefact directory is still **empty** afterwards. A guard that fires
only after the campaign has been measured is not this guard.

## The mutex profiler is blind to the module's worst site

`generation-publish-read` is the worst-scaling workload measured — **0.094x at 8
goroutines**, ten times slower with eight cores than with one — and its mutex
profile holds **64.34 ms**. The CPU profile says why: `sync/atomic.(*Int64).Add`
is **76.92% of all CPU**, split `Publisher.Acquire` 41.25% / `releaseRef` 40.08%.
The per-generation refcount is one atomic on one cache line and every reader
touches it.

**Cache-line coherence is not a lock.** No profiler that measures *blocked time*
can see it, because nothing blocks: the CAS succeeds, it just costs a coherence
round trip every time. Only the scaling column can report it, and only a ceiling
arm can price it.

Two more sites are in the same class, and both are on real engine paths:

| workload | scaling @8 | mutex delay @1024 | what the CPU profile says |
|---|---|---|---|
| `index-count-hot` | 0.328 | **4.90 ms** | `atomic.Int32.Add` = 44.85% of CPU, under `count.(*Store).Apply` |
| `metrics-emit` | 0.445 | **5.23 ms** | `metrics.IncCounter` = 82.01% cumulative |

The workload that provoked #1 predicted this in its own godoc before it was run:
"a surface can be contended without a single blocked nanosecond, and only the
scaling column can say so". It was right.

## Big share, small ceiling

Round 1's central lesson repeated twice this round, now with the ceiling numbers
that prove it. These two sites hold the largest shares of their profiles in the
Cypher paths, and partitioning them is worth almost nothing:

| site | share of its profile | ceiling @8 raw | normalised |
|---|---:|---:|---:|
| `cypher/plan_cache.go:85` `planCache.get` | 61.48% | 1.111x | **1.074x** |
| `cypher/api.go:4933` `parseAndAnalyse` | 53.29% | 1.152x | **1.200x** |

The two corrections run in opposite directions, which is the point of the
section above: `cypher-mixed-rw`'s arm is favoured at level 1 (1.034x) so its
ceiling shrinks, `cypher-read-label-small`'s is handicapped (0.960x) so its
ceiling grows. Neither correction changes the conclusion — both sites are worth
almost nothing — but neither ceiling was what it was published as.

`planCache.get` was the #2 site of round 1 at 74.68%, and #2691 addressed it.
It still holds 61.48% of `cypher-mixed-rw`@1024's delay — **and there is 11% left
behind it.** Ranking by share would put it near the top of this document; ranking
by ceiling puts it eleventh, which is where the evidence says it belongs.

## Coverage

### DST drivers — the gap round 1 recorded, now closed

| driver | status |
|---|---|
| `sim.RunConcurrent` | **reached** — `dst-concurrent-bolt` drives it against one shared `sim.SimServer`, one connection per operation so `level` stays the only concurrency variable |
| Fault injection | **reached** — `dst-disk-fault-wal`, a real durable Cypher write path over `sim.SimDisk` via `wal.OpenWith`, every append and fsync crossing `SimDisk.mu`, at a seeded 1/512 per-sync fault probability |
| `sim.RunMVCCSessions` | **reached** — `dst-mvcc-sessions`; see below |
| Full durable DST stack over `SimDisk` | **not constructible** — `sim.OpenSimStore` is exported but its second parameter is the unexported type `simStoreConfig`, and its only constructor `durableStoreConfig()` is unexported too. Reaching it requires editing `internal/sim`, which this task placed out of scope |

**The fault arm is honest only as a pair, and the probe that established this is
worth recording.** Driving 5000 durable commits at four fault rates:

| fault rate | ok | faults | syncs |
|---|---:|---:|---:|
| 0 | 5000 | 0 | 5000 |
| 1e-05 | 5000 | 0 | — |
| 1e-04 | 5000 | 0 | — |
| 1/512 | **559** | **4441** | **561** |

The WAL writer **fail-stops on the first injected fsync fault and never syncs
again** — syncs stop at 561 for 5000 attempted commits, and the first fault reads
`wal: durability failed; the un-synced suffix was discarded and this writer is
poisoned`. That is the reliability mandate working exactly as specified, not a
defect. But it means there is **no steady state of "durable writes with occasional
faults" to measure**: after the first fault the window measures the poisoned-writer
error path and nothing else. Published as a controlled pair with one variable
between the arms — `dst-disk-wal` (rate 0) and `dst-disk-fault-wal` (rate 1/512) —
rather than as a single row whose number would have been 99.75% one fail-stop
branch.

### The three surfaces round 1 never drove

| surface | status | result |
|---|---|---|
| `graph/generation` | **covered** — `generation-publish-read` | the worst site in this inventory, #1 |
| `graph/index` `Manager` | **covered** — `index-manager-fanout` | #2 |
| `internal/metrics` | **covered** — `metrics-emit` | #4 |

`internal/metrics` needed care to cover honestly: its default backend is a no-op
behind an `atomic.Pointer`, so driving it as shipped measures nothing. The
workload installs the real `internal/metrics/prometheus.Registry` backend, and
the row is therefore about metrics **as enabled**, not as defaulted.

### dst-mvcc-sessions shares nothing, and that is the point

Every other workload in the registry puts `level` goroutines onto ONE shared
fixture, so its scaling column reports how that fixture behaves under sharing.
This one cannot: `sim.RunMVCCSessions` builds its own `sim.SimDisk`, its own
store and its own engine on every call, and the mode is single-goroutine
internally. N concurrent operations are N independent simulations touching no
common object.

That makes it useless as a lock probe and valuable as a different one. **The
reliability mandate forbids hidden global state, so N shares-nothing simulations
ought to scale with the cores available to them.** They reach only **2.169x at 8
and 2.839x at 64**, where a shares-nothing workload on 10 cores should approach
the 5-6x the other CPU-bound workloads reach (`search-bfs-csr` 5.819,
`index-btree-rw` 5.750). It has no ceiling arm, because there is nothing shared
to unshare.

**This is recorded as an open question, not as a finding.** The obvious
candidate is GC and allocator pressure — each operation builds and discards a
whole store — and that has NOT been measured. It is not evidence of a lock.

### What is NOT covered

- Crash and recovery paths — `dst-disk-fault-wal` reaches the fail-stop branch
  and `sim.MVCCSessionsConfig.Crash` is left at its zero value, so no workload
  restarts a store and replays a WAL under concurrency.
- `sim.OpenSimStore`'s full durable stack, for the constructibility reason above.

## The error columns are backpressure, and the first probe hid half of it

Two cells reported non-zero errors, both only at 1024: `bolt-connect-churn`
**1691 / 30000 (5.6%)** and `dst-concurrent-bolt` 31. Round 1 published "0
errors", so the column is new and had to be explained rather than assumed.

Reproduced by driving the workload's own op at 1024 goroutines:

| start shape | errors | distinct classes |
|---|---:|---|
| staggered | 9 / 30720 (0.03%) | 1 |
| release barrier, matching `drive` | **2163 / 30720 (7.04%)** | **2** |

**The staggered probe was off by 180x on rate and missed an entire error class.**
With the barrier the two classes are 1934 x `connect: sim: handshake read: EOF`
and 229 x `connect: sim: handshake write: sim: SimConn is closed`.

Root cause, read from the source at `bolt/server/serve.go:768-779`:
`defaultMaxConnections` is **1024** (`serve.go:28`) and the workload runs exactly
1024 dialling goroutines. On a full semaphore the accept loop increments
`metricConnRejected`, logs `bolt: max connections reached, rejecting`, and closes
the socket. Both client messages are that one close, seen from either side of the
handshake.

**Not a defect.** Saturation is answered with a refusal that is counted and
logged, which is what the reliability mandate requires. The Bolt handshake has no
pre-negotiation error frame, so a closed socket is the only refusal available at
that point.

## Wiring the MVCC driver fired its oracles immediately

`dst-mvcc-sessions` reported **3 errors at level 1** — a single goroutine, seeds
0..299, deterministic by construction and therefore replayable. Replaying them
gives two distinct classes.

### Class A — an ACID_CONSISTENCY violation (seed 29)

```
[ACID_CONSISTENCY] tick=60 op="edge count": edge-count mismatch: oracle=3 engine=4
```

Established, not inferred:

- **Deterministic**: identical on 3 of 3 replays, same counts every time
  (committed=11, rolledback=3, statements=35).
- **Transient**: fires at `Ticks` 60 and 61, clean at 55-59 and at 62-72. The
  divergence heals.
- **It is the TERMINAL check, not an in-loop one.** `CheckEvery` normalises to 1,
  so parity is verified at every tick and the loop stops at the first failure.
  A run of 70 ticks passes through tick 60 and stays clean, so tick 60's in-loop
  check passes; the failure is the check at `mvcc_sessions.go:367`, which runs
  after the drain has rolled back every open transaction.
- **The schedule leaves two interleaved transactions open at the drain**: at tick
  56 session 0 runs an uncommitted `CREATE (a)-[:KNOWS]->(b)` from `mv-s0-m2`,
  and at tick 60 session 2 runs an uncommitted `DETACH DELETE` of that same node
  `mv-s0-m2`.
- **Neither shape leaks on its own.** Both were tested in isolation against a
  fresh engine — an uncommitted `CREATE` of an edge, then rollback, and an
  uncommitted `DETACH DELETE` of a node with an edge, then rollback — and the
  edge count is unchanged in both. **The defect needs the interleaving.**

Which side is wrong — a rolled-back edge surviving in the engine, or the oracle
dropping one it should keep — is NOT established here and must not be assumed.
Registered as its own task; reducing it to a minimal reproduction is that task's
first job.

### Class B — the scenario generator emits statements its own graph forbids

Seeds 102 and 215 return a run error, not a violation:

```
exec: CreateRelationship AddEdge: cypher: cannot create a parallel edge on a
non-multigraph graph ... (between "__cx_1135" and "__cx_1135")
```

The generator's `CREATE (a)-[:KNOWS]->(b)` can draw a pair that already has a
`KNOWS` edge, and — seed 215 — can draw `a == b`. The engine refuses correctly
with a typed error; the mode's graph is not a multigraph. **This is a harness
defect in `internal/sim`, not a module defect**, and it makes roughly 0.7% of
seeds abort a `RunMVCCSessions` run that should have completed.

## Validity control

`cypher-write-mem` measured against **itself**, carried in the same probe run
that produced every ceiling above, arms interleaved and alternating exactly as
the real pairs:

| level | base | ceiling | ratio |
|---|---:|---:|---:|
| 1 | 367,451/s (1.27%) | 369,030/s (0.83%) | **1.004x** |
| 8 | 767,412/s (2.46%) | 769,231/s (1.21%) | **1.002x** |
| 1024 | 517,058/s (4.69%) | 502,092/s (1.98%) | 0.971x |

The 1024 cell's 2.9% gap does not clear the base arm's own 4.69% spread, so by
this document's own working rule it is 1.00x within noise, not a 3% effect.

## Correction: site 2 IS on an engine path

**This document first stated that `graph/index.Manager` had no engine caller.
That was wrong, and the error is recorded here rather than quietly edited out.**

The claim came from a `grep` whose output was truncated at twenty lines. Every
line that survived the truncation was a doc comment, so the absence of calls
looked established when it had merely been cut off. A type-aware cross-reference
over the module (`golang.org/x/tools/go/packages`, resolving each identifier to
its `types.Object`) finds **113 references to the `Manager` type across 28
production files**, and a direct count confirms **88 non-comment production
references** to `index.Manager` under `cypher/`, `graph/`, `store/` and `bolt/`.
Two of them, read and verified:

- `cypher/exec/index_writeback.go:45` — `IndexBuffer.Commit` calls
  `mgr.ApplyBatch(b.changes)`, so **every buffered index change on the write path
  goes through the Manager**.
- `graph/query/index_seek.go:494` — the seek planner calls `mgr.ListIndexes()`
  and `mgr.GetIndex(name)` on the read path.

The two godoc quotes that misled me say something narrower than I read into them.
`graph/index/label/index.go:16` is about the **`label.Index` type as a
`Subscriber`**, not about the Manager, and the same comment says plainly that
"every production call site registers a btree or hash index". `graph/query/query.go:12`
("a future iteration will plug in") is simply **stale** — `index_seek.go` already
does it.

**One nuance survives, and it matters for the fix.** The measured workload drives
`Manager.Apply` (singular), and `Apply` itself has **zero** production callers:
production batches through `ApplyBatch`. But both take the same `m.mu.RLock()`
over the same subscriber set (`manager.go:254` and `manager.go:262`), so the
contended object is the one the engine really uses. What differs is the
**frequency**, because production amortises many changes into one batch. The
32.223x ceiling is therefore a real ceiling on a real lock, measured through an
entry point the engine does not itself call — treat it as an upper bound on what
the engine could recover, not as throughput the engine is losing today.

Site 1 is unaffected by this correction. `graph/generation.Publisher` really has
no production importer, and `graph/generation/generation.go:33-40` says so
deliberately: "This package is NOT a second snapshot mechanism in the engine —
Nothing in the module uses it ... It is a utility a consumer may use to cache a
derived structure." It is consumer-facing API, so its 0.094x scaling is a defect
in something the module publishes, not a cap on the module's own queries.

## Correction: the write-barrier rows rank a CUMULATIVE frame, and the durable ceiling is confounded

**Rows 5, 8 and 9 of the ranked inventory attribute blocked time to
`cypher/api.go:18379` `execUnderBarrier`. That frame holds ZERO of it.**
Established by rmp #2697 and verified independently from the same profiles:

| workload | `execUnderBarrier` flat | cumulative |
|---|---:|---:|
| `cypher-write-mem`@1024 | **0 (0%)** | 84.90% |
| `mvcc-session-write`@1024 | **0 (0%)** | 89.62% |
| `dst-disk-wal`@1024 | **0 (0%)** | 99.08% |

The ranking above was produced with `go tool pprof -top -cum -lines`. Cumulative
attribution locates a *subsystem*; it does not identify where goroutines
actually block, because a parent frame inherits everything beneath it.
`execUnderBarrier` wraps the whole write path, so it inherits all of it. **A
cumulative share is not a bottleneck**, exactly as a mutex share is not a
ceiling — the same lesson this document already records, arrived at from the
other direction.

Worse, the frame's name misled the reading and its own godoc did not: it says
"THE NAME IS HISTORICAL … concurrent writers DO run alongside this one". The
bracket is held **shared**, and `mvcc.Gate`'s weak path is an atomic add on a
striped padded slot, not an RWMutex. Writers are not serialised there.

**Where the delay actually is**, from the same profiles:

- `cypher-write-mem` — `lpg.setNodeLabelInfo` **46.0%**, `HasNodeLabel` 11.7%,
  `Mapper.Lookup` 9.3%, `planCache.get` 6.4%, antlr 5.2%
- `mvcc-session-write` — `setNodeLabelInfo` 36.5%, `Mapper.Lookup` 10.2%,
  `label.Index.mutate` 9.7%, antlr 9.2%, `AwaitVisible` 9.2%
- `dst-disk-wal` — `waitApplyTurn` **49.9%**, `wal.AppendRun` 22.2%,
  `wal.syncToLocked` 24.4% — about 99% in `store/wal` plus the apply gate

**And the 3.860x durable ceiling is confounded.** `dstDiskCeiling`
(`bench/contention/ceiling_arms.go:475`) builds a fresh `sim.NewSimDisk` per
replica, so the arm unshares the harness's own simulated-disk mutex alongside
the module's structures. It is not a clean measure of what the engine could
recover, and row 5 must not be read as one.

One further measured caution recorded here because it bounds every ratio in
this document: the write path already runs at **649%, 669% and 506% of ten
nominal cores**, against this host's practical ceiling of **7.61x, not 10x**
(4 performance + 6 efficiency cores). At ~650% it is near capacity, and the
blocked time is a symptom of hundredfold oversubscription rather than its cause.

## What the evidence says to do next

Ranked by ceiling weighted against reachability. Every ceiling below is given as
**raw → normalised** by the arm's own level-1 cell; the normalised figure is the
better estimate, and the ranking is unchanged by the correction.

1. **`graph/generation` refcount — 47.6x → 53.3x at 8, exported, deliberately
   unwired.** One shared `atomic.Int64` per generation. The obvious remedy is a
   striped or per-P refcount summed on publish. Highest ceiling in the module by
   a factor of 12, and it caps a consumer's throughput rather than the engine's.
   Its arm is handicapped at level 1 (0.893x), so even 53.3x is a lower bound.
2. **`graph/index` `Manager` fan-out — 32.2x → 33.0x at 1024, and it IS wired.**
   One `RWMutex` over the whole subscriber set, taken by `ApplyBatch` on every
   write-path index writeback and by `ListIndexes`/`GetIndex` on the seek path.
   Measured through `Apply`, which the engine does not call; see the correction
   above for what that does and does not license you to claim.
3. **`store/wal` durable commit — 3.860x → 3.765x at 8, on the engine path.** The
   highest ceiling of any wired site at the concurrency the hardware can actually
   serve. Read it as an **upper** bound twice over: its arm is favoured at level 1
   (1.025x), and `dstDiskCeiling` also unshares the harness's own simulated disk.
4. **`internal/metrics.IncCounter` — 3.300x → 5.263x at 8.** The largest
   correction in the document: the arm pays a 37% handicap at level 1, which is
   its own metric-name allocation and not the locality this document previously
   blamed. With the real Prometheus backend installed the surface scales at
   0.445x: eight goroutines emit metrics at less than half the rate one does.
   What enabling metrics costs against the no-op default was NOT measured here
   and must not be inferred from this number.
5. **`graph/index/count` hot type — 2.916x → 3.374x at 8, on the engine path.**
   Round 1's #2682 fixed the *spread* case; the *single hot type* case is still
   0.328x, and is atomic contention rather than lock contention.
6. **`graph/index/hash` Insert — 1.960x → 2.066x at 8.** #2692 cut its blocked
   time from 1199.90 s to 273.74 s and turned 0.916x into 1.742x, but the site
   still holds 97.91% of its profile and there is roughly 2x left behind it.
7. **The write barrier `cypher/api.go:18379` — 1.70x → 1.69x at 1024.** Modest,
   but it is the single point every write in the module passes through.

Stop below that line. `graph/index/btree` reads 0.948x → 0.998x and 1.005x →
1.058x, and `lpg-neighbours-read` reads 1.004x → 1.017x and 1.001x → 1.014x:
their sharing costs nothing, and no amount of sharding would repay the effort.

---

## Correction to ranked row 3 — measured 2026-09-03 (rmp #2710)

Row 3 above is **misattributed at the leaf, and its provoking workload is not
measuring what the row implies**. Both corrections are defects in how the number
was derived, not in the measurement itself. Established at HEAD `225b08d5`,
Apple M4, 10 cores, go1.27.1, darwin/arm64.

### 1. The leaf is `graph/lpg` node-property shard locks, not the Bolt server

`serve.go:802` is the `handleConn` call — a cumulative frame that inherits every
byte of Bolt work beneath it by construction — and `applyVersionedInstant` is the
write barrier, which rmp #2697 established holds zero delay. Peeked to its
callers at `dst-concurrent-bolt`@64 (mutex profile, `SetMutexProfileFraction(1)`,
so every contention event is sampled):

| site | delay | share of all mutex delay |
|---|---:|---:|
| `graph/lpg/property.go` `delNodePropertyInfo` | 153.03 s | **62.8%** |
| `graph/lpg/property.go` `setNodePropertyInfo` | 51.85 s | **21.3%** |
| everything in `bolt/server` | — | **< 0.2%** |

Means of n=8 interleaved runs. `sync.(*Mutex).Unlock` carries 42.9% of the delay
flat, but 98.67% of it is reached from `sync.(*RWMutex).Unlock` inline — it is the
RWMutex's own internal mutex, **not** a second global lock, and the hypothesis
that one existed is refuted.

### 2. The @64 collapse is dominated by fixture cross-talk, not by the engine

`dst-concurrent-bolt` runs the **same 2000 operations at every level**. Counting
the calls that reach the node-property shard lock (temporary counters, one
counting-only run per level):

| level | `delNodePropertyInfo` calls | per operation | removed a property |
|---:|---:|---:|---:|
| 1 | 11 264 | 5.6 | 100.0% |
| 8 | 13 312 | 6.7 | 88.3% |
| 64 | 3 190 784 | **1 595.4** | **2.2%** |

**A 283x amplification of the work for an unchanged operation count.** The cause
is in the harness fixture, not the engine: every `sim.RunConcurrent` call runs
`probeWireParamTypes` (`internal/sim/wire_param_types.go`), whose per-call fixture
uses a **fixed label `WireParam` and a fixed id `wp-1`** against the **one shared
`SimServer`** all `level` workers drive. With no uniqueness constraint on that id,
concurrent probes leave many live matching nodes, so each probe's
`MATCH (n:WireParam {id:$id}) SET n.s = $nul` and its `DETACH DELETE` fan out
across every worker's nodes instead of its own.

Consequently **the 0.453 scaling figure at @64 is not a clean measure of engine
write scaling**, and a change that removes lock contention there will not move it
much — which is exactly what was measured (below). The level-1 and level-8 cells
are unaffected by the cross-talk and remain sound.

### 3. What the fix changed, and what it did not

`delNodePropertyInfo` now settles its three non-mutating outcomes under the
shard's **shared** lock (`graph/lpg/property.go`, `delNodePropertyShared`). At @64,
97.8% of its exclusive acquisitions removed nothing: 14.1% found no bag, 37.7%
found a bag without the key, 46.0% were refused by the conflict test.

Interleaved A/B, n=8 per arm, arms alternated ABABAB, loadavg bracketed on every
run:

| metric | baseline | with the pre-pass | delta |
|---|---:|---:|---:|
| `delNodePropertyInfo` mutex delay | 153.03 s (sd 5.32) | **7.03 s** (sd 2.72) | **−95.4%** |
| total mutex delay | 243.99 s (sd 11.60) | 145.16 s (sd 43.89) | −40.5% |
| `setNodePropertyInfo` mutex delay | 51.85 s (sd 5.17) | 65.79 s (sd 21.20) | +26.9% |
| ops/s @64 | 253.4 (sd 22.5) | 277.0 (sd 25.0) | +9.3%, Welch t=1.98 |

**The throughput gain is NOT established.** +9.3% sits inside this cell's own
noise: the A-vs-A spread measured here is **±32.8%** (n=8), wider still than the
±11.5% the Bolt evaluation published for it, and t=1.98 is p≈0.07 two-sided. The
mutex-delay result is unambiguous; the throughput result is not, and the reason is
§2 — removing the *waiting* does not remove the *work*.

No regression at any swept level of `cypher-write-mem` or `mvcc-session-write`
(18 cells, worst −1.97% at `mvcc-session-write`@1024, t=−1.88, not significant;
the two cells that appeared to move at n=2 collapsed to noise at n=5).
