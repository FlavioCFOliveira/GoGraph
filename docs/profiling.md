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

## GC tuning for read-heavy workloads

Under sustained concurrent read load the per-row allocation rate of the
scan/projection path drives the Go runtime's page **scavenger**: each GC
cycle frees the per-row garbage and `madvise(2)`s the freed pages back to the
OS, which then fault them back in on the next batch. The 2026-06-24
performance audit measured `runtime.madvise` at **~29 %** of CPU on a
concurrent per-row filter at `GOMAXPROCS=10` — a co-cause of the read-path
multicore plateau.

The primary cure is **allocating less** (the sprint S-PA7 read-path work:
lock-free copy-on-write registries, the `ParallelScanProject` per-worker row
arena, and reusing the result-row field in `ResultSet.Next`), which removes
the garbage at the source. GC tuning is the complementary **deployment**
lever.

GoGraph deliberately **does not set any process-global GC state itself**: a
library that called `debug.SetGCPercent` / `debug.SetMemoryLimit` from a
constructor would silently override the embedding process's policy — the
kind of hidden global state the reliability mandate forbids. The knobs belong
to the application that owns the process. For a steady read-heavy deployment:

- **`GOMEMLIMIT`** — set a soft memory ceiling (the `GOMEMLIMIT` env var, or
  `runtime/debug.SetMemoryLimit`) at ~70–80 % of the container/RAM budget.
  With a soft limit the scavenger keeps the heap resident up to the limit
  instead of returning pages every cycle, so the `madvise` traffic collapses
  while resident memory stays bounded (predictable degradation under
  pressure).
- **`GOGC`** — raise it (for example `GOGC=200` or higher, via the env var or
  `runtime/debug.SetGCPercent`) so the heap grows further between
  collections: fewer GC cycles and less scavenging. Always pair it with
  `GOMEMLIMIT` so memory still degrades predictably rather than growing
  unbounded.

Set both once at process startup. Confirm the effect by re-running the read
workload under `GODEBUG=gctrace=1` and checking that `runtime.madvise` falls
in a fresh CPU profile.

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

## Goroutine labels (pprof)

CLAUDE.md requires every long-lived goroutine to be observable. The
Bolt server (`bolt/server.handleConn`) labels its per-connection
goroutine via `pprof.SetGoroutineLabels`. In a pprof goroutine dump
or `go tool pprof` listing, those goroutines appear under:

| Label key  | Value(s)                |
|------------|-------------------------|
| component  | `bolt-server-conn`      |
| remote     | the client's remote addr |

Inspecting them under load:

```sh
# Live server:
curl -s 'http://localhost:6060/debug/pprof/goroutine?debug=2' \
  | grep -A1 'component=bolt-server-conn'

# Or via go tool pprof:
go tool pprof -labels 'component=bolt-server-conn' \
  http://localhost:6060/debug/pprof/goroutine
```

When adding a new long-lived goroutine to the project, label it the
same way — the key/value pair should make the source code site
self-evident from the dump.
