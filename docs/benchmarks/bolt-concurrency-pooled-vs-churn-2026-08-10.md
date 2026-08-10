# Bolt concurrency sweep — pooled vs churn, 1 / 8 / 64 / 256 / 1024 clients (2026-08-10)

This is the instrument that establishes **engine query throughput at concurrency**,
and the record of why the previous figures did not.

The 2026-08-10 production-readiness certification could certify GoGraph's
correctness, safety and reliability under sustained concurrent load, but **not its
absolute throughput at extreme concurrency**, for one reason: the only sweep that
existed opened a fresh TCP connection and completed a full Bolt handshake for
**every single operation**. Its absolute figures were therefore connection-setup
cost, not engine cost, and could not be quoted as queries per second
(rmp #2397). This run adds the pooled arm that answers the question, keeps the
churn arm because connection-churn behaviour is worth measuring in its own right,
and reports both.

## Which arm answers which question

| Arm | Timed region | Answers |
|---|---|---|
| **Churn** (`BenchmarkBoltReadOnly`, `…WriteOnly`, `…Mixed`) | dial → handshake → HELLO → query → GOODBYE → close | What does connection establishment cost, and does the server stay correct under connection churn at extreme concurrency? |
| **Pooled** (`BenchmarkBoltPooledRead`, `…Write`, `…Mixed`) | query round-trip only, on a connection opened and handshaken **before** `b.ResetTimer` | What does the **engine** cost per query at this concurrency? **These are the figures to quote.** |
| **Pooled, work-sized** (`BenchmarkBoltPooledReadWork`) | as pooled, over 2 000 nodes instead of 16 | Engine throughput when the query does measurable work rather than counting 16 nodes |

The churn and pooled arms are **matched**: same 16-node seed, same
`MATCH (n) RETURN count(n)` and `CREATE (n:BenchNode)`. They differ in exactly one
respect — connection reuse — so the difference between them **is** the handshake.

> A first draft of the pooled arm changed the connection handling *and* the
> workload at once, and measured 1 923 allocs/op against the churn arm's 333 — it
> looked like a 5.8× regression when in fact the 2 000-node scan had swamped the
> handshake it was meant to isolate. Hence two seed sizes in separate arms, each
> differing from its comparator in one respect.

## Reading `ns/op`

Under `b.RunParallel`, `ns/op` is wall-time ÷ **total** iterations across all
goroutines — the inverse of **aggregate** throughput, not per-client latency:

- aggregate ops/s = `1e9 / ns_per_op`
- mean per-client latency = `ns_per_op × concurrency`

Conflating them overstates throughput by a factor of the concurrency level, which
is how the first version of the previous cycle's table went wrong.

## Run environment

| Property | Value |
|---|---|
| Date | 2026-08-10 |
| CPU | Apple M4, 10 cores (4 performance + 6 efficiency) |
| Platform | `darwin/arm64`, macOS 26.5.2 |
| Go | go1.26.5 |
| `kern.ipc.somaxconn` | **128** (the listen backlog; it bounds the churn arm — see below) |
| `ulimit -n` | 1 048 576 |
| Invocation | `go test -run='^$' -bench=BenchmarkBolt -benchmem -count=3 -timeout=7200s ./bench/soak/` |
| Result | **105/105 sub-benchmarks, `SWEEP_EXIT=0`, zero errors, zero failures** |
| Host state | 1-minute load average **2.38** at launch (the sweep waited for the host to settle; a loaded host biases concurrency ratios systematically, not noisily) |

Figures are **medians of 3**.

## Read — matched churn vs pooled

| Clients | Churn ops/s | **Pooled ops/s** | Pooled ÷ churn | Churn B/op | Pooled B/op | Churn allocs | Pooled allocs |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 6 669 | **21 348** | **3.20×** | 64 422 | 26 500 | 333 | 134 |
| 8 | 15 843 | **24 959** | 1.58× | 64 927 | 26 507 | 329 | 134 |
| 64 | 15 290 | **18 906** | 1.24× | 70 357 | 26 543 | 338 | 134 |
| 256 | 13 192 | 9 569 | **0.73×** | 69 116 | 27 665 | 338 | 135 |
| 1024 | 2 363 | **6 798** | **2.88×** | 74 293 | 31 626 | 347 | 141 |

## Write — one committed transaction per operation

| Clients | Churn ops/s | **Pooled ops/s** | Pooled ÷ churn | Churn allocs | Pooled allocs |
|---:|---:|---:|---:|---:|---:|
| 1 | 5 234 | **11 377** | 2.17× | 391 | 197 |
| 8 | 7 274 | **12 537** | 1.72× | 389 | 197 |
| 64 | 5 140 | **9 080** | 1.77× | 389 | 195 |
| 256 | 2 868 | **4 753** | 1.66× | 396 | 200 |
| 1024 | 993 | **3 674** | **3.70×** | 419 | 211 |

`BEGIN` and `COMMIT` stay **inside** the pooled timed region. A write that never
commits is not a write: holding one transaction open across every iteration would
measure an ever-growing uncommitted transaction. What pooling removes is the
connection, not the transaction — which is also what a real driver does for an
autocommit write.

## Mixed 80/20

| Clients | Churn ops/s | **Pooled ops/s** | Pooled ÷ churn |
|---:|---:|---:|---:|
| 1 | 6 333 | **17 973** | 2.84× |
| 8 | 9 268 | **20 809** | 2.25× |
| 64 | 7 170 | **15 898** | 2.22× |
| 256 | 3 699 | **7 830** | 2.12× |
| 1024 | 2 185 | **5 878** | 2.69× |

## Engine throughput on a work-sized graph

`MATCH (n:BenchSeed) RETURN count(n)` over **2 000** nodes, pooled connections.
The read is label-scoped on purpose: `MATCH (n)` also scans the `:BenchNode` rows
a writer keeps creating, so its per-operation cost would drift upward as the
benchmark ran and the measurement would become a function of how long it had been
running.

| Clients | ops/s | Mean per-client latency | B/op | allocs/op |
|---:|---:|---:|---:|---:|
| 1 | 8 207 | 0.12 ms | 97 884 | 1 923 |
| 8 | **20 749** | 0.39 ms | 97 915 | 1 923 |
| 64 | 19 672 | 3.25 ms | 98 137 | 1 923 |
| 256 | 10 115 | 25.31 ms | 99 927 | 1 925 |
| 1024 | 5 282 | 193.86 ms | 103 099 | 1 931 |

## What this establishes

1. **Engine query throughput at concurrency is now measured, not inferred.** Peak
   aggregate read throughput is **24 959 ops/s at 8 clients**; at **1024**
   concurrent clients — 102× the core count — the engine still sustains **6 798
   reads/s, 3 674 writes/s and 5 878 mixed ops/s with zero errors**.
2. **The handshake was most of the previous measurement.** At matched
   concurrency, pooling removes **199 of 333 allocations (60%)** and **37.9 KB of
   64.4 KB (59%)** per read operation, and **194 of 391 allocations (50%)** per
   write. The residual 134 allocs/op is the engine's own cost for a 16-node count
   over a live Bolt session.
3. **The 1024-client "cliff" was largely connection churn, not the engine.** The
   previous sweep read 2 363 ops/s at 1024 clients; the same engine over pooled
   connections does **6 798 (2.88×)**, and the write arm improves **3.70×**. The
   churn arm's cliff is consistent with the host's **128-deep** accept backlog
   being overrun by 1024 concurrent connects — the churn arm is measuring the
   kernel there as much as the server.
4. **Degradation past saturation is monotonic in every arm.** Throughput rises to
   a peak at 8 clients and then falls back smoothly; nothing collapses to zero,
   deadlocks, panics, or errors at 102× over-subscription of the cores.
5. **Per-operation allocation is flat in the concurrency level.** 134 → 141
   allocs/op from 1 to 1024 clients. A per-operation metric that tracked the peer
   count would be a fixture bug rather than a finding, so this flatness is also
   evidence the harness is sound.

## One result that goes the other way, reported as measured

At **256** clients the pooled read arm is **slower** than the churn arm — 9 569 vs
13 192 ops/s (0.73×) — the only inversion in 15 matched pairs. Every one of the
three pooled runs falls below every one of the three churn runs, so the ordering
is not a median artefact:

| | run 1 | run 2 | run 3 |
|---|---:|---:|---:|
| churn ops/s | 13 050 | 13 192 | 13 694 |
| pooled ops/s | 9 569 | 11 091 | 9 410 |

The pooled arm is also the noisier of the two here (a ~18% spread against ~5%),
which is itself consistent with contention rather than with a steady cost.

A plausible mechanism is that connection churn **accidentally throttles
concurrency**: with a 128-deep accept backlog, 256 churning clients cannot all be
in the engine at once, whereas 256 pooled connections each hold a live server-side
goroutine and all press on the engine simultaneously. That would make the churn
arm's apparent advantage an admission-control artefact rather than an engine
property.

**This mechanism is a hypothesis and has not been tested.** Naming a cause is not
diagnosing one; it is recorded here as an open question, not as a finding.

## Reproduce

```bash
# Wait for the host to settle first — check `uptime`; a 1-minute load average
# above ~2.5 biases concurrency ratios systematically.
go test -run='^$' -bench=BenchmarkBolt -benchmem -count=3 \
  -timeout=7200s ./bench/soak/ > sweep.out 2> sweep.err

# Just the matched pair that isolates the handshake:
go test -run='^$' -bench='(BenchmarkBoltReadOnly|BenchmarkBoltPooledRead)/conc=' \
  -benchmem -count=3 ./bench/soak/
```

The harness now installs a discard `slog.Logger` on the benchmark server and
builds its graph as a multigraph. Both are readability fixes, not measurement
changes: the engine's non-multigraph warning and the server's no-auth/no-TLS
warnings are emitted once per server construction, and the benchmark constructs
one per sub-benchmark *and* per `b.N` trial, so they interleave with — and hid —
the `ns/op` values. Redirecting stdout and stderr separately does not help.
