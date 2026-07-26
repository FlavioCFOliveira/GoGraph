# Stream 2 — Transactions, isolation levels, concurrency control

Baseline: `6f31f61` (v0.10.0). Hardware for every measurement: Apple M4 (10 cores: 4P+6E),
darwin/arm64, `go1.26.5`. Method stated per finding.

## Verdict summary

GoGraph's **isolation semantics are the strongest of the three** and round 1 understated the
margin: write skew — the one anomaly Memgraph's own documentation admits *no* isolation level it
offers can prevent (`G2_item` → "Disallowed by: NONE") and which Neo4j requires manual
`acquireWriteLock` to prevent — is structurally impossible in GoGraph, measured. GoGraph is also
immune to Neo4j's documented "missing and double reads" index-scan anomaly, needs `Eager` at
exactly one planner site against Neo4j's four conflict classes, and has no MVCC-version retention
hazard. But the **implementation of that isolation is now the engine's single largest scalability
defect, and it is not the one round 1 identified**. The re-entrancy guard calls `runtime.Stack` on
every barrier entry; `runtime.Stack` takes the Go runtime's process-global `debuglock`, so
`Graph.View` costs 2.1 µs + 64 B and **read throughput falls by half between 1 and 10 cores**
(pprof: `runtime.Stack.func1` = 85.09 % of CPU on a trivial parallel read). Two further NEW defects:
`BeginTx(ctx)`/`Run(ctx)` silently overrun their deadlines by >10× while queued on the barrier
(Bolt `tx_timeout` does not bound BEGIN), and `BeginReadTx` — documented as "never blocks" — inherits
100 % of an open write transaction's hold. The single most valuable lever from the incumbents is
Neo4j's **transaction-local write overlay (TxState)**: it is what lets Neo4j keep an explicit
transaction's writes private without MVCC, and it is the only sound way to shrink GoGraph's
barrier hold from "whole transaction lifetime including client think-time" to "one commit window"
without building the MVCC round 1 rejected.

## Feature-by-feature comparison

| Feature | GoGraph (file:line) | Neo4j | Memgraph | Verdict | Label |
|---|---|---|---|---|---|
| Concurrency-control mechanism | Global RWMutex visibility barrier + single-writer semaphore (`graph/lpg/lpg.go:491`, `store/txn/txn.go:425`) | 2PL, Forseti lock manager, node/rel granularity | MVCC delta chains (HyPer, SIGMOD 2015), lock-free skiplists | BETTER on correctness, WORSE on concurrency | CONFIRMED-R1 |
| Default isolation, autocommit read | Statement-level snapshot isolation (`cypher/api.go:1900` whole query in one `View`) | READ COMMITTED, no read locks, no snapshot | SNAPSHOT_ISOLATION (default) | BETTER than Neo4j, PARITY with Memgraph | NEW |
| Default isolation, explicit write tx | Full serialisation (barrier held BEGIN→COMMIT, `cypher/exectx.go:304`) | READ COMMITTED | SNAPSHOT_ISOLATION | BETTER than both | CONFIRMED-R1 |
| Explicit read-only tx | Per-statement read-committed (`cypher/exectx.go:395`) | READ COMMITTED | SNAPSHOT_ISOLATION (repeatable) | PARITY w/ Neo4j, WORSE than Memgraph | NEW |
| Selectable isolation levels | none | explicit locks only (no Cypher syntax) | `--isolation-level`, `SET GLOBAL/SESSION/NEXT TRANSACTION ISOLATION LEVEL` | WORSE | NEW |
| Write skew (G2-item) | **impossible** (measured) | possible; manual locks required | **documented as unpreventable at every level** | BETTER than both | NEW |
| Lost update | impossible, both paths (measured 480/480) | documented anomaly; auto write-lock only when SET RHS directly reads the property | prevented by first-updater-wins + client retry | BETTER than both | NEW |
| Index-scan missing/double reads | impossible (statement pinned in `View`) | **documented anomaly, even on uniqueness-constraint indexes** | not applicable (MVCC) | BETTER than Neo4j | NEW |
| Deadlock | structurally impossible (total lock order, one global writer) | Forseti + Dreadlocks O(1) + secondary cycle verify; `Neo.TransientError.Transaction.DeadlockDetected`; client retry | avoided; write-write conflict instead → retry | BETTER | NEW |
| Client retry contract | **none needed** | required (drivers auto-retry managed tx) | required (`TransactionSerializationException`) | BETTER | CONFIRMED-R1 |
| Concurrent disjoint writes | none; 126 k/s @1 core → 98 k/s @10 (measured) | yes (node-level locks; dense nodes avoid exclusive locks) | yes (MVCC + skiplists) | WORSE than both | CONFIRMED-R1 |
| Readers blocked by an open write tx | **yes, 100 %** (p50 95 µs → 10 653 µs) | no | no | WORSE than both | CONFIRMED-R1 (new median figure) |
| Barrier-entry cost / scaling | 2.1 µs, 64 B, **negative scaling** (`graph/lpg/goid.go:36`) | n/a | n/a | WORSE than both | **NEW** |
| Lock-acquisition timeout | none; `LockBarrier`/`writeMu` ignore ctx | `db.lock.acquisition.timeout` (default `0s`) | `--storage-access-timeout-sec` (default 1 s) | WORSE than both | **NEW** |
| MVCC version retention hazard | **none** (no versions) | none (no versions) | oldest active tx pins GC horizon; 56 B/delta | BETTER than Memgraph | NEW |
| In-flight tx state per mutation | 226.6 B (measured); unbounded on store-less engine | "kept in memory"; split large updates | 56 B/Delta; OOM documented | WORSE | NEW |
| `Eager` insertion sites | 1 (`cypher/ir/writes.go:233`), capped at 10 M rows | 4 conflict classes, uncapped, documented memory warning | n/a | BETTER | CONFIRMED-R1 |
| `CALL {} IN TRANSACTIONS` | absent from the grammar (`cypher/parser/grammar/CypherParser.g4:58,142`) | full: `OF n ROWS`, `ON ERROR CONTINUE\|BREAK\|FAIL\|RETRY`, `IN n CONCURRENT`, `DISJOINT BY`, `REPORT STATUS` | absent | WORSE than Neo4j | NEW ruling |
| Nested tx / savepoints | none | none (placebo tx only) | none | PARITY | NEW |
| Analytical / non-ACID mode | none | none (offline `neo4j-admin import`) | `IN_MEMORY_ANALYTICAL` | **reject — see ruling** | NEW |

---

## Findings

### F1. Barrier entry serialises the whole engine on the Go runtime's global `debuglock` [NEW] (severity: HIGH)

- **What they do:** neither incumbent needs goroutine identity. Neo4j threads a `KernelTransaction`
  object; Memgraph threads a `Transaction` with `start_timestamp`/`command_id`
  (`src/storage/v2/delta.hpp`). Re-entrancy is a non-question because the transaction context is an
  explicit parameter, not something recovered from the thread.
- **What GoGraph does:** the visibility barrier's re-entrancy guard identifies the caller by parsing
  `runtime.Stack` output. `graph/lpg/goid.go:36-62` is the only caller of `runtime.Stack` in the
  module (pprof `-peek` shows `lpg.goID` = 100 % of its callers), and it is invoked on **every**
  `Graph.View` (`graph/lpg/lpg.go:630` → `reentrancy.go:174`), every `ApplyAtomically`
  (`lpg.go:529` → `reentrancy.go:123`), every `LockBarrier` (`lpg.go:566`) and every
  `UnlockBarrier` (`lpg.go:583`). `Engine.Run` takes exactly one `View` per query
  (`cypher/api.go:1900`), so this is one `runtime.Stack` per statement, not per row.
- **Evidence** (`go test -bench -benchmem -cpu=1,2,4,8,10 -benchtime=500ms`, M4/10-core):

  | benchmark | 1 | 2 | 4 | 8 | 10 | allocs |
  |---|---|---|---|---|---|---|
  | `goID` (serial) | 1452 ns | — | — | — | — | 64 B, 1 |
  | `Graph.View` (serial) | 2131 ns | — | — | — | — | 64 B, 1 |
  | `Graph.View` (RunParallel, ns/op aggregate) | 1623 | 2327 | 2747 | 3109 | **3215** | 64 B, 1 |
  | bare `sync.RWMutex` RLock/RUnlock (RunParallel) | 3.7 | 8.7 | 18.6 | 75.1 | 78.4 | 0 |
  | `RETURN 1` end-to-end (RunParallel) | 5763 | 5716 | 7834 | 8635 | **9818** | 28 |

  Aggregate `View` throughput **halves** from 1 → 10 cores (0.62 → 0.31 Mops/s); trivial-query
  throughput falls 41 %. Adding cores makes the engine slower.

  CPU profile of `RETURN 1` at `-cpu=10` (3.78 s, 7.38 s samples):
  `runtime.Stack.func1` **85.09 % cum**, `runtime.printlock` **55.28 % cum**,
  `runtime.lock2` 55.42 %, `runtime.unlock2` 25.61 %.
  `pprof -peek runtime.printlock` shows its callers are `printFuncName` (30 %),
  `goroutineheader` (25 %), `traceback2` (16 %), `printArgs` (21 %) — i.e. the traceback's
  print path, which takes the runtime's process-global `debuglock` **once per stack frame**.

  Cost is therefore linear in stack depth — measured 2044 ns @ depth 1, 6825 ns @ depth 20,
  17 163 ns @ depth 60 (~250 ns/frame). Deeper Cypher call stacks pay more.

  Escape analysis (`go build -gcflags=-m ./graph/lpg/`):
  `graph/lpg/goid.go:39:6: moved to heap: buf` → 64 B heap allocation on every barrier entry.
  This violates CLAUDE.md "zero-allocation hot paths" and "Hot paths must not hold a global lock".
- **Lever:** compile the guard out of production builds. Put `checkWriter`/`enterReader`/
  `exitReader` behind a build tag (`//go:build barrierguard || race`) with no-op stubs otherwise,
  and add `-tags=barrierguard` to `make ci` so every gated run still enforces it. The guard is a
  programmer-error assertion, exactly the class Go itself compiles out (`sync.Mutex` does not
  detect self-deadlock either). Expected effect: barrier entry 2131 ns → ~5 ns, 64 B → 0 B, and
  `View` scaling reverts to the bare-RWMutex curve. If the guard must stay on in production, the
  alternative is a cheap goroutine identity, but that requires `unsafe`/`linkname` into runtime
  internals and is fragile — recommend the build tag.
- **TCK/ACID impact:** none. The guard participates in no isolation decision; it only converts a
  would-be self-deadlock into a panic. `visMu` acquisition order and every barrier window are
  byte-for-byte unchanged. Nesting is a fixed, statically-known property of the call graph
  (`cypher/exec` never re-enters — isolation-design.md:259) and stays enforced in CI and under
  `-race`.
- **Effort:** S.

### F2. `BeginTx(ctx)` and `Engine.Run(ctx,…)` block past their deadlines and return `nil` error [NEW] (severity: HIGH)

- **What they do:** Neo4j provides `db.lock.acquisition.timeout` as a *separate* bound on lock
  waits, precisely because a transaction blocked on a lock executes no code and cannot observe its
  own termination flag: "A transaction in such a state is waiting for a lock to be released by
  another transaction, and not executing code. This includes code that checks to see if the
  transaction has been marked for termination… It's highly recommended that when you set the
  transaction timeout that you set the lock acquisition timeout as well."
  (https://neo4j.com/developer/kb/understanding-transaction-and-lock-timeouts/). Memgraph has
  `--storage-access-timeout-sec` (default 1 s) with a typed failure
  ("Cannot get shared/unique/read-only access to the storage. Try stopping other parallel queries.").
- **What GoGraph does:** `cypher/exectx.go:290` acquires the store semaphore **ctx-aware**
  (`store/txn/txn.go:652-662`, `select` on `ctx.Done()`), then `cypher/exectx.go:304` calls
  `tx.eng.g.LockBarrier()` → `graph/lpg/lpg.go:567` `g.visMu.Lock()` with **no context and no
  post-acquire deadline recheck**. On the store-less engine the same hole exists one level earlier:
  `cypher/api.go:1013` `e.writeMu.Lock()`. The read path has it too: `Engine.Run` calls
  `checkContext(ctx)` at `cypher/api.go:1831` and then blocks in `g.View` →
  `graph/lpg/lpg.go:632` `visMu.RLock()`.
- **Evidence** (direct probe, temp test, deleted after measuring): with a reader holding the
  barrier for 600 ms, `BeginTx(ctx)` carrying a **50 ms** deadline returned after **601 ms** with
  `err=nil` — it handed back a live transaction holding both the single-writer semaphore and the
  global barrier, whose context was already expired, so the caller's next `Exec` fails while the
  whole engine is stalled behind it. Symmetrically, with a write `ExplicitTx` held 600 ms,
  `Engine.RunAny(ctx,…)` carrying a 50 ms deadline returned after **580 ms** with `err=nil`.
  Operational reach: Bolt derives `tx_timeout` onto this ctx
  (`bolt/server/session.go:1188`, default 30 s at `serve.go:257`), so a client's `tx_timeout`
  does not bound `BEGIN`.
- **Lever:** two changes, independently shippable.
  (i) *Cheap, closes the silent-`nil` case:* re-check `ctx.Err()` immediately after
  `LockBarrier()`/`writeMu.Lock()` and, if expired, release and return the context error. One `if`
  per site; makes the API honest.
  (ii) *Correct fix:* make barrier acquisition ctx-aware. Replace `visMu` with a
  semaphore-channel-based RW lock, or wrap acquisition in a `TryLock` + bounded-backoff loop that
  polls `ctx.Done()`. Then surface an explicit `LockAcquisitionTimeout` engine option mirroring
  Neo4j's setting, so an embedder can bound head-of-line blocking independently of the query
  timeout.
- **TCK/ACID impact:** none. Failing to acquire the barrier means the transaction never starts —
  no state change, no WAL frame, nothing to recover. TCK is Cypher semantics and is insensitive to
  acquisition timing. The change strictly *adds* a failure mode that currently manifests as an
  unbounded stall.
- **Effort:** S for (i), M for (ii).

### F3. `BeginReadTx` — documented as never blocking — inherits 100 % of an open write transaction's hold [NEW] (severity: HIGH)

- **What they do:** Neo4j read-committed acquires **no read locks** ("only write locks are
  acquired and held until the end of the transaction",
  https://neo4j.com/docs/java-reference/4.4/transaction-management/), so a reader is never blocked
  by a writer. Memgraph: "Built on multi-version concurrency control (MVCC), Memgraph ensures that
  **writes don't block reads** and **reads don't block writes**"
  (https://memgraph.com/docs/deployment/workloads/memgraph-in-high-throughput-workloads).
- **What GoGraph does:** the *handle* takes nothing (`cypher/exectx.go:334-348`), but every
  statement routes to `Engine.Run` (`cypher/exectx.go:395`), which takes `g.View` →
  `visMu.RLock` — the same mutex the write `ExplicitTx` holds exclusively for its whole lifetime
  (`cypher/exectx.go:304`).
- **Evidence:** probe — a `BeginReadTx` statement measured **3.23 ms** with no writer, and
  **300.46 ms** when issued while a write `ExplicitTx` was held open for 300 ms. Full inheritance.
  The docs assert the opposite in two places:
  `cypher/exectx.go:74-76` "takes neither the writer serialisation nor the visibility barrier and
  so never blocks", and `docs/isolation-design.md:56-58` "acquires **neither** the single-writer
  serialisation **nor** the visibility barrier **nor** a WAL transaction" — which
  `docs/isolation-design.md:60` then contradicts in the same paragraph ("per-statement
  `Graph.View` RLock"). CLAUDE.md requires documentation "accurate and faithful to the code".
- **Lever:** the accuracy fix is a doc correction (state that the *handle* holds nothing but each
  statement takes the read barrier, and that a read-only transaction is therefore blocked for the
  lifetime of any open write transaction). The *behavioural* fix is F5 or #2051 — there is no
  narrower one, because the write transaction's uncommitted writes are on the live graph and the
  barrier is the only thing hiding them.
- **TCK/ACID impact:** doc-only change is neutral; the behavioural fix is covered under F5.
- **Effort:** S (doc), L (behaviour, = F5).

### F4. Head-of-line blocking hits the median, not just the tail [CONFIRMED-R1] (severity: HIGH)

- **What GoGraph does:** `cypher/exectx.go:61-80` documents this honestly as an "operational
  contract", and the project already ships the characterisation benchmark
  (`cypher/reader_under_write_tx_bench_test.go:62`).
- **Evidence** (`-benchtime=2s`, M4): scanning read over 2 000 nodes —

  | writer | p50 | p99 | max |
  |---|---|---|---|
  | none | 95 µs | 388 µs | 5 956 µs |
  | write tx held 2 ms | 2 320 µs | 2 759 µs | 8 001 µs |
  | write tx held 10 ms | **10 653 µs** | 11 737 µs | 12 562 µs |

  Round 1 framed this as a *tail* problem. It is not: at a 10 ms hold the **median** degrades
  112× and p99/p50 converges to 1.1 — every reader pays the full hold, deterministically.
- **Lever:** F5, then #2051.
- **Effort:** see F5.
- **Label rationale:** the phenomenon was known; the median measurement and its severity are new.

### F5. Adopt a transaction-local write overlay so the barrier hold shrinks from "whole tx lifetime" to "one commit window" [NEW] (severity: HIGH — highest-value lever)

- **What they do:** Neo4j keeps a transaction's writes in transaction-local state and merges them
  into that transaction's reads, so nothing is published until commit and the transaction still
  sees its own writes: "All modifications performed in a transaction are kept in memory. This
  means that very large updates must be split into several transactions to avoid running out of
  memory."
  (https://neo4j.com/docs/operations-manual/current/database-internals/transaction-management/).
  Memgraph achieves the same intra-transaction visibility with `command_id` + `View::NEW`/`OLD` on
  the delta chain (`src/storage/v2/delta.hpp`).
- **What GoGraph does:** `ExplicitTx.Exec` applies every mutation **eagerly to the live shared
  graph** (`cypher/exectx.go:39-45`, mutator adapters at `cypher/api.go:9719` ff.) and records
  inverses in one shared undo log. Because the writes are on the live graph, the *only* thing
  preventing a dirty read is holding `visMu` exclusively for the entire transaction — which is
  exactly what `BeginTx` does (`cypher/exectx.go:298-305`). That window includes every client
  network round-trip.
- **Evidence:** GoGraph already implements the better shape on the other write path. The
  store-direct `Tx` buffers its ops and applies the whole list inside **one** `ApplyAtomically` at
  commit (`store/txn/txn.go:1176-1186`), so its barrier hold is O(ops), not O(session lifetime).
  The Cypher path diverged deliberately to get read-your-own-writes for free
  (`cypher/exectx.go:55-57` — later statements see earlier ones "because they share the live
  in-memory graph"). An overlay restores that property without publishing.
- **Lever:** stage the explicit transaction's forward writes in a private overlay (per-tx maps
  keyed by node/edge id for labels, properties, adjacency deltas and tombstones) and make the
  transaction's own read path consult overlay-then-graph. Publish the overlay under one
  `ApplyAtomically` at Commit, exactly as `Tx.Commit` does today. Effects: `BeginTx` no longer
  takes the barrier at all; readers and read-only transactions run unblocked throughout
  (fixes F3 and F4); the undo log can be dropped for the overlay path (rollback = discard the
  overlay, an O(1) free instead of an O(ops) replay); and the write-side single-writer
  serialisation is untouched, so write-write isolation, the retry-free contract and deadlock
  freedom all survive verbatim.
- **TCK/ACID impact:** Atomicity **strengthened** (nothing is ever on the live graph before it is
  durable, so `ErrUndoFailed` — the "graph may be inconsistent until reopen" hazard at
  `cypher/undo.go:56` — becomes unreachable for the explicit-tx path). Isolation strengthened
  (readers no longer merely blocked; they are genuinely unaffected). Durability unchanged (one
  `CommitWALOnly` fsync, still durable-before-visible). TCK is unaffected provided overlay
  read-through is exact — that is the whole risk and it must be proven by running the full 3897
  suite through an explicit transaction, not only autocommit.
- **Effort:** L. Sequence it **after** F1 and F2, which are S/M and independently valuable.

### F6. Write skew is structurally impossible in GoGraph — and Memgraph documents that no level of its own prevents it [NEW] (severity: informational — a win to defend)

- **What they do:** Memgraph publishes an Adya-phenomena table whose last row is
  `G2_item` → "Disallowed by: **NONE**"
  (https://memgraph.com/docs/fundamentals/transactions), and its source states
  "The paper implements a fully serializable storage, in our implementation we only implement
  snapshot isolation for transactions"
  (`src/storage/v2/inmemory/storage.hpp:104-107`). Neo4j at READ COMMITTED cannot prevent it
  either; the documented remedy is manual `Transaction.acquireWriteLock`, and "In Cypher there is
  no explicit support for this" — the docs offer a dummy-property write as the workaround.
- **What GoGraph does:** a write `ExplicitTx` holds the global exclusive barrier from BEGIN to
  COMMIT, so two transactions' read-sets and write-sets can never interleave.
- **Evidence:** doctors-on-call probe (two transactions each read `count(oncall=true)`, and if
  ≥ 2 take themselves off call) — result **1 doctor still on call**, i.e. no skew. Companion
  probes: lost update 480/480 on both the explicit-tx read-modify-write path and the autocommit
  `SET c.v = c.v + 1` path (8 goroutines × 60 iterations); non-repeatable read inside a write
  transaction 1→1 with a concurrent writer given 80 ms to interleave.
- **Lever:** nothing to take. Defend it: any move toward concurrent writers (F10) forfeits it.
- **Effort:** n/a.

### Anomaly matrix (all rows measured at `6f31f61`)

| Anomaly | Autocommit `Run`/`RunInTx` | Write `ExplicitTx` (`BeginTx`) | Read-only tx (`BeginReadTx`) | Preventing code path | Test |
|---|---|---|---|---|---|
| Dirty read | prevented | prevented | prevented | reader's `visMu.RLock` cannot overlap the writer's `Lock` (`lpg.go:632` vs `:567`) | probe A1 ✔ (reader blocked 150 ms, then saw the pre-tx value after rollback); shipped: `cypher.TestIsolation_Cypher_NoPartialWriteObservable` |
| Non-repeatable read | n/a (one statement) | prevented (1→1) | **PERMITTED** (1→2) | write tx holds exclusive barrier; read tx takes a fresh `View` per statement (`exectx.go:395`) | probe A2/A2b ✔ — **no shipped test asserts the read-tx behaviour** |
| Phantom | n/a | prevented | **PERMITTED** (count 1→2) | same | probe A3 ✔ — **no shipped test** |
| Read skew | prevented within the statement (whole query in one `View`) | prevented | PERMITTED across statements | same | probe A4 ✔ — **no shipped test** |
| Lost update | prevented (480/480) | prevented (480/480) | n/a (writes rejected, `ErrWriteInReadOnlyTx`) | single-writer semaphore + barrier | probe A5 ✔ |
| Write skew | n/a | **prevented** | n/a | exclusive barrier BEGIN→COMMIT | probe A6 ✔ — **no shipped test** |
| Missing/double read on index scan | prevented | prevented | prevented | statement pinned in one `View`; index buffer flips inside the same window (`exectx.go:557`) | implied by F3.4/F3.6 battery |

**Gap:** the read-committed-across-statements contract of `BeginReadTx` is *documented*
(`cypher/exectx.go:319-329`) but **not asserted by any test**. A regression that accidentally
pinned or accidentally widened that contract would pass `make ci`. Add three cheap table-driven
cases (non-repeatable read, phantom, read skew) asserting the *permitted* behaviour, so the
documented level is pinned in both directions. Effort: S. TCK/ACID impact: none (test-only).

### F7. `Eager` needed at one site, and bounded — confirmed and extended [CONFIRMED-R1] (severity: informational)

- **What they do:** Neo4j's planner inserts `Eager` for four conflict classes, named verbatim in
  the PROFILE Details column: `read/create conflict`, `read/delete conflict`,
  `read/set conflict for label`, `read/set conflict for property`
  (https://neo4j.com/docs/cypher-manual/current/planning-and-tuning/operators/operators-detail/).
  It is uncapped and the docs warn: "The Eager operator can cause high memory usage when importing
  data or migrating graph structures."
- **What GoGraph does:** exactly one insertion site, `cypher/ir/writes.go:233`, and only for
  DELETE-then-MERGE. `cypher/exec/eager.go:31` caps it at `DefaultMaxEagerRows = 10_000_000` with
  `ErrEagerMemoryExceeded`.
- **Evidence:** I probed the other three Neo4j classes at `6f31f61` — `MATCH (n:X) CREATE (:X)`
  over 2 nodes → 4 (correct, no runaway); `MATCH (a:A) SET a:B` and the cartesian
  `MATCH (a:A),(n:N) SET n:A` → correct counts; `MATCH (a:P),(b:P) WHERE a.id<b.id SET a.v=b.v` →
  consistent; `MATCH (a:D) MATCH (b:D) DELETE a` → 0 remaining. All correct **without** an Eager
  operator. Round 1's claim holds, and the reason is now explicit: `Engine.Run`/`RunInTx`
  materialise the entire result inside a single barrier window (`cypher/api.go:3925`,
  `docs/isolation-design.md:253-266`), so GoGraph is *unconditionally* eager where Neo4j is
  *selectively* eager.
- **Honest counterweight:** that unconditional materialisation is a real cost — no streaming result
  path exists, documented at `docs/isolation-design.md:310-314`. It is bounded
  (`DefaultMaxResultRows = 10_000_000` at `cypher/api.go:3200`, plus `DefaultMaxResultBytes`), so
  the resource mandate holds, but the barrier is held for the whole drain — for a write query,
  exclusively.
- **Lever:** nothing to take from Neo4j's broad Eager. Do **not** import planner-side conflict
  analysis; GoGraph's execution model makes it redundant.
- **Effort:** n/a.

### F8. `CALL { … } IN TRANSACTIONS` — the ruling round 1 deferred [NEW] (severity: MEDIUM, needs a user decision)

- **What they do (verbatim, strongest warning):** "**If an error occurs, any inner transactions
  that were successfully committed remain unchanged and are not rolled back. However, any inner
  transactions that failed are fully rolled back. This behavior applies regardless of which ON
  ERROR option is used.**"
  (https://neo4j.com/docs/cypher-manual/current/subqueries/subqueries-in-transactions/). Also:
  "CALL { … } IN TRANSACTIONS is only allowed in implicit transactions"; default batch 1 000 rows;
  `ON ERROR FAIL` is the default; modern syntax adds `IN n CONCURRENT TRANSACTIONS`,
  `DISJOINT BY`, `ON ERROR RETRY … THEN …`, `REPORT STATUS AS`. Memgraph has no equivalent.
- **What GoGraph does:** nothing — `CALL` in the grammar accepts only a procedure invocation
  (`cypher/parser/grammar/CypherParser.g4:58,142`); there is no `CALL { }` subquery of any kind,
  and `TRANSACTIONS` is not a lexer token. It is **not TCK-covered** (openCypher 9 has no CALL
  subqueries; `grep -rn "TRANSACTIONS" cypher/tck/features/` → no matches), so **the TCK baseline
  of 3897 is unaffected whichever way this goes**.
- **What it would break — precisely:**
  1. *Nothing in the ACID mandate, if framed correctly.* The mandate binds **transactions**
     ("every transaction is all-or-nothing"), not statements. `CALL {} IN TRANSACTIONS` is a
     transaction-control construct: N genuine ACID transactions driven by one query. It is
     semantically identical to a client loop issuing N autocommit `RunInTx` calls — which GoGraph
     already permits and which nobody calls an ACID violation. What would violate the mandate is
     letting it run *inside* an explicit transaction, because then a transaction would commit part
     of itself. Neo4j already forbids exactly that.
  2. *One GoGraph-specific structural blocker.* `graph/lpg/reentrancy.go:26-28` names this feature
     by name as the case the guard exists to catch: "a future CALL { … } IN TRANSACTIONS … would
     silently freeze the engine." A batch loop driven from inside the enclosing query's barrier
     window would re-enter `ApplyAtomically` from the same goroutine and panic. The implementation
     must therefore materialise the driving rows, **release** the barrier, and then run each batch
     in its own barrier window — structurally the same Eager-then-batch shape Neo4j uses.
  3. *`REPORT STATUS` and the outer statistics roll-up* need a per-batch result accumulator that
     survives across barrier windows; nothing in `Result` supports that today.
- **Why it matters beyond convenience:** it is the incumbents' *official* mitigation for unbounded
  in-flight transaction state (F9). Neo4j: "very large updates must be split into several
  transactions to avoid running out of memory." Memgraph: "the user needs to batch transactions in
  the `IN_MEMORY_TRANSACTIONAL` storage mode … large-update transactions can make the number of
  delta objects be huge - therefore resulting in Memgraph getting out of memory."
- **Options for the user (recommend a):**
  - **a) Implement it, with three binding conditions** — (i) each batch is a full ACID transaction
    (own WAL fsync, own barrier window, own undo log/overlay); (ii) rejected with a typed error
    inside an explicit transaction, matching Neo4j's implicit-only rule; (iii) the non-atomicity of
    the *statement* is stated explicitly in the godoc and in `docs/cypher.md`, next to the mandate,
    so "100 % ACID" is never read as "this statement is atomic". Start with `OF n ROWS` +
    `ON ERROR FAIL|CONTINUE|BREAK`; defer `IN n CONCURRENT` (it needs concurrent writers — F10) and
    `DISJOINT BY`.
  - **b) Go-level batching API only** (`Engine.RunBatched(ctx, query, batchSize)`), no Cypher
    surface. Delivers the OOM mitigation with zero grammar change and zero ambiguity about the
    mandate. Cheapest.
  - **c) Decline.** Then F9's mitigation must come from an engine-level mutation cap instead, and
    GoGraph has no answer for "how do I update 100 M nodes".
  - Recommendation: **(b) now, (a) later.** (b) is S-effort, unblocks the real problem, and cannot
    be misread as weakening the mandate; (a) is the Neo4j-parity surface and should follow once
    F1/F2/F5 have settled the barrier model.
- **TCK/ACID impact:** zero TCK either way (not covered). ACID preserved under conditions (i)–(iii).
- **Effort:** S for (b), M for (a).

### F9. In-flight transaction state costs 226.6 B/mutation and is unbounded on the store-less engine [NEW] (severity: MEDIUM)

- **What they do:** Memgraph documents its per-change cost precisely — "Each `Delta` object has a
  least **56B**" (https://memgraph.com/docs/fundamentals/storage-memory-usage) — and documents both
  the failure mode and the mitigation (batch your transactions). Neo4j documents the failure mode
  ("All modifications performed in a transaction are kept in memory… very large updates must be
  split into several transactions to avoid running out of memory") and ships `CALL {} IN
  TRANSACTIONS` as the mitigation.
- **What GoGraph does:** `cypher/undo.go:60-80` accumulates **one heap-allocated closure per
  mutation** in an unbounded `[]func()`, shared across every statement of the transaction
  (`cypher/exectx.go:183`). The WAL-backed path has `DefaultMaxTxnOps = 16_000_000`
  (`store/txn/txn.go:99`) but it is enforced "in the commit/append" — after the 16 M closures and
  16 M eager in-memory mutations already exist. The **store-less engine (`NewEngine`) has no
  mutation cap at all**.
- **Evidence:** 200 000 property `SET`s inside one open, uncommitted `ExplicitTx` over a 200 k-node
  graph, `runtime.ReadMemStats` after `runtime.GC()`: heap grew **45 320 864 B = 226.6 B per
  mutation** (undo closures + index buffer + count buffer; upper bound — Memgraph's 56 B is the
  Delta object alone). Rollback returned the heap below baseline, so there is no leak — only a
  high-water mark.
- **Lever:** two, both small.
  (i) An engine-level mutation cap mirroring `maxTxnOps`, checked in `undoLog.record` as mutations
  are *recorded* rather than at commit, with a typed `ErrTransactionTooLarge`. This restores the
  bounded-resource mandate on the store-less path and makes the WAL path fail fast instead of after
  the memory is spent.
  (ii) Replace the closure-per-mutation with a typed undo record (a small struct + a switch,
  Memgraph's `Delta` shape). One closure is ~2–3 allocations and defeats slice-of-struct locality;
  a typed record collapses it to one flat append. Also makes (i) trivially accountable in bytes.
  Note F5 supersedes both for the explicit-tx path (an overlay discards in O(1) and needs no undo
  log), so sequence (i) now and fold (ii) into F5.
- **TCK/ACID impact:** (i) adds a documented failure before any partial state is durable — atomicity
  preserved, and it removes an OOM path that today can kill the process mid-transaction. (ii) is
  representation-only; the inverse semantics are unchanged, and the existing rollback-fidelity
  tests are the regression gate. No TCK scenario approaches these sizes.
- **Effort:** S for (i), M for (ii).

### F10. Single-writer ceiling measured; concurrent disjoint writes cannot be admitted incrementally [CONFIRMED-R1] (severity: MEDIUM — ruling, not a defect)

- **What they do:** Neo4j takes write locks at NODE and RELATIONSHIP granularity, so disjoint writes
  proceed concurrently; dense nodes avoid exclusive locks entirely — "relationship modifications
  acquire coarse-grained shared node locks when doing the operation in the transaction, and then
  acquire precise exclusive locks during the commit"
  (https://neo4j.com/docs/operations-manual/current/database-internals/concurrent-data-access/).
  Memgraph: MVCC + "lock-free, concurrent skip list structures" + node/relationship-level locking →
  "highly concurrent writes".
- **What GoGraph does:** one writer at a time, always — the capacity-1 semaphore
  (`store/txn/txn.go:425`) or `Engine.writeMu` (`cypher/api.go:920`).
- **Evidence:** `CREATE (:W {id:$i})` with every writer touching a **disjoint** node —

  | goroutines | 1 | 2 | 4 | 8 | 10 |
  |---|---|---|---|---|---|
  | ns/op (RunParallel) | 7944 | 8633 | 9415 | 9085 | **10187** |
  | serial loop, same GOMAXPROCS | 9449 | 7311 | 6938 | 7021 | 6951 |

  126 k writes/s at one goroutine → 98 k/s at ten: a **22 % regression** from added concurrency,
  with zero logical conflict. In-memory, no WAL.
- **Fairness / tails (positive):** the store's single-writer lock is a capacity-1 **channel**, and
  Go's `hchan` send queue is FIFO, so writer admission is first-come-first-served — no barging, no
  starvation, bounded queueing delay. `visMu` is a write-preferring `sync.RWMutex`, so a queued
  writer stops admitting new readers and cannot be starved by a reader stream. `Engine.writeMu`
  (`sync.Mutex`) enters starvation mode after 1 ms. Fairness is genuinely good; it is *throughput*
  that is capped.
- **Ruling on "could disjoint writes be admitted without breaking ACID?":** **No, not
  incrementally.** Three things must change together. (1) Write-write conflict detection at object
  granularity — Neo4j pays for it with Forseti + Dreadlocks + a secondary cycle verifier + a
  client-visible `Neo.TransientError.Transaction.DeadlockDetected` retry contract; Memgraph pays
  with `TransactionSerializationException` ("Cannot resolve conflicting transactions. Retry this
  transaction…", `src/query/exceptions.hpp:300-308`). GoGraph's retry-free contract is a real
  advantage and would be forfeited. (2) Per-transaction visibility versioning — two concurrent
  writers cannot each "flip atomically" behind one global RWMutex; that is MVCC, which round 1
  rejected on Fekete-2005 grounds that still hold (Memgraph's own docs concede `G2_item` is
  unpreventable). (3) The UNIQUE constraint registry, the commit-time NOT NULL check
  (`cypher/exectx.go:533`) and the relationship count store (`cypher/exectx.go:563`) all assume
  serialised commits and would each need optimistic validation with abort — Memgraph is explicit
  that "In constraint checking, Memgraph takes the optimistic approach."
  The correct next increment remains **#2051 pinned-reader snapshot** (already designed): it removes
  reader/writer exclusion while keeping the single writer, the retry-free contract and deadlock
  freedom intact.
- **Effort:** n/a (ruling).

### F11. Isolation documentation contradicts the code in three places [NEW] (severity: LOW)

- `docs/isolation-design.md:56-58` ("acquires neither … the visibility barrier") vs `:60`
  ("per-statement `Graph.View` RLock") — mutually contradictory within one paragraph; see F3.
- `cypher/exectx.go:74-76` — same false claim, plus "the count fast path is likewise
  barrier-free", which is not true of any path reachable through `Engine.Run` (`cypher/api.go:1900`
  takes `View` unconditionally, before any plan-shape dispatch).
- `docs/isolation-design.md:12-13` — "a transaction's ops are applied to the live graph **one at a
  time** (`store/txn/txn.go` `Tx.Commit` apply loop)". Stale: `store/txn/txn.go:1176-1186` applies
  the whole op list inside one `ApplyAtomically`. The sentence sits under "The gap (what we are
  fixing)", so a reader cannot tell it is historical.
- **Lever:** correct all three; add one sentence to `docs/isolation-design.md` stating plainly that
  a read-only transaction is blocked for the lifetime of any open write transaction, with the
  measured figure from F3.
- **TCK/ACID impact:** none (documentation). Required by CLAUDE.md "Documentation must be accurate
  and faithful to the code — never document intent, only what is actually implemented."
- **Effort:** S.

---

## The mandate question: is an opt-in analytical mode compatible with GoGraph's ACID mandate?

**Recommendation: reject it. Do not build an analytical storage mode.** Not "defer" — reject, and
record the reasoning so it is not re-litigated. Two independent grounds, plus what *is* worth taking.

**Ground 1 — it is incompatible with the mandate as written, and "opt-in" does not rescue it.**
Memgraph's own documentation of what the mode gives up is unambiguous
(https://memgraph.com/docs/fundamentals/storage-memory-usage): "In the analytical storage mode,
there are no ACID guarantees and other transactions can see the changes of ongoing transactions…
the transactions can be committed in random orders, and the updates to the data, in the end, might
not be correct." Per property: "Atomicity - if your write transaction fails for any reason, the
changes are not rolled back"; "Consistency - database consistency is not guaranteed"; "Isolation -
other transactions can see the changes of ongoing transactions even if changes are not committed";
"Durability - Memgraph does not create WAL files and periodic snapshots". Plus: "deleting same part
of the graph from parallel transaction will lead to undefined behavior" and it "does not support
aborting or rolling back transactions".

CLAUDE.md states the mandate as a property of the module, not of a configuration: "The module
guarantees the ACID transactional properties… across every feature" and "These properties must be
preserved both for the in-memory engine and for every persistence backend." A flag that disables
atomicity, consistency, isolation and durability converts an unconditional claim into a conditional
one, and every downstream artefact — the README, the release notes, the crash battery's meaning,
this audit series — becomes mode-qualified. Memgraph itself demonstrates the maintenance cost:
three current official pages give **three different answers** to whether snapshot creation in
analytical mode blocks other transactions (storage-memory-usage: "the only transaction present in
the system"; data-durability: "other transactions will be prevented from starting"; storage-access:
"allowing for other (shared access) read queries to run in parallel"). Mode-dependent semantics are
where documentation goes to rot.

**Ground 2 — and this is decisive — GoGraph has nothing to buy back.** Memgraph's analytical mode
exists to *refund the cost of MVCC*. Every documented gain is a delta object not allocated: "By
switching the storage mode to `IN_MEMORY_ANALYTICAL` mode **disables the creation of `Deltas`** thus
drastically speeding up import with lower memory consumption - up to 6 times faster import and less
memory consumption." GoGraph never allocates a delta, because it has no version chains. Concretely:

- **The memory saving does not exist for GoGraph.** Memgraph pays 56 B/delta on top of an 80 B
  vertex / 32 B edge; round 1 measured GoGraph at ~22 B/edge against Memgraph's ≥88 B. GoGraph's
  in-flight cost (F9, 226.6 B/mutation) is transaction-scoped and released at commit, not
  per-object and retained until GC.
- **The read-path saving does not exist either.** Memgraph's analytical reads are faster partly
  because they skip delta-chain traversal. GoGraph's analytics path is already lock-free and
  version-free: the immutable CSR is explicitly outside the barrier
  (`docs/isolation-design.md:238-240`, `:303-305` — "the immutable-CSR analytics path is untouched
  and stays lock-free").
- **The GC hazard analytical mode escapes does not exist.** Memgraph's `CollectGarbage` cannot
  reclaim anything newer than `commit_log_->OldestActive()`, so a single long-running transaction —
  read or write — pins the whole delta horizon. GoGraph has no version retention at all: a reader
  holds an RLock, not a snapshot generation.
- **The remaining gain is bulk import, and both incumbents' real answer there is an offline
  loader**, not a degraded online mode: Neo4j ships `neo4j-admin import`, and GoGraph already has
  the equivalent — the CSR-direct bulk build with single-publisher atomic-rename publish
  (`store/bulk`, `graph/csrfile`). That path is *already* outside the transactional write path and
  *already* ACID-safe (its durability is the tmp→CRC→fsync→rename→parent-fsync publish).

**What is worth taking from the idea (the salvageable 20 %).** The correct reading of Memgraph's
mode is not "ACID is negotiable" but "bulk ingestion must not run through per-transaction
machinery". GoGraph agrees with that and is under-delivering on it:

1. **Wire and document the existing bulk path** as the supported import route. It exists and is
   certified but is not reachable ergonomically from the public API — the single largest practical
   gap versus `IN_MEMORY_ANALYTICAL`, and it costs nothing in ACID because the publish is atomic.
2. **Add batched commit** (backlog #1512 / `BeginBatch`). Today a bulk load is fsync-bound at
   ~251 tx/s because there is no API to amortise one fsync across K logical units. Each batch stays
   a real, all-or-nothing, durable transaction — the mandate is fully preserved, and this recovers
   the bulk of Memgraph's import advantage without conceding anything.
3. **Or expose it as `Engine.RunBatched`** (F8 option b), which is the same idea at the Cypher
   surface.

Stated in one line: **Memgraph's analytical mode is a refund for a cost GoGraph never paid; buying
it would mean paying the mandate instead.**

---

## Nothing-to-take list

| Their decision | Why GoGraph should reject it |
|---|---|
| **MVCC / delta chains** (Memgraph) | Round-1 rejection reinforced by new evidence: Memgraph's own docs publish `G2_item` → "Disallowed by: NONE" and its source concedes "we only implement snapshot isolation", so MVCC would *weaken* GoGraph's write-skew guarantee (F6). It also imports the GC-horizon retention hazard (oldest active tx pins every newer delta) that GoGraph structurally does not have, and 56 B/object of steady-state overhead. |
| **Deadlock detection apparatus** (Neo4j: Forseti + Dreadlocks O(1) wait-set union + secondary cycle verification + `DeadlockDetectedException`) | Deadlock is structurally impossible in GoGraph: a single total lock order (store semaphore → `visMu` → per-shard mutexes → index manager), a single global writer, and `readerMu` provably never held across `visMu` (LIFO defer order in `lpg.go:630-634` and `:529-541`). Importing per-object locking would import the entire detection apparatus *and* Neo4j's documented false-positive rate ("it may also mistakenly identify a deadlock where there is none"). |
| **Client-visible serialization-error retry contract** (both) | Neo4j requires idempotent transaction functions because "a transaction might be re-run"; Memgraph's sample retry loop runs to 100 attempts with exponential backoff. GoGraph's write path never aborts for a concurrency reason. This is the single clearest correctness advantage in the comparison and must not be traded. |
| **Broad planner-inserted `Eager`** (Neo4j, 4 conflict classes, uncapped) | All four classes are correct in GoGraph without it (F7), because the statement materialises inside one barrier window. Importing conflict analysis would add planner complexity for zero behaviour change. |
| **`IN_MEMORY_ANALYTICAL`** (Memgraph) | See the ruling above — a refund for a cost GoGraph never paid. |
| **Selectable `READ UNCOMMITTED`** (Memgraph) | GoGraph cannot express it without exposing eagerly-applied uncommitted writes, i.e. exposing state that a rollback will erase. No use case justifies it in an embeddable library. |
| **Nested transactions / savepoints** | Neither incumbent has them (Neo4j's nested `beginTx` is a placebo joining the outer transaction). Parity; nothing to take. |

---

## Priority

1. **F1** (barrier-entry global-lock, S) — largest measured win, zero semantic risk.
2. **F2** (ctx-aware acquisition, S+M) — correctness of a documented API contract; Bolt `tx_timeout` is currently inert at BEGIN.
3. **F9(i)** + **F8(b)** (mutation cap + batched run, S each) — closes the unbounded-resource hole and delivers the real bulk-import need without touching the mandate.
4. **F5** (transaction-local write overlay, L) — the architectural lever; removes F3 and F4 without MVCC. Sequence after 1–3.
5. **F11** + the missing `BeginReadTx` anomaly tests (S) — documentation accuracy and contract pinning.
6. **#2051** (already designed) remains the end-state for reader/writer exclusion.
