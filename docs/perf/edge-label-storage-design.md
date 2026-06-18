# Edge-label storage — memory redesign (SPIKE, task #1582)

**Status:** design / go-no-go. **Date:** 2026-06-18. **Sprint:** 205 (S-mem-2026-06-18 #1).

## Problem (measured)

Per-edge relationship-type labels are stored in `graph/lpg/lpg.go` as:

```go
type edgeLabelShard struct {
    mu    sync.RWMutex
    one   map[edgeKey]LabelID   // single-label fast path
    multi map[edgeKey][]LabelID // rare multi-label spill
}
// edgeKey{ src, dst graph.NodeID } — a 16-byte key
```

A heap profile (`inuse_space`) of example 26 at 40k users / 4k articles
(≈ 13M edges, float64 weight) measured this structure at **418.7 MB =
56.7 % of resident heap (~32 B/edge)** — the single largest memory
consumer in the module. The same build with implicit types (no edge
labels) drops to 295 MB, isolating the label store as the dominant cost.
At the full 1M-user specification (~325M edges) this is ~10–13 GB.

The 16-byte `edgeKey` **redundantly re-stores the `(src,dst)` pair that
`graph/adjlist` already holds** in its per-source neighbour slices; the
Go (swiss-table) map adds per-entry bucket/load-factor overhead on top.

## Invariants that constrain the redesign (verified in source)

1. **On-disk format is representation-independent.** Snapshot
   serialization (`store/snapshot/labels.go:292`) reads edge labels via
   the public `g.EdgeLabels(src,dst)` and `edgeLabelShard.forEach`
   (`graph/lpg/introspect.go:84`). Any in-memory change is on-disk
   compatible **with zero migration** as long as `EdgeLabels()` /
   `forEach()` yield the same `(src,dst,label)` triples.
2. **Multigraph per-edge labels are stored elsewhere.**
   `edgeInstanceLabelShards` (instance-idx keyed) and
   `edgeHandleLabelShards` (stable-handle keyed) carry per-edge labels in
   multigraph mode. The per-pair `edgeLabelShard` is a **coalesced union
   view** across parallel edges — that contract must be preserved.
3. **`SetEdgeLabel` requires the adjacency edge to exist**
   (`lpg.go:1321`, `HasEdge` guard).
4. **`RemoveEdgeLabel` does NOT require the edge to still exist**
   (`lpg.go` contract): it can strip a label from a `(src,dst)` whose
   adjacency edge was already removed within a failed statement (the
   executor's transaction-undo path). The per-pair store can therefore
   legitimately hold a label for a pair with no live adjacency slot.
5. **Concurrency / ACID:** the store is 16-way sharded by `src` with a
   per-shard `RWMutex`; atomic transaction visibility is provided by the
   graph's `visMu` / `View` / `ApplyAtomically` barrier. Any redesign
   must keep this model and the openCypher TCK (3897 scenarios) green.
6. `graph/adjlist` already carries an **optional opaque `handles []uint64`
   parallel column** (populated only via `AddEdgeH`), compacted in
   lockstep with `neighbours` and never renumbered — a precedent for an
   optional per-slot column in the generic backend.

## Candidates evaluated

| # | Representation | B/edge (single-label) | vs now | Risk | Notes |
|---|---|---:|---:|---|---|
| A | per-slot label column in `adjEntry`, **derived** per-pair union (graph-theory-expert's "Variant D") | **~4** | **~8× / −88 %** | **Medium** | Biggest win. Needs: (i) lockstep compaction of the column (reuse `compactEntry`); (ii) an LPG-side overflow for multi-label edges AND for **orphaned labels on removed edges** (invariant 4); (iii) a `LabelID` (uint32) column in the generic `adjlist` — a layering compromise, though consistent with the existing `handles` precedent. |
| B | packed-key `map[uint64]LabelID`, key `(src<<32\|dst)`, guarded fallback ≥2³² | ~20 | ~1.5× / −37 % | **Low** | Keeps the exact current per-pair semantics (incl. labels on removed edges, invariant 4). Simple, fully safe. Keeps the redundant key but halves it. |
| C | per-source nested `map[NodeID]→map[NodeID]LabelID` | ~20 | ~1.5× | Low–Med | No memory gain over B, extra indirection, worse locality. Rejected. |
| D | handle-keyed labels (force `AddEdgeH` for labelled edges) | ~20–24 | ~1.4× | Low | Safe, but the forced +8 B handle column in adjlist/CSR cancels most of the saving. Weak. |

## Recommendation

- **Maximum-gain path: candidate A (~8×).** Achievable while preserving
  TCK / ACID / on-disk format / multigraph semantics, *provided* the two
  edge cases are handled explicitly: multi-label edges and labels on
  removed edges both spill to a small LPG-side overflow keyed by a stable
  identity (not the volatile slot index). It introduces a `LabelID`
  column into the generic `adjlist` (layering compromise, mitigated by
  the `handles` precedent and by keeping the column nil for label-free
  graphs).
- **Maximum-safety path: candidate B (~1.5×).** No semantic edge cases,
  no layering change, trivially TCK/ACID-neutral.

The choice is a deliberate **risk vs. headline-gain trade-off on an
ACID/TCK-critical subsystem and the generic adjacency backend** — per the
project's decision-autonomy rule it is escalated to the maintainer rather
than chosen unilaterally. Go/no-go and variant selection are recorded in
the sprint-205 task #1583 once decided.
