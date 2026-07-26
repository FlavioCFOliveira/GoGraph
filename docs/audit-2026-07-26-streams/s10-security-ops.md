# Stream 10 — Security, operations, observability and administration

Baseline: `6f31f61` (v0.10.0). Hardware: Apple M-series (darwin/arm64), Go 1.26.5.
All measurements reproducible from `/Users/flaviocfo/dev/xumiga/GoGraph/bench/s10probe/`
(`go test -tags=s10probe -v ./bench/s10probe/`; the files are build-tag gated and
excluded from the default build — `go build ./...` is clean).

## Verdict summary

GoGraph's **network surface is genuinely well built** — secure-by-default auth, bounded
handshake/idle/statement/transaction timeouts, panic boundaries on both per-connection
goroutines, a bounded inbound-decode budget, anchored `=~`, `O_NOFOLLOW`, `0o600`/`0o750`
file modes, `govulncheck` clean on Go 1.26.5 with 9 direct dependencies. Every prior-round
fix I re-tested still holds, and my own prior INFO (unauthenticated COMMIT/ROLLBACK) is
fixed at `2a05cf9`. **The brief's premise that the 22.5 s triangle query is an
ungoverned DoS vector is wrong and I overturn it**: a `context` deadline interrupts that
query with a **56 µs** overrun. The real exposure is elsewhere and it is worse: the single
global visibility barrier. One authenticated Bolt client that sends `BEGIN` and then goes
silent **stalls every reader on every connection for the full 30 s default transaction
timeout**, and `Engine.BeginTx(ctx)` blocks on that barrier **ignoring its context
deadline entirely**, returning `err=nil` after a 232× overrun. The single most valuable
lever is Neo4j's `db.lock.acquisition.timeout` (Community, `GraphDatabaseSettings.java:495`):
a bounded, context-aware barrier acquisition — cheap, ACID-neutral, TCK-neutral.

## Feature-by-feature comparison

| Feature | GoGraph (`file:line`) | Neo4j | Memgraph | Verdict | Label |
|---|---|---|---|---|---|
| Query cancellation latency | ctx polled per-`Next`; **56 µs** overrun measured | `db.transaction.timeout` (`GraphDatabaseSettings.java:487`, default 0=off) | `--query-execution-timeout-sec` default **600** | **BETTER** (on by default at 30 s, and precise) | NEW |
| Lock/barrier acquisition timeout | **none** — `lpg.go:565` `LockBarrier()` takes no ctx | `db.lock.acquisition.timeout` (`:495`, Community) | MVCC — no global barrier to wait on | **WORSE** than both | NEW |
| Reader/writer interference | global `visMu`; writer blocks all readers (`lpg.go:629`) | per-`(resourceType,resourceId)` Forseti lock maps (`ForsetiClient.java:81-89`) | snapshot isolation / MVCC (docs) | **WORSE** than both | NEW |
| Tx introspection | **none** | `SHOW TRANSACTIONS` (Community: `ShowTransactionsCommand.scala`) | `SHOW TRANSACTIONS` (7 cols incl. `query`, `elapsed_ms`) | **WORSE** than both | NEW |
| Tx termination | **none** (only the internal reaper) | `TERMINATE TRANSACTIONS` (Community) | `TERMINATE TRANSACTIONS "tid"` | **WORSE** than both | NEW |
| Per-tx memory limit | result-bytes only (`api.go:576,600`) | `db.memory.transaction.max` (`:1064`) + `dbms.memory.transaction.total.max` (`:1038`, 70% heap) | `--memory-limit` | **WORSE** than both | NEW |
| Query log | none (errors only, query text never logged) | Enterprise-only (`GraphDatabaseSettings.java:810`) | `--audit-enabled` | PARITY w/ Neo4j **Community**; WORSE than Memgraph | NEW |
| Metrics | 448 distinct names, zero-dep Prometheus text | JMX + Prometheus (Enterprise) | monitoring server | **BETTER** in reach, **WORSE** in shape (no gauges/labels; 13 bolt, 0 query) | NEW (revises R1) |
| Tracing | **none** (no OTel) | OTel supported | — | WORSE than Neo4j | NEW |
| Auth | secure-by-default; nil `Auth` → `ErrNoAuthHandler`; loud WARN on `NoAuthHandler`/nil TLS | native + LDAP/SSO (Ent.) | auth modules | PARITY (host-delegated is right for a library) | CONFIRMED-R1 |
| RBAC / users / roles | none | Enterprise | Enterprise | **N/A** — meaningless in-process | CONFIRMED-R1 |
| Multi-database | one `Engine` per `Server`; `db` field ignored (`route.go:30`) | multi-DB + composite (Ent.) | multi-tenancy (Ent.) | N/A-to-LOW | NEW |
| Encryption at rest | absent | Enterprise | Enterprise | N/A (host/FS concern) | NEW |
| Supply chain | 9 direct deps, `govulncheck` clean, Go 1.26.5 | large JVM tree | large C++ tree | **BETTER** | NEW |
| Value-depth DoS | capped; verified rejected at 200/5 000/100 000 | — | — | HOLDS | CONFIRMED-R1 |
| Per-IP rate limiting | none (global `MaxConnections`=1024) | connector limits | — | WORSE (LOW) | NEW |

## Findings

### F1. Abandoned Bolt write transaction stalls every reader for the whole transaction timeout, repeatable indefinitely  [NEW]  (severity: HIGH)

- **What they do:** Memgraph uses snapshot isolation/MVCC, so "all reads made in a
  transaction will see a consistent snapshot" and never wait on a writer
  (<https://memgraph.com/docs/fundamentals/transactions>). Neo4j takes locks per
  `(resourceType, resourceId)` in sharded lock maps — `ForsetiClient.java:81-89`
  ("resourceType -> lock map … Array[ resourceType -> Map( resourceId -> num locks ) ]") —
  so an idle write transaction blocks only the specific entities it touched.
- **What GoGraph does:** `BEGIN` defaults to a **write** transaction when the client sends
  no `mode` (`bolt/server/session.go:1205` `mode := "w"`) → `newTx` (`bolt/server/tx.go:87`)
  → `Engine.BeginTx` → `tx.eng.g.LockBarrier()` (`cypher/exectx.go:304`), which takes
  `visMu.Lock()` (`graph/lpg/lpg.go:565`) and **holds it for the whole transaction**. Every
  read goes through `Graph.View` → `visMu.RLock()` (`graph/lpg/lpg.go:629`).
- **Evidence (measured, `bolt_dos_probe_test.go`):** default `Options{}`, 1 000-node graph,
  attacker opens a write tx over Bolt and sends nothing further.
  ```
  BASELINE read (no attacker):                        4.738ms  err=<nil>
  ATTACKER: write tx opened, now going silent
  VICTIM read #0 issued t+0s      -> took 30.001s  err=Neo.ClientError.Transaction.TransactionTimedOut
  VICTIM read #1 issued t+30.203s -> took      1ms  err=<nil>
  RECOVERED after 30.81s
  ```
  Not merely slow — the victim's read **hard-fails**. Attacker cost: one authenticated
  connection, two messages, zero bytes thereafter. Nothing throttles re-opening (no
  rate-limiting anywhere in `bolt/`), so the cycle repeats forever.
  Mitigation measured (`wire_probe_test.go`): tightening `DefaultTxTimeout` to **1 s**
  converts the outage into a latency spike — `ok=13 failed=0 worstLatency=1s` versus a
  ~1 ms baseline, still **1000×**. The lever exists but only bounds the damage, and it
  breaks legitimate long write transactions.
- **Corollary (reasoned from code, not measured):** `s.txDeadline` is computed *after*
  `newTx` returns (`session.go:1245`), while the engine `txCtx` deadline is computed
  *before* the barrier wait (`tx.go:76-78`). Under contention the reaper window restarts
  after the wait, so the barrier can be held for up to `barrier-wait + tx-timeout` while
  every statement the client issues already fails on the expired engine context.
- **Impact:** Total read unavailability of the graph, in 30 s blocks, from a single
  low-cost connection. Authentication is the only barrier — and since GoGraph has no RBAC
  (correctly, see Nothing-to-take), *every* credential holder, including a nominally
  read-only user, can do this.
- **Lever:** (a) reduce `DefaultTxTimeout` and add a distinct, much shorter
  `MaxTxIdleTime` reaped on inactivity rather than total duration — an abandoned tx is
  detectable by "no message since BEGIN"; (b) count concurrently-open explicit
  transactions per principal/connection and refuse beyond a bound; (c) strategically,
  the round-1 T1(3) lock-free per-shard snapshot (#1671/#2051) removes the primitive
  entirely — this is its **security** justification, not just a throughput one.
- **TCK/ACID impact:** None. An idle-timeout reap is the existing `#1346` reaper with a
  different clock input; it aborts and rolls back, which is already the modelled outcome.
  Isolation is unchanged: the tx still holds the barrier while it is *active*.
- **Effort:** S for (a)+(b); L for (c).

### F2. `Engine.BeginTx(ctx)` ignores its context deadline on barrier acquisition and returns `err=nil` after a 232× overrun  [NEW]  (severity: HIGH)

- **What they do:** Neo4j ships `db.lock.acquisition.timeout` — "The maximum time interval
  within which lock should be acquired" (`GraphDatabaseSettings.java:493-498`, default
  `Duration.ZERO` = disabled, `.dynamic()`), with a dedicated
  `LockAcquisitionTimeoutException` in `community/lock/`. **Community edition.**
- **What GoGraph does:** `BeginTx` has three acquisitions and **two of them are
  context-blind**: `e.lockWriter()` (`cypher/api.go:1009` → `e.writeMu.Lock()`, no ctx) and
  `tx.eng.g.LockBarrier()` (`cypher/exectx.go:304` → `graph/lpg/lpg.go:565`, no ctx). Only
  `e.store.BeginCtx(ctx)` is context-aware — and it is skipped on a store-less engine,
  which is what `cypher.NewEngine` returns. `LockBarrier` is context-blind on **both**
  wirings.
- **Evidence (measured, `barrier_probe_test.go`):**
  ```
  [B] BeginTx(50ms deadline): returned after 11.602596167s err=<nil> deadlineHonoured=false
  ```
  A **232× overrun**, and — the serious part — `err=nil`. The caller is handed a
  successfully-opened transaction 11.6 s after its deadline expired, with no signal that
  the deadline was violated. This confirms and explains stream 2's `601 ms / err=nil`
  observation. It contradicts the method's own godoc, which cites task #1301 and claims
  "The acquire is context-aware: … a caller whose ctx is cancelled or whose deadline
  elapses gets back the context error instead of blocking on the lock for the holder's
  full duration" (`cypher/exectx.go:282-287`) — true only of `store.BeginCtx`. It also
  breaches the CLAUDE.md mandate "Every public API that may block on … a lock … accepts a
  `context.Context` and honours cancellation and deadlines."
- **Impact:** Every caller-imposed protective deadline — including the Bolt server's own
  derived `txCtx` — is silently defeated at exactly the moment it matters (contention).
  An operator cannot bound writer stalls by any configuration available today.
- **Lever:** Add `LockBarrierCtx(ctx) error` on `lpg.Graph` and a ctx-aware `lockWriter`,
  implemented as the standard `chan struct{}`-of-capacity-1 semaphore selected against
  `ctx.Done()`, and have `BeginTx` return the context error. Optionally surface Neo4j's
  knob as `EngineOptions.LockAcquisitionTimeout`. Fix the godoc.
- **TCK/ACID impact:** None. Failing to *acquire* a lock means the transaction never
  starts — no partial state, nothing to roll back. Atomicity/Durability untouched;
  Isolation strengthened (a caller can no longer be handed a tx whose deadline has
  already passed). No Cypher semantics involved, so the TCK is unaffected.
- **Effort:** S.

### F3. One slow read plus one writer collapses concurrent reads into a total stall — 12 000× amplification  [NEW]  (severity: HIGH)

- **What they do:** as F1 — Memgraph MVCC snapshots, Neo4j per-entity locks. In neither can
  a single read query plus a single writer stop all other reads.
- **What GoGraph does:** `Engine.Run` executes the **entire query**, not just the plan
  build, inside `e.g.View(...)` — the closure opened at `cypher/api.go:1900` ends after
  `r.materialize()` at `cypher/api.go:1977`. So a long read holds `visMu.RLock` for its
  whole lifetime. Go's `sync.RWMutex` is writer-preferring, so the moment one writer queues
  on `Lock()`, every *subsequent* `RLock()` also blocks. (The in-code comment at
  `api.go:1899` says "build runs under visMu.RLock" — materially understating the scope.)
- **Evidence (measured, `dos_probe_test.go` + `barrier_probe_test.go`), 20 000 nodes /
  200 000 edges, the brief's triangle query:**
  - Control, **no writer**: trivial `count` issued during the 11.9 s triangle → **938 µs**.
    Readers genuinely do not block readers.
  - Test, **one writer queued**: trivial `count` issued at t+600 ms/800 ms/1 000 ms →
    **11.30 s / 11.10 s / 10.90 s**.
  - Amplification: 938 µs → 11.3 s = **≈12 000×**, from adding one writer.
- **Impact:** The blast radius of any slow query is multiplied by the presence of a single
  concurrent writer, turning a one-query slowdown into a whole-database outage. It also
  makes the F1 primitive available *without* an explicit transaction.
- **Lever:** This is the security case for round-1 **T1(3), lock-free per-shard snapshot
  (#1671/#2051)** — currently justified only on throughput. Interim, much cheaper
  mitigation: narrow the `View` scope so only plan build and snapshot capture are under
  `RLock` and iteration runs against the captured snapshot; where that is not yet possible,
  document that `MaxStatementTimeout` is the *only* bound on a reader's barrier hold and
  should be set aggressively on any multi-tenant deployment.
- **TCK/ACID impact:** Narrowing `View` must preserve the read-committed contract the
  engine documents (`cypher/exectx.go:215-225`). A per-shard immutable snapshot is
  *stronger* (snapshot-consistent) than what is promised, so no committed-state invariant
  is weakened and no TCK scenario observes the difference — the openCypher TCK does not
  specify cross-statement isolation.
- **Effort:** L (it is #1671/#2051); S for the documentation/`MaxStatementTimeout` interim.

### F4. No transaction or query introspection, and no way to terminate one  [NEW]  (severity: MEDIUM)

- **What they do:** Neo4j `SHOW TRANSACTIONS` and `TERMINATE TRANSACTIONS` are
  **Community** features — `ShowTransactionsCommand.scala` and
  `TerminateTransactionsCommand.scala` both live under
  `community/cypher/interpreted-runtime/`. Memgraph's `SHOW TRANSACTIONS` returns
  `username, transaction_id, query, status, metadata, start_time, elapsed_ms`, supports
  `SHOW RUNNING TRANSACTIONS`, gates cross-user visibility behind a
  `TRANSACTION_MANAGEMENT` privilege, and `TERMINATE TRANSACTIONS "tid1", …` signals the
  executing thread to stop "and the whole system will stay in a consistent state"
  (<https://memgraph.com/docs/fundamentals/transactions>).
- **What GoGraph does:** nothing. The procedure registry (`cypher/procs/builtin_db.go`)
  exposes only `db.labels()`, `db.relationshipTypes()`, `db.propertyKeys()`. Grepping the
  non-test tree for `listQueries|SHOW TRANSACTIONS|TERMINATE|QueryLog` returns zero hits.
- **Evidence:** During the F1 outage the operator's total signal is one counter increment
  (`bolt.server.tx.timedout`, `bolt/server/metrics.go:84`) and one log line —
  `WARN bolt: explicit transaction timed out; rolled back to release the writer lock
  remote=127.0.0.1:64253` — with no session id, no principal, no query, no elapsed time,
  and it arrives only *after* the 30 s damage is done.
- **Impact:** An operator experiencing F1 or F3 cannot determine which transaction is
  blocking, who owns it, or end it. Recovery is "restart the host process".
- **Applicability note:** this is *not* a server-only feature. An embeddable library must
  give the **host application** the primitives to build its own admin surface; today the
  host cannot enumerate its own in-flight transactions.
- **Lever:** Register open transactions in an `Engine`-scoped table (id, principal from the
  Bolt session, statement text, start time, state) and expose `Engine.ListTransactions()`
  and `Engine.TerminateTransaction(id) error` (cancel the tx context — which requires F2's
  fix to be effective). Surface them over Bolt as `db.*` procedures rather than new Cypher
  syntax.
- **TCK/ACID impact:** Procedures under the `db.` namespace add no Cypher grammar and no
  TCK-covered semantics (the TCK does not cover `SHOW TRANSACTIONS`). Termination = abort +
  rollback, an outcome the ACID model already defines.
- **Effort:** M.

### F5. Observability inversion: rich metrics everywhere except the surface that fails  [NEW, revises R1]  (severity: MEDIUM)

- **What GoGraph does:** the metric inventory is **larger** than round 1 credited — 448
  distinct names / 282 base symbols emitted from 699 non-test call sites (measured by
  extracting the literal first argument of `IncCounter(`/`Time(`), versus the 162 credited
  and the 151 rows in `docs/metrics.md`. **[STALE-R1]** on the count. But the distribution
  is inverted: `search`=199, `store`=179, `graph`=33, `cypher`=32, **`bolt`=13**, and
  **zero** per-query series (no query latency, no rows returned, no queries-in-flight).
  The `Backend` interface is `IncCounter(name, uint64)` + `ObserveLatency(name, Duration)`
  only (`internal/metrics/metrics.go`) — **no gauges, no labels**. The Bolt package works
  around this thoughtfully with paired counters (`bolt/server/metrics.go:12-22`,
  accepted/closed and opened/closed as derivable gauges) — a good design under the
  constraint, but it cannot express "current heap held by transactions" or
  "blocked-acquire count", which CLAUDE.md's observability section mandates. No
  OpenTelemetry (the only `otel` mentions in the tree are two doc comments).
- **Evidence:** `grep -rhoE '(IncCounter|Time)\("[a-zA-Z0-9_.]+"'` non-test, excluding
  `examples/`: 448 unique; `cut -d. -f1 | uniq -c` gives the distribution above.
- **Impact:** Compounds F4. The three highest-risk behaviours in this report — barrier wait
  time, in-flight query duration, open-transaction age — are precisely the ones with no
  metric.
- **Lever:** (1) add `ObserveGauge(name string, v float64)` to `Backend` (additive; the
  no-op and Prometheus backends absorb it); (2) add three series that would have made F1/F3
  self-diagnosing: `cypher.barrier.waitSeconds` (histogram), `cypher.query.inflight`
  (gauge), `bolt.server.statement.durationSeconds` (histogram — this is round-1 T2(9),
  still unshipped, and I am reinforcing it on operability grounds).
- **TCK/ACID impact:** None; observability only.
- **Effort:** S.

### F6. Aggregate inbound-decode ceiling is default-off without `GOMEMLIMIT`  [CONFIRMED-R1]  (severity: LOW-MEDIUM)

- **What GoGraph does:** `resolveMaxInboundDecodeBytes` (`bolt/server/serve.go`) returns
  `0` (disabled) unless `Options.MaxInboundDecodeBytes` is set or a Go soft memory limit is
  installed. Per-connection decode stays hard-bounded, but aggregate pre-auth decode across
  `MaxConnections`=1024 is unbounded without `GOMEMLIMIT`.
- **Evidence:** code unchanged since my 2026-07-02 assessment; the conscious trade-off is
  documented in the `Options.MaxInboundDecodeBytes` godoc (`serve.go:191-220`).
- **Lever:** none new — but given F1/F3, the deployment guidance ("set `GOMEMLIMIT` or an
  explicit `MaxInboundDecodeBytes` before exposing Bolt") deserves promotion from a godoc
  paragraph to `SECURITY.md`/`docs/bolt.md`.
- **Effort:** S (documentation).

### F7. No per-IP or per-principal connection/transaction rate limiting  [NEW]  (severity: LOW)

- **What GoGraph does:** the only admission control is the global `MaxConnections`
  semaphore (default 1024, `serve.go:27`). No per-IP cap, no accept-rate limit, no
  per-principal transaction cap. `grep -rn 'rate\.|RateLimit|perIP|throttle' bolt/` (non-test)
  is empty.
- **Impact:** Low on its own — the handshake (10 s) and idle (30 s) deadlines already
  defeat classic slowloris, and `MaxConnections` bounds goroutines. It matters mainly as
  the missing brake on F1's repeatability.
- **Lever:** a per-remote-IP concurrent-connection cap and a per-principal open-transaction
  cap, both as `Options` fields defaulting to off.
- **TCK/ACID impact:** none.
- **Effort:** S.

### F8. No per-query or per-transaction memory limit  [NEW]  (severity: LOW)

- **What they do:** Neo4j `db.memory.transaction.max` (`GraphDatabaseSettings.java:1064`,
  0 = unlimited) and `dbms.memory.transaction.total.max` (`:1038`, "Defaults to 70% of the
  heap size limit") — both Community, both `.dynamic()`. Memgraph `--memory-limit`
  (default 0 → 90-100 % of physical RAM).
- **What GoGraph does:** bounds only *result* size — `MaxResultRows`, `MaxResultBytes`
  (`cypher/api.go:576`), `GlobalMaxResultBytes` (`:600`) — plus targeted caps
  (`MaxCollectItems`, the distinct-aggregator cap, `regexCacheCapacity`=1024 with FIFO
  eviction, `expr.MaxValueDepth`=1000). Intermediate execution state (hash-join build side,
  eager materialisation, sort buffers) is not charged against any transaction-wide budget.
- **Impact:** Low today because prior rounds capped the individual amplifiers one by one;
  the residual is that each new operator must remember to add its own cap, with no
  backstop. Neo4j's design puts the backstop at the transaction, so an uncapped operator
  cannot escape it.
- **Lever:** a transaction-scoped heap accounting handle threaded through the operator
  tree, charged on allocation and checked against `EngineOptions.MaxTransactionBytes` /
  `GlobalMaxTransactionBytes`, mirroring the existing `GlobalMaxResultBytes` plumbing.
- **TCK/ACID impact:** must surface as a typed error, not a panic; TCK scenarios are small
  and would never approach any sane default (or ship it default-off, as Neo4j does).
- **Effort:** M.

## Verified holding — no finding

Re-tested at `6f31f61`, all sound:

- **Query cancellation (the brief's premise, overturned).** 20 000 nodes / 200 000 edges,
  triangle query. Baseline **11.57 s**. With a 500 ms deadline: returns at **500.056 ms**
  (overrun **56.5 µs**) with `context deadline exceeded`. With 2 s: **2.000080 s**
  (overrun 80 µs). The default `DefaultStatementTimeout` = 30 s (`serve.go:89`) applies
  **unconditionally** to autocommit statements. The slow triangle query is a *performance*
  problem (stream 2/round 2's territory), **not** an availability one.
- **Value-depth cap** (`182c6f4`, `cypher/expr/depth.go:39` `MaxValueDepth`=1000; PackStream
  wire cap 128). Measured over Bolt: depth 100 accepted; 200, 5 000 and 100 000 rejected
  with `Neo.ClientError.Request.Invalid (malformed Bolt message)`; server alive and serving
  afterwards. No fatal stack overflow (which `recover()` could not catch).
- **`COMMIT`/`ROLLBACK` now authenticated** (`2a05cf9`, gates at `session.go:1267` and
  `:1311`) — my own prior INFO finding, closed.
- **Unanchored-regex authz fix**: `anchorRegexMatch` emits `\A(?:…)\z`, with the
  non-capturing group correctly binding top-level alternation (`cypher/expr/regexcache.go`).
  Cache bounded at 1024 with FIFO eviction. Go's RE2 engine is linear-time, so classic
  catastrophic-backtracking ReDoS is structurally impossible.
- **`tx_timeout` overflow** guarded (`session.go:1188-1191`, explicitly handling `1<<62` and
  `math.MaxInt64`).
- **`O_NOFOLLOW`** present on both WAL (`store/wal/nofollow_unix.go`) and snapshot
  (`store/snapshot/safe_open_unix.go`) read paths.
- **Data at rest**: files `0o600`, directories `0o750` — correct by default. No
  encryption at rest, which is the right call for an embeddable library (the host chooses
  FDE/LUKS/FileVault); adding it would create a key-management obligation GoGraph has no
  business owning.
- **Panic boundaries** on both per-connection goroutines (`serve.go:668`, `:812`), recover-
  log-terminate only, matching the CLAUDE.md exception exactly.
- **Supply chain**: `govulncheck ./...` → *"No vulnerabilities found"*, exit 0. Go 1.26.5,
  9 direct dependencies, 11 indirect — a small, auditable graph. **BETTER** than either
  incumbent by a wide margin.

## Nothing-to-take list

- **RBAC, users, roles, privileges, `SHOW USERS/ROLES/PRIVILEGES`.** GoGraph runs
  **in-process** inside a host that already has full memory access to the graph. An
  access-control layer the host can bypass with a struct-field read is theatre. The right
  boundary is the host's own authorization, and `bolt/server/auth.go` correctly delegates
  credential validation to a host-supplied `AuthHandler`. **Reject.** *(One caveat: the
  absence of RBAC is what makes F1 available to every credential holder, which is an
  argument for fixing F1 — not for adding RBAC.)*
- **Property-/label-level fine-grained security** (Neo4j Enterprise, Memgraph Enterprise).
  Same reasoning, more so. **Reject.**
- **Multi-database / composite databases.** A host wanting several graphs instantiates
  several `Engine`s — cheaper and more flexible than an in-database namespace. The only
  thing genuinely missing is a *process-wide* resource governor across engines, which is
  F8's concern, not multi-database's. **Reject the feature, take the governor.**
- **`neo4j-admin`-style CLI.** A library has no daemon to administer; `store.DB` already
  exposes checkpoint/snapshot programmatically, which is the correct shape. **Reject.**
- **Encryption at rest.** See above. **Reject.**
- **Neo4j's `db.transaction.timeout` / `db.lock.acquisition.timeout` *defaults*.** Both
  default to `Duration.ZERO` — *disabled*. GoGraph's choice of finite defaults everywhere
  (30 s statement, 30 s transaction, 30 s idle, 10 s handshake) is **strictly safer** and
  should not be relaxed. Take Neo4j's *mechanism* for lock-acquisition timeout (F2), not
  its default.
- **Memgraph's `--query-execution-timeout-sec` default of 600 s.** 20× looser than
  GoGraph's 30 s. **Reject the value, GoGraph is already better.**
