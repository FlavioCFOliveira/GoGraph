# Exhaustive audit: GoGraph vs Neo4j vs Memgraph (round 3)

**Date:** 2026-07-26 · **Baseline:** `6f31f61` (v0.10.0) · **Scope:** every functional area of the module

This is the third comparative evaluation. Round 1 (2026-07-24) audited the whole functional surface
architecturally; round 2 (2026-07-25, [`audit-planner-vs-neo4j-memgraph-2026-07-25.md`](audit-planner-vs-neo4j-memgraph-2026-07-25.md))
audited the planner. Both reached their verdicts by reasoning from source and documentation. Round 3
adds what neither had: **measurement against the running incumbents**.

Eleven parallel specialist streams, each required to label every finding `NEW`, `CONFIRMED-R1` or
`STALE-R1`, to cite `file:line` for GoGraph claims and primary sources for Neo4j/Memgraph claims, and
to prefer a measured number to an argued one.

---

## 1. Verdict

**GoGraph is better than both incumbents at being a correct, durable, embeddable storage engine. It
is substantially worse than both at executing graph queries — which is the core competence of a graph
database.**

Round 1 concluded that GoGraph's *"gaps are BREADTH not core quality."* **The measured evidence
refutes that for query execution.** On an identical dataset, identical Cypher and cross-checked
identical results, GoGraph is 30–60× slower than Neo4j and Memgraph on traversal, 107× slower on
cyclic patterns, and 2 184× slower on bulk loading. Those are not breadth gaps; they are the centre
of the product.

The encouraging half: **not one of those deficits is architectural.** Each traces to a specific,
bounded defect — an index the planner declines to use, an operator that was never written, a
recogniser that is too rigid, a fast path that was built and then never connected to the query
language. Several have fixes already prescribed in GoGraph's own source comments. The architecture is
sound; the execution path has holes in it.

Above all of that sits one finding of a different kind: **the lexer silently discards characters it
does not recognise, so certain invalid queries return confidently wrong answers instead of an error.**
Under the project's own `correct → secure → fast` priority, that outranks every performance number in
this report.

---

## 2. The empirical head-to-head

Full data: [`benchmarks/threeway-2026-07-26.md`](benchmarks/threeway-2026-07-26.md).
Harness: `bench/comparison/threeway_test.go` (build tag `threeway`).

Apple M4 (10 cores, 32 GB). Neo4j 5.26 Community and Memgraph 2.22.0 in Docker/colima; GoGraph
in-process. Identical deterministic dataset, identical `UNWIND` batches, identical Cypher bar two
documented dialect divergences. Row counts were cross-checked across all targets before any timing
was compared.

**20 000 nodes / 199 941 edges:**

| Query shape | GoGraph | Neo4j | Memgraph | Verdict |
|---|---|---|---|---|
| Global label count | **540 µs** | 1.143 ms | 1.217 ms | **GoGraph 2.1× faster** |
| Multi-label conjunction | **16 µs** | 972 µs | 862 µs | **GoGraph 54× faster** |
| Single-node write | **14 µs** | 1.055 ms | 289 µs | **GoGraph 21× faster** |
| Group-by + order + limit | **2.716 ms** | 3.863 ms | 2.855 ms | **GoGraph fastest** |
| Point lookup (int key) | 714 µs | 1.331 ms | 311 µs | 2.3× slower |
| Range scan + count | 4.807 ms | 1.048 ms | 446 µs | 10.8× slower |
| Top-k unindexed | 30.782 ms | 2.824 ms | 5.833 ms | 10.9× slower |
| **1-hop expand** | 8.711 ms | 937 µs | 286 µs | **30× slower** |
| **2-hop DISTINCT** | 14.046 ms | 1.260 ms | 341 µs | **41× slower** |
| **Shortest path ≤6** | 23.797 ms | 1.089 ms | 395 µs | **60× slower** |
| **Triangle count** | 22.506 s | 437 ms | 210 ms | **107× slower** |
| **Bulk load** | **35 m 33 s** | 2.39 s | 0.977 s | **2 184× slower** |

At a tenth of that scale GoGraph wins nearly everything, because it has almost no fixed overhead — no
JVM, no server, no serialisation. That advantage is real and worth protecting. But its per-query cost
then grows with the graph where the incumbents' does not, so the ranking inverts as data grows. **The
crossover is well below the scale at which anyone would choose a graph database.**

Two caveats stated plainly. Neo4j Community excludes the Enterprise pipelined/Morsel runtime, so the
Neo4j column is a floor, not a ceiling. And GoGraph ran in-process against two containerised rivals —
a handicap in GoGraph's favour, which makes the traversal deficits more serious, not less.

---

## 3. Findings ranked by value

Ordered by the project's own decision framework: correctness first, then security, then speed.

### Tier 0 — correctness

**C1. The lexer silently discards unrecognised characters, producing wrong answers.** *(Stream 4;
independently reproduced.)* `CypherLexer.g4:157` routes `ERRCHAR : .` to `channel(HIDDEN)`, so any
character the grammar does not know is dropped and the remainder parses as if it were never typed.

Measured on a graph of 2 `:A` and 3 `:B` nodes:

| Query | Returned | Correct |
|---|---|---|
| `MATCH (n) WHERE n.v != 2 RETURN n` | 1 row — *the row where `v = 2`* | syntax error (or 4 rows) |
| `MATCH (n:!A) RETURN n` | 2 rows — *exactly the `:A` nodes* | syntax error (or the 3 `:B` nodes) |

Both were accepted with **no error**. The `!` is deleted and the query inverts. `:!A` is **valid
Neo4j 5 syntax**, so a query ported from Neo4j returns the precise complement of the intended result,
silently. This is invisible to the TCK, which only runs valid queries. Stream 4 verified the fix is
safe: `!` appears exactly twice in all 220 feature files, both inside string literals.
**Effort S. This should be fixed before anything else in this report.**

### Tier 1 — availability and safety

**A1. Barrier acquisition ignores context deadlines.** *(Streams 2, 8, 10 — three independent
confirmations.)* `lpg.go:565 LockBarrier()` and `cypher/api.go:1009 lockWriter()` take no `ctx` at
all. `Engine.BeginTx(ctx)` with a 50 ms deadline returned after **601 ms** (Stream 2) and, under
sustained load, after **11.60 s — a 232× overrun — with `err=nil`** (Stream 10), handing back a live
transaction that holds both the writer semaphore and the global barrier on an already-expired
context. This contradicts the method's own godoc (`cypher/exectx.go:282-287`) and CLAUDE.md's
context-aware-blocking mandate.

Consequences, both PoC-measured:
- One authenticated Bolt client that sends `BEGIN` and then goes silent **stalls every reader on
  every connection**: baseline 4.7 ms → **30.001 s** plus a hard `TransactionTimedOut`, repeatable
  indefinitely (there is no per-IP or per-principal rate limiting in `bolt/`).
- Because `Engine.Run` executes the entire query inside `View` (`api.go:1900` → `:1977`), one slow
  read plus one writer amplifies a 938 µs read to **11.3 s (~12 000×)** through Go's writer-preferring
  RWMutex. A control run without the writer confirms readers alone are unaffected. This is a new and
  independent justification for the `#1671/#2051` lock-free snapshot work.

**Lever:** Neo4j's `db.lock.acquisition.timeout` (Community, `GraphDatabaseSettings.java:495`) — take
the mechanism, not its default, which is off. Add an idle-based `MaxTxIdleTime` and a per-principal
open-transaction cap. ACID-neutral, TCK-neutral. **Effort S.**

Stream 10 overturned this audit's own working premise here: the 22.5 s triangle query is *not* an
ungoverned DoS vector — a 500 ms deadline returns at 500.056 ms, a **56 µs** overrun, which is better
than either incumbent (Neo4j's `db.transaction.timeout` defaults to off, Memgraph's to 600 s). The
barrier, not the query, is the exposure. Note also that where GoGraph is worse here, it is worse than
**Neo4j Community** — `SHOW TRANSACTIONS` and `TERMINATE TRANSACTIONS` are not Enterprise features, so
the absence of any transaction introspection or termination primitive cannot be waved away as an
Enterprise gap. During the outage above, an operator gets one counter and one log line — no session,
principal, query text or elapsed time, and no way to kill it.

### Tier 2 — the query-execution collapse

These four findings, together, account for essentially all of the measured deficit.

**Q1. Correlated seek keys never reach any index.** *(Stream 6, corroborated empirically.)*
`resolveSeekValue` (`cypher/api.go:9642`) takes the seek value as raw **source text** and resolves
only `$param` and literals. Any key bound by a preceding row — `UNWIND` **or** `WITH` — loses the
index. Even `WITH 'name-1' AS nm MATCH (a:P {name: nm})` plans
`CartesianProduct(Projection, NodeByLabelScan)`. There is no per-row seek operator at all. Measured:
seek path flat in N (1.96 → 1.84 ms as N grows 5k → 20k) versus the correlated path growing
(5.81 → 11.80 ms) — **Θ(rows·N) against Θ(rows)**. This is the standard Cypher bulk-load idiom, and
it is the principal cause of the 2 184× load deficit. **Lever:** a correlated/batched index seek;
Stream 6 recommends the set-at-a-time roaring-OR form over Neo4j's `Apply` shape. **Effort M.**

**Q2. Numeric equality never reaches an index.** *(Stream 6, correcting this auditor's first
diagnosis.)* `projectStringPropValue` (`cypher/index_binding.go:55`) rejects every non-string value,
so Cypher-created hash indexes are string-only and `MATCH (a:P {id: 12345})` full-scans. The stated
reason is sound — openCypher numeric equality is cross-type (`5 = 5.0` is TRUE), so an int64-keyed
index would silently drop float-valued matches. But numeric **range** already works via a float64
btree companion (`cypher/api.go:2394`), so the fix is cheaper than the source comment at
`cypher/api.go:9710` proposes: **do not build a float64 hash index — rewrite numeric `=` into the
closed range `[v, v]` on the companion that already ships.** TCK-safe: the companion's monotone
int64→float64 superset property is already certified and the always-retained residual filter applies
the exact comparator, so `2^53+1 = 2^53.0` stays FALSE despite bucket collision. **Effort S.**

**Q3. The fast bulk loader is walled off from the query language.** *(Streams 1 and 9, independently.)*
`store/bulk` measures **4.18 M edges/s** — it would do the benchmark's edge volume in ~48 ms against
the Cypher path's 35 m 33 s, a **~72 000×** gap. It is unreachable three ways: `grep -rn "bulk\." cypher/ bolt/`
returns nothing, its output is a `csrfile` that no `store.DB` can open, and `bulk.Edge` carries no
labels or properties. **Lever:** an offline store-import mode emitting a self-sufficient snapshot
directory through the existing crash-atomic publish. This fixes the user-visible problem **without
waiting on the planner**. **Effort M.**

**Q4. Bound-destination expansion has no `ExpandInto`.** *(Round-2 finding 3, now measured against
the incumbents.)* The closing edge of a cyclic pattern, with both endpoints already bound, is planned
as a full `Expand` into a synthetic variable plus a `Selection` equating it — Θ(deg) per candidate
instead of one probe:

```
Expand (c)-[:KNOWS]->(__anon_3_to_a)   ← both c and a already bound
└─ Selection
```

This is why triangle counting is 107× slower than Memgraph. **It also reframes round 2's WCOJ
conclusion:** round 2 positioned worst-case-optimal joins as *"the one place GoGraph could be
asymptotically better than both."* That remains theoretically true — both incumbents are
binary-join-only and cannot meet the AGM bound — but the present reality is that GoGraph is two orders
of magnitude *worse* on exactly that shape. **`ExpandInto` should be re-prioritised ahead of the
keystone work it currently sits behind**, and WCOJ treated as the later, speculative step.
**Effort M.**

**Q5 (supporting). The per-row execution tax.** *(Stream 5.)* Row-mode filtered label scan costs
398–511 ns per scanned row; 20 000 × that brackets the measured 8.711 ms 1-hop exactly. The tax:
interface-boxing the NodeID (25.2 % of allocated objects), `clear()`-ing a pooled map and rebinding by
string hash (67 ns/row), re-walking the predicate AST, and boxing the property (34.7 %). The columnar
tier that would avoid this is real and **neither incumbent has one** — but it is wired by rigid IR-spine
pattern matching and reaches **only 5 of 15** ordinary filter/project shapes. Adding a label that every
node already carries turns 8.6 ms / 133 allocs into 40.7 ms / 218 058 allocs. Every parallel path is
likewise gated on a bare `*ir.AllNodesScan`, so any real query with a label is serial.
**Levers:** admit `Selection` below `Expand` in the recogniser (M); partition the label scan as both
incumbents do (M); unbox the aggregate argument (**S — best effort-to-value ratio in the audit**);
positional slot binding to replace `expr.RowContext = map[string]Value`, measured at 29–70× in
isolation (L).

### Tier 3 — concurrency

**N1. A debug guard costs 97–99 % of every read.** *(Streams 2 and 8, independently, agreeing to
within noise.)* `graph/lpg/goid.go:36` calls `runtime.Stack` on every `Graph.View`/`ApplyAtomically`
to detect barrier re-entrancy. `runtime.Stack` takes the Go runtime's process-global `debuglock` once
per stack frame. Measured: `View` = **1.65 µs serial / 3.29 µs at 10 cores with a 64 B allocation**,
against **3.6 ns** for the bare RWMutex pair it guards. Read throughput **halves** from 1 to 10 cores.
The guard makes no isolation decision. **Lever:** build-tag it out of non-race builds.
**Effort S, zero TCK/ACID surface — the cheapest large win in this report.**

**N2. No group commit.** *(Stream 8.)* Engine writes are **flat at 261 op/s from 1 to 1024 writers**
with zero fsync coalescing, because `commitUnderBarrier` fsyncs inside `visMu.Lock`. The store-direct
`Tx.Commit` path — same WAL, fsync outside the lock — reaches **19 567 op/s (85×)**. **Effort L.**

**N3. Concurrent disjoint writers: decline.** A durable commit is 3 830 µs of which the in-memory
apply is **5.29 µs (0.14 %)**. Disjoint writers can only parallelise that 0.14 %, at the cost of a
conflict-detection-and-retry contract on the one path that is currently retry-free and serialisable
by construction. **Recommendation: reject, and take group commit instead.** Likewise **reject**
Memgraph's `IN_MEMORY_ANALYTICAL` mode (Stream 2): it is a refund for the cost of MVCC, and GoGraph
never pays that cost — every documented gain is a delta object GoGraph does not allocate.

### Tier 4 — storage, memory, protocol

**S1. Recovery leaves a 3× speed-up on the table.** *(Stream 1.)* The snapshot-apply block
(`recovery.go:1033-1101`) omits the adjacency commit window that WAL replay already uses 290 lines
below (`recovery.go:1335`), so every CSR edge full-clones a shard slot array. Measured,
output-identical: **629–664 ms → 213–227 ms (3×), 4 306 MiB → 204 MiB allocated (21×)**. ~4 lines.
Today a checkpoint buys only 0.88–0.92× versus full WAL replay — this is why. **Effort S.**

**S2. No space reclamation, ever.** Delete 90 % of a graph and 30 % of disk is freed; `csr.bin` and
`mapper.bin` free 0 % (`nEdges` literally unchanged) while `tombstones.bin` grows. ~7× permanent
amplification, unbounded under churn, with no compaction path. Memgraph reclaims implicitly.
**Effort L.**

**S3. Segmented WAL — round 1's rejection is reversed.** Checkpoint phase 3 holds the **commit lock**
while copying the entire surviving WAL suffix, because there are no segments to unlink. Measured stall
grows with write rate: 1.1 MB → 8.1 ms, 56.5 MB → **30.9 ms** — already larger than the 3.8 ms phase-1
capture the design exists to minimise. Neo4j, PostgreSQL and RocksDB all unlink whole files, O(1).
This is a latency-correctness fix, not a nicety.

**M1. Memory: the round-1 verdict must be split.** *(Stream 3.)* **Edges are decisively best** — 8.71 B/edge
topology, ~7× better than Memgraph's published 154 B and ~2× Neo4j's 34 B record. **Nodes are worse
than both** — measured **378.6–423.0 B/node** against Memgraph 204 and Neo4j block-format 128, because
labels and properties live in `map[NodeID]X` keyed by an already-dense ID. A dense slot column measures
**28× smaller**. The fix is the already-shipped edge-label design applied to nodes.
Also: **`edgePropCols.Compact()` never calls `reshaped()`** (`edge_property_column.go:408`), so FOR
bit-packing never fires on the fused path — **30.5 % waste, one-line fix**. And deletes reclaim
nothing in memory either (500k nodes, 90 % removed → 0.0 % reclaimed).

**P1. GoGraph's Bolt value layer is not Bolt.** *(Stream 9.)* Against the official
`neo4j-go-driver v5.28.4`: **13 hard failures in 37 checks.** Entities are emitted as PackStream maps
where `Node(0x4E)`/`Relationship(0x52)`/`Path(0x50)` structures belong (confirmed by hex capture);
**no `stats` is ever sent**, so `NodesCreated=0` and `ContainsUpdates=false` after a successful CREATE
— plausible-looking wrong data, arguably worse than a type error; and missing `db` metadata makes
`summary.Database().Name()` **panic with a nil dereference** inside the driver (~20 lines to fix,
highest value-per-line in the codebase). Round 1's "zero risk" assessment is **confirmed and
upgraded**: the change touches exactly one production file, `go list -deps ./cypher/tck/... | grep -c bolt`
is **0** so TCK impact is structurally impossible, and the struct form is **19.8 % smaller on the wire
and 28.8 % faster to encode**. Sell it on compatibility, not speed — the 46 µs round-trip tax dwarfs
the 40 ns saving.

---

## 4. Where GoGraph is genuinely ahead — do not take their approach

- **Isolation semantics.** Write skew is *impossible* in GoGraph (measured), while Memgraph's own
  documentation publishes `G2_item` → *"Disallowed by: NONE"*. GoGraph needs `Eager` in exactly one
  place (`cypher/ir/writes.go:233`); Neo4j inserts it broadly for read-write hazards and it is a
  notorious OOM source. Snapshot isolation removes the need — TCK-green without it.
- **Durability by default.** Memgraph loses up to 100 k acknowledged transactions on its defaults.
  GoGraph is durable by default with snapshot checksums and torn-write-versus-corruption
  discrimination that neither rival has.
- **Exact statistics.** The count store gives exact `T(labelA, relType, labelB)`. Neo4j has no value
  histograms at all, only sampled selectivity; Memgraph has a single chi-squared scalar.
- **Columnar execution.** Neither incumbent has a columnar chunk pipeline: Neo4j's morsel is a batch
  of *rows*, and Memgraph's `Cursor::Pull` is one row at a time.
- **Exact algorithms and out-of-core.** Global min-cut, Hungarian assignment and Eulerian circuits are
  absent from both GDS and MAGE. GoGraph is the only one of the three that can run an algorithm on a
  graph larger than RAM (semi-external, ~21 B/vertex, validated at 24 M/60 M nightly).
- **Operational openness.** 448 metric names in a zero-dependency Prometheus registry, un-gated —
  where Neo4j's metrics subsystem and Memgraph's metrics server are both **Enterprise-only**.
- **Query cancellation.** A context deadline interrupts a 22.5 s query with a **56 µs** overrun, on by
  default at 30 s. Neo4j's `db.transaction.timeout` defaults to *off*; Memgraph's to 600 s.
- **Supply chain.** 9 direct dependencies, `govulncheck` clean on Go 1.26.5.
- **Bolt version reach** — 5.0–5.6, where Memgraph tops out at 5.2.

**Explicitly rejected after evidence:** full MVCC; concurrent disjoint writers; `IN_MEMORY_ANALYTICAL`;
vector/ANN indexes (approximate results contradict GoGraph's exactness claim and would need a whole
new durability surface — *downgraded from round 1's T3 to declined*); delta-stepping parallel SSSP
(Stream 7: Δ is untunable by a library — 2^8 spread across graphs with *identical* weight
distributions, published wins are at 96 cores, and GDS's own docs admit non-determinism);
Cypher-over-HTTP; cluster routing.

---

## 5. Corrections to rounds 1 and 2

Nine round-1 conclusions did not survive measurement:

| Round-1 claim | Status |
|---|---|
| *"100 % TCK" is a frozen ~2017 openCypher 9 suite* | **FALSE.** Byte-identical to openCypher **2024.3** (`diff -rq` → 0 files). The TCK itself is frozen; GQL work went into the *grammar*, which GoGraph does not track. |
| *LOAD CSV is TCK-covered* | **FALSE.** Zero feature files, absent from the 2024.3 grammar. Implementing it is TCK-*neutral*, which raises its priority. |
| *GDS has no max-flow* | **FALSE.** `gds.maxFlow.*` shipped in GDS 2.23.0 (2025-11-14). The residual GoGraph advantage is min-cut, Hungarian and Eulerian. |
| *macOS `fsync` ≠ `F_FULLFSYNC`* | **FALSE/stale.** Go's `os.File.Sync` on darwin calls `fcntl(F_FULLFSYNC)` since Go 1.12. macOS gets a full device flush — *stronger* than Linux `fdatasync`. Close the backlog item. |
| *GoGraph has the best read path* | **FALSE.** 97–99 % of `View` is a debug guard; read throughput halves from 1 to 10 cores; `BeginReadTx`, documented as "never blocks", inherits 100 % of an open write transaction's hold. |
| *In-memory model = GoGraph* | **Split.** Edges yes, decisively. Nodes are worse than both. |
| *#1671/#2051 designed but unshipped* | **Implemented and REVERTED** — `docs/count-store-design.md:488` records a 5.4× time and 43× memory regression. |
| *Segmented WAL: rejected* | **Reversed** on new evidence (S3 above). |
| *Statistics shipped in v0.10.0* | **Inert.** Nothing on the query path reads them; they feed only `Explain` display text. `RefreshStatistics` is Go-API-only, unreachable from Bolt. |

Round 2 stands, with one re-prioritisation: `ExpandInto` (sprint 314) is the most acute measured
deficit in the engine and should move ahead of the keystone it currently sits behind.

Two documentation defects found in passing: `bench/ldbc/ic_helpers_test.go:12` documents a
silent-no-op bug in comma-joined inline integer filters that **no longer exists** (all four variants
verified correct), and `docs/cypher.md:947` claims `COLLECT { }` is supported when it does not parse.

---

## 6. Recommended sequence

Ordered by correctness-first, then value per unit of effort. Effort in brackets.

1. **C1** — reject unrecognised characters at the lexer. [S] *Silent wrong answers.*
2. **N1** — build-tag the `runtime.Stack` guard out of non-race builds. [S] *Doubles read scaling.*
3. **Q2** — numeric `=` → degenerate range on the existing float64 companion. [S]
4. **S1** — reuse the adjacency commit window in snapshot recovery. [S] *3× recovery, 21× allocations.*
5. **M1a** — `edgePropCols.Compact()` → call `reshaped()`. [S] *30.5 % edge-property memory.*
6. **P1a** — send `db` metadata. [S] *Stops the official driver panicking.*
7. **A1** — context-aware barrier acquisition. [S–M] *Closes the reader-stall exposure.*
8. **Q3** — Cypher-reachable bulk import. [M] *Fixes the load deficit without touching the planner.*
9. **Q1** — correlated/batched index seek. [M] *The root cause of the load deficit.*
10. **Q4** — `ExpandInto`. [M] *Most of the 107× cyclic gap.*
11. **Q5a/b** — unbox aggregate arguments [S]; admit `Selection` below `Expand` [M].
12. **P1b** — Bolt entity structs + `element_id` + `stats`. [M] *Wire compatibility.*
13. **M1b** — dense node label/property columns. [L] *28× on the node side.*
14. **N2 / S2 / S3** — group commit; space reclamation; WAL segmentation. [L]

Items 1–7 are all effort-S, carry no TCK or ACID surface, and between them address the correctness
defect, double read scaling, triple recovery speed, and stop the reference driver crashing.

---

## 7. Method and evidence

Eleven streams: empirical head-to-head (this auditor), storage/durability, transactions/isolation,
in-memory model, language surface, runtime, indexes/constraints/statistics, algorithms, concurrency,
protocol/drivers/IO, and security/operations. Per-stream reports with full findings, measurements and
citations are retained alongside this document.

Every stream was instructed to adversarially verify its own findings, and several overturned their
own or this audit's initial position — Stream 10 refuted the brief's DoS premise, Stream 6 corrected
this auditor's numeric-index diagnosis, Stream 7 demoted its own BADIOS proposal after measuring 1.0×,
and Stream 3 re-scoped round 1's headline "~22 B/edge" figure. Those reversals are the strongest
evidence that the numbers here were measured rather than assumed.

**Known gaps.** Stream 8's Neo4j/Memgraph comparison is incomplete — its web-search budget was
exhausted, and it correctly asserted nothing from memory rather than guessing. `gograph-bolt` is
missing from the 20 000-node table because GoGraph's own Bolt server timed out loading GoGraph's own
data. Neo4j Enterprise's pipelined runtime was not measured. A per-stream "NOT INVESTIGATED" list is
recorded in each report.
