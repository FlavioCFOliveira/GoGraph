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

And it surfaces the **planner statistics and their quality**. The statistics
are best-effort and are rebuilt only when a caller asks — there is no background
worker, by design — which left an operator with a question nothing could answer:
is a refresh overdue? A dedicated phase refreshes the statistics, then makes one
of them stale on purpose and PROFILEs a query that reads it. Because the
planner's estimate and the measured row count now sit on the same plan node, the
ratio between them — the **q-error** — is available at no extra cost, and it goes
out as the `cypher.stats.qerror` distribution, the `cypher.stats.qerror.high`
counter, and the `Engine.StatsMisestimatedPairs` accessor that names how many
tracked `(label, property)` statistics were caught predicting badly. Only PROFILE
emits any of it: an `Engine.Run` produces no measurement to compare against.

And it surfaces the **Bolt network surface**. A real `bolt/server` is started
on a real TCP socket and driven by the official `neo4j-go-driver` through an
autocommit read, a committed explicit transaction and a rolled-back one. That
populates the per-message latency histograms
`bolt.server.HandleMessage.message.<type>` alongside the connection and
transaction counters, so an operator can see how long a RUN, a PULL, a BEGIN or
a COMMIT took **as the server measured it**. That distinction is the point: the
Bolt latencies previously quoted for GoGraph came from a client-side stopwatch
in `examples/23_bolt_server`, which no deployment can emit.

Seven of the thirteen message types in the closed label set appear in the
scrape, because seven is what these three session shapes deterministically
send. DISCARD, RESET, ROUTE, LOGOFF and GOODBYE are not pinned here — whether a
client library sends them is its own choice, and a presence fact that depends
on that is not a fact. They are covered instead by
`bolt/server/msgmetrics_wire_test.go`, which drives every one of them over the
raw wire.

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
paths so their observability metrics appear in the scrape. A final phase
decommissions most of a small dedicated `legacy` tier — those services lose the
`:SERVICE` label and leave the live topology, while their nodes, their `tier`
and their `:CALLS` edges stay on the record — which is what makes the
planner's statistics stale for the q-error reading described below.

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
bolt.services_before=201
bolt.services_after=202
bolt.commit_delta=1
mvcc.commits.delta=258
mvcc.conflicts.observed=1
mvcc.writers.settled=0
mvcc.chain_depth.deepest_at_least_two=1
stats.tracked_pairs_positive=1
stats.tier_after_decommission=3
stats.misestimated_pairs=1
stats.misestimated_cleared_by_refresh=1
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
metric.present.bolt.server.HandleMessage.message.hello=true
metric.present.bolt.server.HandleMessage.message.logon=true
metric.present.bolt.server.HandleMessage.message.run=true
metric.present.bolt.server.HandleMessage.message.pull=true
metric.present.bolt.server.HandleMessage.message.begin=true
metric.present.bolt.server.HandleMessage.message.commit=true
metric.present.bolt.server.HandleMessage.message.rollback=true
metric.present.bolt.server.conn.accepted=true
metric.present.bolt.server.conn.closed=true
metric.present.bolt.server.tx.opened=true
metric.present.bolt.server.tx.closed=true
metric.present.cypher.countstore.recompute=true
metric.present.cypher.countstore.delta.applied=true
metric.present.cypher.countstore.relabel.dirtied=true
metric.present.cypher.stats.refresh=true
metric.present.cypher.stats.refresh.latency=true
metric.present.cypher.stats.lookup=true
metric.present.cypher.stats.qerror=true
metric.present.cypher.stats.qerror.high=true
metric.present.graph.lpg.ApplyVersioned=true
metric.present.graph.lpg.EndVersionedTx=true
metric.present.lpg.mvcc.writers.active=true
metric.present.lpg.mvcc.commits=true
metric.present.lpg.mvcc.aborts=true
metric.present.lpg.mvcc.conflict_rate=true
metric.present.lpg.mvcc.conflicts=true
metric.present.lpg.mvcc.conflicts.store.node_properties=true
metric.present.lpg.mvcc.chain_depth.deepest=true
metric.present.lpg.mvcc.chain_depth.bucket.1=true
metric.present.lpg.mvcc.oldest_snapshot_age=true
metric.present.lpg.mvcc.snapshots.active=true
metric.present.lpg.mvcc.vacuum.passes=true
metric.present.lpg.mvcc.vacuum.pass=true
metric.present.count=45
metric.expected.count=45
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
  (the `EncodePool` get/put counters, the `bolt.server.conn.*` and
  `bolt.server.tx.*` counters, and the per-message
  `bolt.server.HandleMessage.message.<type>` latency histograms). Scaling up
  thickens the latency histograms so bucket distributions become meaningful.
- **The server's own view of Bolt latency** — the per-message histograms are
  the only Bolt timing in the module that a deployed GoGraph can publish. The
  scrape carries a `_bucket`, `_sum` and `_count` triple per message type, so
  p50/p99 per message type are derivable from the exposition alone.
- **The count-store's footprint and write-neutrality** — `countstore.cells_*`
  telemetry shows the store's live-cell count stays bounded by schema
  cardinality (four cells for the single `(:SERVICE)-[:CALLS]->(:SERVICE)`
  combination) rather than by `|E|`, and `countstore.write_throughput_ops_per_sec`
  samples the autocommit write rate with the store active, so its neutrality
  to the write path is observable.
- **How wrong the planner's estimates turn out to be** — the statistics are
  rebuilt only when a caller invokes `RefreshStatistics`, and nothing used to
  tell a caller when to. Step 10 refreshes, **decommissions** 9 of the 12
  `legacy` services — they lose the `:SERVICE` label — WITHOUT refreshing, and
  PROFILEs a query that reads the now stale statistic. The profiled plan
  (telemetry, `# stats.profile|`) shows `Est.Rows = 12` beside `Rows = 3` on the
  same `Filter` line, the `cypher.stats.qerror` histogram carries that 4x ratio
  as a sample, and `stats.misestimated_pairs=1` names how many tracked
  `(label, property)` statistics were caught. A second refresh clears it
  (`stats.misestimated_cleared_by_refresh=1`), which is what makes the accessor
  a "refresh overdue?" signal rather than a lifetime tally.
- **Why the mutation is a decommission and not a re-tier** (rmp #2795) — it was
  a property write (`SET s.tier = 'quarantine'`) until rmp #2772 gave the
  most-common-value provider the staleness screen its range sibling always had.
  A property write moves both staleness counters, so the snapshot is stale by
  its own measure and the estimate is correctly **demoted** — and a demoted
  estimate is scored by nothing, which left this step emitting no q-error at
  all. Removing the label instead touches no property, so both counters stay at
  zero while the live `:SERVICE` population falls and the most-common-value
  entry goes 4x wrong.
- **Why the tier is a small dedicated cohort and not a quarter of the fleet**
  (rmp #2785) — the decommission used to take 45 of the 50 `core` services,
  and rmp #2785 closed that route: the staleness screen's drift numerator now
  carries a population-**shrinkage** term as well as the write counter, so
  removing a quarter of the fleet shrinks `:SERVICE` by ~22%, far past the
  screen's 9.6% firing region, and the estimate is correctly demoted. That is
  the fix working, and the workload moved rather than the screen — the premise
  guard below caught it by name, at the point of cause. What is left is the
  residual the screen still cannot see, and it is the honest place for this
  demonstration: the rule is a fraction of the **population**, so a small
  shrinkage **concentrated** on one most-common value is invisible to it. 9
  services leaving a fleet of 193 is 4.7%, inside the region, while the
  most-common-value entry for their tier goes 4x wrong. The stale estimate is
  therefore still tagged **exact** and rendered as a bare, unmarked number, and
  this metric is the only thing in the module that can see it. Both halves of
  the premise — that the tier really shrank, and that the resulting misestimate
  really was scored — abort step 10 with a named error rather than reporting a
  silent zero, and the regression test asserts `stats.misestimated_pairs`
  directly so a future change to the staleness screen cannot hollow the
  demonstration out unnoticed. The cohort is a fixed count, so the 4x miss holds
  at every scale; what does vary is whether the 9 removed services are a small
  enough fraction of the fleet, which measures as `-services 101` (94 live
  services after the decommission, 9/94 = 9.57%) being the smallest scale at
  which `cypher.stats.qerror.high` fires, and `-services 100` (93 live,
  9/93 = 9.68%) the largest at which it does not. The second premise guard is
  conditional on both the achieved miss and the achieved shrinkage for exactly
  that reason, so a small run reports honestly instead of failing.
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
- `cypher.Engine.RefreshStatistics` — the sole, caller-driven rebuild of the
  planner statistics; there is no background worker.
- `cypher.Engine.ProfileTable` — the profiled physical plan, and the ONLY
  surface that emits `cypher.stats.qerror`: the comparison needs a measurement,
  which an `Engine.Run` does not produce.
- `cypher.Engine.StatsTrackedPairs` / `cypher.Engine.StatsMisestimatedPairs` —
  how many `(label, property)` statistics the engine holds, and how many of
  them a PROFILE has caught predicting badly by 3x or more.
- `search.Dijkstra` — instrumented single-source shortest-path over a CSR.
- `csv.WriteCtx` / `csv.ReadIntoCtx` — instrumented edge-list interchange.
- `packstream.EncodePool` — the pooled Bolt encoder whose `Get`/`Put` emit
  utilisation counters.
- `server.NewServer` / `server.Server.Serve` / `server.Server.Shutdown` — the
  Bolt v5 server, started on a real socket so its per-message latency
  histograms are populated by genuine wire traffic.
- `neo4j.NewDriverWithContext` — the official Bolt client, used as a real
  external consumer rather than an in-process call.
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
