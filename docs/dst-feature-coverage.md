# DST Feature Coverage

This document records how the Deterministic Simulation Testing harness
(`internal/sim/`) exercises the GoGraph feature surface, and the coverage work
completed on 2026-07-13 to make the DST drive **every implemented feature**.

The goal: for every implemented GoGraph feature, the DST has a scenario that
drives it during simulation and validates it against an **independent** oracle
or reference — including, wherever applicable, across crash and recovery.

## Method

Three domain audits (Cypher language, graph/search algorithms, storage/
durability) enumerated the implemented feature surface and cross-referenced it
against the scenarios the DST actually ran. Every "implemented but unexercised"
feature became a tracked task. Each new check validates the engine against an
independent computation — never the code under test — and every deterministic
scenario is bit-reproducible from its seed.

Two classes of verification vehicle are used:

- **Oracle-computed checks**: an invariant computed independently from the
  shadow model (`GraphOracle`, or a scenario-private model) is compared to the
  engine's result — the strongest form (catches absolute wrongness).
- **Absolute-literal checks**: a self-contained `RETURN <expr>` whose answer is
  a known constant, compared to the engine's canonical rendering.
- **Independent naive references**: for the search algorithms, a from-scratch
  reference (naive BFS/Bellman-Ford/power-iteration/degree-parity/…) computed on
  a shaped fixture with known ground truth.

## Cypher language coverage

### Mutation clauses (`schema-mutation`, `merge-rel` scenarios)

| Feature | Scenario | Independent check |
|---|---|---|
| `REMOVE n.prop`, `REMOVE n:Label` | schema-mutation | oracle read-back: property reads NULL / label dropped, across crash+checkpoint recovery |
| `SET n:Label`, `SET n += $map`, `SET n = $map` | schema-mutation | oracle labels/properties after each op, across recovery |
| multi-label match `(n:A:B)` | schema-mutation | oracle count of dual-labelled nodes |
| `MERGE (a)-[r]->(b) ON CREATE/ON MATCH SET` | merge-rel | idempotent edge count + `r.n` counter round-trips across recovery |

Map-valued parameters are bound by the harness adapter (`toExprValue`) so
`SET n += $map` / `MERGE (n $map)` can be driven.

### Read clauses, expressions, functions (`cypher-surface` scenario)

`CheckCypherSurfaceExtended` (oracle-computed over the Person/KNOWS graph):
`count(DISTINCT)`; 3VL `AND`/`OR`/`XOR`/`NOT`, `IN`, `IS NULL`, `<>`;
`STARTS WITH`/`ENDS WITH`/`CONTAINS`/`=~`; `ORDER BY … SKIP … LIMIT`;
`avg`/`min`/`max`/`sum` and `percentileCont`/`percentileDisc` invariants;
`EXISTS { }` / `COUNT { }` / pattern-comprehension subqueries;
`CALL db.labels`/`relationshipTypes`/`propertyKeys` vs the modelled schema.

`CheckExprLiterals` (absolute-literal battery, ~40 probes): `UNION`/`UNION ALL`;
`CASE`, list comprehension, `reduce`, `all`/`any`/`none`/`single`; the
scalar/list/string/math function surface; list subscript, list slice, map
projection; temporal constructors (`date`, `duration`) and component access.

## Search algorithm coverage

Every `search/` algorithm the DST did not previously exercise (or exercised
only in a degenerate regime) is now cross-checked against an independent naive
reference on shaped, seed-deterministic fixtures, folded into the
`search` / `search-crash` battery (so each is validated post-crash-recovery):

negative weights + negative-cycle detection (Bellman-Ford / Floyd-Warshall /
Johnson); `MinCostMaxFlow`; `PushRelabelMaxFlow`; `Closeness` / `Harmonic` /
`Eigenvector` / `Katz` / `PersonalisedPushPageRank`; serial-vs-parallel
`Betweenness`; parallel-edge k-shortest; `TopologicalSort` DAG success;
`Diameter`; triangle counting (serial == parallel); `WCCParallel` vs serial;
undirected Euler; `BiBFS`; direction-optimised BFS on a hub fixture; the
`*Into` / `NewSSSP` buffer-reuse APIs; external-memory `extern.BFS` /
`extern.PageRank`.

## Storage / durability coverage

| Feature | Scenario / vehicle | Invariant |
|---|---|---|
| Concurrent durable commits + crash recovery | `durable-commit-crash` | acked ⊆ recovered ⊆ issued; failures absent; no torn CREATE |
| Background checkpointer + crash-safe `store.DB` teardown | `checkpoint-teardown` | no `ErrWriterClosed` into an acked commit; recovered ⊇ acked; `Stop()` joins |
| Read-transaction behaviour under concurrency + crash | `readtx-isolation` | no dirty/partial reads; whole-batch atomicity on recovery |
| Atomic csrfile publish under fault/ENOSPC | `csrfile-publish-fault` | a failed publish leaves either no file or the complete prior csrfile — never torn |
| Recovery genuine-corruption fail-stop | `wal-corruption-failstop` | a corrupted interior WAL frame is detected (CRC), recovery reconstructs exactly the clean prefix and refuses to append; a benign torn tail is not treated as corruption |
| Post-rename dir-fsync fail-stop (WAL prefix reclaim) | `checkpoint-dirfsync-fault` | a post-rename parent-dir fsync failure poisons the writer, yet reopen recovers the exact committed state |
| DDL (index + UNIQUE constraint) across the checkpoint/snapshot boundary | `ddl-checkpoint-crash`; `constraint-enforce` and `index-diversity` now checkpoint too | the checkpoint's reclaimed WAL prefix COVERS the DDL frames (measured on the SimDisk image), the pure-snapshot phase replays ZERO WAL ops, and the recovered schema still enforces UNIQUE, answers every index seek, and matches `SHOW`/`db.*` |
| CSV / JSONL export→import round-trip under fault | `io-roundtrip-fault` | a clean round-trip reproduces the modelled edge set exactly; an export under ENOSPC fails with a typed error and leaves no partial artifact a re-import would accept |
| Offline bulk-import publication (`store/bulkimport`) — **parity only, NOT fault coverage** | `bulkimport-parity` | a published snapshot reopens through real recovery equal to the harness model exactly (node set two-sided, labels, properties by **kind and value**, per-handle edge multisets including parallel twins), `SnapshotHit` with **zero** replayed WAL ops on two successive opens, and the measured lifecycle contract (`ErrNotFinished` / `ErrFinished` / `ErrStoreNotEmpty`, their precedence, and `PublishResult.Stats`); plus the publish's byte-reproducibility boundary. **No fault regime is reachable** — see the note below |
| Crash **during** the snapshot publish, at each step of the crash-atomic swap | `checkpoint-crash-storm` | acked ⊆ recovered ⊆ issued across a crash inside the publish window; a stranded backup is promoted by recovery (measured on the durable image and on `store.recovery.snapshot.promoteParentFsync`), never a half-published snapshot |
| Corruption of a published snapshot COMPONENT | `snapshot-corruption-failstop` | a byte flipped in any of the nine components fail-stops recovery with that component's typed sentinel; recovery returns no store, mutates nothing on disk and leaves `db/wal` byte-identical; the restored image still recovers the exact committed model. One documented non-fail-stop is pinned in the same run: a corrupt `indexes/<name>.bin` is REBUILT (and the rebuild verified against a full scan). The manifest's key-name region was the second until rmp #2520 checksummed it — see below |

### Snapshot component corruption is now covered; the manifest is checksummed (rmp #2467, #2520)

`store/snapshot` declares **nine** typed corruption sentinels — one per durable
component — and the manifest carries a CRC32C for each. Before this task the
only corruption the simulator injected was a byte flip inside a WAL frame
(`wal-corruption-failstop`), `SimDisk.CorruptRange` appeared nowhere else
outside the disk's own unit tests, and **none of the nine sentinels had ever
been reached under simulation**. `snapshot-corruption-failstop` now flips a byte
in each component of a published snapshot and adjudicates the reopen that
follows, over a fixture whose checkpoint has folded the **whole** WAL — so the
snapshot is the only durable source of the committed graph and a refusal is a
genuine fail-stop rather than a fallback.

The sentinel a flip produces is a function of **where** it lands, which the
scenario measures rather than assumes:

| Component | flip at byte 0 (the header) | flip anywhere else |
|---|---|---|
| `manifest.json` | `ErrManifestCorrupted` (JSON parse) | `ErrManifestCorrupted` (CRC32C trailer) |
| `csr.bin` | `ErrCSRCorrupted` + `ErrCorrupted` | `ErrCorrupted` (CRC) |
| `labels.bin` | `ErrLabelsCorrupted` + `ErrCorrupted` | `ErrCorrupted`, or both |
| `properties.bin` | `ErrPropertiesCorrupted` + `ErrCorrupted` | `ErrCorrupted` |
| `mapper.bin` | `ErrMapperCorrupted` **alone** | `ErrCorrupted`, or both |
| `tombstones.bin` | `ErrTombstonesCorrupted` + `ErrCorrupted` | `ErrCorrupted`, or both |
| `edgehandles.bin` | `ErrEdgeHandlesCorrupted` + `ErrCorrupted` | `ErrCorrupted` |
| `constraints.bin` | `ErrConstraintsCorrupted` + `ErrCorrupted` | `ErrCorrupted`, or both |
| `indexdefs.bin` | `ErrIndexDefsCorrupted` + `ErrCorrupted` | `ErrCorrupted` |
| `indexes/<name>.bin` | *(no error — see below)* | *(no error)* |

Two asymmetries in that table are worth naming. First, a component's **own**
sentinel is raised only when the flip breaks the structural parse; a flip in a
payload region parses cleanly and is caught later, when the CRC32C is compared
against the manifest entry, which surfaces as the directory-level
`snapshot.ErrCorrupted` **without** the component's own sentinel. Second,
`mapper.bin` is the one component whose header failure is **not** wrapped in
`ErrCorrupted`: `LoadSnapshotFull` peeks the mapper's format version before
handing it to the verified reader, and that peek returns its error unwrapped.
Everything else in the load path wraps.

`indexes/<name>.bin` is **tolerated by design**, not a gap: `snapshot.LoadIndexes`
reports a CRC-failing payload as nil bytes and recovery rebuilds that index from
the LPG. The scenario therefore requires the reopen to SUCCEED for a corrupt
payload — and then cross-checks every declared index against a full label scan,
so "tolerated" cannot degrade into "silently wrong".

**The finding (rmp #2467): `manifest.json` carried the CRC of every other
component and none of its own.** A byte flipped inside a JSON **key name** left a
syntactically valid document whose key `encoding/json` no longer recognised, so
the field was dropped and decoded to its zero value with no error anywhere. A
byte-by-byte sweep of this scenario's published manifest (1399 bytes) found **360
bytes, 25.7% of it, whose corruption recovery accepted silently** — the key names
(`"version"`, `"order"`, `"size"`, `"commit_ts"`, `"crc32c"`, `"name"`), the
index-name string values, and the trailing newline. The consequence measured on
the worst of them: flipping one character of the `"commit_ts"` **key** dropped the
MVCC clock floor recovery derives (`recovery.Result.MaxCommitTS`) from 20 to 0, so
`RestoreMVCCClock` was never called and the reopened graph re-minted instants the
image already contained — the loss rmp #2309 exists to prevent, reachable through
an undetected corruption. No committed node was lost; the damage was to the clock,
not to the data, which is why no test that counts recovered nodes could see it.

**The fix (rmp #2520): a CRC32C trailer.** `snapshot.WriteManifest` now appends a
fixed 16-byte trailer after the JSON document — an 8-byte magic, a 4-byte
algorithm identifier, and a CRC32C over every preceding byte. `LoadManifest`
verifies it **before it reads a single field**, the version check included, since
a manifest is the first thing a store open reads and nothing in it may be trusted
until its bytes are established.

The checksum lives *outside* the structure it protects, which is the property
that closes the gap. A `crc32c` field *inside* the JSON would have to be excluded
from its own computation, leaving its own key name unprotected — the same defect,
merely relocated. The trailer covers the document whole: every key name, every
value, every byte of indentation.

Two rules make the coverage total rather than nearly total:

- **A non-empty tail MUST verify.** The reader splits the file at
  `json.Decoder.InputOffset()`. A tail of nothing but whitespace means "written
  before the trailer existed" and is accepted unverified; anything else must be a
  well-formed, verifying trailer. Without this a flip in the *magic* would demote
  a protected manifest to "legacy, accepted" — the silent acceptance moved into
  the trailer instead of removed.
- **A manifest that declares a trailer must carry one.** `Manifest.Integrity`
  records the framing scheme, so a manifest whose trailer was lost entirely
  (a zeroed tail block, a truncating copy) is refused rather than mistaken for a
  legacy file. Losing the protection needs two independent damages, not one.

Re-measured after the fix: **0 of 1450 bytes** of the published manifest can be
flipped without recovery refusing (`TestSnapshotCorruption_ManifestUncheckedByteCensus`,
which now requires zero). `TestSnapshotCorruption_ManifestKeyRegionIsChecksummed`
pins the `commit_ts` key specifically, and
`TestSnapshotCorruption_MVCCClockFloorIsNotSilentlyZeroable` states the same
guarantee in engine terms: sweeping all 1449 manifest bytes, the restored clock
floor of 20 was never moved — 1449 flips refused, 0 accepted.

**Forward compatibility is preserved in both directions, and `ManifestVersion` is
deliberately NOT bumped.** The trailer is a change to the file *framing*, not to
the JSON *schema*, and keeping those layers apart is what lets integrity and
`encoding/json`'s ignore-unknown-fields policy hold together. An older snapshot
has no trailer, is accepted unverified, and reports `Manifest.IntegrityVerified`
false — the frozen v1 fixture in `store/snapshot/testdata` still loads byte-for-byte
unchanged, with no migration. A newer snapshot read by an older build is
unaffected too: `json.Decoder` stops at the end of the first complete value, so a
build that predates the trailer never reads past the closing brace. Bumping the
schema version would have made those readers reject the file with
`ErrManifestUnsupported` for no reason. The "absent means no value" policy
`Manifest.CommitTS` documents is therefore unchanged — but it is now only ever
evaluated over bytes the trailer has established, so "absent" can only mean the
writer omitted the field, never that corruption renamed its key.

The guarantee is scoped to *accidental* corruption. CRC32C is not a MAC: an
attacker who can rewrite `manifest.json` can recompute the trailer. The defence
against a hostile store directory remains the surrounding controls (`O_NOFOLLOW`
component opens, `DefaultMaxManifestBytes`, the per-component allocation bounds).

Closing the gap removed the substrate the battery's primary oracle stood on.
Every arm asserts recovery REFUSED, and an assertion that can only ever hold
proves nothing, so the reachability control moved rather than being dropped:
`TestSnapshotCorruption_OracleFiresWhenRecoveryWronglySucceeds` now aims its arm
at an `indexes/<name>.bin` payload — the one durable file in a published snapshot
a reopen accepts damaged — and still requires the run to REPORT the acceptance.

A third observation, measured by the scenario's own determinism check rather
than assumed: two identical fixtures built in the same process publish snapshots
whose components differ in LENGTH in two places. `manifest.json` embeds
`created_at`, whose RFC3339 fraction trims trailing zeros; and `mapper.bin` holds
the engine-minted natural keys, which are `__cx_<hex>` drawn from a
**process-global** counter (`cypher/exec/create_node.go`, where the counter is
deliberately seeded from the largest existing suffix so a new process cannot
collide with keys an earlier one persisted). The keys' hex width — and with it
the component's size — therefore grows as a process creates more nodes: a
measured 303-byte `mapper.bin` on a process's first fixture against 318 bytes on
every later one. This is documented, intentional behaviour, not a defect, but it
means a snapshot's component sizes are a function of process history, so
`TestSnapshotCorruption_Deterministic` compares offsets where the sizes match and
REPORTS a size difference rather than hiding it. It is the same class of
observation as the bulk-import byte-reproducibility note below.

A second, smaller finding, measured while probing the manifest guards:
`ErrManifestTooLarge` bounded what the decoder **consumed**, not the file's
length. `json.Decoder` stops at the end of the first complete value, so
whitespace appended after the closing brace was never read and a `manifest.json`
of any size on disk was accepted. That was defensible for a guard whose stated
purpose is to bound the transient decode allocation, but it meant the ceiling
could only be reached by bytes inside the JSON value — which is how the scenario
drives it. **Closed by rmp #2520**: verifying the trailer requires reading to the
end of the file, so the ceiling now bounds the bytes read. The scenario still
pads *inside* the object, because that probe reaches the guard under both
readings and so remains the stricter one.

### Bulk-import publication is covered for PARITY, not for faults (rmp #2466)

The `bulkimport-parity` row above is deliberately narrower than every other row
in the table, and the gap is structural rather than an omission.

Every other durability scenario injects its faults through `SimDisk`, which
reaches the persistence packages via their filesystem seams (`wal.OpenFS`,
`recovery.OpenFS`, `snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS`
and siblings). `bulkimport.Publish` has **no such seam**: it calls `os.MkdirAll`
and `os.ReadDir` directly and writes through the **non-seamed**
`snapshot.WriteSnapshotFullCtx`, while `ImportInto` takes a `storeDir string`
plus an `Options` that carries no filesystem. A `SimDisk` therefore cannot be
placed underneath a bulk-import publish without changing the production API —
**filed for a user decision as rmp #2518**, and deliberately not done under
#2466.

So for bulk-import publication the following remain **uncovered**: `ENOSPC`
mid-write; a failing `fsync` on a component, on the staging directory, or on the
parent directory; a failing or crash-interrupted `snapshot.tmp` → `snapshot`
rename; and a crash landing inside the publish window. The scenario's
crashed-import arm reconstructs the *outcome* state of such a crash — a complete
snapshot moved to the assembly name, which recovery must ignore and clean up —
which measures recovery's treatment of that state, **not** the writer's
behaviour while reaching it.

A second finding from the same task, measured rather than assumed: a bulk-import
publish is **not byte-reproducible** once items carry two or more properties.
Republishing the identical record slices twice yields data components of the same
names and sizes but different bytes, and stripping properties — or reducing each
item to exactly one — makes it byte-identical, which isolates the cause to Go map
iteration over the `Properties` maps. The *logical* result is identical every run
(the parity pass proves it), so this is not a correctness defect and matches what
`bulkimport.Node` documents; but two imports of identical data cannot be compared
by checksum, and bulk-import snapshots will not deduplicate in content-addressed
storage. `TestBulkImportParity_ByteBoundary` pins the boundary.

The `DiskConfig.FaultRate` knob and five `SimDisk` primitives back these
scenarios — four faults (`CorruptRange`, `ArmSyncFaultAt`,
`ArmParentDirSyncFaultForPath`, `ArmRenameFaultForPath`) and one crash-window
selector (`ArmRenameWritebackForPath`, which chooses whether a rename's dirent
had reached stable storage when the crash landed). All default to inert, so
existing scenarios are byte-identical.

### Crash during the snapshot publish (rmp #2465, closing #1827)

Until sprint 347 a checkpoint could never be *interrupted*: `SimStore.Checkpoint`
is synchronous and always ran to completion, and `Simulator.maybeCheckpoint`
treats any checkpoint error as a hard run failure. The whole interrupted-publish
half of the durability contract was therefore unexercised, and recovery's
snapshot-promote repair — the block in `store/recovery` that promotes a stranded
`snapshot.bak` back to the live name, marked by the
`recovery.snapshot-promote-post-rename-pre-fsync` crash point — was **dead code
under simulation**.

Two things had to change before the window could be reached at all, and both
were found by measurement rather than assumed:

* **The renames could not fail.** Every other step of the publish
  (`write+fsync components → fsync staging dir → archive rename → publish rename
  → fsync parent`) could already be faulted, but the two renames could not, so
  the publish path's own archive-restore branch was unreachable.
  `ArmRenameFaultForPath` closes that gap. The task's premise that the
  *parent fsync* also could not fail was **wrong**: the pre-existing
  path-keyed `ArmParentDirSyncFaultForPath` already targets it, which a probe
  confirmed.
* **A crash in the publish window manufactured a false total loss.**
  `SimDisk.Crash` revokes *every* not-yet-fsync'd dirent, and the publish issues
  its two renames back to back with no fsync between them — so the crash dropped
  both the archived backup and the newly published snapshot, an outcome no real
  filesystem produces (a lost rename leaves the *old* name). That single
  modelled outcome is also the reason the promote repair was unreachable: it
  exists precisely for "the archive rename reached disk, the publish rename did
  not". `ArmRenameWritebackForPath` selects that other, equally legal branch of
  the crash-window non-determinism, one rename at a time and opt-in.

`checkpoint-crash-storm` then crashes at three points of the swap
(`stranded-backup`, `publish-rename`, `archive-rename`) while concurrent Bolt
committers are still writing — the publish is checkpoint phase 2 and holds no
commit lock, so the window is genuinely raced, which the run measures as durable
commits landing *during* the interrupted checkpoint.

The DST does not observe crash points directly (see
`CoverageTracker.UnobservableSignals`): `crashpoint.Breakpoint` is compiled out
without the `gograph_crashinject` tag and SIGKILLs the process with it, which
would kill the test binary instead of producing the harness's in-process crash.
Bridging it would mean adding a pluggable handler to a production-callable
package. The scenario instead reproduces the *window* the site marks and observes
the *branch* it guards through surfaces that already exist — the durable image
(backup-only before the reopen, live-only after it) and store/recovery's own
exported `store.recovery.snapshot.promoteParentFsync` counter.

### DDL across the snapshot boundary (rmp #2464)

Until sprint 347 every DDL-issuing scenario (`schema-chaos`, `constraint-enforce`,
`index-diversity`) ran **WAL-only**, so recovery always replayed the
`CREATE INDEX` / `CREATE CONSTRAINT` frames and the snapshot's schema components
(`store/snapshot/constraints.go`, `indexdefs.go`, `indexes.go`) were never the
source of a recovered index or constraint. The loss mode the checkpointer's
phase-3 self-sufficiency re-verification exists to prevent — truncating the WAL
prefix that first *declared* a constraint or an index (#1334 / #1464 / #1755) —
was therefore never exercised.

`ddl-checkpoint-crash` occupies that intersection directly, and
`constraint-enforce` and `index-diversity` now enable in-loop checkpointing so
their existing post-recovery oracles adjudicate a **snapshot-loaded** schema.
A `CheckpointConfig` is INERT unless the run loop calls `maybeCheckpoint`, which
only the default `Simulator.Run` does automatically; each custom loop wires the
call and each scenario carries a terminal gate asserting a **non-zero checkpoint
count**, so a configuration that stops taking effect fails the run rather than
passing quietly.

### The group-commit clarification

Through the engine (and therefore the Bolt wire), every write commit — including
its WAL `fsync` — is serialised under a single `visMu` lock, so `SyncGroup` is
always a **solo leader with zero followers**. Multi-member group-commit
coalescing and the fail-all path are unreachable via the engine and are covered
by store-layer unit tests (`store/wal/syncgroup_test.go`,
`store/txn/group_commit_durability_test.go`). The DST drives the solo-leader
`SyncGroup` on every durable write, and now additionally under concurrent
crash/recovery.

### Read-transaction isolation

`cypher.Engine.BeginReadTx` provides **snapshot isolation across the whole
transaction** since rmp #2307: one read instant is pinned at `BEGIN` and every
statement of the handle executes at it. It was per-statement read-committed when
this section was first written, and the `readtx-isolation` scenario — which
asserts only that no dirty or partial read is ever observed — certifies a
property both levels satisfy, so it remains valid under the stronger contract.

### MVCC multi-session and concurrency coverage (sprint 345)

The MVCC machinery is exercised end to end by four dedicated modes (see
[docs/dst.md](dst.md#mvcc-multi-session-and-concurrency-coverage) for the full
description): the deterministic multi-session mode with in-transaction
isolation checkers (`RunMVCCSessions` + `mvcc_isolation.go`, rmp #2436), the
contended lost-update scenario (`RunMVCCContention`, rmp #2437), crashes with
open transactions and transaction-granular recovery adjudication (rmp #2438),
Bolt-wire transactional roles with typed conflict accounting and during-run
isolation oracles (rmp #2439/#2440), and the `production-profile` catalogue
scenario combining all of it over the durable store in crash cycles
(rmp #2441). The checkers found four engine isolation defects on arrival
(rmp #2445, #2446), all fixed and regression-pinned.

## Defects surfaced by this coverage work

The coverage work exercised the engine against these scenarios and found:

1. **`MERGE … ON CREATE/ON MATCH SET` dropped an expression right-hand side**
   (fail-silent): `ON MATCH SET n.n = n.n + 1` committed but never applied.
   **Fixed** — the merge operators now evaluate a non-literal RHS per-row
   (openCypher TCK unchanged at 3897/3897). The `merge-rel` scenario is the
   regression guard.
2. **`CALL … YIELD … WHERE <pred>`** silently ignored the `WHERE` filter
   (read-only). **Fixed** (#1966) — the visitor now captures `Call.Where` and
   the translator lifts it as a `Selection` over the `ProcedureCall`. The
   engine-level guard is `cypher/call_yield_where_test.go`; since rmp #2462 the
   DST also holds the procedure form to the harness's DDL model on every
   schema-introspection check (`checkCallYieldWhere`, see
   [DST language-surface gaps](dst.md#language-surface-gaps-rmp-2462)).
3. **k-shortest multigraph semantics** diverge between `YenKShortest` (dedups by
   node sequence, cheapest parallel edge) and `Loopless`/`Eppstein` (parallel
   edges as distinct paths). Recorded for adjudication (#1967).
4. **A list-valued column was stringified on the Bolt wire.**
   `bolt/server/session.go`'s `exprValueToPackstream` documented an
   `expr.ListValue` case but its switch had none, so a list column fell through
   to the `default` arm and was emitted as a PackStream **String**
   (`"[1, 2, 3]"`) instead of a PackStream **List**. A literal list
   (`RETURN [1,2,3]`) was stringified identically, so this was the RECORD encoder
   rather than parameter binding — a bound list evaluated correctly (indexing,
   `size`, and equality against a literal list all agreed). It affected every
   list-producing construct: `collect()`, `labels()`, `keys()`, `nodes(p)`,
   `relationships(p)`, list literals, and list-valued properties; `nodes(p)`
   additionally lost all node structure. Found by the wire parameter matrix
   (rmp #2462), which PINNED the rendering so a fix would flip the probe
   deliberately. **Fixed** (#2513) — the arm encodes element-wise with the
   negotiated Bolt major threaded through, so nested containers and entity or
   temporal elements encode structurally on both 4.4 and 5.x. The pin is now the
   live assertion, and `internal/sim/wire_list_encoding_test.go` carries the
   end-to-end matrix.

5. **`manifest.json` was not checksummed.** It carried a CRC32C for every other
   snapshot component and none of its own, so a single byte flipped inside a
   JSON **key name** decoded as an unknown field, was dropped by
   `encoding/json`, and zeroed the corresponding `Manifest` field with no error
   anywhere — 360 bytes, 25.7% of a published 1399-byte manifest, were
   silently accepted. Measured consequence on the worst field: flipping one character of
   the `"commit_ts"` key dropped the recovered MVCC clock floor from 20 to 0, so
   `RestoreMVCCClock` was skipped and the reopened graph re-minted instants the
   image already contained (the loss #2309 prevents). No committed node was lost,
   which is why a green suite could not see it. Found by
   `snapshot-corruption-failstop` (rmp #2467), which changed nothing outside
   `internal/sim` and `docs/`. **Fixed** (#2520): `WriteManifest` appends a
   CRC32C trailer over the whole document and `LoadManifest` verifies it before
   reading any field. Re-measured census: **0 of 1450 bytes**. `ManifestVersion`
   is unchanged, so old snapshots open and new ones stay readable by older
   builds; see the section above for why the framing and schema layers are kept
   apart.

## Documented debt / out of scope

- **GraphML round-trip under fault** is not yet covered: its ergonomic entry
  point is a property-graph, not the edge-list the fault scenario builds. CSV
  and JSONL round-trips are covered. (A property-graph fixture would extend this
  to GraphML.)
- **Snapshot isolation for read transactions** shipped in rmp #2307 (sprint
  334) via MVCC version chains rather than the copy-on-write epic (#1671) once
  considered for it. The DST scenarios assert no-dirty-read, which the stronger
  contract still satisfies; the isolation level itself is gated by
  `cypher/readtx_snapshot_test.go` and `bolt/server/e2e_readtx_snapshot_test.go`.
- **Multi-member WAL group-commit coalescing / fail-all** is engine-unreachable
  (serialised under `visMu`) and is covered by store-layer unit tests, not the
  DST — see the group-commit clarification above.
