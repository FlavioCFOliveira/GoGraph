# SimDisk crash-model fidelity audit

**Date:** 2026-08-18
**Subject:** `internal/sim/disk.go` — the deterministic simulated filesystem behind the DST harness
**Audited revision:** SHA-256 `ede0dabb21963a67cde32c70959de84a4a475edc0c02f5dcc3fd4a43c188626c`, 1583 lines, mtime `2026-08-18 04:55:58`
**Task:** rmp #2514 (audit half)
**Method:** read-only. No file under `internal/sim/` was modified.

---

## 1. Scope and method

The audit covers every `SimDisk` and `SimFileHandle` operation **except** the already-identified
`Rename` double-loss defect, which was excluded by the task brief and which landed during this audit
(the `renameUndos` / `undoRenameLocked` / `ArmRenameRollbackForPath` machinery, `disk.go:584-692`,
`1215-1319`). That fix is neither re-reported nor re-audited here.

Findings are classified as the brief requires:

- **Class 1 — impossible loss.** A crash outcome that loses an effect no real filesystem would lose.
  Dangerous because it makes the simulator accuse the engine of a defect it does not have, and
  because it can render a real recovery branch unreachable.
- **Class 2 — impossible survival.** An operation modelled as *more* durable than a real filesystem
  guarantees. Dangerous because the engine passes on a durability the harness invented.
- **Class 3 — unreachable-by-construction.** A legal filesystem state the model can never produce,
  so engine code that exists to handle it is never exercised.

### 1.1 Evidence discipline

Every behavioural claim about the model is **measured**, not inferred. `disk.go` and `seed.go` were
copied byte-identically (verified by SHA-256) into an isolated scratch module and driven by 30
purpose-built probes. The model compiles and runs standalone — it has no other in-package
dependency than `Seed` — so the probes exercise exactly the shipping code.

The audit was begun against an earlier revision of the file. When the concurrent rename work landed
mid-audit, **every probe was re-run against the revision named above and all results were
identical**; no finding below depends on the pre-fix code.

Filesystem behaviour is cited to POSIX (Open Group Base Specifications Issue 7 / Issue 8), the Linux
man-pages 6.18, the ALICE and fsync-failure literature, and to reference implementations read at a
pinned commit. Three claims I could not source are stated as such in §5.

---

## 2. Verdict summary — ranked by consequence, not by novelty

| # | Finding | Class | Sev | Verdict consequence |
|---|---|---|---|---|
| **F1** | `Crash()` never discards unsynced file data — fsync is not load-bearing | 2 | **Critical** | **BLINDS** the whole durability suite; also **FALSE ACCUSATION** (phantom commit) |
| **F2** | `Remove`/`RemoveAll` model unlink as instantly durable | 2 + 3 | **High** | **BLINDS** — "the deletion did not stick" is unreachable |
| **F3** | `DirSync` can never fail | 3 | **High** | **BLINDS** — 4 staging-fsync error branches unreachable |
| **F4** | `Remove`/`RemoveAll` leave a stale `d.dirs` ghost that destroys a fsync'd subtree | 1 | **Medium** | **FALSE ACCUSATION** — demonstrated, but latent (not reachable via today's publish protocol) |
| **F5** | `isRootLevel` exempts root-level names from the dirent model | 2 | **Medium** | **BLINDS**, bounded to WAL-only mode |
| **F6** | A failed `Sync` retains the data, and a retry succeeds | 2 + 3 | **Medium** | **BLINDS** any future fsync-retry logic |
| **F7** | No partial write; a truncated trailing frame is unreachable | 3 | **Medium** | **BLINDS** — the WAL's torn-tail path is not reachable from the model |
| **F8** | `O_TRUNC` / `Truncate` / `TruncatePath` are instantly durable | 2 | **Medium** | **BLINDS** — stale-content-after-truncate unreachable |
| **F9** | `MkdirAll` is a no-op: implicit directory durability, no empty directories | 2 + 3 | **Medium** | **BLINDS** + unreachable partial-publish states |
| **F10** | A `SimFileHandle` stays live across `Crash()` and its writes vanish silently | — | **Low** | **FALSE ACCUSATION** (harness trap) — **FIXED 2026-08-25, rmp #2544** |
| **F11** | `Remove` on a directory or an absent path always succeeds | 3 | **Low** | Narrow |
| **F12** | `OpenFile` ignores `O_EXCL` and the access mode | 3 | **Low** | Latent; no current call site |
| **F13** | `TruncatePath` does not clear sector fault marks; handle `Truncate` does | — | Info | Inconsistency to settle deliberately |
| **F14** | No `ReadDir`; no fsync/fdatasync distinction | 3 | Info | Bounded |
| **F15** | `h.closed` read/written without the disk lock | — | Info | Latent, not demonstrated |

**One finding dominates.** F1 alone means the DST harness cannot currently fail an engine that
stopped calling `fsync`. Everything else is secondary to fixing it.

---

## 3. Findings

### F1 — `Crash()` never discards unsynced file data (Class 2, **Critical**)

**Location.** `disk.go:1185-1203` (`Crash`), `disk.go:317-321` (`simFile`), `disk.go:1427-1464`
(`Write`), `disk.go:1536-1553` (`Sync`), `disk.go:1578-1599` (`syncResultLocked`).

**Modelled outcome.** `simFile` holds exactly three fields — `faulted`, `data`, `direntDurable`.
There is no durable-data shadow and no durable-length watermark. `Write` mutates `f.data` in place;
`Sync` computes a fault outcome and touches no data at all; `Crash()` mutates only the *name* maps
(`d.files` keys and `d.dirs`). Consequently **every byte ever written survives every crash,
regardless of whether it was ever fsync'd.**

Measured (probe `Q3`): 100 frames, 1600 bytes appended, **`SyncCount() == 0`**, one `ParentDirSync`
for the name only, then `Crash()` — **1600 of 1600 bytes survive.** Probe `P1b`, WAL-shaped: frames
`B` and `C` written after the last `Sync` survive intact.

**Real-filesystem outcome.** A successful `write()` carries no durability whatever. Linux
`write(2)` NOTES: *"A successful return from write() does not make any guarantee that data has been
committed to disk… The only way to be sure is to call fsync(2)."* POSIX defines durability solely
through *Successfully Transferred* (XBD §3.377 / I8 §3.360): *"when the system ensures that all data
written is readable on any subsequent open of the file (even one that follows a system or power
failure)"* — reached only via `fsync`/`fdatasync`/`O_SYNC`/`O_DSYNC`. POSIX's own *System Crash*
definition (XBD §3.393 / I8 §3.377) declares post-crash file state **undefined** *"except as required
elsewhere"* — that is, except where a synchronized-I/O completion was actually performed.

**The defect is an internal contradiction, not merely an omission.** The model applies **host-crash**
semantics to metadata (a rename or create whose parent was never fsync'd is revoked) and
**process-crash** semantics to data (every buffered byte survives). No real event has that shape.
RocksDB's filesystem abstraction names exactly these two levels and keeps them apart —
`include/rocksdb/file_system.h:1297-1305`, `FSWritableFile::Flush()`: *"After this call, the data
should survive a **process crash** but is not necessarily persisted to stable storage. Use Sync() for
that guarantee"*; and `:1307-1314`, `Sync()`: *"Persist data to stable storage. After this call, the
data should survive **power failures**."* `SimDisk.Crash()` currently grants `Flush()`'s guarantee to
data and `Sync()`'s guarantee to nothing, while revoking names as if power had been lost.

**Verdict consequence — this is the priority item.**

1. **BLINDS the durability suite.** Every DST assertion of the form "the commit was acked, therefore
   the bytes are recovered after `Crash()`" is satisfied by the byte image *irrespective of any
   fsync*. Deleting the WAL commit fsync, the snapshot component fsyncs, or the csrfile fsync — or
   reordering any of them after the publish — would not fail a single `SimDisk` crash scenario.
   Given that durability is the property the DST exists to certify, this is the "passes for the wrong
   reason" failure mode at the scale of the whole harness.

2. **FALSE ACCUSATION — phantom commits.** The converse bites in concurrent scenarios. The WAL
   leader flushes its buffer under `w.mu` and then fsyncs with `w.mu` **released**
   (`store/wal/writer.go:757+`, `leadGroupSyncLocked`). A crash landing in that window leaves the
   committer's frames *and its `OpCommit` marker* in the durable image although the committer was
   never acked. Recovery replays them, and the oracle sees a transaction nobody acknowledged.
   Measured (probe `S1`), using the harness's own `ArmSyncGateAt` to park a committer inside its
   fsync and crashing while it is parked:

   ```
   S1 gate fired=true  committer's Sync returned=<nil> (never acked)
   S1 durable image after crash = "TXN-1;COMMIT-1;TXN-2;COMMIT-2;"
   S1 unacked TXN-2 bytes present in the durable image: true
   ```

   This is precisely the shape `store/wal/writer.go:686-712` records as having been charged to the
   engine at three seeds under rmp #2322.

**Honest mitigations — why the harness has not obviously broken yet.** The WAL writes through a
64 KiB `bufio.Writer`, and `leadGroupSyncLocked` flushes immediately before its fsync, so in the
*serial* case the written-but-unsynced window is usually empty. `poison()`
(`store/wal/writer.go:865-894`) truncates the file back to `w.durableSize` on a flush/fsync failure,
and the snapshot and csrfile writers `RemoveAll` their staging directory on error — so the
**failure** paths self-compensate at the engine layer. None of that helps the **crash** path, and
none of it is a property of the disk model. Note also that `bufio` auto-flushes when the 64 KiB
buffer fills mid-transaction, so a large transaction reaches the disk model well before any sync.

**Severity: Critical.**

**Recommended fix.** Give `simFile` a durable shadow and split the crash primitive:

- add `durableData []byte` (or, cheaper for the append-only WAL, `durableLen int64` plus a
  copy-on-shrink for non-append writes), updated **only** by a `Sync` that returns `nil`;
- `CrashHost()` sets `f.data = f.durableData` and applies the existing dirent revocation — the true
  power-failure model;
- `CrashProcess()` keeps `f.data` and skips dirent revocation — the true `kill -9` model, which is
  what `SimStore.Crash()`'s own godoc claims to provide;
- keep `Crash()` as an alias for `CrashHost()` so existing scenarios get the stronger model, and
  expect a wave of newly-failing scenarios: those are the ones that were passing for the wrong
  reason.

A non-vacuity guard belongs with the fix: delete the WAL commit fsync and assert that a crash
scenario now fails. Per the project's own discipline, an oracle that cannot fail proves nothing.

---

### F2 — `Remove` / `RemoveAll` model unlink as instantly durable (Class 2 + 3, **High**)

**Location.** `disk.go:994-1003` (`Remove`), `disk.go:1008-1016` (`RemoveAll`), `disk.go:977-985`
(`removeSubtreeLocked`).

**Modelled outcome.** The key is deleted from `d.files` immediately and unconditionally. `Crash()`
can never bring it back. Measured (`P3`): a file created, synced, and `DirSync`'d, then `Remove`d
with **no** subsequent parent-directory fsync, is absent after `Crash()` — `present=false`. Same for
`RemoveAll` of a subtree (`P3b`).

**Real-filesystem outcome.** `unlink(2)` and `rmdir(2)` mutate a directory entry. They are in
exactly the same durability class as the create and the rename the model already treats carefully:
not on stable storage until the containing directory is fsync'd. A crash inside the writeback window
legally leaves **the file still present**.

Sourcing, stated precisely: **neither POSIX nor the Linux man-pages address removal durability at
all** — verified by count across POSIX I7/I8 `unlink` and `rmdir` and Linux `unlink(2)`/`rmdir(2)`:
`fsync`, `durab*`, `crash`, `stable storage`, `sync` are **zero occurrences in every one of them**
(with control terms confirming the documents extracted correctly). The applicable rule is the
general one in Linux `fsync(2)`: *"Calling fsync() does not necessarily ensure that the entry in the
directory containing the file has also reached disk. For that an explicit fsync() on a file
descriptor for the directory is also needed."* POSIX Issue 8 strengthens this materially with a
paragraph absent from Issue 7 (XBD §3.368): *"an operation that modifies a directory is considered to
be a write operation, and a directory's entries are considered to be the data read or written"* —
bringing directory-entry changes, removals included, formally inside synchronized I/O.

Two reference implementations treat deletion as a durability event outright:

- **RocksDB** enumerates the reasons to fsync a directory and gives deletion its own:
  `DirFsyncOptions::FsyncReason { kNewFileSynced, kFileRenamed, kDirRenamed, kFileDeleted, kDefault }`
  (`include/rocksdb/file_system.h:165-182`), and `PosixDirectory::FsyncWithDirOptions`
  (`env/io_posix.cc:2107-2140`) performs a real directory fsync for `kFileDeleted` even on btrfs,
  where it elides the fsync for `kNewFileSynced`.
- **SQLite** makes a *deletion* the commit point itself — `atomiccommit.html` §3.11: *"the rollback
  journal file is deleted. **This is the instant where the transaction commits.**"* — and §9.3 flags
  the assumption as load-bearing: *"SQLite assumes that file deletion is an atomic operation from the
  point of view of a user process… Transactions may not be atomic on systems that do not work this
  way."*

ALICE models this at micro-op granularity too: `unlink` decomposes into `delete dir entry + truncate
if last link`, i.e. removals are **not** atomic in the default persistence model
(§3.2.2, Table 2(a)); and directory-operation atomicity fails outright on ext2, ext2-sync and
reiserfs-nolog (Table 1).

**Verdict consequence — BLINDS.** Every "the removal did not stick" state is unreachable, which
matters because the publish protocol is built out of removals:

- `store/snapshot/writer.go:600` `RemoveAll(tmp)`, `:695` `RemoveAll(bak)` (stale-backup cleanup),
  `:722` `RemoveAll(bak)` (happy-path cleanup);
- `store/wal/writer.go` `Remove(tmpPath)` after the prefix-truncate rename;
- `store/recovery/recovery.go:1046` `RemoveAll(snapDir + ".tmp")` — the stale-staging cleanup that
  exists *precisely* to tolerate a `.tmp` left behind by a previous crash.

The engine's tolerance of a leftover `.tmp` or `.bak` is therefore never exercised **from the
removal side**. The model can only reach that state by crashing before the removal, never by the
removal failing to reach disk — and the latter is the case a real deployment will hit.

**Severity: High.**

**Recommended fix.** Model removal symmetrically with creation, reusing the machinery the rename fix
just introduced: record a pending unlink (path + `*simFile` + prior `direntDurable`), have
`DirSync`/`ParentDirSync` of the parent retire it, and have `Crash()` restore any pending unlink it
selects — exactly the nondeterministic-suffix rule `rollbackRenamesLocked` already implements. Add
`ArmRemoveWritebackForPath` / `ArmRemoveRollbackForPath` so a scenario can pin either branch, and a
`RemoveRollbackCount` reachability observable to match the rename arms.

---

### F3 — `DirSync` can never fail (Class 3, **High**)

**Location.** `disk.go:1085-1104` (`DirSync`) — the function has no fault arm and no error return
path; it ends `return nil` unconditionally. Contrast `ParentDirSync` (`disk.go:1138-1148`), which
*does* consult `ArmParentDirSyncFaultForPath`.

**Measured (`P11`).** With `ArmSyncFaultAt(1)` and `ArmParentDirSyncFaultForPath("dir/f")` both
armed, `DirSync("dir")` returns `nil`.

**Real-filesystem outcome.** `fsync` on a directory descriptor is an ordinary `fsync` and can fail —
Linux `fsync(2)` ERRORS lists `EIO`, `ENOSPC`, `EDQUOT`, `EBADF`, `EINVAL`, and POSIX states that on
failure *"outstanding I/O operations are not guaranteed to have been completed."*

**Verdict consequence — BLINDS.** Four error branches in the snapshot writer are unreachable under
simulation:

| Call site | Branch that cannot be reached |
|---|---|
| `store/snapshot/writer.go:677` | staging-dir fsync fails → `RemoveAll(tmp)` + abort before publish |
| `store/snapshot/indexes.go:156` | `indexes/` dir fsync fails |
| `store/snapshot/full.go:813` | staging-dir fsync fails (full-snapshot path) |
| `store/snapshot/full.go:913` | `indexes/` dir fsync fails (full-snapshot path) |

The `writer.go:677` branch is the important one: it is the last durability gate before the archive
and publish renames. A regression that swallowed its error — publishing a snapshot whose staging
directory was never durable — would be invisible to the DST.

**Severity: High.** Cheap to fix and it closes a gate on the publish protocol's critical path.

**Recommended fix.** Add `ArmDirSyncFaultForPath(dir string)` and a `DirSyncFaultCount()`
reachability observable, mirroring `ArmParentDirSyncFaultForPath` exactly. Route `ParentDirSync`'s
existing arm through the same check so the two cannot drift.

---

### F4 — `Remove`/`RemoveAll` leave a stale `d.dirs` ghost that destroys a fully-fsync'd subtree (Class 1, **Medium**, latent)

**Location.** `removeSubtreeLocked` (`disk.go:977-985`) deletes only from `d.files`. `Remove`
(`:1001`) and `RemoveAll` (`:1014`) call it without touching `d.dirs`. `Crash()` (`:1191-1196`) then
honours the orphaned `d.dirs[path] == false` and deletes the subtree that has since been recreated
there.

**Measured — controlled experiment (`R1`).** Three arms, identical file and identical fsyncs; the
only variable is a prior `Rename`+`RemoveAll` at the same path:

```
R1 grandparent NOT fsync'd                    -> fully-fsync'd dir/live/x survives: false
R1 ParentDirSync(dir/live) [=DirSync(dir)]    -> fully-fsync'd dir/live/x survives: true
R1 no prior rename                            -> survives: true  (CONTROL, no ghost)
```

The destroyed file had been written, `Sync`'d, and `DirSync("dir/live")`'d. Nothing about it was
non-durable. It was deleted solely because a *previous, unrelated* directory that once occupied that
path left `d.dirs["dir/live"] = false` behind, and only an fsync of the **grandparent** clears it —
a call the creating code has no reason to make when it builds a directory in place rather than by
rename.

**Real-filesystem outcome.** No filesystem loses a file whose data and whose directory entry are both
on stable storage because a differently-named predecessor was once deleted at that path.

**Verdict consequence — FALSE ACCUSATION, but latent.** This is the same class as the rename defect
that prompted this audit: the simulator manufactures a durable-data loss and the oracle charges it to
the engine.

**I could not demonstrate it reachable through today's publish protocol, and say so plainly.**
Probe `R2` replays `store/snapshot/writer.go`'s exact sequence twice
(`RemoveAll(tmp)` → write+fsync → `DirSync(tmp)` → `RemoveAll(bak)` → `Rename(dir,bak)` →
`Rename(tmp,dir)` → `ParentDirSync(dir)` → `RemoveAll(bak)`) and a following `Crash()` is a no-op:

```
R2 before=[cp/snapshot/manifest.json] after=[cp/snapshot/manifest.json] (want: unchanged)
```

The reason is that `<dir>/snapshot` is only ever recreated **by rename**, and `Rename` re-registers
`d.dirs[newPath]` fresh each time, overwriting the ghost. The defect therefore requires a path that
is (a) once a rename destination, (b) later removed, and (c) later recreated by direct file creation.
No current caller does that.

It nonetheless warrants fixing rather than documenting, for two reasons. First, its unreachability is
accidental, not designed — one new caller that builds a directory in place at a previously-renamed
path detonates it, and the failure presents as a durability violation in the engine. Second, the
gap is now demonstrably known-adjacent: `undoRenameLocked` (`disk.go:1256-1260`) has to
**hand-delete** `d.dirs` entries precisely because `removeSubtreeLocked` does not, yet the two older
callers were not updated.

**Severity: Medium** (Critical impact, currently unreachable).

**Recommended fix.** One line of intent: make `removeSubtreeLocked` delete the matching `d.dirs`
entries for `path` and everything under `path + "/"`, and drop the now-redundant hand-deletion in
`undoRenameLocked`. That makes the invariant "`d.dirs` never outlives its subtree" hold by
construction at all three call sites.

---

### F5 — `isRootLevel` exempts root-level names from the dirent model (Class 2, **Medium**)

**Location.** `disk.go:1329-1332` (`isRootLevel`); applied at `disk.go:551` (`OpenFile` create) and
`disk.go:599` (`Rename`).

**Measured (`P14`).**

```
P14 root-level 'wal' after crash with NO dir fsync ever = "ROOT-WAL"
P14 root-level rename with NO ParentDirSync survives    = "NEW"
P14 below-root rename with NO ParentDirSync             = keys []
```

A root-level file is durably linked on creation *and* on rename, with no directory fsync ever.

**Real-filesystem outcome.** The filesystem root is not special. A file created at `/wal` needs an
fsync of `/` exactly as `dir/wal` needs an fsync of `dir`. Linux `fsync(2)`'s directory-entry
paragraph draws no distinction by depth.

**Verdict consequence — BLINDS, and the blast radius is bounded.** Both modes matter:

- **WAL-only mode** (`cfg.dir == ""` → `simWALPath = "wal"`, `internal/sim/simstore.go:25,40-45`) —
  fully exempt. In this mode `wal.OpenFS`'s unconditional `fsys.ParentDirSync(path)` is inert.
- **Full-stack mode** (`cfg.dir != ""` → `dir/wal`) — **governed by the dirent model**, so the WAL
  dirent fsync *is* load-bearing there.

That second half is the mitigation, and it is a real one: `store/wal/writer_vfs.go` documents the
`ParentDirSync` in `OpenFS` as *"the load-bearing fix for a WAL that lives below the filesystem root
(dir/wal in the full-stack layout): without it a crash before the first directory fsync could drop
the newly-linked WAL dirent and lose every committed frame."* Full-stack scenarios do cover it. The
residual exposure is that WAL-only scenarios — the legacy `recovery.ReplayWAL` path — silently
provide a durability guarantee that production does not, so any defect they alone would catch is
missed.

**Severity: Medium.** The exemption is deliberate and documented; it is the *silence* that is the
defect, not the simplification.

**Recommended fix.** Preferred: retire the exemption and give the WAL-only layout a root directory
that must be fsync'd like any other, accepting the scenario churn. If it is retained, make it
explicit and observable — a `RootLevelExempt(path) bool` accessor plus an assertion in the WAL-only
scenarios that they are knowingly running with a strengthened dirent model, so the gap cannot be
mistaken for coverage.

---

### F6 — A failed `Sync` retains the data, and a retry succeeds (Class 2 + 3, **Medium**)

**Location.** `disk.go:1536-1553` (`Sync`), `disk.go:1578-1599` (`syncResultLocked`). The fault is
one-shot; the data is never touched on either path.

**Measured (`P2`).**

```
P2 sync1=sim: injected disk fault  sync2=<nil>  image-after-crash="BEFORE;AFTER-FAILED-FSYNC;"
```

The failed fsync leaves the bytes present and crash-durable, and the immediate retry reports success.

**Real-filesystem outcome — the opposite, in both halves.** Rebello et al., ATC '20 §3.3.4: *"All the
file systems mark the page clean even after `fsync` fails… none of them retry data or journal-block
writes."* The pages stay resident but clean, so the data is never written back; on ext4 (both modes)
and XFS the cache holds the **new** data while the disk holds the **old**, so a later read silently
flips to stale content once the page is evicted (§3.3.1-§3.3.2). And the retry: §2, *"when PostgreSQL
retried the `fsync` a second time, there were no dirty pages for the file system to write, resulting
in the second `fsync` succeeding without actually writing data to disk… PostgreSQL had been using
`fsync` incorrectly for 20 years."* PostgreSQL's fix is in the tree today —
`src/backend/storage/file/fd.c:3980-4001` (`REL_18_0`), `data_sync_elevel()`: *"A later attempt to
fsync again might falsely report success. Therefore we must not allow any further checkpoints to be
attempted"* — PANIC by default, with `data_sync_retry` (default `false`) as the only escape.

**Verdict consequence — BLINDS.** No current caller retries — the WAL poisons fail-stop, and
RocksDB's equivalent is structural (`file/writable_file_writer.cc:468-514`: `seen_error()`
short-circuits every subsequent call on the writer). So there is no live defect. What the model
cannot express is the *consequence of introducing one*: a future "retry the fsync once before
poisoning" optimisation would look perfectly safe under DST, because in the model the retry both
succeeds and the data really is durable. That is the exact 20-year bug, re-armed.

A second, smaller gap: `ErrSimFault` is a bare sentinel, so the model cannot distinguish the
delayed-reporting case ATC '20 documents for ext4 data mode, where a data-block fault causes the
**second** fsync to fail rather than the first (§3.3.1, Table 1 Q6).

**Severity: Medium.**

**Recommended fix.** Couple the sync outcome to the durable shadow introduced for F1: on a failed
`Sync`, do **not** advance `durableLen`, and mark the file so that any later `Sync` returns success
without advancing it either — reproducing "the page was marked clean, the retry is a no-op, the data
is gone". That single change turns F6 from unreachable into a first-class, cheap-to-trigger scenario.

---

### F7 — No partial write; a truncated trailing frame is unreachable (Class 3, **Medium**)

**Location.** `disk.go:1440-1442` (`Write` returns `(0, ENOSPC)` and grows nothing);
`disk.go:1185-1203` (`Crash` never shortens a file); `disk.go:1468-1473` (`corruptSector` flips a
byte, never truncates).

**Measured.** `P9`: `n=0 err=…ENOSPC is-ENOSPC=true file-len=0` — all-or-nothing. `P16`: with
`faultRate=1.0`, WAL length before crash 320, after crash 320 — the fault injector never shortens a
file.

**Real-filesystem outcome.** POSIX `write()` DESCRIPTION is explicit, with a worked example: *"If a
write() requests that more bytes be written than there is room for… only as many bytes as there is
room for shall be written. For example, suppose there is space for 20 bytes more in a file before
reaching a limit. A write of 512 bytes will return 20. The next write of a non-zero number of bytes
would give a failure return."* Linux `write(2)` RETURN VALUE agrees: *"partial writes can occur… because
there was insufficient space on the disk device to write all of the requested bytes."* So ENOSPC
arrives **after** a short count, not instead of it.

More decisively, ALICE Table 1 has exactly one row marked × in **all sixteen** configurations
studied: *multi-block append/writes are not atomic anywhere*. §2.2.1: *"Current file systems do not
provide atomic multi-block appends."* A model whose appends are all-or-nothing is unfaithful to every
filesystem in that study. ALICE §4.4.2 adds a detail the model would also need: the torn region
contains **garbage**, not zeroes — *"Filling the appended portion with zeros instead of garbage still
causes failure."*

**Verdict consequence — BLINDS.** A truncated trailing WAL frame is the single most important crash
state a write-ahead log must survive, and the model cannot produce one by any route: not by crash,
not by ENOSPC, not by the sector-fault injector (which flips bytes, producing a CRC failure — a
different defect class). The harness compensates by *manufacturing* the tail:
`truncateSimWALAt` (`internal/sim/simstore.go:530-540`) truncates to the offset **recovery itself
reported** (`ReplayResult.WALTailOffset`), which `wal.OpenFS` requires of its caller by contract.
That is a legitimate mechanism for reopening, but it is not an independent test: recovery is being
handed a boundary it computed.

Scope note, stated honestly: torn tails *are* covered elsewhere — `internal/crashinject` and the
`store/wal` unit tests. This is a DST coverage gap, not a project-wide one.

**Severity: Medium.**

**Recommended fix.** Two additions, both cheap: (a) honour the POSIX short-count contract — on
ENOSPC in eager mode, write as many bytes as fit and return that count with `nil`, letting the
caller's next write get `ENOSPC`; (b) add `ArmTornAppendAt(ordinal, keepBytes)` so a crash can leave
a deliberately partial trailing record, filled with garbage rather than zeroes per ALICE §4.4.2.
With F1's durable shadow in place, (b) falls out almost free: a crash truncates to `durableLen` plus
a seed-chosen partial prefix.

---

### F8 — `O_TRUNC` / `Truncate` / `TruncatePath` are instantly durable (Class 2, **Medium**)

**Location.** `disk.go:554-557` (`OpenFile` `O_TRUNC`), `disk.go:1504-1530` (handle `Truncate`),
`disk.go:1056-1077` (`TruncatePath`).

**Measured (`P6`).** A file created, written, `Sync`'d and `DirSync`'d, then reopened `O_TRUNC` and
never fsync'd, is **empty** after `Crash()` — the truncation is permanent.

**Real-filesystem outcome.** The durability of a truncation is unspecified in every primary source —
verified by count across POSIX I7/I8 `ftruncate` and Linux `truncate(2)`: `O_SYNC`, `O_DSYNC`,
`synchronized`, `durab*`, `crash` are **zero in all three**, and POSIX's `open()` O_TRUNC text is
equally silent. The only cross-reference is indirect: Linux `fsync(2)` notes that a changed `st_size`
*"would require a metadata flush"* under `fdatasync`. Since the size change is unspecified until
synced, a crash may legally leave the **old, longer content**. ALICE Table 1 confirms the practical
consequence: *O_TRUNC Append → Any op* re-ordering fails on ext2, ext3-writeback, ext4-writeback,
reiserfs-nolog and reiserfs-writeback.

**Verdict consequence — BLINDS.** Both staging-file creators open `O_CREATE|O_WRONLY|O_TRUNC` —
`store/snapshot/safe_create.go:20` and `store/csrfile/fs.go:67` — as does the WAL suffix temp
(`store/wal/writer.go:1235`). The state "the `.tmp` I truncated still holds a longer previous
generation's bytes, and my new shorter content sits in front of it" is unreachable. Any reader that
trusts a length header over the real file length, or that fails to bound a read by the file size,
would not be caught here. `poison()`'s `Truncate(durableSize)` is likewise modelled as instantly
effective, which is what makes the F6 compensation look stronger than it is.

**Severity: Medium.**

**Recommended fix.** Fold the truncation into F1's durable shadow: a truncation shortens `data`
immediately (visibility) but only shortens `durableData`/`durableLen` at the next successful `Sync`,
so `Crash()` restores the longer prior image. That single mechanism covers `O_TRUNC`, `Truncate` and
`TruncatePath` uniformly.

---

### F9 — `MkdirAll` is a no-op: implicit directory durability, no empty directories (Class 2 + 3, **Medium**)

**Location.** `disk.go:990` (`MkdirAll` — `return nil`), `disk.go:1023-1036` (`Stat`, which infers a
directory purely from a key prefix), `disk.go:1355-1360` (`Exists`, which never sees directories).

**Measured.**

```
P8 keys after crash (parent of dir/sub never fsync'd) = [dir/sub/f]
P5 Stat(mkdir'd empty dir) err = stat dir/empty: file does not exist
P5 Stat(dir after removing its only file) err = stat dir/e2: file does not exist
P5 Exists(dir/e2) = false (Exists never sees directories)
```

Two distinct defects. **(a)** A directory created by `MkdirAll` never enters `d.dirs`, so it is
implicitly durable: its children survive a crash even though the new directory's own entry in its
parent was never fsync'd — Class 2. **(b)** A directory with no files is indistinguishable from a
missing one — Class 3.

**Real-filesystem outcome.** `mkdir(2)` creates a directory entry in the parent, subject to the same
`fsync(2)` directory-entry rule as any other name. And an empty directory is an entirely ordinary
crash state: crash after `mkdir` + parent fsync but before the first component is written, or after
the contents are removed but before the `rmdir`. ALICE §4.4.4 found that *six* of the seven
applications with durability loss failed specifically because they did not fsync directories.

**Verdict consequence — BLINDS, with limited current exposure.** The unreachable state is "the
snapshot directory exists but is empty or incomplete". Today's recovery probe is
`Stat(filepath.Join(snapDir, "manifest.json"))` (`store/recovery/recovery.go:1047`), which keys on the
manifest rather than the directory and is therefore robust to the gap — so I am **not** claiming a
live defect. The exposure is prospective: any future check of the form "does the snapshot directory
exist?" would be untestable, and would behave differently in production. `Exists()` never returning
true for a directory is a related sharp edge worth a godoc note at minimum.

**Severity: Medium.**

**Recommended fix.** Make `MkdirAll` register the created directory in `d.dirs` with
`direntDurable = isRootLevel(dir)` (or unconditionally `false` once F5 is settled), so a new
directory needs a parent fsync like every other name; and give directories a first-class presence in
the key space so an empty one is representable and `Stat`/`Exists` can report it.

---

### F10 — A `SimFileHandle` stays live across `Crash()`, and its writes vanish silently (**Low**)

**Location.** `disk.go:1185-1203` (`Crash` drops map entries but leaves every `*simFile` reachable
from any open handle), `disk.go:1427-1464` (`Write` holds `h.file` directly).

**Measured (`P10`).** After `Crash()` dropped the file, a write through the surviving handle returns
`n=16, err=<nil>` and `Sync()` returns `nil`, while the path is absent from the durable image.

**Real-filesystem outcome.** None — after a host crash the process is dead. This is a harness
fidelity issue, not a filesystem one.

**Verdict consequence — FALSE ACCUSATION (harness trap).** A scenario that crashes while holding its
own handle continues writing into an orphaned `simFile`. The bytes are silently discarded and the
symptom presents as missing data attributable to the engine. `SimStore.Crash()`
(`internal/sim/simstore.go:616-624`) drops `engine`/`store`/`wlog`/`graph`, so the main path is safe;
the exposure is a scenario that opened a handle itself.

**Severity: Low.**

**Recommended fix.** Set a `crashed` generation counter on the disk in `Crash()`, stamp it on each
handle at open, and have every handle method return `fs.ErrClosed` (or a dedicated `ErrCrashedDisk`)
when the stamps differ. Fail-stop beats silent discard.

**FIXED 2026-08-25 (rmp #2544), as recommended.** `SimDisk.crashGen` is bumped by **both** crash
kinds and stamped onto each handle at open; every handle method that touches the file returns
`ErrCrashedDisk` when the stamps differ. Three details the recommendation did not settle:

- **The counter is separate from `hostCrashGen`.** That one models in-flight fsync invalidation and
  is deliberately *not* bumped by `CrashProcess`, because an fsync already handed to the kernel
  completes even though the process that issued it is gone. A **file descriptor** does not survive
  its process, so a handle must die on a `CrashProcess` too. Reusing the existing counter would have
  left the SIGKILL case silently discarding exactly as before.
- **`ErrCrashedDisk` wraps `fs.ErrClosed`**, so code that already treats a closed handle as terminal
  keeps working unchanged, while `errors.Is` against the sentinel still tells a scenario author which
  of the two happened.
- **`Close` is deliberately not gated.** Every caller closes with `defer`; returning the error there
  would turn one fail-stop into a second, spurious failure on a path that is only cleaning up.

**No scenario legitimately held a handle across a crash**, which was checked by running rather than
by reading: the whole `internal/sim` package passes with the guard in place (rc=0, zero failures,
91.3 s). Pinned by `TestHandleDoesNotSurviveACrash` — which drives all three crash entry points and
fails with the literal `(5, nil)` silent discard when the stamp check is removed — and by
`TestHandleOpenedAfterACrashIsLive`, which stops the guard from being satisfied by a disk that simply
refuses everything after its first crash.

---

### F11 — `Remove` on a directory or an absent path always succeeds (Class 3, **Low**)

**Location.** `disk.go:994-1003`.

**Measured (`P13`).** `Remove("dir/sub")` where `dir/sub/f` exists returns `nil` and deletes
**nothing** (`keys=[dir/sub/f]`). `Remove("dir/absent")` returns `nil`.

**Real-filesystem outcome.** `os.Remove` on a non-empty directory returns `ENOTEMPTY`; on an absent
path, `ENOENT`. POSIX `unlink()` on a directory returns `EPERM`/`EISDIR`.

**Verdict consequence — narrow.** The tolerant absent-path behaviour is deliberate and documented
(the snapshot writer relies on it), and every current `Remove` caller treats the error as
best-effort. The genuinely risky half is the **silent no-op on a directory**: a caller that means to
delete a directory gets success and no deletion, and the model would never reveal the mistake.

**Severity: Low.** Recommended fix: return `fs.ErrInvalid`/`syscall.EISDIR` when `path` has children,
keeping the absent-path tolerance as documented.

---

### F12 — `OpenFile` ignores `O_EXCL` and the access mode (Class 3, **Low**)

**Location.** `disk.go:535-563` — only `O_CREATE`, `O_TRUNC` and `O_APPEND` are consulted.

**Measured (`P7`).** `O_CREATE|O_EXCL` on an existing file returns `err=<nil>`; a handle opened
`O_RDONLY` accepts a 20-byte write with `err=<nil>`.

**Real-filesystem outcome.** POSIX `open()`: *"If O_CREAT and O_EXCL are set, open() shall fail if the
file exists"* → `EEXIST`. A write to a read-only descriptor returns `EBADF`.

**Verdict consequence — none today, latent tomorrow.** The only `O_EXCL` in `store/` is
`wal/lock_other.go:26`, the non-Unix lock fallback, which is not routed through `SimDisk`
(`wal.OpenFS` deliberately acquires no lock). So there is no current exposure. It is recorded because
`O_EXCL` is the natural primitive for a "did a previous crash leave this behind?" guard, and such a
guard would be silently inert under simulation.

**Severity: Low.** Recommended fix: honour `O_EXCL` (return `fs.ErrExist`) and reject writes on
`O_RDONLY` handles.

---

### F13 — `TruncatePath` does not clear sector fault marks; handle `Truncate` does (Informational)

`disk.go:1056-1077` shrinks `f.data` without touching `f.faulted`, whereas `disk.go:1504-1530`
deletes the marks above the new size. Verified: after `Truncate(0)` + `Seek(0)` + rewrite on a
zero-fault disk the content is pristine (`CLEANAGAIN`), confirming the handle path clears correctly;
`TruncatePath` leaves the marks in place, so a later write into a reclaimed sector is corrupted.
Neither behaviour is wrong in the abstract — a genuinely bad sector *does* persist across a truncate
— but the two paths disagree, and `simCSRFS.Truncate` (`internal/sim/diskfs.go:66`) routes the csrfile
writer through the non-clearing one. Settle it deliberately in one direction and document the choice.

### F14 — No `ReadDir`; no fsync/fdatasync distinction (Class 3, Informational)

Enumerated every method on `SimDisk`/`SimFileHandle`: there is **no** `ReadDir`, `Lstat`, `Symlink`,
`Link`, `ReadAt` or `WriteAt`. No engine path that enumerates a directory can be simulated at all; the
seams in scope do not currently need one, so this bounds future work rather than describing a defect.
`Sync` models `fsync` and `fdatasync` identically, which is defensible: Linux `fsync(2)` states that
a changed `st_size` *"would require a metadata flush"* under `fdatasync`, so for an append-only WAL
the two coincide — consistent with the `#1510` decision to use `fdatasync` per commit.

### F15 — `h.closed` is read and written without the disk lock (Informational)

`Read`/`Write`/`Seek`/`Truncate`/`Sync`/`Stat` all test `h.closed` **before** acquiring `disk.mu`
(`disk.go:1398, 1415, 1428, 1479, 1505, 1537`), while `Close` (`disk.go:1602-1605`) writes it under no
lock. The documented contract is one goroutine per handle, and the WAL flushes under its own mutex,
so I could **not** demonstrate an actual race — recorded as a latent hazard only, given that
`leadGroupSyncLocked` deliberately releases `w.mu` across `f.Sync()`.

---

## 4. What I checked and found CLEAN

A list of problems alone does not describe the audit's coverage. The following were examined and are
**faithful, or correct within their documented remit**. Each was measured, not eyeballed.

**Handle semantics**
- `Seek` — all three whence values correct (`start=0, end=10, end-3=7`); negative result and invalid
  whence both rejected. Matches POSIX `lseek`.
- **Sparse extension** — seeking past EOF and writing zero-fills the hole
  (`"0123456789\x00\x00\x00\x00\x00X"`), matching POSIX's zero-fill requirement for extension.
- `Read` — returns `(0, io.EOF)` at and past EOF; correct `io.Reader` contract.
- `Close` — idempotent (second `Close` returns `nil`), and **does not sync**. This is exactly right:
  Linux `close(2)` CAVEATS, *"A successful close does not guarantee that the data has been
  successfully saved to disk."* A model whose `Close` flushed would be a Class 2 defect; this one
  does not.
- **Closed-handle contract** — `Read`, `Write`, `Sync`, `Truncate`, `Seek` and `Stat` all return
  `fs.ErrClosed` after `Close`. Uniform, no gaps.
- Handle `Truncate` — shrink drops the tail, extend zero-fills, fault marks above the new size are
  cleared, and the `size == 0` edge case is correctly guarded against the `(0-1)/512 == 0` trap.

**Fault injection and determinism**
- `CorruptRange` — bounds-checked on all four edges (absent path → `fs.ErrNotExist`; offset past EOF,
  negative offset, and in-range all behave correctly), deterministic, and draws nothing from the seed.
  A correct injector for already-durable bytes.
- **Sector fault bitmap** — sectors iterated in ascending index, so the draw order is stable;
  `corruptSector` is deterministic.
- **Capacity does not perturb the fault stream.** Measured: the sync-fault sequence over 12 commits is
  byte-identical with and without a capacity set (`F.FFFFFF...F` in both cases). The purity claim in
  `wouldExceedLocked`'s godoc holds.
- **ENOSPC error shape** — `&os.PathError{Err: syscall.ENOSPC}`, so `errors.Is(err, syscall.ENOSPC)`
  is true, as both the WAL's errno classifier and `internal/testfs` require.
- **Whole-model determinism** — the same seed reproduces the identical fault sequence *and* the
  identical byte image across runs.

**Copy and isolation semantics**
- `ReadFile` and `Snapshot` both return true deep copies — mutating the result never reaches the live
  filesystem. Verified by mutation.
- `Rename`'s undo capture (`captureSubtreeLocked`) correctly captures `d.dirs` alongside `d.files`.

**`SyncGate`**
- `reach`/`Release` are one-shot via `sync.Once`; `Release` is idempotent and safe on a gate that was
  never reached; `Fired()` correctly reports non-firing. The gate is claimed under the lock and
  resolved outside it, so a gated `Sync` genuinely does not wedge the disk — the design is sound and
  the reachability observable is the right one.

**Directory-entry model (the part that works)**
- `DirSync` correctly durabilises both the files whose parent is `dir` and the directories whose
  parent is `dir`, which is what makes `ParentDirSync(live)` durabilise a published snapshot's name.
- The dirent revocation in `Crash()` is correct in its own terms: a non-durable directory takes its
  whole subtree with it, and individual non-durable files are dropped. This is a faithful model of
  Linux `fsync(2)`'s directory-entry caveat, and it is the part of the model that already works.
- `ParentDirSync`'s one-shot fault arm correctly makes **no** dirent durable when it fires.

**Explicitly checked and NOT found defective**
- I looked for, and did **not** find, an impossible-loss defect in `OpenFile`, `Stat`, `ReadFile`,
  `Seek`, `Read`, `CorruptRange`, `SetCapacity`, `ArmSyncFaultAt`, `ArmSyncGateAt`,
  `ArmRenameFaultForPath` or `SyncGate`. **Class 1 yielded exactly one finding (F4), and that one is
  latent.** I have not manufactured others to balance the categories.
- The ENOSPC-at-`Sync` (delayed-allocation) path returns the correct error at the correct moment; its
  only defect is the shared F1 one — measured `Q4`: `sync err=…ENOSPC` yet `bytes after crash=16`.

---

## 5. Evidence and sources

**Specifications**
- The Open Group Base Specifications **Issue 7** (IEEE Std 1003.1-2017) and **Issue 8**
  (IEEE Std 1003.1-2024) — `rename`, `fsync`, `fdatasync`, `unlink`, `rmdir`, `write`, `read`,
  `close`, `open`, `ftruncate`; XBD §3.377/§3.360 *Successfully Transferred*, §3.384-3.385 /
  §3.368-3.369 *Synchronized I/O …Completion*, §3.393/§3.377 *System Crash*, XSH §2.9.7.
  `https://pubs.opengroup.org/onlinepubs/9699919799/`, `…/9799919799/`
- **Linux man-pages 6.18** — `fsync(2)`, `write(2)`, `close(2)`, `open(2)`, `truncate(2)`,
  `rename(2)`, `unlink(2)`, `rmdir(2)`, `sync_file_range(2)`. `https://man7.org/linux/man-pages/man2/`

**Literature**
- Pillai et al., *All File Systems Are Not Created Equal*, **OSDI '14** — §2.1-2.2.2 (persistence
  properties; size- and content-atomicity), **Table 1** (16 configurations), §3.2.2 + Table 2(a)
  (micro-op decomposition of `rename`/`unlink`), §4.3, §4.4.2-4.4.5, Tables 3(b)(c)(d).
- Rebello et al., *Can Applications Recover from fsync Failures?*, **USENIX ATC '20** — §2
  (motivation / PostgreSQL), §3.3.1-3.3.4 + Table 1 (page state after failure; Q3-Q6, Q10-Q11),
  §5 (#7, #8), §7.

**Reference implementations (read at a pinned revision)**
- **PostgreSQL** tag `REL_18_0` — `src/backend/storage/file/fd.c:162, 386, 3600, 3980-4001`
  (`data_sync_elevel`, PANIC-not-retry); `src/backend/utils/misc/guc_tables.c:2091-2098`
  (`data_sync_retry`, `PGC_POSTMASTER`, default false).
- **RocksDB** commit `bbdcd2825fd907c8014ba0995dca58880cb476fe` (v11.9.0) —
  `include/rocksdb/file_system.h:1297-1328` (`Flush` = survives process crash; `Sync` = survives power
  failure), `:165-182` (`DirFsyncOptions::FsyncReason`, incl. `kFileDeleted`), `:1554-1564`
  (`FSDirectory::Fsync`); `env/io_posix.cc:2090-2140`; `file/filename.cc:429-467`;
  `db/flush_job.cc:1120-1124`; `file/writable_file_writer.cc:468-514` (`seen_error_`, no retry).
- **SQLite** `https://sqlite.org/atomiccommit.html` — §2 (hardware assumptions; powersafe overwrite),
  §3.5, §3.11 (journal deletion as the commit point), §4.2, §5.2 (directory sync), §6.1-6.2, §7.4,
  §9.2-9.5.

**This repository** — `internal/sim/disk.go` (revision above); `internal/sim/diskfs.go`,
`simstore.go`; `store/wal/writer.go`, `writer_vfs.go`; `store/snapshot/writer.go`, `fs.go`,
`indexes.go`, `full.go`, `safe_create.go`; `store/csrfile/fs.go`; `store/recovery/recovery.go`,
`fs.go`.

**Probe harness.** 30 probes in an isolated scratch module over a SHA-256-verified copy of
`disk.go` + `seed.go`, referenced above as `P1`-`P16`, `Q1`-`Q4`, `R1`-`R2`, `S1`, `C1`-`C10`. Not
committed; reproducible from the descriptions in each finding.

---

## 6. Unverified items

Stated explicitly rather than glossed, per the evidence standard.

1. **No man-page warning against retrying `fsync` exists.** The Linux `fsync(2)` page in man-pages
   6.18 has **no** NOTES or "Error handling" section, and contains no statement that the error is
   reported once or that a retry is unsafe (verified against both the rendered page and the
   man-pages git source). F6 is therefore grounded in ATC '20 and the PostgreSQL source, **not** in a
   man page. The retry warning that does exist in the man-pages is about `close(2)`, not `fsync`, and
   must not be substituted for one.
2. **POSIX does not address rename durability across a crash.** Verified by count: `crash`,
   `stable storage`, `durab*`, `fsync`, `power fail` are all zero on POSIX I7/I8 `rename` and on
   Linux `rename(2)` (the single `crash` hit is an unrelated NFS remark in BUGS). Rename's atomicity
   is *concurrency* atomicity only. This bounds what may be claimed about the adjacent rename fix.
3. **POSIX does not address removal durability at all** (F2). The finding rests on the general
   `fsync(2)` directory-entry rule, POSIX Issue 8 §3.368's new directory-as-data paragraph, and the
   RocksDB/SQLite implementations — not on a `unlink(2)` durability clause, because none exists.
4. **No sector-atomicity guarantee exists** in POSIX or the Linux man-pages (`sector` and `torn`:
   zero occurrences across 30 documents checked). The model's 512-byte sector granularity is a
   modelling choice with no specification behind it — reasonable, but not normatively grounded.
5. **F4 reachability.** I demonstrated the false accusation but **could not** reach it through the
   current `store/snapshot` publish protocol (probe `R2`). It is ranked Medium on that basis. If a
   caller is later found that recreates a previously-renamed path by direct file creation, it
   becomes High immediately.
6. **F15** is a latent hazard by inspection; I did **not** demonstrate an actual data race, and did
   not run the race detector against `internal/sim` because another agent owns that package's gate
   during this audit.
7. **RocksDB's no-retry rationale is structural, not documented.** The `seen_error_` short-circuit is
   unambiguous in the code, but no comment states "we do not retry a failed fsync" or cites the
   fsyncgate reasoning; `FSWritableFile::Sync()`'s contract is silent on post-error file state.
