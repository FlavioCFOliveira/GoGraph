# Design: a Cypher-reachable durable bulk-ingest path

**Spike for rmp #2177** · sprint 318 · 2026-07-26 · baseline `8955709`

Every figure below was measured during this spike on an Apple M4, on the audit's
own dataset shape (20 000 nodes, 200 000 edges), not projected. The measurement
harness was temporary and deleted; its results are reproduced here and its
assertions are carried into the implementation tasks.

---

## 1. The problem

`store/bulk` ingests at millions of edges per second and is unreachable from any
user-facing path. The round-3 comparative audit measured the consequence: the
same dataset takes **35 m 33 s** through the Cypher write path, against
**977 ms** for Memgraph and **2.39 s** for Neo4j — a 2 184× deficit that
dominates any first evaluation of the module.

The audit named three walls. All three are confirmed:

| Wall | Confirmed how |
|---|---|
| No Cypher or Bolt hook | `grep -rn 'bulk\.' cypher/ bolt/` returns nothing |
| Output is a `csrfile` no `store.DB` can open | `bulk.Options.OutputPath` is a bare csrfile; recovery reads a snapshot *directory* |
| `bulk.Edge` carries no labels or properties | `Edge{Src, Dst string; Weight int64}` — `store/bulk/bulk.go:68` |

---

## 2. Measurements

All timings are for 20 000 nodes and 200 000 edges.

| # | Step | Measured | Rate |
|---|---|---|---|
| A | `store/bulk` exactly as it ships (adjacency only, csrfile out) | 50.98 ms | **3.92 M edges/s** |
| B | The same graph built as an `lpg.Graph` carrying one label + one property per node **and** per edge | 73.60 ms | **2.72 M edges/s** |
| C | `snapshot.WriteSnapshotFullCtx` of that graph | 52.82 ms | — |
| D | `recovery.Open` of the result, no WAL present | 73.05 ms | — |

A confirms the audit's 4.18 M edges/s figure to within measurement noise.
B is the important one: **carrying labels and properties costs 44 % more time
and nothing else** — the adjacency build is not the only fast part.

**Import cost = B + C = 126.4 ms.** Against the audit's own numbers for the same
dataset:

| | Measured | vs this design |
|---|---|---|
| GoGraph, Cypher write path | 35 m 33 s | **16 875× slower** |
| Neo4j 5.26 | 2.39 s | 18.9× slower |
| Memgraph 2.22 | 977 ms | 7.7× slower |

The deficit does not merely close. It inverts: this design would make bulk
loading GoGraph's **fastest** result against both incumbents, from its worst.

---

## 3. The design

### 3.1 Entry point — offline, Go API, not a Cypher clause

```go
// package store (or store/bulkimport)
func ImportInto(ctx context.Context, dir string, src EdgeSource, opts ImportOptions) (ImportResult, error)
```

`dir` is a **store directory that does not exist or is empty**. The import
assembles a complete graph in memory, publishes it as the store's snapshot, and
returns; the caller then opens `dir` normally.

It is deliberately **not** a Cypher clause and not a Bolt message:

- It needs exclusive ownership of the directory. Publishing a snapshot under a
  live server would race the checkpointer and invalidate open readers' view.
- It is not a transaction. Making `LOAD BULK …` look like one inside a session
  would promise atomicity semantics it does not have (§4).
- Neo4j draws the same line: `neo4j-admin database import` is an offline tool,
  separate from `LOAD CSV`.

What Cypher users get instead is task #2180's remit: a documented, discoverable
route from "I have a CSV" to "the store is loaded", plus the option of a
`LOAD CSV`-shaped online path later for the small-input case. The offline path
fixes the user-visible problem now, without waiting on the planner.

### 3.2 Label and property carriage — build an `lpg.Graph`, do not extend `bulk.Edge`

**This is the spike's main recommendation, and it revises task #2178.**

The obvious reading of #2178 is to add label and property fields to `bulk.Edge`
and thread them through the loader. Measurement B says that is unnecessary work:
`lpg.Graph` **already owns** labels and properties, already has
`AddEdgeLabeledWithProperty`, and already reaches 2.72 M edges/s when driven
inside one adjacency commit window. Extending `bulk.Edge` would reimplement, in a
second place, storage that exists and is tested.

So:

- The importer builds an `lpg.Graph[string, W]` directly, wrapped in a single
  `AdjList().BeginCommit()` / `EndCommit()` window — the same exclusive-build
  mode WAL replay and (since #2170) snapshot recovery use. Outside a window every
  edge clones a shard slot array; inside it, once per shard.
- `store/bulk` is left **untouched**. It keeps its role: the fastest possible
  adjacency-only build for callers who want a Tier-2 csrfile, which is what
  `bench/ldbc` and `bench/rmat` use it for. Nothing regresses.
- The record the importer consumes is a new type in the importer's own package,
  with node labels/properties and edge label/properties, so the two ingest
  shapes evolve independently.

The cost of NOT reusing `bulk.Loader` is its parallel adjacency build. At
2.72 M edges/s the sequential path is already 7.7× faster end-to-end than
Memgraph, so parallelism is a later optimisation with a benchmark to justify it,
not a precondition.

### 3.3 On-disk output — a published snapshot directory, at exactly one path

The importer calls `snapshot.WriteSnapshotFullCtx(ctx, filepath.Join(dir, "snapshot"), csr, g)`.

**The path is not a free choice.** Recovery looks for the snapshot at
`<storeDir>/snapshot` (`store/recovery/recovery.go:882`) and nowhere else. The
spike discovered this the direct way: writing the snapshot to an arbitrary
directory and calling `recovery.Open` on it returned `SnapshotHit=false` and an
empty graph — no error, just silence. Publishing at `<dir>/snapshot` returned
`SnapshotHit=true`, `WALOps=0`, 20 000 nodes.

`WriteSnapshotFull` emits a **self-sufficient v3 snapshot** — verified file set:
`csr.bin`, `labels.bin`, `properties.bin`, `mapper.bin`, `manifest.json`
(5.52 MB for this dataset) — plus an `indexes/` sub-directory when the graph
carries a serialisable index. `mapper.bin` is what makes it self-sufficient: the
durable (NodeID → natural key) table, so no WAL prefix is needed to resolve ids.

Content survival is verified, not assumed: after `recovery.Open`, node label
`Person`, node property `id=7`, edge label `KNOWS` and edge property `since` all
read back correctly, with `WALOps=0` — every byte came from the snapshot.

### 3.4 No WAL is written, and that is a stated precondition

The importer writes **no WAL**. A store whose directory contains a WAL from
earlier writes must therefore not be imported into: recovery would replay that
WAL *on top of* the freshly published snapshot, which is not a merge but
corruption.

The precondition is enforced, not documented-and-hoped: `ImportInto` fails with a
typed error if `dir` exists and is non-empty. Importing into a live or previously
used store is out of scope for this design; a store that must absorb bulk data
into existing content uses the transactional path.

---

## 4. The ACID contract, stated exactly

### What IS atomic

**Publication.** `WriteSnapshotFull` assembles the whole snapshot under
`<dir>/snapshot.tmp` and renames it to `<dir>/snapshot` on success. A rename
within a directory is atomic, so at every instant the store either has no
snapshot or has a complete one. There is no window in which a reader can observe
half a graph.

**The unit of atomicity is the entire import.** Either the store opens with all
of the imported data or with none of it. There is no partial-import state, and no
intermediate state is ever visible.

### What is NOT atomic, and must not be claimed

- **The import is not a transaction.** It has no transaction id, appends no WAL
  record, participates in no isolation level, and cannot be rolled back once
  published. Rolling it back means deleting the directory.
- **There is no durability acknowledgement per edge.** Nothing is durable until
  the publish completes. A caller that streams 10 M edges and crashes at 9 M has
  no partial result and no resumption point; it re-runs the import.
- **It is not concurrent with anything.** No reader, no writer, no checkpointer
  may touch `dir` during the import. This is why the entry point is offline.

### Crash recovery, verified

A crash at any point before the rename leaves `<dir>/snapshot.tmp` and no
published snapshot. Measured behaviour on the next `recovery.Open`:

- the partial assembly is **invisible** — `SnapshotHit=false`, `LiveOrder()=0`,
  and no error; and
- it is **swept** — recovery calls `RemoveAll(snapDir + ".tmp")`
  (`store/recovery/recovery.go:896`, "best-effort: stale staging cleanup"), so
  the next open finds no debris.

A crash after the rename leaves a complete, self-sufficient snapshot that opens
normally. There is no third case: the rename is the commit point.

Durability of the published bytes rests on the snapshot writer's existing
fsync discipline (file fsync, then parent-directory fsync — the same protocol the
checkpointer uses and the crash-injection battery already exercises). This design
adds no new durability mechanism, which is the point: it inherits one that is
already certified.

---

## 5. Recommendation: **GO**

The evidence is unusually clean for a spike of this size:

1. Both components already exist, are already tested, and already have the
   properties needed — `lpg.Graph` owns labels and properties;
   `WriteSnapshotFull` publishes crash-atomically.
2. The end-to-end path was **executed**, not designed on paper: build, publish,
   reopen, verify content — 126.4 ms to import, and the reopened graph carries
   every label and property.
3. The projected result inverts the audit's worst finding into a win: 7.7×
   faster than Memgraph and 18.9× faster than Neo4j on the dataset where GoGraph
   was 2 184× slower.
4. The ACID story is honest and narrow: one atomic publish, no transaction
   semantics claimed, crash behaviour measured rather than argued.

### Consequences for the rest of sprint 318

- **#2178** ("carry labels and properties through the bulk ingest
  representation") should be re-scoped: do **not** extend `bulk.Edge`. Build into
  `lpg.Graph`, which already carries both, and leave `store/bulk` untouched so
  its existing consumers do not regress. The task's intent — that the ingest
  path carries labels and properties — is met; its assumed mechanism is not the
  cheapest one.
- **#2179** ("emit a `store.DB`-openable artefact") is largely discharged by this
  spike's finding: the artefact is a `WriteSnapshotFull` directory published at
  `<dir>/snapshot`. What remains is the enforced empty-directory precondition and
  the typed errors.
- **#2180** (expose to users and benchmark end to end) is unchanged, and should
  benchmark against the recorded Neo4j and Memgraph numbers rather than only
  against GoGraph's own before-state.

### What this design deliberately does not do

- It does not make `LOAD CSV` fast. That is an online, transactional path with
  different semantics; this is the offline import.
- It does not import into an existing store.
- It does not parallelise the build. 2.72 M edges/s is already sufficient, and a
  parallel variant needs a benchmark to justify its complexity.
- It does not add a Cypher clause. If one is wanted later, it must answer who may
  invoke it and what happens to concurrent sessions — questions this design does
  not need to answer because its entry point is offline.
