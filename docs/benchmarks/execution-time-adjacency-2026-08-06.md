# Resolving adjacency at execution time — the measured cost

rmp #2317 (MVCC C1d), stage 2b. Acceptance criterion 4.
Machine: darwin/arm64, Apple M4, 10 cores, quiet. `-benchmem -count=10`, benchstat.
Baseline commit `7ffae605` (stage 2a), against the working tree with stage 2b applied.

## What changed

`Expand` and `OptionalExpand` no longer receive two CSRs captured while the operator
tree was being assembled. They receive an `exec.AdjacencySource` and call it in
`Init`, which under Apply runs once per outer row. The relationship-type filter
travels with the pair rather than being a separate `ExpandConfig` field, because it
is keyed to that pair's absolute edge positions.

## The correctness it buys

A later reading clause of a statement now observes an earlier edge CREATE and an
earlier edge DELETE, which it did not before. The full same-statement matrix:

| earlier clause | before | after |
|---|---|---|
| node CREATE | visible | visible |
| node DELETE | visible | visible |
| label SET | visible | visible |
| node property SET | visible | visible |
| edge property SET | visible | visible |
| **edge CREATE** | **NOT visible** | **visible** |
| **edge DELETE** | **NOT visible** | **visible** |

## The cost: none measurable

### bench/cypher_scale

| benchmark | base sec/op | new sec/op | vs base |
|---|---:|---:|---|
| Expand1Hop | 702.7m | 699.4m | ~ (p=0.247) |
| Expand1HopSelective_Warm | 11.52m | 11.49m | ~ (p=1.000) |
| Expand1HopSelective_Cold | 513.9m | 511.3m | ~ (p=0.218) |
| **geomean** | 160.8m | 160.2m | **−0.41%** |

B/op geomean −0.22%, allocs/op geomean −0.28%, no benchmark individually significant.

### bench/expandinto

18 benchmarks across ClosingHop, Triangle and OpenControl at degrees 4–64, seek and
filter arms. **geomean +0.02%.** One arm moved beyond noise in each direction —
`ClosingHop/degree8/filter` +0.90% (p=0.004) and `ClosingHop/degree32/seek` −1.13%
(p=0.001) — which is the pattern of measurement noise surviving a p-value, not a
trend. Every allocation figure is unchanged to four significant figures.

## Why it is free

The concern this measurement was run to answer is that sprints 311–315 built their
wins on the plan-build CSR — prefix range seek 385×, bitmap intersection 2909×,
ExpandInto seek −77.69%, ExpandIntersect fusion −54.24% — and that resolving
adjacency per row would retire them.

It does not, because *resolving* is not *rebuilding*. The pair comes from a cache
keyed on `csrPairKey{epoch, startTS, versioned}`. For a read the key does not move
during the statement, so each `Init` is two mutex-guarded cache hits returning the
same pair the plan build would have captured — the same arrays, the same seeks, the
same fused operators. What used to happen once at plan-build now happens once per
`Init` and costs a map lookup.

For a WRITE the key does move, and the rebuild is what makes the later clause
correct. That is the cost the correctness is bought with, and it is paid only by
statements that write and then traverse.

## Stage 2c — the remaining four operators

`VarLengthExpand`, `ShortestPath`, `AllShortestPaths` and `ExpandIntersect` were
converted the same way, and with them the last plan-build CSR materialisation on the
traversal path is gone: `tryFuseCyclicIntersect` no longer needed the pair it was
being handed, so the `csrPairCachedForAt` call at the top of the Expand case was
removed outright.

`ShortestPath.WithTypeFilter` lost its filter argument for the same reason the
`ExpandConfig` field did. Its doc used to warn that the filter had to be chained on
BEFORE `Init`, because `Init` builds the reverse-position admit bitset from it — a
caller obligation that is now structural, since the filter and its adjacency arrive
together at the one point that needs them.

### bench/cypher_scale, after 2c

| benchmark | base sec/op | 2c sec/op | vs base |
|---|---:|---:|---|
| Expand1Hop | 702.7m | 694.1m | −1.23% (p=0.002) |
| Expand1HopSelective_Warm | 11.52m | 11.37m | ~ (p=0.075) |
| Expand1HopSelective_Cold | 513.9m | 507.7m | ~ (p=0.089) |
| **geomean** | 160.8m | 158.8m | **−1.25%** |

Slightly FASTER than the baseline, because dropping the plan-build resolution
removed work the fused-intersect recogniser was doing and discarding.

### bench/cyclicjoin, after 2c

12 arms over Triangle/TwoCycle/NonQualifying at degrees 4–64, each in a `twoexpand`
and a `fused` variant. **geomean +0.27%.**

The `fused` arms — the ones `ExpandIntersect` actually drives — are neutral
throughout, apart from `TwoCycle/d=16` at +1.17%. The `twoexpand` arms carry the
whole regression, +0.68% to +1.62%, which is the per-`Init` source call paid twice
per outer row by a two-operator chain rather than once. Allocation figures are
unchanged everywhere.

That is the honest cost of the change: **under 2% on the chained-expand shape, zero
on everything else**, against closing a correctness gap that made an edge written by
a statement invisible to the rest of it.

## Not covered here

- `bench/cypher_ldbc`, the fourth suite acceptance criterion 4 names.
- Relationship identity is still the absolute CSR position, not the stable handle
  stage 2a made available. Acceptance criterion 3 is therefore open, and with it the
  `ensureEdgeIDResolver` path-reconstruction helper, which is the last thing keyed on
  a CSR position.

## Stage 3 — the emitted relationship identity becomes the handle

Acceptance criterion 3, and the fix for rmp #2334.

The identity a row carries for a relationship was the absolute position of the edge
in the forward CSR. It is now the stable per-slot handle, on every operator that
emits one: `Expand`, `ExpandIntersect`, `VarLengthExpand`, `ShortestPath` and
`AllShortestPaths`.

Positions remain, and are still the right tool, wherever they are internal to one
`Init`: the expand-into cursor seek, the shortest-path cursors, the type filter's
keying, and the CSR range arithmetic. What changed is only what OUTLIVES an `Init` —
because that is exactly what an identity must survive.

### The correctness it buys

`RelationshipValue.ID` is now the handle, so a write issued through a projected
relationship reaches the instance a read resolves. Before, the two disagreed and the
same edge reported two values by query shape (rmp #2334):

| after `WITH r SET r.k = 2` | before | after |
|---|---|---|
| `MATCH (:A)-[r:R]->(:B) RETURN r.k` | 1 | 2 |
| `... WITH r RETURN r.k` | 2 | 2 |
| `... RETURN properties(r).k` | 1 | 2 |

Gated across the full cross-product of five write shapes and three read shapes, plus
`REMOVE` and `DELETE` after a barrier, and validated in both directions: reverting
the emit alone fails the topology-change and parallel-edge gates; reverting the
`RelationshipValue` handle alone fails the write-shape cross-product.

### The cost

| suite | geomean sec/op | allocations |
|---|---|---|
| bench/cypher_scale | +0.52% (nothing individually significant) | +0.49% B/op |
| bench/expandinto | +0.88% over 18 arms | unchanged to 4 s.f. |

### What it removed

A reverse hop used to recover the corresponding FORWARD position to produce an id
the forward hop would agree with — an O(deg(dst)) scan of dst's outgoing range per
reverse edge, plus a handle match on top of it in a multigraph, because otherwise
parallel edges all collapsed onto the first forward position (rmp #1634). A handle
is already shared across both directions by `csr.BuildReverse`, so the scan is gone
and the reverse hop reads one array slot.

The whole position→handle resolution layer went with it: `EdgeHandleAtPosition` on
the `GraphMutator` interface and both adapters, and the `edgeHandleAtFwdPos` helper
the read path used. Both existed only to recover an identity the row now carries.

`RemoveEdgeByHandle` on the adapters became orientation-tolerant. A handle names one
logical edge and is orientation-free, but the endpoint keys come from a row's
traversal columns — and `MATCH (a)-[r]-(b)` over a directed edge produces a row per
direction, so half of them name the storage pair backwards. That was latent while
the id was a position (it simply failed to resolve in the swapped orientation and
fell back to endpoint matching) and became live the moment the handle resolved.
openCypher TCK Delete4 [1] caught it.
