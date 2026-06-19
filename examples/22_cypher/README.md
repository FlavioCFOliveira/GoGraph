# Example 22 — Cypher engine

## What it demonstrates

The GoGraph Cypher engine — the module's flagship, 100% compliant with the
openCypher TCK at the execution level — driven over a realistic, seeded
social graph. It exercises four Cypher idioms: a label scan with a property
projection and `ORDER BY ... LIMIT`, a `WHERE` filter passed a parameter, a
directed relationship pattern (with a bound-relationship read of a date
property), and a `CREATE` inside a write transaction whose effect is
verified by a follow-up read. Every value is read back from the result
record and rendered in human-readable form (names, ages, dates), never as a
raw node ID.

## Domain / scenario

A small social network produced by a **seeded generator**. Each `:USER`
node carries a unique hex `id`, a unique `name`, an `age` (drawn from
18–80), and a `city`. A directed `KNOWS` relationship connects users:

```
(:USER {name, age, city}) -[:KNOWS {since}]-> (:USER)
```

Every user is given a random `KNOWS` out-degree in `[knows-min, knows-max]`
to distinct other users (no self-loops, no duplicate targets). Each `KNOWS`
carries a mandatory `since` date, stored as an ISO-8601 (`YYYY-MM-DD`)
string so the engine reads it back as a non-null value that sorts
chronologically under `ORDER BY`. The dates are drawn from the seeded RNG
anchored to a fixed reference date, so the whole dataset is reproducible
for a fixed `-seed`.

The `CREATE` adds one new `:USER` in its own transaction. Because each run
builds a fresh graph, the example reads the `:USER` count immediately
before and after the write and reports the delta, which is always exactly
one.

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
q.users_before_create=50
q.users_after_create=51
create.user_delta=1
```

Interleaved with the facts are `# `-prefixed **telemetry** lines that vary
per run and per machine, for example:

```
# build.elapsed=578µs
# mem.heap_alloc=537.23 KiB
# q.oldest_users.latency=4.037ms
# q.oldest_users.sample="Olivia Gonzalez #22" age 80
# q.knows_sample="Camila Lopez #0" KNOWS "Liam Taylor #30" since 2019-05-11
# q.create.latency=426µs
```

The regression test asserts the bare fact lines and ignores every `# `
line. `edges.knows`, `q.older_than`, and the sample rows are determined by
the `-seed`; with a different seed the totals and the rendered sample
change but the invariants (e.g. `q.knows_count == edges.knows`,
`create.user_delta == 1`) hold.

## Evidence it collects

This is a Cypher example, so it reports the evidence the standard's
taxonomy lists for the `cypher` subject: **per-query latency** (one `# `
line per query) and **live heap** (`# mem.heap_alloc`, read after a forced
GC so it reflects reachable bytes). Scale `-users` up and the latency lines
show how each query class — label scan, `WHERE` filter, relationship
pattern, anchored point read, and the write transaction — responds as the
graph grows, while the heap figure tracks the in-memory footprint.

## Key APIs

- `graph/lpg.New` / `Graph.AddNode` / `SetNodeLabel` / `SetNodeProperty` /
  `AddEdgeLabeled` / `SetEdgeProperty` — build the labelled property graph
  (the labelled-edge insert lands the relationship type in O(1)-amortised).
- `cypher.NewEngine` — bind the graph to a query engine.
- `cypher.Engine.Run` — execute a read query (the scan, filter, and
  relationship-pattern queries).
- `cypher.Engine.RunInTx` — execute the `CREATE` write transaction
  atomically.
- `cypher.Result` — forward-only streaming result set (`Next`, `Record`,
  `Err`, `Close`).
- `cypher/expr.StringValue` / `expr.IntegerValue` — the runtime value types
  the engine returns for projected `string` and integer properties; the
  example unwraps them to bare Go `string` / `int64` for printing.

## Further reading

- [`cypher`](../../cypher) — the Cypher engine package documentation
- [`cypher/expr`](../../cypher/expr) — the runtime value model returned in result records
- [`graph/lpg`](../../graph/lpg) — the labelled property graph used as the query surface
- [Example 24 — social network CLI](../24_social_network_cli) — an interactive CLI running Cypher over a persistent LPG
- [Example 25 — software house API](../25_software_house_api) — a REST API serving Cypher queries
- [Example 26 — social scale benchmark](../26_social_scale_bench) — the same social subject at full scale, the examples-standard reference
- [docs/cypher.md](../../docs/cypher.md) — the Cypher engine design note
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
```
