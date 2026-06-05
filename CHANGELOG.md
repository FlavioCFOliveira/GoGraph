# Changelog

All notable changes to GoGraph are documented in this file. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project follows [Semantic Versioning](https://semver.org/).

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

[0.2.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.2.0
[0.1.0]: https://github.com/FlavioCFOliveira/GoGraph/releases/tag/v0.1.0
