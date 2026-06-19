# Example 15 — Task assignment

## What it demonstrates

Two bipartite assignment algorithms side by side over **one** seeded,
scale-parametrised worker/task instance, and how their answers relate.
`search.Hungarian` finds the globally cheapest one-to-one assignment over
the full cost matrix; `search.HopcroftKarp` finds the largest matching
once a feasibility rule prunes the edges. The example reports the optimal
total cost and the maximum matching size as deterministic facts, and the
per-algorithm wall-clock and live heap as telemetry.

## Domain / scenario

A skilled-workforce / task-dispatch problem with a **latent competency
model**. Each worker carries a hidden skill vector and each task a hidden
requirement vector, both in `[0,1]^skills`, drawn from the seeded RNG. The
cost of worker *i* taking task *j* is a weighted *under-qualification
deficit* — the worker pays only for the skills a task needs and they lack,
never for surplus skill — plus a little idiosyncratic noise, scaled and
rounded to a clean integer:

```
cost(i,j) = round( SCALE · ( Σ_k w_k · max(0, req_j[k] − skill_i[k]) + ε ) )
```

This low-rank-plus-noise structure makes the optimal assignment
non-trivial: the per-worker cheapest task frequently collides, so resolving
the collisions optimally needs the global Hungarian trade-off rather than a
greedy pick. Costs are integers held as `float64`, so the optimal total
cost is an exact integer the regression test asserts.

A **feasibility rule** connects the two algorithms. The Hopcroft-Karp
graph keeps only the pairs a worker is competent for: edge `(i,j)` survives
when its cost is at or below the `feasible-pct`-th percentile of all costs.
A percentile (not an absolute constant) is self-calibrating — it stays in
the interesting regime as the scale, skill count, or noise change. At the
default ~15% retention the cheap edges cluster on the easy tasks, so by
Hall's theorem the maximum matching falls **strictly short** of
`min(workers, tasks)`: some workers cannot be staffed at all under the
rule, even though Hungarian could assign everyone. That shortfall is the
point the example surfaces.

## How to run

```sh
go run ./examples/15_task_assignment                          # small deterministic default
go run ./examples/15_task_assignment -workers 1000 -tasks 1200 -seed 7   # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Representative large value |
|---|---|---|---|
| `-workers` | number of workers (left partition; must be ≤ tasks) | `200` | `1000` |
| `-tasks` | number of tasks (right partition) | `240` | `1200` |
| `-skills` | latent skill/requirement vector dimensionality | `6` | `10` |
| `-noise-frac` | idiosyncratic-noise magnitude as a fraction of a skill deficit | `0.3` | `0.3` |
| `-feasible-pct` | percentile of costs kept as feasible pairs, in `(0,1]` | `0.15` | `0.30` |
| `-seed` | RNG seed (fixes the deterministic instance shape) | `1` | `7` |

Hungarian requires at least as many tasks as workers (`workers ≤ tasks`),
so its O(V³) cost grows with the larger dimension; a 1000×1200 instance
still solves in well under a second. Raising `-feasible-pct` toward `0.30`
relaxes the rule until the matching becomes perfect (non-binding); lowering
it toward `0.05` collapses the matching.

## Expected output

```
config.workers=200
config.tasks=240
config.skills=6
config.feasible_pct=0.15
config.seed=1
feasible.pairs=7206
hungarian.optimal_cost=161476
matching.size=180
matching.ceiling=200
feasibility.binding=1
# hungarian.elapsed=6.7ms
# matching.elapsed=1.2ms
# mem.heap_alloc=203.54 KiB
# mem.heap_growth=16 B
# mem.total_alloc=2.24 MiB
# mem.num_gc=1
```

The bare lines are deterministic facts pinned by the regression test:
`hungarian.optimal_cost=161476` is the unique minimum total cost, and
`matching.size=180` is the unique maximum matching cardinality — 20 of the
200 workers cannot be staffed under the feasibility rule, so
`feasibility.binding=1`. The `# `-prefixed telemetry varies per run and per
machine and is never pinned.

## Evidence it collects

Following the search/path-finding row of the evidence taxonomy, the
example reports **per-algorithm wall-clock** (`# hungarian.elapsed`,
`# matching.elapsed`) and **live heap and allocation** (`# mem.heap_alloc`,
`# mem.heap_growth`, `# mem.total_alloc`, `# mem.num_gc`). When you scale
it up, watch how Hungarian's O(V³) time grows against the larger dimension
while Hopcroft-Karp's O(E·√V) time tracks the feasible edge count, and how
the matching size relative to `matching.ceiling` moves as `-feasible-pct`
tightens or relaxes.

## Key APIs

- `search.Hungarian` / `search.HungarianCtx` — minimum-cost rectangular assignment (Jonker-Volgenant / Kuhn-Munkres) over a row-major `float64` cost matrix; requires columns ≥ rows.
- `search.Assignment.TotalCost` — the optimal total cost (a sum of integer cost cells, exact in `float64`).
- `search.HopcroftKarp` / `search.HopcroftKarpCtx` — maximum-cardinality matching on a bipartite graph in `O(E·√V)`.
- `search.Matching.Size` — the maximum matching cardinality.
- `graph/adjlist.New` / `AdjList.AddNode` / `AdjList.AddEdge` — build the feasible-pairs bipartite graph (edge only where the worker is competent for the task).
- `graph/csr.BuildFromAdjList` — freeze the bipartite graph into the immutable CSR snapshot that `HopcroftKarp` consumes.

## Further reading

- [`search`](../../search) — traversal, path-finding, and matching package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the matching surface
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list builder
- [Example 01 — basic shortest paths](../01_basic) — the minimal build → snapshot → query flow this example extends
- [Example 26 — social scale bench](../26_social_scale_bench) — the reference end state for the examples standard
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
