# Prior art in the Go module ecosystem

**Date:** 2026-10-08 · **Scope:** Go modules whose code GoGraph may copy, whole or in part, or study.

## 1. Purpose

CLAUDE.md, section *Copying open-source code*, allows code from any Go module with a
licence compatible with GoGraph's MIT licence to be copied whole or in part when it
benefits GoGraph or one of its features. This document is the catalogue of those modules.
It records, per GoGraph feature, which modules contain code GoGraph could reuse, what
licence governs that code, and what the concrete opportunity is. It is a source of
evolution opportunities, not a decision: adopting any entry still requires the
measurement, testing, and attribution that CLAUDE.md requires.

Graph modules come first (LPG databases, Cypher engines, Bolt, graph algorithms); the
remaining sections follow GoGraph's components.

## 2. Method

1. **Feature map.** GoGraph's components were taken from `go list ./...`, `go.mod`,
   README.md, CLAUDE.md *Intended Architecture*, and the design documents in `docs/`
   (see section 3).
2. **Discovery.** GitHub repository search restricted to Go (`gh search repos
   --language=go`) for: graph database, property graph, cypher, opencypher, GQL, bolt
   protocol, packstream, graph library, graph algorithms, hnsw, write ahead log,
   deterministic simulation testing; plus modules known as the standard Go solutions
   for each GoGraph feature.
3. **Licence verification.** For every listed module, the licence was read from the
   repository's licence file at its default-branch HEAD on 2026-10-08, through the
   GitHub API (`repos/{repo}/license`, which returns the file and its SPDX
   classification). Every file GitHub classified `NOASSERTION` or that was missing was
   read directly and classified by hand.
4. **Content verification.** For each module, the files or directories named in the
   *Useful content* column were confirmed to exist at the checked commit. The evidence
   level is recorded next to the commit:
   - **R** — the code was cloned (`git clone --depth 1`) and the relevant files read;
   - **L** — the named files or directories were listed at the checked commit;
   - **M** — metadata only: licence, activity, and repository description. The
     *Useful content* of an **M** row is the module's documented purpose and is not
     verified in code.
5. **Maturity.** GitHub stars, date of last push, and the archived flag, as returned by
   the GitHub API on 2026-10-08.

### Licence verdict rules (against GoGraph's MIT)

| Verdict | Licences | Obligation when copying |
|---|---|---|
| **compatible** | MIT, MIT-0, BSD-2-Clause, BSD-3-Clause, ISC, Apache-2.0, Unlicense, 0BSD, CC0-1.0, zlib | Keep the copyright and licence notice in the copied file or in `THIRD_PARTY_NOTICES`. For Apache-2.0, also carry any `NOTICE` file content and mark modified files. |
| **conditional** | MPL-2.0; any licence with an unusual term (explained in the row) | MPL-2.0 is file-level copyleft: a copied file stays MPL-2.0 and its source must stay available. Mixing MPL code into an MIT file is not allowed; keep it in separate files. |
| **incompatible** | GPL, LGPL, AGPL, BSL, SSPL, Elastic, Commons Clause, proprietary or source-available terms, no licence | Code is never copied. The module is listed as **insight-only**. |

### Column legend

- **Checked at** — default-branch HEAD commit (12 hex digits) on 2026-10-08, followed by
  the evidence level (R, L, M).
- **Maturity** — `★ stars · last push · status`. *archived* means read-only upstream.
- **Target** — the GoGraph package or file the code would feed.

### Limits of this survey

- Licence classification is by the root licence file. Large repositories (dgraph, tidb,
  vitess, milvus, weaviate, prometheus, etcd, arrow-go) may contain individual
  sub-directories under another licence; verify the file header before copying from
  them. Weaviate is known to do so (see its row).
- Performance claims made by upstream projects are not GoGraph evidence. Each top
  opportunity names the benchmark or test that must decide it.
- Stars are a weak maturity signal; the `google/cel-go` API response returned 2 stars,
  which is inconsistent with that project's known adoption and was not explained.

## 3. GoGraph feature map

| Feature | GoGraph packages |
|---|---|
| LPG core: nodes, edges, labels, properties, tokens, sessions | `graph`, `graph/lpg`, `graph/lpg/schema`, `graph/adjlist`, `graph/query` |
| MVCC: commit log, horizon, conflict detection, vacuum | `graph/mvcc`, `graph/lpg/mvcc_*.go` |
| CSR and adjacency | `graph/csr` (CSR, intersection, ordering), `graph/adjlist`, `graph/generation` |
| Indexes and statistics | `graph/index` (manager, nodeset), `graph/index/btree` (B+ tree), `graph/index/hash`, `graph/index/label` (roaring label bitmaps), `graph/index/count`, `graph/index/stats` (histogram, HLL, MCV) |
| Search and algorithms | `search` (BFS, direction-optimising BFS, bidirectional BFS, DFS, Dijkstra, bidirectional Dijkstra, A\*, Bellman-Ford, Johnson, Floyd-Warshall, Yen and Eppstein k-shortest, Tarjan SCC, WCC, BCC, bridges, topological sort, k-core, Kruskal, Prim, Hierholzer, Hopcroft-Karp, Hungarian, triangles, transitive closure, diameter), `search/centrality` (Brandes, closeness, harmonic, eigenvector, Katz, PageRank, personalised PageRank push), `search/community` (label propagation, Leiden), `search/flow` (Dinic, Edmonds-Karp, push-relabel, min-cost flow, Stoer-Wagner), `search/extern`, `ds` (union-find) |
| Cypher | `cypher/parser` (ANTLR 4 generated), `cypher/ast`, `cypher/sema`, `cypher/ir`, planner and `cypher/exec` (operators, hash join, columnar chunks, parallel scan/aggregate, shortest path, var-length expand), `cypher/expr`, `cypher/funcs` (temporal, math, list, aggregators), `cypher/procs`, `cypher/explain`, `cypher/tck` |
| Bolt | `bolt/packstream`, `bolt/proto`, `bolt/server` (sessions, TLS reload, routing, bookmarks, tx registry, quotas) |
| Store | `store` (`store.DB`), `store/wal`, `store/checkpoint`, `store/snapshot`, `store/recovery`, `store/txn`, `store/bulk`, `store/bulkimport`, `store/csrfile` |
| Graph I/O | `graph/io/csv`, `graph/io/dot`, `graph/io/graphml`, `graph/io/jsonl`, `cmd/gograph-import` |
| Concurrency, memory | `internal/memlimit`, `internal/clock`, `internal/synclatency`, sharded and atomic structures across `graph/lpg` |
| Observability | `metrics`, `internal/metrics`, `internal/metrics/prometheus` |
| Testing | `internal/sim` (DST simulator), `internal/isolationtest`, `internal/crashinject`, `internal/crashpoint`, `internal/testfs`, `internal/shapegen`, `pgregory.net/rapid`, `go.uber.org/goleak`, `cucumber/godog` (TCK) |

Not present in GoGraph at the checked HEAD (searched by name in `graph/`, `cypher/`,
`search/`): vector (ANN) index, full-text index, graph colouring, clique enumeration,
elementary-cycle enumeration, dominator trees, HITS, node-similarity algorithms, node
embeddings, compression of CSR neighbour lists in `store/csrfile`, a linearizability
checker, and Cypher fuzzing beyond the parser recovery and PackStream fuzz tests.

Current module dependencies (`go.mod`): `RoaringBitmap/roaring/v2 v2.26.0`,
`antlr4-go/antlr/v4 v4.13.1`, `cucumber/godog v0.16.0`, `edsrzf/mmap-go v1.2.0`,
`klauspost/compress v1.19.2`, `neo4j/neo4j-go-driver/v5 v5.28.4` (tests and benchmarks),
`go.uber.org/goleak v1.3.0`, `golang.org/x/sys v0.47.0`, `pgregory.net/rapid v1.3.0`.

---

## 4. LPG and graph databases written in Go

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/orneryd/NornicDB` | <https://github.com/orneryd/NornicDB> | MIT (+ defensive patent non-assertion in `PATENTS.md`) → **compatible** | `15644e54cf20` (R) | ★897 · 2026-10-08 · active | Go graph+vector DB with Neo4j compatibility: `pkg/bolt` (Bolt server, PackStream, WebSocket transport), `pkg/cypher` (247 files, includes an ANTLR variant and a hand-written parser, duration functions, APOC procedures), `pkg/storage` (Badger-backed MVCC, WAL segments and repair), `pkg/search` (HNSW, IVF-PQ, BM25 full-text, rerank), `pkg/simd` (amd64/arm64/NEON kernels), `apoc/` | `bolt/server`, `cypher/funcs`, `cypher/procs`, new vector and full-text indexes | Closest Go peer to GoGraph; mine APOC functions, full-text and vector index code, Bolt WebSocket transport. The patent clause is additive to MIT and terminates only the patent grant for a party that sues. |
| `github.com/dgraph-io/dgraph` | <https://github.com/dgraph-io/dgraph> | Apache-2.0 → **compatible** | `06f47597a23a` (L) | ★21803 · 2026-10-08 · active | `posting/` (posting lists, MVCC rollups, `oracle.go`), `algo/` (sorted UID-list intersection, `packed.go` packed UID lists, `cm-sketch.go`) | `graph/csr/intersect.go`, `graph/mvcc`, `graph/index/stats` | Compare galloping/packed UID intersection with `csr/intersect.go`; reuse packed sorted-ID encoding for adjacency on disk. |
| `github.com/cayleygraph/cayley` | <https://github.com/cayleygraph/cayley> | Apache-2.0 → **compatible** | `81dcd7d73e45` (L) | ★15067 · 2026-08-27 · active | Quad store with iterator algebra (`graph/iterator`), `hasa.go`/`linksto.go` iterator optimisation, KV and SQL back-ends | `cypher/exec` | Study iterator re-ordering and cost hints; low direct reuse (RDF/quad model). |
| `github.com/cayleygraph/quad` | <https://github.com/cayleygraph/quad> | Apache-2.0 → **compatible** | `a9b1aedeecc3` (M) | ★33 · 2024-07-06 | N-Quads and RDF format readers/writers | `graph/io` | Optional RDF import/export. |
| `github.com/liliang-cn/cortexdb` | <https://github.com/liliang-cn/cortexdb> | MIT → **compatible** | `4247495821b4` (R) | ★274 · 2026-10-08 · active | `pkg/graph` (Cypher subset with TCK test file, Leiden, personalised PageRank, HNSW top-k, temporal Allen-interval reasoning, RDF/SPARQL/SHACL), `pkg/index` (flat, HNSW, IVF, LSH, binary quantisation) | `search/community`, vector index, `graph/io` | Second Go source for HNSW/IVF/LSH and Leiden cross-checks. |
| `github.com/cloudprivacylabs/lpg` | <https://github.com/cloudprivacylabs/lpg> | Apache-2.0 → **compatible** | `f5a57d3aac18` (L) | ★9 · 2023-10-02 · inactive | In-memory LPG with B-tree and hash indexes, pattern matching, plan processors | `graph/lpg` | Insight into a minimal LPG API; little to copy. |
| `github.com/vescale/zgraph` | <https://github.com/vescale/zgraph> | Apache-2.0 → **compatible** | `959c02d50f95` (R) | ★75 · 2023-04-16 · inactive | Embeddable graph DB with a goyacc parser (`parser/parser.y`), planner, executor, and a Percolator-style storage layer (`storage/memdb_arena.go`, `storage/mvcc`, `latch`, `resolver`) | `graph/mvcc`, `store/txn` | Arena memdb and latch scheduler are compact, readable MVCC references. |
| `github.com/graphikDB/graphik` | <https://github.com/graphikDB/graphik> | Apache-2.0 → **compatible** | `6b3defb7e26f` (M) | ★322 · 2022-01-31 · inactive | gRPC graph DB on bbolt with CEL expressions | — | Low value. |
| `github.com/tamnd/gr` | <https://github.com/tamnd/gr> | Apache-2.0 → **compatible** | `dc2bc694546b` (M) | ★1 · 2026-06-29 | Single-file embedded LPG database | — | Watch only. |
| `github.com/justintout/go-sqlite-graph` | <https://github.com/justintout/go-sqlite-graph> | BSD-3-Clause → **compatible** | `65ce6cae4a93` (M) | ★1 · 2026-09-14 | LPG on SQLite | — | Watch only. |
| `github.com/giovibal/intreccio` | <https://github.com/giovibal/intreccio> | MIT → **compatible** | `ef02a5deac0e` (M) | ★0 · 2026-09-14 | Small graph DB with openCypher support | — | Watch only. |
| `github.com/svlocks/sheets` | <https://github.com/svlocks/sheets> | MIT → **compatible** | `769c42162ffe` (M) | ★0 · 2026-09-01 | Temporal property-graph storage with Cypher reads | — | Watch only. |
| `github.com/kuzudb/go-kuzu` | <https://github.com/kuzudb/go-kuzu> | MIT → **compatible** | `3950bb8051f9` (M) | ★43 · 2025-10-10 · archived | cgo binding to Kùzu (C++) | — | No Go engine code; Kùzu itself is C++. |
| `github.com/LadybugDB/go-ladybug` | <https://github.com/LadybugDB/go-ladybug> | MIT → **compatible** | `42bbf464c74c` (M) | ★25 · 2026-08-04 | cgo binding (Kùzu fork) | — | No Go engine code. |
| `github.com/mstrYoda/goraphdb` | <https://github.com/mstrYoda/goraphdb> | no licence file → **incompatible (insight-only)** | `01eff165f35c` (L) | ★108 · 2026-06-26 | Cypher lexer/parser/executor, WAL, bloom, sharding | — | Insight only. |
| `github.com/SamuelSupe/graphdb` | <https://github.com/SamuelSupe/graphdb> | "All rights reserved, no licence granted" → **incompatible (insight-only)** | `1c6466bfcf91` (M) | ★4 · 2026-10-04 | Multi-tenant property-graph DB | — | Insight only. |

## 5. Cypher, openCypher, GQL and PGQL in Go

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/CelineWuest/dinkel` | <https://github.com/CelineWuest/dinkel> | MIT → **compatible** | `fd247df448ed` (L) | ★4 · 2025-04-15 | Cypher fuzzer (state-aware AST generation, query reduction, bug-report replay); targets Neo4j, FalkorDB, Apache AGE, Memgraph (`targets-config.yml`, `translator/`, `dbms/`) | `cypher` testing, `internal/sim` | Add a GoGraph target and run differential fuzzing against Neo4j. |
| `github.com/cloudprivacylabs/opencypher` | <https://github.com/cloudprivacylabs/opencypher> | Apache-2.0 → **compatible** | `b4c389456819` (R) | ★17 · 2024-01-21 · inactive | ANTLR `Cypher.g4`, evaluator, `datetime.go`, `duration.go`, `datefuncs.go`, string and scalar functions | `cypher/funcs` | Cross-check temporal and string function edge cases. |
| `github.com/AvitalTamir/cyphernetes` | <https://github.com/AvitalTamir/cyphernetes> | Apache-2.0 → **compatible** | `1feece81dc81` (M) | ★1221 · 2026-07-03 | Hand-written Cypher-like lexer/parser for Kubernetes | — | Low value: dialect, not openCypher. |
| `github.com/seuros/gopher-cypher` | <https://github.com/seuros/gopher-cypher> | MIT → **compatible** | `2e6c020b1e23` (M) | ★2 · 2025-12-13 | Cypher tooling | — | Watch only. |
| `github.com/a-poor/cypher` | <https://github.com/a-poor/cypher> | MIT → **compatible** | `268f818ef003` (M) | ★2 · 2022-07-15 | Cypher parser | — | Low value. |
| `github.com/rafaelcaricio/cypher-parser` | <https://github.com/rafaelcaricio/cypher-parser> | MIT → **compatible** | `6d50254444c8` (M) | ★10 · 2019-01-03 · archived | Cypher parser | — | Low value. |
| `github.com/leiysky/parser` | <https://github.com/leiysky/parser> | Apache-2.0 → **compatible** | `788039f96951` (M) | ★0 · 2019-10-03 | goyacc openCypher parser | `cypher/parser` | Data point for a hand-written/yacc parser vs ANTLR. |
| `github.com/itergia/pgql-go` | <https://github.com/itergia/pgql-go> | Apache-2.0 → **compatible** | `abe9691e62ae` (M) | ★4 · 2022-12-10 | PGQL skeleton | — | Low value. |
| `github.com/jtejido/go-opencypher` | <https://github.com/jtejido/go-opencypher> | no licence file → **incompatible (insight-only)** | `1b3d3838bda1` (L) | ★5 · 2021-03-16 | ANTLR-generated openCypher 9 parser | — | Insight only. |
| `github.com/antlr4-go/antlr` (dependency) | <https://github.com/antlr4-go/antlr> | BSD-3-Clause → **compatible** | `4d7e18847d88` (M) | ★151 · 2024-06-28 | ANTLR 4 Go runtime | `cypher/parser` | Vendoring and trimming the runtime (prediction cache, ATN simulator) is allowed if parsing cost justifies it. |
| `github.com/alecthomas/participle` | <https://github.com/alecthomas/participle> | MIT → **compatible** | `6a9cbdc28572` (M) | ★3893 · 2026-10-08 · active | Struct-tag parser generator with lexer | — | Not a fit for a full openCypher grammar; listed for completeness. |

## 6. Bolt and PackStream

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/neo4j/neo4j-go-driver/v6` (v5 is a test dependency) | <https://github.com/neo4j/neo4j-go-driver> | Apache-2.0 → **compatible** | `69f4017c3e80` (R) | ★556 · 2026-10-05 · active | `neo4j/internal/packstream` (`packer.go`, `unpacker.go`), `neo4j/internal/bolt` (Bolt 3–5 client state machines, hydration) | `bolt/packstream`, `bolt/server` | Reference encoder/decoder for differential PackStream tests; client-side message hydration as a conformance oracle. |
| `github.com/orneryd/NornicDB/pkg/bolt` | see section 4 | MIT → **compatible** | `15644e54cf20` (R) | see section 4 | Bolt server, PackStream, WebSocket transport, transaction lifecycle, metrics | `bolt/server` | Bolt-over-WebSocket transport (browser clients) is absent in GoGraph. |
| `github.com/memgraph/bolt-proxy` | <https://github.com/memgraph/bolt-proxy> | Apache-2.0 → **compatible** | `64150a426721` (M) | ★27 · 2023-03-03 | Bolt message parsing and proxying | `bolt/proto` | Low value. |
| `github.com/johnnadratowski/golang-neo4j-bolt-driver` | <https://github.com/johnnadratowski/golang-neo4j-bolt-driver> | MIT → **compatible** | `807201386efa` (M) | ★209 · 2021-04-08 · inactive | Bolt v1 driver, PackStream | — | Superseded by the official driver. |
| `github.com/mindstand/go-bolt` | <https://github.com/mindstand/go-bolt> | MIT → **compatible** | `e9d73b13d98d` (M) | ★5 · 2020-05-24 · archived | Bolt driver | — | Low value. |
| `github.com/bost-h/go-packstream` | <https://github.com/bost-h/go-packstream> | MIT → **compatible** | `44e1ba90aed1` (M) | ★1 · 2016-08-03 | PackStream codec | — | Low value. |

## 7. Graph algorithm libraries

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `gonum.org/v1/gonum/graph` | <https://github.com/gonum/gonum> | BSD-3-Clause → **compatible** | `0d48ceec2250` (R) | ★8438 · 2026-10-06 · active | `topo` (Bron–Kerbosch cliques, Johnson and Paton cycles, transitive reduction, Tarjan, 2-SAT), `coloring` (DSatur, Welsh–Powell, randomised), `network` (Brandes, PageRank, HITS, diffusion, distance), `community` (Louvain incl. multiplex, Leiden, k-communities, bisection), `flow` (Lengauer–Tarjan dominators), `path` (A\*, Bellman-Ford, Dijkstra, Floyd-Warshall, Johnson, Yen, D\* Lite in `path/dynamic`), `spectral`, `layout`, `graphs/gen` (Gnp, Gnm, small-world, Holme–Kim, Barabási–Albert), `encoding` (DOT, graph6, digraph6, GraphQL), `formats` (GEXF, Cytoscape.js, Sigma.js, RDF) | `search/*`, `graph/io`, `internal/shapegen` | Largest compatible algorithm source: colouring, cliques, cycles, dominators, HITS, Louvain fill GoGraph gaps; its test fixtures serve as cross-check oracles. |
| `github.com/yourbasic/graph` | <https://github.com/yourbasic/graph> | BSD-2-Clause → **compatible** | `8ecfec1c2869` (L) | ★752 · 2023-05-11 · inactive | Compact immutable graph, BFS, bipartite check, Euler, max-flow, MST, SCC, topo, WCC | `search` | Cross-check oracle only; GoGraph already covers these. |
| `github.com/dominikbraun/graph` | <https://github.com/dominikbraun/graph> | Apache-2.0 → **compatible** | `b8919a8021b1` (L) | ★2230 · 2024-12-11 | Generic graph with pluggable store, DAG helpers, traversal | — | API ergonomics reference; no performance value. |
| `github.com/hmdsefi/gograph` | <https://github.com/hmdsefi/gograph> | Apache-2.0 → **compatible** | `f7b5ca02c0d7` (L) | ★133 · 2026-10-08 · active | Generic graph, `connectivity`, `partition`, `path`, `encoding` | `search` | Cross-check oracle. |
| `github.com/soniakeys/graph` | <https://github.com/soniakeys/graph> | MIT declared in `readme.adoc`, no licence file → **compatible** (notice must be reconstructed from the README declaration) | `7b5d1f6e4fe0` (L) | ★72 · 2020-04-15 · inactive | Adjacency-list algorithms (MST, SSSP, random generators) | `search` | Low value. |
| `github.com/autom8ter/dagger` | <https://github.com/autom8ter/dagger> | Apache-2.0 → **compatible** | `48451bb2aea1` (M) | ★327 · 2023-07-07 | Concurrency-safe in-memory directed graph | — | Low value. |
| `github.com/gyuho/goraph` | <https://github.com/gyuho/goraph> | MIT → **compatible** | `ad625acf7ae3` (M) | ★749 · 2022-04-10 | Classic algorithms | — | Low value. |
| `github.com/twmb/algoimpl` | <https://github.com/twmb/algoimpl> | BSD-2-Clause → **compatible** | `076353e90b94` (M) | ★54 · 2017-07-17 | Graph algorithms | — | Low value. |
| `github.com/thcyron/graphs` | <https://github.com/thcyron/graphs> | MIT → **compatible** | `56ee3b256ff9` (M) | ★60 · 2021-12-24 | Graph algorithms | — | Low value. |
| `github.com/intelligrit/graphwizard` | <https://github.com/intelligrit/graphwizard> | MIT → **compatible** | `4f4dbdf01c3c` (M) | ★1 · 2026-08-20 | 40+ algorithms, gonum-compatible | — | Watch only. |
| `github.com/LuisLSousa/gonx` | <https://github.com/LuisLSousa/gonx> | MIT → **compatible** | `273181261671` (M) | ★11 · 2026-10-02 | NetworkX-style library | — | Watch only. |
| `github.com/mrl00/zxcq-gograph` | <https://github.com/mrl00/zxcq-gograph> | MIT (read by hand; GitHub reported NOASSERTION) → **compatible** | `d00e7ef4561b` (M) | ★1 · 2026-09-14 | Port of Graphs.jl | — | Watch only. |
| `github.com/katalvlaran/lvlath` | <https://github.com/katalvlaran/lvlath> | AGPL-3.0 → **incompatible (insight-only)** | `f3c13b6823e8` (M) | ★3 · 2026-07-21 | Graph algorithms | — | Insight only. |
| `github.com/ScottSallinen/lollipop` | <https://github.com/ScottSallinen/lollipop> | LGPL-2.1 (search result) → **incompatible (insight-only)** | not pinned (M) | ★5 · 2025-03-20 | Testing framework for streaming graph algorithms | — | Insight only. |

## 8. Graph formats and I/O

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `gonum.org/v1/gonum/graph/encoding`, `/formats` | see section 7 | BSD-3-Clause → **compatible** | `0d48ceec2250` (R) | see section 7 | DOT parser (`formats/dot` with lexer, parser, fuzz corpus), graph6/digraph6, GEXF 1.2, Cytoscape.js, Sigma.js | `graph/io/dot`, new `graph/io` formats | DOT reader with an existing fuzz corpus; graph6 for compact test fixtures. |
| `github.com/awalterschulze/gographviz` | <https://github.com/awalterschulze/gographviz> | Apache-2.0 (read by hand; GitHub reported NOASSERTION) → **compatible** | `1aeb6b15b39e` (M) | ★565 · 2023-02-28 | DOT parser and writer | `graph/io/dot` | Alternative to gonum's DOT parser. |
| `github.com/goccy/go-graphviz` | <https://github.com/goccy/go-graphviz> | MIT → **compatible** | `76e04975df88` (M) | ★833 · 2025-11-29 | Graphviz compiled to WebAssembly | — | Rendering only; out of scope. |
| `github.com/yaricom/goGraphML` | <https://github.com/yaricom/goGraphML> | MIT → **compatible** | `ba9a22b8130a` (M) | ★15 · 2024-05-15 | GraphML reader/writer | `graph/io/graphml` | Cross-check GraphML attribute typing. |
| `github.com/jszwec/csvutil` | <https://github.com/jszwec/csvutil> | MIT → **compatible** | `b9b849659044` (M) | ★1037 · 2025-03-15 | Fast CSV-to-struct decoding | `graph/io/csv`, `store/bulkimport` | Low value: GoGraph's CSV is column-typed, not struct-bound. |

## 9. Storage engines and key-value stores

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/cockroachdb/pebble` | <https://github.com/cockroachdb/pebble> | BSD-3-Clause → **compatible** | `e0818dd07ec5` (L) | ★6050 · 2026-10-08 · active | `vfs` (FS abstraction, `disk_health.go`, `disk_full.go`, `atomicfs`), `vfs/errorfs` (rule-based I/O fault injection DSL, injected latency), `internal/arenaskl` (arena skiplist, own Apache-2.0 LICENSE), record/WAL format, block cache | `internal/testfs`, `internal/crashinject`, `store/wal` | errorfs DSL for deterministic I/O fault injection; disk-health monitor for stalled fsync detection. |
| `go.etcd.io/bbolt` | <https://github.com/etcd-io/bbolt> | MIT → **compatible** | `4dc08f7187c7` (L) | ★9768 · 2026-09-15 · active | Copy-on-write B+tree with mmap, freelist, meta-page double buffering | `store/snapshot`, `store/csrfile` | Reference for meta-page alternation and torn-write detection. |
| `github.com/dgraph-io/badger/v4` | <https://github.com/dgraph-io/badger> | Apache-2.0 → **compatible** | `78b99b8b0f01` (R) | ★15782 · 2026-10-05 · active | `txn.go` oracle: SSI-style conflict detection by 64-bit key fingerprints (`conflictKeys`, `hasConflict`), watermark (`y/watermark.go`), value log, skiplist | `graph/mvcc/conflict.go` | Fingerprint read-set conflict check as a compact alternative or cross-check. |
| `github.com/syndtr/goleveldb` | <https://github.com/syndtr/goleveldb> | BSD-2-Clause → **compatible** | `126854af5e6d` (M) | ★6317 · 2024-05-14 | LevelDB port: journal, memdb skiplist | — | Superseded by Pebble as a reference. |
| `github.com/tidwall/buntdb` | <https://github.com/tidwall/buntdb> | MIT → **compatible** | `0dbc8c18459a` (M) | ★4868 · 2026-05-19 | In-memory KV with AOF log and custom indexes | — | Low value. |
| `github.com/nutsdb/nutsdb` | <https://github.com/nutsdb/nutsdb> | Apache-2.0 → **compatible** | `a74d70d521f7` (M) | ★3580 · 2026-10-03 | Bitcask-style KV with data structures | — | Low value. |
| `github.com/rosedblabs/rosedb` | <https://github.com/rosedblabs/rosedb> | Apache-2.0 → **compatible** | `bcb43052ada6` (M) | ★4886 · 2026-02-10 | Bitcask KV over `rosedblabs/wal` | — | Low value. |
| `github.com/akrylysov/pogreb` | <https://github.com/akrylysov/pogreb> | Apache-2.0 → **compatible** | `b86080d06267` (M) | ★1350 · 2026-04-06 | Read-optimised embedded KV, linear hashing | — | Low value. |
| `github.com/dolthub/dolt` | <https://github.com/dolthub/dolt> | Apache-2.0 → **compatible** | `6b3c5bee956d` (L) | ★24600 · 2026-10-08 · active | `go/store/prolly` (prolly trees: content-addressed, structurally shared ordered maps) | `graph/index/btree` | Insight for versioned indexes; prolly trees trade write cost for diffability, which GoGraph does not need today. |
| `github.com/thomasjungblut/go-sstables` | <https://github.com/thomasjungblut/go-sstables> | Apache-2.0 → **compatible** | `8044d6a7ecc9` (M) | ★368 · 2026-09-30 | SSTables, skiplist, recordio, WAL | — | Low value. |
| `github.com/chaisql/chai` | <https://github.com/chaisql/chai> | MIT → **compatible** | `e53516f8185c` (M) | ★1704 · 2026-01-16 | Embedded SQL DB on Pebble | — | Low value. |
| `github.com/polarsignals/frostdb` | <https://github.com/polarsignals/frostdb> | Apache-2.0 → **compatible** | `9e5cfe0171ad` (M) | ★1548 · 2026-10-07 | Embedded columnar DB on Arrow/Parquet with WAL and DST-style tests | `graph/lpg/edge_property_column.go` | Insight for columnar property storage. |
| `github.com/prologic/bitcask` | <https://github.com/prologic/bitcask> | GitHub repository holds only a README; no licence file → **incompatible (insight-only)** | `64ef13a20f13` (L) | ★33 · 2023-11-04 | Moved off GitHub | — | Rejected (no code at this location). |

## 10. Write-ahead logs

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `go.etcd.io/etcd/server/v3/storage/wal` | <https://github.com/etcd-io/etcd> | Apache-2.0 → **compatible** | `f061acd0902d` (L) | ★52343 · 2026-10-08 · active | `wal.go`, `encoder.go`/`decoder.go` (CRC-chained records), `file_pipeline.go` (pre-allocated next segment), `repair.go` (torn-tail repair) | `store/wal` | Pre-allocating the next segment off the commit path; compare repair rules with `store/recovery`. |
| `github.com/prometheus/prometheus/tsdb/wlog` | <https://github.com/prometheus/prometheus> | Apache-2.0 → **compatible** | `d4467eede8e6` (L) | ★66426 · 2026-10-08 · active | Page-aligned segmented WAL (`wlog.go`), `live_reader.go`, `checkpoint.go`, `watcher.go` | `store/wal`, `store/checkpoint` | Page-fragmented record layout and live-tail reader as references. |
| `github.com/tidwall/wal` | <https://github.com/tidwall/wal> | MIT → **compatible** | `4b09f9519cba` (L) | ★733 · 2025-08-31 | Single-file segmented log with batch writes | — | Low value: no torn-write semantics beyond truncation. |
| `github.com/rosedblabs/wal` | <https://github.com/rosedblabs/wal> | Apache-2.0 → **compatible** | `9f1a61878495` (L) | ★288 · 2025-01-26 | Block-based segment WAL (`segment.go`) with chunk types | — | Low value. |
| `github.com/hashicorp/raft-wal` | <https://github.com/hashicorp/raft-wal> | MPL-2.0 → **conditional** | `ebffec3619e9` (M) | ★119 · 2026-09-16 | Segmented WAL with lock-free reads of sealed segments | `store/wal` | Study only unless kept in separate MPL files. |

## 11. Ordered indexes: B-trees, radix trees, skiplists

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/tidwall/btree` | <https://github.com/tidwall/btree> | MIT → **compatible** | `142fe5941ebb` (L) | ★1231 · 2026-09-29 · active | Generic B-tree (`btreeg.go`) with `PathHint` for clustered keys and copy-on-write copies (per-node `isoid`) | `graph/index/btree` | Benchmark path hints and lazy COW against GoGraph's B+ tree. |
| `github.com/google/btree` | <https://github.com/google/btree> | Apache-2.0 → **compatible** | `aeba20f7a1e1` (L) | ★4163 · 2024-08-21 · archived | Generic B-tree with COW clone and freelist | `graph/index/btree` | Superseded by tidwall/btree as a reference. |
| `github.com/plar/go-adaptive-radix-tree/v2` | <https://github.com/plar/go-adaptive-radix-tree> | MIT → **compatible** | `bdbea33ddf35` (M) | ★413 · 2025-11-21 | Adaptive radix tree (ART) | `graph/index/btree` (string prefix seek) | Candidate for string-key prefix indexes; measure against B+ tree prefix seek. |
| `github.com/armon/go-radix` | <https://github.com/armon/go-radix> | MIT → **compatible** | `54df44f2176c` (M) | ★946 · 2024-07-06 | Radix tree | — | Low value. |
| `github.com/hashicorp/go-immutable-radix/v2` (indirect dependency) | <https://github.com/hashicorp/go-immutable-radix> | MPL-2.0 → **conditional** | `581942a789eb` (L) | ★1106 · 2026-09-28 | Persistent radix tree with transactions and watch channels | — | Study only. |
| `github.com/hashicorp/go-memdb` (indirect dependency) | <https://github.com/hashicorp/go-memdb> | MPL-2.0 → **conditional** | `7d3fdd5f0f98` (M) | ★3475 · 2026-06-28 | MVCC in-memory DB on immutable radix trees | `graph/mvcc` | Study only. |
| `github.com/benbjohnson/immutable` | <https://github.com/benbjohnson/immutable> | MIT → **compatible** | `2477f6ef5b29` (L) | ★746 · 2023-08-17 | Persistent List, Map (HAMT), SortedMap (B+tree) | `graph/generation`, catalogue snapshots | Immutable sorted map for copy-on-write catalogue/schema snapshots. |
| `github.com/huandu/skiplist` | <https://github.com/huandu/skiplist> | MIT → **compatible** | `ceb3cd57cee0` (M) | ★428 · 2024-09-23 | Skiplist | — | Low value. |
| `github.com/zhangyunhao116/skipmap` | <https://github.com/zhangyunhao116/skipmap> | MIT → **compatible** | `81e3d4fd4147` (L) | ★222 · 2024-08-30 | Lock-free-read concurrent sorted skipmap (generated per key type) | `graph/index/btree` (concurrent ordered index) | Measure as a concurrent ordered index against the latched B+ tree. |
| `github.com/andy-kimball/arenaskl` | <https://github.com/andy-kimball/arenaskl> | Apache-2.0 → **compatible** | `f701008588b9` (M) | ★45 · 2026-06-08 | Lock-free arena-allocated skiplist (origin of Pebble's `arenaskl`) | — | Reference for zero-GC-pointer ordered memtables. |
| `github.com/emirpasic/gods/v2` | <https://github.com/emirpasic/gods> | BSD-2-Clause (read by hand; GitHub reported NOASSERTION) → **compatible** | `1d83d5ae39fb` (M) | ★17459 · 2025-03-12 | Containers: trees, heaps, lists | — | Low value. |
| `github.com/tidwall/rtree` | <https://github.com/tidwall/rtree> | MIT → **compatible** | `5a3893f505b0` (M) | ★349 · 2026-08-11 | Generic R-tree | future point/spatial index | Only if Cypher `point` indexing is added. |

## 12. Bitmaps, filters, and statistics sketches

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/RoaringBitmap/roaring/v2` (dependency) | <https://github.com/RoaringBitmap/roaring> | Apache-2.0 → **compatible** | `142f2902c210` (L) | ★2944 · 2026-09-21 · active | `BitSliceIndexing/bsi.go` (bit-sliced index: range, sum, min/max over integer columns), `roaring64`, frozen (zero-copy) serialisation | `graph/index/btree`, `graph/index/count`, `graph/index/stats` | BSI as a range index for integer properties and as a fast `sum`/`min`/`max` source; already in the module graph. |
| `github.com/dgraph-io/sroar` | <https://github.com/dgraph-io/sroar> | Apache-2.0 → **compatible** | `b92b7eaaf6e0` (L) | ★277 · 2023-03-29 · archived | Roaring bitmap that operates directly on a `[]byte` buffer (no decode) | `store/snapshot/labels.go`, `store/csrfile` | Zero-decode label bitmaps from mmap; archived, so copy and own. |
| `github.com/kelindar/bitmap` | <https://github.com/kelindar/bitmap> | MIT → **compatible** | `7c356f442d64` (L) | ★384 · 2026-09-23 · active | Dense bitmap with AVX/AVX-512/NEON assembly (`simd_*.s`) for And/Or/Xor/Count | `graph/index/label`, `search` visited sets | SIMD dense bitmaps for BFS frontiers and dense label sets. |
| `github.com/bits-and-blooms/bitset` (indirect dependency) | <https://github.com/bits-and-blooms/bitset> | BSD-3-Clause → **compatible** | `9f658c276882` (M) | ★1513 · 2026-10-04 | Dense bitset | `search` | Already pulled in by roaring. |
| `github.com/bits-and-blooms/bloom/v3` | <https://github.com/bits-and-blooms/bloom> | BSD-2-Clause → **compatible** | `4f9e5176a172` (M) | ★2816 · 2026-07-10 | Bloom filter | — | Superseded by binary fuse filters for static sets. |
| `github.com/FastFilter/xorfilter` | <https://github.com/FastFilter/xorfilter> | Apache-2.0 → **compatible** | `d0b48aea2307` (L) | ★764 · 2026-01-26 | Xor and binary fuse filters (`binaryfusefilter.go`), serialisation | `store/csrfile`, `cypher/exec/hash_join.go` | Static membership filter for semi-join pre-filtering and on-disk existence checks. |
| `github.com/axiomhq/hyperloglog` | <https://github.com/axiomhq/hyperloglog> | MIT → **compatible** | `bf0455c36bcb` (L) | ★1052 · 2026-09-14 · active | HLL with sparse representation and LogLog-Beta (`beta.go`, `sparse.go`) | `graph/index/stats/hll.go` | Compare accuracy and memory with GoGraph's HLL; sparse mode cuts memory for low-cardinality keys. |
| `github.com/DataDog/sketches-go` | <https://github.com/DataDog/sketches-go> | Apache-2.0 → **compatible** | `228b76a8e69d` (L) | ★187 · 2026-08-06 | DDSketch: relative-error quantiles, mergeable | `graph/index/stats/histogram.go`, latency metrics | Mergeable quantile sketch for statistics and latency histograms. |
| `github.com/influxdata/tdigest` | <https://github.com/influxdata/tdigest> | Apache-2.0 → **compatible** | `fc98d27c9e8b` (M) | ★152 · 2023-04-22 | t-digest | — | Alternative to DDSketch. |
| `github.com/pingcap/tidb/pkg/statistics` | <https://github.com/pingcap/tidb> | Apache-2.0 → **compatible** | `1f819a0b4a6c` (L) | ★40633 · 2026-10-08 · active | `histogram.go`, `cmsketch.go` (Count-Min + TopN), `fmsketch.go`, `estimate.go` | `graph/index/stats`, planner cardinality | Count-Min/TopN for frequent property values; equi-depth histogram merging. |

## 13. MVCC and transactions

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/dgraph-io/badger/v4` | see section 9 | Apache-2.0 → **compatible** | `78b99b8b0f01` (R) | see section 9 | Oracle with read/commit watermarks and fingerprint conflict detection | `graph/mvcc` | See top opportunity 6. |
| `github.com/vescale/zgraph/storage` | see section 4 | Apache-2.0 → **compatible** | `959c02d50f95` (R) | see section 4 | Arena memdb, Percolator MVCC, latches, lock resolver | `graph/mvcc` | Reference only. |
| `github.com/tikv/client-go/v2` | <https://github.com/tikv/client-go> | Apache-2.0 → **compatible** | `0bed899efb60` (L) | ★363 · 2026-10-08 · active | Arena-backed memdb in two variants, red-black tree and ART (`internal/unionstore/memdb_rbt.go`, `memdb_art.go`, `arena/`) | `graph/lpg/writeview.go` | Arena-backed per-transaction write buffer to cut allocations. |
| `github.com/kelindar/column` | <https://github.com/kelindar/column> | MIT → **compatible** | `b35d478cda58` (L) | ★1513 · 2025-06-28 | Columnar in-memory store with bitmap indexes, transactions (`txn.go`, `commit/`), snapshots | `graph/lpg/edge_property_column.go` | Columnar commit-log design reference. |

## 14. Concurrency primitives and concurrent maps

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/puzpuzpuz/xsync/v4` | <https://github.com/puzpuzpuz/xsync> | Apache-2.0 → **compatible** | `8a8c7d366d14` (R) | ★1725 · 2026-09-27 · active | `rbmutex.go` (BRAVO reader-biased RW mutex, per-slot padded reader counters), `counter.go` (striped counter), `map.go` (cache-line hash table), MPMC/SPSC/UMPSC queues | `graph` mapper and `graph/lpg` label-store `sync.RWMutex` (rmp #2741), `graph/index/count`, `metrics` | Implements a distributed reader indicator. `docs/design-reader-indicator.md` (#2203) targeted `Graph.View`, which #2344 removed; the live use is the reader-counter contention of #2741. |
| `golang.org/x/sync` | <https://github.com/golang/sync> | BSD-3-Clause → **compatible** | `36f2d70ecde9` (M) | ★933 · 2026-09-23 | `errgroup`, `semaphore`, `singleflight` | `cypher/exec/parallel*.go`, `bolt/server` | Weighted semaphore for bounded parallelism; singleflight for plan-cache misses. |
| `github.com/sourcegraph/conc` | <https://github.com/sourcegraph/conc> | MIT → **compatible** | `5f936abd7ae8` (M) | ★10434 · 2026-09-01 | Structured concurrency, panic propagation in pools | — | Low value. |
| `github.com/panjf2000/ants/v2` | <https://github.com/panjf2000/ants> | MIT → **compatible** | `c101e307893f` (M) | ★14514 · 2026-09-19 | Bounded goroutine pool | — | Low value: GoGraph already bounds parallelism. |
| `github.com/alitto/pond/v2` | <https://github.com/alitto/pond> | MIT → **compatible** | `7b7c16ad4903` (M) | ★2197 · 2026-10-05 | Bounded worker pool | — | Low value. |
| `github.com/cockroachdb/swiss` | <https://github.com/cockroachdb/swiss> | Apache-2.0 → **compatible** | `333444432258` (M) | ★479 · 2026-08-20 | Swiss table map | — | Low value: Go 1.24+ runtime maps are Swiss tables. |
| `github.com/dolthub/swiss` | <https://github.com/dolthub/swiss> | Apache-2.0 → **compatible** | `9ab8df34488d` (M) | ★822 · 2025-03-07 · archived | Swiss table map | — | Rejected (see section 27). |
| `github.com/alphadose/haxmap` | <https://github.com/alphadose/haxmap> | MIT → **compatible** | `fae115ca0907` (M) | ★1037 · 2024-10-27 | Lock-free hash map | — | Low value versus xsync. |
| `github.com/cornelk/hashmap` | <https://github.com/cornelk/hashmap> | Apache-2.0 → **compatible** | `3cb50f89b9db` (M) | ★1876 · 2025-07-30 | Lock-free hash map | — | Low value versus xsync. |
| `github.com/orcaman/concurrent-map/v2` | <https://github.com/orcaman/concurrent-map> | MIT → **compatible** | `85296bce0525` (M) | ★4528 · 2024-05-22 | Sharded map with mutexes | — | Low value. |
| `go.uber.org/atomic` | <https://github.com/uber-go/atomic> | MIT → **compatible** | `2d2bdbd262f9` (M) | ★1454 · 2026-09-21 | Typed atomics | — | Redundant with `sync/atomic` typed values. |
| `github.com/bytedance/gopkg` | <https://github.com/bytedance/gopkg> | Apache-2.0 → **compatible** | `c1b3d74ed4a3` (M) | ★2050 · 2026-09-18 | `lang/syncx` (sharded pool, RWMutex), `lang/mcache`, `collection/skipmap` | — | Reference only. |

## 15. Hashing and checksums

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/zeebo/xxh3` | <https://github.com/zeebo/xxh3> | BSD-2-Clause → **compatible** | `f7225998cebf` (L) | ★587 · 2026-09-08 · active | XXH3 64/128 with SSE/AVX2/AVX-512/NEON assembly | `cypher/exec/hash_join.go`, `distinct.go`, eager aggregation | Measure against `hash/maphash` for composite keys; 128-bit variant for collision-free fingerprints. |
| `github.com/cespare/xxhash/v2` | <https://github.com/cespare/xxhash> | MIT → **compatible** | `ab37246c889f` (M) | ★2145 · 2024-07-03 | XXH64 with assembly | — | Superseded by XXH3 for short keys. |
| `github.com/minio/highwayhash` | <https://github.com/minio/highwayhash> | Apache-2.0 → **compatible** | `070ab1a87a76` (M) | ★958 · 2026-03-21 | Keyed SIMD hash | — | Only if hash-flooding resistance is needed beyond `maphash`'s random seed. |
| `github.com/klauspost/crc32` | <https://github.com/klauspost/crc32> | BSD-3-Clause → **compatible** | `8d0d1a33e57d` (M) | ★84 · 2025-07-22 | CRC32 with extra SIMD paths | `store/wal`, `store/snapshot` | Low value: stdlib CRC32-C is already hardware-accelerated; verify with `store/wal/crc_strategy_bench_test.go`. |
| `github.com/dgryski/go-metro` | <https://github.com/dgryski/go-metro> | MIT, mechanical translation of MetroHash (MIT) → **compatible** | `edb8663e5e33` (M) | ★119 · 2025-01-06 | MetroHash | — | Low value. |

## 16. Compression and encoding

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/ronanh/intcomp` | <https://github.com/ronanh/intcomp> | Apache-2.0 → **compatible** | `f26ea81af5fd` (L) | ★99 · 2025-02-18 | Delta + bit-packing and delta + variable-byte codecs for int32/int64/uint32/uint64 sorted arrays (`deltapack*.go`, generated) | `store/csrfile`, `store/snapshot`, WAL id lists | Compress sorted neighbour lists and node-id columns on disk. |
| `github.com/mhr3/streamvbyte` | <https://github.com/mhr3/streamvbyte> | Apache-2.0 → **compatible** | `a31e0ac00f32` (M) | ★3 · 2026-07-27 | Stream VByte with SIMD decode | `store/csrfile` | Alternative codec to intcomp; low maturity. |
| `github.com/klauspost/compress` (dependency) | <https://github.com/klauspost/compress> | Root BSD-3-Clause; sub-packages carry their own notices → **compatible** (verify per sub-package) | `dd54d8695696` (M) | ★5660 · 2026-10-08 · active | zstd, S2, snappy, gzip, huff0, FSE | `store/snapshot`, `store/checkpoint` | Already used; `huff0`/`fse` could encode dictionary codes. |
| `github.com/pierrec/lz4/v4` | <https://github.com/pierrec/lz4> | BSD-3-Clause → **compatible** | `1595542f2080` (M) | ★972 · 2026-10-03 | LZ4 | — | Low value versus S2. |
| `github.com/golang/snappy` | <https://github.com/golang/snappy> | BSD-3-Clause → **compatible** | `9ae09f520e93` (M) | ★1572 · 2026-07-16 | Snappy | — | Superseded by S2. |
| `github.com/vmihailenco/msgpack/v5` | <https://github.com/vmihailenco/msgpack> | BSD-2-Clause → **compatible** | `19c91dfdfa06` (M) | ★2676 · 2024-06-04 | MessagePack codec (PackStream is MessagePack-like) | `bolt/packstream` | Reference only. |
| `github.com/tinylib/msgp` | <https://github.com/tinylib/msgp> | MIT → **compatible** | `c3ec6a5f8f37` (M) | ★1951 · 2026-09-30 | Code-generated MessagePack | `bolt/packstream` | Reference for allocation-free append-style encoders. |
| `github.com/fxamacker/cbor/v2` | <https://github.com/fxamacker/cbor> | MIT → **compatible** | `80edb983d498` (M) | ★1093 · 2026-10-08 | CBOR codec with decode limits | `bolt/packstream` | Reference for decoder resource limits (depth, length). |
| `github.com/segmentio/asm` | <https://github.com/segmentio/asm> | MIT-0 → **compatible** | `1cfacc81a878` (M) | ★926 · 2026-06-25 | Assembly kernels (base64, ascii, sort/dedupe, bswap) | `graph/csr`, `cypher/exec` | Sorted-slice dedupe and ascii validation kernels. |

## 17. Memory: arenas, pools, sorting, memory limits

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/ortuman/nuke` | <https://github.com/ortuman/nuke> | Apache-2.0 → **compatible** | `9b1f50e76915` (M) | ★548 · 2024-03-16 · archived | Bump arena allocator | `cypher/exec` per-query scratch | Insight only; arenas bypass GC safety, measure first. |
| `github.com/valyala/bytebufferpool` | <https://github.com/valyala/bytebufferpool> | MIT → **compatible** | `18533face0df` (M) | ★1333 · 2024-07-20 | Self-calibrating byte-buffer pool | `bolt/packstream/pool.go` | Calibrated size classes for Bolt response buffers. |
| `github.com/shawnsmithdev/zermelo/v2` | <https://github.com/shawnsmithdev/zermelo> | MIT → **compatible** | `385516e4283f` (L) | ★53 · 2023-08-10 | Generic LSD radix sort for integers and floats | `store/bulkimport`, `graph/csr` build, `internal/sortseam` | Radix sort for edge lists during CSR build and bulk import. |
| `github.com/twotwotwo/sorts` | <https://github.com/twotwotwo/sorts> | BSD-3-Clause → **compatible** | `bf5c1f2b8553` (L) | ★102 · 2023-02-13 | Parallel radix sort and quicksort (`parallel.go`, `radixsort.go`) | `store/bulkimport` | Parallel sort for large bulk imports. |
| `github.com/KimMachineGun/automemlimit` | <https://github.com/KimMachineGun/automemlimit> | MIT → **compatible** | `d879f1560fe7` (M) | ★565 · 2026-09-02 | cgroup-aware `GOMEMLIMIT` | `internal/memlimit` | cgroup v1/v2 limit detection for container deployments. |
| `go.uber.org/automaxprocs` | <https://github.com/uber-go/automaxprocs> | MIT → **compatible** | `1ea14c35ce47` (M) | ★4853 · 2025-11-02 | cgroup-aware `GOMAXPROCS` | — | Redundant from Go 1.25 (runtime is cgroup-aware). |
| `github.com/edsrzf/mmap-go` (dependency) | <https://github.com/edsrzf/mmap-go> | BSD-3-Clause → **compatible** | `fad1cd13edbd` (M) | ★1114 · 2024-12-12 | Portable mmap | `store/csrfile` | Already used. |

## 18. Caches

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/maypok86/otter/v2` | <https://github.com/maypok86/otter> | Apache-2.0 → **compatible** | `8c5263075564` (M) | ★2689 · 2026-06-19 | S3-FIFO / W-TinyLFU concurrent cache | plan cache in `cypher` | Bounded, contention-light plan cache with hit-ratio metrics. |
| `github.com/dgraph-io/ristretto/v2` | <https://github.com/dgraph-io/ristretto> | Apache-2.0 → **compatible** | `a56a38ebeec7` (M) | ★6998 · 2026-09-21 | TinyLFU cache with sampled eviction | plan cache | Alternative to otter. |
| `github.com/elastic/go-freelru` | <https://github.com/elastic/go-freelru> | Apache-2.0 → **compatible** | `3a0a715f3309` (M) | ★270 · 2026-09-25 | GC-less sharded LRU | plan cache | Alternative with lower GC pressure. |
| `github.com/dgryski/go-tinylfu` | <https://github.com/dgryski/go-tinylfu> | MIT → **compatible** | `8f22a03a23da` (M) | ★268 · 2025-01-24 | TinyLFU | — | Low value. |
| `github.com/hashicorp/golang-lru/v2` (indirect dependency) | <https://github.com/hashicorp/golang-lru> | MPL-2.0 → **conditional** | `9c13c57de0be` (M) | ★5124 · 2026-09-03 | LRU, 2Q, ARC | — | Study only. |

## 19. Vector (ANN) search

GoGraph has no vector index. These modules would seed one.

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/coder/hnsw` | <https://github.com/coder/hnsw> | CC0-1.0 → **compatible** | `36cab6028fed` (L) | ★239 · 2026-06-22 | Generic in-memory HNSW (`graph.go`, 13 KB) with binary encode/decode (`encode.go`) | new `graph/index/vector` | Smallest clean HNSW to adopt; no attribution obligation. |
| `github.com/weaviate/weaviate` | <https://github.com/weaviate/weaviate> | BSD-3-Clause outside `wl/`; `wl/` is proprietary (read by hand) → **compatible outside `wl/`** | `519a9ba23b39` (L) | ★16874 · 2026-10-08 · active | `adapters/repos/db/vector/hnsw` (production HNSW with commit log, compression, tombstone cleanup), `adapters/repos/db/lsmkv` (LSM store, BM25 block-max) | vector and full-text indexes | Production-grade HNSW persistence and deletion; check every file's path is outside `wl/`. |
| `github.com/orneryd/NornicDB/pkg/search` | see section 4 | MIT → **compatible** | `15644e54cf20` (R) | see section 4 | HNSW, IVF-HNSW, IVF-PQ, BM25 full-text, rerank | vector and full-text indexes | Graph-DB-integrated vector search reference. |
| `github.com/liliang-cn/cortexdb/pkg/index` | see section 4 | MIT → **compatible** | `4247495821b4` (R) | see section 4 | Flat, HNSW, IVF, LSH, binary quantisation | vector index | Second compatible source. |
| `github.com/fogfish/hnsw` | <https://github.com/fogfish/hnsw> | MIT → **compatible** | `89a7eb7eb6cc` (M) | ★27 · 2024-08-17 | HNSW | — | Low maturity. |
| `github.com/hupe1980/vecgo` | <https://github.com/hupe1980/vecgo> | Apache-2.0 → **compatible** | `08c67ffbfe81` (M) | ★23 · 2026-01-19 | Hybrid vector DB (HNSW, DiskANN-style) | — | Watch only. |
| `github.com/viterin/vek` | <https://github.com/viterin/vek> | MIT → **compatible** | `66944ab4bfc1` (L) | ★204 · 2025-09-06 | SIMD float32/float64 vector math (dot, distance) | vector index, `cypher/funcs` (`vector.similarity.*`) | SIMD distance kernels. |
| `github.com/milvus-io/milvus` | <https://github.com/milvus-io/milvus> | Apache-2.0 → **compatible** | `cab9d6525626` (L) | ★46340 · 2026-10-08 · active | Go control plane; index kernels are C++ (Knowhere) | — | Low Go-code value. |
| `github.com/philippgille/chromem-go` | <https://github.com/philippgille/chromem-go> | MPL-2.0 → **conditional** | `0622e2f6bedd` (M) | ★1063 · 2026-09-06 | Embedded vector store (exhaustive search) | — | Study only. |
| `github.com/sjy-dv/coltt` | <https://github.com/sjy-dv/coltt> | GPL-3.0 (search result) → **incompatible (insight-only)** | not pinned (M) | ★182 · 2025-03-23 | Vector DB | — | Insight only. |

## 20. Full-text search

GoGraph has no full-text index.

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/blevesearch/bleve/v2` | <https://github.com/blevesearch/bleve> | Apache-2.0 → **compatible** | `323561700942` (L) | ★11230 · 2026-10-05 · active | Analysis chains, query types (`search/query`: match, phrase, fuzzy, boolean, kNN), scorch segments | new full-text index | Analysers and query parser for a Neo4j-style `db.index.fulltext.*` procedure set. |
| `github.com/blugelabs/bluge` | <https://github.com/blugelabs/bluge> | Apache-2.0 → **compatible** | `574141970051` (L) | ★2034 · 2026-01-25 | Leaner bleve successor (`analysis`, `index`, `search`) | new full-text index | Smaller code base to adapt. |
| `github.com/kljensen/snowball` | <https://github.com/kljensen/snowball> | MIT → **compatible** | `80af012a384a` (L) | ★296 · 2025-11-13 | Snowball stemmers (English, French, Spanish, Russian, Swedish, Norwegian, Hungarian) | full-text analysers | Stemmers without the full bleve stack. |

## 21. Temporal types

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/rickb777/period` | <https://github.com/rickb777/period> | BSD-3-Clause → **compatible** | `e3e92cf2b04e` (M) | ★16 · 2026-10-06 | ISO-8601 periods (years/months/days/time parts kept separate) | `cypher/funcs/temporal.go` | Cross-check Cypher `duration()` parsing and normalisation. |
| `github.com/rickb777/date/v2` | <https://github.com/rickb777/date> | BSD-3-Clause → **compatible** | `bd068da6d29c` (M) | ★142 · 2026-10-06 | Calendar date, ISO week date | `cypher/funcs/temporal.go` | Week-based-year and ordinal date edge cases. |
| `github.com/sosodev/duration` | <https://github.com/sosodev/duration> | MIT → **compatible** | `26225984f2de` (L) | ★39 · 2026-05-29 | ISO-8601 duration parser (`duration.go`) | `cypher/funcs/temporal.go` | Small parser to diff against. |
| `github.com/itchyny/timefmt-go` | <https://github.com/itchyny/timefmt-go> | MIT → **compatible** | `013f4b9ebf33` (M) | ★197 · 2026-10-01 | strftime/strptime | — | Low value: Cypher has no strftime. |
| `github.com/lestrrat-go/strftime` | <https://github.com/lestrrat-go/strftime> | MIT → **compatible** | `41f81f6c05d9` (M) | ★135 · 2026-10-05 | strftime | — | Low value. |

## 22. Expressions, planning, and execution

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/dolthub/go-mysql-server` | <https://github.com/dolthub/go-mysql-server> | Apache-2.0 → **compatible** | `71bfcfcc3400` (L) | ★2658 · 2026-10-08 · active | `sql/memo` (memo, `join_order_builder.go`, `coster.go`, join hints) | Cypher planner (join ordering) | Join-order enumeration over a memo for multi-pattern MATCH. |
| `github.com/pingcap/tidb/pkg/planner/cascades` | see section 12 | Apache-2.0 → **compatible** | `1f819a0b4a6c` (L) | see section 12 | Cascades optimiser (memo, rules, tasks), `pkg/util/ranger` (predicate → range detachment) | Cypher planner, index range seek | Ranger logic for composite-index range derivation. |
| `vitess.io/vitess` | <https://github.com/vitessio/vitess> | Apache-2.0 → **compatible** | `2b77bd16d671` (L) | ★21378 · 2026-10-08 · active | goyacc SQL grammar (`go/vt/sqlparser/sql.y`) with a hand-written tokenizer (`token.go`), planner | `cypher/parser` | Insight for a hand-written lexer if ANTLR lexing cost dominates. |
| `github.com/expr-lang/expr` | <https://github.com/expr-lang/expr> | MIT → **compatible** | `4b31df3a2e0e` (L) | ★8037 · 2026-07-07 | Expression compiler to bytecode and stack VM (`vm/vm.go`, `vm/opcodes.go`) | `cypher/expr` | Bytecode evaluation of WHERE/RETURN expressions instead of tree walking; measure. |
| `github.com/google/cel-go` | <https://github.com/google/cel-go> | Apache-2.0 → **compatible** | `6d32ef54e4f1` (M) | ★2 (API value, see section 2) · 2026-09-10 | CEL with partial evaluation and cost estimation | `cypher/expr` | Insight for static cost estimation of expressions. |
| `github.com/PaesslerAG/gval` | <https://github.com/PaesslerAG/gval> | BSD-3-Clause → **compatible** | `c622ea2f0e82` (M) | ★817 · 2026-09-13 | Closure-compiled expression evaluator | `cypher/expr` | Closure compilation as a lighter alternative to bytecode. |
| `github.com/Knetic/govaluate` | <https://github.com/Knetic/govaluate> | MIT → **compatible** | `7625b7f8c03d` (M) | ★3929 · 2025-03-25 · archived | Expression evaluator | — | Rejected (see section 27). |
| `github.com/apache/arrow-go/v18` | <https://github.com/apache/arrow-go> | Apache-2.0 → **compatible** | `b02cc0111095` (L) | ★412 · 2026-10-06 · active | `arrow/compute` vectorised kernels (arithmetic, cast, take, filter), memory allocators | `cypher/exec` columnar chunks | Kernel shapes for columnar filter/project; NOTICE file must be carried. |
| `github.com/matrixorigin/matrixone` | <https://github.com/matrixorigin/matrixone> | Apache-2.0 → **compatible** | `5b7453a924c0` (M) | ★2040 · 2026-10-08 | Vectorised SQL engine in Go | `cypher/exec` | Insight only. |
| `github.com/cockroachdb/cockroach` | <https://github.com/cockroachdb/cockroach> | CockroachDB Software License (source-available; read by hand) → **incompatible (insight-only)** | `d30c905fff79` (M) | ★32554 · 2026-10-03 | Cost-based optimiser (`opt`), vectorised `colexec` | — | Insight only. |

## 23. Observability and metrics

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/HdrHistogram/hdrhistogram-go` | <https://github.com/HdrHistogram/hdrhistogram-go> | MIT → **compatible** | `687f30397c26` (L) | ★470 · 2026-10-08 · active | HDR histogram, windowed histograms, hardened decoder | `internal/metrics` (`ObserveLatency`), `bench/*` | Fixed-memory, high-precision latency recording for p99/p999 reporting. |
| `github.com/VictoriaMetrics/metrics` | <https://github.com/VictoriaMetrics/metrics> | MIT → **compatible** | `ad81605b76d0` (M) | ★710 · 2026-09-28 | Lightweight Prometheus exposition with `vmrange` histograms | `internal/metrics/prometheus` | Allocation-light exposition writer. |
| `github.com/prometheus/client_golang` | <https://github.com/prometheus/client_golang> | Apache-2.0 → **compatible** | `de866d63fa67` (M) | ★6041 · 2026-10-07 | Reference client, native histograms | `internal/metrics/prometheus` | Native-histogram exposition format reference. |
| `go.opentelemetry.io/otel` | <https://github.com/open-telemetry/opentelemetry-go> | Apache-2.0 → **compatible** | `6b3ec1621285` (M) | ★6575 · 2026-10-08 | Tracing and metrics API | — | Out of scope unless tracing is requested. |
| `github.com/rcrowley/go-metrics` | <https://github.com/rcrowley/go-metrics> | BSD-2-Clause style (read by hand; GitHub reported NOASSERTION) → **compatible** | `65e299d6c5c9` (M) | ★3460 · 2025-04-01 · archived | Metrics registry | — | Rejected (see section 27). |

## 24. Testing: property tests, fuzzing, DST, fault injection, linearizability

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/anishathalye/porcupine` | <https://github.com/anishathalye/porcupine> | MIT → **compatible** | `97cd067defd9` (R) | ★1256 · 2026-09-21 · active | Linearizability checker (`checker.go`, P-compositional partitioning, `model.go`), HTML visualisation | `internal/isolationtest`, `internal/sim` | Check concurrent single-key histories (node property register, counters) for linearizability. |
| `github.com/AdaLogics/go-fuzz-headers` | <https://github.com/AdaLogics/go-fuzz-headers> | Apache-2.0 → **compatible** | `e8a1dd7889d6` (L) | ★111 · 2024-08-06 | `consumer.go` (structured values from fuzz bytes), `sql.go` (SQL generation from fuzz input) | `cypher` fuzz tests, `bolt/server` | Structured fuzzing of Cypher parameters and Bolt message sequences. |
| `github.com/leanovate/gopter` | <https://github.com/leanovate/gopter> | MIT → **compatible** | `967a5004fb70` (M) | ★639 · 2026-04-20 | Property testing with stateful commands | — | Redundant with rapid. |
| `pgregory.net/rapid` (dependency) | <https://github.com/flyingmutant/rapid> | MPL-2.0 → **conditional** | `6706a6fd8373` (M) | ★896 · 2026-09-04 | Property testing, state machines, shrinking | test code | Already a test dependency; using it as a dependency is fine, copying its files keeps them MPL-2.0. |
| `github.com/pingcap/failpoint` | <https://github.com/pingcap/failpoint> | Apache-2.0 → **compatible** | `55ac33a48e3b` (L) | ★896 · 2026-08-11 | Marker-based fail points rewritten at build time (`failpoint-ctl`, `failpoint-toolexec`) | `internal/crashpoint` | Zero-cost-in-production fail points via toolexec rewriting. |
| `go.etcd.io/gofail` | <https://github.com/etcd-io/gofail> | Apache-2.0 → **compatible** | `9052e0cbf738` (M) | ★427 · 2026-09-02 | Comment-based fail points | `internal/crashpoint` | Alternative to failpoint. |
| `github.com/cockroachdb/pebble/vfs/errorfs` | see section 9 | BSD-3-Clause → **compatible** | `e0818dd07ec5` (L) | see section 9 | I/O fault-injection DSL, latency injection | `internal/testfs`, `internal/synclatency` | See top opportunity 4. |
| `github.com/spf13/afero` | <https://github.com/spf13/afero> | Apache-2.0 → **compatible** | `eb6a92826ea5` (M) | ★6710 · 2026-10-06 | Filesystem abstraction, in-memory FS | `internal/testfs` | Low value versus Pebble's vfs for durability semantics. |
| `github.com/sasha-s/go-deadlock` | <https://github.com/sasha-s/go-deadlock> | Apache-2.0 → **compatible** | `222d9c980ff7` (L) | ★1197 · 2026-08-01 | Drop-in mutexes with lock-order inversion and timeout detection | `graph/lpg/reentrancy_*.go` test builds | Lock-order checking in a test build tag. |
| `github.com/hmdsefi/faultline` | <https://github.com/hmdsefi/faultline> | Apache-2.0 → **compatible** | `b3910de25b59` (M) | ★1 · 2026-10-08 | DST with simulated network and time | `internal/sim` | Watch only. |
| `github.com/formancehq/dst` | <https://github.com/formancehq/dst> | MIT → **compatible** | `1289386c0525` (M) | ★3 · 2026-08-18 | DST harness | `internal/sim` | Watch only. |
| `github.com/k1LoW/detest` | <https://github.com/k1LoW/detest> | MIT → **compatible** | `bab42f2dd54d` (M) | ★0 · 2026-10-08 | In-process DST | `internal/sim` | Watch only. |
| `github.com/pingcap/go-ycsb` | <https://github.com/pingcap/go-ycsb> | Apache-2.0 → **compatible** | `f030f9942393` (M) | ★642 · 2025-12-31 | YCSB workloads and generators (zipfian, latest) | `bench/*` | Zipfian key generators for skewed concurrency benchmarks. |
| `go.uber.org/goleak` (dependency) | <https://github.com/uber-go/goleak> | MIT → **compatible** | `b656bfda2fbf` (M) | ★5291 · 2026-09-15 | Goroutine leak detection | tests | Already used. |
| `github.com/cucumber/godog` (dependency) | <https://github.com/cucumber/godog> | MIT → **compatible** | `42208e75a9f7` (M) | ★2678 · 2026-09-25 | Gherkin runner | `cypher/tck` | Already used. |

## 25. Networking and runtime

| Module | Repo | Licence → verdict | Checked at | Maturity | Useful content | Target | Opportunity |
|---|---|---|---|---|---|---|---|
| `github.com/panjf2000/gnet/v2` | <https://github.com/panjf2000/gnet> | Apache-2.0 → **compatible** | `441457f07983` (M) | ★11253 · 2026-07-09 | Event-loop networking (epoll/kqueue) | `bolt/server` | Insight only: replaces goroutine-per-connection; conflicts with the blocking session model. |
| `github.com/cloudwego/netpoll` | <https://github.com/cloudwego/netpoll> | Apache-2.0 → **compatible** | `d63d249a3ba0` (M) | ★4609 · 2026-08-06 | Zero-copy linked buffers and event loop | `bolt/proto/chunking.go` | Linked-buffer reader design for chunk reassembly. |
| `github.com/klauspost/cpuid/v2` | <https://github.com/klauspost/cpuid> | MIT → **compatible** | `8ce0278afc65` (M) | ★1221 · 2026-07-20 | CPU feature detection | SIMD dispatch | Needed only if GoGraph adds its own SIMD; `golang.org/x/sys/cpu` is already available. |

---

## 26. Top opportunities

Ranked by expected gain across correctness, performance, efficiency, and security.
Every expected gain is a hypothesis to be measured in GoGraph; none is established.

### 1. Reader-biased RW lock for the mapper and label store (BRAVO)
- **Module and files:** `github.com/puzpuzpuz/xsync/v4` — `rbmutex.go`, `util.go` (cache-line constant, parallelism).
- **Target:** the `sync.RWMutex` reader-counter contention in the `graph` mapper and the `graph/lpg` label store, measured by rmp #2741 at 8.44% of CPU at level 8. (`docs/design-reader-indicator.md`, #2203, targeted the `Graph.View` barrier, which #2344 removed.)
- **Expected gain:** removes the shared `readerCount` cache line that the design document measured at 17.6× degradation from 1 to 10 cores for bare `sync.RWMutex`; read scalability with core count (CLAUDE.md mandate 3).
- **Licence:** Apache-2.0 → compatible (carry the licence; mark the file modified). Adaptation: the package-level `rtokenPool` must become instance-owned to respect "no package-level mutable state".
- **Verify:** the #2741 profile workload at 1, 8, 64, 256 and 1024 goroutines with `benchstat`; `go test -race`; a writer-starvation test under sustained reads.

### 2. Graph colouring, maximal cliques, and cycle enumeration
- **Module and files:** `gonum.org/v1/gonum/graph` — `coloring/coloring.go`, `topo/bron_kerbosch.go`, `topo/johnson_cycles.go`, `topo/paton_cycles.go`, `topo/transitive_reduction.go`.
- **Target:** new files in `search/` (and `cypher/procs` exposure if requested).
- **Expected gain:** new algorithm features with gonum's tests as oracles.
- **Licence:** BSD-3-Clause → compatible.
- **Verify:** port gonum's table tests; property tests (a colouring is proper; every reported clique is maximal; cycle counts on known graphs).

### 3. Linearizability checking in the isolation harness
- **Module and files:** `github.com/anishathalye/porcupine` — `checker.go`, `model.go`, `porcupine.go`, `bitset.go`.
- **Target:** `internal/isolationtest`, `internal/sim` concurrent scenarios.
- **Expected gain:** correctness: detects isolation anomalies on single-key registers (property values, counters, label membership) that golden-file scenarios cannot enumerate.
- **Licence:** MIT → compatible.
- **Verify:** seed a known anomaly (the regression of an already-fixed isolation defect) and assert the checker reports it; assert a correct history passes.

### 4. Deterministic I/O fault injection for the persistence layer
- **Module and files:** `github.com/cockroachdb/pebble` — `vfs/errorfs/errorfs.go`, `vfs/errorfs/dsl.go`, `vfs/errorfs/latency.go`, `vfs/disk_health.go`.
- **Target:** `internal/testfs`, `internal/crashinject`, `internal/synclatency`, `store/wal`.
- **Expected gain:** correctness and durability evidence: rule-driven injection of `EIO`/`ENOSPC`/short writes on chosen operations and paths; disk-health monitoring that surfaces stalled fsync as a typed error.
- **Licence:** BSD-3-Clause → compatible.
- **Verify:** crash/recovery battery with injected errors on WAL `Sync`, segment rename, and checkpoint write; each must fail-stop with no acknowledged commit lost.

### 5. Compressed neighbour lists and ID columns on disk
- **Module and files:** `github.com/ronanh/intcomp` — `deltapackuint32.go`, `deltapackuint64.go`, `delta_bitpacking.go`, `compress.go`.
- **Target:** `store/csrfile` (format version bump), `store/snapshot/nodeids.go`.
- **Expected gain:** storage and I/O efficiency: sorted adjacency lists delta-encode well; smaller files reduce mmap page faults.
- **Licence:** Apache-2.0 → compatible.
- **Verify:** on-disk size and cold-read traversal benchmarks on RMAT and LDBC graphs before/after; decode cost in `bench/csrorder`; format round-trip and corruption tests.

### 6. Fingerprint-based write-conflict detection cross-check
- **Module and files:** `github.com/dgraph-io/badger/v4` — `txn.go` (`oracle`, `hasConflict`, `conflictKeys`), `y/watermark.go`.
- **Target:** `graph/mvcc/conflict.go`, `graph/mvcc/horizon.go`.
- **Expected gain:** a second, independent conflict-detection implementation as a differential oracle; possible memory saving for large write sets.
- **Licence:** Apache-2.0 → compatible.
- **Verify:** differential test feeding identical transaction schedules to both detectors; MVCC write-contention benchmark (`bench/mvccwrite`).

### 7. Vector index (HNSW)
- **Module and files:** `github.com/coder/hnsw` — `graph.go` and companions; production references in `weaviate/weaviate` `adapters/repos/db/vector/hnsw` (BSD-3-Clause outside `wl/`) and `orneryd/NornicDB` `pkg/search/hnsw_index.go`.
- **Target:** new `graph/index/vector`, `cypher/procs` (`db.index.vector.*`), `cypher/funcs` (`vector.similarity.*`).
- **Expected gain:** a new feature with Neo4j parity; coder/hnsw carries no attribution obligation.
- **Licence:** CC0-1.0 → compatible (weaviate: compatible outside `wl/`; NornicDB: MIT).
- **Verify:** recall@10 against brute force on a public dataset; insert/delete under MVCC; persistence through checkpoint and recovery.

### 8. Full-text index with analysers
- **Module and files:** `github.com/blugelabs/bluge` (`analysis/`, `index/`, `search/`) or `github.com/blevesearch/bleve/v2` (`analysis/`, `search/query/`); stemmers from `github.com/kljensen/snowball`.
- **Target:** new full-text index and `db.index.fulltext.*` procedures.
- **Expected gain:** a new feature with Neo4j parity.
- **Licence:** Apache-2.0 / MIT → compatible.
- **Verify:** scoring and tokenisation tests against Neo4j behaviour; index maintenance under concurrent writes; recovery after crash.

### 9. Differential Cypher fuzzing with Dinkel
- **Module and files:** `github.com/CelineWuest/dinkel` — `translator/`, `dbms/`, `scheduler/`, `targets-config.yml`.
- **Target:** a new `cmd/` fuzz driver or `internal/sim` adapter targeting GoGraph over Bolt.
- **Expected gain:** correctness and security: state-aware query generation found bugs in Neo4j, FalkorDB, Apache AGE, and Memgraph according to its README; differential runs against Neo4j expose semantic divergences the TCK does not cover.
- **Licence:** MIT → compatible.
- **Verify:** run against GoGraph's Bolt server for a fixed seed budget; every crash or divergence becomes a regression test.

### 10. Structured fuzzing of Cypher parameters and Bolt sequences
- **Module and files:** `github.com/AdaLogics/go-fuzz-headers` — `consumer.go`.
- **Target:** new `Fuzz*` tests in `cypher` and `bolt/server`.
- **Expected gain:** security: reaches executor and session-state paths that byte-level fuzzing of PackStream does not.
- **Licence:** Apache-2.0 → compatible.
- **Verify:** coverage of `cypher/exec` and `bolt/server` reached by the new fuzz targets in a fixed time budget; no panics, no unbounded allocation.

### 11. Join-order enumeration for multi-pattern MATCH
- **Module and files:** `github.com/dolthub/go-mysql-server` — `sql/memo/join_order_builder.go`, `memo.go`, `coster.go`.
- **Target:** Cypher planner (`cypher/ir`, plan construction feeding `cypher/exec`).
- **Expected gain:** performance on multi-pattern and cyclic queries. This survey did not audit the current planner's join ordering; confirm the gap (see `docs/audit-planner-vs-neo4j-memgraph-2026-07-25.md`) before adopting.
- **Licence:** Apache-2.0 → compatible.
- **Verify:** `bench/cypher_ldbc`, `bench/cyclicjoin` plans and timings before/after; TCK must stay at baseline.

### 12. Bit-sliced index for integer property ranges and aggregates
- **Module and files:** `github.com/RoaringBitmap/roaring/v2` — `BitSliceIndexing/bsi.go` (already in the module graph).
- **Target:** `graph/index/btree` alternative for integer range predicates; `cypher/exec/agg_column_kernel.go` for `sum`/`min`/`max` over a label.
- **Expected gain:** range filters become bitmap operations combinable with label bitmaps; no new dependency.
- **Licence:** Apache-2.0 → compatible.
- **Verify:** range-seek and aggregation benchmarks against the B+ tree; index maintenance under MVCC.

### 13. Zero-decode label bitmaps in snapshots
- **Module and files:** `github.com/dgraph-io/sroar` — `bitmap.go`, `container.go`, `setutil.go` (archived upstream; copy and own).
- **Target:** `store/snapshot/labels.go`, `store/csrfile`.
- **Expected gain:** recovery time and memory: label bitmaps used straight from the mmap buffer instead of decoded.
- **Licence:** Apache-2.0 → compatible.
- **Verify:** recovery-time and resident-memory measurement on a large snapshot; round-trip tests against roaring.

### 14. SIMD dense bitmaps for traversal frontiers
- **Module and files:** `github.com/kelindar/bitmap` — `bitmap.go`, `simd_neon.s`, `simd_avx.s`, `simd_avx512.s`, `simd_generic.go`.
- **Target:** `search/bfs_do.go` (direction-optimising BFS frontier), `graph/index/label` dense sets.
- **Expected gain:** CPU on dense frontiers and dense label intersections.
- **Licence:** MIT → compatible.
- **Verify:** `bench/rmat` BFS and label-intersection benchmarks with `benchstat`; generic fallback tested on all architectures.

### 15. Bytecode evaluation of Cypher expressions
- **Module and files:** `github.com/expr-lang/expr` — `vm/vm.go`, `vm/opcodes.go`, `vm/program.go`, `compiler/`.
- **Target:** `cypher/expr`.
- **Expected gain:** CPU on filter- and projection-heavy queries by replacing tree walking with a flat instruction stream.
- **Licence:** MIT → compatible.
- **Verify:** `bench/cypher_alloc` and filter-heavy LDBC queries; three-valued-logic and TCK conformance unchanged.

### 16. Plan cache with a bounded, contention-light eviction policy
- **Module and files:** `github.com/maypok86/otter/v2` (S3-FIFO); alternatives `dgraph-io/ristretto`, `elastic/go-freelru`.
- **Target:** the Cypher statement/plan cache.
- **Expected gain:** hit ratio and contention under 256–1024 goroutines; exported utilisation metrics as CLAUDE.md requires.
- **Licence:** Apache-2.0 → compatible.
- **Verify:** concurrency benchmark at the published goroutine levels; hit-ratio metric on a skewed (zipfian) workload.

### 17. Mergeable quantile sketches for statistics and latency
- **Module and files:** `github.com/DataDog/sketches-go/ddsketch`; `github.com/HdrHistogram/hdrhistogram-go` (`hdr.go`).
- **Target:** `graph/index/stats/histogram.go`, `internal/metrics` (`ObserveLatency`).
- **Expected gain:** bounded-error percentiles with fixed memory, mergeable across shards.
- **Licence:** Apache-2.0 / MIT → compatible.
- **Verify:** quantile-error tests against exact quantiles; planner estimate error on `bench/scenarios`.

### 18. Count-Min sketch with Top-N for frequent property values
- **Module and files:** `github.com/pingcap/tidb/pkg/statistics` — `cmsketch.go`, `cmsketch_util.go`, `fmsketch.go`.
- **Target:** `graph/index/stats/mcv.go`, `collector.go`.
- **Expected gain:** better equality-selectivity estimates for skewed values beyond the MCV list.
- **Licence:** Apache-2.0 → compatible.
- **Verify:** estimate-error measurement on skewed datasets; plan-choice changes on LDBC.

### 19. Radix sort for CSR build and bulk import
- **Module and files:** `github.com/shawnsmithdev/zermelo/v2` (`sorter.go`, `zermelo.go`); `github.com/twotwotwo/sorts` (`parallel.go`, `radixsort.go`).
- **Target:** `store/bulkimport`, `graph/csr` construction, `internal/sortseam`.
- **Expected gain:** CPU on large edge-list sorts.
- **Licence:** MIT / BSD-3-Clause → compatible.
- **Verify:** bulk-import wall-clock and allocation benchmarks against `slices.Sort`.

### 20. Build-time fail points with zero production cost
- **Module and files:** `github.com/pingcap/failpoint` — `failpoint-toolexec/`, `code/` (rewriter), `failpoint.go`.
- **Target:** `internal/crashpoint`.
- **Expected gain:** fail points compiled out of production binaries entirely; finer crash-injection placement.
- **Licence:** Apache-2.0 → compatible.
- **Verify:** binary diff shows no fail-point code in release builds; crash battery reaches the new points.

### 21. Pre-allocated next WAL segment off the commit path
- **Module and files:** `go.etcd.io/etcd/server/v3/storage/wal` — `file_pipeline.go`, `wal.go` (segment cut).
- **Target:** `store/wal/writer.go`.
- **Expected gain:** removes file creation and allocation latency from the commit that crosses a segment boundary (tail latency).
- **Licence:** Apache-2.0 → compatible.
- **Verify:** p99/p999 commit latency across segment rollover on a real disk (not the RAM drive); crash tests at the rollover point.

### 22. HITS, diffusion, and dominator trees
- **Module and files:** `gonum.org/v1/gonum/graph` — `network/hits.go`, `network/diffusion.go`, `flow/control_flow_lt.go`, `flow/control_flow_slt.go`.
- **Target:** `search/centrality`, new `search` files.
- **Expected gain:** new algorithm features.
- **Licence:** BSD-3-Clause → compatible.
- **Verify:** port gonum's tests; cross-check HITS against PageRank on known graphs.

### 23. Louvain community detection
- **Module and files:** `gonum.org/v1/gonum/graph/community` — `louvain_common.go`, `louvain_undirected.go`, `louvain_directed.go`.
- **Target:** `search/community` (next to Leiden and label propagation).
- **Expected gain:** a commonly requested algorithm; gonum's Leiden is also a cross-check for `search/community/leiden.go`.
- **Licence:** BSD-3-Clause → compatible.
- **Verify:** modularity on benchmark graphs (Zachary, LFR) against gonum's reference values.

### 24. Bolt over WebSocket
- **Module and files:** `github.com/orneryd/NornicDB/pkg/bolt` — `transport_ws.go`, `wsconn.go`, `transport_select.go`.
- **Target:** `bolt/server`.
- **Expected gain:** browser clients (Neo4j Browser, neo4j-driver JS over WebSocket) can connect.
- **Licence:** MIT → compatible.
- **Verify:** handshake and message round-trip tests with a WebSocket client; slow-loris and size-limit tests apply to the new transport.

---

## 27. Modules examined and rejected

| Module | Reason |
|---|---|
| `github.com/SamuelSupe/graphdb` | Licence file reserves all rights. |
| `github.com/mstrYoda/goraphdb` | No licence file. |
| `github.com/jtejido/go-opencypher` | No licence file; generated parser only. |
| `github.com/prologic/bitcask` | GitHub location holds only a README; no code and no licence there. |
| `github.com/katalvlaran/lvlath` | AGPL-3.0. |
| `github.com/ScottSallinen/lollipop` | LGPL-2.1 (search-result classification). |
| `github.com/sjy-dv/coltt` | GPL-3.0 (search-result classification). |
| `github.com/tamerh/biobtree`, `github.com/postmannen/graphed`, `github.com/spaceqraft/vitaledge` | AGPL-3.0 (search-result classification); unrelated scope. |
| `github.com/vchakoshy/graphdb`, `github.com/voodooEntity/slingshotdb` | GPL-3.0 (search-result classification). |
| `github.com/cockroachdb/cockroach` | CockroachDB Software License (source-available); insight only. |
| `weaviate/weaviate` `wl/` directory | Proprietary; the rest of the repository is BSD-3-Clause. |
| `github.com/dolthub/swiss`, `github.com/cockroachdb/swiss` | Go 1.24+ runtime maps are already Swiss tables; dolthub/swiss is archived. |
| `github.com/Knetic/govaluate` | Archived; superseded by expr-lang/expr. |
| `github.com/rcrowley/go-metrics` | Archived; not Prometheus-native. |
| `go.uber.org/automaxprocs` | Redundant: the Go runtime is cgroup-aware for `GOMAXPROCS` from Go 1.25. |
| `go.uber.org/atomic` | Redundant with `sync/atomic` typed values. |
| `github.com/google/btree` | Archived; tidwall/btree is the maintained reference. |
| `github.com/golang/snappy`, `github.com/pierrec/lz4` | S2 in the existing `klauspost/compress` dependency covers the same need. |
| `github.com/kuzudb/go-kuzu`, `github.com/LadybugDB/go-ladybug`, `github.com/FalkorDB/falkordb-go`, `github.com/RedisGraph/redisgraph-go`, `github.com/vesoft-inc/nebula-go`, `github.com/apache/age` Go driver | Client drivers or cgo bindings: no engine code in Go. |
| `github.com/z5labs/gogm`, `github.com/rlch/neogo`, `github.com/manhcuongbk56/cypher-go-dsl`, `github.com/mindstand/go-cypherdsl` | Cypher query builders and OGMs for Neo4j clients; no engine code. |
| `github.com/milvus-io/milvus` | Index kernels are C++; the Go code is control plane. |
| `github.com/ortuman/nuke` | Archived; arena allocation conflicts with GC safety guarantees unless measured otherwise. |
| `github.com/panjf2000/gnet`, `github.com/cloudwego/netpoll` | Event-loop model conflicts with Bolt's blocking per-session design; insight only. |
| `github.com/go-gremlin/gremlin`, `github.com/qasaur/gremgo`, `github.com/northwesternmutual/grammes` | Gremlin clients; no engine code. |
| `github.com/hashicorp/raft`, `github.com/lni/dragonboat`, `github.com/rqlite/rqlite` | Replication and consensus: no corresponding GoGraph feature. |
| Small or single-author graph libraries (`amitkgupta/goraph`, `soniakeys/graph2`, `h12w/graph`, `etnz/graph`, `vc-souza/gga`, and similar results of the "graph algorithms" search) | No licence or no maturity, and no algorithm GoGraph lacks that gonum does not provide better tested. |

## 28. Counts

Modules with a verdict row in sections 4–25 (each module counted once; repository
sections cross-referenced from another section are not counted again):

| Verdict | Count |
|---|---|
| compatible | 159 |
| conditional (MPL-2.0) | 6 |
| incompatible (insight-only) | 8 |
| **total** | **173** |

The incompatible count covers the rows in sections 4–25; section 27 lists further
GPL/AGPL search results that were not given a row.
