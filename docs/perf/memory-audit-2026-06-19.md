# GoGraph memory-usage audit — 2026-06-19

**Type.** Read-only empirical audit. Nothing in this document has been
implemented; the findings are filed as `rmp` backlog (sprint 221, tasks
1628–1633) for a later implementation cycle.

**Goal.** A detailed Go memory audit of the module, gathering empirical evidence
by exercising the 26 examples across diverse scenarios, to identify
efficiency (resident-RAM and allocation/GC) improvement opportunities.

**Environment.** 10-core / 32 GiB Apple M4, go1.26.4, branch `main @ ea36227`
(includes the prior memory-audit merge `049706f`, sprints S205–S210).

**Method.**

1. Built all 26 example binaries; ran each at a representative scale, capturing
   the examples' own post-GC live-heap telemetry (`# mem.heap_alloc`,
   `# mem.heap_growth`, `# bytes_per_node|edge`, `# *.mallocs`) and process peak
   RSS via `/usr/bin/time -l`.
2. Attributed the two standout consumers (ex02 LPG-with-indexes, ex22 Cypher)
   with `go tool pprof` `alloc_space` profiles, via temporary in-package
   benchmark harnesses that call the real `run()` (removed after profiling).
3. Cross-checked every structural finding against source and had three
   specialists certify the designs — columnar-db-expert (layout), go-developer
   (Go idiom + exact call-site surface), storage-engine-auditor
   (durability/on-disk-format safety) — per the project's measure-to-decide and
   specialist-consultation mandates.

---

## 1. Empirical sweep — where memory goes, by scenario

Live heap = post-GC reachable bytes (each example's own `readMem()`); RSS = peak
process resident set. Superlinear algorithms (exact Brandes, Hungarian,
max-flow, Yen) were run at moderate scale — their interest is transient
allocations, not resident heap.

| Ex | Scenario / dominant structure | Scale | live heap | per-element | peak RSS |
|----|-------------------------------|-------|-----------|-------------|----------|
| **22** | **Cypher engine** (scan/order/where/rel/create) | 200k users, 900k edges | **483.8 MiB** | — | **1276 MB** |
| **26** | **LPG social + Cypher**, dated edges | 200k users, ~13M edges | (build ~287 MiB¹) | **446.1 B/edge** | **>6.5 GB queries** |
| **02** | **LPG + schema + 2 indexes** | 300k persons, 300k edges | 227.6 MiB | **786.9 B/node** | 332 MB |
| 01 | CSR Dijkstra (RGG), in-memory build | 300k nodes | 214.3 MiB | — | 562 MB |
| 06 | CSV/JSONL round-trip over an LPG | 150k users | 192.2 MiB | 52 B/edge CSV | 462 MB |
| 19 | LPG fluent pattern query | 200k pkgs, 800k edges | 153.9 MiB | ~190 B/elem | 264 MB |
| 05 | csrfile mmap, semi-external PageRank | 500k nodes, 4M edges | 38.4 MiB | — | 328 MB |
| 12 | CSR DAG (TopSort / Tarjan) | 200k mod, 375k edges | 36.8 MiB | — | 87 MB |
| 08 | CSR PageRank (directed scale-free) | 500k pages | 26.9 MiB | — | 346 MB |
| 20 | frozen CSR, lock-free concurrent reads | 200k nodes, 1.6M edges | 26.2 MiB | ~14 B/edge | 384 MB |
| 17 | WAL ledger + checkpoint + recovery | 3k acct, 15k xfer | small | — | 37 MB |
| 18 | OOC pipeline CSV→CSR→csrfile→mmap | 400k nodes, 3.2M edges | 3.3 MiB | (disk 55 MiB) | 530 MB |

¹ ex26 build heap from the 2026-06-19 re-measurement; the multi-GB figure
observed today is the Cypher **query** phase on the 200k-user graph, not the
build. The 446.1 B/edge reflects the mandatory dated edge properties.

Examples not tabulated were either already characterised in batch 1 or hit a
generator pre-condition under the chosen scale knobs (e.g. ex11 needs
`bridges ≥ communities−1`) — none change the conclusions below. ex07
(GraphML round-trip) is **CPU-, not memory-bound**: ~67 MB RSS but minutes of
CPU at 80k nodes/400k edges — recorded as an out-of-scope CPU-efficiency
observation, not a memory finding.

### 1.1 Reading
The cost is **not** in the immutable or out-of-core path: a frozen CSR sits at
the textbook floor (~14 B/edge, ex20; ex08/12 tens of MiB for half a million
elements) and `csrfile` mmap holds heap to single-digit MiB regardless of graph
size (ex05/18). Persistence heap is small (ex17). The resident and allocation
cost concentrates in the **mutable labelled-property graph** (ex02:
786.9 B/node; ex26: 446 B/edge) and the **Cypher query path** (ex22/26, peak RSS
2.6–20× the resident heap). Every opportunity below lives there.

---

## 2. Ranked opportunities

The prior (2026-06-18) audit was ex26-centric and shipped S205 (edge-**label**
inline column), S206 (index `NodeSet` small tier), S207 (node `propBag` small
tier), S208 (Cypher **node** lazy-materialisation), S209 (search transients),
S210 (snapshot CSR codec). It left three *sibling* structures un-tiered and the
relationship value eager. All findings below are NEW relative to that work and
are profile-confirmed.

### F2 — Edge properties: un-tiered per-edge nested map ★ do first  (rmp #1628)
- **Evidence.** `graph/lpg/lpg.go:163` `edgePropShard.m map[edgeKey]map[PropertyKeyID]PropertyValue`;
  per-edge allocation at `graph/lpg/edge_property.go:25`. **Largest single
  allocation site in ex22: `SetEdgeProperty` 212 MB / 16.8%.** ex26's dated
  edges measure 446 B/edge.
- **Fix.** Reuse `propBag` **verbatim** → `map[edgeKey]propBag`, with the
  by-value read-modify-**write-back** idiom under the shard lock. ≈348 → ≈80
  B/edge (~4×).
- **Certification.** storage-engine-auditor: format-neutral and ACID-safe —
  serialization reads through the public `EdgeProperties()` accessor (logical,
  not in-memory-order), `propertiesFormatVersion` stays `1`, no migration. The
  write-back line is load-bearing (omitting it = lost update).
- **Leverage.** Highest, lowest effort; edges typically outnumber nodes, so by
  total bytes it beats F1, and it reuses already-audited code.

### F1 — Node labels: un-tiered per-node map ★ do second  (rmp #1629)
- **Evidence.** `graph/lpg/lpg.go:155` `nodeLabelShard.m map[graph.NodeID]map[LabelID]struct{}`;
  per-node allocation `lpg.go:1153` `make(map[LabelID]struct{})`. ex02 profile:
  **`SetNodeLabel` cum 27.9 MB / 21.9%** of build allocations (~166 B/node).
- **Fix.** New **unexported** `labelBag` tiered union (singleton / small
  `[]LabelID` / map; threshold 8), mirroring `NodeSet` but value-less; label
  order is unobservable. ≈300 → ≈34 B/node (~9×).
- **Risk.** go-developer enumerated the full surface (8 `nodeLabelShardFor`
  methods in `lpg.go` + `introspect.go`). One real hazard: keep the `nodeIdx`
  label-bitmap (`lpg.go:1158`) in lockstep with the bag, plus the tombstone
  strip/restore path — add a dedicated regression test. No on-disk/TCK change.
  Keep the type unexported (the concurrencydoc ratchet fires only on exported
  types).

### F4a — Cypher relationship values are never lazy-ified ★ compounds with F2  (rmp #1630)
- **Evidence.** `cypher/api.go:7419` `buildRelationshipValueFromRow`
  **unconditionally** calls `g.EdgeProperties` (+ `EdgeLabels` +
  `buildEdgeTypeFilter`) to materialise every matched edge's full property/type
  map per row — whether or not the query reads it. ex22 query profile:
  `buildRelationshipValueFromRow` cum **384 MB / 30%**, `EdgeProperties` 190 MB,
  `buildEdgeTypeFilter` 146 MB; the umbrella `populateRowCtx` is cum 632 MB
  (50%) and `exec.(*Project).Next` cum 689 MB (54.6%). Nodes were made lazy in
  S208 (`LazyNodeValue`, `api.go:6876`, now only 20 MB); relationships were not.
- **Fix.** A `LazyRelationshipValue` mirroring `LazyNodeValue`: carry the edge id
  + endpoints + a resolver, defer `EdgeProperties`/`EdgeLabels` until a property
  or type is actually accessed. Compounds with F2 (cheaper materialisation when
  it does occur).
- **Risk.** cypher-expert review of deleted-entity and per-instance-label
  (multigraph) semantics — S208 preserved these for nodes; the relationship path
  must too. TCK-sensitive → re-verify 3897.

### F3 — `PropertyValue` boxes every scalar in `any` ◇ gate hard, may abandon  (rmp #1631)
- **Evidence.** `graph/lpg/property.go:39` `struct { kind PropertyKind; v any }`.
  Every int64/float64/bool is boxed → a heap word per scalar + GC scan pressure,
  on node *and* edge properties (ex02 `StringValue`/`Float64Value` ~5.3 MB;
  pervasive).
- **Fix.** `{ kind; num uint64; ref any }` — scalars packed in `num`,
  strings/lists behind `ref`. Internal-only (`.v` is read only inside
  `property.go`; the struct fields are unexported).
- **Caveat (both columnar-db-expert and go-developer concur).** The resident win
  is **~neutral** (the +8 B struct growth ≈ the saved scalar box). The real win
  is **allocation count + GC scan** (scalar values become pointer-free). Decide
  by `-benchmem` + `GODEBUG=gctrace=1`, NOT resident bytes. A NaN/−0.0
  bit-exactness round-trip test is mandatory (TCK NaN-equivalence paths). **Ship
  only on unambiguous benchstat evidence; otherwise record the numbers and
  abandon.**

### #1597 — `csrfile/writer.go` widening copy ★ confirmed still open  (rmp #1632)
- **Evidence.** `store/csrfile/writer.go:131-137` allocates
  `tmp := make([]uint64, len(edges))` then widens element-by-element — but
  `graph.NodeID` *is* `uint64`, so it is a pure no-op copy of a full edge column
  (~8 B/edge) plus `binary.Write` scratch, at every bulk publish. Same defect
  S210 fixed in the snapshot codec; `csrfile` is a separate package that never
  received it. The `Reinterpret[T]` primitive to fix it already exists in-package
  (`store/csrfile/reinterpret.go:35`).
- **Risk.** Byte-identical on disk (LE host, LE format), CRC unchanged → no
  format bump, ACID-neutral. storage-engine-auditor verdict: this is the top
  remaining persistence transient lever; S210 is confirmed complete for the
  snapshot read *and* write paths.

### Follow-ons — same pathology, surfaced by the auditor  (rmp #1633)
`graph/lpg/edge_instance_props.go:19` (per-CREATE instance props) and
`graph/lpg/edge_handle.go:71`/`:65` (per-handle props *and* per-handle labels)
carry the identical un-tiered innermost map. These are the authoritative
per-instance surface in **multigraph** mode, so for parallel-edge-heavy graphs
they can dominate the per-pair store F2 targets. Apply `propBag`/`labelBag`
there too.

### Re-validated still-open backlog
- **#1596** — 16-byte `unsafe` `NodeSet` union (~5.1× over the current ~48 B
  value). Minor for low-cardinality indexes (ex02), material for high-cardinality
  unique `{id}` indexes (ex19/26).

---

## 3. Higher-ceiling direction (separate initiative)
For this build-once-query-many graph, columnar-db-expert recommends the
C-Store / Vertica **read-store + write-store + compaction** pattern as the
eventual ceiling: an immutable CSR/columnar base built at checkpoint plus a
small mutable tiered-union delta overlay, compacted on checkpoint; with true
per-`(label, propKey)` columnar value storage (dictionary / RLE / FOR encoding)
as the highest tier. This breaks the schemaless property-bag model and is a
distinct initiative — F1/F2/F4a are the incremental, low-risk wins to take
first.

## 3a. Implementation results (branch perf/mem-audit-2026-06-19)

Measured on this machine (10-core M4, go1.26.4). Each fix is its own commit with
a `-benchmem`/heap-snapshot gate; TCK 3897, `go test -race`, and the full short
suite (98 packages) stay green throughout.

| Fix | Commit | Controlled harness (200k nodes / 600k edges) | Real example |
|-----|--------|----------------------------------------------|--------------|
| **F2** edge-props → propBag | cd4d24d | edge-prop store **365 → 154 B/edge (−58%)**; full mix 355.4 → 229.1 MiB (−35.5%) | ex22 live heap **483.8 → 246.4 MiB (−49.1%)**, peak RSS 1276 → 747 MB (−41.5%) |
| **F1** node-labels → labelBag | c7ef402 | label store **~145 → ~72 B/node (−50%)**; labels-only 68.0 → 53.4 MiB | ex02 786.9 → 726.3 B/node, heap 227.6 → 210.1 MiB |
| **#1597** csrfile byte-views | db8a504 | — | WriteToFile 400k edges **4.26 → 1.85 MB/op (−56.6%)**, 27 → 24 allocs, −13% ns |
| **F3** PropertyValue de-box | (none) | **Deferred** — gated experiment | see below |
| **F1+F2 combined** | — | full LPG build **355.4 → 214.5 MiB (−39.6%)** | — |

**F3 outcome (gated, rejected).** The proposed `{kind; num uint64; ref any}`
keeps the `ref` pointer in every value, so the type stays GC-scanned (no
pointer-free win) and grows 24 → 32 B. In realistic mixes Go already caches
small ints and bools (zero box on escape); only float64/large-int/string box. So
de-boxing removes ~one float64 box per scalar prop but adds +8 B to *every*
stored value → a net **resident regression** (~+24 B/node in a 5-prop/3-scalar
node) with no GC-scan benefit. Closed as measured-not-worth-it (rmp #1631); a
true win needs a fully unboxed scalar union (no `ref` for scalar kinds), tracked
separately and only if alloc/GC throughput — not resident memory — is the target.

**F4a** (Cypher `LazyRelationshipValue`) — `RelationshipValue` is a concrete
struct consumed at ~60 sites across `cypher/api.go` + `cypher/exec/*`; a lazy
variant is a large, TCK-sensitive value-model change. Under cypher-expert design
review for a contained, openCypher-safe approach before implementation (rmp #1630).

## 4. Suggested sequencing
F2 → F1 → F4a (each its own commit with a `-benchmem` gate and a heap-snapshot
harness on the ex26 generators) → #1597 (independent, trivial) → F3 (gated, may
abandon) → multigraph per-instance follow-ons → #1596.
