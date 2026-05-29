# ACID Compliance Audit

Status: **in remediation** — this document is the specification for the
ACID-hardening sprint. It records, with first-hand `file:line` evidence,
every gap found between the module's behaviour and the ACID mandate stated
in `CLAUDE.md` ("100% ACID Compliant"). It is updated as each gap is closed.

ACID is defined here exactly as the mandate states:

- **Atomicity** — every transaction is all-or-nothing: either every write
  becomes visible together or none of them do. Partial application after a
  crash or error is forbidden.
- **Consistency** — every committed transaction leaves the graph satisfying
  every declared invariant; reads never observe an invariant-violating state.
- **Isolation** — concurrent transactions behave as if serialised; readers
  never observe the partial writes of an in-flight transaction; writers never
  silently overwrite each other.
- **Durability** — once a commit acknowledgement is returned, the change
  survives process crash, host crash, and `kill -9`.

## Scope and method

The audit covers the full write/read/recovery path, not only `store/`:

- `store/wal` — the write-ahead log (frame format, fsync, truncate).
- `store/txn` — the transaction surface (`Begin`/`Commit`/`Rollback`).
- `store/recovery` — snapshot + WAL replay on `Open`.
- `store/checkpoint` — the background snapshot+truncate loop.
- `store/snapshot` — the on-disk snapshot writer/reader.
- `graph/adjlist`, `graph/lpg` — the in-memory engine readers/writers see.
- `cypher/exec` — the write operators (CREATE/SET/MERGE/DELETE) that produce
  multi-op transactions.

Evidence baseline (2026-05-29): `go test -race ./store/... ./internal/crashinject/...`
is **green**. The gaps below are therefore *untested* paths, not failing
tests: the suite validates atomicity only for **single-op** transactions
(`store/recovery/torn_tail_test.go:68` — "each commit is exactly one op").

## Findings

| ID | Property | Severity | Status | One-line |
|----|----------|----------|--------|----------|
| F1 | Atomicity | **Critical** | open | Multi-op transactions are not atomic across a crash — recovery can replay a *prefix* of a transaction's ops. |
| F2 | Durability + Consistency | **Critical** | **fixed** | A checkpoint writes a CSR-only snapshot then truncates the WAL, destroying committed labels/properties (and the mapper for non-string keys). |
| F3 | Isolation | **High** | open | The in-memory apply loop publishes ops one-by-one, so a lock-free reader can observe a partially-applied, in-flight transaction. |
| F4 | Durability | **Medium** | open | `wal.Open` never fsyncs the parent directory, so a freshly-created WAL file's directory entry may not survive a crash. |
| F5 | Atomicity | **Low** | open | `Tx.Commit` applies after `Sync`; an apply error mid-loop leaves the in-memory view partial while the WAL is fully durable, and returns an error although the transaction is durably committed. |

### F1 — Multi-op transactions are not atomic across a crash (Atomicity)

**Evidence.** The WAL frame carries no transaction identity: it is
`magic | version | len | crc32c | payload`, one mutation per frame
(`store/wal/format.go:53-84`). `Tx.Commit` appends one frame per buffered op
and calls `Sync` exactly once at the end (`store/txn/txn.go:436-456`).
Recovery replays frame-by-frame and applies each op immediately, stopping at
the first torn/CRC-bad frame after having already applied everything before it
(`store/recovery/recovery.go:662-683`). There is no commit record.

**Why it breaks Atomicity.** A transaction with N>1 ops (every Cypher
`CREATE`/`MERGE`/`SET`/multi-`DELETE` produces several ops in a single `Tx`)
can be torn between op *k* and op *k+1*: a crash, a torn tail write, or a CRC
failure on a middle frame leaves recovery applying ops `1..k` and dropping
`k+1..N`. Example: `CREATE (a:L {p:1})-[:R]->(b)` partially replays as "a node
with a label but no property" or "an edge whose endpoint lost its label" —
a state the transaction never intended to be visible.

**Fix (design under specialist review).** Introduce transaction atomicity in
the WAL so a torn multi-op transaction is dropped *entirely*. Two candidate
designs: (a) explicit COMMIT-record framing with a per-transaction sequence,
recovery buffering ops until the matching COMMIT; (b) encode the whole op
batch as a single CRC-protected frame (all-or-nothing by construction). The
chosen design must preserve replay of existing single-op v1/v2 WALs unchanged.

**Acceptance.** A new crash-injection scenario tears a multi-op transaction at
every interior frame boundary; recovery must yield either the full transaction
or none of it — never a prefix. Existing single-op torn-tail tests still pass.

### F2 — Checkpoint destroys committed labels/properties (Durability + Consistency)

**Evidence.** `checkpoint.runCheckpoint` builds a CSR from the adjacency list
and calls `snapshot.WriteSnapshotCSR` — the *legacy CSR-only* writer, whose own
doc says callers needing label durability must use `WriteSnapshotFull`
(`store/snapshot/writer.go:177-184`) — then immediately calls `wlog.Truncate()`
(`store/checkpoint/checkpoint.go:234-244`). The CSR snapshot captures adjacency
only: no labels, no properties, no indexes, no mapper.

**Why it breaks Durability/Consistency.** Labels and properties committed before
the checkpoint are durable *only* in the WAL. The checkpoint snapshot does not
capture them, and the truncate erases the WAL frames that held them. On the next
`recovery.Open` the graph rebuilds adjacency but the labels/properties are gone —
a committed transaction did not survive. For non-string node keys the loss is
worse: `WriteSnapshotFull` only emits `mapper.bin` (the NodeID↔key table) for
`N=string` (`store/snapshot/full.go:41-66`); for other `N` it relies on WAL
replay to re-intern keys, so truncating the WAL also strands the adjacency.

**Fix.** The checkpoint must persist a *self-sufficient* snapshot — CSR + labels
+ properties + indexes + mapper for **all** `N` — and fsync it durably
(including the parent directory and the publish rename) *before* truncating the
WAL. The crash-safe ordering is: write+fsync snapshot tmp dir → rename → fsync
parent dir → `wal.Sync` → `wal.Truncate`.

**Acceptance.** A test commits node/edge labels and properties, runs a
checkpoint (truncating the WAL), reopens via `recovery.Open`, and asserts every
label and property survives — for both string and non-string `N`. A
crash-injection scenario between snapshot-publish and WAL-truncate must never
lose committed data.

**Resolution (fixed).** `runCheckpoint` now calls `snapshot.WriteSnapshotFull`
(CSR + labels + properties + indexes + mapper) instead of the CSR-only
`WriteSnapshotCSR`, and truncates the WAL **only** when the resulting snapshot
is self-sufficient — detected by the presence of `mapper.bin` in the manifest
(`snapshotIsSelfSufficient`). For string keys (every production caller) the
snapshot is fully self-sufficient (v3) and the WAL is reclaimed as before, now
with labels/properties/mapper preserved. For non-string keys the snapshot lacks
`mapper.bin`, so truncation is skipped (surfaced via
`store.checkpoint.truncate_skipped_not_self_sufficient`) and the WAL is retained
and replayed at recovery — **no committed data is ever lost for any `N`**.
Regression tests: `recovery.TestCheckpointDurability_LabelsPropertiesEdgesSurvive`
(string `N`: edges + labels + node/edge properties survive with `WALOps == 0`),
`recovery.TestCheckpointDurability_NonStringKeysNotLost` (int64 `N`: guard
engages, WAL retained, all state survives), plus the strengthened
`recovery.TestSequencing_WALCheckpointWAL`, `checkpoint.TestCheckpoint_TransitionRecovery`,
and `bulk.TestSequencing_BulkCheckpointSnapshotRecovery`. Extending `mapper.bin`
to all key types (so non-string checkpoints can also truncate, bounding WAL
growth) is a tracked operational follow-up; it does not affect the Durability
guarantee, which now holds for every `N`.

### F3 — Readers can observe partial transactions (Isolation)

**Evidence.** The adjacency list publishes each single op atomically via
`atomic.StorePointer` per shard/slot, and readers are lock-free
(`graph/adjlist/adjlist.go:16-30, 92-99`). But `Tx.Commit` applies a
transaction's ops to the live graph one at a time in a plain loop
(`store/txn/txn.go:457-463`); the loop is not atomic with respect to concurrent
readers.

**Why it breaks Isolation.** A reader running concurrently with a committing
writer can load the graph after ops `1..k` are published and before `k+1..N`
are — observing a partially-applied, in-flight transaction. The mandate
requires "readers never observe the partial writes of an in-flight
transaction". Single-writer means write-write conflicts are out of scope, which
simplifies the target to *atomic visibility* of each transaction.

**Fix (design under specialist review).** Make a whole transaction's writes
flip visible to lock-free readers atomically (e.g. a single top-level
versioned-snapshot pointer swapped once per commit, with structural sharing so
no full-graph copy is needed), such that secondary indexes flip atomically too.
Read paths must stay lock-free and allocation-free on the hot loop.

**Acceptance.** An invariant checker runs many lock-free readers asserting a
cross-op invariant (e.g. "if the edge exists, both endpoint labels exist")
while a writer commits multi-op transactions, under `-race`; no reader ever
observes a violation.

### F4 — WAL file creation is not durable against directory loss (Durability)

**Evidence.** `wal.Open` opens with `O_RDWR|O_CREATE|O_APPEND` and never fsyncs
the parent directory (`store/wal/writer.go:54-65`); `Sync` fsyncs the file only
(`store/wal/writer.go:124-152`). The snapshot package already fsyncs parent
directories (`store/snapshot/parent_fsync_unix.go`), but the WAL does not.

**Why it breaks Durability.** On POSIX filesystems, fsyncing a newly-created
file does not guarantee its directory entry is durable. A crash after the first
commit's file fsync but before the directory metadata is flushed can lose the
entire WAL file — and with it every transaction it held.

**Fix.** fsync the parent directory once after creating the WAL file (reuse or
mirror the snapshot package's parent-fsync helper, with the existing
unix/other build-tag split).

**Acceptance.** A unit test verifies the parent directory is fsynced on first
`Open` of a not-yet-existing WAL path; existing WAL tests still pass.

### F5 — A failed Commit can be durably committed (Atomicity, minor)

**Evidence.** `Tx.Commit` fsyncs the WAL, then applies ops to memory; an apply
error mid-loop returns an error to the caller even though every op is already
durable in the WAL (`store/txn/txn.go:457-463`). The only reachable apply error
today is `adjlist.ErrShardFull`, and only when `MaxShardCapacity` is set.

**Why it is a wart.** The caller sees a non-nil error from `Commit` and may
assume the transaction did not happen, but on the next recovery the durable WAL
replays all ops — the in-memory and durable views disagree until restart.

**Fix.** Resolved as a by-product of F1 (atomic apply once the transaction is
known-durable) plus validating apply-ability before `Sync` where feasible, or
documenting the post-durability apply error as "committed; in-memory catches up
on next recovery" and surfacing it as a distinct sentinel.

## Remediation order

1. **F2** (self-contained, highest data-loss risk) — checkpoint full snapshot.
2. **F1** (atomic WAL commit) — the central atomicity guarantee.
3. **F4** (WAL parent-dir fsync) — small, completes durability of creation.
4. **F3** (transaction-atomic visibility) — the deepest change; isolation.
5. **F5** (folded into F1).

Each item follows Specify → Implement → Test → Document and is tracked as an
atomic task in the `gograph` roadmap (`rmp`).
