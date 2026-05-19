# Persistence in GoGraph

This document describes how GoGraph durably persists the in-memory
graph state and how it recovers it after a process crash.

## Components

| Package             | Purpose                                                        |
|---------------------|----------------------------------------------------------------|
| `store/wal`         | Write-Ahead Log: framed records, CRC32C-checksummed.          |
| `store/snapshot`    | Immutable directory snapshots of the CSR view.                |
| `store/txn`         | Transactional surface (`Store.Begin`, `Tx.Commit/Rollback`).  |
| `store/checkpoint`  | Background folder of WAL tail into a fresh snapshot.          |
| `store/recovery`    | Inverse of commit: snapshot + WAL replay on open.             |

## Durability contract

`Tx.Commit` writes every buffered op to the WAL, calls
`wal.Writer.Sync` (which fsyncs the file), and only then applies the
ops to the live graph. A process killed at any point during this
sequence is recoverable by `recovery.OpenString(dir)`:

- If the crash happened **before the fsync**, the WAL tail is torn;
  recovery drops it and the in-memory graph is exactly what was
  durable at the last successful fsync.
- If the crash happened **after the fsync but before any in-memory
  apply**, the WAL contains the ops; recovery re-applies them.
- If the crash happened **after some ops are applied in memory**,
  the in-memory state is lost (it was not durable anyway) — recovery
  re-applies from the WAL.

`Tx.Rollback` neither writes to the WAL nor mutates the graph;
nothing is durable, nothing is visible, mutex released.

## Transaction isolation

`Store` holds a single `sync.Mutex` taken by `Begin` and released
by `Commit`/`Rollback`. The transactional layer is therefore
**single-writer / multi-reader**: reads on the underlying graph go
straight through `csr.CSR` / `adjlist.AdjList` which are lock-free
on the read path, while writes serialise.

## File layout on disk

```
<dir>/
  wal                — single appended file of framed records
  snapshot/
    manifest.json    — versioned index of files + CRC32C per file
    csr.bin          — serialised CSR (vertices / edges / weights)
    labels.bin       — serialised LPG labels (v2 manifests only)
    properties.bin   — serialised LPG typed properties (v2 manifests only)
```

## WAL payload schema

Each WAL frame carries one op payload. The encoder supports two
on-disk layouts, distinguished by the first byte of the payload:

| Version | First byte                  | Producer                                       |
|---------|-----------------------------|------------------------------------------------|
| v1      | `[OpKind]` (one of 0x01..0x04) | `txn.NewStore` (legacy)                        |
| v2      | `0xFE` (`txn.OpRecordV2`)   | `txn.NewStoreWithCodec` / `txn.NewStoreWithOptions` |

`OpKind` values currently occupy 0x01..0x04, leaving the low byte
region free for future kinds. The v2 magic byte 0xFE is chosen far
outside that range so a reader can disambiguate by peeking the first
byte alone — any payload starting with 0xFE is necessarily a v2 frame.

| `OpKind`              | Value | Mutation                                          |
|-----------------------|-------|---------------------------------------------------|
| `OpAddEdge`           | 0x01  | `AddEdge(src, dst, zero)` — no weight payload     |
| `OpSetNodeLabel`      | 0x02  | `SetNodeLabel(node, label)`                       |
| `OpSetEdgeLabel`      | 0x03  | `SetEdgeLabel(src, dst, label)`                   |
| `OpAddEdgeWeighted`   | 0x04  | `AddEdge(src, dst, w)` — typed weight payload     |

`OpAddEdgeWeighted` is the only kind whose v2 layout differs from the
plain `OpAddEdge` shape (see below). A pre-T8 reader that only knows
about `OpAddEdge` skips `OpAddEdgeWeighted` frames as an unknown
kind, which is the intended forward-compat behaviour: the typed
weight payload cannot be inferred without the registered
`WeightCodec`.

### v1 (legacy, untagged) layout

```
uint8  kind
uint16 srcLen (LE)
[srcLen]byte src   (fmt.Sprintf("%v") of op.Src)
uint16 dstLen (LE)
[dstLen]byte dst   (fmt.Sprintf("%v") of op.Dst)
uint16 labelLen (LE)
[labelLen]byte label
```

v1 endpoints are serialised via `fmt.Sprintf("%v")`, which is only
reliably reversible for the `string` type. Recovery via
`recovery.OpenString` works because `string(SrcBytes)` returns the
original key. For arbitrary node types, v1 is **not** generally
reversible; callers wanting to migrate must first replay the v1 log
into a typed store and re-emit v2 frames.

### v2 (tagged, typed) layout

For `OpAddEdge`, `OpSetNodeLabel`, and `OpSetEdgeLabel`:

```
uint8  version  (always 0xFE — txn.OpRecordV2)
uint8  kind
codec  src      (self-delimiting; see codec table below)
codec  dst      (self-delimiting)
uint16 labelLen (LE)
[labelLen]byte label
```

For `OpAddEdgeWeighted` (only emitted by `txn.NewStoreWithOptions`):

```
uint8       version  (always 0xFE — txn.OpRecordV2)
uint8       kind     (0x04 — txn.OpAddEdgeWeighted)
codec       src      (self-delimiting)
codec       dst      (self-delimiting)
wcodec      w        (self-delimiting; see weight-codec table below)
uint16      labelLen (LE, always 0 today; reserved for future use)
[labelLen]byte label
```

The codec writes the framing for src/dst inline — no separate length
prefix at the payload level. The optional weight payload follows the
same self-delimiting contract via the `WeightCodec`. The trailing
label keeps the v1 uint16 prefix for symmetry with the legacy
reader.

### Built-in node codecs

| Type                              | Wire form                                       |
|-----------------------------------|-------------------------------------------------|
| `string` (`NewStringCodec`)       | uint32 LE length prefix + utf-8 bytes           |
| `int` (`NewIntCodec`)             | varint                                          |
| `int32` (`NewInt32Codec`)         | varint                                          |
| `int64` (`NewInt64Codec`)         | varint                                          |
| `uint64` (`NewUint64Codec`)       | uvarint                                         |
| `[16]byte` (`NewUUIDCodec`)       | fixed 16 bytes                                  |
| `encoding.BinaryMarshaler` (`NewBinaryMarshalerCodec[N, *N]`) | uint32 LE length prefix + opaque marshaler payload |

### Built-in weight codecs

| Type                              | Wire form                                       |
|-----------------------------------|-------------------------------------------------|
| `int64` (`NewInt64WeightCodec`)   | varint                                          |
| `float64` (`NewFloat64WeightCodec`) | fixed 8 bytes (`math.Float64bits` little-endian)|
| `encoding.BinaryMarshaler` (`NewBinaryMarshalerWeightCodec[W, *W]`) | uint32 LE length prefix + opaque marshaler payload |

`Float64WeightCodec` round-trips bits losslessly, including ±0.0,
±Inf, and every NaN payload. Note that NaN comparison rules apply on
read (`NaN != NaN`); compare via `math.Float64bits` or `math.IsNaN`
when checking equality.

All built-in codecs (node and weight) are stateless and safe for
concurrent use.

## Weighted edges

A store constructed via `txn.NewStoreWithOptions` carries both a
`Codec[N]` and a `WeightCodec[W]`. `Tx.AddEdge(src, dst, w)` then
records every commit as an `OpAddEdgeWeighted` frame (kind byte
`0x04`) — the weight payload sits between the codec-encoded
endpoints and the trailing label, framed by the registered
`WeightCodec`.

Stores constructed via `NewStore` or `NewStoreWithCodec` have no
`WeightCodec`. They accept zero-valued `AddEdge` calls (which buffer
an `OpAddEdge` frame, applied with `var zero W`) and reject
non-zero weights with `txn.ErrNoWeightCodec`. Callers that need
durable weighted edges must upgrade to `NewStoreWithOptions`.

The recovery side mirrors the constructor matrix:

| Open path             | N decoded via | W decoded via | OpAddEdge | OpAddEdgeWeighted |
|-----------------------|---------------|----------------|-----------|-------------------|
| `OpenString`          | `string(SrcBytes)` (v1) or `NewStringCodec` (v2) | none — applies zero | applied with `W=zero` | dropped, `store.recovery.applyOp.fallbackZeroWeight` |
| `OpenWithCodec`       | typed `Codec[N]` | none — applies zero | applied with `W=zero` | dropped, `store.recovery.applyOp.fallbackZeroWeight` |
| `OpenWithOptions`     | typed `Codec[N]` | typed `WeightCodec[W]` | applied with `W=zero` | applied with decoded weight |

Forward compatibility: a pre-T8 WAL that contains only `OpAddEdge`
frames replays cleanly under `OpenWithOptions`. The apply path
writes `var zero W` for those records and reserves the typed weight
payload for `OpAddEdgeWeighted` frames only. The recovery test
`TestTxn_ForwardCompat_PreT8WALReplays` locks this contract.

### Recovery surface

`recovery.OpenString(dir)` accepts both v1 and v2-`StringCodec` frames
on the same WAL. It has no `WeightCodec` and therefore drops
`OpAddEdgeWeighted` frames (the loss is metered via
`store.recovery.applyOp.fallbackZeroWeight`).

For arbitrary `N` with no durable weights, use
`recovery.OpenWithCodec[N, W](dir, codec)`; this variant supports v2
`OpAddEdge` frames natively, applies a zero W for them, and drops
`OpAddEdgeWeighted` frames with the same fallback metric.

For arbitrary `N` and durable weights, use
`recovery.OpenWithOptions[N, W](dir, opts)` where `opts` carries
both `Codec[N]` and `WeightCodec[W]`. This is the only open path
that preserves edge weights on replay.

### Migrating a v1 corpus

Existing stores keep emitting v1 frames bit-for-bit while the
constructor is `NewStore`. To migrate to a typed-codec v2 store:

1. Replay the v1 log via `recovery.OpenString` into an in-memory
   graph.
2. Construct a new store via `txn.NewStoreWithCodec` against a fresh
   WAL.
3. Re-commit the recovered ops through the new store; subsequent
   frames are v2-tagged.

This is the same migration recipe used by snapshot + WAL-truncation
checkpoints, so the workflow does not require any new tooling.

### Migrating an unweighted v2 corpus to durable weights

A store that started life under `NewStoreWithCodec` (typed N, no W
codec) emits v2 frames with only `OpAddEdge`. To upgrade to durable
weights:

1. Replay the existing WAL via `recovery.OpenWithCodec[N, W]` (or
   `OpenWithOptions` with the legacy weights interpreted as zero).
   The recovered graph carries every committed edge with `W=zero`.
2. Reassign the real weights in memory if they are known from an
   out-of-band source (otherwise the migration is a no-op weight-
   wise; only future commits will carry typed weights).
3. Construct a new store via `txn.NewStoreWithOptions[N, W]`
   against a fresh WAL.
4. Re-commit any explicit weighted edges through the new store;
   subsequent frames are `OpAddEdgeWeighted` (v2 kind `0x04`).

No on-disk migration of existing `OpAddEdge` frames is required —
post-T8 readers continue to walk them with `W=zero`, exactly as
pre-T8 readers did.

`snapshot/labels.bin` and `snapshot/properties.bin` are shipped as
of manifest **v2** (see below). A future revision will add
`snapshot/indexes/*.bin` to extend the snapshot to the full LPG
state.

## Snapshot file format

The manifest schema is versioned. The build understands two versions
transparently:

- **v1** — `csr.bin` only. Written by the legacy
  `snapshot.WriteSnapshotCSR` / `WriteSnapshotCSRCtx` helpers and by
  the current `store/checkpoint.Checkpointer`. The on-disk shape is
  identical to v1.0.0 so existing fixtures keep loading bit-for-bit.
- **v2** — `csr.bin` + `labels.bin` + `properties.bin`. Written by
  `snapshot.WriteSnapshotFull` / `WriteSnapshotFullCtx`. Adds
  durable LPG label and typed-property state to the snapshot. The
  individual component files are independent of each other: a v2
  manifest may carry any combination of `labels.bin` and
  `properties.bin`, or neither (CSR-only v2), and
  `snapshot.LoadSnapshotFull` tolerates every combination.

`snapshot.LoadManifest` accepts both versions; a future version 3+
manifest surfaces `snapshot.ErrManifestUnsupported`. The high-level
`snapshot.LoadSnapshotFull` reads both shapes and returns empty
`LabelsReadback` / `PropertiesReadback` for components that are
absent.

### `snapshot/manifest.json` (v1)

```json
{
  "version": 1,
  "created_at": "2026-05-19T14:00:00Z",
  "order": 1000,
  "size": 5000,
  "files": [
    {"name": "csr.bin", "size": 24014, "crc32c": 305419896}
  ]
}
```

### `snapshot/manifest.json` (v2)

```json
{
  "version": 2,
  "created_at": "2026-05-19T14:00:00Z",
  "order": 1000,
  "size": 5000,
  "files": [
    {"name": "csr.bin",        "size": 24014, "crc32c": 305419896},
    {"name": "labels.bin",     "size":   312, "crc32c":  47119123},
    {"name": "properties.bin", "size":   568, "crc32c": 837469102}
  ]
}
```

The `labels.bin` and `properties.bin` entries are independent. A v2
manifest written by an older build (or by a custom emitter) may
include only one or omit both; readers handle each case
transparently.

### `snapshot/csr.bin` (binary, identical across v1 and v2)

| Offset  | Field            | Type                          |
|---------|------------------|-------------------------------|
| 0       | nVertices        | uint64 LE                     |
| 8       | nEdges           | uint64 LE                     |
| 16      | hasWeights       | uint8 (0 or 1)                |
| 17      | weightSizeBytes  | uint8                         |
| 18      | vertices         | uint64[nVertices]             |
| ...     | edges            | uint64[nEdges]                |
| ...     | weights          | raw[weightSize·nEdges] (opt.) |

### `snapshot/labels.bin` (binary, v2 only)

Little-endian throughout. The whole file is covered by the CRC32C
stored in the manifest entry, including the magic header.

| Offset  | Field             | Type                                  |
|---------|-------------------|---------------------------------------|
| 0       | magic             | uint32 LE = `0x4C424C53` (`'SLBL'`)   |
| 4       | formatVersion     | uint32 LE (currently 1)               |
| 8       | stringTableLen    | uint64 LE                             |
| ...     | strings           | stringTableLen × (uint32 utf8Len, [utf8Len]byte) |
| ...     | nodeEntries       | uint64 LE                             |
| ...     | node records      | nodeEntries × (uint64 NodeID, uint32 labelStringIdx) |
| ...     | edgeEntries       | uint64 LE                             |
| ...     | edge records      | edgeEntries × (uint64 src, uint64 dst, uint32 labelStringIdx) |

The string table is the deduplicated set of label names, written in
the order the writer interns them from `lpg.LabelRegistry`. Each
record's `labelStringIdx` indexes into that table. A reader rebuilds
the registry by re-interning each string in order; because
`lpg.LabelID` is assigned in interning order, the resulting LabelIDs
match the IDs that were live when the snapshot was taken, with no
extra remap step.

`labels.bin` is independent of the manifest version: a future change
to the labels layout (e.g., parallel-edge labels) bumps the
`formatVersion` byte without forcing a `manifest.json` schema bump.

#### Recovery semantics

`snapshot.ApplyLabelsToGraph` re-attaches the readback to a live
`*lpg.Graph`. Its pre-condition is that the underlying
`graph.Mapper` is already populated with every NodeID the labels
reference. In the standard durability path that pre-condition is
met by the WAL replay performed earlier in `recovery.OpenString` /
`OpenWithCodec` / `OpenWithOptions`. Label records whose NodeID
cannot be resolved by the mapper are skipped and counted via
`store.snapshot.ApplyLabels.unresolved` so observability surfaces
the loss instead of failing recovery.

Edge label records whose endpoints resolve but whose edge is absent
from the adjacency list (for example, when the CSR has not been
applied) are skipped and counted via
`store.snapshot.ApplyLabels.edgeMissing`. This matches
`lpg.Graph.SetEdgeLabel`'s own no-op-on-missing-edge contract.

#### Migrating a v1 snapshot directory

Existing v1 directories keep loading under all current `snapshot`
APIs: `Open`, `LoadSnapshotFull`, and `LoadManifest`. There is no
on-disk migration step — the next call to
`snapshot.WriteSnapshotFull` simply emits a fresh v2 directory at
the same path under the atomic `.tmp` + `os.Rename` protocol.

### `snapshot/properties.bin` (binary, v2 only)

Little-endian throughout. The whole file is covered by the CRC32C
stored in the manifest entry, including the magic header.

| Offset  | Field             | Type                                                                |
|---------|-------------------|---------------------------------------------------------------------|
| 0       | magic             | uint32 LE = `0x50525053` (`'SPRP'`)                                 |
| 4       | formatVersion     | uint32 LE (currently 1)                                             |
| 8       | keyTableLen       | uint64 LE                                                           |
| ...     | keys              | keyTableLen × (uint32 utf8Len, [utf8Len]byte)                       |
| ...     | nodeEntries       | uint64 LE                                                           |
| ...     | node records      | nodeEntries × (uint64 NodeID, uint32 keyIdx, uint8 kind, uint32 valueLen, [valueLen]byte) |
| ...     | edgeEntries       | uint64 LE                                                           |
| ...     | edge records      | edgeEntries × (uint64 src, uint64 dst, uint32 keyIdx, uint8 kind, uint32 valueLen, [valueLen]byte) |

The key table is the deduplicated set of property keys, written in
the order the writer interns them from `lpg.PropertyKeyRegistry`.
Each record's `keyIdx` indexes into that table. A reader rebuilds
the registry by re-interning each key in order; because
`lpg.PropertyKeyID` is assigned in interning order, the resulting
key IDs match the IDs that were live when the snapshot was taken,
with no extra remap step.

`properties.bin` is independent of the manifest version: a future
change to the properties layout (e.g., variable-width integer
encoding, new kinds) bumps the `formatVersion` byte without forcing
a `manifest.json` schema bump.

#### Value encoding per kind

The `kind` byte matches `lpg.PropertyValue.Kind` exactly. The
on-disk representation is fixed-width for all numeric and
fixed-size kinds so the file is straightforward to dump and
inspect with `xxd`:

| Kind tag | `lpg.PropertyKind` | `valueLen`         | Bytes                                                                  |
|----------|--------------------|--------------------|------------------------------------------------------------------------|
| 1        | `PropString`       | variable           | raw utf-8 bytes                                                        |
| 2        | `PropInt64`        | 8                  | little-endian two's-complement                                         |
| 3        | `PropFloat64`      | 8                  | `math.Float64bits` little-endian                                       |
| 4        | `PropBool`         | 1                  | `0x00` (false) / `0x01` (true)                                         |
| 5        | `PropTime`         | 16                 | uint64 seconds since Unix epoch ‖ uint64 nanoseconds-within-second     |
| 6        | `PropBytes`        | variable           | raw opaque bytes                                                       |

`PropTime` is reconstituted via `time.Unix(sec, nsec).UTC()` —
snapshots travel between machines so the caller's location is
deliberately dropped on read. `PropFloat64` round-trips bits
losslessly, including ±0.0, ±Inf, and every NaN payload (note that
`NaN != NaN` by IEEE rules; compare via `math.Float64bits`).

A record whose `kind` tag is outside the documented enum surfaces
as `snapshot.ErrPropertiesCorrupted`; the reader does not silently
drop unknown kinds.

#### Recovery semantics

`snapshot.ApplyPropertiesToGraph` re-attaches the readback to a
live `*lpg.Graph`. Its pre-condition mirrors
`ApplyLabelsToGraph`: the underlying `graph.Mapper` must already
be populated with every NodeID the properties reference. In the
standard durability path that pre-condition is met by the WAL
replay performed earlier in `recovery.OpenString` /
`OpenWithCodec` / `OpenWithOptions`. Property records whose
NodeID cannot be resolved by the mapper are skipped and counted
via `store.snapshot.ApplyProperties.unresolved` so observability
surfaces the loss instead of failing recovery.

Edge property records whose endpoints resolve but whose edge is
absent from the adjacency list are skipped and counted via
`store.snapshot.ApplyProperties.edgeMissing`. This matches
`lpg.Graph.SetEdgeProperty`'s own no-op-on-missing-edge contract.

#### Today's WAL coverage

Typed property writes are not currently part of the WAL surface
(`txn.Tx` records edge and label ops only). Properties survive
exclusively via the snapshot's `properties.bin`. A crash between
two snapshots therefore loses any property changes that were not
captured in the prior snapshot. Closing this gap by extending the
WAL with typed-property ops is tracked under the LPG durability
sub-roadmap.

## Checkpoint policy

`store/checkpoint.Checkpointer` runs a goroutine that takes a
snapshot every `MaxAge` interval (or on `Trigger()`). Each
checkpoint:

1. Acquires the store mutex.
2. Builds a CSR from the current adjacency list.
3. Releases the mutex.
4. Writes `snapshot/` atomically via the `.tmp` + `os.Rename`
   protocol documented above.
5. Calls `wal.Writer.Sync` so the WAL is in a defined state.

A future revision will additionally truncate the WAL prefix once
the snapshot covers the corresponding ops.

## Recovery procedure

`recovery.OpenString(dir)` returns a `Result` containing the
rebuilt `*lpg.Graph[string, int64]` plus:

- `SnapshotHit bool` — whether `snapshot/manifest.json` was found
  and validated.
- `SnapshotLabels int` — how many label records the snapshot's
  `labels.bin` contributed back into the graph after WAL replay.
  v1 snapshots and v2 snapshots without `labels.bin` leave this at
  0.
- `SnapshotProperties int` — how many typed-property records the
  snapshot's `properties.bin` contributed back into the graph after
  WAL replay. v1 snapshots and v2 snapshots without
  `properties.bin` leave this at 0.
- `WALOps int` — how many WAL ops were applied.
- `TailErr error` — `wal.ErrTornFrame` is normal (clean tail
  truncation after a crash); `wal.ErrCRCMismatch` indicates real
  corruption and must be surfaced.

Component apply order during open is fixed: CSR (implicit, via
mapper replay from WAL) → labels.bin → properties.bin. The
properties pass runs last so the mapper is fully populated and the
edge bag (built from CSR + WAL) is in place — property records
that point at endpoints the apply phase has not yet seen are
skipped and metered rather than aborting recovery.

The recovery contract is verified by the fuzz test in
`store/recovery/recovery_test.go::TestRecovery_FuzzedTruncation`,
which truncates the WAL at random offsets for 200 iterations and
asserts that the recovered graph is always a prefix of the
committed op sequence.

## Rolling-upgrade harness

Two frozen v1 fixtures live under the persistence packages so any
future build proves it still loads the on-disk shape this release
emits:

- `store/wal/testdata/v1/sample.wal` — five framed payloads; loaded
  by `store/wal/format_compat_test.go`.
- `store/snapshot/testdata/v1/sample/` — `manifest.json` + `csr.bin`
  for a deterministic 3-edge graph; loaded by
  `store/snapshot/format_compat_test.go`.

Each compat test pairs a happy-path "old fixture loads under new
code" assertion with a synthesised future-version assertion that
checks the decoder rejects unknown versions cleanly via
`wal.ErrUnsupportedVersion` / `snapshot.ErrManifestUnsupported`
(rather than mis-parsing or panicking).

The fixtures are regenerated by:

```bash
go run ./cmd/fmtfixture            # all three packages
go run ./cmd/fmtfixture -pkg wal   # one package at a time
```

Commit the refreshed `testdata/v1/...` files alongside any writer
change that intentionally bumps the on-disk shape, and add a fresh
`testdata/v2/` tree before the writer starts emitting v2 frames.
