# Degree primitive — measured

**Task #2218** · sprint 326 · 2026-07-27 · Apple M4 (10 cores) ·
`go test -run '^$' -bench 'BenchmarkOutDegree' -benchmem ./graph/lpg/`

The benchmarks live in `graph/lpg/outdegree_bench_test.go` and are permanent:
they are the regression gate for the O(1) claim this primitive is built on.

This is part 1 of the original #2218, split because the task's own notes said the
two steps are shippable separately. The planner rewrites that turn the measured
88× `COUNT { … } > 0` tax into a degree read are **#2232**; this part only exposes
the primitive they need, and changes no query behaviour.

---

## 1. Why a primitive at all

The adjacency stores each node's out-edges contiguously, so a per-node degree is
O(1) by construction — it was simply never exposed. `grep` for a degree accessor
across `graph/` returned nothing; `AdjList` offered only `Order()` and `Size()`,
both graph-wide.

The round-4 audit measured what its absence costs per outer row, against a bare
label-scan baseline of 0.027 µs: `OPTIONAL MATCH` 16×, `EXISTS { … }` 20×, a
pattern predicate 28×, a list comprehension 65×, and `COUNT { … } > 0` **88×**.
Counting every neighbour in order to compare the count against zero is the
indefensible case.

## 2. O(1) in the node's degree

Graph held at 4 096 nodes; the measured node's degree grows. The enumeration
column is the thing the primitive replaces, not an abstract expectation.

| Node degree | `OutDegree` | enumeration | speedup |
|---|---|---|---|
| 1 | 10.60 ns | 14.62 ns | 1.4× |
| 16 | 10.71 ns | 77.93 ns | 7.3× |
| 256 | 10.45 ns | 1 015 ns | 97× |
| 4 096 | 10.57 ns | 16 427 ns | **1 554×** |

`OutDegree` is flat across a 4 096× range of degree — 10.45 to 10.71 ns, which is
measurement noise — while enumeration is linear in it. Both are zero-allocation;
the win is in the work avoided, not in the garbage avoided.

## 3. O(1) in the graph size

The measured node's degree is held at 8; the graph grows by 256×.

| Graph nodes | `OutDegree` |
|---|---|
| 1 024 | 9.15 ns |
| 16 384 | 10.47 ns |
| 262 144 | 10.49 ns |

Flat, which is the second half of the claim: nothing graph-wide is touched. The
count is one atomic load of the node's adjacency entry followed by a slice length.

## 4. The two paths that are honestly O(d)

Neither is a defect; both are documented on the methods, and they are measured so
the cost is on the record rather than implied.

**Type-filtered.** `OutDegreeByType` must read the relationship-type column to
decide which slots match, so it is linear in the node's degree — still 14× faster
than enumeration at d = 4 096, because it resolves no neighbour keys and allocates
nothing.

| Node degree | `OutDegreeByType` |
|---|---|
| 1 | 10.90 ns |
| 16 | 14.96 ns |
| 256 | 91.16 ns |
| 4 096 | 1 121 ns |

**Tombstone-filtered.** Once the graph holds any tombstone, `lpg.Graph.OutDegree`
must check each far endpoint: 1 941 ns at d = 256 against 10.45 ns on the fast
path. The fast path is therefore worth about 186× on that shape, and it is
selected by one lock-free counter read (`TombstoneCount() == 0`), so a graph that
has never deleted anything never pays.

## 5. Correctness — derived from the traversal, not from arithmetic

A degree is only substitutable for an expansion if it equals the number of rows
that expansion yields, so nothing here is asserted against a hand-computed
constant. Every test compares against the traversal itself.

- `graph/lpg/outdegree_test.go` runs the differential across directed,
  undirected, directed-multigraph and undirected-multigraph, plus a self-loop, an
  isolated node, an uninterned node, and a tombstoned endpoint.
- `graph/lpg/outdegree_property_test.go` generates the graph — size, edge list,
  relationship types, which nodes are tombstoned, and the configuration — and
  asserts the same identity for every node of every generated graph. 2 000
  generated cases pass.

**The property test earned its place immediately: it found a bug in my own
oracle, not in the implementation.** On an undirected multigraph holding one
typed and one untyped edge between the same pair, `Graph.HasEdgeLabel` — a
**per-pair** check — reported the type for *both* edges, so the oracle claimed a
typed degree of 2 where 1 is correct. The engine matches relationship type **per
instance** (the parallel-edge work in #1685 and #2016), so the oracle was
rewritten to walk instances. Had the oracle been trusted, the implementation would
have been "fixed" to be wrong.

**A second real bug the differential caught.** The adjacency column stores
`encodeSlotLabel(id)` — `id + 1`, reserving 0 for "no label" — not the raw
`LabelID`. The first implementation compared a raw id against it, which silently
counted a *different* relationship type: it reported 0 edges of the type that had
two, and 2 of the type that had one. A test written against expected constants
would have been debugged into agreement with the bug; one written against the
traversal simply failed.

## 6. Scope: out-degree only, and why

Undirected graphs mirror insertion, so a node's adjacency already holds every
incident edge and this **is** the full degree. Directed graphs append forward
edges only, and there is no reverse index in `AdjList` — in-edge enumeration is
served by a separately built reverse CSR (`csr.CSR.BuildReverse`).

An in-degree on a directed graph is therefore O(V+E) at this layer, so the
primitive does not offer it: a `direction` parameter that silently full-scanned
would be a footgun in an API whose whole purpose is a constant-time answer. The
consequence for **#2232** is explicit: an incoming pattern `(a)<-[:R]-()` is not
rewritable until a reverse degree source exists.

## 7. Reproducing

```bash
go test -run '^$' -bench 'BenchmarkOutDegree' -benchmem ./graph/lpg/
cd graph/lpg && go test -run TestOutDegreeProperty -rapid.checks=2000 .
```
