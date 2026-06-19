# Example 21 — Typed recovery

## What it demonstrates

Durable recovery of a typed `(int64, float64)` graph through the canonical
`recovery.Open[N, W]` path. It builds a seeded, scale-parametrised weighted
network with numeric node IDs and real-valued edge weights, persists it to a
v2 snapshot, drops every in-memory reference, then rebuilds the graph from
disk with the matching codec pair — confirming that edges (with **bit-exact**
float64 weights), labels, and four kinds of typed property all survive the
round-trip, and that `Result.SnapshotSchemaVersion` reports the on-disk schema
so callers can branch on it without re-opening the manifest. Because it is a
persistence example, it also reports the evidence that matters for the subject:
on-disk snapshot bytes, recovery wall-clock, and live heap before versus after
recovery.

## Domain / scenario

A weighted **routing network**: numeric `int64` station IDs (counting up from a
fixed base so the int64 codec sees realistic, large keys) and real-valued
`float64` edge distances in kilometres. A fraction of the nodes are promoted to
`:HUB` (interchanges); the rest are `:STATION`. Roads are directed out-edges —
each node connects to a random fan-out of distinct other nodes — and carry a
road-class label (`:HIGHWAY` / `:REGIONAL` / `:LOCAL`).

```
(:STATION|:HUB {name, zone, elevation_m})
(:STATION|:HUB)-[:HIGHWAY|:REGIONAL|:LOCAL {distance_km, lanes, toll}]->(:STATION|:HUB)
```

Every node carries three typed properties — `name` (string), `zone` (int64),
`elevation_m` (float64) — and every edge three more — `distance_km` (float64,
**equal to the edge weight**), `lanes` (int64), `toll` (bool). The float64
weights are drawn to exercise the IEEE-754 round-trip: most are
transcendental-scaled (full 52-bit mantissa), with a guaranteed exact integer
and a guaranteed denormal-adjacent (`1e-300`) value sprinkled in, so the
bit-exact check has something non-trivial to verify. A node and all its
out-edges are committed in a single transaction through a typed `txn.Store`;
the graph is then snapshotted and recovered into fresh process state.

Because the keys are `int64` (not `string`), `WriteSnapshotFull` stamps a
**v2** manifest: a string-keyed graph would additionally emit a `mapper.bin`
and be stamped v3, but a numeric-keyed graph needs no mapper sidecar, so v2 is
the correct and expected on-disk version here.

## How to run

```sh
go run ./examples/21_typed_recovery                            # small deterministic default
go run ./examples/21_typed_recovery -nodes 500000 -fanout-max 12 -seed 7  # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Large value |
|---|---|---|---|
| `-nodes` | number of nodes (`:STATION` + `:HUB`) | `256` | `500000` |
| `-fanout-min` | minimum out-degree per node | `3` | `3` |
| `-fanout-max` | maximum out-degree per node | `6` | `12` |
| `-hub-frac` | fraction of nodes promoted to `:HUB`, in `[0,1]` | `0.1` | `0.1` |
| `-seed` | RNG seed (fixes the data shape exactly) | `1` | `7` |

The default is tiny so `go test` stays far under the 60 s short-test budget;
the large run is where the snapshot footprint and recovery cost become
observable.

## Expected output

```
config.nodes=256
config.fanout=[3,6]
config.seed=1
recovered.nodes=256
recovered.edges=1139
recovered.label_records=1395
recovered.property_records=4185
recovered.schema_version=v2
weights.verified=1139
weights.bit_exact=true
sample.node_name=Northmoor
sample.node_zone=8
sample.edge_distance_bits=0x40629773ae3dcb2a
sample.edge_toll=false
# build.elapsed=1.58s
# snapshot.write_elapsed=26ms
# disk.snapshot_bytes=178.20 KiB
# disk.wal_bytes=119.05 KiB
# recovery.elapsed=2.2ms
# recovery.ops=2790
# mem.heap_before_recovery=273.26 KiB
# mem.heap_after_recovery=1015.66 KiB
# mem.heap_growth=742.40 KiB
```

The bare lines are deterministic **facts** pinned by the regression test;
`recovered.property_records` is `256*3` node properties plus `1139*3` edge
properties. The lines prefixed with `# ` are volatile **telemetry** —
durations, on-disk bytes, and heap — that vary per run and per machine and are
never pinned. The example persists to an `os.MkdirTemp` directory removed on
exit, and that absolute path is deliberately kept out of the report so the
deterministic output stays byte-stable.

## Evidence it collects

For a persistence subject the example reports the recovery footprint and cost:

- **on-disk bytes** — `disk.snapshot_bytes` (the snapshot directory) and
  `disk.wal_bytes` (the write-ahead log);
- **recovery wall-clock** — `recovery.elapsed` and the number of WAL ops
  replayed (`recovery.ops`);
- **live heap before versus after recovery** — `mem.heap_before_recovery`,
  `mem.heap_after_recovery`, and their `mem.heap_growth`, each measured after a
  forced GC so the figure reflects reachable bytes.

Scale `-nodes` up and watch the snapshot bytes and recovery time grow roughly
linearly while `weights.bit_exact` stays `true` — the durability and the
bit-exact codec contract hold at any size.

## Key APIs

- `store/recovery.OpenCtx[N, W]` / `recovery.Options[N, W]` — recover a typed graph from a WAL + snapshot directory using a codec pair, honouring context cancellation.
- `store/recovery.Result` — recovery report: `Graph`, `WALOps`, `SnapshotHit`, `SnapshotSchemaVersion`, `SnapshotLabels`, `SnapshotProperties`, and `IsClean()`.
- `store/txn.NewStoreWithOptions` / `txn.NewInt64Codec` / `txn.NewFloat64WeightCodec` — the typed store and the codecs that make int64 keys and float64 weights round-trip bit-for-bit.
- `store/snapshot.WriteSnapshotFullCtx` — write the full snapshot; stamps v2 for a non-string graph, v3 (with `mapper.bin`) for a string-keyed one.
- `graph/lpg.New` / `lpg.Float64Value` / `lpg.Int64Value` / `lpg.StringValue` / `lpg.BoolValue` — the in-memory labelled property graph and its typed property constructors.

## Further reading

- [`store/recovery`](../../store/recovery) — the typed recovery package documentation
- [`store/snapshot`](../../store/snapshot) — snapshot format and the v2/v3 manifest distinction
- [`store/txn`](../../store/txn) — the transactional store and codec API
- [Example 04 — persistence](../04_persistence) — the string-keyed persistence round-trip
- [Example 17 — transactional log](../17_transactional_log) — WAL + background checkpoint flow
- [Example 26 — social scale bench](../26_social_scale_bench) — the reference example for the standard this one follows
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
