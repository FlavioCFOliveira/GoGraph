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

Three sibling packages under `store/` are **not** part of that durability
composition and are documented elsewhere: `store/csrfile` (the Tier 2
mmap-backed CSR file format, specified in [`csrfile-v1.md`](csrfile-v1.md)),
`store/bulk` (a bulk-load path that bypasses the WAL stack and writes a
`csrfile` directly), and `store/bulkimport` (builds an LPG from a record
stream so the result can be published as a snapshot).

## Durability contract

`Tx.Commit` writes every buffered op to the WAL as a v3 frame and appends a
single `OpCommit` marker frame — both inside one `wal.Writer.AppendRun` call, so
the transaction's frames are one contiguous run that no other appender can
interleave with (see
[`design-wal-transaction-contiguity.md`](design-wal-transaction-contiguity.md))
— then calls `wal.Writer.SyncGroup` with the offset that run returned, and only
then applies the ops to the live graph. A process killed at any point during
this sequence is recoverable by `recovery.Open`/`OpenCtx`:

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
while the single-threaded commit cost is unchanged.

Each committer's **durability watermark is its own**: `wal.Writer.AppendRun`
returns the offset immediately after the run's last frame, and the committer
passes that offset to `SyncGroup`. The watermark cannot be recovered afterwards
from the writer's accepted offset, because that offset is shared — another
appender advances it, and a durability failure rewinds it to the durable size.
Deriving the watermark from it instead told a committer whose frames were already
on stable storage that its commit had failed, while recovery went on to replay
the (durable, fully marked) transaction: a durable transaction nobody
acknowledged (#2322). For the same reason `SyncGroup` tests the caller's
watermark against the durable size **before** it consults the writer's sticky
failure — the two are not mutually exclusive, since the failure can belong to a
later round that failed after a leader had already fsynced this caller's marker.
`wal.Writer.SyncBuffered` remains for callers with nothing of their own to
acknowledge (an empty commit's courtesy flush of a buffered tail) and must never
be used to decide a commit's fate.

A commit is acknowledged only after the covering fsync, so atomicity and
durability are preserved (a failed group fsync fails every member of the group
whose frames that fsync did not cover), and the in-memory state is applied
strictly in WAL sequence order, so isolation is preserved (the
group-commit measurements are in
[docs/benchmarks/v0.3.1.md](benchmarks/v0.3.1.md)). Every store is a typed store
on the v3 commit path; the legacy v1
fmt-codec write path was removed (see "WAL payload schema" below), so
there is no non-durable, non-atomic per-op framing left in the module.

`Tx.Rollback` neither writes to the WAL nor mutates the graph;
nothing is durable, nothing is visible, and the writer deregisters.

`Tx.Commit` also has two **refusals that make nothing durable and apply
nothing**, so an atomicity boundary is preserved rather than half-crossed. It
returns `txn.ErrFieldTooLong` when a buffered op carries a string too long for
the length prefix its frame reserves (65535 bytes for a label, a property key
or a schema identifier; 4 GiB for a property value), and again when the
transaction would leave one edge handle carrying more than 1 Mi labels or more
than 1 Mi properties — a record `store/snapshot` cannot capture, and therefore
one that would block every subsequent checkpoint while the WAL grew unbounded.
The transaction consumes a sequence, applies nothing, and leaves the store
usable.

## Transaction isolation

**Writes do not serialise.** Since rmp #2306 the transactional layer's
concurrency control is MVCC and nothing else: `Begin`/`BeginCtx` register the
transaction as an admitted writer and `Commit`/`Rollback` deregister it, but the
registration excludes no other writer. A write-write collision between two
transactions is DETECTED at commit by first-updater-wins on the version chain and
reported as a serialization conflict, rather than prevented by a lock. The
registration exists for one purpose: a quiesce
(`txn.Store.RunUnderCommitLock`) closes the admission gate and drains the
admitted writers to zero, which is what lets the checkpointer — and
`store.DB.Close` when it is wired with `store.WithQuiesce` — touch the WAL with
no commit in flight.

Reads are unaffected: they resolve through an MVCC read view
(`lpg.Graph.ReadAt` → `lpg.ReadView`) pinned at an instant, and the adjacency
traversal underneath it runs over `csr.CSR` / `adjlist.AdjList` without taking a
lock on the hot path. The MVCC design is documented separately under
`docs/design-mvcc-*.md`; this document covers only how that state is made
durable.

This replaced a capacity-one semaphore that made the layer single-writer. Retiring
it was throughput-neutral — it was released after the WAL append and never covered
the coalesced fsync that dominates a durable commit — so its removal is an
architectural correction rather than an optimisation. See
[benchmarks/store-semaphore-retirement-2026-08-04.md](benchmarks/store-semaphore-retirement-2026-08-04.md).

## File layout on disk

```
<dir>/
  wal                — single un-segmented, appended file of framed records
  wal.lock           — 0-byte sentinel carrying an exclusive flock(2). Held for
                       the writer's lifetime; a second process opening the same
                       WAL gets wal.ErrWALLocked instead of blocking.
  snapshot/
    manifest.json    — versioned index of files + CRC32C per file, framed by a
                       16-byte CRC32C trailer (see "Manifest integrity")
    csr.bin          — serialised CSR (vertices / edges / weights / handles)
    labels.bin       — serialised LPG labels (v2+ manifests only)
    properties.bin   — serialised LPG typed properties (v2+ manifests only)
    mapper.bin       — durable NodeID→key interning table (v3 manifests only)
    tombstones.bin   — durable node-removal set (only when ≥1 node is tombstoned)
    edgehandles.bin  — per-handle edge type/property records (only when ≥1 record)
    constraints.bin  — declared schema constraints (only when the writer is
                       given a constraint set)
    indexdefs.bin    — declared secondary-index definitions (only when the
                       writer is given an index-definition set)
    indexes/         — secondary indexes registered with index.Manager
      <name>.bin     — one file per registered serialisable index (v2+)
  snapshot.tmp/      — transient: the directory being assembled, before it is
                       renamed onto `snapshot/`. A leftover one means a crash
                       mid-write; the next write and the next recovery both
                       remove it, and nothing ever reads it.
  snapshot.bak/      — transient: the PREVIOUS snapshot, archived for the
                       duration of the publish swap. A leftover one is the only
                       surviving image, and recovery PROMOTES it — see
                       "Publishing a snapshot" below.
```

Only `manifest.json` and `csr.bin` are always present. The full writer always
emits `labels.bin` and `properties.bin` too, as empty tables when the graph has
none, but the loader tolerates their absence so a v1 or hand-written v2
directory still opens. Every remaining component is **additive and optional**:
it is written only when the graph has something to put in it, its absence loads
as an empty component, and none of them bumps the manifest version — see
"Snapshot file format" below.

The WAL is one file. It is **not** segmented: `wal.Writer` reclaims space by
rewriting the surviving suffix under an atomic rename
(`wal.Writer.TruncatePrefix`), not by unlinking whole segments.

## WAL payload schema

A WAL frame is `magic("GGWA") | uint16 version | uint32 length | uint32 crc32c`
followed by the payload — a 14-byte header (`wal.HeaderSize`), with
`wal.CurrentVersion` currently `1`. The frame layer is documented in
[`store/wal/FORMAT.md`](../store/wal/FORMAT.md); this section covers the
payload the transaction layer puts inside it. A single frame payload is capped
at 1 GiB, so a corrupt or crafted length field cannot force an unbounded
allocation before the CRC has a chance to reject the frame.

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

`OpKind` values currently occupy 0x01..0x17 (see the table below);
`OpCommit`, the v3 commit marker, sits at 0x0D in the middle of that range
because every kind added after it was appended at the end to keep the
pre-existing wire identities stable. The rest of the low byte region is free.
The magic bytes 0xFE/0xFD are chosen far outside that range so a reader
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
the trailing `OpCommit` marker is the atomicity boundary — recovery applies a
v3 transaction's buffered ops only on reading its durable marker (see the
Durability contract above). Typed stores emit v3; v2 frames remain fully
readable so existing on-disk WALs replay unchanged.

The `OpCommit` marker is `version | OpCommit | uint64 txnSeq | uint64 commitTS`,
all little-endian. `commitTS` is the **MVCC instant at which the transaction
becomes visible**, and it is what lets recovery **derive** the MVCC clock from
the WAL instead of trusting a persisted counter — the shape InnoDB and Memgraph
both settled on after removing theirs (see
[`design-mvcc-clock-recovery.md`](design-mvcc-clock-recovery.md)). It is written
by `txn.Tx.CommitWALOnly(commitTS)`, which is what the Cypher engine calls after
allocating the instant; the store's own `txn.Tx.Commit` has no MVCC clock and
passes zero.

**No format bump was needed, and that was verified rather than assumed.** The
frame header is `magic | version | length | crc32c`, so it carries no per-record
shape, and the marker's body was previously empty and ignored by the replay
state machine. So an **older reader** ignores the extra eight bytes, and a
**newer reader on an older file** sees an empty body and contributes nothing to
the derived maximum. The compatibility policy is therefore *"absent body means
no timestamp"* — a test obligation (`recovery.Op.CommitTS` reports presence
separately from value) rather than a version negotiation. Neither
`wal.CurrentVersion` nor `txn.OpRecordV3` changed.

| `OpKind`                       | Value | Mutation                                              |
|--------------------------------|-------|-------------------------------------------------------|
| `OpAddEdge`                    | 0x01  | `AddEdge(src, dst, zero)` — no weight payload         |
| `OpSetNodeLabel`               | 0x02  | `SetNodeLabel(node, label)`                           |
| `OpSetEdgeLabel`               | 0x03  | `SetEdgeLabel(src, dst, label)`                       |
| `OpAddEdgeWeighted`            | 0x04  | `AddEdge(src, dst, w)` — typed weight payload         |
| `OpAddNode`                    | 0x05  | `AddNode(key)`                                        |
| `OpRemoveNode`                 | 0x06  | `RemoveNode(key)` — records a tombstone               |
| `OpRemoveNodeLabel`            | 0x07  | `RemoveNodeLabel(node, label)`                        |
| `OpSetNodeProperty`            | 0x08  | `SetNodeProperty(node, key, value)`                   |
| `OpDelNodeProperty`            | 0x09  | `DelNodeProperty(node, key)`                          |
| `OpRemoveEdge`                 | 0x0A  | `RemoveEdge(src, dst)` — the first src→dst slot       |
| `OpSetEdgeProperty`            | 0x0B  | `SetEdgeProperty(src, dst, key, value)`               |
| `OpDelEdgeProperty`            | 0x0C  | `DelEdgeProperty(src, dst, key)`                      |
| `OpCommit`                     | 0x0D  | **control record** — the v3 atomicity boundary        |
| `OpAddEdgeH`                   | 0x0E  | `AddEdgeHIfAbsent(src, dst, w, handle)`               |
| `OpSetEdgeLabelByHandle`       | 0x0F  | `SetEdgeLabelByHandle(src, dst, handle, label)`       |
| `OpSetEdgePropertyByHandle`    | 0x10  | `SetEdgePropertyByHandle(src, dst, handle, k, v)`     |
| `OpRemoveEdgeInstanceByHandle` | 0x11  | `RemoveEdgeInstanceByHandle(src, dst, handle)`        |
| `OpCreateConstraint`           | 0x12  | **schema record** — CREATE CONSTRAINT                 |
| `OpDropConstraint`             | 0x13  | **schema record** — DROP CONSTRAINT                   |
| `OpCreateIndex`                | 0x14  | **schema record** — CREATE INDEX                      |
| `OpDropIndex`                  | 0x15  | **schema record** — DROP INDEX                        |
| `OpDelEdgePropertyByHandle`    | 0x16  | `DelEdgePropertyByHandle(src, dst, handle, key)`      |
| `OpRemoveEdgeByHandle`         | 0x17  | `RemoveEdgeByHandle(src, dst, handle)`                |

Three groups sit after `OpCommit` because each was appended at the end of the
enum so no pre-existing value moved: the **stable-edge-handle** kinds
(0x0E–0x11, 0x16–0x17), which append an 8-byte little-endian handle after the
body a handle-less op of the same shape carries; and the **schema-DDL** kinds
(0x12–0x15), which carry no node endpoints at all — their body is a
`ConstraintKind`/`IndexKind` tag followed by length-prefixed strings, encoded
independently of the node `Codec`. Recovery surfaces the schema records through
`recovery.Result.Constraints` / `recovery.Result.Indexes` rather than applying
them to the graph, because a constraint or index definition is engine schema,
not graph topology.

`OpAddEdgeWeighted` is the only kind whose v2 layout differs from the
plain `OpAddEdge` shape (see below). A pre-T8 reader that only knows
about `OpAddEdge` skips `OpAddEdgeWeighted` frames as an unknown
kind, which is the intended forward-compat behaviour: the typed
weight payload cannot be inferred without the registered
`WeightCodec`. The same rule governs every kind above: a reader that predates
a kind surfaces its frames as an unknown kind rather than mis-parsing them.

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
`Codec[N]` and a `WeightCodec[W]`. `Tx.AddEdge(src, dst, w)` records every
commit as an **`OpAddEdgeH` frame (kind byte `0x0E`)**: the weighted-edge body
— the weight payload between the codec-encoded endpoints and the trailing
label, framed by the registered `WeightCodec` — followed by an 8-byte
little-endian stable edge handle. The handle is minted from the graph's
monotone counter when the op is buffered, so its value is fixed in the frame
before the fsync, and replay re-inserts the edge through
`lpg.Graph.AddEdgeHIfAbsent`, which is idempotent against a snapshot that
already carries it.

`OpAddEdge` (`0x01`) and `OpAddEdgeWeighted` (`0x04`) are therefore **read-only
legacy kinds**: no production path emits either one any more, and both remain
decoded so existing on-disk WALs replay unchanged.

Stores constructed via `NewStoreWithCodec` have no `WeightCodec`. They
accept zero-valued `AddEdge` calls (which buffer an `OpAddEdgeH` frame with no
weight payload, applied with `var zero W`) and reject non-zero weights with
`txn.ErrNoWeightCodec`. Callers that need durable weighted edges must
upgrade to `NewStoreWithOptions`.

`recovery.Open` and its context-aware twin `recovery.OpenCtx` are the
recovery entry points; `recovery.OpenFS` / `recovery.OpenCtxFS` are the same
two over an injected filesystem, used by the deterministic-simulation harness.
The deprecated `OpenString`, `OpenWithCodec`, and `OpenWithOptions` wrappers
were removed together with the v1 WAL format; pass the codecs explicitly via
`recovery.Options` instead:

| Open path                                 | N decoded via | W decoded via | OpAddEdge | OpAddEdgeWeighted |
|-------------------------------------------|---------------|----------------|-----------|-------------------|
| `Open` / `OpenCtx` (canonical)            | typed `Codec[N]` (required) | typed `WeightCodec[W]` (mirrors the producer) | applied with `W=zero` | applied with decoded weight |

Only `Codec` is required: a nil `Codec` fails with `recovery: nil codec` before
any frame is read. `WeightCodec` is not validated at the door, but it must
**mirror the producer store** — nil for a codec-only store
(`txn.NewStoreWithCodec`, whose frames carry no weight bytes and replay with the
zero value of `W`), non-nil for a weight-codec store
(`txn.NewStoreWithOptions`).

Getting that wrong is **not** a silent degradation to zero weights. A weighted
frame whose weight cannot be decoded is an undecodable op inside an
already-committed transaction, so replay stops at it: `Open` returns nil, the
graph holds only the prefix before it, `Result.TailErr` wraps
`recovery.ErrCommittedTxnCorruptOp` and `Result.IsClean()` is false. Measured on
a two-commit weighted WAL with no snapshot: opening with the producer's
`WeightCodec` recovered `WALOps=2`, 3 nodes and 2 edges; opening the same
directory with a nil `WeightCodec` recovered `WALOps=0`, 0 nodes and 0 edges,
`IsClean()=false`, and logged a `WARN` naming the frame. Supply the producer's
codec.

Forward compatibility: a pre-T8 WAL that contains only `OpAddEdge`
frames replays cleanly under `Open`. The apply path writes `var zero W`
for those records and reserves the typed weight payload for
`OpAddEdgeWeighted` and `OpAddEdgeH` frames only. The recovery test
`TestTxn_ForwardCompat_PreT8WALReplays`
(`store/recovery/weight_replay_test.go`) locks this contract.

### Generic recovery API

`recovery.Open[N, W](dir, opts)` is the canonical recovery entry
point. `opts` is a `recovery.Options[N, W]` carrying `Codec[N]` and
`WeightCodec[W]` — the same two codecs the typed Store was built with — plus
`MaxTxnOps`, which bounds how many ops recovery buffers for a single v3
transaction before its `OpCommit` marker. `Codec` is required; `WeightCodec`
mirrors the producer (see above); `MaxTxnOps` is optional and 0 selects
`txn.DefaultMaxTxnOps`, the same default the producer uses. A transaction whose
buffered op count exceeds the resolved cap fails recovery with
`recovery.ErrTransactionTooLarge` rather than allocating in proportion to an
unbounded, marker-less run — a legitimately huge transaction or a crafted WAL
tail alike.

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

`Result[N, W].SnapshotSchemaVersion` reports the on-disk manifest
version of the snapshot that was loaded (`1` for legacy CSR-only
directories, `2` for the labels + properties + indexes shape, `3` when
`mapper.bin` is also present). The field is `0` when no snapshot was found, so
callers can branch on `res.SnapshotSchemaVersion >= 2` to detect a v2 directory
without re-reading the manifest. The full field list is under
"Recovery procedure" below.

`recovery.Options[N, W]`'s `Codec` and `WeightCodec` mirror
`txn.Options[N, W]` field-for-field, so a call site that already holds a
`txn.Options` value converts with `recovery.OptionsFromTxn(opts)`. The two
types are **not** interchangeable by a direct Go conversion: `recovery.Options`
adds `MaxTxnOps` (recovery-specific; the producer cap is set on the store) and
`txn.Options` adds `ResumeTxnSeq` (producer-specific; see
`recovery.Result.NewStore` under "Recovery procedure"). Keeping the
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
   subsequent frames are `OpAddEdgeH` (kind `0x0E`), which carries the
   weight payload plus the stable edge handle.

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

Four further components are **optional and additive** (like `indexes/`). Each
is emitted only when the graph has something to put in it, none of them bumps
the manifest version — a manifest still reports v3 — and each loads as an empty
readback when absent, so a snapshot that does not need one is byte-identical to
one produced before the component existed:

| Component        | Emitted when                                                    |
|------------------|-----------------------------------------------------------------|
| `tombstones.bin` | the graph currently has ≥ 1 tombstoned node                       |
| `edgehandles.bin`| the graph currently has ≥ 1 per-handle edge type/property record  |
| `constraints.bin`| the writer was given a non-empty constraint set                   |
| `indexdefs.bin`  | the writer was given a non-empty index-definition set             |

See [Node and edge deletion durability](#node-and-edge-deletion-durability) for
`tombstones.bin`. `constraints.bin` and `indexdefs.bin` are what let a
checkpoint truncate the WAL prefix that first declared a constraint or an index
without losing it; the checkpointer refuses to truncate when the graph has
either and the snapshot does not carry the matching component (see
[Checkpoint policy](#checkpoint-policy)).

`snapshot.LoadManifest` accepts all three versions; a manifest whose
version exceeds `ManifestVersion` (4 or higher) surfaces
`snapshot.ErrManifestUnsupported`. The high-level
`snapshot.LoadSnapshotFull` reads every shape and returns empty
`LabelsReadback` / `PropertiesReadback` / `MapperReadback` /
`TombstonesReadback` / `EdgeHandlesReadback` / `ConstraintsReadback` /
`IndexDefsReadback` for components that are absent.

The **checkpointer** does not call `WriteSnapshotFull*` at all: it serialises
the graph into a `snapshot.Capture` under an MVCC instant and publishes it with
`snapshot.WriteCapture`, which writes the same directory shape. The
`WriteSnapshotFull*` family remains the present-time entry point for callers
that hold their own exclusion; see [Checkpoint policy](#checkpoint-policy).

### Node and edge deletion durability

Node deletion is a **tombstone**, not a slot release: the `NodeID`→key
mapper entry is permanent (NodeID stability is a hard contract), so
`lpg.Graph.RemoveNode` records the removed `NodeID` in an in-memory
tombstone set and the read paths (`IsTombstoned`, `LiveOrder`, the
Cypher `AllNodesScan`) filter it. For that deletion to **survive a
store reopen**, the tombstone set is round-tripped through persistence:

- **Snapshot** — the checkpointer writes the sorted tombstone set to
  `tombstones.bin` (see the format table below) before truncating the
  WAL. Like every component its CRC32C lives in the manifest entry, so a
  corrupt file is detected at load. On load,
  `snapshot.ApplyTombstonesToGraph` restores the set
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

### Publishing a snapshot

Publication is crash-atomic on any POSIX filesystem, and it is a **three-step
swap**, not a single rename:

1. Assemble the new directory under `snapshot.tmp/`, `fsync` every component
   file, then `fsync` the staging directory itself.
2. `rename(snapshot → snapshot.bak)` — atomically archive the live snapshot.
   On the first checkpoint this fails with `os.ErrNotExist`, which is fine.
3. `rename(snapshot.tmp → snapshot)`, then **`fsync` the parent directory** so
   the new directory entry survives the writeback window. Only after that
   fsync is `snapshot.bak/` removed.

Both renames are atomic, so at every instant at least one complete snapshot
exists on disk. Should step 3's rename fail, the archive is renamed back so the
caller retries against an intact live snapshot.

The earlier `RemoveAll(snapshot)` → `Rename(tmp, snapshot)` sequence had a fatal
window: a crash between the two left **no** live snapshot while the staging
directory was stranded under its `.tmp` name — and because an earlier checkpoint
had already truncated the WAL, recovery would silently rebuild an empty graph.

The counterpart lives in `recovery.Open`: before it probes for
`snapshot/manifest.json` it removes any stale `snapshot.tmp/` and, if the live
manifest is missing while `snapshot.bak/manifest.json` exists, it **promotes**
the backup by renaming it back and fsyncing the parent directory. Both the
promotion rename failing and its fsync failing are **fail-stop**, because the
backup is then the only durable copy of the checkpointed state. The window
between that rename and that fsync is itself a crashpoint,
`recovery.snapshot-promote-post-rename-pre-fsync`, exercised by
`store/recovery/snapshot_promote_crashinject_test.go`.

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
  "created_at": "2026-05-19T14:00:00Z",
  "graph_config": {"directed": true, "multigraph": true},
  "files": [
    {"name": "csr.bin",        "size": 24014, "crc32c": 305419896},
    {"name": "labels.bin",     "size":   312, "crc32c":  47119123},
    {"name": "properties.bin", "size":   568, "crc32c": 837469102},
    {"name": "mapper.bin",     "size":  4096, "crc32c": 314159265}
  ],
  "indexes": [
    {"name": "labels.nodes",  "size":  1024, "crc32c": 123456789}
  ],
  "version": 3,
  "order": 1000,
  "size": 5000,
  "commit_ts": 4271,
  "indexes_commit_ts": 4271,
  "index_builder_epoch": 1,
  "integrity": "crc32c-trailer"
}
```

(The writer emits pretty-printed JSON in that field order, followed by the
16-byte binary trailer described under "Manifest integrity" below. The examples
above are shown compacted for readability.)

A v3 manifest is a v2 manifest with an extra `mapper.bin` entry in
`files`. The `version` field is bumped to `3` only when `mapper.bin`
is present; the writer stamps `2` for non-string-keyed snapshots that
omit the mapper. `snapshot/csr.bin`, `labels.bin`, and
`properties.bin` are byte-identical across v2 and v3.

**`graph_config` (optional).** The originating graph's `directed` /
`multigraph` / `weightless` shape, so recovery reconstructs the same adjacency
variant. It is a pointer with `omitempty`, so it is absent from every snapshot
written before the field existed and from the legacy CSR-only writer, which has
no live graph to read; a reader that finds it absent defaults to the historical
recovery behaviour (`Directed: true, Multigraph: true`).
`adjlist.Config.MaxShardCapacity` is deliberately **not** persisted — it is a
runtime growth bound, and re-imposing it could make recovery itself fail with
`adjlist.ErrShardFull` while replaying data that legitimately exceeds the cap.
A recovered graph is always reconstructed unbounded.

#### Manifest integrity: the CRC32C trailer

`snapshot.WriteManifest` appends a fixed **16-byte trailer** after the JSON
value:

| Offset | Width | Field     | Value                                          |
|--------|-------|-----------|------------------------------------------------|
| 0      | 8     | magic     | `00 47 47 4D 41 4E 49 46` (`\0GGMANIF`)         |
| 8      | 4     | algorithm | uint32 LE, `1` = CRC32C (Castagnoli)           |
| 12     | 4     | checksum  | uint32 LE, CRC32C over every byte before the trailer |

The checksum covers the whole JSON document — every key name, every value, and
any whitespace up to the trailer — and never itself, so it is not
self-referential. The manifest is read first and carries the CRC32C of every
other component, so a manifest that is itself wrong makes every downstream check
meaningless; integrity is therefore enforced at the **file-framing** layer and
never at the JSON-schema layer. That separation is what lets the schema stay
open: unknown fields are still ignored and an absent field still decodes to its
zero value, yet "absent" can now only mean the writer genuinely omitted the
field, never that corruption renamed its key.

`LoadManifest` adjudicates the trailer region asymmetrically, and the asymmetry
is the whole mechanism:

- An **empty** region (nothing but ASCII whitespace) means the file predates the
  trailer. It is accepted unverified, `Manifest.IntegrityVerified` is false, and
  `store.snapshot.LoadManifest.unverified` is incremented.
- A **non-empty** region must be a well-formed, verifying trailer. There is no
  third outcome — otherwise a single flipped byte in the magic would demote a
  protected manifest to "legacy, accepted unverified".

The `integrity` field (`"crc32c-trailer"`) covers the one case the trailer
cannot speak for: a manifest whose trailer was lost entirely (a zeroed tail
block, a truncating copy). Losing the protection then requires two independent
damages rather than one.

`ManifestVersion` is deliberately **not** bumped for the trailer:
`json.Decoder` stops at the end of the first complete value, so a build that
predates the trailer never reads past the closing brace. The frozen v1 fixture
still loads byte-for-byte unchanged.

CRC32C detects accidental corruption — a flipped bit, a torn block, a bad
cable. It is **not** a message authentication code: an attacker who can rewrite
`manifest.json` can recompute the trailer. The defence against a hostile store
directory remains the surrounding controls (`O_NOFOLLOW` component opens,
`snapshot.DefaultMaxManifestBytes` = 32 MiB, the per-component allocation
bounds), not this checksum.

**`commit_ts` (optional).** A manifest written from a graph with an MVCC clock
also carries `"commit_ts": <uint64>` — the **MVCC instant the image was captured
at**. Recovery seeds the derived clock floor from it and then folds the WAL's
maximum on top, so a reopened graph never re-mints an instant the image already
contains (see [`design-mvcc-clock-recovery.md`](design-mvcc-clock-recovery.md)).
It is the quantity Memgraph reads back as `info.start_timestamp`.

This is the half of the derivation that matters in a checkpointed directory: a
checkpoint truncates the WAL prefix, so the instants of everything the image
folded are no longer in the log at all.

**`indexes_commit_ts` (optional).** The MVCC instant the `indexes/<name>.bin`
payloads are declared to describe. It is written **only** by the quiesced
checkpointer path, which pairs its MVCC instant with the WAL watermark so
everything committed afterwards is in the WAL suffix a recovery replays. The
present-time writers (`snapshot.WriteSnapshotFull` and its siblings) capture with
a nil instant and have no such pairing, so they publish **no** watermark.

**Absent means never hydrate.** That is the back-compat guarantee: every snapshot
written before this field existed, and every snapshot a present-time writer
produces, has its indexes rebuilt from the recovered graph exactly as before.
`omitempty` keeps a watermark-less manifest byte-identical to what previous
builds wrote, so the `version` field is **not** bumped — the same additive-JSON
reasoning `commit_ts`, `indexdefs.bin` and `indexes/` already rely on. The field
sits inside the region the manifest trailer checksums, so a flip in its key name
fails the checksum rather than silently losing the optimisation.

**No version bump.** The manifest is JSON, so an older reader ignores the field
and a newer reader on an older manifest decodes zero — the same *"absent means no
timestamp"* policy the `OpCommit` body uses. `omitempty` keeps a manifest with no
instant (the legacy CSR-only writer, which has no graph in hand, and any
non-MVCC graph) byte-identical to what previous builds wrote.

**`index_builder_epoch` (optional).** Which secondary-index **builder** produced
the `indexes/<name>.bin` payloads, as opposed to *when* they were taken. The full
writer stamps `snapshot.CurrentIndexBuilderEpoch` (currently `1`) on every
snapshot it publishes — unconditionally, including when it publishes no payload
and no watermark, because a snapshot that gains its first index later must not
inherit a stale absence. A reader hydrates a payload only when the manifest names
**exactly** its own epoch.

The field exists because an index payload is *durable*. Two backfill defects
(rmp #2778 on `CREATE INDEX`, rmp #2792 on `CREATE CONSTRAINT`) wrote index
entries for values an explicit transaction had eagerly written and rolled back;
a checkpoint then persisted the fabricated entry, and a reopen *loads* it rather
than rebuilding it — so fixing the builder does not heal a store already on disk.
Measured on a two-build experiment, a store written by the pre-fix build and
reopened on the fixed build reported `hydrated=2 rebuilt=0` and answered a seek
for the rolled-back value with the row of a node that never held it, while the
seek for that node's real value returned nothing. A pure-WAL store is not
affected: recovery rebuilds from the committed graph.

Refusing a **newer** epoch as well as an absent one is deliberate: this build
cannot know what a future builder writes, and a rebuild is always correct, so the
comparison is equality rather than "at least".

**Absent means never hydrate**, exactly like the watermark, and for the same
back-compat reason: every snapshot written before the field rebuilds its indexes
once, `O(N)` in the mapper length, on its first open — after which the next
checkpoint stamps the current epoch and the store hydrates again. The cost was
measured, not assumed: at 50 000 nodes with four registered indexes on an Apple
M4 without `-race`, a whole first open cost **73.79 ms ±1%** with the epoch
absent against **56.99 ms ±2%** with it present — a one-time **+16.80 ms**
(+29.48%, `p=0.000`, `n=10`), against a same-code noise floor of about 3% on the
same host. The benchmarks are `BenchmarkIndexBuilderEpochFirstOpen` (which drives
the field) and `BenchmarkRecoveredIndexPopulation` (which isolates the
rebuild-versus-hydrate step), both in package `cypher`.

`omitempty` keeps an epoch-less manifest byte-identical to what previous builds
wrote, so the `version` field is **not** bumped. A flip in the key name zeroes
the field into a *rebuild* rather than into a hydration, and fails the manifest
trailer checksum on top of that.

**Bump the epoch** whenever a defect is fixed in what a backfill *writes into* an
index — the values, the node set, or the label gate it resolves. Do **not** bump
it for a payload serialisation change (the payload carries its own magic and
version, and an index that refuses a payload already falls back to a rebuild) or
for a performance rewrite.

### `snapshot/csr.bin` (binary, identical across v1, v2 and v3)

| Offset  | Field            | Type                                    |
|---------|------------------|-----------------------------------------|
| 0       | nVertices        | uint64 LE                               |
| 8       | nEdges           | uint64 LE                               |
| 16      | hasWeights       | uint8 (0 or 1)                          |
| 17      | weightSizeBytes  | uint8 (0, 1, 2, 4, 8, or 0xFF sentinel) |
| 18      | vertices         | uint64[nVertices]                       |
| ...     | edges            | uint64[nEdges]                          |
| ...     | weights          | see below (optional)                    |
| ...     | hasHandles       | uint8 (optional trailing)               |
| ...     | handles          | uint64[nEdges] (opt.)                   |

The weights section has **two** shapes, selected by `weightSizeBytes`:

- **Fixed-width (1, 2, 4 or 8).** A dense `raw[weightSize·nEdges]` array, one
  native little-endian element per edge slot. This is what `float64`, `int64`
  and the other fixed-width primitives write, and it is unchanged.
- **Codec-encoded (`0xFF`, the `weightSizeCodec` sentinel).** A weight type of
  any other shape — a struct, a named integer type such as `time.Duration`, a
  string — has no fixed width, so `snapshot.WriteCSRWithWeightCodec` frames it
  through the store's `txn.WeightCodec` as an Arrow-style offsets + payload
  pair:

  ```
  offsets   (nEdges+1) × uint64 LE
  payload   offsets[nEdges] bytes
  ```

  `offsets[k]..offsets[k+1]` is edge *k*'s encoded weight, so weights stay
  randomly addressable by slot — which matters because `ApplyCSRToGraph` skips
  slots whose endpoints the mapper cannot resolve and therefore does not visit
  every *k* in order. There is deliberately no separate payload-length field:
  `offsets[nEdges]` is that length.

`snapshot.WriteCSR` — the entry point with no weight codec — **refuses**
`ErrWeightNotPersistable` for such a type rather than writing a weightless
snapshot, because a weightless snapshot would let the caller's checkpoint
truncate the WAL prefix holding the only surviving copy of the weights
(rmp #2526). No manifest version was bumped: the sentinel appears only in the
files that need it, so a `float64` or `int64` snapshot stays byte-identical and
keeps opening on older builds. An older reader meeting `0xFF` fails loudly —
`0xFF` was never a legitimate width, so its width guard rejects the file rather
than mis-reading it.

The trailing `hasHandles` + `handles` block is emitted **only** when the
source CSR carries a per-slot stable edge-handle column
(`csr.CSR.HandlesSlice() != nil`). A graph that never used `AddEdgeH`
produces no trailing block at all, so its `csr.bin` is byte-identical to
one written before the handle column existed.

Within a source, the order of the `edges` entries is **derived at build
time** from the adjacency by `csr.BuildFromAdjList`; it is never read
from, nor trusted from, disk. `store/snapshot.ApplyCSRToGraph` replays
the file in its stored order and the next build re-derives the order from
the rebuilt adjacency, so a snapshot stays readable across any change to
the build's ordering rule without a format version bump.

### `snapshot/labels.bin` (binary, v2+ only)

Little-endian throughout. The whole file is covered by the CRC32C
stored in the manifest entry, including the magic header.

| Offset  | Field             | Type                                  |
|---------|-------------------|---------------------------------------|
| 0       | magic             | uint32 LE = `0x4C424C53` (`'SLBL'`)   |
| 4       | formatVersion     | uint32 LE (currently 2)               |
| 8       | stringTableLen    | uint64 LE                             |
| ...     | strings           | stringTableLen × (uint32 utf8Len, [utf8Len]byte) |
| ...     | nodeEntries       | uint64 LE                             |
| ...     | node records      | nodeEntries × (uint64 NodeID, uint32 labelStringIdx) |
| ...     | edgeEntries       | uint64 LE                             |
| ...     | edge records      | edgeEntries × (uint64 src, uint64 dst, uint32 slot, uint32 labelStringIdx) |

The string table is the deduplicated set of label names, written in
the order the writer interns them from `lpg.LabelRegistry`. Each
record's `labelStringIdx` indexes into that table. A reader rebuilds
the registry by re-interning every string in table order; because
`lpg.LabelID` is assigned in interning order, the resulting LabelIDs
match the IDs that were live when the snapshot was taken, with no
extra remap step.

`labels.bin` is independent of the manifest version: a change to the
labels layout bumps the `formatVersion` field without forcing a
`manifest.json` schema bump.

#### Edge records are per SLOT (format version 2)

A relationship type belongs to the relationship *instance*, not to the
node pair, so two parallel edges between the same endpoints may carry
different types — or one may carry a type and the other none. The
`slot` field is what lets the file say which is which:

- a value below `0xFFFFFFFF` is a **canonical slot ordinal**: the
  slot's position among the pair's slots after a *stable* sort by
  stable-edge handle ascending. The record carries that slot's inline
  entry in the adjacency label column.
- `0xFFFFFFFF` marks a record belonging to the pair's **overflow
  list** — a type that could not be placed in any slot's column, and
  that every column-typed slot of the pair therefore carries.

Those two halves are the whole of a pair's durable type state, so
recording them verbatim is lossless in both directions.

The ordinal is defined against the canonical order because that is the
order *both* recovery paths converge on. The self-sufficient path
replays `csr.bin`, whose runs are already stably ordered by
`(destination, handle)`; the WAL-authoritative path replays
`OpAddEdge`/`OpAddEdgeH` in commit order. A stable sort is idempotent
and preserves the relative order of the handle-0 residual, so
canonicalising either sequence yields the same one, and the file never
has to carry the handle.

A slot's inline entry is recorded whether or not the by-handle type
store (`edgehandles.bin`) also covers it. That store stays
authoritative for *what such a slot is*, but the label column is what
`lpg.Graph.EdgeLabels` and `lpg.Graph.RelationshipTypesInUse` read, so
omitting it would leave every Cypher-created relationship's type absent
from those answers after a restart.

#### Reading a version-1 file

Format version 1 had no `slot` field and keyed an edge record by
`(src, dst)` alone, so a multigraph pair's parallel slots folded into
one record: a checkpoint could lose a committed relationship type or
invent one that was never attached.

A version-1 file **is still read**, with the per-pair semantics it was
written with: each record is replayed through `lpg.Graph.SetEdgeLabel`,
which types every free column-typed slot of the pair. Rejecting it
would turn the fix into a regression — a store that opened before the
upgrade would fail to open after it, and the missing per-slot
information is not recoverable from the file either way. The loss such
a file already carries is therefore *frozen*, not repaired; the first
checkpoint after the upgrade writes version 2 and the pair is durable
from then on. A version the reader does not know is rejected with
`ErrLabelsCorrupted`.

#### Recovery semantics for labels

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
on-disk migration step — the next full write (`snapshot.WriteSnapshotFull` or
the checkpointer's `snapshot.WriteCapture`) simply emits a fresh v2 or v3
directory at the same path under the publish swap described above.

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
| 7        | `PropList`         | variable           | uint32 LE element count, then per element: uint8 kind, uint32 LE payload length, payload |

A `PropList` element's payload is encoded by the same table, one level down. A
**nested** list is refused with an error rather than encoded: openCypher
restricts a property value to a primitive or a flat list of primitives and
classifies a nested list as `InvalidPropertyType`, so the durable format
deliberately does not carry one (see `txn.ErrNestedPropertyList`).

A single encoded value is capped at 1 GiB (`maxValueLen`) by the writer as well
as the reader.

`PropTime` is reconstituted via `time.Unix(sec, nsec).UTC()` —
snapshots travel between machines so the caller's location is
deliberately dropped on read. `PropFloat64` round-trips bits
losslessly, including ±0.0, ±Inf, and every NaN payload (note that
`NaN != NaN` by IEEE rules; compare via `math.Float64bits`).

A record whose `kind` tag is outside the documented enum surfaces
as `snapshot.ErrPropertiesCorrupted`; the reader does not silently
drop unknown kinds.

#### Recovery semantics for properties

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

### `snapshot/tombstones.bin` (binary, optional)

Little-endian throughout. The whole file is covered by the CRC32C stored in the
manifest entry, including the magic header; there is no in-file trailer.

| Offset | Field         | Type                                |
|--------|---------------|-------------------------------------|
| 0      | magic         | uint32 LE = `0x424D5453` (`'STMB'`) |
| 4      | formatVersion | uint32 LE (currently 1)             |
| 8      | count         | uint64 LE                           |
| 16     | ids           | uint64[count], ascending            |

The ids are the tombstoned `NodeID`s in ascending order, so the file is a pure
function of the tombstone set and two snapshots of the same state are
byte-identical.

### `snapshot/edgehandles.bin` (binary, optional)

The per-handle edge metadata: each parallel edge's own relationship type and
properties, keyed by its stable handle. `labels.bin` and `properties.bin`
deliberately collapse parallel edges onto a single `(src, dst)` per-pair record,
and `csr.bin`'s trailing handle column restores the identities but not the
metadata — so without this component a self-sufficient snapshot would recover
the right parallel edges with the right handles, and every one of them would
read back the per-pair *union* of types instead of its own.

Little-endian throughout; the CRC32C is in the manifest entry.

| Offset | Field         | Type                                                   |
|--------|---------------|--------------------------------------------------------|
| 0      | magic         | uint32 LE = `0x44484553` (`'SEHD'`)                    |
| 4      | formatVersion | uint32 LE (currently 1)                                |
| 8      | labelTableLen | uint64 LE                                              |
| ...    | labels        | labelTableLen × (uint32 utf8Len, [utf8Len]byte)        |
| ...    | keyTableLen   | uint64 LE                                              |
| ...    | keys          | keyTableLen × (uint32 utf8Len, [utf8Len]byte)          |
| ...    | recordCount   | uint64 LE                                              |
| ...    | records       | see below                                              |

Each record is:

```
uint64 src
uint64 dst
uint64 handle
uint32 labelCount
[labelCount × uint32 labelIdx]     — index into the label table
uint32 propCount
[propCount × (uint32 keyIdx, uint8 kind, uint32 valueLen, [valueLen]byte)]
```

The per-kind value bytes are identical to `properties.bin`, so the two
components share one value codec.

### `snapshot/constraints.bin` and `snapshot/indexdefs.bin` (binary, optional)

The durable **schema**: the declared constraint set and the declared
secondary-index definition set. They are what let a checkpoint truncate the WAL
prefix that first declared a constraint or an index without losing it. Both
share one layout, differing only in the magic and in the field order inside a
record.

| Offset | Field         | Type                                                          |
|--------|---------------|----------------------------------------------------------------|
| 0      | magic         | uint32 LE — `0x534E4353` (`'SCNS'`) / `0x58444953` (`'SIDX'`)   |
| 4      | formatVersion | uint32 LE (currently 1 for both)                                |
| 8      | count         | uint32 LE (rejected above 1 Mi on read)                         |
| 12     | records       | count × (uint8 kind, then three uint32-length-prefixed strings) |

The three strings are `(label, property, name)` for `constraints.bin` and
`(name, label, property)` for `indexdefs.bin`. Records are written in a
deterministic sort order — `(kind, label, property, name)` and
`(kind, name, label, property)` respectively — so two snapshots of the same
schema are byte-identical. Every string is capped at 64 KiB by the writer as
well as the reader, so the writer can never emit a record the reader would
refuse.

`indexdefs.bin` is **distinct from `indexes/`**: this file holds the
*definitions*, which are load-bearing (recovery rebuilds each index by
backfilling it from the recovered graph), while `indexes/<name>.bin` holds the
optional serialised *payloads*, which are only a recovery speed-up.

Recovery surfaces both sets through `recovery.Result.Constraints` and
`recovery.Result.Indexes`, seeded from these components and then layered with
the `OpCreateConstraint` / `OpDropConstraint` / `OpCreateIndex` / `OpDropIndex`
frames replayed from the WAL suffix, so the result is the schema as of the last
durable commit.

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

Four deterministic crash-injection scenarios cover the truncation window and
prove full recovery on `SIGKILL`; they are listed under
[Checkpoint policy](#checkpoint-policy) below. All are no-ops in production
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

#### Recovery semantics for index payloads

**Recovery reports the payloads; the engine loads them.** `store/recovery`
never registers or populates an index itself, for two reasons. It cannot build
the right one — an `index.Manager` has no "create an index from this definition"
entry point, and the binding that makes an index self-maintaining (interned
property/label ids, the value projection, the liveness and label gates) belongs
to the Cypher engine. And it must not register one anyway: **WAL replay does not
feed the `index.Manager` change fan-out** (the only production
`Manager.ApplyBatch` call site is the engine's commit-time write-back, and
`store/txn` does not import `graph/index` at all), so an index registered before
replay would be frozen at the snapshot instant while the planner kept seeking
it — silent wrong answers rather than a lost optimisation.

`snapshot.LoadSnapshotFull` returns one `IndexReadback` per entry in
`manifest.indexes`. When the on-disk file is missing or the manifest's CRC32C
does not match the file bytes, `IndexReadback.Bytes` is `nil` and the counter
`store.snapshot.indexes.corrupted` is incremented.

`recovery.Open` classifies those readbacks into `Result.SnapshotIndexPayloads`,
one `IndexPayload{Name, Bytes, Err}` each, and exposes the supported lookup
`Result.IndexPayloadFor(name) ([]byte, error)`. A non-nil `Err` (always paired
with nil `Bytes`) is one of:

| Reason | Meaning |
|---|---|
| `recovery.ErrIndexPayloadUnreadable` | the manifest declared the payload and it did not survive: missing file, or CRC32C mismatch |
| `recovery.ErrIndexPayloadStale`      | readable and CRC-valid, but describes a state the recovered graph has left |
| `recovery.ErrIndexPayloadNotFound`   | the snapshot declared no payload under that name (a new index, or no snapshot) |

Neither sentinel is ever the **function** error of `Open` / `OpenCtx`: they are
per-payload reason codes, exactly like `Result.TailErr`.

**Four preconditions must hold before a payload may be used.** The first three
are whole-image and are folded into `Err` by recovery; the fourth is per index
and is evaluated by the caller, because only the engine knows which
`(label, property)` an index name covers:

1. **The snapshot was self-sufficient** (`Result.SnapshotSelfSufficient`) — it
   carried a `mapper.bin`, so node ids were restored rather than re-derived by
   replay interning. On the mapper-less path the raw `uint64` NodeIDs inside a
   payload name nothing: `graph.Mapper` has no un-intern, and one discarded
   transaction shifts every later id.
2. **The manifest carried `indexes_commit_ts`** (see below). Without the instant
   the payloads describe, there is no way to tell whether the WAL replayed on top
   of them invalidates them.
3. **The manifest named this build's `index_builder_epoch`** (see above).
   Without it the payloads were produced by a builder whose entries this build
   cannot vouch for — which includes every build that predates the field, and so
   every payload the rmp #2778 / rmp #2792 backfill defects could have
   fabricated.
4. **The replayed WAL suffix did not touch that index's `(label, property)`**.
   Recovery reports the facts — `Result.WALTouchedNodeLabels` and
   `Result.WALTouchedNodePropertyKeys`, both sorted and de-duplicated — and the
   predicate `Result.WALSuffixTouchesNodeIndex(label, property)` over them. The
   union is a deliberate over-approximation: it can refuse a hydration that would
   have been sound, never permit one that is not.

`cypher.NewEngineWithStoreAndRecovery(store, res)` is the recommended
constructor: it threads the whole `Result` so the payloads travel with it and
cannot be dropped by accident. `NewEngineWithStoreAndSchema` remains available
and unchanged — it carries no payloads and therefore always rebuilds.

**A damaged payload is a per-index rebuild, never a fail-stop.** An index is
derived data — a pure function of an already-recovered, independently
integrity-checked graph — so a rebuild restores byte-identical content and loses
nothing. This matches the reference engines: PostgreSQL discards and rebuilds
`pg_internal.init`, Memgraph rebuilds its label indexes unconditionally on
recovery, and Neo4j coerces an unreadable index header to `POPULATING` and
repopulates. The fallback is not silent — a typed `Err`, a structured warning,
and the counters below record it. A `Deserialize` failure on a CRC-valid payload
(for example an inner format-version bump) takes the same per-index rebuild path.

The one index-related condition that **is** fail-stop stays fail-stop: a manifest
index name that would escape the `indexes/` directory raises
`snapshot.ErrManifestCorrupted` and fails the open. That is a path-traversal
attempt from attacker-controlled manifest bytes, not benign corruption.

An index is never seekable while unpopulated: the engine populates each index —
hydrate or backfill — and only then calls `index.Manager.CreateIndex`, so an
index the planner can find is always already complete. Hydration is confined to
`NewEngineWithOptions`, before the engine is published, because
`hash.Index.Deserialize` swaps its shards sequentially and is not atomic across
them; a call afterwards panics.

`recovery.Result.SnapshotIndexes` is the number of payloads recovery certified
hydratable (those whose `Err` is nil). It is **not** a count of indexes loaded —
recovery loads none. Which path each index actually took is reported by:

| Metric | Meaning |
|---|---|
| `store.recovery.indexes.hydrated`           | indexes populated from a snapshot payload |
| `store.recovery.indexes.rebuilt`            | indexes populated by scanning the recovered graph |
| `store.recovery.indexes.backfill_nodes`     | node references those rebuilds materialised (the work hydration avoids) |
| `store.recovery.indexes.payload_unreadable` | payloads recovery reported unreadable |
| `store.recovery.indexes.payload_corrupted`  | CRC-valid payloads whose `Deserialize` failed |

#### Migrating a snapshot directory without `indexes/`

A v2 snapshot produced before this extension simply has no
`indexes` array in its manifest and no `indexes/` sub-directory.
Such snapshots continue to load cleanly; `LoadedSnapshot.Indexes`
is `nil` and the next snapshot pass produces an updated layout the
moment a serialisable index is registered with the graph.

## Checkpoint policy

`store/checkpoint.Checkpointer` runs a goroutine that takes a
snapshot every `MaxAge` interval (or on `Trigger()` / `RunCheckpoint()`).
A checkpoint is **non-blocking**: it holds the commit lock only for two
O(1) readings at the start and a brief truncate at the end, and does every
piece of work that scales with the graph — the serialisation and all the disk
I/O — outside it. Writers commit throughout.

**Phase 1 — under the commit lock (the quiesce boundary).**

1. Drain: the wired commit serialiser — `txn.Store.RunUnderCommitLock`, passed
   via `checkpoint.WithCommitSerialiser` — closes the admission gate and waits
   for every admitted writer to finish; then `awaitCommitQuiescence` waits out
   the durable-but-not-yet-published window, in which a transaction has passed
   its fsync but has not yet made its instant visible (rmp #2349). That wait is
   bounded at 30 s and **fails the checkpoint** rather than truncating on a
   stalled frontier, counting
   `store.checkpoint.quiesce.timeouts`. Without a serialiser the checkpointer
   falls back to the `storeMu` passed to `checkpoint.New`, which does **not**
   drain: that fallback is correct only for a caller that performs every write
   under that same mutex. Any engine-driven store must pass
   `WithCommitSerialiser`.
2. Gate on WAL health (`wal.Writer.Poisoned`): a schema DDL whose WAL commit
   failed at fsync poisons the writer while the engine's in-memory registry
   still reflects the attempted change, so folding the registry in that window
   would persist a schema change the client saw fail.
3. Take **two O(1) readings**: the WAL durable offset *W*
   (`wal.Writer.DurableOffset`, always on a frame boundary), which is a
   *durability* position, and an MVCC instant, which is a *visibility* position.
   The drain plus the quiescence wait is what makes those two — two different
   clocks — name the same set of transactions; without it a transaction could be
   durable below *W* yet invisible to the instant, so the image would omit it and
   phase 3 would truncate away its only record. The constraint and
   index-definition sets are read here too.

**Phase 1b — lock-free.** Serialise the **entire** graph image at that instant
— adjacency, mapper, labels, properties, tombstones, edge handles and index
payloads — into an in-memory `snapshot.Capture`, so every component reflects one
transaction boundary (rmp #2269). Capturing only the CSR here and handing the
*live* graph to the writer is what once made a checkpoint publish a partial
transaction. The instant is released as soon as the bytes exist, before any
disk I/O.

**Phase 2 — lock-free.** Publish the capture with `snapshot.WriteCapture`,
atomically via the three-step publish swap described above, while concurrent
transactions commit and append frames *past W*. Then:

4. **Read the published snapshot back** with the same reader recovery uses
   (`snapshot.LoadSnapshotFull`) and decode every codec-encoded mapper key with
   the store's own codec, exactly as recovery's next call does. A snapshot that
   was written correctly but cannot be *parsed and applied* would otherwise pass
   the composition gate below and have the WAL truncated behind it (rmp #2749,
   rmp #2780). A failure here aborts the checkpoint, retains the WAL, and
   increments `store.checkpoint.snapshot_unreadable`. This is a fail-stop, not
   the supported degraded mode in step 6.
5. `wal.Writer.Sync`, so the suffix *[W, end)* — the frames committed during
   phase 2 — is durable **before** any prefix byte is discarded. The ordering is
   snapshot durable, then suffix durable, then prefix truncated.

**Phase 3 — under the commit lock again, briefly.**

6. Re-verify self-sufficiency from the published manifest's file names:
   `mapper.bin` must be present, plus `constraints.bin` when the graph has
   constraints and `indexdefs.bin` when it has indexes. It is re-checked *here*,
   not reused from phase 1, because a DDL may have committed during the
   lock-free phase 2, in which case the image (captured before it) cannot stand
   alone. If it is **not** self-sufficient, truncation is **skipped** and the WAL
   is retained — truncating it would erase the only durable copy of what the
   snapshot lacks. That is a supported degraded mode (unbounded WAL growth,
   Durability intact), surfaced via
   `store.checkpoint.truncate_skipped_not_self_sufficient`.
7. Otherwise call `wal.Writer.TruncatePrefix(W)`, which discards **only**
   *[0, W)* and preserves every frame committed during phase 2. It is itself
   crash-safe: it writes the surviving suffix to a temporary file and renames it
   over the WAL. The reclaimed byte count is recorded on `Stats.WALTruncBytes`
   and emitted via `store.checkpoint.wal_truncated_bytes`.

Measured end to end on a 40 000-commit `int64`-keyed store with four writer
goroutines committing throughout: the checkpoint completed in 103.8 ms, 45
transactions committed while it ran, `WALTruncBytes` was exactly the 3 183 490
bytes of the phase-1 prefix, and the WAL was left at 3 645 bytes rather than 0 —
the concurrent suffix. Reopening the directory recovered `WALOps=45` and the
identical graph (40 091 live nodes, 40 045 edges), `IsClean()` true.

Crash safety at any interleaving follows from the ordering: the snapshot is
self-sufficient, and recovery replays the **whole** surviving WAL idempotently
on top of it, so the folded prefix is re-applied harmlessly and the suffix lands
on top. Four crashpoints exercise the window, all no-ops in production
(`GOGRAPH_CRASH_AT` unset) and driven by
`store/recovery/checkpoint_crashinject_test.go`:
`checkpoint.p2-snapshot-published-pre-truncate` (snapshot durable, full WAL
intact, nothing truncated) in `store/checkpoint.writeAndTruncate`, and
`checkpoint.truncprefix.tmp-written-pre-rename`,
`checkpoint.truncprefix.post-rename-pre-dirfsync` and
`checkpoint.truncprefix.post-rename-pre-bookkeeping` inside
`wal.Writer.TruncatePrefix`.

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

That position-tracked design — capture the watermark under the lock, write the
snapshot lock-free, truncate up to the watermark under the lock — is the
three-phase sequence above (rmp #2310). The earlier shape, which held the store
mutex across the whole snapshot write, no longer exists.

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

Quiescing writers is a **separate** responsibility: a `txn.Store` transaction is
a registered writer from `Begin` until `Commit`/`Rollback`, and a
`cypher.Engine` write registers for the statement's duration. Registration does
not exclude other writers — since rmp #2306 concurrency control is MVCC alone —
but it is exactly what a quiesce drains. Without it, a `Close` racing an
in-flight commit pits `wal.Writer.Close` (flush + fsync + refuse appends)
against the commit's own append and sync, which can leave a transaction whose
frames `Close` made durable while its `Commit` returned an error —
in-memory/durable divergence at shutdown.

There are two ways to satisfy it, and one of them is built in:

- **`store.WithQuiesce(txn.Store.RunUnderCommitLock)`** — the WAL close (step 3)
  then runs while holding the store's commit lock, so the in-flight commit
  finishes first and any commit that starts afterwards gets a clean
  `wal.ErrWriterClosed`.
- **The embedder drains.** Without `WithQuiesce`, `DB.Close` does **not** drain
  in-flight transactions and the embedder must stop new writes and let the
  active one finish before calling it. A Bolt server does exactly this by
  draining its connections before tearing the DB down (see below).

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
    store.WithCheckpointer(cp),                     // omit for a WAL-only store
    store.WithFinalCheckpoint(),                    // omit to skip the step-1 compaction
    store.WithQuiesce(st.RunUnderCommitLock))       // omit only if the embedder drains
defer db.Close()                                    // step 1 (if enabled) → step 2 → step 3
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
`recovery.OpenCtx[N, W](ctx, dir, opts)` — plus `OpenFS` / `OpenCtxFS`, the same
two over an injected filesystem — return a `Result` containing the rebuilt
`*lpg.Graph[N, W]` plus:

- `SnapshotHit bool` — whether `snapshot/manifest.json` was found
  and validated.
- `SnapshotSchemaVersion int` — the on-disk manifest version of the
  snapshot that was loaded (1 for legacy CSR-only directories, 2 for
  the labels + properties + indexes shape, 3 when `mapper.bin` is also
  present). 0 when no snapshot was found.
- `SnapshotLabels int` — how many label records the snapshot's
  `labels.bin` contributed back into the graph (before WAL replay on the
  self-sufficient path, after it on the WAL-authoritative one — see
  "Component apply order" below). v1 snapshots and v2 snapshots without
  `labels.bin` leave this at 0.
- `SnapshotProperties int` — how many typed-property records the
  snapshot's `properties.bin` contributed back into the graph, at the same
  point in the order as the labels. v1 snapshots and v2 snapshots without
  `properties.bin` leave this at 0.
- `SnapshotIndexes int` — how many `indexes/<name>.bin` payloads recovery
  certified **hydratable**. Recovery loads no index itself; see
  [Recovery semantics for index payloads](#recovery-semantics-for-index-payloads)
  above.
- `SnapshotIndexPayloads []IndexPayload` — one entry per declared payload,
  carrying the verified bytes or the reason they must not be used. The supported
  lookup is `Result.IndexPayloadFor(name)`.
- `SnapshotSelfSufficient bool` — whether the loaded snapshot carried a
  `mapper.bin` and was applied before WAL replay. The first hydration
  precondition.
- `WALTouchedNodeLabels []string` / `WALTouchedNodePropertyKeys []string` — the
  sorted, de-duplicated node facets the replayed WAL wrote, behind the
  `Result.WALSuffixTouchesNodeIndex(label, property)` staleness predicate.
- `SnapshotTombstones int` — how many node ids the snapshot's
  `tombstones.bin` restored. Only the self-sufficient (v3) path applies
  snapshot tombstones; on the WAL-authoritative path the deletions are
  reconstructed by replaying `OpRemoveNode`.
- `Constraints []ConstraintRecord` / `Indexes []IndexRecord` — the schema
  declared by `constraints.bin` / `indexdefs.bin` and by the replayed
  `OpCreateConstraint` / `OpCreateIndex` frames, with later DROPs applied.
  Both slices are deterministically ordered so a reopen is reproducible.
  Recovery does not enforce or build them; the engine does, via
  `cypher.NewEngineWithStoreAndSchema` or
  `cypher.NewEngineWithStoreAndRecovery`.
- `WALOps int` — how many WAL ops were applied.
- `WALTailOffset int64` — the byte offset at which replay stopped.
- `MaxTxnSeq uint64` — the highest transaction sequence any durable `OpCommit`
  marker carried. This is the value a reopened store must resume **from**; see
  `Result.NewStore` below.
- `MaxCommitTS uint64` — the MVCC instant floor derived from the snapshot's
  `commit_ts` and the maximum `commitTS` in the replayed WAL, so a reopened
  graph never re-mints an instant an already-durable transaction used.
- `MaxTxnOps int` — the per-transaction op cap **this recovery replayed under**,
  reported so a producer can never be built looser than the replayer that will
  have to read it back. `NewStoreCapped` clamps a looser request down to it (rmp
  #2530); see *Handing recovery's result to the transaction store* below for why
  a mismatch turned a rejected write into an unopenable database. 0 on a `Result`
  built by hand rather than returned by `Open`, which carries no information and
  clamps nothing.
- `TailErr error` — why WAL replay stopped before the end of the
  file, or nil at a clean EOF. A benign torn tail
  (`wal.ErrTornFrame`) is the normal crash-after-fsync state and is
  tolerated; genuine corruption inside an already-durable frame
  (`wal.ErrCRCMismatch`, `wal.ErrBadMagic`,
  `wal.ErrUnsupportedVersion`, `wal.ErrFrameTooLarge`,
  `wal.ErrTornFrameMasksData`, `recovery.ErrUnsupportedRecordVersion`,
  `recovery.ErrTransactionTooLarge`, or
  `recovery.ErrCommittedTxnCorruptOp`) is surfaced.
  `wal.ErrTornFrameMasksData` is the case where a corrupt length field
  over-declared past EOF and swallowed the durable frames that followed it, so
  it must not be mistaken for a benign torn tail.

On genuine corruption, `recovery.Open` / `OpenCtx` are **fail-stop**:
the function returns that error (the committed prefix is still placed
in `Result.Graph` for diagnostics) instead of returning nil and
hiding the damage in `TailErr`. A benign torn tail returns a nil
error and the committed prefix.

`Result.IsClean()` is the **stronger** of the two signals, and the one a
recover-then-append caller must branch on. It is false for every case in which
the function returns an error, and for one case in which the function returns
**nil**: `recovery.ErrCommittedTxnCorruptOp`, an undecodable op inside an
already-committed transaction. Such a directory opens — the committed prefix is
intact, and the discarded suffix is irrecoverable, so refusing the open would
buy no repair — but it did not replay cleanly and must not be appended to:

```go
res, err := recovery.Open[int64, float64](dir, opts)
if err != nil {
    return err // corrupt WAL: do not append onto it
}
if !res.IsClean() {
    return res.TailErr // did not replay cleanly: do not append onto it either
}
w, err := wal.Open(walPath) // safe to append: clean or benign torn tail
if err != nil {
    return err
}
// Resume the transaction sequence. NEVER build the store by hand here.
st := res.NewStore(w, txn.Options[int64, float64]{
    Codec:       txn.NewInt64Codec(),
    WeightCodec: txn.NewFloat64WeightCodec(),
})
```

Appending to a corrupt WAL would permanently embed the corruption and
silently drop every committed op that followed the bad frame, so the
safe behaviour — refusing to append — is the default. Every shipped
example under `examples/` that recovers then reopens the WAL for
append checks `IsClean()` before doing so.

**Reopen through `Result.NewStore`, not through a hand-built `txn.Store`.**
`NewStore` is `txn.NewStoreWithOptions` with `Options.ResumeTxnSeq` already set
to `Result.MaxTxnSeq`, so the first transaction the reopened store commits is
numbered `MaxTxnSeq+1`. A store built directly restarts the sequence at 0 and
re-mints numbers that transactions still present in the same WAL already carry —
one WAL then holds two different transactions under one sequence, and recovery's
TxnSeq-suffix atomicity filter only disambiguates them by the accident of frame
contiguity, which stops holding the moment a reopen follows a torn tail
(rmp #2302, rmp #2522).

`NewStoreCapped` is the same constructor with an explicit per-transaction op cap,
and **the cap it installs is not necessarily the one you asked for**: since rmp
#2530 the requested producer bound is **clamped down** to `Result.MaxTxnOps` — the
bound this recovery actually replayed under — whenever the request is looser,
including `txn.MaxTxnOpsUnlimited` against a finite replay bound. The clamp only
ever lowers; a replay bound of `txn.MaxTxnOpsUnlimited` clamps nothing, and a
`Result` built by hand rather than returned by `Open` carries no information and
clamps nothing either.

The reason is that a producer cap above the replay cap does not reject the
oversized transaction: `txn.Tx.Commit` acknowledges it **durable**, and the next
reopen then refuses the whole directory with `ErrTransactionTooLarge` — stranding
every transaction committed before it behind a fail-stop and discarding every one
committed after. Raising the producer bound for a bulk load while leaving recovery
at its default therefore converted a rejected write into an unopenable database.
What the caller gets instead is an ordinary `txn.ErrTransactionTooLarge` at commit
time, before any WAL frame is written. **A clamp that fires is logged at warn level
and counted as `store.recovery.NewStoreCapped.producerCapClamped`**, because a bulk
load that silently keeps the default bound is a surprise worth seeing. Callers that
legitimately need the two sides configured independently build the store with
`txn.NewStoreWithOptionsCapped` directly and own the invariant themselves.

`Result.WALTailOffset` is the byte offset of the last durable frame boundary. A
caller that reopens the WAL for append by some other route must truncate the
file to it first, so new frames are not written after torn-tail junk that every
later reader would stop at; `wal.Open` performs that truncation itself for a
benign torn tail.

### `ErrCommittedTxnCorruptOp`: not clean, but still opens

An op that cannot be decoded and applied inside an **already-durable,
already-committed** v3 transaction stops replay at that frame. The transaction
carrying it is not applicable as a unit, and every transaction *after* it is
discarded too — even though each of those commits was acknowledged. Those ops
are irrecoverable.

The reported outcome is therefore deliberately asymmetric:

| Signal | Value |
| --- | --- |
| `recovery.Open` / `OpenCtx` return value | `nil` — the directory opens, `Result.Graph` holds the committed prefix |
| `Result.TailErr` | wraps `recovery.ErrCommittedTxnCorruptOp`, naming the transaction sequence, the op's index within it, and the WAL frame |
| `Result.IsClean()` | **`false`** |
| Metric | `store.recovery.openCodec.committedTxnCorruptOp` incremented |
| Log | a structured `slog` warning at `WARN` naming the frame and the transaction |

The open is allowed to succeed because failing it would convert an affected
directory from "opens, and reports that it lost a suffix" into "does not open,
ever, with no repair path" — there is nothing left to recover once the bytes are
undecodable, so a refusal costs the whole service and buys nothing. What the
fail-stop mandate actually forbids is the *silence*, and that is what is
removed: the state is not clean, it is counted, and it is logged. The reasoning
is recorded in the `tailErrIsCorruption` godoc in `store/recovery`, and is a
decision rather than an oversight (rmp #2794).

Component apply order during open is fixed, and it differs between the two
paths.

**Self-sufficient (v3, `mapper.bin` present).** Every snapshot component is
applied **before** WAL replay, so the WAL tail's mutations win chronologically:

```
mapper.bin → csr.bin → tombstones.bin → labels.bin → properties.bin
           → edgehandles.bin → WAL replay
```

The snapshot is the committed state at checkpoint time and the WAL holds only
the deltas that came after it, so applying the image first and the WAL on top is
what makes a delete-then-recreate in the WAL tail yield the re-created state.
Applying the snapshot **after** the WAL — which is what this path used to do —
re-added the stale snapshot labels and clobbered the re-created properties
(rmp #1266). The CSR apply is bracketed in one adjacency commit window
(`BeginExclusiveBuild` / `EndExclusiveBuild`); outside such a window every
`AddEdge` clones the touched shard's entire slot array, so a snapshot cost
`O(edges × shard size)` instead of `O(shards touched)`. Measured at 50k nodes /
500k edges: 147.45 ms → 73.57 ms and 737.6 MiB → 113.6 MiB allocated, with a
byte-identical recovered graph (rmp #2170).

**WAL-authoritative (v1/v2, no `mapper.bin`).** There is no durable mapper, so
the WAL replay is what interns the nodes the snapshot's records refer to and the
snapshot side must come after it:

```
WAL replay (rebuilds mapper + CSR adjacency) → labels.bin → properties.bin
           → edgehandles.bin
```

Snapshot tombstones are **not** applied on this path: the WAL was never
truncated, so the deletions are reconstructed by replaying `OpRemoveNode`, and
applying a possibly-stale snapshot set could wrongly re-tombstone a re-created
node.

On both paths the properties pass runs after the labels pass, so the mapper is
populated and the edge bag is in place — records that point at endpoints the
apply phase has not seen are skipped and metered rather than aborting
recovery. Recovery also raises the graph's MVCC clock to `MaxCommitTS + 1`
before returning.

The recovery contract is verified by the fuzz test in
`store/recovery/recovery_test.go::TestRecovery_FuzzedTruncation`,
which truncates the WAL at random offsets for 200 iterations and
asserts that the recovered graph is always a prefix of the
committed op sequence.

## Rolling-upgrade harness

Three frozen v1 fixtures live under the persistence packages so any
future build proves it still loads the on-disk shape this release
emits:

- `store/wal/testdata/v1/sample.wal` — five framed payloads; loaded
  by `store/wal/format_compat_test.go`.
- `store/snapshot/testdata/v1/sample/` — `manifest.json` + `csr.bin`
  for a deterministic 3-edge graph; loaded by
  `store/snapshot/format_compat_test.go`. Its `manifest.json` predates the
  CRC32C trailer, so it exercises the accepted-unverified branch too.
- `store/csrfile/testdata/v1/sample.csr` — a Tier 2 CSR file; loaded by
  `store/csrfile/format_compat_test.go`.

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

*Last reviewed: 2026-09-08 against commit `efd32fb991f415c3a2871dab1ea1bfb83434d189`. If you edit code referenced by this document and do not update this footer, the doc-staleness lint will flag the PR.*
