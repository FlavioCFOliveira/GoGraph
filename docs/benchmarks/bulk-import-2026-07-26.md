# Bulk import: end-to-end load time

**rmp #2180** · sprint 318 · 2026-07-26 · Apple M4 (10 cores, 32 GB), Go 1.26.5

Measured through the real command line — `gograph-import` reading CSV files from
disk — not through a library benchmark, because the point of the task was that a
user can load data without dropping to the Go API.

## Dataset

The round-3 comparative audit's own shape, so the numbers sit beside the ones it
recorded: **20 000 nodes, 200 000 edges**. Every node carries a label and a
property; every edge carries a type, a weight and a property. Input is 458 KB of
`nodes.csv` and 5.96 MB of `edges.csv`; the resulting store is 16 MB.

## Result

Five consecutive runs, each into a fresh directory:

| Run | Import phase | Process wall clock |
|---|---|---|
| 1 | 226 ms | 0.57 s (cold page cache) |
| 2 | 232 ms | 0.28 s |
| 3 | 233 ms | 0.28 s |
| 4 | 237 ms | 0.29 s |
| 5 | 235 ms | 0.29 s |

**Import phase: 233 ms median, 0.86 M edges/s.** The process wall clock adds CSV
parsing and process startup and settles at **0.28 s**.

## Beside the audit's recorded figures

| Target | Load time | Ratio |
|---|---|---|
| **GoGraph `gograph-import` (this)** | **0.28 s** | — |
| Memgraph 2.22 | 0.977 s | 3.5× slower |
| Neo4j 5.26 Community | 2.39 s | 8.5× slower |
| GoGraph Cypher write path | 35 m 33 s | **7 618× slower** |

The audit's 2 184× deficit against Memgraph on this dataset is closed.

## What this comparison does and does not establish

**It does** establish that the user-visible problem is fixed. Before this sprint,
the only way to load 200 000 edges into GoGraph was the Cypher write path, at
35 m 33 s. There is now a route that takes 0.28 s, and the audit's headline
deficit no longer exists.

**It does not** establish that GoGraph's bulk loading beats Neo4j's or
Memgraph's bulk loading. The 0.977 s and 2.39 s figures are the audit's
measurements of an **online transactional** load — `UNWIND` batches over Bolt —
while this is an **offline** import that owns the store directory exclusively and
publishes a snapshot. Both incumbents also ship offline importers
(`neo4j-admin database import`, Memgraph's snapshot load) which were not measured
and would very likely beat their own Bolt figures.

The comparison is nevertheless the right one for the deficit, because it is the
same comparison the audit made: it measured what a user actually had to do to load
data. What changed is that GoGraph now has the offline option the incumbents
always had.

The GoGraph Cypher write path is unchanged by this sprint and remains the
2 184×-slower number for an online transactional load. Sprints 319 and 320 are
what address that.

## Reproducing

```sh
go build -o gograph-import ./cmd/gograph-import
gograph-import -store ./mystore -nodes nodes.csv -edges edges.csv -node-labels kind
```

The store directory must be absent or empty. The resulting directory opens with
`recovery.Open` and needs no write-ahead log: the published snapshot is
self-sufficient, which the round-trip tests assert with `WALOps == 0`.

## Durability

The whole import is atomic and nothing else about it is. See the contract on
`bulkimport.Publish` and the command's own documentation: the snapshot is
assembled under `<store>/snapshot.tmp` and renamed into place, so the store either
has no snapshot or a complete one; a crash before the rename leaves the store
exactly as it was, with the partial assembly invisible to recovery and removed by
it. It is not a transaction, has no per-record durability acknowledgement or
resumption point, and is concurrent with nothing.

## Component breakdown

From the #2177 spike, on a slightly lighter property set:

| Step | Measured |
|---|---|
| Build the graph with labels and properties | 2.068 M edges/s (`BenchmarkImport_LabelsAndProperties`) |
| Write and publish the snapshot | 52.8 ms |
| Reopen through `recovery.Open` | 73.1 ms |

The 233 ms measured here exceeds the spike's 126 ms projection because this
dataset carries more properties per element — two per node and three per edge
against one each — and the resulting store is 16 MB rather than 5.5 MB, so both
the build and the fsync of the published files do more work. The projection was
for the lighter shape and is not contradicted; it was simply not the shape a user
loads.
