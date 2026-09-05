# Slot-aligned relationship-type column — measured record

> **rmp #2251** (sprint 352). Baseline commit `35990293`, measured 2026-08-29.
> Host: Apple M4 (10 cores, 4P+6E), darwin/arm64, go1.27.0.
> Load average during every run: 1.6–3.5, recorded per run below. The host was
> **not idle** — a desktop session was live throughout — which is why every
> comparison here is INTERLEAVED and nothing rests on a single run.

## 1. What changed

Every typed expand used to test a slot's relationship type by probing a
`map[uint64]string` keyed by the slot's absolute FORWARD CSR position. The map
held one entry per accepted arc across the whole graph, and it was probed once per
CSR slot the traversal touched. The reverse direction was worse: a reverse slot
carries no type information, so the operator first RECOVERED the slot's forward
position (an `O(log d + r)` handle match, or an `O(log d)` lower bound) and only
then probed.

It is now a slot-aligned `[]uint32` column parallel to the arcs, in both
directions, tested against a bitmask over LabelID space. A type check is one
indexed load and one bit test, in either direction, with no position recovery.

## 2. Methodology

Two git worktrees — pristine `35990293` and the change — each compiled to its own
test binary with `go test -c`. Runs ALTERNATE `A,B,A,B,…` for six pairs; each
invocation is `-test.count=1`, so no arm ever runs two samples back to back.
`benchstat` compares the six samples per arm.

Interleaving is not optional on this machine. Sprint 313 established that a
back-to-back cross-commit A/B here manufactures significance: a byte-identical
control produced 22 of 36 "significant" rows spanning −11%…+4% (LEDGER row 0031).

**Two methodology errors were made and corrected during this measurement, and both
are recorded because either would have produced a confident wrong number:**

1. A `cd` into the baseline worktree persisted across the rest of a compound
   shell command, so an "A vs B" comparison ran BOTH arms in the baseline tree.
   It produced byte-identical deltas, which is what gave it away. Every later
   comparison runs each arm in its own subshell and asserts the two binaries
   differ by SHA-256 before measuring.
2. The benchmarks under `bench/csrorder` DRAIN their results without asserting
   them, so a speedup caused by emitting fewer rows would have been invisible.
   An absolute oracle was run over all 14 fixture/query combinations in both
   arms; every result was identical (`524288` on the degree sweep, and matching
   values on the power-law and RMAT fixtures). The oracle was a measurement
   instrument and was removed afterwards — it cost 7.1 s in a package that
   otherwise runs in 2.3 s.

## 3. Reverse and undirected typed traversal — the primary result

`bench/csrorder`, `-test.benchtime=1s`, 6 interleaved pairs, load 2.08–2.75.
Queries: `MATCH (t:Target)<-[:LINK]-(h:Hub) RETURN count(*)` and its undirected
form; the power-law and RMAT fixtures use `(a:Person)<-[:KNOWS]-(b:Person)`.

| benchmark | base sec/op | column sec/op | delta |
|---|---:|---:|---:|
| TraversalReverse degree=8 | 110.80m ± 9% | 43.48m ± 2% | **−60.76%** |
| TraversalReverse degree=16 | 92.57m ± 10% | 42.98m ± 1% | **−53.57%** |
| TraversalReverse degree=32 | 90.69m ± 5% | 41.64m ± 2% | **−54.08%** |
| TraversalReverse degree=64 | 85.77m ± 4% | 41.11m ± 1% | **−52.06%** |
| TraversalReverse degree=512 | 75.67m ± 4% | 38.42m ± 1% | **−49.23%** |
| TraversalReverse degree=4096 | 70.68m ± 3% | 34.49m ± 2% | **−51.20%** |
| TraversalUndirected degree=8 | 107.33m ± 11% | 42.32m ± 2% | **−60.57%** |
| TraversalUndirected degree=16 | 97.64m ± 7% | 43.09m ± 3% | **−55.87%** |
| TraversalUndirected degree=32 | 88.69m ± 3% | 42.06m ± 3% | **−52.58%** |
| TraversalUndirected degree=64 | 85.40m ± 4% | 41.26m ± 2% | **−51.68%** |
| TraversalUndirected degree=512 | 75.07m ± 2% | 38.15m ± 2% | **−49.19%** |
| TraversalUndirected degree=4096 | 69.74m ± 2% | 34.42m ± 2% | **−50.64%** |
| TraversalPowerLaw | 36.55m ± 3% | 22.55m ± 2% | **−38.29%** |
| TraversalRMAT | 181.92m ± 2% | 70.00m ± 1% | **−61.52%** |
| **geomean** | **85.84m** | **40.10m** | **−53.28%** |

Every row `p=0.002, n=6`. `allocs/op` is EXACTLY unchanged on every row (all
samples equal); `B/op` moves +0.02%, a fixed ~48 bytes per query.

**The power-law row is the one to quote for "what does this buy in practice"**
(−38.29%): the degree sweep deliberately includes degrees a real graph rarely
presents, and the RMAT fixture is documented in this project as overstating.

## 4. Forward typed one-hop

`bench/cypher_scale` (960 008 arcs), `-test.benchtime=1s`, 6 interleaved pairs,
load 2.47–3.51.

| benchmark | sec/op | B/op | allocs/op |
|---|---:|---:|---:|
| Expand1Hop | **−38.93%** (623.5m → 380.7m) | **−27.25%** | −2.36% |
| Expand1HopSelective_Warm | **−40.23%** (11.463m → 6.851m) | **−22.87%** | **−36.53%** |
| Expand1HopSelective_Cold | ~ (p=0.394) | **+11.95%** | −0.01% |
| geomean | −28.35% | −14.35% | −14.75% |

### The cold-engine allocation cost is real, and it is the change's one regression

`Expand1HopSelective_Cold` builds a fresh Engine per iteration and filters on
`:MENTORS` — **8 arcs out of 960 008, 0.0008%**. The retired map held only those 8
accepted positions; the column describes EVERY arc, because it is type-set
independent. At 960 008 arcs that column is 7.68 MB, and the measured cost is
+8.26 MiB/op. Wall time is unaffected (`p=0.394`).

This is the honest shape of the trade: the column is a whole-graph structure, so a
COLD engine answering a single very rare type pays for coverage it does not use. A
warm engine amortises it across every type set, which is why the warm row of the
same query improves on both time and bytes.

**Half of this regression was avoidable and was removed.** The first measurement
showed +22.55% B/op, against a predicted +7.32 MB from the column itself. The
excess was a `[]uint64` position mapping — 7.68 MB — that the transpose replay
materialised only to be walked once. `projectFwdToRevByTranspose` now hands each
validated pair to a callback instead, and the regression halved to +11.95%, which
matches the column's own size. The remaining cost is the column and nothing else.

## 5. Cyclic patterns, and the §7.1 counterfactual

`bench/cyclicjoin`, `-test.benchtime=1x`, 6 interleaved pairs, load 2.22–2.95.
Geomean **−9.85% sec/op**; every one of the 22 arms improves, `p ≤ 0.009` except
`TwoCycle/d=16/fused` (`p=0.065`). Largest: `Triangle_Uniform/d=64/twoexpand`
−25.72%, `NonQualifying/acyclic` −18.72%, `Triangle_SmallDegree/d=1/twoexpand`
−16.27%.

### The §7.1 prediction is NOT reproduced, and the reason is that it measured a
### plan that does not exist in this tree

`docs/design-wcoj-cyclic-patterns.md` §7.1 measured a **sorted-set-intersection**
plan — the SPIKE #2155 prototype, never built — against the binary plan on an
adversarial clique fixture, with a hand-built type column given to both arms. It
reported the ratio moving from 1.45×–1.56× to 4.40×–6.04×.

The in-tree cyclic operator is `ExpandIntersect` (#2157), a different plan, and
`bench/cyclicjoin` uses uniform/power-law/square fixtures rather than that clique.
So the specific 4.54×–6.29× ratio has no in-tree vehicle and was not reproduced.

What the fused-versus-two-expand ratio actually did, in place:

| shape | base ratio | column ratio |
|---|---:|---:|
| Triangle_Uniform d=4 | 1.61× | 1.53× |
| Triangle_Uniform d=16 | 4.20× | 3.60× |
| Triangle_Uniform d=64 | 10.16× | 8.22× |
| PowerLaw | 2.35× | 2.15× |
| TwoCycle d=16 | 2.01× | 1.78× |
| Square d=8 | 3.41× | 3.30× |

**The fused plan's advantage did not widen; it narrowed slightly**, because the
column speeds up the incumbent two-expand plan MORE than it speeds up the fused
one — the two-expand plan performs more per-slot type tests, so it had more to
gain. That is the opposite of the direction §7.1 extrapolated for its own
different plan, and it is reported as such rather than smoothed over.

What §7.1 got right and IS confirmed in place is its causal claim: *"a per-slot map
probe compresses the win in BOTH arms; it never inverts it."* Both arms speed up
here, and the fused plan stays ahead at every shape (1.53×–8.22×).

## 6. The primitive, corrected

The task carried a "~60×" figure for the lookup itself. Measured in place
(`cypher/exec`, 1 Mi arcs, half accepted, cache-hostile access order,
`-benchtime=2s -count=5`):

| primitive | ns/op |
|---|---:|
| `map[uint64]string` membership probe | 10.70 – 11.20 |
| column indexed load + bit test | 1.471 – 1.499 |
| **ratio** | **7.35×** |
| reverse: `firstDstPos` recovery + map probe | 5.204 – 5.246 |
| reverse: column indexed load + bit test | 1.802 – 1.820 |
| **ratio** | **2.88×** |

**The "~60×" figure is CORRECTED to 7.35×.** It is a primitive ratio and must never
be quoted as an end-to-end result: end to end the same change is −53.28% (2.14×)
on reverse/undirected traversal and −38.9%/−40.2% on the forward one-hop, because
a query does much besides test types. The reverse ratio measured here (2.88×)
understates the real reverse saving, since the synthetic loop keeps the recovery's
working set hot in a way a real traversal does not.

## 7. Resident memory

Measured with the identical probe compiled into both trees
(`cypher/reltype_memory_probe_test.go`), three interleaved repetitions, reporting
retained `HeapAlloc` after two collections with the Engine and graph still
reachable.

**Structural facts, asserted rather than estimated:**

| structure | footprint |
|---|---|
| type column | **exactly 8 B per arc** (4 B × 2 directions), asserted by `TestRelTypeColumnSize` |
| retired filter map | **34.95 B per ACCEPTED entry** (measured: 3 495 088 B for 100 000 entries) |

**Retained-heap delta, Engine warm over the graph:**

| arcs | type sets | base | column | delta |
|---:|---|---:|---:|---:|
| 50 000 | 1 | 4 420 168 B | 3 942 328 B | **−477 840 B (−10.8%)** |
| 50 000 | 3 | 6 457 072 B | 3 357 648 B | **−3 099 424 B (−48.0%)** |
| 200 000 | 1 | 15 496 024 B | 13 592 872 B | **−1 903 152 B (−12.28%)** |
| 200 000 | 3 | 25 396 800 B | 13 008 176 B | **−12 388 624 B (−48.78%)** |

The end-to-end reading and the structural one agree: at 200 000 arcs with one type
set covering half of them, the predicted saving is 17.48 − 8.00 = **9.48 B/arc**
and the measured saving is **9.52 B/arc**.

**The break-even is a fraction, not a constant.** The column costs 8 B/arc
regardless; the map cost 34.95 B per accepted arc. So:

- with ONE relationship-type set, the column is smaller whenever the pattern's
  type covers more than **22.9%** of the graph's arcs;
- with THREE type sets — the shape the retired per-type-set LRU existed for — from
  about **7.6%**;
- below that, the column is larger, and §4's cold-engine row is that case at its
  extreme (0.0008% coverage, +8.26 MiB).

## 8. Reproduction

```
git worktree add /tmp/base 35990293
go test -c -o /tmp/head_csrorder.test ./bench/csrorder/
( cd /tmp/base && go test -c -o /tmp/base_csrorder.test ./bench/csrorder/ )
shasum -a 256 /tmp/head_csrorder.test /tmp/base_csrorder.test   # MUST differ
for i in 1 2 3 4 5 6; do
  /tmp/base_csrorder.test -test.bench=BenchmarkTraversal -test.run='^$' \
      -test.benchtime=1s -test.count=1 >> /tmp/ab_base.txt
  /tmp/head_csrorder.test -test.bench=BenchmarkTraversal -test.run='^$' \
      -test.benchtime=1s -test.count=1 >> /tmp/ab_head.txt
done
benchstat /tmp/ab_base.txt /tmp/ab_head.txt
```

The same recipe with `./bench/cypher_scale/ -test.bench=Expand` and
`./bench/cyclicjoin/ -test.bench=BenchmarkCyclic -test.benchtime=1x` produces §4
and §5. §6 is `go test ./cypher/exec/ -bench BenchmarkRelTypeProbe -run '^$'
-benchtime=2s -count=5`. §7 is `go test ./cypher/ -run TestRelTypeResidentMemory
-v -count=1` in each tree, and `go test ./cypher/ -run TestRelTypeColumnSize -v`.

Every `cd` must be inside its own subshell, for the reason §2 records.
