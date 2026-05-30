# Example 15 — Task assignment

## What it demonstrates

Two bipartite assignment algorithms side by side, and how their answers
relate. `search.Hungarian` finds the globally cheapest one-to-one
assignment over a full cost matrix; `search.HopcroftKarp` finds the
largest matching once a "willing to take" business rule prunes the
edges. The example prints both and a comparison that ties them together.

## Domain / scenario

Four workers (`alice`, `bob`, `carol`, `dave`) must be staffed onto four
tasks (`task-A` … `task-D`). Each worker has a cost for each task (lower
is better):

```
        task-A task-B task-C task-D
alice      8      4      7      5
bob        6      9      5      6
carol      5      3      8      7
dave       7      6      4      9
```

A **business rule** connects the two algorithms: a worker only accepts a
task whose cost to them is at most `willingCostThreshold` (6); a costlier
task is refused regardless of how cheap it is for the employer. The
Hungarian assignment is computed over the *full* matrix and may use a
pair a worker would refuse; the Hopcroft-Karp matching is computed over
*only* the willing pairs, so it shows how many workers can still be
staffed once refusals are honoured.

For this matrix the cheapest assignment (total 18) happens to lie
entirely inside the willing set and covers all four workers, so the rule
is not binding — but the Hopcroft-Karp matching is a *different*
assignment (total 21), because it maximises coverage and ignores cost.

## How to run

```sh
go run ./examples/15_task_assignment
```

## Expected output

```
=== Minimum-cost assignment (Hungarian) ===
  alice   -> task-D  (cost 5, willing)
  bob     -> task-A  (cost 6, willing)
  carol   -> task-B  (cost 3, willing)
  dave    -> task-C  (cost 4, willing)
  total = 18

=== Willing set (worker accepts task when cost <= 6) ===
  alice   willing: task-B(4) task-D(5)
          refuses: task-A(8) task-C(7)
  bob     willing: task-A(6) task-C(5) task-D(6)
          refuses: task-B(9)
  carol   willing: task-A(5) task-B(3)
          refuses: task-C(8) task-D(7)
  dave    willing: task-B(6) task-C(4)
          refuses: task-A(7) task-D(9)

=== Maximum willing matching (Hopcroft-Karp) ===
  alice   -> task-D  (cost 5)
  bob     -> task-C  (cost 5)
  carol   -> task-A  (cost 5)
  dave    -> task-B  (cost 6)
  matched pairs: 4 of 4 workers

=== Comparison ===
  Hungarian: all 4 pairs are within the willing set (total cost 18).
  Hopcroft-Karp: 4 of 4 workers can be staffed using willing pairs only (total cost 21).
  Verdict: the willingness rule is not binding here — the cheapest
  assignment is already fully willing and every worker stays staffed,
  so cost-optimality (18) and full coverage (4/4) are achievable together.
```

## Key APIs

- `search.Hungarian` — minimum-cost rectangular assignment (Jonker-Volgenant / Kuhn-Munkres) over a row-major `float64` cost matrix.
- `search.Assignment.RowToCol` / `search.Assignment.TotalCost` — read back which task each worker was assigned and the optimal total cost.
- `search.HopcroftKarp` — maximum-cardinality matching on a bipartite graph in `O(E·√V)`.
- `search.Matching.MatchL` / `search.Matching.Size` — read back which right vertex each left vertex matched and the matching size.
- `graph/adjlist.New` / `AdjList.AddEdge` — build the willing-pairs bipartite graph (edge only where the worker accepts the task).
- `graph/adjlist.AdjList.Mapper` — resolve the sparse, hash-derived `NodeID`s in the matching back to worker and task indices (`Lookup`, `Resolve`).
- `graph/csr.BuildFromAdjList` — freeze the bipartite graph into the immutable CSR snapshot that `HopcroftKarp` consumes.

## Further reading

- [`search`](../../search) — traversal, path-finding, and matching package documentation
- [`graph/csr`](../../graph/csr) — the immutable CSR snapshot used as the matching surface
- [`graph/adjlist`](../../graph/adjlist) — the mutable adjacency-list builder and its mapper
- [Example 01 — basic shortest paths](../01_basic) — the minimal build → snapshot → query flow this example extends
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
