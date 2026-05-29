# Performance history

This directory is the empirical record of the module's performance over time.
Every entry is produced by [`scripts/bench-history.sh`](../../../scripts/bench-history.sh)
and is meant to make the gain or regression of each change **measurable and
reviewable**, rather than asserted from memory.

## Workflow

```bash
# 1. Establish (or re-confirm) a baseline on a clean tree.
./scripts/bench-history.sh baseline

# 2. Make one focused change, then measure it.
./scripts/bench-history.sh <kebab-case-label>
```

Each run writes three things:

- `NNNN__<label>__<commit>.txt` — raw `go test -bench -benchmem` output for the
  curated set, `-count=6` by default so benchstat has enough samples.
- `NNNN__<label>__<commit>.delta.txt` — `benchstat` comparing this run against
  the immediately previous run. This is the per-change gain/regression record.
- a row appended to [`LEDGER.md`](LEDGER.md) — the chronological index. After a
  run, replace the row's `_pending_` summary with the headline delta from the
  `.delta.txt`.

A `-dirty` suffix on the commit marks an uncommitted tree (measuring a change
before it is committed). Re-run after committing if you want a clean tag.

## The curated benchmark set

The set is deliberately small and fixed — adding entries lengthens every run.
It spans the layers an optimisation can move so a win in one cannot silently
regress another:

| Group | Benchmarks | Package | Guards |
|-------|-----------|---------|--------|
| Cypher read | IC1, IC2, IC9, IC10 | `bench/cypher_ldbc` | result-materialisation hot path |
| Cypher write | IC5, IC13 | `bench/cypher_ldbc` | CREATE / MERGE write path |
| Operators | Project, ResultSet, AllNodesScan | `bench/cypher_alloc` | per-operator allocation gates |
| Graph algorithms | Dijkstra (Large, PostWarmup), BFSDirectionOpt, Brandes | `search`, `search/centrality` | regression guard band |

## Invariants every change must hold

A performance change is only acceptable when, alongside the measured gain:

- **TCK stays at 100%** — `go test -run TestTCKExecution ./cypher/tck/...`
  reports `3897 scenarios, 3897 passed` (`tckExecutionBaseline = 3897`).
- **ACID holds** — `go test -race ./store/...` (WAL, recovery, checkpoint,
  snapshot, txn) stays green.
- **No data races** — `go test -race ./...` on the touched packages.
- **No graph-algorithm regression** — the guard-band group in the delta shows
  `~` (statistically insignificant) for sec/op, B/op and allocs/op.
