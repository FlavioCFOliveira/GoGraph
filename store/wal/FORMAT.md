# GoGraph Write-Ahead Log Format

This document specifies the on-disk format of a GoGraph write-ahead log (WAL).
All integers are little-endian. The design and its rationale are in
`docs/design-wal-v2.md`.

## Store layout

A store's log lives next to its base path `walPath` (`dir/wal` in a store
directory):

| Path | Content |
|---|---|
| `walPath.control` | The control file (64 bytes). |
| `walPath.d/<016x segNo>.wal` | The segments, numbered consecutively from 1. |
| `walPath` | The legacy single-file log of a store written before segments, ending with its seal; or a one-frame seal stub. |
| `walPath.lock` | The writer's exclusive lock file. |

A directory with segments and no control file is refused (`ErrMissingControl`).
A directory with neither is a legacy store: `walPath` is read as one file of
frames, and the first writable open migrates it.

## Frames

Each frame is a header followed by `length` payload bytes. Two versions exist.

### Version 2 (written)

| Offset | Field | Size | Meaning |
|---|---|---|---|
| 0 | magic | 4 | ASCII `GGWA`. |
| 4 | version | 2 | `2`. |
| 6 | flags | 2 | Reserved, `0`. |
| 8 | length | 4 | Payload length; at most 1 GiB. |
| 12 | pos | 8 | Logical position: the number of frame bytes the log held before this frame. Never reset, independent of segments. |
| 20 | prevLen | 4 | `pos` minus the predecessor's `pos`; `0` only for the store's first frame. |
| 24 | storeID | 8 | The store id from the control file. |
| 32 | crc32c | 4 | CRC32C (Castagnoli) of the payload, continued over header bytes `[0,32)`. |

Frame size: `36 + length` bytes. A frame never spans two segments.

### Version 1 (read only)

The legacy frame: magic (4), version `1` (2), length (4), and a CRC32C (4) of
magic, version, length and payload. Frame size: `14 + length`. It carries no
position and no store identity.

### Decoding

Fewer bytes than a version-1 header, or a payload shorter than its declared
length, is a torn tail (`ErrTornFrame`), unless a CRC-valid frame begins inside
the bytes an over-declared length consumed (`ErrTornFrameMasksData`). A wrong
magic is `ErrBadMagic`, an unknown version `ErrUnsupportedVersion`, a length
above 1 GiB `ErrFrameTooLarge` (rejected before allocation), and a checksum
mismatch `ErrCRCMismatch`.

## Control file

Written by temp file, fsync, rename and parent-directory fsync, so a crash leaves
the old or the new file.

| Offset | Field | Size | Meaning |
|---|---|---|---|
| 0 | magic | 4 | ASCII `GGCT`. |
| 4 | version | 2 | `1`. |
| 6 | length | 2 | `64`. |
| 8 | storeID | 8 | Random, non-zero, created once. |
| 16 | flags | 4 | bit 0 `PrefixTruncated` (history requires a snapshot); bit 1 `LegacyV1Pending` (the legacy file still holds history). |
| 20 | reserved | 4 | `0`. |
| 24 | oldestRetainedPos (OR) | 8 | First position of the oldest retained segment, or the end of the log when no retained segment holds a frame. |
| 32 | prevFramePosAtOR | 8 | Position of the frame preceding OR; `2^64-1` for none. |
| 40 | checkpointRedoPos | 8 | Redo position of the last checkpoint that recorded itself. |
| 48 | createdUnixNano | 8 | Diagnostic. |
| 56 | crc32c | 4 | CRC32C of bytes `[0,56)`. |
| 60 | padding | 4 | `0`. |

A bad magic, version, length or checksum, or a zero store id, is
`ErrControlCorrupt`.

## Segments

Each segment starts with a 32-byte header:

| Offset | Field | Size |
|---|---|---|
| 0 | magic `GGWS` | 4 |
| 4 | version `2` | 2 |
| 6 | header length `32` | 2 |
| 8 | storeID | 8 |
| 16 | segNo | 8 |
| 24 | reserved | 4 |
| 28 | crc32c of `[0,28)` | 4 |

Frames follow back to back. The header holds no position: a segment's first
position is its first frame's `pos`.

- A segment is created whole (exclusive create, header, fsync, directory fsync)
  before it receives a frame. A trailing segment with a short or invalid header
  and no frame is an interrupted creation: readers ignore it and the writer
  deletes it.
- The writer rolls the active segment over at a run boundary once it reaches
  its target size (16 MiB by default): it makes the segment durable first, so
  every non-tail segment is fully durable.
- Segment numbers are consecutive (`ErrSegmentGap` otherwise); an empty segment
  may exist only at the tail.
- A checkpoint unlinks the segments whose frames all lie below OR. Segments
  below the one holding OR that survive an interrupted unlink are ignored by
  readers and deleted by the writer.

## Reading a segmented log

Reading starts at the first frame of the segment that holds OR. For every frame:

- its store id must equal the control file's (`ErrForeignStore`; a segment
  header with another store id is refused the same way);
- its `pos` must be the end of its predecessor, and a frame must start exactly at
  OR (`ErrFramePosition`);
- `pos - prevLen` must be the predecessor's position, and the first retained
  frame's must be `prevFramePosAtOR` (`ErrPrevLink`).

A torn frame is benign only at the end of the last segment holding frames;
elsewhere it is `ErrTornSegment`.

## Control records

A payload whose first byte is `0xFC` is a control record, not a transaction op.
This build writes and reads three kinds:

| Kind | Name | Body |
|---|---|---|
| 1 | ReserveIDs | `0xFC 0x01`, shard (1), limit (8): every node id below `limit` in `shard` may have been issued. |
| 2 | NextIDsExact | `0xFC 0x02`, 256 × uvarint: the exact per-shard high-water marks, written at a clean close (`txn.Store.Close`). |
| 3 | LegacySeal | `0xFC 0x03`, storeID (8), v2StartPos (8) |

The seal is written, as a version-2 frame with position 0, at the end of a
migrated legacy file, and is the single frame of the seal stub. Because its
version is 2, a build that predates segments refuses the directory instead of
ignoring the segments.

## Concurrency

The format imposes no concurrency model. `wal.Writer` is safe for concurrent use;
`wal.Reader` and `wal.Log` are read by one goroutine each.
