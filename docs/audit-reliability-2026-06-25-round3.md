# Reliability Audit — 2026-06-25 (round 3)

A third empirical reliability pass after rounds 1–2 (20 findings, all fixed).
This round went into territory the earlier passes did not cover: Cypher
**pattern-matching / variable-length-path / clause-composition** semantics, the
**Bolt protocol state machine**, **storage recovery under corruption**, and
**analytics adversarial correctness** (incl. the weightless-mode CSR path). The
deeper probing was productive — it surfaced **2 Critical** defects in
less-trodden paths that rounds 1–2 missed.

## Method (empirical)

| Gate | Result |
|---|---|
| `go build ./...`, `go vet ./...`, `gofmt` | clean |
| Deep fuzz (50–60 s) — csv, csrfile, jsonl-sec | no crashers |
| DST swarm — write-heavy / read-heavy / bad-actor workloads (not run before) | all pass, 0 failures |
| Cross-release back-compat (`-tags=soak`) — recover v0.2.0/v0.3.0 WAL images | green |
| `go test -race ./...` (whole module) | green |
| openCypher TCK | 3897, held before & after every fix |

Four specialist clusters: cypher pattern/path/composition; storage
recovery-under-corruption; Bolt protocol state-machine; analytics adversarial +
weightless-mode.

## Findings → rmp sprint 244 (8 tasks, 11 findings)

| # | Sev | Finding | Fix |
|---|---|---|---|
| 1776 | **Critical** | shortest-path relaxers (Dijkstra/Bellman-Ford/A\*/BiDijkstra/Johnson) **panic** on a weightless-mode CSR (nil weights) | `5f5ee47` nil-weight guard |
| 1777 | **Critical** | relationship-uniqueness not enforced across comma-separated MATCH patterns → wrong results | `6749eca` thread prior-pattern rel vars into the no-repeat predicate |
| 1778 | High | WAL corrupt length-field over-declaration masquerades as a benign torn tail → silent data loss | `57618ab` scan consumed bytes for an embedded CRC-valid frame |
| 1779 | High | shortestPath/allShortestPaths src==dst lower-bound ≥1 never finds the shortest cycle | `92c1db9` directed cycle (undirected safe + deferred → #1785) |
| 1780 | High | allShortestPaths ignores ctx + no work cap → hang/OOM | `bead0e2` ctx checks in reconstructAll |
| 1781 | Medium | Bolt FAILED state replies FAILURE instead of IGNORED | `9c842d7` IGNORE request-phase msgs on authenticated FAILED |
| 1782 | Medium | WHERE on shortestPath applied as post-filter, not during search | `92c1db9` documented + pinned (full fix → #1786) |
| 1783 | Low | Bolt qid not validated / DISCARD ignores n / tx_metadata dropped | `739684a` qid validation; n + tx_metadata documented (partial-DISCARD → #1787) |

### Deferred (documented + tracked backlog)

Three findings had a correct, focused fix shipped plus a larger full
implementation tracked, because forcing the larger change would have risked
destabilising a correct path or shipping a worse result:

- **#1785** — undirected src==dst shortest cycle (Itai–Rodeh branch-collision).
  An undirected edge is stored as two CSR arcs sharing one handle, so node-keyed
  BFS cannot find it; the shipped behaviour is *safe* (under-reports, never emits
  an edge-reusing/invalid cycle) but incomplete.
- **#1786** — WHERE whole-path predicate evaluated *during* the shortestPath
  search (exhaustive fallback).
- **#1787** — `DISCARD {n}` partial discard with `has_more`.
- **#1784** — server-initiated (tx-timeout) termination should deliver a typed
  Terminated FAILURE before IGNORING (Neo4j semantics).

## Verified sound (negative results)

VLE bounds + relationship-uniqueness within a path; named-path functions; dense-
graph VLE caps; OPTIONAL MATCH null-propagation; MATCH cartesian product;
WITH-scoping barrier; UNWIND×MATCH. Johnson APSP vs Floyd-Warshall (signed
edges); Yen ties/parallel edges; Brandes directed/disconnected/weighted; Dinic /
Edmonds-Karp / push-relabel vs reference (max-flow=min-cut); Stoer-Wagner vs
brute force; Hopcroft-Karp / Hungarian; WCC/SCC vs union-find; triangle / k-core;
extern BFS+PageRank vs in-core. WAL/snapshot/csrfile corruption fail-stops
(mid-stream CRC, frame-too-large, snapshot component CRC, .bak promotion,
group-commit poison) — all sound except the #1778 length-field gap. Bolt
state-machine safety (no crash/deadlock/leak; write not applied in FAILED;
RESET/RUN-in-streaming/COMMIT-no-tx/nested-BEGIN all correct).
