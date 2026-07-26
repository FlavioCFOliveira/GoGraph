# Stream 3 — In-memory data model and memory efficiency

Baseline commit `6f31f61` (v0.10.0). All GoGraph numbers are **measured**, not argued.

**Hardware / method.** Apple M4, 10 cores, 32 GiB RAM, macOS 26.5.2, Go 1.26.5 darwin/arm64.
Measurement = `runtime.GC(); runtime.GC(); runtime.ReadMemStats().HeapAlloc`, differenced across
build layers, graph kept live with `runtime.KeepAlive`, `debug.SetGCPercent(20)`. Harnesses are
standalone modules with a `replace` onto the repo, at
`/private/tmp/claude-501/-Users-flaviocfo-dev-xumiga-GoGraph/48f01b28-6444-4504-ac86-d3a529405419/scratchpad/audit3/memlab/`
(`main.go` layered/scale sweep, `deg/` degree sweep, `probe/` A+B+C experiments, `probe2/` node
properties + reclamation, `sizes/` exact `unsafe.Sizeof` of the unexported hot structs, `csrm/` CSR
duplication). Struct sizes are from faithful field-order replicas — Go layout is deterministic, so
they are exact, and every one is corroborated by an independent heap measurement.

---

## Verdict summary

Round 1's headline — "in-memory model = GoGraph, ~22 B/edge vs Memgraph ≥88 B" — is **half right and
must be split in two**. On the **edge** side GoGraph is decisively best-in-class and the 22 B/edge
figure is confirmed (22.43 B/edge measured at degree 324), beating Memgraph's published 154 B/edge by
~7× and Neo4j's 34 B relationship record by ~2×. On the **node** side GoGraph is **WORSE than both
incumbents**: a measured **378–423 B/node** for one label and two properties, versus Memgraph's
published 204 B/vertex and Neo4j block format's 128 B node block. The cause is three sharded
`map[NodeID]X` Go maps holding data keyed by an ID that is *already dense*, while the edge side long
ago moved to dense slot columns. Worse, the whole advantage is **degree-conditional**: a ~392 B
per-source-node fixed overhead means GoGraph loses to Neo4j's record format below degree 8. Two more
[NEW] defects are outright bugs of omission: **`Compact()` never re-evaluates sparse↔dense, so the
flagship FOR bit-packing never fires on the fused bulk-build path (30.5% of edge memory wasted, and
GoGraph's own showcase example measures that slow path)**, and **deleting 90% of a graph reclaims
0.0% of the heap**.

Single most valuable lever from the incumbents: **Memgraph's `PropertyStore`** — a 12-byte field
inside the vertex with an 11-byte inline small-buffer and width-tagged values, giving *zero
allocations* for a node with a few small properties, where GoGraph pays 132.78 B for a single
boolean.

---

## Feature-by-feature comparison

| Feature | GoGraph (file:line) | Neo4j | Memgraph | Verdict | Label |
|---|---|---|---|---|---|
| Edge topology, high degree | `graph/adjlist/adjlist.go:278` SoA `adjEntry`; **8.71 B/edge @d=324** | 34 B rel record (`RelationshipRecordFormat.RECORD_SIZE=34`), doubly-linked chain | 32 B `Edge` + 56 B `Delta` + 24 B skip-list + 2×24 B `EdgeTriple` ≈ 160 B | **BETTER** (both) | CONFIRMED-R1 |
| Edge topology, low degree (d≤6) | **70.14 B/edge @d=2, 38.97 @d=4** (392 B/node fixed cost) | 34 B/rel flat, no per-node amortisation | ~160 B/edge flat | **WORSE than Neo4j**, better than Memgraph | NEW |
| Edge + 1 property | **15.60 B/edge @d=324** (set-after), 41.98 @d=16, 119.9 @d=4 | 34 + 41 B property record = 75 B | ~176 B | BETTER @d≥8, **WORSE @d≤6** | NEW |
| Edge-property representation | Columnar SoA, de-boxed, Arrow validity, COO/dense hysteresis, FOR bit-pack (`graph/lpg/edge_property_column.go:114`) | Property record chain, 32 B payload | Byte-buffer `PropertyStore`, width-tagged | **BETTER** (nobody else is columnar in RAM) | CONFIRMED-R1 |
| FOR bit-packing actually firing | **Never on the fused build path** (`edge_property_column.go:408`) | n/a | n/a | Feature present, **inert on 1 of 2 paths** | NEW |
| Node storage (1 label + 2 props) | **378.6 B/node** (int64 keys), **423.0** (Cypher string keys) | **128 B** block / 56 B aligned | **~204 B** (published formula) | **WORSE** (both) | NEW |
| Node label store | `map[NodeID]labelBag` `lpg.go:201` — **117.53 B/node** | inline, 5 label bytes in the 15 B node record | 4 B/label in a `small_vector` | **WORSE** (both) | NEW |
| Node property store | `map[NodeID]propBag` `lpg.go:191` — **132.78 B/node for one bool** | inline in a 41 B property record | **0 B** — fits the 11 B inline buffer | **WORSE** (both) | NEW |
| Node `PropertyValue` boxing | `v any` (`graph/lpg/property.go:39`), 24 B + heap box; **+16 B/int64 prop measured** | primitive in record | tagged bytes, no boxing | WORSE | CONFIRMED-R1 |
| Edge label store | slot column `labels []uint32` (`adjlist.go:283`) + lazily-nil overflow (`lpg.go:234`) — **exactly 4 B/edge measured** | 4 B rel type in the record | 4 B `EdgeTypeId` in `EdgeTriple` | PARITY / BETTER | **STALE-R1** (57%-of-heap claim is fixed) |
| Adjacency contiguity | Contiguous `[]NodeID` per source | Pointer-chased doubly-linked chain (+ rel groups over threshold 50) | `small_vector<EdgeTriple>` contiguous | **BETTER than Neo4j record**, parity Memgraph | CONFIRMED-R1 |
| Slack / growth policy | `growCap` uncapped ×2 (`adjlist.go:683`) — **31–33% slack @d=324**, manual `Compact()` | Fixed-size records, zero slack | `std::vector` geometric, same issue | **WORSE than Neo4j** | NEW |
| Memory reclamation on delete | **0.0% reclaimed** (`lpg.go:1481` tombstone-only) | Record id free-list, ids reused | GC unlinks deltas; `FREE MEMORY` purges jemalloc arenas | **WORSE** (both) | NEW |
| Memory-pressure behaviour | No byte budget on stored graph; only `MaxShardCapacity` (slot count) and per-query `MaxResultBytes` | `dbms.memory.transaction.total.max` = 70% heap | `--memory-limit` + `MemoryTracker` → typed abort | WORSE | NEW |
| Out-of-core | `store/csrfile` mmap + `madvise` (`madvise_unix.go:16`), zero-copy typed reinterpretation | page cache over record files | `ON_DISK_TRANSACTIONAL` (RocksDB, "experimental") | **BETTER** | CONFIRMED-R1 |
| Dual representation | Per-query fwd+rev CSR = **+17 B/edge, 1.78× the mutable adjacency** (`cypher/api.go:15395`) | none | none | WORSE | NEW |

---

## Measured data

### Per-edge cost by degree (n = 50,000, weightless, after `Compact()`)

| layer | d=2 | d=4 | d=8 | d=16 | d=32 | d=64 | d=324 |
|---|---|---|---|---|---|---|---|
| topology only | 70.14 | 38.97 | 23.48 | 15.74 | 11.87 | 9.94 | **8.71** |
| + rel type | 74.11 | 43.05 | 27.53 | 19.77 | 15.89 | 13.95 | 13.06 |
| + rel type + 1 date prop | 218.11 | 119.05 | 69.53 | 44.77 | 32.39 | 26.21 | **22.60** |
| + rel type + 3 props | 508.43 | 275.07 | 159.53 | 101.78 | 72.91 | 60.40 | — |

The topology row is exactly `8 + 120/d` (R² ≈ 1.0 against all seven points): 8 B for the `[]NodeID`
element plus a **120 B fixed per-source-node** cost (`adjEntry` 112 B + 8 B slot pointer, both
confirmed by `unsafe.Sizeof`).

### Per-node cost (marginal heap, n = 1,000,000)

| layer | B/node |
|---|---|
| Mapper, `N=int64` | 48.32 |
| Mapper, `N=string` Cypher synthetic key | **92.76** |
| + 1 node label | **117.53** |
| + 2 node props (int64 + 11-char string) | **212.73** |
| **total, int64 keys** | **378.58** |
| **total, Cypher string keys** | **423.02** |

### Node-property store, isolated (n = 500,000)

| value kind | 1 prop | 2 props | 4 props |
|---|---|---|---|
| bool | 132.76 | 164.75 | 228.75 |
| small int (0–255, no heap box) | 132.78 | 164.75 | 228.75 |
| int64 (heap-boxed) | 148.74 | 196.75 | 292.74 |
| 11-char string | 164.75 | 228.75 | 356.75 |

Fixed cost of "this node has any properties at all" ≈ **101 B**; marginal per property = 32 B
(`sizeof(kv)`) + 16 B if the value heap-boxes + 16 B for a short string body.

### Exact struct sizes (`unsafe.Sizeof`, faithful replicas)

| struct | size |
|---|---|
| `PropertyValue` (`property.go:39`) | 24 B |
| `kv` (`propbag.go:56`) | 32 B |
| `propBag` (`propbag.go:69`) | 32 B |
| `labelBag` (`labelbag.go:54`) | 40 B |
| **`adjEntry`** (`adjlist.go:278`) | **112 B** |
| `edgePropCols` (`edge_property_column.go:221`) | 32 B |
| **`edgePropColumn`** (`edge_property_column.go:114`) | **240 B** |

Fixed per-source-node overhead: **120 B** (topology) → **392 B** with one property key → **632 B**
with two → **872 B** with three.

---

## Findings

### F1. `Compact()` never re-evaluates sparse↔dense, so FOR bit-packing never fires on the fused build path  [NEW]  (severity: HIGH)

- **What GoGraph does:** `edgePropCols.Compact()` (`graph/lpg/edge_property_column.go:408`) does
  exactly two things — `compactBacking()` (trim slack) and `maybePackDate()` (FOR bit-pack). It
  **never calls `reshaped()`**. Meanwhile the fused append path deliberately forces the column
  SPARSE and skips `reshaped()` to keep the build O(d) rather than O(d²) — stated in the design
  comment at `edge_property_column.go:266-278`. `maybePackDate` requires a **DENSE** date column
  (`edge_property_column.go:2095`, plus `minPackLength = 32` at line 2058). Net effect: a column
  built through `AddEdgeLabeledWithProperty` is COO-sparse forever, at fill = 1.0, and can never be
  bit-packed. `reshaped()` is reachable only from `set`/`grown`/`grownTo`
  (`edge_property_column.go:489,1125,1143,1164,1194`).
- **Evidence (measured, `probe/`):** identical logical content, built two ways, measured after
  `AdjList.Compact()`:

  | degree | fused `AddEdgeLabeledWithProperty` | `AddEdgeLabeled` + `SetEdgeProperty` | waste |
  |---|---|---|---|
  | 8 | 69.93 B/edge | 67.94 | 2.9% |
  | 16 | 44.98 | 41.98 | 6.7% |
  | 64 | 26.28 | 19.90 | **24.3%** |
  | 324 | 22.43 | 15.60 | **30.5%** |

  The mechanism is confirmed by a second, independent signal: widening the date range from 2192 days
  (12-bit residual) to 40,000 days (16-bit) moves the **set-after** number 15.60 → 16.17 B/edge
  (+0.5 B/edge = exactly the extra 4 bits/slot), while the **fused** number is 22.43 for *both* ranges
  to two decimals — the signature of no packing at all. Byte model: sparse COO = `idx []int32` 4 B +
  `days []int32` 4 B = 8 B/edge (13.06 + 8 + 0.84 = 21.9 ≈ 22.43 ✓); dense + FOR-12 = 1.5 B/edge
  (13.06 + 1.5 + 0.84 = 15.4 ≈ 15.60 ✓). Model and measurement agree to 0.3 B/edge.
- **Aggravating:** the only production caller of the fused path is
  `examples/26_social_scale_bench/main.go:501` — GoGraph's flagship memory-evidence example, whose
  `# bytes_per_edge=` line is the origin of round 1's "~22 B/edge". **GoGraph's own public evidence
  artefact measures its worst memory path.** The Cypher engine does *not* use the fused path (no
  caller in `cypher/` or `store/`), so it already benefits — this is a Go-API/bulk-load defect.
- **Lever:** in `edgePropCols.Compact()`, call `reshaped()` on each column **before**
  `maybePackDate()`. This is the Arrow "pick the physical encoding at `Finish()`, not per `Append()`"
  contract the file's own comments cite (Arrow Columnar Format spec, *Fixed-Size Primitive Layout*);
  the code implements half of it. Freeze-only, O(P) per column, paid once. Secondly, revisit
  `minPackLength = 32`: with the exact byte gate already in place at line 2107, the floor forfeits
  every column of degree < 32 for no measured reason.
- **TCK/ACID impact:** none. This is a purely physical in-memory representation change behind the
  same accessors; the on-disk format is representation-independent (`store/snapshot/properties.go`
  serialises through the public `EdgeProperties` value path), so no snapshot/WAL/recovery change.
  `reshaped()` already returns a value copy, preserving the COW/atomic-publication discipline that
  `trimEntry` (`adjlist.go:1348`) depends on. Gate with a mixed dense/sparse compact-middle-slot
  read-back test plus a benchstat assertion on B/edge.
- **Effort:** **S** (the reshape call), S–M (the `minPackLength` review).

### F2. The node side is 2–3× worse than both incumbents — three Go maps keyed by an already-dense ID  [NEW]  (severity: HIGH)

- **What they do:**
  - **Memgraph:** `struct Vertex` is `static_assert(sizeof(Vertex) == 80)` (`src/storage/v2/vertex.hpp:32-73`)
    with `PropertyStore properties` as a **12-byte inline field**. `PropertyStore`
    (`src/storage/v2/property_store.hpp:38`) is `std::array<uint8_t, sizeof(uint32_t) + sizeof(uint8_t*)>`
    = 12 B, of which **11 bytes are usable inline** (`property_store.cpp:2304,2322`:
    `can_fit_in_local = size < sizeof(buffer)`), using a type-tagged byte encoding with integers
    width-compressed to 1/2/4/8 bytes. Memgraph's own note: *"Compared to a `std::map<PropertyValue>`,
    the `PropertyStore` uses approximately 10 times less memory."* A bool costs 2 B, a small int 3 B —
    both **free**, inline, zero allocations. Published total: **204 B/vertex**
    (<https://memgraph.com/docs/fundamentals/storage-memory-usage>), and that 204 B *includes* a 56 B
    MVCC `Delta` and a ~24 B skip-list node that GoGraph does not have to pay at all.
  - **Neo4j:** labels inline in 5 bytes of the 15 B node record; properties inline in the 32 B payload
    of a 41 B property record, with short strings inlined up to 27–54 characters depending on alphabet
    (`LongerShortString.maxLength`, community/record-storage-engine, tag 5.26.28). Block format
    (5.14+, GA 5.16, EE default 5.22) gives a fixed **128 B `block.x1.db` block per node** holding
    *"typically up to 10 labels, 6-7 properties, and up to 5 relationships"*
    (<https://neo4j.com/docs/operations-manual/current/database-internals/store-formats/>). Because the
    page cache holds byte-identical store pages, **RAM cost = disk cost** for Neo4j.
- **What GoGraph does:** `nodeLabelShards [64]nodeLabelShard` with `m map[graph.NodeID]labelBag`
  (`graph/lpg/lpg.go:201`) and `nodePropShards [64]nodePropShard` with `m map[graph.NodeID]propBag`
  (`graph/lpg/lpg.go:191`), plus a bidirectional `Mapper` (`graph/mapper.go:212`:
  `forward map[N]NodeID` + `reverse []N`).
- **Evidence:** measured 117.53 B/node for one label and 212.73 B/node for two properties (n=1M);
  132.78 B/node for a *single boolean* (n=500k). A structural replica confirms the cause exactly: a
  sharded `map[NodeID]labelBag` costs **117.53 B/node** — identical to the live measurement to two
  decimals — while a **dense `[]labelBag` indexed by the intra-shard index costs 40.37 B/node (2.91×
  better)** and a **dense `[]uint32` single-label slot column costs 4.19 B/node (28.02× better)**.
  `NodeID` is dense by construction: `packNodeID(shard, idx) = idx<<8 | shard`
  (`graph/mapper.go:476`), and the intra-shard index is a contiguous `0..n-1` (`mapper.go:297`) — the
  hash map is hashing an integer that is already a perfect array index.
- **Lever:** apply the **already-shipped edge-label design to nodes**. The edge side solved exactly
  this in #1583/#1633: one `[]uint32` slot column co-located with the data plus a lazily-nil overflow
  map for the rare multi-label case (`adjlist.go:254-264`, `lpg.go:206-239`). Do the same per node:
  a per-shard dense `[]uint32` primary-label column indexed by intra-shard index (4.19 B/node,
  **28×**), with `map[NodeID][]LabelID` overflow only for multi-label nodes; and a per-shard dense
  `[]propBag` (40 B, 2.9×) as the low-risk first step for properties, or Memgraph's small-buffer
  `PropertyStore` for the full win. `adjlist`'s `shardSlots.slots []unsafe.Pointer` (`adjlist.go:232`)
  is the proven template — the atomic-publish-on-grow pattern for a dense per-shard array already
  exists in this codebase and is lock-free on the read path.
- **TCK/ACID impact:** none semantic. Labels and properties keep identical observable behaviour;
  on-disk snapshots already serialise `(NodeID, keyIdx, kind, value)` records independent of the
  in-memory container (`store/snapshot/properties.go`), so no format change. The array must grow
  under the shard write lock and be published via `atomic.Pointer` so lock-free readers see either
  the old or the new backing — the same discipline `adjShard` already implements. Tombstoned/never-
  labelled nodes occupy a 4 B hole, which is why the dense column wins anyway.
- **Effort:** **M** for node labels (direct analogue of shipped code), **L** for node properties.

### F3. Deleting nodes reclaims nothing — measured 0.0%  [NEW]  (severity: HIGH)

- **What GoGraph does:** `RemoveNode` (`graph/lpg/lpg.go:1481`) adds the id to a roaring bitmap and
  strips the label *bitmaps*, and does nothing else. The adjacency entry, its `neighbours`, `labels`
  and aux property columns, the `labelBag`, the `propBag`, and both Mapper directions all survive —
  by design, because *"the underlying Mapper cannot release the index slot (NodeID stability is a
  hard contract)"* (`lpg.go:348-355`). `csr.BuildFromAdjListLive` exists precisely to filter the
  resulting ghost edges (`graph/csr/csr.go:51-56`).
- **Evidence (measured, `probe2/`):** 500,000 nodes each with 1 label, 1 string property and 8
  out-edges = 261.6 MiB. After `RemoveNode` on 90% of them: **261.7 MiB — 0.0% reclaimed** (it grew,
  by the tombstone bitmap). `AdjList().Size()` still reports 4,000,000 edges; `TombstoneCount()` =
  450,000.
- **What they do:** Neo4j puts deleted records on per-store id free-lists (`*.id` files) and reuses
  them for subsequent creations, so a delete-heavy workload plateaus. Memgraph's two-phase
  `CollectGarbage` (`src/storage/v2/inmemory/storage.cpp:2931`) unlinks and then frees deltas and
  skip-list nodes on a `--storage-gc-cycle-sec` (default 30) cycle, and `FREE MEMORY` calls
  `memory::PurgeUnusedMemory()` → `je_mallctl("arena.all.purge")`
  (`src/memory/global_memory_control.cpp:148`) to return pages to the OS.
- **Lever:** NodeID stability forbids reusing the *slot*, but nothing forbids freeing the *payload*.
  On `RemoveNode`, (a) publish a nil/empty `adjEntry` for the node and remove its arcs from the
  neighbours' entries — the machinery already exists (`RemoveAllEdgesFrom`, `compactEntry`); (b)
  `delete` its `labelBag` and `propBag` shard entries; (c) keep only the Mapper `reverse` slot and the
  tombstone bit. This turns a monotonic leak into an O(degree) delete. A cheaper interim: an explicit
  `Graph.Vacuum(ctx)` that sweeps tombstoned ids and frees their payload, matching Memgraph's explicit
  `FREE MEMORY`. Also note the tombstone set itself is copy-on-write cloned per delete
  (`lpg.go:1488-1497`) — O(tombstones) per removal, so a bulk delete of *t* nodes is O(t²) bitmap
  work; batch it under the existing `ApplyAtomically` window.
- **TCK/ACID impact:** the tombstone set is already durable (`tombstones.bin`, `TombstonedIDs`), and
  recovery rebuilds the graph from snapshot + WAL, so freeing *derived* in-memory state cannot lose
  committed data. Care needed on the revive path (`AddNode` un-tombstones and calls
  `restoreLabelBitmaps`, `lpg.go:1530`) — if the bag is freed, revive must produce a bare node, which
  is the correct openCypher semantics for re-creating a deleted node anyway. Gate with the existing
  `tombstone_durability_test.go` plus a new reclamation assertion.
- **Effort:** **M**.

### F4. `edgePropColumn` is 240 B, of which ~192 B are permanently-nil slice headers  [NEW]  (severity: MEDIUM)

- **What GoGraph does:** `edgePropColumn` (`graph/lpg/edge_property_column.go:114-194`) declares nine
  slice headers — `i64, f64, boolBits, days, str, boxed, packed, valid, idx` — of which **at most one
  of the six typed backings and at most one of `{packed, valid, idx}` is ever non-nil**, by the
  struct's own documented invariants. Measured `unsafe.Sizeof` = **240 B** per (source-node ×
  property-key). With `edgePropCols` (32 B) that is 272 B of metadata for a column that may hold as
  little as 16 slots × 4 B = 64 B of data.
- **Evidence:** the whole `44.98 − 19.77 = 25.2 B/edge` cost of adding one date property at d=16 is
  dominated by this: `(32 + 240)/16 = 17.0 B/edge` of pure struct, versus 4 B of actual data. At d=4
  it is 68 B/edge. At d=324 it is 0.84 B/edge — the reason the tier looks excellent only on hubs.
- **What they do:** Arrow's own model for a fixed-width primitive array is exactly *one* data buffer +
  *one* validity buffer, with the logical type carried as metadata rather than as a separate typed
  field per kind (Arrow Columnar Format specification, *Fixed-Size Primitive Layout*). DuckDB's
  `Vector` likewise carries a single `data` pointer reinterpreted by `VectorType`/`LogicalType`.
- **Lever:** collapse the five **noscan** numeric backings (`i64, f64, boolBits, days, packed`) into a
  single `data []uint64` word buffer discriminated by the existing `kind`/`packedDate` fields,
  keeping `str []string` and `boxed []PropertyValue` separate (they are GC-scanned and must stay so).
  240 → ~144 B, a **40% cut**, with GC-scan characteristics unchanged. At d=16 that is ~6 B/edge
  (13% of the total). A larger variant splits `edgePropColumn` into a small header plus a per-kind
  concrete type behind a 16 B interface; measure both.
- **TCK/ACID impact:** none — internal representation only, same accessors, same
  representation-independent serialisation.
- **Effort:** **M**.

### F5. Adjacency growth is uncapped ×2, unlike Go's own runtime, and `Compact()` is manual  [NEW]  (severity: MEDIUM)

- **What GoGraph does:** `growCap(cur) { if cur < 4 { return 4 }; return cur*2 }`
  (`graph/adjlist/adjlist.go:683`) — pure doubling with no taper, for `neighbours`, `weights`,
  `handles` and `labels` alike.
- **What Go itself does:** `runtime.nextslicecap` (`$GOROOT/src/runtime/slice.go:333-341`) doubles only
  below a `threshold = 256` and then converges to ~1.25× (`newcap += (newcap + 3*threshold) >> 2`),
  with the source comment *"Transition from growing 2x for small slices to growing 1.25x for large
  slices"* — precisely to bound waste on large slices.
- **Evidence:** measured pre-`Compact()` slack, n=50,000: **33.4% at d=324** (13.07 → 8.71 B/edge),
  32.7% with rel types, 31.4% with a property; 25.3% at d=20, 19.1% at d=10. (Power-of-two degrees
  show 0% because doubling lands exactly — a trap for anyone benchmarking at d=16 or d=64.) The
  layered run at d=324 showed **65.41 MiB reclaimed by `Compact()` on a 209 MiB graph**.
- **Neo4j has zero slack** — fixed-size records packed into pages (`NodeRecordFormat.RECORD_SIZE=15`,
  546 records per 8 KiB page). Memgraph's `small_vector` has the same geometric issue, so this is a
  Neo4j-only lever.
- **Lever:** adopt Go's own taper in `growCap` (2× below 256, ~1.25× above). One function, bounded
  blast radius, and it makes the *un-compacted* steady state — which is what a long-running mutable
  graph actually has, since `Compact()` is an explicit user call — 4× closer to tight. Optionally
  auto-trim an entry when its slack exceeds a threshold at the end of a commit window
  (`EndCommit` already walks the dirty shards, `adjlist.go:170`).
- **TCK/ACID impact:** none — capacity is invisible above the slice length; `trimEntry` already
  preserves the nil-vs-empty distinction that downstream code branches on.
- **Effort:** **S**.

### F6. The Cypher engine pays 92.76 B/node for synthetic string keys it never shows anyone  [NEW]  (severity: MEDIUM)

- **What GoGraph does:** `Engine` is hard-wired to `*lpg.Graph[string, float64]`
  (`cypher/api.go:840`), and every created node gets
  `synthKeyPrefix + strconv.FormatUint(n, 16)` = `"__cx_<hex>"` (`cypher/exec/create_node.go:343`,
  prefix at line 73). The function's own doc says *"The key is never visible to Cypher callers; only
  the NodeID is emitted into the row."* Each key is stored twice — `forward map[string]NodeID` and
  `reverse []string` (`graph/mapper.go:212`) — plus the string body.
- **Evidence:** measured at n=1,000,000: `N=int64` Mapper = 48.34 B/node; `N=string` with real
  `"__cx_<hex>"` keys = **92.76 B/node**. That is 44 B/node of pure overhead, and 92.76 B/node in
  total for a dictionary the engine consults only to translate back to the id it already had.
- **What they do:** neither incumbent has an external-key dictionary. Neo4j addresses a node as
  `128 × nodeId` byte arithmetic into `block.x1.db`; Memgraph's `Gid` is the identity.
- **Lever:** two options — (a) recommended: give `lpg.Graph` a *keyless* mode in which `AddNode`
  returns a NodeID without interning anything, and have the Cypher engine's `CreateNode` use it
  (the synthetic key is already derived from a monotonic counter, so nothing is lost); (b) cheaper:
  keep the Mapper but store synthetic keys implicitly — a shard flag saying "this key is
  `"__cx_"+hex(intraIdx)`" so no string is materialised. Option (a) is cleaner and also removes a
  hash lookup from the create path.
- **TCK/ACID impact:** the key never reaches the surface, so no observable Cypher behaviour changes.
  Care: `seedGlobalNodeCounter` (`cypher/exec/create_node.go:346`) walks interned keys on recovery to
  advance the counter, and the snapshot's mapper entries carry the keys — a keyless mode must persist
  the high-water mark instead. That is a snapshot-format touch, so it needs a version bump and a
  recovery test.
- **Effort:** **L**.

### F7. GoGraph's edge advantage is degree-conditional and reverses below degree 8  [NEW]  (severity: MEDIUM)

- **Evidence:** the 392 B per-source-node fixed cost (F4 + `adjEntry`) means the per-edge figure is
  `data + 392/d`. Against Neo4j's flat 34 B relationship record + 41 B property record = **75 B/edge**:

  | degree | GoGraph (rel type + 1 date prop, best path) | Neo4j aligned | Memgraph |
  |---|---|---|---|
  | 2 | 219.7 | 75 | ~176 |
  | 4 | 119.9 | 75 | ~176 |
  | 6 | 85.3 | 75 | ~176 |
  | **8** | **67.9** | 75 | ~176 |
  | 16 | 42.0 | 75 | ~176 |
  | 324 | 15.6 | 75 | ~176 |

  **Crossover at d ≈ 7.** GoGraph beats Memgraph at every degree tested; it loses to Neo4j's record
  format below degree ~7. `adjlist.go:242` itself states the design targets *"the typical low average
  degree of property graphs (4-16)"* — the lower half of that band is where GoGraph is currently
  weakest.
- **Lever:** F4 (240 → 144 B) moves the crossover from d≈7 to d≈5; F1 helps at every degree; the two
  together plus F2 make the low-degree regime competitive. There is no *incumbent* technique to copy
  here beyond "keep the per-entity fixed cost near zero", which is exactly what a fixed-size record
  store buys Neo4j.
- **TCK/ACID impact:** as F1/F4.
- **Effort:** covered by F1 + F4.

### F8. Node `PropertyValue` is still boxed  [CONFIRMED-R1]  (severity: MEDIUM)

- `PropertyValue{ v any; kind PropertyKind }` (`graph/lpg/property.go:39`) — 24 B, and `v any` heap-
  boxes every scalar. Round 1's T2.8 stands unchanged at `6f31f61`.
- **Evidence:** measured cost of the box in isolation — a node with one int64 property in
  `[256, ∞)` costs 148.74 B/node versus 132.78 B/node for one in `[0, 255]` (which
  `runtime.convT64` serves from `staticuint64s` without allocating): **+15.96 B per boxed scalar**.
  A string property costs 164.75 (+31.97: a 16 B `convTstring` header box plus a 16 B body).
- **Caveat that reframes the lever:** boxing is only ~16 B of the 132.78 B baseline. **The Go map
  around it (F2) is ~100 B and is the bigger prize.** De-boxing alone yields ~12% on the node
  property store; de-boxing *inside* a dense columnar node-property tier yields far more. Sequence F2
  first, or do them together.
- **Effort:** **L** (it is the node-side analogue of the whole `edge_property_column.go` tier).

### F9. Per-query CSR duplication costs 1.78× the mutable adjacency  [NEW]  (severity: MEDIUM)

- **What GoGraph does:** `csrPairFromGraph` (`cypher/api.go:15387`) builds a forward **and** a reverse
  `csr.CSR`, cached only in the per-query `buildOpts` (`ensureFwdCSR`, `cypher/api.go:15452`) — so it
  is rebuilt O(V+E) per query and discarded.
- **Evidence:** n=200,000, e=3,199,872, mutable adjacency 66.75 MiB; forward CSR **+25.95 MiB
  (8.50 B/edge)**, reverse CSR **+25.95 MiB (8.50 B/edge)** — total **1.78×**. Note `csr.CSR`
  (`graph/csr/csr.go:33`) carries only topology: no label column, no property columns.
- **What they do:** neither incumbent materialises a second topology copy per query — both traverse
  the primary store directly. (Neo4j's GDS *does* build a separate compressed in-heap projection, but
  it is explicitly created and reused, never per-query.)
- **Lever:** this is the memory face of round 1's already-ranked lever #3 (lock-free per-shard
  snapshot, #1671/#2051) — an engine-lifetime, invalidation-tracked CSR converts a recurring
  O(V+E)-alloc GC churn into a one-time 1.78× resident cost. Cheaper interim: build the reverse CSR
  lazily (only queries with an incoming/undirected traversal need it), halving the cost for the common
  case. Secondary: `vertices []uint64` is 8 B/node where a `uint32` offset suffices below 4 G edges.
- **TCK/ACID impact:** a cached CSR must be invalidated on every committed write; the existing
  visibility barrier (`lpg.Graph.ApplyAtomically` / `LockBarrier`) is the natural hook. Getting this
  wrong is a correctness bomb (stale topology), which is why round 1 flagged tearing risk.
- **Effort:** **S** (lazy reverse) / **L** (cached snapshot, already designed).

### F10. Edge-label map is no longer 57% of resident heap  [STALE-R1]  (severity: n/a — resolved)

- The June 2026 audit's finding is **fixed and verified**. The single relationship type of a live
  edge now lives in the per-slot `labels []uint32` column inside the adjacency entry
  (`adjlist.go:283`), and `edgeLabelShard.overflow` (`lpg.go:234`) is *"allocated lazily, so an
  all-single-label graph never pays for sixteen empty spill maps"* — confirmed at `lpg.go:250`.
- **Evidence:** adding one relationship type per edge costs **exactly 4.00–4.35 B/edge** across the
  whole degree sweep (8.71 → 13.06 at d=324; 15.74 → 19.77 at d=16), i.e. the bare `uint32` column
  and nothing else. No 16-byte `edgeKey` and no map entry. Do not re-raise.

### F11. "~22 B/edge" is confirmed but is neither the floor nor representative  [CONFIRMED-R1, re-scoped]

- 22.43 B/edge is exactly reproducible at d=324 on the fused path — round 1's number is sound.
- But the same content costs **15.60 B/edge** on the set-after path *today* (F1), and **41.98 B/edge
  at d=16** / **119.9 at d=4**. Any future claim should state the degree and the write path.

---

## Columnar-analytics techniques neither incumbent uses

Round 1 credited GoGraph with in-memory columnar edge properties that neither incumbent has. That
stands. Assessing the next steps, in descending value:

1. **Fix what already exists before adding more (F1, F4).** The storage tier is not the laggard
   relative to v0.10.0's execution work — it is *ahead in design and behind in wiring*. The
   sparse↔dense reshape and the FOR packer are both written, tested and shipped; one of the two write
   paths simply never reaches them. That is a 30.5% win for a one-line change and it dominates every
   new-encoding idea below.

2. **Width-adaptive integer columns (Arrow / ClickHouse `LowCardinality`).** Two candidates:
   - **`neighbours []graph.NodeID` (uint64) → `[]uint32`** when `MaxNodeID < 2^32`. This is the single
     largest edge line item — 8 of the 8.71 B/edge topology floor, and 8 of the 15.60 B/edge best case
     (51%). `packNodeID` (`mapper.go:476`) already packs `idx<<8 | shard`, so 4 bytes addresses
     16.7 M nodes/shard × 256 shards = 4.29 G nodes. **Neo4j deliberately does this** — its node
     record stores a 4-byte id plus high-order bits rather than an 8-byte id. Implement as a
     freeze-only promote/demote exactly like `maybePackDate`, so `Compact()` narrows and any write
     that exceeds the range widens back. **~4 B/edge, 25–46% of the edge cost. Effort M.**
   - **`labels []uint32` → width-adaptive u8/u16.** Relationship-type cardinality is typically 2–20;
     one byte suffices. 4 → 1 B/edge (another 6–19%). Requires widening the `adjlist` `AuxColumn`
     contract, which currently hard-codes "one OPAQUE 4-byte value per slot" (`adjlist.go:254`).
     **Effort M.** Prefer this to RLE: run-length would need a decode branch inside the traversal hot
     loop, and my prior finding stands that DuckDB does not compress in-memory vectors by default.

3. **Delta + varint / Elias-Fano on the neighbour column — conditionally, and this is a cross-stream
   observation.** Delta encoding requires *sorted* adjacency, which GoGraph deliberately does not have
   (`adjlist.go:241-243`). Round 2's KEYSTONE finding is **degree-adaptive sorting above d≈64**. If
   that ships, every sorted hub becomes delta-encodable for free, and sorted-hub neighbour lists
   compress to ~2–3 bits/edge with Elias-Fano at typical densities. **The two findings compound: round
   2's sorting lever unlocks a storage lever it did not itself claim.** Worth recording, not worth
   filing until the sorting lands.

4. **Node-property dictionary / small-buffer.** Do **not** add a dictionary in memory (the DuckDB
   no-in-memory-compression finding holds). Instead take **Memgraph's `PropertyStore`** — a
   type-tagged byte buffer with an 11-byte inline small-buffer and width-compressed integers. It is
   not "compression", it is de-boxing plus inlining, and it is strictly the right model for the node
   side (F2/F8). Reserve real dictionary encoding for `store/csrfile`, where it is paid once at
   checkpoint.

5. **Already done, do not re-propose:** Arrow validity bitmaps (`edge_property_column.go:152`,
   omitted at null-count 0), bit-packed bools, COO/dense hysteresis with a derived break-even
   (`breakevenFill`, line 720), FOR bit-packing (line 2068), tiered `NodeSet`/`propBag`/`labelBag`
   small-value unions.

---

## Nothing-to-take list

- **Neo4j's disk-page-oriented RAM model / off-heap page cache.** Round-1 consensus rejection, and
  nothing here overturns it. Neo4j is page-oriented because it must be: its RAM *is* its disk image.
  That forces the 128 B fixed block (charged even for an empty node), the 8 KiB page granularity, the
  string-store indirection above 27–54 characters, and a spill hierarchy of five separate store files.
  GoGraph's RAM-native model has no such constraint. **Take the *inlining discipline*, not the
  paging.**
- **Memgraph's MVCC delta chains.** 56 B per change (`static_assert(sizeof(Delta) <= 56)`,
  `src/storage/v2/delta.hpp`), pinned indefinitely by any long-running read transaction
  (`OldestActive()` watermark, `storage.cpp:2993`). It is 27% of their 204 B vertex and 36% of their
  154 B edge. GoGraph already rejected MVCC on correctness grounds (Fekete 2005) and gets the memory
  saving as a bonus. Reject.
- **Memgraph's skip-list storage.** `kSkipListMaxHeight = 32`, ~8 B fixed + ~16 B expected pointers =
  ~24 B per vertex *and* per edge, up to +256 B for a max-height tower
  (`src/utils/skip_list.hpp:61,155,177`). GoGraph's `shardSlots.slots []unsafe.Pointer` gives O(1)
  direct indexing at 8 B/node with no tower. Strictly better. Reject.
- **Neo4j's doubly-linked relationship chains + relationship groups.** Four link pointers per 34 B
  relationship record and a random page lookup per hop; relationship groups add a *second*
  indirection for dense nodes and the dense flag is *irreversible*. Neo4j's own justification for
  block format is escaping this. GoGraph's contiguous `[]NodeID` per source is strictly better for
  traversal. Reject.
- **Memgraph's `--storage-property-store-compression-enabled` (zlib).** In-memory heavyweight
  compression on the read path; matches round 1's explicit "in-mem zlib" rejection and the DuckDB
  in-memory finding. Reject. (Their `--storage-floating-point-resolution-bits` 32/16 is a *lossy*
  precision knob that would violate openCypher float semantics. Reject.)
- **Memgraph's `IN_MEMORY_ANALYTICAL` mode** (disable deltas for 6× faster import). GoGraph has no
  deltas to disable; the equivalent is already the default.

## Things that are genuinely GoGraph's alone

- **In-memory columnar, de-boxed, Arrow-validity edge properties with FOR bit-packing.** Confirmed
  unique against both incumbents' documented designs. Both store per-edge properties as a byte buffer
  or record chain *per edge*; neither has a per-source-node column with a shared validity plane.
- **`Config.Weightless`** (`adjlist.go:120`) — an explicit "this graph has no edge weights" mode that
  drops the whole weights column. Measured 8 B/edge. Neither incumbent has an equivalent for their
  property machinery (Memgraph's `--storage-properties-on-edges=false` is the closest analogue and is
  all-or-nothing).
- **Zero-copy mmap out-of-core CSR** (`store/csrfile`, `madvise_unix.go:16` with SEQUENTIAL/RANDOM/
  WILLNEED/DONTNEED hints, typed reinterpretation with no parse and no heap copy). Neo4j's page cache
  parses records out of pages; Memgraph's `ON_DISK_TRANSACTIONAL` is RocksDB-backed, row-oriented, and
  self-described as *"still in the experimental phase"* with the further caveat that *"all the graph
  objects used in the transactions still need to be able to fit in the RAM"*. GoGraph's semi-external
  PageRank keeping only the rank vector resident is a capability neither has.

## Open gap with no lever proposed

**Behaviour under memory pressure.** GoGraph has no byte budget on the stored graph — only
`Config.MaxShardCapacity` (a *slot count*, `adjlist.go:81`) and the per-query `MaxResultBytes` /
`byte_budget.go` breaker budget. Memgraph has a hierarchical `MemoryTracker` and aborts the offending
query with a typed `OutOfMemoryException` (`src/utils/memory_tracker.cpp:147`), preserving the
instance; Neo4j caps transaction memory at 70% of heap by default
(`dbms.memory.transaction.total.max`). A GoGraph embedder that overcommits gets an OS OOM kill of the
*host application*. Whether an embeddable library should own a global memory budget — or should
expose a resident-bytes estimator and let the host decide — is a scope/architecture question, not a
technical one. Per the decision-autonomy rule I am flagging it rather than proposing a design.
