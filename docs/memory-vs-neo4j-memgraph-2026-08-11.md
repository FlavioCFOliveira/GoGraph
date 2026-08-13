# Memory efficiency: GoGraph vs Neo4j and Memgraph

**Date** 2026-08-11 · **GoGraph baseline** `b3887752` · **Peers** Neo4j `5.26.9` (image `neo4j:5.26-community`), Memgraph `v3.9.0` (image `memgraph/memgraph:3.9.0`), both read from full local clones at those exact tags.

This is the resource-efficiency counterpart to
[`concurrency-vs-neo4j-memgraph-2026-08-11.md`](concurrency-vs-neo4j-memgraph-2026-08-11.md).
That audit measured how the three engines behave under concurrent load and found GoGraph
scaling best of the three. This one measures the other half of the
[ULTRA EFFICIENT by Design](../CLAUDE.md#4-ultra-efficient-by-design) mandate: **how many bytes
of memory each engine spends to hold the same graph, and on what.**

> **Remediation.** This document records the state at `b3887752` and is not revised as findings
> are fixed. Every figure below is the pre-remediation measurement. Sprint 339 has since
> addressed F1, F4 and F7:
>
> | | measured here | after sprint 339 |
> |---|---:|---:|
> | typed relationship | 1 326.61 B | **483.92 B** |
> | node, 1 label, 3 properties | 505.48 B | **320.72 B** |
> | 4 M-relationship fixture in 8 GB | **OOM-killed** | completes |
>
> See [`benchmarks/edge-instmap-2026-08-11.md`](benchmarks/edge-instmap-2026-08-11.md) and
> [`benchmarks/propbag-bytestream-2026-08-11.md`](benchmarks/propbag-bytestream-2026-08-11.md).
> The property-carrying-relationship fixture needed one further thing the audit did not consider:
> **`GOMEMLIMIT` is set nowhere in the module**, so the Go runtime targets twice the live heap
> with no knowledge of a container cap. Setting it to the cap is what let that fixture finish
> (#2407).

---

## 1. Verdict

**GoGraph is the least memory-efficient of the three engines, by a wide and precisely
attributable margin — and on the relationship write path the gap is severe enough to be an
availability defect, not merely an inefficiency.**

Two findings, in order of severity.

### The relationship write path costs 1 079 B/edge, and it OOM-killed the engine

A relationship created through **Cypher** costs **1 078.87 B**, against **45.21 B** for the same
relationship written through the Go API — a **23.9× amplification** that belongs entirely to the
query path's own bookkeeping. A heap profile attributes **87.9 % of it to two nested per-edge
maps that both store the relationship type the adjacency already holds in 4 bytes** (§4.3).

This is not theoretical. Loading a 500 000-node, degree-8 graph into an 8 GB container, the
kernel **OOM-killed `ggserver` at 8 370 788 kB while creating the 3.5-millionth relationship**:

```
oom-kill:constraint=CONSTRAINT_MEMCG … task=ggserver
Memory cgroup out of memory: Killed process 117896 (ggserver) anon-rss:8370788kB
```

The other two engines held the whole graph comfortably. For the identical fixture — 500 000
`:Person` nodes with an index, 4 000 000 `:KNOWS` relationships — in identical `--cpus=4 -m 8g`
containers:

| Engine | Resident (`anon`) holding the complete graph |
|---|---|
| **Memgraph** | **658 MB** |
| **Neo4j** | 2 774 MB, of which 2 048 MB is the pinned heap; 223 MB on disk |
| **GoGraph** | **OOM-killed at 8 371 MB, having reached ~3.5 M of the 4 M relationships** |

**This is a direct breach of the "Bounded resources … Backpressure, never panic" clause of the
reliability mandate**: the engine did not degrade, throttle or return a typed error — it was
killed.

### Nodes cost 3.4× Memgraph, and the cause is structural

For a node with one label and three properties GoGraph spends **505 B/node against Memgraph's
147 B**. The cause is not diffuse and it is not "Go overhead":

> **GoGraph has no per-node record.** Every attribute of a node lives in a *separate sharded Go
> hash map keyed by node id* — one map for labels, one for properties, one for the external key.
> A Go map entry costs roughly 2.3–2.5× the bytes of the value it holds, and GoGraph pays that
> multiplier three times per node. Memgraph stores a node as **one 80-byte object** with its
> labels, properties and adjacency inline.

Measured at 1 000 000 nodes carrying one label and one 9-character string property, a heap
profile attributes **375 B/node** as: 111 B to the label map, 101 B to the property map, 71 B to
the identity map, 33 B to the property bag's slice, 18 B to an interface box — and **17.3 B to
the string the user actually asked to store.** Under 5 % of what GoGraph spends on that node is
the node's data.

### Three qualifications, all measured rather than assumed

- **GoGraph's storage layer is not the problem; the layers above it are.** The adjacency costs
  41.21 B/edge and a relationship *type* costs exactly **4.00 B/edge** — the width of the `uint32`
  label column. The columnar edge-property tier stores an `int64` in **7.95 B**, i.e. the payload
  and nothing else. Every large number in this report comes from a map above that layer.
- **GoGraph's secondary index is the cheapest of the three in RAM** (71 B/entry against
  Memgraph's 117 B) — but it is a *hash* index that cannot serve ranges, so this is not a
  like-for-like win. See §4.4.
- **Neo4j must not be ranked on resident memory.** Its graph lives in fixed-width records in
  files; RAM is a demand-paged cache over them, and its own tree runs databases at an 8 MiB cache
  (`ConfigurableStandalonePageCacheFactory.java:78`). It holds the same graph in **123 B/node and
  40 B/relationship on disk**. Memgraph's in-memory storage has no equivalent and cannot hold a
  graph larger than RAM. **GoGraph does have an out-of-core tier** — the mmap-backed immutable
  CSR in `store/csrfile` — but it is not what the Cypher engine reads: everything measured here
  is the mutable `graph/lpg` engine, which is wholly resident.

---

## 2. Method

### 2.1 The measurement is a slope, never a difference

Every figure below is the **least-squares slope of resident bytes against element count**, taken
over nine readings as the fixture grows from empty to 1 000 000 elements in eight equal steps,
each reading preceded by a forced quiesce. The R² of each fit is reported so the reader can see
how straight the readings actually were.

This is not fastidiousness. A first version of this harness took a single before/after difference
and reported **a three-property node as cheaper than a one-property node** — an impossibility.
All three engines grow in steps (Go maps double their bucket array; the JVM heap grows in
regions; an allocator takes arenas in chunks), and a difference taken across such a step charges
the whole step to whichever elements happened to be loading when it fired. The fit spreads the
steps across the range.

Absolute footprint is never compared. An empty Neo4j already holds a JVM, a page cache and a
system database; an empty Memgraph already holds its allocator arenas. The marginal slope cancels
every fixed term, and it is the figure an operator can multiply by their own graph's size.

### 2.2 Each shape gets a freshly started engine

Every shape is measured in a container started for that shape and holding nothing else, so the
structures whose growth is measured start empty. All phases only ever **add** data: none of the
three engines returns freed memory promptly, so a sequence that deleted between readings would
report allocator hysteresis as engine footprint.

### 2.3 The instrument, and its cross-check

Readings come from the container's own **cgroup v2** accounting — the one instrument all three
engines are measured by identically — split into `anon` (private pages: heap, stacks, arenas) and
`file` (page cache). Each is cross-checked against the engine's own counter:

| Engine | Quiesce | Own counter | Agreement with cgroup `anon` |
|---|---|---|---|
| GoGraph | `GET /gc` → `runtime.GC()` ×2 + `debug.FreeOSMemory()` | `runtime.MemStats.HeapAlloc` | `heap_alloc` runs 8–10 % below `anon`, the difference being runtime structures outside the heap |
| Memgraph | `FREE MEMORY` | `SHOW STORAGE INFO` → `graph_memory_tracked` | **within 2 % at every shape** |
| Neo4j | `jcmd GC.run`, then wait out a timed checkpoint | `jcmd GC.heap_info` | n/a — see §2.5 |

Memgraph's agreement is the strongest evidence that the instrument is sound: two independent
accountings, one inside the engine and one in the kernel, agree to within 2 % across four shapes.

### 2.4 Which number means "the RAM this graph needs"

| Engine | Metric | Why |
|---|---|---|
| GoGraph | `anon` | Wholly in memory; nothing is file-backed |
| Memgraph | `anon` | Wholly in memory. Its `file` term is the write-ahead log, which is evictable |
| Neo4j | `disk` for the data, `page_cache` for the RAM it *wants* | The graph is on disk; RAM is a cache whose size is configured, not organic |

### 2.5 Configuration, stated because it changes the numbers

- All four containers: `--cpus=4 -m 8g`, one Docker network.
- **Neo4j heap was pinned** at `initial=max=2G` with `pagecache=4G` and
  `db.checkpoint.interval.time=5s`. Left at defaults the JVM heap grows in ~40 MB regions that
  swamp the data signal entirely, and its `anon` slope is a configuration artefact rather than an
  engine property — visible below as R² of 0.73 on `anon` against 1.0000 on `page_cache`.
  **Neo4j 5.26 Community has no `db.checkpoint()` procedure** (it is Enterprise-only); an earlier
  version of this harness called it on every reading and failed.
- **The Memgraph image overrides its own source defaults.** `/etc/memgraph/memgraph.conf` in
  `memgraph/memgraph:3.9.0` sets `--storage-wal-enabled=true`, `--storage-snapshot-on-exit=true`
  and **`--storage-properties-on-edges=true`**, while the source at v3.9.0 declares all three
  `false` (`src/flags/general.cpp:74,77,94`). This corrects a claim recorded in the concurrency
  audit: the shipped image *is* running a WAL. It also means the shipped image runs Memgraph's
  single largest memory lever in its **expensive** position — measured as an A/B in §4.3.
- GoGraph's `ggserver` is **in-memory only**: no WAL, no snapshot. Its `file` and `disk` terms are
  therefore zero by construction, and the durability comparison is out of scope here.

### 2.6 Two harness defects found and fixed — both produced plausible wrong numbers

Recorded because each is a general trap, not a one-off:

1. **`go test` replayed a cached run.** The shape is selected by an environment variable read in a
   package-level initialiser, which Go's test cache does not track. A four-shape sweep ran to
   completion with **exit 0 on all four**, and all four logs contained `shape=bare` with identical
   fits — the only tell was `ok … (cached)` on the last line. Three of four shapes never executed.
   The driver now passes `-count=1`, asserts the log names the shape it asked for, and fails on
   `(cached)`.
2. **An in-process probe measured "new graph minus old graph".** The same build reported 93.06
   B/node in one test and 44.53 in another; the difference, 44.09, was exactly the previous arm's
   graph, still reachable from a returned frame at baseline time and collected during the next
   build. Each probe now runs in its own process and asserts its baseline is under an 8 MiB floor.

A third instrument fault was caught by the same reflex: a shell loop meant to vary node count and
degree produced three *identical* results, because zsh does not word-split an unquoted expansion,
so both parameters silently fell back to their defaults. **Two runs of a parameterised experiment
producing identical output is evidence of a broken harness, not of a robust result.**

---

## 3. What each engine stores, read at the tested tags

### 3.1 Memgraph — one object per vertex, 80 bytes, zero padding

`src/storage/v2/vertex.hpp:26-69`, with the repository's own assertion:

```cpp
struct Vertex {
  const Gid gid;                                  //  8
  utils::small_vector<LabelId> labels;            // 16
  utils::small_vector<EdgeTriple> in_edges;       // 16
  utils::small_vector<EdgeTriple> out_edges;      // 16
  PropertyStore properties;                       // 12
  mutable utils::RWSpinLock lock;                 //  4
  utils::PointerPack<Delta, 2> delta_;            //  8
};
static_assert(sizeof(Vertex) == 80, "If this changes documentation needs changing");
```

- **Labels are free up to two.** `small_vector`'s inline capacity is `sizeof(T*)/sizeof(T)` =
  8/4 = 2 (`src/utils/small_vector.hpp:545`), so a node with one or two labels allocates nothing.
- **Properties are a 12-byte object with an 11-byte inline buffer.**
  `property_store.hpp:188` is `std::array<uint8_t, sizeof(uint32_t) + sizeof(uint8_t*)>` — the
  whole object. A payload under 12 bytes lives inside it with **zero heap**
  (`property_store.cpp:2303-2324`). Beyond that it is one `new uint8_t[]` rounded to a multiple
  of 8.
- **The property encoding is a sorted byte stream, not a map.** Each record is
  `[metadata 1B][property id 1/2/4/8B][payload]`, where the metadata byte packs a 4-bit type and
  two 2-bit width selectors (`property_store.cpp:99-113`; types at
  `property_store_types.hpp:17-35`). Booleans and nulls consume **zero** payload bytes — the
  value rides in the metadata nibble. `{sid:"p00000001", name:"person-1", age:25}` encodes to
  **26 bytes**, rounded to a 32-byte allocation.
- **A vertex sits in a skip list**, costing 8 fixed bytes plus 8 per level, with a geometric
  height distribution of p=½ so E[height]=2 (`src/utils/skip_list.hpp:85-131`) — **24 B of
  average overhead per vertex**, giving 104 B requested per vertex node.

**Predicted from source: 104 B/vertex. Measured: 111.16 B (cgroup) / 109.80 B (Memgraph's own
counter).**

### 3.2 Neo4j — fixed-width records in files, RAM is a cache

| Record | Bytes | Citation |
|---|---|---|
| Node | **15** | `standard/NodeRecordFormat.java:31-32` |
| Relationship | **34** | `standard/RelationshipRecordFormat.java:31-35` |
| Property | **41** = 9 header + 4 blocks × 8 B | `standard/PropertyRecordFormat.java:34-39` |
| Relationship group | 25 | `standard/RelationshipGroupRecordFormat.java:31-38` |

The default format at 5.26 is **`aligned`** (`GraphDatabaseSettings.java:179-190`), which is
`standard` with pages padded so no record straddles an 8 KiB boundary — costing 0.02 %–0.40 % of
file size depending on the store.

- **Labels inline into the node's 5-byte label field** for up to 7 labels, provided each id fits
  in `36/count` bits (`InlineNodeLabels.java:41,115-131`). One label: inlined, zero extra bytes.
- **Short strings inline into property blocks** through twelve hand-tuned alphabets at 4–7 bits
  per character (`LongerShortString.java`, `ShortStringCodec.java`). Both `"p00000001"` and
  `"person-1"` select the 6-bit URI codec and occupy 2 blocks each; `age:25` occupies 1. Five
  blocks against a 4-block record means **two property records, 82 bytes, of which 24 payload
  bytes are dead space.** Neither string reaches the dynamic string store.
- **The page cache costs 8224 bytes per resident 8 KiB page** — 8192 for the page plus 32 bytes
  of off-heap metadata (`PageList.java:54`), i.e. 0.39 % overhead — plus 4 bytes of Java heap per
  file page for the translation table.

**Predicted from source: 15 B/node record. Measured on disk: 15.68 B/node (R² 0.9999).**

### 3.3 GoGraph — sharded hash maps keyed by node id

There is no node struct. `graph/lpg/lpg.go:353` declares a `Graph` holding, among others:

```go
nodeLabelShards [propMapShards]nodeLabelShard   // map[graph.NodeID]labelBag
nodePropShards  [propMapShards]nodePropShard    // map[graph.NodeID]propBag
```

with `propMapShards = 64`, and identity held separately in `graph.Mapper`, whose shards each keep
**both** a `map[N]NodeID` and a `[]N` reverse slice (`graph/mapper.go:311-353`):

```go
idx := uint64(len(s.reverse))
id := packNodeID(shardIdx, idx)
s.reverse = append(s.reverse, k)
s.forward[k] = id
```

So a node's existence, its labels and its properties are three separate hash-map entries in three
separate maps, and the key is stored twice.

Compounding this, **the key is a string that Cypher invents and never exposes.**
`cypher/exec/create_node.go:357-360`:

```go
// freshNodeKey returns a string key that is guaranteed to be unique within the
// current process by drawing from a global monotonic counter. The key is never
// visible to Cypher callers; only the NodeID is emitted into the row.
func (op *CreateNode) freshNodeKey() string {
	n := globalNodeCounter.Add(1)
	return synthKeyPrefix + strconv.FormatUint(n, 16)
}
```

Neo4j and Memgraph both identify a node by a dense integer and have no equivalent structure.

---

## 4. Measured results

All figures are bytes per element, from the least-squares slope over 9 readings at
N = 1 000 000 (nodes) or 4 000 000 (edges). Full logs in the harness output; raw fits carry their
R².

### 4.1 Nodes

RAM to hold one node, by node shape:

| Node shape | **GoGraph** `anon` | **Memgraph** `anon` | **Neo4j** RAM (anon+cache) | Neo4j on disk |
|---|---:|---:|---:|---:|
| one label, no properties | **207.98** | **111.16** | 109.07 | 15.68 |
| + one 9-char string property | **354.64** | **130.61** | 248.18 | 83.11 |
| + two strings and one integer | **505.48** | **147.34** | 413.79 | 122.75 |
| one property, **with a secondary index** | **425.67** | **247.83** | 344.77 | 159.59 |

R² — GoGraph 0.918–0.968; Memgraph 0.9971–0.9999; Neo4j `page_cache` and `disk` 0.91–1.0000,
`anon` 0.73–0.95 (a pinned 2 GB heap, so its slope is near-noise and is not used).

Derived marginal costs:

| What is being added | GoGraph | Memgraph | Ratio |
|---|---:|---:|---:|
| one 9-character string property | **146.66** | **19.45** | **7.54×** |
| two strings + one integer (vs bare) | **297.50** | **36.18** | **8.22×** |
| a secondary index entry over a string key | **71.03** | **117.22** | **0.61×** ✅ |

**GoGraph's per-property cost is the single worst result in this audit**, and Memgraph's 19.45 B
is exactly what its architecture predicts: a 12-byte payload rounded to a 16-byte allocation,
with the 12-byte `PropertyStore` object already inside the vertex.

> **A cliff worth knowing.** Memgraph's inline buffer holds **11 bytes**. `{sid:"p00000001"}`
> encodes to 12 bytes and therefore allocates; the same property with an 8-character value
> encodes to 11 and costs **zero heap**. Any cross-engine benchmark that varies property-name or
> value lengths will cross that boundary by accident.

### 4.2 GoGraph's cost, attributed

A heap profile taken while the graph is live, 1 000 000 nodes with one label and one 9-character
string property, total 375.35 B/node:

| Allocation site | B/node | Share | What it is |
|---|---:|---:|---|
| `lpg.setNodeLabelInfo` | 111.25 | 29.8 % | the `map[NodeID]labelBag` shard map |
| `lpg.setNodePropertyInfo` | 101.06 | 27.1 % | the `map[NodeID]propBag` shard map |
| `graph.Mapper.internSlowHook` | 71.05 | 19.1 % | forward map + reverse slice |
| `lpg.propBag.set` | 33.03 | 8.9 % | the `pairs []kv` slice |
| `lpg.StringValue` | 18.35 | 4.9 % | boxing the value into `any` |
| the string value itself | 17.30 | 4.6 % | **the user's data** |
| the synthetic key string | 17.30 | 4.6 % | an identity Cypher never exposes |

**The two sharded maps alone are 212.3 B/node — 57 % of the total.** They hold a 48-byte and a
40-byte payload respectively, so the Go map is applying a **2.3–2.5× multiplier** on top of value
structs that are themselves large. (pprof reports in MiB; the figures above are converted at
2²⁰ bytes and sum to 369.4 of the measured 375.35 B/node, the remainder being runtime
allocations below the profile's reporting threshold.)

Isolating the identity decision with a direct counterfactual — the same 1 000 000 nodes interned
under a string key and under a `uint64` key, nothing else changed:

| Mapper key type | B/node |
|---|---:|
| synthetic string (`__cx_<hex>`, what Cypher creates) | **93.03** |
| `uint64` | **48.52** |

**The invisible string identity costs 44.51 B/node, 1.92× an integer key.**

### 4.3 Relationships — the severe result

RAM to hold one typed relationship at degree 8, measured in the containers:

| Engine and configuration | `anon` B/edge | own counter | R² |
|---|---:|---:|---:|
| **GoGraph** (Cypher over Bolt) | **1 326.61** | 1 138.34 (`HeapAlloc`) | 0.9990 |
| **Memgraph**, image default (`properties-on-edges=true`) | **126.68** | 126.47 | 0.9924 |
| **Memgraph**, `properties-on-edges=false` | **51.84** | 51.75 | 0.9701 |
| **Neo4j** | 42.52 | — | 0.9952 |
| **Neo4j**, on disk | 40.25 | — | 0.9609 |

**GoGraph spends 10.5× Memgraph's shipped default and 25.6× Memgraph's lean configuration per
relationship.** GoGraph's own arm is a 7-point fit rather than 9: the engine was OOM-killed at
step 7 (§1), and the fit uses the readings taken before that.

Adding **one `int64` property** to each relationship separates the three architectures completely:

| Engine | bare edge | edge + one `int64` | cost of the property |
|---|---:|---:|---:|
| **GoGraph** | 1 326.61 | **2 610.91** (R² 0.9985, 4 points) | **+1 284.30** |
| **Memgraph** | 126.68 | 126.38 | **≈ 0** |
| **Neo4j** (`anon`) | 42.52 | 82.96 | +40.44 |
| **Neo4j** (disk) | 40.25 | 82.25 | +42.00 |
| **Memgraph**, `properties-on-edges=false` | 51.84 | *rejected* | — |

- **Memgraph's integer property is free**, because a 3-byte encoded record fits in the 11-byte
  buffer already inside the `PropertyStore` object, which is already inside the `Edge`. No
  allocation occurs at all.
- **Neo4j's costs one property record**, 41 bytes plus page padding — the granularity its
  fixed-width format quantises to.
- **GoGraph's costs 1 284 B**, because the Cypher path writes the property into
  `edgeInstancePropShards` and `edgeHandlePropShards`, the exact property analogues of the two
  label maps of F1, with the same nested `map[edgeKey]map[K]…` shape. **The defect of F1 is a
  class, not an instance.**
- The lean Memgraph arm did not merely cost less — it **refused the write** with a typed error,
  `Can't set property because properties on edges are disabled`, exactly as
  `src/storage/v2/edge_accessor.cpp:171` specifies. That is what its 51.84 B/edge buys and costs.

Memgraph's A/B is the clearest confirmation in this audit that a single documented decision
produces a measured result. Its source predicts an edge with no `Edge` object at **48 B** — two
24-byte `EdgeTriple` entries, one in each endpoint's adjacency, with edge identity held as a
`Gid` inline in the tuple (`src/storage/v2/edge_ref.hpp:20-38`). **Measured: 51.84 B.** Switching
edge objects on adds a 32-byte `Edge` in a skip-list node plus a delta; predicted ~104 B,
**measured 126.68 B**. The cost of the lean mode is functional and steep: no edge properties at
all, and no edge indexes.

#### Where GoGraph's 1 326 B goes

GoGraph's storage layer is not responsible. Driving the *same* graph through the Go API instead
of Cypher, in-process, over 4 000 000 edges:

| GoGraph arm, Go API | B/edge |
|---|---:|
| untyped, **multigraph** | 41.21 |
| untyped, **simple graph** | 41.21 |
| typed (`:KNOWS`) | 45.21 |
| typed + one `int64` property | 87.23 |

**A relationship type costs exactly 4.00 B/edge** — the width of the `uint32` label column
carried parallel to the neighbour array. That is an excellent result and the clearest evidence
that the columnar work paid off.

The same graph built through Cypher, in-process, at 100 000 nodes and 800 000 relationships:
**1 078.87 B/edge — 23.9× the Go-API figure.** A heap profile taken while the graph is live
attributes it:

| Allocation site | B/edge | Share | What it is |
|---|---:|---:|---|
| `lpg.setEdgeLabelAtInfo` | 490.1 | 44.1 % | `edgeInstanceLabelShards`, `map[edgeKey]map[int64]labelBag` |
| `lpg.setEdgeLabelByHandleInfo` | 486.6 | 43.8 % | `edgeHandleLabelShards`, `map[edgeKey]map[uint64]labelBag` |
| `lpg.IncEdgeCreateCount` | 37.6 | 3.4 % | `edgeCreateCountShards` |
| `adjlist.upsertEdgeLocked` | 31.5 | 2.8 % | **the adjacency itself** |
| `adjlist.setEdgeLabelSlotsAtTx` | 13.8 | 1.2 % | the `uint32` type column |

> **The relationship type is stored three times**: once in the adjacency's 4-byte column, and
> once in each of two nested per-edge maps that together cost **977 B/edge — 87.9 % of the
> total.** Both are `map[edgeKey]map[K]labelBag`, so a **whole inner Go map is allocated per node
> pair even when that pair carries exactly one relationship**, which is the overwhelmingly common
> case.

This is the same anti-pattern the 2026-06-19 memory audit found and fixed for edge *properties*
(`map[edgeKey]map[PropertyKeyID]PropertyValue` → `map[edgeKey]propBag`, −58 %). Sprint 221's
follow-on #1633 tiered the *innermost* layer of these two stores to `labelBag`, but **the middle
`map[edgeKey]map[K]…` layer was left in place**, and it is where the bytes are.

Both stores have live readers — `Graph.EdgeLabelsAt` for the ordinal-keyed one,
`Graph.EdgeLabelsByHandleID` for the handle-keyed one — so neither can simply be deleted. The
handle store exists because the ordinal store's positional re-derivation *"broke after a delete"*
(its own documentation, `graph/lpg/lpg.go`), and `CreateRelationship` writes through both.

**A configuration that should have avoided half of this does not.** `graph/lpg/lpg.go` documents
the per-handle stores as *"Populated only in multigraph mode (one handle per CREATE)"*. Measured
with `adjlist.Config{Multigraph: false}`, the Cypher path costs **1 120.48 B/edge against
1 120.52 B/edge** with it true — identical to within 0.01 % — and a heap profile of the
simple-graph arm still attributes **44.6 % to `setEdgeLabelByHandleInfo`. The per-handle store is
populated regardless of the multigraph setting.**

#### The columnar edge-property tier

The `int64` edge property at 42.02 B/edge is not what a de-boxed columnar tier should cost, so it
was decomposed by varying degree at constant total edge count:

| degree | property cost, B/edge |
|---:|---:|
| 2 | 143.95 |
| 8 | 42.00 |
| 32 | 16.50 |

Fitting `cost = a + b/degree` gives **a = 7.95 B/edge and b = 272 B per source node**; that model
predicts degree 8 at 41.95 against 42.00 measured.

> **The tier stores the value optimally — 7.95 B for an `int64`, the payload and nothing else —
> but instantiates a ~272-byte `edgePropColumn` once per (source node, property key).** That
> struct carries nine slice headers for the alternative physical representations it may take
> (`graph/lpg/edge_property_column.go:114`). At degree 8 the metadata is 4× the data; at degree 2
> it is 18×; at degree 32 it amortises to 2×.

The fused `AddEdgeLabeledWithProperty` path was measured as a counterfactual and is **not**
cheaper in memory (102.88 vs 98.90 B/edge all-in). Its documented benefit is avoiding a quadratic
build — a time and churn property, not a footprint one.

### 4.4 The index result needs a caveat

GoGraph's 71.03 B/entry against Memgraph's 117.22 is a real measurement, and Memgraph's figure is
exactly what its source predicts (a 40-byte `Entry` in a skip-list node plus a heap vector block,
≈117.3 B — `src/storage/v2/inmemory/label_property_index.hpp:32-43`). But the two indexes do not
offer the same thing: **GoGraph's default index type is a hash index**, which serves equality
only, while Memgraph's label-property index is an ordered skip list that also serves ranges.
A like-for-like comparison would need GoGraph's btree index, which was not measured here.

---

## 5. How the peers got there — the decisions in their git history

Both peers arrived at their present footprint deliberately, and both records are explicit about
bytes. Every commit below was verified to be an ancestor of the tested tag with
`git merge-base --is-ancestor <sha> <tag>`; unmerged pull-request branches were found and
excluded in both repositories.

### 5.1 Memgraph — 32 bytes off the vertex, 48 off the delta

| Date | Commit | Decision | Saving |
|---|---|---|---|
| 2019-07-18 | `10136f43d` | `std::unordered_map` → `std::map` for properties | 8 B/vertex, 8 B/edge (quoted in the message) |
| 2019-09-24 | `9ad49698e` | `EdgeRef` union + `properties_on_edges` flag | the entire `Edge` object, when off |
| 2019-12-23 | `d968370c3` | **properties become an encoded byte buffer, not a map** | "approximately 10 times less memory" (source comment) |
| 2019-12-23 | `b5e255b89` | small-buffer optimisation inside that buffer | one allocation removed per small record |
| 2020-05-20 | `b923d2bc3` | `std::map` → `PropertyStore` in the records | 32 B/vertex, 32 B/edge |
| 2024-02-06 | `4ef6a1f9c` | Delta tagged union | 104 B → 80 B per delta |
| 2024-02-27 | `da898be8f` | `optional<string>` → an 8-byte struct in the delta union | 80 B → 56 B per delta |
| 2024-05-10 | `c88bba5d9` | `std::vector` ×3 → `small_vector`, members reordered | **112 B → 88 B per vertex** |
| 2024-07-10 | `24aa260e7` | deltas into 16 KiB page slabs, 292 per slab | 99.8 % packing; RSS returnable |
| 2026-02-23 | `5022587d3` | booleans packed into the low bits of the `Delta*` | **88 B → 80 B per vertex** |

The pattern is worth naming: **Memgraph treats `sizeof(Vertex)` as a tracked, asserted quantity**
(`static_assert(sizeof(Vertex) == 80, "If this changes documentation needs changing")`), and has
reduced it by 28.6 % over two years in increments of 8 and 24 bytes. GoGraph has no comparable
assertion because it has no comparable struct.

Memgraph's own transient cost is the mirror image of this discipline: an MVCC delta is 56 bytes
and a bulk load of this scenario creates roughly 4 000 000 of them, giving a **peak/steady ratio
of about 3.5×**, released on a 30-second GC cycle. Its escape hatch is `IN_MEMORY_ANALYTICAL`
mode, which creates **zero** deltas and gives up rollback and snapshot isolation to do it.

### 5.2 Neo4j — move everything off the heap, then shrink the metadata

| Date | Commit | Decision | Saving |
|---|---|---|---|
| 2011-02-17 | `e7e018612` | compressed short strings inlined into property blocks | removes the dynamic-store indirection entirely |
| 2011-09-01 | `eb4bfad17` | `PropertyBlock` — multiple properties per record | amortises 9 B of framing over 4 properties |
| 2013-02-07 | `211b4ae0d` | labels stored on the node record itself | removes a dynamic record per node |
| 2014-01-15 | `366d30928` | relationship groups, with a **dense-node threshold of 50** | group records cost nothing below the threshold |
| 2014-09-16 | `6003c21d7` | removed the per-page `finalize()` | ~40 B of heap per cached page |
| 2017-02-19 | `fc2a66312` | **page metadata moved off-heap** — `MuninnPage.java` deleted | one Java object per page → 32 B in a flat array |
| 2018-01-29 | `73b21b2bb` | packed the off-heap page record back down | 64 B → **32 B per page** |
| 2022-11-10 | `30f5d2a92` | ID-generator cache grows and shrinks dynamically | **~410 KB → 12 KB** per generator |
| 2023-05-10 | `d4e5ed533` | capped page-cache buffer alignment | 1/8 of native memory on 64 KiB-page kernels |

And one reversal worth recording, because it points the other way: Neo4j introduced **off-heap
transaction state** in 2018 and **deprecated it for removal in 5.8** (`395b7d1f0`, 2023-04-20).
The shipped setting now argues the opposite of the original motivation — *"for small transactions
you can gain up to 25 % write speed by setting it to `ON_HEAP`"*. Transaction state is on-heap
and metered.

---

## 6. Findings

Ranked by the size of the win available.

### F1 — The Cypher relationship write path costs 977 B/edge in two redundant maps, and it OOM-kills the engine

**Severity: this is an availability defect, not a tuning opportunity.** `ggserver` was
OOM-killed by the kernel at 8.37 GB anon-RSS while creating the 3.5-millionth relationship in an
8 GB container (§1), on a graph Neo4j held in ~2.7 GB. The reliability mandate requires that
saturation be answered "with backpressure or a typed error, never with failure"; here the process
died.

The cost is attributed, not suspected. `edgeInstanceLabelShards` (490 B/edge) and
`edgeHandleLabelShards` (487 B/edge) are both `map[edgeKey]map[K]labelBag`, so **each allocates a
whole inner Go map per node pair to hold, in the common case, a single relationship's type** —
the same type the adjacency already holds in its 4-byte `uint32` column. Sprint 221's #1633
tiered the innermost layer of both stores to `labelBag` but left the middle map, which is where
the bytes are; the fix that worked for edge *properties* in the same sprint
(`map[edgeKey]map[…]` → `map[edgeKey]propBag`, −58 %) has not been applied here.

Three sub-findings, each independently actionable:

- **F1a — the per-handle store is populated even when parallel edges are disallowed.**
  `graph/lpg/lpg.go` documents it as *"Populated only in multigraph mode"*. Measured with
  `Multigraph: false`, the per-edge cost is **1 120.48 B against 1 120.52 B** with it true, and
  the heap profile still attributes 44.6 % to `setEdgeLabelByHandleInfo`. Either the setting
  should suppress the store or the documentation is wrong; today the configuration buys nothing.
- **F1b — both stores are written on every CREATE and both have live readers**
  (`EdgeLabelsAt`, `EdgeLabelsByHandleID`), so neither can be deleted without a design decision.
  The handle store exists because the ordinal store's positional re-derivation *"broke after a
  delete"*. **Whether the ordinal store can now be retired in favour of the handle store is the
  single highest-value open question in this report, and it is a question for the user, not an
  assumption for this audit to make.**
- **F1c — the middle map should be tiered regardless.** Even if both stores must survive, a
  single-entry inner map per node pair is the wrong representation for a graph in which almost
  every pair has exactly one relationship.

### F2 — The synthetic string node key costs 44.5 B/node for an identity Cypher never exposes

`cypher/exec/create_node.go:357` mints `__cx_<hex>` per created node; `graph/mapper.go:311-353`
stores it twice, in a `map[string]NodeID` and in a `[]string`. Measured against a `uint64` key on
the identical build: **93.03 → 48.52 B/node**. The key's own documentation states it is never
visible to callers, so no observable behaviour depends on its being a string.

This is not a one-line change — `Graph` is generic over `N comparable` and the Bolt/Cypher stack
is instantiated at `lpg.New[string, float64]` — but the cost is now measured rather than
suspected.

### F3 — Three hash maps per node is the dominant cost, at a 2.3–2.5× multiplier

Labels (111 B), properties (101 B) and identity (71 B) are three separate sharded Go maps. This
is 76 % of a node's footprint before any value is stored, and the two attribute maps alone are
57 %. Both peers keep a node's attributes in one
contiguous record. Any consolidation — a per-node record, or even merging the label and property
shard maps so one lookup and one entry serve both — attacks the largest term.

### F4 — `PropertyValue` boxes every scalar into an `any`

`lpg.StringValue` alone is 18.35 B/node in the profile, on top of the 17.30 B of string data. A
previous cycle (sprint 221, F3) evaluated a `{kind; num uint64; ref any}` layout and **rejected
it with measurement** because it grew the struct 24→32 B while keeping a GC-scanned pointer. That
rejection stands for that design; the measurement here says the target is worth revisiting with a
**fully unboxed** scalar union, since Memgraph's answer — a tagged byte stream with 4-bit type
nibbles — spends *zero* bytes on type dispatch per value.

### F5 — The columnar edge-property tier's metadata is 272 B per (source node, key)

`graph/lpg/edge_property_column.go:114` declares nine slice headers so that one struct can be any
of dense/sparse/bit-packed/boxed. The value encoding is optimal (7.95 B for an `int64`); the
struct that describes it is not. At degree 8 that is 34 B/edge of pure metadata. Candidate
directions: a discriminated union rather than nine parallel slices, or hoisting rarely-used
representations behind a pointer.

### F6 — GoGraph's per-node cost is lumpy, and the peak is worse than the steady state

GoGraph's fits carry R² 0.918–0.968 against Memgraph's 0.9971–0.9999. The residuals are real: 64
shard maps hash uniformly, so they reach their doubling thresholds at nearly the same time and
**rehash together**, and during a rehash both the old and the new bucket array are live. An
operator sizing a host from a steady-state figure will be surprised by the transient. This is a
documentation obligation at minimum.

### F7 — Two claims in the previous audit need correcting

- The concurrency audit recorded that *"Memgraph's WAL is off by default"* on the strength of
  `src/flags/general.cpp:77`. The source default is indeed `false`, but **the shipped Docker image
  sets it `true`** in `/etc/memgraph/memgraph.conf`, along with snapshots and
  `properties-on-edges`. Configuration read from source is not configuration in force.
- Memgraph's `properties_on_edges` struct default (`config.hpp:43`) is `true` while its flag
  default (`flags/general.cpp:74`) is `false`. Either read alone gives the wrong answer.

---

## 7. Reproduce

```bash
docker network create ggbench
# Engines are restarted per shape by the driver; see bench/comparison/memory_test.go §2.2.
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o ggserver ./bench/comparison/ggserver
docker build -t gograph-bench:local .          # alpine + the binary, ports 7689 and 6060

# One shape per invocation, each in freshly started engines. -count=1 is mandatory (§2.6).
# Node shapes: bare | prop1 | prop3 | index      (MEM_NODES nodes each)
MEM_SHAPE=prop3 MEM_NODES=1000000 MEM_STEPS=8 MEM_ONLY="gograph,neo4j,memgraph" \
  go test -count=1 -tags=threeway -run TestMemoryFootprint -v -timeout 180m ./bench/comparison/

# Edge shapes: edge | edgeprop   (MEM_NODES × MEM_DEGREE edges each). The
# memgraph-noedgeprops arm needs the second Memgraph container of §2.5.
MEM_SHAPE=edge MEM_NODES=500000 MEM_DEGREE=8 MEM_STEPS=8 \
  MEM_ONLY="gograph,neo4j,memgraph,memgraph-noedgeprops" \
  go test -count=1 -tags=threeway -run TestMemoryFootprint -v -timeout 180m ./bench/comparison/

# In-process attribution — one arm per process (§2.6).
go test -count=1 -tags=threeway -run '^TestProbe_MapperStringKey$'  -v ./bench/memprobe/
go test -count=1 -tags=threeway -run '^TestProbe_MapperUint64Key$'  -v ./bench/memprobe/
PROBE_HEAP_PROFILE=/tmp/heap.pb.gz \
  go test -count=1 -tags=threeway -run '^TestProbe_NodesWithLabelAndProp$' -v ./bench/memprobe/
go tool pprof -inuse_space -top /tmp/heap.pb.gz

# The Cypher-vs-Go-API amplification, and its attribution.
go test -count=1 -tags=threeway -run '^TestProbe_EdgesTyped$'          -v ./bench/memprobe/
PROBE_HEAP_PROFILE=/tmp/heap-edges.pb.gz \
  go test -count=1 -tags=threeway -run '^TestProbe_CypherEdges$'       -v ./bench/memprobe/

# Separating a per-EDGE cost from a per-SOURCE-NODE one: same total edges, varying degree.
for d in 2 8 32; do n=$((4000000/d)); \
  PROBE_NODES=$n PROBE_DEGREE=$d go test -count=1 -tags=threeway \
    -run '^TestProbe_EdgesTypedWithProp$' -v ./bench/memprobe/; done
```

---

## 8. What was not measured

Stated so the report's boundaries are not mistaken for its conclusions.

- **Durability was excluded by construction.** `ggserver` has no WAL; Memgraph's image has one;
  Neo4j fsyncs every commit and preallocates a 256 MiB transaction log. The disk column therefore
  compares three different things and is reported per engine, never ranked.
- **Neo4j's transaction log is outside the measured directory.** The harness reads
  `data/databases/neo4j`; during a load `data/transactions` reached 526 MB against a 1.9 MB store.
  The store figure is the durable data; the log is a bounded, pruned buffer.
- **Peak memory during load** was not measured for any engine, only steady state after quiesce.
  Memgraph's source predicts a ~3.5× transient from MVCC deltas; GoGraph's rehash transient (F6)
  is unquantified — except in the one case where it terminated the process (§1).
- **GoGraph's btree index** was not measured, so the index comparison in §4.4 is not like-for-like.
- **GoGraph's edge arms are 7-point and 4-point fits**, not 9-point, because the engine was
  OOM-killed before completing them. The R² is 0.9990 and 0.9985 respectively over the range that
  did run, so the slopes are sound, but they are extrapolated past the point of failure.
- Memgraph's `IN_MEMORY_ANALYTICAL` mode and its opt-in property compression were not exercised.
- **No engine was measured with a graph larger than its RAM.** That is the regime Neo4j's
  architecture is designed for, and this audit says nothing empirical about it. GoGraph's own
  out-of-core tier (`store/csrfile`, mmap-backed immutable CSR) was likewise not exercised: it is
  not the storage the Cypher engine reads, so it is outside this comparison's scope — but it means
  "GoGraph is RAM-bound" is true of the measured engine, not of the module.

---

## 9. The state of the module

Answering the question this audit was commissioned to settle.

**GoGraph's storage layer is in good shape and its query layer is not.** Every structure the
engine was designed to be efficient at is efficient: the adjacency is 41 B/edge, a relationship
type is 4 B, an `int64` edge property is 7.95 B, and a secondary index entry is cheaper than
Memgraph's. Every large number in this report comes from a **sharded Go map sitting above that
layer**, and there are five of them on the relationship write path alone.

That distinction matters for what to do next. This is not a rewrite: it is the same class of fix
that sprint 221 already applied successfully to edge properties in June 2026
(`map[edgeKey]map[…]` → `map[edgeKey]propBag`, −58 % measured), left unapplied to the four label
and property stores that the Cypher write path uses.

Ranked against the module's own [Compliance Mandates](../CLAUDE.md#compliance-mandates):

| Mandate | Status on the evidence of this audit |
|---|---|
| 100 % openCypher TCK | Untouched by this work; 3897/3897 holds |
| 100 % ACID | Untouched by this work |
| EXTREME concurrency | **Confirmed strong** by the sibling audit — GoGraph scales best of the three (8.76× vs 6.44× vs 3.86× at 1→64 clients) |
| **ULTRA EFFICIENT by design** | **Not met.** 3.4× Memgraph per node, 10.5× per relationship, and an OOM kill on a graph two rivals held comfortably |

The concurrency result and this one are not in tension, and the reason is worth stating plainly:
**the sharding that makes GoGraph's read path the only one of the three with no globally shared
cache line is the same sharding that costs it 2.3–2.5× per stored attribute.** Any remedy must be
measured against both mandates — which is why the two spikes filed from this audit (#2403, #2405)
both require concurrent-throughput measurements at 1, 8 and 64 goroutines alongside their memory
numbers, and why neither may be implemented without the user's decision first.

### Filed

`rmp` roadmap `gograph`, all in BACKLOG, priority order:

| Task | Type | P | Title |
|---|---|---:|---|
| #2401 | BUG | 9 | Cypher relationship writes cost ~1079 B/edge in two nested per-edge label maps, OOM-killing the engine |
| #2403 | SPIKE | 8 | Can the ordinal-keyed edge instance stores be retired in favour of the handle-keyed ones |
| #2402 | BUG | 7 | Per-handle edge label store is populated even when Multigraph is false |
| #2405 | SPIKE | 7 | A per-node record, against three sharded hash maps costing 2.3–2.5× their payload |
| #2404 | IMPROVEMENT | 6 | Cypher's synthetic string node key costs 44.5 B/node for an identity it never exposes |
| #2406 | IMPROVEMENT | 5 | Columnar edge-property tier spends 272 B of metadata per (source node, property key) |
| #2407 | TASK | 4 | Peak memory during node growth is unmeasured and exceeds steady state |
