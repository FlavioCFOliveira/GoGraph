# MVCC P0 — the delta-chain cost model, measured

**rmp #2275 · sprint 330 · 2026-07-31 · Apple M4 (10 cores), `darwin/arm64`, go1.26.5**

## Verdict: **the cost model HOLDS. Phases P1–P7 are authorised.**

Per-write cost is **independent of graph size**, the mechanism costs **1.02×** on a read when
no version is live, and there is **no regression with the flag off**. The conclusion recorded
in rmp #2051 — that MVCC in GoGraph requires replacing the LPG core maps with persistent data
structures — **does not apply to this design**, and is corrected below.

---

## 1. What was measured, and why only this

The programme in [`design-mvcc-delta-chains.md`](design-mvcc-delta-chains.md) rests on one
inherited premise, and this cycle has already found three inherited numbers to be wrong. So P0
builds the smallest thing that can refute it: delta chains on **node labels only**, behind
`Graph.EnableLabelDeltas`, with no transaction layer, no garbage collection and no read-path
change. It answers one question — **is the per-write cost independent of graph size?** — and
nothing else.

Both arms run in **one process toggled by an option**, not two builds compared back to back,
because a byte-identical control on this machine has previously produced 22 of 36
flat-by-construction rows as "significant". The two graph sizes are 100× apart because the
claim is about scaling, and one size cannot support or refute it.

## 2. Write cost — the question P0 exists to answer

`BenchmarkLabelWrite`, one label added and removed per iteration, so **two** deltas per
iteration when armed. Medians of 6 runs.

| | ns/op | B/op | allocs/op |
|---|---|---|---|
| 10 000 nodes, deltas off | 140.3 | 8 | 1 |
| 10 000 nodes, deltas **on** | 189.8 | 56 | 3 |
| 1 000 000 nodes, deltas off | 384.0 | 8 | 1 |
| 1 000 000 nodes, deltas **on** | 454.9 | 56 | 3 |

| overhead of the mechanism | time | memory | allocations |
|---|---|---|---|
| at 10 000 nodes | +49.5 ns (1.35×) | **+48 B** | **+2** |
| at 1 000 000 nodes | +70.9 ns (1.18×) | **+48 B** | **+2** |

**Memory and allocation overhead are identical at both sizes** — +24 B and +1 allocation per
*modification*, which is the flat 24-byte `nodeLabelDelta` and nothing else. A 100× larger
graph changes them not at all.

Time overhead rises 1.43× across that 100× range, but the **baseline itself rises 2.74×**
(140.3 → 384.0 ns) over the same range: the delta cost tracks the cost of touching a larger
map, which the write already pays, not the number of nodes. In *relative* terms the mechanism
gets **cheaper** as the graph grows — 1.35× at 10k, 1.18× at 1M.

### Against rmp #2051

| | per-shard COW (#2051) | delta chains (this spike) |
|---|---|---|
| write time | **5.4×** | 1.18×–1.35× |
| write memory | **43×** | +48 B/op, flat in graph size |
| scaling of per-write cost | **O(shard size)** — worsens as the graph grows | **O(1)** — flat |
| requires persistent maps (HAMT/CTrie) | yes, by construction | **no** |

The #2051 *measurement* was sound. Its *conclusion* generalised from the whole-graph-snapshot
model to MVCC as such, and that step does not hold: a version here is never materialised, it
is reconstructed from bounded undo records, which is what Memgraph does.

## 3. Read cost — the number that decides affordability

`BenchmarkLabelRead`, 100 000 nodes, medians of 8 runs.

| | ns/op | vs control | allocs |
|---|---|---|---|
| control — mechanism absent | 7.855 | 1.00× | 0 |
| armed, **no live delta** (the lock-free gate) | **7.978** | **1.02×** | 0 |
| armed, one live delta on the node read | 24.105 | 3.07× | 1 (8 B) |

**The idle cost is 0.12 ns, or 1.6 %.** A read-only workload, and the overwhelming majority of
a mixed one, pays one uncontended atomic load and nothing else. The 3.07× is paid only on a
node a concurrent writer has actually touched and has not yet had reclaimed — which is the
trade the design is for.

### Two harness errors that would have produced the wrong verdict

Both are recorded because each, uncorrected, would have failed the design on a measurement
artefact.

- **The fixture armed the spike BEFORE seeding**, so every node's first label recorded a
  delta and the graph carried 100 000 live ones. The "no live delta" arm was therefore
  measuring a chain walk plus a bag clone, and reported the fast path at **15.2 ns, 3.0×**.
  The benchmark now asserts `LabelDeltaCount() == 0` in that arm rather than assuming it.
- **The control was the read written inline in the benchmark loop**, where the compiler
  inlines the shard index, the lock and the map access and never copies a 40-byte `labelBag`
  out through a return — costs the MVCC read pays and the control did not. Against that
  control the fast path read **1.60×**. Measured against `labelBagPlain`, which has the same
  call and return shape, it is **1.02×**. Most of the apparent regression was the comparison.

The first version of `labelBagAsOf` was also genuinely too slow: it deferred the unlock and
held the chain walk inline, which made it uninlinable at 9.06 ns. Splitting the slow path into
a `//go:noinline` helper and checking the gate before taking the lock is what closed it.

## 4. No regression with the flag off

`BenchmarkEngWriteAutocommit`, medians of 6 runs, before and after the spike landed:

| | ns/op | B/op | allocs/op |
|---|---|---|---|
| before | 2066.5 | 2641 | 33 |
| after | 2053.0 | 2639 | 33 |
| delta | **−0.65 %** | −1 B | ±0 |

**Note for anyone comparing against #2051:** its recorded baseline for this benchmark is
5664 ns/op, 2398 B/op, 34 allocs. Measured at HEAD today it is **2066 ns/op** — the write path
has become **2.7× faster** since that record was written, so the #2051 figures cannot be used
as a direct comparison and the before/after pair above was taken fresh.

## 5. Correctness of what was built

The cost of a prototype that answers wrongly is not interesting, so the visibility rule is
pinned by test even though the spike ships disabled:

- the three cases of the rule — own uncommitted change visible, another transaction's
  uncommitted change never visible, a committed change visible only if it committed at or
  before the reader's start timestamp;
- an older version is reconstructed correctly and the **stored** version is not disturbed;
- a re-assertion that changes nothing records **no** delta — MERGE's MATCH branch re-asserts
  labels on every match, so a delta per statement rather than per real change would grow the
  chain without bound on an ordinary workload;
- the mechanism is **inert unless armed**.

## 6. The GoGraph-specific adaptation

Memgraph hangs the delta pointer off its `Vertex` struct, which is free because a Vertex is
already a struct. **GoGraph has no per-node struct** — node labels live in
`map[graph.NodeID]labelBag` across 64 shards — so a pointer field on `labelBag` would grow the
map's value for every labelled node, a permanent cost paid by graphs that never write.

Deltas are transient. The head therefore lives in a **sparse side map per shard**, allocated
lazily, holding only nodes with a live chain, with a graph-level atomic counter mirroring the
total. That counter is the gate measured at 1.02× above, and it is the same lock-free-gate
idiom `tombstoneActive` and `edgeLabelOverflowActive` already use.

## 7. What P0 deliberately does not answer

- **Garbage collection.** Nothing reclaims deltas, so the spike's memory grows without bound
  under sustained writes. P6 owes a bound and its metrics; `LabelDeltaCount` exists to measure
  it.
- **Contention.** Every number here is single-threaded. The design's whole purpose is
  behaviour under concurrent read and write, which P4 must measure against
  `bench/mtaudit/fairness_soak_test.go`.
- **Properties, edges and edge types**, whose values are larger and whose undo records will
  not be 24 bytes. The cost model is confirmed for the cheapest structure; P2 and P3 must
  re-measure it for the others rather than assume it carries.
- **The read path is not retired.** `Engine.Run` still holds the visibility barrier; nothing
  in #2274 is fixed yet.
