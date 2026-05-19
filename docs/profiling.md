# Profiling GoGraph

This guide walks through capturing CPU and heap profiles for the
hot algorithms in `search/`, `search/centrality/`, `search/flow/`,
`store/csrfile/`, and the LDBC / DIMACS9 harnesses.

## Capturing a profile

Every benchmark in the project is a standard `go test -bench`
entry point. To capture a CPU profile of any benchmark:

```sh
go test -bench=BenchmarkDijkstra_RandomGraph \
        -cpuprofile=cpu.pprof \
        -benchtime=2s \
        ./search/...
```

For heap (allocation) profiles:

```sh
go test -bench=BenchmarkAdjList_AddEdge_Million \
        -memprofile=mem.pprof \
        -benchtime=2s \
        ./graph/adjlist/...
```

Inspect with `go tool pprof`:

```sh
go tool pprof -http=:0 cpu.pprof
```

## Headline hot paths (Apple M4, Go 1.26.3)

| Site                                  | Cost                       |
|---------------------------------------|----------------------------|
| `graph.Mapper.Intern` (hot key)       | 17 ns/op, 0 allocs         |
| `adjlist.HasEdge` hot cache           | 49 ns/op, 0 allocs         |
| `adjlist.AddEdge` million-node graph  | 423 ns/op, 2 allocs (COW)  |
| `csr.NeighboursByID`                  | 10.6 ns/op, 0 allocs       |
| `csr.BuildFromAdjList` over 10^7 edges| ~51 ms                     |
| `search.BFS` over 10^7-node chain     | 38 ms, 1.25 MB, 0 allocs   |
| `search.Dijkstra` 1M nodes / 4M edges | ~320 ms                    |
| `wal.Encode` 4 KiB frame              | 4.9 GB/s                   |
| `wal.Reader.Replay`                   | 3.95 GB/s                  |
| `csrfile.Reinterpret[uint64]`         | 1.31 ns/op, 0 allocs       |

## Optimisation pass discipline

Per `CLAUDE.md` mandates:

- Every optimisation must show a measured improvement via
  `benchstat`; regressions require an explicit justification or
  must be reverted.
- `make bench` runs the project's headline benchmarks; couple it
  with `benchstat old.txt new.txt` to verify each change.

The `simplify` skill (see `.claude/skills/`) is the recommended
helper for review-then-apply optimisation rounds.

## Sample workflow

```sh
go test -bench=. -benchmem -count=10 ./search/... > /tmp/base.txt
# ... apply change ...
go test -bench=. -benchmem -count=10 ./search/... > /tmp/exp.txt
benchstat /tmp/base.txt /tmp/exp.txt
```

If a row shows a meaningful `~` or `-` delta with `p < 0.05`, the
change is accepted; otherwise it stays in a branch until the
benchmark gap is closed.
