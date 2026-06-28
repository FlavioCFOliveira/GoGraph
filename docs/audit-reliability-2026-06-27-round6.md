# Reliability Audit — 2026-06-27 (round 6, completeness pass)

A **complete, zero-baseline** audit across **all 102 packages** and their
interconnections — the comprehensive sweep the project goal calls for (rounds
4–5 were targeted/iterative). Every package was enumerated and mapped to its
audit coverage; the full per-package test baseline was run; and fresh
specialists were directed at the areas rounds 1–5 never touched: the Cypher
**front-end** (`ast`/`sema`/`ir`/`explain`/`procs`), the **support/infra**
packages (`graph/query`, `graph/lpg/schema`, `ds`, `internal/*`), `cmd/*`, and —
critically — the **fidelity of the test/verification harnesses** themselves (a
buggy oracle gives false confidence).

## Method (empirical)

| Gate | Result |
|---|---|
| `go build ./...`, `go vet ./...`, `gofmt` | clean |
| `go test ./...` (all 102 packages) | green (exit 0) |
| `go test -race ./...` | green |
| openCypher TCK | 3897, held before & after every change; error-fidelity ratcheted 121 → 122 |
| DST crash-storm (multi-seed, write/read-heavy + scenarios) | pass, ACID-D held |

The DST oracle was proven **non-vacuous** by mutation testing (a deliberately
wrong engine triggers the expected typed violation; a correct engine produces
none). Three new harness-fidelity gates were added (`internal/sim/round6_*`,
`bolt/server/round6_wire_durability_test.go`).

## Findings → rmp sprint 253 (12 findings)

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1801 | High | `ORDER BY … LIMIT 0` returned ALL rows (Sort+Limit→Top fusion treated limit 0 as no-limit) | fixed `91e5198` |
| 1803 | High | Non-aliased compound grouping key (`RETURN n.a+1, count(*)`) rendered `null` | fixed `bd56df1` (eval-layer; aliased keys untouched) |
| 1802 | High → **not a bug** | claimed `WITH … WHERE <pre-WITH var>` is non-conformant | **TCK proves it conformant** (`WithWhere7 [1]`, `WithWhere1 [2,3,4]`); reverted, closed |
| 1804 | Medium | nested aggregation `count(count(*))` accepted instead of rejected | fixed `f59cfb2` (NestedAggregation; fidelity 121→122) |
| 1805 | Medium | ORDER BY passthrough variable leaked into result columns | fixed `d099d5e` |
| 1806 | Low | aggregation-in-WHERE error message wrongly said "ORDER BY" | fixed `84b1342` |
| 1807 | Low | disconnected MATCH shown as `Apply`, not `CartesianProduct`, in EXPLAIN | fixed `a0a8db7` |
| 1808 | Medium | testfs fabricated phantom durable data after Truncate+sync-fault | fixed `2b51011` |
| 1809 | Low | testfs suffix-only fault model (doc gap) | documented + pinned `3b78984` |
| 1810 | Low | `RequireSoak` ignored the `soakfull`/`stress` tags | fixed `b37a46e` |
| 1811 | Low | integrated sim crash loop never called `disk.Crash()` | fixed `27f15fc` |
| 1812 | Low | dead `checkpoint.mid-truncate` crashpoint (test-only path) | removed `0e726f0` |

Plus a follow-up: the round-5 centrality additions had tripped the
concurrency-doc ratchet (`go test ./...` gate missed in round 5) — fixed
`dacbcac`.

**Key lesson (#1802):** a specialist's spec reading was wrong; the empirical TCK
is the authority and caught the regression (the sema "fix" broke 5 scenarios).
Every Cypher finding was verified against the TCK before and after.

## Coverage map (all 102 packages)

Every package is now covered by an empirical audit across rounds 1–6 — Cypher
(parser/exec/expr/funcs/procs/ast/sema/ir/explain/tck), graph core
(lpg/adjlist/csr/index/generation/query/io), search (centrality/community/flow/
extern), store (wal/checkpoint/recovery/snapshot/csrfile/bulk/txn), bolt
(packstream/proto/server), and the `internal/*` infrastructure (sim/crashinject/
crashpoint/testfs/testlayers/metrics/invariants/clock/shapegen/ds) — plus the
cross-layer interconnection (Bolt → engine → txn → lpg → wal → recovery, end to
end through the wire) and the harness-fidelity tier.

## Status

Build, vet, gofmt, `go test ./...`, `go test -race ./...`, and the openCypher TCK
(3897, error-fidelity 122) are all green. No compliance mandate is regressed.
