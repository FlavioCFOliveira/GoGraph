# NodeID packing, `MaxNodeID()`, and live-node enumeration

This document explains the relationship between user-supplied node
keys (the `N comparable` type parameter), the internal `graph.NodeID`
space, and how analytical algorithms expose results in that space.

## How NodeIDs get assigned

`graph.Mapper[N]` interns user keys into compact `NodeID` values. To
keep concurrent inserts contention-free, the mapper is sharded into
**256 stripes** by `maphash(N) & 0xFF`; the shard index is encoded in
the top byte of the resulting NodeID.

A consequence: NodeIDs are *not* assigned densely starting at 0. The
first interned key lands in some stripe — say stripe 17, position 0 —
and gets `NodeID(17 << 56)`. The second goes to whichever stripe its
hash routes to. After a handful of insertions the NodeID space is
sparse.

`graph.Mapper.MaxNodeID()` returns the smallest NodeID strictly
greater than every NodeID actually assigned. On a graph built from
**5 unique keys**, `MaxNodeID()` will commonly round up to a number
in the low hundreds (16, 32, 256, ...) because at least one shard
needed an entry past its first slot, and `MaxNodeID()` reports the
upper bound of the packed space.

A NodeID `id` with `id < MaxNodeID()` is **not necessarily live** —
it may be a "ghost slot" within a shard whose intervening positions
were never filled.

## What this means for algorithms

Algorithms that allocate per-NodeID buffers (rank vectors, community
ID slices, distance matrices) use `MaxNodeID()` as the buffer length.
Ghost slots receive a sentinel value:

| Algorithm                                  | Output type                                   | Ghost-slot value |
|--------------------------------------------|-----------------------------------------------|------------------|
| `centrality.PageRank`                      | `[]float64`                                   | `0.0`            |
| `centrality.PersonalisedPushPageRank`      | `[]float64`                                   | `0.0`            |
| `extern.PageRank`                          | `[]float64`                                   | `0.0`            |
| `community.Leiden.Community`               | `[]int`                                       | `-1`             |
| `community.LabelPropagation.Community`     | `[]int`                                       | `-1`             |
| `search.APSP.At(i, j)`                     | `(W, bool)`                                   | `(zero, false)`  |

## Reading back live results

Use `graph/csr.CSR.LiveMask`, `LiveNodes`, or `LiveCount` to
enumerate only the NodeIDs that participate in at least one edge:

```go
mask := c.LiveMask()                                   // []bool of length MaxNodeID()
for id, alive := range mask {
    if !alive {
        continue
    }
    fmt.Println(id, ranks[id])
}
```

```go
ids := c.LiveNodes()                                   // sorted []NodeID
for _, id := range ids {
    fmt.Println(id, p.Community[id])
}
```

```go
liveCount := c.LiveCount()                             // O(V+E) cardinality
fmt.Printf("PageRank produced %d live ranks\n", liveCount)
```

## Translating back to user keys

The user-key value associated with a live NodeID is recovered through
the originating `Mapper`:

```go
mapper := adjlist.Mapper()
for _, id := range c.LiveNodes() {
    key, _ := mapper.Resolve(id)
    fmt.Println(key, ranks[id])
}
```

`Mapper.Resolve(id)` is a constant-time lookup; pre-allocating a
`[]N` of length `LiveCount()` and filling it once is the
recommended pattern when post-processing many algorithms over the
same CSR.

## Example walkthrough

A 5-node directed cycle built via `int` keys:

```go
a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
for i := 0; i < 5; i++ {
    a.AddEdge(i, (i+1)%5, struct{}{})
}
c := csr.BuildFromAdjList(a)
ranks, _, _ := centrality.PageRank(c, centrality.DefaultPageRankOptions())
```

`c.MaxNodeID()` is **256** here (the 5 ints distribute into 5 different
shards, each landing at position 0; MaxNodeID rounds to the top of the
highest occupied shard).

`len(ranks)` is **256**, but only **5** entries carry non-zero values.
Reading `for i, r := range ranks { fmt.Println(i, r) }` prints 256
lines, most of them `0.0`. To print the meaningful output, walk via
`c.LiveNodes()` or the Mapper.

This is *expected* behaviour, not a bug: it is the price of the
allocation-free sharded Mapper. The
`examples/08_pagerank`, `examples/09_leiden`,
`examples/18_oocore_pipeline`, and `examples/20_concurrent_reads`
programs all demonstrate the live-NodeID iteration pattern.
