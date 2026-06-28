# Changelog

All notable changes to GoGraph are documented in this file. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project follows [Semantic Versioning](https://semver.org/).

## [0.6.0] — 2026-06-28

The eighth published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release. Its headline is a substantially **wider analytics and Cypher
surface**: four new centrality measures (Closeness, Harmonic,
Eigenvector, Katz), node-label round-trip in the JSONL and GraphML
exporters, Cypher **map projection** (`n{.name, .*, k: expr}`), and
`shortestPath()`/`allShortestPaths()` with **per-instance relationship
typing**. The release also lands a deep reliability and openCypher
conformance hardening pass and a deterministic-simulation-testing (DST)
infrastructure expansion.

The bump is **MINOR** under [Semantic Versioning](https://semver.org/):
every change in this release is **additive** over `v0.5.0` (new exported
functions, new Cypher features, new options) or an **internal** fix that
restores a previously-documented contract. No breaking change to the
exported Go API ships in this release. As a `0.y.z` release the public
API remains unstable; pin the exact version you depend on.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios, 16 006 / 16 006 steps) and **100 %
ACID-compliant** across the in-memory engine and every persistence
backend. The Go toolchain remains **go1.26.4** (unchanged), and
`govulncheck ./...` stays clean.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.6.0
```

### Added

- **Four new centrality measures (`search/centrality`).** `Closeness`
  (Wasserman–Faust normalisation), `Harmonic` (Boldi–Vigna),
  `Eigenvector`, and `Katz`, each with a context-aware `…Ctx` variant.
  `Eigenvector` and `Katz` take an options struct and return the
  iteration count alongside the score vector. (#1800)
- **Node-label round-trip in the interchange exporters.** The JSONL and
  GraphML writers now carry node labels through export, so a graph
  exported and re-imported preserves its label set. (#1793)
- **Cypher map projection.** `RETURN n{.name, .*, k: expr, var}` is now
  supported — selected-property, all-properties (`.*`), literal-entry,
  and variable-selector forms, composable in a single projection.
  (#1775)
- **`shortestPath()` / `allShortestPaths()` with per-instance
  relationship typing.** Path functions honour relationship-type
  predicates and per-instance relationship types, including across
  parallel edges in a multigraph. (#1685, #1690, #1691, #1692)
- **Per-instance by-handle edge properties.** `SET` and `REMOVE` on a
  bound relationship now maintain per-instance edge properties addressed
  by handle, correct under parallel edges. (#1684, #1686, #1688, #1689)
- **`MERGE` of a distinct relationship type creates the parallel edge.**
  `MERGE` now creates a parallel edge when its type differs from an
  existing edge between the same endpoints. (#1683)
- **`CSR.Validate` boundary check.** A new `CSR.Validate` method and the
  `ErrMalformedCSR` sentinel detect a structurally malformed CSR
  snapshot at the boundary before it reaches a search hot path. (#1762)
- **Columnar edge-property storage tier.** Edge properties are stored in
  a de-boxed, per-`(key, kind)` columnar tier with a validity bitmap,
  cutting per-edge resident memory substantially on property-heavy
  graphs.
- **Deterministic-simulation-testing (DST) infrastructure.** A
  `SimDisk`-backed filesystem seam (`OpenFS`) now backs snapshot, CSR
  file, and checkpoint paths, and the crash-storm scenario exercises
  full snapshot + WAL crash-recovery end to end. (#1546, #1740, #1752)
- **`EngineOptions.DisableParallelBackfill` toggle.** Opts a deployment
  out of parallel index backfill when single-threaded backfill is
  preferred. (#1747)

### Changed

- **`EXPLAIN` labels a disconnected `MATCH` as `CartesianProduct`.** A
  query whose `MATCH` patterns share no variable is now reported as a
  `CartesianProduct` in the `EXPLAIN` plan, surfacing the accidental
  cross-product to the query author. (#1807)
- **String + number concatenation returns null.** This behaviour is now
  pinned and documented, with a `SyntaxError` raised where the
  openCypher specification requires it. (#1770, #1794)

### Fixed

- **`ORDER BY … LIMIT 0` yields an empty result**, not all rows. (#1801)
- **Integer and Float order as one Number tier** in comparison and
  `ORDER BY`, matching openCypher numeric ordering. (#1789)
- **Nested aggregation is rejected at compile time** rather than
  producing an undefined result. (#1804)
- **A non-aliased compound grouping key reads its precomputed column**
  instead of evaluating to null. (#1803)
- **`ORDER BY` passthrough variable dropped from result columns** so a
  sort key does not leak into the projection. (#1805)
- **Aggregation-in-`WHERE` error names `WHERE`**, not `ORDER BY`. (#1806)
- **Parser: compact subtraction disambiguation.** The lexer now
  correctly discriminates a binary minus from a signed literal in
  compact subtraction expressions, and the variable-length bound rewrite
  is scoped to relationship brackets. (#1788, #1796, #1797, #1798)
- **Relationship-uniqueness enforced across comma-separated `MATCH`
  patterns**, preventing a single relationship from binding twice across
  patterns. (#1777)
- **`shortestPath` / `allShortestPaths` find directed `src == dst`
  cycles; the undirected case is safe.** Context cancellation is now
  honoured during `allShortestPaths` enumeration. (#1779, #1780, #1782)
- **Shortest-path relaxers guard nil weights** on a weightless CSR
  rather than panicking. (#1776)
- **GraphML: per-`(name, kind)` keys** so heterogeneous node and edge
  property keys round-trip without collision. (#1791)
- **Nested-list property serialisation is bounded** to prevent writer
  out-of-memory on deeply nested values. (#1792)
- **Temporal time-zone offset preserved on export.** (#1769)
- **Tombstone-aware CSR build** drops ghost edges from the search
  snapshot. (#1790)
- **WAL torn-tail detection.** A corrupt length field that would mask
  durable frames as a torn tail is now detected, preventing silent data
  loss. (#1778)
- **Schema DDL durably persists across a WAL-truncating checkpoint.**
  Index and constraint definitions survive a checkpoint that truncates
  the WAL. (#1755, #1756)
- **`NOT NULL` existence constraints enforced at commit time**, covering
  omit-at-`CREATE` and set-label paths, not only `SET`-to-null. (#1754)
- **A rolled-back no-op edge `CREATE` no longer deletes a pre-existing
  edge** between the same endpoints. (#1751)
- **`SET r = map` / `SET r = node` have true `REPLACE` semantics.**
  (#1687)
- **Bolt protocol hardening.** Typed `Terminated` `FAILURE` on the reaper
  path, partial `DISCARD {n}` handling, `qid` validation, and query
  messages ignored on an authenticated `FAILED` connection. (#1781,
  #1783, #1784, #1787)
- **`ParallelScan` closer goroutine is joined in `Close`** and the
  `Run` plan is closed when root `Init` fails — no goroutine leak on the
  error path. (#1760, #1795)
- **Round-2 reliability-audit conformance fixes** across the Cypher and
  Bolt surfaces. (#1764–#1768, #1771–#1773)
- **`DateValue.String` is the inverse of `ParseDate` for expanded
  years.** (#1658)
- **Aggregate `DISTINCT` equivalence and `sum()` empty-input identity**
  match openCypher semantics.

### Performance

- Numerous hot-path optimisations landed across the search, Cypher
  execution, storage, and analytics layers, including a columnar
  edge-property tier, registry copy-on-write, scan arena reuse, an
  adaptive-parallelism governor, snapshot write-path reductions, parallel
  index backfill, and parallel variants of additional graph algorithms.
  These are tracked per change in
  [`docs/benchmarks/history/`](docs/benchmarks/history/) and summarised
  for this release in
  [`docs/benchmarks/v0.6.0.md`](docs/benchmarks/v0.6.0.md). Every
  optimisation preserves the documented API contracts and both
  compliance invariants.

### Documentation

- New centrality functions carry an explicit concurrency contract.
  (#1800 follow-up)
- A* integer f-score overflow precondition, the Leiden / Label
  Propagation unit-weight contract, GC tuning guidance for read-heavy
  workloads, and the full-stack checkpoint + WAL crash-recovery flow are
  documented. (#1758, #1763, #1706, #1741)
- Release-gate cleanup: `golangci-lint` and `staticcheck` cleared, and
  the gated-documentation freshness footers re-stamped after
  verification against the current code. (#1813, #1814, #1815)

### Examples

- All 26 examples were upgraded to a common standard: seeded generators,
  scale knobs, subject-appropriate evidence collection (CPU, RAM,
  storage), and machine-readable telemetry.

## [0.5.0] — 2026-06-17

The seventh published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release. Its headline is **live `db.*` schema introspection**: the
built-in `db.labels()`, `db.relationshipTypes()`, and
`db.propertyKeys()` procedures now report only the schema tokens
**currently in use** — labels, relationship types, and property keys
attached to at least one live (non-tombstoned) node or relationship —
rather than every token ever indexed or seen. A token disappears from
the result as soon as its last bearing element is deleted.

The bump is **MINOR** under [Semantic Versioning](https://semver.org/):
the new behaviour is additive on top of `v0.4.0`, and three new
exported methods are introduced on `lpg.Graph` — `NodeLabelsInUse`,
`RelationshipTypesInUse`, and `PropertyKeysInUse`. The low-level helper
`procs.RegisterBuiltins` changed its third parameter from a single
closure to a `procs.BuiltinSources` struct; this is permitted in a
`0.y.z` line and affects only direct callers of that internal-style
registration helper, not users of the public Cypher engine. As a
`0.y.z` release the public API remains unstable; pin the exact version
you depend on.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios, 16 006 / 16 006 steps) and **100 %
ACID-compliant**. The `db.*` introspection procedures are not covered by
the openCypher TCK, so the in-use semantics are free to diverge from
Neo4j (see Notes). The Go toolchain remains **go1.26.4** (unchanged),
and `govulncheck ./...` stays clean.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.5.0
```

### Added

- **`db.labels()` — labels in use.** `CALL db.labels()` returns every
  distinct node label currently attached to at least one live node,
  whether or not an index exists for that label. The order is
  unspecified.
- **`db.relationshipTypes()` — types in use.** `CALL
  db.relationshipTypes()` returns every distinct relationship type
  currently attached to at least one live relationship. The order is
  unspecified.
- **`db.propertyKeys()` — keys in use.** `CALL db.propertyKeys()`
  returns every distinct property key currently in use across nodes
  **and** relationships. The order is unspecified.
- **`lpg.Graph` introspection methods.** New exported methods
  `NodeLabelsInUse() []string`, `RelationshipTypesInUse() []string`, and
  `PropertyKeysInUse() []string` enumerate the schema tokens in use,
  filtering out tombstoned elements. They back the `db.*` procedures and
  are available directly on the graph type.

### Changed

- **`db.labels()` no longer reports index-only labels.** Before this
  release the procedure could report a label merely because an index
  existed for it; it now reports a label only while at least one live
  node bears it.
- **`procs.RegisterBuiltins` signature.** Its third parameter is now a
  `procs.BuiltinSources` struct bundling the `ListConstraints`,
  `Labels`, `RelationshipTypes`, and `PropertyKeys` data-source
  closures, replacing the previous single constraint-rows closure. Every
  field is optional; a nil closure makes its procedure return an empty
  result set. This decouples the `procs` package from the concrete graph
  type.

### Documentation

- **`docs/cypher.md`** documents the in-use semantics of `db.labels()`,
  `db.relationshipTypes()`, and `db.propertyKeys()`, the deliberate
  divergence from Neo4j for `db.propertyKeys()`, and the
  not-yet-implemented status of `db.schema.visualization()` (registered
  but currently returns an empty result set).

### Compliance

- **openCypher TCK.** Execution suite remains fully green at
  **3 897 / 3 897 scenarios** and **16 006 / 16 006 steps**; the
  regression gate (`tckExecutionBaseline = 3897`) is unchanged. The
  `db.*` introspection procedures are not TCK-covered.
- **ACID.** Atomicity, Consistency, Isolation, and Durability hold
  across the in-memory engine and every persistence backend; the new
  introspection is read-only and does not touch the write path.

### Notes

- **Divergence from Neo4j (`db.propertyKeys()`).** Neo4j lists
  property-key tokens from a token store that is never
  garbage-collected, so it keeps reporting a key after its last bearer
  is deleted. GoGraph reports only keys currently in use. This is
  observable but not an openCypher-conformance issue, because the `db.*`
  procedures are outside the TCK.
- **Pre-1.0 stability.** This is a `0.y.z` release. The public Go API
  may change without a major-version bump until `1.0.0`; pin the exact
  version you depend on.
- **Module path.** The Go module path is
  `github.com/FlavioCFOliveira/GoGraph` with no `/vN` suffix, which is
  Semantic-Import-Versioning-correct for a `0.x` line.

## [0.4.0] — 2026-06-17

The sixth published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release. Its headline is a **new public API** —
`(*cypher.Engine).BeginReadTx` — that opens a **lock-free, read-only
explicit transaction**, lifting concurrent read throughput by roughly
**2×** by never acquiring the single-writer serialisation, the
visibility barrier, or a WAL transaction. The release also lands
**Cypher read-path performance work** (per-query forward-CSR caching in
relationship reconstruction; non-escaping `RowContext` pooling) and the
**first publication of the deterministic-simulation-testing (DST)
harness** (`internal/sim` + `cmd/sim`), TigerBeetle-VOPR-modelled
test/CI infrastructure that found and fixed two latent defects.

The bump is **MINOR** under [Semantic Versioning](https://semver.org/):
the new `BeginReadTx` API is **purely additive** over `v0.3.2` — no
exported identifier was removed or renamed, and there is **no breaking
change**. As a `0.y.z` release the public API remains unstable; pin the
exact version you depend on.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios, 16 006 / 16 006 steps) and **100 %
ACID-compliant**. The Go toolchain remains **go1.26.4** (unchanged), and
`govulncheck ./...` stays clean.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.4.0
```

### Added

- **`(*cypher.Engine).BeginReadTx(ctx)` — lock-free read-only explicit
  transactions.** A read-only transaction handle that acquires **neither**
  the engine writer serialisation, **nor** the visibility barrier, **nor**
  a WAL transaction — so it never serialises behind, or blocks, a
  concurrent writer. Every statement run through the handle is rejected
  **before execution** with the new exported sentinel
  `cypher.ErrWriteInReadOnlyTx` if it contains a writing clause or is DDL;
  this guard is what keeps the lock-free path safe. Permitted reads route
  through the engine's concurrent read path, each taking its own
  per-statement snapshot — **read-committed** isolation across the
  statements of the transaction, matching Neo4j's default. `Commit` and
  `Rollback` on a read-only handle are teardown-only no-ops. Over the Bolt
  protocol, `BEGIN` with `mode="r"` now routes through `BeginReadTx`, and
  `ErrWriteInReadOnlyTx` maps to `Neo.ClientError.Request.Invalid` with
  the message forwarded to the client (client-fault). Motivated by
  block-profiling that attributed **83 %** of Bolt-load blocking time to
  the single-writer semaphore held by `BeginTx` for the whole explicit
  transaction — even for read-only ones. (#1573)
- **Deterministic-simulation-testing (DST) harness** (`internal/sim`,
  `cmd/sim`) — first publication of a TigerBeetle-VOPR-modelled
  deterministic simulator: seed-driven virtual clock, in-memory faulting
  disk with real WAL recovery, graph oracle and invariant checkers,
  crash-and-recovery cycles, a real in-process Bolt wire path, a named
  scenario catalogue with deterministic trace recording, scripted replay
  and `ddmin` shrinking, a reproducible bounded-worker swarm, and upgrade,
  cross-release, and differential harnesses. The `cmd/sim` CLI exposes
  `-swarm`, `-scenario`, `-replay`, and `-check-every`. This is **test and
  CI infrastructure**, not a public module API. (#1528–#1576)

### Performance

- **Cypher relationship reconstruction — cache the forward CSR per
  query.** `edgeHandleAtFwdPos` / `edgeInstanceIdxFor` previously rebuilt
  the entire forward CSR (`O(V+E)`) on **every** result row to read one
  slot, making relationship reconstruction `O(R·(V+E))` per query. The
  forward CSR snapshot is now built once per query and reused, mirroring
  the existing edge-ID-resolver pattern. On `MATCH (a)-[r]->(b) RETURN
  count(r)` over a multigraph (benchstat, count = 6): **dense graph
  −88.2 % sec/op, −95.9 % B/op, −16.7 % allocs/op**; small graph −73.6 %
  sec/op, −83.3 % B/op. Certified by `graph-theory-expert`,
  `storage-engine-auditor`, and `go-developer`. (#1574)
- **Cypher scalar projection — pool the per-row `RowContext`.** The
  per-row context map is now recycled through a `sync.Pool` at the two
  **non-escaping** evaluation sites (the `Filter` predicate closure and
  the scalar-projection evaluator), gated on the existing analysis flag
  that bails on any expression kind that could retain the context past the
  call. On `MATCH (n:N) WHERE n.i>=0 AND n.j>=n.i RETURN n.i,n.j`
  (benchstat, count = 6): **−42.1 % B/op, −26.6 % allocs/op, −11.5 %
  sec/op**. Only the outer map container is recycled; values that escape
  into a result row are independently owned and never reused. Certified by
  `go-developer`; `rust-perf-engineer` consulted. (#1575)
- **Brandes betweenness — zero-allocation predecessor arena.** The
  per-source predecessor lists are flattened into a reusable arena,
  cutting allocations on the centrality hot path (bit-identical results).
  (#1515)
- **DST simulator — configurable invariant-check cadence.** The simulator
  invariant checker now runs on a configurable cadence with a guaranteed
  terminal check, reducing per-tick overhead in long runs without
  weakening end-state verification. (#1576)

### Fixed

- **Bolt illegal-transition error names the originating state.** An
  illegal protocol transition into `FAILURE` now reports the state the
  connection was actually in, rather than always reporting `FAILED` —
  found via the DST Bolt wire harness. (commit `82d98af`)
- **`DROP CONSTRAINT` by name is now atomic and never a silent no-op.**
  Dropping a constraint by name now drops the constraint **and** its
  backing index atomically; a previously fail-silent path that left the
  constraint in place is fixed. Schema DDL is not covered by the
  openCypher TCK, so the execution-conformance count is unaffected. Found
  via the DST schema-chaos scenario. (#1556)

## [0.3.2] — 2026-06-15

The fifth published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **PATCH**
release with a single, focused change: it **fixes a data-compatibility
recovery panic** that could crash the process when opening an existing
on-disk store. It is **API-additive** over `v0.3.1` — no exported
identifier was added, removed, or renamed, there is **no breaking
change**, and there is **no new user-facing public API**. It is a
**strongly recommended upgrade** for anyone running `v0.3.0` or `v0.3.1`,
in particular anyone whose store was first written by `v0.2.0` or
earlier.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios) and **100 % ACID-compliant**. The Go toolchain
remains **go1.26.4** (unchanged), and `govulncheck ./...` stays clean.
This is a correctness-only patch that touches no hot path, so the
benchmark figures are **inherited unchanged from `v0.3.1`**
(see [docs/benchmarks/v0.3.2.md](docs/benchmarks/v0.3.2.md)).

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.3.2
```

### Fixed

- **Recovery panic on the edge fast-path handle column (data
  compatibility).** `graph/adjlist.upsertEdgeLocked` could panic with
  `makeslice: cap out of range` on the spare-capacity fast path when
  growing the per-edge handle column for a node that had accrued a
  **handle-less prefix**. The fast path sized the new handle column from
  `len(current.handles)` rather than from the neighbour count `oldLen`;
  for a node whose handle column was still nil/short (for example length
  `0`) while its neighbour backing array had grown with spare capacity, a
  later handle-bearing append computed a capacity (`growCap(0) = 4`) below
  the required length (`6`), so `make([]uint64, newLen, <newLen)`
  panicked. The fix sizes the column from `oldLen`
  (`make([]uint64, newLen, growCap(oldLen))`), matching the slow path in
  the same function; `growCap(oldLen)` is always `>= newLen`, and the
  copy-plus-zero-fill keeps the handle column length-aligned with the
  neighbour list (leading handle-less slots stay the `0` "no handle"
  sentinel).

  This was a hard **data-compatibility break** introduced by the
  amortised-O(1) `AddEdge` hub rewrite (`877e455`) and shipped in
  `v0.3.0` and `v0.3.1`; it is **absent in `v0.2.0`**.
  `store/snapshot.ApplyCSRToGraph` replays each node's edges as a mix of
  handle-less (`AddEdge`) and handle-bearing (`AddEdgeHIfAbsent`)
  inserts, so **any snapshot containing such a node crashed the process
  on open** — under both read and write recovery. Upgrading restores the
  ability to open these stores. Two regression tests guard the fix
  (`graph/adjlist/handle_prefix_regression_test.go` and
  `store/snapshot/apply_handle_prefix_test.go`), each verified red without
  the fix and green with it.

## [0.3.1] — 2026-06-15

The fourth published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **PATCH**
release: it is **API-additive** over `v0.3.0`, with **no breaking change**
and **no new user-facing public API**. Its headline is a deep
**performance cycle** (tasks #1497–#1525) paired with two further
**security audits** (SEC-2026-06-14b and SEC-2026-06-14c, tasks
#1480–#1496). Every documented API contract is preserved unchanged, and
the performance work is correctness-preserving: the new write and
analytics paths produce **byte-identical / bit-identical** results and
regress **nothing** on the curated benchmark set. No exported identifier
was added, removed, or renamed.

Both compliance invariants continue to hold without regression: the
module is **100 % openCypher TCK-compliant at the execution level**
(3 897 / 3 897 scenarios) and **100 % ACID-compliant**. The group-commit
write path was certified by the storage-engine auditor as preserving
Atomicity, Consistency, Isolation, and Durability. The Go toolchain
remains **go1.26.4** (unchanged), and `govulncheck ./...` stays clean.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.3.1
```

### Security

- **Second security audit (SEC-2026-06-14b).** A follow-on, additive
  six-domain audit found and fixed **9** issues (tasks #1480–#1489); the
  full report is in
  [docs/security-audit-2026-06-14b.md](docs/security-audit-2026-06-14b.md).
  Its dominant theme was that the morning audit's "bound the eager
  allocation before the integrity gate" discipline had **not been
  generalised** to sibling decoders. All reachable from untrusted input
  (an on-disk artefact, a Cypher query, or Bolt bytes):
  - **Untrusted-artefact allocation bounds** — the **btree index**
    decoder (#1480) and the **label index** decoder (#1481) now clamp
    their eager allocation to `min(count, 1<<20)` and grow on demand,
    instead of pre-sizing on an unbounded untrusted count (a 20-byte
    CRC-valid payload could drive a 16 TiB reservation). The snapshot
    component readers (`readVerified{Labels,Properties,Mapper,EdgeHandles}`)
    now thread `FileEntry.Size` through an `io.LimitReader`, so the
    `append` loop can no longer grow past the real on-disk size — closing
    a back-door re-open of the earlier recovery-OOM hole (#1486). The
    property-value decoder now rejects a nested `PropList` element,
    enforcing the encoder's no-nesting invariant and closing an
    unbounded-recursion stack-exhaustion path (#1488).
  - **Cypher string-byte budget** — string concatenation (`+`) is now
    charged against a per-evaluation **byte** budget
    (`DefaultMaxStringEvalBytes`) before allocation, so a tiny doubling
    query such as `reduce(s='x', i IN range(1,33) | s+s)` can no longer
    grow an 8 GiB string from ~100 bytes of query text (#1482).
  - **Bolt `tx_timeout` overflow** — a client `tx_timeout` / `timeout`
    is now converted overflow-safely; a non-positive or overflowing value
    falls back to the server default unconditionally, so the wall-clock
    transaction reaper is always armed and the single global writer lock
    can no longer be pinned indefinitely (#1484).
  - **Cartesian-product visibility and cancellation** — a disconnected
    multi-pattern `MATCH` now surfaces a plan-time Cartesian-product
    **notification** (via the new `Result.Notifications()` accessor and
    the Bolt PULL `SUCCESS` metadata, faithful to Neo4j's
    `CartesianProductWarning`) and a locked-in deadline-cancellation
    guarantee (#1483).
  - **Bolt NOOP keep-alive** — a bare `00 00` NOOP keep-alive chunk is
    now silently discarded instead of being answered with a spurious
    `FAILURE` that evicted the very idle connection it was meant to
    preserve (#1485).
  - **DOT reserved-keyword quoting** — the DOT exporter now quotes node
    ids that collide with Graphviz reserved keywords (`node`, `edge`,
    `graph`, `digraph`, `subgraph`, `strict`), preventing silent
    export-integrity corruption (#1489).
- **Third security audit (SEC-2026-06-14c).** A four-phase red-team audit
  found and fixed **7** issues (tasks #1490–#1496), with **no Critical
  finding and no prior-fix regression**; the full report is in
  [docs/security-audit-2026-06-14c.md](docs/security-audit-2026-06-14c.md):
  - **`substring()` integer overflow** — the end bound is computed
    overflow-safely, so a huge `length` argument returns the conforming
    truncated tail instead of panicking on a negative slice index (#1492).
  - **`percentileCont()` NaN guard** — a non-finite percentile parameter
    is now rejected by `validPercentileParam` (a NaN previously bypassed
    the `[0,1]` check and could panic via a platform-dependent
    `int(NaN)` index); the continuous aggregator also clamps its index
    defensively (#1493).
  - **`replace()` amplification bound** — `replace(s, '', r)` now computes
    its worst-case output size overflow-safely and returns a typed
    `NumberOutOfRange` budget error before allocating, closing a
    quadratic output-amplification OOM (#1494).
  - **txn PropList decoder clamp** — `decodeTxnListProp` now clamps its
    capacity hint to `min(count, remaining/5)`, bringing the third
    parallel PropList decoder into lockstep with its two clamped siblings
    (#1490).
  - **Bolt reader panic boundary** — the per-connection reader goroutine
    now carries a `defer`/`recover` boundary mirroring the connection
    handler, so a future panic-on-input bug crashes one connection rather
    than the whole process (#1491).
  - **Supply-chain integrity** — a stale `.goreleaser.yaml` Dependabot
    comment was corrected with a regression gate (#1495), and
    `gen_tck_tzdata.sh` now pins the SHA-256 of the IANA tzdata tarball
    and aborts on mismatch (#1496).

### Performance

The 2026-06-14 performance cycle (rmp sprints S-PA1..S-PA6, tasks
#1497–#1525) delivered measured, benchstat-gated wins across the write,
analytics, query, and read paths. Every change is gated against the
`f6f8c7a` baseline (ledger row 0006) and the curated guard band
(Dijkstra / BFS / Brandes) stayed flat throughout. The raw before/after
numbers live in [docs/benchmarks/history/](docs/benchmarks/history/) and
the per-release report in [docs/benchmarks/v0.3.1.md](docs/benchmarks/v0.3.1.md).

- **Group commit / WAL fsync coalescing** (#1507) — the per-commit
  `fsync`, previously the dominant write-throughput ceiling, is replaced
  by a PostgreSQL-`XLogFlush`-style leader/follower coalesced fsync
  (`wal.Writer.SyncGroup`): one leader flushes and fsyncs the whole
  buffered suffix once while followers keep appending, and every follower
  whose durability watermark is covered returns success with no I/O of
  its own. Measured on `BenchmarkCommitConcurrent`: **−74.15 % at 8
  goroutines, −96.72 % at 64, and −99.16 % at 256 (≈ 118× concurrent
  write throughput)**, with **zero single-thread regression** (1
  goroutine flat at 3.683 ms, p = 0.959). The storage-engine auditor
  certified all four ACID properties preserved (durability acked only
  after the covering fsync; in-memory apply only in WAL sequence order
  after durability).
- **Parallel pull-formulation PageRank over reverse-CSR** (#1513) —
  PageRank's per-iteration SpMV, previously a single-goroutine push
  scatter, now runs a parallel pull formulation
  (`next[v] = baseShare + d·Σ cur[u]/outdeg[u]`) over a structure-only
  reverse-CSR across a persistent worker pool, partitioned by
  approximately equal in-edge count. Results are **bit-for-bit identical
  to the serial path regardless of `GOMAXPROCS` or scheduling** (proven
  cross-process). Measured **1.68–1.77× on large graphs** at 30
  iterations (up to **2.40×** for the SpMV kernel with the transpose
  amortised). Gated behind `live >= 2048 && GOMAXPROCS > 1`; smaller
  graphs and single-core runs take the unchanged serial path with **zero
  regression**.
- **Range-predicate B+tree index seek** (#1505) — a range predicate
  (`n.p > x`, `>=`, `<`, `<=`, or a two-sided AND) on a property backed
  by a bound string btree index now builds a `NodeByIndexRangeScan` in
  place of the label scan, with the original `Selection` filter retained
  on top. Measured **≈ 114× (−99.11 % time)** on the targeted
  0.5 %-selective shape, with **zero regression** on the curated set (the
  seek only fires under the selectivity / cardinality guards). This
  release also makes a Cypher `CREATE INDEX … {indexType:'btree'}` a
  bound, backfilled, self-maintaining index (it was previously registered
  empty with a no-op `Apply`).
- **Hash join for disconnected equi-join patterns** (#1506) — a
  disconnected multi-pattern `MATCH` joined by an equality predicate now
  builds an `exec.HashJoin` (O(|A|+|B|)) in place of the nested-loop
  Cartesian product (O(|A|·|B|)), under a structural trigger, a size
  floor, and an order-safety guard. The differential test proves an
  identical result multiset join-on vs join-off across NULL / NaN /
  cross-type keys; measured **≈ 93× faster, ≈ 95× less memory** on the
  targeted shape.
- **Real B+ tree replacing the sorted-array index** (#1514) — the
  range property index is now a real B+ tree.
- **Cypher read-path allocation cuts** (#1499–#1503) — column-oriented
  (SoA) materialised result rows (#1499, IC1 **−32.4 % time / −60.9 %
  B/op**), deferred node materialisation for scalar-only projections
  (#1500), a dropped redundant per-row map in the `WITH`/`RETURN`
  projection (#1501), shared node property/label views across the
  lpg→expr→wire seam (#1502), and a lock-free copy-on-write metadata name
  registry on the read path (#1503, **−81.57 % time at 8-way contention**
  on the isolated metadata-read benchmark). All curated entries are
  byte-identical or lower on allocations and B/op.
- **Storage write-path hardening of the durability stack** —
  a pre-sized partitioned-parallel bulk loader (#1512); a non-blocking
  LSN-watermarked checkpoint with WAL prefix truncation (#1508);
  elimination of the per-WAL-frame double allocation and copy (#1509);
  and `fdatasync` for the per-commit WAL sync on Linux (#1510). A CRC32C
  strategy microbenchmark confirmed the incremental `Update` path already
  uses the hardware path (#1511).

### Validation

- The full validation battery was run **green** at the release commit on
  **Go 1.26.4**: `go build ./...`, `go test -race ./...` (0 data races),
  `TestTCKExecution` (**3 897 / 3 897** scenarios, error-fidelity gate
  121), the `internal/crashinject` crash-injection battery,
  `golangci-lint`, `staticcheck`, and `govulncheck` (no vulnerabilities).
- A reproducible security test battery accompanies both audits, with
  every demonstration flipped to a strict regression assertion that
  passes on the fixed code and fails on regression.

## [0.3.0] — 2026-06-14

The third published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release: it is additive over `v0.2.0` and its headline work is a deep
**reliability and robustness hardening pass** drawn from four
successive code audits (three reliability and one security), alongside
four additive features. No exported
identifier was removed or renamed. Both compliance invariants continue
to hold without regression: the module is **100 % openCypher
TCK-compliant at the execution level** (3 897 / 3 897 scenarios) and
**100 % ACID-compliant**.

The hardening spans the Cypher engine (correctness and robustness:
`reduce()`, openCypher equivalence semantics for `DISTINCT` and
grouping, parser-panic and arithmetic-overflow guards,
`ParameterMissing`, type-error fidelity, and `SET`-clause spec
fidelity), the engine, Bolt protocol, and search layers (Bolt
autocommit read-path lock removal, `HopcroftKarp` input validation,
PackStream temporal wire encoding, and client-fault classification),
the import/export and observability surface (CSV/GraphML token-OOM
ceilings, fail-stop on XML-illegal characters, Prometheus name
sanitisation, portable non-finite-number encoding, CSV byte-order-mark
stripping, and a JSON Lines line cap), the durability path (a
recovery-promotion parent-directory `fsync` and a checkpoint
constraint-survival fail-safe), and the test-battery and release-gate
infrastructure.

Under Semantic Versioning, a `0.y.z` version signals that the public Go
API is **not yet stable** and may change while the module matures toward
`1.0.0`. Pin the exact version you depend on. This release introduces no
breaking API change. It tightens one consumer-visible default (the
CSV/GraphML import byte ceiling) and corrects one consumer-visible
behaviour — the Cypher `=~` regex operator, which was silently evaluated
as plain string equality and now performs an anchored regular-expression
match per the openCypher specification. Consumers should read the
**Upgrade notes** in [release-notes/v0.3.0.md](release-notes/v0.3.0.md).

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.3.0
```

### Added

- **`reduce()` list-folding expression** — the `reduce(acc = init, x IN
  list | expr)` accumulator expression is now wired through the Cypher
  grammar, the AST (`*ast.ReduceExpr`), semantic analysis, and the
  evaluator, so list folding is reachable from the public API. A null
  source list yields null, per the openCypher specification (#1426).
- **PackStream temporal wire encoding over Bolt** — the six Cypher
  temporal value kinds (`Date`, `LocalTime`, `Time`, `LocalDateTime`,
  `DateTime`, `Duration`) are now encoded as the canonical PackStream
  temporal Structs the neo4j-go-driver hydrator expects, instead of
  falling through to plain strings. `DateTime` encoding is Bolt-version-
  and time-zone-aware (the UTC convention on Bolt v5.0+, the legacy
  wall-clock convention on v4.4), so a real driver receives
  `neo4j.Date` / `neo4j.Duration` / `time.Time` values that support
  `.AsTime()` and temporal arithmetic (#1434).
- **Public `metrics` facade package** — a new top-level `metrics`
  package re-exports the previously internal metrics types via type
  aliases (`metrics.Backend`, `metrics.Registry`,
  `metrics.SetBackend`, `metrics.NewPrometheusRegistry`), so the
  observability wire-up documented in `docs/metrics.md` is now
  compilable from outside the module. A single implementation is
  retained behind the aliases.
- **Filesystem fsync-failure fault modes for testing** — the internal
  `testfs` fault library gains `Faults.FailSyncAfter` and
  `Faults.ReturnEIOOnSync`, both surfacing a new `ErrSyncFailed`
  sentinel and discarding the unsynced suffix, so durability tests can
  assert that an unacknowledged commit leaves no trace without a full
  crash-injection harness.
- **Additive TCK error-type fidelity gate** — a new conformance gate
  asserts that the engine raises the correct openCypher error *type*
  (not only that the scenario fails), built on the existing semantic
  error vocabulary. The execution result count is unchanged at
  3 897 / 3 897 (#1443).
- **Opt-in CSV formula-injection sanitisation** — the CSV writer gains
  `Options.SanitizeFormulae` (default off). When enabled, cells whose
  first character is `=`, `+`, `-`, `@`, tab, or carriage return are
  prefixed with a quote so a spreadsheet opening the export treats them
  as text (OWASP CSV/formula injection, CWE-1236). The default preserves
  the lossless round-trip (#1471).

### Changed

- **CSV and GraphML single-token import RAM is bounded.** The default
  import byte ceiling for the CSV and GraphML decoders is lowered to
  **128 MiB** (from 1 GiB) to cap the peak memory a single oversized
  token can pin while a record is assembled. The ceiling remains
  configurable, and `MaxBytes <= 0` still means unlimited (#1436).
- **`reduce()` is now a first-class expression** rather than an
  unreachable evaluator: queries that use `reduce(...)` now parse and
  execute instead of being rejected at parse time (#1426).
- **The `cypher/ir/rewrite` package documents its experimental status**
  and is guarded by an import-graph test, clarifying that it is not part
  of the stable public surface.
- **The Cypher `=~` regex-match operator now matches correctly.** It was
  silently parsed as `=` (string equality), so `'abc' =~ '[a-z]+'`
  returned false; it now performs an anchored full-match regular
  expression (`\A(?:…)\z`, openCypher / Java `String.matches` semantics).
  This corrects a latent fail-open hazard for any query that used `=~` in
  an authorisation or allow/deny predicate. See the Upgrade notes (#1479).
- **Dependencies bumped** (Dependabot): `RoaringBitmap/roaring/v2`
  2.18.0 → 2.18.2, `cucumber/godog` 0.14.1 → 0.15.1, `golang.org/x/sys`
  0.44.0 → 0.46.0, `spf13/pflag` 1.0.5 → 1.0.7. `govulncheck` stays clean
  and the TCK execution count is unchanged at 3 897 / 3 897.

### Fixed

- **Cypher correctness and robustness.**
  - `DISTINCT` and grouping now compare by openCypher **equivalence**
    rather than equality, so `NaN ≡ NaN` and nested nulls are grouped
    correctly (CIP2016-06-14) (#1420).
  - The parser no longer crashes the process on malformed input:
    `p.Script()` is wrapped in `recover()` for malformed `WITH`-clause
    and pipe-in-argument queries (#1422), and the pre-parse guard is
    extended to `CASE` nesting and binary-operator chains.
  - Deep operator chains can no longer overflow the stack during
    semantic analysis: `checkExpr` carries a recursion depth budget
    (#1424).
  - A query parameter that is referenced but not supplied now raises
    `ParameterMissing` instead of evaluating to a silent null (#1431).
  - Numeric-literal property access such as `(5).foo` raises
    `InvalidArgumentType` (#1430).
  - Integer and floating-point overflow are detected and reported as
    typed errors throughout: `sum()` accumulation (#1427), `MinInt64 /
    -1` integer division (#1419), `toInteger()` boundary rounding at
    `2^63` (#1428), pure integer literals over 19 digits, and
    exponent-form float literals that overflow `float64` (#1421).
  - The `Parse` and `ParseStrict` entry points now share one normaliser
    pipeline, eliminating a divergence on valid float literals (#1423).
  - `MaxResultRows` is applied to write-only queries, returning
    `ErrResultRowsExceeded` and rolling the write back atomically
    (#1425).
  - A leading `WITH ... WHERE` is seeded with a single-row argument so
    it no longer crashes.
- **SET-clause spec fidelity** (#1455, #1456, #1457, #1458) — four
  silent-divergence defects now fail loud and correct: setting or
  removing a **label on a relationship** is rejected with a `TypeError`
  (closing a latent hazard that reinterpreted an edge-position counter
  as a node id and could mislabel the wrong node); `SET n = null` clears
  all properties, `SET n += null` is a no-op, and a non-null non-map RHS
  (`SET n = 5`) is a compile-time `TypeError`; a nested map property
  raises `InvalidPropertyType`; and a non-map parameter to `SET n = $p`
  raises a `TypeError`.
- **Cypher DELETE no longer resurrects nodes.** The Cypher mutator
  adapter's `RemoveNode` now emits an `OpRemoveNode` WAL frame, so a
  node deleted through Cypher stays deleted across a store reopen
  instead of returning as a ghost on WAL replay (#1411).
- **Transaction isolation and atomicity.**
  - An explicit transaction holds the visibility lock for its whole
    lifetime, so concurrent readers block during an open transaction —
    closing a read-uncommitted isolation gap (#1412).
  - `ExplicitTx.Commit` is rejected after a failed `Exec`, preventing a
    partial transaction from becoming durable (#1413), and surfaces
    `ErrUndoFailed` when undo replay fails on a WAL `fsync` error.
  - DDL index and constraint registration and backfill now run inside
    the visibility barrier (#1417).
- **Engine, protocol, and search robustness.**
  - Bolt **autocommit reads use `RunAny`, not `RunInTxAny`**, so a
    read-only autocommit statement no longer takes the single-writer
    lock (#1432).
  - `HopcroftKarp` validates `nLeft` and returns `ErrInvalidInput`
    instead of panicking on malformed bipartite input (#1433).
  - Bolt and Cypher now **classify client-fault conditions** as client
    errors rather than server errors (#1435).
- **Import / export (`graph/io`).**
  - The GraphML writer **fails stop on XML-illegal characters** in
    string properties instead of silently emitting `U+FFFD`, on both
    write paths, and validates node ids on the plain `Write` path too
    (#1437).
  - The JSON Lines reader returns a typed `ErrLineTooLong` for an
    oversized single line (#1442).
  - The CSV reader strips a leading UTF-8 byte-order mark so spreadsheet
    exports parse correctly (#1441).
  - GraphML emits portable `xs:double` `NaN` / `INF` / `-INF`, and the
    non-finite contract for JSON Lines is documented (#1440).
  - The DOT writer emits bare node statements so isolated vertices are
    no longer dropped (#1439).
- **Observability.** The Prometheus exposition writer **sanitises
  metric names** to the valid grammar, closing an exposition-injection
  vector through the public metrics facade (#1438), and `WriteText`
  returns its accumulated write error.
- **Durability.**
  - Recovery **`fsync`s the parent directory** after promoting a
    snapshot backup, so a crash between the promotion rename and the
    directory flush cannot lose the promoted snapshot (A1-F4, #1454).
  - A checkpoint gates WAL truncation on `Graph.HasConstraints`, so
    declared constraints survive a checkpoint even when the embedder
    has not wired `checkpoint.WithConstraintSpecs` (#1464).
  - The WAL acquires an exclusive OS file lock on `Open` to prevent
    multi-process WAL corruption (#1416), `fsync`s after `ftruncate` in
    `poison()` (#1414), and the snapshot writer `fsync`s the `indexes`
    subdirectory after writing index files (#1415).

### Performance

- **Adjacency-list hub operations** (`graph/adjlist`) — `AddEdge` is now
  amortised **O(1)** and `RemoveAllEdgesFrom` **O(d)** for high-degree
  (hub) nodes, replacing the previous quadratic behaviour, via geometric
  pre-allocation with in-place append reuse and a single-lock bulk
  removal path. A degree-10 000 hub is now roughly 11.6× slower than a
  degree-1 000 hub, where it was previously 50–100× slower.

### Security

- **Exhaustive security audit and remediation (SEC-2026-06-14).** A
  phased, six-domain security audit found and fixed **13** issues
  (tasks #1467–#1479); the full report is in
  [docs/security-audit-2026-06-14.md](docs/security-audit-2026-06-14.md).
  All are reachable from untrusted input (a Cypher query, an imported
  file, or an on-disk artefact):
  - **Memory-exhaustion DoS bounds** — the snapshot record/string-table
    decoders now clamp their eager allocation *before* the CRC/size gate
    (a hostile snapshot could OOM `recovery.Open`), and the Cypher
    expression evaluator enforces a per-evaluation list-element budget so
    a tiny `reduce()`/comprehension query can no longer exhaust host
    memory (#1467, #1468, #1469, #1475).
  - **Cypher cancellability** — `reduce()`/comprehension/quantifier loops
    now honour `context` cancellation, so a deadline aborts a runaway
    query (#1477); variable-length-path traversal gained a per-query edge
    budget and a default hop ceiling (#1478).
  - **Bolt credentialed authentication** — `BasicAuthHandler` now
    authenticates at `LOGON` for Bolt ≥ 5.1, so credentialed auth works
    with modern drivers instead of forcing `NoAuth` (#1470).
  - **`=~` regex correctness** — the operator no longer behaves as string
    equality and is anchored per the openCypher specification, closing a
    fail-open hazard in authorisation predicates (#1479).
  - **Analytics over untrusted graphs** — `TransitiveClosure`, WCC and
    Kruskal compact over the live node set and `UnionFindSlice` uses
    64-bit indices, so hash-flooded node keys can no longer drive
    O(MaxNodeID²) over-allocation or index overflow (#1474, #1476).
  - **Export hardening** — opt-in CSV formula-injection sanitisation
    (#1471).
  - **Supply chain** — every GitHub Action in the release pipeline is
    pinned to a full-length commit SHA and the SBOM generator
    (`cyclonedx-gomod`) to an exact version, hardening the published
    artefacts against a moved/poisoned upstream tag
    (tj-actions/CVE-2025-30066 class) (#1472, #1473).

  Both compliance invariants held throughout: openCypher TCK at
  3 897 / 3 897 and ACID preserved; `go test -race`, golangci-lint,
  staticcheck and `govulncheck` are all clean.

## [0.2.0] — 2026-06-05

The second published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search. This is a pre-1.0 **MINOR**
release: it is additive over `v0.1.0` and the headline work is a
reliability, ACID, and durability hardening pass across the persistence
stack, the Cypher engine, and the Bolt server. Both compliance
invariants continue to hold without regression: the module is **100 %
openCypher TCK-compliant at the execution level** (3 897 / 3 897
scenarios) and **100 % ACID-compliant**.

Under Semantic Versioning, a `0.y.z` version signals that the public Go
API is **not yet stable** and may change while the module matures toward
`1.0.0`. Pin the exact version you depend on. A small number of
behavioural defaults were tightened in this release; consumers should
read the **Upgrade notes** in
[release-notes/v0.2.0.md](release-notes/v0.2.0.md).

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.2.0
```

### Added

- **True explicit transactions over Bolt** — `Engine.BeginTx` returns an
  `*ExplicitTx` with `Exec` / `Commit` / `Rollback`, wired through the
  Bolt `BEGIN` / `RUN` / `COMMIT` / `ROLLBACK` protocol on both the
  WAL-backed and store-less engine wirings. A Bolt transaction now owns
  a single engine transaction whose statements are **atomic together**
  (made durable and visible as one unit on commit, unwound together on
  rollback), **isolated** by the single-writer serialisation, and
  **bounded** by a configurable per-transaction timeout
  (`Server.Options.DefaultTxTimeout`, default 30 s) so the global write
  lock can never be held indefinitely.
- **Persisted, fully enforced Cypher constraints** — `CREATE CONSTRAINT`
  / `DROP CONSTRAINT` are now durable, validating, and complete across
  every property kind. Constraints are journalled through the store
  transaction, replayed on recovery, and carried in a `constraints.bin`
  snapshot component so they survive a checkpoint and WAL truncation.
  `CREATE CONSTRAINT` validates and seeds against pre-existing data, and
  `UNIQUE` is enforced for `float`, `bool`, `time`, `[]byte`, and list
  values, not only strings. New constructor
  `NewEngineWithStoreAndConstraints` re-registers recovered constraints
  on open.
- **Composed crash-safe store** (`store.DB`) — a new top-level `store`
  package with a `DB` type that owns the Write-Ahead Log and an optional
  checkpointer and performs the one correct teardown order in
  `Close` / `CloseCtx`: optional final checkpoint, stop the checkpoint
  goroutine, then close the WAL. The order is idempotent and safe under
  concurrent `Close`. `bolt.Server` adopts it via an optional
  `Options.Closer`.
- **Finite result caps with typed Bolt failures** — the engine exposes
  `Engine.ResultRowCap()`, and the Bolt server maps the engine's
  bounded-resource sentinels to the `Neo.ClientError.General.LimitExceeded`
  failure code so an over-broad query is rejected cleanly while the
  connection stays healthy. `NewServer` logs a loud warning when it is
  handed a deliberately uncapped engine.
- **Server-level Bolt metrics** — six counters emitted through the
  internal metrics backend: `bolt.server.conn.accepted` / `.rejected` /
  `.closed` and `bolt.server.tx.opened` / `.closed` / `.abandoned`, with
  live-connection and open-transaction values derived as paired counters.
- **New labelled-property-graph accessors** — `lpg.Graph.FirstEdgeHandle`
  (the stable handle of the first `src → dst` slot), `lpg.Graph.Config`
  (the immutable directed / multigraph configuration), and
  `lpg.Graph.ValidateNode` (run the installed schema validator's
  whole-node check at finalisation). The matching `adjlist.AdjList.Config`
  accessor is also added.
- **Commit-serialised checkpointing** — `store/txn.Store.RunUnderCommitLock`
  and `store/checkpoint.WithCommitSerialiser`, which run the
  snapshot-and-truncate window under the store's real commit lock so a
  checkpoint can never capture a partial transaction or truncate away a
  transaction committed during the snapshot.
- **Scoped-read CSR file API** — `store/csrfile.Reader.Read`, which holds
  an internal read lock for the duration of a callback so `Close` cannot
  unmap the `mmap` region under an in-flight reader. The semi-external
  `search/extern` BFS and PageRank traversals run inside this scope.
- **Configurable resource budgets** — opt-out sentinels and tunables on
  `EngineOptions` for the new caps: `MaxResultRows`, `MaxResultBytes`,
  and `MaxCollectItems`, each with an explicit unbounded sentinel
  (`-1`).

### Changed

- **Finite default resource caps now apply where results were previously
  unbounded.** Result materialisation, per-group aggregator buffers, the
  `Eager` pipeline-breaker, pattern-comprehension lists, and the
  per-transaction WAL op buffer now carry finite defaults:
  `DefaultMaxResultRows`, `DefaultMaxCollectItems`, and
  `DefaultMaxEagerRows` are `10_000_000`; `DefaultMaxGroups` is
  `1_000_000`; `DefaultMaxResultBytes` is 1 GiB; `DefaultMaxTxnOps` is
  `16_000_000`. A query that previously streamed an unbounded result now
  returns a typed bounded-resource error once a default is exceeded. The
  defaults are high enough that the openCypher TCK and every shipped
  example stay green; each is configurable, with an explicit unbounded
  opt-out.
- **Import decoders enforce a byte ceiling.** The CSV, JSON Lines, and
  GraphML decoders now reject input above `DefaultMaxBytes` (1 GiB) with
  the typed `ErrInputTooLarge`, bounding memory at the untrusted-input
  boundary. The cap is configurable, with `MaxBytes <= 0` meaning
  unlimited.
- **`graph/io` decoders return a nil graph on any decode error.** The
  CSV, JSON Lines, and GraphML readers now return a **nil** graph and
  discard the partial result on **any** failure — a parse error, context
  cancellation, or the byte-ceiling error — making import uniformly
  all-or-nothing. The typed errors are unchanged.
- **Re-entrant `Graph.View` / `ApplyAtomically` now panics instead of
  deadlocking.** A goroutine already inside the visibility barrier that
  nests another `View` or `ApplyAtomically` call now panics immediately
  with an actionable message, rather than silently deadlocking the
  engine. The panic is never recovered, because it indicates a
  programmer error.

### Fixed

- **Durability and crash-safety hardening.** Cypher autocommit writes are
  now made durable (WAL `fsync`) **before** they become visible to
  concurrent readers, matching the transaction-layer commit ordering. The
  snapshot writer `fsync`s the staging directory before the publish
  rename, the bulk loader (`store/csrfile`) `fsync`s the parent directory
  after its rename, and the graph's directed / multigraph configuration
  is persisted in the snapshot manifest so a simple graph no longer
  silently becomes a multigraph after a reopen.
- **Fail-stop on genuine WAL corruption.** Recovery now returns genuine
  corruption (CRC mismatch, unsupported record version) to the caller
  instead of swallowing it into a tail-error field, and the shipped
  examples refuse to append to a corrupt WAL. A benign torn or truncated
  tail remains a clean success; `Result.IsClean` reports the distinction.
- **Atomic in-memory rollback for write queries.** A Cypher write query
  that errors or panics mid-execution now rolls its in-memory mutations
  back through an inverse-op undo log inside the visibility barrier, so a
  failed query never leaves the in-memory graph diverged from the durable
  state. Per-handle edge metadata is restored on the multigraph
  removal-then-fail interleaving.
- **Context-aware blocking and cancellation.** The engine's write-path
  store acquisition is now context-aware (a cancellable semaphore
  replaces the blocking mutex), and five long traversals
  (direction-optimised BFS, diameter, Tarjan SCC, triangle counting, and
  Cypher `DETACH DELETE`) poll for cancellation inside their inner loops
  so a deadline or cancellation is honoured promptly.
- **Bounded resources, never panic.** Per-transaction, per-group, and
  `Eager` buffers are bounded; the `Distinct` cap now counts distinct
  rows rather than hash buckets, so engineered hash collisions can no
  longer retain more than the cap; and a coarse result-byte budget
  complements the row cap so a single very wide row cannot exhaust memory.
- **Concurrency-primitive hardening.** A scoped-read API prevents
  `store/csrfile.Reader.Close` from unmapping an `mmap` region under an
  in-flight reader; the `generation` publisher enforces its
  single-publisher contract and fixes a missed-wakeup that could hang a
  draining publisher; the checkpoint loop drains its trigger channel on
  exit so a `Trigger` racing `Stop` cannot leak a goroutine; the Bolt
  session rolls its transaction back at the `FAILED` transition so the
  writer lock is released promptly; and the metrics backend's hot path is
  made nil-safe.
- **Multigraph edge-metadata rollback.** Removing a parallel edge while a
  sibling between the same endpoints survives, then failing the query,
  now restores the removed instance's per-handle identity, labels, and
  properties exactly.
- **Integer-overflow guard in network flow.** Flow-capacity and cost
  accumulation now validate their inputs and return the typed
  `ErrCapacityOverflow` instead of silently producing a wrapped, negative
  result; the integer shortest-path algorithms document their
  cumulative-weight precondition.
- **Schema required-property validation.** A declared required
  (`NOT NULL` / existence) property is now enforced at node finalisation
  via `Graph.ValidateNode`, instead of being advisory-only, upholding the
  ACID Consistency invariant.

### Performance

- **Personalised PageRank** (`search/centrality`) — the push-relabel
  worklist now compacts its consumed prefix to track the live frontier,
  bounding the worklist's backing array to the frontier size rather than
  letting it grow toward the total step count. The rank vector is
  bit-for-bit identical to the previous implementation.

### Security

- **Toolchain bumped to go1.26.4** (`chore(toolchain)` commit `6969201`),
  resolving two Go standard-library vulnerabilities reachable from the
  module's own code paths:
  - **GO-2026-5039** (`net/textproto`) — reached via `snapshot.Open` →
    `textproto.Reader.ReadMIMEHeader`.
  - **GO-2026-5037** (`crypto/x509`) — reached via the Bolt TLS handshake
    in `bolt/proto/handshake.go`.

  Only the `toolchain` directive changed; the `go 1.26` language
  directive is untouched. `govulncheck ./...` is clean on go1.26.4
  (2 vulnerabilities reported before the bump, 0 after).

## [0.1.0] — 2026-06-02

The first published release of **GoGraph**, a Go module for graph
persistence, manipulation, and fast search that scales from in-memory
graphs to graphs that exceed RAM.

This release is published at a pre-1.0 baseline. Under Semantic
Versioning, a `0.y.z` version signals that the public API is **not yet
stable** and may change without a major bump while the module matures
toward `1.0.0`. The two compliance invariants are nonetheless already
in force at this version: the module is **100 % openCypher
TCK-compliant at the execution level** and **100 % ACID-compliant**.

Install with:

```bash
go get github.com/FlavioCFOliveira/GoGraph@v0.1.0
```

### Added

#### Core graph model

- **Generic graph contract** (`graph`) — a `Graph[N comparable, W]`
  interface parameterised over the node-identifier type `N` and the
  edge-weight type `W`, so the library is not tied to `int64`/`float64`.
  A sharded `Mapper` interns external keys to dense internal `NodeID`s
  with lock-free hot-path reads.
- **Directed and undirected graphs, multigraphs, and self-loops** —
  the model supports parallel edges between the same ordered pair and
  edges from a node to itself.
- **Mutable adjacency-list backend** (`graph/adjlist`) — a sharded,
  copy-on-write adjacency list with lock-free reads; the writer-side
  representation.
- **Immutable CSR snapshot** (`graph/csr`) — a Compressed Sparse Row
  view for read-mostly analytics with zero-synchronisation traversal.
- **Atomic snapshot rotation** (`graph/generation`) — a
  refcount-protected publisher that swaps immutable snapshots across
  concurrent readers and a single writer via an atomic pointer.
- **Labelled Property Graph** (`graph/lpg`) — vertex and edge labels
  with typed properties. `PropertyValue` covers `string`, `int64`,
  `float64`, `bool`, `time.Time`, and `[]byte`.
- **Optional type schema** (`graph/lpg/schema`) — a declarative schema
  with `Validate` for label/property typing.
- **Stable per-edge handle** — every directed edge carries an immutable
  `uint64` handle assigned at creation that is never reused or
  renumbered on delete, providing durable per-edge identity for the
  multigraph model.
- **Durable node tombstones** — deleted nodes are tombstoned (their
  `NodeID` is never reused) and the tombstone set survives a store
  reopen, so a deleted node never resurrects as a "ghost".

#### Indexing and pattern query

- **Index manager** (`graph/index`) — a `Manager` that coordinates
  named indexes and fans out `Change` events to subscribers.
- **Inverted label index** (`graph/index/label`) — Roaring-bitmap
  inverted index over vertex labels.
- **Exact-match property index** (`graph/index/hash`) — a sharded hash
  index for equality lookups.
- **Range property index** (`graph/index/btree`) — an order-preserving
  B-tree index for range queries.
- **Fluent pattern engine** (`graph/query`) — a `MATCH`-style fluent
  query API over labels and property predicates.

#### Search, path-finding, and analytics

- **Traversal and shortest paths** (`search`) — BFS, iterative DFS,
  Dijkstra, Bellman-Ford, A\*, bidirectional BFS, Yen k-shortest paths,
  topological sort (Kahn), Tarjan strongly-connected components,
  biconnected components, Eulerian path, and all-pairs shortest paths.
- **Centrality** (`search/centrality`) — Brandes betweenness, PageRank,
  and personalised PageRank.
- **Community detection** (`search/community`) — Leiden and label
  propagation.
- **Network flow** (`search/flow`) — Dinic, Edmonds-Karp, push-relabel,
  Stoer-Wagner global min-cut, and min-cost max-flow.
- **Semi-external algorithms** (`search/extern`) — BFS and PageRank that
  operate directly over Tier 2 `csrfile` readers without first loading
  the graph into the Go heap.
- **Union-find** (`ds`) — a disjoint-set primitive supporting the
  analytics layer.

#### Storage and persistence (ACID)

- **Write-Ahead Log** (`store/wal`) — a versioned, CRC32C-framed WAL
  with group-commit and a parent-directory `fsync` for crash-safe
  durability.
- **Atomic snapshots** (`store/snapshot`) — on-disk snapshot directories
  with a manifest and per-file CRC, including the optional
  `tombstones.bin` and `edgehandles.bin` components.
- **Single-writer transactions** (`store/txn`) — a `Begin` / `Commit` /
  `Rollback` API with all-or-nothing atomicity and a transaction-
  visibility barrier for isolation.
- **Background checkpointer** (`store/checkpoint`) — folds the WAL into
  a snapshot folder on a background goroutine with a defined lifecycle.
- **Recovery** (`store/recovery`) — snapshot restore plus idempotent
  WAL replay on open, producing a true multigraph so parallel and typed
  relationships survive a reopen.
- **Tier 2 CSR file format** (`store/csrfile`) — an `mmap`-backed,
  versioned, 64-byte-aligned CSR file with a validated header, a
  zero-copy `Reinterpret` helper, and a deterministic fixture generator.
- **Bulk loader** (`store/bulk`) — high-throughput ingestion that
  bypasses the WAL for initial loads.

#### Cypher engine

- **openCypher-compatible engine** (`cypher`) — a full
  parser → planner → executor pipeline with a plan cache,
  `EXPLAIN` / `PROFILE`, and dbhits accounting. WAL-durable writes are
  available via `NewEngineWithStore`.
- **Pipeline packages** (`cypher/parser`, `cypher/ast`, `cypher/sema`,
  `cypher/ir`, `cypher/plan`, `cypher/exec`) — the staged
  parse-to-execution components.
- **Built-in functions and procedures** (`cypher/funcs`,
  `cypher/procs`).
- **openCypher TCK harness** (`cypher/tck`) — the Technology
  Compatibility Kit runner. The execution suite is fully green at
  **3 897 / 3 897 scenarios** (16 006 / 16 006 steps); the regression
  gate (`tckExecutionBaseline = 3897` in `cypher/tck/runner_test.go`)
  rejects any change that lowers the passing count.

#### Bolt server

- **Bolt v5 protocol** (`bolt/proto`, `bolt/packstream`) — Bolt v5
  protocol handling and PackStream encoding (v5.0–v5.6 preferred, v4.4
  fallback).
- **TCP server** (`bolt/server`) — a Bolt server compatible with
  `neo4j-go-driver` v5 and `cypher-shell`, with TLS certificate
  hot-reload and graceful shutdown. `NewServer` fails closed when no
  authentication handler is configured.

#### Interchange

- **Import / export** (`graph/io/csv`, `graph/io/graphml`,
  `graph/io/dot`, `graph/io/jsonl`) — CSV, GraphML, Graphviz DOT, and
  JSON Lines formats.

#### Benchmarks and examples

- **Benchmark harnesses** (`bench/ldbc`, `bench/dimacs9`, `bench/rmat`,
  `bench/soak`, `bench/comparison`) — LDBC SNB, DIMACS 9 SSSP, RMAT
  power-law generation, a multi-hour mixed-workload soak harness, and a
  cross-library comparison.
- **25 runnable examples** (`examples/`) — covering the core API,
  persistence and out-of-core processing, the Cypher engine, the Bolt
  server, analytics, interchange, and end-to-end recipes. See
  [examples/README.md](examples/README.md) for the categorised index.

### Compliance

- **100 % openCypher TCK-compliant at the execution level** — every
  scenario in `cypher/tck/features/` passes, with no `failed`, no
  `undefined`, and no `pending` steps (3 897 / 3 897 scenarios).
- **100 % ACID-compliant** — Atomicity, Consistency, Isolation, and
  Durability hold across the in-memory engine and every persistence
  backend, verified by the WAL recovery tests in `store/wal` and
  `store/recovery` and the deterministic crash-injection battery in
  `internal/crashinject`.

### Notes

- **Pre-1.0 stability.** This is a `0.y.z` release. The public Go API
  may change without a major-version bump until `1.0.0`; pin the exact
  version you depend on.
- **Module path.** The Go module path is
  `github.com/FlavioCFOliveira/GoGraph` with no `/vN` suffix, which is
  Semantic-Import-Versioning-correct for a `0.x` line.

[0.5.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.5.0
[0.4.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.4.0
[0.3.2]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.3.2
[0.3.1]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.3.1
[0.3.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.3.0
[0.2.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.2.0
[0.1.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.1.0
