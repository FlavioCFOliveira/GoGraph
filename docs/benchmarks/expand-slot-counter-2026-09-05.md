# `Expand` counts the adjacency slots it walked — cost measured

**Task:** rmp #2761 (sprint 355) · **Date:** 2026-09-05 · **Machine:** Apple M4
(4P+6E, 10 cores), darwin 25.5.0, go1.27.1 darwin/arm64 · **Baseline commit:**
`cb1304cc` · **Build mode:** NO `-race` (allocation counts differ under `-race`)

## What changed

`Expand` carried `exec.StorageRecordScan`, the marker asserting one storage record
read per row emitted. It is false for this operator: `Expand` walks every slot of
the source's adjacency run and emits only the slots the relationship-type filter,
the cyphermorphism check and the expand-into comparison admit. A type filter
admitting one edge in a hundred therefore reported one db-hit for a hundred-slot
walk (`docs/explain-profile-honesty-audit-2026-09-03.md` §3, refutation 2).

`Expand` and `OptionalExpand` now implement `storageAccessCounter` instead;
`columnarExpand` inherits it through its embedded `*Expand`.

**The counter is maintained unconditionally**, which departs from the
"no counting code at all when off" property `StorageRecordScan` documents. That is
why it had to be measured. It is affordable because it is **not a per-slot
increment**: the expansion cursors already advance one position per slot consumed,
so the count is recovered from the cursor positions in **O(1) per input row** —
two subtractions and an add in `Expand.closeSlotWindow`, one forward and one
reverse — plus the same again when the figure is read.

## Method

- **Three arms interleaved in the same rounds**, `BEFORE, AFTER, AFTER2` per round,
  eight rounds, `-benchmem -count=1` per invocation, pooled to n=8 per arm and
  compared with `benchstat`. Interleaving in one loop means the noise floor and the
  signal share thermal and scheduling conditions rather than being measured at
  different times.
- **BEFORE** is a `go test -c` binary built from `git show HEAD:` versions of
  `cypher/exec/{expand,optional_expand,profile}.go`. **AFTER** and **AFTER2** are
  two independent builds of the identical working tree, so `AFTER vs AFTER2` is the
  noise floor and `BEFORE vs AFTER` is the signal.
- **Host was NOT idle.** Load average ran 1.5–3.0 throughout on a 10-core machine,
  from another user's desktop session (iTerm2, `osascript`, `system_profiler`,
  `WindowServer`). Every round's `uptime` is recorded in the raw `.meta` files. The
  timing conclusion below is therefore stated only against the measured noise
  floor; the allocation conclusion is load-invariant and stands on its own.
- Exit status of every benchmark invocation was appended to the `.meta` file and
  read from there, never from a pipeline's exit code. All 40 invocations exited 0.

## Result

**Allocations and bytes: IDENTICAL.** In all 14 `cypher/exec` benchmarks and all 4
`cypher` engine benchmarks, `allocs/op` and `B/op` compare `~` and benchstat
reports "all samples are equal" for every allocation row. The change allocates
nothing.

**Time: no regression, and nothing above the noise floor.**

| Comparison | `sec/op` geomean | Benchmarks flagged significant |
|---|---|---|
| Noise floor — `AFTER` vs `AFTER2` (identical source) | −0.01% | 1 (`ExpandFilter_RowMode` −1.50%, p=0.038) |
| Signal — `BEFORE` vs `AFTER` (`cypher/exec`, n=8) | **−0.56%** | 2, both FASTER (`ExpandFilter_Columnar` −2.00% p=0.010; `ExpandOut_PerEdge_SingleSource` −2.33% p=0.050) |
| Signal — `BEFORE` vs `AFTER` (`cypher` engine, n=8) | **−0.08%** | 0 |

The noise floor is the control that makes the signal readable: two builds of the
**same source** produced one "statistically significant" −1.50% at p=0.038, so a
2%-scale delta at p≈0.01–0.05 is what this host produces from nothing. Both flagged
signal deltas are in the faster direction and of that magnitude, so they are read as
noise, not as a speedup. **No benchmark in either package regressed.**

## Reproduction

```sh
# BEFORE binary
git show HEAD:cypher/exec/expand.go          > cypher/exec/expand.go
git show HEAD:cypher/exec/optional_expand.go > cypher/exec/optional_expand.go
git show HEAD:cypher/exec/profile.go         > cypher/exec/profile.go
rm cypher/exec/expand_slotcount_test.go
go test -c -o exec_BEFORE.test ./cypher/exec/
go test -c -o cypher_BEFORE.test ./cypher/
# (restore the working tree, then)
go test -c -o exec_AFTER.test  ./cypher/exec/
go test -c -o exec_AFTER2.test ./cypher/exec/
go test -c -o cypher_AFTER.test ./cypher/

# eight interleaved rounds, appending each invocation to its own arm's file
for i in $(seq 1 8); do
  ./exec_BEFORE.test -test.run=XXXNONE -test.bench=BenchmarkExpand -test.benchmem -test.count=1 >> before.txt
  ./exec_AFTER.test  -test.run=XXXNONE -test.bench=BenchmarkExpand -test.benchmem -test.count=1 >> after.txt
  ./exec_AFTER2.test -test.run=XXXNONE -test.bench=BenchmarkExpand -test.benchmem -test.count=1 >> after2.txt
done
benchstat after.txt after2.txt   # noise floor
benchstat before.txt after.txt   # signal
```

Raw benchmark output, the per-round `uptime` and exit-status logs, and the three
`benchstat` reports are in
[`expand-slot-counter-2026-09-05-raw/`](expand-slot-counter-2026-09-05-raw/).
