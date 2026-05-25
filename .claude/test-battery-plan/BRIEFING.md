# GoGraph Production-Readiness Test Battery — Subagent Briefing

You are populating tasks under the `gograph` rmp roadmap for a comprehensive test battery. **Sprints 58 (test infra) and 59 (core graph) are already populated.** You must populate the sprint(s) assigned to you and ONLY those.

## Project context (one-shot)

GoGraph is a Go graph database at `/Users/flaviocfo/dev/xumiga/GoGraph`, version v2.0.0-rc2. Subsystems already implemented (so tests target *behaviour*, not absence of code):

- **Core graph**: `graph/` (`adjlist`, `csr`, `generation`, `mapper` — FNV-1a 256-way sharded, cross-process stable, `lpg` — labels + 6 PropertyKind: string/int64/float64/bool/time/bytes, `lpg/schema`, `index/{label,hash,btree}`, `index/Manager`, `query` fluent MATCH, `io/{csv,graphml,dot,jsonl}`).
- **Search/analytics**: `search/` — BFS (incl. direction-optimised BFS-DO), DFS, Dijkstra (+bidirectional), Bellman-Ford, A\*, BiBFS, Yen k-loopless, Eppstein, Diameter, Tarjan SCC, BCC + bridges + articulations, Hierholzer (directed+undirected), Kruskal, Prim, Kahn topo, Floyd-Warshall, Johnson, K-core, Triangles, Transitive closure, WCC, Hopcroft-Karp, Hungarian. `search/centrality` Brandes + PageRank + personalised PageRank. `search/community` Leiden + label propagation. `search/flow` Dinic, Edmonds-Karp, push-relabel, Stoer-Wagner, MCMF. `search/extern` semi-external BFS + PageRank over Tier 2 csrfile.
- **Persistence**: `store/wal` (CRC32C frames), `store/snapshot` (atomic dir with manifest, v1/v2/v3 mapper.bin), `store/txn` (Begin/Commit/Rollback), `store/checkpoint` (background WAL→snapshot), `store/recovery` (Open/OpenCtx/OpenString/OpenWithCodec/OpenWithOptions), `store/csrfile` (mmap-backed Tier 2, versioned, 64-byte aligned, `Reinterpret` zero-copy), `store/bulk` (bypasses WAL).
- **Cypher engine**: `cypher/` — full parser (~99.5% TCK), planner with plan_cache LRU, ~30 exec operators (scan_all, scan_label, scan_index_btree/hash, expand, optional_expand, varlen_expand, filter, project, sort, top, limit, distinct, union, unwind, semi_apply, apply, correlated_apply, rollup_apply, eager_aggregation, parallel, single_row, argument, produce_results, create_node, create_relationship, set, remove, delete, detach_delete, merge, create_constraint, create_index, drop_constraint, drop_index, index_writeback, procedure_call, temporal_literal, write_graph, cyphermorphism, global_aggregate_adapter), aggregations (count/sum/avg/min/max/collect/stdDev/stdDevP/percentileCont/percentileDisc), built-in functions and procedures, EXPLAIN/PROFILE, dbhits, WAL-durable writes.
- **Bolt server**: `bolt/proto` (v5.0-v5.6 + v4.4 fallback, state machine: Hello/Logon/Begin/Run/Pull/Discard/Commit/Rollback/Reset/Goodbye), `bolt/packstream` (PackStream encoding), `bolt/server` (TCP server, TLS hot-reload, MaxConnections, cumulative cap, InFlightCursorCap, bookmarks, routing table, auth no-auth/basic/scheme-unknown).
- **Shape generators** are in `internal/shapegen/*` (Sprint #58 creates them) — assume they exist with this API: every shape has a constructor returning `*lpg.Graph[N,W]`; the registry exposes them for property-based iteration.

## Existing test-battery deps in go.mod

- `pgregory.net/rapid v1.3.0` for property-based testing.
- `go.uber.org/goleak v1.3.0` for goroutine leak detection.
- `github.com/cucumber/godog v0.14.1` already used by Cypher TCK.

**neo4j-go-driver is NOT yet a dep.** Sprint #74 must add it as a test-only dep.

## Decisions already made

- One **rmp task = one atomic test scenario** (the user's standing preference; do NOT collapse scenarios into one task).
- Three test layers: `short` (default, every PR, <60s/pkg), `soak` (build tag `soak` or env `SOAK_FULL=1`, minutes), `nightly` (build tag `nightly`, hours).
- TDD failure policy: when a test you propose would fail today against an existing bug, mark it `Layer: short` anyway but note in TR: "expected to surface bug; create BUG sub-task at implementation time and `t.Skip` with a reference until the bug is fixed in a follow-up sprint." Do not create the BUG sub-task now — that comes later when implementation begins.
- Documentation strictly in **English**, no spelling/grammar errors, faithful to current code.

## Shape catalogue (the cross-product axis)

You must cross every relevant test in your sprint with shapes from this catalogue (the test is a row, the shape is a column variation expressed inside the task TR/AC):

**Family 1 — Trivial:** E0, K1, K2, self-loop, parallel digon, isolated-only, universal self-loops.
**Family 2 — Pathological skeletons:** Pn (path), Cn (cycle), Star Sn, double-star, caterpillar, spider, lobster, Kn (complete), Km,n (complete bipartite), K(n1..nk) multipartite, hypercube Qd, grid Lm×n, torus, rook, Möbius ladder, ladder, prism, theta.
**Family 3 — Trees:** balanced binary, complete k-ary, Prüfer random, path-degenerate.
**Family 4 — Named specials:** Petersen, dodecahedral, Goldner-Harary, Moser spindle, Kneser.
**Family 5 — DAGs:** transitive tournament, diamond, layered, Lengauer-Tarjan dominator, build-dependency, negative-weight acyclic.
**Family 6 — Random:** Erdős-Rényi G(n,p)/G(n,m), Barabási-Albert (power-law), Watts-Strogatz (small-world), R-MAT (Graph500), random d-regular, configuration model, RGG (random geometric), SBM/planted partition, LFR.
**Family 7 — Real-world datasets:** DIMACS9 USA road (24M nodes), SNAP web-Google/soc-LJ1/cit-HepPh, LDBC SNB SF1/SF10, LDBC Graphalytics cit-Patents/dota-league/kgs.
**Family 8 — Adversarial paths:** negative cycle, zero weights, extreme weight magnitudes, decoy graph for bidirectional, disconnected forest, mixed-property typed weights.
**Family 9 — Adversarial centrality/community:** super-node star, bridge-connected double cluster, ring-of-cliques, planted partition, LFR overlapping, caveman.
**Family 10 — Adversarial flow:** zero-capacity edges, capacity overflow, anti-parallel edges, Dinic worst case, push-relabel worst case, Edmonds-Karp bad case, bipartite-to-flow reduction, MCMF on assignment, Stoer-Wagner worst.
**Family 11 — Adversarial matching:** imbalanced bipartite, forced single augmenting path, dense bipartite Kn,n, isolated vertices.
**Family 12 — MST tie-breaks:** all-equal-weight Kn, Kruskal-vs-Prim divergence, forest MST, negative-weight MST.
**Family 13 — Eulerian edge-cases:** figure-eight multi-tour, no Eulerian (3 odd-degree), multigraph Eulerian, directed Eulerian.
**Family 14 — SCC/BCC adversarial:** chain of SCCs, single ring SCC, many articulation points, many bridges, dense single block.
**Family 15 — Index stressors:** single-label universe, label cardinality explosion, Zipfian labels, hash-shard-0 collision, BTree adversarial insertion order, range-query at page boundary.
**Family 16 — Storage stressors:** mega-string properties, Unicode astral plane, embedded NUL bytes, year-9999/year-1 time, int64 ±Max, float64 ±Inf/NaN/denormal, zero-payload bytes, fragmented mapper, mapper shard-0 storm.
**Family 17 — Concurrency stressors:** hot-shard write storm, reader-writer overlap on hub, mapper shard-0 storm, goroutine-leak bait, ctx-cancel mid-algorithm, plan-cache thrash, sustained mixed workload (soak).
**Family 18 — Extremes:** maximum density Kn, ultra-sparse mega (1e7 nodes), diameter = N-1, star-of-stars hierarchical, multigraph DAG, signed multigraph, weighted multigraph with property weights.
**Family 19 — Cypher-specific:** varlen-friendly, pattern-cycle trap, OPTIONAL MATCH null bait, UNWIND list-vs-graph mix, MERGE race, aggregation skew, DETACH DELETE hub.
**Family 20 — Bolt transport:** streaming Cartesian, wide rows, pipelining burst, TLS + zlib.

## Task creation rules (mandatory)

Every task must use this rmp invocation pattern:

```
rmp task create -r gograph -t "<title under 90 chars>" --type TASK -p <priority 5-8> \
  -fr "<Functional requirements — what observable behaviour is asserted; reference the shape family/families and the capability under test>" \
  -tr "<Technical requirements — file path, key Go APIs/types involved, layer (short|soak|nightly), key Go test patterns (rapid, goleak, AllocsPerRun, race), constraints>" \
  -ac "<Acceptance criteria — concrete, verifiable bullets: behaviour assertions, race-clean, goleak-clean, layer, golden file paths if any>"
```

**Important conventions**:

- Each field max 4096 chars. Keep them tight (≈3–5 concise sentences per field).
- Title under 90 chars, no period at the end.
- `--type`: `TASK` for test scenarios; `CHORE` for infra/doc/CI; `IMPROVEMENT` for enhancements to existing code; `BUG` is reserved for sub-tasks at implementation time, not here.
- `-p` priority: 7 for foundational; 6 default; 5 for soak/nightly-only scenarios.
- Mention the layer (short/soak/nightly) explicitly in TR.
- Mention every shape family the test exercises, by number (e.g., "Family 2 Pn, Family 6 BA").
- Where the test will likely surface an existing bug, note it in TR.

After creating ALL tasks for your sprint, run:

```
rmp sprint add-tasks -r gograph <sprint_id> <comma,separated,task,ids>
```

Then run `rmp sprint show -r gograph <sprint_id>` and report the final task_order array.

## Quality bar (look at Sprint #58 and #59 for examples)

Examples are tasks already created with IDs 505–558. Each strictly follows the FR/TR/AC pattern. Match their granularity (1 task = 1 atomic scenario; do NOT bundle "BFS + DFS on path graph" into one task — those are two tasks).

## What you must do

Read the section below for your sprint(s). Each lists the exact task titles to create. **Do not invent new scenarios beyond the list; do not skip any.** For each title, expand it into a complete `rmp task create` invocation with FR/TR/AC populated according to the rules above. After all tasks are created, add them to the sprint with `rmp sprint add-tasks`.

Report at the end: sprint id, task ids created, sprint show output.
