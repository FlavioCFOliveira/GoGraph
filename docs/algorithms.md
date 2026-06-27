# Advanced Algorithms

This document indexes every analytical algorithm shipped in
`github.com/FlavioCFOliveira/GoGraph/search` and its subpackages, with a one-line summary,
complexity bound, and pointer to the implementing file. Every
algorithm has unit tests under the same package.

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
| KShortestPathsLoopless   | best-first loopless enumeration (former `EppsteinKShortest`) | `search/kshortest_loopless.go` |
| Floyd-Warshall           | O(V^3) APSP                                      | `search/floyd_warshall.go` |
| Johnson                  | O(V * (V + E) log V) APSP, mixed-sign weights; negative cycles detected | `search/johnson.go`        |

## Connectivity

| Algorithm                 | File                  |
|---------------------------|-----------------------|
| Topological sort (Kahn)   | `search/topo.go`      |
| Tarjan SCC                | `search/tarjan.go`    |
| Hopcroft-Tarjan BCC + bridges + articulation | `search/bcc.go` |
| Hierholzer Eulerian       | `search/hierholzer.go`|

## Matching and flows

| Algorithm                    | File                                |
|------------------------------|-------------------------------------|
| Hopcroft-Karp                | `search/hopcroft_karp.go`           |
| Hungarian (Kuhn-Munkres)     | `search/hungarian.go`               |
| Dinic max-flow               | `search/flow/dinic.go`              |
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

## Supporting data structures

| Structure             | File                       |
|-----------------------|----------------------------|
| Union-Find (DSU)      | `ds/unionfind.go`          |
| APSP matrix wrapper   | `search/floyd_warshall.go` |

## Caveats and v1 limits

- `KShortestPathsLoopless` (previously named `EppsteinKShortest`)
  is a best-first enumeration over the loopless-path tree, not
  the heap-of-heaps construction of Eppstein 1998. The true
  Eppstein algorithm — `O(m + n log n + k)` via the D(G) sidetrack
  graph — is deferred. For sparse graphs with few alternative
  routes `YenKShortest` is typically faster in practice.
- Leiden in v1 is simplified to local moving + connected-
  community split; the refinement and aggregation phases of
  the full paper are deferred.
- Bridge / articulation detection in Hopcroft-Tarjan is
  surfaced through the [`BCCResult`](../search/bcc.go) struct.
