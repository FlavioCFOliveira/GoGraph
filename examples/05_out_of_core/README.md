# Example 05 — Out-of-core PageRank (Tier 2)

## What it demonstrates

GoGraph's **Tier 2** external-memory storage: persist a CSR snapshot to
disk in the `csrfile` binary format, re-open it by `mmap`'ing the file and
reinterpreting its aligned sections as typed slices in place (no parse, no
heap copy), apply a `SEQUENTIAL` access hint, and run **semi-external
PageRank** that keeps only the rank vector in RAM while streaming the
adjacency from the mapped file each iteration.

## Domain / scenario

A uniform 1000-node directed ring: node `i` points to node `(i+1) mod
1000`, so every node has exactly one in-edge and one out-edge. This shape
is deliberately chosen because its PageRank stationary distribution is
perfectly uniform — every live node holds rank `1/1000 = 0.001`. That
makes the result trivially verifiable: the minimum and maximum live ranks
must be equal, and they must match any sampled node's rank, which proves
PageRank actually ran over the mapped region rather than merely returning a
count.

Note that the rank slice is indexed by `NodeID` and its length is the
sharded, `MaxNodeID`-rounded vertex count (1024 here), so it carries
"ghost" slots with zero rank beyond the 1000 live nodes. The example
counts only the **live** (non-zero) ranks.

## How to run

```sh
go run ./examples/05_out_of_core
```

## Expected output

The csrfile is written under an `os.MkdirTemp` directory whose absolute
path varies per run; that path is intentionally kept out of stdout, so the
report below is byte-stable.

```
Tier 2: wrote graph.csr (1000 vertices, 1000 edges).
PageRank: converged in 1 iteration(s), 1000 live ranks.
Verify: uniform=true, min=max=0.001000, node 0 rank=0.001000 (expected 0.001000).
```

## Key APIs

- `graph/adjlist.New` / `AdjList.AddEdge` — build the mutable directed ring.
- `graph/csr.BuildFromAdjList` — freeze the builder into an immutable Tier 1 in-memory CSR snapshot.
- `store/csrfile.WriteToFile` — persist the CSR atomically as a Tier 2 on-disk file.
- `store/csrfile.Open` / `Reader.SetHint` — mmap the file read-only and hint the OS about the access pattern (`AccessSequential`).
- `search/extern.PageRank` / `extern.DefaultPageRankOptions` — semi-external PageRank over the mmap-backed reader; only the rank vector lives in RAM.

## Further reading

- [`store/csrfile`](../../store/csrfile) — the Tier 2 on-disk CSR format and mmap-backed reader
- [`search/extern`](../../search/extern) — semi-external graph algorithms over a `csrfile.Reader`
- [`graph/csr`](../../graph/csr) — the immutable Tier 1 in-memory CSR snapshot
- [Example 18 — out-of-core pipeline](../18_oocore_pipeline) — CSV ingest plus Tier 2 BFS and PageRank
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
