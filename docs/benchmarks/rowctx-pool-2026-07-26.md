# Pooled RowContext width — measured, and the audit premise refuted

**Task #2188** · sprint 320 · 2026-07-26 · Apple M4 (10 cores, `darwin/arm64`) ·
`go test -run=^$ -bench='^BenchmarkRowCtxPool_Width<N>$' -benchmem -count=10 ./cypher/`,
**one width per process**

Benchmarks in `cypher/rowctx_pool_bench_test.go`. They are kept as the permanent
instrument for the per-row binding cost and the regression gate for tasks #2210 and
#2211.

---

## 1. The claim under test

The round-3 audit's finding F3 proposed a "Tier A (cheap, self-contained)" lever:

> size the pooled map to the actual schema width — bucket `rowCtxPool` by width
> (1/2/4/8/16) instead of always handing out a cap-16 map. Measured headroom ≈ **28
> ns/row**, no API change, no semantic surface.

The reasoning was sound in form: `acquireRowCtx` `clear()`s the recycled map on every
row, and Go's map `clear` is O(capacity), not O(len) — so an over-sized pooled map
would make a one-variable query pay to clear sixteen slots per row. The audit's isolated
attribution supported it (map cap 16 + clear 67.2 ns; map cap 1 + clear 46.0 ns).

## 2. The premise is false

The pool's `New` never made a cap-16 map. It made a **cap-8** one:

```go
var rowCtxPool = sync.Pool{New: func() any { return &pooledRowCtx{ctx: make(expr.RowContext, 8)} }}
```

The cap-16 construction the audit cited lives in `acquireRowCtx`'s `if p == nil`
fallback, which is **unreachable**: `sync.Pool.Get` does not return nil while `New` is
set. The audit read the dead branch as the live path.

That matters because Go serves any map hint of 8 or below from a **single group**. A
`make(map, 1)` and a `make(map, 8)` therefore have the same physical capacity, so
clearing them costs the same. For the common 1–8 variable query there was no
over-clearing to recover — the live capacity was already the single-group size.

## 3. Measured

The bucketed-pool change was implemented in full (per-class `sync.Pool`s keyed on the
smallest width class fitting the schema, with the unit carrying its class so release
cannot cross-pool it) and measured against the unchanged code.

**Each width was run in its own process.** That is essential here: with a single shared
pool, running the widths in one process lets each benchmark inherit maps sized by the
previous one, so a same-process comparison measures benchmark order rather than the
change. A first same-process run reported −75 % at width 1 and −43 % at width 32 — but
width 32 bypasses the pool entirely and cannot be affected by any pool change, which is
what exposed the artefact.

Isolated, `-count=10`:

| Width | before | after | verdict |
|---|---|---|---|
| 1 | 80.36 ns ± 24 % | 78.92 ns ± 1 % | ~ (p=1.000) |
| 2 | 98.52 ns ± 3 % | 99.64 ns ± 1 % | ~ (p=0.105) |
| 4 | 137.4 ns ± 2 % | 137.5 ns ± 1 % | ~ (p=0.588) |
| 8 | 206.3 ns ± 2 % | 209.9 ns ± 1 % | +1.74 % (p=0.023) |
| 16 | 525.7 ns ± 2 % | 531.4 ns ± 2 % | ~ (p=0.315) |
| 32 (bypasses the pool) | 1.211 µs ± 2 % | 1.211 µs ± 2 % | ~ (p=0.617) |
| **geomean** | 228.7 ns | 229.5 ns | **+0.35 %** |

Zero allocations per operation on both sides at every pooled width, so this is CPU only.

**No width improves.** Width 16 is unchanged too: the old pool handed it a cap-8 map that
grew on first use and was then recycled already-grown, so steady state was identical.

## 4. What was done instead

The bucketed pool was **not merged** — a change with no measured benefit does not belong
in a hot path, and the project's benchmarking mandate cuts both ways.

Kept instead:

- **The benchmarks**, as the standing instrument for per-row binding cost and the
  regression gate for #2210 (positional binding) and #2211 (typed long lane).
- **A correction at the source.** `rowCtxPool`'s capacity is now the named constant
  `rowCtxPoolCap = 8` with a comment stating why 8 and not 16, recording this refutation
  and pointing at #2210 for the cost that *is* real. The unreachable fallback now says
  so, and mirrors `rowCtxPoolCap` rather than `rowCtxPoolMaxSchema`, so the misreading
  cannot recur.

## 5. What remains real

F3's Tier B is untouched by this refutation and is where the cost actually lives: the
per-row map iteration and hash writes, plus the hash read per `ast.Variable`. The audit
measured that mechanism against positional access at 70× (one variable, one read), 42×,
37× and 29× at widths 1/1, 1/2, 4/4 and 8/8. The table in §3 shows the same shape from
the real code path — 80 ns at one variable rising to 526 ns at sixteen — and none of it
is the `clear()`.

That work is task **#2210** (plan-time positional binding), with **#2211** (typed long
lane) depending on it. Task #2188 was rescoped to Tier A alone and is closed by this
refutation.
