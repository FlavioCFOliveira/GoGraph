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
| Atomic csrfile publish under fault/ENOSPC, across every weight kind and access pattern | `csrfile-publish-fault` (`internal/sim/csrfile_access_matrix.go`) | a failed publish leaves either no file or the complete prior csrfile — never torn, now also at a SEED-DRAWN weight kind; and the whole `WeightKind` x `AccessPattern` grid round-trips exactly, with the weights decoded independently of the package's typed accessors, an advisory access hint proven to change no byte, `WeightAbsent` distinguished from a weighted file on four independent signals, and a truncated file refused with a typed sentinel while `Reinterpret` refuses to build a view — see below |
| Recovery genuine-corruption fail-stop | `wal-corruption-failstop` | a corrupted interior WAL frame is detected (CRC), recovery reconstructs exactly the clean prefix and refuses to append; a benign torn tail is not treated as corruption |
| Post-rename dir-fsync fail-stop (WAL prefix reclaim) | `checkpoint-dirfsync-fault` | a post-rename parent-dir fsync failure poisons the writer, yet reopen recovers the exact committed state |
| DDL (index + UNIQUE constraint) across the checkpoint/snapshot boundary | `ddl-checkpoint-crash`; `constraint-enforce` and `index-diversity` now checkpoint too | the checkpoint's reclaimed WAL prefix COVERS the DDL frames (measured on the SimDisk image), the pure-snapshot phase replays ZERO WAL ops, and the recovered schema still enforces UNIQUE, answers every index seek, and matches `SHOW`/`db.*` |
| `graph/io` export→import: CSV, JSONL, GraphML and DOT | `io-roundtrip-fault` (`internal/sim/storage_fault_scenarios.go` + `internal/sim/graph_io_surface.go`) | a clean round-trip reproduces the modelled edge set exactly and an export under ENOSPC fails with a typed error leaving no partial artefact a re-import would accept; the DOT writer — which has no reader — is adjudicated by CROSS-FORMAT AGREEMENT with CSV and JSONL over a model built to force quoting, weight labels and a bare node statement, with the one legitimate disagreement (an edge-list CSV cannot carry an isolated vertex) asserted in shape rather than waived; the JSONL property path round-trips every property KIND; the `csv.Options` delimiter / comment / header / weight-column / formula-sanitisation space is driven beyond `DefaultOptions`; every export is checked for byte-reproducibility; and a seed-mutated export sweep requires no panic and an effective mutation per format. The sweep's bounded-allocation bound is adjudicated in a SERIALISED test arm rather than in the scenario, because it is measured with a process-global counter that bills a concurrently scheduled scenario for its neighbours (rmp #2553). Every defensive cap in `graph/io` is provoked, and every `*Ctx` reader is cancelled mid-parse, by `RunGraphIOGuards` — see below |
| Offline bulk-import publication (`store/bulkimport`) — **parity only, NOT fault coverage** | `bulkimport-parity` | a published snapshot reopens through real recovery equal to the harness model exactly (node set two-sided, labels, properties by **kind and value**, per-handle edge multisets including parallel twins), `SnapshotHit` with **zero** replayed WAL ops on two successive opens, and the measured lifecycle contract (`ErrNotFinished` / `ErrFinished` / `ErrStoreNotEmpty`, their precedence, and `PublishResult.Stats`); plus the publish's byte-reproducibility boundary. **No fault regime is reachable** — see the note below |
| Crash **during** the snapshot publish, at each step of the crash-atomic swap | `checkpoint-crash-storm` | acked ⊆ recovered ⊆ issued across a crash inside the publish window; a stranded backup is promoted by recovery (measured on the durable image and on `store.recovery.snapshot.promoteParentFsync`), never a half-published snapshot |
| Node-key and edge-weight CODEC matrix across crash and upgrade | `codec-matrix` (soak; `internal/sim/codec_matrix.go`) | seven `(key codec, weight codec)` arms each survive the three snapshot-publish crash windows AND the upgrade + snapshot boundaries with acked ⊆ recovered ⊆ issued adjudicated BY KEY; the durable `mapper.bin` carries the layout the key type selects (v1 for the string control, v2 for the other six) and the snapshot-only reopen replays ZERO WAL ops, so every recovered key came through the mapper; `txn.ErrNoWeightCodec` is provoked and its actual behaviour pinned. One measured gap is pinned rather than tolerated: a struct weight is dropped by the snapshot CSR writer — see below |
| Corruption of a published snapshot COMPONENT | `snapshot-corruption-failstop` | a byte flipped in any of the nine components fail-stops recovery with that component's typed sentinel; recovery returns no store, mutates nothing on disk and leaves `db/wal` byte-identical; the restored image still recovers the exact committed model. One documented non-fail-stop is pinned in the same run: a corrupt `indexes/<name>.bin` is REBUILT (and the rebuild verified against a full scan). The manifest's key-name region was the second until rmp #2520 checksummed it — see below |
| Cross-release compatibility of the on-disk SNAPSHOT format | `TestCrossRelease_*` (soak: prior-tag subprocess) + `TestCrossReleaseCompat_*` (short: frozen fixtures) | a PRIOR release publishes a **checkpoint** — snapshot directory plus truncated WAL — and the current code reopens it through the FULL-STACK `recovery.OpenCtx`, so that release's `manifest.json`, `csr.bin`, `labels.bin`, `properties.bin` and `mapper.bin` are parsed by current code. "The snapshot was opened" is adjudicated from two INDEPENDENT observations — the filesystem read with the current manifest reader, and recovery's own `SnapshotHit` — so a directory present but skipped is a `SnapshotProvenanceGap` that fails parity, not an unfalsifiable false. On the short layer a frozen pre-#2520/#2526 snapshot directory asserts both directions of the documented contract: an older artefact still opens (manifest loads reporting `IntegrityVerified` **false**; the dense width-8 weights column parses with its exact `float64` values), and a newer artefact is refused by the older reader's documented rule **deterministically**, by both of that release's guards. Fixture digests are pinned in the test source rather than in a golden, so `-update` cannot regenerate the old format away — see below |
| Per-transaction op caps (CWE-770), producer **and** replay | `txn-oversize` (`internal/sim/txn_oversize.go`) | an over-cap commit is refused with `txn.ErrTransactionTooLarge` **before any frame is written** — proved by the durable WAL image being BYTE-identical across the refusal and the live graph unmutated, not by the error alone — and the surviving file recovers clean with every refused key absent; a hand-built WAL whose marker-less run exceeds the replay cap fail-stops with `recovery.ErrTransactionTooLarge`, keeps exactly the committed prefix, and is refused by the store-open rather than appended onto. The boundary is MEASURED on both sides and the two caps agree exactly (cap ops passes, cap+1 fails both). Until this task the cap reached only the replayer, so neither sentinel was reachable under simulation at any setting — see below |

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

### Cross-release testing reached the WAL and nothing else (rmp #2477)

The cross-release harness (`internal/sim/crossrelease*.go`,
`cmd/sim-xrelease-helper`) imported `store/wal`, `store/recovery` and
`store/txn` — grepping that path for `snapshot` returned nothing. A prior
release therefore only ever handed the current code a **WAL**: no prior
release's snapshot directory had ever been opened by current code, and no v1 or
v2 manifest had ever been loaded by the current reader. The entire on-disk
snapshot format family — `manifest.json`, `csr.bin`, `labels.bin`,
`properties.bin`, `mapper.bin`, `edgehandles.bin` — sat outside cross-version
testing.

Two surfaces created in this same sprint sat exactly in that gap, and both were
shipped **without a schema-version step** precisely so old and new artefacts
would keep interoperating:

- **rmp #2520** appended a CRC32C integrity trailer after the manifest JSON.
  Older snapshots have no trailer and must still open, reporting themselves
  unverified rather than claiming an integrity they do not have.
- **rmp #2526** introduced the `0xFF` sentinel in `csr.bin`'s weight-width
  header byte to select a variable-width, codec-encoded weights section. Older
  files keep the dense native layout byte for byte, and an older reader meeting
  a sentinel-bearing file must fail loudly rather than mis-slice the column.

Because neither bumped a version, no version gate would have caught a regression
in either direction, and neither had a cross-release test.

**What now covers it.** The helper publishes a checkpoint before it exits and
the reopen routes through the full-stack recovery path; frozen fixtures pin the
old shapes on the short layer and hold the legacy reader's documented rule to
both directions. The design — the two-stage helper build that degrades one
capability instead of skipping a whole tag, the two independent observations
behind the snapshot-provenance verdict, and the three guards against a silently
regenerated fixture — is described in
[docs/dst.md](dst.md#cross-release-compatibility-beyond-the-wal-rmp-2477).

**The checkpoint half did not build at any real tag (rmp #2531).** As shipped,
that coverage reached `HEAD`-as-prior only. `Checkpointer.RunCheckpoint` was
exported only from **v0.6.0**, so at `v0.2.0` and `v0.3.0` the helper's checkpoint
file failed to compile, the two-stage build dropped it, and both tags fell back to
a WAL-only image — reporting the degradation loudly, which is how it was found.
Driving the checkpoint through `Start`/`TriggerCtx`/`Stop` instead — exported
unchanged since **v0.1.0** — makes every one of the fourteen release tags publish
a manifest-v3 snapshot that current code opens with snapshot-only recovery, so
there is no snapshot floor. The snapshot facts are now asserted per tag rather
than logged. Because a published snapshot truncates the WAL and thereby hides any
prior defect living in WAL replay — `v0.1.0` and `v0.2.0` have one, reading their
own 32-node image back as 79 nodes — a deliberately WAL-only arm
(`ForceWALOnly`) retains that path and doubles as the negative control proving
`SnapshotOpened` can read false. Both are described in
[docs/dst.md](dst.md#the-fallback-fired-at-every-real-tag-rmp-2531).

**Measured on arrival.** No cross-version defect was found: a pre-#2520 manifest
loads and reports `IntegrityVerified` false, a pre-#2526 dense `csr.bin` returns
its exact weights, a sentinel-bearing `csr.bin` is refused by both legacy guards
(over-determined, not by luck), and the full-stack reopen recovers the whole
graph from a directory whose WAL had shrunk to zero bytes with zero replayed WAL
ops. What changed is that all of that is now falsifiable.

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

The `DiskConfig.FaultRate` knob, the direct `CorruptRange` mutator and ten
one-shot arms back these scenarios. All default to inert and none draws from the
[Seed], so arming one never perturbs the fault stream and existing scenarios stay
byte-identical.

**Faults** — an operation fails: `CorruptRange` (applied directly rather than
armed), `ArmSyncFaultAt`,
`ArmDirSyncFaultForPath` (rmp #2537), `ArmParentDirSyncFaultForPath` and
`ArmRenameFaultForPath`. `ArmSyncGateAt` parks a caller inside its fsync instead
of failing it, which is how a crash is made to land in the phantom-commit window.

**Crash-window selectors** — an operation succeeds, and the arm pins *which* of
its legal crash outcomes the run takes, because a metadata mutation is not
crash-survivable until the containing directory is fsynced.
`ArmRenameWritebackForPath` pins "the rename had reached stable storage";
`ArmRenameRollbackForPath` (rmp #2514) pins the other legal branch; and
`ArmRemoveWritebackForPath` / `ArmRemoveRollbackForPath` (rmp #2536) do the same
for an unlink, whose two outcomes are "the removal stuck" and "the file I deleted
is back after the crash". A removal has no illegal outcome, so it has no
counterpart to `ArmRenameRevokeBothForPath`, which exists purely to reproduce the
physically impossible both-names-lost state on purpose so the harness's own gates
can be shown to reject it.

Because a journalling filesystem commits metadata **in order**, renames and
unlinks share ONE ordered log with a single durable-prefix draw
(`SimDisk.direntUndos`, rmp #2536): the crash keeps a prefix of the issued
mutations and reverses the suffix. Two independent draws would let a crash keep
an unlink while reversing a rename issued before it, which is an interleaving no
filesystem can produce.

Every arm has a reachability observable, and a scenario that arms one is expected
to assert it: `RenameFaultCount`, `RenameWritebackCount`, `RenameRollbackCount`,
`RenameRevokeBothCount`, `DirSyncFaultCount`, `RemoveWritebackCount`,
`RemoveRollbackCount`, plus the window and shape observables `SyncCount`,
`PendingRenameCount`, `PendingRemoveCount`, `LastCrashRenameOutcome`,
`LastCrashRemoveOutcome`, `LastCrashDiscardedBytes` and the removal-hit counters
`RemoveHitCount` / `RemoveHitCountForPath`. An arm that never fires is not an
assertion, so the counter is what distinguishes "the primitive fired" from "the
arm was silently ignored".

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

### The codec surface is now covered; struct weights do not survive a checkpoint (rmp #2473)

Before sprint 347 the simulator drove exactly **one** codec pair. That was
structural rather than an omission: every Cypher-driven scenario reaches the
graph through `cypher.Engine`, whose constructors take a
`*txn.Store[string, float64]` and nothing else, so `OpenSimStore` hardcoded
`txn.NewStringCodec` / `txn.NewFloat64WeightCodec`. `NewIntCodec`,
`NewInt32Codec`, `NewInt64Codec`, `NewUint64Codec`, `NewUUIDCodec`,
`NewBinaryMarshalerCodec`, `NewInt64WeightCodec` and
`NewBinaryMarshalerWeightCodec` had never appeared in a simulated crash, and
neither had the **version-2 byte-mapper**: `snapshot.WriteMapper` delegates to
`WriteMapperString` for `N == string`, so the codec-framed layout (and its read
side, `ReadMapperBytes` → `snapshot.ApplyMapperToGraphWithCodec`) was
unreachable from this harness.

`OpenSimStore` is now the string/float64 specialisation of a codec-generic core,
and `codec-matrix` runs seven arms through the crash-storm and upgrade
scenarios. Six of the seven hold completely.

**The seventh measured a real gap.** For a `BinaryMarshaler` weight the numbers
separate the two durable paths cleanly: at the WAL-only boundary **95 of 95**
acknowledged edges came back with their weight; after one folding checkpoint
over the same image — WAL measured to 0 bytes, 0 WAL ops replayed — **191 of
191** came back with the ZERO weight.

The cause is that `store/snapshot`'s CSR component never consults
`txn.WeightCodec`. It sizes a weight with the fixed table in `csrWeightSize`
(`store/snapshot/writer.go`), which returns 0 for every type outside the Go
primitives — including any struct, the case
`txn.NewBinaryMarshalerWeightCodec` exists for. `WriteCSR` then emits
`hasWeights=0`, which is **the same on-disk encoding a deliberately weightless
graph produces**, and the checkpoint truncates the WAL prefix holding the true
values. So an embedder using a non-primitive weight with checkpointing enabled
loses those weights silently and permanently, with no error at any layer.
Measured: `csrWeightSize[float64]` = 8, `csrWeightSize[int64]` = 8,
`csrWeightSize` of a struct with one `int64` field = 0,
`csrWeightSize[struct{}]` = 0.

Fixing it means changing `store/snapshot`, which was outside this task's scope,
so the behaviour is **pinned** instead: the affected arm must come back with the
zero weight on a snapshot-only recovery, and a separate non-vacuity check
requires that outcome to have actually been observed. Both fire the day the
engine changes, in either direction.

**Still uncovered on the codec dimension**: the codec arms are single-writer,
because every concurrency, Bolt and Cypher oracle in `internal/sim` is bound to
string keys through `cypher.Engine`. Concurrent-writer coverage for a non-string
key codec would require the engine itself to be generic, and is not reachable
from the harness as it stands.

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

### Group-commit coalescing and fail-all (rmp #2471)

This section previously stated that every write commit — including its WAL
`fsync` — is serialised under a single `visMu` lock, so `SyncGroup` was "always a
solo leader with zero followers" and multi-member coalescing was unreachable
through the engine. **That is false, and has been since sprint 334 made MVCC the
module's concurrency control.** An ordinary write takes the barrier SHARED
(`cypher/api.go`, `Engine.schemaGate.WeakLockAuto`), so two commits run
concurrently by design and their fsyncs coalesce.

The correction is measured, not argued. Driving 12 concurrent Bolt writer
connections × 40 commits through a real WAL-backed store on a `SimDisk`
(`RunGroupCommitCoalescing`, `internal/sim/group_commit.go`):

| committers | SyncGroup rounds | leaders | followers | acked commits |
|---|---|---|---|---|
| 12 | 483 | 422 | **61** | 480 |
| 1 (control) | 43 | 43 | **0** | 40 |

Both properties are now gated in the DST rather than recorded in a comment:

- **Coalescing** is a coverage precondition. `checkGroupCommitCoverage` fails the
  run if `store.wal.SyncGroup.coalesced` is zero under ≥ 8 concurrent
  committers — the signature of a regression to solo-leader commits, which halves
  write throughput and un-covers the fail-all branch entirely while leaving every
  other scenario green. `checkGroupCommitNonVacuity` is a **separate** gate, so a
  run that simply failed to commit is reported as uninformative rather than as a
  writer regression. The single-committer control arm is retained as a permanent
  sensitivity proof: it must read zero followers, which also keeps the
  `coalesced` counter honest (it is shared with `SyncBuffered`'s
  durable-already path).
- **Fail-all** is asserted end to end by `RunGroupCommitFailAll`. It builds a
  genuine 8-member group — `SimDisk.ArmSyncGateAt` holds the leader inside its
  fsync while the followers arrive, which is the only way the group is
  deterministic rather than lucky — fails that one shared fsync, and asserts every
  member receives the durability fail-stop (`wal.ErrDurabilityFailed`), that
  **none** is acknowledged, that exactly **one** round was poisoned, and that
  recovery keeps the commit acknowledged before the group while discarding the
  whole failed group.

The store-layer unit tests (`store/wal/syncgroup_test.go`,
`store/txn/group_commit_durability_test.go`) remain the arithmetic gate. What the
DST adds is a group whose membership is constructed rather than assumed: the WAL
unit test fails *every* fsync, so it cannot distinguish one shared round from N
serialised ones.

### The WAL writer surface: watermark, frame contiguity, truncate, guards (rmp #2472)

Most of what `store/wal.Writer` **exports** was invisible to the simulator.
`Stats`, `DurableOffset`, `Poisoned` and `SyncBuffered` were never read by any
scenario, and the whole-file `Truncate` — as distinct from `TruncatePrefix`, which
the checkpoint path drives — was never called. Each states a contract the rest of
the store depends on: the checkpointer picks its WAL cut point from
`DurableOffset` and aborts on `Poisoned`, and the txn layer's empty commit resolves
through `SyncBuffered`. `internal/sim/wal_writer_surface.go` adjudicates all of
them.

**The durability watermark** is observed after *every* acknowledged commit, and
what "expected" means is split in two, because only one form is available to every
caller:

- **Exact**, when the harness chose the payloads (`RunWALWatermarkDirect`): the
  durable offset must equal the watermark `AppendRun` returned for that commit, and
  `wal.Stats` must equal `SUM(wal.HeaderSize + len(payload))` over every frame
  emitted. Measured: 12 commits, 24 frames, `durable == appended == boundary ==
  imageLen == 656`.
- **Relative**, when the payloads are the engine's (`RunWALWatermarkEngine`): every
  counter is monotonic, the durable offset never exceeds the bytes accepted, and it
  lands on a **frame boundary** of the durable image — the accumulation over some
  whole number of leading complete frames. Measured: 8 commits, 32 frames, final
  offset 1456.

The relative form deliberately asserts **no absolute size**. rmp #2521 measured
that the durable image varies with process wall-clock time, because a commit marker
encodes the instant it was written; an oracle pinning a byte count would be pinning
the clock. The frame-boundary relation is derived from the frames actually on disk,
so it is invariant under that variation and is still exactly the invariant
`DurableOffset` documents.

**Per-transaction frame contiguity** is the claim `AppendRun` makes *by
construction* — it holds `w.mu` across a whole transaction's frames — and that
claim is only testable under concurrency. Before commit `9eee3b18` the contiguity
came from the store's single-writer semaphore two layers up, and `store/recovery`
discards a transaction's buffered prefix as orphaned on the stated ground that
frames never interleave, so an interleaved image makes recovery drop **committed**
ops. `RunWALContiguity` drives 8 concurrent committers × 12 transactions × 4 frames
through the real writer, then partitions the durable image by payload tag — what is
physically on disk, not what any counter claims:

| append path | frames | transactions | maximal runs | split transactions | worst fragments |
|---|---|---|---|---|---|
| `AppendRun` (one call per transaction) | 384 | 96 | **96** | **0** | 1 |

**The claim holds**, and it is asserted **unconditionally** — a split is a defect
however little concurrency produced it.

**The evidence that attributes contiguity to `AppendRun` is CONSTRUCTED, not
raced.** The first version of the control drove eight committers concurrently and
asserted that per-frame `Append` produced at least one split. It did on an idle
machine — 31 of 96 transactions — and then failed under `make ci`'s coverage step
with the suite running in parallel, where all four retries measured
`committerSwitches=7, split=0`: the scheduler simply never overlapped the
committers. **An assertion on a scheduling outcome measures the machine, not the
module** (the defect class filed as rmp #2517), and raising the retry count would
have traded a red gate for a slow one while still measuring the scheduler.

`RunWALContiguityAlternating` replaces it with a handoff protocol. Two committers
pass a token, so exactly one is eligible to append at any instant and the durable
image is determined by the protocol. Both modes run the **same** protocol; only the
append API changes, so the difference is attributable to the API alone:

| mode | frames | transactions | maximal runs | split | worst fragments | committer switches |
|---|---|---|---|---|---|---|
| per-frame `Append`, strict alternation | 8 | 2 | **8** | **2** | 4 | 7 |
| `AppendRun`, same handoff | 8 | 2 | **2** | **0** | 1 | 1 |

Per-frame `Append` releases the writer mutex between frames, so the partner's frame
really does land in the middle and each transaction ends up in four fragments —
a genuinely interleaved image from the real writer, reproducible byte for byte.
Under `AppendRun` the partner signals from *inside* the run and still cannot get
in, because the run holds the mutex throughout; that ordering is decided by the
**mutex, not the scheduler**, so it is equally deterministic. The signal is one-way
on purpose: a full ping-pong would deadlock there, and that deadlock is the
mechanism under test. The **whole layout** is asserted, not just "at least one
split", so a lost frame or a skipped handoff changes a number and is caught.

Verified in the condition that broke the original: under `GOMAXPROCS=1` with
coverage instrumentation — where the concurrent arm reproduces
`committerSwitches=7` exactly — five consecutive runs produced **byte-identical**
layouts for both alternating modes and exited 0.

Three gates are kept apart:

- **Non-vacuity** rejects shapes that would make the census worthless: fewer than 8
  committers, a **single frame per transaction** (contiguous by definition), or a
  transaction missing or short of frames.
- **The verdict** (`checkWALContiguity`) is asserted unconditionally.
- **The concurrency witness** (`checkWALContiguityConcurrencyWitness`) reports
  whether the machine actually granted concurrency. A shortfall is logged as
  **UNINFORMATIVE, never as a failure**, because it measures the scheduler; it
  never excuses a split, and the deterministic proof does not depend on it.

**`Truncate` and the poisoned writer** are pinned by `RunWALLifecycle`. The
documented parts hold: `Truncate` returns the bytes that were in the file, zeroes
the watermark, leaves the **lifetime** counters untouched, and the next append
restarts at offset 0 of the empty file; a poisoned writer reports a stable sticky
error carrying `wal.ErrDurabilityFailed`, rejects every append with that same value,
fails a `SyncGroup` for a watermark the poison discarded, and — per rmp #2322 —
still returns nil for a watermark that was already durable. Two members of the
contract were **undocumented and are pinned as measured**:

- **`SyncBuffered` on a poisoned writer returns nil.** The poison rewinds the
  accepted offset to the durable one, so "make everything accepted durable" is
  already satisfied and the durable-already fast path fires. It is correct —
  nothing accepted is un-durable — but it means `SyncBuffered` is **not** a health
  probe; `Poisoned` is.
- **`Truncate` on a poisoned writer succeeds** and empties the file, while the
  writer stays poisoned. `Truncate` is the one mutator that does not consult the
  sticky error. It is not a durability hole — the writer still refuses every
  append, so nothing can be written after the emptied file — and `Truncate` is
  documented as a maintenance helper off the production checkpoint path, which cuts
  the WAL with `TruncatePrefix` instead. It is pinned so a change putting `Truncate`
  on a live path is caught rather than absorbed.

Also measured: after `Close`, `Append` and `Truncate` return `wal.ErrWriterClosed`
rather than the poison (the closed check precedes the poison check), while
`Poisoned` still reports why the handle died.

**`ErrWALLocked` and the `O_NOFOLLOW` refusal remain unreachable through
`SimDisk`, and that is stated rather than papered over.** `SimDisk` is a flat
in-memory key table with no inodes, links or advisory locks, so a flock and a
symlink cannot exist in it. Of the two honest routes — grow `SimDisk` a
lock-and-symlink model, or drive the two opens against a real directory —
`RunWALRealFSGuards` takes the second, on the ground that a modelled flock would
prove the model while a real second `wal.Open` exercises the syscall the guard is
made of. (`os.MkdirTemp` where the SimDisk seam does not exist is already
precedent, rmp #2466.) It asserts a second `wal.Open` of a locked path returns
`wal.ErrWALLocked` — `flock(2)` binds to the open file description, so the two
opens conflict within one process and no subprocess is needed — that closing the
first releases it, and that a symlinked final component is refused at **both**
`O_NOFOLLOW` sites: the WAL data file and the `LOCK` sentinel, which is opened
before any WAL data is touched. The victim file outside the directory is verified
byte-unchanged, which is the property that actually matters (CWE-59, security
finding #1843). The consequence to record: **these two guards sit outside every
seeded, crash-injecting scenario in this package, and will while `SimDisk` has no
link or lock semantics.** The adjudicator makes no claim at all on a platform that
cannot express them, so a skip never reads as a pass.

Every gate here is proved falsifiable as well as satisfied — a doctored record for
each clause, plus the live per-frame control for contiguity.

### Transaction-size caps: producer refusal and replay fail-stop (rmp #2474)

The store bounds a single transaction on **both** sides, and for one reason:
recovery buffers a whole transaction's ops in memory before applying them on its
`OpCommit` marker, so a producer able to commit an arbitrarily large transaction
could write a WAL that recovery cannot replay without allocating in proportion to
it. That is the CWE-770 shape of the persistence layer. Two typed sentinels
answer it:

| Bound | Default | Refusal |
|---|---|---|
| Producer (`txn.Tx.appendOnly`) | `txn.DefaultMaxTxnOps` = 16 000 000 | `txn.ErrTransactionTooLarge`, **before** a sequence is minted or a frame written |
| Replay (`recovery.Options.MaxTxnOps`) | the same value | `recovery.ErrTransactionTooLarge`, classified by `tailErrIsCorruption` as genuine corruption |

**Neither sentinel had ever been produced under simulation, and the workloads
were not the reason.** `simStoreConfig.maxTxnOps` was plumbed carefully — through
`recovery.OpenFS` on the full-stack path and `recovery.ReplayWAL` on the WAL-only
one — and reached **recovery and nothing else**: the store itself was built with
the uncapped `txn.NewStoreWithOptions`, so the replay bound was configurable and
the commit bound was not. Lowering the cap could never make the producer refuse,
and reaching 16 000 000 ops by workload is not a test but an out-of-memory. The
`overload` actor's own comment records the gap: it pushes "toward"
`DefaultMaxTxnOps`, and nothing ever arrived. `simstore.go` now passes the cap to
`txn.NewStoreWithOptionsCapped` as well; the change is behaviour-neutral for
every existing caller, all of which carry `0` and resolve to the same default as
before.

**A typed error is not the assertion.** A refusal that appended frames and then
truncated them is how a bug becomes permanent loss (rmp #2526), so the producer
arm reads the whole durable WAL image off the `SimDisk` before and after each
refused attempt and requires them **byte-identical**, and reads the live graph's
node count across the same boundary. Measured at a deliberately small cap of 32
ops:

| attempt | ops | outcome | WAL bytes | live order |
|---|---|---|---|---|
| `warmup` | 8 | committed | 0 → 436 | 0 → 4 |
| `one-over` | 33 | **refused**, `txn.ErrTransactionTooLarge` | 436 → 436, **identical** | 4 → 4 |
| `far-over` | 128 | **refused**, same sentinel | 436 → 436, **identical** | 4 → 4 |
| `at-cap` | 32 | committed | 436 → 2084 | 4 → 20 |

The at-cap transaction is driven **after** the refusals deliberately: a refusal
that had silently poisoned the writer fails there rather than passing as a clean
refusal. The reopen then recovers clean and equal to the model, with all 80 keys
of the two refused transactions absent.

**The boundary is measured, not inferred from the two comparisons.** The producer
refuses when `len(ops) > cap`; recovery stops when, before appending another
frame, `len(pending) >= cap`. The operators differ because the counts are taken
at different moments, and the arms pin what that actually yields: **32 ops
commits and replays, 33 does neither** — the two caps agree exactly, which is the
`producer <= replay` invariant `txn.DefaultMaxTxnOps` documents.

**The oversize WAL is built by hand, because the engine cannot produce one.**
The producer cap is `<=` the replay cap by construction, so any transaction large
enough to trouble recovery is refused before a frame is written — that is the
whole point of the pairing. The file is therefore constructed one v3 op payload
at a time and written through the real `wal.Writer`, so only the op stream is
hand-made and the framing, CRC and fsync are the production ones. Measured with a
replay cap of 16 over a 4-op committed prefix:

| arm | run ops | cap | outcome | ops applied |
|---|---|---|---|---|
| `at-cap` | 16 | 16 | clean | 20 (prefix + run) |
| `over-cap` | 17 | 16 | **fail-stop**, `recovery.ErrTransactionTooLarge` | 4 — exactly the committed prefix |
| `over-cap-unlimited` | 17 | unlimited | clean | 21 |

The harness store-open is checked alongside recovery's own report: an embedder
that swallowed the fail-stop would append onto the corruption and embed it
permanently, so the `over-cap` image must be **refused** by `openSimTypedStore`
with the sentinel intact, and the two clean arms must not be.

**Three sensitivity seams pin the oracles, and two drive the real defect rather
than fabricated evidence.**

- `txn.MaxTxnOpsUnlimited` runs the byte-identical producer plan and commits all
  four transactions, so the capped arm's refusals are attributable to the cap and
  not to those op counts being rejected for some reason of their own.
- The `over-cap-unlimited` replay arm replays the **byte-identical** 954-byte file
  with the cap disabled and recovers all 21 ops, which rules out a file the
  harness simply built wrong. It doubles as the standing proof that the
  hand-written v3 payload layout matches `store/txn`'s unexported encoder: a wrong
  version tag, kind, sequence width or body layout could not decode into exactly
  the nodes the frames name.
- `simStoreConfig.uncappedProducerSeam` restores the pre-#2474 plumbing — the cap
  reaching the replayer and not the producer — and **measures the hazard the
  invariant exists to prevent**, which is worse than a missed refusal: the 33-op
  transaction is acknowledged durable, and recovery then refuses to replay the
  file at all. The store does not lose that transaction; it **fails to reopen**,
  and every committed transaction in the WAL becomes unreachable behind a
  fail-stop.

The non-vacuity gate is **separate and shape-only**, so an uninformative run never
reads as a faulty one: it requires an attempt genuinely larger than the reference
cap, a **non-empty** WAL underneath it — a byte-unchanged assertion over an absent
file is satisfied by definition — and at least one transaction actually
committed. Every clause of both verdicts and both non-vacuity gates is proved
falsifiable by perturbing a hand-built control one field at a time, with the
unperturbed control silent.


### csrfile access patterns, weight kinds, and what truncation does NOT reach (rmp #2478)

The csrfile arm published **one** fixture before this task: a `float64`-weighted
CSR read back through the default access pattern. Four of the five
`csrfile.WeightKind` values were therefore never written by the simulator,
`csrfile.AccessPattern` and `Reader.SetHint` were never called at all, and
`csrfile.Reinterpret`'s alignment precondition was never probed. The arm now
enumerates the whole 5 x 5 grid. Five things it measured are worth recording,
because three of them contradict what the surface suggests.

**There are five access patterns, not three.** `AccessDefault`,
`AccessSequential`, `AccessRandom`, `AccessWillNeed` and `AccessDontNeed`.
`store/csrfile`'s own `TestReader_SetHint` drives four of them; `AccessDontNeed`
was reached by no test in the repository before this task, and it is the one that
makes "a hint does not change the data" a real question rather than a formality —
on a live mapping it tells the kernel to drop the resident pages, so the read that
follows must fault them back in and yield the same bytes.

**The in-memory disk cannot reach madvise, and a green matrix over it is not
evidence that it did.** `Reader.SetHint` short-circuits when there is no mapping
to advise, and the DST filesystem seam produces exactly that: `csrfile.OpenWith`
over a non-OS backend reads the whole image into a heap buffer and leaves the
Reader's `mm` nil, so `SetHint` returns nil **before** the platform call. Every
cell of the in-memory grid therefore proves the CONTRACT — a hint is accepted on a
live reader, refused with `ErrReaderClosed` on a closed one, and alters no byte —
and none of them proves the syscall ran. The madvise path is reachable only
through `csrfile.Open`, which always mmaps; one test
(`TestCSRFileMatrix_MadviseOverRealMapping`) drives all five patterns against a
real temp directory for that reason. A second measured fact belongs with it: an
**out-of-range** `AccessPattern` is not rejected — the switch falls through to
`MADV_NORMAL` and `SetHint` returns nil — so `SetHint` validates nothing.

**An aligned truncation and a misaligned one behave identically, and the reason is
structural.** `Header.validate` compares the file's length against the ONE
canonical layout for its counts and demands EXACT equality, so alignment never
enters the decision. Measured on a 964-byte file: a cut at 128 (a multiple of
`Alignment` = 64), a cut at 277 (a multiple of neither 64 nor 8), a cut at the
edges offset, a cut at the CRC offset and a cut one byte short all produce the
same `ErrHeaderInconsistent`, which wraps `ErrFileCorrupted`. The only threshold
that changes the answer is `HeaderSize+4`: **67 bytes gives `ErrHeaderTooShort`
and 68 gives `ErrHeaderInconsistent`**, because below 68 the length gate fires
before the header is decoded. The two backends diverge at exactly one length —
zero. The byte-backed reader reports `ErrHeaderTooShort`; the mmap path fails
earlier, because `mmap(2)` refuses a zero-length mapping, and surfaces an
**untyped** wrapped syscall error that no `errors.Is` against a package sentinel
will match.

**`Reinterpret` refuses by PANIC, and truncation cannot reach its alignment
half.** There is no error return: a short buffer, a negative `n` and an `n` whose
byte requirement overflows all panic, and the refusal must be caught with
`recover`. More importantly, its alignment precondition is on the **base address**
of the buffer, which truncating a file cannot change — truncation only ever trips
the LENGTH half. The alignment gate is therefore probed the only way it can be:
by sweeping all eight byte phases of a buffer, of which exactly one is 8-byte
aligned. The measurement is 1 accepted, 7 refused with "base address not aligned
to 8 bytes". `n == 0` is the documented non-refusal — it returns nil without
inspecting the buffer at all — so an alignment probe written at `n = 0` would
prove nothing.

**`WeightAbsent` is distinguishable from a weighted file on four independent
signals, and collapses in exactly one case.** Over one shared topology the
unweighted file differs in its header kind, in `WeightsRaw()` being nil, in
`WeightsOffset` being 0, and — the signal that owes nothing to any header field —
in being strictly smaller (measured: 580 bytes unweighted, 708 with a 4-byte
weight, 836 with an 8-byte one). The collapse is the csrfile-side shape of what
rmp #2526 fixed in the snapshot: a CSR declared at a weighted Go type but
carrying an **empty weights slice at runtime** is downgraded by the writer and
lands on disk **byte-identical** to a graph that never had weights. That is
pinned rather than tolerated silently, and it is also what makes the coverage
gate honest — the gate counts which kinds were OBSERVED in the published headers,
so driving the `float64` arm with an empty weights slice registers as `float64`
**unreached**, not as `float64` covered.

One thing found here is out of this task's scope and filed as **rmp #2529**:
`weightKindOf` advertises `int`, `uint` and `uintptr` as `WeightUint64`, but
`binary.Write` refuses them ("some values are not fixed-sized in type `[]int`"),
so a publish at those types always fails — cleanly, leaving neither the file nor
the temp behind, but failing. The arm table stays on the four widths that
round-trip, plus `struct{}`.

Every verdict here is proved falsifiable by a control that drives it red: the
round-trip check is run against a DIFFERENT topology and against the same
topology with one weight value changed (so it cannot be passing on length
alone), the truncation check is run against a file that was not truncated (which
makes all three of its clauses fire at once), the size-discrimination check is
fed collapsed sizes, and the alignment sweep is fed both a gate that refuses
nothing and one whose refusals come from the length check.


### Which counter proves the fault fired (rmp #2479)

A fault scenario that asserts the **effect** of a fault without confirming the
faulted code path was **entered** proves less than it appears to. rmp #2465 had
to establish that the mid-publish crash window was genuinely entered before its
durability verdict meant anything; rmp #2471 gated group-commit coalescing on a
metric and then found the metric itself could be satisfied by an unrelated path;
rmp #2478 found the in-memory backend silently skips `madvise`, so a green matrix
would have been evidence of nothing.

The simulator's metrics oracle read **four** counters, all from the Cypher layer:
`cypher.Run`, `cypher.RunInTx` and their paired `.errors`. Every storage- and
Bolt-layer metric emitted across the module was unasserted — so the counter that
would prove a fault fired was precisely the one nothing read.
`internal/sim/metrics_required.go` closes that. Each fault scenario now
**declares** the counters its faults must move, and failing to move a declared
counter is a violation: a coverage precondition in the shape rmp #2470/#2471
established, kept apart from the scenario's own verdict because a declaration
that did not fire means the **run** proved nothing, not that the **engine** is
broken.

**Every name was driven out, not read off a list.** `docs/metrics.md` carries an
inventory of wired metric names and nothing here was taken from it. Each
scenario was run with a recording sink installed and the arriving names were
read; the floors are the structural counts the scenario fixes (one per publish
window, one per corrupted component), not the total one run happened to produce.
Two of the declarations would have been wrong if written from the obvious guess:
the cadence arm never moves `store.checkpoint.RunCheckpoint.errors` at all,
because the cadence environment drives the checkpointer through its own fold
callback, and the csrfile arm moves nothing whatsoever.

**`store.wal.Decode.errors` is the trap, and it is not hypothetical.**
`store/wal/format.go` increments it on **every** decode failure class, including
the `io.EOF` / `io.ErrUnexpectedEOF` path that yields `wal.ErrTornFrame` — the
ordinary shape of a WAL whose writer was killed mid-write, with no corruption
anywhere. It is the only fault counter the WAL-corruption arm emits, so declaring
it alone would be satisfied by a benign crash tail. The discriminator is a
**control**, the standing guard rmp #2471 kept for the same reason:
`runWALCorruptionFailStopWith` runs the identical scenario with the interior byte
flip withheld — same commits, same clean close, same clean and prefix replays,
same reopen — and the control requires the counter to stay at **0**. Measured: 2
with the flip, 0 without.

**The csrfile publish arm is metrics-blind, and the blindness is now asserted.**
`store/csrfile` contains no reference to `internal/metrics`, and a full driven run
of the scenario emitted **zero** metric names of any kind. The atomic publish
protocol (tmp write -> fsync -> rename -> parent-dir fsync) is entirely
unobserved: no counter can witness the ENOSPC bound or the armed `Sync` fault,
and borrowing one from a neighbouring layer would be exactly the non-unique
declaration this task exists to prevent. So the declaration states the blindness
and pins it — no name under `store.` may be emitted while the arm runs — which
turns "we declared nothing" from a vacuous pass into a falsifiable claim. The day
csrfile gains a counter, the declaration fails and must be replaced by the real
name.

**Eight of the nine snapshot components carry a unique witness; the mapper does
not.** The aggregates (`store.recovery.OpenCtxFS.errors`,
`store.recovery.openCodec.errors`, `store.snapshot.LoadSnapshotFull.errors`) move
for **any** reopen failure, WAL-side included, so none of them can say the damage
was seen where it was done. The per-component decoder counters can, and
`ReadCSR`, `ReadLabels`, `ReadProperties`, `ReadTombstones`, `ReadEdgeHandles`,
`ReadConstraints`, `ReadIndexDefs` and `ReadManifestFile` all move on their own
arm. `store.snapshot.ReadMapperString.errors` does not: the mapper arm's damage
is caught before that decoder is reached, so the mapper is witnessed only by the
aggregates. That is recorded rather than papered over, and logged as a witness on
every run.

The declared arms, with the counters as **observed**:

| Scenario / arm | Required counters | Uniqueness |
|---|---|---|
| `csrfile-publish-fault` | *(none — metrics-blind; `store.` asserted silent)* | n/a |
| `wal-corruption-failstop` | `store.wal.Decode.errors` >= 2 | shared with the benign torn tail — control arm discriminates |
| `checkpoint-dirfsync-fault` | `store.wal.TruncatePrefix.errors`, `store.wal.Close.errors` >= 1 (unique); `store.checkpoint.RunCheckpoint.errors`, `store.wal.Append.errors`, `store.wal.Sync.errors` >= 1 | the truncate failure is unique; the append/sync/close triple is the **poison signature** downstream of it |
| `checkpoint-crash-storm` | `store.recovery.snapshot.promoteParentFsync` >= 1 (unique); `store.checkpoint.RunCheckpoint.errors`, `store.snapshot.WriteSnapshotFullCtx.errors` >= 3 | the promote counter has one emission site in the module; the other two are required at the **window count** |
| `snapshot-corruption-failstop` | three aggregates >= 9; eight per-component `Read*.errors` >= 1 | aggregates shared, per-component decoders unique |
| `db-teardown[fault-on-close]` | `store.DB.Close.errors`, `store.wal.Close.errors` >= 1 | both unique: the teardown failed **and** it failed at step 3, where the fault was armed |
| `checkpoint-cadence[transient-fault]` | `store.snapshot.WriteSnapshotFullCtx.errors`, `store.snapshot.WriteCapture.errors` >= 1 | shared — the clean cadence arm is the standing control |

**Three disable-a-fault proofs, on three different scenarios**, show the
declarations are load-bearing rather than incidentally satisfied. Withdrawing the
WAL byte flip, withdrawing `FaultOnClose` from the teardown, and running the
clean cadence arm each leave **every** declared counter at zero, and each proof
asserts that the specific declared counters are the ones reported missing — not
merely that something failed. A separate, shape-only gate
(`CheckCounterDeclShape`) reads no run at all: it rejects a declaration that
names nothing, that claims blindness and counters at once, that sets a floor of
zero, or that admits a shared counter with no discriminator — every form that
would pass by saying nothing.


### graph/io completeness: DOT, the property path, the caps, and cancellation (rmp #2480)

`io-roundtrip-fault` drove two edge-list formats and one property format before
this task. Four things were therefore untouched, and each was invisible to a
green suite for the same reason: nothing referenced them at all.

**The DOT writer has no reader, so a round-trip cannot adjudicate it.**
`graph/io/dot` exports and never imports, which is why it was imported nowhere in
the simulator. It is now adjudicated by **cross-format agreement**: the same
model is written as DOT, as CSV and as JSONL, and the three must describe the
same graph. The DOT text is read back by a character scanner in
`internal/sim/graph_io_surface.go` rather than by a line split, because the
writer quotes an identifier containing the edge operator or a statement
terminator and a line-oriented parser would mis-split exactly the identifiers the
arm exists to drive. The model is built to force every branch at once: a DOT
reserved keyword (`graph`), identifiers carrying a space, a quote, a backslash,
`->`, a comma and a leading `-`, the empty identifier the engine accepts (rmp
#2043), zero and non-zero weights, and one isolated vertex. The measured census
at the pinned seed is 36 quoted identifiers, 13 weight labels and 1 bare node
statement over 26 edges.

**The one legitimate format disagreement is asserted in SHAPE, not waived.** An
edge-list CSV cannot encode a vertex with no incident edge. Rather than compare
only the edges, the arm asserts that the CSV vertex set is **exactly** the model
minus the isolated vertex and that DOT and JSONL both carry it — so a format that
began losing ordinary vertices would fail, where "the formats differ, never mind"
would not. The non-vacuity gate refuses a run in which CSV carried as many
vertices as the model, because the disagreement the verdict adjudicates could
then not arise.

**Three of the eight caps in the audit list are WRITER-side and unreachable from
a mutated export.** `ErrPropertyValueTooLarge`, `ErrPropertyNestingTooDeep` (both
packages) and `graphml.ErrInvalidXMLChar` are raised by the encoders
(`graph/io/jsonl/writer.go`, `graph/io/graphml/writer_props.go`), so they are
provoked by handing the writer a hostile GRAPH, not by feeding a corrupted file
to a reader. `graph/io/csv` also has no `*CappedCtx` variant at all — its ceiling
is the `Options.MaxBytes` field — and it carries a sentinel the list omitted,
`ErrTooManyFields`, as `jsonl` does with `ErrUnknownType`. The verified surface
is 14 sentinels, declared with their side in `GraphIOGuardDecls`; **13 are
provoked and matched with `errors.Is`** on every run, and the verdict fails when
a cap declared reachable was not reached, so deleting a probe cannot quietly
reduce the coverage.

**The two size caps and the two depth caps need DIFFERENT payloads, and a single
one would reach only one of them.** The encoders check depth on the way DOWN and
size after serialising on the way back UP, and the nested-list wire grows ~2x per
level, so a nested list carrying data trips the 64 MiB **size** cap at depth ~24
and never reaches the depth ceiling of 128. The size probe is therefore a single
oversized value and the depth probe is 130 nested EMPTY lists, which costs
0.3 MiB and no serialisation at all.

**One cap is structurally unreachable, and the reason is asserted rather than
assumed.** `jsonl.ErrListTooDeep` fires at nesting depth 64. The wire embeds each
level as a re-escaped JSON string, so the encoded size roughly doubles per level
— measured 112 bytes at depth 1 and 2,097,401 bytes at depth 18, a ratio of
**2.00x per level** — which puts an input reaching depth 64 at order 2^67 bytes.
The declaration carries that reason, and the run still adjudicates it: the
measured growth ratio must stay at or above 1.9x, the extrapolated size at the
guard's depth must stay above 2^62 bytes, and the deepest nesting that still
round-trips must stay well above the trivial, so a change to the encoding or to
the depth ceiling that made the guard reachable fails the run instead of leaving
a declaration quietly stale.

**Bounded allocation is measured, not assumed — and what each bound proves
differs by probe.** Every `ErrInputTooLarge` probe is fed an **unbounded**
generator with a 64 MiB safety ceiling, so without the cap the reader would not
stop; reaching the ceiling is itself a violation ("the cap did not stop the
reader"), which is what makes those bounds decisive rather than decorative. The
crafted probes (`ErrTooManyKeys`, `ErrTooManyData`, `ErrTooManyFields`) bound the
per-element amplification the guard exists to refuse. The two writer size caps
are checked **after** the value is serialised, so their bound is a ceiling on the
encoder's blow-up and is documented as such rather than presented as a
zero-copy claim. Measured under `-race`: 5.8-13.5 MiB for the endless probes,
107.6 MiB for the `<key>` flood, 320.3 MiB for each 64 MiB size cap.

**Mid-parse cancellation is now driven on all five `*Ctx` readers.** Every one
checks `ctx.Err()` once per 4096 units — rows for the CSV and JSON Lines readers,
**edges** for both GraphML readers — so a short document runs to completion and
proves nothing. The arms import a 12,000-edge chain and cancel from inside the
`io.Reader`, at the byte offset of the 5,000th unit, so the cancellation is
observed at the check at unit 8,192 with thousands of units already folded in.
All five return an error wrapping `context.Canceled` and a **nil** graph, so no
partial graph escapes; each is paired with an uncancelled control over the same
bytes that must reproduce the model exactly, because "the reader returned nil" is
otherwise satisfied by a reader that always returns nil.

**The caps are crafted, not seeded, and the split is recorded as an
assertion.** No byte flip or truncation of an ordinary export produces 65,537
`<key>` declarations, so the caps are driven deterministically once rather than
on every seed. The seeded sweep does what a seed sweep can: four corruptions
(byte flip, truncation, spliced prefix, delimiter run) against four importers,
requiring no panic, a genuinely changed artefact, at least one semantically
effective mutation per format, at least one rejection overall, and allocation
bounded at 64x the bytes fed in. A test asserts that not every mutation reaches a
typed cap — if it did, the crafted battery would be redundant, which contradicts
its stated purpose.

Every gate here is proved falsifiable by a synthetic result that drives it red:
ten for the surface non-vacuity gate (each removing one piece of evidence the
verdict rests on), eleven for the cap and cancellation verdict (an unprobed cap,
a wrong sentinel, a panic, an overrun ceiling, a blown heap bound, a wire that
stopped re-escaping, a depth guard firing early, an untyped cancellation, an
escaped partial graph, a cancellation landing before parsing began, and a control
that did not reproduce the model), and six for the declaration shape gate.


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
(rmp #2441), which since rmp #2469 checkpoints inside its traffic and
adjudicates the MVCC clock and the transaction sequence across the snapshot
boundary. The checkers found four engine isolation defects on arrival
(rmp #2445, #2446), all fixed and regression-pinned.

## Bolt wire-surface coverage

### The authentication surface, and why the WAL is the witness (rmp #2481)

Every SimServer in `internal/sim` was constructed with
`server.NoAuthHandler` — a handler that returns success for any scheme, any
principal and any credential. The consequence is stronger than "the credential
path was untested": an assertion made against that handler is **vacuous**,
because it asserts the absence of a check nobody installed. A probe that sent a
wrong password and then observed a successful write would have been observing
correct behaviour. So the gap could not be closed by adding arms to the existing
servers; the harness first needed a server that genuinely refuses, which is what
`NewSimServerAuth` provides (`BasicAuthHandler` over `ConstantTimeValidate`).

Four facts about the surface came out of reading the server rather than
describing it, and each one shaped an arm.

**The credentials arrive on two different messages, handled by two different
functions.** On Bolt >= 5.1 `handleLogon` authenticates and `HELLO` carries only
driver metadata; on <= 5.0 `handleHello` authenticates inline. Covering one
leaves the other untested, and the harness's `WireClient.Handshake` always leads
with 5.6 — the server correctly picks the highest version it is offered — so the
inline path was unreachable until `HandshakeOffering` was added to withhold the
newer versions. The wrong-password arm therefore runs twice, at 5.6 and at 4.4.

**A first authentication failure and a re-authentication failure are different
branches.** A failed first `LOGON` (or a failed inline `HELLO`) sets
`StateDefunct` and the connection closes; a failed `LOGON` from `READY`/`TX_READY`
calls `enterFailed`, which reclaims any open transaction and leaves the session
recoverable through `RESET`. Both are driven, and the second is the one that can
leave an explicit transaction open, so it is also where a reclaim defect would
show.

**`LOGOFF` from `TX_READY` leaves the session in `TX_READY`.** The state machine
does not close the transaction; only `s.authenticated` changes. That is precisely
why `handleCommit` and `handleRollback` carry their own authentication gate
(CWE-306, audit 2026-07-13 security F5) rather than relying on the state machine
— and it is what makes the `commit-after-logoff` arm the sharpest one in the set:
the write is already staged in the engine, and one boolean stands between it and
the durable log.

**A FAILURE reply proves the server SAID no, not that nothing happened.** This is
the reason the scenario is backed by a real WAL (`SimStore` over `SimDisk`) rather
than the in-memory engine every other wire scenario uses. Each arm is bracketed
between two readings of `wal.Writer.Stats`, and a refused arm must leave both the
frame and the byte counter exactly where it found them. The sentinel node its
statement would have created is then censused twice — in the live engine, and in a
graph reopened through real recovery after a crash — because a frame appended but
not yet visible would hide from the live census alone.

The exact failure **code** is pinned per arm rather than "some failure", since
mapping one onto another changes what a driver is told. Measured:
`Neo.ClientError.Security.Unauthorized` for a bad credential (both entry points,
and both branches),
`Neo.ClientError.Security.AuthProviderFailed` for an unknown scheme, and
`Neo.ClientError.Request.Invalid` for every de-authorised transition — the
illegal-transition code, not a security code, because the session reaches those
gates through `failTransition`.

**A shared failure code cannot attribute a refusal, so the ORIGIN STATE does.**
The authentication gate and the state-machine gate both answer
`Neo.ClientError.Request.Invalid`, because both go through `failTransition`. A
code-only assertion is therefore blind to the case that matters: if `LOGOFF`'s
target state regressed, `commit-after-logoff` would be refused by the *state*
check one line above the auth check, the arm would still see the expected code,
and the CWE-306 gate would be untested behind a green scenario. `failTransition`
names the state the session was in, which is exactly the discriminator — a refusal
by the auth gate names a LEGAL state, one by the state machine names `FAILED` —
so every gate arm pins it. Measured: `... in state AUTHENTICATION`,
`... in state READY`, `... in state TX_READY`, `... in state TX_STREAMING`,
`... in state NEGOTIATION`, `... in state FAILED`.

**Four arms exist because a security review asked what the roster still could not
see.** `route-after-logoff` completes the five auth-gated verbs and is the only
one whose violation neither the WAL counter nor the census could ever catch — a
leaked `ROUTE` writes nothing, so it needs its own assertion or none.
`logoff-in-tx-streaming` asserts the guard that lets `handlePull` and
`handleDiscard` run with NO authentication gate of their own: the flag can only be
cleared by `LOGOFF`, and `LOGOFF` is illegal in the streaming states, so a session
cannot become de-authorised mid-stream. That edge — load-bearing for two ungated
handlers — was driven by no test at any level. `reset-after-logoff-open-tx`
reaches the reclamation limb of `handleReset`, which every existing RESET test
misses because they all run with no transaction open; it asserts that RESET
discards the staged write and returns an unauthenticated connection to
NEGOTIATION, where a bare `LOGON` is illegal. And
`second-message-after-refusal` pins the scoping of the soft-IGNORE: `dispatch`
softens a request in FAILED to `IGNORED` only when the session is still
authenticated, so a de-authorised client must get a hard FAILURE — dropping the
`&& s.authenticated` half would have broken no other arm, because every one of
them stops at its first refusal.

**The instrument is shown moving in the same run.** Two arms are ADMIT arms: an
honest authenticated write, and a write after re-authenticating following
`LOGOFF`. Both must ADVANCE the counters (measured +4 frames, +183 and +188 bytes)
and both nodes must survive recovery. The second carries a second duty: a server
that refused every post-`LOGOFF` write — including the legitimate one — would
satisfy every refusal clause in the scenario and fail only here.

Non-vacuity is adjudicated by a separate shape-only gate (rmp #2470): the full
arm roster must have run, refusals and admissions must both have occurred, the
frame counter must have been observed moving, and all three failure codes must
have been seen. The **control** is a real alternative configuration rather than a
perturbed value — the identical wrong-password exchange against a
`NoAuthHandler` server must be ADMITTED — which is what attributes the refusals
to the `AuthHandler` and not to the state machine, the framing, or a mistake in
the harness. Every clause of both adjudicators is additionally falsified by
injection into the pure checker: 22 single-field perturbations, each required to
produce a violation naming its own defect.

The three new abuse families are classified by what they need, not by what they
are. `AbuseLogoffThenRun` and `AbuseCommitAfterLogoff` are refused by ANY server,
because the gate they reach is the session's own `authenticated` flag, so they
join the randomly-drawn set that runs against the NoAuth `bad-actors` server.
`AbuseBadCredentials` is admitted by `NoAuthHandler` — correctly — so
`BoltAbuser.PickFamily` must never draw it there, and a test drives it against
both server kinds to prove the distinction is real rather than decorative.

### TLS certificate rotation under fault (rmp #2481)

`server.CertReloader` had unit tests for its happy path, a parse failure and
missing files; what it had never been driven through is the sequence an operator
actually produces. The scenario runs seven steps — initial load, clean rotation,
torn key, garbled key, absent key, mismatched pair, completed rotation — and
three things about it are worth recording.

**The oracle is a real TLS handshake, because the dangerous failure mode parses
cleanly.** A cert rotated without its key leaves two files that both decode
perfectly and no longer belong together. `crypto/tls` is the independent
reference that can see it: the verifier completes a genuine TLS 1.3 handshake
over a `net.Pipe` against whatever `GetCertificate` currently serves, with the
client trusting exactly that certificate and verifying the SAN. A successful
handshake proves the certificate parses, the name matches, and the private key
corresponds to the certificate's public key. It deliberately does NOT trust a
pre-agreed root, so it cannot by itself detect that the WRONG pair went live —
that is settled separately by the served leaf's Common Name, and crediting the
handshake with it would overstate what it checks. Measured across all four
faults:
`rotation-B` stayed in service and kept handshaking, and `rotation-C` took over
the moment its key landed. The verifier's own falsifiability is proved by pointing
it at a certificate issued for a different name, which `crypto/tls` rejects.

**Three documented contract halves were untested and are now pinned.** An
unloaded `CertReloader` refuses to serve rather than returning a nil certificate
the TLS stack would dereference; `NewCertReloader` over a torn key fails closed —
the initial load is mandatory, and a reloader that started on unparseable material
would put a server into service with no certificate at all; and the `Watch`
poller's `onError` callback is now asserted to FIRE over a broken pair. That last
one was reachable by nothing in the module: `onError` appeared only in
`tls_reload.go` itself, every caller passed a discarding closure, and deleting
`r.onError(err)` from `Watch` would have broken no test — even though it is the
only operator-visible signal that an unattended rotation failed and a stale
certificate is still in service. It is the same defect class as the
`Options.Logger` bypass this sprint fixed, and it is now evidence: measured 2
deliveries per broken-pair arm.

One latent defect in the scenario itself was found the same way and fixed. The
fixtures carry fixed validity bounds so their bytes are seed-reproducible, but the
verifying handshake originally evaluated them against the real clock — which made
the whole scenario a TIME BOMB that would start failing on 2036-01-01 for a reason
having nothing to do with the code under test. Both sides of the handshake now
pin `tls.Config.Time` to a fixed instant inside the window.

**The torn key is produced by an actual crash, not by writing a short file.** The
first draft of this section claimed the opposite — that `SimDisk.CrashHost` never
discards un-synced data, so the truncation had to be faked. That was a
restatement of the model rmp #2535 *replaced*: #2535 is the fix that made fsync
load-bearing, and since it landed each file carries a durable image advanced only
by a `Sync` that returned nil, with `CrashHost` reverting to that image (power
failure) and `CrashProcess` keeping the bytes (SIGKILL). The claim was corrected
by reading `internal/sim/disk.go` rather than by trusting the note, and the arm
now uses the real mechanism: write the prefix, `Sync` it, write the remainder,
leave it un-synced, `CrashHost`. Measured: 85 of 119 bytes survive, and the arm
fails loudly if a crash ever discards nothing. `SimDisk` is likewise the image
authority for the garbled arm (`SimDisk.CorruptRange`). Only the projection onto
a real temporary directory leaves the simulated disk, and it must: `CertReloader`
reads through `os.Stat` and `tls.LoadX509KeyPair` and exposes no filesystem seam,
so growing one purely for this scenario would change a production API rather than
test it. The precedent is `wal_writer_surface.go`.

Two details make the run reproducible and the roster honest. The fixtures are
**Ed25519 with fixed validity bounds**, so the PEM bytes are a pure function of
the seed; an ECDSA pair, whose signature draws randomness, would regenerate
differently every run. And because the torn and garbled arms produce the
IDENTICAL parse error (`tls: failed to find any PEM data in key input`), the
non-vacuity gate compares the key sizes each step left on disk — a torn key must
be strictly shorter than the key it truncates (measured 85 of 119 bytes), a
garbled one exactly as long (119 of 119), an absent one zero — so the scenario
cannot claim two faults where only one was ever applied. The gate also requires at
least one reload to have succeeded, at least one to have failed, and the
certificate in service to have genuinely changed, since a reloader that ignored
every rotation would pass every retention clause.

One projection detail is load-bearing and easy to lose: the mtimes are stamped
**explicitly**, one second apart. `CertReloader.Reload` short-circuits when
neither file's mtime has advanced past the last successful load, so on a
filesystem whose timestamp granularity is coarser than the time two consecutive
projections take, an honest rotation would be silently skipped and the scenario
would be measuring the clock instead of the reloader.

### The transaction registry, the idle reaper and the per-principal cap (rmp #2482)

`Server.Transactions`, `Server.TerminateTransaction`, `Options.MaxTxIdleTime` and
`Options.MaxOpenTxPerPrincipal` were all added after the round-3 comparative
audit demonstrated a whole-server stall from one abandoned `BEGIN` (rmp #2175,
#2176). Six tests in `bolt/server` cover them, and between them they establish
what follows.

**What the six pre-existing tests already covered.**
`tx_introspection_test.go` covers the listing's FIELDS (id, principal, mode,
remote, query, state, a non-zero `StartedAt` and a positive `Elapsed`), that a
termination rolls back atomically, that a NEVER-SEEN id returns
`ErrNoSuchTransaction`, that an idle offender blocks neither a reader nor a
writer, and that both read and write transactions are listed oldest-first.
`abandoned_tx_test.go` covers the idle reaper bounding a reader stall, a BUSY
transaction NOT being reaped, the per-principal cap refusing with a typed error,
one slot being returned by a client `ROLLBACK`, and the cap being per-principal
rather than per-server.

**What a wall clock cannot reach, and what the fake clock adds.** All six run on
real time, and that is not a stylistic difference — it is a ceiling on what they
can assert:

- **Exact instants.** A test that does not know what the server's clock read when
  an entry was registered can assert no more than `Elapsed > 0`. Driving the
  server's clock through `Server.SetClock` makes the harness the sole author of
  every instant, so `StartedAt` is EXACTLY the fake instant it opened at and
  `Elapsed` is the listing instant minus that, to the nanosecond. A registry that
  stamped every entry at once, or that computed `Elapsed` against wall time while
  the clock was injected, passes `Elapsed > 0` and fails this.
- **An ordinal instead of a timescale.** `TestAbandonedTx_IdleReaperBoundsTheReaderStall`
  asserts an order of magnitude — the reader unblocked nearer the 300 ms idle
  bound than the 20 s total bound — because a real-time test cannot do better
  without becoming flaky. On virtual time the reap lands on a specific ADVANCE,
  predicted before the run by an independent model of the rule and compared
  exactly. A reaper one advance early or late is then a failure rather than
  noise, which is precisely the deviation the arm's live control produces on
  purpose by shortening the server's bound by one step.
- **Quiet ordinals.** Real time cannot assert that the reaper DECLINED to reap at
  a particular moment. The staggered plan gives five advances at the front of the
  measured sequence at which nothing may be reclaimed, so a reaper that emptied
  the registry on its first fire is caught.
- **Reaper-free attribution.** A termination test on real time cannot rule out
  that a bound fired instead. Arm 2 installs both bounds at ten minutes of FAKE
  time and makes ZERO advances; `clock.Fake` delivers only from `Advance`
  (`internal/clock/fake.go`), so no timer it armed can possibly fire and every
  departure from the registry is provably the operator call's doing. That is
  asserted as a non-vacuity clause rather than argued in a comment.
- **Minutes of transaction lifetime in milliseconds of wall time.** The scale arm
  drives 64 transactions through 70 advances — 13.3 s of simulated time — in
  412 ms of real time under `-race`, and the churn arm drives 3m20s of simulated
  time in 97 ms.

**Three properties the pre-existing tests do not reach at all.**

1. **Successor immunity.** `txRegistry` documents that a stale id can never
   terminate whatever transaction the same connection opened next, and nothing
   tested it. Two mechanisms implement it: `txRegistry.nextID` mints
   `"<sessionID>-<seq>"` from a server-wide counter that only ever increases, and
   `Session.unregisterTx` drains any terminate request that arrived for the
   transaction just ended. The arm exercises the first directly — the stale id is
   refused by the registry lookup, which sends no signal at all — and asserts the
   OBSERVABLE property for the second across a settle window, because the
   interleaving the drain exists for (a signal queued while the session is inside
   `HandleMessage`) is a scheduler outcome the harness cannot construct. The
   `ErrNoSuchTransaction` case the pre-existing test covers uses a hand-written
   id the server never minted; the two here were WATCHED live and then watched
   finish, one by termination and one by a client `COMMIT`.
2. **Who was refused, and at what number.** The cap test checks the failure CODE
   and that the message is non-empty. A code-only assertion is equally satisfied
   by refusing the wrong principal, or the right one at the wrong count. The
   quota arm RECOMPUTES the text from the principal and the limit it configured
   and requires an exact match, which is sound because `handleBegin` returns the
   quota error VERBATIM rather than through `Session.sanitiseErr`
   (`session.go:1604`) — unlike every neighbouring failure in that handler.
3. **The other three ways a slot comes back.** `abandoned_tx_test.go` covers a
   client `ROLLBACK`. The arm drives the idle reaper, `TerminateTransaction`, and
   a DE-AUTHORISED session's refused `COMMIT`, each ending in a `BEGIN` the cap
   must now allow. The third is the clause rmp #2482 carries over from the #2481
   security review, and it exists because no WAL or census oracle can see it: a
   refusal that left the transaction OPEN with its slot held and its registry
   entry live would write nothing either, so only the registry and the quota can
   distinguish "declined the message" from "reclaimed the transaction".

**One measurement worth recording.** `txRegistry.list` ranges a Go map — whose
iteration order is randomised — and insertion-sorts the result by `StartedAt`,
swapping only on a strict `Before`. Its cost therefore depends entirely on
whether the instants are DISTINCT, and the first version of the measurement in
the soak arm missed that: a fake-clock harness that never advances registers
every entry at one instant, the sort makes ZERO swaps, and the per-entry cost
came out FLAT with `n` — which looked like evidence the sort was cheap and was
evidence it had nothing to do. Measured with both arrangements, one
`Transactions()` call, no `-race`:

| open | same instant | distinct instants |
|---|---|---|
| 8 | 326 ns (40 ns/entry) | 314 ns (39 ns/entry) |
| 64 | 2.954 µs (46 ns/entry) | 11.839 µs (184 ns/entry) |
| 256 | 8.628 µs (33 ns/entry) | 143.153 µs (559 ns/entry) |
| 512 | 16.74 µs (32 ns/entry) | 599.715 µs (1.171 µs/entry) |

A production server's clock is real, so it is always in the second column: the
call is QUADRATIC in the number of open transactions, and 256 → 512 costs 4.19×
for twice the input. The comment above the sort justifies it by saying open
transactions are kept small, which was written when a writing transaction
serialised every other writer; rmp #2305/#2306 retired that, so the bound on
"small" is now `Options.MaxOpenTxPerPrincipal` (default 2048) times the
principals in play. Recorded, not fixed — it is outside this task.


### The graceful teardown: drain, Closer ordering, and what a RUN reply means (rmp #2483)

`Options.Closer` — the store-level teardown owner a Bolt server closes after its
connections drain — was passed by nothing in the module outside `bolt/server`'s own
tests. `SimServer.Close` only cancels the serve context, and the durable scenarios
close their store directly, so the ordering `store.DB` documents a Bolt server as
relying on was exercised end to end nowhere. Four deterministic arms and one
concurrent arm now drive it.

**The ordering is asserted on two observables, neither of them a timing guess.** A
`net.Conn` decorator counts accepted-and-not-yet-closed server connections; it is
one-sided by construction, because the connection handler's `conn.Close` runs
strictly before its `wg.Done`, so the count can lag but cannot claim a connection
finished before it did. And a rendezvous is CONSTRUCTED rather than waited for: a
commit is parked inside its WAL fsync with `SimDisk.ArmSyncGateAt`, and the closer
must have run zero times across a window in which the listener is already closed —
the listener flag being positive evidence that `Shutdown` has entered its drain
wait, rather than not yet started.

**Three measurements refute the obvious model of `Shutdown`.** Neither of its
failure branches closes the owned store: at the instant an expiring `Shutdown`
returned, the closer had run zero times (12/12 with a deadline, 12/12 with a
cancel), and the store is closed afterwards by `Serve`'s deferred exit path, once
the abandoned connections finish. It is never left unclosed in any reachable case
found. On a CLEAN drain, though, *who* closes is a genuine race: `Shutdown` cancels
the accept context before draining, so `Serve`'s exit path and `Shutdown`'s
drain-success branch wait on the same `WaitGroup` — measured **22 `Serve` / 3
`Shutdown`** over 25 successful drains. A `Shutdown` returning nil therefore does
not mean `Shutdown` closed the store, which is worth knowing for anyone reasoning
about teardown ordering from its return value.

**The third is a lesson about assertions, not about the server.** Which error a
DEADLINE-bounded `Shutdown` reports is also a race: it clamps its drain timeout to
`time.Until(deadline)` and then selects over both that clamped `time.After` and
`ctx.Done()`, which come due at nearly the same instant, and Go's select is uniform
when both are ready. The distribution is heavily skewed to the drain timeout —
measured 12 of 12 when the arm was written, and 8 of 8 in a later sitting — and the
arm PINNED that branch on the strength of it. Once the other branch surfaced under
`-race`, that pin and its siblings made **5 of 6 `-race` runs of the file red**,
each time on a different test. Both branches are now legal, the distribution is
reported rather than asserted, and the deadline arm is excluded from the
determinism clause because that field is not a function of its seed. An assertion
that holds twenty times and then fails is worse than one that never held, because
by the time it breaks it is trusted.

**One clause could not be written as the task posed it.** "No `wal.ErrWriterClosed`
reaches a client" is unfalsifiable as stated: it never can reach one, because
`Session.sanitiseErr` replaces the text of any error that is not client-fault and
`FailureCode` maps it to the catch-all — measured, a client whose store is closed
under it receives `Neo.DatabaseError.General.UnknownError` and a message naming
only a crypto-random session id. The oracle is therefore split in two: on the wire,
no statement on an undrained connection may receive a DatabaseError-class code; at
the store, `errors.Is(err, wal.ErrWriterClosed)` is checked on a commit attempted
after the teardown — which is simultaneously the proof that the WAL really closed
and the proof that the detector is not blind.

**A RUN SUCCESS is not the durability acknowledgement for an auto-commit write.**
This is the most transferable thing the task produced, and it arrived as two
harness defects rather than one engine defect. The concurrent arm reported an
`ACID_DURABILITY` violation in 4 of 25 runs; the lost row always had the same
signature — `RUN` answered SUCCESS, the terminal never arrived, the connection cut
— and the name was in neither the live engine nor the raw WAL bytes, with the WAL
image fully durable. Nothing had been made durable, so nothing acknowledged had
been lost. `handleRun` replies SUCCESS whenever the engine returns no error and
never consults `Result.Err()`; its metadata is `fields`, `qid` and `db` — statement
accepted, here are your columns. The BOOKMARK, which is what a driver uses to
establish that a write landed, rides on the terminal `PULL`/`DISCARD` SUCCESS and
on `COMMIT`, and is absent from `RUN`. When a graceful shutdown cancels an
in-flight statement, `commitUnderBarrier` early-returns on the materialise error,
appends no WAL frame, and the client that already holds its RUN SUCCESS is told
nothing further.

The same file had already made the same class of mistake once, in a way worth
recording beside it: it counted a `*proto.Ignored` — the reply a session in FAILED
gives every request-phase message until it is RESET — as an acknowledgement,
manufacturing the identical violation in 8 of 30 runs. Both are one rule: **an
acknowledgement is an explicit terminal SUCCESS and nothing else.** The RUN reply
and any IGNORED are kept as witnesses, because they are what separates "never
dispatched" from "dispatched, outcome unknown to the client", and the parked
in-flight commit is adjudicated on the invariant that does hold for it — the
statement the drain found executing must have RUN and must be DURABLE, read from a
reopen through real recovery rather than from any reply. That also closes the escape
hatch an absence-only oracle would leave: a drain that abandoned every in-flight
write would satisfy "nothing acknowledged was lost" by acknowledging nothing.

**A write cut short by a graceful shutdown can be reported as non-retryable.** The
session's own checks answer `Neo.TransientError.General.RequestInterrupted`, but a
cancellation surfacing from the engine is mapped to
`Neo.ClientError.Transaction.Terminated` — and this module already documents that
`neo4j-go-driver` v5.28.4's `reclassify()` demotes `Transaction.Terminated` out of
the retryable family. Both codes are pinned as named constants so a correction fails
the arm deliberately.

### Streaming semantics: PULL n paging, DISCARD, and the qid that routes nothing (rmp #2484)

Every result stream this package had ever opened was drained with a single
`PULL {n:-1, qid:-1}`. The consequence was not simply that paging was untested: no
`PULL` had ever carried a finite `n`, so `has_more` had been false on every reading
the harness ever took and never once observed true; `DISCARD` did not appear
anywhere in `internal/sim`; and no arm had ever addressed a stream by an explicit
qid. Three server paths — `handlePull`'s `n` limit and its look-ahead peek,
`handleDiscard`'s own `n` accounting, and the qid validation both share — were
reachable only from `bolt/server`'s unit tests.

**The task's premise was half wrong, and the refutation is worth more than the
scenario would have been.** It asked for "QID multiplexing" and "QID routing":
several open result streams on one session, addressed by qid. This server has
neither, and cannot, and each limb was verified in the code rather than inferred
from a passing test:

- `handlePull` refuses any `qid >= 0` outright, answering
  `Neo.ClientError.Request.Invalid` with `no such query: qid %d`
  (`bolt/server/session.go:1240-1243`). `handleDiscard` carries the identical guard
  (`:1421-1424`).
- RUN's SUCCESS always reports `"qid": int64(-1)` (`:1223`), so no positive qid is
  ever minted for a client to send back.
- A second RUN while a stream is open is refused by the state machine: `handleRun`
  requires READY or TX_READY (`:1075`), and a live stream leaves the session in
  STREAMING or TX_STREAMING (`bolt/server/state.go:181-228`, `:230-277`).

There is therefore exactly ONE open stream per session at any instant, and
"routing" is a property to REFUTE rather than to test. The scenario asserts the
refutation instead of arguing it: every RUN reply in the run is inspected and must
report `qid = -1` (26 readings at the catalogue seed, distinct set exactly `[-1]`),
and both refusals are pinned to their exact code AND exact message text.

What DOES exist — and is the honest reading of the objective — is that cursors
ACCUMULATE across SEQUENTIAL RUNs inside one explicit transaction. Each RUN appends
a cursor to `tx.results` (`bolt/server/tx.go:135` and `:140`), the slice is cleared
only by `Tx.closeCursors` on COMMIT or ROLLBACK, and
`Options.MaxInFlightPerConnection` is the bound (`session.go:518-526` counts it,
`:1086` refuses past it). Nothing in the harness had ever passed that option, so the
only cap the DST could have reached was the server's own default of 1024 cursors
deep inside one transaction — neither a short-layer budget nor a legible report.
`NewSimServerInFlight` now sets it, and REFUSES a non-positive value rather than
defaulting it, because passing zero through would silently hand a cap-driving
scenario a cap of 1024 and the refusal it then failed to observe would read as a
pass.

**The load-bearing oracle is an independent reference drain.** The same query is
drained twice, on two connections: once with a single `PULL -1`, which is the
reference record set, and once with a seed-drawn sequence of `PULL n` pages. The
concatenation must equal the reference ELEMENT BY ELEMENT and IN ORDER, compared
through the package's existing `compareWireRow`, which compares the decoded value
AND its concrete Go type because the dynamic type IS the wire encoding. That
distinction is load-bearing rather than decorative, and the falsifiability table
proves it: replacing `int64(41)` with `float64(41)` — three identical characters
under any `String()` rendering — fires the equivalence clause. The reference query
spans five PackStream encodings (Integer, String, Boolean, List, Float) and touches
no node, so every value it yields is a pure function of the query text with no
created-node internal key anywhere in the rows.

The partial-DISCARD arm sharpens equivalence into an exact statement about WHICH
rows were skipped. A seed-drawn prefix is paged, a seed-drawn window is DISCARDed,
the remainder is pulled, and prefix++remainder must equal the reference with exactly
that window cut out of it. A DISCARD that dropped one row too many, or one too few,
shifts the suffix and fails here; "the session still works afterwards" could see
neither. A controlled revert confirms it end to end: changing `handleDiscard`'s loop
bound from `discarded < n` to `discarded <= n` takes the scenario to exit 1 with
`prefix(21 rows)++suffix after DISCARD n=7 delivered 21 row(s), want 90`.

Measured at the catalogue seed: 12 pages from the plan
`[12 5 3 11 5 5 8 16 11 3 16 8]`, `has_more` 11 true / 1 false, the bookmark present
on exactly the terminal page and on no other, a window of 21 rows paged over 2 pages
with `DISCARD n=7` and a 69-row suffix, and the `qid = -1` control served all 97
rows.

**DISCARD abandons delivery, not the statement.** An autocommit write commits during
the DRAIN rather than at RUN (`session.go:1144-1148` explains why the statement's
deadline is held across the drain for exactly that reason), and `handleDiscard`
drains the cursor with the same `s.result.Next()` loop PULL uses (`:1453-1458`). So
the interesting question was never whether DISCARD is safe but whether it silently
drops the write along with the rows. Measured: it does not. The DISCARD delivers
ZERO records, its terminal SUCCESS still reports
`nodes-created=1 labels-added=1 properties-set=1 contains-updates=1` — the write
counters being the only route by which the effect can reach a client that took no
rows — and the node is present both in the live engine and in a graph reopened
through real WAL recovery after a crash.

**Two gates share one failure code, so the refusal is attributed by ORIGIN STATE.**
`handleRun`'s authentication gate (`:1072`) and its state gate (`:1075`) both return
`failTransition`'s `Neo.ClientError.Request.Invalid`, so a code match cannot say
which one refused — the discipline rmp #2481 established. `failTransition` reports
the ORIGIN state (`:1885`), which is the discriminator, and the needle is the whole
`in state X` phrase rather than the bare state name: **`TX_STREAMING` contains
`STREAMING` as a substring**, so a containment check on the name alone would let a
TX_STREAMING refusal satisfy the STREAMING clause, which is precisely the confusion
the attribution exists to prevent. A controlled revert moving `origin := s.state` to
after `s.enterFailed()` — a plausible refactoring mistake — takes the scenario to
exit 1 on `refusal-origin-state` and `refusal-message` alone.

**Both refusals POISON the session, and that was measured rather than assumed.** A
qid refusal routes through `failWith` → `enterFailed`, and the next request-phase
message on that connection draws `*proto.Ignored`, not a FAILURE and not a SUCCESS.
Only RESET restores it, after which a RUN+PULL is acknowledged again. Both facts are
asserted, which matters twice over: an IGNORED is a refusal, so a helper that
treated "not a FAILURE" as an acknowledgement would let every "the session is still
usable" clause in the file pass on a poisoned connection. A dedicated test drives a
real server into that state to prove the helper refuses it.

The in-flight arm runs two transactions against a WAL-backed store, each bracketed
by two readings of `wal.Writer.Stats`:

- **Under the cap.** Exactly `MaxInFlightPerConnection` RUN+PULL cycles accumulate
  and the transaction COMMITs. Measured frames +10, three nodes live and three
  recovered. This half is the non-vacuity witness in two senses: it proves the cap
  admits accumulation up to its bound, and it is the run in which the frame counter
  is observed MOVING, without which "the doomed transaction appended no frame" would
  be a statement about a dead instrument.
- **Over the cap.** The same cycles, then one more RUN, which must draw
  `Neo.ClientError.General.LimitExceeded` naming `cap=3, open=3`. The `open=` figure
  is parsed back out of the message and cross-checked against the harness's own
  cycle count, so two independent accountings of the same quantity must agree. The
  decisive RUN runs under an armed read deadline, so a stall becomes a harness error
  instead of a silent pass — the "backpressure or a typed error, never a block"
  mandate stated as an observation rather than as a hope. Measured frames +0 and
  bytes +0 across the whole arm, with the staged nodes absent both live and after
  recovery: the cap breach moves the session to FAILED, which rolls the transaction
  back.

**Frame counts are seed-pure; byte totals are not, and the rendering respects the
difference.** A created node's hidden internal key is minted by `cypher/exec` as
`"__cx_"+hex(n)` from a PROCESS-GLOBAL counter, so the same seed yields frames of
different widths depending on how many nodes every other test in the process created
first — the limitation already documented for `bolt-auth` and `schema-mutation`. The
evidence rendering therefore carries frame counts for both halves but a byte total
only where the expected value is ZERO, since zero is zero at any width, and every
map it walks is walked in sorted key order. Two runs of one seed render
byte-identically.

**The controls are real alternative configurations, not doctored values.** The
identical `PULL`/`DISCARD` message with `qid = -1` must be SERVED the whole record
set, which is what pins the refusals on the qid rather than on the message type, the
framing, or a typo in the harness. And the identical cap script with only
`MaxInFlightPerConnection` raised must stop being refused, which pins the refusal on
the cap rather than on explicit transactions, sequential RUNs, or the `CREATE`
statement — every one of which would refuse the raised-cap run too.

**The concurrent arm reuses the existing slow-consumer actor**, and one of its
oracles had to be demoted after reading the harness's own code. `halfPipe.write`
chunks every write to the space remaining (`internal/sim/simconn.go:96-124`), so the
queue can NEVER exceed `simConnBufferSize`: "the server did not buffer past the
bound" is an invariant of the pipe, not a property of the server, and a clause
asserting it cannot fail against a real server. It is kept as a labelled guard on
the harness itself, and the server-side HEAP bound — that a page is not materialised
into a second in-memory copy ahead of the wire — is left where it can actually be
measured, the live-heap gate in `bolt/server/streaming_backpressure_test.go`, rather
than restated where it cannot be. The reading also had to be CONSTRUCTED rather than
sampled: a single `ReadBuffered` call at the instant the consumer stalls was MEASURED
at 0 bytes on 2 of 3 seeds, and a bound asserted against 0 is a bound asserted
against nothing, so the arm polls until the queue is full and reports the peak
(65536 of 65536 on 9 of 9 runs under `-race`). What the arm then asserts is only what
every interleaving shares: a non-empty PROPER prefix reached the consumer, the writer
was provably blocked when the connection was torn down, the teardown leaked no
goroutine, and a FRESH connection's paged drain still matched plain range arithmetic
with `has_more` true on exactly its non-final pages.

**The coverage gate returns `Violation`s rather than the `[]string` rmp #2554
demoted the MERGE and FOREACH gates to**, and the distinction is deliberate. Those
gates reported a shortfall when a SEEDED WORKLOAD happened not to drive a branch,
which is an uninformative run and not a defect. Every precondition here is
CONSTRUCTED: each arm runs by rule on its own connection, and the draws are bounded
so that a discard window is always strictly interior (at most four prefix pages of at
most 16 rows leaves at least 33 of the 97 behind, against a window of at most 16) and
a paged drain always takes at least `ceil(97/16) = 7` pages. A shortfall therefore
means the harness itself stopped exercising the surface, which must fail loudly, as
it does for the constructed battery of rmp #2483. The soak sweep runs 400 seeds and
was clean on all 400.

### BEGIN extras: bookmarks, tx_timeout, metadata, mode, db and ROUTE (rmp #2485)

The harness had exactly one way to open an explicit transaction — `WireClient.Begin`,
which sends BEGIN with an EMPTY extras map. rmp #2482 added `WireClient.BeginMode`
for the single key `mode`, and the three scenarios that used it sent only the two
canonical spellings `"r"` and `"w"` (`internal/sim/bolt_tx_quota.go:696`,
`bolt_tx_registry.go:1058-1070`, `bolt_tx_terminate.go:486-496`). Everything else a
real driver puts in those extras was driven by nothing at all: no BEGIN or RUN
anywhere in `internal/sim` had carried a `bookmarks` list, a `tx_timeout`, a
`tx_metadata` map or a `db` name. rmp #2484 reads the `bookmark` key back OFF a
terminal SUCCESS and pins its presence to exactly the terminal page
(`internal/sim/bolt_stream_semantics.go:1150`), but nothing had ever SENT one, so
nothing in the module distinguished a server that honours the token from one that
ignores it. ROUTE had one call site in the package, rmp #2481's `route-after-logoff` arm
(`internal/sim/bolt_auth_surface.go:671`), which sends the ZERO message on a
DE-AUTHORISED session and requires it to be REFUSED — so `handleRoute` had never once
produced a routing table under simulation, and its payload was reached by nothing.
`WireClient.BeginExtras` (`internal/sim/wireclient.go:374`) is the new primitive, and
`Begin` and `BeginMode` now both route through it.

**This server does not honour an incoming bookmark, so the arm that looks like a
causality test proves nothing on its own.** `server.ExtractBookmarks` has exactly two
non-test call sites in the module — `bolt/server/session.go:1099` (RUN) and `:1529`
(BEGIN) — and neither does anything with the result but write it to a Debug log. The
RUN site says so in as many words: "Log any incoming bookmarks for observability;
single-host server ignores them for causal consistency but they should not be
silently dropped" (`:1097-1098`); the BEGIN site carries the shorter "Log incoming
bookmarks for observability" (`:1528`). The extractor validates nothing — it reads
the `bookmarks` key, keeps whichever elements assert to `string`, and returns nil
when none do (`bolt/server/bookmark.go:28-47`).

A reader on a second connection that presents the writer's bookmark and then sees the
write has therefore seen it for a reason that has nothing to do with the token it
sent. A single-host server has ONE store, and a committed write is already visible to
every later read of it: the property a bookmark exists to provide holds here
unconditionally. "The causal read observed the write" is a TRUE assertion that proves
nothing, and an arm that stopped there would be exactly the vacuous shape this sprint
has hit repeatedly.

The scenario makes the assertion honest by driving the SAME causal read five ways, on
five separate connections in a seed-drawn order, and requiring the five to be
indistinguishable:

- with the writer's REAL bookmark, which is what a driver actually depends on;
- with a FABRICATED far-future token, `FB:kffffffff`, whose counter is separately
  asserted to be strictly above every bookmark the server did issue, so "the server
  never minted this" is an observation rather than an assumption;
- with a token that is not of the shape this server mints at all
  (`not-a-gograph-bookmark`), which the extractor nevertheless keeps, and which is
  therefore logged like any other, because it validates nothing;
- with a `bookmarks` list whose single element is an `int64`, which the extractor
  filters out, so the server sees ZERO tokens rather than one it cannot parse;
- with no `bookmarks` key at all, the baseline.

All five must be ACCEPTED, must observe the identical count, and must reply inside a
real-time bound. That a token the server never issued is accepted is the evidence
that the token is IGNORED rather than honoured, and it is what makes the first arm's
meaning honest: a server that honoured bookmarks and was handed a far-future one
could only block until its own counter reached it — caught by `bookmark-does-not-wait`
— or refuse it — caught by `bookmark-accepted`. Either way the pin fires deliberately
and this section has to be rewritten, which is the point of the pin.

This is pinned INTENDED behaviour, not a defect, and it belongs here rather than in
the defect list below. What is new is not the behaviour but that anything in the
module now asserts it: before this task, nothing distinguished "ignored" from
"honoured", so a change in either direction would have gone unobserved.

**What a bookmark IS here, and where it arrives, was measured rather than described.**
`server.NextBookmark` (`bolt/server/bookmark.go:20-23`) returns `"FB:k"` followed by a
process-global atomic counter (`:13`) as eight zero-padded hexadecimal digits. It is
assigned in exactly ONE place — `s.bookmark = NextBookmark()` in `handleCommit`
(`bolt/server/session.go:1694`) — and delivered in three: the COMMIT SUCCESS, whose
metadata is that bookmark and nothing else (`:1696`); the terminal PULL SUCCESS, the
one with `has_more` false (`:1397`); and the terminal DISCARD SUCCESS (`:1500`). Since
rmp #2484 established that the terminal reply is also the durability acknowledgement,
the bookmark rides on the ack.

Two consequences follow from "assigned only in `handleCommit`", and both are pinned
because both are what a driver sees:

- On a session that has never committed an explicit transaction, an AUTOCOMMIT
  write's terminal PULL SUCCESS carries the EMPTY string in its `bookmark` field —
  measured, on a reply whose own `stats` map in that same SUCCESS reports
  `contains-updates`. The stats reading is what stops the empty bookmark being
  explained away as "the statement wrote nothing".
- On a session that HAS committed one, a later autocommit write's terminal PULL
  SUCCESS carries that EARLIER transaction's bookmark — measured EQUAL to it, not
  merely similar. A driver chaining causality off an autocommit `ResultSummary` is
  therefore chaining a strictly earlier transaction's token.

Neither is asserted anywhere in `bolt/server`: the only bookmark-key assertion in that
package is on a COMMIT SUCCESS and checks existence and non-emptiness alone
(`bolt/server/tx_test.go:82-88`).

**The bookmark VALUE is not reachable from the seed, and the rendering respects the
difference.** `bookmarkCounter` is process-global (`bolt/server/bookmark.go:13`),
exactly like the `"__cx_"+hex(n)` node key that bounds the authentication surface's
byte oracle, so the literal text of an issued bookmark depends on how many
transactions every other test in the process committed first. Every clause is
consequently written over a DERIVED relation — equality between two observed
bookmarks, a strict advance between two successive ones, an ordering against the
fabricated counter — and never over a literal value; and the evidence rendering emits
an issued bookmark purely positionally (`#0=<issued>`, `#1=<issued>`), so two runs of
one seed render byte-identically.

That rendering originally carried the ADVANCE between consecutive counters
(`#1=<issued,+1>`), on the reasoning that the advance, unlike the absolute value, is
seed-determined. It is not. The advance is one only while nothing else commits in
between, and `sim -swarm -workers N` runs N scenarios concurrently in ONE process
(`internal/sim/swarm.go:271-278`), so a concurrent COMMIT inflates it. MEASURED over
six fixed seeds, the advance read `+1` at `workers=1` and `+5`, `+6` or `+7` at
`workers=6` — six of six seeds rendering differently at the two worker counts — and
with the advance dropped, sixteen of sixteen seeds render identically at `workers=16`.
`TestBoltBeginExtras_Deterministic` could not have caught it, because it compares two
SERIAL runs. The property was never in the rendering's gift anyway:
`bookmark-strictly-advances` adjudicates the relation `n > prev` between two OBSERVED
counters, which survives any interleaving.

**`tx_timeout` is attributed by its CONTROL, not by its subject.** Four arms run in a
FIXED order, each against its own server on its own fake clock
(`internal/sim/bolt_begin_extras.go:1265-1306`); every advance is virtual, and no arm
depends on wall time for its outcome, only for its bound. The order is fixed rather
than drawn because an arm and its control are comparable only as the same script.

- `client-tx-timeout`, the subject, asks for a 100 ms `tx_timeout` with the idle bound
  and the server's default total bound both lifted to 10 virtual minutes, and a single
  advance of exactly that bound must reap it. Lifting both is load-bearing: the serve
  loop reaps at the EARLIER of the two bounds (`effectiveTxDeadline`,
  `bolt/server/serve.go:1155-1167`, established by rmp #2482), so an arm that left the
  idle bound at its default would be timing the wrong reaper. The non-vacuity gate
  re-derives that separation from the arm's own recorded bounds rather than trusting
  the plan.
- `no-tx-timeout-control` is THE attribution. It is the byte-identical script with the
  `tx_timeout` key removed, given the identical advance, and it must both survive and
  COMMIT. Without it, "advance and the transaction died" is satisfiable by any timer
  at all; with it, the single difference between the two arms is the extra.
- `idle-bound-control` is a CONSTRUCTED collision: the same reap reached through the
  IDLE bound instead. It differs from the subject in TWO fields, not one — the idle
  bound is the small one AND `tx_timeout` is not sent — and the second is forced by the
  first, since leaving the client's bound in place would arm a total-lifetime deadline
  at the same instant and the arm could no longer attribute its reap to the idle
  reaper.
  The checker requires its code AND its message to be byte-identical to the subject's,
  which is how the shared-failure finding is ASSERTED rather than restated. Reading
  `bolt/server` widens rmp #2560 from two paths to three — the idle reaper, the
  total-lifetime reaper and `Server.TerminateTransaction` all funnel through
  `Session.reapTimedOutTx` (`bolt/server/session.go:1831`), which arms a single
  `pendingTermErr` carrying one code and one message (`:1839-1842`). A client cannot
  tell the three apart.
- `overflow-tx-timeout` is the hostile arm, and its mechanism is worth stating exactly
  because the obvious reading of it is wrong. It sends `tx_timeout = 1<<62`
  milliseconds, and that value never reaches a multiplication: `clientMillisToDuration`
  (`bolt/server/session.go:460-465`) returns `(0, false)` for
  `ms <= 0 || ms > maxClientTimeoutMillis`, and `maxClientTimeoutMillis` is
  `math.MaxInt64 / int64(time.Millisecond)` = 9,223,372,036,854 ms, about 2,562,047
  hours (`:452`), which 1<<62 = 4,611,686,018,427,387,904 exceeds by a factor of
  500,000. `handleBegin` treats `(0, false)` as "unset" and leaves the server default
  in force (`:1543-1555`), so an out-of-range client bound is SILENTLY IGNORED. The
  guard is what makes it silent rather than catastrophic: had the multiplication
  happened, 2^62 x 10^6 is 2^68 x 5^6, a multiple of 2^64, so the int64 product would
  be exactly ZERO — a non-positive duration that would leave `txDeadline` unset and
  DISABLE the reaper altogether. The arm asserts the outcome in BOTH directions with
  two advances of half the 100 ms server default each: the first must not reap, and
  the second must. An arm that only advanced past the default could not tell "the
  default is in force" from "a shorter bound is".

**The abort is typed, delivered ONCE, and then the session ignores.** Every reaped arm
is adjudicated by one shared checker, so the three cannot drift apart, and the checker
is skipped entirely for an arm that was not reaped — an arm with nothing to report
must not be able to satisfy a clause about a report. It pins the exact code
`Neo.ClientError.Transaction.TransactionTimedOut` and the exact message "the
transaction has been terminated because it exceeded its timeout; the writer lock was
released" against the named constants of rmp #2482
(`internal/sim/bolt_tx_registry.go:517-518`); it requires the SECOND request-phase
message after the abort to draw `*proto.Ignored`, because `pendingTermErr` is
delivered on the first such message and cleared there
(`bolt/server/session.go:594-597`), after which the switch falls through to
`&proto.Ignored{}` (`:599`); it brackets, in REAL time, the interval from the reaping
advance to the abort reaching the client, so a stall reads as a failure rather than as
a pass; and it requires the injected clock to have registered at least one timer,
because a reap is attributable to the reaper only if the reaper was armed.

**The `mode` coercion FAILS OPEN, and that is measured on two independent
observables.** `handleBegin` selects read-only for the exact string `"r"` and for
nothing else (`bolt/server/session.go:1560-1566`): a non-string value, a misspelling,
the uppercase `"R"`, an absent key — every one of them silently yields a WRITE
transaction, and the client is told nothing. Five arms drive `"r"`, `"w"`, `"R"`,
`"bogus"` and no key at all, in a seed-drawn order, and each is adjudicated on two
observables: the server's own `server.TransactionInfo.Mode`, read off
`Server.Transactions()` while the transaction is open, and whether a `CREATE` inside
that transaction is accepted. One observable alone could not tell a mis-recorded mode
from a mis-enforced one. The read-only arm's refusal is pinned to the exact code
`Neo.ClientError.Request.Invalid`, while its message is required only to CONTAIN
"read-only transaction": that text comes from `cypher`, not from `bolt/server`, so
pinning it verbatim would couple the scenario to a message the engine owns. `"R"` is
in the roster because it is the plausible misspelling, and the clause that matters is
that its write is ACCEPTED. No earlier scenario had ever attempted a write inside a
Bolt read-only transaction, so the refusal itself is new coverage too.

**`db` is echoed unvalidated, agrees across both replies, and is never empty.**
`selectDatabaseFrom` (`bolt/server/session.go:322-324`) records the extra verbatim,
and `databaseName` (`:309-317`) reports it, falling back to the server's own name and
then to `DefaultDatabaseName`, which is `"neo4j"` (`bolt/server/serve.go:195`). A name
this server does not serve is therefore ECHOED rather than refused with
`Neo.ClientError.Database.DatabaseNotFound`, which `Options.DatabaseName`'s own godoc
states deliberately (`bolt/server/serve.go:308-322`): GoGraph serves one graph per
server, so the name is a label and not a selector. Four arms drive `"neo4j"`, a foreign
`"not-this-server"`, `"system"` — a name that in Neo4j is a real and distinct database
and here is echoed like any other label — and no key at all. Each pins the echo; pins
that the RUN SUCCESS and the terminal PULL SUCCESS report the SAME name, because a
driver reads it off whichever one it consumes; pins that the reported name is never
empty, which is the rmp #2172 guard (the official driver returns a nil `DatabaseInfo`
for an absent or empty `db`, so the idiomatic `summary.Database().Name()` panics inside
the driver); and pins that the COMMIT SUCCESS carries the bookmark and NOTHING else, so
a widening of that reply is noticed rather than absorbed. The name is sent on BEGIN and
not on the following RUN, which is what a real driver does inside an explicit
transaction and what `handleRun`'s `if !s.txActive` guard
(`bolt/server/session.go:1134-1142`) makes safe: were that guard absent, the RUN's
empty extras would CLEAR the selection BEGIN recorded, and this arm is what would see
it.

**`tx_metadata` is accepted and echoed nowhere, which is asserted instead of a round
trip that does not happen.** The key is read in no file under `bolt/`: a sweep of every
`.go` file in the module finds it only in this task's own files, the catalogue entry
and `WireClient.BeginExtras`'s godoc, so unlike `bookmarks` it is not even logged.
`docs/bolt.md:225-226` already claimed "accepted in `BEGIN`/`RUN` extras and silently
ignored; the server stores and echoes no transaction metadata", and nothing drove it.
The arm sends two keys on BEGIN and then requires the BEGIN SUCCESS, the terminal PULL
SUCCESS and the COMMIT SUCCESS each to carry none of them.

**The ROUTE payload is compared against an INDEPENDENT reference, not against a
constant restated.** `handleRoute` (`bolt/server/session.go:1728-1753`) reads nothing
whatever from the message — not its `Routing` map, not its `Bookmarks`, not its `DB`.
Past the authentication gate (`:1745-1747`) and the state gate (`:1748-1750`) it
answers `RoutingTable(s.localAddr)` (`:1751`), a table whose TTL is a hardcoded 300
seconds, whose own `db` is the EMPTY string, and whose three roles WRITE, READ and
ROUTE all point at the one address (`bolt/server/route.go:11-33`). Two ROUTE messages
are sent on one connection, one carrying a routing context, a bookmark and a database
name, and one the zero message. Their rendered tables must be IDENTICAL, which is the
assertion that all three fields are dropped; and the populated request's table `db`
must be empty, which pins that a ROUTE naming a database is answered by a table
labelled with nothing. ROUTE's bookmarks are dropped without even the Debug line RUN
and BEGIN give theirs. rmp #2481 covered ROUTE's authentication gate and deliberately
left the payload here.

The address clause is where the independence matters. The table is built from
`s.localAddr`, which the accept loop copied off the listener —
`localAddr = s.ln.Addr().String()` at `bolt/server/serve.go:1000-1005`, handed to
`newSession` at `:1006` — and the checker compares every advertised address against
`SimServer.ListenerAddr()` (`internal/sim/simserver.go:474`), which reads
`s.ln.Addr().String()` on the harness side. The two reach the same source of truth by
different routes, so "the table names THIS server" is a comparison of two independently
obtained values. A checker that compared the reply against `server.RoutingTable`'s own
output would be comparing that function with itself. The non-vacuity gate additionally
fails the run when the listener reports an empty address, because every advertised
address would then be compared against `""`.

**The non-vacuity family is a separate function because it answers a different
question.** `checkBoltBeginExtras` asks whether the server misbehaved;
`checkBoltBeginExtrasNonVacuity` asks whether the run was in a position to notice.
Between them they carry 36 named contract clauses and 22 `nv-` ones. Both feed the SAME
report, so a coverage shortfall FAILS the scenario exactly as a contract violation does
— the `Violation`-returning discipline rmp #2484 adopted, rather than the advisory
`[]string` rmp #2554 demoted the MERGE and FOREACH gates to — but every `nv-` message
names what the run failed to construct instead of accusing the server. Every
precondition here is CONSTRUCTED rather than left to a seeded workload, so a shortfall
means the harness itself stopped exercising the surface. What the gate refuses to let
pass: a causal read of ZERO nodes, which could not distinguish "the reader saw the
write" from "the write never happened" (the writer's node count is drawn from [2, 6],
so a positive count is guaranteed by construction); fewer than one real and two
fabricated tokens, without which "the token changed nothing" compares nothing; fewer
than two arms that completed a read, which would compare a value with itself; fewer
than two issued bookmarks, because one event is not a sequence a strict-advance clause
can falsify; an EMPTY reference bookmark, which would collapse the stale-autocommit
equality into "both are empty"; a timeout family with no reaped arm or no surviving
one; any timeout arm whose injected clock registered no timer, which is what separates
"the reaper declined" from "there was no reaper"; a `client-tx-timeout` whose server
bounds are not strictly beyond its advance; a mode family missing either the read-only
or the write side, or in which no write was ever accepted; a database family with no
foreign name or no absent-key arm, without which an echo and a fallback are the same
observation; an empty listener address, an unnamed database in the ROUTE request, an
undecoded routing table, or an empty table rendering; and a `tx_metadata` arm that sent
no keys, which would make "no reply echoed one" vacuously true.

Measured at the catalogue seed 612741132: 3 nodes under `:BeginCausal` committed across
2 transactions; all five causal arms accepted and each observing 3 of 3, with
`ExtractBookmarks` keeping one token on three of them and zero on the other two; the
autocommit bookmark EMPTY on a fresh session and equal to the prior COMMIT's on a
session that had committed one, on a reply reporting `contains-updates`; the four
timeout arms reaped after advance ordinals 0, never, 0 and 1 respectively, each with
exactly one timer armed on its injected clock, and all three reaped arms answering the
byte-identical code and message with IGNORED on the message after it; the registry
reporting mode `"w"` for `"w"`, `"R"`, `"bogus"` and the absent key with the write
ACCEPTED in every one, and `"r"` for `"r"` with the write refused
`Neo.ClientError.Request.Invalid` / "cypher: write or DDL statement not allowed in a
read-only transaction"; `db` reported as `neo4j` for the arm that sent no key and as
`not-this-server`, `neo4j` and `system` verbatim for the three that named one, with
the COMMIT SUCCESS carrying exactly `[bookmark]` in all four; the
routing table advertising `[WRITE READ ROUTE]` over three entries at the listener's own
address with `ttl=300` and `db=""`, the populated and zero ROUTE answered identically;
and the `tx_metadata` BEGIN SUCCESS carrying no metadata keys at all, its terminal PULL
carrying `[bookmark db has_more]` and its COMMIT `[bookmark]`, none of them a key the
client sent. A serial sweep of seeds 1 to 100 was clean on all 100.

### The protocol version matrix: 4.4, 5.0 and 5.x side by side (rmp #2486)

**Every DST connection before this task negotiated 5.6.** `WireClient.Handshake` offers
5.6 with a minor range down to 5.0 in slot 0 and 4.4 in slot 1, and the server picks the
highest version it supports inside any offered range, so 5.6 is what every arm of every
scenario got. rmp #2481 added `HandshakeOffering` to reach a specific version and used it
only to reach 5.0, only to check that a credential-bearing HELLO is accepted there. Bolt
4.4 had never been negotiated by anything in `internal/sim`, and no two versions had ever
been compared against each other.

**Two whole axes of the server were undriven, and they are different axes.** The entity
and temporal encodings branch on the MAJOR version: a Node is three fields at Bolt 4 and
four at Bolt 5 (`bolt/server/entity_struct.go:96-98`), a Relationship five and eight
(`:112-118`), an UnboundRelationship three and four (`:130-132`), and a Path inherits all
of them by recursion (`:144-152`); a zoned DateTime switches both its struct tag and the
MEANING of its seconds field — `0x49`/`0x69` carrying a true UTC epoch second at major
≥ 5, `0x46`/`0x66` carrying the wall clock expressed as if UTC at 4.4
(`dateTimeToPackstream`, `bolt/server/session.go:2222-2243`). Authentication branches on
the MINOR version at a different place: `authDeferredToLogon` compares against
`proto.Version{5, 1}` (`bolt/server/state.go:294-305`), so ≥ 5.1 sends a credential-less
HELLO and authenticates on a separate LOGON while ≤ 5.0 carries the credentials on HELLO
itself. **The task text calls the second axis "4.4 (no LOGON)"; reading the code says
more than that** — 5.0 is on the same side of the auth split as 4.4 and the other side of
the encoding split, which is exactly why it is called out separately as never entered,
and it is what makes the matrix a CROSSED design rather than a list. 4.4 against 5.0
moves the encoding axis with auth held fixed; 5.0 against 5.1 moves the auth axis with
the encoding held fixed, so either difference is attributable to ONE axis, which a
4.4-versus-5.6 comparison alone could never be.
`TestBoltVersionMatrix_TableIsCrossed` pins that the 5.0 row exists, because dropping it
would silently collapse the design while every clause still passed.

**The load-bearing shape is that semantics are INVARIANT while encodings DIFFER, and both
halves are asserted**, because either alone is satisfiable by a broken server. A run that
only required the decoded values to agree across versions would pass against a server
that ignored the negotiated version entirely and emitted Bolt 5 structures to a 4.4
client — the values would agree perfectly and a real 4.4 driver would fail to hydrate
them, since its hydrator asserts the field count. So
`encoding-differs-across-majors` is written deliberately as the guard on the other
clauses: the SAME query's record captured at 4.4 and at 5.6 must not produce the same
struct census or the same byte length. Measured at the catalogue seed, `[N/3 R/5 P/3 N/3
N/3 r/3]` against `[N/4 R/8 P/3 N/4 N/4 r/4]`, and 144 bytes against 168 — the census is
stable run to run, the two lengths are not (see the instrument trap below), but they
always differ, because the Bolt 5 layout adds seven decimal element_id strings to this
record and cannot encode to the same size. **A controlled
revert confirmed it end to end**: making `boltVersionExpectedWidths` version-blind turned
the live 4.4 arm red on `encoding-struct-layout`, and declaring 5.0 deferred-auth turned
the live 5.0 arm red on five auth clauses at once.

**The oracle is an independent PackStream reader, not the codec it adjudicates.**
`decodeBoltWire` is a minimal reader written in `internal/sim/bolt_version_matrix.go`
from the marker table — derived first from real hex captures of this server and then
confirmed against the published constants at `bolt/packstream/encoder.go:24-59` — and it
is what produces each record's struct census from the raw chunked bytes.
`TestDecodeBoltWire_ReadsHandBuiltBytes` pins it against hand-written byte strings whose
content is known independently of any encoder, and `TestDecodeBoltWire_RejectsMalformed`
pins its refusals, because a reader that tolerated a truncation could report a short
census as a correct one. `encoding-walker-agrees-with-codec` then runs the module's own
decoder over the identical bytes and requires the two censuses to match, so a bug in the
independent reader surfaces as a disagreement rather than as a confident wrong verdict.
The value-level oracles are computed by the harness: the entity ids and property maps are
what the scenario itself created, each element_id must be `strconv.FormatInt` of the id
the same structure reports, and every temporal field is computed with Go's `time` package
from the literal the query carries.

**The no-LOGON contract is measured against a CREDENTIALED server**, never
`NoAuthHandler`: against a handler that admits everyone, "the credentials were accepted"
is true at every version and proves nothing. With `BasicAuthHandler` the same bytes
produce opposite outcomes on the two sides of 5.1, which is what makes the contract
falsifiable. A WRONG password on HELLO draws `Neo.ClientError.Security.Unauthorized` and
the connection is torn down at 4.4 and 5.0, and draws SUCCESS with the connection intact
at 5.1 and 5.6. A credential-less HELLO is refused at 4.4 and 5.0 and succeeds at 5.1 and
5.6. A RUN sent straight after a successful HELLO is SERVED at 4.4 and 5.0 and refused at
5.1 and 5.6 by the state gate, which names the state it refused from. And a RESET on that
pre-LOGON session returns it to NEGOTIATION rather than to READY — the deliberate
pre-authentication RESET gate of task #1345 (`bolt/server/state.go:124-133` and
`Session.handleReset`'s `!s.authenticated` branch, `bolt/server/session.go:1038-1041`) —
so the following RUN is refused naming NEGOTIATION, while the same RESET on an inline-auth
session leaves it usable. Every refusal clause pins the whole `in state X` phrase rather
than the bare state name, following rmp #2484.

**Negotiation is adjudicated by a literal expectation table over raw 20-byte preambles**,
written directly on `SimConn` rather than through `WireClient`, because
`HandshakeOfferingSlots` collapses a rejection into an error and telling a rejection apart
from a transport failure by matching that error's text would be a fragile oracle. Fifteen
cases: the four exact versions; four range offers, including one whose top is ABOVE
everything the server supports and which still resolves, to 5.6; the legacy version
offered FIRST and losing anyway, which shows the choice is driven by the server's
preference and not by slot order; a supported version with an unsupported decoy alongside
it; and four ways to have nothing in common, all refused. Because every expectation
follows from `proto.SupportedVersions`, `negotiate-supported-list` is a TRIPWIRE that
compares that list against a literal copy, so adding 5.7 upstream is a loud failure at the
one clause whose job is to notice rather than silent staleness across the other fourteen.

**The offer SPELLING is seed-chosen and its invariance is the claim.** Each arm negotiates
its target twice — once canonically (exact version, slot 0, no range) and once with a
seed-drawn slot, minor range and optional unsupported decoy — and the seeded spelling is
the one the working connection uses, so every observation the arm makes was produced over
it. This needed one new primitive, `WireClient.HandshakeOfferingSlots`, because nothing in
the harness could send a range offer other than the one `Handshake` hard-codes or place an
offer in a chosen slot. `Handshake` and `HandshakeOffering` were both refactored onto it,
and `TestWireClientHandshake_PreambleBytesAreUnchanged` pins the exact 20 bytes each still
writes, read off a bare `SimListener` with no server behind it — comparing negotiated
versions could not have caught the change, because a range offer and an exact offer of the
same top version negotiate the same result.

**One trap was found in this scenario's own instrument rather than in the server.** An
early draft rendered node ids and the record's byte length, on the stated belief that they
were assigned in fixture-creation order and therefore a function of the seed. The
determinism test refuted it: two runs of the same seed produced node ids 38/215 and then
227/48, and records of 138 and then 140 bytes, because the id derives from a node key
minted from a process-global counter and the byte length follows it through the decimal
element_id strings. Both are now rendered POSITIONALLY — `n0`, `n1`, `e0` in
first-encounter order, with each element_id shown as the token of the id whose decimal it
is — which keeps every structural fact (which entity appears where, which element_id
belongs to which id) while dropping the process-dependent value. The CHECKERS still read
the raw values, and are entitled to: every clause over them is a derived relation, never a
literal. This is the same class of defect as the rmp #2485 report field that rendered `+1`
at one worker and `+5` at six.

The remaining families are the version-invariant half. The parameter round trip drives
seven kinds (null, boolean, integer, float, string, a mixed list, a map) and compares the
decoded value AND its concrete Go type across versions, following rmp #2484 — the dynamic
type IS the wire encoding, so an Integer re-encoded as the identically-rendered Float must
fail. The zone-less temporals (`date` `0x44`, `localtime` `0x74`, `time` `0x54`,
`localdatetime` `0x64`, `duration` `0x45`) are required to be BYTE-identical at every
version, which is the control proving the version knob is narrow rather than global. Both
zoned datetimes are asked at a NON-ZERO offset (`+02:00`, and Europe/Athens on 2 January,
also `+02:00`) because at a zone offset of zero the legacy and UTC conventions encode the
identical seconds field and the clause degenerates to a tag-only check — Europe/Lisbon in
January is exactly that trap and was the first zone tried;
`TestBoltVersionMatrix_TemporalReferenceOffsetIsNonZero` pins it. A bad-actor battery
(garbage opcode, COMMIT with no transaction, PULL with no RUN) must draw the identical
typed refusal at every version, pinned to the literal code and message so that two
versions agreeing on a wrong answer does not pass. And each arm commits a marker node in an
explicit transaction: the census must advance by exactly one per arm, and every marker must
be present both live and in a graph reopened through real WAL recovery after a crash, so
the protocol version a write arrived over provably does not reach the durable state.

The non-vacuity gate is a separate function answering a different question — was the run in
a position to notice — and its shortfalls fail the scenario just as a contract violation
does. It censuses which versions were actually negotiated (the trap being an arm that
silently landed on 5.6 while believing it negotiated 4.4), requires the crossed design to
have been constructed, requires the same entity tag to have been SEEN at two different
arities and both zoned-datetime conventions to have been seen, requires the negotiation
table to have produced at least one refusal and one range resolution, requires the seeded
spelling to have differed from the canonical one for at least one arm, and reports a
missing zone database as a shortfall rather than letting the named-zone clause pass
unexercised. 38 named contract clauses and 10 `nv-` ones; 53 falsifiability subtests each
perturb one field and assert the clause that must catch it.

### Aggregate inbound-decode backpressure, and two nesting caps that are not one (rmp #2487)

The harness had exactly one arm anywhere near inbound-memory abuse: the `BoltAbuser`'s
oversized frame, which drives the PER-MESSAGE framing cap on ONE connection. Three bounds
that matter more were driven by nothing. The **engine-wide inbound-decode pool**
(`packstream.InboundBudget`) is created ONCE PER SERVER — `bolt/server/serve.go:654`,
`NewInboundBudget(resolveMaxInboundDecodeBytes(opts.MaxInboundDecodeBytes))` — and that
single pointer is what makes it the cross-connection vector, because the per-message cap
times the connection limit is unbounded and pre-authentication-reachable, which is the
CWE-770 the pool exists to close. The **wire nesting cap** (`packstream maxValueDepth =
128`, `value.go:21`) is a hard security boundary rather than a convenience: without it a
crafted message can request millions of stack frames and kill the process, and it is
reachable during the FIRST HELLO decode. And a third that this task found by measurement
rather than by reading: the engine's **own parameter nesting cap** (`cypher
maxParamBindDepth = 32`, declared at `cypher/api.go:4303` and enforced at `:4256`), a
second, lower, independent cap on the same axis.

**The load-bearing oracle is a closed-form model of the pool, not the server's word.** The
harness re-derives what a RUN's decode holds from the shared pool out of packstream's
published per-slot costs:

```
held(query, key, n) = 32 + 3*48    the RUN struct: container + 3 fields
                    + 512 + 112    the one-entry parameters map
                    + 32           the parameter list's container
                    + 512          the empty extra map
                    + len(query)   String payloads are charged 1:1 (decoder.go:712)
                    + len(key)     and so is a map key
                    + 48*n         one 48-byte slot per list element
                    = 1344 + len(query) + len(key) + 48*n
```

The two string terms are not obvious and were found by measurement: `ReadString` charges
its raw payload against the SAME shared pool that `chargeDecoded` draws on, so a longer
query text moves the admission boundary. The model was calibrated against the real decoder
by binary search on the smallest budget that admits a payload — `48n + 1353` for every `n`
from 0 to 174,734 with an 8-byte query and a one-byte key, exactly what the closed form
says — and it then named the last admitted element count EXACTLY at nine ceilings from
2 MiB to 32 MiB (the soak sweep). The scenario does not trust that: it SCANS a window
around the prediction and requires the measured boundary to be one element wide, monotone,
and equal to the model's. That makes `pool-boundary-matches-model` a tripwire on five
packstream constants; if one changes, this scenario names the divergence instead of
drifting.

**A one-element-wide boundary is the strongest available refutation of "the per-message cap
did it."** At the 4 MiB ceiling, `n=87353` (modelled hold 4,194,304 B, slack +0 B) is
SERVED and `n=87354` (slack -48 B) is REFUSED: the two differ by 48 charged bytes out of
~4 MiB, both are ~200x under the 16 MiB framing cap and 32x under the 128 MiB per-message
decoded-collection cap. The **control** closes it: a second server differing in the
ceiling (64 MiB) and in NOTHING else serves the identical 165,796-element bytes the
pressured one refused. The two write arms carry the same query shape and differ only in
their parameter's element count, so the census is attributable: the accepted write's node
is present live AND after real WAL recovery, the refused write's node is present nowhere,
and the same bytes wrote a node into the control server's own engine.

**Three abuse vectors, three DIFFERENT answers — measured, not inferred.** A server that
collapsed any two would be indistinguishable, from a client's side, from one with no
aggregate pool and no depth cap at all, and every other clause here could still pass
against it:

| vector | code | session afterwards |
|---|---|---|
| aggregate pool breach | `Neo.TransientError.General.OutOfMemoryError` | READY (usable with NO RESET) |
| wire nesting cap (>= 128) | `Neo.ClientError.Request.Invalid` / `malformed Bolt message` | READY (usable with NO RESET) |
| engine parameter cap (> 32) | `Neo.DatabaseError.General.UnknownError` | FAILED (next message IGNORED) |

The first two are answered ABOVE the session state machine — the serve loop rejects them
between the read and `sess.HandleMessage` (`serve.go:1258` and `:1289`) — which is why the
session survives them intact; the third travels through it into `cypher.BindParams`, so it
fails the session. That state-after difference is a **third discriminator, independent of
the codes**, and it is asserted separately. The classification segment is asserted on its
own too, read out of the OBSERVED code rather than compared to the literal this file
declares: neo4j-go-driver's `IsRetriableTransient` tests `classification ==
"TransientError"` (`bolt/server/errors.go:129-131`), so "typed RETRYABLE backpressure" is a
checked property of the code the server actually sent. Testing the classification only
after the whole code matched the literal would have made that guard unreachable, and an
earlier revision did exactly that.

**The harness reads that segment at the driver's arity, not a laxer one (rmp #2575).**
`boltDecodeClassification` accepts a code of EXACTLY four dot-separated segments and
returns `""` for anything else, mirroring
`github.com/neo4j/neo4j-go-driver/v5` v5.28.4, whose `(*Neo4jError).parse`
(`neo4j/db/errors.go:114-127`) abandons a code on `len(parts) != 4` and so leaves the
classification empty, making `IsRetriableTransient` (`neo4j/db/errors.go:156-159`) report
false. An earlier revision split on dots and took `parts[1]` from any code with two or more
segments, so a regression emitting the three-segment `Neo.TransientError.OutOfMemoryError`
would have SATISFIED `pool-refusal-retryable` while no real driver would retry it. The gap
was latent — every `Neo.*` code the server can emit is four-part, verified by sweeping the
tree's Go AST rather than by assuming it, and every dynamic `Code:` assignment on the Bolt
path resolves through `FailureCode`/`authErrorCode`/`evalErrorCode`, which return only
four-part literals — so closing it changed no verdict. The mirroring is deliberate and is a
third-party contract that can drift, so the arity must be re-derived from the pinned
dependency rather than remembered.

**The nesting family is bracketed at every boundary and is deliberately tiny on the wire.**
32 accepted / 33 refused, and 127 refused-by-the-engine / 128 refused-by-the-decoder,
identically for LIST chains and MAP chains — the bound is on composite depth, not on lists.
Every payload is far under the 64 KiB anti-confound ceiling that is asserted — 55 to
4046 wire bytes at the catalogue seed, and at most 6166 on any seed, since the
deliberately excessive arm's chain length is drawn from `[2048, 6144)` — because a
message refused for its SIZE proves nothing about a DEPTH cap. The chains are
hand-built from the marker table rather than through `packstream.Encoder`, and
not for authenticity: the encoder CANNOT express them, because `writeValue` carries the
same `maxValueDepth` bound as `readValue` (`value.go:68-69`). An abuse the module's own
encoder refuses to encode is exactly the abuse a hostile peer hand-rolls, and building it
here keeps the harness from validating the decoder with the encoder that shares its bound.
The **pre-authentication** arm is the one that isolates the wire cap cleanly, since no
parameter is bound and the engine's cap is not in the way: a 127-deep HELLO succeeds, a
128-deep one is refused, and the connection survives with the session still
UNauthenticated — a following plain HELLO succeeds, which it could only do from
NEGOTIATION.

**No-leak is proved through the wire, not by reading the pool.** `InboundBudget` exposes
`Enabled`, `TryReserve` and `Release` but no `Remaining`, and the Server's pool is
unexported. Rather than reach for an accessor, the run repeats the calibrated
boundary-sized message after every abuse arm: a message whose modelled hold is within one
element's charge of the whole ceiling can only be admitted by a pool restored to within
that many bytes of full. Measured slack at the 4 MiB ceiling is **+0 B**, so a leak of a
single byte is detectable, and the gate asserts that slack stays tight — a probe that had
gone slack would pass whether or not the pool came back short. The soak layer adds the
statement the short layer structurally cannot make: 4000 alternating served and refused
decodes against ONE long-lived server (2000 each, so both release paths run equally), after
which the boundary probe is still admitted.

**The concurrent sibling exists because the aggregate vector is unreachable without it.**
Every charge is released before its reply is written — the reassembly reader releases on
every return path from `ReadMessage` (`bolt/proto/chunking.go:160-165`), the decoder's hold
by the deferred `ReleaseInboundBudget` (`serve.go:1419-1423`) — so a single-threaded
lock-step script can never observe two charges outstanding at once, whatever it sends. The
`bolt-decode-swarm` scenario runs four abusers at 55% of an 8 MiB pool (one fits, two
cannot) against an honest client on its own connection.

**Two of its oracles had to be CONSTRUCTED rather than raced for, and the cost of not doing
so was measured at each step.** Started together with fixed counts, the abusers finished in
~38 ms while the honest client was still pausing between exchanges: exactly ONE of 24
honest exchanges straddled a refusal, a coverage clause one scheduling decision from
failing for no reason and one from PASSING while the run showed honest traffic working
before and after the pressure rather than during it. Pinning the window's ENDS — the honest
client waits for the first refusal, the abusers push until it finishes — took it to 9 of
24. That is still a race, and the measurement says why: under `-race` a narrow honest
exchange takes 633 us at the median (357 us to 902 us over 20 samples) while refusals
arrive about once per 3.8 ms of honest in-flight time, so most exchanges land in a gap. So
the WIDTH is controlled too: every sixth honest exchange holds its open stream for 50 ms
between RUN and PULL, genuinely in flight with the server holding a cursor for it, and the
overlap clause gates on those alone. That hold is ~13x the inter-refusal interval and ~79x
the median narrow exchange. Measured 4 of 4 across 25 seeds under `-race`, with the
narrowest wide window holding 9 to 11 refusals against the 2 to 3 a 20 ms hold gave; the
100-seed soak sweep, which runs under heavier concurrent load, saw a worst case of 6, and
it asserts that worst case rather than only the pass. The hold is deliberately NOT
a wait for a refusal to be counted: that would make the clause true by construction of the
HARNESS instead of by behaviour of the SERVER. An independent density clause requires the
whole honest run to contain at least 8 refusals (measured 41 to 47), so the liveness claim
does not rest on the per-exchange overlap alone.

**The liveness bound is a wall clock, and this is the case rmp #2567 left standing.** That
task removed deadlines used as oracles over BOUNDED payloads, where a deadline can only
misattribute a slow machine. This wait is bounded by nothing: a server that starved honest
traffic under aggregate pressure would never serve it, so the honest client would wait for
ever and only a clock can tell. The bound is set against a MEASUREMENT rather than a
guess — 30 s against a measured 633 us median and 902 us worst honest exchange with the
fleet pushing under `-race`, about 33,000x the worst observed service time — the
message that fires says STARVED rather than "slow", and it is paired with the claim that
matters and involves no clock at all — every honest exchange must return the value the
harness chose for it, so a reply belonging to a different exchange is a failure and not a
pass.

The swarm's sizing also has to keep the honest client clear of the REASSEMBLY reader, whose
budget breach is **not** the connection-preserving refusal the decode layer's is: it
returns `packstream.ErrInboundBudgetExceeded` as a READ error (`chunking.go:223-227`) and
the serve loop tears the connection down on every read error (`serve.go:1237-1247`). The
pool floor is therefore computed rather than hoped for — one abuser's charge is 4.4 MiB of
an 8 MiB pool, leaving 3.6 MiB, and three refused abusers transiently holding one 1 MiB
reservation chunk each leaves a worst floor of ~0.6 MiB against ~100 bytes of honest
reassembly — and
`swarm-no-transport-loss` reports it by name if the sizing ever stops holding.

The coverage gate is a separate family, as elsewhere. It requires the boundary window to
have BRACKETED the transition (window counts and run-wide counts are kept separate, after
an earlier revision summed them and a window that had gone entirely green still satisfied
the bracketing clause because the breach arm's single refusal was being counted as if the
scan had produced it), requires both a refusal and an ACCEPT (a pool stuck at zero refuses
everything and satisfies "every refusal is typed" perfectly), requires the leak probe's
slack to stay tight, requires the nesting family to be complete and to have produced all
THREE outcomes (with fewer, the distinctness clause returns without adjudicating anything),
requires an over-nested HELLO to have been refused so the pre-authentication path was
actually visited, requires the control to be a genuinely different configuration that
genuinely disagreed, and requires the live census to be non-empty so "the refused write
left nothing behind" is not trivially true of an empty graph. 29 named contract clauses and
17 `nv-` ones; 52 falsifiability subtests each perturb one field and assert the clause that
must catch it, and a further 6 subtests drive every pairwise collapse direction (see below).

**Four of those checks are guards on the harness, and say so (rmp #2576, #2579).**
`nesting-not-by-size`, `pool-control-identical-payload`, the `ControlBudget <= Budget`
branch of `nv-control-differs` and `nv-nesting-family-complete` read quantities that no
server behaviour can move: the nesting payloads are built by the harness and top out at
6166 wire bytes against a 64 KiB ceiling, the control's element count is copied from the
breach arm inside the same run, both ceilings are compile-time constants, and a missing
nesting arm aborts the run before adjudication. Each is kept — a harness that has been
re-wired is worth catching — and each carries the label "A guard on the HARNESS, not on the
server", so a reader does not count it as evidence about the subject. The deterministic
`nv-leak-probe-tight` is deliberately NOT in that list: it probes at the MEASURED boundary,
so a server that admitted fewer elements widens its slack and fires it.

**`nv-swarm-leak-probe-tight` was the fifth, and is now a real clause about the server
(rmp #2579).** It shares its checker with the deterministic `nv-leak-probe-tight` and
differed from it only in which probe it was handed: the swarm's was sent at the MODELLED
boundary, so its slack was the constant 16 B and neither a server nor anything else could
move it. `RunBoltDecodeSwarm` now MEASURES the boundary first, with a seven-probe scan on
one connection against a pristine pool, and sizes the post-swarm leak probe to the largest
element count the server actually admitted. Measuring it DURING the swarm would have been
unsound — under aggregate pressure the boundary is whatever the other connections happen to
be holding at that instant — so the scan runs before `boltDecodeSwarm.run` starts a single
goroutine and cannot race the abusers. That the clause is now server-falsifiable was
demonstrated rather than asserted: shrinking the real server's pool by one element's charge
(48 B) while leaving the harness's declared ceiling alone moved the measured boundary from
174734 to 174733, widened the slack from 16 B to 64 B, and fired the clause; 96 B moved it
two elements and gave 112 B. Measured across 13 seeds including the catalogue default, the
scan brackets the transition every time (four probes accepted, three refused), the measured
boundary equals the model's, and the slack stays 16 B, so no verdict changed. An A/B with
the scan disabled showed it does not disturb the fleet's dynamics either — refusals across
the honest run were 59-65 without it and 59-64 with it, and every wide exchange still
straddled. It costs about 0.35 s per swarm run. Because the probe now depends on a
measurement, the new `nv-swarm-window-spans-boundary` reports a scan that failed to bracket
the transition, so the "no calibrated size" fallback to the model's boundary can never be
taken silently — which is the only way this clause could quietly become a harness guard
again.

**The collapse-detection claim is pinned by a test rather than by a deleted probe
(rmp #2579).** `checkBoltDecodeCapsAnswerDifferently`'s godoc says the clause is not the
only thing standing between the run and a server that answers every abuse vector with one
code, because the literal-pinning clauses catch such a collapse first. rmp #2576 justified
that wording with a throwaway probe and then deleted it, leaving the claim resting on a
measurement no longer in the tree: narrowing `nesting-answer` later would silently make the
distinctness clause the sole detector and the godoc quietly false.
`TestBoltDecodePressure_DistinctnessIsNeverTheSoleCollapseDetector` is that probe, kept. It
enumerates the ordered pairs of the three vectors rather than listing them — six directions
over three vectors, so a fourth vector cannot be added without the table growing — and for
each one requires both that `caps-answer-differently` fires and that a literal-pinning
clause fires with it. Re-derived independently, a literal-pinning clause co-fires in 6 of 6:
`pool-refusal-typed` with `pool-refusal-retryable` on the two directions that move the pool
arm, `nesting-answer` on all four that move a nesting arm, joined by
`nesting-is-not-backpressure` on the two that collapse onto the budget code. The two
directions between the wire cap and the parameter cap rest on `nesting-answer` ALONE, which
is what gives the test its teeth: narrowing that clause to stop pinning the code turns
exactly those two red.

**One collision was found in the harness itself.** Generalising rmp #2485's single-scenario
seed-mix guard into a table over every Bolt scenario immediately went red:
`txQuotaSeedMix` and `boltTxQuotaDefaultSeed` were both `0x2482_9074`, so `bolt-tx-quota`
built its `SimDisk` from `NewSeed(0)` on the one run every report starts from — precisely
the defect the original guard was written to prevent, unnoticed because the guard had been
copied per surface instead of iterating. Fixed to `0x2482_5EED`, and the table now fails if
a Bolt scenario is added to the catalogue without an entry.

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
6. **A struct edge weight is silently dropped by the snapshot CSR writer**
   (fail-silent, permanent data loss). `store/snapshot` never consulted
   `txn.WeightCodec`: `csrWeightSize` (`store/snapshot/writer.go`) returns 0 for
   every weight type outside the Go primitives — every struct, and every NAMED
   integer type such as `time.Duration` — so `WriteCSR` emitted `hasWeights=0`,
   the same encoding a deliberately weightless graph produces, and the
   checkpoint then truncated the WAL prefix holding the true values. Measured by
   `codec-matrix` (rmp #2473): the same image returned **95 of 95** weights at
   the WAL-only boundary and **0 of 191** after one folding checkpoint.
   **FIXED (rmp #2526).** The snapshot persists weights of any type through the
   store's own codec (`checkpoint.WithWeightCodec` /
   `recovery.Options.WeightCodec`), and a weight that still cannot be encoded
   fails the snapshot write with `snapshot.ErrWeightNotPersistable` instead of
   publishing a weightless image, so the WAL prefix holding the surviving copy
   is never truncated. The matrix assertion is inverted: every arm must now
   confirm its weights after a SNAPSHOT-ONLY recovery, and a non-vacuity gate
   refuses a run in which any arm never crossed a checkpoint.
7. **`jsonl.WriteWithProps` is not byte-reproducible** (fail-silent; no data
   loss). It emits one `"property"` record per entry of the node's property
   MAP, iterating it directly (`graph/io/jsonl/writer.go`), so two exports of
   the same graph carry the same records in a different order. Measured by the
   graph/io surface arm (rmp #2480) over a four-node, seven-kind fixture:
   **7 of 7** repeat exports differed byte for byte from the first, while
   `graphml.WriteWithProps` — which emits in a fixed key order — differed in
   **0 of 7**, as did `dot.Write`, `csv.Write`, `jsonl.Write` and
   `graphml.Write`. Nothing is lost (the reader is order-insensitive within a
   node, and the round-trip is exact), but the artefact cannot be compared by
   digest, diffs spuriously, and defeats content-addressing — and it made a
   seed-derived byte offset into the export non-reproducible, which is how the
   simulator found it. The mutation sweep canonicalises the JSONL property
   suffix for its own use and the verdict asserts byte-reproducibility for every
   OTHER encoder, so this one is recorded rather than papered over. Not fixed:
   the fix is in `graph/io`, outside that task's scope.
8. **`Options.Logger` never reached the Bolt session** (fail-silent; no data
   loss). `Options.Logger` documents itself as "the structured logger for server
   events", and `NewServer` threads it into the `Server`. `newSession`
   nevertheless hard-coded `slog.Default()` and the bootstrap never overrode it,
   so all eleven session-level log sites — every refused credential, every failed
   query, every failed `BEGIN` and `COMMIT`, the transaction-quota refusal, and
   the received-bookmark debug records — bypassed the configured logger. That is
   the majority of what a Bolt server logs: an embedder who routed the server's
   output to a file, a collector, or `io.Discard` still had its
   security-relevant events written to the process default. Found while building
   the auth scenario (rmp #2481), which discards its server's log precisely
   because it provokes dozens of refused credentials on purpose — and saw them on
   stderr anyway. Measured on the unfixed build: a capturing handler received 4
   records and NEITHER the authentication failure nor the query failure was among
   them. **Fixed** — the bootstrap calls `sess.setLogger(s.log)`;
   `bolt/server/session_logger_test.go` is the regression guard and was verified
   to fail without the fix.
9. **An OPERATOR termination tells the client its transaction timed out and that
   a writer lock was released** (fail-misleading; no data loss).
   `Server.TerminateTransaction` routes through `Session.reapTimedOutTx`, which
   is shared with the idle/total reaper, so the client is answered
   `Neo.ClientError.Transaction.TransactionTimedOut` with "the transaction has
   been terminated because it exceeded its timeout; the writer lock was
   released". BOTH halves are false for a termination on demand: it exceeded no
   timeout, and no writer lock has been held for a transaction's lifetime since
   rmp #2305/#2306 retired it (`Engine.beginTxSession` acquires no writer
   serialisation, `cypher/exectx.go`). A driver cannot distinguish an operator
   ending a transaction from a transaction that ran too long, which is exactly
   the distinction an operator needs the client's logs to preserve. Filed as
   **rmp #2560**; both strings are PINNED against named constants in
   `internal/sim/bolt_tx_registry.go`, and both the abandoned-registry and the
   operator-terminate arms adjudicate against them, so the eventual correction
   fails those arms deliberately instead of slipping through.
10. **A quota-refused `BEGIN` leaves the session READY** (behavioural
    inconsistency; no data loss). `handleBegin`'s per-principal-cap branch
    returns before `Transition` and never calls `enterFailed`
    (`bolt/server/session.go:1597-1606`), unlike the `newTx` failure path
    directly above it, which does (`:1583`). The two failures are a step apart in
    one handler and leave the session in different states, so a client whose
    `BEGIN` was refused by the cap is served normally on its next message while
    one refused by `newTx` must `RESET` first. Which behaviour is correct is a
    contract question, not an obvious bug — the Bolt state machine arguably
    should not fail a session for a resource refusal — so it is filed as **rmp
    #2561** and the OBSERVED behaviour is pinned: `internal/sim/bolt_tx_quota.go`
    drives a statement down the refused connection and requires it to be served,
    so whichever way #2561 is closed, the change is deliberate.


## Documented debt / out of scope

- ~~**GraphML round-trip under fault** is not yet covered.~~ **Closed** (verified
  rmp #2471): `internal/sim/storage_fault_scenarios.go` carries the
  property-graph fixture this bullet said was missing (`graphmlModel`, labelled
  and propertied) and drives it through both halves of the ST8 contract —
  `graphmlRoundTripClean` (exact round-trip via `graphml.WriteWithProps` /
  `ReadWithProps`) and `graphmlExportFaultFailsClean` (a clean typed failure
  under a sub-full ENOSPC bound, with no silently-accepted partial). CSV, JSONL
  and GraphML are all covered. **Extended** (rmp #2480): DOT was still
  uncovered when that was written — `graph/io/dot` was imported nowhere in the
  simulator — and is now adjudicated by cross-format agreement, alongside the
  JSONL property path, the whole `csv.Options` space, every defensive cap and
  mid-parse cancellation on every `*Ctx` reader. See
  [graph/io completeness](#graphio-completeness-dot-the-property-path-the-caps-and-cancellation-rmp-2480)
  above.
- **Snapshot isolation for read transactions** shipped in rmp #2307 (sprint
  334) via MVCC version chains rather than the copy-on-write epic (#1671) once
  considered for it. The DST scenarios assert no-dirty-read, which the stronger
  contract still satisfies; the isolation level itself is gated by
  `cypher/readtx_snapshot_test.go` and `bolt/server/e2e_readtx_snapshot_test.go`.
- ~~**Multi-member WAL group-commit coalescing / fail-all** is engine-unreachable
  (serialised under `visMu`).~~ **Closed** (rmp #2471): the premise was false —
  an ordinary write has taken the barrier SHARED since sprint 334, and multi-member
  coalescing is measured at 61 followers in 483 rounds through the engine. Both
  coalescing and fail-all are now gated in the DST; see
  [Group-commit coalescing and fail-all](#group-commit-coalescing-and-fail-all-rmp-2471)
  above.
- **`wal.ErrWALLocked` and the WAL `O_NOFOLLOW` refusal are STRUCTURALLY
  unreachable through `SimDisk`** — it is a flat in-memory key table with no
  inodes, links or advisory locks — so they are **not** covered by any seeded,
  crash-injecting scenario, and will not be unless `SimDisk` grows a
  lock-and-symlink model. Since rmp #2472 both are driven against a real
  temporary directory by `RunWALRealFSGuards`, alongside the store-layer unit
  tests (`store/wal/lock_test.go`, `store/wal/symlink_escape_test.go`); that arm
  is their only representation in the simulator, it leaves the simulated disk to
  do it, and it makes no claim on a platform that cannot express the guards. See
  [The WAL writer surface](#the-wal-writer-surface-watermark-frame-contiguity-truncate-guards-rmp-2472)
  above for why a real directory was chosen over a model.
