# Reliability Audit — 2026-06-25

A full empirical reliability audit of GoGraph: every component individually, the
seams between components, and the module as a whole. The goal was to find
**reliability fragilities** — places where a documented invariant (100%
openCypher TCK conformance, 100% ACID, crash-safety, no data races, no leaks,
bounded resources) could be broken — using concrete, reproducible evidence
rather than inspection alone.

## Method (empirical)

Global gates, all run and observed green:

| Gate | Result |
|---|---|
| `go build ./...` | green |
| `go test ./...` (short) | green |
| `go test -race ./...` (102 packages) | **green — zero races** |
| Fuzz smoke (20 s each): `FuzzDecodeValue`, `FuzzCSRFileReader`, `FuzzParse`, `FuzzCSVReader`, `FuzzGraphMLReader` | **no crashers** |
| DST swarm + crash injection (`cmd/sim -swarm -crashes -bias`, 359 seeds, 20 scenario buckets) | **359/359 pass, 0 failures** |

Five specialist cluster audits then read the code on the relevant paths and
proved or refuted each suspicion with a throwaway reproducer (run, captured,
deleted — the working tree was left clean):

1. **Storage / persistence** (`store/*`, WAL, recovery, checkpoint, snapshot) — durability & atomicity.
2. **Cypher engine** (`cypher/*`, TCK) — conformance & consistency.
3. **Concurrency & interconnections** (txn engine, isolation, registries, Bolt server) — isolation, leaks, bounds.
4. **Graph core & algorithms** (`graph/*`, `search/*`) — algorithmic correctness & edge cases.
5. **Bolt protocol & IO adapters & error discipline** — robustness against hostile bytes; swallowed errors.

TCK independently re-verified: `go test ./cypher/tck/ -run TestTCK` → `ok`, with
`tckExecutionBaseline = 3897` enforced and zero failed/undefined/pending.

## Findings → rmp sprint 242

Nine verified findings, each tracked with an empirical reproduction and a
regression gate. No Critical (active data-loss / live ACID breach in shipped
code paths) was found; the two High durability gaps are reachable only through
the public `txn.Store` API, not by any code shipped in the repo today, so they
are latent API-contract traps.

| # | Sev | Finding | Location |
|---|---|---|---|
| 1755 | High | `CREATE INDEX` definition lost across a checkpoint (txn.Store path) — index def lives only in the truncated WAL frame | `store/recovery/recovery.go:945`, `store/checkpoint/checkpoint.go:760`, `store/snapshot/indexes.go:60` |
| 1756 | High | `CREATE CONSTRAINT` lost across a checkpoint for txn.Store-direct embedders — fail-safe `HasConstraints()` only driven by the engine | `store/checkpoint/checkpoint.go:676`, `graph/lpg/lpg.go:1405`, `store/txn/txn.go:1004` |
| 1757 | High | `count(DISTINCT)`/`collect(DISTINCT)` dedup by comparability not equivalence → wrong on `NaN` and nested-null | `cypher/api.go:6398` |
| 1758 | High | Leiden & Label Propagation silently ignore edge weights → wrong partition on weighted input | `search/community/leiden.go:289`, `label_propagation.go:81` |
| 1759 | Medium | `sum()` over empty/all-null returns `null` instead of `0` | `cypher/funcs/aggregators.go:264` |
| 1760 | Medium (latent) | `exec.Run` suppresses `Close()` on `Init` error → `ParallelGovernor` inflight leak + goroutine leak | `cypher/exec/produce_results.go:109` |
| 1761 | Low | Stale `t.Skip` guards mask coverage (Bolt MERGE idempotence untested though implemented; UNION; multi-label REMOVE) | `bolt/server/e2e_merge_idempotence_test.go:55`, `cypher/union_test.go`, `cypher/remove_label_test.go:111` |
| 1762 | Low | `csr.FromArrays` malformed input panics opaque out-of-range instead of a typed error | `graph/csr/csr.go:345,280,409` |
| 1763 | Low | A\* integer `f = g + h` can overflow and mis-order exploration (cost stays correct) | `search/astar.go:164,196` |

### Two decisions deferred to execution time

- **#1758 (Leiden weights):** implement weighted Leiden/LP **or** document that
  weights are ignored. The weight-generic signature currently misleads callers.
- **#1762 / #1763:** harden with a typed error / saturating add **or** pin the
  documented precondition with a test.

## Verified sound (negative results)

Recorded so future audits do not re-tread them:

- **WAL / recovery / snapshot / checkpoint / txn commit** — fail-stop poison,
  physical discard of un-synced suffix, parent-dir fsync, atomic
  copy-suffix-then-rename truncate, TxnSeq-suffix atomicity filter,
  torn-tail-benign vs corruption classification, durable-before-visible commit.
  No defect beyond the two schema-DDL gaps above.
- **Bolt protocol & IO** — PackStream wire-byte budget + 128 MiB decoded-memory
  budget + `MaxInt32` length cap + depth-128 nesting cap; chunk cumulative-size
  bound; handshake Slowloris timeout; XML no entity expansion; complete
  error→status-code mapping with a safe fallback. No crashers in fuzz or crafted
  inputs.
- **Algorithms** — Dijkstra, Bellman-Ford, Floyd-Warshall, Tarjan (iterative, 1 M
  nodes), Kahn, Kruskal/Prim, Dinic, Stoer-Wagner (3000 graphs vs brute force),
  Hungarian, A\* (vs Dijkstra over 200 graphs), Yen, PageRank (dangling mass
  conserved), Brandes — all verified correct on adversarial/edge-case inputs.
  adjlist COW atomic publish, CSR snapshot integrity, B+tree range bounds,
  seeded-generator determinism — all sound.
- **Concurrency** — txn lock order & context-aware blocking, `BeginReadTx`
  lock-free read path, no racy global mutable state, Bolt server goroutine
  lifecycle + tx-timeout reaper + bounded buffers, `ParallelGovernor`
  oversubscription bound. No new data race / leak (beyond the latent #1760).
- **Error discipline** — every `_ =` on a reliability-relevant non-test path is
  a benign best-effort close/rollback/flush or backed by a sticky fail-stop.

## Remediation — all 9 fixed (sprint 242, same day)

Every finding was fixed with a regression gate, preserving TCK 3897 and ACID.

| # | Commit | Gate |
|---|---|---|
| 1757 | `1837a80` | `cypher/aggregate_equivalence_test.go` |
| 1759 | `1837a80` | `cypher/aggregate_equivalence_test.go` |
| 1760 | `ced0a0b` | `cypher/exec/produce_results_initclose_internal_test.go` |
| 1762 | `94b07eb` | `graph/csr/validate_test.go` |
| 1763 | `46c1061` | `search/astar_overflow_test.go` |
| 1761 | `7ff1caf` | Bolt MERGE idempotence now asserted over the wire |
| 1758 | `8039e7f` | `search/community/weight_contract_test.go` (unit-weight contract) |
| 1755 | `f3168a7` | `store/checkpoint/index_survival_test.go`, `indexdefs_survival_test.go` |
| 1756 | `f3168a7` | `store/checkpoint/constraints_storedirect_unwired_test.go` |

For #1758 the unit-weight contract was documented and gated (the `W any`
signature cannot interpret an arbitrary weight numerically; a weighted variant
would be a new API). #1755/#1756 add a self-sufficient `indexdefs.bin` snapshot
component plus a store-direct `HasIndexes`/`HasConstraints` WAL-retention
fail-safe, mirroring the proven `constraints.bin` discipline — storage-engine
certified, back-compatible (no manifest-version bump), `-race` and crash-battery
clean.

## Carry-forward (already tracked, not re-reported)

`#1671/#1670/#1526` (COW lock-free read path, user-deferred); `#1752` (WAL
filesystem seam for SimDisk checkpoint); `#1740/#1741` (DST G9/G10).
