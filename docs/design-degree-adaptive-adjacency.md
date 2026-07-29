# Degree-adaptive neighbour ordering — design and calibration

Deliverable of rmp #2139 (SPIKE), sprint 313. Records the empirical calibration
of the adjacency-probe crossover, the representation decision, the maintenance
strategy, the parallel-edge contract, the durability and recovery analysis, the
verdict on openCypher-observable ordering, and the write-path neutrality budget
the implementation must meet.

No production code was written for this task.

Hardware and toolchain for every measurement below: Apple M4 (arm64, 10 cores,
P-core L1d 128 KiB, L2 16 MiB, 128-byte cache lines), Go 1.26.5, darwin/arm64,
single-threaded, median of 5 runs at `-benchtime=0.3s`.

---

## 1. Summary

The sprint's motivating measurement is wrong, and correcting it changes the
conclusion. Three independent lines of evidence refute
`docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md` §2.4:

1. **Direct measurement.** A linear scan of `[]graph.NodeID` costs
   **0.268–0.348 ns/element**. §2.4's 164 ns at degree 4096 implies
   0.040 ns/element.
2. **A floor argument.** A branch-free, 4-way-unrolled, dependency-broken
   accumulate over `uint64` — the fastest a Go scan can be, with no early exit
   and no data-dependent branch — measures **0.164 ns/element**. §2.4's figure is
   **4.1× faster than that floor**. Go does not auto-vectorise a `range` loop
   carrying a data-dependent comparison.
3. **Internal inconsistency in §2.4 itself.** Its own per-element cost *falls*
   from 0.0824 ns (d=8) to 0.0400 ns (d=512) and then plateaus. A longer scan
   cannot get cheaper per element; cache and TLB pressure only push the other
   way.

With the probe measured honestly — a 64 MiB arena (4× the L2), an unpredictable
key stream, in-range miss keys, and the harness floor subtracted — the results
are:

| Claim in §2.4 | Measured here |
|---|---|
| Crossover at degree ≈ 64 | **Crossover at degree ≈ 16** |
| Linear wins 2.8× at degree 8 | Linear wins **1.7×** at degree 8 |
| Binary wins 30.9× at degree 4096 | Binary wins **6.0×** (hit) / **10.9×** (miss) |

The direction of §2.4's conclusion survives — ordering does help hubs — but the
magnitude does not, and the magnitude is what justified accepting storage-layer
risk.

**Decision: order the CSR snapshot build only. Do not order the adjacency.**

Ordering the CSR (#2141) captures three of the four downstream wins at zero
write-path cost, because all three read-path probes traverse the CSR snapshot,
not the adjacency. Ordering the adjacency (#2140) buys exactly one win —
`AdjList.HasEdge` — and it is the one that sits on the commit path. It is
blocked by three independent obstacles, two of them structural rather than
budgetary (§5).

---

## 2. Calibrated crossover threshold

### 2.1 Method

`BenchmarkHonest*` measures a membership probe over a flat `[]graph.NodeID`
arena laid out exactly as a CSR is — one contiguous allocation, per-source runs
addressed by computed offset. Corrections over a naive harness, each of which
was found to matter:

- **Unpredictable keys.** A register-only LCG picks both the hub and the probe
  key every iteration. A rotating set of 64 keys gives a branch predictor only
  64 distinct decision paths to memorise, and TAGE learns them: the same binary
  probe measures 11.4 ns with 64 rotating keys and 139 ns with an LCG at
  d=4096, a 12× difference that is pure predictor training.
- **In-range miss keys.** Every stored value is even and a miss probes
  `value+1`, so a miss walks a full-depth interior path. An out-of-range miss
  key (e.g. `1<<62`) makes binary search take one fixed rightward path,
  perfectly predicted — the tell being that a miss then measures *cheaper* than
  a hit, which is impossible for real input.
- **Arena larger than the last-level cache.** A constant 64 MiB regardless of
  degree. A 4 MiB arena fits inside the M4's 16 MiB L2, so a "cold" regime built
  that way measures the same thing as a hot one.
- **Constant working-set size.** Sizing the arena as `hubs × d` with a fixed hub
  count makes memory pressure vary with the independent variable.
- **A control arm.** Offset arithmetic plus one load measures **7.1 ns** on this
  machine. Attributing that to the probe inflates every low-degree result;
  all figures below have it subtracted.
- **No modulo on the address path.** Hub selection is a mask, not `i % n`, which
  would put a ~8-cycle UDIV in the dependency chain.

`TestHonestProbesAgree` asserts all probes are functionally equivalent, for
every degree and every slot, including the absence of phantom hits on `value+1`.

### 2.2 Result

Net nanoseconds per probe, control subtracted:

| degree | linear (hit) | binary (hit) | ratio | linear (miss) | binary (miss) | ratio |
|---:|---:|---:|---:|---:|---:|---:|
| 4 | 2.99 | 7.39 | 0.40× | 4.66 | 6.55 | 0.71× |
| 8 | 6.59 | 11.36 | 0.58× | 10.18 | 12.41 | 0.82× |
| **16** | **22.70** | **22.50** | **1.01×** | **22.57** | **22.36** | **1.01×** |
| 32 | 47.84 | 32.89 | 1.45× | 49.45 | 32.92 | 1.50× |
| 48 | 62.95 | 39.53 | 1.59× | 67.06 | 39.23 | 1.71× |
| 64 | 86.52 | 42.47 | 2.04× | 94.08 | 43.16 | 2.18× |
| 128 | 108.73 | 52.48 | 2.07× | 149.53 | 52.58 | 2.84× |
| 256 | 154.98 | 68.54 | 2.26× | 230.28 | 69.12 | 3.33× |
| 512 | 225.99 | 87.83 | 2.57× | 352.49 | 87.96 | 4.01× |
| 1024 | 337.12 | 101.12 | 3.33× | 534.72 | 101.92 | 5.25× |
| 4096 | 797.27 | 132.07 | 6.04× | 1423.87 | 131.07 | 10.86× |
| 16384 | 2418.93 | 171.53 | 14.10× | 4656.93 | 172.93 | 26.93× |

Cost models fitted to the data:

- **Linear** — `0.268 ns/element` asymptotically (0.2678 at d=4096 and 0.2679 at
  d=16384 in an L2-resident arena; 0.348 in the 64 MiB arena). Go emits no
  unrolling for this loop and the loop-carried index increment caps throughput
  at about one element per cycle regardless of core width.
- **Binary** — `7–12 ns per level` in a cold arena. Each level is a dependent
  load whose address depends on the previous comparison, so no prefetch is
  possible and the levels serialise. This is why binary search is *far* more
  expensive here than in an L1-resident microbenchmark, where the same probe
  costs 1–4 ns/level.

**Calibrated threshold: T = 16.** Parity is 1.01× at d=16 in both the hit and
the miss case. A branchless (conditional-move) binary search was also measured
and is not better in this regime: 13.19 ns vs 14.60 ns at d=4, converging to
parity by d=32. In a memory-bound search the branch predictor acts as a
prefetcher — a mispredicted but speculated load starts hundreds of cycles early
— so removing the branch removes the speculation and lengthens the chain.

### 2.3 Hysteresis

**Rule: promote at T, never demote.** Justification:

- Demotion has no benefit. An ordered entry costs nothing to keep ordered,
  because the append fast path is indifferent to whether the existing prefix is
  ordered.
- Any demotion is itself a copy-on-write republication, so a vertex oscillating
  around T would allocate on every crossing — precisely the thrash to avoid.
- It matches the shipped small-set index tier, which already promotes to a
  roaring bitmap and never demotes, for the same reason.

**However — and this is a finding, not a detail — a *history-dependent*
promotion rule is incompatible with recovery determinism.** See §6.3. The
ordering predicate must be a pure function of an entry's final content, never of
how the entry was built. "Promote at T, never demote" is history-dependent by
construction. This is one of the three reasons §5 rejects adjacency ordering,
and it is the reason the CSR decision in §4 is **unconditional** rather than
degree-adaptive.

### 2.4 Leverage — what a threshold actually buys

A threshold's value is not the fraction of vertices it captures but the fraction
of total linear-scan *cost*, which is the share of `Σd²` above the threshold —
the right weight when one probe is issued per traversed edge. Measured on the
repository's own generators (`internal/shapegen`):

| fixture | avg out | max out | metric | T=16 | T=32 | T=64 | T=128 |
|---|---:|---:|---|---:|---:|---:|---:|
| Barabási–Albert n=100k m=8 | 15.98 | 1612 | vertexFrac | 23.49% | 6.47% | 1.66% | 0.44% |
| | | | edgeFrac | 49.97% | 26.52% | 13.48% | 6.92% |
| | | | **costFrac** | **89.21%** | 78.70% | 67.18% | 55.71% |
| Barabási–Albert n=50k m=4 | 7.97 | 679 | **costFrac** | **80.46%** | 69.33% | 57.69% | 46.18% |
| RMAT scale=16 ef=16 | 14.58 | 6215 | **costFrac** | **99.68%** | 99.47% | 97.78% | 93.41% |

Two consequences:

1. **T=16 captures 80–89% of scan cost on the Barabási–Albert fixtures; T=64
   captures only 58–67%.** §2.4's threshold of 64 leaves roughly a third of the
   available cost in the band it recommended keeping linear.
2. **RMAT overstates the win.** At T=64 it reports 97.78% against Barabási–
   Albert's 67.18%. A benchmark run only on RMAT will look far better than a
   real workload — see §8.

**Real-graph caveat.** The `graph-theory-expert` consultation measured the two
real SNAP datasets this repository already wires up (`internal/shapegen/snap.go`)
and reported max out-degree **411** (cit-HepPh) and **456** (web-Google), with
costFrac at T=64 of **34.1%** and **9.7%** respectively. Those figures are
recorded here as sourced from that consultation and were **not independently
reproduced** — the datasets are download-on-demand and are not cached in the
working tree. The synthetic corroboration is exact where it can be checked
(that consultation reported costFrac@64 = 97.8% for RMAT scale=16 ef=16, against
97.78% measured here), so the methodology is sound, but the real-dataset numbers
should be reproduced before being quoted as project fact. If they hold, the
operative degree for a real property graph is ~450, where the measured win is
**2.5–4×**, and the d=4096 row of §2.2 describes a graph shape that does not
occur outside RMAT.

---

## 3. Representation decision

Candidates, ranked, against the actual constraint set:

| # | Representation | Probe | Extra bytes/edge | Verdict |
|---|---|---|---|---|
| 1 | **Sorted run + binary search** | O(log d) | **0** | **Chosen** (for the CSR) |
| 2 | Sorted prefix + unsorted tail of K | O(log d + K) | 0 | Rejected — §5.2 |
| 3 | Per-entry fingerprint byte column | O(d/8) word-parallel | +1 | Rejected — a fifth parallel column allocated and grown on the append path; answers membership but not *position*, which every caller needs |
| 4 | Eytzinger / blocked layout | O(log d), prefetchable | +12 (slot map) | Rejected — wins only past L2, which real degrees never reach; destroys the parallel-column alignment |
| 5 | Auxiliary hash set | O(1) | **+36** | Rejected — measured below |

**The hash set is rejected on measured memory, not on speed.** A
`map[graph.NodeID]struct{}` per hub costs **34.7–36.2 bytes per edge** of extra
resident memory (1757 B at d=64; 147 283 B at d=4096; independently reproduced
by two consultations within 0.4%), against **0** for permuting a slice that
already exists. Pre-sizing does not help. On a project where an edge-label map
was previously measured at ~57% of resident heap — the finding that created the
`labels` column in the first place — a 36 B/edge structure on the hottest
vertices is not admissible. It also answers only "is it present", while
`lookupFwdEdgePos` and `EdgeLabelsAt` need the *position*.

**Prior art.** Neither incumbent LPG engine destination-sorts adjacency. Neo4j's
degree-adaptive dense-node split groups relationships by **type and direction**,
which buys type-filtered traversal — a different win from O(log d) membership.
Memgraph keeps `out_edges` unsorted. SuiteSparse:GraphBLAS carries an explicit
`jumbled` state and sorts lazily at a bulk boundary rather than maintaining
order on insert; RocksDB and LevelDB establish SST ordering at flush, not per
write. The convergent lesson — ordering belongs at a build boundary for this
class of structure — is what §4 adopts.

---

## 4. Chosen design: unconditional stable ordering in the CSR build

Order every source's neighbour run inside the CSR build. **Unconditionally, not
adaptively.**

Dropping the degree threshold is deliberate and it simplifies three things at
once: no per-source "is sorted" bit, no branch on that bit at every probe site,
no correctness obligation that every probe site consult it, and no hysteresis
rule to be incompatible with recovery (§2.3, §6.3). A typical run of degree
6–16 is a small insertion sort; the cost is measured in §7.

### 4.1 Ordering key — a total order, not a stable sort

**Order by the total key `(destination, handle)`.**

This is a correctness requirement, not a preference. `buildEdgeTypeFilter`
(`cypher/api.go` ~17400–17455) resolves a parallel edge's relationship **type**
from its **ordinal within the source's run**: it counts `dstSeen[dst]++` in CSR
position order and calls `g.EdgeLabelsAt(srcStr, dstStr, dstSeen[dst])` on the
handle-less path (`handles[pos] == 0`, which is what MERGE-created slots carry).
`buildRevToFwd` (`cypher/exec/revtofwd.go`) has the same positional-ordinal
fallback when either CSR lacks handles. **Reordering slots within a source
therefore reassigns relationship types to parallel edges unless the within-run
ordinal is preserved.**

A stable sort on `destination` alone preserves it — but relying on stability
makes correctness depend on an algorithm property that a later refactor can
silently remove. `slices.Sort` and `sort.Slice` are pdqsort and are **not**
stable; reaching for either is the natural mistake and it is silent. Using the
handle as a tiebreaker makes duplicate destinations *totally* ordered so no
stability property is needed. This is the technique PostgreSQL's nbtree adopted
by making the heap TID a tiebreaker index column, and that RocksDB and LevelDB
use by suffixing the user key with a sequence number.

Handle-0 slots have no tiebreaker and must remain in a stably ordered residual.
That must be stated as an explicit invariant and tested, not left implicit.

### 4.2 Probe contract for parallel edges

With a total order, all slots sharing a destination form one contiguous run in a
defined order:

```
lookupFwdEdgePos(src, dst):
    lo = lowerBound(fwdEdges[start:end], dst)              // O(log d)
    return (start+lo, fwdEdges[start+lo] == dst)

lookupFwdEdgePosByHandle(src, dst, handle):
    lo = lowerBound(fwdEdges[start:end], dst)              // O(log d)
    for p = start+lo; p < end && fwdEdges[p] == dst; p++    // O(r), r = multiplicity
        if fwdHandles[p] == handle: return (p, true)
    return (0, false)
```

Total O(log d + r). No adversarial regression: r ≤ d and the current cost is
already O(d).

The multiset of `(destination, handle)` pairs is invariant under any permutation
trivially — a permutation does not change a multiset — so that is not the
property to check. The property that must be checked is the **within-run
ordinal** (§4.1).

### 4.3 The reverse CSR is already ordered

`BuildReverse` (`graph/csr/csr.go` ~515–535) scatters with `cursor[v]++` while
iterating `u` ascending, so within every reverse bucket the sources are already
ascending. Any "find the in-edge from a given source" probe can binary-search
the reverse CSR **today, at zero cost**. No work is required for this.

---

## 5. Why the adjacency is not ordered

Three independent blockers. Any one is sufficient; the first two are structural
rather than budgetary, so no amount of tuning reaches them.

### 5.1 `AuxColumn` has no permutation primitive

`AuxColumn` (`graph/adjlist/adjlist.go` ~307–355) exposes exactly four methods:
`GrowSlot`, `GrowSlotWithValue`, `CompactSlot`, `Compact`. None reorders.
`GrowSlotWithValue`'s documented contract explicitly blesses a coordinate-list
representation that appends `oldLen` "at the end of its strictly-ascending index
array — never an ordered insert", and `graph/lpg/edge_property_column.go`
implements exactly that. Permuting the neighbour column therefore requires
**adding a method to the `AuxColumn` interface and implementing a full index
remap-and-resort in the sparse column** — a breaking interface change plus real
work in a sparse representation, not a `memmove`.

### 5.2 The zero-allocation write path cannot survive, so #2143's bar is unreachable by construction

Today's append fast path is `nb := current.neighbours[:newLen]; nb[oldLen] = dst`
— it writes past the previously published length in the existing backing array
and publishes a longer slice header. It allocates **nothing** and is O(1)
amortised. It is sound only because a reader holding the prior entry sees the
shorter length and never reads index `oldLen`.

An ordered insert writes at a position `< oldLen`, which a concurrent lock-free
reader is actively reading. It is therefore unsound in place and forces a fresh
copy-on-write of four columns plus an aux transform per append: O(d) per append,
O(d²) to build a degree-d hub.

Every incremental maintenance strategy was costed rather than dismissed. For the
sorted-prefix-plus-tail-of-K design, with `R` probes per append, `c_l` the
per-element tail-scan cost and `c_m` the per-element merge cost:

```
C(K) = R·(c_b·log₂d + c_l·K) + c_m·d/K
K* = √(c_m·d / (R·c_l))
```

At the optimum the two terms are equal, so the irreducible maintenance term is
`2√(R·c_l·c_m·d)` — **Ω(√d) per append, and never zero**. K only chooses where
the loss lands. Measured at d=4096 with K=71, maintenance adds ~185 ns/append
against a shipped ~8.9 ns/append, and the merge events contribute hundreds of
extra allocations. Piggybacking the sort on the existing geometric `growCap`
reallocation was also checked and refuted: with ×2 growth the unsorted tail is
expected ≈ d/4, which converts the win into ~4× while still costing 3.8–4.1× the
shipped hub-build time.

rmp #2143 requires **allocs/op unchanged sample-for-sample**. For any
insert-time ordering this is not a demanding target — it is unattainable.

### 5.3 A history-dependent representation breaks recovery determinism

`ApplyCSRToGraph` (`store/snapshot/apply.go`) replays edges **in bulk, in
`csr.bin` order**, which is a different degree trajectory from the original
interleaved write history. If the physical representation depends on how an
entry was built — which any promote-at-T rule with hysteresis does — a hub can
recover into the *unsorted* representation where it was *sorted* before the
crash, and the next `csr.bin` then differs. Invariant **I-POS**
(`cypher/api.go` ~371–373) states that identical per-source out-degree counts and
insertion order imply identical edge positions, `BuildFromAdjList` being a
deterministic pure function of the adjacency; a history-dependent
representation breaks that.

So #2139's requirement (a) — a hysteresis margin so a boundary vertex cannot
oscillate — and requirement (e) — durability and recovery safety — **cannot both
be satisfied**. A pure threshold satisfies recovery determinism but reinstates
the oscillation; hysteresis suppresses the oscillation but breaks recovery
determinism.

### 5.4 What the adjacency half would have bought

Of the four wins §2.4 attributes to this change, three land on the CSR and one
on the adjacency:

| Win | Probe location | CSR ordering enough? |
|---|---|---|
| `Expand(Into)` for bound destinations | `Expand.lookupFwdEdgePos` — CSR | **Yes** |
| Symmetric anchor swap | CSR | **Yes** |
| Sorted-merge intersection primitive | CSR | **Yes** |
| O(log d) `HasEdge` on the MERGE write path | `AdjList.HasEdge` — adjacency | No |

**The adjacency change buys one of four wins, and it is the one on the commit
path** — the highest-risk change purchasing the narrowest benefit. Its ceiling
is also capped independently: `AdjList.HasEdge` takes user-typed keys and calls
`Mapper.Lookup` twice, each an `RWMutex.RLock` plus a map probe, measured at
17.8–19.6 ns of fixed preamble before the scan begins. Sorting therefore buys
~4% at degree 8 and ~77% at degree 64. Adding a `HasEdgeByID` that skips the
mapper — which does not exist today, though `OutDegreeByID` does — removes ~18 ns
at *every* degree with no write-path change at all, and is the better first move.

---

## 6. Durability and recovery analysis

### 6.1 Which persisted artefacts capture within-source slot order

| Artefact | Producer | Captures slot order | Re-hydrated into a graph |
|---|---|---|---|
| `snapshot/csr.bin` | `checkpoint.go` → `csr.BuildFromAdjList` | **Yes** | **Yes**, via `ApplyCSRToGraph` |
| `snapshot/edgehandles.bin` | `store/snapshot/edgehandles.go` | No — keyed by `(src,dst,handle)` | Yes, by handle |
| WAL frames | `store/txn` | No — logical op stream | Yes |
| `*.csr` (`store/csrfile`) | `csrfile/writer.go` | Yes | **No** |

### 6.2 Cross-version reopen parity: no format change required

Old writer → new reader: `csr.bin` holds append-ordered runs; `ApplyCSRToGraph`
rebuilds the adjacency in that order and the *next* `BuildFromAdjList` re-derives
the ordering from the rebuilt adjacency. New writer → old reader: the old reader
never assumed sortedness. Both recover identically, because **within-source order
is derived at build time and never trusted from disk.**

`store/csrfile/format.go` has `CurrentVersion = 1`, a `Version uint16` header
field, seven reserved bytes that `DecodeHeader` never validates, and a reader
that rejects only *newer* versions. A flags bit could be smuggled into the
reserved bytes, but `docs/csrfile-v1.md`'s own versioning rule requires a bump
when any header field is repurposed — so a flag is both unnecessary and more
expensive than the alternative.

**Verdict: no version bump, no flags bit, no sort-on-load — conditional on the
design committing in writing to the derivation rule above.** That sentence must
land in `docs/csrfile-v1.md` and `docs/persistence.md`.

Two facts make this safe that are not obvious from the code:

- **The per-instance stores are in-memory only.** `IncEdgeCreateCount`,
  `SetEdgeLabelAt` and `SetEdgePropertyAt` have no callers under `store/`. After
  a restart `EdgeCreateCount == 0`, so the positional-ordinal parallel-edge
  typing path of §4.1 is *unreachable* post-recovery; parallel-edge typing then
  rests entirely on the handles column plus `edgehandles.bin`. The §4.1 hazard is
  therefore a **live-session** correctness hazard, not a recovery-parity one.
- **`csrfile` artefacts are never re-hydrated into a graph**, and the format
  carries no handles section, so a csrfile-backed CSR always takes the
  handle-less fallbacks.

### 6.3 Tombstones: no coupling

`BuildFromAdjListLive` applies `live(src) && live(dst)` per arc,
element-by-element, precisely so the parallel columns stay arc-aligned after
filtering. Reordering commutes with a per-arc predicate
(`filter ∘ permute == permute ∘ filter`), so there is no resurrection path and no
live-edge drop. The checkpoint uses the **raw** build, not the live one, and
tombstones travel separately in `tombstones.bin`. Handle preservation is
unaffected: `compactEntry` already permutes all columns under one index
transform and publishes one entry.

`graph/csr/build_live_test.go` `TestBuildLive_NilFilterMatchesRaw_1790` compares
raw and live arcs **without sorting**, so it fires if the ordering rule is
applied to only one of the two build paths. It is the single most valuable
existing guard here and must be kept.

### 6.4 Atomicity and isolation

A reader can never observe a partially ordered entry, and the reason is stronger
than the visibility barrier: `storeEntry` publishes an entire `*adjEntry` through
a **single pointer store**, and all five columns hang off that one pointer. A
reader's `loadEntry` therefore obtains a complete old-or-new entry. The
obligation on any implementation is narrow and absolute: **never permute a
published entry's backing arrays** — allocate fresh columns, permute into them,
publish once. This is the template `compactEntry`, `trimEntry` and
`SetEdgeLabelSlots` already follow.

`BeginCommit`/`EndCommit`'s clone-once-per-(shard, window) `building` mutation
is unaffected: it governs the slot array's lifecycle, not the contents of a
published `adjEntry`.

### 6.5 The crash battery named in #2143 is largely false comfort

This must be stated plainly, because #2143 cites these suites as its acceptance
evidence:

| Battery | Detects an ordering change? |
|---|---|
| `internal/crashinject/` | **No — it contains no graph-shape assertions at all.** It tests the harness: subprocess spawn, SIGKILL detection, timeout disambiguation. Nothing in it builds a graph. |
| `store/recovery/` crash tests (~14) | **No.** All funnel through `graphFingerprint`, which sorts edges by destination and keys edge properties by destination name — blind to within-source order and to which parallel slot a property landed on. |
| `store/wal/` | **No.** WAL frames are a logical op stream; the compat test pins synthetic payloads, not graph edges. |
| Byte-equality tests (snapshot, csrfile, csr cross-process) | **No.** Every one is a *self*-comparison — build twice, or parent vs child. A deterministic reorder passes all of them. No checked-in golden byte blob of a `BuildFromAdjList` output exists. |
| `graph/csr/build_order_property_test.go` | **No** — it explicitly disclaims within-source order and sorts before comparing. |
| `internal/invariants/AssertShapeEqual` | **No** — a multiset comparison of `(u,v)`. |
| `store/recovery/snapshot_self_sufficient_test.go` `RoundTripByteStable` | The one real guard — but its fixture is 4 edges with max out-degree 2, so it **passes vacuously**. |
| `cypher/tck/` | **Yes** — genuine coverage. See §9. |

Four tests must therefore be written **before** the implementation, not after:

1. A **golden absolute-order assertion**: `EdgesSlice()` compared
   element-by-element against a hand-written expectation, for one hand-built
   graph containing a hub above the threshold and parallel edges with distinct
   handles.
2. **Re-parameterise `RoundTripByteStable`** with a hub above the threshold,
   plus a variant where the pre-crash insertion trajectory differs from the
   bulk recovery trajectory — the only test that can catch §5.3.
3. A **parallel-edge ordinal differential** on a multigraph hub where each
   parallel edge carries a different relationship type, asserting resolved types
   against a **hand-computed absolute oracle**. Not against the other build
   path: two prior false-greens in this project came from both arms of a
   differential sharing the broken code.
4. An **order-preserving `graphFingerprint` variant**, or a companion
   fingerprint keyed by `(dst, handle)`, so the crash battery stops being blind
   to slot identity.

---

## 7. Write-path neutrality budget

Because the adjacency is not ordered, the write path is untouched and the budget
is exact:

| Metric | Budget |
|---|---|
| `BenchmarkEngWriteAutocommit` allocs/op | **Unchanged, sample-for-sample** |
| `BenchmarkEngWriteAutocommit` ns/op | Within run-to-run noise |
| `adjlist` append fast path | **Byte-identical** — no code change |
| `AuxColumn` interface | **Unchanged** — no new method |

The cost moves to the CSR build, which is off the write path:

| Metric | Budget |
|---|---|
| `BuildFromAdjList` complexity | O(V + E) → O(V + E log d̄) |
| `BuildFromAdjList` allocations | No per-source scratch allocation for runs below the insertion-sort cutoff |
| Checkpoint wall time | To be measured in #2145; the reference point is that one full per-source stable sort of 5.1M edges measured ~139 ms, comparable to one `BuildReverse` |

Implementation note for #2141: use insertion sort below d≈32 — naturally stable,
in-place, no scratch buffer — and a stable merge above. On a Barabási–Albert
fixture with average out-degree 16 that keeps the large majority of sources
allocation-free.

---

## 8. Benchmark obligations for #2145

- Report **per degree**, never aggregated, so the low-degree no-regression claim
  is visible rather than averaged away.
- Do **not** benchmark only on RMAT. At T=64 RMAT reports 97.78% of scan cost
  above threshold against Barabási–Albert's 67.18% (§2.4); a change measured only
  on RMAT will look like a triumph and then fail to reproduce.
- Quote the corrected probe figures from §2.2, not §2.4's. The §2.4 reference
  numbers the task text asks to record (0.659 ns vs 1.865 ns at degree 8;
  164 ns vs 5.31 ns at degree 4096) are **refuted** and must not be carried
  forward; record them only as the superseded values with a pointer to §1.

**Example 26 (#2147) will not exercise this at T=64.** Its defaults are
`friendsMin 150, friendsMax 200`, a *uniform* out-degree in which every user is a
hub. #2147's bounded parameters (`-friends-min 20 -friends-max 40`) give
out-degrees of 20–40 — above a threshold of 16, but **entirely below 64**. At a
threshold of 64 the ordered path would engage for zero FRIEND vertices. With the
unconditional CSR ordering chosen in §4 the question disappears, but the example
must report its actual degree distribution so this is visible rather than assumed.

---

## 9. openCypher-observable ordering — verdict

**Ordering the CSR is a behaviour change and must be treated as one.** The CSR's
within-source order is the order `Expand` emits rows, which is observable in any
query without `ORDER BY`, and in the element order of `collect()`.

The TCK comparison layer is order-sensitive by design:
`resultShouldBeInOrder` → `compareOrdered` enforces strict row *and* list-element
order; `resultShouldBeInAnyOrder` → `compareMultiset` still enforces
list-element order strictly. Only the `*IgnoringListOrder` variants normalise it.
So a `collect()`-order change does surface.

`id(r)` is also a forward-CSR position (`resolveHopRel` sets `rel.ID = fwdPos`).
It is already unstable across rebuilds, but a reorder changes the values.

**Verdict: no openCypher-mandated order depends on slot order — the specification
does not constrain the order of rows without `ORDER BY` — but the TCK's own
comparison of unordered results keeps list-element order strict, so the suite is
a genuine gate and `tckExecutionBaseline = 3897` must hold unchanged.** This
cannot be settled by reading feature files; #2141 must run the full suite against
the implementation. That run is the acceptance evidence.

---

## 10. Obligations carried into #2141

1. Apply the ordering rule in **both** `buildFromAdjListRaw` and
   `BuildFromAdjListLive`, identically. Pass 2 of the raw path currently does a
   bulk `copy(edges[start:], nb)`; ordering replaces that with a per-source
   permute of `edges`, `weights` and `handles` under one index transform.
2. Order by the total key `(destination, handle)`; do not rely on sort
   stability; do not use `slices.Sort`/`sort.Slice` on the destination alone.
   Document and test the handle-0 residual rule.
3. Decide and document `csr.FromArrays`, which today explicitly promises caller
   order and is relied on by `store/bulk` and three `internal/sim` oracles.
   Either make it sort, or document that a `FromArrays` CSR carries a different
   order guarantee — and then prove no consumer mixes the two.
4. State the derivation rule of §6.2 in `docs/csrfile-v1.md` and
   `docs/persistence.md`. While there, fix `docs/persistence.md`'s `csr.bin`
   table, which stops at `weights` and omits the trailing `uint8 hasHandles` plus
   handles block that `store/snapshot/writer.go` actually emits.
5. Land the four tests of §6.5 **before** the implementation.
6. Keep `csr.Validate` honest about the new invariant, and keep it
   non-allocating and panic-free.
7. Correct the three `internal/sim` oracle comments
   (`search_community.go`, `search_euler.go`, `search_centrality.go`) that claim
   to build "exactly as `csr.BuildFromAdjList` would" while calling
   `csr.FromArrays` directly.

## 11. Opportunities found, out of scope here

Recorded so they are not lost; each needs its own task.

1. **`Expand.lookupFwdEdgePos` can be deleted, not merely accelerated.**
   `BuildReverse` pass 2 already holds both the forward position `k` and the
   reverse position `pos` in the same loop iteration — it already copies
   `c.handles[k]` to `revHandles[pos]`. One more store, `revFwdPos[pos] = k`,
   makes the lookup an O(1) array load and makes the by-handle variant
   unnecessary. Cost: +4 bytes per reverse edge on an immutable snapshot. This
   dominates any layout choice, but it changes edge identity for parallel edges
   in graphs without handles, so it needs a full TCK run.
2. **`edgeTypeFilter` should be a bitset, not `map[uint64]string`.** All six
   read sites use `_, ok :=` — the string value is **never read**. It is a
   presence set over a dense integer key space. Measured 17–25× faster and
   ~215× smaller (0.13 vs 27.95 B/edge).
3. **Add `AdjList.HasEdgeByID`** to skip the double `Mapper.Lookup`, worth ~18 ns
   at every degree (§5.4).
4. **The §2.4 audit table should be corrected in place**, and its harness
   committed. It is cited as the basis for this sprint *and* for the OUT-only
   restriction on the anchor-swap peephole. A load-bearing measurement that was
   "retained outside the repository" and cannot be re-run is not evidence.

---

## 12. Calibration artefacts

The harnesses backing §2.2 and §2.4 are calibration instruments, not committed
tests, and live outside the working tree at
`$SCRATCH/probecal/` (`probe_test.go`, `honest_test.go`). The permanent
benchmarks are #2145's deliverable and belong under `bench/`. The degree
distribution of §2.4 was measured with a temporary test against
`internal/shapegen`'s `BarabasiAlbert` and `RMAT` generators, removed after use;
#2145 should make that measurement permanent so the fixture's skew is reported
alongside every result.
