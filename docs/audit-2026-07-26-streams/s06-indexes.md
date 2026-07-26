# Stream 6 — Indexes, constraints, schema and statistics

Baseline: `6f31f61`, v0.10.0. Hardware for all measurements: Apple M4, macOS 25.5.0, go1.26.5 darwin/arm64.

## Verdict summary

Round 1's verdict ("GoGraph wins on core index quality, loses on index KINDS") is **half right and
dangerously incomplete**. The count store genuinely beats Neo4j (exact `(labelA, relType, labelB)`
triples vs Neo4j's sampled index selectivity — Neo4j keeps exact *label/type counts* but only
*sampled* index selectivity and **no value histograms at all**), and GoGraph's statistics
estimators are, on paper, the most sophisticated of the three (HLL-NDV + exact top-32 MCV +
256-bucket equi-depth histograms; Memgraph has one chi-squared group-size number, Neo4j has a
scalar selectivity ratio). But three defects found this round outrank every "missing index kind":
**(1) numeric equality never reaches any index** — a Cypher `CREATE INDEX` on an integer property
is inert for `=`; **(2) the index seek only fires on a plan-time constant** — any seek value bound
by `UNWIND` or `WITH` falls back to `CartesianProduct` + full label scan, which is precisely the
Cypher bulk-load idiom; **(3) the v0.10.0 statistics ship INERT** — nothing on the query path reads
them, they only decorate `Engine.Explain` text. The single most valuable lever is **(2)**, and the
second is **(1)**, which is nearly free because the fix reuses machinery that already ships and is
already proven correct. Index *kinds* are a distant third and vector/ANN is a distraction.

**TCK safety, verified:** across 220 `.feature` files under `cypher/tck/features/`, `grep -ril
"CREATE INDEX|CREATE CONSTRAINT|DROP INDEX|DROP CONSTRAINT"` returns **zero** matches, and the
directory listing contains no `schema`/`index`/`constraint` feature directory (only `clauses/`,
`expressions/`, `useCases/`). Round 1's claim is **[CONFIRMED-R1]**. Every finding below is
therefore TCK-neutral *by construction on the DDL surface*; the only TCK exposure is on the *plan
selection* side, where the invariant is "seek must return a superset of scan+filter, with the
residual Filter retained".

## Feature-by-feature comparison

| Feature | GoGraph (file:line) | Neo4j 5 | Memgraph 3.x | Verdict | Label |
|---|---|---|---|---|---|
| Index kinds | hash(string), btree(string)+hidden btree(float64) companion, label bitmap — `cypher/ir/ddl_parser.go:763-805` | RANGE, TEXT, POINT, LOOKUP, FULLTEXT, VECTOR | label, label-property(+composite), edge-type, edge-type-prop, global edge-prop, point, text, vector | WORSE both | CONFIRMED-R1 |
| **Numeric equality seek** | **none — full scan** (measured) | RANGE index | label-property index | **WORSE both** | **NEW** |
| Numeric range seek | works via float64 companion — measured `est. rows=1, exact` | yes | yes | PARITY | **NEW (refutes)** |
| **Correlated (per-row) seek** | **none** — `resolveSeekValue` is plan-time, `cypher/api.go:9642` | `Apply`+`NodeIndexSeek` | yes | **WORSE both** | **NEW** |
| Composite index | rejected, `ddl_parser.go:735` | RANGE composite | since v3.2.0, leftmost-prefix | WORSE both | CONFIRMED-R1 |
| Relationship-prop index | none | RANGE/TEXT/POINT on rels | `CREATE EDGE INDEX` | WORSE both | CONFIRMED-R1 (#2166) |
| Index-ordered `ORDER BY` | impossible by construction (roaring bitmap) | yes | yes (+ explicit DESC index) | WORSE both | CONFIRMED-R1 (#2163) |
| Node count `N(label)` | exact, label bitmap | exact | exact | PARITY | CONFIRMED-R1 |
| Rel triple `T(a,rt,b)` | **exact**, `graph/index/count/count.go:280` | exact count store | coefficient × cardinality | **BETTER both** | CONFIRMED-R1 |
| Value histograms | 256-bucket equi-depth per domain, `stats/histogram.go` | **none** (selectivity ratio only) | **none** (chi-squared scalar) | BETTER both *on paper* | **NEW** |
| **Statistics actually used** | **no — inert**, `cypher/stats_estimate.go:18-23` | yes | yes | **WORSE both** | **NEW** |
| Stats refresh trigger | Go API `RefreshStatistics(ctx)` only | automatic background sampling | `ANALYZE GRAPH` (Cypher, manual) | WORSE both | **NEW** |
| Constraint: uniqueness | single-prop node only, `ddl_parser.go:984` | node+rel, composite, CE | node, composite, CE | WORSE both | CONFIRMED-R1 |
| Constraint: existence | node single-prop, commit-time | **Enterprise only** | Community | **BETTER than Neo4j**, PARITY Memgraph | **NEW** |
| Constraint: NODE KEY | rejected, `ddl_parser.go:990` | Enterprise | absent | WORSE Neo4j, PARITY Memgraph | CONFIRMED-R1 |
| Constraint: property type | DDL rejects (`ddl_parser.go:996`) but **enforcement engine exists** `graph/lpg/schema/schema.go:137` | Enterprise | `IS TYPED`, Community | WORSE both | **NEW** |
| Index lifecycle | synchronous, always `ONLINE`, `cypher/show.go:78` | POPULATING→ONLINE→FAILED, online build | online | Different, not worse for embedded | CONFIRMED-R1 |
| `USING INDEX` hints | none | yes | none | PARITY Memgraph | CONFIRMED-R1 (#2165) |

## Findings

### F1. Numeric equality never reaches an index; numeric *range* already does  [NEW]  (severity: HIGH)

- **What they do:** Neo4j RANGE indexes serve equality, range, prefix and `IN`
  (https://neo4j.com/docs/cypher-manual/current/indexes/search-performance-indexes/overview/).
  Memgraph's label-property skip list serves equality and range identically.
- **What GoGraph does:** measured with `Engine.Explain` on 20 000 `:Person` nodes carrying an
  `INTEGER id` and a `STRING name`, with indexes created on both:

  | query | plan |
  |---|---|
  | `MATCH (a:Person {id: $x})` | `NodeByLabelScan (est. rows=20000, exact)` |
  | `MATCH (a:Person) WHERE a.id = $x` | `NodeByLabelScan (est. rows=20000, exact)` |
  | `MATCH (a:Person) WHERE a.id = 12345` | `NodeByLabelScan (est. rows=20000, exact)` |
  | `MATCH (a:Person) WHERE a.name = $s` | `NodeByIndexSeek` |

  Root cause is a documented contract, not an oversight: `cypher/index_binding.go:55`
  `projectStringPropValue` returns `ok=false` for any `pv.Kind() != lpg.PropString`, so a numeric
  value is never inserted into a hash index; `cypher/api.go:9704-9712` states the int64 hash index
  is "a Go-API-only building block — a Cypher `CREATE INDEX` never builds one", because an
  int64-keyed equality seek is not a cross-type superset (openCypher `5 = 5.0` is TRUE).
- **Evidence — and a correction to the brief I was given.** The claim that
  `WHERE a.id >= 12345 AND a.id < 12346` also full-scans is **REFUTED at `6f31f61`**. With a btree
  index present it plans `NodeByIndexRangeScan (est. rows=2, exact)`. Isolating the two index
  kinds:

  | index present | `a.id >= 12345 AND a.id < 12346` | `a.id = 12345` |
  |---|---|---|
  | hash only | `NodeByLabelScan` | `NodeByLabelScan` |
  | btree only | **`NodeByIndexRangeScan (est. rows=2, exact)`** | `NodeByLabelScan` |
  | btree only, `>= 12345 AND <= 12345` | **`NodeByIndexRangeScan (est. rows=1, exact)`** | — |

  The prior observation was almost certainly taken with only a default (`hash`) index, which for a
  numeric property is a genuinely empty index. **Numeric ranges already work**, served by the
  hidden `btree.Index[float64]` numeric companion that `createBTreeIndexLocked` (`cypher/api.go:2394`,
  companion built at 2426-2432) creates unconditionally alongside every btree index.
- **Lever — much cheaper than the one `api.go:9710` prescribes.** That comment recommends building
  a *new* float64-keyed hash index. Do not: the third row of the table above shows the closed range
  `[v, v]` **already returns exactly one row via the existing companion**. Numeric equality is a
  degenerate closed range. The fix is a planner peephole that rewrites
  `<prop> = <numeric literal|param>` into `RangeSeek(companion, lo=f64(v), hi=f64(v))` and routes it
  down the *existing* `NodeByIndexRangeScan` path — no new index type, no new storage, no new
  backfill, no new durability surface, no new recovery path. Gate it on the same exact-count
  selectivity test `range_seek_plan.go` already applies.
- **TCK/ACID impact:** TCK-safe, and safer than the alternative. The superset proof is the one
  `docs/audit-index-readiness-2026-07-12.md` already certified for the companion ("the numeric
  companion's monotone `int64→float64` superset property, 0 misses in 200k trials"). Monotonicity
  gives the guarantee that matters: a node whose value is numerically equal to the seek value maps
  into `[f(v), f(v))`, so it is never *dropped*; above 2^53 two distinct int64s can collide onto one
  float64, which *over*-returns, and the **residual Filter is always retained**
  (`cypher/range_seek_plan.go:33`), applying the canonical exact int/float comparator — the same
  big-decimal-exact `cmpInt64Float64` used for aggregation grouping — so `2^53+1 = 2^53.0` still
  evaluates FALSE. Superset + exact residual filter = result-identical to scan+filter. Zero
  DDL-grammar change, and there are no index TCK feature files regardless. ACID untouched (read
  path only).
- **Effort:** S.

### F2. The index seek fires only on a plan-time constant — no correlated seek  [NEW]  (severity: HIGH)

- **What they do:** Neo4j plans `Apply` with a `NodeIndexSeek` on the right-hand side whose seek
  value comes from the left-hand row, so `UNWIND $rows AS r MATCH (a:Person {name: r})` is
  O(rows · log n). Memgraph does the same through its label-property skip list.
- **What GoGraph does:** `resolveSeekValue` (`cypher/api.go:9642-9670`) takes the seek value as the
  **raw source text of the expression** and resolves only `$param` or a literal; anything else
  returns `unsupported seek value`. The seek value is therefore fixed once at plan build time, and
  `exec.NewNodeByIndexSeek(idx, seekVal)` stores a single `expr.Value`. There is no operator that
  re-evaluates a seek key per row.
- **Evidence — measured, and the scope is wider than "UNWIND":**

  | query | plan |
  |---|---|
  | `MATCH (a:Person {name: $s})` | `NodeByIndexSeek` |
  | `UNWIND ['name-1','name-2'] AS nm MATCH (a:Person {name: nm})` | `Selection → CartesianProduct(Unwind, NodeByLabelScan est. 20000)` |
  | `UNWIND ['name-1','name-2'] AS nm MATCH (a:Person) WHERE a.name = nm` | same |
  | `UNWIND $rows AS r MATCH (a:Person {name: r})` | same |
  | **`WITH 'name-1' AS nm MATCH (a:Person {name: nm})`** | `Selection → CartesianProduct(Projection, NodeByLabelScan est. 20000)` |

  The last row is the important one: **even a compile-time constant loses the index once it passes
  through `WITH`**. This is not an UNWIND-specific defect; it is "the seek value must be
  syntactically a literal or a parameter *in the MATCH itself*". Note also that the planner emits a
  full `CartesianProduct`, not an `Apply` — so the label scan is re-driven for every left row.

  Cost, measured (200 seek keys, `RETURN count(a)`):

  | label population N | `UNWIND $rows` (one query) | 200 × single param seek | ratio |
  |---|---|---|---|
  | 5 000 | 5.81 ms | 1.96 ms | 3.0× |
  | 20 000 | 11.80 ms | 1.84 ms | 6.4× |

  The decisive shape is not the ratio but the **scaling**: the seek path is flat in N
  (1.96 ms → 1.84 ms across a 4× larger graph) while the UNWIND path grows with N
  (5.81 ms → 11.80 ms). The UNWIND-driven MATCH is Θ(rows · N); the seek is Θ(rows). This is the
  standard Cypher bulk-load idiom (`UNWIND $batch AS row MERGE/MATCH ...`), so the penalty compounds
  exactly where a load is heaviest, and it explains an order-of-magnitude bulk-load gap far better
  than any missing index kind does.
- **Lever:** two options, and GoGraph's architecture favours the second.
  (a) *Tuple-at-a-time*: add an `Apply`-driven parameterised seek — replace the fixed `expr.Value`
  in `NodeByIndexSeek` with an evaluable expression plus a `RowContext`, and let the planner choose
  `Apply(lhs, IndexSeek(rhs))` whenever the seek key's free variables are all bound by the LHS.
  This is the Neo4j shape.
  (b) *Set-at-a-time* — **the better fit**: when the seek key is bound by `UNWIND` over a list, do
  one multi-key probe and union the postings with a roaring OR, feeding a single bitmap-driven scan.
  This reuses the same set-at-a-time bitmap machinery round 2 identified as GoGraph's structural
  advantage (§2.2 of `docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md`), turns rows × log n into
  one merged posting list, and beats what *both* incumbents do for the batch case. (a) is still
  needed for the general correlated case (seek key from a preceding `MATCH`).
  A trivial third slice worth taking first: constant-fold a `WITH`-projected literal so row 5 of
  the table above collapses to the `$param` case.
- **TCK/ACID impact:** no DDL grammar change and no index/constraint TCK files exist. The TCK
  exposure is plan selection only, and the invariant is unchanged from today's seek: the probe must
  return a superset and the residual `Selection` must be retained. Option (b) must preserve
  `UNWIND`'s row multiplicity — a duplicate key in the list must still yield duplicate rows, so the
  union-bitmap form needs a join back to the driving rows rather than a bare `DISTINCT` bitmap; get
  that wrong and `Unwind` scenarios in the TCK will catch it. Read path only; ACID untouched.
- **Effort:** M for (a) or (b); S for the `WITH`-constant fold.

### F3. The v0.10.0 statistics are inert — nothing on the query path reads them  [NEW]  (severity: HIGH)

- **What they do:** Neo4j's cost planner consumes label/type counts and index selectivity for every
  cardinality estimate (https://neo4j.com/docs/operations-manual/current/performance/statistics-execution-plans/).
  Memgraph's planner uses `ANALYZE GRAPH`'s average-group-size and chi-squared to *choose which
  label-property index to use* and to pick `MERGE` expansion direction
  (https://memgraph.com/docs/fundamentals/indexes#analyze-graph).
- **What GoGraph does:** `cypher/stats_estimate.go:18-23` — *"As of #2097/#2098 these providers ship
  INERT: nothing on the query path calls them yet."* The only consumer of `statsRangeEstimate` /
  `statsEqualityEstimate` is `cypher/explain_estimate.go:151,155`, which appends a display suffix to
  `Engine.Explain` output. `cypher/stats_build.go:36-38` confirms: *"Statistics built here ship
  INERT: no query-path consumer reads them yet, so a rebuild changes no plan."* The shipped
  peepholes (`join_reorder_plan.go`, `range_seek_plan.go`) gate on the separate **exact** count
  providers in `count_estimate.go`, not on these statistics.
- **Evidence:** the `est. rows=20000, exact` / `est. rows=2, exact` annotations in every plan above
  are exact-count-derived, not statistics-derived. Grep for callers of the estimate providers
  outside their own file returns only `explain_estimate.go`.
- **Lever:** wire the histogram to the one decision that currently has no good answer — the
  **range-seek selectivity gate**. Today `range_seek_plan.go` must compute an *exact* in-range count
  by walking the sorted index with an early-exit budget before it may fire. A 256-bucket equi-depth
  histogram answers the same question in O(log B) with a certified 1/B error bound, which is exactly
  the design's stated purpose. Second consumer: anchor selection between two indexed properties
  (which of `a.x = 1` / `a.y = 2` is more selective) — the MCV gives an *exact* answer on heavy
  values, which neither Neo4j nor Memgraph can do. Until a consumer exists, the whole `stats/`
  package (1 189 LOC incl. tests) is dead weight and the v0.10.0 release note overstates what
  shipped.
- **TCK/ACID impact:** statistics may only ever change *which* plan runs, never the result, so the
  existing trustworthiness classification (`estStats` / `estExact` / `estHeuristic` / `estFallback`,
  `stats_estimate.go:161-233`) must remain the gate — a stale or absent statistic must demote to
  `estFallback` and the exact-count plan, which it already does. TCK-neutral if that discipline
  holds. ACID untouched.
- **Effort:** M.

### F4. Statistics have no Cypher/Bolt surface — Go API only  [NEW]  (severity: MEDIUM)

- **What they do:** Memgraph exposes `ANALYZE GRAPH;` as a Cypher statement returning
  `label | property | num estimation nodes | num groups | avg group size | chi-squared value | avg degree`,
  plus `ANALYZE GRAPH DELETE STATISTICS`. Neo4j refreshes automatically in the background
  (`db.index_sampling.background_enabled=true`, `db.index_sampling.update_percentage=5`) and offers
  `db.resampleIndex(name)` / `db.resampleOutdatedIndexes()` as procedures.
- **What GoGraph does:** `Engine.RefreshStatistics(ctx)` (`cypher/stats_build.go:39`) is the sole
  entry point and is a **Go method**. It is not registered in `cypher/procs` — the built-in
  procedure list (`cypher/procs/builtin_db.go:69-74`) is `db.indexes`, `db.constraints`, `db.labels`,
  `db.relationshipTypes`, `db.propertyKeys`, `db.schema.visualization`. The only in-repo caller is
  `examples/26_social_scale_bench/main.go:941`.
- **Evidence:** grep for `RefreshStatistics` finds no registration in the procedure registry.
- **Lever:** register `CALL db.stats.refresh()` (or `ANALYZE GRAPH`, but that needs a DDL-prefix
  branch in `ir.IsDDL`, `ddl_parser.go:88`, whereas a `CALL` needs none). Without it, **a Bolt client
  cannot refresh statistics at all** — the embedder must reach into Go. For a library that also
  ships a Bolt server, that is a real hole. Add a `SHOW STATISTICS`-equivalent yield so an operator
  can see staleness (Δ, deletes, g0, N0 are all already exposed as accessors on `stats.Stats`).
  Manual-only refresh is defensible (Memgraph does the same, and a background goroutine would
  violate the project's no-unbounded-goroutine rule) — the gap is *reachability*, not the policy.
- **TCK/ACID impact:** a new `db.*` procedure is invisible to the TCK (no TCK scenario enumerates
  the procedure namespace — see the existing `db.labels` precedent). Read-only, off the write path.
- **Effort:** S.

### F5. The statistics rebuild materialises an exact frequency table over every distinct value  [NEW]  (severity: MEDIUM — *not measured, see caveat*)

- **What they do:** Neo4j deliberately **samples**: *"To produce a selectivity number, Neo4j runs a
  full index scan in the background… a full index scan is triggered only when the changed data
  reaches a specified threshold"*, bounded by `db.index_sampling.sample_size_limit` (default
  8 388 608). The bound exists precisely to cap the transient cost.
- **What GoGraph does:** `statsAccum` (`cypher/stats_build.go:75-78`) holds
  `freq map[uint64][]statsFreqCell` — **one entry per distinct value**, with an exact count, per
  (label, property), all live simultaneously for the whole scan, and only reduced to a 32-entry MCV
  + 256-bucket histogram in `finalize` (line 109). For a high-NDV property (a UUID or a name on N
  nodes) the table holds Θ(N) `expr.Value` boxes across Θ(labels × props) accumulators before any
  reduction happens. There is no sample cap and no bound in the constructor — which sits awkwardly
  against CLAUDE.md's "no unbounded caches; every cache declares an explicit upper bound".
- **Evidence:** code-read only. **CAVEAT: the empirical measurement of peak heap during
  `RefreshStatistics` was NOT COMPLETED** (the measuring subagent was cut off by a session limit).
  Treat the magnitude as unquantified; the structural claim (no cap, Θ(distinct values) transient)
  is a direct code reading and is solid.
- **Lever:** either (a) cap the frequency table and fall back to reservoir sampling past the cap —
  the Neo4j decision, and the one that makes the memory bound explicit; or (b) exploit the fact that
  the *histogram* only needs the sorted distinct values and the *MCV* only needs the top 32, so a
  bounded-size Space-Saving / Misra-Gries counter (top-k in O(k) space) plus a streaming quantile
  sketch would replace the exact table entirely at bounded memory. (b) is strictly better and is the
  standard answer in the literature.
- **TCK/ACID impact:** none — statistics are inert (F3) and off the write path.
- **Effort:** M.

### F6. HLL approximates an NDV the rebuild already computes exactly  [NEW]  (severity: LOW)

- **What GoGraph does:** `statsAccum.feed` (`cypher/stats_build.go:88-103`) inserts every value into
  `a.ndv` (HLL, p=12, m=4096, relative standard error 1.04/√m ≈ **1.625%**, `stats/hll.go:8-16`)
  *and* into the exact frequency table. In `finalize` (line 109-120) the flattened `entries` slice
  has exactly one element per distinct non-NaN value — i.e. `len(entries)` **is** the exact NDV — and
  it is discarded; the published `Stats.NDV` is the HLL estimate.
- **Evidence:** code-read. The exact-NDV-is-available claim follows directly from `feed`'s
  chain-walk with `statsEquivalent` disambiguation (line 95-102), which guarantees one cell per
  distinct value.
- **Lever:** publish the exact NDV (plus the NaN count, tracked separately) and keep the HLL only if
  and when a *sampled* or *incremental/mergeable* build lands — HLL's value is mergeability and
  fixed footprint under streaming, neither of which the current one-shot exact scan needs. This
  removes a 1.6% error term from equality selectivity for free. If F5's bounded-memory rebuild lands
  first, the HLL becomes genuinely necessary and this finding is void — so **sequence F5 before F6**,
  or do F6 only as part of deciding F5.
- **TCK/ACID impact:** none (inert, off write path).
- **Effort:** S.

### F7. Property-type constraints are ~90% built but have no DDL surface  [NEW]  (severity: MEDIUM)

- **What they do:** Memgraph ships `CREATE CONSTRAINT ON (n:label) ASSERT n.property IS TYPED <T>`
  in **Community**, over `NULL, STRING, BOOLEAN, INTEGER, FLOAT, LIST, MAP, DURATION, DATE,
  LOCALTIME, LOCALDATETIME, ZONEDDATETIME, ENUM, POINT`
  (https://memgraph.com/docs/fundamentals/constraints). Neo4j has property type constraints as
  **Enterprise only** (https://neo4j.com/docs/cypher-manual/current/constraints/).
- **What GoGraph does:** the DDL rejects them — `cypher/ir/ddl_parser.go:996`, *"property type
  constraints (IS :: <TYPE>) are not supported"*. But `graph/lpg/schema/schema.go` already
  implements exactly this feature: `RegisterProperty(name, kind)` declares a `lpg.PropertyKind`, and
  `Schema.Validate` (line 137-149) is *"a runtime enforcement hook, not merely advisory"* — installed
  via `lpg.Graph.SetValidator`, it *"runs inside every SetNodeProperty / SetEdgeProperty call,
  rejecting a value whose kind disagrees with its declaration before the write is applied"* (package
  doc, lines 8-14). `RequireProperty`/`ValidateNode` (line 162, 182) likewise implement an existence
  constraint at the node-finalisation boundary.
- **Evidence:** the enforcement engine, its error types (`ErrTypeMismatch`, `ErrMissingRequired`),
  and its write-path integration all exist and are tested (`validate_kinds_test.go`,
  `validate_typemismatch_test.go`, `enforce_writes_test.go`). What is missing is (i) the parser
  branch and (ii) durability of the declaration.
- **Lever:** wire `REQUIRE n.prop IS :: <TYPE>` to `schema.RegisterProperty`, and persist the
  declaration the same way UNIQUE/NOT_NULL already are (`snapshot.ConstraintSpec`,
  `store/snapshot/constraints.go:57-93`; `txn.OpCreateConstraint`, `store/txn/txn.go:240`). Two
  design points to settle with the user first, per CLAUDE.md decision autonomy: (1) the *scope*
  mismatch — `schema` declares a kind **per property key globally**, whereas Cypher declares it
  **per (label, property)**; the registry needs a label dimension. (2) The project would then have
  **two** existence-constraint mechanisms — `schema.RequireProperty` (node-finalisation) and
  `constraint_check.go` (commit-time touched-set scan) — and one should be retired.
  Also note this is a **place GoGraph can beat Neo4j on licensing**: Neo4j gates both existence and
  type constraints behind Enterprise; GoGraph already has existence in the open, and type would
  match Memgraph Community.
- **TCK/ACID impact:** zero index/constraint TCK feature files, so the DDL grammar addition is free.
  The ACID requirement is the strict one: a type constraint is a **Consistency** invariant, so it
  must be enforced at commit time against the final committed state, exactly as
  `constraint_check.go:113-155` does for NOT NULL, and `CREATE CONSTRAINT` must reject pre-existing
  violations. Rejecting a write must roll the whole transaction back via the existing undo-log path.
- **Effort:** M.

### F8. Index kinds — critical re-assessment of the "bitmap AND replaces composite" claim  [CONFIRMED-R1, with a new caveat]

- **What they do:** Memgraph composite label-property indexes since v3.2.0, `CREATE INDEX ON
  :Person(name, occupation)`, usable for any **leftmost prefix**; Neo4j composite RANGE with the
  rule "equality/IN must cover the leading properties, at most one range predicate, anything after
  it must be an existence check".
- **What GoGraph does:** rejected at parse time (`ddl_parser.go:735`). Round 2 argued a composite
  index type is unnecessary because `label.Index.Intersect` composes two single-property indexes via
  roaring AND.
- **Critical assessment — the claim is *mostly* right but has one hard limit.** On cost, the two are
  not equivalent: a composite index answers `a=1 AND b=2` in Θ(log n + |result|), whereas a
  two-bitmap AND is Θ(|a=1| + |b=2|) — it must materialise both posting lists first. When each
  predicate is individually unselective but jointly selective (1 M nodes, 500 k match each,
  10 match both — the canonical reason composite indexes exist), the AND does ~100 000× more work in
  posting terms. Roaring's container-wise AND makes the constant small enough that this rarely
  dominates at GoGraph's target scale, and since GoGraph does *neither* today, the round-2 lever is
  unambiguously the right first move. But the claim "a composite index type may be unnecessary"
  should be narrowed to: **unnecessary for selectivity, insufficient for ordering.** A composite
  index also provides *ordered iteration on the leading prefix* — Memgraph explicitly serves
  `ORDER BY a, b` from it, ascending or (v3.x) descending. A roaring AND returns a NodeID-ordered
  bitmap and destroys index order by construction, which is exactly round 2's #2163. So composite
  and bitmap-AND are **not interchangeable**, and if index-ordered `ORDER BY` is ever wanted, a
  cursor-style ordered index is the prerequisite for both.
- **Ranking of the remaining missing kinds, for GoGraph specifically:**
  1. **Text (indexed `CONTAINS`/`ENDS WITH`)** — *skip the index, take round 2's §2.1 instead*.
     `STARTS WITH` is already a range on the existing btree (measured 283× in round 2, unshipped).
     `CONTAINS`/`ENDS WITH` need a suffix structure; Neo4j needs a dedicated TEXT index for what
     GoGraph's btree already does for prefixes. Low value.
  2. **Relationship-property index (#2166)** — genuinely useful, both incumbents have it, and
     GoGraph already has a columnar edge-property tier to build on. Worth doing, ranked below F1/F2.
  3. **Point/spatial** — GoGraph has no POINT type in the first place; out of scope until it does.
  4. **Token-lookup** — GoGraph's label bitmap *is* this, and is better than Neo4j's LOOKUP index
     (a roaring bitmap vs a token scan). **Nothing to take.**
  5. **Composite** — see above; low priority, and its ordering half is blocked on #2163.
- **Vector/ANN — recommend NOT doing it.** Both incumbents ship it (Neo4j: Lucene HNSW, GA 5.13,
  cosine/euclidean, quantization 5.23; Memgraph: USearch/HNSW, GA v3.0, ten metrics). It is
  nonetheless the wrong call for GoGraph in 2026: (i) HNSW is a large, memory-hungry structure whose
  quality/recall depends on parameters that need tuning infrastructure GoGraph does not have; (ii)
  ANN is *approximate*, which sits badly with a project whose distinguishing claim is exact counts
  and bit-identical parallel results; (iii) it would need a whole new durability + recovery + crash-
  injection surface for a structure that cannot be cheaply rebuilt; (iv) an embeddable Go library
  competes here with dedicated single-purpose libraries the embedder can already use alongside
  GoGraph, which is not true of the graph engine itself. **Round 1 ranked vector at T3; downgrade it
  to "declined, with reasons".**
- **TCK/ACID impact:** all of the above are DDL/plan additions with zero TCK feature coverage.
- **Effort:** relationship-property index M; composite L (needs the ordered-cursor work).

### F9. UNIQUE recovery silently tolerates a duplicate  [NEW]  (severity: LOW)

- **What GoGraph does:** `Engine.registerRecoveredConstraints` (`cypher/api.go:1296-1348`)
  re-derives the UNIQUE value-set by rescanning the recovered graph and calls
  `SeedUniqueValuesIgnoringDuplicates`, deliberately not rejecting a pre-existing duplicate because
  *"recovery must always succeed"* (lines 1303-1308).
- **Evidence:** code-read; the corresponding NOT NULL path (lines 1339-1342) does no scan at all,
  which is correct since its enforcement is a stateless commit-time predicate.
- **Assessment:** the *decision* is right — refusing to open a database is worse than opening it
  with a flagged inconsistency, and write-time enforcement plus `CREATE CONSTRAINT`'s rejection of
  pre-existing violations means this should be unreachable. But it is **silent**: no metric, no log,
  no `SHOW CONSTRAINTS` column. An operator has no way to learn that a Consistency invariant is
  violated in their data.
- **Lever:** count duplicates found during reseed, emit a Prometheus counter and a warning, and add
  a `state` column to `SHOW CONSTRAINTS` (GoGraph already hard-codes `state: "ONLINE"` for indexes at
  `cypher/show.go:78`; constraints have no state column at all — `ir.ShowConstraintColumns` is
  `name, type, entityType, labelsOrTypes, properties`, `cypher/ir/ddl_show.go:28`). Neo4j surfaces
  index `state` including `FAILED` precisely for this class of problem.
- **TCK/ACID impact:** none; adding a column to a non-TCK-covered `SHOW` output is free.
- **Effort:** S.

### F10. Index lifecycle — both prior HIGH findings verified still fixed  [CONFIRMED-R1]  (severity: —)

- `#1895` (string range-seek fixed 32×`0xFF` sentinel silently dropping high byte-strings):
  **still fixed** — `btree.RangeFrom` and `RangeCountFrom` present at `graph/index/btree/index.go:285`
  and `:304`, with regression coverage in `range_from_test.go`.
- `#1894` (CREATE/DROP INDEX racing a checkpoint could resurrect a dropped index): **still fixed** —
  `recordIndexDef` is called *before* `commitIndexTx` inside the single-writer DDL window in all
  paths (`cypher/api.go:2490` then `:2498`; `cypher/index_binding.go:834` then `:842`), with unwind
  via `forgetIndexDef` on WAL failure (`api.go:2499`, `index_binding.go:845`).
- **Still open:** `#1902`, the WAL-commit-*failure* edge where a racing checkpoint can publish a
  transient failed-CREATE schema def before the DDL unwinds. The 2026-07-12 audit noted the clean
  closure is checkpoint-side and *"also covers the more-serious constraint-DDL instance"* — worth
  re-checking whether the constraint-DDL instance is still open, which I did **NOT** verify.
- **Lifecycle vs the incumbents:** GoGraph builds every index synchronously before `CREATE INDEX`
  returns, so `state` is always `ONLINE` and `POPULATING`/`FAILED` do not exist
  (`cypher/show.go:41,76,78`). For an embedded library this is the *better* contract — no window
  where a query silently misses an index the user just created. **Nothing to take** from Neo4j's
  online build, except that a very large synchronous backfill blocks the single writer; the existing
  morsel-parallel phase-2 (`index_binding.go:110-155`, ctx-cancellable every 4096 nodes) already
  mitigates it.

## Nothing-to-take list

- **Exact relationship count store.** GoGraph maintains exact `E(relType)`, `D(label, relType, dir)`
  and `T(labelA, relType, labelB)` (`graph/index/count/count.go`), with a principled dirty-marking
  escape hatch for relabels that would exceed the fan-out budget (`MarkDirty`, line 294) rather than
  writing a wrong exact. Neo4j keeps exact label/type counts but *sampled* index selectivity;
  Memgraph estimates relationship cardinality as coefficient × cardinality. Maintenance cost is one
  buffered `Delta` per edge write applied under the existing commit barrier, and footprint is bounded
  by *schema* cardinality, not |V| or |E| — cells are deleted when they hit zero (`add`, line 241).
  This is genuinely better than both. **Keep.**
- **Label index as a roaring bitmap.** Strictly better than Neo4j's LOOKUP token index for the
  operations that matter (cardinality in O(1), conjunction via container-wise AND). **Keep.**
- **Synchronous index build / always-ONLINE.** Better contract for an embedded library than Neo4j's
  POPULATING window. **Keep.**
- **No background statistics goroutine.** Neo4j's background sampler would violate the project's
  goroutine-lifecycle and bounded-resource rules; Memgraph is also manual-only. The policy is right —
  only its *reachability* from Cypher/Bolt is wrong (F4). **Keep the policy.**
- **Existence constraints in the open.** Neo4j gates these behind Enterprise. GoGraph should not
  copy that. **Keep.**
- **Vector/ANN index.** Declined — see F8. Both incumbents have it; it is still the wrong investment
  for an exact, embeddable Go graph library.
- **Neo4j's `USING INDEX` hints.** Memgraph has none either; with a cost model this shallow, hints
  would ossify bad plans. Round 2 already filed #2165; leave it low.

## NOT INVESTIGATED

- Empirical peak-memory measurement of `RefreshStatistics` (F5) — subagent cut off by session limit.
  The structural claim stands on code reading; the magnitude does not.
- Per-index resident memory cost (bytes/node for hash vs btree+companion vs label bitmap) — planned,
  not measured.
- HLL relative-error measurement against known NDV (F6) — the redundancy claim is a code reading;
  the 1.625% figure is the design's own stated bound (`stats/hll.go:10`), not measured here.
- Whether `#1902`'s constraint-DDL instance is still open.
- `MERGE`'s internal match path: `UNWIND ['name-1'] AS nm MERGE (a:Person {name: nm})` plans as
  `Merge → Unwind` with no visible seek, but I did not trace whether `MERGE`'s match uses the index
  internally. Given F2's root cause is shared, it almost certainly does not — this is the highest-
  value follow-up, because `UNWIND … MERGE` is *the* bulk-load statement.
- Concurrency behaviour of constraint enforcement under concurrent writers (single-writer engine
  makes this mostly moot, but not verified).
- Memory cost of the count store at high schema cardinality.

## Ranked lever list

| # | Lever | Finding | Effort | Why this rank |
|---|---|---|---|---|
| 1 | Correlated / batched index seek (`UNWIND`- and `WITH`-bound keys) | F2 | M | Only defect that is Θ(rows·N); hits the standard bulk-load idiom; both incumbents solve it |
| 2 | Numeric equality → degenerate range on the existing float64 companion | F1 | S | Removes a whole-type-class blind spot reusing already-certified machinery |
| 3 | Give the inert statistics a consumer (range-seek gate first) | F3 | M | v0.10.0 shipped the hard part; the cheap part is missing |
| 4 | `CALL db.stats.refresh()` + staleness introspection | F4 | S | Statistics are unreachable from Bolt today |
| 5 | Property-type constraints: wire DDL to the existing `schema` enforcement | F7 | M | ~90% built; matches Memgraph Community, beats Neo4j Community |
| 6 | Bound the statistics rebuild's frequency table | F5 | M | Unbounded transient allocation vs CLAUDE.md |
| 7 | Relationship-property index | F8 | M | Both incumbents have it; real gap, but below the above |
| 8 | Surface recovered-duplicate + constraint `state` | F9 | S | Silent Consistency violation is unobservable today |
