# Example 33 — Generation snapshot-swap (read-mostly MVCC)

## What it demonstrates

The `graph/generation` package: publishing successive immutable CSR snapshots
under a refcount-protected pointer so that concurrent readers always observe a
whole, consistent generation while a new one is swapped in — the read-mostly
equivalent of an MVCC snapshot. Every reader `Acquire`s the current generation,
uses it, and `Release`s it; a publisher `Publish`es the next generation in a
fresh allocation and atomically swaps the pointer; an old generation is
reclaimed only after its last reader has released it.

The example certifies the two guarantees under concurrency (`-race`, goroutine-
leak-checked): **no torn reads** — every read observes a node count equal to one
of the published versions, never a half-built mix — and **correct refcount
accounting** — an `Acquire`/`Release` pair leaves the current generation's
refcount exactly where it started.

## Domain / scenario

A live routing service. Its road network is rebuilt periodically as new roads
open, and each rebuild produces a larger immutable CSR snapshot. Queries must
keep being served against a consistent network throughout the swap, and no
query may ever see a partially-published graph. The example models each version
as a connected ring of `base-nodes + v·growth` intersections; the reader's unit
of work is a BFS reach over whichever version it acquired.

## How to run

```sh
go run ./examples/33_generation_swap                                    # small deterministic default
go run ./examples/33_generation_swap -readers 64 -versions 20 -base-nodes 100000
```

Run the package under the race detector to exercise the concurrency contract:

```sh
go test -race ./examples/33_generation_swap/...
```

## Scale and flags

| Flag | Meaning | Default | Large |
|---|---|---|---|
| `-versions` | successive snapshots to publish | `8` | `20` |
| `-readers` | concurrent reader goroutines | `8` | `64` |
| `-reads-per-reader` | Acquire/read/Release cycles per reader | `200` | `1000` |
| `-base-nodes` | nodes in the first version | `500` | `100000` |
| `-growth` | extra nodes each subsequent version adds | `250` | `10000` |
| `-seed` | RNG seed | `1` | any |

## Expected output

At the default config the deterministic **fact** lines are:

```
config.versions=8
config.readers=8
config.reads_per_reader=200
config.base_nodes=500
generations.published=8
reads.total=1600
reads.all_consistent=true
final.order=2250
final.current_order=2250
refcount.accounted=true
```

Interleaved with the facts are volatile **telemetry** lines, prefixed with
`# `, that vary per run and per machine:

```
# swap.elapsed=3.05ms
# reads.throughput=524053 reads/s
# reads.distinct_generations_observed=2
```

`reads.distinct_generations_observed` is telemetry, not a fact: how many of the
eight versions any given schedule catches depends on timing, but every read is
consistent regardless. `reads.all_consistent=true` and `refcount.accounted=true`
are the headline correctness facts.

## Evidence it collects

For the concurrency subject (per `docs/examples-standard.md`): **aggregate read
throughput** (`# reads.throughput`), the **swap wall-clock** (`# swap.elapsed`),
and the **number of distinct generations observed** (`# reads.distinct_…`). The
correctness evidence is asserted as facts: consistency of every read and correct
refcount accounting.

## Key APIs

- `graph/generation.New` — start a publisher over an initial CSR snapshot.
- `graph/generation.Publisher.Acquire` / `Release` — a reader takes and returns a refcounted handle to the current generation.
- `graph/generation.Publisher.Publish` — atomically swap in a fresh generation; the old one is reclaimed once its refcount drains.
- `graph/generation.Publisher.Current` / `Generation.CSR` / `Generation.Refcount` — inspect the live generation, its snapshot, and its outstanding-reader count.
- `graph/csr.BuildFromAdjList` — build each version's immutable snapshot.

## Further reading

- [`graph/generation`](../../graph/generation) — snapshot-swap package documentation
- [Example 20 — Concurrent reads](../20_concurrent_reads) — the lock-free read contract of a single frozen CSR
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
