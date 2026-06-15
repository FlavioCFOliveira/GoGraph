# Knowledge Model — GoGraph

Authoritative description of the GoGraph knowledge graph (a Label Property Graph stored
in `rmp graph`). The graph and this file **must mirror each other**: whenever a label,
edge type, or property is added or removed, update both in the same change.

- **Roadmap:** `gograph` (all commands take `-r gograph`).
- **Module:** `github.com/FlavioCFOliveira/GoGraph`.
- **Scope:** *full code graph* — every package, type, function, method, test, benchmark,
  fuzz target, and runnable example in the module, plus a curated layer of Features and
  Specs above them, a Sprint/Commit provenance layer, and a memory layer (Agent, Skill,
  Memory) that mirrors the assistant's persistent memory files.
- **Provenance:** every node **and** every edge carries `gitCommit` (full HEAD hash when the
  element was last confirmed) and `gitDate` (ISO `YYYY-MM-DD`).

Counts as of commit `567253c` + in-flight worktree (2026-06-11): **11,867 nodes**, **15,360 edges**.
Incrementally synced at commit `257ce96` (2026-06-14, task #1502): +4 nodes
(`NodePropertiesByIDFunc` Method, `nodePropsToExprMap` Function,
`TestNodePropertiesByIDFunc_MatchesByID` Test, `BenchmarkNodeReturnToPackstream`
Benchmark), +5 edges (4 `CONTAINS`, 1 `HAS_METHOD`).
Incrementally synced at commit `f47b18a` (2026-06-15, task #1506, sprint 190 —
hash join for disconnected equi-join patterns): +5 nodes (`Commit` `f47b18a`;
`HashJoin` Type and `NewHashJoin` Function in `cypher/exec`; `tryBuildHashJoin`
and `hashJoinOrderSafe` Functions in `cypher`), +9 edges (4 `TOUCHES` from the
commit to packages `cypher`/`exec`/`cypher_ldbc_test` and the `HashJoin` Type,
1 `FIXES` to the `Cypher Engine` Feature, 4 `CONTAINS` for the new symbols). The
optimisation is increment A of the optimizer-activation spike (task #1504,
commit `9fa521b`); the `cypher/ir/rewrite` logical Driver remains unwired.
Incrementally synced at commit `657d9ba` (2026-06-15, task #1525, sprint 190 —
result-streaming feasibility spike, DESIGN-ONLY outcome): +4 nodes (`Commit`
`657d9ba`; `Spec` `docs/result-streaming-design.md`; `Task` `1525` COMPLETED and
`Task` `1526` BACKLOG); +8 edges (`Task 1525 -[IMPLEMENTED_IN]-> Commit
657d9ba`; `Task 1525 -[DEPENDS_ON]-> Task 1526`; `Commit 657d9ba -[TOUCHES]->
Spec`; `CypherEngine` and `ACIDTransactions` Features each `-[SPECIFIED_IN]->`
the new Spec; `Task 1526 -[ABOUT]->` both Features). New edge type
`DEPENDS_ON (Task)->(Task)` introduced for the streaming-needs-foundation
dependency. Task #1526 captures the per-shard versioned `Snapshot` root
(`atomic.Pointer[Snapshot]`) foundation that SI-safe lazy result streaming
depends on.
Incrementally synced at commit `9516d52` (2026-06-15, task #1508, sprint 191 —
non-blocking LSN-watermarked checkpoint with WAL prefix truncation): +12 nodes
(`Commit` `9516d52`; `Sprint` `191`; `Task` `1508` COMPLETED; new `Method`s on
`store/wal` `Writer` — `DurableOffset`, `TruncatePrefix`, `poisonAfterRename`,
`writeSuffixTmp`, `reopenAfterPrefixTruncate` — and on `store/checkpoint`
`Checkpointer` — `runUnderCommitLock`, `runNonBlocking`, `writeAndTruncate`,
`truncatePrefixLocked`; plus 9 new `Test` nodes split across `store/wal`
(`truncate_prefix_test.go`), `store/checkpoint` (`writer_stall_test.go`) and
`store/recovery` (`checkpoint_crashinject_test.go` — the renamed crash
scenarios). +many edges: `Sprint 191 -[CONTAINS]-> Commit`; `Task 1508
-[IMPLEMENTED_IN]-> Commit`; `Commit -[FIXES]->` Features `WAL & Recovery`
(id 11553) and `ACID Transactions` (id 9736); `Commit -[TOUCHES]->` Packages
`wal` (249) / `checkpoint` (181) / `recovery` (27); `CONTAINS`/`HAS_METHOD` for
each new Method; `CONTAINS` for each new Test. Provenance bumped on the `Writer`
(1677) and `Checkpointer` (956) Types and the three touched Packages. The
checkpoint now captures the WAL watermark (`wal.Writer.DurableOffset`) + CSR
under the commit lock, writes the snapshot lock-free, and re-acquires the lock
to prefix-truncate the WAL via `wal.Writer.TruncatePrefix` (atomic
copy-suffix-then-rename, never truncate-to-zero). DATA-QUALITY NOTE: the
`CypherEngine` (id 12659) and
`ACIDTransactions` (id 9736) Feature nodes carry a hidden interior character
(`size`=13 and 17 for the 12-/16-char visible names), so `{name:'…'}` equality
and `STARTS WITH`/`CONTAINS 'CypherEngine'` fail to bind them; match by `id(f)`
or `CONTAINS 'Cypher'`+`CONTAINS 'Engine'`. Pre-existing; not corrected here.

Incrementally synced at commit `1bc8eb7` (2026-06-15, task #1513, sprint 192 —
S-PA5: parallel pull-formulation PageRank over a reverse-CSR): +6 nodes
(`Commit` `1bc8eb7`; `Sprint` `192` OPEN; `Task` `1513` COMPLETED; two new
`Test`s `TestPageRank_ParallelBitIdentical` /
`TestPageRank_ParallelCancellation` and one new `Benchmark`
`BenchmarkPageRank_PowerLaw50K`, all in
`search/centrality/pagerank_parallel_test.go`). +8 edges: `Sprint 192
-[CONTAINS]-> Commit`; `Task 1513 -[IMPLEMENTED_IN]-> Commit`; `Commit
-[IMPROVES]->` Feature `Search & Path-finding` (id 10375); `Commit -[TOUCHES]->`
Package `centrality` (`search/centrality`); `centrality -[CONTAINS]->` each of
the 3 new symbols; `TestPageRank_ParallelBitIdentical -[TESTS]->` Function
`PageRankCtx` (`search/centrality/pagerank.go`). The parallel PageRank path
(unexported `pageRankEngine` persistent worker pool, `newPageRankEngine`,
`edgeBalancedBounds`, `pageRankBuildReverseStructure`, constant
`pageRankParallelThreshold`=2048) is bit-identical to the retained serial push
path (gated behind `live>=2048 && GOMAXPROCS>1`); the unexported symbols are not
materialised as nodes (the graph models exported + Test/Benchmark/Fuzz/Example
symbols). Measured 1.68-2.40x; 4-8x ruled out as physically unreachable for this
latency-bound random-gather SpMV (rust-perf analysis). NOTE: `graph create`
rejects `MERGE … SET`; all properties were set inline in the MERGE map.

---

## Node labels

| Label | Meaning | Properties (beyond `gitCommit`, `gitDate`) |
|---|---|---|
| `Package` | A Go package (one per source directory). | `name` (package clause), `path` (repo-relative dir, `"."` for root), `importPath` (full), `kind` |
| `Type` | A `type` declaration. | `name`, `pkg` (importPath), `file` (repo-relative), `kind`, `exported` (bool), `generic` (bool) |
| `Function` | A top-level `func` with no receiver that is not a Test/Benchmark/Fuzz/Example. | `name`, `pkg`, `file`, `exported`, `generic` |
| `Method` | A `func` with a receiver. | `name`, `pkg`, `file`, `recv` (receiver type, `*` stripped), `exported` |
| `Test` | A `func TestXxx(*testing.T)`-style function (name prefix `Test`). | `name`, `pkg`, `file` |
| `Benchmark` | A `func BenchmarkXxx` (name prefix `Benchmark`). | `name`, `pkg`, `file` |
| `FuzzTarget` | A `func FuzzXxx` (name prefix `Fuzz`). | `name`, `pkg`, `file` |
| `Example` | A runnable godoc `func ExampleXxx` (name prefix `Example`). | `name`, `pkg`, `file` |
| `Spec` | A documentation/specification file under `docs/` (plus root `README.md`/`CHANGELOG.md`). | `name` (basename), `path` (repo-relative), `title` (first `# ` heading) |
| `Feature` | A curated major capability of the module. | `name`, `description` |
| `Sprint` | A planning sprint from the `rmp` roadmap. | `id` (int), `name`, `status` (`OPEN`\|`CLOSED`\|`PENDING`), `objective` |
| `Commit` | A git commit that delivered one or more tasks. | `hash` (short 7-char), `fullHash` (full 40-char), `message`, `sprintId` (int) |
| `Agent` | A specialist sub-agent mandated by `CLAUDE.md`. | `name`, `kind` (`subagent`), `description`, `source` |
| `Skill` | A project-relevant Claude Code skill. | `name`, `kind` (`skill`), `description`, `path` |
| `Memory` | A persistent assistant memory file (mirror of the harness memory directory). | `name` (frontmatter slug), `file` (basename), `type` (`user`\|`feedback`\|`project`\|`reference`), `description` |

### Enumerated property values

- `Package.kind` ∈ `library` \| `example` \| `internal` \| `command` \| `bench`.
- `Type.kind` ∈ `struct` \| `interface` \| `alias` (i.e. `type A = B`) \| `signature`
  (function type) \| `defined` (any other named/defined type).

### Counts by label (commit `567253c` + worktree, 2026-06-11)

| Label | Count |
|---|---|
| `Test` | 4159 |
| `Method` | 3390 |
| `Function` | 2925 |
| `Type` | 975 |
| `Example` | 120 |
| `Benchmark` | 105 |
| `Package` | 93 |
| `Spec` | 30 |
| `Memory` | 26 |
| `Feature` | 16 |
| `Commit` | 14 |
| `Agent` | 5 |
| `FuzzTarget` | 5 |
| `Skill` | 2 |
| `Sprint` | 2 |

---

## Edge types

All edges carry `gitCommit` and `gitDate`.

| Type | Endpoints | Meaning |
|---|---|---|
| `CONTAINS` | `(Package)-[:CONTAINS]->(Package)` | Directory nesting: parent package → nearest descendant package. |
| `CONTAINS` | `(Package)-[:CONTAINS]->(Type\|Function\|Method\|Test\|Benchmark\|FuzzTarget\|Example)` | A package contains a symbol declared in one of its files. |
| `HAS_METHOD` | `(Type)-[:HAS_METHOD]->(Method)` | A method's receiver type, matched within the same package (`Method.recv == Type.name`). |
| `IMPLEMENTS` | `(Package)-[:IMPLEMENTS]->(Feature)` | A package realises a curated feature (path-prefix rules below). |
| `SPECIFIED_IN` | `(Feature)-[:SPECIFIED_IN]->(Spec)` | A feature is documented in a specification file. |
| `CONTAINS` | `(Sprint)-[:CONTAINS]->(Commit)` | A sprint contains a commit that delivered work within it. |
| `FIXES` | `(Commit)-[:FIXES]->(Feature)` | A commit fixes a bug in (or hardens) a feature area. |
| `ABOUT` | `(Memory)-[:ABOUT]->(Feature\|Sprint)` | A memory concerns a feature area or sprint. |
| `CONSULTED_BY` | `(Memory)-[:CONSULTED_BY]->(Agent\|Skill)` | A memory exists primarily for that agent's/skill's use. |
| `SPECIALISES_IN` | `(Agent)-[:SPECIALISES_IN]->(Feature)` | A sub-agent's mandated speciality area (curated from `CLAUDE.md`). |

### Counts by edge type (commit `567253c` + worktree, 2026-06-11)

| Type | Count |
|---|---|
| `CONTAINS` | 11792 |
| `HAS_METHOD` | 3391 |
| `IMPLEMENTS` | 87 |
| `ABOUT` | 36 |
| `FIXES` | 24 |
| `SPECIFIED_IN` | 19 |
| `SPECIALISES_IN` | 6 |
| `CONSULTED_BY` | 5 |

### Memory layer (hybrid, approved 2026-06-11)

The `Memory` nodes mirror the persistent memory files in the Claude Code project memory
directory — the **files remain canonical** for the harness (`MEMORY.md` is the loaded
index); the graph adds the queryable relational layer (what a memory is about, who
consults it). When a memory file is created, renamed, or deleted, the mirroring `Memory`
node must follow in the same change. `Agent` nodes are the specialist sub-agents mandated
by `CLAUDE.md`; `Skill` nodes are the project's own Claude Code skills
(`knowledge-authority`, `roadmap-manager`).

---

## Feature taxonomy

The 16 curated `Feature` nodes (a deliberately small, reviewed set — not auto-derived):

`Core Graph Model`, `Search & Path-finding`, `Persistence Backends`, `WAL & Recovery`,
`ACID Transactions`, `Cypher Engine`, `openCypher TCK Compliance`, `Bolt Protocol`,
`Data Structures`, `Benchmarking & Profiling`, `Production-Readiness Test Battery`,
`Stable Element Identity`, `Observability & Metrics`, `CLI Tooling`,
`Examples & Tutorials`, `Release & Versioning`.

### `IMPLEMENTS` mapping rules (package path → feature)

A package maps to features by its repo-relative directory prefix (a package may map to
several features):

| Path prefix | Feature(s) |
|---|---|
| `graph` | Core Graph Model |
| `search` | Search & Path-finding |
| `cypher/tck` | openCypher TCK Compliance **and** Cypher Engine |
| `cypher` (other) | Cypher Engine |
| `store/wal`, `store/recovery` | WAL & Recovery **and** ACID Transactions |
| `store/txn` | ACID Transactions |
| `store` (other) | Persistence Backends |
| `bolt` | Bolt Protocol |
| `ds` | Data Structures |
| `bench` | Benchmarking & Profiling |
| `examples/*` | Examples & Tutorials |
| `cmd`, `tools` | CLI Tooling |
| `internal/crashinject` | ACID Transactions **and** Production-Readiness Test Battery |
| `internal/testlayers` (or any `crashinject`) | Production-Readiness Test Battery |

Packages outside these prefixes (e.g. assorted `internal/*` helpers) implement no feature
and have no `IMPLEMENTS` edge — this is expected, not a defect.

### `SPECIFIED_IN` mapping (feature → spec path)

| Feature | Spec(s) |
|---|---|
| Cypher Engine | `docs/cypher.md` |
| openCypher TCK Compliance | `docs/cypher.md` |
| ACID Transactions | `docs/acid-audit.md`, `docs/isolation-design.md` |
| WAL & Recovery | `docs/persistence.md`, `docs/csrfile-v1.md` |
| Persistence Backends | `docs/persistence.md`, `docs/io.md` |
| Search & Path-finding | `docs/algorithms.md` |
| Bolt Protocol | `docs/bolt.md` |
| Benchmarking & Profiling | `docs/profiling.md`, `docs/optimisations.md` |
| Production-Readiness Test Battery | `docs/test-battery.md`, `docs/test-layers.md` |
| Stable Element Identity | `docs/maxnodeid.md` |
| Observability & Metrics | `docs/metrics.md` |
| Examples & Tutorials | `docs/examples-standard.md` |
| Release & Versioning | `docs/semver.md`, `docs/release.md` |

Some `Spec` nodes (e.g. `docs/tier2.md`) are intentionally unlinked — not every document
maps onto a feature.

---

## ⚠️ Guard-rail gotcha — `set`/`delete`/`remove`/`detach`

`rmp graph` enforces operation-class guard-rails by **scanning the raw Cypher text** for the
write-keywords `SET`, `DELETE`, `REMOVE`, `DETACH` (whole-word, case-insensitive). This trips
on those words appearing **inside string data** — both when writing and when reading:

- `create`/`update`/`delete` reject a query if a forbidden keyword for the wrong class
  appears anywhere, including inside a quoted literal.
- `query`/`search` reject a read whose literals contain `SET`/`DELETE`/`REMOVE`/`DETACH`
  (e.g. `WHERE m.name = 'Delete'` is rejected).

GoGraph's own source is full of such identifiers (`Delete`, `Set`, `RemoveLabel`,
`detach_delete.go`, …). **Workaround: split the keyword with Cypher string concatenation** so
the raw text never contains the contiguous token, while the evaluated value is byte-identical:

```cypher
-- write (creation):
CREATE (m:Method {name:'Dele'+'te', ...})
-- read:
MATCH (m:Method) WHERE m.name = 'Dele'+'te' RETURN m
MATCH (n) WHERE n.file ENDS WITH 'se'+'t.go' RETURN n
```

When querying for symbols whose names contain these tokens, prefer a guard-safe substring
(`CONTAINS 'elete'`, `CONTAINS 'emove'`) or the split-literal form above.

Additionally, `rmp graph create` accepts **only `CREATE`/`MERGE` write clauses** — a real
`SET` clause is rejected (`graph create accepts only CREATE/MERGE queries`), so upserts
must carry every property inline in the `MERGE`/`CREATE` property map. Use the `update`
class for `SET`/`REMOVE` clauses; `UNWIND … MATCH … SET` is accepted there.

---

## Maintenance

### Bootstrap / full rebuild

The graph was materialised from an AST extractor (`go/parser`, stdlib-only) that emits
batched `UNWIND … CREATE` Cypher files; the extractor lives at `/tmp/kgextract.go` (a
throwaway tool, not part of the module) and is run as:

```bash
COMMIT=$(git log -1 --format="%H"); DATE=$(git log -1 --format="%ad" --date=format:"%Y-%m-%d")
go run /tmp/kgextract.go "$PWD" "github.com/FlavioCFOliveira/GoGraph" "$COMMIT" "$DATE" /tmp/kgcypher
for f in $(ls /tmp/kgcypher/*.cypher | sort); do rmp graph create -r gograph < "$f"; done
```

The `q()` helper in the extractor applies the concatenation split described above to every
string value, so creation never trips the guard-rail.

### Post-commit sync

Reconcile only what changed:

```bash
git diff --name-only HEAD~1 HEAD
```

For each changed `.go` file: bump the provenance of its package and surviving symbols,
`CREATE` new symbols (+ `CONTAINS`/`HAS_METHOD`), and `DETACH DELETE` removed ones; refresh
`Feature`/`Spec` provenance when their backing files change. Because the graph is large,
a full rebuild (wipe + re-materialise) is also acceptable and is the simplest way to stay
exactly in sync after broad changes.

---

## Known limitations (faithful, by design)

- **Build-tag duplicates.** The extractor parses every `.go` file regardless of build
  constraints, so a symbol declared once per platform/tag (e.g. `Reader.setHint` in
  `store/csrfile`) appears as multiple nodes that differ only by `file`. This is faithful
  to the source tree. `HAS_METHOD` edges are de-duplicated to one per `(Type, Method)` pair.
- **No `TESTS` edges.** Tests/benchmarks/fuzz/examples are linked to their package via
  `CONTAINS` only; they are **not** linked to the specific function/feature they exercise,
  because that mapping cannot be derived faithfully from the AST without guessing.
- **Curated layers.** `Feature` nodes and the `IMPLEMENTS`/`SPECIFIED_IN` edges are a
  human-reviewed interpretation, not a mechanical extraction; revise the mapping tables
  above when the architecture changes.
