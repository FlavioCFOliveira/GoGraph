# Result-Streaming Feasibility Spike (#1525)

Status: **spike — feasibility verdict, design-only.** No implementation ships
with this document. This is the authoritative record of *why* bounded-memory
streaming of large Cypher result sets cannot be done safely on the current F3
isolation foundation, *what* the precise Isolation hazard is, and *what*
foundation a follow-up must build first.

Audited at HEAD `8f0e785`. Certified by `cypher-expert-consultant` (pipeline
breaker taxonomy) and `storage-engine-auditor` (Isolation foundation).

## The question

The seam audit flagged that a query returning a large result set
**materialises the entire result** before the F3 transaction-visibility barrier
releases (`cypher/api.go`, `Result.materialize`), causing a peak heap
proportional to the result size. Could the engine instead **stream** rows lazily
to the consumer — bounding that memory — while still guaranteeing every streamed
row reflects exactly the snapshot the query began with (Isolation preserved)?

## Verdict

**Design-only. Bounded-memory snapshot-pinned streaming is INFEASIBLE on the
current foundation and must not be forced.** It requires a foundation not yet
present: an immutable, atomically-published graph snapshot (the per-shard
versioned `Snapshot` root behind `atomic.Pointer[Snapshot]` already specified —
but not implemented — in [`isolation-design.md`](isolation-design.md), §"The
Snapshot root", and named there, lines 282–302, as the deferred lock-free
optimisation that "restores streaming"). Until that root exists, releasing the
visibility lock mid-drain to stream to a slow consumer would let a concurrent
commit be observed mid-result — a torn cross-substructure read that violates
ACID Isolation.

This is an explicitly valid spike outcome under the task's decision rule: the
safe path depends on a snapshot pin lifecycle beyond the current barrier, so we
record the verdict and a scoped follow-up rather than ship an unsafe change.

## Current memory behaviour (re-assessed against HEAD)

Since the original audit flag, `#1499` (column-oriented/SoA result rows),
`#1500` (lazy node materialisation), and `#1502` ran. Current behaviour, read
from source:

1. **Go API read path (`Engine.Run`, `cypher/api.go` ~1240–1344).** The whole
   query — plan build, `exec.Run`, and `Result.materialize()` (line 1338) — runs
   inside a *single* `e.g.View(...)` closure (opens at line 1309), i.e. under one
   acquisition of the `visMu` read lock. `materialize()` (~line 2616) drains the
   entire `ResultSet` into a flat column-oriented backing slice (`matRows`),
   *then* the `View` closure returns and releases `visMu.RLock`. The consumer
   iterates afterwards, served from `matRows` while holding no lock.
   - **Peak engine heap is O(result size)** — the whole result is buffered. This
     is the spike's target spike.
   - The `#1499` SoA layout and `#1500` lazy node materialisation reduced the
     *per-row* and *per-value* cost (fewer maps, lazy node hydration) but did
     **not** change the O(result) buffering: every row is still retained before
     the lock releases.

2. **Write path (`Engine.RunInTx`).** Same shape, but the drain runs inside
   `Graph.ApplyAtomically` (the `visMu` *write* lock), with the WAL fsync folded
   in before visibility (`commitUnderBarrier`, #1281). Writes are eager and
   pipeline-breaking by nature; out of scope for read streaming.

3. **Bolt PULL path (`bolt/server/session.go` ~795–862).** Bolt *chunks* RECORD
   messages to the wire and supports `PULL n` with `has_more` — so the **socket
   / wire** memory per PULL is bounded. But it reads each row **positionally
   from the already fully-materialised engine result** (`s.result.ValueAt(i)`,
   comment at line 805: "the engine result is always materialised"). Bolt
   streaming therefore bounds *wire* memory, **not** the engine-side result
   buffer. The O(result) engine heap spike is present on the Bolt path too.

**Net:** the existing row cap (`DefaultMaxResultRows`, 10M) and byte budget
(`ErrResultBytesExceeded`, #1328) are the *only* current bounds on result
memory — and they bound by **rejecting** an oversized result, not by streaming
it. There is no streaming producer anywhere in the result path today.

## Why the barrier cannot simply be released mid-drain

### What provides Isolation today

F3 snapshot isolation is provided **entirely by mutual exclusion on a
`sync.RWMutex` (`visMu`)**, not by an immutable pinned view:

- `Graph.View(fn)` holds `visMu.RLock` for `fn`'s whole duration
  (`graph/lpg/lpg.go:411–417`); `Graph.ApplyAtomically(fn)` holds `visMu.Lock`
  for the whole apply (`lpg.go:319–334`). RLock↔Lock mutual exclusion is the
  whole mechanism — stated as a deliberate, correctness-first choice in the code
  (`lpg.go:285–286`).
- The read-servable structures the executor reads — node/edge labels and
  properties (16-shard RWMutex maps), the tombstone set, the roaring label
  bitmaps, the hash/B-tree secondary indexes — are **still mutated in place**
  under their own per-shard locks. There is **no** `type Snapshot` and **no**
  `atomic.Pointer[Snapshot]` in `graph/` (verified). The `Snapshot` root in
  `isolation-design.md` (§"The Snapshot root") is **design-only**.

Consequently the buffer that `materialize()` produces *is the only stable
snapshot in existence*. The view is consistent **only because** `visMu.RLock` is
held continuously across the entire drain, freezing the single writer out for
that window. The drain-before-release ordering is load-bearing, and the code
says so: "materialising releases the read lock before the caller iterates, so a
long-open Result can never deadlock a writer" (`cypher/api.go:1290–1292`).

### The Isolation hazard if we streamed under the current barrier

Single-writer model; reader R streaming lazily to a slow consumer; writer W
committing transaction T:

1. R opens `View`, emits rows `1..k`, then **releases `visMu.RLock`** and yields
   to the slow consumer (the hypothetical streaming change).
2. W acquires `visMu.Lock` and applies T — mutating the in-place label/property
   shards, tombstone set, and roaring bitmaps. T commits.
3. R resumes and pulls rows `k+1..N`, now reading the **post-T** live structures.

Rows `1..k` reflect pre-T state; rows `k+1..N` reflect post-T state. R has
returned a result for a graph state that **never existed at any instant** — a
partial-transaction / torn cross-substructure read, precisely the anomaly F3 was
built to eliminate. This breaks the ACID mandate ("readers never observe the
partial writes of an in-flight transaction"). The power-checked
`cypher.TestIsolation_Cypher_NoPartialWriteObservable` would be expected to trip.

There is a second, independent reason not to hold the lock *across* the consumer
instead: holding `visMu.RLock` for the consumer's (unbounded, network-paced)
drain duration would let a slow or malicious consumer **starve the single
writer** for arbitrarily long — a liveness / backpressure regression. So neither
"release mid-drain" (breaks Isolation) nor "hold across the consumer" (starves
writers) is acceptable. Full materialisation is the current correct compromise:
the lock is held only for the bounded drain time, decoupled from consumer speed.

## The trade-off, quantified

| Strategy | Writer exclusion | Peak engine heap | Isolation | Status |
|---|---|---|---|---|
| **Today: full materialisation in `View`** | for the **drain** duration only (bounded, consumer-independent) | **O(result size)** | safe (SI) | shipped |
| **Hold barrier across the consumer drain** | for the **consumer** duration (unbounded, network-paced) | O(result size) (no gain) | safe (SI) | rejected — starves the single writer; no memory win |
| **Release barrier + stream lazily (current foundation)** | none after release | **O(1) / bounded** | **BROKEN** — torn cross-substructure read | rejected — violates ACID Isolation |
| **Snapshot-pinned streaming (future `atomic.Pointer[Snapshot]`)** | none (lock-free read) | **O(1) / bounded** | safe (SI) | **requires foundation — see follow-up** |

The only quadrant that is both bounded-memory *and* Isolation-safe is the last
one, and it is gated on the snapshot foundation.

## What *would* be streamable (for the follow-up)

`cypher-expert-consultant` certified the openCypher pipeline-breaker taxonomy
that any future streaming implementation must honour:

- **Must remain fully materialised (eager pipeline breakers):** `ORDER BY`
  (global sort), **all** aggregation — `count` / `sum` / `avg` / `collect` /
  `min` / `max` / `percentileCont` / `percentileDisc` / `stDev`, **grouped or
  not** (group completeness is only known at end-of-input — grouping keys do
  **not** make aggregation streamable), `DISTINCT` (incl. `RETURN DISTINCT`,
  `count(DISTINCT …)`), `ORDER BY … LIMIT` (Top-N), and any planner-inserted
  write-then-read `Eager` boundary.
- **Streamable (row `k` depends only on input rows `1..k`):** plain
  `MATCH … [OPTIONAL MATCH] [WHERE] [UNWIND] … RETURN <non-aggregating
  projection>` with no `ORDER BY`, no `DISTINCT`, no aggregate. For these a
  row-at-a-time lazy producer yields the **same multiset** as full
  materialisation (openCypher results are multisets; order is unspecified
  without `ORDER BY`), so streamed-vs-materialised is observationally identical.
- **Streamable but content differs from materialisation (still conformant):**
  add `SKIP` / `LIMIT` without `ORDER BY` — lazy and correct, but *which* rows
  return is implementation-defined, so do **not** assert multiset equality there.

This taxonomy is what the eventual differential test (streamed vs materialised →
identical multiset) would assert on the streamable shapes, with the eager shapes
explicitly excluded from streaming.

## Scoped follow-up

Streaming is downstream of the snapshot foundation. The concrete, ordered
prerequisites (mapping onto the existing F3 staging in
[`isolation-design.md`](isolation-design.md), table lines 175–181):

1. **F3.2** — `Snapshot` value + `atomic.Pointer[Snapshot]` on `lpg.Graph`;
   adjacency reads served from the pinned snapshot.
2. **F3.3** — node/edge labels, properties, tombstones move into the snapshot
   (immutable per-shard versions; drop the RWMutex reads).
3. **F3.4** — immutable roaring label bitmaps + live hash/B-tree index versions
   folded into the same atomic flip.

   *Every* substructure the executor reads must be reachable only through the
   pinned root; a partial pin (e.g. adjacency-only) still leaks a torn read
   through any unpinned structure. `graph/generation` is **not** a drop-in pin
   (it refcounts a `csr.CSR`, not the LPG substructures), but its
   atomic-publish + refcount-drain pattern is the validated template, and
   `Publisher.PublishWithDrain` is the right primitive if deterministic
   reclamation of a retired snapshot is ever needed.

4. **Streaming executor change (the actual #1525 goal, deferred).** Once the
   root exists: `Engine.Run` pins one `*Snapshot` at query start and threads it
   through every read, replacing the `View`-bracketed drain; for streamable
   result shapes (taxonomy above) it returns a lazy producer over the pinned
   snapshot — bounded memory, no lock held across the consumer, Isolation
   preserved because every row reads the same immutable pin. Eager shapes
   (`ORDER BY` / aggregation / `DISTINCT` / Top-N) stay fully materialised. The
   row cap and byte budget remain as a backstop. Deliverables at that point:
   differential test (streamed vs materialised multiset identity on streamable
   shapes), a memory benchmark proving the spike is bounded, and the full
   validation pipeline + TCK 3897.

**Recommended scoping:** the foundation (F3.2–F3.4 snapshot root) is the
prerequisite work; result streaming is step 4 *after* it. The work should be
opened as "deliver the per-shard `Snapshot` foundation; streaming follows," not
"stream under the current barrier." Sizing each of F3.2/F3.3/F3.4 is itself
non-trivial (each touches ~one substructure family across read and write paths,
under the 3897-scenario TCK gate); the foundation is a multi-task effort, which
is exactly why the original F3 work delivered the correctness-first barrier and
deferred this.

## References

- `cypher/api.go` — `Engine.Run` (1240–1344), `View`-bracketed drain
  (1309–1339), `materialize` (2616), drain-before-release rationale (1290–1292),
  row cap / byte budget (`DefaultMaxResultRows`, `ErrResultBytesExceeded`).
- `graph/lpg/lpg.go` — `visMu` + deliberate-RWMutex comment (285–286),
  `ApplyAtomically` (319–334), `View` (411–417).
- `bolt/server/session.go` — PULL chunking over the materialised result
  (795–862).
- [`isolation-design.md`](isolation-design.md) — F3 mechanism, the design-only
  `Snapshot` root, the materialise trade-off (F3.3, 235–248), and the
  "optimisation restores streaming" statement (282–302).
- Berenson et al., *A Critique of ANSI SQL Isolation Levels*, SIGMOD 1995;
  Fekete et al., *Making Snapshot Isolation Serializable*, ACM TODS 2005;
  Francis et al., *Cypher: An Evolving Query Language for Property Graphs*,
  SIGMOD 2018 (multiset semantics, clause composition, update semantics).
