# Changelog

All notable changes to GoGraph are documented in this file. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

Durability and correctness fixes. Both compliance invariants held: the
openCypher TCK execution gate stayed at **3 897/3 897** and every ACID
property was preserved on every change.

### Added

- **Durable node tombstones** — new `snapshot` component `tombstones.bin`
  (`WriteTombstones` / `ReadTombstones` / `ApplyTombstonesToGraph` /
  `TombstonesReadback` / `TombstonesFile` / `ErrTombstonesCorrupted`, plus
  `LoadedSnapshot.Tombstones`). It is optional and additive — emitted only
  when the graph has tombstoned nodes and does **not** bump the manifest
  version, so snapshots stay byte-identical for graphs that never delete.
- `lpg.Graph.TombstonedIDs`, `TombstoneCount`, and `RestoreTombstones` —
  accessors and load-phase restore for the node-tombstone set.
- `lpg.Graph.RemoveEdge` — the edge-deletion entry point used by the Cypher
  executor and WAL replay; clears the per-pair edge labels/properties once
  the endpoint pair is fully disconnected.
- `recovery.Result.SnapshotTombstones` — count of tombstones restored from
  the snapshot before WAL replay.

### Fixed

- **Node deletion is now durable across a store reopen.** Deleted nodes no
  longer resurrect as label-stripped, undeletable "ghosts": the in-memory
  tombstone set is persisted in the snapshot and reconstructed on WAL replay
  (`OpRemoveNode` now re-tombstones, not merely strips labels/properties),
  and re-creating a removed key revives the node under the same stable
  `NodeID`. Acute for one-process-per-command callers that checkpoint and
  truncate the WAL after every write.
- **Recovery ordering.** On the self-sufficient (mapper.bin) path the
  snapshot's labels and properties are now applied **before** WAL replay, so
  a WAL-tail delete-then-recreate no longer carries stale snapshot labels or
  clobbers the re-created property values.
- **Edge deletion hygiene.** Removing an edge clears its per-pair labels and
  properties once the endpoint pair is fully disconnected, so re-creating an
  edge between the same endpoints no longer resurrects the removed
  relationship's type or properties (multigraph-safe: a shared per-pair label
  is kept until the last parallel edge is removed).
- **`range()` int64 step overflow** (#1238) — an under-cap range whose last
  element sits at the int64 boundary (e.g. `range(9223372036854775805,
  9223372036854775807)`) no longer overflows the materialisation loop into a
  non-terminating, OOM-ing append; it iterates the overflow-safe element count.
- **Build tooling** (#1241) — repaired the malformed `staticcheck.conf` (an
  invalid `[staticcheck "<path>"]` per-file table) so standalone
  `staticcheck ./...` runs again, with directory-scoped overrides for the
  documentation regexes and the generated parser; removed a dead test helper
  it surfaced.

## [3.0.0] — 2026-06-01

The third major release. v3.0.0 is a **security-hardening and
distribution** release: it lands a complete, proof-of-concept-confirmed
remediation of a 20-finding security audit (2 Critical, 7 High, 1 Medium,
4 Low, 6 Info) and adopts the conventional URL-based Go module path so the
library can be consumed with `go get`. Both compliance invariants were held
throughout — the openCypher TCK execution gate stayed at **3 897/3 897**
(16 006/16 006 steps) and every ACID property was preserved on every change.

This release contains **two breaking changes**: the Go module import path
changed, and `bolt/server.NewServer` now returns an `error` and fails closed
when no authentication handler is configured. Both are covered, with
copy-pasteable before/after snippets, in the
[2.x → 3.x migration guide](docs/migration-2-to-3.md). The full narrative,
the per-severity finding table, and the validation evidence are in
[release-notes/v3.0.0.md](release-notes/v3.0.0.md).

### Changed (breaking)

- **Module import path** — the Go module path changed from `gograph` to
  **`github.com/FlavioCFOliveira/GoGraph`** (#1239). Every external and
  internal importer must rewrite `import "gograph/..."` to
  `import "github.com/FlavioCFOliveira/GoGraph/..."`. This adopts the
  conventional URL-based path that matches the GitHub repository, enabling
  `go get` and external import. **Unchanged on purpose:** the `gograph`
  package identifier, the `gograph-*` cache-directory names, the `rmp`
  roadmap name, and the frozen historical benchmark captures. No
  `/v3` major-version path suffix is added — the import base stays
  `github.com/FlavioCFOliveira/GoGraph` (matching the v2.0.0 precedent).
- **`bolt/server.NewServer` signature** — was
  `func NewServer(eng, opts) *Server`; now
  `func NewServer(eng, opts) (*Server, error)` (#1223, finding L1). A nil
  `Options.Auth` previously installed `NoAuthHandler` silently, so an
  `Options{}` shipped a fully open server. `NewServer` now **fails closed**:
  a nil `Auth` returns the new `ErrNoAuthHandler` sentinel. To run without
  authentication (development or testing) set `Auth: NoAuthHandler{}`
  explicitly — the explicit value is itself the opt-in and still logs the
  insecure-default warning. All in-tree callers (examples, tests, bench)
  were updated.

### Security

The 2026 security audit identified 20 findings; all 20 are fixed in v3.0.0,
each confirmed with a proof of concept and each landed without lowering the
TCK count or weakening an ACID property.

- **C1 (Critical)** — `bolt/packstream`: `Decoder.ReadValue` recursed one
  stack frame per nesting level with no bound, so a single pre-auth `HELLO`
  frame of repeated `0x91` markers triggered a fatal stack-overflow that
  killed the process. Bounded to `maxValueDepth = 128` with the
  `ErrNestingTooDeep` sentinel across all three recursive arms (#1218).
- **C2 (Critical)** — `cypher/parser`: the AST visitor recursed one frame
  per nesting level with no bound; a ~300 KB query of nested brackets
  triggered a fatal stack-overflow reachable from `Engine.Run` and Bolt
  `RunInTxAny`. `guardInput()` now runs first in `Parse`/`ParseStrict` with
  `maxQueryBytes = 1<<20` and `maxNestingDepth = 256` (the latter via an
  O(n) scanner that ignores brackets inside string literals and comments),
  returning `*ParseError` instead of crashing (#1219).
- **H1 (High)** — `store/csrfile`: `bindSlices` reinterpreted mmap bytes
  using header counts/offsets with no validation, so a crafted header (with
  a recomputed CRC) could panic on a bounds check or build an
  `unsafe.Slice` view past the mmap via `uint64` overflow (OOB read /
  disclosure). `Header.validate` now recomputes the canonical
  overflow-safe layout and requires exact offset equality before the
  `unsafe.Slice`, returning `ErrFileCorrupted` (#1226).
- **H2 (High)** — `bolt/packstream`: the decoder allocated a slice/map
  sized by a wire-controlled 32-bit length prefix before reading the bytes,
  so a 5-byte marker could force a multi-gigabyte (bytes/string) or ~64 GiB
  (list/map) allocation pre-auth. A per-message remaining-byte budget now
  rejects any length/count exceeding the input with `ErrLengthExceedsInput`
  before the allocation (#1221).
- **H3 (High)** — `store/snapshot`: `ReadCSR` allocated slices sized by
  unvalidated `uint64` counts from the file (gigabytes to terabytes) and
  computed `weightBytes` with an overflowing multiply. `readCSRLimited`
  now bounds every real load path by the manifest `FileEntry.Size`, with a
  `1<<40` backstop and overflow-safe (`bits.Mul64`) weight sizing (#1227).
- **H4 (High)** — `store/snapshot`: `ApplyCSRToGraph` read vertex/edge
  offsets from the file with no monotonicity or range check, so a crafted
  offset drove an out-of-bounds panic and a `start > end` pair underflowed
  a metric counter. The offset array is now validated once (monotonic and
  in range) before replay, returning `ErrCorrupted` (#1228).
- **H5 (High)** — `store/snapshot`: `LoadIndexes`/`WriteIndexes` built
  `filepath.Join(idxDir, name+".bin")` from the attacker-controllable
  manifest index name, so `"../../../../tmp/pwned"` or an absolute path
  escaped `idxDir` (arbitrary file read on recovery). `validateIndexName`
  now rejects any name that is not its own `filepath.Base`, fails
  `fs.ValidPath`, contains a separator or `".."`, or is absolute, failing
  stop with `ErrManifestCorrupted` (#1229).
- **H6 (High)** — `cypher/funcs`: `range()` pre-computed a capacity that
  could overflow or be astronomically large, so `range(1, MaxInt64)`
  panicked with `makeslice: cap out of range` and `range(1, 5e9)` allocated
  tens of gigabytes and was OOM-killed — both reachable from any untrusted
  query. The materialised element count is now capped at
  `maxRangeElements = 1e8`, derived overflow-safely in `uint64`, returning a
  typed `expr.EvalError` (`ArgumentError: NumberOutOfRange`) over the cap
  (#1235).
- **H7 (High)** — `bolt/server` + `cypher`: a recoverable panic in the Bolt
  per-connection goroutine or in `Engine.Run`/`RunInTx` would unwind past
  the library and crash the whole process. `recover()` boundaries were added
  in `handleConn` (log + `bolt.server.conn.panics` metric + close one
  connection) and in `Run`/`RunInTx` (`ErrInternalPanic` typed error + log +
  panics metric). The `RunInTx` boundary also **rolls back the in-flight WAL
  transaction**, so the store single-writer mutex is never leaked on panic —
  preserving ACID (a subsequent write no longer deadlocks) (#1220).
- **M1 (Medium)** — `bolt/server`: `ConnTimeout` defaulted to `0`, so no
  I/O deadline was ever applied and the handshake could block forever
  (Slowloris connection-slot exhaustion). `NewServer` now defaults
  `ConnTimeout` to 30 s (idle, post-auth) and the unauthenticated handshake
  is bounded by a 10 s `DefaultHandshakeTimeout` (#1222).
- **L2 (Low)** — `store/wal`, `store/snapshot`, `store/csrfile`: WAL, CSR,
  and snapshot component files were created world-/group-readable
  (`0o644`/`0o666 & ~umask`). They are now created `0o600` (owner-only); the
  atomic write+rename publish protocol and all durability semantics are
  unchanged (#1230).
- **L3 (Low)** — `bolt/server`: new exported `DefaultTLSConfig()` helper
  returns a hardened baseline (TLS 1.2 floor, modern AEAD/ECDHE cipher
  suites, TLS 1.3 auto-negotiated) for operators to start from. Behaviour is
  unchanged for callers that pass their own config; a nil `TLSConfig` still
  means plaintext (#1224).
- **L4 (Low)** — `bolt/server`: `ROUTE` was accepted in `StateNegotiation`,
  letting an unauthenticated client elicit a routing-table response. It is
  now accepted only from `StateReady`/`StateTxReady` (verified compatible
  with `neo4j-go-driver`, which always completes `HELLO`/`LOGON` first), and
  the serve-loop decode-error `FAILURE` message is now sanitised instead of
  returning the raw internal error string (#1225).
- **I1 (Info)** — `store/csrfile`: `Reinterpret`'s `size*n` byte-requirement
  multiply could wrap `int` for an untrusted `n`, so a wrapped-small product
  could pass the length check and produce an out-of-bounds alias. The
  product is now computed in 64-bit precision (`math/bits.Mul64`) and
  saturates to `math.MaxInt` on overflow so the call takes the existing
  "data too short" panic path (#1231).
- **I2 (Info)** — `store/wal`: `Decode` allocated `make([]byte, plen)` (a
  `uint32`, up to ~4 GiB) before the CRC check. A `maxFrameSize = 1<<30`
  (1 GiB) bound now rejects an oversized `plen` with `ErrFrameTooLarge`
  before the allocation (#1232).
- **I3 (Info)** — `store/snapshot`, `store/recovery`: list-property decoders
  pre-sized a slice with an untrusted `uint32` count (up to ~4.3e9). The
  capacity hint is now clamped to `min(count, remaining/minElemBytes)`,
  bounding a hostile count while pre-sizing legitimate lists exactly (#1233).
- **I4 (Info)** — `store/snapshot`: component reads opened files with no
  symlink defence, so an untrusted snapshot directory could ship a component
  symlinked to `/etc/...`. `openSnapshotComponent` now opens with
  `O_NOFOLLOW` on Unix (degrading to a plain open on Windows); every read
  path is routed through it (#1234).
- **I5 (Info)** — `cypher`: `Engine.Run`/`RunInTx` parsed synchronously
  before consulting the context, so a cancelled context could not interrupt
  an expensive parse. A `checkContext` guard now runs before
  `parseAndAnalyse` / any `store.Begin`, returning
  `context.Canceled`/`DeadlineExceeded` promptly (#1236).
- **I6 (Info)** — `cypher/expr`: the `=~` operator recompiled its pattern on
  every evaluated row. A process-wide bounded FIFO cache (capacity 1 024) of
  compiled `*regexp.Regexp`, keyed by pattern string and guarded by a mutex,
  removes the per-row recompilation; behaviour is byte-identical, including
  the NULL-on-invalid-pattern mapping, and the bound prevents an untrusted
  pattern from driving unbounded growth (#1237).

### Fixed

- The Critical and High parser/decoder/persistence findings above
  (**C1, C2, H1–H6**) also fix concrete crash and out-of-memory bugs that a
  hostile input could trigger: pre-auth stack overflows (C1, C2), OOB reads
  on crafted CSR/snapshot files (H1, H4), and `makeslice`/OOM panics on
  oversized PackStream frames, snapshot CSRs, and `range()` bounds (H2, H3,
  H6).
- **`bolt/server`** (#1220, H7): a recoverable panic in a per-connection
  goroutine or in `Engine.RunInTx` no longer leaks the store single-writer
  mutex; the in-flight WAL transaction is rolled back on the panic path, so a
  subsequent write no longer deadlocks. A deadlock-freedom regression test
  covers the write path.

### Performance

- **`cypher/expr`** (#1237, I6): the `=~` operator no longer recompiles its
  pattern per row. `WHERE x =~ $p` over a large scan now compiles each
  distinct pattern at most once (bounded FIFO cache), eliminating redundant
  RE2 compilation on the hot path. Match results and NULL-on-invalid-pattern
  semantics are unchanged.

## [2.0.0] — 2026-05-31

The first major release since the 1.x line. v2.0.0 turns GoGraph from a
graph-algorithm library into a query-capable, server-ready graph engine,
and it ships the two compliance guarantees that define the project: it is
**100 % openCypher TCK-compliant at the execution level** (3 897/3 897
scenarios, 16 006/16 006 steps) and **100 % ACID-compliant** (Atomicity,
Consistency, Isolation, Durability) across the in-memory engine and every
persistence backend.

This release consolidates the two pre-release candidates —
[v2.0.0-rc1](release-notes/v2.0.0-rc1.md) (sprints 21–32: the Cypher
execution engine, the Bolt v5 server, and the TCK harness at 90.7 % parser
/ 10.4 % execution) and [v2.0.0-rc2](release-notes/v2.0.0-rc2.md)
(sprints 37–40: WAL durability bridging, 99.5 % parser, 25.8 % execution) —
together with the post-rc2 work recorded below: openCypher execution
conformance driven from 25.8 % to **100 %**, the ACID hardening programme
(F1–F5), the production-readiness blockers (Sprints 125–126), the godoc
effort (+95 runnable examples), the examples-excellence pass, and the
breaking v1 WAL-format excision.

The complete release narrative — grouped change explanations, the breaking
changes, the 1.x → 2.0 migration walkthrough, and the validation and soak
evidence — is in [release-notes/v2.0.0.md](release-notes/v2.0.0.md). Callers
upgrading from a 1.x release must read the
[1.x → 2.x migration guide](docs/migration-1-to-2.md).

### Added — Sprint 125 (Production readiness — P0 blockers & P1 items, 2026-05-30)

- **`internal/metrics/prometheus`**: new subpackage providing a self-contained
  Prometheus text-exposition-format backend implementing `metrics.Backend`. No
  external dependencies; exposes `New()`, `WriteText(io.Writer)`, and
  `Handler() http.Handler` for `/metrics` scrape endpoints.
- **`bolt/server`**: `Options.MaxStatementTimeout` — server-side cap on
  client-supplied query timeouts. When set, client timeouts exceeding the cap are
  silently clamped; queries with no client timeout receive the cap unconditionally.
- **`bolt/server`**: startup warning via `slog.Warn` when `NoAuthHandler` is the
  active auth handler, making insecure-default deployments immediately visible in logs.
- **`cypher`**: per-query statement-now registry (`nowAwareRegistry`) so concurrent
  `Engine.Run` and `Engine.RunInTx` calls each observe an independent frozen
  timestamp for temporal `now` constructors — resolves the process-global race on
  `NOW()`, `date()`, `datetime()`, etc.

### Fixed — Sprint 125

- **`cypher/api.go`**: `Engine.Run` and `Engine.RunInTx` now emit latency
  histograms (`cypher.Run`, `cypher.RunInTx`) and paired error counters via
  `internal/metrics`, satisfying the CLAUDE.md mandate for latency observation
  on every public blocking API.
- **`bolt/server/session.go`**: internal error messages are sanitised before
  being sent to Bolt clients; raw `err.Error()` strings (including auth-failure
  details and internal stack paths) are replaced with safe client-visible
  categories. The real error is logged server-side with the session correlation ID.
- **`.github/workflows/tck.yml`**: added explicit `TestTCKExecution` step with
  `-timeout 600s` so the full 3 897-scenario execution gate runs on every PR/push
  independently of the `make test-short` timeout budget.

### Changed — Sprint 125

- **`docs/tck/DIVERGENCES.md`**: execution-level table updated to reflect
  3 897/3 897 (100 %) at HEAD; all Category 5 gaps marked RESOLVED.
- **`README.md`**: execution-level TCK status updated from 39.4 % to 100 %
  (3 897/3 897); examples count updated from 23 to 25.
- **`cypher/tck/conformance_history.go`**: package godoc extended with the full
  execution-level progression through Sprints 58–64 and the 100 % milestone.

### Added — Sprint 119 (Godoc effort, 2026-05-30)

- **+95 runnable `Example*` functions** across all ~44 exported packages
  (2 → 97 total), covering every major API surface with compilable, testable
  demonstrations.
- **Package-level doc comments** on all library packages that lacked them.
- **Entrypoint map** in the root `doc.go` cross-linking every subsystem.

### Added — Sprint 110 (ACID hardening, 2026-05-29)

- **`store/txn`**: `ErrCommittedNotApplied` sentinel (F5) ensures post-durability
  apply failures are never ambiguous plain errors.
- **`store/wal`**: parent-directory `fsync` on WAL file creation (F4).
- **`store/checkpoint`**: self-sufficiency gate before WAL truncation (F2) —
  checkpoints include `mapper.bin` before truncating.
- **`store/txn`**: isolation barrier via `lpg.Graph.ApplyAtomically` / `Graph.View`
  (F3) — no reader observes a partial transaction.
- **`store/recovery`**: `committed_apply_failed_test.go` — cross-process test
  proving recovery reconciles a durable-but-unapplied transaction (F5).

### Added — Sprint 58 (Test infrastructure & shape generators)

- **Makefile**: three new layer-aligned test targets `test-short`, `test-soak`,
  `test-nightly` (each with `-race -count=1`). Two new CI pipeline targets
  `ci-soak` and `ci-nightly`. The existing `ci` target now delegates to
  `test-short`. See [`docs/test-layers.md`](docs/test-layers.md) for the
  full specification.
- **`internal/testfs`**: `FaultFile` FS fault-injection wrapper with
  `FailWritesAfterBytes`, `ReturnENOSPC`, `FsyncDelay`, `CorruptOnRead`.
  `store/wal` now accepts a `walFile` interface and exposes `OpenWith` for
  test-time fault injection.
- **`internal/crashinject`**: subprocess crash-injection harness.
  `Breakpoint(name)` self-kills via SIGKILL; `Run(t, scenario, Opts)`
  spawns `cmd/crashinject-helper` and returns `Out{Killed, Signal, Dir}`.
- **`internal/invariants`**: graph assertion helpers `AssertConnected`,
  `AssertDAG`, `AssertBipartite`, `AssertDistanceBound`, `AssertShapeEqual`,
  `BuildBFSDepths`.
- **`internal/goldens`**: golden-file helper `Assert(t, path, got)` with
  unified-diff output, `-update` flag / `GOGRAPH_UPDATE_GOLDENS=1` env,
  and atomic write (temp + rename).
- **`internal/subproc`**: subprocess helper `Register` + `Dispatch` +
  `Run`/`RunCtx`/`RunWithTimeout` for deterministic cross-process tests.
- **`internal/shapegen`**: soak-layer tests for LDBC Graphalytics reference
  graphs (`cit-Patents`, `dota-league`, `kgs`).

Gate status at the v2.0.0 tag: **parser-level TCK 100 %** (3 897/3 897) and
**execution-level TCK 100 %** (3 897/3 897, 16 006/16 006 steps,
`tckExecutionBaseline = 3897` enforced on every PR). ACID hardening complete
(F1–F5 all closed). **Soak provenance (honest):** the 1 024-connection
4-hour Bolt soak PASSED, but against commit `b5453b9` — six infrastructure
and test-only commits behind the `v2.0.0` tag (`#1213`–`#1216`: CI action
bumps, a lint fix, a coverage-gate fix, and two flaky-test fixes; none touch
a runtime hot path). A canonical full 4-hour **Cypher** mixed-load soak has
not yet been recorded against the tag — only a 30-minute Cypher read/write
run (2026-05-21) is archived. A 60-second soak-smoke was run against the
release commit and passed (zero goroutine growth, stable heap); the full
4-hour soak is being re-run against the tag out-of-band. See
[release-notes/v2.0.0.md](release-notes/v2.0.0.md) for the full evidence
table, `docs/semver.md` for the release-gate specification, and
`docs/tck/DIVERGENCES.md` for the authoritative pass-rate table.

### Added — Sprint 78 (Production-readiness blockers)

- **`search/`**: NaN/Inf float-weight gate at the public-API boundary of
  Dijkstra, A\*, Bidirectional Dijkstra, Yen, K-shortest loopless, Prim,
  Kruskal, Floyd-Warshall, and Johnson APSP — returns
  `ErrInvalidInput` (existing sentinel reused). Integer Weight kinds
  short-circuit in O(1) via type switch (closes T926).
- **`search/floyd_warshall.go`**: CLRS §25.2 post-DP diagonal scan
  detects negative-weight cycles and returns `ErrNegativeCycle`
  (reuses the BellmanFord sentinel). Previously the matrix silently
  returned distances polluted by the cycle (closes T927).
- **`search/bibfs.go`**: bidirectional BFS now completes the current
  frontier-expansion level before declaring the meet point, picking
  the minimum-total-distance intersection. Previous first-collision
  rule could return paths strictly longer than the unweighted
  shortest distance on asymmetric topologies (closes T928).
- **`cypher/plan_cache.go`** + **`cypher/exec/{create,drop}_{index,constraint}.go`**:
  plan cache is invalidated after CREATE/DROP INDEX/CONSTRAINT via a
  per-operator `onSchemaChange` callback wired to `Engine.ClearPlanCache`.
  `IF [NOT] EXISTS` silent-success branches do NOT invalidate. New
  `cypher.plan_cache.invalidations` counter (closes T933).

### Fixed — Sprint 78

- **`search/dijkstra_ctx_cancel_test.go`**: `TestDijkstraCtx_Cancel_ViaChan`
  bumped from 100 k to 1 M nodes so the traversal outlives the 1 ms
  cancellation goroutine on Apple M-series and other fast hardware
  (closes T925).

### Documentation — Sprint 78

- **`README.md`**, **`docs/semver.md`**, **`docs/tck/DIVERGENCES.md`**:
  reconciled drift across parser conformance (100 % everywhere),
  snapshot manifest version (`3` in `semver.md`, matching the writer),
  and the v2.0.0 execution-TCK milestone target (80 % everywhere,
  matching the release gate). Closes T936.

### Removed (breaking) — v1 WAL format excision (#929)

- **`store/txn`**, **`store/recovery`**: the legacy **v1 WAL record
  format** (untagged, `fmt.Sprintf`-encoded endpoints) and its entire
  read/write API are removed. Gone: the `txn.NewStore` constructor and
  the `recovery.OpenString`, `recovery.OpenStringCtx`,
  `recovery.OpenWithCodec`, `recovery.OpenWithCodecCtx`,
  `recovery.OpenWithOptions`, and `recovery.OpenWithOptionsCtx` read
  wrappers. Use `txn.NewStoreWithCodec` / `txn.NewStoreWithOptions` to
  write and `recovery.Open` / `recovery.OpenCtx` (with explicit
  `recovery.Options` codecs) to read.
- **`store/recovery`**: `recovery.Decode` now rejects any WAL record
  whose leading byte is neither `txn.OpRecordV2` (`0xFE`) nor
  `txn.OpRecordV3` (`0xFD`) with the new sentinel
  `recovery.ErrUnsupportedRecordVersion`; a legacy v1 frame on disk is
  surfaced via `Result.TailErr` rather than mis-decoded. There is **no
  in-place v1 WAL reader** in 2.0.0 — a v1 corpus must be migrated to a
  typed v2 store with a 1.x build **before** upgrading (see the
  [1.x → 2.x migration guide](docs/migration-1-to-2.md)).
- **`store/txn`**: `txn.OpRecordV1` (value 0) is retained only as a
  reserved, never-written sentinel so the rejection path can name the
  version it refuses; `Op.Version` continues to carry the v2/v3 tag.
  On-disk **snapshot** directories are unaffected — a v1 snapshot still
  loads, since only the WAL record format changed.

Adopters upgrading from a 1.x release line should consult the new
[1.x → 2.x migration guide](docs/migration-1-to-2.md), which collects
the API and on-disk-format changes (error returns on `AddNode`/`AddEdge`,
the bounded-resource defaults, the typed `store/txn` codecs, manifest
version bump, toolchain pin) together with the upgrade steps.

## [2.0.0-rc2] — 2026-05-21

Sprint 37–40 improvements over rc1: WAL durability bridging for all Cypher
write operators, parser conformance raised to 99.5 %, execution-level TCK
raised from 10.4 % to 25.8 %, UNWIND and variable-length expand wired,
Bolt benchmark suite, and a confirmed 1 024-connection 4-hour soak.

### Added — Sprint 37 (WAL Durability Bridge)

- `cypher/exec`: `walMutatorAdapter` wires every Cypher write operator
  (`CreateNode`, `CreateRelationship`, `Set`, `Remove`, `Delete`,
  `DetachDelete`, `Merge`) through the WAL on commit. Closes the
  "Cypher ↔ WAL integration" known limitation listed in rc1 (closes #370).
- `cypher/api`: `NewEngineWithStore` constructor accepts a `store.Store`
  so the engine owns its WAL-backed persistence lifecycle.
- `cypher/tck`: 4 new persistence tests — WAL round-trip (label survival,
  property survival), crash simulation (recovery after mid-write `kill -9`
  equivalent), and multi-snapshot accumulation (closes #370).

### Added — Sprint 38 (Parser Conformance Batch 1)

- `cypher/parser`: varlen dotdot (`[*1..5]`), zero-dot-float (`.5`),
  leading-dot-float (`.123e4`), neg-hex-oct (`-0xFF`), double-not
  (`NOT NOT x`), call-no-paren (`CALL db.labels` without `()`), and
  long-float (`1.23e-45`) literal forms now parse correctly (closes #375).
- `cypher/parser`: chained `WITH` clauses across multiple query parts
  supported via `MultiPartQ` rewrite — enables patterns like
  `MATCH … WITH … MATCH … RETURN …` (closes #376).
- Parser conformance: 90.7 % → 99.5 % overall TCK scenarios.

### Added — Sprint 39 (Parser Conformance Batch 2 + Execution Wiring)

- `cypher/parser`: integer tokenisation fix — bare integer literals
  previously mis-tokenised as identifiers in certain positions, causing
  380 TCK scenarios to fail at parse time; now parsed correctly (closes
  #381).
- `cypher/exec`: `Unwind` operator fully implemented and wired;
  `UNWIND list AS var` now executes end-to-end (closes #382).
- `cypher/exec`: variable-length path expand (`VarLengthExpand`) wired
  through the planner and execution engine; `MATCH (a)-[*1..n]->(b)`
  patterns now execute (closes #383).
- `cypher/api`: IR → exec operator mapping extended to cover all
  aggregation functions, `ORDER BY` expressions, and `SemiApply` /
  `RollUpApply` operators (closes #384).
- Execution-level TCK: 10.4 % → 24.8 % (closes #381–#384).

### Added — Sprint 40 (Bolt Benchmark Suite + Soak Confirmation)

- `bench/soak`: Bolt round-trip benchmark suite — 3 benchmark types
  (`BoltPingPong`, `BoltReadQuery`, `BoltWriteQuery`) × 5 concurrency
  levels (1, 8, 64, 256, 1 024 goroutines); results published to
  `docs/benchmarks/cypher.md` (closes #387).
- `bench/soak`: Cypher RW 30-minute soak test PASS — zero goroutine
  leaks, zero race conditions, heap delta within bounds (closes #388).
- `bench/soak`: Bolt 1 024-connection 4-hour soak PASS — gate threshold
  corrected (`MaxConnections` sentinel updated to match `1024`-connection
  burst); CI soak report updated in `soak-artefacts/` (closes #389).

### Fixed — Sprint 39

- `cypher/exec`: property access expressions (`n.prop`, `r.prop`) now
  correctly resolve against the in-scope row record during plan execution.

### Fixed — Sprint 40

- `bolt/server`: 4-hour soak gate threshold was set to the short-CI
  connection count rather than the full-run value; corrected to prevent
  false gate failures under `SOAK_FULL=1`.

### Performance — Sprint 39

- `cypher/plan`: `MATCH (n:L {prop: val})` now performs a hash-index seek
  instead of a full label scan, reducing per-lookup cost from O(|L|) to
  O(1) amortised.

### Known limitations (v2.0.0-rc2)

- Execution-level TCK conformance is 25.8 % (parser-level: 99.5 %). The
  remaining execution gaps are documented in `docs/tck/DIVERGENCES.md`.
- v2.0.0 stable is not yet cut; see the `[Unreleased]` section above for
  the gate requirements.

## [2.0.0-rc1] — 2026-05-21

Twelve sprints of work (21–32) delivering a full Cypher execution engine, a
Bolt v5 server, 90.7 % TCK conformance at the parser level, a comprehensive
benchmark harness, and a 4-hour soak-tested persistence layer. This is the
first major pre-release that exposes a query language interface.

### Added — Sprint 21–26 (Cypher Execution Engine)

- `cypher/exec`: Volcano/iterator operator tree — `AllNodesScan`, `LabelScan`,
  `IndexSeekHash`, `IndexSeekBTree`, `Filter`, `Project`, `Limit`, `Skip`,
  `Distinct`, `Union`, `Sort`, `Top`, `EagerAggregation`, `Apply`,
  `SemiApply`, `RollUpApply`, `OptionalExpand`, `VarLengthExpand`,
  `ShortestPath`, `Argument`, `SingleRow`, and `ProduceResults` (closes
  #241–#262).
- `cypher/expr`: expression evaluator with full built-in function library
  (string, math, aggregation, list, map, temporal); morsel-parallel evaluation
  path (closes #247–#250).
- `cypher/exec`: write operators — `CreateNode`, `CreateRelationship`, `Set`,
  `Remove`, `Delete`, `DetachDelete`, `Merge`, `WriteGraph`, `IndexBuffer`
  transactional index writeback (closes #268–#275).
- `cypher/exec`: `EagerAggregation` with COUNT, SUM, AVG, MIN, MAX, COLLECT,
  PERCENTILE_CONT, PERCENTILE_DISC, ST_DEV (closes #251).
- `cypher/plan`: LRU plan cache, cardinality estimator, stats maintenance with
  snapshot-rotation invalidation, index-registry introspection, scan-strategy
  selection, greedy join enumeration (closes #280–#285).
- `cypher/plan`: EXPLAIN / PROFILE with tree-formatted text output and
  `db_hits` accounting (closes #286–#289).
- `cypher/exec`: DDL operators — `CreateIndex`, `DropIndex`,
  `CreateConstraint`, `DropConstraint`; pre-write UNIQUE and NOT NULL
  enforcement via `ConstraintRegistry` (closes #294–#298).
- `cypher/procs`: thread-safe procedure registry; built-in procedures
  `db.indexes`, `db.constraints`, `db.labels`, `db.relationshipTypes`,
  `db.propertyKeys`, `db.schema.visualization`; `ProcedureCallOp` exec
  operator (closes #299–#305).
- `cypher/api`: `Engine.Run`, `Engine.RunInTx`, `Engine.RunAny`,
  `Engine.RunInTxAny` public API; plan caching and DDL pass-through (closes
  #247).
- `cypher/parser`: single-quote string pre-processor normalises `'…'` to
  `"…"` before ANTLR, resolving 579 previously skipped TCK scenarios (closes
  #306).
- `bench/cypher_ldbc`: LDBC IC1–IC14 benchmark suite with parallel variants
  and `docs/benchmarks/cypher.md` baseline (closes #290, #327).
- `bench/cypher_alloc`: per-operator zero-alloc gate tests using
  `testing.AllocsPerRun` (closes #329).
- `internal/stress`: write-conflict stress test for MERGE / SET / DELETE under
  `-race` (closes #277).

### Added — Sprint 27–31 (Bolt v5 Server + TCK Harness)

- `bolt/packstream`: full PackStream v2 encoder / decoder with zero-alloc
  primitive path, `sync.Pool`-backed instances (closes #307–#308).
- `bolt/proto`: Bolt v5 message types (12 request + 4 response), magic/version
  handshake supporting Bolt 5.0–5.6 and 4.4 fallback, chunked framing
  (closes #309–#310).
- `bolt/server`: Bolt v5 TCP server — state machine, `AuthHandler`
  (`NoAuth`, `BasicAuth`), TLS support, `Session` message dispatcher,
  explicit-transaction (`BEGIN`/`COMMIT`/`ROLLBACK`), peek-ahead `PULL`,
  bookmarks, routing table, structured error codes, graceful `Shutdown`
  (closes #311–#316).
- `bolt/server`: soak test (32 goroutines × 10 s CI; 1 024 goroutines × 4 h
  full), end-to-end smoke tests with `boltTestClient` harness, `docs/bolt.md`
  (closes #317–#318, #330).
- `cypher/tck`: godog-based execution-level TCK runner; 3 534 scenarios run,
  100 % pass on run, 90.7 % overall (363 grammar-gap skips); dedicated
  `.github/workflows/tck.yml` CI gate (≥ 90 % required); sprint-by-sprint
  conformance history in `cypher/tck/conformance_history.go` (closes
  #319–#326).

### Added — Sprint 32 (Performance + Hardening + Release)

- `perf(exec)`: `ResultSet.Next` now pre-allocates the `Record` map once and
  reuses it across iterations — IC1 benchmark: −50 % ns/op, −76 % B/op,
  −35 % allocs/op (closes #328).
- `bench/soak`: 1 024-connection 4-hour Bolt soak test gated on `SOAK_FULL=1`;
  CI soak report committed to `soak-artefacts/` (closes #330).
- `cypher/tck`: three persistence round-trip tests — 50-node label survival,
  multi-label (3 × 10), empty graph — via WAL + `WriteSnapshotFull` + recovery
  (closes #331).
- `docs/cypher.md`: comprehensive Cypher language reference (closes #332).
- `docs/bolt.md`: expanded with deployment, observability, and troubleshooting
  sections (closes #333).
- `docs/benchmarks/cypher.md`: cross-concurrency table, Bolt round-trip
  placeholder, reproducibility methodology (closes #334).
- `examples/22_cypher`: runnable social-graph demo using the Cypher engine
  (closes #335).
- `examples/23_bolt_server`: runnable Bolt v5 server start + graceful shutdown
  demo (closes #336).
- `scripts/profile-cypher.sh`: one-shot CPU + heap profiling script for IC
  benchmarks (closes #328).

### Changed

- `cypher/exec.ResultSet.Next`: Record map is now reused across calls;
  callers must not retain a `Record` pointer beyond the next `Next()` call
  (this was already the documented contract — no behaviour change for correct
  callers).

### Known limitations (v2.0.0-rc1)

- Execution-level TCK conformance is 10.4 % (parser-level: 90.7 %). The
  remaining execution gaps are documented in `docs/tck/DIVERGENCES.md`.
- Properties set via Cypher `CREATE`/`SET` bypass the WAL; a bridging step
  is required before snapshotting (see `cypher/tck/persistence_test.go`).
- Bolt round-trip benchmark is pending (`bench/soak/cypher_rw.go` scaffold
  exists).

## [1.2.0] — 2026-05-20

Seven post-v1.1.0 sprints (14–20) of documentation accuracy,
reliability evidence, coverage uplift, observability wire-up,
algorithmic completeness, transactional generality, durable LPG, and
finally the audit-driven correctness closeout. Nothing here is yet
tagged; the next release will be cut from this section.

### Added — Sprint 14 (Documentation Accuracy & Reliability Evidence)

- `README.md` Module Layout enumeration completed so every shipped
  subpackage appears (closes #179).
- `graph/index/label` lifted to 100% statement coverage via dedicated
  unit, property, and concurrency tests (closes #177).
- Error-path coverage for `store/wal`, `store/snapshot`, and
  `store/recovery` lifted by exercising every typed error in tests
  (closes #178).
- First publishable reliability soak — 30-minute mixed-workload run
  archived under `docs/benchmarks/v1.1.0.md` with heap, FD, and
  goroutine snapshots; verdict GREEN, heap delta −36.3 % (closes
  #181). The canonical 4-hour run is tracked as a follow-up.
- `CHANGELOG.md` "Unreleased" entry corrected the v1.1.0
  observability overstatement (the metrics call-site wire-up had not
  landed when v1.1.0 was cut — see Sprint 16 below; closes #167).

### Added — Sprint 15 (Coverage Uplift, GraphBLAS Baseline & Format Migration)

- Rolling-upgrade compatibility harness for `store/wal`,
  `store/snapshot`, and `store/csrfile` — committed v1 fixtures
  under `testdata/v1/` and added `format_compat_test.go` so every
  format change must keep loading the v1 fixture (closes #175).
- Aggregate library coverage lifted from 79.1 % to 91.3 % with a
  statement-weighted CI gate (`make cover-gate`, aggregate ≥ 85 %,
  per-package ≥ 75 %) — see `scripts/cover_gate.sh` (closes #176).
- Cross-library comparison `docs/benchmarks/comparison.md` extended
  with a SuiteSparse:GraphBLAS column (BFS 1.700 ms, SSSP 5.438 ms,
  PageRank 3.532 ms on M4) via `python-graphblas`, plus a bare-metal
  C harness `bench/comparison/c/lagraph_baseline.c` (closes #180).

### Added — Sprint 16 (Observability Completion & Algorithm Gap Closure)

- `internal/metrics` wired into 56 non-test files spanning every
  public blocking API in `search/`, `search/centrality/`,
  `search/community/`, `search/flow/`, `search/extern/`,
  `graph/io/{csv,graphml,dot,jsonl}`, and
  `store/{wal,snapshot,txn,checkpoint,recovery,bulk}`. Every wired
  symbol carries `metrics.Time("<pkg>.<Symbol>")()` plus a
  `<pkg>.<Symbol>.errors` counter; the inventory is documented in
  `docs/metrics.md`. Headline benchmark geomean +0.68 % vs the
  pre-wire baseline. This closes the v1.1.0 deferred wire-up
  (closes #168).
- `search.JohnsonAPSP` rewritten with the canonical Bellman-Ford
  reweighting via a virtual-source SPFA followed by per-source
  reweighted Dijkstra; supports mixed-sign weights and surfaces
  `search.ErrNegativeCycle`. The previous Johnson implementation
  was an alias for `DijkstraAPSP` and rejected negative weights.
  Sparse-graph win: −61.75 % vs Floyd-Warshall (closes #182).

### Changed (breaking) — Sprint 17 (Transactional API Generalization)

- `store/txn` now supports a typed `Codec[N]` over the node-key
  type, with built-in codecs for `string`, `int`, `int32`, `int64`,
  `uint64`, UUID, and any `encoding.BinaryMarshaler`. WAL v2 frames
  carry the `OpRecordV2 = 0xFE` tag; v1 frames (raw `fmt.Sprintf`
  bytes) remain decodable, so existing files do not break. New
  constructor `NewStoreWithCodec` (closes #173).
- `store/txn.Tx.AddEdge` now records the per-edge weight via a
  `WeightCodec[W]` over `int64`, `float64`, or any
  `encoding.BinaryMarshaler`. New opcode `OpAddEdgeWeighted = 4`
  for v2 frames; v1 and v2-without-weight frames still replay
  correctly. New constructor `NewStoreWithOptions` and sentinel
  `txn.ErrNoWeightCodec` for `(N, W)` pairs whose `W` is not
  declared in `Options` (closes #174).

### Added — Sprint 18 (LPG Snapshot Completeness)

- `store/snapshot` extended to persist LPG labels in `labels.bin`
  (`SLBL` magic, packed string table + per-node and per-edge
  records, CRC32C trailer). Manifest version bumped to 2; v1
  manifests still load (closes #170).
- LPG typed properties persisted in `properties.bin` (`SPRP`
  magic, fixed-width per-kind encoding covering all six
  `PropertyValue` kinds, 1 GiB cap). Round-trip property test via
  `rapid` (closes #171).
- Secondary indexes persisted in `indexes/<name>.bin`, one file
  per registered index. Native Roaring serialisation for the
  label index plus type-switched V codecs for the hash and B+ tree
  indexes. Snapshot-vs-rebuild ratio 1.04x on a 10^5-node graph;
  a CRC error or missing index file is tolerated by falling back
  to a rebuild from the in-memory graph (closes #172).

### Added — Sprint 19 (Capstone: Generic Durable LPG)

- `store/recovery.Open[N, W](dir, Options[N, W])` (and
  `OpenCtx[N, W]`) is the canonical typed-recovery entry point.
  `recovery.Options` mirrors `txn.Options`; `Result.SnapshotSchemaVersion`
  is populated from the snapshot manifest. The deprecated wrappers
  `OpenString`, `OpenWithCodec`, and `OpenWithOptions` keep their
  v1 behaviour. New example `examples/21_typed_recovery` exercises
  (string, int64), (int64, int64), (int64, float64), and
  (UUID, float64) combinations (closes #169).

### Fixed — Sprint 20 (Audit Correctness Closeout)

The 2026-05-20 deep audit identified five algorithmic correctness
defects in v1.1.0. Sprint 20 closes them all under
`Specify → Implement → Test → Document`.

- `search.KShortestPathsLoopless` / `KShortestPathsLooplessCtx`:
  honest name for what was previously called `EppsteinKShortest`.
  The shipped implementation is best-first enumeration over the
  loopless-path tree, NOT the heap-of-heaps sidetrack construction
  of Eppstein 1998; the rename is documented and the deprecated
  alias preserves backwards compatibility (closes #183).
- `flow.MinCostMaxFlow`: replaced the silent `if rc<0 { rc=0 }`
  clamp with a Bellman-Ford bootstrap that initialises the node
  potentials when the input network has negative-cost arcs, so the
  invariant `rc >= 0` holds entering every Dijkstra round. A
  negative cycle reachable from the source surfaces via the new
  sentinel `flow.ErrNegativeCycle`; the clamp is replaced by an
  internal invariant assert (panic indicates a programmer error in
  the algorithm, never input) (closes #184).
- `search.Diameter` / `DiameterCtx`: separated the BFS scratch
  slices so the iFUB refinement's level filter `distFromU[v]==k`
  is no longer corrupted by the inner-loop sweep. Rapid-based
  regression test `Diameter_ExactVsBruteVBFS` covers the
  invariant against a brute V-BFS oracle (closes #185).
- `search.HopcroftTarjanBCC`: the articulation-point root test now
  uses `p != start` (DFS-root identity) instead of `disc[p] != 0`
  (timer-derived, fragile in forests). Roots of components 2+ are
  no longer mis-classified as articulation points. Rapid property
  test compares the algorithm against a remove-vertex brute-force
  oracle on random multi-component forests (closes #186).
- `search.BellmanFord` and `centrality.WeightedBetweenness`: NaN
  / +/-Inf edge weights now fail fast with `search.ErrInvalidInput`
  rather than silently dropping every relaxation. Integer Weight
  types skip the validation pass entirely (zero-value type-switch
  short-circuits). BREAKING: `centrality.WeightedBetweenness`
  signature changed from `([]float64)` to `([]float64, error)` for
  consistency with `centrality.PageRank` and
  `centrality.PersonalisedPushPageRank`; it also rejects strictly
  negative weights via `search.ErrNegativeWeight` (closes #187).

### Fixed (documentation)

- `docs/algorithms.md`: replaced the stale "Eppstein deferred" and
  "Leiden simplified" caveats; added rows for the renamed
  `KShortestPathsLoopless` and for the input contracts on
  `BellmanFord` and `WeightedBetweenness`.
- `docs/metrics.md`: added the new `search.KShortestPathsLoopless`
  / `*Ctx` metric rows; the deprecated `search.EppsteinKShortest`
  / `*Ctx` rows are preserved so existing dashboards keep working.
- The Sprint 10 observability entry below and the matching paragraph
  in `release-notes/v1.1.0.md` previously stated that the
  `internal/metrics` Prometheus-style histogram hook was wired into
  every public blocking API. The hook ships and the
  Backend/IncCounter/ObserveLatency/Time API is stable, but the
  call-site integration across `search/`, `search/centrality/`,
  `search/community/`, `search/flow/`, `search/extern/`,
  `graph/io/{csv,graphml,dot,jsonl}`, and
  `store/{wal,snapshot,txn,checkpoint,recovery,bulk}` has not
  landed yet. The package doc of `internal/metrics` already records
  this as "a Sprint 11 or 12 follow-up"; the changelog and release
  notes are now consistent with the code. No source change; no
  retag of `v1.1.0`. Wire-up is tracked for a future release.

## [1.1.0] — 2026-05-19

Six sprints (8–13) of correctness, observability, hot-path
optimisation, algorithm completeness, and release hygiene. The
release closes the v1.0.0 audit and ships the first set of post-1.0
algorithmic and reliability work.

### Added — Sprint 9 (Concurrency Contract)

- `context.Context` is now accepted by every public blocking API in
  `search/`, `search/centrality/`, `search/community/`,
  `search/flow/`, `search/extern/`, `graph/io/`, `store/` so every
  long-running call honours cancellation and deadlines.
- `goleak.VerifyTestMain` adopted by every package that spawns
  goroutines so leaks fail the test pass.

### Added — Sprint 10 (Observability)

- `internal/metrics` Prometheus-style histogram **API hook** — a
  Backend interface, lock-free `atomic.Pointer[Backend]` swap, and
  the `IncCounter` / `ObserveLatency` / `Time` helpers, all backed
  by a zero-overhead no-op default. The hook is the interface
  contract for the CLAUDE.md "latency histograms on every public
  blocking API" mandate; **wiring it into individual call-sites
  across `search/`, `store/`, and `graph/io/` is deferred** so the
  wire-up can land incrementally without further API churn (see
  the `internal/metrics` package doc and the Unreleased note
  above).
- `pprof.SetGoroutineLabels` on every long-lived goroutine.
- `docs/benchmarks/` archive with multi-concurrency-level numbers.
- `govulncheck` job in CI (daily schedule).
- `internal/stress` concurrency stress suite — new CI job runs the
  suite under `-race` on every PR.
- `csrfile` crash-injection fuzz test for truncation recovery.

### Added — Sprint 11 (Hot-path Optimisation)

- `search.DijkstraInto`, `search.BellmanFordInto`, `search.AStarInto`
  — zero-allocation primitives that operate on caller-provided
  scratch slices (`BenchmarkDijkstra_PostWarmup` allocs/op == 0).
- Type-switch per-W `sync.Pool` dispatch (Dijkstra heap acquire
  drops from 5.4 ns/op to 1.08 ns/op).
- BFS index-head queue across Brandes / PPR-push / Topo /
  Dinic / Leiden (Brandes allocs/op −70.8 %).
- Leiden / LabelPropagation scratch+touched-list replaces the per-
  vertex `map[int]float64`/`map[int]int` (`BenchmarkLeiden` at
  V=1e5: 5.12x faster, allocs/op −99.96 %).
- BFS-DO inline bitmap frontier + pooled scratch + Beamer beta
  switch-back (6.08x vs vanilla top-down on power-law graphs).
- Iterative DFS for `flow.Dinic` augmentFlow and
  `search.HopcroftKarp` dfsAugment (no goroutine-stack growth at
  V=1e7).
- Floyd-Warshall column materialisation.
- Hierholzer trail pre-allocation.
- PageRank `outdeg` changed from `float64` to `uint32` (memory
  −50 % on that slice).
- SPFA + SLF deque for Bellman-Ford (4.17x on dense graphs).
- Yen candidate arena (Yen K100 allocs/op −96.65 %).
- `slices.Sort` in `extern/bfs.go`.
- `graph.Mapper.Walk` for shard-batched name lookup; IO writers
  use it to amortise `Resolve` shard-lock acquisitions.
- `strconv.FormatInt` in dot writer.
- `ds.UnionFindSlice` (22.2x faster than the generic map-backed
  variant on a bounded ID space).

### Added — Sprint 12 (Algorithm Completeness)

- `search.BidirectionalDijkstra` / `BidirectionalDijkstraOn`.
- `flow.EdmondsKarp` (max-flow reference / baseline).
- `search.KruskalMST` / `search.PrimMST`.
- `search.WCC` (weakly-connected components).
- `search.KCore` (Batagelj-Zaversnik 2003).
- `search.CountTriangles` (degree-ordered node-iterator).
- `search.TransitiveClosure` (bitset matrix oracle).
- `centrality.WeightedBetweenness` (Dijkstra-augmented Brandes).
- `centrality.BetweennessParallel` (4.9x on M4 at GOMAXPROCS=10).
- `flow.PushRelabelMaxFlow` (FIFO + gap heuristic).
- `search.Diameter` (2-sweep + iFUB refinement).
- `search.HierholzerUndirected`.
- `search.BiBFSOn` reverse-CSR variant; BiBFS now handles directed
  graphs by building the reverse internally.
- `search.EppsteinKShortest`.
- `flow.MinCostMaxFlow` (SSP + node potentials).

### Added — Sprint 13 (Release Hygiene)

- `bench/soak` 4-hour mixed-workload reliability harness with heap
  / FD / goroutine snapshots.
- benchstat regression gate on pull-requests.
- LDBC SF1/SF10 + DIMACS SF1/USA `Benchmark*` functions (the
  large-scale ones skip under `-short`).
- goreleaser pipeline + `.github/workflows/release.yml` + Makefile
  release targets. Documentation at `docs/release.md`.
- Cross-library comparison vs NetworkX 3.2.1 with measured numbers
  (BFS 178x, Dijkstra 25x, PageRank 28x on the same graph). See
  `docs/benchmarks/comparison.md`.
- Rapid-based property tests covering triangle inequality (Dijkstra),
  precedence (TopologicalSort), reflexivity (Tarjan SCC), and
  matching cardinality (Hopcroft-Karp).
- Fuzz tests for `store/csrfile`, `graph/io/graphml`, `graph/io/csv`
  parsers.
- t.Run subtest pattern adopted across representative table-shaped
  tests (the bulk migration can land incrementally; the pattern is
  in place and exercised).
- Concurrency-contract godoc clauses added to `search.APSP`,
  `search.Matching`, `search.TC`.

### Added — Sprint 8 (Correctness Hardening, retained from the v1.0.0 audit)

- LICENSE file at repo root: MIT (closes #92).
- 10 new example programs (`examples/11_social_network` through
  `examples/20_concurrent_reads`) demonstrating every major feature
  (commit `ffe335a`).
- `graph/csr.CSR.LiveMask`, `LiveNodes`, `LiveCount`, `IsSymmetric`
  helpers (closes #79 and the first half of #109).
- `search.ErrInvalidInput`, `centrality.ErrInvalidInput`,
  `extern.ErrInvalidInput` for NaN/Inf input rejection (closes #91).
- `search.ErrNotUndirected` returned by `BiBFS` on directed CSRs
  (closes #89).
- `search.ErrNegativeEdgeAPSP` returned by `DijkstraAPSP` on negative
  edges (closes #88).
- `search.DijkstraAPSP` (primary export; `JohnsonAPSP` is now a
  deprecated alias) (closes #88).
- `wal.Writer.Truncate` returning the freed byte count (closes #82).
- `checkpoint.Checkpointer.TriggerCtx` honouring context cancellation
  (closes #84).
- Property test `TestLeiden_ModularityNonDecrease` via
  `pgregory.net/rapid` (closes #80).

### Fixed

- `centrality.PageRank` and `extern.PageRank` now conserve total mass
  by redistributing dangling-node rank uniformly each iteration; the
  v1.0.0 implementations lost the sink's accumulated mass at every
  buffer swap (closes #77, #78).
- `community.Leiden` is now an actual Traag-Waltman implementation
  (local-moving + refinement + aggregation) rather than majority-vote
  label propagation. `Partition.NumCommunities` reflects the live
  community count, not the inflated MaxNodeID-based count (closes #80).
- `centrality.PersonalisedPushPageRank` handles dangling nodes per
  Andersen-Chung-Lang (teleport residue back to source); removed the
  residue-drain pass that double-counted absorbed mass (closes #87).
- `search.HopcroftTarjanBCC` correctly handles multigraph parallel
  edges; tracks the entry-edge index per frame instead of the parent
  NodeID, and the edge-stack pop condition now matches only the
  tree-edge ordering (closes #81).
- `search.Yen` and `search.FloydWarshall` no longer use the
  overflow-prone `v += v` Inf sentinel; reachability is tracked via
  an explicit `found[]` bitmap (closes #85, #86).
- `store/checkpoint.runCheckpoint` actually truncates the WAL on
  disk after writing a snapshot — the v1.0.0 implementation only
  recorded a counter and the WAL grew unbounded in steady state
  (closes #82).
- `store/checkpoint.Stop` is now idempotent (closes #83).
- The `maxID` over-iteration pattern in centrality/community/APSP is
  eliminated; algorithms iterate only live NodeIDs and ghost slots
  carry sentinel `-1` (closes #79).

### Changed (breaking)

- `search.Hungarian` signature: `(Assignment)` → `(Assignment, error)`
  (closes #91).
- `centrality.PageRank` signature: `(ranks, iters)` →
  `(ranks, iters, error)` (closes #91).
- `centrality.PersonalisedPushPageRank` signature: `(ranks)` →
  `(ranks, error)` (closes #91).
- `extern.PageRank` signature: `(ranks, iters)` →
  `(ranks, iters, error)` (closes #91).
- `community.Partition.Community[id]` returns `-1` for ghost NodeID
  slots (closes #79).
- `search.APSP` internal layout switched to a compact `live*live`
  matrix with a NodeID→index map; the public `At`/`N` API is
  preserved but `N` now returns the live count, not `MaxNodeID()`
  (closes #79).

### Deprecated

- `search.JohnsonAPSP`: deprecated alias for `DijkstraAPSP`; scheduled
  for removal in a future major release once Bellman-Ford reweighting
  lands (closes #88).

### Documentation

- README license section updated to point at the LICENSE file
  (closes #92).
- Examples 08, 09, 18, 20 print live-NodeID counts via
  `Mapper().Lookup` rather than the misleading `MaxNodeID`-sized
  slice length (closes #79).

## [1.0.0] — 2026-05-19

The first stable release of GoGraph. Seven sprints landed the
foundation, the property-graph model, durable persistence, the
out-of-core Tier 2 substrate, I/O interop, the analytical algorithm
suite, and the benchmark harnesses.

### Added — Sprint 1 (Foundation & In-Memory Core)

- `graph` — generic NodeID, Graph[N, W] contract, sharded Mapper.
- `graph/adjlist` — mutable copy-on-write adjacency-list backend.
- `graph/csr` — immutable Compressed Sparse Row snapshot.
- `search` — BFS (wavefront), DFS (iterative), Dijkstra (binary
  heap), Bellman-Ford, A\*, Bidirectional BFS, topological sort
  (Kahn), Tarjan SCC.
- `ds` — Union-Find with path compression.
- `examples/01_basic` and the README quickstart.
- CI pipeline (gofmt, vet, build, test, race, golangci-lint).

### Added — Sprint 2 (Property Graph + Indexes)

- `graph/lpg` — Labelled Property Graph with vertex and edge labels
  and a 24-byte tagged PropertyValue (string, int64, float64,
  bool, time.Time, []byte).
- `graph/lpg/schema` — declarative type schema with `Validate`.
- `graph/index/label` — Roaring-bitmap label index with intersect
  and union.
- `graph/index/hash` — sharded hash exact-match property index.
- `graph/index/btree` — order-preserving range property index with
  the sub-microsecond `RangeFirst`.
- `graph/index` — `Manager` fanning out `Change` events to
  subscribers.
- `graph/query` — fluent MATCH-style pattern engine.
- `examples/02_property_graph`.

### Added — Sprint 3 (Durable Persistence)

- `store/wal` — versioned, CRC32C-checksummed Write-Ahead Log
  reader / writer.
- `store/snapshot` — atomic snapshot directories with manifest and
  per-file CRC.
- `store/txn` — single-writer transactions (Begin/Commit/Rollback)
  with fsync-at-commit durability.
- `store/checkpoint` — background WAL → snapshot folder goroutine.
- `store/recovery` — snapshot + WAL replay on open.
- `docs/persistence.md`.

### Added — Sprint 4 (Out-of-Core Tier 2)

- `store/csrfile` — versioned, 64-byte-aligned mmap'd CSR file
  format with atomic writer, mmap reader, `madvise` hints, and
  the `Reinterpret` zero-copy helper.
- `store/csrfile.BuildFixture` — deterministic reproducible
  fixture generator.
- `graph/generation` — refcount-protected `Publisher` for atomic
  snapshot rotation across readers and writers.
- `search/extern` — semi-external BFS and PageRank over a Tier 2
  reader.
- `docs/tier2.md`, `docs/csrfile-v1.md`, `CONTRIBUTING.md` (unsafe
  policy).

### Added — Sprint 5 (I/O Interop)

- `graph/io/csv` — read and write edge-list CSV.
- `graph/io/graphml` — read and write GraphML XML.
- `graph/io/dot` — write Graphviz DOT.
- `graph/io/jsonl` — read and write JSON Lines.
- `store/bulk` — bulk ingestion bypassing the WAL.
- `docs/io.md`.

### Added — Sprint 6 (Advanced Algorithms)

- `search/bfs_do.go` — direction-optimising BFS (Beamer 2012).
- `search/yen.go` — Yen's k-shortest paths.
- `search/floyd_warshall.go` and `search/johnson.go` — APSP.
- `search/bcc.go` — Hopcroft-Tarjan BCC + bridges + articulation.
- `search/hierholzer.go` — Eulerian circuit / path.
- `search/hopcroft_karp.go` — bipartite matching.
- `search/hungarian.go` — weighted assignment.
- `search/flow/dinic.go` — max-flow.
- `search/flow/stoer_wagner.go` — global min-cut.
- `search/centrality/brandes.go` — exact betweenness.
- `search/centrality/pagerank.go` — in-memory power iteration.
- `search/centrality/ppr_push.go` — personalised PageRank (push).
- `search/community/leiden.go` — Leiden-style community detection.
- `search/community/label_propagation.go` — label propagation.
- `docs/algorithms.md`.

### Added — Sprint 7 (Benchmarks, Hardening, Release)

- `bench/ldbc` — LDBC SNB SF1 / SF10 harness.
- `bench/dimacs9` — DIMACS 9 SSSP harness.
- `bench/rmat` — RMAT power-law generator (Graph500 defaults).
- `docs/profiling.md`, `docs/optimisations.md`, `docs/semver.md`.
- `release-notes/v1.0.0.md`.

### Documented limits (v1.0.0)

- Johnson APSP restricts to non-negative weights; Bellman-Ford
  reweighting is deferred.
- Yen's k-shortest is O(k * (V + E) log V); Eppstein's is
  deferred.
- Leiden ships the local-moving + connected-component-split
  simplification; the refinement / aggregation phases of the
  full Traag-Waltman-van Eck paper are deferred.
- `adjlist.AddEdge` cost is dominated by the COW; the delta-log
  in-place atomic-append variant is deferred (tracked in
  `docs/optimisations.md`).
- `bench/ldbc.Run` non-synthetic mode (the LDBC Datagen
  integration) is deferred.
