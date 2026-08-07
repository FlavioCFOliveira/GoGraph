# Example 31 — Metrics & Observability (Prometheus)

## What it demonstrates

How to turn on GoGraph's observability surface — the metrics the module
mandates on every public blocking API — through the public
`metrics.NewPrometheusRegistry` / `metrics.SetBackend` facade, drive a mixed
workload across subsystems, and scrape the latency histograms and
utilisation counters over an HTTP `/metrics` endpoint in Prometheus
text-exposition format. It is the one example that exercises the
observability axis.

It also surfaces the **relationship count-store**'s bounded observability
metrics — the exact-cardinality statistics the planner maintains on the
write path — alongside the rest: a write burst drives the `delta.applied`
and `relabel.dirtied` counters and the reopen `recompute` histogram, and the
same burst samples write throughput with the store active so its neutrality
to the write path is directly observable.

And it surfaces the **MVCC substrate**, which since sprint 334 is GoGraph's
only concurrency-control mechanism — so its health is the module's health. A
dedicated phase drives four concurrent writers, provokes a deliberate
write-write conflict, and holds a read snapshot open across a write burst so
the version chains have a depth worth reporting. That lights the writer gauge,
the commit and abort counts, the conflict rate and its per-store attribution,
the retained chain-depth distribution, and the background vacuum's own
lifecycle and per-pass latency.

## Domain / scenario

A **service-mesh call graph**: nodes are microservices (`:SERVICE`), edges
are directed RPC dependencies (`:CALLS`) each attributed a synthetic
latency. A seeded generator gives every service a random out-degree in
`[calls-min, calls-max]` to distinct callees, so the topology — and every
deterministic fact below — is reproducible for a fixed `-seed`. The call
graph is materialised into three representations that feed instrumented
APIs: a labelled property graph for Cypher, an int64-weighted adjacency
list (latency in microseconds) for the CSV round-trip, and a CSR (built
from that adjacency list) for Dijkstra. Over the property graph a small
write burst then adds and retires monitored `CALLS` relationships and marks
one service `:DEGRADED` and back, exercising the count-store maintenance
paths so their observability metrics appear in the scrape.

## How to run

```sh
go run ./examples/31_metrics_observability                                  # small deterministic default
go run ./examples/31_metrics_observability -services 200000 -calls-max 12 -seed 7  # observable-scale run
```

## Scale and flags

| Flag         | Meaning                                   | Default | Large example |
|--------------|-------------------------------------------|---------|---------------|
| `-services`  | number of `:SERVICE` nodes                | `200`   | `200000`      |
| `-calls-min` | minimum `CALLS` out-degree per service    | `2`     | `2`           |
| `-calls-max` | maximum `CALLS` out-degree per service    | `6`     | `12`          |
| `-seed`      | RNG seed (fixes the deterministic shape)  | `1`     | `7`           |

The metric names are identical at every scale; only the `# ` telemetry
(observed counts, latency distribution, scrape size) changes with scale.

## Expected output

At the default config the deterministic **fact** lines are:

```
config.services=200
config.calls=[2,6]
config.seed=1
nodes.services=200
edges.calls=786
cypher.services_before=200
cypher.services_after=201
cypher.write_delta=1
dijkstra.src_reached=196
csv.roundtrip.edges_match=1
countstore.cells_positive=1
metric.present.cypher.Run=true
metric.present.cypher.RunInTx=true
metric.present.cypher.plan_cache.misses=true
metric.present.cypher.plan_cache.hits=true
metric.present.search.Dijkstra=true
metric.present.search.DijkstraCtx=true
metric.present.search.pool.dijkstra.get=true
metric.present.search.pool.dijkstra.put=true
metric.present.graph.io.csv.Write=true
metric.present.graph.io.csv.ReadInto=true
metric.present.bolt.pool.encoder.get=true
metric.present.bolt.pool.encoder.put=true
metric.present.cypher.countstore.recompute=true
metric.present.cypher.countstore.delta.applied=true
metric.present.cypher.countstore.relabel.dirtied=true
metric.present.count=15
metric.expected.count=15
```

Followed by `# `-prefixed telemetry that varies per run and per machine,
for example:

```
# observed.search_Dijkstra=1 (search.Dijkstra)
# observed.cypher_plan_cache_hits=2 (Engine.Run (repeat query))
# observed.cypher_countstore_delta_applied=844 (count-store commit fan-out (#2087))
# observed.cypher_countstore_relabel_dirtied=2 (count-store relabel (#2087))
# observed.cypher_countstore_recompute=1 (Engine reopen recompute (#2087))
# countstore.cells_after=4
# countstore.write_batch=200
# countstore.write_throughput_ops_per_sec=5682
# scrape.series.total=15
# scrape.series.extra=0
# workload.elapsed=45ms
# mem.heap_alloc=1.11 MiB
# scrape.bytes=4100
```

The `metric.present.<name>=true` lines are the point of the example: each
asserts that the documented metric (schema `<package-path>.<Symbol>` from
[`docs/metrics.md`](../../docs/metrics.md)) fired and appears in the scraped
Prometheus exposition, where the backend renders it with dots mapped to
underscores (`search.Dijkstra` → `search_Dijkstra`). Names are deterministic
and pinned by the test; the observed values behind them are telemetry.

## Evidence it collects

- **Which instrumented APIs surface which metrics** — a presence fact per
  expected metric, split across subsystems: `cypher` (`Run`, `RunInTx`, the
  `plan_cache` hit/miss counters, and the `countstore` `delta.applied` /
  `relabel.dirtied` counters plus the `recompute` histogram), `search`
  (`Dijkstra`/`DijkstraCtx` latency plus the `search.pool.dijkstra` get/put
  utilisation counters), `graph.io.csv` (`Write`, `ReadInto`), and `bolt`
  (the `EncodePool` get/put counters). Scaling up thickens the latency
  histograms so bucket distributions become meaningful.
- **The count-store's footprint and write-neutrality** — `countstore.cells_*`
  telemetry shows the store's live-cell count stays bounded by schema
  cardinality (four cells for the single `(:SERVICE)-[:CALLS]->(:SERVICE)`
  combination) rather than by `|E|`, and `countstore.write_throughput_ops_per_sec`
  samples the autocommit write rate with the store active, so its neutrality
  to the write path is observable.
- **The MVCC substrate under concurrent writers** — `mvcc.commits.delta`
  counts the transactions that published an instant, `mvcc.conflicts.observed`
  proves the conflict path was actually taken rather than hoped for,
  `mvcc.writers.settled` asserts no writer is left in flight, and
  `mvcc.chain_depth.deepest_at_least_two` asserts the pinned read snapshot
  really did hold a chain open. The volatile shape — abort count, retry count,
  conflict rate, chain-depth buckets, version totals against the bound,
  horizon occupancy against its capacity, and the vacuum's pass count and mean
  pass duration — goes out as telemetry.
- **A real retriable conflict, and one that should not happen** — the four
  writers touch disjoint keys, so nothing should contend; a conflict
  nonetheless appears every few hundred writes once enough writers are in
  flight. It is not contention but the contiguous commit frontier leaving a
  writer's own previous commit invisible to its next transaction, measured and
  filed as rmp #2328. The example retries, which is what a client must do
  under MVCC, and counts the retries so the effect is visible rather than
  hidden.
- **The exposition itself** — total series count, extra series discovered
  beyond the pinned set, and total scrape byte size, so a reader sees the
  real shape of a `/metrics` scrape.
- **The activation cost model** — the workload runs under the Prometheus
  backend; on the default no-op backend each wired site costs ~50 ns (two
  atomic loads + a `time.Now` pair), which is why the surface is dark until
  a backend is installed.

## Key APIs

- `metrics.NewPrometheusRegistry` / `metrics.SetBackend` — install (and, with
  `nil`, restore) the global metrics backend.
- `metrics.Registry.Handler` — the `http.Handler` that serves the Prometheus
  text exposition on `/metrics`.
- `cypher.Engine.Run` / `cypher.Engine.RunInTx` — instrumented read and
  write query entry points.
- `cypher.Engine.CountStoreCells` — the count-store size indicator the
  metrics backend cannot express as a gauge; read directly for the
  `countstore.cells_*` telemetry.
- `search.Dijkstra` — instrumented single-source shortest-path over a CSR.
- `csv.WriteCtx` / `csv.ReadIntoCtx` — instrumented edge-list interchange.
- `packstream.EncodePool` — the pooled Bolt encoder whose `Get`/`Put` emit
  utilisation counters.
- `lpg.Graph.ApplyVersioned` / `BeginVersionedTx` / `EndVersionedTx` — the
  instrumented MVCC write brackets: an autocommit transaction, and the
  begin/publish pair of a multi-statement one.
- `lpg.Graph.MVCCStats` / `VacuumStats` / `ChainDepths` — the substrate's
  state, read directly for the facts and telemetry the metrics backend
  publishes as gauges.
- `lpg.Graph.BeginRead` / `EndRead` / `ReclaimNow` — a pinned read snapshot
  and a synchronous sweep, which is what gives the chain-depth distribution
  something to measure.
- `mvcc.ErrSerializationConflict` — the retriable error a write-write
  conflict surfaces as.

## Further reading

- [`metrics`](../../metrics) — the public observability facade.
- [docs/metrics.md](../../docs/metrics.md) — the authoritative metric-name
  inventory and naming schema.
- [Example 22 — Cypher](../22_cypher) — the Cypher engine driven over a
  seeded social graph.
- [Example 26 — Social-scale benchmark](../26_social_scale_bench) — the
  reference end state for the examples standard.
