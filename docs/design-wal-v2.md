# Design: WAL v2 — one on-disk format revision for #3021, #3020, #2195 and #3014

Status: **approved; steps 1, 2 and 3 implemented** (step 4 not yet implemented).

This document records a design, not the current behaviour of the code. Each implementation
step (§9) updates it to match what was built.

Every `file:line` citation refers to commit `2f64a880`.

---

## Summary

- The WAL becomes a directory of numbered segment files.
- A durable control file holds a random 64-bit store id.
- Every frame carries its logical position, a prev-link and the store id.
- Truncation unlinks whole segments, so checkpoint phase 3 no longer takes the commit lock
  (#2195).
- #3014: each snapshot records the WAL position it covers (R); recovery refuses unless the
  first retained position F ≤ R.
- #3021 A: the commit marker carries an id annex naming every node id the transaction
  created. Replay places each key at that exact mapper position. Mapper, snapshot and capture
  accept never-born holes.
- #3021 B: per-shard id reservations are logged ahead of use in batches of K (PostgreSQL
  NEXTOID style, no extra fsync). Each checkpoint stores 256 per-shard high-water marks.

The WAL can append records outside the commit path: `Writer.AppendCtx`
(`store/wal/writer.go:417-445`) and `AppendRun` (`writer.go:511-558`) are generic.
`recovery.Decode` rejects unknown leading bytes with `ErrUnsupportedRecordVersion`
(`store/recovery/recovery.go:771-777`). Reservations therefore ride as a stand-alone v2
control record via `AppendRun`.

---

## 0. Current code facts

### Frame format

- Frame = magic, version, length, CRC32C: 14 bytes (`store/wal/format.go:31-33`).
- CRC covers header `[0,10)` plus payload (`format.go:171-177`).
- Newer versions are refused (`format.go:216-220`). `CurrentVersion=1` (`format.go:29`).
- The torn-tail scan knows only its own versions (`format.go:322-325`).
- Frames carry no position and no store identity. The marker exists because frames carry no
  position (`writer.go:1272-1278`).
- Offsets reset: `reopenAfterPrefixTruncate` sets `durableSize=suffixLen`
  (`writer.go:1564-1565`).

### Files and truncation

- One file, `dir/wal` (`store/open.go:18`); lock path `path+".lock"` (`writer.go:268`).
- Prefix truncation copies the surviving suffix to a temp file and renames it under the
  commit lock (`writer.go:1226-1361`; copy `writer.go:1293-1300`, `1520-1545`).
- Checkpoint phase 3 calls it inside `runUnderCommitLock`
  (`store/checkpoint/checkpoint.go:1178-1181`, `1234`). This is the 8.1 ms / 30.9 ms stall.
- Marker `wal.prefix-truncated`:
  - written before the first truncation (`writer.go:1279-1285`);
  - rewritten at self-sufficient checkpoints (`checkpoint.go:1147-1158`);
  - refused with no snapshot (`recovery.go:1437-1450`);
  - backfilled on clean recovery (`recovery.go:1832-1858`);
  - sentinel `ErrMissingSnapshot` (`recovery.go:745`).
- Fsync-latency seam: `dataSyncFile` waits on `SyncLatency`
  (`store/wal/sync_latency.go:102-107`); `OpenWithSyncLatency` wraps `dirFsync`
  (`sync_latency.go:86-98`); the marker directory fsync goes through it (`writer.go:1486`).

### Snapshot and checkpoint

- The manifest has no WAL position (`store/snapshot/manifest.go:379-516`).
- Recovery replays the whole surviving WAL idempotently on the snapshot
  (`checkpoint.go:778-782`, `1168-1176`).
- W = durable offset read under the commit lock (`checkpoint.go:859`). The capture read beside
  it records the mapper watermark (`checkpoint.go:943-955`).

### Transactions and node creation

- `Tx.AddNode` buffers `{OpAddNode, Src: key}` (`store/txn/txn.go:1338-1344`); body: key,
  zero dst, empty label (`txn.go:2429-2440`); replay calls `g.AddNode(src)`
  (`recovery.go:2328-2332`).
- Commit applies before it appends (`txn.go:1928-1940`).
- MVCC is always armed in production (`graph/lpg/mvcc_write.go:926-971`); the unarmed
  `durable()`-first branch is test-only (`graph/lpg/lpg.go:1437-1442`).
- Cypher applies eagerly, then calls `CommitWALOnly` (`txn.go:2105-2152`).
- Implicit node creation: `internEndpoint` on `AddEdge`, `SetNodeLabel`, `SetNodeProperty`
  (`lpg.go:2491-2501`). The Cypher adapter emits only `tx.AddEdge` for endpoints it creates
  (`cypher/api.go:22972-23025`).
- The transaction sequence is minted inside the append critical section
  (`txn.go:2343-2377`, mint `2362`).
- The commit-marker body can grow without a version bump (`txn.go:2526-2552`).

### Mapper and capture

- Mapper id = `(idx<<8)|shard`, `idx = len(reverse)` (`graph/mapper.go:340-354`, `551-553`).
- Interning is permanent. An aborted creation leaves the key interned, tombstoned, in
  `unborn` (`lpg.go:529-539`); a later append revives it (`lpg.go:2505-2511`).
- `LoadFrom` rejects gaps (`graph/mapper_restore.go:135-140`; sort `124-126`).
- `MapperWatermark` is 256 × `uint64` of `len(reverse)` (`graph/mapper_watermark.go:17-48`).
  `NodeInternedAsOf` answers from it for capture snapshots
  (`graph/lpg/mvcc_life.go:1058-1087`).
- Capture drops ids not interned as of the instant and fails `ErrCaptureNotQuiesced` on a hole
  (`store/snapshot/capture.go:381-411`, doc `40-70`). It writes interned-but-not-alive ids as
  tombstones with their key (`capture.go:403-406`, doc `62-68`).
- `mapper.bin` stores an explicit `NodeID` per pair (`store/snapshot/mapper.go:122-136`,
  `279-347`): holes need no layout change.

---

## 1. Frames and records

All integers are little-endian. **Position** is the logical byte position counting frame
bytes only. It never resets and is independent of the segment. A frame never spans segments.

### 1.1 Frame header v2 — 36 bytes, layout L36

| Offset | Field | Size | Meaning |
|---|---|---|---|
| 0 | magic `GGWA` | 4 | Unchanged, so the old scanner and the downgrade guard recognise it. |
| 4 | version `u16` = 2 | 2 | An old build returns `ErrUnsupportedVersion` (`format.go:216-220`), open-fatal (`recovery.go:501`). |
| 6 | flags `u16` = 0 | 2 | Reserved. |
| 8 | length `u32` | 4 | Capped at `maxFrameSize` (`format.go:45`). |
| 12 | pos `u64` | 8 | `xlp_pageaddr` role. |
| 20 | prevLen `u32` | 4 | `pos − prevPos`; 0 only for the store's first frame. `xl_prev` role. |
| 24 | storeID `u64` | 8 | `xlp_sysid` role. |
| 32 | crc32c `u32` | 4 | Over the payload first, then header `[0,32)`. |

- `prevLen` replaces an 8-byte `prevPos`: same information; the maximum, 36 B + 1 GiB, fits
  `uint32`; saves 4 B.
- Payload-first CRC lets the payload CRC be computed before `w.mu`. The PostgreSQL ordering
  (`XLogRecordAssemble`, then the header after `xl_prev` in `XLogInsertRecord`) is verified
  at PostgreSQL `10cc5aa9`: `xloginsert.c:979-1007` and `xlog.c:1001-1007`.

Bytes per frame:

| Layout | Bytes | Δ vs v1 | Note |
|---|---|---|---|
| v1 | 14 | — | |
| L40 | 40 | +26 | |
| L36 | 36 | +22 | Recommended. |
| L28 | 28 | +14 | Store id only in the segment header, folded into the CRC seed. Departs from the decision: no typed per-frame foreign-store error. |

Per transaction, Δ = 22 × (ops + 1) for L36. A 4-op Cypher create costs ≈ +110 B, against
≈ 70 B of v1 headers plus ≈ 200–300 B of payload: roughly +30–45 % WAL bytes. Step 2 measures
WAL bytes per transaction (example 37 ladder, TCK WAL output, 1M-node bulk create, `benchstat`)
and confirms the header.

### 1.2 Payload kinds

Transaction ops stay `OpRecordV3` `0xFD`, unchanged (`txn.go:413-429`, `2585-2643`).

**OpCommit v2 body:**

```
u8 0xFD | u8 OpCommit | u64 txnSeq | u64 commitTS | uvarint nCreated |
nCreated × { uvarint ref ; [codec key bytes if ref == 0] ; uvarint nodeID }
```

- `ref = 1 + 2·opIndex + endpoint` (0 src, 1 dst): points at an op of the same transaction
  holding the key.
- `ref = 0`: key inline (the codec is self-delimiting).
- `nodeID`: the full packed id, never delta-coded.
- Cost ≈ 5–7 B per create (ref 1–3 B; id 3 B for idx < 2^13 ≈ 2M nodes, 4 B for
  idx < 2^20 ≈ 268M) plus 1 B per commit. An 8-byte id per endpoint would cost 16 B per edge
  op.

**Control records**, leading byte `0xFC` (`OpRecordCtl`, unused today):

| ctlKind | Name | Body | Use |
|---|---|---|---|
| 1 | `ReserveIDs` | `u8 shard \| u64 limit` | Every idx < limit in this shard may have been issued. |
| 2 | `NextIDsExact` | 256 × `uvarint next` | Clean close; exact marks. |
| 3 | `LegacySeal` | `u64 storeID \| u64 v2StartPos` | Only in the v1 file (§6). |

A reservation frame is 36 + 11 = 47 B per K ids per shard: ≈ 0.7 B/node at K = 64,
≈ 0.05 B at K = 1024.

### 1.3 Annex replay

1. On the commit marker, call `Mapper.PlaceUnborn(key, id)` for every annex entry. This binds
   the key to that slot in the unborn state: interned, tombstoned, in `unborn`
   (`lpg.go:529-539`).
2. Apply the ops as today (`recovery.go:2101-2117`). Ops revive the node through
   `internEndpoint`'s unborn branch (`lpg.go:2508-2511`) and `addNodeInfo` → revive
   (`lpg.go:2473`).

- A node created then removed in one transaction ends dead both live and on replay.
- An annexed id whose birth a statement rollback withdrew stays dead.
- **Strict v2 replay rule:** auto-intern is off during v2 replay. An op naming a key neither
  placed nor interned fails `ErrUnboundNodeKey`.

---

## 2. Segments, control file and checkpoint reclamation

### 2.1 Layout

| Path | Content |
|---|---|
| `dir/wal.control` | 64 B; `pg_control` analogue. |
| `dir/wal.d/<016x segNo>.wal` | Segments. |
| `dir/wal` | Kept only as the sealed legacy file or a 1-frame stub (downgrade guard, §6). |
| `dir/wal.lock` | Unchanged path (`writer.go:268`). |

### 2.2 Control file

Written as temp, fsync, rename, directory fsync.

| Offset | Field | Meaning |
|---|---|---|
| 0 | magic `GGCT` (4), version `u16` = 1, length `u16` = 64 | |
| 8 | storeID `u64` | From `crypto/rand`, non-zero, created exactly once. |
| 16 | flags `u32` | bit0 `PrefixTruncated` (replaces the marker's meaning); bit1 `LegacyV1Pending`. |
| 24 | OR `oldestRetainedPos u64` | First frame of the oldest retained segment, or the WAL end if retained segments hold no frame. |
| 32 | `prevFramePosAtOR u64` | `^0` = none. |
| 40 | `checkpointRedoPos u64` | R of the last truncating checkpoint; diagnostic. |
| 48 | `createdUnixNano u64` | Diagnostic. |
| 56 | crc32c `u32` + 4 padding | |

The store id is created at the first writable open of a fresh directory or a v1 store, before
the first segment exists, through the `writePrefixMarkerFS` body (`writer.go:1459-1490`), so
`dirFsync` and `SyncLatency` apply. It is copied into every segment header, every frame and
every v4 manifest (`store_id`).

### 2.3 Segments

Segment header, 32 B:

```
magic GGWS (4) | version u16 = 2 | hdrLen u16 = 32 | storeID u64 | segNo u64 | reserved u32 | crc32c u32
```

- The header holds no first position; that is the first frame's `pos`. A background preparer
  therefore creates the next segment as a spare (header written, fsynced, directory fsynced)
  before it is needed. Rollover needs no create and no directory fsync under the lock.
- Empty segments exist only at the tail. A non-tail segment with no frames is `ErrSegmentGap`.
- Size: `SegmentSize`, default 16 MiB (PostgreSQL's default), configurable, minimum 1 MiB.
- Rollover happens only at a run boundary: a transaction never spans segments; one huge run may
  exceed the target.

**Rollover**, under `w.mu`, at the start of `AppendCtx`/`AppendRun` when the segment is at or
above target:

1. Wait for the in-flight group leader, as `Close` does (`writer.go:1597-1599`).
2. `bw.Flush` into the old file, then fdatasync via `dataSyncFile` (the latency seam applies).
   On failure, poison.
3. Set `durablePos = appendedPos` and broadcast `groupCond`.
4. Switch `w.f` to the spare, `bw.Reset`, close the old file, signal the preparer. With no
   spare, create one synchronously (slow path); on failure, poison (the old segment is already
   durable).

- Cost: one fdatasync per 16 MiB under `w.mu`. `SyncGroup` still fsyncs one file, the active
  one.
- **Invariant:** every non-tail segment is fully durable before any byte lands in its
  successor. A torn frame in a non-tail segment is therefore corruption (`ErrTornSegment`).
- **Rejected alternative:** leaving the old segment's fsync to the next leader. It is
  indistinguishable from external truncation of an acknowledged segment.

### 2.4 Checkpoint

- **Phase 1**, under the commit lock: unchanged, except that `W = DurableOffset()` is a logical
  position (`checkpoint.go:800-969`). Then `hwm = store.LoggedIDLimits()` is read after W (§5).
- **Phase 1b**, lock-free capture (§3). `nodeids.bin` is built from `max(watermark, hwm)`.
- **Phase 2**, lock-free:
  - publish the snapshot with manifest v4 `{store_id, wal_redo_pos: W, wal_format: 2}`;
  - `VerifySnapshotReadable` unchanged (`checkpoint.go:1123-1128`);
  - self-sufficiency check (`checkpoint.go:1147`);
  - if self-sufficient, write the control file with `PrefixTruncated`,
    OR = first position of the segment containing W (or W if every segment up to the active
    one ends at or before W), `prevFramePosAtOR`, `checkpointRedoPos = W`. This replaces
    `MarkPrefixTruncated` (`checkpoint.go:1153`);
  - the `wlog.Sync()` at `checkpoint.go:1164` is dropped.
- **Phase 3**, **no commit lock**:
  - unlink every segment whose frames all lie below OR (never the active one); directory fsync
    through the seam;
  - if `LegacyV1Pending`, replace `dir/wal` with the 1-frame seal stub (temp, fsync, rename,
    directory fsync) and clear the flag.

Why phase 3 needs no lock:

- The writer never touches non-active segments.
- The self-sufficiency re-check under the lock (`checkpoint.go:1202-1228`) only protects frames
  below W. Those are fixed at capture, since `HasConstraints()`/`HasIndexes()` are read under
  the phase-1 lock.
- A DDL committed in phase 2 sits at or above W and is retained.

This removes the second commit-lock acquisition. It changes the argument #1508 certified and
must be re-certified by the storage-engine auditor.

Test helper `Truncate()`: roll over, unlink every older segment, write the control file with
OR = current position.

---

## 3. Mapper and snapshot holes

### 3.1 Mapper (`graph/mapper.go`)

- `reverse []N` stays dense.
- Each shard gains a lazily allocated `holes []uint64` bitset (nil when there is no hole) and
  `next uint64` (≥ `len(reverse)`, the high-water mark).
- `internSlowHook` assigns `idx = next`, extends `reverse` with zero values and sets hole bits
  if `idx > len(reverse)`.
- `Resolve` returns false for a hole; `Walk` skips holes; `Len = len(reverse) − holeCount`.
- `MaxNodeID` stays `len(reverse)`-based (`mapper.go:441-459`).
- `Watermark` stays `len(reverse)` (`mapper_watermark.go:30-39`); `Covers` may be true for a
  hole, which is harmless.
- New `PlaceUnborn(k, id)`, recovery only:
  - same key, same id: no-op;
  - same key, different id: `ErrNodeIDMismatch`;
  - slot taken by another key: `ErrNodeIDMismatch`;
  - otherwise: place, fill holes below, raise `next`.
- The same bitset can later represent "id kept, key dropped" (design-space-reclamation phase 2,
  `docs/design-space-reclamation.md:73-75`).

`LoadFrom(entries, next *[256]uint64)`:

- drop the contiguity check (`mapper_restore.go:135-140`);
- keep the shard-hash (`99-113`) and duplicate (`141-144`) checks;
- add `idx ≥ next[s]` → `ErrMapperEntryCorrupted`;
- materialise `reverse` only to max idx + 1; `next` is stored separately.

### 3.2 Capture

- An id is included ⇔ `Covers(id)` and it was ever born as of `at` (birth record visible to
  `at`, or no record and not in `unborn`).
- Excluded ids become holes: absent from `mapper.bin`, not tombstones.
- Tombstones are only ids born then dead as of `at`.
- The `gapErr` path is removed (`capture.go:388-402`); `ErrCaptureNotQuiesced` is retired.
- New predicate `NodeBornAsOf(id, s)` in `lpg`, reading under the life-shard lock.
- Abort withdrawal must mark unborn before deleting the record. Today `finishLifeWithdrawal`
  settles outside the shard locks (`graph/lpg/mvcc_abort_sides.go:300-306`, to verify —
  risk 6).
- `NodeInternedAsOf` is unchanged (`mvcc_life.go:1058-1087`). Capture uses `NodeBornAsOf` for
  membership and `NodeExistsAsOf` for tombstones. The tombstone listing at
  `mvcc_life.go:1019` gets the same born-as-of filter.

### 3.3 Snapshot image

- `mapper.bin` unchanged: holes are absent pairs (`store/snapshot/mapper.go:122-136`).
- New component `nodeids.bin`:
  `magic GNID u32 | version u16 = 1 | shards u16 = 256 | 256 × u64 next` (2 KiB, CRC'd through
  the manifest `FileEntry`), where `next[s] = max(watermark.n[s], loggedReservationLimit[s])`.
- `ManifestVersion` 3 → 4 (`manifest.go:34`) with optional `store_id`, `wal_redo_pos`,
  `wal_format`, so a pre-change build refuses with `ErrManifestUnsupported`.

---

## 4. Recovery v2

1. Promote `snapshot.bak` as today (`recovery.go:1333-1373`).
2. Read `dir/wal.control`:
   - missing, with segments in `dir/wal.d` → `ErrMissingControl`;
   - missing, no segments → v1-only store: today's path, including the marker check
     (`recovery.go:1437-1450`);
   - bad CRC or magic → `ErrControlCorrupt`.
3. Load the manifest. `store_id` present and ≠ `control.storeID` → `ErrForeignSnapshot`.
4. Enumerate segments: sort by number, drop `*.tmp`.
   - header CRC/magic → `ErrSegmentHeader`; store id → `ErrForeignStore`; name ≠ `segNo` →
     `ErrSegmentHeader`;
   - numbers must be consecutive, else `ErrSegmentGap`;
   - a trailing segment with a short or invalid header and no frames is an unfinished spare:
     ignored, and deleted by the writer;
   - segments wholly below OR are interrupted-unlink leftovers: ignored.
5. Stream frames from the segment containing OR. Per frame, in order:
   - CRC → `ErrCRCMismatch`;
   - storeID → `ErrForeignStore`;
   - `pos == expectedPos` (starts at OR, equals the predecessor's end, across segment
     boundaries), else `ErrFramePosition`;
   - `pos − prevLen == lastFramePos` (the first retained frame is checked against
     `prevFramePosAtOR`), else `ErrPrevLink`.

   A torn frame is benign only at the end of the last segment holding frames (the
   `ErrTornFrameMasksData` discrimination still applies, `format.go:242-262`); elsewhere it is
   `ErrTornSegment`. E = end of the last valid frame.
6. Snapshot-reaches-WAL (#3014), with F = OR and R = `manifest.wal_redo_pos`:
   - v4 with R: R < F → `ErrSnapshotTooOld`; R > E → `ErrSnapshotAheadOfWAL`; R ≠ E and no
     frame starts at R → `ErrRedoPointNotFrameBoundary`;
   - snapshot without R beside v2 segments: F ≠ 0 → `ErrSnapshotTooOld`;
   - no snapshot: F ≠ 0, or `PrefixTruncated` set, or legacy marker present →
     `ErrMissingSnapshot` (keeps #2990/#3002);
   - R < `checkpointRedoPos` with F ≤ R is complete data: it opens, with a metric and a log
     line.
7. Apply the snapshot as today (`recovery.go:1514-1626`); the mapper via
   `LoadFrom(entries, next)`.
8. The legacy v1 file is replayed only when the snapshot has no R (it sits before position 0),
   with today's semantics (re-intern in order, no id migration). It must end with the
   `LegacySeal` frame; otherwise, with v2 frames present → `ErrLegacyNotSealed`.
9. Replay frames with `pos ≥ R` using today's state machine (`recovery.go:2043-2175`). Frames
   below R in the first segment are validated, not applied.
   - Commit marker: annex via `PlaceUnborn`, then ops; mismatch → `ErrNodeIDMismatch`; unknown
     key → `ErrUnboundNodeKey`.
   - A transaction whose first op lies below R and whose marker lies at or above R →
     `ErrRedoPointMidTransaction` (W is at a transaction boundary, `checkpoint.go:854-859`).
   - `0xFC ReserveIDs`: `limit[s] = max`. `NextIDsExact`: `exact[s]`.
   - With step 4: an annex id with idx ≥ the largest limit seen → `ErrIDBeyondReservation`.
10. `next[s] = max(nodeids.bin[s], max ReserveIDs.limit[s], NextIDsExact[s], max placed idx + 1,
    len(reverse))`. The live store starts with `loggedLimit[s] = next[s]`.
11. Clock, `MaxTxnSeq` and schema accumulators as today (`recovery.go:1676-1736`).
12. Return `WALEnd` (position), the tail segment number and offset, and `lastFramePos`,
    replacing `WALTailOffset`.

Every new sentinel joins `tailErrIsCorruption` and `tailErrIsOpenFatal`
(`recovery.go:484-531`). Id errors are open-fatal.

---

## 5. Id reservations

### 5.1 Mechanism

- The mapper gets an optional `IDReserver` and per-shard atomics `limit[s]`. A WAL-less graph
  installs none (unlimited).
- In `internSlowHook`, under the shard write lock:
  - `idx ≥ limit[s] − K/2` → non-blocking prefetch signal;
  - `idx ≥ limit[s]` → wait (rare).
- The reserver goroutine (owned by `txn.Store`, stopped on close, covered by `goleak`) calls
  `wlog.AppendRun(fn)`. `fn` emits one `ReserveIDs{s, newLimit}` frame and stores
  `loggedLimit[s] = newLimit` under `w.mu`; the reserver then publishes `limit[s]`.
- No fsync. PostgreSQL `XLogPutNextOid` does not flush — recalled from `varsup.c`/`xlog.c`,
  to verify at a pinned commit.

### 5.2 Ordering

- Lock order: mapper shard → `w.mu`. Nothing takes a mapper lock under `w.mu`:
  `appendChecked`'s `fn` only encodes (`txn.go:2343-2377`); annex keys are resolved before
  `AppendRun`; the checkpoint reads `Watermark()` and `DurableOffset()` one after the other,
  never nested.
- Reading hwm after W is safe: `loggedLimit` is written under `w.mu` in the same hold that
  appends the reservation, and `DurableOffset` takes `w.mu` (`writer.go:1115-1119`), so any
  reservation below W is visible to a later read.
- WAL ordering: a transaction using a reserved id interned it after the reservation was
  appended, so its run lands after the reservation.

### 5.3 Clean close and batch size

- Clean close: `Store.Close`, inside the quiesce `store.DB` already runs
  (`store/open.go:291-293`), appends `NextIDsExact`; the writer's `Close` then fsyncs.
- A clean restart wastes no ids; a crash restart wastes at most K per shard.
- K defaults to 64, doubling up to 65 536 for a shard that exhausts its batch before the
  prefetch lands.
- A reserved-but-unused range costs memory only when the shard next interns: up to 256 × K
  zero values plus hole bits per crash restart (K = 64 ≈ 16K slots ≈ 256 KiB for string keys).

### 5.4 Guarantee

- **Exact guarantee:** no id is ever reissued if a durable record or a checkpoint could
  reference it.
- An id seen only by a transaction that never became durable can be reissued after a crash or
  poison that also lost its reservation before any fsync covered it (PostgreSQL NEXTOID
  semantics).
- **Strict option:** the reserver `SyncGroup`s its reservation before publishing `limit[s]`
  (coalesced with group commit; latency on the prefetch only).

### 5.5 Annex production

- `lpg.WriteTx` gains `CreatedNodes()`, recording ids from `InternNewHook(created=true)` and
  unborn revivals (`lpg.go:2449-2451`, `2501-2511`, `2473`).
- `Tx.Commit` takes it from the final `ApplyDurable` attempt's `wtx`.
- `CommitWALOnly` gets it via the new `Tx.AttachWriteTx(wtx)`, which the Cypher adapter calls
  with `a.wtx`.
- A caller attaching nothing gets a correct, larger fallback: every key an intern-capable op
  names is resolved and annexed.
- Keys are resolved and refs computed before `AppendRun`.

---

## 6. Compatibility

### 6.1 Migration

The switch happens at the first writable open by a step-2 build after a clean recovery
(`store/open.go:285`). Read-only or unclean opens (`store/open.go:296-312`) never migrate.

Order:

1. Write the control file with a new store id and `LegacyV1Pending`.
2. The preparer creates segment 1 durably.
3. Append `LegacySeal` to `dir/wal` as a frame with version 2 in its header, and fsync.
4. Accept commits. v2 positions start at 0.

### 6.2 Old binaries

- Reading `dir/wal` → `ErrUnsupportedVersion` → refuse (`format.go:216-220`,
  `recovery.go:501`).
- A v4 manifest → `ErrManifestUnsupported`.
- They contend on `dir/wal.lock` (`writer.go:268`).
- The first v2 checkpoint replaces `dir/wal` with a seal-only stub, so the guard is permanent.

### 6.3 Existing ids

No migration of existing ids. The v1 tail replays as today (`recovery.go:2328-2332`),
deterministically, and is then frozen by the first v4 snapshot. v1 snapshots load through
`LoadFrom`.

### 6.4 Code removal and moving seams

- Writer-side v1 code is removed after step 2: `TruncatePrefix`, `MarkPrefixTruncated`,
  copy-rename. Recovery keeps reading v1 directories and the marker.
- Seams moving in step 2:
  - `walFS` gains `ReadDir` and `O_EXCL` create (`store/wal/writer_vfs.go:92-104`);
  - `recoveryFS.OpenWALReader` becomes a segment iterator (`store/recovery/fs.go:68`);
  - consumers: `internal/sim` (`disk`, `diskfs`, `simstore`, `group_commit`,
    `wal_writer_surface`) and examples 17 and 37;
  - `OpenWith` stays a single-segment test writer with no rollover.

---

## 7. Concurrency

| Concern | Where it synchronises | Hot-path cost |
|---|---|---|
| `pos`/`prevLen` assignment | Existing `w.mu` in `appendLocked` (`writer.go:566-580`) | Two integer stores; the payload CRC can move outside the lock. |
| Group commit | Unchanged `SyncGroup`/leader (`writer.go:717-913`); watermarks become positions | None. |
| Rollover | `w.mu`, run boundary, waits `!leaderActive`, one fdatasync per segment | Once per 16 MiB. |
| Spare segment | Background preparer | None. |
| Reservation | `AppendRun` under `w.mu` once per K ids per shard; atomics `limit[s]` | One atomic load per new node. |
| Checkpoint phase 3 | No commit lock (was a copy under the lock) | Stall removed. |
| Annex | Computed before `AppendRun`; `CreatedNodes` per transaction | O(created). |

No global lock is added. Contiguity and the mint inside the run are unchanged
(`txn.go:2343-2362`).

---

## 8. Crash points

"Existing" marks a breakpoint that exists today.

| Breakpoint | On-disk state | Recovery outcome |
|---|---|---|
| `wal.control.tmp-written-pre-rename` (create) | No control file, no segments | Fresh store or v1 path; retried with a new id (nothing references the old one). |
| `wal.control.renamed-pre-dirfsync` (create) | Control may vanish, no segments | As above. |
| `wal.segment.spare-created-pre-dirfsync` | Trailing spare may vanish or be short | Ignored or deleted; the writer recreates it. |
| `wal.migrate.control-written-pre-seal` | Control `LegacyV1Pending`, v1 unsealed, no v2 frames | Replay v1; migration resumes; an old binary still opens safely. |
| `wal.migrate.sealed-pre-first-v2-frame` | Seal durable, segment empty | v1 replay, E = 0. |
| `wal.rollover.old-flushed-pre-fsync` | Old segment may be torn, spare unused | Torn tail of the last segment with frames → benign. |
| `wal.rollover.switched-pre-first-frame` | Old durable, new empty | E = old end. |
| `wal.reserve.appended-pre-fsync` | Reservation may be lost | `next` from earlier records; ids never durable may be reissued (documented). |
| Existing `wal.appendrun.frame-emitted`, `wal.sync.pre-datasync` | Unchanged | Unmarked tail discarded. |
| Existing `checkpoint.p2-snapshot-published-pre-truncate` | New snapshot (R), old control | F ≤ R; replay from R. |
| `checkpoint.control-tmp-pre-rename`, `checkpoint.control-renamed-pre-dirfsync` | Old or new control, all segments | Both consistent. |
| `checkpoint.unlink-partial`, `checkpoint.unlink-done-pre-dirfsync` | Some segments below OR survive | Ignored as leftovers; the writer deletes them. |
| `checkpoint.legacy-stub-renamed-pre-dirfsync` | Full v1 file or stub | v4 snapshot ⇒ legacy content skipped either way. |
| Existing `recovery.snapshot-promote-post-rename-pre-fsync` | Promoted `.bak` (older R) | Control not advanced past it ⇒ F ≤ R. |
| `wal.close.exactnext-appended-pre-fsync` | Exact record may be lost | Falls back to reservations (≤ K per shard wasted). |
| `wal.rollover.fsync-fails` (fault injection) | Writer poisoned before the switch | Old segment truncated to durable; reopen re-validates. |

---

## 9. Implementation steps

Common to every step: all persistence suites create and persist their graphs (store directories,
WAL segments, snapshots, control files) on the RAM drive — and nothing else: binaries, build
temporaries (`go`'s own `GOTMPDIR`), logs, profiles and benchmark output stay on disk, while the test
process gets `TMPDIR` and `GOTMPDIR` on the RAM drive through `go test -exec` (CLAUDE.md, Tests and
validation: under Go 1.27 `t.TempDir()` follows `GOTMPDIR`); race-sensitive tests use
`internal/synclatency.ForTest` (from `8cb0aafb`); each step ends with its targeted validation;
`make ci` runs at sprint close.

Dependencies: step 3 depends only on step 1; step 4 needs steps 1 and 2.

### Step 1 — mapper holes, per-shard next, capture born-as-of, `nodeids.bin`, manifest v4

Scope: `graph/mapper.go`, `mapper_restore.go`; `graph/lpg` `NodeBornAsOf` and withdrawal
ordering; `store/snapshot` capture/apply/manifest/`nodeids.go`; `store/recovery` `LoadFrom`
with `next`.

Tests:

- **First**, the finding test: a transaction interns K at the instant, commits after W through
  `AddEdge` only; crash; recover; compare the liveness of K with the live graph. Expected to
  fail today (risk 7).
- `LoadFrom` with gaps, `next > len`, ids ≥ `next` rejected.
- `Resolve`/`Walk`/`Len`/`MaxNodeID`/`Watermark` with holes.
- Capture with an aborted id and with an open transaction at the instant: no
  `ErrCaptureNotQuiesced`, hole absent from `mapper.bin`, no tombstone.
- v1/v2/v3 fixtures load (`store/snapshot/testdata/v1`).
- Concurrent capture with interning and aborts at 1, 8, 64, 256, 1024 goroutines under
  `-race`.

#### Step 1 — as implemented

The finding test (`checkpoint.TestCheckpoint_KeyInternedAtInstantRevivedByAddEdgeOnly`)
failed 3 of 3 runs before any change: recovery held K dead with a live edge onto it
(`live alive=true edge=true, recovered alive=false edge=true`). Risk 7 was a real defect.
The test passes with step 1 and stays as its regression guard.

Built as designed:

- `graph/mapper.go`: per-shard `holes` bitset, `holeCount` and `next`; intern assigns
  `idx = next` and fills holes below it; `Resolve` false and `Walk` skips on holes;
  `Len` excludes holes; `MaxNodeID` stays `len(reverse)`-based.
- `graph/mapper_restore.go`: `LoadFrom(entries, next *[256]uint64)` accepts gaps, keeps
  the shard-hash and duplicate-key checks, adds a duplicate-intra check and
  `idx ≥ next[s]` → `ErrMapperEntryCorrupted`, and materialises `reverse` to max idx + 1.
- `graph/lpg`: `NodeBornAsOf` under the life-shard lock; the tombstone listing filters on
  it. Risk 6 was real: `finishLifeWithdrawal` marked unborn after the record was deleted
  and the lock released. `withdrawLocked` now marks unborn under the lock before deleting
  the record; `TestNodeBornAsOf_AbortedCreationNeverReadsBorn` reads 647–799 wrong answers
  per run (5 of 5 runs) with the old order and none with the new one.
- `store/snapshot`: capture membership is `Covers ∧ NodeBornAsOf`; `nodeids.bin`
  (`nodeids.go`); `ManifestVersion` 4 with optional `store_id`, `wal_redo_pos`,
  `wal_format`, written empty and zero; loading accepts versions 1–4.
- `store/recovery`: the mapper is restored with `MapperReadback.Next`, the marks read from
  `nodeids.bin`.

Deviations, with reasons:

- **`Watermark` records the high-water mark `max(next, len(reverse))`, not
  `len(reverse)`.** After a restore with `next > len(reverse)`, `len(reverse)` is below the
  restored marks, so a following checkpoint would have written lower marks to
  `nodeids.bin` and allowed restored-but-unborn ids to be reissued. `Covers` is true for a
  hole either way.
- **`NodeBornAsOf` refines "birth record visible to `at`".** A birth record `at` cannot see
  answers born exactly when `lifeStamp.unbornBefore` is false: a revival, after `at`, of a
  node removed earlier is dead at `at`, not a hole.
- **`ErrCaptureNotQuiesced` is removed** (§3.2 "retired"), with example 37's
  `capture_not_refused` gate and `checkpoint_refused_not_quiesced` telemetry, which could
  no longer fail; `D09 checkpoint_ran` still gates the single checkpoint attempt.
- **`LoadFrom(entries, nil)`** is the load of a snapshot without `nodeids.bin` (manifest
  versions 1–3): the marks are derived from the highest restored index per shard.
- **An image with no mapper pair** (an empty graph, or every assigned id a hole) still
  takes the WAL-replay path and does not restore its `nodeids.bin` marks, so recovery's
  self-sufficiency classification is unchanged (`cypher.TestIndexHydration_MapperlessSnapshotNeverHydrates`).
  Nothing on disk names a hole's id until step 3's annex, which must revisit this;
  `snapshot.ApplyMapperToGraph` already restores marks without pairs for any key type.
- **`nodeids.bin` and manifest version 4 come only with `mapper.bin`.** A capture that
  emits no mapper (a non-string key type without a mapper codec) still writes version 2.
- **The present-time capture (`at == nil`, offline writer under exclusion) is unchanged:**
  it carries every interned id.

### Step 2 — WAL v2 container (#3020, #2195, #3014)

Scope: control file and store id; segments, preparer, rollover; L36 frames; reader validation
and sentinels; recovery streaming and the §4 checks; manifest `wal_redo_pos`/`store_id`;
checkpoint phase 2 control write and lock-free phase 3 unlink; v1 seal migration;
`walFS`/`recoveryFS`/sim/examples adaptation; `0xFC` decode (`LegacySeal` only).

Tests:

- Tamper tests with recomputed CRCs: `ErrFramePosition`, `ErrPrevLink`, `ErrForeignStore` (a
  foreign segment and a mismatched control), `ErrSegmentGap`, `ErrTornSegment`.
- #3014: snapshot A saved, commits, checkpoint B truncates, A put back → `ErrSnapshotTooOld`;
  old segments behind a newer snapshot → `ErrSnapshotAheadOfWAL`; missing snapshot with
  segments starting above 0 → `ErrMissingSnapshot`.
- #2195: `TestCheckpoint_WriterStallBoundedByCapture` extended with a 56.5 MB suffix: no commit
  waits on phase 3; phase-3 duration `benchstat` before/after.
- Rollover under 256 concurrent committers with seeded latency: every acknowledged commit
  survives `kill -9` (`cross_proc_sigkill` / `soak_100cycles` style).
- Every §8 crash point via `internal/crashinject`.
- Compatibility: a frozen v1 fixture opens, migrates, keeps its ids; a v1 reader on the
  migrated directory refuses.
- Persisted-store round trip.
- WAL bytes per transaction measurement (header gate).

#### Step 2 — as implemented

Built as designed: the control file, store id, segments with a background preparer and
rollover at a run boundary, L36 frames, reader validation with the §2 sentinels, recovery
streaming with the §4 checks, manifest v4 `store_id`/`wal_redo_pos`/`wal_format`, checkpoint
phase 2 control write and phase 3 unlink outside the commit lock, the v1 seal migration, and
`0xFC` decode of `LegacySeal`. `TruncatePrefix`, `MarkPrefixTruncated` and
`ErrPrefixTruncateUnsupported` are removed; the legacy prefix marker is still read for
unmigrated stores.

Deviations, each with its reason:

- **A fresh store writes the seal stub at creation**, so a build that predates segments
  refuses the directory instead of reading an empty legacy log.
- **Migration runs in `wal.Open`**; a legacy file with a corrupt frame is refused rather than
  sealed, so no history is sealed away unread.
- **Schema DDL below the redo position is replayed** when the control file's
  `checkpointRedoPos` differs from the snapshot's, so constraints survive a checkpoint whose
  snapshot is not self-sufficient. `MaxTxnSeq` and `MaxCommitTS` include frames below the redo
  position.
- **Streaming starts at the first frame of the segment holding OR**, and OR is the start of
  the last segment at or below the redo position: whole-segment granularity.
- **`OpenFS` prepares spares synchronously**, so the simulator stays deterministic.
- **`Truncate` on a poisoned writer returns the sticky error** (previously it emptied the file regardless of the poison);
  a control-write failure does not poison the writer, because the old control file stays valid.
- **`OpenReader` iterates the whole store log** (legacy file minus its seal, then segments).
- **`Encode` with `Version` 0 writes a version-1 frame**, for the legacy fixtures and tests.
- **The frame header buffer is owned by the writer**: 0 allocations per frame.
- Test oracles that compared WAL byte counts now compare the control file's recorded
  checkpoint; example 37 D09 observes `pre_capture` and `post_checkpoint`, and D04 injects its
  fsync failures into the tail segment.
- The `ErrRedoPointNotFrameBoundary`, `ErrRedoPointMidTransaction` and `ErrLegacyNotSealed`
  paths have no dedicated test.

Measurements (header gate, §11 risk 1). Bytes per frame +22. Bytes per transaction, step 2
against its parent commit: Cypher 4-op create 517.3 → 737.3 (+42.5 %, 10 frames per
transaction); `SET` of one property 80 → 124 (+55 %); 1M-node bulk create 143.5 → 209.5 B per
node (+46 %). The TCK runs in memory and writes no WAL. The projected cost of L28 is +27–35 %
and of L40 +50–65 %. The layout stays L36 pending the user's decision.

Timings below are relative A/B runs on a RAM drive with fsync latency off, interleaved,
n = 6, compared with `benchstat`; they are not production latencies. WAL append (encode
only) −9.5 % to −28.8 %; 4096-byte frames with fsync +7.9 % (p = 0.004); allocations per frame
1 → 0. `txn` commit: 1 op +2.07 %, 16 ops +8.28 %, 8 and 64 concurrent committers +3.96 % and
+8.02 %, 1 and 256 no significant difference; allocations 13 → 11 per commit. Checkpoint
phase 3 over a 56.5 MB suffix: 14.30 ms under the commit lock → 0.39 ms without it; a commit
issued while phase 3 is parked completes (`TestCheckpoint_Phase3HoldsNoCommitLock_LargeSuffix`).

### Step 3 — id annex and exact replay (#3021 A)

Scope: `lpg.WriteTx.CreatedNodes`, `Tx.AttachWriteTx`, Cypher wiring, `OpCommit` annex,
`PlaceUnborn`, strict unbound-key replay.

Tests:

- Commit order: T1 interns K1 at idx i, T2 interns K2 at idx i+1 in one shard, with adversarial
  same-shard keys as in `graph/security_shard_amplification_test.go`; T2 commits first; crash;
  reopen → ids identical.
- Rollback: T1 interns K1 and rolls back, T2 creates K2 → id(K2) unchanged; K1's slot is a
  hole.
- Create-then-delete in one transaction.
- Cypher statement-level undo.
- `CREATE (n) RETURN id(n)` before and after restart, with and without a checkpoint.
- Annex-mismatch and unbound-key tamper tests.
- DST oracle "NodeID stability" in `internal/sim`.
- Allocations and `benchstat` on the commit path.

#### Step 3 — as implemented

Built as designed:

- `lpg.WriteTx.CreatedNodes(dst)` lists ids first interned by the transaction and unborn
  ids it revived (`noteNodeBorn`, and `noteNodeRevived` with `wasUnborn`); the list lives
  on the recycled `writeCtx` and is reset on reuse.
- `txn.Tx.Commit` attaches the final `ApplyDurable` attempt's transaction;
  `txn.Tx.AttachWriteTx` is called by the Cypher engine before both of its in-barrier
  `CommitWALOnly` calls (`cypher/api.go`, `cypher/exectx.go`). The post-bracket
  `CommitWALOnly(0)` path and DDL commits attach nothing and take the fallback annex.
- The annex (`store/txn/annex.go`) is resolved and encoded before `AppendRun` and appended
  to the `OpCommit` body after the commit timestamp: `uvarint n`, then per entry
  `uvarint ref`, the codec key when `ref = 0`, `uvarint id`. Every marker this build writes
  carries one, `n = 0` included; a body that ends after the timestamp is a pre-step-3
  marker and replays as before (`TestAnnex_LegacyMarkerReplaysAsBefore`).
- `graph.Mapper.PlaceUnborn` with `graph.ErrNodeIDMismatch`; replay places every annexed
  key, then applies the ops, under the strict rule (`recovery.ErrUnboundNodeKey`).
  `ErrUnboundNodeKey`, `ErrNodeIDMismatch` and the new `recovery.ErrCommitAnnexCorrupt`
  (an undecodable annex) are open-fatal.
- The DST oracle "NodeID stability" (`internal/sim/nodeid_stability.go`) runs at every
  crash recovery of the simulator and of the multi-session mode: every node alive on both
  sides keeps its id.

Deviations, with reasons:

- **A placed key is bound alive, not "interned, tombstoned, unborn".** The replayed op that
  created it then finds it bound and alive. An annexed id that no intern-capable op names
  — a creation the transaction withdrew before it committed — is settled afterwards
  (`lpg.Graph.SettleNeverBorn`) as tombstoned and NOT unborn, because the live graph holds
  such a node as a committed death inside the transaction, not as an aborted creation
  (`TestAnnex_UndoneCreationInCommittedTxn`). Placing tombstoned would also clone the
  tombstone bitmap twice per created node during replay.
- **A transaction whose replay stops at an op it cannot apply withdraws its placements**
  (`graph.Mapper.Unplace`), keeping only the keys an op that did apply names — what the
  pre-step-3 replay would have interned. Without it a discarded transaction's keys stayed
  bound in a recovered graph that still opens (`ErrCommittedTxnCorruptOp`;
  `TestRecovery_NilWeightCodecDiscardsWeightedTxn_2808`). An annex entry that refers to an
  op whose own body is undecodable makes replay skip the annex, so that op's apply
  classifies the corruption exactly as before.
- **The fallback annex reserves ids.** A `CommitWALOnly` caller that attaches nothing and
  never applied its ops to the graph (`TestApplyGate_NoLostWakeup_MixedCommitPaths` does)
  has keys the graph has not interned; `lpg.Graph.ReserveNodeID` interns each as a
  never-born node (tombstoned and unborn, marked inside the mapper's interning critical
  section) so the annex can name an id no other key will get. Writing no annex instead
  let replay intern such keys at ids later placements needed, and recovery refused with
  `ErrNodeIDMismatch`.
- **An abort in progress is a conflict.** Withdrawing an aborted first creation marks the
  id unborn and deletes its birth record under the life-shard lock, and flips the
  tombstone only afterwards; a write that reached the key in between built on a node about
  to vanish without creating it, so its marker did not annex it and the strict rule
  refused the recovery (found by `store/txn.TestDifferential_MemoryEqualsRecovery`, 1 run
  in 1 to 1 in 3). `lpg.Graph.existenceNoOpAdmits` now refuses that state as a retryable
  conflict (6 of 6 runs clean after).
- **Step 1 deviation 5 stays.** An image with no mapper pair still does not restore its
  `nodeids.bin` marks: every id a later transaction created is placed by its annex, and
  `PlaceUnborn` raises the shard's high-water mark past it, so the marks are not needed
  for the ids to be exact.

Measured:

- Commit path, RAM drive, latency off, interleaved HEAD/new binaries, 10 × 2000 ops:
  `Tx.Commit` of one created node 75.20 → 76.47 µs (p = 0.28), one property update
  75.54 → 75.63 µs (p = 0.91), one Cypher `CREATE` 80.80 → 79.64 µs (p = 0.09); allocations
  unchanged (8, 10, 43 per op); bytes +17, +16, +19 per op (the `Tx` carries the attached
  transaction).
- Red without the annex (an overlay that writes none): the commit-order, rollback and
  undone-creation store tests fail; the Cypher `id()` tests move 26–102 of 300 ids (restart test) and 89 of 200 (statement-undo
  test); the
  multi-session DST oracle reports moved ids on seed 1; GG07 `ids_reused_across_processes`
  fails in 6 of 20 runs. With the annex: all green, GG07 0 of 13 runs failing.

### Step 4 — reservations and high-water marks (#3021 B)

Scope: `IDReserver` hook, reserver goroutine, `ReserveIDs`/`NextIDsExact`, checkpoint hwm into
`nodeids.bin`, restore `max(...)`, `ErrIDBeyondReservation`.

Tests:

- After any mix of commits, aborts, checkpoints, clean closes and `kill -9`, new ids never
  equal an id previously returned by a transaction that reached durability or whose
  reservation was durable (strict mode: any id ever returned).
- Shard-exhaustion bursts under seeded latency with no deadlock (reserver holding `w.mu` while
  a run encodes).
- A clean restart wastes 0 ids; a crash restart ≤ K per shard.
- `goleak` for the reserver.
- Throughput at 1, 8, 64, 256, 1024 goroutines against step 3, with `benchstat`.

---

## 10. Prior art

"Recalled" means taken from memory of the source and **not verified**; it must be verified at
a pinned commit. "Repo-cited" means the reference is already cited in this repository.

| Decision | Source | Status |
|---|---|---|
| Position, prev-link and store id per frame; typed refusals | PostgreSQL `XLogRecord.xl_prev` (`xlogrecord.h`), `XLogPageHeaderData.xlp_pageaddr`, long-header `xlp_sysid` at segment start (`xlog_internal.h`); `xlogreader.c` "record with incorrect prev-link", "unexpected pageaddr", "WAL file is from different database system" | Verified at pinned commit (PostgreSQL `10cc5aa96a6df7cb1d7c47e978cc94f01f785cb7`). PostgreSQL keeps the sysid per segment; the per-frame store id is the user's decision. |
| Data CRC first, header CRC after the position | PostgreSQL `XLogRecordAssemble`/`XLogInsertRecord`; position reserved inside the insertion lock (`ReserveXLogInsertLocation`, cited at `txn.go:2349-2353`, commit `50d6e533`) | Verified at pinned commit (PostgreSQL `10cc5aa96a6df7cb1d7c47e978cc94f01f785cb7`, `xloginsert.c:979-1007`, `xlog.c:1001-1007`). |
| Control file before old WAL removal | PostgreSQL `UpdateControlFile` in `CreateCheckPoint` before `RemoveOldXlogFiles` (cited at `writer.go:1406-1409`, `checkpoint.go:1135-1137`) | Repo-cited. |
| Segments, 16 MiB, remove below redo | PostgreSQL `wal_segment_size`, `KeepLogSeg`/`RemoveOldXlogFiles`; RocksDB numbered `NNNNNN.log` deleted below `min_log_number_to_keep` (`PurgeObsoleteFiles`); its recyclable record type adds a log number, stored as a 4-byte value truncated from the 64-bit log number | Verified at pinned commit (PostgreSQL `10cc5aa96a6df7cb1d7c47e978cc94f01f785cb7`, RocksDB `0561153b73fb0a24387a8808215b076424546f7e`). GoGraph does not recycle segments, so `pos` alone defeats stale data. |
| Reservation without fsync; checkpoint stores the counter | PostgreSQL `GetNewObjectId`/`XLogPutNextOid`, `VAR_OID_PREFETCH = 8192`, `CheckPoint.nextOid` | Recalled, verify. |
| Explicit id in the WAL; next = max + 1 | Memgraph vertex-create deltas carry the Gid; recovery derives `next_vertex_id`; the snapshot stores next ids | Recalled, verify (BSL; ideas only). |
| Id high-water persisted; Neo4j reuses freed ids | Neo4j `IndexedIdGenerator` (highId + free-id tree), `neostore.transaction.db.N` with a store-id header, `db.tx_log.rotation.size` | Recalled, verify (GPLv3; ideas only). Reuse rejected per `docs/design-space-reclamation.md:85-93`. |
| Checkpoint waits on the observer, not on commits | PostgreSQL `DELAY_CHKPT_START` (`checkpoint.go:925-931`) | Repo-cited. |

The inspiration protocol requires reading these sources at pinned commits before step 2, and
recording them in the knowledge graph.

---

## 11. Risks and open questions

1. **Header layout** L36/L40/L28 (+22/+26/+14 B per frame), decided by the step-2
   measurement. A follow-up of one frame per transaction (RocksDB `WriteBatch`) is out of
   scope.
2. **Reservation durability:** PostgreSQL semantics (decided) or strict.
3. **K** default and adaptivity, and crash-restart hole memory.
4. **Lock-free phase 3** changes the #1508 C2 argument; storage-engine-auditor
   re-certification is required.
5. **One-way switch** — confirmed by the user.
6. **Born-as-of predicate atomicity:** abort withdrawal settles outside the shard locks
   (`mvcc_abort_sides.go:300-306`); it must mark unborn before removing the record.
7. **Suspected existing defect, not verified:** capture writes an id interned at the instant
   but not yet born as a tombstone with its key (`capture.go:62-68`, `403-406`). If its
   transaction commits after W through `AddEdge` alone, replay's `internEndpoint` does not
   revive a tombstoned non-unborn node (`lpg.go:2486-2489`, `2508-2518`), so the recovered node
   is dead while the live one is alive. Step 1's first test settles it; the holes rule removes
   it either way.
8. **Cloned directories share a store id.** Mixing their segments is caught only by pos/prev
   divergence (PostgreSQL has the same limitation with base backups).
9. **Annex size for huge transactions:** up to 16M creates in one commit frame ≈ 100 MB, under
   the 1 GiB cap. Option: chunk the annex into pre-marker frames if bulk loads need it.
10. **Embedders calling `CommitWALOnly` without `AttachWriteTx`** get the larger fallback
    annex — hard requirement or not.
11. **Blast radius of step 2:** sim harness, examples 17 and 37, every `OpenWith`/`OpenFS`
    test. Splitting the frame format from segmentation would build a position-mapping copy path
    only to delete it.

Critical files: `store/wal/writer.go`, `store/recovery/recovery.go`,
`store/checkpoint/checkpoint.go`, `graph/mapper_restore.go`, `store/txn/txn.go`.

---

## Decisions

### User decisions (2026-10-07)

| Decision | Task |
|---|---|
| PostgreSQL-style frame integrity: prev-link, position and store id in every frame. | #3020 |
| Ids stable across reopen via A (id annex, exact replay, holes) plus B (batched reservations, PostgreSQL NEXTOID semantics, no fsync). | #3021 |
| One-way migration at the first writable clean open; existing ids kept; older builds refuse. | Confirmed |
| WAL segmentation with unlink truncation. | #2195 |
| Snapshot-reaches-WAL check. | #3014 |

### Coordinator defaults, to be confirmed by measurement

| Default | Confirmation |
|---|---|
| Header layout L36. | The WAL-bytes-per-transaction measurement in step 2. |
| Reservation batch K = 64, adaptive. | Measurement. |
| Lock-free checkpoint phase 3. | Storage-engine-auditor re-certification. |

### Open items

- Annex chunking for very large transactions — only if bulk loads need it (risk 9).
- Fallback annex for `CommitWALOnly` callers that do not attach their write transaction
  (risk 10).
