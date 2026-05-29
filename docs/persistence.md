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

`Tx.Commit` writes every buffered op to the WAL as a v3 frame, appends a
single `OpCommit` marker frame, calls `wal.Writer.Sync` (which fsyncs the
file), and only then applies the ops to the live graph. A process killed at
any point during this sequence is recoverable by `recovery.Open`/`OpenString`:

- If the crash happened **before the fsync**, the WAL tail is torn;
  recovery drops it and the in-memory graph is exactly what was
  durable at the last successful fsync.
- If the crash happened **after the fsync but before any in-memory
  apply**, the WAL contains the ops; recovery re-applies them.
- If the crash happened **after some ops are applied in memory**,
  the in-memory state is lost (it was not durable anyway) — recovery
  re-applies from the WAL.

**Atomicity of multi-op transactions (audit gap F1).** A typed-store
transaction is all-or-nothing across a crash. Recovery buffers a v3
transaction's op frames and applies them only on reading the durable
`OpCommit` marker, which is the last frame the commit writes. A torn write
that loses any op frame *or* the marker therefore discards the **entire**
transaction — recovery never applies a prefix, so a `CREATE`/`MERGE`/
multi-`SET` statement can never leave a half-built node or a dangling edge.
The commit issues exactly one fsync, so group-commit throughput is unchanged;
bufio may flush a prefix of frames to the OS before the fsync, but that is
benign because durability is gated on the fsync and an un-marked tail is
discarded. (The legacy `NewStore` fmt-codec path keeps per-op v1 framing
without a marker and is not durable-multi-op-atomic; every production write
path uses a typed store.)

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
    indexes/         — secondary indexes registered with index.Manager
      <name>.bin     — one file per registered serialisable index (v2)
```

## WAL payload schema

Each WAL frame carries one op payload. The encoder supports three
on-disk layouts, distinguished by the first byte of the payload:

| Version | First byte                  | Producer                                       |
|---------|-----------------------------|------------------------------------------------|
| v1      | `[OpKind]` (one of 0x01..0x04) | `txn.NewStore` (legacy)                        |
| v2      | `0xFE` (`txn.OpRecordV2`)   | superseded by v3 (still read for old WALs)     |
| v3      | `0xFD` (`txn.OpRecordV3`)   | `txn.NewStoreWithCodec` / `txn.NewStoreWithOptions` |

`OpKind` values currently occupy 0x01..0x0D (the last being `OpCommit`,
the v3 commit marker), leaving the rest of the low byte region free. The
magic bytes 0xFE/0xFD are chosen far outside that range so a reader
disambiguates by peeking the first byte alone — any payload starting with
0xFE is a v2 frame, 0xFD a v3 frame, anything else a v1 frame.

A **v3** payload is `version(0xFD) | kind | uint64 txnSeq (LE) | <v2 body>`.
The body after `txnSeq` is byte-identical to the v2 body for that kind, so
recovery reuses the v2 body walk. The `txnSeq` groups a transaction's frames;
the trailing `OpCommit` marker (`version | OpCommit | txnSeq`, no body) is the
atomicity boundary — recovery applies a v3 transaction's buffered ops only on
reading its durable marker (see the Durability contract above). Typed stores
emit v3; v2 frames remain fully readable so existing on-disk WALs replay
unchanged.

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

| Open path                                 | N decoded via | W decoded via | OpAddEdge | OpAddEdgeWeighted |
|-------------------------------------------|---------------|----------------|-----------|-------------------|
| `Open` (canonical)                        | typed `Codec[N]` | typed `WeightCodec[W]` | applied with `W=zero` | applied with decoded weight |
| `OpenString` (Deprecated)                 | `string(SrcBytes)` (v1) or `NewStringCodec` (v2) | none — applies zero | applied with `W=zero` | dropped, `store.recovery.applyOp.fallbackZeroWeight` |
| `OpenWithCodec` (Deprecated)              | typed `Codec[N]` | none — applies zero | applied with `W=zero` | dropped, `store.recovery.applyOp.fallbackZeroWeight` |
| `OpenWithOptions` (Deprecated)            | typed `Codec[N]` | typed `WeightCodec[W]` | applied with `W=zero` | applied with decoded weight |

Forward compatibility: a pre-T8 WAL that contains only `OpAddEdge`
frames replays cleanly under `Open` / `OpenWithOptions`. The apply
path writes `var zero W` for those records and reserves the typed
weight payload for `OpAddEdgeWeighted` frames only. The recovery
test `TestTxn_ForwardCompat_PreT8WALReplays` locks this contract.

### Generic recovery API

`recovery.Open[N, W](dir, opts)` is the canonical recovery entry
point. `opts` is a `recovery.Options[N, W]` carrying both `Codec[N]`
and `WeightCodec[W]` — the same two codecs the typed Store was built
with. Both fields are required; passing nil for either returns an
error.

```go
res, err := recovery.Open[int64, float64](dir, recovery.Options[int64, float64]{
    Codec:       txn.NewInt64Codec(),
    WeightCodec: txn.NewFloat64WeightCodec(),
})
```

`recovery.OpenCtx` is the context-aware variant: `ctx.Err()` is
checked at the snapshot-load boundary and every 4096 WAL frames
during replay. On cancellation the function returns the partially
recovered `Result` paired with the wrapped `ctx.Err`.

`Result[N, W]` exposes the same fields as before plus
`SnapshotSchemaVersion int`, which reports the on-disk manifest
version of the snapshot that was loaded (`1` for legacy CSR-only
directories, `2` for the labels + properties + indexes shape). The
field is `0` when no snapshot was found, so callers can branch on
`res.SnapshotSchemaVersion >= 2` to detect a v2 directory without
re-reading the manifest.

`recovery.Options[N, W]` mirrors `txn.Options[N, W]` field-for-field
so call sites that already hold a `txn.Options` value can convert
in place (`recovery.Options[N, W](opts)`). Keeping the
recovery-argument type local to the recovery package spares callers
the cross-package import.

### Recovery surface (legacy / wrappers)

The following entry points predate the canonical `Open[N, W]` API.
They remain available for backwards compatibility and are
documented as `Deprecated:` in their godoc:

`recovery.OpenString(dir)` accepts both v1 and v2-`StringCodec`
frames on the same WAL. It has no `WeightCodec` and therefore drops
`OpAddEdgeWeighted` frames (the loss is metered via
`store.recovery.applyOp.fallbackZeroWeight`). New code should use
`Open[string, int64]` with `txn.NewStringCodec()` +
`txn.NewInt64WeightCodec()`.

`recovery.OpenWithCodec[N, W](dir, codec)` is the codec-only path
that predates `txn.WeightCodec`. It supports v2 `OpAddEdge` frames
natively, applies zero W for them, and drops `OpAddEdgeWeighted`
frames with the same fallback metric.

`recovery.OpenWithOptions[N, W](dir, opts)` takes a `txn.Options`
across package boundaries. Behaviour is identical to `Open`; the
only difference is the argument's package.

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

`snapshot/labels.bin`, `snapshot/properties.bin`, and
`snapshot/indexes/*.bin` are all shipped as part of manifest **v2**
(see below). The CSR, labels, and properties components are required
to recover the graph state; the `indexes/` sub-directory is optional
and only present when at least one secondary index registered with
[`index.Manager`](../graph/index/manager.go) implements the
`index.Serializer` interface.

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
  ],
  "indexes": [
    {"name": "labels.nodes",  "size":  1024, "crc32c": 123456789},
    {"name": "hash.email",    "size": 16384, "crc32c": 987654321},
    {"name": "btree.score",   "size":  8192, "crc32c": 246813579}
  ]
}
```

The `labels.bin` and `properties.bin` entries are independent. A v2
manifest written by an older build (or by a custom emitter) may
include only one or omit both; readers handle each case
transparently. The `indexes` array is omitted entirely when no
registered indexes implement `index.Serializer`, keeping the on-disk
form bit-identical to pre-extension v2 snapshots.

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

#### WAL coverage for properties (T931)

Typed property writes are part of the WAL surface. `txn.Tx` carries
`OpSetNodeProperty`, `OpDelNodeProperty`, `OpSetEdgeProperty` and
`OpDelEdgeProperty` ops; each is encoded as a v2 OpRecord with the
property key (uint16-length prefix) followed by the
`encodePropertyValue` payload (a single byte kind tag plus the
typed value bytes). Recovery walks the same encoding in
`decodeRecoveryPropertyValue` and re-applies the property to the
recovered graph via `Graph.SetNodeProperty` / `SetEdgeProperty`
(or `DelNodeProperty` / `DelEdgeProperty` for the Del variants).

A crash between two snapshots no longer loses property changes:
every `SET n.prop = ...` or `REMOVE n.prop` issued through the
Cypher engine's WAL-backed adapter is fsynced to the WAL on
transaction commit and replayed on restart. The snapshot's
`properties.bin` remains the authoritative source for the
historical baseline; the WAL contributes the delta since the
prior snapshot.

### `snapshot/indexes/<name>.bin` (binary, v2 only)

Each registered secondary index that implements `index.Serializer`
is persisted under `snapshot/indexes/<name>.bin`, where `<name>` is
the logical name the index was created under via
`index.Manager.CreateIndex(name, sub)`. The manifest's `indexes`
array carries the size and CRC32C of each file so a corrupted
component can be detected at load time without re-running the
serializer.

| Index kind | Magic         | Layout                                                                                                    |
|------------|---------------|-----------------------------------------------------------------------------------------------------------|
| `label`    | `0x49424C53` `'SLBI'` | `magic`, `formatVersion`, `labelCount`, repeat `(labelID, bitmapLen, bitmap bytes via Roaring)`           |
| `hash`     | `0x48534853` `'SHSH'` | `magic`, `formatVersion`, `entryCount`, repeat `(valueLen, value bytes, idCount, [idCount]uint64)`        |
| `btree`    | `0x52544253` `'SBTR'` | `magic`, `formatVersion`, `entryCount`, repeat `(keyLen, key bytes, idCount, [idCount]uint64)` (in order) |

Every payload terminates with a `uint32` little-endian CRC32C
trailer covering the entire prefix (magic through last record). The
manifest's CRC32C covers the whole file, including that trailer, so
either a corrupted trailer or a corrupted byte upstream surfaces at
load time. The B+ tree dump is written in ascending key order so
the reader can build the sorted internal slice in a single O(n) pass
without re-sorting.

#### Supported value-type encodings

The generic hash and B+ tree indexes serialise the comparable
(respectively `cmp.Ordered`) value type through a kind-aware
encoder:

| Go type    | Wire form                                       |
|------------|-------------------------------------------------|
| `string`   | raw utf-8 bytes                                 |
| `[]byte`   | raw bytes (hash only)                           |
| `int64`    | 8 bytes little-endian two's-complement          |
| `int32`    | 4 bytes little-endian                           |
| `int`      | 8 bytes little-endian (btree only)              |
| `uint64`   | 8 bytes little-endian                           |
| `uint32`   | 4 bytes little-endian                           |
| `uint`     | 8 bytes little-endian (btree only)              |
| `float64`  | 8 bytes `math.Float64bits` little-endian        |
| `bool`     | 1 byte (`0x00` / `0x01`, hash only)             |

Other value types surface `index.ErrIndexValueTypeUnsupported` on
`Serialize`. Callers that need to persist an index keyed by an
exotic type should convert to one of the supported types before
registering for snapshot.

#### Recovery semantics

`snapshot.LoadSnapshotFull` returns one `IndexReadback` per entry
in `manifest.indexes`. When the on-disk file is missing or the
manifest's CRC32C does not match the file bytes,
`IndexReadback.Bytes` is `nil` and the counter
`store.snapshot.indexes.corrupted` is incremented; the recovery
code path in `store/recovery` logs the warning
`recovery: index "<name>" corrupted, will rebuild from LPG` and
the index is left empty. The next mutation pass through the live
`index.Manager` re-populates it via the change-stream fan-out, so
the in-memory state is eventually consistent again without manual
intervention.

A clean readback's `Bytes` are passed to
`index.Serializer.Deserialize` on the live index registered under
the same name; an extra `Deserialize` error (for example because
the inner format version was bumped between writer and reader)
surfaces as another `index.ErrIndexCorrupted` and the same
rebuild-from-LPG path applies.

`recovery.Result.SnapshotIndexes` records how many indexes
successfully re-hydrated. Indexes that were rebuilt instead are
not counted here; they show up via
`store.snapshot.indexes.corrupted`.

#### Migrating a snapshot directory without `indexes/`

A v2 snapshot produced before this extension simply has no
`indexes` array in its manifest and no `indexes/` sub-directory.
Such snapshots continue to load cleanly; `LoadedSnapshot.Indexes`
is `nil` and the next snapshot pass produces an updated layout the
moment a serialisable index is registered with the graph.

## Checkpoint policy

`store/checkpoint.Checkpointer` runs a goroutine that takes a
snapshot every `MaxAge` interval (or on `Trigger()`). Each
checkpoint runs under the store mutex so no transaction commits
new WAL frames between snapshot capture and WAL truncation:

1. Acquire the store mutex.
2. Build a CSR from the current adjacency list.
3. Write `snapshot/` atomically via the `.tmp` + `os.Rename`
   protocol documented above, using `snapshot.WriteSnapshotFull`
   so the snapshot is a **self-sufficient** image of committed
   state: CSR adjacency *plus* `labels.bin`, `properties.bin`,
   registered indexes, and — for string-keyed graphs — `mapper.bin`
   (the durable `NodeID→key` table).
4. Determine whether the snapshot is self-sufficient — i.e. whether
   its manifest carries `mapper.bin`, the only file that lets
   recovery rebuild the `NodeID→key` mapping *without* the WAL.
5. Call `wal.Writer.Sync` so the WAL is in a defined state.
6. If the snapshot is self-sufficient, call `wal.Writer.Truncate`
   to reclaim the WAL prefix now folded into the snapshot. The
   reclaimed byte count is recorded on `Stats.WALTruncBytes` and
   emitted via `store.checkpoint.wal_truncated_bytes`.
   If it is **not** self-sufficient (a non-string key type, for
   which `mapper.bin` is not yet written), truncation is **skipped**
   and the WAL is retained — truncating it would erase the only
   durable copy of the `NodeID→key` mapping and destroy committed
   data. The skip is surfaced via
   `store.checkpoint.truncate_skipped_not_self_sufficient`.
7. Release the store mutex.

Why this matters (audit gap F2, see `docs/acid-audit.md`): the
v1.0 checkpoint wrote a *CSR-only* snapshot and then truncated the
WAL unconditionally. Because a CSR-only snapshot carries neither
labels/properties nor a mapper, recovery after such a checkpoint
lost every committed label and property and could not even
reconstruct the adjacency by key — a **Durability** violation. The
self-sufficient snapshot plus the truncate-only-when-self-sufficient
guard close that gap for every key type: a committed transaction
always survives a checkpoint.

Non-string key types currently take the WAL-retained path (no
truncation), so their WAL is not reclaimed by the checkpointer yet;
extending `mapper.bin` to all key types (so non-string checkpoints
can also truncate) is tracked as a follow-up operational improvement
and does not affect the Durability guarantee.

The lock-during-IO trade-off is acknowledged in `checkpoint.go`:
for very large graphs this can stall writers and may be reworked
later to a position-tracked truncate (capture LSN under lock, write
snapshot lock-free, truncate up-to-LSN under lock).

## Recovery procedure

`recovery.Open[N, W](dir, opts)` (canonical) and
`recovery.OpenString(dir)` (deprecated wrapper) return a `Result`
containing the rebuilt `*lpg.Graph[N, W]` plus:

- `SnapshotHit bool` — whether `snapshot/manifest.json` was found
  and validated.
- `SnapshotSchemaVersion int` — the on-disk manifest version of the
  snapshot that was loaded (1 for legacy CSR-only directories, 2 for
  the labels + properties + indexes shape). 0 when no snapshot was
  found.
- `SnapshotLabels int` — how many label records the snapshot's
  `labels.bin` contributed back into the graph after WAL replay.
  v1 snapshots and v2 snapshots without `labels.bin` leave this at
  0.
- `SnapshotProperties int` — how many typed-property records the
  snapshot's `properties.bin` contributed back into the graph after
  WAL replay. v1 snapshots and v2 snapshots without
  `properties.bin` leave this at 0.
- `SnapshotIndexes int` — how many secondary indexes were
  re-hydrated from `indexes/<name>.bin` payloads.
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


---

*Last reviewed: 2026-05-22 against commit `cd97f07`. If you edit code referenced by this document and do not update this footer, the doc-staleness lint will flag the PR.*
