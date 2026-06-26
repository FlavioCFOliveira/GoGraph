# Reliability Audit — 2026-06-26 (round 4)

A fourth empirical whole-module reliability pass, covering every component **and
the seams where components interconnect**, at HEAD `25524c0`. Rounds 1–3 went
deep on Cypher pattern-matching, the Bolt state machine, storage
recovery-under-corruption, and analytics adversarial correctness; this round
deliberately pushed into fresher territory: Cypher behaviour *outside* TCK
coverage, IO round-trip fidelity, the lpg↔CSR snapshot seam, cross-component
concurrency lifecycle, and the combined snapshot+WAL durability path.

## Method (empirical, evidence over assumption)

| Gate | Result |
|---|---|
| `go build ./...`, `go vet ./...`, `gofmt` | clean |
| `go test -race ./...` (whole module, 102 packages) | green (exit 0) |
| openCypher TCK | 3897, held |
| DST full-stack crash-storm (snapshot+WAL, aggressive checkpoint, 12 seeds) | all pass, ACID-D oracle held |
| Crash-injection battery (SIGKILL truncate-boundary windows) | all pass |

Five specialist clusters ran concurrently, each **read-only** and each required
to **reproduce** a finding before reporting it: storage-engine-auditor
(store/wal/checkpoint/recovery/snapshot/csrfile/bulk/txn + interconnection),
cypher-expert (exec/sema/expr/funcs/procs outside TCK), concurrency-architect
(bolt↔engine↔store seams, goroutine lifecycle, lock ordering), graph-theory-expert
(graph core + search + lpg→CSR seam), go-developer (IO round-trip/malformed-input
+ internal packages + examples). The two Critical findings were then
independently re-confirmed by the main agent.

## Findings → rmp sprint 250 (8 tasks)

| # | Sev | Finding | Location |
|---|---|---|---|
| 1788 | **Critical** | `[N*M]` inside a list literal / comprehension silently **negates** the RHS integer (`[2*10]`→`[-20]`; `[x IN [1,2,3] \| x*10]`→`[-10,-20,-30]`). Trigger: `*` adjacent to an integer literal, no space, inside `[...]`. | `cypher/parser/normalize.go` `normalizeVarlenBounds` |
| 1789 | **Critical** | ORDER BY / `min()` / `max()` order Integer and Float in **separate type buckets** instead of one Number tier (`ORDER BY` on `[1,1.5,2]`→`1.5,1,2`; `max(10.5,3)`→`3`). Violates CIP2016-06-14. | `cypher/expr/value.go` `kindOrder`/`Compare` |
| 1790 | Medium | Tombstone-only `RemoveNode` leaves **ghost edges** in any CSR built for search (Dijkstra traverses a deleted node). Masked in the Cypher flow (strips edges first); exposed to direct Go-API callers. | `graph/lpg/lpg.go:1347`, `graph/csr/csr.go:50` |
| 1791 | High | GraphML import **aborts** (or silently degrades types) when a property key holds heterogeneous kinds across nodes (one `attr.type` per key name). JSONL immune. | `graph/io/graphml/writer_props.go:269,527` |
| 1792 | High | **Exponential blowup** serializing a nested-list property — the writer re-JSON-escapes the whole accumulated blob per level (~4×/level); depth 200 does not finish in 120 s. Export-path OOM/hang. | `graph/io/jsonl/writer.go:268`, `graph/io/graphml/writer_props.go:137` |
| 1793 | High | Node/edge **labels silently dropped** on every export format; export→import loses every label (round-trip Consistency gap). | `graph/io/{jsonl,graphml,csv}` writers |
| 1794 | Low | `'a' + 1` returns `null` (string+number) — defensible but undocumented/unpinned divergence from Neo4j. | `cypher/expr/eval.go` `evalArith` |
| 1795 | Low | `ParallelScan` closer goroutine not joined in `Close()` — benign and in unwired dead code; harden if ever promoted. | `cypher/exec/parallel.go:156,219` |

Each task carries an empirical reproduction, the exact responsible file:line, a
focused fix direction, and a mandated regression gate. Tasks #1791, #1793, #1794
also flag a small design decision to be surfaced to the user (per the
decision-autonomy rule) before implementation.

## Certified sound (negative results)

- **Storage stack & interconnection — CERTIFIED, zero findings.** Checkpoint
  phase-3 self-sufficiency re-check; watermark/suffix truncation boundary
  (200 concurrent commits); snapshot publish crash-atomicity (archive→publish→
  parent-fsync→drop-bak); recovery when snapshot+WAL disagree (chronological
  last-writer-wins); DROP INDEX during lock-free phase-2 (no resurrection);
  OpenFS/SimDisk fsync seam faithful to the `os` path; recovery resource bounds;
  #1778 corrupt-length masquerade fix holds. Bulk loader is isolated from the
  transactional WAL by construction.
- **Concurrency seams — zero new findings.** bolt/server goroutine lifecycle
  (once-guarded teardown, reader join); checkpoint-vs-reader/writer interleavings;
  phase-2 unbarriered label/property walk (per-shard RLock); lock ordering
  (commit-lock → visMu → per-shard, unidirectional); lock-free registry reads;
  single-threaded session / tx-timeout reaper; bounded resources & backpressure;
  registration-ordered index backfill atomicity; CertReloader.Watch.
- **Graph core & search — no algorithmic defect.** CSR fidelity after
  `RemoveEdge`; undirected weight symmetry & single-arc self-loops; Bellman-Ford
  / SPFA negative-cycle (incl. negative self-loop, unreachable cycle);
  Floyd-Warshall negative-cycle; A* inadmissible-heuristic contract; k-shortest
  with k > available; Dijkstra/Brandes over parallel edges & self-loops;
  topological sort; WCC on disconnected input; btree (numeric range, duplicate
  keys, NaN), hash index; deterministic seed-stable generators (RMAT/ER/WS/BA).
  `CSR.IsSymmetric()` is a *set* predicate (correct for its only consumer, BiBFS).
- **IO — verified-sound negatives.** JSONL heterogeneous-key round-trip; isolated
  nodes; self-loops + parallel edges; empty-string properties; non-finite floats
  via GraphML; context cancellation; malformed/oversized input (typed errors, no
  panic/hang/unbounded alloc); metrics counters monotone; Prometheus name
  sanitisation. (lpg has no map property kind — map round-trip N/A.)
- **Cypher outside TCK — broad conformance confirmed.** Three-valued (NULL)
  logic across operators/membership/aggregation; integer-overflow fail-stop;
  float ÷0 (Inf/NaN) and integer ÷0 (raises); modulo sign; `toInteger`/`toFloat`
  out-of-range; `round` half-away-from-zero; string fns (substring/split/replace/
  left/right/size); regex `=~`; `range()` (zero-step error); list indexing/slicing/
  concat; `reduce`/comprehension; aggregation null handling; `percentileCont/Disc`
  bounds; `stDev` n=1; SKIP/LIMIT validation; min/max across *genuinely* mixed
  kinds (only the within-Number tier, #1789, is wrong).

## Remediation — all 8 findings fixed

Every finding in sprint 250 was implemented, each with the regression gate named
in its task. The three IO findings carried a wire-format design decision, each
resolved with the user before implementation.

| # | Fix | Commit |
|---|---|---|
| 1788 | `normalizeVarlenBounds` scopes the `*N`→`*-N` rewrite to relationship-detail brackets (mirrors `normalizeVarlenDotDot`); list literals/comprehensions no longer negate products | `204954c` |
| 1789 | `Compare` treats Integer+Float as one Number tier via an exact `int64`↔`float64` comparison (≥2⁵³ precision, NaN/Inf); two stale tests corrected | `c9e2ed0` |
| 1790 | `csr.BuildFromAdjListLive(adj, live)` + `lpg.Graph.LiveNodeFilter` (nil ⇒ fast path); cypher seam wired defensively; `BuildFromAdjList` documented tombstone-agnostic | `8d70751` |
| 1792 | JSONL/GraphML encoders fail fast (`ErrPropertyNestingTooDeep`/`ErrPropertyValueTooLarge`) instead of OOM/hang on deeply-nested lists | `a574e94` |
| 1791 | GraphML emits one `<key>` per (name, kind); heterogeneous keys round-trip; homogeneous output byte-stable (user: full fidelity) | `0b6f966` |
| 1793 | JSONL+GraphML carry node labels (user: carry labels); reader restores via `SetNodeLabel`; label-less output byte-stable; CSV/DOT documented | `7302942` |
| 1794 | `'a' + 1` → `null` documented + pinned (user: keep null) | `6e169a1` |
| 1795 | `ParallelScan.Close` joins the closer goroutine (`closerWG`); strict no-leak gate | `6d89f60` |

After remediation: build, vet, gofmt, the openCypher TCK (3897), and
`go test -race ./...` are all green. No compliance mandate is regressed.
