# Stream 1 — Storage engine, on-disk format, durability, recovery, backup

Baseline `6f31f61` (v0.10.0). Hardware for every measurement: Apple Silicon (darwin/arm64),
Go 1.26.5, APFS on internal NVMe, `GOGC=100` unless stated. Store `store/` = 16,983 non-test LOC.

## Verdict summary

GoGraph's **durability correctness** remains best-in-class and is re-verified empirically at this
commit (crash-injection battery `internal/crashinject` + `internal/crashpoint` + `store/recovery`
under `-tags=gograph_crashinject`: **exit 0, all green, 39.9 s**). Durable-by-default still beats
Memgraph's shipping default, now confirmed from Memgraph's own source
(`wal_file_flush_every_n_tx{100'000}`). But round 1 measured *correctness* and stopped there.
Measured on *efficiency*, the storage engine has three structural gaps that rounds 1–2 never
reached: **(1) crash recovery leaves a 3× speedup on the table** because the snapshot-apply path
omits the adjacency commit window that WAL replay already uses — 4,307 MiB of transient allocation
to rebuild a 190 MiB graph; **(2) there is no space reclamation at all** — deleting 90 % of a graph
frees 30 % of the disk and a second checkpoint frees nothing more, so a churning store grows without
bound; **(3) checkpoint cost is O(database size), not O(delta)** — an *idle* checkpoint rewrites the
entire snapshot. The single most valuable lever is not from Neo4j or Memgraph at all: it is a
**four-line fix already present 290 lines away in the same file** (F1). The most valuable *external*
lever is Neo4j's **segmented transaction log** (`db.tx_log.rotation.size` = 256 MiB), which turns
GoGraph's O(WAL-suffix) copy-under-the-commit-lock into an O(1) unlink — this **overturns round 1's
rejection of a segmented WAL**, with measurement below.

One cross-stream result reframes the priority order. The planner stream measured Cypher `UNWIND`
bulk load at **35 m 33 s** for 20,000 nodes / 200,000 edges (Memgraph: 977 ms). I measured the same
shape through `store/bulk`: **29.6 ms** — a **≈72,000× gap between the ingest machinery GoGraph
already has and the ingest path its users can actually reach** (F10). The storage layer is not the
bulk-load bottleneck; it is three to four orders of magnitude faster than the only path exposed.
That fast path is unreachable on three independent counts — not callable from Cypher, its output is
a csrfile no `store.DB` can open, and its record type carries no labels or properties. Giving
`store/bulk` an offline store-import mode is therefore a **headline recommendation in its own
right**: it is wiring over component writers that all already exist, and it fixes the user-visible
problem without waiting on the planner.

## Feature-by-feature comparison

| Feature | GoGraph (file:line) | Neo4j 5.x/2025.x | Memgraph 2.x/3.x | Verdict | Label |
|---|---|---|---|---|---|
| Commit durability default | fdatasync/F_FULLFSYNC per group commit before ack — `store/wal/writer.go:491`,`579` | Durable; group commit via `BatchingTransactionAppender` | `write()` per tx, **fsync every 100,000 tx** | **BETTER** than Memgraph, PARITY Neo4j | CONFIRMED-R1 |
| macOS full-device flush | `os.File.Sync()` → `F_FULLFSYNC` (Go 1.26.5 `internal/poll/fd_fsync_darwin.go`) | n/a | n/a | **round-1 defect does not exist** | **STALE-R1** |
| Torn tail vs. masked data | `ErrTornFrameMasksData` + `embedsValidFrame` — `store/wal/format.go:229`,`279` | not documented | fail-stop on boot; RocksDB mode = `kPointInTimeRecovery` (truncate) | **BETTER** (only engine that separates the two) | CONFIRMED-R1 |
| Snapshot integrity | CRC32C per component in `manifest.json` — `store/snapshot/manifest.go:101` | checksummed store/log | *"Snapshots are not yet protected by checksums"* | **BETTER** than Memgraph | NEW |
| WAL record granularity | **one frame per OP** (14 B hdr + CRC each) — `store/wal/format.go:33` | one log entry per tx command batch | per-transaction delta + 1 CRC | **WORSE** than both | NEW |
| WAL segmentation / rotation | single file; prefix reclaim = **copy whole suffix** `store/wal/writer.go:984` | `db.tx_log.rotation.size`=256 MiB, retention `2 days 2G` | `--storage-wal-file-size-kib`=20480 | **WORSE** than both | NEW |
| Checkpoint trigger | time only (`Dir`/`MaxAge`/`Interval`) — `store/checkpoint/checkpoint.go:63-74` | PERIODIC(default)/VOLUME/CONTINUOUS/VOLUMETRIC; `interval.time`=15m, `.tx`=100000, `.volume`=250 MiB | `--storage-snapshot-interval`=300 s (+cron, EE) | **WORSE** than both | CONFIRMED-R1 (T1.4) |
| No-op checkpoint suppression | none — idle ckpt rewrites everything | *"checks … whether there are changes pending flushing and if so"* | n/a | **WORSE** than Neo4j | NEW |
| Checkpoint blocking behaviour | 3-phase; lock held only for CSR capture (3.8 ms/100k nodes) + phase-3 suffix copy | dirty-page flush, `iops.limit`=600 | non-blocking, batched, parallel opt-in | **PARITY** (see F4 caveat) | NEW |
| Checkpoint cost model | **O(database size)** full rewrite every time | **O(dirty pages)** since last checkpoint | O(database size) full snapshot | **WORSE** Neo4j, PARITY Memgraph | NEW |
| Snapshot/recovery parallelism | single-threaded | — | `--storage-parallel-snapshot-creation`, `--storage-parallel-schema-recovery`, `--storage-recovery-thread-count` | **WORSE** than Memgraph | NEW |
| Space reclamation | **none** — no compaction, IDs never reused | ID free-lists; offline `neo4j-admin database copy` | snapshot re-serialises live set only | **WORSE** than both | NEW |
| Backup / PITR | **no API, no documented procedure** | EE hot backup + differential + `--restore-until` (txId/timestamp) | `LOCK/UNLOCK DATA DIRECTORY` + copy; `RECOVER SNAPSHOT`; no PITR | **WORSE** than both | NEW |
| Bulk ingest **rate** | **8.07 M edges/s** measured (`store/bulk`, parallel) | — | ~1 M objects/s claimed | **BETTER** than both | NEW |
| Bulk import **reachability** | csrfile only; unreachable from Cypher and from `store.DB` — `store/bulk/bulk.go:1-10` | `neo4j-admin database import full` → native store files | `LOAD CSV` into the live store | **WORSE** than both (F10) | NEW |
| Replication / HA | none | Raft, quorum-on-commit (EE) | primary-replica; `STRICT_SYNC` 2PC; Raft coordinators (EE) | **N/A** for an embedded library | — |
| On-disk footprint (live data) | 193 B/node, 48 B/edge measured | node 15 B, rel 34 B, prop 41 B/record | not published | **PARITY** | NEW |

### Measured storage footprint (100,000 nodes ×[1 label + 2 props], 400,000 weighted edges)

| Component | Bytes | Per unit |
|---|---|---|
| `snapshot/csr.bin` | 10,435,611 | 24 B/edge (dst 8 + weight 8 + handle 8) + 8 B/vertex-slot |
| `snapshot/properties.bin` | 5,500,047 | 27.5 B/property record |
| `snapshot/mapper.bin` | 2,200,014 | 22 B/node (natural-key interning table) |
| `snapshot/labels.bin` | 1,200,042 | 12 B/label record |
| `snapshot/manifest.json` | 553 | — |
| **total** | **19,336,267** | **193 B/node, 48 B/edge** |
| WAL for the same build, un-checkpointed | 41,884,886 | 59.8 B/op, 700,500 frames for 700,000 ops |

Neo4j's classic record store for the same shape is ≈ 23.3 MB (100k×15 + 400k×34 + 200k×41), so
GoGraph is **competitive to slightly better on live bytes**. Note GoGraph pays 22 B/node for the
mapper that Neo4j does not need (the price of natural-key addressing) and 8 B/edge for the handle
column (the price of multigraph parallel-edge identity), and recovers it by storing adjacency as
CSR rather than a 34-byte doubly-linked relationship record.

---

## Findings

### F1. Snapshot recovery omits the adjacency commit window that WAL replay already uses — 3× recovery speedup available  [NEW]  (severity: HIGH)

- **What GoGraph does:** `store/recovery/recovery.go:1335-1336` brackets **WAL replay** in one
  adjacency commit window with the correct justification in-comment: *"WAL recovery is
  single-threaded with no concurrent reader and no concurrent PinSnapshot, so it is the sanctioned
  exclusive-build mode… O(shards touched) instead of O(ops)"* (task #1526).
  The **snapshot-apply block** at `store/recovery/recovery.go:1033-1101` — `ApplyMapperToGraph*`,
  `ApplyCSRToGraph` (`:1048`), `ApplyTombstonesToGraph`, `ApplyLabelsToGraph`,
  `ApplyPropertiesToGraph`, `ApplyEdgeHandlesToGraph` — runs **outside any window**. Every one of
  the CSR's edges therefore takes the cold branch of `graph/adjlist/adjlist.go:1893-1895`:
  ```go
  next := &shardSlots{slots: make([]unsafe.Pointer, len(base.slots))}
  copy(next.slots, base.slots)
  ```
  — a **full clone of the shard's entire slot array per edge inserted**.
- **Evidence (measured):** CPU profile of one recovery (200k nodes / 800k edges) is entirely
  `runtime.*` GC/allocator frames — `madvise` 10.9 %, `scanObject` 23.2 % cum, `tryDeferToSpanScan`
  15.9 %, `bulkBarrierPreWrite` 5.8 % — with no GoGraph function above 1.1 %. The alloc_space
  profile attributes **3,981 MB of 4,188 MB (95.07 %)** to a single frame:
  `adjlist.storeEntry` ← `upsertEdgeLocked` ← `addEdge` ← `lpg.AddEdgeHIfAbsent` ←
  `snapshot.ApplyCSRToGraph` ← `recovery.openCodec`.
  A/B run through the **public API only** (`lpg.Graph.ApplyAtomically`, which brackets
  `adj.BeginCommit`/`EndCommit` at `graph/lpg/lpg.go:536`,`542`), repo unmodified, output verified
  identical (`order=200000 size=800000` in both arms):

  | | apply phase | allocated | GC cycles |
  |---|---|---|---|
  | current (no window) | 629 / 664 / 641 ms | 4,306.6 MiB | 59–60 |
  | one commit window | 213 / 216 / 227 ms | 204.3 MiB | 4 |
  | **delta** | **2.9–3.1× faster** | **21.1× less** | **15× fewer** |

  Consequence today: the checkpoint buys almost nothing in recovery time. Clean A/B on identical
  content (100k/400k), one dir checkpointed and one not: WAL replay 271–281 ms vs snapshot restore
  242–249 ms — **only 0.88–0.92×**, despite the snapshot eliminating 700,000 WAL ops and half the
  bytes. With F1 applied the snapshot path becomes the decisive win a checkpoint is supposed to be.
- **Lever:** add `g.AdjList().BeginCommit(); defer g.AdjList().EndCommit()` around the
  `haveMapper` block (`recovery.go:1034-1102`), byte-for-byte mirroring line 1335-1336. Use the bare
  pair, not `ApplyAtomically`, to match the existing precedent (the measurement above used
  `ApplyAtomically`, a strict superset — it additionally takes `visMu`, which recovery does not need
  since the graph is not yet published).
- **TCK/ACID impact:** none. The mutation sequence and its order are unchanged — only the number of
  shard-array clones changes; `EndCommit` freezes the builders before the graph is returned.
  Recovery already declares the exact contract `BeginCommit` requires: *"Recovery is a one-shot
  bootstrap step run by a single goroutine before the graph is published for serving"*
  (`recovery.go:13-14`). The A/B above empirically proves no re-entrancy/deadlock in any Apply\*
  function. Recovery is not on any query path, so TCK is untouched.
- **Effort:** **S** (4 lines + a regression test asserting allocation churn).

### F2. No space reclamation: deleted nodes and their edges are never removed from disk  [NEW]  (severity: HIGH)

- **What GoGraph does:** deletion is logical only. `Tx.RemoveNode` tombstones the node; the
  tombstone set is persisted to `tombstones.bin` (`store/snapshot/tombstones.go:32`) and masks the
  node at read time. Nothing ever compacts.
- **Evidence (measured):** 100,000 nodes + 100,000 edges, checkpoint, then `RemoveNode` on 90,000
  nodes, then checkpoint:

  | file | before | after | reclaimed |
  |---|---|---|---|
  | `csr.bin` | 3,235,611 | 3,235,611 | **0 %** |
  | `mapper.bin` | 2,200,014 | 2,200,014 | **0 %** |
  | `labels.bin` | 1,200,042 | 120,042 | 90 % |
  | `properties.bin` | 2,700,040 | 270,040 | 90 % |
  | `tombstones.bin` | 0 | **+720,016** | grows |
  | **total** | **9,336,260** | **6,546,370** | **30 %** |

  A second checkpoint reclaims **nothing** (6,546,370 → 6,546,370). Reading the `csr.bin` header
  directly: `nVertexSlots` 104,449 → 104,449 and `nEdges` 100,000 → **100,000** — i.e. the edges of
  deleted nodes are still fully serialised, and `AdjList().Size()` still reports 100,000 live edges
  with 90,000 tombstones. Live data after the delete is ~10 % of the original, so the store carries
  **~7× permanent space amplification**, and it is unbounded under create/delete churn at constant
  live-set size.
- **What they do:** Neo4j ships an offline store rewrite — `neo4j-admin database copy` /
  `neo4j-admin database migrate` (Operations Manual, *Migrate a database*) — which rebuilds the
  store from live records; internal IDs are reassigned precisely because they encode physical
  location. Memgraph reclaims implicitly: a snapshot *"the entire data storage is written to the
  drive"* enumerates only currently-live vertices/relationships
  (memgraph.com/docs/fundamentals/data-durability), so its on-disk size tracks the live set.
- **Lever:** add an **offline compaction entry point** — e.g. `store/compact` that opens a store,
  re-mints dense NodeIDs for live nodes only, drops tombstoned nodes and their edges, writes a fresh
  self-sufficient snapshot to a staging dir, and publishes it with the existing crash-atomic
  archive→rename→fsync sequence (`store/snapshot/full.go:739-769`). This is the exact shape of
  `neo4j-admin database copy`, and every primitive it needs already exists. Cheaper interim step:
  make the checkpoint's CSR capture skip tombstoned sources so `csr.bin` at least tracks live edges.
- **TCK/ACID impact:** offline (store closed) ⇒ no isolation surface; publish reuses the certified
  crash-atomic path; NodeID re-minting is invisible above the mapper (openCypher `id()` is not
  required to be stable across a compaction, but this must be stated in the compaction contract —
  see Unverified items). No query path touched ⇒ TCK-neutral.
- **Effort:** **L**.

### F3. Checkpoint cost is O(database size), not O(delta); an idle checkpoint rewrites everything  [NEW]  (severity: HIGH)

- **What GoGraph does:** `runNonBlocking` (`store/checkpoint/checkpoint.go:666`) has no
  "is there anything to do?" gate. It captures the CSR, writes a **complete** snapshot
  (`writeSnapshot` → `WriteSnapshotFull*`), then prefix-truncates. There is no dirty-region
  tracking and no incremental component.
- **Evidence (measured):** two checkpoints back-to-back with **zero transactions between them**:
  wall **70.6 ms**, and `csr.bin` mtime changes with the same 835,610 bytes rewritten. Cost scales
  linearly with graph size and is independent of the delta:

  | nodes / edges | snapshot bytes | ckpt #1 (folds full WAL) | ckpt #2 (**zero new tx**) |
  |---|---|---|---|
  | 25k / 100k | 4,848,913 | 59.4 ms | 48.9 ms |
  | 50k / 200k | 9,678,715 | 73.9 ms | 68.9 ms |
  | 100k / 400k | 19,336,267 | 119.8 ms | 122.8 ms |
  | 200k / 800k | 38,647,276 | 238.5 ms | 238.1 ms |

  Extrapolated: a 100 M-node/400 M-edge store on a Neo4j-like 15-minute cadence would rewrite
  ~19 GB every 15 minutes whether or not a single transaction committed — pure write amplification
  and SSD wear.
- **What they do:** Neo4j's default `db.checkpoint=PERIODIC` is explicitly delta-gated —
  *"This policy checks every 10 minutes **whether there are changes pending flushing** and if so, it
  performs a checkpoint"* (Operations Manual, *Checkpointing and log pruning*) — and the checkpoint
  itself flushes only **dirty pages** out of the 8 KiB-page off-heap page cache, throttled by
  `db.checkpoint.iops.limit`=600. Cost is proportional to write volume, not to database size.
  Memgraph shares GoGraph's full-snapshot model but mitigates it with
  `--storage-parallel-snapshot-creation` + `--storage-snapshot-thread-count`.
- **Lever:** two tiers.
  (a) **Cheap and immediate:** skip the checkpoint when `wlog.DurableOffset() == 0` and no
  schema DDL has occurred since the last successful run — GoGraph's exact analogue of Neo4j's
  "changes pending flushing" gate. Removes 100 % of idle-checkpoint I/O for a few lines.
  (b) **Structural:** dirty-component tracking, so a checkpoint rewrites only the components that
  changed (`labels.bin`/`properties.bin`/`csr.bin` independently), with the manifest carrying
  per-component generation numbers and unchanged components hard-linked from the previous snapshot.
- **TCK/ACID impact:** (a) is a pure no-op suppression — the on-disk state it declines to rewrite is
  byte-identical to what is already published, and the self-sufficiency gate at
  `checkpoint.go:829` is unaffected. (b) must keep `snapshotIsSelfSufficient` semantics: a
  hard-linked component is still listed in the manifest with its CRC32C, so the existing
  manifest-content check (`checkpoint.go:950-976`) stays correct unchanged. No query path ⇒
  TCK-neutral.
- **Effort:** **S** for (a), **L** for (b).

### F4. Checkpoint phase 3 copies the whole surviving WAL suffix **under the commit lock** — segmented WAL now justified  [NEW]  (severity: MEDIUM-HIGH)

**This overturns round 1's consensus rejection of a segmented WAL. New decisive evidence follows.**

- **What GoGraph does:** phase 3 runs inside `runUnderCommitLock` (`checkpoint.go:806-808`) and
  calls `wal.Writer.TruncatePrefix` (`store/wal/writer.go:843`), which — because the WAL is one
  un-segmented file — **streams the entire surviving suffix into a temp file, fsyncs it, renames,
  and fsyncs the parent dir** (`writeSuffixTmp`, `store/wal/writer.go:984-1009`). The suffix is
  exactly the transactions committed during the deliberately lock-free phase 2, so **the stall grows
  with write-rate × snapshot duration** — largest precisely under the load where a stall hurts most.
  The code comment at `writer.go:809-812` acknowledges the shape: *"This mirrors the file-granularity
  WAL reclamation of RocksDB … and PostgreSQL …, adapted to GoGraph's single un-segmented WAL file."*
- **Evidence (measured):** `TruncatePrefix` timed under `RunUnderCommitLock` (= the real writer stall):

  | surviving suffix | stall |
  |---|---|
  | 1.1 MB | 8.1 ms |
  | 5.7 MB | 9.5 ms |
  | 22.6 MB | 15.4 ms |
  | 56.5 MB | **30.9 ms** |

  ~7 ms fixed floor (three fsyncs, `F_FULLFSYNC` on APFS) plus a linear copy term. For comparison,
  the phase-1 capture this design was built to minimise is only **3.8 ms** for a 100k/400k graph —
  so **phase 3 is already the dominant blocking phase**, and it is the one that grows without bound.
- **What they do:** Neo4j rotates the transaction log at `db.tx_log.rotation.size`=256 MiB and prunes
  whole files after a checkpoint under `db.tx_log.rotation.retention_policy`=`2 days 2G`
  (Operations Manual, *Transaction logging*). Memgraph rotates at
  `--storage-wal-file-size-kib`=20480 and *"Older WAL files are deleted automatically after a
  snapshot is created"*. Both reclaim at **whole-file granularity: unlink(2), O(1), no data copy,
  nothing held across it.**
- **Lever:** segment the WAL into fixed-size files (`wal.000001`, …) rolled at a configurable size.
  Phase 3 then becomes: unlink every segment strictly below the watermark segment + one parent-dir
  fsync — sub-millisecond and independent of write rate. The watermark already exists in the right
  form (`DurableOffset()` is a frame boundary); it becomes (segment, offset). Recovery already
  iterates frames to EOF, so it needs only to chain segments in order.
- **Why round 1's rejection no longer holds:** the rejection treated segmentation as a
  space-management nicety. It is not — it is the only way to remove an unbounded O(suffix) data copy
  from the write-blocking critical section. The measurement above is the evidence round 1 lacked.
- **TCK/ACID impact:** strictly *stronger*. Today a phase-3 failure after the rename must fail-stop
  the writer (`poisonAfterRename`, `writer.go:971`) because the on-disk state has already advanced
  irreversibly; with segments, reclamation is a sequence of independent unlinks, each of which is
  individually crash-atomic and idempotent — a crash mid-reclaim simply leaves extra segments that
  recovery replays idempotently on top of the self-sufficient snapshot (the exact argument
  `checkpoint.go:796-803` already makes). No query path ⇒ TCK-neutral.
- **Effort:** **M**.

### F5. No WAL-volume/txn-count checkpoint trigger — and no WAL-size metric to build one  [CONFIRMED-R1, strengthened]  (severity: MEDIUM)

- **What GoGraph does:** `checkpoint.Config` exposes only `Dir`, `MaxAge`, `Interval`
  (`store/checkpoint/checkpoint.go:63-74`). Round 1's T1.4 stands. Two aggravations round 1 missed:
  1. **No WAL-size observability.** Every WAL metric is a counter, not a gauge
     (`store.wal.TruncatePrefix.bytes_reclaimed`, `store.wal.SyncGroup.*`, `*.errors`);
     `Stats.Bytes` is a monotonic lifetime total, not the current file size. `DurableOffset()` is
     the live size but is not exported as a metric. An operator therefore cannot even **alert** on
     WAL growth, let alone trigger on it.
  2. **`MaxAge == 0` silently disables periodic checkpointing entirely** — `Interval` defaults to
     `MaxAge/4` only when `MaxAge > 0` (`checkpoint.go:372-377`), and the loop installs no ticker
     when `Interval == 0` (`checkpoint.go:539`). Every non-test construction in the repo except
     `examples/17_transactional_log/main.go:399` passes bare `checkpoint.Config{Dir: …}` — i.e. a
     checkpointer that never fires on its own. The Bolt server wires **no checkpointer at all**.
- **What they do:** Neo4j offers four policies with all three trigger axes wired by default —
  `db.checkpoint.interval.time`=15m, `db.checkpoint.interval.tx`=100000,
  `db.checkpoint.interval.volume`=250.00MiB. Memgraph: `--storage-snapshot-interval`=300 s.
- **Lever:** add `Config.MaxWALBytes` and `Config.MaxWALTxns` checked on the same poll tick against
  `wlog.DurableOffset()` and the store's committed-transaction counter; publish
  `store.wal.size_bytes` as a gauge. Neo4j's 250 MiB is a defensible default.
- **TCK/ACID impact:** none — a checkpoint is already safe at any transaction boundary; this only
  changes *when* the existing certified three-phase path runs.
- **Effort:** **S**.

### F6. WAL frames one record per operation, not per transaction  [NEW]  (severity: MEDIUM)

- **What GoGraph does:** every op gets its own frame: 14-byte header (magic+version+len+CRC32C,
  `store/wal/format.go:33`) + a 10-byte v3 record header (version+kind+txnSeq) + payload, plus one
  further frame for the `OpCommit` marker.
- **Evidence (measured):** 100,000 `AddNode` ops with a 10-char key:

  | ops/txn | frames/op | bytes/op | fsyncs |
  |---|---|---|---|
  | 1 | 2.000 | 68.0 | 100,000 |
  | 10 | 1.100 | 46.4 | 10,000 |
  | 100 | 1.010 | 44.2 | 1,000 |
  | 1000 | 1.001 | 44.0 | 100 |

  At best 14 of 44 bytes/op (**32 %**) is framing; a single-op transaction pays 68 bytes to record
  one node. The mixed 700,000-op workload measured 59.8 B/frame over 41.9 MB.
- **Why the framing buys nothing:** recovery already buffers a whole v3 transaction and applies it
  only at the `OpCommit` marker (`recovery.go:1347`, and `ErrTransactionTooLarge` exists precisely
  to bound that buffer). The transaction, not the op, is already the atomic replay unit — so
  per-op magic/length/CRC provides no recovery capability that a per-transaction frame would not.
- **What they do:** Neo4j appends a transaction's command batch as one log entry
  (`BatchingTransactionAppender`, verified in `neo4j/neo4j`), and Memgraph applies *"a 4-byte
  checksum covering the transaction's bytes"* — one checksum per transaction, not per delta.
- **Lever:** emit one frame per transaction whose payload is the concatenated op records
  (`[opCount][op]…[op]`), keeping the `OpCommit` semantics as a trailing sentinel inside the frame.
  Expect ~30 % smaller WAL, ~N× fewer CRC32C invocations and `bufio` writes per commit, and
  proportionally faster replay (recovery currently runs at 80–125 MiB/s and degrades from
  0.357 µs/frame at 20k frames to 0.561 µs/frame at 800k).
- **TCK/ACID impact:** the durability boundary is unchanged (marker durable before ack). Atomicity
  strengthens slightly: a torn transaction becomes a single torn frame rather than a partial run of
  valid frames plus a missing marker. `ErrTornFrameMasksData` (F11) still applies, unchanged, to the
  single frame. Requires a WAL format-version bump (`CurrentVersion` is 1, `format.go:29`) and the
  reader must keep accepting v1 — the code already mandates *"Readers must accept all versions <=
  CurrentVersion"*. No query path ⇒ TCK-neutral.
- **Effort:** **M**.

### F7. No backup primitive — and the obvious `cp -r` backup silently loses data  [NEW]  (severity: HIGH, operational)

- **What GoGraph does:** there is **no** backup/restore API anywhere in `store/` and **no documented
  backup procedure** (`docs/persistence.md` mentions "backup" only in the internal `snapshot.bak`
  sense). For an embeddable library the obvious user story is "copy the store directory".
- **The trap:** the checkpoint publishes the new snapshot in phase 2 and truncates the WAL prefix in
  phase 3. A recursive copy that reads `snapshot/` **before** `wal` can therefore capture the *old*
  snapshot together with the *post-truncation, suffix-only* WAL — and the transactions in the
  discarded prefix are then present in neither. `snapshot` sorts before `wal`, so `cp -a`, `tar`,
  `rsync` and `filepath.WalkDir` all take exactly the dangerous order by default. (The reverse
  order is safe: a new snapshot plus an un-truncated WAL replays idempotently — the property
  `checkpoint.go:796-803` already relies on.) Nothing in the code or docs warns about this.
- **What they do:** Memgraph documents precisely this procedure and supplies the missing primitive —
  `CREATE SNAPSHOT;` → **`LOCK DATA DIRECTORY;`** (which *"only suspends Memgraph's own background
  deletion of superseded snapshot/WAL files"*, so reads and writes continue) → copy → **`UNLOCK DATA
  DIRECTORY;`** (memgraph.com/docs/database-management/backup-and-restore). Neo4j EE goes further
  with online backup, differential chains, and `--restore-until=<txId|timestamp>` PITR.
- **Lever:** add `DB.LockForBackup() / DB.UnlockForBackup()` (or a `DB.Backup(dstDir)` that does it
  internally) which suspends **WAL prefix truncation and `snapshot.bak` removal** for the duration.
  Any file-order copy taken inside the lock is then consistent by construction. This is small, is
  exactly Memgraph's design, and is the single highest value-per-line item after F1 for anyone
  running GoGraph in production. PITR is genuinely out of scope for an embedded library and should
  be declined explicitly.
- **TCK/ACID impact:** none — it only *defers* reclamation, which the checkpointer already treats as
  optional (it declines to truncate whenever the snapshot is not self-sufficient,
  `checkpoint.go:834-846`, and merely emits `store.checkpoint.truncate_skipped_not_self_sufficient`).
  The same metric/skip path can carry the backup-lock case. No query path ⇒ TCK-neutral.
- **Effort:** **S** (lock + docs), **M** (with a `DB.Backup` convenience).

### F8. macOS durability defect from round 1 does not exist  [STALE-R1]  (severity: informational — remove from the backlog)

- **Round-1 claim:** *"macOS `fsync` != `F_FULLFSYNC` (weaker durability on Darwin) — known, opt-in
  fix pending."*
- **Why it is wrong:** on non-Linux, `dataSync` delegates to the handle's `Sync()`
  (`store/wal/data_sync_other.go`), which for the production `*os.File` is `os.File.Sync()` →
  `internal/poll.FD.Fsync()`. In Go 1.26.5 (the toolchain this repo pins) that is:
  ```go
  // Fsync invokes SYS_FCNTL with SYS_FULLFSYNC because
  // on OS X, SYS_FSYNC doesn't fully flush contents to disk.
  _, err := unix.Fcntl(fd.Sysfd, syscall.F_FULLFSYNC, 0)
  ```
  (`$GOROOT/src/internal/poll/fd_fsync_darwin.go`, present since Go 1.12 / issue #26650). GoGraph on
  macOS therefore already issues a **full device-cache flush** — strictly stronger than Linux
  `fdatasync`, which does not flush a volatile drive cache unless the FS/device enforces barriers.
  The doc comment at `data_sync_other.go:6` is accurate as written.
- **Residual caveat (narrow, worth one line of docs):** the same stdlib function falls back to plain
  `fsync(2)` when `F_FULLFSYNC` returns `ENOTSUP` — *"scenarios such as SMB mounts"* (Go issue
  #64215). A store on a network mount silently degrades. Worth stating in `docs/persistence.md`;
  not worth code.
- **Lever:** none. **Close the backlog item.**

### F9. Snapshot components are checksummed; Memgraph's are not  [NEW]  (severity: informational — a win to keep)

`manifest.json` records a CRC32C per component (`store/snapshot/manifest.go:101`), verified on load;
`csrfile` additionally carries a tail CRC32C (`store/csrfile/format.go:62`). Memgraph's own docs
state plainly: *"Snapshots are not yet protected by checksums"* (WAL got CRC32 only in v3.12).
Nothing to take; do not regress it.

### F10. A fast bulk-load path exists in `store/bulk` and **no user can reach it** — 26,000× faster than the Cypher path it should be serving  [NEW]  (severity: **HIGH** — headline)

- **Cross-stream input:** the planner stream measured bulk load through identical Cypher `UNWIND`
  batches: 2,000 nodes + 19,931 edges took GoGraph **19.3 s** (Neo4j 1.85 s, Memgraph 0.127 s); at
  20,000 nodes / 200,000 edges GoGraph took **35 m 33 s** (Memgraph 977 ms). Root cause is in the
  planner (index not reached), not in `store/`.
- **What GoGraph does:** `store/bulk` *"bypasses the transactional WAL stack and writes a Tier 2
  csrfile directly"* (`store/bulk/bulk.go:1-10`), verified by `TestLoader_BypassesWAL`. Measured
  here at the **exact shapes the cross-stream benchmark used**:

  | shape | Cypher `UNWIND` | `store/bulk` | speed-up | Memgraph (Cypher) |
  |---|---|---|---|---|
  | 2,000 nodes / 19,931 edges | 19.3 s | **24.1 ms** (827 k edges/s) | **≈800×** | 0.127 s |
  | 20,000 nodes / 200,000 edges | 35 m 33 s | **81.3 ms** seq / **29.6 ms** parallel (6.75 M edges/s) | **≈26,000× / 72,000×** | 0.977 s |
  | 100,000 nodes / 1,000,000 edges | not run | **123.9 ms** (8.07 M edges/s) | — | — |

  So the storage layer is **not** the bulk-load bottleneck — it is three to four orders of magnitude
  faster than the path users actually have, and at 29.6 ms for 20k/200k it is ~33× faster than
  Memgraph's Cypher loader on the same shape.
- **Why users cannot reach it — three separate walls:**
  1. **Not reachable from Cypher at all.** There is no `LOAD CSV`-equivalent, no bulk DML hook; the
     loader is a Go API only.
  2. **Its output is not a database.** The csrfile is consumed only by the out-of-core reader
     `search/extern` and `examples/{05,18}` — **no `store.DB` / `recovery.Open` path reads a
     csrfile**. `store/bulk/crossproc_bulk_recover_test.go` makes this explicit: after bulk-loading
     it separately rebuilds an LPG and hand-calls `snapshot.WriteSnapshotFull` to obtain a
     recoverable directory.
  3. **It drops the property graph.** `bulk.Edge` is `{Src, Dst string; Weight int64}` — no labels,
     no properties, no indexes, no constraints. So even the Go API cannot load a realistic dataset.

  These are honest caveats, not quibbles: the numbers above are **not** apples-to-apples with the
  Cypher figures (no labels/properties, no durability, no transaction). But they do prove the
  ingest machinery — deterministic, bounded-parallel, byte-identical across sequential/parallel
  paths, crash-atomic on publish — is already production-grade and already 90 % of what an offline
  importer needs.
- **What they do:** `neo4j-admin database import full` writes CSV straight into the native store
  files the database then opens (EE adds `--incremental`, `--schema`). Memgraph's documented fastest
  path is batched `LOAD CSV` in `IN_MEMORY_ANALYTICAL` mode, claiming *"one million nodes or
  relationships per second"* — a figure `store/bulk` already exceeds by 8× on raw edges.
- **Lever (headline recommendation):** give `store/bulk` an **offline store-import mode** that emits
  a self-sufficient snapshot directory (CSR + mapper + labels + properties + indexdefs + an empty
  WAL) instead of a bare csrfile, published through the existing crash-atomic
  `WriteSnapshotFull*` path (`store/snapshot/full.go:739-769`), and widen the input record to carry
  node labels and properties. Every component writer already exists — this is wiring, not new
  storage machinery. Then expose it as an offline `gograph import` entry point. That converts a
  35-minute load into a sub-second one **without touching the planner**, and gives the planner
  stream's fix a target to be measured against rather than being the only remedy.
  It shares its output contract with F2's compaction pass — design the two together.
- **TCK/ACID impact:** offline (store closed) and single-writer, so there is no isolation surface;
  publication reuses the certified crash-atomic archive→rename→fsync sequence. Bulk load stays
  explicitly non-transactional — exactly as it is today and exactly as `neo4j-admin import` is —
  and that must remain documented. No query path is touched ⇒ TCK-neutral.
- **Effort:** **M**.

### F11. Torn-tail vs. corruption discrimination — still unique, now with comparative citations  [CONFIRMED-R1]  (severity: informational)

`Decode` promotes a short read to `ErrTornFrameMasksData` when the bytes the over-long read consumed
themselves contain a CRC-valid frame (`store/wal/format.go:229-232`, `embedsValidFrame:279`, with an
O(n) CRC budget against an adversarially-shaped tail at `:282-318`). This separates *"the writer
crashed mid-write of the last frame"* (benign; truncate and continue) from *"a corrupt length field
swallowed durable committed frames"* (fail-stop). The comparison round 1 asserted but did not cite:
- RocksDB — which Memgraph's `ON_DISK_TRANSACTIONAL` mode configures with
  `WALRecoveryMode::kPointInTimeRecovery` — is documented as *"Recover to point-in-time consistency
  (default). We stop the WAL playback on discovering WAL inconsistency"*, i.e. truncate-and-accept:
  exactly the posture that would silently drop the swallowed frames.
- Memgraph's in-memory engine is fail-stop but undiscriminating: *"if a database fails durability
  recovery on startup — because of a truncated WAL, a corrupt snapshot, a missing prefix WAL file,
  or any other malformed durability file — the entire Memgraph process refuses to boot."*
GoGraph is the only one of the three that both accepts the benign case without operator
intervention **and** fail-stops the masking case. **Nothing to take.**

### Durability re-verification at `6f31f61`

`go test -tags=gograph_crashinject -count=1 ./internal/crashinject/... ./internal/crashpoint/...
./store/recovery/...` → **ok / ok / ok, exit 0** (39.9 s for `store/recovery`, which holds the
SIGKILL, torn-tail, snapshot-promote, checkpoint-crashpoint and cross-process scenarios). One
positive side-observation: a store configured without a `WeightCodec` **refuses** to persist a
non-zero edge weight (`ErrNoWeightCodec`) rather than silently dropping it — correct fail-stop at a
durability boundary, encountered while building the harness.

---

## Nothing-to-take list

- **Memgraph's default fsync cadence.** `wal_file_flush_every_n_tx{100'000}`
  (`memgraph/src/storage/v2/config.hpp`, confirmed against the live repo) means up to 99,999
  acknowledged transactions can be lost on host crash or power loss. Precision round 1 lacked:
  Memgraph `write()`s every transaction, so plain `kill -9` is survivable; the exposure is
  specifically host crash / power loss. GoGraph's `SyncGroup` acknowledges only after the covering
  fsync (`store/wal/writer.go:491`). **Reject.**
- **Memgraph `IN_MEMORY_ANALYTICAL`** — *"no isolation levels and no ACID guarantees"*, snapshots
  and WAL not created. Incompatible with GoGraph's ACID mandate. **Reject.**
- **Memgraph `ON_DISK_TRANSACTIONAL`** — still labelled *"experimental… not recommended for
  production"*, snapshot isolation only, RocksDB compression explicitly disabled
  (`compression = kNoCompression`), and the working set must still fit in RAM. It buys nothing over
  GoGraph's existing out-of-core Tier-2 csrfile. **Reject.**
- **Full MVCC / per-object locking** — re-confirmed out of scope here; GoGraph's single-writer
  barrier is what makes the checkpoint's transaction-boundary capture cheap (3.8 ms for 100k/400k).
- **Async WAL flush** — the round-1 rejection stands and F5/F4 do not weaken it; bounding WAL
  *growth* and removing the phase-3 *copy* are the right levers, not weakening the ack contract.
- **Neo4j clustering / PITR / online differential backup** — server features. An embedded library
  should decline PITR outright and satisfy the real need with F7's backup lock.
- **Neo4j `block` format's inlining** — Enterprise-only, and its purpose (co-locate properties with
  nodes to cut pointer chasing in a page-cache design) is already GoGraph's status quo: the graph is
  RAM-native and the columnar property tier is already de-boxed. **Nothing to take.**
- **Off-heap page cache** — round-1 rejection stands; F1 shows recovery is allocator-bound, not
  page-cache-bound, and F1 fixes that without an off-heap redesign.

## NOT INVESTIGATED (explicit coverage gaps in this stream)

1. **On-disk format upgrade path, not exercised.** I read the versioning scheme (WAL
   `CurrentVersion`=1 with a documented "readers must accept all versions <= CurrentVersion" rule,
   `wal/format.go:26-29`; snapshot manifest v1/v2/v3 with a transparently-accepting loader,
   `snapshot/manifest.go:23-45`; csrfile v1, `csrfile/format.go:20`) and noted that
   `store/{csrfile,snapshot,wal}/format_compat_test.go` and `csrfile/version_upgrade_test.go` exist
   — but I did **not** run them, and did **not** load a genuinely old on-disk corpus against this
   build. The design is sound on inspection; the guarantee is unverified by me.
2. **Windows durability posture.** `ParentDirSync`/`DirSync` are documented as **no-ops on
   platforms without a directory-fsync primitive (Windows)** (`snapshot/full.go:719-720`,
   `recovery.go:922-923`). Every crash-atomic rename in the engine — snapshot publish, snapshot
   promote, `wal.TruncatePrefix` — depends on that fsync to make the directory entry durable. On
   Windows those renames are therefore **not** proven durable across a host crash. I did not
   investigate whether an equivalent primitive exists (`FlushFileBuffers` on a directory handle is
   not generally available) or what the real exposure is. This deserves its own audit; it is a
   potential Durability gap on a supported platform.
3. **`store/csrfile` Tier-2 mmap reader** was read only at the header/format level. Its bounds and
   overflow handling were certified in an earlier round (see memory
   `gograph_ondisk_decoders_security_audit`) and were not re-audited here.
4. **`store/txn` internals** (codec framing, undo log, rollback) were traversed only as far as the
   commit/durability path required; prior rounds certified them and nothing I measured contradicts
   that.
5. **Replication / HA** was assessed as out of scope for an embeddable library and not investigated
   beyond confirming that no such code exists in `store/`.
6. **Concurrent-writer stall during a live checkpoint** was measured indirectly (by timing
   `TruncatePrefix` under `RunUnderCommitLock`, F4) rather than by driving concurrent committers
   against a running checkpointer and recording their latency distribution. The stall figures are
   therefore the lock-hold duration, not an observed p99 commit latency.

## Unverified items

1. **Neo4j ID reuse / free-lists.** F2's lever cites `neo4j-admin database copy`/`migrate` (verified
   by documentation) as the *shape* of the remedy. I did **not** verify Neo4j's `.id` free-list reuse
   behaviour or `dbms.ids.reuse.types.override` against primary sources, so F2 makes no claim about
   online ID reuse in Neo4j.
2. **`neo4j-admin database import full` throughput.** No quantified figure is published; F10's
   comparison is qualitative.
3. **openCypher stability of `id()` across a compaction.** F2's NodeID re-minting needs a ruling on
   whether `id()`/`elementId()` may change across an offline compaction. openCypher does not
   guarantee id stability across restarts, but this repo's Primary-Id feature may impose a stronger
   local contract. **This is a scope/behaviour decision and must go to the user before F2 is
   planned.**
4. **Memgraph snapshot creation time / memory multiplier.** Only qualitative language
   (*"spikes in memory consumption"*) is published; no absolute numbers exist to compare against
   GoGraph's measured checkpoint costs in F3.
5. **Whether Memgraph 3.x formally deprecated `ON_DISK_TRANSACTIONAL`.** Current docs say
   "experimental", not "deprecated"; the round-1/brief premise of deprecation is unconfirmed.
6. **Recovery scaling beyond 400k nodes.** Measured 0.357 → 0.561 µs/frame from 20k to 800k frames
   (a 57 % per-frame degradation, i.e. mildly super-linear). I did not isolate whether that is cache
   behaviour or the same COW growth as F1; F1 should be re-measured at 10⁶–10⁷ scale afterwards.
