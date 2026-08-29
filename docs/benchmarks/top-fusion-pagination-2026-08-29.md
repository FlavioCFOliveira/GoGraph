# Fusing `ORDER BY … SKIP … LIMIT` into `Skip` over `Top` — measured

**Task:** rmp #2509 · **Date:** 2026-08-29 · **Machine:** Apple M4 (4P+6E, 10 cores),
darwin 25.5.0, go1.27.0 darwin/arm64 · **Baseline commit:** `c664395a`

## What changed

Two planner gaps and one operator that was not safe to open them on.

1. **A parameterised bound lost the fusion its identical literal received.**
   `ORDER BY … LIMIT 5` planned as `Top`; `ORDER BY … LIMIT $m` planned as
   `Limit` over `Sort`. `StripLiterals` clears the hoistable flag at SKIP/LIMIT,
   so `LIMIT 10/20/30` occupy three plan-cache entries while `LIMIT $m` occupies
   one — the spelling the engine recommends was the spelling that planned worse.
2. **Any SKIP blocked the fusion**, literal or not, so the ordinary pagination
   idiom always sorted the whole input. `SKIP 0 LIMIT 10` full-sorted 120 000
   rows to ship 10.
3. `exec.Top` had **neither a row cap nor a byte budget**, which was masked only
   because the bound was always a small literal. Opening the fusion to parameters
   without the caps would have turned a hostile `$skip` into an out-of-memory kill
   where `Sort` returns `ErrSortMemoryExceeded`.

The plan shape is now `Skip(s)` over `Top(s+k)` — Neo4j's shape, which keeps both
clauses visible in EXPLAIN. The operator accumulates arrivals until the buffer
reaches `2n` before it builds a heap (PostgreSQL's tuned bounded-sort heuristic),
compares each later arrival before copying it, and carries an arrival ordinal so
its output is the stable `Sort` order truncated to `n` rather than merely the same
set of rows.

## Method

- **Interleaved A/B across two builds**, three rounds of `A,B,A,B,A,B`, `-count=2`
  each, pooled to `n=6` per arm and compared with `benchstat`. The BEFORE build is
  a `git worktree` at `c664395a` carrying the identical benchmark file; the AFTER
  build is the working tree.
- **In-build invariant control:** the `unlimited_sort` arm (`ORDER BY` with no
  bound) is untouched by this change. It reads `~` in time, bytes and allocations
  across the two builds, so the comparison is not measuring a build or environment
  difference.
- **Noise floor**, measured first by running the identical HEAD binary against
  itself with the same methodology: geomean **+0.15 %**, worst single arm
  **+0.77 %**. A delta under roughly 1 % is not a finding.
- Host load average was between 2.4 and 6.0 throughout (the benchmark itself is
  most of it); no run was taken with a competing workload beyond the OS's own
  indexer.

Commands:

```
go test -run '^$' -bench BenchmarkPagination -benchmem -benchtime=30x -count=2 ./bench/audit352/
go test -run '^$' -bench BenchmarkBoundedOrder -benchmem -benchtime=20x -count=6 ./cypher/exec/
benchstat before.txt after.txt
```

## Query level — `bench/audit352` `BenchmarkPagination`

120 000 `:Person` nodes, 55 000 duplicate salaries, ordering on a non-projected
property so the sort key is evaluator-backed.

| arm | plan before → after | sec/op | B/op | allocs/op |
|---|---|---|---|---|
| `skip0_limit10` | `Limit,Skip,Sort` → `Skip,Top` | **−52.02 %** | **−15.33 %** | −6.65 % |
| `skip100_limit10` | `Limit,Skip,Sort` → `Skip,Top` | **−51.40 %** | **−15.22 %** | −6.60 % |
| `skip10000_limit10` | `Limit,Skip,Sort` → `Skip,Top` | **−34.25 %** | **−12.21 %** | −4.62 % |
| `limit10` | `Top` → `Top` (operator only) | −2.02 % | −2.90 % | −6.64 % |
| `limit110_noskip` | `Top` → `Top` (operator only) | −2.13 % | −2.90 % | −6.62 % |
| `skip100000_limit10` | `Limit,Skip,Sort` → `Skip,Top` | ~ | ~ | ~ |
| `unlimited_sort` *(control)* | `Sort` → `Sort` | ~ | ~ | ~ |
| **geomean** | | **−24.02 %** | **−7.17 %** | −4.49 % |

Every delta except the control and the `skip100000` arm is significant at
`p = 0.002`, `n=6`.

`skip100000_limit10` is deep pagination — the fused bound is 100 010 of a
120 000-row input, the regime in which a bounded operator has least to gain. It
was added to this table precisely so the trade-off would be measured rather than
assumed, and it caught one: the first working version of this change was
**+4.55 % on time and +24.23 % on bytes** there. It is now at parity in all three
dimensions; see "The one vector that is still up" for what closing it took.

## Operator level — `cypher/exec` `BenchmarkBoundedOrder`

`Sort → take(n)` against `Top(n)` over the same 120 000 rows, run with the same
interleaved A/B/A/B/A/B methodology. `Sort` is the in-build control.

| n | Top before | Top after | Δ time | Top B/op before → after | Δ allocs |
|---|---|---|---|---|---|
| 10 | 2.372 ms | 1.429 ms | −39.78 % | 3.67 MiB → 3.95 KiB | −99.97 % |
| 110 | 2.378 ms | 1.436 ms | −39.63 % | 3.70 MiB → 38.1 KiB | −99.75 % |
| 10 010 | 7.522 ms | 4.895 ms | −34.93 % | 7.65 MiB → 4.02 MiB | −83.30 % |
| 60 000 | 39.22 ms | 9.085 ms | −76.84 % | 27.31 MiB → 25.73 MiB | −59.99 % |
| 120 000 | 56.82 ms | 8.866 ms | −84.40 % | 51.26 MiB → 22.06 MiB | −74.99 % |

`Sort` measured 8.55–8.81 ms and 22.06 MiB throughout, and read `~` across the
two builds on four of its five arms (the fifth, −1.34 %, is the only reason to
read any Top delta under about 2 % with suspicion; none of them is).

Two numbers are worth stating plainly.

- **The task predicted `Top` at `n` close to `M` cost "~3x a plain Sort". At HEAD
  it measured 6.45x at `n == M` and 4.54x at `n == M/2`** — the prediction was
  understated. It is now **1.01x** at `n == M` (8.866 ms against Sort's 8.774 ms,
  22.06 MiB against 22.06 MiB: byte-identical) and 1.05x at `n == M/2`.
- **Allocations at a small bound fell by 99.97 %** — 120 045 allocations to
  retain 10 rows, down to 42 — because the operator no longer copies every input
  row before deciding whether to keep it, and because the accumulation path no
  longer boxes a heap entry into an `any` per push and per pop, which
  `container/heap`'s signature had forced.

## The one vector that is still up

At `n == M/2` the operator allocates 25.73 MiB against `Sort`'s 22.06 MiB
(+16.6 %), though still −5.81 % against the operator before this change. That is
the `2n` accumulation heuristic's declared trade: the buffer transiently holds
`min(2n, M)` rows before it is cut to `n`, where the old heap-from-the-first-row
shape held exactly `n`. It buys −76.84 % on time at the same point, and both the
row cap and the new retained-byte budget bound it. It does not surface at query
level: the `skip10000` arm is −12.21 % on bytes and the `skip100000` arm is at
parity.

Four intermediate designs were measured and rejected on the way here, each
because it moved a resource vector the wrong way:

1. Buffering `topEntry` values (64 bytes) rather than `Row` headers (24 bytes)
   during accumulation: **+24.23 % B/op** on the deep-pagination arm.
2. Heapifying eagerly at the threshold rather than on the first arrival that
   needs a heap: a threshold landing on the last input row paid a second full
   sort for nothing — 23.00 ms against 8.9 ms at `n == M/2`.
3. Comparing through each entry's key slice header rather than through the flat
   key arena: 22.80 ms against `Sort`'s 8.88 ms at `n == M`, a random
   64-byte-strided load per operand.
4. Growing the key arena by append as each row was buffered, rather than sizing
   it once when the arrivals stop: 30.99 MiB against `Sort`'s 22.06 MiB at
   `n == M`, and +6.21 % B/op on the deep-pagination arm at query level.

## Reproduction

Raw benchstat output, both runs, and the noise-floor run are reproduced by the
commands above. The benchmark arms and their asserted plans live in
`bench/audit352/rowcost_test.go` (`paginationShapes`, `TestPaginationPlans`) and
`cypher/exec/top_bound_bench_test.go` (`BenchmarkBoundedOrder`).
