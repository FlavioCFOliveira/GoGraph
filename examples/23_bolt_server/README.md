# Example 23 — Bolt v5 server round-trip

## What it demonstrates

GoGraph speaking the Bolt v5 wire protocol end to end: it starts the
embedded `bolt/server` over an in-memory graph, connects the official
`neo4j-go-driver/v5` as a real client, drives a battery of Cypher
queries over driver sessions, and shuts the server down cleanly with no
goroutine left behind. Because it puts the wire path under load, it also
serves as a Bolt-throughput benchmark, reporting query throughput, a
p50/p95/p99 latency distribution, and live heap.

## Domain / scenario

A directed social network. Each `:Person` node carries an `id` (a
24-char hex string) and a `name`. Every person is given a random
out-degree in `[knows-min, knows-max]` to distinct other people through
`:KNOWS` edges, and every `:KNOWS` edge carries a mandatory `since` date.
The dates are stored as ISO-8601 (`YYYY-MM-DD`) strings drawn from the
seeded RNG and anchored to a fixed reference date, so they are
reproducible for a given `-seed` and the engine reads them back as
non-null, chronologically sortable values.

The graph is seeded in process through the property-graph API before the
server starts. A pool of Bolt client sessions then fires the fixed query
`MATCH (n:Person) RETURN count(n) AS c` repeatedly over the wire,
verifying each response and timing it.

The listener binds to `127.0.0.1:0`, so the kernel assigns a free port
(a test run never collides on a fixed port); the client discovers it from
`ln.Addr()`. The server is fully compatible with `neo4j-go-driver/v5`
and `cypher-shell`.

## How to run

```sh
go run ./examples/23_bolt_server                                          # small deterministic default
go run ./examples/23_bolt_server -nodes 200000 -queries 50000 -sessions 16 -seed 7  # observable-scale run
```

## Scale and flags

| Flag | Meaning | Default | Representative large value |
|---|---|---|---|
| `-nodes` | number of `:Person` nodes to seed | `2000` | `200000` |
| `-knows-min` | minimum `:KNOWS` out-degree per person | `5` | `20` |
| `-knows-max` | maximum `:KNOWS` out-degree per person | `8` | `50` |
| `-queries` | number of read queries to fire over the wire | `2000` | `50000` |
| `-sessions` | number of concurrent driver sessions | `4` | `16` |
| `-seed` | RNG seed (fixes the deterministic data shape) | `42` | any `int64` |

The default is small enough to seed, serve, and query in well under a
second, so the regression test stays comfortably within the short-layer
60 s package budget. Scale up the dataset, the query count, and the
session count to make the Bolt path's throughput and latency tail
observable.

## Expected output

The deterministic facts — the seeded node count, the fixed query's result
over that data, the number of queries that succeeded, and the
seed-stable edge total — are reproducible for a fixed `-seed`. The
`# `-prefixed telemetry lines (throughput, latency percentiles, heap)
vary per run and per machine and are never pinned.

```
config.nodes=2000
config.knows=[5,8]
config.queries=2000
config.sessions=4
config.seed=42
nodes.person=2000
edges.knows=13012
q.count_person=2000
queries.ok=2000
# load.elapsed=856ms
# load.throughput=2336 queries/s
# load.latency_p50=1.671ms
# load.latency_p95=2.304ms
# load.latency_p99=2.77ms
# mem.heap_alloc=1001.59 KiB
# server shut down cleanly
```

`edges.knows` is the realised sum of the random per-person out-degrees:
it is fixed for `-seed 42` but changes with a different seed. The five
`# load.*` lines and `# mem.heap_alloc` are illustrative — every run
prints different timings, throughput, and heap.

## Evidence it collects

For the Bolt/Cypher subject, the example reports:

- **Query throughput** (`# load.throughput`) — successful queries per
  second across all sessions.
- **Latency distribution** (`# load.latency_p50/p95/p99`) — the per-query
  round-trip latency tail over the whole load.
- **Live heap** (`# mem.heap_alloc`) — the resident Go heap after a forced
  GC.

Scale `-nodes`, `-queries`, and `-sessions` up and watch how throughput
and the latency tail respond to more data and more concurrent sessions —
that is where the wire path's behaviour becomes interesting.

## Key APIs

- `bolt/server.NewServer` / `Server.Serve` / `Server.Shutdown` — start the Bolt v5 TCP server on a listener and tear it down gracefully, draining every per-connection goroutine.
- `bolt/server.Options` — bound concurrent connections (`MaxConnections`) and set the per-connection idle deadline (`ConnTimeout`).
- `cypher.NewEngine` — build the query engine over the in-memory graph the server serves.
- `graph/lpg.New` / `Graph.AddEdgeLabeled` / `Graph.SetEdgeProperty` — seed the labelled property graph with `:Person` nodes and dated `:KNOWS` edges.
- `github.com/neo4j/neo4j-go-driver/v5/neo4j` — the official Bolt client: `NewDriverWithContext`, `VerifyConnectivity`, `NewSession`, `Session.Run`, `Result.Single`, and clean `Close` on teardown.

## Further reading

- [`bolt/server`](../../bolt/server) — the Bolt v5 server package documentation
- [`bolt/server/example_test.go`](../../bolt/server/example_test.go) — the in-repo precedent this example mirrors for the assertion-based round-trip and no-leak teardown
- [`cypher`](../../cypher) — the Cypher query engine the server executes against
- [Example 22 — Cypher engine](../22_cypher) — running Cypher in process without the Bolt wire layer
- [Example 26 — social scale bench](../26_social_scale_bench) — the reference example this one is brought up to
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
