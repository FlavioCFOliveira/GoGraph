# Atomic Task Titles per Sprint (60–76)

Read `.claude/test-battery-plan/BRIEFING.md` first for context, conventions, and the catalogue of shapes (Families 1–20). Each row below is one rmp task. The subagent must:

1. Expand each title into a full `rmp task create -r gograph -t "<title>" --type TASK -p <prio> -fr ... -tr ... -ac ...` invocation following the BRIEFING rules. Family references in the title (e.g. "Sec.2 Pn") are guidance for FR/TR.
2. Capture the resulting task id (returned as JSON `{"id": N}`).
3. After all tasks in a sprint are created, run `rmp sprint add-tasks -r gograph <sprint_id> <comma,separated,ids>`.
4. Run `rmp sprint show -r gograph <sprint_id>` and report the `summary` and `task_order` arrays.

Use `--type TASK` for test scenarios, `--type CHORE` for infra/CI/doc, `--type IMPROVEMENT` for enhancements to existing code. Default `-p 6`; bump to `-p 7` for foundation tests and `-p 5` for soak-only or nightly-only scenarios.

---

## Sprint 60 — LPG + schema + indexes + query (28 tasks)

1. LPG SetNodeLabel/HasNodeLabel/RemoveNodeLabel on Star, Kn, Km,n
2. LPG SetEdgeLabel + edge-label scan on multigraph parallels (Family 1)
3. LPG: 6 PropertyKind round-trip on K1 and Pn
4. LPG: typed property update idempotence (rapid)
5. LPG: DelNodeProperty/DelEdgeProperty + NodeLabels listing
6. LPG: concurrent SetNodeProperty across 16 shards (race)
7. schema.Validate ok + fail across all 6 PropertyKind
8. schema.Validate: missing required property returns typed error
9. schema.Validate: type mismatch returns typed error
10. schema: graph rejects writes that violate schema after enabling
11. index/label: roaring bitmap on single-label universe (Family 15.1)
12. index/label: label-cardinality explosion (Family 15.2)
13. index/label: Zipfian label distribution (Family 15.3)
14. index/label: AddRange + RemoveRange consistency under concurrent writes
15. index/hash: equality match correctness on string / int64 / float64 keys
16. index/hash: hash-shard-0 collision storm correctness (Family 16.10)
17. index/hash: degenerate property-value singleton (Family 15.4)
18. index/btree: range query inclusivity at page boundary (Family 15.10)
19. index/btree: adversarial ascending then descending insertion (Family 15.9)
20. index/btree: empty range, single-key range, full range
21. index/btree: float64 ordering with ±Inf and NaN
22. index/Manager: Change-event fanout to subscribers under concurrent writes
23. index/Manager: subscriber registration/unregistration race
24. query: fluent MATCH with label and property filter on LDBC SF1 (Family 7)
25. query: MATCH composing multiple labels (intersection)
26. query: MATCH with range property filter using BTree
27. Property-based: label round-trip preserves SetNodeLabel insertion order
28. Property-based: property update commutes for distinct keys

## Sprint 61 — IO formats (20 tasks)

1. csv read: header skip, comments, bad-weight error, short row
2. csv read: ctx-cancel mid-stream
3. csv write: round-trip on all 6 PropertyKind
4. csv: 1 GB streaming write + read under bounded memory (soak)
5. csv: Unicode astral plane in node ids (Family 16.2)
6. csv: embedded NUL bytes returns typed error
7. csv: fuzz expand seeds in graph/io/csv/testdata/fuzz
8. graphml round-trip on classic shapes (Pn, Cn, Star, Kn, Km,n)
9. graphml: bad XML returns typed error, no panic
10. graphml: typed properties in attribute syntax (Family 16 adversarial)
11. graphml: empty graph and isolated-only graph
12. graphml: fuzz expand seeds
13. dot write: special-char IDs quoted correctly
14. dot write: directed and undirected edge syntax
15. dot write: round-trip via Graphviz dot if installed, else byte-equal golden
16. jsonl read: multi-type records (Node + Edge + Property)
17. jsonl write: 6 PropertyKind round-trip
18. jsonl: unknown record type returns typed error
19. jsonl: ctx-cancel mid-stream
20. jsonl: 100 MB streaming round-trip under bounded memory (soak)

## Sprint 62 — Search: traversal & path-finding (32 tasks)

1. BFS on Pn (depth n)
2. BFS on Cn
3. BFS on Star (depth 1, fan-out N-1)
4. BFS on Kn (saturates frontier on hop 1)
5. BFS on hypercube Qd (uniform expansion)
6. BFS on grid Lm×n
7. BFS on disconnected forest (multiple components)
8. BFS-DO direction-optimised on Barabási-Albert hub graph
9. DFS iterative on Pn (no stack overflow)
10. DFS on balanced binary tree
11. Dijkstra on Kn with random weights
12. Dijkstra on grid with Manhattan weights
13. Dijkstra on zero-weight edges (Family 8.3)
14. Dijkstra on extreme-magnitude weights (Family 8.4 overflow check)
15. Dijkstra rejects negative edge (typed error, Family 8.1)
16. Dijkstra on disconnected forest
17. Bellman-Ford detects negative cycle (Family 8.2)
18. Bellman-Ford on acyclic with negatives (correct)
19. A* on grid with Manhattan heuristic (admissible) equals Dijkstra
20. A* on Family 8.7 decoy graph (heuristic helps)
21. BiBFS on Pn long path: wall-clock < 2x BFS one-way
22. Bidirectional Dijkstra on DIMACS9 NY subset (cross-check vs Dijkstra)
23. Yen k-shortest loopless on Family 8.8 worst-case
24. Yen k-shortest on Star with k > number of paths returns truncated list
25. Eppstein k-shortest comparison vs Yen on small graphs
26. Diameter on Pn equals n−1
27. Diameter on Cn equals floor(n/2)
28. Diameter on Kn equals 1
29. Property-based: BFS-distance ≤ Dijkstra-distance on non-negative weights (rapid)
30. Property-based: BiBFS dist equals BFS dist
31. Property-based: A* with zero heuristic equals Dijkstra
32. ctx-cancel mid-Dijkstra on 1M-node graph: stops within 50 ms

## Sprint 63 — Search: structural (26 tasks)

1. Tarjan SCC on chain of SCCs (Family 14.1)
2. Tarjan SCC on single ring SCC (Family 14.2)
3. Tarjan SCC on transitive tournament (N singletons)
4. Tarjan SCC condensation produces DAG (topological order check)
5. BCC on tree (every edge a bridge)
6. BCC on dense block (== 1 BCC)
7. BCC on chain of triangles (articulation points, Family 14.3)
8. Bridges on Cn (== 0)
9. Hierholzer undirected on figure-eight (multi-tour merge, Family 13.1)
10. Hierholzer rejects graph with 3 odd-degree vertices (Family 13.2)
11. Hierholzer on multigraph Eulerian (Family 13.3)
12. Hierholzer directed Eulerian (Family 13.4)
13. Kruskal MST on all-equal-weight Kn (canonical tie-break, Family 12.1)
14. Prim MST on all-equal-weight Kn with same canonical order
15. Kruskal vs Prim divergence test (Family 12.2)
16. MST on disconnected forest returns minimum spanning forest
17. MST on negative weights (Family 12.4)
18. Kahn topo on layered DAG produces strict per-layer order
19. Kahn topo on transitive tournament (unique linearisation)
20. Kahn topo on diamond DAG
21. Floyd-Warshall APSP on small Kn vs golden output
22. Johnson APSP on sparse random with negatives (no neg cycle)
23. K-core decomposition on Barabási-Albert
24. Triangle count on Kn (== C(n,3))
25. Transitive closure on transitive tournament (== upper triangle filled)
26. WCC on disconnected forest equals component count

## Sprint 64 — Analytics + flow + matching (28 tasks)

1. Brandes betweenness on Star (centre dominates)
2. Brandes betweenness on bridge-connected double cluster (bridge dominates, Family 9.2)
3. Brandes betweenness on planted partition (boundary vertices)
4. PageRank on dangling-node web graph (sum normalisation)
5. PageRank on Barabási-Albert (hubs concentrate rank)
6. PageRank on hypercube (uniform)
7. Personalised PageRank with single-node teleport
8. Leiden on bridge-connected double cluster (must split, Family 9.2)
9. Leiden on ring of cliques (Family 9.3)
10. Leiden on planted partition (NMI vs ground truth ≥ 0.95)
11. Leiden on LFR parametric in μ (NMI vs ground truth)
12. Label propagation on bridge double cluster
13. Label propagation on planted partition
14. Dinic max-flow with zero-capacity edges (Family 10.1)
15. Dinic max-flow on worst-case (level-graph rebuild ≤ O(V), Family 10.4)
16. Edmonds-Karp on CLRS bad-case (Family 10.6)
17. Push-relabel on worst-case (Family 10.5)
18. Push-relabel handles anti-parallel edges (Family 10.3)
19. Stoer-Wagner global min-cut on weighted undirected
20. MCMF on bipartite assignment cross-checks Hungarian result (Family 10.8)
21. Hopcroft-Karp on imbalanced bipartite (matching size == min(m,n), Family 11.1)
22. Hopcroft-Karp on forced single augmenting path (Family 11.2)
23. Hopcroft-Karp on dense bipartite Kn,n (Family 11.3)
24. Hopcroft-Karp handles isolated vertices on one side
25. Hungarian on n×n dense bipartite with random costs
26. Hungarian on cost matrix with zero rows / zero columns
27. Cross-check: Dinic on bipartite reduction equals Hopcroft-Karp output (Family 10.7)
28. Property-based: Brandes betweenness sums to expected total on small random graphs

## Sprint 65 — External / Tier 2 (14 tasks)

1. csrfile.Writer: deterministic byte-stable output for the same input
2. csrfile.Reader: mmap + Reinterpret zero-copy round-trip
3. csrfile.Reader: 1e7-edge file load, page-boundary alignment (soak)
4. csrfile: deterministic fixture generator
5. semi-external BFS on DIMACS9 NY (correctness vs in-memory BFS)
6. semi-external BFS on DIMACS9 full 24M nodes (nightly)
7. semi-external BFS on LDBC SF10 (soak)
8. semi-external BFS on RMAT scale 26 (nightly)
9. semi-external PageRank on LDBC SF10 (soak)
10. semi-external PageRank on RMAT scale 24 (soak)
11. Tier 1 vs Tier 2 result equality on same DIMACS9 NY input
12. Concurrent semi-external readers: 32 goroutines on same csrfile (race)
13. csrfile under FS fault: ENOSPC at write time (internal/testfs)
14. csrfile under FS fault: truncate mid-write yields typed error on Open

## Sprint 66 — Persistence: WAL + snapshot + recovery (24 tasks)

1. WAL Frame Encode/Decode round-trip on every PropertyKind
2. WAL Frame round-trip on every op kind (AddNode/AddEdge/Remove*/Set*/Del*)
3. WAL torn-frame detection at every byte offset (extend existing)
4. WAL CRC32C corruption every byte (extend existing)
5. WAL crash-injection SIGKILL mid-fsync (internal/crashinject)
6. WAL crash-injection SIGKILL mid-frame-header
7. WAL crash-injection SIGKILL mid-payload
8. WAL group-commit correctness under N concurrent writers
9. snapshot v1 → v3 forward compatibility on existing fixtures
10. snapshot v3 self-sufficient via mapper.bin (reinforces 0c659b7)
11. snapshot atomic dir: partial write recovered or rejected cleanly
12. snapshot per-file CRC corruption rejected with typed error
13. snapshot manifest schema_version forward-rejected
14. recovery.Open round-trip on every typed graph (string/int64/float64/UUID)
15. recovery.OpenCtx honours pre-cancelled context
16. recovery.OpenWithCodec rejects v1 frames after v2 introduction
17. recovery.OpenWithOptions custom codec round-trip
18. recovery: torn tail drops last op, replays the rest
19. recovery: snapshot + WAL replay equals pre-shutdown in-memory state
20. recovery: empty dir opens to empty graph
21. recovery: unreadable WAL returns typed error
22. Property-based: snapshot + WAL = current state (rapid + crash-injection)
23. snapshot rotation under load: concurrent readers see consistent snapshot
24. recovery: indexes survive restart (label, hash, btree)

## Sprint 67 — Persistence: checkpoint + bulk + csrfile + FS-fault (22 tasks)

1. checkpoint background goroutine: WAL → snapshot transition correct
2. checkpoint: sustained writes 1k ops/s for 60 s, no goroutine leak (soak)
3. checkpoint cadence: every N ops or every T seconds (whichever first)
4. checkpoint: graceful shutdown drains pending WAL frames
5. checkpoint: forced checkpoint via API
6. checkpoint + crash injection: SIGKILL mid-checkpoint
7. bulk loader: 1M nodes + 5M edges throughput target (soak)
8. bulk loader bypasses WAL (verified by WAL frame count post-load)
9. bulk loader + subsequent recovery yields equal state
10. bulk loader: ctx-cancel mid-load returns partial result + typed error
11. csrfile alignment: 64-byte boundaries enforced
12. csrfile.Reinterpret zero-copy: same backing memory
13. csrfile version upgrade path documented and tested
14. FS fault: disk full ENOSPC during WAL append
15. FS fault: partial write at byte 128 of a frame
16. FS fault: fsync delay 500 ms (group-commit batching correctness)
17. FS fault: CRC corruption mid-snapshot file
18. FS fault: truncate mid-manifest yields typed error on Open
19. FS fault: read-only file system rejects writes cleanly
20. Sequencing: bulk → checkpoint → snapshot → recovery yields equal state
21. Sequencing: WAL write → checkpoint → WAL write → recovery preserves order
22. Sequencing: snapshot during active WAL writes (race-clean)

## Sprint 68 — Cross-process determinism (14 tasks)

1. Write proc A / read proc B: mapper FNV-1a yields identical NodeIDs
2. Write proc A / recover proc B: same graph state
3. Snapshot proc A / load proc B: byte-equal snapshot
4. csrfile proc A / proc B mmap reads identical neighbours
5. Plan-cache key stability: same query compiles to same plan in two processes
6. Bulk load proc A + recovery proc B: equal state
7. Cross-process: PropertyKey registry IDs stable on restart
8. Cross-process: LabelRegistry IDs stable on restart
9. Cross-process: rebuilt CSR is byte-equal (deterministic build order)
10. Crash + relaunch: SIGKILL mid-write, recovery in fresh process
11. Crash + relaunch under SOAK_FULL: 100 cycles of write/kill/recover (soak)
12. Cross-process Cypher: CREATE in A, MATCH in B sees the row
13. Cross-process Cypher: MERGE idempotence across two processes on same node
14. Cross-process: writes from N processes to disjoint shards either merge or are rejected (verify documented contract)

## Sprint 69 — Cypher: read paths (32 tasks)

1. scan_all on trivial shapes (E0, K1, K2)
2. scan_all on classic shapes (Pn, Cn, Star, Kn, Km,n)
3. scan_label on single-label universe (Family 15.1, fast path)
4. scan_label on label-cardinality explosion (Family 15.2)
5. scan_index_hash on string equality query
6. scan_index_hash on int64 equality query
7. scan_index_btree range query inclusive bounds
8. scan_index_btree range query exclusive bounds
9. expand (single hop) on Kn (fan-out N-1)
10. expand on Star centre
11. optional_expand on disconnected pattern (null bait, Family 19.3)
12. varlen_expand [1..k] on Pn correctness
13. varlen_expand [1..k] on Cn cycle handling (loopless)
14. varlen_expand on pattern-cycle trap (Family 19.2)
15. filter with property predicate
16. filter with NULL three-valued logic (n.age IS NULL semantics)
17. project on multiple expressions
18. sort by single key, ASC and DESC
19. sort with ties (deterministic on second key)
20. top (LIMIT after sort) on large input
21. limit 0 returns empty
22. distinct on duplicate property values
23. union of two MATCH branches
24. unwind on [1,2,3] yields 3 rows
25. unwind on list containing NULL elements
26. unwind ctx-cancel mid-iteration
27. semi_apply for EXISTS predicate
28. apply / correlated_apply for subquery
29. eager_aggregation: count/sum/avg/min/max + collect
30. eager_aggregation with percentileCont and percentileDisc
31. parallel operator: parallel scan with deterministic output
32. ORDER tie-breaking on aggregation result

## Sprint 70 — Cypher: write paths (22 tasks)

1. CREATE single node with labels and properties
2. CREATE relationship with type and properties
3. CREATE multi-pattern in one clause (regression for ed0063c)
4. MERGE creates when not present (idempotence)
5. MERGE matches when present (no-op)
6. MERGE race: concurrent same-key merges produce single node
7. SET property on existing node
8. SET multiple properties in one clause
9. SET twice equals SET once (idempotence)
10. REMOVE property
11. REMOVE label
12. DELETE single node (no relationships)
13. DELETE rejects node with relationships (typed error)
14. DETACH DELETE: hub centre with 1e6 leaves cascade (Family 19.7, soak)
15. DELETE relationship leaves endpoints intact
16. CREATE INDEX on label.property
17. DROP INDEX
18. CREATE CONSTRAINT UNIQUE on label.property
19. DROP CONSTRAINT
20. Write engine durability: CREATE + crash + recovery preserves state
21. Write + RETURN dispatch (regression for b3de2bf)
22. Property-based: CREATE → MATCH idempotent (rapid)

## Sprint 71 — Cypher: subqueries + procedures + plan-cache (14 tasks)

1. EXISTS subquery on simple pattern
2. COUNT subquery
3. COLLECT subquery
4. CALL { } IN TRANSACTIONS bounded by OF n ROWS
5. CALL IN TRANSACTIONS rolls back on error
6. procedure_call: db.labels()
7. procedure_call: db.relationshipTypes()
8. procedure_call: db.propertyKeys()
9. plan-cache: same query hits cache on second invocation
10. plan-cache: parameter-only change yields cache hit
11. plan-cache: 1e5 distinct queries triggers LRU eviction (Family 17.7)
12. plan-cache: race-clean under 16 concurrent compile calls
13. plan-cache: parameterised query and literal-only equivalent share plan
14. plan-cache: eviction order is least-recently-used

## Sprint 72 — Cypher: planner + EXPLAIN/PROFILE (12 tasks)

1. EXPLAIN returns plan without execution
2. PROFILE returns plan with dbhits per operator
3. dbhits accounting: scan_all dbhits == Order
4. dbhits accounting: scan_label dbhits == label cardinality
5. USING INDEX hint forces index usage
6. Planner picks scan_index_hash for equality predicate
7. Planner picks scan_index_btree for range predicate
8. Planner picks scan_label when label index exists
9. varlen plan correctness on long Pn
10. OPTIONAL MATCH null propagation in plan
11. Aggregation plan: eager vs streaming choice documented
12. Plan stability: same canonical query + shape yields identical plan twice

## Sprint 73 — Bolt: protocol surface (22 tasks)

1. PackStream round-trip: primitives (int, float, string, bool, null)
2. PackStream round-trip: list and map
3. PackStream round-trip: NodeValue
4. PackStream round-trip: RelationshipValue
5. PackStream round-trip: PathValue
6. PackStream round-trip: Date / Time / LocalTime / DateTime / Duration
7. PackStream: unknown tag returns typed error
8. Bolt v5 handshake selects latest mutual version
9. Bolt v4.4 fallback when client only speaks v4
10. State machine: Hello → Ready transition
11. State machine: Logon → Ready
12. State machine: Run → Streaming → Ready
13. State machine: Reset drains streaming
14. State machine: illegal transition returns typed FailureCode
15. Auth: basic auth success and failure
16. Auth: scheme-unknown returns typed FailureCode
17. TLS: cert hot-reload on file change
18. MaxConnections enforced (typed close on overflow)
19. Cumulative byte cap: oversize message rejected, connection drops
20. InFlightCursorCap enforced (typed error on second run-in-tx)
21. Bookmarks: NextBookmark monotonically increasing
22. Routing table response includes server URL

## Sprint 74 — Bolt: end-to-end via neo4j-go-driver (26 tasks)

1. Add neo4j-go-driver as test-only dep (go.mod + go.sum) — CHORE
2. e2e: CREATE single node via driver session
3. e2e: MATCH and RETURN via driver
4. e2e: RETURN node shape exposes Labels, Properties, ElementId
5. e2e: RETURN relationship shape (closes backlog #504)
6. e2e: RETURN path shape (list of nodes and relationships)
7. e2e: CREATE relationship with properties
8. e2e: MERGE idempotence
9. e2e: DELETE and DETACH DELETE
10. e2e: SET / REMOVE properties
11. e2e: Explicit transaction begin/commit
12. e2e: Explicit transaction rollback
13. e2e: Autocommit transaction
14. e2e: Streaming PULL on 100k-row result
15. e2e: Paginated PULL with explicit size
16. e2e: DISCARD remaining rows
17. e2e: Bookmarks ordering across sessions
18. e2e: RoutingTable advertised by server
19. e2e: Failure mapping: timeout → Neo.ClientError.*
20. e2e: Failure mapping: statement cancel → Neo.ClientError.*
21. e2e: Illegal state transition surfaces correct FailureCode
22. e2e: ctx-cancel mid-streaming
23. e2e: 100 concurrent sessions on single server (soak)
24. e2e: shape categories Sec.1, Sec.2, Sec.6 round-trip CREATE / MATCH
25. e2e: TLS round-trip with self-signed cert
26. e2e: 10k queries in single connection (pipelining, soak)

## Sprint 75 — Concurrency stress & soak (16 tasks)

1. Hot-shard write storm: 64 goroutines targeting one shard
2. Reader-writer overlap on hub: 100 readers + 16 writers
3. Mapper shard-0 storm: 1e6 preimage keys (soak)
4. Goleak teardown on every spawned goroutine in cypher/exec
5. Goleak teardown on every spawned goroutine in bolt/server
6. ctx-cancel mid-Dijkstra on 1M-node graph
7. ctx-cancel mid-BFS on 10M-node chain
8. ctx-cancel mid-Brandes betweenness
9. ctx-cancel mid-Leiden iteration
10. Plan-cache thrash: 1e5 distinct queries under -race
11. SOAK_FULL Cypher RW 30 m extension (analytics reads added)
12. SOAK_FULL Bolt 4 h with 1024 connections + Cypher RW mixed
13. Soak: heap / FD / goroutine growth zero after warm-up
14. Soak: latency p99 stable across 4 h window
15. Soak: GC pause stable (no monotonic growth)
16. Soak: pprof CPU + heap profiles captured every 30 m as artefacts

## Sprint 76 — End-to-end production scenarios (14 tasks)

1. LDBC SNB IC1 (Friends-of-Friends) via Bolt e2e on SF1
2. LDBC SNB IC2 via Bolt e2e
3. LDBC SNB IC3 via Bolt e2e
4. LDBC SNB IC4–IC14 batch via Bolt e2e (soak)
5. DIMACS9 NY routing via public Go API (Dijkstra + A*)
6. DIMACS9 NY routing via Bolt e2e (Cypher distance pattern)
7. Social CLI cross-process extended scenario (extend example 24)
8. Build dependency: topological sort + Tarjan SCC on Linux make-dep DAG
9. Network reliability: Dinic max-flow on internet AS graph
10. Fraud detection: k-hop neighbours + triangle count + Leiden communities
11. Recommendation: personalised PageRank on social SF1
12. Knowledge-graph traversal: varlen MATCH on Wikidata sub-dump (nightly)
13. Streaming ingest: bulk loader + concurrent reads producing live snapshot
14. Performance dashboard: aggregate results from #58–#75 into docs/benchmarks/test-battery-summary.md — CHORE
