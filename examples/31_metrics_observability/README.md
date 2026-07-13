# Example 31 — Metrics & Observability (Prometheus)

## What it demonstrates

How to turn on GoGraph's observability surface — the metrics the module
mandates on every public blocking API — through the public
`metrics.NewPrometheusRegistry` / `metrics.SetBackend` facade, drive a mixed
workload across subsystems, and scrape the latency histograms and
utilisation counters over an HTTP `/metrics` endpoint in Prometheus
text-exposition format. It is the one example that exercises the
observability axis.

## Domain / scenario

A **service-mesh call graph**: nodes are microservices (`:SERVICE`), edges
are directed RPC dependencies (`:CALLS`) each attributed a synthetic
latency. A seeded generator gives every service a random out-degree in
`[calls-min, calls-max]` to distinct callees, so the topology — and every
deterministic fact below — is reproducible for a fixed `-seed`. The call
graph is materialised into three representations that feed instrumented
APIs: a labelled property graph for Cypher, an int64-weighted adjacency
list (latency in microseconds) for the CSV round-trip, and a CSR (built
from that adjacency list) for Dijkstra.

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
metric.present.count=12
metric.expected.count=12
```

Followed by `# `-prefixed telemetry that varies per run and per machine,
for example:

```
# observed.search_Dijkstra=1 (search.Dijkstra)
# observed.cypher_plan_cache_hits=2 (Engine.Run (repeat query))
# scrape.series.total=12
# scrape.series.extra=0
# workload.elapsed=10.7ms
# mem.heap_alloc=1.11 MiB
# scrape.bytes=3365
```

The `metric.present.<name>=true` lines are the point of the example: each
asserts that the documented metric (schema `<package-path>.<Symbol>` from
[`docs/metrics.md`](../../docs/metrics.md)) fired and appears in the scraped
Prometheus exposition, where the backend renders it with dots mapped to
underscores (`search.Dijkstra` → `search_Dijkstra`). Names are deterministic
and pinned by the test; the observed values behind them are telemetry.

## Evidence it collects

- **Which instrumented APIs surface which metrics** — a presence fact per
  expected metric, split across five subsystems: `cypher` (`Run`, `RunInTx`,
  the `plan_cache` hit/miss counters), `search` (`Dijkstra`/`DijkstraCtx`
  latency plus the `search.pool.dijkstra` get/put utilisation counters),
  `graph.io.csv` (`Write`, `ReadInto`), and `bolt` (the `EncodePool` get/put
  counters). Scaling up thickens the latency histograms so bucket
  distributions become meaningful.
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
- `search.Dijkstra` — instrumented single-source shortest-path over a CSR.
- `csv.WriteCtx` / `csv.ReadIntoCtx` — instrumented edge-list interchange.
- `packstream.EncodePool` — the pooled Bolt encoder whose `Get`/`Put` emit
  utilisation counters.

## Further reading

- [`metrics`](../../metrics) — the public observability facade.
- [docs/metrics.md](../../docs/metrics.md) — the authoritative metric-name
  inventory and naming schema.
- [Example 22 — Cypher](../22_cypher) — the Cypher engine driven over a
  seeded social graph.
- [Example 26 — Social-scale benchmark](../26_social_scale_bench) — the
  reference end state for the examples standard.
