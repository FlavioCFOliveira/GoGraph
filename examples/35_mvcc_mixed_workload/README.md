# Example 35 — Reader latency under a mixed OLTP-and-analytics workload

## Scenario

An application serving a graph while it keeps being written to. Three roles run
against one engine at the same time, which is the ordinary shape of a
production deployment rather than a stress case:

- **OLTP readers** — a pool of goroutines running an indexed point lookup,
  `MATCH (n:Account {id: $id}) RETURN n.balance`. This is the latency-sensitive
  path: a few microseconds, and users wait on it.
- **Analytics** — one goroutine running a reporting query, an unbounded
  self-join over every account. Seconds, not microseconds.
- **Ingest** — a writer committing a node every 100 ms through the WAL-backed
  engine, so each write takes the exclusive visibility barrier and pays a real
  `fsync`.

## Objective

Answer one question with evidence: **does a point query keep its latency while
an analytical query and an ingest stream run beside it?**

The example measures all **four** combinations — neither role, each role alone,
and both together — because the answer is only meaningful as a comparison. A
system can be slow under load for uninteresting reasons; what matters is
whether the *combination* costs more than the parts, which is the signature of
one role blocking another rather than of shared CPU.

## Purpose

This is the instrument for rmp **#2274** and the acceptance evidence for the
MVCC programme in [`docs/design-mvcc-delta-chains.md`](../../docs/design-mvcc-delta-chains.md).

`Engine.Run` holds the graph read barrier across a query's whole execution, a
write takes that barrier exclusively, and Go's `sync.RWMutex` prefers a waiting
writer — so once the writer queues behind the long read, every reader arriving
after it parks until the long read finishes. When the programme's phase P4
retires the read barrier, the numbers below should change and this example is
how that is demonstrated.

No other example covers this. Example 27 certifies transactional isolation —
*correctness* under concurrency; example 20 measures read scaling over an
immutable CSR with **no writers at all**, which is precisely the configuration
in which the effect cannot appear.

## What it measures

| Indicator | Why |
|---|---|
| short-read throughput per phase | the headline, but the weaker of the two |
| short-read p50, p99, **max** | the max is the sharp one: it is what a user actually waits |
| analytical query duration | the effect lasts exactly as long as this, so it is reported, not assumed |
| writes completed | confirms the ingest role really ran |
| `verdict.worst_read_vs_analytics` | the ratio that names the defect: ≈1 means the point query waited out the whole report |
| `mvcc.label_deltas`, `mvcc.prop_deltas` | the versioning substrate's live records — the memory a reclamation phase owes |
| heap bytes | the resource axis |

## Running it

```bash
cd examples/35_mvcc_mixed_workload
go run .                      # ~4 s
go run . -nodes 20000         # the analytical query grows to ~95 s
go run . -readers 16 -phase-window 2s
```

Sample output (Apple M4, 10 cores, defaults):

```
# analytics.duration=402ms
# phase.baseline.throughput_ops=450867.1          max=991µs
# phase.analytics_only.throughput_ops=372998.6    max=355µs
# phase.writer_only.throughput_ops=422962.9       max=11.133ms
# phase.analytics_and_writer.throughput_ops=40914.3   max=799.871ms
# verdict.throughput_collapse=11.0x
# verdict.max_latency_amplification=807.3x
# verdict.worst_read_vs_analytics=1.99
readers_starved=1
```

Read the middle two rows first: **each role alone is nearly free.** A concurrent
analytical query costs the readers nothing; a concurrent writer costs them
nothing. Only together do they collapse — and the worst point read grows to
about twice the analytical query's duration, meaning it waited out two of them
back to back.

## Two notes on the measurement

**The analytical query's cost is O(nodes²) and `-nodes` is the only knob that
changes it.** A first version of this example carried an `-analytics-n` flag
that bounded the join's left-hand side with `a.id < N`. It made no difference:
the join is a Cartesian product and the predicate filters its *output*, so the
scan is nodes × nodes whatever `N` is — the query measured 1m35s at both `N=8`
and `N=400`. The flag was removed rather than documented, because a knob that
does nothing is worse than no knob.

**The effect is invisible if the long read is short.** The stall lasts exactly
as long as the analytical query, so a version of this measurement using a
millisecond-scale "long" read reports no problem at all. The example therefore
calibrates first and emits `analytics.is_long`, and the paired gate in
`bench/mtaudit/fairness_soak_test.go` *fails* if its long read is under five
seconds rather than quietly measuring nothing.
