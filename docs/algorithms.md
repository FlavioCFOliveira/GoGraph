# Advanced Algorithms

This document indexes every analytical algorithm shipped in
`gograph/search` and its subpackages, with a one-line summary,
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
| Bellman-Ford             | negative weights; negative cycles detected       | `search/bellman_ford.go`   |
| A*                       | admissible heuristic                             | `search/astar.go`          |
| Yen's k-shortest         | sorted by total cost                             | `search/yen.go`            |
| Floyd-Warshall           | O(V^3) APSP                                      | `search/floyd_warshall.go` |
| Johnson                  | O(V * (V + E) log V) APSP, non-negative weights  | `search/johnson.go`        |

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
| PageRank (in-memory power iter)    | `search/centrality/pagerank.go`       |
| Personalised PageRank (push)       | `search/centrality/ppr_push.go`       |
| PageRank (semi-external mmap)      | `search/extern/pagerank.go`           |

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

- Yen's k-shortest is O(k * (V + E) log V); Eppstein's
  algorithm (better k > 1000) is deferred.
- Johnson APSP requires non-negative edges in v1; the Bellman-
  Ford reweighting pass for negative weights is deferred (use
  Floyd-Warshall instead).
- Leiden in v1 is simplified to local moving + connected-
  community split; the refinement and aggregation phases of
  the full paper are deferred.
- Bridge / articulation detection in Hopcroft-Tarjan is
  surfaced through the [`BCCResult`](../search/bcc.go) struct.
