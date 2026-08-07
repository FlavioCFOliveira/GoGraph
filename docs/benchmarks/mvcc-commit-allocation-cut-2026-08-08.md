# Cutting the commit's allocation rate — rmp #2339

**Date** 2026-08-08 · **Sprint** 335 (MVCC) · **Parent** `232be262` · **Host** Apple M4
(10 cores: 4P+6E), darwin/arm64, go1.26.5 · **Method** two `go test -c` binaries run
alternately in one loop, `benchstat`, n=10 unless stated.

## Headline

One autocommit `CREATE (n:Account {id: $id})` went from **56 allocations / 4248 B** to
**43 allocations / 4159 B**. Nothing was traded for it: latency improved at every
writer count and the read path is byte-identical.

| | parent `232be262` | rmp #2339 | change |
|---|---:|---:|---:|
| allocs/op, 1 writer | 56.00 ± 0% | 43.00 ± 2% | **−23.21%** (p=0.000) |
| allocs/op, 32 writers | 54.00 ± 0% | 41.00 ± 0% | **−24.07%** (p=0.000) |
| B/op, geomean | 4.019 KiB | 3.905 KiB | −2.85% (p=0.000) |
| ns/commit, 1 writer | 2891 ± 1% | 2696 ± 1% | **−6.75%** (p=0.000) |
| ns/commit, geomean | 1598 | 1531 | −4.19% (p=0.000) |

The durable (`wal`) wiring collects the same allocation cut — 65 → 52 allocs/op at one
writer, **−19.66% geomean** — and, as expected of an fsync-bound path, no latency
change: geomean +0.56%, every writer count p ≥ 0.09 except a −0.96% at four.

## THE PREMISE THIS TASK WAS GIVEN IS REFUTED

rmp #2339 was scoped on the claim that *"the only lever that raises the CEILING ITSELF
rather than the constant is reducing allocations per commit"*. **It is not.** Cutting
the allocation rate raises the constant and leaves the ceiling exactly where it was.

`BenchmarkAllocScalingControl` is the instrument that decides this: N goroutines each
allocating one commit's exact profile, padded by a non-allocating spin to one commit's
exact cost, **sharing nothing** — no map, no counter, no lock, no critical section.
Whatever it reaches is the ceiling an allocation-matched, perfectly parallel Go program
achieves on this host. The two arms below are the SAME control code compiled with the
old profile constants (56 objects / 4242 B / 2892 ns) and with the re-calibrated ones
(43 / 4151 / 2690), run interleaved on the same machine:

| control, writers | old profile | re-calibrated | change |
|---|---:|---:|---:|
| scaling @32 | 2.718 ± 3% | 2.713 ± 5% | ~ (p=1.000) |
| scaling geomean | 2.135 | 2.138 | ~ (+0.15%) |
| commits/s @32 | 934.5k ± 3% | 1012.9k ± 6% | **+8.38%** (p=0.000) |
| commits/s geomean | 735.2k | 794.9k | **+8.12%** (p=0.000) |

Cutting the per-goroutine allocation rate by 17.5% (56 objects per 2892 ns → 43 per
2690 ns) moved the ceiling **not at all** — p=1.000 at thirty-two writers — while
raising absolute throughput 8.1% at every writer count. The ceiling on this host is
~2.7× and is insensitive to the allocation rate over this range; what the allocation
rate buys is a constant factor, which is real and which the engine collected.

### And the engine collected it LESS COMPLETELY as writers rise

| engine, writers | parent | rmp #2339 | change |
|---|---:|---:|---:|
| scaling @2 | 1.593 ± 1% | 1.565 ± 2% | −1.76% (p=0.011) |
| scaling @8 | 2.069 ± 2% | 1.991 ± 3% | −3.77% (p=0.002) |
| scaling @16 | 2.210 ± 2% | 2.115 ± 1% | −4.25% (p=0.000) |
| scaling @32 | 2.221 ± 2% | 2.133 ± 3% | −3.94% (p=0.000) |

**This is not a regression — throughput rose at every writer count.** The scaling
factor is throughput(N)/throughput(1), and the single-writer denominator moved 6.75%
while the saturated numerator moved 3.03%, so the ratio falls by arithmetic. It is the
same shape rmp #2338 recorded for the delete-the-label-subsystem arm, and it means the
same thing: **per-operation COST, not contention.**

What it does say, and this is the finding worth carrying forward:

> At thirty-two writers the allocation-matched control gained **+8.38%** from the same
> allocation cut and the engine gained **+3.1%**. The engine-versus-ceiling ratio
> therefore FELL, from 2.221/2.718 = **81.7%** to 2.133/2.713 = **78.6%**.
> Allocation is no longer the binding constraint at saturation; something else is.

The candidates are the ones rmp #2338 named and did not ablate, now worth the ladder
they were deferred for: the mapper's intern path, `mvcc.Clock.finishCommitTS`'s
process-global `pubMu` (taken on every commit's publish), the plan cache, and the count
store.

## What was removed, and where the objects were

Attribution is per SOURCE LINE, from `-memprofile` at `-memprofilerate=1` over 200 000
commits of `BenchmarkWriteScaling/mem/writers=1`, before and after. Every line below
is absent from the after-profile.

| allocs/commit | site | change |
|---:|---|---|
| 3.00 | `cypher/undo.go:104` — `append(u.inverses, inv)` | `undoLog` carries `inline [4]func()`; a CREATE records exactly 3 inverses and used to grow the nil slice through caps 1, 2, 4 |
| 3.00 | `cypher/exec/create_node.go:251` — `var childRow Row` | the local's address escaped through the child's interface call on every `Next` (3 per statement); now a `CreateNode.childRow` field |
| 2.00 | `cypher/exec/index_writeback.go:19` — `append(b.changes, c)` | `IndexBuffer` carries `inline [2]index.Change`; a CREATE enqueues exactly 2 |
| 2.00 | `cypher/api.go` — `&exec.IndexBuffer{}`, `&undoLog{}` | both now ride INSIDE the mutator adapter as `bufStore`/`undoStore`, the way `countersStore` already did |
| 2.00 | `cypher/api.go` — `&lpgNodeWalker{}`, `&lpgLabelResolver{}` | the adapter carries a `buildScratch` the bracket binds to the statement's writer view |
| 1.00 | `cypher/stmt_now_reg.go:31` — `newNowAwareRegistry` | the adapter carries the wrapper inline; the frozen instant is still read at the top of the statement, unchanged |
| **13.00** | | |

Two of the removals also cut bytes materially: the undo ladder was 56 B/commit and the
index-change ladder 192 B, against 32 B and 128 B added inline to objects the statement
already allocated.

`clear()` was added to `undoLog.replay` and to a new `IndexBuffer.reset` because the
backing array is now usually part of the owning struct: truncating the slice header
alone would pin every inverse closure and every drained `index.Change` — which carries
its old and new property values as `any` — for as long as the adapter lives.

## What was deliberately LEFT, and why

43 objects remain. Named here so the next pass starts from the ledger rather than
re-deriving it.

**~3.00 belong to the benchmark, not the engine.** `bench/mvccwrite.commit` builds a
fresh `map[string]expr.Value` per call (`scaling_test.go:215`, `:217`). Any headline
"allocs per commit" figure must exclude it or it claims a win that belongs to the
harness. The engine's own cost is therefore **40**.

**~16.00 are the per-statement PLAN BUILD, and removing them is architectural.**
`buildPlanWithMutatorFull` ×4 (the schema map, the arg-by-tag map, `bopts`, the
write-fallback closure), `buildOperatorWrite`, `buildPropsEvalFn` ×3 plus its closure,
`buildRowCtxFromMutator`, `copySchema`, `NewCreateNode` ×2, `NewSingleRowOperator`,
`parsePropLiteralWithParamsCtx`, `splitMapItems` ×2. The read path has a plan cache;
the write path does not, because the operator tree it builds has the statement's
mutator and its transaction-bound views wired into it. **rmp #2339's own technical
requirements rule this out of a sweep** — it is a change to the write path's
architecture and must be scoped and decided as one.

Two of these were probed and rejected on evidence rather than left untried:

- `splitMapItems` (2.00) — pre-sizing does not help. The 2.00 is the function being
  called TWICE at one allocation each, not once at two; the benchmark's statement
  carries ONE property, so a pre-sized slice allocates exactly as often. Measured
  interleaved at n=5: 56.00 allocs/op before and after, p=1.000. See the "read the
  ledger COLUMN, not the row" note in `mvcc-write-ceiling-attribution-2026-08-07.md`.
- `bopts` (1.00) — it caches a forward-CSR snapshot (`ensureFwdCSR`). Reusing one
  across the statements of an explicit transaction would carry a stale CSR into the
  next statement. It can only move inline behind an explicit per-statement reset, which
  belongs with the plan-cache work that has to reason about that lifetime anyway.

**~5.6 are MVCC's own version records** — `pushPropDelta` and `pushLabelDelta` (1.89
each) plus the amortised growth of their shard maps and of `noteNodeLife`'s birth map
(0.89 each). These are the substrate's cost of isolation. They are the least likely to
be removable without weakening a guarantee, and they are the cost the module is
supposed to be paying.

**The remaining ~15 are one object apiece** at fifteen distinct sites —
`newResultWithLimit`, `exec.Run`, `mergeProps`, `copyLabels`, `reserveConstraintValue`,
`propBag.set`, `Int64Value`, `NewCommitInfo`, `freshNodeKey`, `ReadAt`,
`SetNodeProperty`, the three `mutationUndo.record*` closures, and the adapter itself.
Each is a genuine object with a genuine lifetime; there is no ladder or escape artefact
left among them, and removing any one is a 2.3% change requiring its own justification.

## Correctness

- openCypher TCK execution: **3897 scenarios, 3897 passed, 0 failed, 0 undefined**
  (baseline 3897). Unchanged.
- `go test -race` green on `cypher`, `cypher/exec`, `graph/lpg`, `graph/index`.
- Read path unaffected: `bench/cypher_alloc` at n=6 is **byte-identical** in allocs/op
  and B/op across all four benchmarks (p=1.000, all samples equal) and statistically
  unchanged in latency (geomean −0.69%, every arm p ≥ 0.165).

## Reproducing

```
go test -c -o ws_HEAD.test ./bench/mvccwrite/          # at the parent commit
go test -c -o ws_ARM.test  ./bench/mvccwrite/          # with the change
for i in $(seq 1 10); do
  ./ws_HEAD.test -test.run='^$' -test.bench='BenchmarkWriteScaling/mem' \
      -test.benchtime=200000x -test.benchmem -test.count=1 >> head.txt
  ./ws_ARM.test  -test.run='^$' -test.bench='BenchmarkWriteScaling/mem' \
      -test.benchtime=200000x -test.benchmem -test.count=1 >> arm.txt
done
benchstat head.txt arm.txt
```

The control's two arms are the same command against `BenchmarkAllocScalingControl` at
`-test.benchtime=20000x`; the difference between the binaries is only the
`allocsPerCommit` / `bytesPerCommit` / `nsPerCommitTarget` constants in
`bench/mvccwrite/alloc_control_test.go`, which **must be re-measured whenever the write
path's allocation profile changes**.

Per-line attribution:

```
./ws_ARM.test -test.run='^$' -test.bench='BenchmarkWriteScaling/mem/writers=1$' \
    -test.benchtime=200000x -test.count=1 \
    -test.memprofile=arm.mprof -test.memprofilerate=1
go tool pprof -sample_index=alloc_objects -lines -top -nodecount=40 ws_ARM.test arm.mprof
```
