# Examples-Driven Module Audit — 2026-06-21

Read-only audit of the GoGraph **module**, conducted by exercising every
runtime-observable functionality through the project's `examples/` at observable
scale, collecting empirical CPU / RAM / storage evidence, and verifying each
candidate finding with a panel of specialist sub-agents. The examples are
**instruments**: a finding is attributed to the module only when the cost lies in
GoGraph code, not in an example's own generator or harness.

**Invariants (every proposal preserves them):** openCypher TCK 3897/3897 and the
ACID properties must not regress; the lock-free immutable-read contract must hold;
`go test -race` must stay clean.

**Environment:** Apple M4 (10-core: 4P+6E), 32 GiB, `go1.26.4`, darwin/arm64,
HEAD `8c96772` (post-columnar sprint 222).

## Method

1. **Coverage map.** All 19 module features (from the Knowledge Graph) mapped to
   the example(s) that exercise them via each example's GoGraph imports.
2. **Breadth sweep.** The 24 scale-knob examples (01–23, 26) run at
   observable-but-bounded scale (linear workloads large; superlinear algorithms —
   exact Brandes O(V·E), max-flow, Hungarian — bounded), capturing wall-clock,
   peak RSS, and each example's `# ` telemetry (heap, bytes/edge, throughput,
   latency, on-disk bytes). Examples 24/25 (flag-less CLI/HTTP apps) are covered
   by their test suites and by 22/04/17/21/26.
3. **Profiling.** CPU + heap (`inuse_space`/`alloc_space`) profiles of the hot
   packages (`lpg`, `adjlist`, `csr`, `cypher`, `cypher/exec`, `search`,
   `search/centrality`, `store/wal`, `store/txn`) via `go test -bench`.
4. **Targeted measurement.** A persistence scaling curve, a deterministic
   reproduction of the crash below (3/3), and recalibrated re-runs of the
   miscalibrated examples.
5. **Specialist verification.** Five domain specialists (storage-engine,
   cypher, columnar, Go, perf) verified each candidate against the code
   (`file:line`) and authoritative references, assessing module-vs-instrument
   attribution, fix complexity, and TCK/ACID safety.

The raw evidence (per-example telemetry, profile tops, reproduction logs) is
retained under the audit scratchpad; the headline numbers are reproduced inline.

---

## CRITICAL — fatal deadlock: background checkpoint vs. commit

**Finding C1 — `Mapper.Walk` re-entry in the snapshot collectors deadlocks
against a concurrent commit.** Severity **Critical**. Module bug. Reproduced 3/3
(deadlocks at 9–34 s).

Example 17 (`-accounts 50000 -transfers 1000000`, with the background
checkpointer it is designed to demonstrate) crashes:

```
fatal error: all goroutines are asleep - deadlock!
goroutine 1  [sync.RWMutex.Lock]  graph.(*Mapper).internSlow            (mapper.go:281)
goroutine 16 [sync.RWMutex.RLock] graph.(*Mapper).Lookup               (mapper.go:302)
              via store/snapshot.collectNodeLabelRecords                (labels.go:241)
```

**Root cause.** `graph.Mapper.Walk` (`graph/mapper.go:346`) holds a shard `RLock`
across its callback. Its own doc (`graph/mapper.go:337-345`) forbids the callback
re-entering the mapper while a writer may run: once a writer's `internSlow`
(`mapper.go:281`) queues on the shard write lock, Go's `sync.RWMutex` admits no
new readers (writer anti-starvation), so a nested `RLock` deadlocks the callback,
the writer, and every future operation on the shard. The four snapshot collectors
violate this contract:

| Collector | Re-entry into the mapper |
|---|---|
| `store/snapshot/labels.go:241` `collectNodeLabelRecords` | `NodeLabels(n)` → `Lookup` (reproduced path) |
| `store/snapshot/labels.go:273` `collectEdgeLabelRecords` | `Resolve(dstID)` + `EdgeLabels` → `Lookup`×2 |
| `store/snapshot/properties.go:356` `collectNodePropertyRecords` | `NodeProperties(n)` → `Lookup` |
| `store/snapshot/properties.go:398` `collectEdgePropertyRecords` | `Resolve(dstID)` + `EdgeProperties` → `Lookup`×2 |

**Why it triggers, and why it had not been caught.** The non-blocking checkpoint
runs label/property serialization in **Phase 2 (`writeAndTruncate`,
`store/checkpoint/checkpoint.go:566`), which is lock-free by design** — it holds
neither the commit lock nor `Graph.View`, precisely so committing writers do not
stall on multi-second disk I/O. `WithCommitSerialiser` (the mitigation example 17
uses) wraps only Phases 1 and 3, so it cannot guard Phase 2. The discriminator is
`visMu`: Cypher's own re-entrant `Walk`s are safe because they run under
`Graph.View` (`visMu.RLock`, mutually exclusive with a commit's `ApplyAtomically`
= `visMu.Lock`), and recovery `Walk`s run single-threaded; the snapshot collectors
hold neither. The existing test `store/checkpoint/writer_stall_test.go:89` commits
a concurrent node with **no labels or properties**, so the collector callback
returns immediately and the race window is never forced.

**Every other `Mapper.Walk` caller was checked and cleared** (the Cypher callers
run under `View`; recovery is single-threaded; the IO writers and
`WalkEdgeHandles` use the lock-free `LoadEntry`/`LoadEntryH` path with no mapper
re-entry; `store/snapshot/mapper.go` emits `id+key` only).

**Fix (Low complexity, ACID- and lock-free-read-preserving).** Collect
`(id, value)` (node) or `(srcID, dstID, srcN, dstN)` (edge) tuples inside `Walk`,
then resolve labels/properties **after** `Walk` returns — the remedy the mapper
doc itself prescribes and that Cypher task #1339 already applied. Node collectors
are a drop-in: the lock-free ID-keyed accessors `NodeLabelsByID` (`lpg.go:1530`)
and `NodePropertiesByID` (`property.go:326`) already exist. Edge collectors need
either new `EdgeLabelsByID`/`EdgePropertiesByID` accessors or post-`Walk` tuple
resolution (the endpoint keys are already in hand). Format-neutral
(`formatVersion` stays 1). **Must be gated** by a new deterministic regression
test: a commit that interns a new key on shard *S* while a collector holds *S*'s
`RLock` mid-`Walk`, under `-race`.

---

## HIGH — memory and query-cost levers (evidence-backed)

**Finding H1 — the `int32` epoch-day date column exists but is not activated for
Go-API writers.** The columnar storage tier already implements an `int32`
epoch-day date column (`graph/lpg/edge_property_column.go`: `dateKind`,
`classify`, the Howard-Hinnant codec) — the "Deferred" note in
`docs/benchmarks/columnar-edge-properties.md:75` is **stale**. But `classify()`
folds a date only when it arrives SOH-tagged (`0x01`); Cypher-written dates are
tagged and fold, whereas the Go API path used by example 26 writes a **plain
untagged ISO string** (`examples/26_social_scale_bench/main.go:431`
`time.Format(...)` → `StringValue`), so the value stays in the `[]string` column.

Heap profile (`inuse_space`, example 26 `BenchmarkBuild` @ 20k/2k, ~478 MB):
`time.Time.Format` **93.5 MB (19.5 %)**, plus `insertStr`+`cloneStr`+`growStr`
≈ **36 %** of resident heap in the no-rel-types profile — i.e. the ISO-string date
storage is ~30 %+ of the heap. Activating the existing column (a public
`lpg.DateValue` constructor, and tagging the example's dates) reclaims it:
~33 → ~4 B/slot (spike #1635), extrapolating ~20 → ~11 GiB at full example-26
scale, and shrinking the per-edge COW `cloneStr` 4× (clones a 4-byte `int32`, not
a 16-byte string header). The encoding is byte-congruent with Arrow `Date32` /
Parquet logical `DATE` / ORC `DATE`. It reads back as a native Cypher `Date`
(strictly better than the lexicographic string), guarded by a round-trip check
(`edge_property_column.go:1937`), so TCK and `kill -9` durability are preserved.

**Finding H2 — the per-edge weight column is dead weight when weights are
unused.** `adjEntry` unconditionally allocates `weights []W`
(`graph/adjlist/adjlist.go:640,739`); example 26 instantiates `W = float64` with a
constant `1` that is never read, costing **8 B/edge** (~52 MB, ~11 % of the
478 MB profile; ~2.6 GiB at full scale). There is no weightless adjacency mode.
Fix: make `weights` lazy (nil-until-first-non-default, mirroring the existing
`handles`/`labels`/`aux` parallel columns), or let weight-free callers instantiate
`W = struct{}`. Weights are not openCypher-visible; format-neutral.

**Finding H3 — integer/parameter range predicates are un-indexable from Cypher,
and the programmatic `graph/query` engine never uses a property index.** The range
B+tree seek (#1505) is scoped to **string literals only**
(`cypher/range_seek_plan.go:25-28`), so `WHERE n.age > 30` and parameterised
bounds fall back to a full label-scan-plus-filter; the programmatic `graph/query`
engine consults only the label index, never `index.Manager`, for property
predicates (`graph/query/query.go:11-14,260-280`, documented as a "future
iteration"). Measured gap: an index seek is **~8000× faster** and allocates
**~7500× less** than the equivalent label scan
(`BenchmarkIndexSeek_vs_LabelScan`: 4.7 µs / 53 allocs vs 38.6 ms / 399 818
allocs); example 02's `dept`/`active` filters cost ~200 ms scanning 500 k nodes
vs 5.9 ms for the label index, and example 22's `age > 30` costs 255 ms. Wiring
`index.Manager` into `graph/query` is a TCK-free win (it is a separate code path
from Cypher); extending the Cypher range seek to integer/parameter bounds is the
Cypher-side lever and must be argued against the openCypher spec.

**Finding H4 — `Mapper.Walk` callback allocation in the build hot path.** Every
`adjlist.AddEdge` republishes a fresh 112-byte `adjEntry` struct via the immutable
copy-on-write slot (`graph/adjlist/adjlist.go:728`): **1 alloc/edge** in steady
state, with geometric array reallocation only on the O(log d) growth events.
(The "O(Σ degree²)" worry is **refuted** — measured growth is O(d·log d):
degree 1k/10k/100k = 4 072 / 33 865 / 306 209 allocs, ≈ linear per decade.) This
single per-edge republish is **52 % of the transient allocation during CSV import**
(Finding H5) and the dominant cost of building high-degree hubs
(`BenchmarkHub_AddEdge_100k`: 19.8 ms, 28.7 MB, 306 k allocs). Fix: an in-place
atomic-length append (fixed-capacity backing + `atomic` length; readers
acquire-load the length, then read `[0:len]`) eliminates the fast-path struct
allocation (→ 0 alloc/edge) while preserving the lock-free read contract via the
memory-model argument already documented at `adjlist.go:615-629`. A `sync.Pool` of
entries is **unsafe** (an in-flight reader may hold an old entry indefinitely).

---

## MEDIUM

**Finding M1 — CSV import transient allocation (2.48 GiB / 2.25 M edges, ex06).**
Reframed: ~52 % is Finding H4 (one `adjEntry` republish per edge), ~23 % is
`encoding/csv`'s per-record field buffer (unavoidable through that API; the reader
already uses `ReuseRecord` and `bufio`), ~9 % is `Mapper.internSlow` map/slice
growth. Fixing H4 removes the majority; a `Mapper.Reserve` node-count hint
(`graph/mapper.go:231`, additive `Options` field) cuts the interning churn.

**Finding M2 — `count(<var>)` eager-materialises a `NodeValue` per row.**
`count(*)` drains all rows (no cardinality path) and `count(node_var)` additionally
builds a `NodeValue` per row purely to null-check it; example 22's
`count(KNOWS)` over 1.35 M edges costs 1.56 s. A null-check that avoids
materialisation is a low-risk, TCK-safe fix for `count(<var>)`; a
relationship-count *pushdown* carries high TCK risk (multigraph CREATE-multiplicity,
TCK `Merge5`) and is **not** recommended.

**Finding M3 — no single-threaded bulk-ingest path.** Each `Tx.Commit` performs
one `fdatasync` (`store/wal/writer.go:478`); group commit (#1507) coalesces only
*concurrent* writers, so a sequential commit loop is fsync-bound (~250 commits/s
here; example 04 at 25 k packages = 100.75 s, linear). A bulk-ingest API batching
K logical entities into one transaction (one fsync, per-batch atomicity) addresses
write-heavy single-threaded ingest (backlog #1512).

**Finding M4 — lazy edge materialisation (the open residual of #1500/#1502).** The
node-side per-row materialisation optimisations #1500 and #1502 are **closed**
(commits `3d37452`/`257ce96`, TCK held); the cypher `alloc_space` profile's
remaining ~50 % is whole-node/edge `RETURN` residual, of which lazy *edge*
materialisation (#1630) is the open lever.

**Finding M5 — `SetEdgeLabelSlot`/`ClearEdgeLabelSlotValue` are O(degree)/call**
(`graph/adjlist/adjlist.go:1359,1412`): each copies the whole label column to flip
one slot, so a post-build re-label loop on a hub is O(d²). Latent (not exercised by
the examples; the fused `AddEdgeLabeledWithProperty` build path avoids it).

**Finding S1 (structural) — the cost-based planner is dead code.** `cypher/plan`
(and `cypher/ir/rewrite`) are imported only by their own tests; the live planner is
hand-rolled in `cypher/api.go`. This is the root of the index brittleness in H3 and
was first noted in the 2026-06-14 audit. Activating or removing it is a strategic
decision, not a quick fix.

---

## Instrument findings (example quality — fed back per the Examples mandate)

These are defects in the **examples** (the instruments), not the module. They are
worth fixing so the examples remain faithful at scale.

- **07_graphml_roundtrip — O(N²) generator.** `preferentialPick`
  (`examples/07_graphml_roundtrip/main.go:314`) rebuilds the full cumulative
  prefix-sum on every draw → 66 s of untimed build at 200 k nodes (the module's
  `AddEdge` is exonerated; out-degree is bounded). Fix: a Fenwick tree or the urn
  method.
- **04_persistence — misleading `-batch` flag.** `-batch` only gates the
  cancellation check (`main.go:262`); `Begin`/`Commit` are per-package, so it does
  **not** batch commits, and the doc comment "batched into WAL commits"
  (`main.go:225`) is false (a documentation-faithfulness violation). 21 and 17
  likewise commit per-entity.
- **26_social_scale_bench — untagged date writes.** Writes dates as plain ISO
  strings via the Go API (`main.go:431`), defeating the `int32` column (Finding
  H1). A `lpg.DateValue` constructor would let it (and any Go-API caller) store the
  compact form.
- **20_concurrent_reads — unsound scaling claim.** Its "throughput climbing with
  worker count is evidence readers don't contend" claim (`main.go:43-45`) is a poor
  proxy: at this workload the 3.1×/8-worker curve is memory-bandwidth-bound and
  distorted by M4 P/E-core asymmetry and the example's own nested-parallel PageRank
  (≈80 goroutines at 8 workers). The lock-free contract genuinely holds (see
  Strengths); the claim should be reframed.
- **13_network_reliability — degenerate-config exit.** Exits non-zero for certain
  cluster/size combinations (a min-cut edge case); a clean default ran fine.

---

## Confirmed strengths (at or near the optimum — no action)

- **Search / traversal.** Direction-optimised BFS 0-alloc (16.6 ms vs 102 ms
  top-down); bidirectional Dijkstra 2× faster and 8× lighter than plain
  (1.0 ms/83 KB vs 2.0 ms/697 KB); pooled Dijkstra 4 allocs/op. Example 10's
  slowness is its *choice* of plain SSSP, not the module.
- **Lock-free read path — contract verified.** The immutable-CSR neighbour read is
  11.45 ns / 0-alloc; under an 8-way parallel Dijkstra sweep the mutex profile shows
  0.14 % contention (all runtime allocator/scheduler, zero GoGraph locks) and the
  block profile shows zero blocking in the read path. Sublinear throughput at high
  worker counts is memory-bandwidth saturation, not contention.
- **Out-of-core.** Example 05: 1 M nodes / 4 M edges in 46 MiB live heap, 38 MiB
  on disk, 7.7 ms mmap; example 18 similar. A standout footprint advantage.
- **Memory tiers already landed.** Parallel PageRank (#1513); the columnar
  edge-property tier (157 → 61.8 B/edge); the node `propBag` small-tier (#1587);
  the `labels []uint32` dictionary, which resolved the former edge-label heap
  dominance (full vs no-rel-types profiles differ by only ~12 MB / 2.5 %); the
  closed cypher materialisation wins #1499/#1500/#1502.

## Hypotheses the evidence refuted (transparency)

The empirical sweep and the specialist panel **corrected** several plausible
candidate findings — recorded so they are not re-raised: node-property storage is
*already* tiered (#1587); the edge-label map is *no longer* a heap consumer; the
adjlist build is O(d·log d), *not* O(d²); the CSV transient is *mostly the adjlist*,
not the parser; `LIMIT 1` below `ORDER BY` not short-circuiting is *correct*
openCypher, not a bug; the cypher materialisation optimisations #1500/#1502 are
*closed*; the `int32` date column already *exists*; example 20's scaling is *not* a
contention defect.

## Backlog mapping

The confirmed module findings are filed as atomic tasks in the `gograph` rmp
roadmap (sprint **S-EA1**); the instrument findings as example-quality tasks. C1
(Critical) is filed but, being a non-trivial concurrency fix, awaits an explicit
go-ahead before implementation per the decision-autonomy rule.
