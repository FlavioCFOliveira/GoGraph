# Advanced Algorithms

This document indexes every analytical algorithm shipped in
`github.com/FlavioCFOliveira/GoGraph/search` and its subpackages, with a one-line summary,
complexity bound where the implementation states one, and a pointer to
the implementing file. Every algorithm has unit tests under the same
package.

The index covers the algorithms themselves. It deliberately does not
enumerate three classes of supporting exported identifier: the
`Default…Options` constructors that supply each tunable algorithm's
defaults, the network builders `flow.NewNetwork` / `flow.NewCostNetwork`,
and the result types (`APSP`, `Distances`, `Matching`, `BCCResult`,
`MSTEdge`, `Partition`, `TC`, `YenPath`, `Assignment`, `MinCutResult`).
The systematic `…Ctx` / `…Parallel` / `…Into` / `…On` variants are
described once under [Naming conventions for the variants](#naming-conventions-for-the-variants)
rather than repeated per row.

## Traversal

| Algorithm                        | Complexity   | File                              |
|----------------------------------|--------------|-----------------------------------|
| BFS                              | O(V + E)     | `search/search.go`                |
| DFS (iterative)                  | O(V + E)     | `search/search.go`                |
| Bidirectional BFS                | O(b^(d/2))   | `search/bibfs.go`                 |
| Direction-optimising BFS         | O(V + E)     | `search/bfs_do.go`                |

## Shortest paths

| Algorithm                | Notes                                            | File                       |
|--------------------------|--------------------------------------------------|----------------------------|
| Dijkstra (binary heap)   | non-negative weights                             | `search/dijkstra.go`       |
| Bellman-Ford             | negative weights; negative cycles detected; rejects NaN/Inf on float Weight via `ErrInvalidInput` | `search/bellman_ford.go`   |
| A*                       | admissible heuristic                             | `search/astar.go`          |
| Yen's k-shortest         | sorted by total cost                             | `search/yen.go`            |
| KShortestPathsLoopless   | best-first loopless enumeration; still reachable under its former name `EppsteinKShortest`, retained as a deprecated alias | `search/kshortest_loopless.go`, alias in `search/eppstein.go` |
| Bidirectional Dijkstra   | forward search on the CSR, simultaneous reverse search on the transpose; `…On` takes a caller-supplied reverse CSR | `search/bidijkstra.go`     |
| Floyd-Warshall           | O(V^3) APSP                                      | `search/floyd_warshall.go` |
| Johnson                  | O(V * (V + E) log V) APSP, mixed-sign weights; negative cycles detected | `search/johnson.go`        |
| Dijkstra APSP            | Dijkstra from every live vertex; non-negative weights only | `search/johnson.go`        |

## Connectivity

| Algorithm                 | File                  |
|---------------------------|-----------------------|
| Topological sort (Kahn)   | `search/topo.go`      |
| Tarjan SCC                | `search/tarjan.go`    |
| Weakly-connected components (union-find) | `search/wcc.go`, parallel variant in `search/wcc_parallel.go` |
| Hopcroft-Tarjan BCC + bridges + articulation | `search/bcc.go` |
| Hierholzer Eulerian (directed)  | `search/hierholzer.go`|
| Hierholzer Eulerian (undirected, symmetric CSR) | `search/hierholzer_undirected.go` |
| Transitive closure (dense bit-matrix reachability oracle, O(1) point queries) | `search/transitive_closure.go` |

## Structural measures

| Algorithm                 | Notes                                          | File                  |
|---------------------------|------------------------------------------------|-----------------------|
| k-core / coreness         | Batagelj-Zaversnik 2003 bucket-list peeling, O(V + E) | `search/kcore.go`     |
| Triangle counting         | total plus per-NodeID counts over the undirected graph | `search/triangles.go`, parallel variant in `search/triangles_parallel.go` |
| Diameter (iFUB)           | 2-sweep BFS lower bound refined by iFUB; returns `(lo, hi, exact)` | `search/diameter.go`  |

## Minimum spanning trees

| Algorithm                 | Notes                                          | File                  |
|---------------------------|------------------------------------------------|-----------------------|
| Prim                      | binary-heap priority queue, rooted at a caller-supplied source; returns the parent array, a reached mask, and the total weight | `search/prim.go`      |
| Kruskal                   | edge sort plus union-find (`ds/unionfind.go`); returns the `MSTEdge` list and the total weight | `search/kruskal.go`   |

## Matching and flows

| Algorithm                    | File                                |
|------------------------------|-------------------------------------|
| Hopcroft-Karp                | `search/hopcroft_karp.go`           |
| Hungarian (Kuhn-Munkres)     | `search/hungarian.go`               |
| Dinic max-flow (`flow.MaxFlow`) | `search/flow/dinic.go`           |
| Edmonds-Karp max-flow        | O(V * E^2); kept as a reference implementation and property-test baseline; `search/flow/edmonds_karp.go` |
| FIFO push-relabel max-flow   | Goldberg-Tarjan 1988 with the gap heuristic, worst case O(V^2 * sqrt(E)); mutates the network in place; `search/flow/push_relabel.go` |
| Min-cost max-flow            | Successive Shortest Paths over reduced costs; negative arc costs bootstrapped by Bellman-Ford; `search/flow/min_cost.go` |
| Stoer-Wagner global min-cut  | `search/flow/stoer_wagner.go`       |

## Centrality

| Algorithm                          | File                                  |
|------------------------------------|---------------------------------------|
| Brandes betweenness                | `search/centrality/brandes.go`        |
| Weighted Brandes betweenness       | rejects NaN/Inf (`ErrInvalidInput`) and negative weights (`search.ErrNegativeWeight`); see `search/centrality/brandes_weighted.go` |
| PageRank (in-memory power iter)    | `search/centrality/pagerank.go`       |
| Personalised PageRank (push)       | `search/centrality/ppr_push.go`       |
| PageRank (semi-external mmap)      | `search/extern/pagerank.go`           |
| Closeness (Wasserman-Faust)        | BFS per source, disconnected-safe; `search/centrality/closeness.go` |
| Harmonic (Boldi-Vigna)             | sum of 1/d, well-defined on disconnected graphs; `search/centrality/harmonic.go` |
| Eigenvector (power iter, I+A)      | left/in-edge convention; `ErrMaxStepsExceeded` on non-convergence; `search/centrality/eigenvector.go` |
| Katz                               | α auto-bounded by max degree; β baseline; `search/centrality/katz.go` |

The four measures above operate on outgoing edges (closeness/harmonic) or the
left/in-edge convention (eigenvector/Katz); pass `c.BuildReverse()` for the
opposite orientation. On an undirected snapshot both coincide. Eigenvector and
Katz score only participating nodes (≥1 incident edge); isolated/ghost slots
get 0.

## Community detection

| Algorithm                  | File                                       |
|----------------------------|--------------------------------------------|
| Leiden (simplified)        | `search/community/leiden.go`               |
| Label propagation          | `search/community/label_propagation.go`    |

## Tier 2 algorithms

The semi-external variants live under `search/extern/`:

| Algorithm                | File                          |
|--------------------------|-------------------------------|
| BFS over csrfile.Reader  | `search/extern/bfs.go`        |
| PageRank over csrfile    | `search/extern/pagerank.go`   |

## Stateful, reusable engines

Each of these binds to one immutable CSR snapshot and caches the snapshot-derived
working storage, so repeated queries against the same graph skip the one-time
allocations the one-shot function pays every call.

| Engine                            | One-shot counterpart          | File                            |
|-----------------------------------|-------------------------------|---------------------------------|
| `search.SSSP` (`NewSSSP`)         | `search.Dijkstra`             | `search/sssp.go`                |
| `centrality.PageRanker` (`NewPageRanker`) | `centrality.PageRank` | `search/centrality/pagerank.go` |

## Naming conventions for the variants

Almost every algorithm above ships in more than one exported form. The suffixes
are systematic, so they are stated once here instead of being tabulated per
algorithm:

- **`…Ctx`** — the context-aware form. It honours cancellation and deadlines and
  returns an `error` where the plain form cannot. Present for essentially every
  algorithm in `search` and its subpackages.
- **`…Parallel` / `…ParallelCtx`** — a worker-pool form taking `numWorkers`:
  `WCCParallel`, `CountTrianglesParallel`, `FloydWarshallParallel`,
  `JohnsonAPSPParallel`, `centrality.BetweennessParallel`,
  `centrality.WeightedBetweennessParallel`.
- **`…Into`** — the zero-allocation primitive writing into caller-provided
  scratch: `AStarInto`, `DijkstraInto`, `BellmanFordInto`.
- **`…On`** — the form taking a caller-supplied reverse CSR instead of building
  one: `BiBFSOn`, `BidirectionalDijkstraOn`.

## Supporting data structures

| Structure             | File                       |
|-----------------------|----------------------------|
| Union-Find (DSU)      | `ds/unionfind.go`          |
| APSP matrix wrapper   | `search/floyd_warshall.go` |

## Caveats and v1 limits

- `KShortestPathsLoopless` is a best-first enumeration over the
  loopless-path tree, not the heap-of-heaps construction of
  Eppstein 1998. The true Eppstein algorithm —
  `O(m + n log n + k)` via the D(G) sidetrack graph — is deferred.
  For sparse graphs with few alternative routes `YenKShortest` is
  typically faster in practice.
- The former name is **still exported**: `EppsteinKShortest` and
  `EppsteinKShortestCtx` remain in `search/eppstein.go` as thin
  deprecated aliases that forward to `KShortestPathsLoopless` /
  `KShortestPathsLooplessCtx`. Their godoc marks them
  `Deprecated:` and points new code at
  `KShortestPathsLooplessCtxWithOpts` (bounded and cancellable) or
  `YenKShortest` (polynomial, node-simple), because the alias's
  own search is worst-case exponential in V.
- Leiden in v1 is simplified to local moving + connected-
  community split; the refinement and aggregation phases of
  the full paper are deferred.
- Bridge / articulation detection in Hopcroft-Tarjan is
  surfaced through the [`BCCResult`](../search/bcc.go) struct.
