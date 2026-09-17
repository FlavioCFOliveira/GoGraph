# Plan-cache hit rate of the four parsing-corpus examples

rmp **#2851**, sprint 362. Measured **before** the plan cache was sharded
(rmp #2852), on the code at the commit named below, so the number describes the
cache the campaign profiled.

## Question

What ratio of `cypher.plan_cache.hits` to `cypher.plan_cache.misses` does each
of the four examples the parsing corpus was harvested from actually produce?

No hit rate from any real workload had been recorded anywhere in this project.
The counters existed and were exercised only by unit tests and by
`examples/31_metrics_observability`, which drives them deliberately rather than
as a by-product of a workload.

## Result

| Example | workload | hits | misses | hits/misses | hit % | evictions | invalidations |
|---|---|---:|---:|---:|---:|---:|---:|
| `examples/19_pattern_query` | documented default, one process | **85** | **4** | **21.2500** | **95.51%** | 0 | 2 |
| `examples/22_cypher` | documented default, one process | **14** | **17** | **0.8235** | **45.16%** | 0 | 0 |
| `examples/24_social_network_cli` | documented End-to-end session, **five processes** | **0** | **9** | **0.0000** | **0.00%** | 0 | 0 |
| `examples/25_software_house_api` | documented session (startup + README request catalogue) | **0** | **34** | **0.0000** | **0.00%** | 0 | 2 |

Example 24 is five separate `go run` invocations in the README, so it is
reported per process as well as in total:

| `24_social_network_cli` step | hits | misses |
|---|---:|---:|
| `init  -d $DATA_DIR` | 0 | 0 |
| `seed  -d $DATA_DIR` | 0 | 0 |
| `stats -d $DATA_DIR` | 0 | 8 |
| `query -d $DATA_DIR 'MATCH (u:User) RETURN u.username AS username ORDER BY username'` | 0 | 1 |
| `snapshot -d $DATA_DIR` | 0 | 0 |
| **total** | **0** | **9** |

Example 25 is a server, so "its documented default" has two readings and both
were taken, each in its own process and its own fresh data directory:

| `25_software_house_api` reading | hits | misses | invalidations |
|---|---:|---:|---:|
| `startup` — `-d <dir> -addr :8080`, no scale flags, no request | 0 | 0 | 0 |
| `session` — the same startup plus the README request sequence | 0 | 34 | 2 |

Every counter value above is an absolute reading, not a derived figure. Each is
the difference between a read of all four counters taken **before** the workload
started and a read taken **after** it finished, in the same process; the raw
before-and-after pairs are in `planwatch.raw.txt`.

## What it settles

The campaign ranked the `StripLiterals` keyword fold (`cb55c3c9`) above two-stage
parsing (`1630e9f6`) on a **97.62% break-even**: the fold saves 30.4 ns on every
execution, hit or miss, while two-stage parsing saves 1,274 ns on every miss, so
the fold wins only above a 97.62% hit rate.

**No example reaches it.** The measured per-execution saving of the two
optimisations, at the hit rate each example actually produces:

| Example | hit rate | fold saves | two-stage saves | which is larger |
|---|---:|---:|---:|---|
| `19_pattern_query` | 95.51% | 30.4 ns | 57.3 ns | two-stage, 1.9× |
| `22_cypher` | 45.16% | 30.4 ns | 699.1 ns | two-stage, 23.0× |
| `24_social_network_cli` | 0.00% | 30.4 ns | 1274.0 ns | two-stage, 41.9× |
| `25_software_house_api` | 0.00% | 30.4 ns | 1274.0 ns | two-stage, 41.9× |

The T1-before-T2 ordering rested on an assumption that the evidence refutes on
all four examples. Both optimisations landed, so nothing has to be undone; what
is refuted is the **rationale**, and the correct reading for future ranking is
that **miss-path cost dominates on every workload this project has measured**.

## The honest limit of the number

These are the hit rates of the examples **as documented**, and the examples are
one-shot demonstrations, not steady-state servers. The plan cache pays off only
from the *second* execution of one query text, and:

- 19 reaches 95.51% because `registry_search.go` re-runs the same statements to
  time them;
- 22 reaches 45.16% because a handful of its statements are re-executed by the
  write battery's read-backs;
- 24 reaches 0% because the README runs each subcommand as its own process, so
  every process starts with a cold cache and issues each of its statements once;
- 25 reaches 0% because the documented request catalogue issues each of its
  statements exactly once, even though the process is long-lived.

A production deployment reissuing the same statements would sit far higher. The
number here is therefore **not** a claim about GoGraph in production; it is the
measured hit rate of the four workloads the parsing corpus was harvested from,
which is exactly the population the ranking's break-even was applied to.

A second consequence matters for #2852: a **miss takes the plan-cache lock
twice** (once in `get`, once in `loadOrStore`). On three of these four workloads
almost every lookup is a miss, so the contention the sharding removes is paid on
the double.

## Method

- The examples were **not modified**. The observation harness is injected into
  the build with `go test -overlay`, so the file exists only inside the compiler's
  view and the example's own directory is never written to. `git status` was
  clean before and after.
- The harness installs a counting `metrics.Backend`, which is the only way these
  counters are observable (the default backend is a no-op).
- Each observation is **bracketed**: all four counters are read before the
  workload and again after it, and both readings are printed.
- Each example ran at its own documented default, with the process boundaries
  the README documents.
- The harness sources are archived here as
  `harness-planwatch-19.go.txt`, `-22`, `-24`, `-25` and the overlay map as
  `harness-planwatch-overlay.json.txt`.

## Reproduction

```sh
# from the repository root, with the archived harnesses restored to a scratch
# directory and overlay.json pointed at them:
go test -overlay <scratch>/overlay.json -c -o <scratch>/19.test ./examples/19_pattern_query/
( cd examples/19_pattern_query && <scratch>/19.test -test.run '^TestZZPlanWatch19$' -test.count=1 -test.v )
# ... likewise for 22; for 24 and 25 set GG_PLANWATCH_STEP and GG_PLANWATCH_DIR
```

Raw output: `planwatch.raw.txt`. Environment: `planwatch.env.txt`.

## Environment

| | |
|---|---|
| commit | `1630e9f68437caaa162ef7a7d50e4fdaf70c53e5` |
| branch | `feature/362-performance-laboratory-20260915` |
| Go | `go1.27.1 darwin/arm64` |
| host | Apple M4, 10 cores, 32 GiB, Darwin 25.6.0 arm64 |
| build flags | none — no `-race`, no build tags |
| loadavg before | `1.51 1.70 2.47` |
| loadavg after | `1.48 1.68 2.45` |

Both readings sit inside this host's measured quiet envelope (median 2.19,
max 3.44), so the run is not contaminated. Counter values are exact integers and
are in any case insensitive to host load.
