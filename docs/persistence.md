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
```

## WAL payload schema

Each WAL frame carries one op payload. The encoder supports two
on-disk layouts, distinguished by the first byte of the payload:

| Version | First byte                  | Producer                      |
|---------|-----------------------------|-------------------------------|
| v1      | `[OpKind]` (one of 0x01..0x03 today) | `txn.NewStore` (legacy)       |
| v2      | `0xFE` (`txn.OpRecordV2`)   | `txn.NewStoreWithCodec`       |

`OpKind` values currently occupy 0x01..0x03, leaving the low byte
region free for future kinds. The v2 magic byte 0xFE is chosen far
outside that range so a reader can disambiguate by peeking the first
byte alone — any payload starting with 0xFE is necessarily a v2 frame.

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

```
uint8  version  (always 0xFE — txn.OpRecordV2)
uint8  kind
codec  src      (self-delimiting; see codec table below)
codec  dst      (self-delimiting)
uint16 labelLen (LE)
[labelLen]byte label
```

The codec writes the framing for src/dst inline — no separate length
prefix at the payload level. The trailing label keeps the v1 uint16
prefix for symmetry with the legacy reader.

### Built-in codecs

| Type                              | Wire form                                       |
|-----------------------------------|-------------------------------------------------|
| `string` (`NewStringCodec`)       | uint32 LE length prefix + utf-8 bytes           |
| `int` (`NewIntCodec`)             | varint                                          |
| `int32` (`NewInt32Codec`)         | varint                                          |
| `int64` (`NewInt64Codec`)         | varint                                          |
| `uint64` (`NewUint64Codec`)       | uvarint                                         |
| `[16]byte` (`NewUUIDCodec`)       | fixed 16 bytes                                  |
| `encoding.BinaryMarshaler` (`NewBinaryMarshalerCodec[N, *N]`) | uint32 LE length prefix + opaque marshaler payload |

All built-in codecs are stateless and safe for concurrent use.

### Recovery surface

`recovery.OpenString(dir)` accepts both v1 and v2-`StringCodec` frames
on the same WAL. For arbitrary `N`, use
`recovery.OpenWithCodec[N, W](dir, codec)`; this variant supports v2
frames natively and supports v1 frames only when the codec is
`NewStringCodec` (because legacy v1 bytes are raw utf-8).

### Migrating a v1 corpus

Existing stores keep emitting v1 frames bit-for-bit while the
constructor is `NewStore`. To migrate:

1. Replay the v1 log via `recovery.OpenString` into an in-memory
   graph.
2. Construct a new store via `txn.NewStoreWithCodec` against a fresh
   WAL.
3. Re-commit the recovered ops through the new store; subsequent
   frames are v2-tagged.

This is the same migration recipe used by snapshot + WAL-truncation
checkpoints, so the workflow does not require any new tooling.

Future revisions will add `snapshot/labels.bin`,
`snapshot/properties.bin`, and `snapshot/indexes/*.bin` to extend
the snapshot to the full LPG state.

## Snapshot file format

`snapshot/manifest.json`:

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

`snapshot/csr.bin` (binary):

| Offset  | Field            | Type                          |
|---------|------------------|-------------------------------|
| 0       | nVertices        | uint64 LE                     |
| 8       | nEdges           | uint64 LE                     |
| 16      | hasWeights       | uint8 (0 or 1)                |
| 17      | weightSizeBytes  | uint8                         |
| 18      | vertices         | uint64[nVertices]             |
| ...     | edges            | uint64[nEdges]                |
| ...     | weights          | raw[weightSize·nEdges] (opt.) |

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
- `WALOps int` — how many WAL ops were applied.
- `TailErr error` — `wal.ErrTornFrame` is normal (clean tail
  truncation after a crash); `wal.ErrCRCMismatch` indicates real
  corruption and must be surfaced.

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
