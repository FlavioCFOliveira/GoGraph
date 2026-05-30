# Example 04 — Persistence and recovery

## What it demonstrates

The full GoGraph durability path on a real directory: commit transactions
through a write-ahead log, attach typed properties to the in-memory graph,
take a **v2 snapshot** (CSR + `labels.bin` + `properties.bin`), then drop
every in-memory reference and rebuild the graph from disk with
`recovery.Open` — proving that both labels and typed properties survive a
restart.

## Domain / scenario

A tiny social graph persisted to a temporary directory. Three transactions
build a `Person` chain wired by relationship edges:

```
alice -[KNOWS]->   bob
bob   -[KNOWS]->   carol
carol -[FOLLOWS]-> dave
```

Before snapshotting, typed properties are attached out-of-band (they travel
through `properties.bin`, not the WAL): `alice` gets a `name` (string), an
`age` (int64), and a `joined` (timestamp), and the edge `alice -> bob` gets
`since` (string) and `weight` (int64). After the "restart", the example
reads each label and property back to confirm it round-tripped intact.

The store is written under a directory created with `os.MkdirTemp`, so its
absolute path differs on every run. That path is deliberately kept out of
stdout, which is why the report below is byte-stable.

## How to run

```sh
go run ./examples/04_persistence
```

## Expected output

```
Committed 3 transactions to the WAL.
Typed properties set on alice and edge alice->bob.
v2 snapshot persisted: csr.bin + labels.bin + properties.bin + manifest.json.
Recovered: WAL ops=12, snapshot hit=true, snapshot label records=7, snapshot property records=5.
  recovered alice -[KNOWS]-> bob (src carries "Person")
  recovered bob -[KNOWS]-> carol (src carries "Person")
  recovered carol -[FOLLOWS]-> dave (src carries "Person")
  recovered alice.name = "Alice"
  recovered alice.age = 30
  recovered alice.joined = 2026-05-19T12:00:00Z
  recovered edge(alice,bob).since = "2026"
  recovered edge(alice,bob).weight = 7
```

## Key APIs

- `store/wal.Open` — open the write-ahead log that frames committed ops.
- `store/txn.NewStoreWithCodec` / `Store.Begin` / `Tx.Commit` — apply transactions to the LPG and append them to the WAL atomically.
- `graph/lpg.Graph.SetNodeProperty` / `SetEdgeProperty` with `lpg.StringValue` / `lpg.Int64Value` / `lpg.TimeValue` — attach typed properties to the in-memory graph.
- `graph/csr.BuildFromAdjList` — freeze the live adjacency list into the CSR view the snapshot persists.
- `store/snapshot.WriteSnapshotFull` — write the v2 snapshot (`csr.bin` + `labels.bin` + `properties.bin` + `manifest.json`) atomically.
- `store/recovery.Open` — rebuild the graph from the snapshot plus WAL replay; the returned `recovery.Result` reports `WALOps`, `SnapshotHit`, `SnapshotLabels`, and `SnapshotProperties`.

## Further reading

- [`store/wal`](../../store/wal) — write-ahead log package documentation
- [`store/snapshot`](../../store/snapshot) — v2 snapshot writer (`labels.bin`, `properties.bin`)
- [`store/recovery`](../../store/recovery) — snapshot-plus-WAL recovery entry point
- [Example 21 — typed recovery](../21_typed_recovery) — a deeper look at typed-property recovery
- [docs/persistence.md](../../docs/persistence.md) — the persistence and recovery design note
