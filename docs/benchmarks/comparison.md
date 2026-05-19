# Cross-library comparison

Headline numbers for GoGraph compared against established graph
libraries on a fixed graph shape. The point of the exercise is not to
crown an absolute winner — every library makes different trade-offs
about API ergonomics, language overhead, parallelism, and the size
range it targets — but to verify that GoGraph's performance claims
are anchored to a public baseline rather than self-comparison.

The acceptance gate set by task #159 is **GoGraph ≥ 10x NetworkX on
BFS, Dijkstra and PageRank on the same graph**. The actual ratios
measured below exceed that gate comfortably.

## Methodology

- Hardware: Apple M4, macOS 15, GOMAXPROCS=10 (Go default).
- Graph: 16 384 nodes, 65 536 directed edges, weights in [1, 100],
  uniform random with PCG seed (31, 1) on the Go side and
  `random.Random(31)` on the Python side. Both sides emit identical
  edge lists modulo the language's RNG (the absolute structure
  differs slightly, but the size, density and degree distribution
  are the same — sufficient for comparing wall-clock per-iteration
  cost).
- BFS: full reachable enumeration from node 0.
- Dijkstra: single-source shortest-path-length from node 0.
- PageRank: damping 0.85, MaxIterations 30, tolerance 1e-6.
- Each measurement is the best of three repeats.
- Go side uses `go test -bench=. -benchmem -count=3 -benchtime=2s`;
  Python side uses `time.perf_counter()` around each call.

## Numbers

|             | GoGraph       | NetworkX 3.2.1 | Speedup |
|------------:|--------------:|---------------:|--------:|
| BFS         | 0.088 ms/op   | 15.64 ms/op    | **178x**|
| Dijkstra    | 1.71 ms/op    | 43.03 ms/op    | **25x** |
| PageRank    | 3.05 ms/op    | 86.63 ms/op    | **28x** |

GoGraph is faster than NetworkX by 25-178x across the three
representative workloads. The BFS gap is the widest because
NetworkX's `bfs_edges` constructs intermediate Python objects per
edge; GoGraph operates directly on the CSR. The Dijkstra and
PageRank gaps are closer because both libraries spend most of
their time in the same arithmetic-heavy inner loops — NetworkX in
CPython, GoGraph in compiled Go.

## SuiteSparse:GraphBLAS

A GraphBLAS comparison is documented but not measured in this
release. The library is the fastest published unbatched-BFS
implementation and would set the absolute lower bound for the
column. Reproducing it requires:

- A C build of SuiteSparse:GraphBLAS 9 or later;
- The same graph fed in through `LAGraph_BreadthFirstSearch`,
  `LAGraph_SingleSourceShortestPath` and `LAGraph_PageRank`;
- An apples-to-apples node-count and edge-count, taken from the
  same RNG seed as above.

The numbers from the LAGraph release notes (single-thread BFS at
~1B edges/s, single-thread PageRank ~50M edges/s on commodity
x86_64) place GraphBLAS comfortably faster than GoGraph on raw
throughput; that headroom is the gap GoGraph closes in subsequent
releases (parallel Brandes #150 was the first deliberate step).

## Reproducing

```bash
# GoGraph side.
go test ./bench/comparison/ -bench Comparison -benchmem -count=3 -benchtime=2s

# NetworkX side.
python3 bench/comparison/networkx_baseline.py
```

Both runs must complete on the same machine to make the numbers
comparable. The values published above were collected on a clean
session (no other CPU-bound work) on the hardware described under
**Methodology**.
