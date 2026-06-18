# Example 26 — Social-network scale benchmark

## What it demonstrates

Building a large labelled property graph that models a social network and
measuring **query performance** and **resource consumption** over it: the
example reports build throughput, Go heap footprint, and the latency of a
battery of representative Cypher queries (label-scan counts, relationship
counts, a friend-of-friend traversal, and a trending-articles
aggregation).

## Domain / scenario

A social network of `USER` and `ARTICLE` nodes:

```
(:USER    {id, name})            // id is a 24-char hex string, name is realistic
(:ARTICLE {id, title})           // id is a 24-char hex string, title is realistic
(:USER)-[:FRIEND]->(:USER)       // friends-min .. friends-max per user
(:USER)-[:LIKE]->(:ARTICLE)      // 0 .. likes-max per user
```

`FRIEND` is a directed out-edge: each user is given a random out-degree in
`[friends-min, friends-max]` to distinct other users (no self-loops, no
duplicate targets). `LIKE` is a directed out-edge to between zero and
`likes-max` distinct articles. The dataset is generated from a seeded RNG,
so its shape is reproducible for a fixed `-seed`; only the telemetry varies
between runs.

The graph is built in memory and queried with an in-memory
`cypher.Engine`. The example deliberately does **not** exercise the
WAL/recovery stack: durably persisting hundreds of millions of edges is
impractical for an example and orthogonal to what this one measures
(persistence is covered by examples 04, 17, 24 and 25).

## How to run

```sh
go run ./examples/26_social_scale_bench
```

With **no flags** the example builds the full specification: **1,000,000
users**, **30,000 articles**, **150–200 friends per user**, and **up to
300 likes per user** — roughly 1.03M nodes and ~3.2 × 10⁸ edges.

> **Resource warning.** At ~187 bytes of live heap per edge (measured),
> the full run needs on the order of **60 GiB of RAM** and several minutes
> to build. Run it only on a machine sized for it. On a laptop, scale down
> first:
>
> ```sh
> go run ./examples/26_social_scale_bench -users 20000 -articles 2000
> ```

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-users` | `1000000` | number of `USER` nodes |
| `-articles` | `30000` | number of `ARTICLE` nodes |
| `-friends-min` | `150` | minimum `FRIEND` out-degree per user |
| `-friends-max` | `200` | maximum `FRIEND` out-degree per user |
| `-likes-max` | `300` | maximum `LIKE` out-degree per user |
| `-seed` | `1` | RNG seed (fixes the deterministic data shape) |

## Expected output

Bare `key=value` lines carry **deterministic** facts (reproducible for a
fixed `-seed`); lines prefixed with `# ` carry **volatile telemetry**
(durations and heap figures that vary per run and per machine). A
representative scaled-down run
(`-users 20000 -articles 2000 -friends-min 150 -friends-max 200 -likes-max 300 -seed 1`):

```
config.users=20000
config.articles=2000
config.friends=[150,200]
config.likes=[0,300]
config.seed=1
nodes.users=20000
nodes.articles=2000
edges.friend=3499345
edges.like=2988203
# build.elapsed=3.889s
# build.node_rate=5657 nodes/s
# build.edge_rate=1668076 edges/s
# mem.heap_alloc=1.13 GiB
# mem.heap_growth=1.13 GiB
# mem.total_alloc=1.95 GiB
# mem.sys=1.44 GiB
# mem.num_gc=14
# bytes_per_edge=186.9
q.count_users=20000
# q.count_users.latency=15.497ms
q.count_articles=2000
# q.count_articles.latency=1.31ms
q.count_friend=3499345
# q.count_friend.latency=7.396601s
q.count_like=2988203
# q.count_like.latency=6.415286s
q.fof_reach=15224
# q.fof_reach.latency=6.078179s
q.top_articles.rows=10
# q.top_articles.latency=11.386062s
```

The `edges.*` totals depend on the seed; `q.count_friend` and `q.count_like`
always equal `edges.friend` and `edges.like` respectively, which is the
core consistency invariant the regression test asserts. The `# `-prefixed
figures (including all latencies) are environment-dependent and are **not**
pinned by the test.

## Key APIs

- `graph/lpg.New` / `Graph.AddNode` / `Graph.SetNodeLabel` / `Graph.SetNodeProperty` — build the labelled property graph in memory.
- `graph/lpg.Graph.AddEdge` / `Graph.SetEdgeLabel` — add typed `FRIEND` / `LIKE` relationships.
- `graph/lpg.StringValue` — wrap string property values.
- `cypher.NewEngine` / `Engine.Run` — query the in-memory graph.
- `cypher.Result.Next` / `Result.Record` / `Result.Err` / `Result.Close` — iterate result rows and read columns.
- `cypher/expr.StringValue` / `expr.IntegerValue` — typed query parameters and result cells.
- `runtime.ReadMemStats` — capture the Go heap footprint of the build.

## Further reading

- [`graph/lpg`](../../graph/lpg) — labelled property graph package
- [`cypher`](../../cypher) — Cypher engine package
- [Example 22 — Cypher](../22_cypher) — the Cypher engine over a small graph
- [Example 11 — Social network](../11_social_network) — analytics over a small social LPG
- [Example 24 — Social-network CLI](../24_social_network_cli) — a persistent social-network store
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
