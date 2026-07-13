# Example 22 — Cypher engine

## What it demonstrates

The GoGraph Cypher engine — the module's flagship, 100% compliant with the
openCypher TCK at the execution level — driven over a realistic, seeded social
graph across three families of Cypher work:

- **Reads** — a label scan with a property projection and `ORDER BY ... LIMIT`,
  a `WHERE` filter passed a parameter, and a directed relationship pattern (with
  a bound-relationship read of a date property).
- **Traversal** — `shortestPath` and `allShortestPaths` over the undirected
  `KNOWS` relation between two seeded users, cross-checked against an
  independent in-Go oracle (see below).
- **Writes** — the mutation surface via `Engine.RunInTx`, each effect verified
  by a follow-up read: a multi-pattern `CREATE`, an `UNWIND` batch create driven
  by a list-of-maps parameter, `MERGE` with `ON CREATE` / `ON MATCH` run twice
  to prove idempotency, `SET` and `REMOVE` of a property, and `DELETE` of a
  relationship followed by `DETACH DELETE` of a node.

Every value is read back from the result record and rendered in human-readable
form (names, ages, dates), never as a raw node ID.

## Domain / scenario

A small social network produced by a **seeded generator**. Each `:USER` node
carries a unique hex `id`, a unique `name`, an `age` (drawn from 18–80), and a
`city`. A directed `KNOWS` relationship connects users:

```
(:USER {name, age, city}) -[:KNOWS {since}]-> (:USER)
```

Every user is given a random `KNOWS` out-degree in `[knows-min, knows-max]` to
distinct other users (no self-loops, no duplicate targets). Each `KNOWS` carries
a mandatory `since` date, stored as an ISO-8601 (`YYYY-MM-DD`) string so the
engine reads it back as a non-null value that sorts chronologically under
`ORDER BY`. The dates are drawn from the seeded RNG anchored to a fixed
reference date, so the whole dataset is reproducible for a fixed `-seed`.

### The traversal cross-check

`shortestPath` and `allShortestPaths` are exercised over the *undirected* KNOWS
relation between the anchor user (`userIDs[0]`) and the farthest user reachable
from it. To catch any engine error, the result is cross-checked against an
**independent oracle** built from the same edges: the KNOWS edges are mirrored
into an undirected `graph/csr.CSR`, a hand-written BFS picks the destination and
its distance, and `search.BiBFS` confirms it on that pair. If the engine's
`shortestPath` length disagrees with the oracle, the run fails and reports a
module bug; agreement is asserted as the fact `sp.len_matches_bibfs=1`.

### The write battery

The writes run after the reads, so every read observes the pristine seeded
graph. Because each run builds a fresh graph, every write's effect is a fixed
fact: the multi-pattern `CREATE` adds two users and two edges, the `UNWIND` adds
`-batch` users, the first `MERGE` adds one user (`ON CREATE`) and the second
adds none (`ON MATCH`), and the `DELETE` / `DETACH DELETE` remove one
relationship and one node (with that node's remaining edge). The net KNOWS count
returns to the build total, since the writes add two edges and later remove two.

## How to run

```sh
go run ./examples/22_cypher                              # small deterministic default
go run ./examples/22_cypher -users 200000 -knows-max 30 -seed 7  # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Representative large value |
|---|---|---|---|
| `-users` | number of `:USER` nodes | `50` | `200000` |
| `-knows-min` | minimum `KNOWS` out-degree per user | `3` | `10` |
| `-knows-max` | maximum `KNOWS` out-degree per user | `6` | `30` |
| `-min-age` | `WHERE` threshold: keep users with `age` greater than this | `30` | `30` |
| `-top` | row count for the oldest-users `ORDER BY ... LIMIT` query | `5` | `20` |
| `-batch` | number of `:USER` rows the `UNWIND` write creates | `3` | `1000` |
| `-max-hops` | upper bound `N` for the `shortestPath` varlen pattern (`KNOWS*..N`) | `10` | `10` |
| `-seed` | RNG seed (fixes the data shape) | `1` | `7` |

The default is instant and deterministic — its facts are pinned by the
regression test. Scaling `-users` up is where the per-query latency and
live-heap telemetry become observable.

## Expected output

At the default config the deterministic **fact** lines are:

```
config.users=50
config.knows=[3,6]
config.min_age=30
config.top=5
config.seed=1
nodes.users=50
edges.knows=223
q.oldest_users.rows=5
q.older_than=41
q.knows_count=223
q.knows_sample.rows=1
sp.len=3
sp.len_matches_bibfs=1
asp.count=7
asp.all_min_length=1
create.node_delta=2
create.edge_delta=2
unwind.created=3
merge.created=1
merge.created_second_pass=0
merge.matched_second_pass=1
set.updated=1
remove.cleared=1
delete.rel_removed=1
delete.nodes_kept=1
detach.node_removed=1
detach.rel_removed=1
users.final=55
knows.final=223
```

Interleaved with the facts are `# `-prefixed **telemetry** lines that vary
per run and per machine, for example:

```
# build.elapsed=777µs
# mem.heap_alloc=501.17 KiB
# q.oldest_users.sample="Olivia Gonzalez #22" age 80
# q.knows_sample="Camila Lopez #0" KNOWS "David Harris #3" since 2019-05-11
# sp.oracle_bfs_dist=3 bibfs_hops=3
# sp.latency=418µs
# write.merge_tag_after_create=created
# write.merge_tag_after_match=matched
# write.set_age=77
```

The regression test asserts the bare fact lines and ignores every `# ` line.
`edges.knows`, `q.older_than`, `sp.len`, `asp.count`, and the sample rows are
determined by the `-seed`; with a different seed the totals and the rendered
sample change but the invariants hold: `q.knows_count == edges.knows`,
`sp.len_matches_bibfs == 1`, `asp.all_min_length == 1`, the write deltas are
fixed, and `knows.final == edges.knows`.

## Evidence it collects

This is a Cypher example, so it reports the evidence the standard's taxonomy
lists for the `cypher` subject: **per-query latency** (one `# ` line per query,
including `# sp.latency` / `# asp.latency` for the traversals and
`# write.elapsed` for the write battery) and **live heap** (`# mem.heap_alloc`,
read after a forced GC so it reflects reachable bytes). It also collects a
**correctness** signal that the taxonomy encourages beyond a bare check: the
`shortestPath` length is cross-checked against an independent `search.BiBFS`
oracle over the same edges, so a divergence between two GoGraph subsystems
surfaces as a failed run rather than a silent wrong answer. Scale `-users` up
and the latency lines show how each query class — label scan, `WHERE` filter,
relationship pattern, bidirectional path search, and the write transactions —
responds as the graph grows, while the heap figure tracks the in-memory
footprint. (The traversal cross-check builds a transient undirected mirror of
the KNOWS edges; it is released after the check.)

## Key APIs

- `graph/lpg.New` / `Graph.AddNode` / `SetNodeLabel` / `SetNodeProperty` /
  `AddEdgeLabeled` / `SetEdgeProperty` — build the labelled property graph.
- `cypher.NewEngine` — bind the graph to a query engine.
- `cypher.Engine.Run` — execute a read query (the scan, filter,
  relationship-pattern, `shortestPath` and `allShortestPaths` queries).
- `cypher.Engine.RunInTx` — execute each write (multi-pattern `CREATE`,
  `UNWIND`, `MERGE`, `SET`, `REMOVE`, `DELETE`, `DETACH DELETE`) atomically.
- `cypher.Result` — forward-only streaming result set (`Next`, `Record`, `Err`,
  `Close`).
- `cypher/expr.StringValue` / `expr.IntegerValue` / `expr.ListValue` /
  `expr.MapValue` — the runtime value types passed as parameters (the `UNWIND`
  list-of-maps and the `MERGE` scalars) and returned in result records.
- `graph/adjlist.New` (undirected) / `graph/csr.BuildFromAdjList` /
  `search.BiBFS` — build the independent undirected oracle and search it to
  cross-check the engine's `shortestPath` length.

## Further reading

- [`cypher`](../../cypher) — the Cypher engine package documentation
- [`cypher/expr`](../../cypher/expr) — the runtime value model returned in result records
- [`graph/lpg`](../../graph/lpg) — the labelled property graph used as the query surface
- [`graph/csr`](../../graph/csr) — the immutable CSR the traversal oracle is built on
- [`search`](../../search) — `BiBFS` and the other traversal algorithms
- [Example 24 — social network CLI](../24_social_network_cli) — an interactive CLI running Cypher over a persistent LPG
- [Example 25 — software house API](../25_software_house_api) — a REST API serving Cypher queries
- [Example 26 — social scale benchmark](../26_social_scale_bench) — the same social subject at full scale, the examples-standard reference
- [docs/cypher.md](../../docs/cypher.md) — the Cypher engine design note
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
