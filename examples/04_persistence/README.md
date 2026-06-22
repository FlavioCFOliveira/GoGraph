# Example 04 — Persistence and recovery

## What it demonstrates

The full GoGraph durability path on a real directory, at a configurable,
reproducible scale: build a seeded graph entirely through **WAL-committed
transactions**, take a **v2 snapshot** (CSR + `labels.bin` + `properties.bin`),
then drop every in-memory reference and rebuild the graph from disk with
`recovery.Open` — proving that node and edge topology, labels, and typed
properties all survive a restart.

## Domain / scenario

A software-supply-chain graph, the kind a package registry maintains:

```
(:Package {name, language, downloads})
(:Release {coord, version, published})
(:Package)-[:PUBLISHED  {weight}]->(:Release)
(:Release)-[:DEPENDS_ON {constraint, weight}]->(:Package)
```

A seeded generator (`math/rand` from `-seed`) creates `-packages` packages,
each owning exactly one release (`coord = name@version`), and gives every
release a random number of `DEPENDS_ON` edges — in `[-deps-min, -deps-max]` —
to other packages. Every node carries typed properties of three kinds (string
`name`/`coord`, int64 `downloads`, timestamp `published`), and every edge
carries a durable int64 weight plus, for `DEPENDS_ON`, a semver `constraint`
string. Fixing `-seed` fixes the data shape — and therefore the recovered
counts and sampled values — exactly.

Every mutation is written **inside a committed transaction**, so the whole
graph travels through the WAL (`OpAddEdgeWeighted`, `OpSetNodeProperty`,
`OpSetEdgeProperty`, …) and is replayed on recovery; the v2 snapshot is the
checkpoint that lets recovery start from a compacted base. The store is written
under a directory created with `os.MkdirTemp`, so its absolute path differs on
every run; that path is deliberately kept out of stdout, so the deterministic
report below is stable.

## How to run

```sh
go run ./examples/04_persistence                       # small deterministic default
go run ./examples/04_persistence -packages 200000 -seed 7  # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Large |
|---|---|---|---|
| `-packages` | number of `Package` nodes (each owns one `Release`) | `300` | `200000` |
| `-deps-min` | minimum `DEPENDS_ON` out-degree per release | `2` | `2` |
| `-deps-max` | maximum `DEPENDS_ON` out-degree per release | `6` | `20` |
| `-batch` | packages processed between context-cancellation checks | `50` | `500` |
| `-seed` | RNG seed (fixes the data shape) | `1` | any |

The default is intentionally modest because persistence I/O is the slow part:
it persists, snapshots and recovers in well under a second, keeping `go test`
inside the short-layer 60 s package budget. The large run is where the
persistence cost — commit throughput, on-disk bytes, recovery wall-clock —
becomes observable.

## Expected output

The deterministic **fact** lines at the default config (`-packages 300
-seed 1`):

```
config.packages=300
config.deps=[2,6]
config.batch=50
config.seed=1
nodes.packages=300
nodes.releases=300
edges.published=300
edges.depends_on=1221
recovered.nodes=600
recovered.edges=1521
recovered.labels=2
recovered.snapshot_hit=true
recovered.sample_name=lib-server-0
recovered.sample_downloads=24941318
recovered.sample_coord=lib-server-0@3.19.1
recovered.sample_published=2021-04-18
```

Interleaved with these, the example prints volatile **telemetry** lines
prefixed with `# `, for example:

```
# commit.elapsed=1.18s
# commit.tx_rate=255 tx/s
# disk.wal_bytes=515.92 KiB
# disk.snapshot_bytes=194.03 KiB
# recovery.elapsed=18.5ms
# mem.heap_after=1.22 MiB
```

The telemetry varies per run and per machine (timings, throughput, on-disk
bytes and heap), and the temp path it persists to is never printed — so only
the bare fact lines are pinned by the regression test.

## Evidence it collects

For the persistence / recovery subject (per the examples standard taxonomy):

- **Commit throughput** — `# commit.tx_rate` (tx/s) and `# commit.edge_rate`
  (edges/s) over the WAL write phase.
- **On-disk footprint** — `# disk.wal_bytes`, `# disk.snapshot_bytes`, and
  `# disk.bytes_per_edge`.
- **Recovery wall-clock** — `# recovery.elapsed`, with `# recovery.wal_ops`
  and the snapshot label/property record counts.
- **Live heap before vs after recovery** — `# mem.heap_before`,
  `# mem.heap_after`, `# mem.heap_growth` (each after a forced GC, so they
  reflect reachable bytes).

When you scale `-packages` up, watch how recovery wall-clock and on-disk bytes
grow against the recovered node/edge counts, and how the snapshot checkpoint
keeps `recovery.wal_ops` replay bounded.

## Key APIs

- `store/wal.Open` — open the write-ahead log that frames committed ops.
- `store/txn.NewStoreWithOptions` / `Store.Begin` / `Tx.Commit` — apply
  transactions to the LPG and append them to the WAL atomically, with a typed
  weight codec so edge weights are durable (`OpAddEdgeWeighted`).
- `store/txn.Tx.SetNodeLabel` / `SetEdgeLabel` / `SetNodeProperty` /
  `SetEdgeProperty` — record labels and typed properties through the WAL.
- `graph/csr.BuildFromAdjList` — freeze the live adjacency list into the CSR
  view the snapshot persists.
- `store/snapshot.WriteSnapshotFull` — write the v2 snapshot (`csr.bin` +
  `labels.bin` + `properties.bin` + `manifest.json`) atomically.
- `store/recovery.OpenCtx` — rebuild the graph from the snapshot plus WAL
  replay; the returned `recovery.Result` reports `WALOps`, `SnapshotHit`,
  `SnapshotLabels`, `SnapshotProperties` and `IsClean`.

## Further reading

- [`store/wal`](../../store/wal) — write-ahead log package documentation
- [`store/snapshot`](../../store/snapshot) — v2 snapshot writer (`labels.bin`, `properties.bin`)
- [`store/recovery`](../../store/recovery) — snapshot-plus-WAL recovery entry point
- [Example 21 — typed recovery](../21_typed_recovery) — a deeper look at typed-property recovery
- [Example 26 — social scale bench](../26_social_scale_bench) — the reference seeded, scale-parametrised example
- [docs/persistence.md](../../docs/persistence.md) — the persistence and recovery design note
- [docs/examples-standard.md](../../docs/examples-standard.md) — the examples standard this example follows
```
