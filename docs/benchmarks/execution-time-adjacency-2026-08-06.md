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

## Not covered here

- `bench/cypher_ldbc` and `bench/cyclicjoin`, the other two suites acceptance
  criterion 4 names.
- `VarLenExpand`, `ExpandIntersect` and `ShortestPath` still receive a pair captured
  at plan-build time; only `Expand` and `OptionalExpand` resolve at execution time so
  far. The visibility matrix above is closed for single-hop traversal; a variable-length
  or shortest-path traversal in a later clause of a writing statement still expands
  over the frozen topology.
- Relationship identity is still the absolute CSR position, not the stable handle
  stage 2a made available. Acceptance criterion 3 is therefore open.
