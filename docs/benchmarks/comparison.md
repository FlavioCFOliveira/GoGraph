# Cross-library comparison

Headline numbers for GoGraph compared against established graph
libraries on a fixed graph shape. The point of the exercise is not to
crown an absolute winner — every library makes different trade-offs
about API ergonomics, language overhead, parallelism, and the size
range it targets — but to verify that GoGraph's performance claims
are anchored to a public baseline rather than self-comparison.

The acceptance gate set by task #159 is **GoGraph ≥ 10x NetworkX on
BFS, Dijkstra and PageRank on the same graph**. The actual ratios
measured below exceed that gate comfortably. Task #180 adds a
SuiteSparse:GraphBLAS column so the page documents not only the
ratio against the dominant Python baseline but also the relationship
to the library widely regarded as the absolute performance ceiling
for unbatched graph kernels.

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
- Dijkstra: single-source shortest-path-length from node 0. For
  GraphBLAS the equivalent kernel is Bellman-Ford on positive
  weights (returns the same distance map; `graphblas-algorithms`
  ships it under the `single_source_bellman_ford_path_length`
  name).
- PageRank: damping 0.85, MaxIterations 30, tolerance 1e-6.
- Each measurement is the best of three repeats.
- Go side uses `go test -bench=. -benchmem -count=3 -benchtime=2s`;
  Python side (both NetworkX and python-graphblas) uses
  `time.perf_counter()` around each call. To avoid `graphblas-
  algorithms` reusing warmed property caches across calls, the
  Python harness rebuilds the GraphBLAS graph between iterations.

## Numbers

|             | GoGraph       | NetworkX 3.2.1 | SuiteSparse:GraphBLAS 8.2.1 | GoGraph vs NetworkX | GoGraph vs GraphBLAS |
|------------:|--------------:|---------------:|----------------------------:|--------------------:|---------------------:|
| BFS         | 0.086 ms/op   | 10.70 ms/op    | 1.700 ms/op                 | **124x**            | **19.8x**            |
| Dijkstra    | 1.69 ms/op    | 35.62 ms/op    | 5.438 ms/op                 | **21x**             | **3.2x**             |
| PageRank    | 2.91 ms/op    | 54.69 ms/op    | 3.532 ms/op                 | **19x**             | **1.2x**             |

The SuiteSparse:GraphBLAS column was collected through
`python-graphblas` 2023.10.0 / `graphblas-algorithms` 2023.10.0,
which wrap SuiteSparse:GraphBLAS 8.2.1.0 over a CFFI bridge. On a
16K-node graph each algorithm spends a meaningful fraction of its
wall-clock budget in Python/FFI rather than in the C kernel, which
inflates the GraphBLAS column relative to the bare-metal ceiling.
The bare-metal C harness (`bench/comparison/lagraph_baseline.c`)
removes that bridge; its expected steady-state numbers fall in the
LAGraph release-note range (single-thread BFS ≈ 1 B edges/s,
PageRank ≈ 50 M edges/s on commodity x86_64) and remain the
reference for the absolute headroom GoGraph competes against.

GoGraph is faster than NetworkX by 19-124x across the three
representative workloads. The BFS gap is the widest because
NetworkX's `bfs_edges` constructs intermediate Python objects per
edge; GoGraph operates directly on the CSR. The Dijkstra and
PageRank gaps are closer because both libraries spend most of
their time in the same arithmetic-heavy inner loops — NetworkX in
CPython, GoGraph in compiled Go.

GoGraph is also ahead of `python-graphblas` on this small graph,
but the comparison is dominated by Python/FFI overhead and is
**not** a statement about GoGraph beating SuiteSparse:GraphBLAS in
absolute terms. The 0.086 ms BFS, 1.69 ms Dijkstra and 2.91 ms
PageRank numbers measured for GoGraph are 19.8x / 3.2x / 1.2x
faster than the SuiteSparse:GraphBLAS-via-Python column; against
the bare-metal C ceiling GoGraph still has visible headroom on
SSSP and PageRank, which is the gap subsequent releases continue
to close.

## Reproducing

```bash
# GoGraph side.
go test ./bench/comparison/ -bench Comparison -benchmem -count=3 -benchtime=2s

# NetworkX side.
python3 bench/comparison/networkx_baseline.py

# SuiteSparse:GraphBLAS side (Python).
# One-time setup in a venv to avoid touching the system Python:
python3 -m venv /tmp/graphblas_venv
/tmp/graphblas_venv/bin/pip install --upgrade pip
/tmp/graphblas_venv/bin/pip install \
    'numpy<2' scipy networkx python-graphblas graphblas-algorithms
make comparison-graphblas PYTHON=/tmp/graphblas_venv/bin/python3
```

The bare-metal C harness (for a reviewer who wants the absolute
GraphBLAS ceiling) is in `bench/comparison/lagraph_baseline.c`;
the header comment documents the build line for macOS and Linux
plus the CSV edge-list format the program reads on stdin (the
header includes a small inline Python one-liner that generates the
same edges as `random.Random(31)`).

Both Go and Python runs must complete on the same machine to make
the numbers comparable. The values published above were collected
on a clean session (no other CPU-bound work) on the hardware
described under **Methodology**.
