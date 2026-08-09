# Production-readiness certification — GoGraph module

**Date:** 2026-08-09 · **Entry head:** `78c21ed9` · **Exit head:** see §8 · Apple M4 (10 cores), `darwin/arm64`, go1.26.5

Scope: the **`GoGraph` module**, exercised through the **37 programs under `examples/`**. The
examples are **instruments**, not subjects — they drive the module under realistic, seeded,
scale-parametrised conditions so its correctness, performance and efficiency can be observed.
Every program was run at its deterministic default and, where its flags allow, at a scale
where the evidence becomes interesting. `pprof` was used to attribute cost to call sites.

---

## Verdict: **NOT CERTIFIED for unrestricted production.**

**Certified for production within the envelope in §6**, with one open item that the envelope
cannot absorb: **#2378**, a cross-substructure **ACID Isolation** violation, now reproducible and diagnosed —
under the gate — now **reproducible on demand and DIAGNOSED**: the visibility basis is re-read per substructure, so a reader straddling a commit classifies each substructure against a different commit state. Confirmed by a pre-registered falsification. The fix is an architecture change and needs your agreement (§3).

Four defects were found, fixed and pinned — including a 2.5× query-performance win that only a
CPU profile could have found. The openCypher TCK is 100 %, coverage is 87 %, the crash-injection
battery is green, and every other ACID check this cycle ran holds. But `graph/lpg`'s
`TestIsolation_CrossSubstructure_EdgeImpliesLabels` reported *"observed 1 cross-substructure
violations (edge/label disagreement inside a pinned SNAPSHOT)"* during a `make ci` run, and the
project's ACID mandate on Isolation is absolute: a reader must never observe the partial writes
of an in-flight transaction. **One observation is enough to withhold an unqualified
certification.** §3 records everything established: the reproduction recipe, the five candidate
mechanisms refuted by measurement, and the two DIFFERENT tears the diagnostic then separated —
an edge-vs-label split caused by bare autocommit writes inside `ApplyAtomically` (fixed), and a
**label-vs-label split across shards inside ONE transaction**, which no caller error explains.

| Gate | Result |
|---|---|
| `make ci` (tidy, fmt, vet, build, `-race ./...`, lint, coverage) | **INTERMITTENT** — green on the exit head, but #2378 makes it red roughly 1 run in 5. See §7 |
| `go test -race ./...` at the exit head | **3 consecutive runs, exit 0**, zero cross-substructure violations |
| openCypher TCK execution | **3897 / 3897**, 0 failed, 0 undefined, baseline unchanged |
| Coverage | aggregate **87.0 %** (gate ≥ 85 %), every package ≥ 75 % |
| Crash-injection battery (`make test-crashinject`) | **exit 0** |
| `goleak` | green (inside `make ci`) |
| Examples: build | **37 / 37** |
| Examples: run to completion at the deterministic default | **34 / 37** — the other three are accounted for below, and none is a defect |

**What is worth carrying forward from this cycle is that the instruments and their operator
were wrong far more often than the module was.** Eight times I gave an example something other
than what I thought: three combinations were genuinely invalid and all three were **refused at
the boundary with a precise, actionable message**, and the other five were valid input I had
misread, where the module did exactly what I asked (§2). Nothing crashed, hung without
explanation, or returned a silently wrong answer.

Twice I was one message away from filing a defect that does not exist: an ACID **Consistency**
violation that was my own fixture error, and a **lost row** that is an ABA artefact in a test
oracle. Both were caught only by measuring instead of concluding — and one of them, #2371, had
been sitting in the backlog at severity 8 on the strength of its symptom's vocabulary.

---

## 1. What was broken, and how it presented

### #2376 — a grouped aggregation materialised a relationship the query never names

**Found by `pprof`, which is the only reason it was found at all.** A CPU profile of
`examples/26_social_scale_bench` (120 000 users, ~3 M `FRIEND` edges, via its `-profile-dir`
flag) attributed **17.16 % of all samples** to `cypher.buildRelationshipValueFromRow`, reached
from exactly one caller, `populateRowCtx`. Its own breakdown:

| callee | share of the 17.16 % |
|---|---:|
| `buildEdgeProps` | 46.8 % |
| `ReadView.HasEdge` | 25.2 % |
| `ReadView.EdgeLabels` | 13.2 % |
| `EdgeLabelsByHandle` | 5.5 % |
| `edgeInstanceIdxFor` | 4.7 % |
| `pickEdgeType` | 3.5 % |

Almost half of it was building the relationship's **property map** — for a variable that
neither slow query mentions:

```cypher
MATCH (u:USER)-[:FRIEND]->(:USER) WITH u, count(*) AS deg
MATCH (:USER)-[:LIKE]->(a:ARTICLE)  RETURN a.id, count(*)
```

The demand gate that prevents exactly this (#1630) was already in `populateRowCtx`, but it
engaged only for a non-nil `scalarUse`, and `newAggregationEval`'s AST path called
`buildRowCtx`, which passes nil. So every matched row built a full relationship value.

`analyseNodeScalarUse` now runs once at build time — the idiom `newRowPredicate` already uses —
and the row context is built through `buildRowCtxWithUse`. A bailout restores the previous path
byte for byte, because `buildRowCtx` *is* `buildRowCtxWithUse` with a nil `scalarUse`.

**The pooled path was rejected on a lifetime argument, not a performance one.**
`evalRowPooled` hands out arena-borrowed lazy values whose lifetime ends at `releaseRowCtx`,
and unlike a `WHERE` predicate the value here becomes a projected row cell. `populateRowCtx`
documents the arena-nil path as the one that preserves escape safety, so with a nil arena the
gate can only ever **omit** a variable the expression never names — and a variable it never
names cannot appear in its result. The pooled variant was implemented and measured anyway
(`top_articles` −65.4 % against −61.8 %); the safe one was kept for 4 points of margin.

Measured **interleaved**, 4 pairs, `examples/26` at 60 000 users:

| query | before | after | change |
|---|---|---|---|
| `top_articles` | 2.983–3.004 s | 1.135–1.163 s | **−61.8 %** (2.62×) |
| `friend_degree` | 4.227–4.249 s | 1.695–1.709 s | **−59.9 %** (2.49×) |
| `count_friend` (control) | 1.766–1.831 s | 1.755–1.781 s | **flat** |

The flat control is what rules out a machine-wide drift reading. Allocations on a 2000-node /
8000-edge fixture with two properties per edge fell **21.45 → 10.95 per matched row (−48.9 %)**,
1374.5 → 774.5 B/row.

The change is **result-identical**, so a differential test cannot see it. The pin is therefore
an allocation bound with a limit of 16 allocations per matched row, injection-validated: it
fails at **20.45** allocs/row against the ungated build and passes at **10.45**. (Those two
figures are the pin's own query — `min`/`max`/`count` — and so differ slightly from the
`min`/`max`/`avg`/`percentileCont` shape measured above; they are not the same fixture and
should not be quoted as one.)

### #2371 — the label-index oracle read an ABA sequence as a lost row

`TestLabelIndex_NeverMissesALabelTheBagHas` failed about **1 run in 10**, and its message
describes a **lost row** — a candidate ACID **Consistency** defect of exactly the class the
previous sprint existed to close. It is not one.

Both mutation paths already hold the shard lock across the bag write **and** the index
maintenance (`setNodeLabelInfo`, `removeNodeLabelInfo`), so no within-operation intermediate is
observable on this direction. A writer-epoch probe then measured what the reader's three
separately-locked reads actually span: **22.4 % of windows had two or more writer operations
complete inside them, and the widest held 714.** A removal followed by a re-add restores the
bag to `true`, so all three reads are honest at their own instants while the conjunction
describes a state that existed at none. The file's own argument for excluding false positives
— *"a removal clears the bag too, so the bag is false by the time we look again"* — holds only
for a window containing **at most one** writer step.

**The rate discriminates on its own.** With the #2326 ordering injected, the test fails **7 runs
in 8 within 0.06–0.38 s**. The reported flake was 1 in 10 over 2 s. Two orders of magnitude
apart — a real ordering defect does not hide.

The fix is a writer epoch sampled either side of the three reads. A window in which no
operation *completed* still contains any operation mid-flight, which is exactly the #2326
intermediate, so the guard removes the ABA reading **without weakening detection**: against the
injected ordering the guarded oracle reported 10, 23 and 36 violations in three 10 s runs, every
one with a stable epoch, and it still fails 7 runs in 8 — the same detection rate as before. A
`sampled == 0` fatal keeps the guard from ever making the gate blind; at HEAD it samples
68 000–77 000 single-instant windows per 2 s run, discarding 37–39 %.

**Stability after the fix: 200 consecutive runs, 0 failures**, 100 of them under a deliberate
10-worker CPU load. Before the fix it did not reproduce at HEAD in 55 runs (25 at load ≈ 2, 30
at load ≈ 20) with nothing in `graph/lpg` or `graph/adjlist` changed since it was measured —
which is why the mechanism had to be established structurally rather than by catching one.

### #2375 — the CSV examples capped their own generated input

`go run ./examples/18_oocore_pipeline -nodes 500000 -out-degree 12` died with
`csv: input exceeds maximum size` part-way through its own generated edge list. 18 is the
**out-of-core** example — its subject is data larger than memory — and its CSV stage silently
ceilinged the whole pipeline at 128 MiB.

**The module is not at fault.** `csv.DefaultMaxBytes` is deliberate memory-exhaustion
hardening, documented with its peak-RAM analysis, and configurable through `Options.MaxBytes`.
The defect is that these examples applied the **untrusted-input** default to input they
generated themselves in process — trusted by construction, and whose exact length they had
already computed for their own telemetry. Same shape in `06_csv_import` and
`31_metrics_observability`.

Each site now sizes `MaxBytes` from the known payload. The cap is **raised, never disabled**:
`MaxBytes <= 0` opts out of the bound entirely, and an example is documentation as much as an
exercise. After the change 18 completes at 500 000 nodes / 6 249 637 edges, and 06 round-trips
8 001 046 edges through **396.78 MiB** of CSV — over three times the previous ceiling.

### #2374 — the examples index was three examples short, and nothing could see it

`examples/README.md` claimed **34** runnable examples against an actual **37**, and omitted
`37_mvcc_write_contention` entirely. The examples are the module's exercise instruments, so an
unindexed example is an instrument nobody knows to run.

Nothing could have caught either drift: `scripts/check_doc_freshness.sh` does not look at
`examples/` at all, and no Go test read `examples/README.md`. The gate now lives in
`internal/docscheck`, which already runs under `go test ./...` and therefore under `make ci`,
and asserts that every `examples/NN_*` directory is linked, that each carries its own
`README.md` as the examples rule requires, and that the stated count equals the directory
count. Validated by injection in both directions.

---

## 2. What the exercise proved about the module

### The three examples that do not exit 0 at their bare default

None is a defect, and each was driven to completion by another route:

- `24_social_network_cli` requires a subcommand and `-d` (prints its usage, exit 2). Driven
  through `init`, `seed`, `stats`, `snapshot`, `plandiff` and `query` — all exit 0.
- `25_software_house_api` requires `-d` (prints `error: -d <dir> is required`, exit 2). Driven
  through its full documented flow including SIGTERM and restart; see below.
- `26_social_scale_bench` defaults to **1 000 000 users / ~175 M edges by design**, so it needs
  more than a 180 s budget (~8.8 GiB resident). It is the cycle's profiling instrument and was
  run at 60 000 and 120 000 users.

### Correctness and ACID

`04_persistence` at 20 000 packages, i.e. 20 000 WAL-committed transactions plus one
deliberately aborted one:

| observable | value | property |
|---|---|---|
| `wal.commit_markers` | **20001** — exactly one per committed transaction | Atomicity |
| `wal.phantom_frames` | **0** — the aborted transaction leaked nothing | Atomicity |
| `rollback.applied_after_reopen` | **0** — the rollback did not resurrect | Atomicity + Durability |
| `recovered.snapshot_hit` | `true`, 40 002 nodes / 100 229 edges rebuilt | Durability |

`25_software_house_api` at a scaled seed (3763 nodes / 19 209 edges), across a SIGTERM and a
restart:

| property | before | after restart |
|---|---|---|
| `UNIQUE` on `Component.key` rejects a duplicate | **409** | **409** |
| `EXPLAIN` on the indexed `Developer.key` | `NodeByIndexSeek` | `NodeByIndexSeek` |
| the API's durable `CREATE` present | — | yes |
| node total | 3763 | 3764 — exactly the one write |
| edge total | 19 209 | 19 209 |

`24_social_network_cli` drives every subcommand in a **separate process**, so the
recovery path is exercised on each; after `plandiff`, `stats` in a fresh process reported the
enlarged graph (2005 users / 50 407 `FOLLOWS`), so the writes crossed the process boundary
durably.

The constraint machinery was additionally probed directly, in all three shapes: an
engine write then the declaration rejects the duplicate; declaring over already-violating data
is refused (`pre-existing data contains duplicate value`); and a write that **bypasses** the
engine, followed by the declaration, also rejects the duplicate.

### Concurrency and scale

| Example | Scale | Wall | CPU | Peak RSS | Parallelism |
|---|---|---:|---:|---:|---:|
| **20_concurrent_reads** | 200 k nodes, 16 workers | 280.1 s | **1589.7 s** | 486 MiB | **5.7×** |
| 35_mvcc_mixed_workload | 200 k nodes, 8 readers | 3.6 s | 13.9 s | 35.9 MiB | 3.9× |
| 05_out_of_core | 500 k nodes | 6.8 s | 22.0 s | 623 MiB | 3.2× |
| 19_pattern_query | 300 k nodes | 12.9 s | 38.6 s | 690 MiB | 3.0× |
| 22_cypher | 300 k users | 41.9 s | 121.8 s | 2017 MiB | 2.9× |
| 01_basic | 400 k nodes, **11 653 070 edges** | 11.2 s | 29.0 s | 1217 MiB | 2.6× |
| 10_dimacs9_routing | 400 k v, 1.6 M e, 3000 probes | 58.8 s | 63.2 s | 367 MiB | — |
| 23_bolt_server | 100 k nodes, 16 sessions, 4000 q | 1.9 s | 5.5 s | 253 MiB | — |

`20_concurrent_reads` reaching **5.7× on 10 cores** is direct evidence for the lock-free
immutable-snapshot read contract.

Two throughput figures from `01_basic` at 400 000 junctions and 11 653 070 roads are worth
quoting on their own: the graph builds at **1 094 975 edges/s**, and a single-source Dijkstra
reaching **all 400 000 nodes completes in 192.9 ms — 2 073 907 nodes/s** over a frozen CSR
snapshot that took 143.5 ms to build. Resident heap for that graph is 433.2 MiB.

### Input validation at the boundary

Eight times over the cycle I gave an example something other than what I thought I was giving
it. The eight split into two kinds, and the distinction matters:

**Three were genuinely invalid, and all three were refused at the boundary before any work,
with a message naming the violated constraint:**

- `-links 12` exceeds `-seed-net 5` — *"a new page needs that many distinct existing targets"* (08)
- `-bridges 8` below `communities-1 = 39` — *"to connect every community"* (11)
- `min-initial` below `writers × ops × max-amount` — *"to guarantee no overdraft"* (27)

**Five were valid input I had misread**, so the module correctly did exactly what I asked and
the surprise was mine:

- `-radius 22` is 22× the auto-tuned connectivity threshold, not 22 units (01)
- `-edges` is out-degree per node capped at the earlier-page count, not a total (07)
- `-batch` is a context-check interval, not a commit batch (04)
- `/seed`'s fields are `scale_components…`, not `components…` (25)
- the duplicate key I asserted on was never in the fixture (25)

No combination produced a crash, an unexplained hang, or a silently wrong result.

---

## 3. The open item: #2378, a REPRODUCIBLE Isolation violation

**What was observed.** During a `make ci` run at `4bc32238`:

```
--- FAIL: TestIsolation_CrossSubstructure_EdgeImpliesLabels (39.76s)
    isolation_test.go:92: observed 1 cross-substructure violations
                          (edge/label disagreement inside a pinned SNAPSHOT)
```

**Why it is severity 9.** The test pins a snapshot (`BeginRead` / `ReadAt` / `EndRead`) and
reads the edge and both node labels through it. The writer toggles between two *consistent*
states — `{edge u→v, u:Hot, v:Hot}` and `{none}` — under `ApplyAtomically`. A reader that sees
the edge without the labels has therefore observed a **partial transaction across two
substructures**, which is the defect class the sprint-336 structural finding named: an invariant
binds two substores while each substore is maintained independently.

**Attribution is settled, and it is not this cycle's.**
`git diff --name-only 78c21ed9 HEAD -- 'graph/**/*.go'` returns exactly one path,
`graph/lpg/mvcc_label_index_atomicity_test.go` — a **test** file, which cannot change engine
behaviour. The defect, if it is one, predates sprint 337.

**The oracle was checked before the engine, and it holds up.** This is *not* the shape that
produced the #2371 false positive. There, three separately-locked **present-time** reads could
straddle many completed writer operations. Here `ReadView.HasNodeLabel` resolves through
`HasNodeLabelAsOf(n, name, v.snap)` and the edge through `EdgeWeightAsOf(..., snap)`, and
`snapshotTimes` returns `walk = true` for **any** non-nil snapshot — so both substructures are
resolved at the same `(startTS, txID)`. An ABA straddle cannot explain a disagreement unless the
snapshot itself fails to cover one of the two substructures, which would be the defect.

### It IS reproducible — the missing variable was the gate's own environment

24 attempts across five substitute environments all came back green:

| Attempt | Environment | Result |
|---|---|---|
| 6 runs | the single test in isolation at `78c21ed9` | green, ~1.9 s each |
| 10 runs | the same, under a deliberate 14-worker CPU load | green |
| 3 runs | whole `graph/lpg` package at `78c21ed9`, with `-race` runs of `cypher`, `store` and `search` as peer load | green |
| 1 run | the entry-head `make ci` itself | green |
| 3 runs | full `go test -race ./...` at the exit head | green |

Every one of those **substituted something** for the gate's environment — CPU burners, a subset
of peer packages, injected schedule points. The failing run had taken **39.76 s against ~1.9 s
in isolation**, which said plainly that contention was the variable; what it did not say is that
*the kind* of contention matters. Running the whole suite as peer load while hammering the test
reproduces it:

```bash
# terminal 1 — the gate's own environment: every package binary, in parallel, under -race
go test -race -count=1 ./...
# terminal 2 — 20x the sampling of one gate run, inside that environment
go test -race -count=20 -run TestIsolation_CrossSubstructure_EdgeImpliesLabels ./graph/lpg/
```

**4 of 20 runs failed — about 20 % — with 1–2 violations each**, the failing runs taking 7.4–9.1 s
against ~1.9 s solo. That is a working recipe, and it moves #2378 from "seen once, unexplained"
to "reproducible on demand", which is the difference between an item that can be investigated and
one that can only be waited for.

The lesson generalises past this defect: **five substitutes for an environment all reported green
on a defect that reproduces at 20 % in the environment itself.** Modelling the load was the wrong
move; running the real thing was cheap and worked immediately.

**Two mechanisms were proposed, tested deterministically, and REFUTED.** They are recorded
because a refuted mechanism that is not written down gets re-derived.

1. **The raw adjacency removal.** The failing test's writer is asymmetric: it adds the edge
   through the graph API (`g.AddEdge`, versioned) but removes it through the **raw** adjacency
   list (`g.AdjList().RemoveEdge`). If the raw removal recorded no version, a snapshot taken
   before it would lose the edge at once while still seeing the properly-versioned labels —
   exactly the reported disagreement, and a test bug rather than an engine defect. A
   single-threaded probe (pin a snapshot in state A, transition to state B, re-read both through
   the same snapshot) showed **both** the raw and the versioned removal leave the pinned snapshot
   at `edge=true label=true`. The asymmetry is real and worth tidying; it is not the cause.
2. **Autocommit inside the bracket.** `ApplyAtomically` opens one write bracket and holds
   `visGate.StrongLock`, but the writes *inside* it are the exported **autocommit** methods,
   which pass `tx = nil` and are documented as *"committed the instant it is made"* — so they may
   take separate commit instants, and a reader allocating a `startTS` mid-bracket could land
   between the edge's commit and the labels'. A probe with an **injected schedule point** between
   the two substructure writes showed the reader is **not excluded** — it completes rather than
   blocking on the gate — but it consistently observes the **pre-write state for both**
   substructures, three runs out of three. The writes' visibility is gated together at bracket
   close.

3. **The same, on the add direction.** The schedule point was then injected on the *other*
   transition (add the edge, pause, add the labels), in case the two directions differ. The
   mid-bracket reader again saw a consistent state — `edge=false label=false`, three runs out of
   three.

So a directly-injected mid-bracket read behaves correctly in **both** directions: the snapshot is
not excluded from taking a `startTS`, but it consistently observes the pre-write state for both
substructures.

### The leading candidate, located but NOT yet tested

Reading the two paths side by side turns up an asymmetry that fits every observed property. It is
recorded here **as a hypothesis, not a diagnosis** — three earlier mechanisms that looked at least
as convincing were refuted by measurement, and this one has not been measured.

`Graph.withLabelBag` (`graph/lpg/snapshot_read.go:120`) resolves a node's label bag as of a
snapshot, and its first branch is:

```go
if !walk || sh.d == nil {
	fn(sh.m[id])   // the PRESENT bag — the snapshot is ignored
	return
}
```

`sh.d` is the shard's label **delta map**, and it is set back to `nil` by the reclaim path once
the shard's deltas are all freed — `graph/lpg/mvcc_abort_reclaim.go:203`:

```go
if len(sh.d) == 0 {
	sh.d = nil
}
```

Both take the shard lock, so there is no data race — but there is a **semantic** one. If the
sweep nils `sh.d` while a reader still holds an older snapshot, that reader's next label read
falls into the present-time branch and answers at the *wrong instant*, while the adjacency side
continues to resolve as-of correctly (`EntryViewAsOf` → `adj.EntryViewAsOf(id, startTS, txID)`).
Edge as-of the snapshot against label at present time is precisely the reported disagreement.

It also explains what the three refuted mechanisms could not: the sweep runs on the **vacuum
goroutine**, which is why the failure is load-sensitive and why no directly-injected mid-bracket
read reproduces it — the sweep is not involved in that interleaving at all. And it is the same
defect class the previous sprint named: **a structure the read path depends on, reclaimed
independently of the reader's instant.**

**And here is the counter-argument, found by reading further — it is recorded because a candidate
stated without its objection is a claim, not a hypothesis.** `ReclaimNow` computes
`watermark := g.horizon.Oldest(g.mvccClock.ReadTS())`, a reclamation horizon that accounts for
live readers, so the ordinary sweep should not free a version a held snapshot still needs. And
`sh.d == nil` means the shard has **no versions to walk at all**, in which case the present bag
*is* the correct answer for every snapshot. The remaining gap is narrower than the branch first
suggests: it needs a shard whose delta map is emptied by the **abort** reclaim
(`mvcc_abort_reclaim.go`, which deliberately runs regardless of the debt) at a moment when a live
older snapshot would still have needed one of them.

**Tested, and the objection wins — so this is REFUTED too.** A probe held a snapshot across a
label removal (confirming the as-of read honours it: `heldSnapshotSeesHot=true`, `len(sh.d)=1`),
then churned the shard and called `ReclaimNow()` **40 times**. `sh.d` was **never** emptied while
the snapshot was live, and the held snapshot's answer never changed. Two runs, same result. The
sweep declines to free what a live reader still needs, exactly as `horizon.Oldest` implies.

### Where that leaves it

**Five candidate mechanisms, all excluded by deterministic measurement** — the raw adjacency
removal, autocommit-inside-the-bracket in each direction, and the reclaim nilling the delta map.
The oracle has been checked and holds up. Attribution is settled and pre-existing. **But every one
of those four probes ran OUTSIDE the environment that reproduces the defect**, so each exclusion is
weaker than it looked when it was made and should be re-run under the recipe above.

**Counter-evidence, which does not clear it but belongs beside it.**
`36_mvcc_snapshot_topology` is the example built for precisely this class — snapshot isolation on
the topology dimension, validated to fail on the defective engine and pass on the fixed one — and
at scale (4000 spokes, 8 readers, 12 s, 300 churn) it reported **all four of its invariants at
zero**: `invisible_commits=0`, `future_reads=0`, `misaligned_far_endpoints=0`, `read_errors=0`.
That is a different substructure pair from the failing assertion and does not settle #2378, but it
is the strongest positive evidence this cycle has on the Isolation axis.

### The signature, from the first reproduction with the diagnostic in place

Re-running the recipe with the improved oracle produced the tear immediately, and it is
specific:

```
edge=true label(u)=false label(v)=false
— the edge disagrees with the labels, which agree with each other
1200503 reads were taken
```

Two things follow. **It is not a cross-shard label disagreement** — `u` and `v` agree, so the
label substore is internally consistent at that instant, and the split is squarely between
adjacency and labels. And the *direction* names the transition: state A is
`{edge, u:Hot, v:Hot}` and state B is `{none}`, so `edge=true, labels=false` is a mix in which
the **edge has landed and the labels have not**. The writer's remove path takes the edge down
*first*, which would show `edge=false, labels=true`; only the **add** path — `AddEdge`, then
`SetNodeLabel(u)`, then `SetNodeLabel(v)` — produces this shape. So the working statement is:

> On the add path, within one `ApplyAtomically` bracket, **the edge becomes visible to a
> snapshot before the labels do.**

That is the same suspect the structural finding of the previous sprint named — two substores,
one invariant — and it is consistent with the two sides being stamped through different
machinery: adjacency through `adj.SetWriteStamp(&g.stamp)`, labels through `deltaStamp`.

The window is narrow: **1 violation in 1 200 503 reads** in that run, and 1 of 20 runs failed
against 4 of 20 in the previous batch, so the per-run rate moves with the ambient load. That is
why probe 2378C — which injected a single schedule point on the add path and saw a consistent
state — did not contradict this: one sampling point cannot find a window this narrow, and a
million can.

### The code-level lead the signature points at, and the fifth exclusion

The two substructures order their write and their version record **the opposite way round**, and
it is deliberate on both sides:

- **Labels record the undo BEFORE mutating.** `setNodeLabelInfo` says so in as many words —
  *"Record the undo BEFORE mutating"* — so there is no instant at which the bag has changed and
  no version exists.
- **Adjacency inserts and stamps AFTER.** `addEdgeInfo` calls
  `g.adj.Writer(tx.adjTx()).AddEdge(...)` at `lpg.go:1833` and only then
  `g.adjVer.stampAppend(srcID, tx)` at `:1843`, with the reason written down: *"Stamped AFTER
  the insert, because an append may CREATE its source and that node's id does not exist until
  now. Stamping before the insert skipped every edge-creates-its-endpoint write."*

That asymmetry produces exactly the observed shape — an edge present with no version to hide it
from an older reader, while the labels are still correctly hidden — and the ordering was chosen
for an unrelated reason (`checkAppend`, #2143), so its Isolation consequence looks unconsidered
rather than accepted.

**It is a lead, not a conclusion, and the obvious deterministic test does NOT trigger it.** A
fifth probe took a snapshot *before* the bracket and then performed the whole add: the older
snapshot correctly saw `edge=false label(u)=false label(v)=false`, three runs of three. So the
simple case is clean and the tear needs the concurrent interleaving — consistent with a window
of 1 in 1 200 503 reads. Whoever picks this up should instrument **inside the reproducing
environment**, around `lpg.go:1833–1843`, rather than reaching for another single-shot probe:
that is now five single-shot probes that all came back clean on a defect that reproduces at 20 %.

### One cause found and fixed; the tear SURVIVES it, and eight mechanisms are now excluded

The suspect above was tested, and the result **split the defect in two**.

`Graph.deltaStamp` (`mvcc_txn.go:230`) answers a **nil** transaction record with `g.stamp.Stamp()`
— *a fresh commit instant per write*. The bare exported mutators pass nil, so `AddEdge` and each
`SetNodeLabel` inside one `ApplyAtomically` bracket commit at **three distinct instants**, and a
snapshot landing between them sees a partial set. That is the edge-vs-label signature exactly, and
the module states the requirement itself on `Graph.ApplyInsideLockedTx`: *"the statement must share
the record the enclosing `LockBarrier` opened, or the explicit transaction is not atomically
visible."*

Threading the writes through one `WriteTx` (`ApplyAtomicallyTx` + `Graph.Writer(tx)`) was then
measured under the same recipe:

| Writer form | Runs | Failed | Signature |
|---|---:|---:|---|
| `ApplyAtomically` + bare autocommit writes | 40 | **5** | `edge=true label(u)=false label(v)=false` |
| `ApplyAtomicallyTx` + one shared `WriteTx` | 60 | **3** | `label(u) != label(v)` — **the two labels disagree** |

**The tear SURVIVES the shared transaction.** Across 100 shared-`WriteTx` runs, 4 failed — and in
**both pairings and both directions**: `label(u) != label(v)`, `edge=false label(u)=true
label(v)=true`, and `edge=true label(u)=false label(v)=false`. So threading one transaction removes
the three-separate-instants cause and **something else remains**. #2378 stays open.

**This section was rewritten twice because I twice stated a signature from too few samples.** First
"0 violations in 60 runs", from the first 20 clean runs — refuted by the next 40. Then "the
edge-vs-label tear is gone", from 3 samples that happened to all be label-vs-label — refuted by the
next 40, which produced the edge-vs-label shape again. Both claims had already been written into two
source comments before the refutation arrived; both are corrected. **At a rate of ~4 %, a signature
needs ~100 runs, not 20.**

### What the instrumentation EXCLUDED

The read path was then instrumented to fire at the violation itself, inside the reproducing
environment. At the tear, both nodes had **live delta chains** — `sh.d != nil` and `sh.d[id] != nil`
for u and for v, confirmed in different shards (`sameShard=false`). So two more mechanisms are dead:

- `labelBagAsOfLocked`'s per-node present-time fallback (`d == nil → return cur`) is **not** taken;
- the reclaim is **not** dropping a chain a live snapshot still needs.

That is **eight** mechanisms refuted by measurement, three of them located precisely in the code and
convincing on inspection.

### DIAGNOSED: the visibility basis is re-read per substructure

The obvious next suspect was that the two substores classify visibility through different machinery.
**That is refuted by reading the code**: `AdjList.versionStamp` returns the caller's shared record
when the write carries a valid transaction (`mvcc_adj.go:251`), and `writeCtx.adjTx()` does carry it
(`mvcc_writectx.go:330`). So adjacency and labels point at the **same** `mvcc.CommitInfo`, and both
resolve through `mvcc.Visible` on that record. Nine.

Which leaves what that shared record actually *does*: **`info.ts` is mutable, and it flips.**
`mvcc.Visible` reads `ts == txID → visible (own work)`, `ts < TxIDBase → committed`, else invisible;
at commit, `CommitInfo.Abort`/publish stores a new value into `c.ts`. The reader holds only the
**per-shard** lock, so it consults `info.TS()` **once per substructure, at two different moments** —
and nothing pins that field across them. A commit landing between the two reads therefore classifies
the first substructure as pre-commit and the second as post-commit, from ONE snapshot.

That explains every observation this cycle: both pairings and both directions, a window of ~1 in
10⁶ reads (a single atomic store), load sensitivity, and — crucially — **why sharing the record does
not help**: sharing is precisely what makes both reads consult the same mutable field at two
instants.

### CONFIRMED, by a pre-registered falsification

The prediction was written down **before** the experiment ran (the reader reads edge → u → v, so if
the flip is the mechanism the observed direction must track the read ORDER; swapping the reads must
invert the signature). Then the reader was changed to labels → edge and run 40 times under the
recipe:

| Read order | Observed |
|---|---|
| edge, u, v (original) | `edge=false label(u)=true label(v)=true` |
| u, v, edge (swapped) | `label(u)=false label(v)=false edge=true` |

**The signature inverted exactly as predicted.** And the second failure in the swapped batch —
`edge=true label(u)=false label(v)=true` — shows the same thing happening *between the two label
reads*: u (read first) old, v (read second) new.

**Every observation of this cycle now fits one statement.** The reader straddles a commit, and each
substructure is classified against whatever commit state was visible **at the moment that
substructure was read**. Which side each lands on is decided by read order; whether the pair reads
`false→true` or `true→false` is decided by whether the straddled transition was an add or a remove.
That accounts for all four shapes seen — including the label-vs-label ones, because `u` and `v` are
read sequentially too.

### The root cause, and why it is architectural

`mvcc.Visible(ts, startTS, txID)` is evaluated **per substructure**, against `info.ts` — a field
that is **mutable and flips at commit**. `Snapshot` is `{startTS, slot}`: a **scalar**. It carries no
record of *which transactions were in flight when it was taken*, so it cannot re-derive a stable
answer; it re-asks the live records, and the live records move underneath it.

Sharing one `CommitInfo` across substructures does not help — it is precisely what makes both reads
consult the same mutable field at two different moments. That is why threading a transaction removed
one cause and not the tear.

**This is the same finding as the retracted half of #2369, arrived at from the read side:** a scalar
snapshot is strictly weaker than the reference shape. PostgreSQL's snapshot is `xmin` plus the
**list of in-progress XIDs** (`GetSnapshotData`), and InnoDB's read view is the same shape; both
decide visibility from state captured **once**, at snapshot time. GoGraph decides it per read, from
state that is still moving.

### THE SYNTHESIS, and it retracts one of my own refutations

A further run instrumented the adjacency directly at the tear:

```
edge: reader=false rawPresent=true asOf(startTS)=false asOf(1)=false
observed: edge=false label(u)=false label(v)=true
```

The adjacency **is** filtering correctly here — the present view holds the edge, the snapshot's view
does not. As of this snapshot the state is therefore B = `{no edge, no labels}`, so `label(u)=false` is
**right** and **`label(v)=true` is the wrong value**. In the previous sample the labels were right and
the edge was wrong. **Neither substore is consistently at fault.**

That is the finding, and it means **no single-substore bug explains this**. What every sample does fit,
without exception, is the plain phenomenon: **the reader straddles a commit, and different
substructures land on different sides of it.** The read-order inversion test — pre-registered and
confirmed — said exactly that, and it remains the strongest evidence in this file.

**I must retract the refutation I published against it.** I claimed the straddle account was
disproved because an instrumented sample showed the same commit record and the same timestamp for both
labels. That measurement was taken **at report time, after the reader had finished** — by which point
the record had moved on. It never observed the values the reads actually used, so it could not
contradict the straddle account, and I treated it as though it did. The "localised to the adjacency"
and "single-substore bug" conclusions built on it are withdrawn.

**So the standing conclusion is the architectural one**, and it is the one I had before I talked myself
out of it: `Snapshot` is `{startTS, slot}`, a scalar, and each substructure a reader touches resolves
independently against commit state that is still moving. Nothing ties the reads together. The fix is to
capture the visibility basis **once** — the in-flight set, PostgreSQL's `xmin` + in-progress-XID shape,
InnoDB's read view — which is **option (a) in #2369's technical requirements** and needs the
maintainer's decision.

### ORIGIN: #2378 is coeval with #2344, and #2344's validating measurement was a false negative

Rather than guess an eighteenth mechanism, the question became *when did this start*. The answer needed
no bisect. `git log -S 'BeginRead' -- graph/lpg/isolation_test.go` names one commit: **`5a71cc1c`,
"remove Graph.View, the last pre-MVCC read barrier" (rmp #2344, 2026-08-07)**. Before it, this test's
reader held the barrier, so its three reads were atomic **by construction** and the defect could not be
observed. That commit migrated the reader to a pinned snapshot.

**Its own message records the validation it relied on:**

> *"Measured on this build: 7040 partial-transaction observations from a View reader against **ZERO
> from a snapshot reader over 6488034 reads**."*

Zero. Against 6.5 M reads. And this cycle measures the same assertion tearing at **2–5 per 100 runs**,
roughly one per 1.5 M reads — a rate that 6.5 M reads would have caught several times over **if the
measurement had been taken in an environment where it reproduces.** It almost certainly was not: this
defect required the gate's full parallel-package `-race` load, and five substitute environments
returned clean before I found that recipe.

So the most probable account, and the one the next cycle should test first: **`Graph.View` was removed
on the strength of a snapshot-reader measurement that could not see this defect**, the migrated tests
went green, and the gap has been open since 2026-08-07. The check is cheap and decisive — run the
recipe at `5a71cc1c` and at its parent.

**That is the same error this cycle made five times**, and it is why the recipe matters more than any
of the seventeen mechanisms: a clean number from the wrong environment is not evidence.

### The blast radius: it REACHES the engine's production write path

The last open question was scope. The failing test writes through `ApplyAtomically` — the **exclusive
bare-Go-API bracket**. The Cypher engine, which is the module's production surface, writes through
`ApplyVersioned` (shared bracket, per-object latches). Those publish differently, and I had been
treating #2378 as module-wide without ever measuring whether it reaches the path real callers use.

Measured, by switching only the writer's bracket and running the recipe at 100 iterations:

| writer bracket | runs | failures |
|---|---:|---:|
| `ApplyAtomically` (bare API, exclusive) | 100 | 2–4 |
| **`ApplyVersioned` (the ENGINE's bracket)** | 100 | **5** |

**It reaches production, at the same rate or slightly worse.** So there is **no envelope to retreat
to**: this cannot be certified as "safe through Cypher, defective only on the bare API". The unqualified
NOT CERTIFIED verdict stands, and it stands on a measurement rather than on caution.

Note this does *not* contradict `36_mvcc_snapshot_topology` / `27` / `35` reporting zero violations at
scale: those examples assert different invariants — topology-dimension snapshot isolation and property
conservation — not this cross-substructure edge-plus-labels pair. Their green results remain valid
evidence for what they cover, and are not evidence about this.

### The fix, designed concretely — a per-snapshot memo, not a Clock change

Option (a) is usually written as "capture the in-flight set at `BeginRead`", the PostgreSQL shape.
There is a strictly smaller form that gives the same guarantee and touches neither the `Clock` nor
`BeginRead`: **memoise the visibility verdict per commit record, per snapshot.**

`Snapshot` gains a `map[*mvcc.CommitInfo]bool` (guarded, or a `sync.Map` if a snapshot may be shared
across goroutines). The first time a snapshot classifies a record it stores the verdict; every later
read reuses it. Threading is the work: `mustUndo(startTS, txID)` and the adjacency's
`Visible(v.supersededAt(), …)` both need the snapshot, not just the two scalars.

**Why it is exactly equivalent to capturing the in-flight set**, case by case:

- record **in flight** at first classification → invisible, and *pinned* invisible for this snapshot's
  lifetime. That is precisely PostgreSQL's rule for a transaction in the in-progress list.
- record **committed at `TS ≤ startTS`** → visible, and `ts` is immutable once committed, so the memo
  changes nothing.
- record **committed at `TS > startTS`** → invisible, likewise immutable.

So the memo alters exactly one case — the in-flight one — and pins it to the answer the snapshot
should already have been giving. Note also that "first classification" need not equal "state at
`BeginRead`": a record that commits in between can only have `TS_c > startTS`, because the contiguous
frontier does not advance past an unfinished commit. So the lazy form is safe without enumerating
anything at snapshot time.

**Validation this fix must pass**, written down before it was run: zero failures in ≥100 runs under
the recipe against a pre-fix 1–4 per 100; TCK 3897/3897; `make ci` green three consecutive times; and
no read-path regression.

### IMPLEMENTED, MEASURED, REFUTED — REVERTED

It was built. `Snapshot` gained `verdict map[*commitInfo]bool` with a mutex; the label path resolved
every delta through it; and `AdjList.EntryViewAsOfVisible(id, visible)` was added so the adjacency
could take the same verdict as a callback without `adjlist` depending on `lpg.Snapshot`.

| arm | runs | failures |
|---|---:|---:|
| pre-fix | — | 1–4 per 100 |
| **label path only** | 100 | **0** |
| **both stores wired** | 100 | **2** |

**The label-only zero was luck, and the pre-stated criterion caught it.** At a ~3 % rate, `P(0 in 100)
≈ 5 %` — so a single clean batch proves nothing, which is exactly why "zero in 100 **with both wired**"
was written down first. Wiring the second store did not improve on it; it landed back inside the
pre-fix range. **The memo is not the mechanism.** Reverted, because keeping it would add a mutex and a
map lookup to the module's fastest path in exchange for nothing.

Note the shape of the near-miss: had the criterion been "zero in 100 on the arm I happened to run
first", this cycle would have shipped a read-path slowdown, declared the defect fixed, and closed the
certification on a false positive. **Sixteen candidates, sixteen refutations.**

Thirteen candidate *mechanisms* were refuted; what survives is the *phenomenon*, measured and
pre-registered. **The lesson for whoever continues: instrument the values the READ used, at the moment
it used them. Every instrumented sample in this file that was taken after the fact misled me,
including the one I used to overturn a correct conclusion.**

### (withdrawn) LOCALISED: the LABEL substore is correct; the ADJACENCY as-of read is not

The decisive measurement. At the tear, for each node, the **reconstructed** value, the **raw stored
bag**, and whether a delta chain exists:

```
reconstructed: u=true v=true | RAW bag: u=false v=false | chain present: u=true v=true
observed:      edge=false label(u)=true label(v)=true
```

Read that carefully, because it exonerates the half of the system I had been investigating for hours.
As of this snapshot the state should be A = `{edge, u:Hot, v:Hot}`. The raw bags say `false` — the
writer's removal has landed in the stored bags. **Both label chains correctly undid it and
reconstructed `true`.** The label substore is doing exactly the right thing, for both nodes, at the
moment of the tear.

**The edge read `false`.** The adjacency as-of reconstruction failed to undo the same transaction's
edge removal that the label chains undid correctly — same transaction, same shared commit record.

So this is **not** a cross-substructure architecture problem, and the framing I committed to earlier
is retired: `Snapshot` does not need to capture an in-flight set for *this* defect, and #2369's option
choice is **not** a prerequisite. It is a **single-substore bug in the adjacency versioned read path**
— `AdjList.EntryViewAsOf` and the version chain that `storeEntry`/`versionStamp` maintain, reached
from `Graph.EdgeWeightAsOf`. That is a bounded target.

**Candidate eleven is also dead.** It predicted a *skipped* delta; the chains are present for both
nodes and they worked. And with a single writer toggling, every transition genuinely changes the bag,
so the `!bag.has(lid)` guard never skips here anyway — which I said before reading the result rather
than after.

**The leading candidate, and it fits the measurement without strain.** `entryAsOfLoaded`
(`graph/adjlist/mvcc_adj.go:163`) opens with:

```go
if a.versionActive.Load() == 0 || e == nil {
    return e   // the PRESENT entry — no as-of filtering at all
}
```

`versionActive` is a **single global counter** of live version records. The label side has the same
shape of fast path, but keyed **per shard and per node** (`sh.d == nil`, `sh.d[id] == nil`) — and the
measurement confirmed both of those chains were **present** at the tear, which is why the labels
reconstructed correctly. The adjacency's gate is global, so it can read 0 for reasons that have
nothing to do with the edge being read: the reclaimer freeing the last version elsewhere is enough,
and the reader then silently takes the present entry.

That produces exactly what was measured — labels correct, edge at present time (`false`, the removed
state) — and it explains the rarity, the load sensitivity (the reclaimer runs on the vacuum
goroutine) and why threading a transaction changed nothing.

**IMPLEMENTED, MEASURED, AND REFUTED — REVERTED.** The disjunct was removed (leaving `if e == nil`)
and the recipe run at **100 iterations**: **3 failures**, against a pre-fix rate of 2–4 per 100. That
is no change. The criteria were written down *before* the run — zero failures in 100, then TCK and
three green gates — so this is a clean refutation rather than a judgement call, and the change was
reverted rather than kept, because shipping a change that fixed nothing would leave a comment
asserting a cause the measurement had already denied.

Worth keeping from the attempt: dropping the counter is **semantically neutral** (with no versions on
the entry, `e.ver.Load()` is nil and the loop breaks immediately), so the gate is *not* what serves
the present-time answer here. Something else in the adjacency as-of path does.

**That is thirteen candidates refuted.** What survives, and it came from measurement rather than from
any of them, is the **localisation**: at the tear the label chains are present and reconstruct
correctly while the adjacency returns the removed state. The defect is in the adjacency versioned read
path — `AdjList.entryAsOfLoaded` / `EntryViewAsOf` and the chain `storeEntry`/`versionStamp` maintain
— and it is *not* the `versionActive` gate. `removeEdgeInfo` having no `stampAppend` counterpart to
`addEdgeInfo`'s (`lpg.go:1843`) is the next thing to check, and the honest next step is to instrument
the adjacency chain itself at the tear — the entry, its version, and `supersededAt()` — exactly as
instrumenting the label bag localised this far.

### A superseded story, kept for the record

The two head deltas were captured at the moment of the tear, over 60 further runs (2 failures):

```
startTS=35199 clockReadTS=35221 | u: stampTS=35221 | v: stampTS=35221 | SAME info pointer=true
startTS=18395 clockReadTS=18413 | u: stampTS=9223372036854812630 | v: same | SAME info pointer=true
```

**Identical record, identical timestamp — and the two labels still disagreed** (`label(u)=true
label(v)=false`). That is a hard fact and it refutes the read-order/flip story as the *whole*
explanation: if both nodes' deltas share one record and one timestamp, `mustUndo` **cannot** classify
them differently. Whatever the flip does, it is not what separated these two reads.

What remains is forced rather than suggested: the delta classification was the same for both, so the
difference came from each node's **stored bag**, and **the undo chain did not correct for it**. An undo
chain can only roll back a transition it recorded — and `setNodeLabelInfo` records one **only when
`!bag.has(lid)`**, a guard read against the RAW stored bag, with the file's own comment noting *"Only
the DELTA is guarded; the conflict test above is not (rmp #2354)."* A write whose delta is skipped
leaves that node with no history for the transition, so a reader that should reconstruct the old value
gets the new bag instead.

**That is candidate eleven, and unlike the ten before it, it is forced by a measurement rather than
suggested by reading.** It is also narrow and does not obviously need an architecture change — which
would make the #2369 option choice unnecessary if it holds. It is **not implemented and not
validated**: the guard sits on the hot write path, the pre-fix rate is 2–4 per 100, and a change here
needs ≥100 runs per arm plus the full gate before anyone believes it.

**Note also what this does NOT explain**: the second sample's `stampTS` is above `TxIDBase`, i.e. the
record was in flight at report time, and the reader had already been given a torn view. Anyone
continuing should reconcile that before treating the missing-delta account as complete.

**A superseded link in the earlier story, kept for the record.** The *experimental* result is solid
and was pre-registered: the answer a reader gets changes between its two substructure reads, and the
direction tracks read order. What is **not** established is *why the contiguous frontier permits it*.
`startTS` is `Clock.ReadTS()`, the contiguous frontier, which only advances past a commit once that
commit has finished — so a transaction in flight when the snapshot was taken should have
`TS_c > startTS`, and committing later should leave it invisible. On that reasoning the tear should be
impossible, and it demonstrably is not. Either the frontier is not as conservative as its contract
says, or the version being read is not stamped by the record I traced. **Anyone continuing this must
close that gap first**; the read-order evidence stands on its own, but the mechanism above is a story
with a hole in it, and ten stories have already died this cycle.

**Either way the fix is an architecture change and needs the maintainer's agreement** (per the
project's decision-autonomy rule): `Snapshot` must capture the visibility basis once — the in-flight
set, or an equivalent — so that every substructure a reader touches is resolved against one
observation. That is option (a) already written up in #2369's technical requirements, and it is not a
tuning change.
Run it under the recipe, with ~100 runs, before designing anything. The recipe above gives ~20 % per
run, which is a workable base rate — the next cycle can iterate on a hypothesis in minutes rather
than waiting for the gate to trip. What is *not* known is the mechanism: four candidates are
excluded, and the deterministic probes that excluded them all ran outside the environment that
actually reproduces it, so they should be re-run inside it before being treated as settled.

The assertion must not be relaxed until the engine is excluded. Two concrete next steps are
recorded on rmp #2378: re-run the four exclusion probes under the peer-load environment, and use
the new diagnostic to establish **which pair** tears — the condition trips on `lu != lv` as well
as on `e != lu`, and those point at different suspects.

## 4. Performance envelope, measured

| Dimension | Measured | Note |
|---|---|---|
| Durable single-writer commit rate | **250 tx/s** | one fsync per commit by definition; matches the recorded 263 tx/s baseline |
| Durable commit at 200 k transactions | 300 s wall / **10.6 s CPU** | 97 % idle — the path is fsync-bound, not CPU-bound |
| WAL + snapshot on disk | 34.84 MiB WAL + 12.49 MiB snapshot for 20 k tx | 495.2 disk bytes per edge |
| Read parallelism | **5.7×** on 10 cores | 16 workers over an immutable snapshot |
| Grouped aggregation over a relationship pattern | **2.5–2.6× faster** this cycle | #2376 |
| CSV parse / serialise | 203.10 MiB/s serialise, 4.1 M rows/s | 8 M edges, 396.78 MiB |

---

## 5. What `pprof` says is left

The CPU profile of `examples/26` after #2376 still puts `populateRowCtx` at the top of the
Cypher read path. The remaining costs there are `upgradeNodeIDToValue` (22.4 % of
`populateRowCtx`) and the map traffic of the row context itself (`mapaccess2_faststr`,
`mapassign_faststr`, `mapIterStart`/`Next` — together another 22 %). A row context keyed by
string in a `map` is a per-row hash-and-probe cost for a schema that is fixed at plan time; a
slice indexed by a plan-assigned column would remove it. **Not attempted this cycle** — it
changes a structure the whole expression evaluator reads, which is an architecture decision for
the maintainer, not a tuning one.

---

## 6. The envelope

**The limit the envelope cannot absorb, and the reason the verdict is not unqualified: a
cross-substructure ACID Isolation violation has been observed once and is not explained** —
#2378, §3. Until it is, a deployment whose correctness depends on a reader never observing a
partial transaction across two substructures (adjacency plus labels) carries an unquantified
risk. It **reproduces at about 20 % per run** under the recipe in §3, it is pre-existing rather
than introduced by this cycle, and the oracle has been checked and holds up. Four candidate
mechanisms were excluded by measurement — but all four probes ran outside the environment that
reproduces it, so those exclusions need re-running before they can be relied on.

The remaining limits are ordinary envelope items, each measured rather than assumed:

1. **Durable writes are fsync-bound at ~250 tx/s per writer.** Group commit amortises the
   fsync across *concurrent* committers (the recorded ladder reaches 78 667 commits/s at 1024
   writers through the store API), but a single writer pays one whole fsync per commit by
   definition. **A workload that issues many small durable transactions from one goroutine
   should batch them into fewer, larger transactions.**
2. **Concurrent Cypher write throughput plateaus at about 2× by four writers.** This is
   recorded in `docs/mvcc-contention-findings.md` and
   `docs/benchmarks/mvcc-contention-arms-2026-08-08.md`; the only genuinely serialising
   structure identified is `pubMu`, whose lock-free fast path landed as #2362, with the
   batching follow-up deferred as #2370. Reads are unaffected.
3. **Node memory is 378–423 bytes per node, against Neo4j's 128 and Memgraph's 204** — 3–3.3×
   worse than the best incumbent, while edges are best-in-class at 8.71 B/edge. This is a
   **known, analysed, unimplemented** finding: `docs/design-node-memory.md` records the
   measurement and concludes the in-memory model must *split* so nodes stop paying an
   edge-shaped cost. That is a representation change and therefore requires the maintainer's
   agreement; this cycle did not attempt it. The sweep points the same way without
   corroborating the number, which is a different measurement: `02_property_graph` reports
   **692.2 bytes per node by its own accounting** at 400 000 persons, and 615 MiB resident for
   the 420 000-node graph. Treat 378–423 as the head-to-head figure and 692.2 as this example's
   own; they are not the same metric and should not be quoted as agreeing.
4. **Interchange readers cap untrusted input at 128 MiB by default** (`csv`, `graphml`,
   `jsonl`). This is intended hardening, not a limit to work around: raise
   `Options.MaxBytes` for input whose provenance and size you know, as the examples now
   demonstrate. Leave the default for anything you did not write.
5. **Exact algorithms remain exact.** Brandes betweenness is O(V·E) and Yen's k-shortest paths
   is expensive by construction; at 40 000 and 120 000 nodes respectively they exceed a
   five-minute budget. That is the algorithm, not the implementation — but it bounds what
   `03_advanced_algorithms` and `14_routing_alternatives` can be scaled to.
6. **`cypher/tck` runs 61.5 s under `-race`**, marginally over the documented 60 s per-package
   short-layer budget.

Not exercised this cycle, and therefore not certified by it: the **soak and nightly layers**,
and a fresh **hostile/security audit** (last covered by the 2026-07-26 and 2026-07-31 cycles).

**Which tools this cycle actually used**, so the evidence is not overstated: `runtime/pprof`
CPU and heap profiles (via the `-profile-dir` flag that only `26_social_scale_bench` and
`37_mvcc_write_contention` expose), `rusage` peak RSS per example, each example's own
`runtime.MemStats` telemetry, on-disk WAL and snapshot sizes, coverage through `make ci`, and
interleaved A/B timing with a flat control. **Not used:** `runtime/trace` and `go tool trace`
(`37` exposes a `-trace` flag that went unexercised), `GODEBUG=gctrace=1`, per-example coverage
attribution via `go build -cover` + `GOCOVERDIR`, and rendered flame graphs — the profiles were
read as `-top`/`-peek` tables instead. **Only 2 of the 37 examples can produce a profile at
all**; the other 35 have no profiling flag, so persistence, recovery, traversal, search,
interchange and Bolt are all unattributable without a throwaway benchmark. That matters
concretely: #2376, the largest performance finding of this cycle, was findable *only* because
`26` happened to expose the flag. Filed as **#2377**.

---

## 7. Gate evidence

Exit status was read from the recorded `MAKE_CI_EXIT` line inside each log, never from a
wrapper's status — a pipeline's exit code is the last stage's, which has misreported a red gate
as green on this project before.

| Run | Result | Cause |
|---|---|---|
| Entry baseline (`78c21ed9`) | `MAKE_CI_EXIT=0`, coverage 87.0 %, TCK 3897/3897 | — |
| Final run 1 | **`MAKE_CI_EXIT=2` — RED** | `internal/cypherdocgate`: **this document** published Cypher without being classified. A defect this cycle introduced; fixed |
| Final run 2 | **`MAKE_CI_EXIT=2` — RED** | `graph/lpg`: **#2378**, the Isolation violation in §3. Pre-existing, unexplained, now reproducible |
| Final run 3 | **`MAKE_CI_EXIT=0` — GREEN** | lint 0 issues, coverage aggregate **87.0 %**, every package ≥ 75 % |
| Final run 4 | **`MAKE_CI_EXIT=0` — GREEN** | after the #2378 call-site fix and the contract corrections; coverage **87.0 %** |
| `make test-crashinject` | `CRASHINJECT_EXIT=0` | — |

**Both notifications reported "exit code 0" over a red gate**, because the shell's status was
that of the trailing `echo`, not of `make`. Only the `MAKE_CI_EXIT` line written *inside* the log
is trustworthy — this is the third cycle on which that has mattered.

**Two red runs, two different causes, and the gate was right both times.** The first found an
omission in this very document within minutes of it being written. The second found the item that
withholds the certification. A gate that had been quietly passing would have shipped both.

**The gate is therefore INTERMITTENT, and that is its own cost.** #2378 reproduces at about 20 %
per run of the isolation test under peer load, and `make ci` tripped on it in 1 of the 5 runs this
cycle — so a green `make ci` is evidence but not proof, and every task whose acceptance criteria
end in "make ci green" is standing on a gate that fails roughly one time in four for a reason
unrelated to the change under test. Until #2378 is closed, read a single green run accordingly,
and re-run before concluding a change is at fault.

**The gate caught a defect this cycle introduced, and it was in this document.**
`internal/cypherdocgate`'s `TestEveryDocWithCypherIsClassified` requires every document under
`docs/` that publishes a ` ```cypher ` fence to be either *gated* — its statements executed —
or recorded as *historical*, with the reason it must not be. §1 quotes two aggregation shapes
to name what the profile attributed, so it belongs in `historicalDocs`: the queries are
verbatim from `examples/26_social_scale_bench` and are exercised there, and the figures here are
timings of that example at a specific commit, which executing the snippets would not assert.
The document is now classified with that reason.

That is worth recording for two reasons. The gate is not decorative — it found a real omission
within minutes of the document being written. And the harness's completion notification claimed
**"exit code 0" over a red gate**, because the shell's status was that of the trailing `echo`,
not of `make`. Only the `MAKE_CI_EXIT` line written *inside* the log is trustworthy.

`cypher/tck` ran 65.9 s under `-race` in the red run and is the package named in the §6
envelope note about the 60 s per-package short-layer budget.

---

## 8. Commits

| Commit | Task | What |
|---|---|---|
| `883b1163` | #2374 | index all 37 examples; gate the index in `internal/docscheck` |
| `a0e5a02a` | #2371 | writer epoch stops the label-index oracle reading an ABA sequence as a lost row |
| `1829420b` | #2375 | size the CSV byte cap to the payload the example generated |
| `4bc32238` | #2376 | demand-gate the aggregation pre-projection (−61.8 % / −59.9 %) |
| `49860f65` | #2378 | this document; classify it in `internal/cypherdocgate`; add the `Document` label to `knowledge-model.md` |
| `d03e8d8f` | #2378 | third refuted mechanism (the schedule point on the add direction) |
| `0e2c873f` | #2378 | the fourth candidate, recorded with its counter-argument |
| `63d83db1` | #2378 | the fourth candidate tested and refuted; the count corrected to 24 |

Filed and left open for the next cycle: **#2378** (the Isolation observation, §3) and **#2377**
(only 2 of 37 examples can produce a profile, §5).
