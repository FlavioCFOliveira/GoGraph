# Persistence in GoGraph

This document describes how GoGraph durably persists the in-memory
graph state and how it recovers it after a process crash.

## Components

| Package             | Purpose                                                        |
|---------------------|----------------------------------------------------------------|
| `store`             | Composed teardown owner (`store.DB`): closes the WAL and checkpointer in the crash-safe order (see "Composed shutdown"). |
| `store/wal`         | Write-Ahead Log: framed records, CRC32C-checksummed.          |
| `store/snapshot`    | Immutable directory snapshots of the CSR view.                |
| `store/txn`         | Transactional surface (`Store.Begin`, `Tx.Commit/Rollback`).  |
| `store/checkpoint`  | Background folder of WAL tail into a fresh snapshot.          |
| `store/recovery`    | Inverse of commit: snapshot + WAL replay on open.             |

## Durability contract

`Tx.Commit` writes every buffered op to the WAL as a v3 frame, appends a
single `OpCommit` marker frame, calls `wal.Writer.Sync` (which fsyncs the
file), and only then applies the ops to the live graph. A process killed at
any point during this sequence is recoverable by `recovery.Open`/`OpenCtx`:

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
Durability is gated on the fsync: bufio may flush a prefix of frames to the OS
before the fsync, but that is benign because an un-marked tail is discarded on
recovery. Concurrent commits are coalesced by **group commit**
(`wal.Writer.SyncGroup`): a single leader fsyncs the whole buffered suffix once
and every committer whose durability watermark the flush covers acknowledges
without issuing its own fsync, so the per-commit fsync no longer caps
write throughput under concurrency (≈ 118× at 256 concurrent writers, #1507)
while the single-threaded commit cost is unchanged. A commit is acknowledged
only after the covering fsync, so atomicity and durability are preserved (a
failed group fsync fails every member of the group), and the in-memory state is
applied strictly in WAL sequence order, so isolation is preserved (the
group-commit measurements are in
[docs/benchmarks/v0.3.1.md](benchmarks/v0.3.1.md)). Every store is a typed store
on the v3 commit path; the legacy v1
fmt-codec write path was removed (see "WAL payload schema" below), so
there is no non-durable, non-atomic per-op framing left in the module.

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
    labels.bin       — serialised LPG labels (v2+ manifests only)
    properties.bin   — serialised LPG typed properties (v2+ manifests only)
    mapper.bin       — durable NodeID→key interning table (v3 manifests only)
    tombstones.bin   — durable node-removal set (present only when ≥1 node is tombstoned)
    indexes/         — secondary indexes registered with index.Manager
      <name>.bin     — one file per registered serialisable index (v2+)
```

## WAL payload schema

Each WAL frame carries one op payload, distinguished by the first byte
of the payload. The **v1 (legacy, untagged) record format was removed**,
together with its `txn.NewStore` write API; it is no longer
produced and is rejected on read (see below). Two layouts remain on the
wire:

| Version | First byte                  | Status                                                          |
|---------|-----------------------------|-----------------------------------------------------------------|
| v1      | any other first byte        | **Removed.** Never written; rejected on read with `recovery.ErrUnsupportedRecordVersion` (`txn.OpRecordV1` is a reserved sentinel — see below). |
| v2      | `0xFE` (`txn.OpRecordV2`)   | Read-only legacy: still decoded so existing on-disk WALs replay, but no longer produced (superseded by v3). |
| v3      | `0xFD` (`txn.OpRecordV3`)   | The only format produced. Emitted by `txn.NewStoreWithCodec` / `txn.NewStoreWithOptions`. |

`OpKind` values currently occupy 0x01..0x0D (the last being `OpCommit`,
the v3 commit marker), leaving the rest of the low byte region free. The
magic bytes 0xFE/0xFD are chosen far outside that range so a reader
disambiguates by peeking the first byte alone — any payload starting with
0xFE is a v2 frame, 0xFD a v3 frame. `recovery.Decode` rejects any other
leading byte with `recovery.ErrUnsupportedRecordVersion`: in practice that
is a legacy v1 frame (whose `fmt.Sprintf`-derived endpoints have no inverse
through a typed codec) or an unknown future tag. `txn.OpRecordV1` (value 0)
is retained only as a reserved sentinel so the rejection path and its tests
can name the version they refuse; it is never written and must not be reused
for a new record version.

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

### v1 (legacy, untagged) layout — removed

The v1 record format (an untagged `uint8 kind` followed by
`fmt.Sprintf("%v")`-encoded endpoints) was removed along with the
`txn.NewStore` constructor that produced it and the recovery read wrappers
that decoded it. Those endpoints were only reliably reversible for the
`string` type, so the format could never round-trip an arbitrary node key.
A v1 frame found on disk is no longer parsed: `recovery.Decode` rejects any
non-`0xFE`/`0xFD` leading byte with `recovery.ErrUnsupportedRecordVersion`.

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
label carries a uint16 little-endian length prefix.

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

Stores constructed via `NewStoreWithCodec` have no `WeightCodec`. They
accept zero-valued `AddEdge` calls (which buffer an `OpAddEdge` frame,
applied with `var zero W`) and reject non-zero weights with
`txn.ErrNoWeightCodec`. Callers that need durable weighted edges must
upgrade to `NewStoreWithOptions`.

`recovery.Open` (and its context-aware twin `recovery.OpenCtx`) is the
only recovery entry point. The deprecated `OpenString`,
`OpenWithCodec`, and `OpenWithOptions` wrappers were removed together with
the v1 WAL format; pass the codecs explicitly via `recovery.Options`
instead:

| Open path                                 | N decoded via | W decoded via | OpAddEdge | OpAddEdgeWeighted |
|-------------------------------------------|---------------|----------------|-----------|-------------------|
| `Open` / `OpenCtx` (canonical, codecs required) | typed `Codec[N]` | typed `WeightCodec[W]` | applied with `W=zero` | applied with decoded weight |

A WAL whose `WeightCodec` is absent from `recovery.Options` cannot open:
both `Codec` and `WeightCodec` are required and a nil for either returns an
error before any frame is read.

Forward compatibility: a pre-T8 WAL that contains only `OpAddEdge`
frames replays cleanly under `Open`. The apply path writes `var zero W`
for those records and reserves the typed weight payload for
`OpAddEdgeWeighted` frames only. The recovery test
`TestTxn_ForwardCompat_PreT8WALReplays` locks this contract.

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

### Recovery surface (removed wrappers)

The deprecated `recovery.OpenString`, `recovery.OpenWithCodec`, and
`recovery.OpenWithOptions` wrappers (and their `*Ctx` variants) were
removed along with the v1 WAL format. `recovery.Open[N, W]` /
`recovery.OpenCtx[N, W]` are the only entry points: pass the same two
codecs the typed `Store` was built with via `recovery.Options[N, W]`.
For example, the former `OpenString(dir)` call is replaced by
`Open[string, int64](dir, recovery.Options[string, int64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewInt64WeightCodec()})`.

### The removed v1 corpus format

The v1 WAL format and its read/write API were removed, so there is no
in-place v1 reader. A v1 corpus is not supported: the only formats that
open are the tagged v2 and v3 frames produced by `txn.NewStoreWithCodec`
and `txn.NewStoreWithOptions`.

### Migrating an unweighted v2 corpus to durable weights

A store that started life under `NewStoreWithCodec` (typed N, no W
codec) emits frames with only `OpAddEdge`. To upgrade to durable
weights:

1. Replay the existing WAL via `recovery.Open[N, W]` with a
   `WeightCodec` supplied (the legacy unweighted frames are interpreted
   as zero). The recovered graph carries every committed edge with
   `W=zero`.
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

The manifest schema is versioned. `snapshot.ManifestVersion` (the
highest schema this build emits) is **3**. The build understands all
three versions transparently:

- **v1** — `csr.bin` only. Written by the legacy
  `snapshot.WriteSnapshotCSR` / `WriteSnapshotCSRCtx` helpers. The
  on-disk shape is unchanged so existing fixtures keep
  loading bit-for-bit.
- **v2** — `csr.bin` + `labels.bin` + `properties.bin`. Written by
  `snapshot.WriteSnapshotFull` / `WriteSnapshotFullCtx` when the graph
  is keyed by a non-string type **and** no mapper codec is supplied.
  Adds durable LPG label and typed-property state, but carries no
  mapper, so recovery from a v2 snapshot must replay the WAL to
  re-intern the `NodeID→key` mapping. The individual component files
  are independent of each other: a v2 manifest may carry any
  combination of `labels.bin` and `properties.bin`, or neither
  (CSR-only v2), and `snapshot.LoadSnapshotFull` tolerates every
  combination.
- **v3** — `csr.bin` + `labels.bin` + `properties.bin` + `mapper.bin`.
  Written by `snapshot.WriteSnapshotFull` for string-keyed graphs, and
  by `snapshot.WriteSnapshotFullWithMapperCodec` for **any** key type.
  The extra `mapper.bin` makes the snapshot **self-sufficient**:
  recovery reconstructs the full graph from the snapshot alone, with
  no WAL replay required (audit gap F3). This is the shape the current
  checkpointer emits.

`tombstones.bin` is an **optional, additive** component (like
`indexes/`): it is emitted alongside the components above **only when
the graph currently has at least one tombstoned node**, and it does
**not** bump the manifest version — a manifest still reports v3. A
snapshot of a graph that has never removed a node is therefore
byte-identical to one produced before the component existed, and an
older snapshot without it loads as an empty tombstone set. See
[Node and edge deletion durability](#node-and-edge-deletion-durability).

`snapshot.LoadManifest` accepts all three versions; a manifest whose
version exceeds `ManifestVersion` (4 or higher) surfaces
`snapshot.ErrManifestUnsupported`. The high-level
`snapshot.LoadSnapshotFull` reads every shape and returns empty
`LabelsReadback` / `PropertiesReadback` / `MapperReadback` /
`TombstonesReadback` for components that are absent.

### Node and edge deletion durability

Node deletion is a **tombstone**, not a slot release: the `NodeID`→key
mapper entry is permanent (NodeID stability is a hard contract), so
`lpg.Graph.RemoveNode` records the removed `NodeID` in an in-memory
tombstone set and the read paths (`IsTombstoned`, `LiveOrder`, the
Cypher `AllNodesScan`) filter it. For that deletion to **survive a
store reopen**, the tombstone set is round-tripped through persistence:

- **Snapshot** — the checkpointer writes the sorted tombstone set to
  `tombstones.bin` (magic + format version + sorted `uint64` ids +
  CRC32C, validated like every other component) before truncating the
  WAL. On load, `snapshot.ApplyTombstonesToGraph` restores the set
  **after** the snapshot nodes are materialised (mapper + CSR) and
  **before** WAL replay, so any WAL-tail mutation lands on top in
  chronological order.
- **WAL replay** — replaying an `OpRemoveNode` frame reconstructs the
  tombstone (`g.RemoveNode`), not merely the label/property strip, in
  both the in-memory `txn` apply and recovery, so live and recovered
  state agree.
- **Resurrection** — re-creating a removed key brings the node back to
  life under the **same** stable `NodeID`: `lpg.Graph.AddNode` clears
  the tombstone. A delete→recreate cycle therefore yields exactly one
  live node, in-process, on WAL replay, and across a snapshot boundary.
  (`SetNodeLabel` and `AddEdge` deliberately do **not** revive — only
  `AddNode` does — so re-applying snapshot-era labels after WAL replay
  cannot resurrect a node the WAL tail deleted.)

Edge deletion has no tombstone (the adjacency entry is genuinely
removed), but the per-pair edge label and property surfaces are kept
hygienic the same way: `lpg.Graph.RemoveEdge` clears the per-pair
labels/properties **once the endpoint pair is fully disconnected** (no
remaining parallel edge), so re-creating an edge between the same
endpoints does not resurrect the removed relationship's type or
properties. While any parallel edge survives, the shared per-pair
surface is left intact.

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

### `snapshot/manifest.json` (v3)

```json
{
  "version": 3,
  "created_at": "2026-05-19T14:00:00Z",
  "order": 1000,
  "size": 5000,
  "files": [
    {"name": "csr.bin",        "size": 24014, "crc32c": 305419896},
    {"name": "labels.bin",     "size":   312, "crc32c":  47119123},
    {"name": "properties.bin", "size":   568, "crc32c": 837469102},
    {"name": "mapper.bin",     "size":  4096, "crc32c": 314159265}
  ],
  "indexes": [
    {"name": "labels.nodes",  "size":  1024, "crc32c": 123456789}
  ]
}
```

A v3 manifest is a v2 manifest with an extra `mapper.bin` entry in
`files`. The `version` field is bumped to `3` only when `mapper.bin`
is present; the writer stamps `2` for non-string-keyed snapshots that
omit the mapper. `snapshot/csr.bin`, `labels.bin`, and
`properties.bin` are byte-identical across v2 and v3.

### `snapshot/csr.bin` (binary, identical across v1, v2 and v3)

| Offset  | Field            | Type                          |
|---------|------------------|-------------------------------|
| 0       | nVertices        | uint64 LE                     |
| 8       | nEdges           | uint64 LE                     |
| 16      | hasWeights       | uint8 (0 or 1)                |
| 17      | weightSizeBytes  | uint8                         |
| 18      | vertices         | uint64[nVertices]             |
| ...     | edges            | uint64[nEdges]                |
| ...     | weights          | raw[weightSize·nEdges] (opt.) |

### `snapshot/labels.bin` (binary, v2+ only)

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
met by the WAL replay (or, for a v3 snapshot, the `mapper.bin`
restore) performed earlier in `recovery.Open` / `OpenCtx`. Label
records whose NodeID
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

### `snapshot/properties.bin` (binary, v2+ only)

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
replay (or, for a v3 snapshot, the `mapper.bin` restore) performed
earlier in `recovery.Open` / `OpenCtx`. Property records whose
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

### `snapshot/mapper.bin` (binary, v3 only)

`mapper.bin` is the durable `NodeID→key` interning table. It is what
turns a snapshot **self-sufficient**: with it, recovery rebuilds the
mapper from the snapshot and applies the CSR adjacency without
replaying the WAL. Little-endian throughout; the whole file is
covered by the CRC32C stored in its manifest entry, including the
magic header. The internal `formatVersion` byte is independent of the
manifest version — a future mapper layout change bumps it without
forcing a `manifest.json` schema bump.

| Offset | Field         | Type                                   |
|--------|---------------|----------------------------------------|
| 0      | magic         | uint32 LE = `0x50414D47` (`'GMAP'`)    |
| 4      | formatVersion | uint16 LE (1 = string, 2 = codec)      |
| 6      | pairCount     | uint64 LE                              |
| ...    | pair records  | pairCount × (uint64 NodeID, uint32 keyLen, [keyLen]byte key) |

Records are emitted in `graph.Mapper.Walk` order (shard-major,
intra-index-major) so the reader reconstructs the interning table
deterministically. A single key entry is capped at 1 GiB
(`maxMapperKeyLen`); a length prefix beyond that, a bad magic, an
unsupported version, or a truncated record all surface as
`snapshot.ErrMapperCorrupted`.

There are two on-disk layouts, distinguished by `formatVersion`:

- **version 1 (string keys)** — the per-record `key` bytes are the
  raw UTF-8 of the string key, with no codec framing. This layout is
  **frozen**: every `mapper.bin` produced for a string-keyed graph is
  byte-identical to the pre-codec writer, so cross-process
  byte-equality is preserved. Written by `snapshot.WriteMapperString`
  and read by `snapshot.ReadMapperString` into `MapperReadback.Pairs`.
- **version 2 (any other key type)** — the per-record `key` bytes are
  the opaque output of `txn.Codec[N].Encode` for the natural key,
  framed by the same `uint32` length prefix the v1 layout uses.
  Written by `snapshot.WriteMapper` (which delegates to
  `WriteMapperString` when `N` is `string`, so strings never emit
  version 2) and read by `snapshot.ReadMapperBytes` into
  `MapperReadback.RawPairs`.

`snapshot.LoadSnapshotFull` peeks the `formatVersion` prefix
(`peekMapperVersion`) and routes to the matching reader, so a single
load path serves both layouts without a codec of its own. The
recovery layer decodes the bytes back into `N`:
`snapshot.ApplyMapperToGraph` consumes `Pairs` directly for
string-keyed graphs, while `snapshot.ApplyMapperToGraphWithCodec`
decodes `RawPairs` through the store's codec.

#### WAL truncation for non-string keys (F3)

Before this change `mapper.bin` was written for string keys only, so
non-string-keyed checkpoints were not self-sufficient and the
checkpointer **retained** the WAL rather than truncating it
(preserving Durability at the cost of unbounded WAL growth). With the
codec-based version-2 layout, a checkpointer constructed with
`checkpoint.WithMapperCodec` emits `mapper.bin` for every key type, so
non-string checkpoints are now self-sufficient and **do** truncate the
WAL. An `int64`- or `[16]byte`-keyed store recovers from the snapshot
alone (`WALOps == 0`) after such a checkpoint.

Two deterministic crash-injection scenarios cover the truncation
window and prove full recovery on `SIGKILL`:
`checkpoint.post-snapshot-pre-truncate` (the snapshot is durable but
the WAL is intact) fires in `store/checkpoint.runCheckpoint`, and
`checkpoint.mid-truncate` (the WAL file has just been shrunk to zero)
fires inside `wal.Writer.Truncate`. Both are no-ops in production
(`GOGRAPH_CRASH_AT` unset) and exercised by
`store/recovery/checkpoint_crashinject_test.go`.

### `snapshot/indexes/<name>.bin` (binary, v2+ only)

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
   protocol documented above so the snapshot is a **self-sufficient**
   image of committed state: CSR adjacency *plus* `labels.bin`,
   `properties.bin`, registered indexes, and `mapper.bin` (the durable
   `NodeID→key` table). When the checkpointer was constructed with
   `checkpoint.WithMapperCodec`, the write goes through
   `snapshot.WriteSnapshotFullWithMapperCodec`, which emits `mapper.bin`
   for **any** key type. Without a codec the checkpointer calls
   `snapshot.WriteSnapshotFull`, which emits `mapper.bin` only for
   string-keyed graphs (the historical fallback).
4. Determine whether the snapshot is self-sufficient — i.e. whether
   its manifest carries `mapper.bin`, the only file that lets
   recovery rebuild the `NodeID→key` mapping *without* the WAL.
5. Call `wal.Writer.Sync` so the WAL is in a defined state.
6. If the snapshot is self-sufficient, call `wal.Writer.Truncate`
   to reclaim the WAL prefix now folded into the snapshot. The
   reclaimed byte count is recorded on `Stats.WALTruncBytes` and
   emitted via `store.checkpoint.wal_truncated_bytes`.
   If it is **not** self-sufficient (a non-string key type when no
   mapper codec was supplied, so `mapper.bin` was not written),
   truncation is **skipped** and the WAL is retained — truncating it
   would erase the only durable copy of the `NodeID→key` mapping and
   destroy committed data. The skip is surfaced via
   `store.checkpoint.truncate_skipped_not_self_sufficient`.
7. Release the store mutex.

Why this matters (audit gaps F2/F3, see `docs/acid-audit.md`): an
earlier checkpoint wrote a *CSR-only* snapshot and then truncated the
WAL unconditionally. Because a CSR-only snapshot carries neither
labels/properties nor a mapper, recovery after such a checkpoint
lost every committed label and property and could not even
reconstruct the adjacency by key — a **Durability** violation. The
self-sufficient snapshot plus the truncate-only-when-self-sufficient
guard close that gap for every key type: a committed transaction
always survives a checkpoint.

When the checkpointer is wired with `checkpoint.WithMapperCodec`
(gap F3), the snapshot carries `mapper.bin` for **every** key type,
so the self-sufficiency guard is satisfied and the WAL is truncated
after each checkpoint regardless of `N`. The WAL therefore stays
bounded for non-string-keyed stores as well.

Without a codec, non-string key types still fall through the
WAL-retained path (no truncation), because `mapper.bin` is then
written only for string keys; their WAL is not reclaimed by the
checkpointer in that configuration. Either way the Durability
guarantee holds: the guard never truncates a WAL whose snapshot
cannot stand alone.

The lock-during-IO trade-off is acknowledged in `checkpoint.go`:
for very large graphs this can stall writers and may be reworked
later to a position-tracked truncate (capture LSN under lock, write
snapshot lock-free, truncate up-to-LSN under lock).

## Composed shutdown

A WAL-backed store is assembled from independent pieces — a
`wal.Writer`, a `txn.Store` (or a `cypher.Engine` over it), and,
when steady-state WAL growth must be bounded, a background
`checkpoint.Checkpointer`. Tearing them down has **one correct
order**, and getting it wrong is a silent correctness bug rather than
a visible crash.

### Mandatory teardown order

1. **(Optional) take a final checkpoint** while the checkpoint loop is
   still running, so a clean shutdown folds the WAL tail into the
   snapshot and the next open replays the minimum. This is best-effort:
   already-committed transactions are durable in the WAL regardless of
   whether the final checkpoint runs. It **must** precede step 2 — once
   the loop is stopped a checkpoint can no longer be requested
   (`checkpoint.Trigger` / `TriggerCtx` then return
   `checkpoint.ErrCheckpointerStopped`).
2. **Stop the checkpoint goroutine** (`checkpoint.Checkpointer.Stop`).
   `Stop` blocks until the goroutine has exited, so once it returns no
   checkpoint can still be in flight.
3. **Close the WAL** (`wal.Writer.Close`), which flushes and fsyncs any
   buffered tail before releasing the file.

Stopping the checkpointer **before** closing the WAL is the invariant.
If the order is reversed — WAL closed while the checkpoint loop is
still alive — the loop's next `wal.Writer.Sync` / `wal.Writer.Truncate`
runs against a closed writer, returns `wal.ErrWriterClosed`, and that
error is **swallowed into the checkpointer's `Stats.LastError`** instead
of surfacing to the caller; worse, the goroutine keeps running past the
process's shutdown intent until its own ticker happens to observe a stop
signal — a goroutine leak. The correct order makes both impossible: the
loop is gone before the WAL is touched for the last time.

Quiescing writers is a **separate** responsibility and comes first: a
`txn.Store` transaction holds the store's single-writer semaphore from
`Begin` until `Commit`/`Rollback`, and a `cypher.Engine` write holds it
for the statement's duration. The shutdown sequence must stop admitting
new writes and let the active one finish **before** step 2, so no
transaction is mid-commit when the WAL is closed.

### `store.DB` — the composed owner

`store.DB` (package `github.com/FlavioCFOliveira/GoGraph/store`)
bundles the WAL writer and the optional checkpointer and runs exactly
that order in `DB.Close` / `DB.CloseCtx`, idempotently and safely under
concurrent callers, so each embedder does not re-derive (and risk
mis-ordering) the sequence:

```go
wlog, _ := wal.Open(walPath)
st := txn.NewStoreWithCodec(g, wlog, txn.NewStringCodec())
eng := cypher.NewEngineWithStore(st)

cp := checkpoint.New(cfg, g, wlog, &unusedMu,
    checkpoint.WithCommitSerialiser[string, float64](st.RunUnderCommitLock),
    checkpoint.WithMapperCodec[string, float64](st.Codec()))
cp.Start(ctx)

db := store.New(wlog,
    store.WithCheckpointer(cp),   // omit for a WAL-only store
    store.WithFinalCheckpoint())  // omit to skip the step-1 compaction
defer db.Close()                  // step 1 (if enabled) → step 2 → step 3
```

`DB.Close` returns the WAL-close error — the one that matters for
durability — and discards the best-effort final-checkpoint error. It
runs the teardown exactly once: a second or racing `Close` (or a later
`Close` after a `CloseCtx`) returns the same result and never produces a
spurious `wal.ErrWriterClosed` from a double WAL close. `DB.CloseCtx`
bounds only the optional final checkpoint with its context; the stop and
the WAL close always run to completion so the goroutine is always joined
and the file always released, even on context cancellation.

`store.DB` satisfies `io.Closer`, so it drops into any owner that closes
an `io.Closer` on shutdown.

### Bolt server adoption

A `bolt/server.Server` backed by a WAL-enabled engine takes the composed
owner via `Options.Closer io.Closer` (typically a `*store.DB`). The
server closes it **after** it has drained every active connection — so
the WAL/checkpoint teardown runs only once no in-flight transaction can
still be writing. Both documented stop mechanisms reach that teardown:
`Server.Shutdown` closes it on its drain-success branch, and
`Server.Serve` closes it on its own exit path once its connection drain
completes (e.g. when the `Serve` context is cancelled):

```go
db := store.New(wlog, store.WithCheckpointer(cp))
srv, _ := server.NewServer(eng, server.Options{Auth: auth, Closer: db})
// …
_ = srv.Shutdown(ctx) // drains connections, THEN tears the durability stack down
// — or, equivalently, cancel the ctx passed to srv.Serve: once Serve's
// drain completes, its exit path performs the same post-drain teardown.
```

The close is once-guarded inside the server: whichever of `Serve` or
`Shutdown` drains first runs it, and the other observes the same cached
result, so the closer is never closed twice (including on a double
`Shutdown`) and need not be idempotent itself.

The closer is **not** torn down on `Shutdown`'s drain-timeout or
context-cancellation paths: an undrained connection may still hold an
open transaction, and closing the WAL underneath it is exactly what the
ordering rule forbids. In those cases the connections are abandoned; a
still-running `Serve` remains blocked on the same drain and performs the
post-drain close when the abandoned connections eventually finish (idle
timeout, transaction reap, client exit). Only if a full drain never
completes is the closer left for process exit.

## Recovery procedure

`recovery.Open[N, W](dir, opts)` and its context-aware twin
`recovery.OpenCtx[N, W](ctx, dir, opts)` return a `Result`
containing the rebuilt `*lpg.Graph[N, W]` plus:

- `SnapshotHit bool` — whether `snapshot/manifest.json` was found
  and validated.
- `SnapshotSchemaVersion int` — the on-disk manifest version of the
  snapshot that was loaded (1 for legacy CSR-only directories, 2 for
  the labels + properties + indexes shape, 3 when `mapper.bin` is also
  present). 0 when no snapshot was found.
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
- `TailErr error` — why WAL replay stopped before the end of the
  file, or nil at a clean EOF. A benign torn tail
  (`wal.ErrTornFrame`) is the normal crash-after-fsync state and is
  tolerated; genuine corruption inside an already-durable frame
  (`wal.ErrCRCMismatch`, `wal.ErrBadMagic`,
  `wal.ErrUnsupportedVersion`, `wal.ErrFrameTooLarge`, or
  `recovery.ErrUnsupportedRecordVersion`) is surfaced.

On genuine corruption, `recovery.Open` / `OpenCtx` are **fail-stop**:
the function returns that error (the committed prefix is still placed
in `Result.Graph` for diagnostics) instead of returning nil and
hiding the damage in `TailErr`. A benign torn tail returns a nil
error and the committed prefix. The boolean helper
`Result.IsClean()` is the exact complement of this contract — it
returns false if and only if the function returned an error — so a
recover-then-append caller can branch on either signal:

```go
res, err := recovery.Open[int64, float64](dir, opts)
if err != nil {
    return err // corrupt WAL: do not append onto it
}
// equivalently: if !res.IsClean() { ... }
w, err := wal.Open(walPath) // safe to append: clean or benign torn tail
```

Appending to a corrupt WAL would permanently embed the corruption and
silently drop every committed op that followed the bad frame, so the
safe behaviour — refusing to append — is the default. Every shipped
example under `examples/` that recovers then reopens the WAL for
append checks this signal before doing so.

Component apply order during open is fixed. When the snapshot is v3
(carries `mapper.bin`) the mapper is restored first and the snapshot
CSR is applied on top of it **before** WAL replay, so the load is
self-sufficient even when the WAL is empty: mapper.bin → snapshot CSR
→ WAL replay → labels.bin → properties.bin. For v1/v2 snapshots there
is no durable mapper, so the mapper is instead populated implicitly by
the WAL replay and the order is: WAL replay (rebuilds mapper + CSR
adjacency) → labels.bin → properties.bin. Either way the properties
pass runs last, so the mapper is fully populated and the edge bag is
in place — property records that point at endpoints the apply phase
has not yet seen are skipped and metered rather than aborting
recovery.

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

*Last reviewed: 2026-06-12 against commit `ec76e6f`. If you edit code referenced by this document and do not update this footer, the doc-staleness lint will flag the PR.*
