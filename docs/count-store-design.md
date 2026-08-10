# Exact Relationship Count-Store — Design (F5a)

Sprint 306, task **#2081** (design SPIKE, design-only — no production code changes).
Foundation for **P3** count-store-gated join reordering.

## 0. Executive recommendation (decisive)

Build a **derived, non-durable, engine-owned count-store** that maintains three
new relationship statistics **exactly**, alongside the node-count statistic that
already exists:

| Stat | Meaning | Home | Provenance it can feed |
|---|---|---|---|
| `N(label)` | live nodes carrying `label` | **existing** `graph/index/label.Index` (`nodeIdx`), read via `lpgLabelResolver.ResolveLabelCount` (`cypher/api.go:4405`) | `estExact` (already, #2076) |
| `E(relType)` | live edges of type `relType` | new count-store | `estExact` |
| `D(label, relType, dir)` | edge-endpoints of `relType` in direction `dir` whose *this-end* node carries `label` (a degree-sum) | new count-store | `estExact` |
| `T(labelA, relType, labelB)` | live edges `(:labelA)-[:relType]->(:labelB)` | new count-store | `estExact` |

Maintenance is driven from a transaction-scoped **`CountBuffer`**, an exact
structural twin of `exec.IndexBuffer` (`cypher/exec/index_writeback.go`), flushed
inside `Result.commitUnderBarrier` (`cypher/api.go:4005`) **after** the WAL
fsync succeeds and **while `visMu` is held** — the same durable-then-visible seam
the secondary-index fan-out already uses. The store is **recomputed in O(V+E)
from the recovered graph on reopen** (mirroring `registerRecoveredIndexes`,
`cypher/index_binding.go:663`), so it carries **no on-disk format, no WAL op, no
checkpoint component, and no new fsync** — it cannot diverge from the graph
because it is a pure function of it.

**The one place the ideal (exact **and** O(delta)) is unattainable is a node
label mutation on a high-degree node** — see §3. Exact incremental maintenance
there is inherently Ω(degree(node)). The design honours the non-negotiable
O(delta) / zero-write-regression mandate by keeping label-mutation maintenance
exact within a bounded per-commit fan-out budget and, only for the rare hub
relabel that exceeds it, marking `D`/`T` **dirty** (an in-session, non-durable
degradation that vetoes to today's default plan — never a wrong exact — and
self-heals by recompute). `N` and `E` are never affected and stay exact under
every mutation.

---

## 1. Given: which counts (graph-theory-expert)

The count selection below is **input from the graph-theory-expert specialist and
is taken as decided**; this document justifies its *storage and maintenance*, not
its choice.

- **Per `(label, relType, direction)` degree-sum counts** — `D`.
- **Full `(labelA, relType, labelB)` triple counts** — `T`.
- Stored **sparsely** (a map over *observed* keys only).
- **Deliberate GoGraph-specific choice:** keep **both** end-labels in the triple.
  Neo4j's count store keeps only a single end-label per relationship count
  (`(labelA, relType, *)` and `(*, relType, labelB)` separately). Keeping the
  full `(labelA, relType, labelB)` triple is what makes an **exact first-join
  cardinality** possible — the planner can read the exact number of
  `(:A)-[:R]->(:B)` edges rather than estimate it from two marginals — but it is
  not free. §5 quantifies the cost.

`D` is **not** derivable from `T`, so both are genuinely needed: an edge whose
destination carries *no* label contributes to `D(L(src), rt, OUT)` but to no `T`
cell; an edge to a *multi-labelled* destination contributes to several `T` cells
but to `D(L(src), rt, OUT)` exactly once. `T` requires *both* endpoints to carry
the respective labels; `D` requires only the one end.

---

## 2. Question 1 — Data structure, key encoding, sharding, counters

### 2.1 One id-space for labels and relationship types

Node labels **and** relationship types are interned in the **same registry**,
`g.reg` (`*lpg.LabelRegistry`): `SetNodeLabel` calls `g.reg.Intern(name)`
(`graph/lpg/lpg.go:1459`) and `AddEdgeLabeled` calls `g.reg.Intern(relType)`
(`graph/lpg/lpg.go:988`). The registry is a **lock-free, monotone-append,
copy-on-write** structure (`graph/lpg/lpg.go:87-159`); interned ids are **stable
and never reused**, even after a label/type falls out of use. Therefore a
`uint32` id captured in a count-store key is permanently valid and can be
resolved against the live registry without a lock — the same property the
adjacency `Snapshot` relies on for its shared mapper.

Key encoding uses these `uint32` ids directly (no strings on the hot path):

```
E : relTypeID                              uint32
D : (labelID<<32 | relTypeID)              uint64, in two maps: dirOut, dirIn
T : struct{ a, rt, b uint32 }              a comparable struct map key
```

`E` is a `map[uint32]*atomic.Int64`. `D` is a pair of `map[uint64]*atomic.Int64`
(one per direction). `T` is a `map[triKey]*atomic.Int64` with
`type triKey struct{ a, rt, b uint32 }` (a `comparable` value, no allocation to
hash). Untyped edges (the Go-API `AddEdge` with no reltype — never produced by
the Cypher path, which always types edges) are counted under a reserved
`relTypeID` sentinel the planner never queries.

### 2.2 Atomic counters + sharded maps

Each cell is an `*atomic.Int64`. Counter *mutation on an existing key* and
*counter read* are lock-free atomic ops; only *key insertion* (first observation
of a combo) and *key deletion* (counter reaches zero) take a write lock. Maps are
**sharded** by a hash of the key across `countShards` buckets (reuse the existing
`propMapShards` fan-out constant, 64/256, that `nodeLabelShards` etc. use,
`graph/lpg/lpg.go:306`), each shard guarding its three maps with a `sync.RWMutex`.
This mirrors the sharding discipline the reliability mandates require ("sharded
structures … lock-free read paths") and keeps insertion contention off the hot
counter path.

### 2.3 Bounded growth

Growth is bounded by the number of **currently-observed distinct combos**, not by
history and not by data size. When a decrement drives a cell to zero the key is
**deleted** (exactly as `label.Index.Remove` deletes an emptied bitmap,
`graph/index/label/index.go:103`), so a combo that no longer occurs frees its
slot. Upper bounds: `|E| ≤ |relTypes|`, `|D| ≤ 2·|labels|·|relTypes|`,
`|T| ≤ |labels|²·|relTypes|` — all functions of **schema cardinality**, never of
`|V|` or `|E|`. §5 quantifies the absolute footprint.

---

## 3. Question 2 — Maintenance on the commit change fan-out

### 3.1 Mirror the secondary-index discipline

The secondary-index maintenance is the exact template to follow:

1. Write operators enqueue `index.Change` events into an `exec.IndexBuffer`
   during statement execution (`a.buf.Enqueue(...)` on the `lpgMutatorAdapter`,
   `cypher/api.go:13197+`; node-removal fan-out via `enqueueNodeRemovalChanges`,
   `cypher/index_binding.go:518`; old values captured because `OpSetNodeProperty`
   carries `OldValue`, `graph/index/manager.go:113`).
2. At the transaction boundary the buffer is applied atomically:
   `r.buf.Commit(r.idxMgr)` inside `commitUnderBarrier` on success
   (`cypher/api.go:4040`), **after** `tx.CommitWALOnly()` fsyncs the WAL
   (`cypher/api.go:4029`); discarded via `buf.Rollback()` on failure.

The count-store adds a parallel, sibling buffer:

```
exec.CountBuffer   // structural twin of exec.IndexBuffer
  Enqueue(countDelta)
  Commit(cs *countStore)   // applies accumulated deltas
  Rollback()               // discards — no undo log needed
```

Wired through the same `lpgMutatorAdapter` methods that already carry the index
fan-out, so the write path grows one more buffer, not a new code path:

| Adapter method (`cypher/api.go`) | Count delta | Cost |
|---|---|---|
| `AddNode` (`:13058`) | none for E/D/T (bare node has no edges); `N` via label index | O(labels) for N (existing) |
| `AddEdge` / `AddEdgeH` / labelled create (`:13089`, `:13120`) | `E(rt)+1`; `D(l,rt,OUT)+1 ∀l∈L(src)`; `D(l,rt,IN)+1 ∀l∈L(dst)`; `T(a,rt,b)+1 ∀(a,b)∈L(src)×L(dst)` | **O(\|L(src)\|·\|L(dst)\|)** |
| `SetEdgeLabel` (`:13314`) | edge-delete of old type + edge-create of new type, for that one edge | O(\|L(src)\|·\|L(dst)\|) |
| `RemoveEdge` / `RemoveEdgeByHandle` (`:13154`, `:13175`) | symmetric decrements of the create deltas | O(\|L(src)\|·\|L(dst)\|) |
| `RemoveNode` (`:13225`) | none for E/D/T (DETACH strips incident edges first, each an edge-delete already counted); `N` via label index | O(labels) for N |
| `SetNodeLabel` (`:13189`) | **the hazard** — see §3.3 | O(deg(node)) |
| `RemoveNodeLabel` (`:13207`) | **the hazard** — symmetric | O(deg(node)) |

Endpoint labels are available at delta time: the adapter holds `a.g`
(`*lpg.Graph`) and reads `a.g.NodeLabels(...)` (`cypher/api.go:13304`), which sees
the transaction's **eager** in-flight state — exactly as the index fan-out reads
eager state for `OldValue` capture. Deltas are therefore computed against the
same graph the statement is building, and applied as one batch at commit.

### 3.2 Why the buffer, not inline `lpg` maintenance

Placing maintenance in the buffer (engine level) rather than inline in the
`lpg.Graph` mutation methods (as `SetNodeLabel` maintains `nodeIdx` at
`graph/lpg/lpg.go:1469`) is deliberate:

- **Atomic rollback for free.** A failed/aborted/capped write discards the buffer
  (`CountBuffer.Rollback`), exactly like `IndexBuffer.Rollback`
  (`cypher/exec/index_writeback.go:32`). Inline `lpg` maintenance would have to be
  reversed through the transaction undo log (`cypher/undo.go`) — more code, more
  risk. The count-store already needs no undo.
- **Durable-then-visible.** Flushing in `commitUnderBarrier` *after* the WAL fsync
  and *before* the barrier releases (`cypher/api.go:4029-4041`) puts the count
  update on the exact same side of the durability seam as the index update: a
  crash before the fsync leaves nothing to reconcile; a reader that sees the
  writes sees the matching counts.
- **Recovery is a separate concern.** WAL replay (`replayWALInto`,
  `store/recovery/recovery.go:1317`) runs at the store layer and would drive
  inline `lpg` maintenance during replay; keeping the count-store out of `lpg`
  means replay stays byte-for-byte its current cost and the store is populated by
  one authoritative recompute afterward (§6).

### 3.3 The hazard, quantified — node label mutation

`T(labelA, rt, labelB)` and `D(label, rt, dir)` are **join-cardinality statistics
keyed on a node's label**. A `SET n:X` / `REMOVE n:X` on node `a` changes the true
value of one `D`/`T` cell **per incident edge of `a`**:

- gaining `X`: `∀` out-edge `a-[rt]->b`: `T(X,rt,lb)+1 ∀lb∈L(b)` and
  `D(X,rt,OUT)+1`; `∀` in-edge `c-[rt]->a`: `T(la,rt,X)+1 ∀la∈L(c)` and
  `D(X,rt,IN)+1`.

This requires **enumerating `a`'s incident edges** → **O(deg(a))**. It is not an
implementation artefact: the information content of relabelling a hub *is*
O(degree), so **no exact incremental scheme can update `T`/`D` in less than
Ω(deg(a))**. On a hub (deg ≈ |E|), that is O(graph size) — a direct violation of
the "NEVER O(graph size)" mandate, and it would execute **inside the `visMu`
barrier** (`commitUnderBarrier` holds `visMu`), stalling every writer and every
`View` reader for its duration.

By contrast every other mutation is strictly bounded: edge create/delete is
O(|L(src)|·|L(dst)|) — a small schema constant, ≈ 4 atomic ops for the
single-labelled nodes openCypher produces in the common case; node create/delete
touches no `E`/`D`/`T` cell (a fresh node has no edges; a deleted node's edges
were stripped first, each strip an already-counted edge-delete).

**Verdict (against the O(delta) mandate):**

- **Rejected: "accept O(deg) because relabels are rare."** The mandate is
  non-negotiable and a single hub relabel is a legitimate small-looking statement
  that would freeze the engine. Correctness-first does not license an unbounded
  barrier hold.
- **Rejected: synchronous recompute on trip.** An O(E) recompute inside the
  barrier is strictly *worse* than the O(deg) it would replace.
- **Adopted: bounded exact + dirty-and-heal.** Maintenance carries an explicit,
  configurable per-commit fan-out budget `maxLabelRecountEdges` (default e.g.
  4096, surfaced in `EngineOptions` per the "bounded resources … surfaced in its
  constructor" mandate). For a commit whose total label-mutation incident-edge
  re-count is **within budget** (the overwhelming majority — most relabelled nodes
  are not hubs), maintenance is **exact and incremental**, O(≤budget) = bounded
  constant, and `D`/`T` stay `estExact`. For a commit that would **exceed** the
  budget, the incremental `D`/`T` update is **skipped**, `D`/`T` are marked
  **dirty** (§4.3), and their exactness is restored by an **off-barrier**
  recompute (a serialized `RecomputeCounts()` the engine may run opportunistically,
  or — for free — the next reopen recompute of §6). `E` and `N` are **never
  dirty** and stay exact through the hazard.

This is the **safe reduced-scope alternative** the mandate requires when exact +
O(delta) cannot both hold: correctness is absolute (a dirty `D`/`T` never yields a
wrong exact — it vetoes, §4.4), and per-commit work is bounded (within budget:
≤budget atomic ops; over budget: an O(1) flag set). Zero write-throughput
regression on the dominant CREATE/DELETE path is preserved by construction, and
**must be confirmed empirically** in P2 against the write-autocommit benchmark
that the reverted #2051 regressed 5.4× (see §7 acceptance gate) — the
measure-to-decide mandate forbids claiming it from intuition.

### 3.3.1 Correction — GoGraph stores only OUT-adjacency (P2 implementation)

The verdict above tacitly assumed a node's incident edges are enumerable in
O(deg(node)) in **both** directions. That is true only for the OUT direction.
GoGraph's directed graph stores adjacency **out-of-node only** (`adjlist`
forward slots); there is **no reverse in-edge index**, and the sole way to
enumerate a node's in-edges is a full `Mapper().Walk` — **O(V+E)** (confirmed:
`lpgMutatorAdapter.InNeighbours`, `cypher/api.go:13495`). Exact incremental
`D(*,*,IN)` / `T(*,rt,X)` maintenance on a relabel of node `X` therefore is
**not** achievable in O(delta): even *checking* the budget would cost O(V+E).

Building a reverse in-edge index is rejected here — it is a module-scale
adjacency restructuring (the deferred EPIC **#1879**), out of scope for the
count-store and colliding with that epic. The P2 resolution, faithful to the
dirty-and-heal spirit above and corrected for the OUT-only storage, is
**OUT-exact + IN-dirty-and-heal, X-scoped**:

- **OUT side — exact and cheap.** On `SET`/`REMOVE n:X`, enumerate `n`'s
  out-edges (`LoadEntryH`, per-instance type via the authoritative per-handle
  store — the per-*slot* label column is unreliable for parallel edges of
  differing type), budget-gated by `maxLabelRecountEdges`, and update the
  OUT-scoped cells `D(X,rt,OUT)` and `T(X,rt,Lb)` **exactly**. Over the
  out-degree budget → mark those OUT X-scoped cells dirty instead.
- **IN side — dirty, minimally X-scoped.** The in-edges cannot be enumerated in
  O(delta), so mark the **minimal** X-scoped IN cells dirty — `D(X,*,IN)` and
  `T(*,*,X)` (the b-position) — without walking in-edges. Family-level dirtying
  is the fallback only if X-scoping is impractical.
- **Cheap skip guard.** A relabel is a full no-op when the graph holds no edges
  at the eager mutation point (`AdjList.Size() == 0`): with zero edges there are
  no `D`/`T` cells to touch, so `CREATE (:N)` — labels are always assigned
  *before* any edge in a CREATE — pays nothing and never dirties. **No per-node
  in-degree counter is introduced** (it would cost O(V) memory and break the
  "bounded, data-size-independent" property of §2.3).

`E(relType)` and `N(label)` remain **never dirty**. Correctness is preserved
exactly as before: an X-scoped-dirty `D`/`T` cell yields `estFallback` →
`planStaysDefault` (§7), never a wrong exact, and self-heals at the §6 reopen
recompute. The consequence versus the idealised §3.3 verdict is only that the
dirty window is **more frequent** than "the rare hub relabel": *any* relabel of a
node in an edge-bearing graph dirties the X-scoped IN cells (and possibly the OUT
ones, over budget). Because label mutations are rare relative to edge CRUD and
dirty self-heals, this is an acceptable, correctness-safe reduction. A future
optional reverse in-edge index (EPIC #1879) would upgrade the IN-relabel path to
exact **with the `estExact` provider contract of §7 unchanged**.

---

## 4. Question 4 — Snapshot / isolation consistency

The pinned-snapshot foundation (#2051) is **deferred / non-existent**; this design
targets the model **as it stood when this document was written**:
`lpg.Graph.View` takes `visMu.RLock` (`graph/lpg/lpg.go:629`), `ApplyAtomically` /
`LockBarrier` take `visMu.Lock` (`:520`, `:565`), and the single writer serialises
commits.

> **Superseded (rmp #2379, 2026-08-10).** That is no longer the current model.
> `Graph.View` was removed by rmp #2344 and reads take no barrier: they pin an MVCC
> snapshot (`Graph.BeginRead` / `Graph.ReadAt` / `Graph.EndRead`). The pinned-snapshot
> foundation this section calls non-existent has since been delivered, so the
> paragraph above describes history, not the engine.

### 4.1 The consistency argument

- **Reads for planning already run under `View`.** The read-path plan build runs
  inside `e.g.View(func(){ … buildPlanEngine … })` (`cypher/api.go:1743`; the
  in-code contract at `:1728`: *"build runs under `visMu.RLock`; nothing here may
  call `g.View`/`g.ApplyAtomically`"*). The count-store resolver is consulted
  there, so its reads hold `visMu.RLock`.
- **Writes flush under the barrier.** `CountBuffer.Commit` runs inside
  `commitUnderBarrier` with `visMu` held (`cypher/api.go:4005-4041`).
- **Therefore `visMu` provides cross-substructure transactional atomicity:** a
  `View` reader observes either *all* of a committed transaction's count deltas or
  *none* — never a mid-commit partial — for exactly the same reason it observes an
  all-or-none view of adjacency, labels, and the secondary indexes. The counts a
  query reads are consistent with the graph state that query sees. **No new
  isolation mechanism (no generation stamp) is required** — the count-store rides
  the existing barrier, precisely as the label index does (`nodeIdx` is written
  under `ApplyAtomically`, read under `View`).

### 4.2 Map-level safety (defence in depth)

The per-shard `RWMutex` (§2.2) protects each map against a torn read of the map
header during insertion/deletion. Under the barrier discipline a flushing writer
(holding `visMu.Lock`) and `View` readers (holding `visMu.RLock`) never overlap,
so the shard lock is strictly defence-in-depth for any non-`View` access path and
keeps `-race` clean. Atomic counters make an individual cell read/increment
lock-free regardless.

### 4.3 The dirty flag

`D`/`T` exactness is a single in-memory epoch flag per family (`tExact`,
`dExact`, `atomic.Bool`), toggled **under the barrier** at flush time (off when a
commit trips the budget, §3.3) and back on when a recompute completes. Because it
is toggled under `visMu` it is subject to the same all-or-none visibility as the
counts themselves. It is **never persisted** (§6): every reopen recomputes and
clears it.

### 4.4 Reads never observe partial / mid-commit counts

Guaranteed by 4.1: the only writer holds `visMu.Lock` across the whole flush, and
every planning read holds `visMu.RLock`. ACID Consistency + Isolation for the
statistic are inherited from the barrier, identical to the graph itself.

---

## 5. Question 3/5 support — cost analysis and the both-end premium

### 5.1 Per-commit maintenance cost (the O(delta) ledger)

| Operation | E | D | T | Total per-op |
|---|---|---|---|---|
| create node (k labels) | — | — | — | O(1) (N: O(k), existing) |
| create/delete edge | ±1 | ±(\|L(src)\|+\|L(dst)\|) | ±(\|L(src)\|·\|L(dst)\|) | **O(\|L(src)\|·\|L(dst)\|)** |
| set/remove edge type | ±1 | ±… | ±… | O(\|L(src)\|·\|L(dst)\|) |
| delete node | — | — | — | O(1) (edges stripped first) |
| set/remove node label on `a` | — | O(deg(a)) | O(deg(a)) | **O(deg(a))** — §3.3, budgeted |

For the single-labelled nodes that dominate real and TCK workloads, edge
create/delete is a fixed **≈4 atomic increments plus at most a handful of
first-observation map inserts**. This is orders of magnitude below the O(shard)
COW that made #2051 catastrophic (that copied whole shard maps —
2,398→102,416 B/op, 43× memory — on every autocommit). The count-store touches
**only the cells the transaction actually changed**; there is no copy of untouched
state anywhere. This is the structural reason it can meet the zero-regression
mandate the #2051 approach could not.

### 5.2 Memory footprint and the both-end premium

Per `T` entry ≈ a `triKey` (12 B, padded) + `*atomic.Int64` (8 B ptr + 8 B
counter) + Go map bucket overhead ≈ **64–80 B**. Absolute footprint is a function
of **schema** cardinality, independent of `|V|`/`|E|`:

- Neo4j single-end store keeps `2·|labels|·|relTypes|` relationship cells.
- GoGraph's both-end `T` keeps up to `|labels|²·|relTypes|` cells — a factor of
  `|labels|` more.

Worked example: 20 labels, 15 relTypes. Single-end ≈ 600 cells; both-end worst
case ≈ 6,000 cells ≈ **~480 KB**, and *far* smaller in practice because the map is
sparse (only *observed* triples exist — a schema rarely realises every
label×type×label product). The premium — bounded, in the KB–low-MB range, and
data-size-independent — is the price of an **exact first-join cardinality**, which
no pair of single-end marginals can provide (the marginals only bound the join).
This is the quantified cost the "keep both end-labels" decision incurs.

---

## 6. Question 5 — Durability and recovery

### 6.1 The count-store is DERIVED and NON-DURABLE

Every count is a pure function of persisted graph state (nodes, their labels,
edges, their types). Therefore the count-store needs **no on-disk representation
of its own**:

- **No WAL op.** Nothing new is appended to or fsynced from the WAL. The write
  path's durability surface is unchanged.
- **No checkpoint component.** Unlike `constraints.bin` / `indexdefs.bin` — which
  exist because constraints and index *definitions* are **not** derivable from
  graph data and would be lost by a WAL-truncating checkpoint (#1755/#1756,
  `graph/lpg/lpg.go:401-429`) — the counts **are** fully derivable from the
  snapshot's graph payload, so persisting them would be redundant and a new
  torn-write / divergence risk for no benefit.
- **Recompute at reopen.** After `store/recovery.Open` (`recovery.go:739`)
  rebuilds the graph (snapshot + WAL replay), the engine populates the count-store
  in **one O(V+E) pass** over the recovered graph — enumerate every live edge,
  read its type and both endpoints' labels, apply the create deltas of §3.1. This
  mirrors `registerRecoveredIndexes` (`cypher/index_binding.go:663`), which
  backfills bound indexes from the recovered graph, and the numeric-companion
  btree that is re-derived (never persisted) at recovery. Wire it in the
  `NewEngineWithStore*` / `NewEngineWithOptions` construction path
  (`cypher/api.go:913`, `:1027`), after `registerRecoveredConstraints` /
  `registerRecoveredIndexes` (`:1126`/`:1132`), from the same fully-materialised
  graph. O(V+E) at startup is explicitly acceptable.

### 6.2 Recovery-correctness argument (kill -9 mid-commit)

The count-store **cannot diverge from the graph after recovery** because it is
computed *from* the recovered graph, *after* recovery, by a single authoritative
pass:

1. WAL recovery is already crash-consistent and certified: committed transactions
   are present, an uncommitted torn tail is discarded (the `OpCommit`-marker /
   `TxnSeq` suffix filter, `recovery.go:1389-1397`; torn-frame / CRC handling).
   After `Open`, the graph is *some* well-defined committed state `G*`.
2. The recompute reads `G*` and produces counts that equal, cell-for-cell, the
   counts of `G*` by definition.
3. Hence for **any** crash point — including `kill -9` between the WAL fsync and
   the in-memory apply, or mid-flush of the `CountBuffer` — the reopened counts
   match the reopened graph exactly. The in-memory count-store of the crashed
   process is simply discarded; there is no persisted count state that could be
   stale, torn, or ahead of/behind the graph.
4. The runtime **dirty** flag (§4.3) is also non-durable: a reopen always
   recomputes and clears it, so a crash during a dirty window heals on restart.

This is a strictly stronger durability posture than any persisted-count scheme:
the design adds **zero** new durability failure modes.

### 6.3 Checkpoint interaction

A checkpoint truncates the WAL prefix and writes a graph snapshot. Because the
snapshot already persists the labelled/typed graph, the post-reopen recompute
reconstructs the counts from `snapshot + WAL tail` with no checkpoint change. The
count-store does not participate in the checkpoint at all.

---

## 7. Question 6 — The estExact promotion rule

The count-store exposes a resolver consumed exactly like `labelCounter`
(`cypher/estimate.go:96`) and `lpgLabelResolver.ResolveLabelCount`
(`cypher/api.go:4405`), producing an `estimate{rows, source}` (`estimate.go:64`)
via a helper analogous to `labelCardinalityEstimate` (`estimate.go:111`). The
promotion rule:

1. **`E(relType)` → `estExact`, always.** Unknown/never-interned type → `(0,
   estExact)` (the type has zero live edges — an exact zero, matching how
   `ResolveLabelCount` returns `(0, true)` for an unknown label, `api.go:4408`).
2. **`N(label)` → `estExact`, always** (unchanged; served by the label index).
3. **`D(label, relType, dir)` / `T(labelA, relType, labelB)` → `estExact` iff the
   family is not dirty.** Unresolvable label/type id → exact `0`. When `dExact` /
   `tExact` is **false** (a hub relabel tripped the budget, §3.3), the lookup
   returns **`estFallback`** — an absolute veto: `planStaysDefault`
   (`estimate.go:82`) forces P3 back to today's default plan. Never a fabricated
   or stale exact.
4. **Barrier requirement.** A count read that feeds an `estExact` estimate MUST be
   issued under the query's `View` (guaranteed on the read path, `api.go:1743`),
   so the count is snapshot-consistent with the graph the query sees (§4). A read
   outside any barrier is per-cell atomic (never torn) but is not guaranteed
   consistent across cells with the query's graph view; such a path must not
   promote to `estExact`.

Because a not-yet-populated or dirty statistic returns `estFallback`, the P3
reorder that consumes these estimates is **provably inert** until real exact
counts are online — the same free no-regression guarantee #2076 established
(`optimizer-activation-design.md` §2.1). P3 lights up one join-order decision at a
time exactly as `estExact` relationship counts become available.

---

## 8. Implementation task breakdown (P2)

Ordered; each is self-contained and gated by `make ci` (TCK 3897/3897 green,
`-race` clean, `goleak`).

**#2082 — structure + maintenance.**
- `graph/index/count` (or `cypher/exec`): the sharded `countStore`
  (`E`/`D`/`T` maps, `*atomic.Int64` cells, per-shard `RWMutex`, `triKey`,
  zero-delete, `dExact`/`tExact` flags, `maxLabelRecountEdges` budget).
- `exec.CountBuffer` (twin of `exec.IndexBuffer`): `Enqueue` / `Commit(cs)` /
  `Rollback` / `Len`.
- Wire delta production into the `lpgMutatorAdapter` methods per the §3.1 table;
  flush in `commitUnderBarrier` (success, after WAL fsync) and discard in
  `rollbackUnderBarrier`.
- Regression tests: exact counts vs a brute-force graph recount after randomised
  CREATE/DELETE/SET-label workloads; budget-trip → dirty; `-race`; `goleak`.
- **Acceptance gate (mandatory, empirical):** re-run the write-autocommit
  benchmark the reverted #2051 regressed (5,664→30,665 ns/op, 2,398→102,416 B/op);
  P2 must show **no statistically-significant ns/op or B/op regression**
  (`benchstat`, `-count≥5`). If it regresses, the design does not ship as-is.

**#2083 — estExact provider.**
- The resolver + `estimate`-producing helper (§7), mirroring
  `labelCardinalityEstimate`; unknown-id → exact 0; dirty → `estFallback`.
- Reuse the existing label index for `N`; do not duplicate node counts.
- Unit tests over the veto/`estExact` matrix, including the dirty veto — no P3
  reorder wired yet (provider is inert on its own, like #2076 shipped inert).

**#2084 — durability / recovery.**
- `RecomputeFrom(graph)` O(V+E) pass; invoke from `NewEngineWithStore*` /
  `NewEngineWithOptions` after `registerRecoveredIndexes` (§6.1).
- Optional serialized `RecomputeCounts()` for off-barrier dirty-healing.
- Tests: crash-inject / `kill -9` mid-commit then reopen → recomputed counts
  equal a brute-force recount of the recovered graph; checkpoint+WAL-tail reopen;
  dirty-then-reopen clears exact. Reuse the `store/recovery` crash battery
  harness.

---

## 9. What changes once #2051 (pinned snapshot) lands

The current design is correct under `visMu` and forward-compatible; the pinned
snapshot only *improves* it:

- **Fold the count-store into the `Snapshot` root.** Per the #2051 phased design,
  a per-query pinned reader must observe every result-servable substructure frozen
  at one instant. Once the root exists, the count-store should become one more
  substructure captured at the commit seam (root `Store` at end-of-window), so a
  pinned reader sees counts frozen *together with* the adjacency/labels it pinned
  — repeatable-read counts per query, strictly stronger than the current
  `View`-scoped consistency. The count-store's copy-on-write freeze is trivial
  compared with the label/adjacency shards: counts are tiny integers, so a
  frozen immutable snapshot per pinned root is cheap (structural sharing of
  unchanged shards, same ver-stamp technique the #2051 P2 ruling specifies for
  the label shards).
- **Off-barrier dirty-heal becomes clean.** With a pinned snapshot, the O(E)
  recompute that heals a dirty `D`/`T` can run against a pinned image *outside*
  the barrier, then publish atomically — removing even the reopen dependency for
  healing.
- **No API break.** The `estExact` provider contract (§7) is unchanged; only the
  consistency mechanism behind it upgrades from "read under `View`" to "read from
  the pinned root".

Until #2051 lands, `View` + `visMu` is the consistency mechanism and the reopen
recompute is the dirty-heal of record — both fully sufficient for correctness.

---

## 10. Verdict summary

- **Structure:** sparse sharded maps of `*atomic.Int64`, keyed by `uint32` ids
  from the single monotone-append `LabelRegistry`; zero-delete keeps growth
  bounded by *observed* schema combos, data-size-independent.
- **Maintenance:** a `CountBuffer` twin of `exec.IndexBuffer`, flushed in
  `commitUnderBarrier` after the WAL fsync. Edge/node CRUD is strictly O(delta)
  (≈4 atomic ops for single-labelled nodes) — **zero regression by construction**,
  the property the O(shard) #2051 approach could not have.
- **Label-mutation hazard verdict:** exact incremental `D`/`T` on a node relabel
  is inherently Ω(deg(node)); accepted as exact **within a bounded per-commit
  budget**, and for the rare over-budget hub relabel `D`/`T` go **dirty**
  (in-session, non-durable) → **veto to default plan** (never a wrong exact) →
  self-heal by recompute. `E`/`N` never affected. This is the mandate-compliant
  reduced-scope path where exact + O(delta) cannot both hold.
- **Snapshot-consistency mechanism:** ride the existing `visMu` barrier — writes
  flush under `visMu.Lock` in `commitUnderBarrier`, planning reads hold
  `visMu.RLock` in `e.g.View` (`api.go:1743`). No new generation stamp. All-or-none
  visibility inherited from the barrier, identical to the label index.
- **Durability:** derived, non-durable, recomputed O(V+E) at reopen from the
  recovered graph — no WAL op, no checkpoint component, no fsync, and provably
  non-divergent across checkpoint, WAL replay, and `kill -9`.
- **estExact rule:** `E`/`N` always exact; `D`/`T` exact iff not dirty; unknown id
  → exact 0; dirty → `estFallback` veto. Read must be under the query's `View`.
