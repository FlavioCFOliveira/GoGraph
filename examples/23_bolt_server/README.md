# Example 23 — Bolt v5 extreme-concurrency laboratory

## What it demonstrates

GoGraph speaking the Bolt v5 wire protocol end to end, and the same binary
turned into a controlled instrument for the one question the wire path has
never had a reproducible answer for: **what does it cost the server to hold,
dispatch for, and refuse a very large number of concurrent connections?**

It starts the embedded `bolt/server` over an in-memory graph, connects the
official `neo4j-go-driver/v5` as a real client, drives Cypher over the wire at
a chosen number of concurrent connections, and shuts the server down cleanly
with no goroutine left behind. With `-artifact-dir` it writes every profile,
trace, and machine-readable record needed to attribute that cost; with
`-ladder` one invocation sweeps the module's published concurrency ladder —
**1, 8, 64, 256, 1024** connections — plus a saturation rung that deliberately
offers more connections than `Options.MaxConnections` admits.

## Domain / scenario

A directed social network. Each `:Person` node carries an `id` (a 24-char hex
string) and a `name`. Every person is given a random out-degree in
`[knows-min, knows-max]` to distinct other people through `:KNOWS` edges, and
every `:KNOWS` edge carries a mandatory `since` date. The dates are stored as
ISO-8601 (`YYYY-MM-DD`) strings drawn from the seeded RNG and anchored to a
fixed reference date, so they are reproducible for a given `-seed` and the
engine reads them back as non-null, chronologically sortable values.

The graph is seeded in process through the property-graph API before the
server starts. Bolt clients then fire the fixed query
`MATCH (n:Person) RETURN count(n) AS c` repeatedly over the wire, verifying
each response and timing it.

The listener binds to `127.0.0.1:0`, so the kernel assigns a free port (a test
run never collides on a fixed port); the client discovers it from `ln.Addr()`.
The server is fully compatible with `neo4j-go-driver/v5` and `cypher-shell`.

## The two client dimensions

`-sessions` and `-connections` are **not** the same knob, and only one of them
can reach the server's connection semaphore.

| Dimension | What it controls | Sockets |
|---|---|---|
| `-sessions` | the driver's logical concurrency — how many Bolt sessions issue queries | decided by the driver pool |
| `-connections` | the socket count itself — each connection is an independent driver whose pool holds exactly one connection | exactly `-connections` |

- **Pooled mode** (`-connections 0`, the default) is the example's original
  shape: one shared driver pool running `-sessions` concurrent sessions. How
  many TCP connections that opens is the pool's decision, so this mode cannot
  drive the server to `Options.MaxConnections`.
- **Connection mode** (`-connections N`, N ≥ 1) opens exactly N sockets. Here
  `-sessions` means *sessions opened per connection*: each connection works
  through its share of the queries using `-sessions` sessions in turn, so
  session lifecycle is exercised without changing the socket count.

Every connection handshakes and then waits at a **barrier** until every other
offered connection has resolved. Without that barrier the early connections
would finish their share and release their semaphore slots while the late ones
were still dialling, the server would never hold N connections at once, and a
saturation run would quietly admit every connection it was meant to refuse.

## Saturation — the reject branch

Offering more connections than the semaphore admits drives the reject branch of
`Serve` (`bolt/server/serve.go`, the `select` on `s.sem`). The refusal is
invisible from the client — a rejected connection simply fails to connect, which
looks like any other network failure — so the example installs a metrics sink
and reports `bolt.server.conn.rejected` directly.

```sh
go run ./examples/23_bolt_server -connections 256 -max-connections 128 -queries 20000
```

```
queries.ok=10014
# conn.offered=256
# conn.established=128
# conn.failed=128
# counters.bolt.server.conn.accepted=128
# counters.bolt.server.conn.rejected=128
# counters.bolt.server.conn.closed=128
```

The deterministic-fact contract is relaxed on a saturation rung and nowhere
else: the queries of the refused connections are deliberately lost, so
`queries.ok` there is an observation rather than an invariant.

## How to run

```sh
# small deterministic default (pooled mode, unchanged from before)
go run ./examples/23_bolt_server

# one rung: 256 independent connections, full artefact set
go run ./examples/23_bolt_server -connections 256 -queries 20000 -sessions 1 \
    -repetitions 10 -artifact-dir /tmp/boltlab/conn256

# the whole ladder plus the saturation rung, in one invocation
go run ./examples/23_bolt_server -ladder -artifact-dir /tmp/boltlab \
    -queries 20000 -sessions 1 -repetitions 10

# compare the rungs
benchstat /tmp/boltlab/bench.txt
```

Use **`-repetitions 10` or more** for any run whose numbers will be compared:
`benchstat` needs at least six samples before it reports a confidence interval.

## Flags

### Scale and shape

| Flag | Meaning | Default | Representative large value |
|---|---|---|---|
| `-nodes` | number of `:Person` nodes to seed | `2000` | `200000` |
| `-knows-min` | minimum `:KNOWS` out-degree per person | `5` | `20` |
| `-knows-max` | maximum `:KNOWS` out-degree per person | `8` | `50` |
| `-queries` | read queries fired over the wire, in total | `2000` | `50000` |
| `-sessions` | concurrent sessions (pooled mode) / sessions per connection | `4` | `16` |
| `-seed` | RNG seed (fixes the deterministic data shape) | `42` | any `int64` |

### Concurrency

| Flag | Meaning | Default |
|---|---|---|
| `-connections` | independent Bolt connections; `0` keeps the shared driver pool | `0` |
| `-max-connections` | `bolt/server.Options.MaxConnections`; `0` derives it as offered + 4 | `0` |
| `-connect-timeout` | client dial and connection-acquisition bound | `5s` |

`-connect-timeout` is load-bearing for saturation: the driver's own default
acquisition timeout is 60 s, so without it every refused connection would stall
the run for a minute before reporting the refusal.

### Instrument

| Flag | Meaning | Default |
|---|---|---|
| `-artifact-dir` | write this run's full artefact set here | *(unset)* |
| `-repetitions` | unprofiled measurement windows, so `benchstat` has a spread | `1` |
| `-label` | `benchstat` sub-name for the run | `conn=<n>` / `pooled` |
| `-mutex-fraction` | `runtime.SetMutexProfileFraction` for the profiled window (`0` disables) | `1` |
| `-block-rate` | `runtime.SetBlockProfileRate` in ns for the profiled window (`0` disables) | `1` |
| `-fd-sampling` | sample the open-descriptor count during a window; its own cost is one syscall per open descriptor per sample, so it is linear in the connection count (measured: 0.62% of process CPU at 64 connections, 2.32% at 256, 5.78% at 1024). `false` drops `max_open_fds` to `0` and leaves `peak_open_fds` intact | `true` |
| `-server-log` | logger given to `bolt/server` `Options.Logger`: `default` \| `discard` \| `error` | `default` |
| `-ladder` | sweep the ladder, one child process per rung (requires `-artifact-dir`) | `false` |
| `-ladder-levels` | comma-separated ladder | `1,8,64,256,1024` |
| `-saturation-offer` | connections offered by the saturation rung (`0` drops it) | `256` |
| `-saturation-admit` | `MaxConnections` for the saturation rung | `128` |

`-artifact-dir` owns every profile of an instrumented run, so it may not be
combined with `exprof`'s `-profile-dir` or `-trace`; the run refuses to start
rather than writing a truncated CPU profile somewhere unexpected.

`-server-log` exists to make the server's own logging measurable instead of
assumed. The accept loop writes a `WARN` for every connection the
`MaxConnections` semaphore refuses, and that emission sits on the accept
goroutine between one `Accept` and the next. The three modes remove one layer
each, so the difference between two runs is the layer named:

| Mode | `Options.Logger` | What it still pays |
|---|---|---|
| `default` | `nil` — the server uses `slog.Default()` | formats the record and writes it to stderr |
| `discard` | a `TextHandler` at `LevelInfo` over `io.Discard` | formats the record; writes nothing |
| `error` | a `TextHandler` at `LevelError` over `io.Discard` | returns before building the record |

No mode removes the cost of **evaluating the call's arguments**, which the
language performs before the call in every one of them.

## Artefact layout

A single instrumented rung writes:

```
<artifact-dir>/
  cpu.pprof         # CPU attribution over the profiled window
  heap.pprof        # live heap after a forced collection
  mutex.pprof       # lock contention, full rate by default
  block.pprof       # blocking events, full rate by default
  goroutine.pprof   # taken AT the peak, while every connection is live
  trace.out         # runtime/trace: scheduling, blocking, GC, syscalls
  metrics.json      # the machine-readable record (below)
  host.json         # cores, load average, descriptor ceiling, idle verdict
  bench.txt         # one benchstat record per unprofiled repetition
```

A ladder sweep writes one such directory per rung, plus an index:

```
<artifact-dir>/
  ladder.json       # every rung, its command, its outcome, its record
  host.json         # the sweep's own environment
  bench.txt         # every rung's records concatenated, for one benchstat call
  conn=1/     …     # the nine artefacts above, per rung
  conn=8/     …
  conn=64/    …
  conn=256/   …
  conn=1024/  …
  saturation/ …     # plus run.log, the child's captured output
```

### Why a child process per rung

The mutex, block, and allocation profiles **accumulate for a process's lifetime
and the runtime offers no way to reset them**. A second rung measured in the
same process would inherit the first one's samples and publish them as its own.
A fresh child also gives each rung a cold heap and an unraised GC goal, so a
rung is measured under conditions the next sweep can reproduce. The same
reasoning, established by measurement, governs `bench/contention/observatory.go`.

### Effect and probe are separate, and only one may be quoted

Full-rate contention profiling perturbs the very contention it measures. Every
rung therefore runs `-repetitions` **unprofiled** windows and then one
**profiled** window:

- the unprofiled windows (`effect` in `metrics.json`, and the only records in
  `bench.txt`) supply throughput and latency — the numbers that may be quoted;
- the profiled window (`probe`) supplies attribution per call site — and its
  throughput must never be quoted as the module's.

### `metrics.json`

```json
{
  "schema": "gograph.examples.23_bolt_server.lab/v1",
  "label": "saturation",
  "config": { "connections": 256, "max_connections": 128, "expect_rejections": true, "...": "" },
  "host":   { "num_cpu": 10, "loadavg_before": [3.94, 2.33, 2.09], "idle": false,
              "idle_note": "NOT IDLE: pre-run loadavg1 3.94 > threshold 1.00 (10 cores x 0.10)" },
  "effect": [ { "conn_offered": 256, "conn_established": 128, "conn_failed": 128,
                "queries_ok": 10016, "throughput_qps": 149485.75,
                "p50_ns": 629083, "p99_ns": 3960625, "p999_ns": 10035792,
                "peak_goroutines": 388, "peak_open_fds": 262,
                "counters": { "bolt.server.conn.accepted": 128,
                              "bolt.server.conn.rejected": 128,
                              "bolt.server.conn.closed": 128 },
                "counters_settled": true } ],
  "probe":  { "...": "same shape, profiled — attribution only" }
}
```

## Evidence it collects

- **Throughput** (`# load.throughput`) — successful queries per second.
- **Latency distribution** (`# load.latency_p50/p95/p99/p999`) — p999 is carried
  because it is the percentile a saturation run actually moves.
- **Connect cost** (`# load.connect_elapsed`) — timed separately from the query
  phase, because at 1024 connections establishing dominates and would otherwise
  be silently charged to throughput.
- **Server counters** (`# counters.bolt.server.*`) — the accepted / rejected /
  closed connection triple and the seven transaction counters, reported as the
  **delta** across each window. The window waits for the server's live-connection
  derivation (`accepted − closed`) to settle before reading the delta, and
  records `counters_settled: false` if it did not.
- **Live process** (`# runtime.goroutines_peak`, `# runtime.open_fds_peak`, and
  the `_max` pair from a 25 ms sampler) — the goroutine and descriptor cost of
  the connection count.
- **Host** (`# host.cores`, `# host.loadavg_before/after`, `# host.idle`) — a run
  is called idle only when its pre-run load average was actually read and sat at
  or below `NumCPU × 0.10`. **An unread load average is reported as not certified
  idle, never as idle.**

### Reading the numbers honestly

The client and the server **run in the same process and share the same cores**.
Throughput and latency therefore include the client driver's own cost, and at
1024 connections the client is itself 2048 of the process's goroutines. The
caveat travels with the evidence, in `host.json`'s `note` field. Comparing rungs
against each other is sound; comparing these figures against a server measured
with a remote client is not.

## Expected output

The deterministic facts — the seeded node count, the fixed query's result over
that data, the number of queries that succeeded, and the seed-stable edge total
— are reproducible for a fixed `-seed`. The `# `-prefixed telemetry lines vary
per run and per machine and are never pinned.

```
config.nodes=2000
config.knows=[5,8]
config.queries=2000
config.sessions=4
config.seed=42
config.connections=0
config.max_connections=8
config.repetitions=1
nodes.person=2000
edges.knows=13012
q.count_person=2000
queries.ok=2000
# load.connect_elapsed=383us
# load.elapsed=31ms
# load.throughput=65506 queries/s
# load.latency_p50=54us
# load.latency_p95=84us
# load.latency_p99=156us
# load.latency_p999=1.661ms
# conn.offered=0
# conn.established=0
# conn.failed=0
# counters.bolt.server.conn.accepted=4
# counters.bolt.server.conn.rejected=0
# counters.bolt.server.conn.closed=4
# counters.bolt.server.conn.panics=0
# runtime.goroutines_peak=11
# runtime.goroutines_max=16
# runtime.open_fds_peak=8
# runtime.open_fds_max=14
# mem.heap_alloc=3.84 MiB
# host.cores=10
# host.gomaxprocs=10
# host.loadavg_before=1.76 2.04 2.00
# host.loadavg_after=1.76 2.04 2.00
# host.fd_limit_soft=122880
# host.idle=false (NOT IDLE: pre-run loadavg1 1.76 > threshold 1.00 (10 cores x 0.10))
# counters.total.bolt.server.conn.accepted=4
# counters.total.bolt.server.conn.rejected=0
# counters.total.bolt.server.conn.closed=4
# server shut down cleanly
```

`edges.knows` is the realised sum of the random per-person out-degrees: it is
fixed for `-seed 42` but changes with a different seed. Every `# ` line above is
one real run's telemetry — timings, counters, and the host's load differ on
every run and on every machine. `conn.offered` is `0` in pooled mode because the
socket count there is the driver pool's business; the server's `accepted`
counter is the authority. That run was measured on a host carrying other work,
and says so on its own `# host.idle` line rather than leaving the reader to
assume otherwise.

## Key APIs

- `bolt/server.NewServer` / `Server.Serve` / `Server.Shutdown` — start the Bolt v5 TCP server on a listener and tear it down gracefully, draining every per-connection goroutine.
- `bolt/server.Options` — bound concurrent connections (`MaxConnections`) and set the per-connection idle deadline (`ConnTimeout`).
- `bolt/server` metrics — `bolt.server.conn.accepted` / `.rejected` / `.closed`, read through the `internal/metrics` backend this example installs.
- `cypher.NewEngine` — build the query engine over the in-memory graph the server serves.
- `graph/lpg.New` / `Graph.AddEdgeLabeled` / `Graph.SetEdgeProperty` — seed the labelled property graph with `:Person` nodes and dated `:KNOWS` edges.
- `examples/internal/exprof` — the shared `-profile-dir` / `-trace` contract; the laboratory drives it per rung for `cpu.pprof`, `heap.pprof`, and `trace.out`.
- `github.com/neo4j/neo4j-go-driver/v5/neo4j` — the official Bolt client: `NewDriverWithContext`, `VerifyConnectivity`, `NewSession`, `Session.Run`, `Result.Single`, and clean `Close` on teardown.

## Further reading

- [`bolt/server`](../../bolt/server) — the Bolt v5 server package documentation
- [`bolt/server/serve.go`](../../bolt/server/serve.go) — the accept loop and the `MaxConnections` semaphore this example saturates
- [`bolt/server/metrics.go`](../../bolt/server/metrics.go) — what each `bolt.server.*` counter means
- [`bench/contention/observatory.go`](../../bench/contention/observatory.go) — the in-repo precedent for the two-window, one-process-per-window profiling discipline
- [`cypher`](../../cypher) — the Cypher query engine the server executes against
- [Example 22 — Cypher engine](../22_cypher) — running Cypher in process without the Bolt wire layer
- [Example 26 — social scale bench](../26_social_scale_bench) — the reference example this one is brought up to
- [docs/examples-standard.md](../../docs/examples-standard.md) — the standard every example follows
