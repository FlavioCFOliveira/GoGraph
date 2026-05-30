# Example 18 — Out-of-core pipeline

## What it demonstrates

The full out-of-core (Tier 2) flow: ingest a graph from a CSV stream,
freeze it into an immutable CSR snapshot, persist it as a memory-mapped
`csrfile`, re-open that file via `mmap` with a sequential-access hint,
and run semi-external BFS and PageRank directly over the mapped region —
without holding the adjacency list in memory.

## Domain / scenario

A tiny seven-edge social graph of five people (`alice`, `bob`, `carol`,
`dave`, `erin`) read from an inline CSV edge list:

```
alice -> bob    alice -> carol
bob   -> carol  bob   -> dave
carol -> dave
dave  -> erin
erin  -> alice
```

The CSV reader interns each name to a compact `NodeID` and returns an
in-memory adjacency list. That list is built into a CSR snapshot and
written to a Tier 2 `csrfile` in a temporary directory. The file is then
re-opened through `mmap`: from this point the graph is queried straight
out of the mapped pages, and the original adjacency list (and its
string ↔ `NodeID` mapper) is discarded.

This is why the example **captures `alice`'s `NodeID` before the build**:
the `csrfile` preserves `NodeID`s across the `mmap` boundary but drops
the string-to-`NodeID` mapping, so the BFS needs a known numeric seed.
Here `alice` interns to `NodeID 7`, and that seed drives the traversal.

## How to run

```sh
go run ./examples/18_oocore_pipeline
```

## Expected output

The `csrfile` is written under an `os.MkdirTemp` directory whose absolute
path differs on every run, so the report prints only the file's **base
name** (`graph.csr`). Every other line is deterministic:

```
CSV: 7 edges ingested.
Wrote graph.csr (7 edges).

Semi-external BFS from alice (NodeID 7):
  visited 5 nodes.
Semi-external PageRank converged in 58 iterations (5 live ranks).
```

## Key APIs

- `graph/io/csv.ReadInto` / `csv.DefaultOptions` — stream an edge-list CSV into an in-memory adjacency list and report the edge count.
- `graph/adjlist.AdjList.Mapper` — capture the seed `NodeID` for `alice` before the mapper is discarded.
- `graph/csr.BuildFromAdjList` — freeze the adjacency list into an immutable CSR snapshot.
- `store/csrfile.WriteToFile` — atomically persist the CSR snapshot as a Tier 2 memory-mapped file.
- `store/csrfile.Open` / `csrfile.Reader.SetHint` / `csrfile.AccessSequential` — re-open the file via `mmap` and hint a sequential access pattern.
- `search/extern.BFS` — semi-external breadth-first traversal over the mapped region from a seed `NodeID`.
- `search/extern.PageRank` / `extern.DefaultPageRankOptions` — semi-external PageRank over the mapped region.

## Further reading

- [`store/csrfile`](../../store/csrfile) — the Tier 2 memory-mapped CSR file format
- [`search/extern`](../../search/extern) — semi-external traversal and analytics over mapped graphs
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot that backs the file
- [`graph/io/csv`](../../graph/io/csv) — the CSV edge-list reader
- [Example 05 — out of core](../05_out_of_core) — the simpler out-of-core building block this example extends
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
