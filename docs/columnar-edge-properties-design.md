# Columnar edge-property storage tier — design note (Phase 1)

Status: **design accepted** (rmp `gograph` sprint 222, task #1636). Implementation
tasks: #1637–#1645.

## Motivation

The edge-property store is the dominant resident-memory consumer at scale. On the
`perf/mem-audit-2026-06-19` branch (post-propBag, #1628) it costs **~128 B/edge** for
one date property — **~82 % of live heap** in the example-26 social-scale workload —
of which only **~16 B is the value**; the remaining ~112 B is structural overhead:
the `map[edgeKey]propBag` slot (~65 B keyed by a redundant `(src,dst)` pair the
adjacency already holds), the one-element backing slice (~31 B), and the `any`
interface box around the value (~17 B).

A measurement spike (#1635, throwaway prototype) confirmed the fix: a typed value
column aligned 1:1 to the adjacency `neighbours` array costs **~4.3 B/edge** for an
`int32` epoch-day date (−96.6 % vs the map) and **~33.2 B/edge** for a generic
string (−74 %), cross-checked by `runtime.ReadMemStats` and `pprof inuse_space`.
Extrapolated to the example-26 full specification (~3.25×10⁸ edges) the total live
heap drops from **~48 GiB to ~11 GiB** (int32) or **~19 GiB** (string). The hot
`count(r)` / `r.since IS NOT NULL` scan also changes from a random map probe per
edge into a sequential, bandwidth-bound stride plus a validity-bitmap popcount, and
the de-boxed column becomes a GC leaf.

This mirrors the relationship-**label** win already shipped (#1633): a per-pair
`map[edgeKey]LabelID` was replaced by the `labels []uint32` column inside `adjEntry`.
This note applies the same idea to property **values**.

## Decision: D1 — opaque per-slot column block inside `adjEntry`

Three placements were evaluated and certified by `graph-theory-expert`,
`storage-engine-auditor`, and `columnar-db-expert`. All three converge on **D1**.

- **D1 (chosen)** — an lpg-owned, immutable column block lives inside each
  `adjEntry` (per source node). `adjlist` drives the slot lifecycle (append, grow,
  swap-compaction) and carries the block verbatim, exactly as it does for the
  opaque `labels`/`handles` columns; it never interprets the contents. `lpg` owns
  the typed encoding, the validity plane, and the key→column mapping.
- **D2 (rejected)** — an lpg-side parallel store mirroring slot order. Re-implements
  `adjlist`'s compaction slot-math across a package boundary and splits topology and
  properties into two independently-published atomic objects → a reader can observe
  new topology with a stale/absent property column (Isolation hazard) unless every
  property read leans on a barrier lock.
- **D3 (rejected)** — a global per-`(key,kind)` column indexed by a dense edge-id.
  Requires a universal dense edge-id, which does **not** exist today (Primary-Id is
  unimplemented); deletes punch holes; it de-correlates the property from the
  adjacency, losing the dominant adjacency-local traversal locality; and it fights
  undirected edges (one global cell vs two physical slots). D3 is the natural
  **Phase 2** representation once a dense edge-id lands — D1 does **not** foreclose
  it (D1 and D3 are the same column content under two indexings).

### Why D1 wins on every axis

- **Isolation / lock-free reads** — the column block rides the *same*
  `atomic.StorePointer` publish as the adjacency `adjEntry`. One atomic store makes
  topology + properties visible together; one atomic load reads both. A torn
  cross-substructure read is structurally impossible, with **no new lock**.
- **Atomicity / Durability** — durability flows through the WAL (logical ops) and the
  snapshot (public surface), not the in-memory representation. D1 changes only the
  in-memory column, so the `kill -9` proof is preserved verbatim (as #1628/#1587
  were format-neutral).
- **Compaction correctness** — binding is positional-within-slot; `compactEntry`
  applies the identical index transform to every parallel column. D1 inherits the
  proven `labels`/`handles` lockstep.
- **Undirected** — an undirected edge is two independent slots (forward + mirror),
  each carrying its own copy of weight/handle/label. D1 inherits this verbatim
  (write both directions, as labels already does); D3 cannot.

## Public-contract decision (load-bearing)

The current public surface `Graph.EdgeProperties(src,dst)` is **per-pair**: parallel
edges between the same `(src,dst)` coalesce, latest-wins. The columns are **per-slot**.

**Decision: preserve the per-pair public contract unchanged; back it with per-slot
columns.** For a simple graph (one slot per pair) per-slot ≡ per-pair. For a
multigraph, `EdgeProperties(src,dst)` coalesces the pair's slots (latest-wins),
exactly as today. Exposing per-parallel-edge properties through `EdgeProperties`
would be a TCK-affecting semantic change and is **out of scope**. The existing
per-CREATE-instance / per-handle property stores (`edge_instance_props.go`,
`edge_handle.go`) are unchanged and remain the authoritative multigraph surface.

This keeps the change behaviour-preserving for the openCypher TCK (3897) and the
public Go API.

## Column representation

Per source node, the block holds a small set of typed columns, **one per
`(propertyKeyID, PropertyKind)`** present on that node's out-edges, each aligned 1:1
to `neighbours` and **lazily allocated** (nil until first set, like `labels`):

| Kind | Column | Bytes/slot |
|---|---|---:|
| `PropInt64` | `[]int64` | 8 |
| `PropFloat64` | `[]float64` | 8 |
| `PropBool` | bit-packed | 0.125 |
| date (see below) | `[]int32` epoch-day | 4 |
| `PropString` | `[]string` (header + bytes) | 16 + len |
| `PropBytes`/`PropList` | `[]PropertyValue` (boxed fallback) | 24 + payload |

Values are stored **de-boxed** (no `any`) for the scalar kinds — this is the bulk of
the win. `PropBytes`/`PropList` keep a boxed representation (rare on edges).

### Validity / presence

A property absent on an edge is `null`. Presence is an **Arrow-style validity
bitmap**, one bit per slot per column, **omitted entirely when the column has no
nulls** (the dense case — so the example-26 date column pays *zero* validity
overhead and the spike's 4.3 B/edge holds). A bitmap is **not** replaceable by a
sentinel: `0` is a legal `int32`/`int64`, and `NaN` is a queryable `float64`.
`IS NULL` / `IS NOT NULL` reads only the bitmap (popcount), never the values.

### Sparse / heterogeneous

- **Sparse key on a high-degree node** — a per-`(key,kind)` column is sized to the
  node's degree even if the key is set on few of its edges. To avoid regressing this
  regime (at degree 1000, fill 1 %, a dense column loses ~8× to the map), each column
  carries a **dense↔sparse switch on fill ratio**: below a threshold it stores a
  compact `(slotIndex, value)` COO pair list; above it, the dense aligned array.
- **Same-key type collision** (the same key carrying different kinds across edges,
  which openCypher allows) — the minority-kind slots spill to a small **boxed
  overflow tier** reusing the existing `propBag`. The common single-kind-per-key case
  never touches it.

### In-memory encoding

De-box only. **No RLE / FOR / bit-packing / dictionary in memory** (beyond bit-packed
`bool` and the validity plane): de-boxing already captures ~97 %, and decode in the
hot scan loop would erode the cache win. Heavyweight encoding is reserved for the
on-disk CSR (already columnar). String dictionary-encoding is **deferred to Phase 2**,
gated on observed cardinality (cf. ClickHouse `LowCardinality` < ~10k distinct). For a
pure date the `int32` epoch-day already beats a dictionary.

## Cypher read-back (TCK-critical)

`lpgPropToExpr` (`cypher/api.go`) has **no `PropTime` case**: a raw lpg temporal reads
back as `expr.Null`. The date column must therefore surface as either a
`\x01`-prefixed `Date` tagged string (round-trips as a native `Date` — strictly better
than today's plain string: correct calendar `ORDER BY`/range) or a plain ISO-8601
`PropString`. **Never `PropTime`.** A Cypher write → store-int32 → read round-trip
test gates this; without it the change passes every memory benchmark and silently
fails conformance.

## Concurrency

Writers serialise on the per-shard mutex and publish a new immutable `adjEntry` via
`atomic.StorePointer`, copy-on-write on the affected column only (sharing the
immutable `neighbours`/`weights` headers) — exactly as `SetEdgeLabelSlot` does. No
new global lock. Readers are lock-free on the published snapshot.

## Persistence

**Format-neutral.** The snapshot writer keeps serializing via the public
`EdgeProperties(src,dst)` accessor, so `propertiesFormatVersion` stays **1** and
v0.5.0 snapshots reopen unchanged. The WAL logs logical
`(src, dst, key, value)` ops (`encodeOpEdgeProperty`); only the in-memory apply path
changes. Recovery replays via the public `SetEdgeProperty`, reconstructing the columns
deterministically.

## Risks and required coverage

1. **Validity-bitmap drift (highest correctness risk).** A slot-creating fast path
   reuses dirty backing-array cells; every such path must explicitly clear the new
   slot's validity bit. Property-based add/remove/re-add against an oracle.
2. **Lockstep compaction.** The value column **and** validity plane must compact in
   the same swap as `neighbours` (`compactEntry`); reuse the `growCap(oldLen)` sizing
   rule (`adjlist.go:518-524` — the one that previously caused a recovery panic). The
   bitmap compaction is a bit-shift, not a slice copy.
3. **Recovery value identity (highest durability risk).** A crashinject scenario must
   assert edge-property **value identity** (kind + payload, not mere presence) over a
   typed mix (string/int64/float64/list) across a `kill -9`/recovery cycle, with
   crashpoints (a) between WAL fsync and snapshot publish and (b) mid-recovery between
   adjacency rebuild and property-column reconstruction. `internal/crashinject` has no
   edge-property durability coverage today.
4. **Dense-path guard.** A monomorphic dense column must remain a bare validity-free
   `[]T` (nil-until-used). A benchstat test asserts the dense date column's B/op
   equals the bare-`[]int32` baseline with zero validity/overflow allocation.

## Task mapping (sprint 222)

| Task | Scope |
|---|---|
| #1637 | de-boxed typed value columns aligned to adjacency |
| #1638 | validity bitmap + `IS NULL`/`IS NOT NULL` popcount fast path |
| #1639 | lockstep swap-compaction of column + bitmap on delete |
| #1640 | copy-on-write writes preserving lock-free reads |
| #1641 | sparse/heterogeneous (dense↔sparse switch + boxed overflow tier) |
| #1642 | Cypher read-back (date → `\x01`-Date / ISO string, never `PropTime`) |
| #1643 | retire `map[edgeKey]propBag`; derive `EdgeProperties` lazily |
| #1644 | snapshot/WAL persistence (format-neutral) |
| #1645 | gate + evidence (TCK 3897, ACID battery, `-race`, benchstat) |

## Phase 2 (not in scope, not foreclosed)

When a universal dense edge-id lands (Primary-Id, or always-on dense handles), the
per-source-node columns can migrate to a global `edgeId → record` SoA table (D3)
without touching the id-agnostic lpg property API. That unlocks O(1) global
edge-keyed access for edge-centric algorithms (contraction, line-graph, flow) and
whole-graph edge-property aggregation. The measured trigger to do so is those access
patterns becoming dominant.
