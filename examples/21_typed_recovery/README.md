# Example 21 — Typed recovery

## What it demonstrates

The canonical typed recovery API `recovery.Open[N, W]` applied to a
non-string graph. It persists an `(int64, float64)` weighted directed
graph to a v2 snapshot, drops every in-memory reference, then rebuilds
the graph from disk with the matching codec pair — confirming that
edges (with bit-exact float weights), labels, and typed properties all
survive the round-trip, and that `Result.SnapshotSchemaVersion` reports
the on-disk schema so callers can branch on it without re-opening the
manifest.

## Domain / scenario

A four-hop weighted route `1001 -> 1002 -> 1003 -> 1004`, numeric node
IDs with real-valued edge weights. The three weights are chosen to
stress the IEEE-754 round-trip: an exact integer (`1.0`), a
transcendental (`π`), and a denormal-adjacent value (`1e-300`). Each
edge carries a label (`PRIMARY`, `ALTERNATE`, `DEGRADED`); the origin
node carries a `name` property and the first edge carries
`latency_ms` / `loss` properties. The graph is committed through a
typed `txn.Store`, snapshotted, then recovered into a fresh process
state.

Because the keys are `int64` (not `string`), `WriteSnapshotFull` stamps
a **v2** manifest: a string-keyed graph would additionally emit a
`mapper.bin` and be stamped v3, but a numeric-keyed graph needs no
mapper sidecar, so v2 is the correct and expected on-disk version here.

## How to run

```sh
go run ./examples/21_typed_recovery
```

## Expected output

```
Committed 3 weighted edges; snapshot persisted.
Recovered: WAL ops=9, snapshot hit=true, schema version=v2, label records=6, property records=3.
  recovered 1001 -[PRIMARY]-> 1002  weight=1  (label OK: true, weight bit-exact: true)
  recovered 1002 -[ALTERNATE]-> 1003  weight=3.141592653589793  (label OK: true, weight bit-exact: true)
  recovered 1003 -[DEGRADED]-> 1004  weight=1e-300  (label OK: true, weight bit-exact: true)
  node 1001.name = "origin"
  edge (1001,1002).latency_ms = 0.5
  schema version v2 confirmed (non-string graph: no mapper.bin).
```

The example persists to an `os.MkdirTemp` directory, but that absolute
path is deliberately kept out of stdout so the output stays
byte-stable.

## Key APIs

- `store/recovery.Open[N, W]` / `recovery.Options[N, W]` — recover a typed graph from a WAL + snapshot directory using a codec pair.
- `store/recovery.Result` — recovery report: `WALOps`, `SnapshotHit`, `SnapshotSchemaVersion`, `SnapshotLabels`, `SnapshotProperties`, and the recovered `Graph`.
- `store/txn.NewStoreWithOptions` / `txn.NewInt64Codec` / `txn.NewFloat64WeightCodec` — the typed store and the codecs that make weights round-trip bit-for-bit.
- `store/snapshot.WriteSnapshotFull` — write the full snapshot; stamps v2 for a non-string graph, v3 (with `mapper.bin`) for a string-keyed one.
- `graph/lpg.New` — the in-memory labelled property graph holding nodes, edges, labels, and typed properties.

## Further reading

- [`store/recovery`](../../store/recovery) — the typed recovery package documentation
- [`store/snapshot`](../../store/snapshot) — snapshot format and the v2/v3 manifest distinction
- [`store/txn`](../../store/txn) — the transactional store and codec API
- [Example 04 — persistence](../04_persistence) — the string-keyed persistence round-trip
- [Example 17 — transactional log](../17_transactional_log) — WAL + background checkpoint flow
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
