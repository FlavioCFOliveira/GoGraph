# Mandatory edge identity — the measured cost

rmp #2317 (MVCC C1d), stage 2a. Machine: darwin/arm64, 10 cores, quiet.
Baseline commit `7cab75b0`.

## What changed

Every adjacency slot now carries a stable handle. It used to be optional: the
column existed only when a caller supplied handles, with `0` as a "no handle"
sentinel, so edges created through Cypher had an identity and edges created
through the Go API, the `graph/io` loaders or bulk import did not.

## Why a position could not be the identity

A relationship's identity was its POSITION in a rebuilt CSR edges array. Two
measurements settled that it cannot stay that way once a later clause of a
statement must observe an earlier edge write:

- **Insertion is safe.** `upsertEdgeLocked` appends (`nb[oldLen] = dst`), so
  existing ordinals do not move. The `(destination, handle)` ordering the CSR has
  is imposed by the *build*, not held in the adjacency.
- **Removal is not.** `removeOneEdgeWithHandle` calls `compactEntry`, so every
  ordinal after a removed slot shifts down. A row bound to `(src, ordinal=3)`
  names a *different edge* after an earlier sibling is deleted in the same
  statement.

So the `(srcID, ordinal)` pair that would have cost nothing is unsound under
exactly the mutation this task exists to make visible. `RemoveEdgeByHandle`
already existed precisely because positions reshuffle.

## The cost

### Adjacency resident memory

60 000 nodes × degree 16 = 960 000 edges, `HeapAlloc` delta after two GCs:

| handle column | resident | per edge |
|---|---:|---:|
| absent (plain `AddEdge`) | 26 662 824 B | 27.77 B/edge |
| present | 34 315 472 B | 35.75 B/edge |

**+7.98 B/edge, +28.7 %.**

### CSR build, `-benchmem -count=10`

`BenchmarkCSR_Build_{No,With}Handles`, mean of 10:

| arm | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| baseline, no handle column | 2 447 114 | 2 138 224 | 4 |
| baseline, handle column | 3 287 474 | 3 743 856 | 4 |
| after, both arms | ~3 330 000 | 3 743 856 | 4 |

## Reading the numbers

**For a Cypher-driven workload the change costs nothing.** Every Cypher
edge-creation path already called `AddEdgeH` — `exec/create_relationship.go:159`,
`exec/merge_pattern.go:983`, `exec/merge_relationship.go:406` — and a Cypher-built
graph was measured to have the handle column present with **zero** zero-handle
slots. Such a graph was already paying the "with handles" column, so its numbers
are unchanged.

**For a graph built through the Go API the cost is real**: the CSR build goes
from 2.45 ms to 3.33 ms (**+36 %**) and from 2.14 MB to 3.74 MB (**+75 %**) per
build, plus the +7.98 B/edge resident. That reaches `bench/expandinto`,
`bench/cyclicjoin`, the `graph/io` loaders and most of `examples/`.

**It is a transitional cost, and it is on the structure #2317 is removing.** The
CSR carries a handle column only because the CSR is what the read path expands
over. Once relationship adjacency resolves per row at execution time — the
remainder of this task — the handle is read from the adjacency slot the row is
already looking at, and the CSR's copy of it stops being on the read path.

The stage was not deferred until then because the identity is a *precondition*
for per-row resolution: without it there is nothing for a per-row Expand to emit
as the relationship's id.

## Not measured here

`bench/cypher_scale`, `bench/cypher_ldbc`, `bench/expandinto` and
`bench/cyclicjoin` under benchstat with n ≥ 10, which acceptance criterion 4
requires for the read path. Those belong with the per-row resolution change that
alters the read path itself; this stage changes storage only, and the CSR-build
figures above bound its effect.
