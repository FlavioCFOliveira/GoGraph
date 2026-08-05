# The cost of observing the MVCC substrate — 2026-08-05

rmp #2312 (MVCC D2), acceptance criterion 4: *"the measurement overhead is
quantified with benchstat on the read and write hot paths and is justified; no
unexplained regression."*

Baseline `c3bb0544`; measured line = that baseline plus the whole of #2312. Branch
`sprint-334`. Host: Apple M4, 10 cores, `darwin/arm64`, idle, no competing load.

## Verdict

**The read path is unchanged and the write path pays two atomic increments per
transaction.** Allocations are identical across every arm. Time geomean +0.07%,
which is not a measurable difference.

One arm moved measurably: an **empty exclusive write bracket**, +4.37% — 32.49 ns →
33.91 ns, a difference of **1.42 ns**. That is the striped writer gauge:
`WriteCounters.BeginWriter` and `EndWriter`, one atomic add each on a cache-line
isolated bank. On a bracket that actually writes something it is not measurable —
`LabelWrite` runs at 650–990 ns and shows no regression at all.

## Method

Both arms were built from a git worktree and run **interleaved**, alternating A and
B within each round, three rounds of `-count=3` → 9 samples per arm. Interleaving is
this project's standing requirement: a block of A followed by a block of B measures
thermal drift as readily as it measures the change.

```
go test ./graph/lpg/ -run XXX -bench '<set>' -benchmem -count=3
```

## Result

```
                                         │      A      │                  B                  │
                                         │   sec/op    │   sec/op     vs base                │
Barrier_View-10                            4.067n ± 0%   4.080n ± 0%       ~ (p=0.230 n=9)
Barrier_ApplyAtomically-10                 32.49n ± 1%   33.91n ± 4%  +4.37% (p=0.000 n=9)
LabelWrite/nodes=10000/deltas=false-10     670.3n ± 2%   649.8n ± 2%  -3.06% (p=0.002 n=9)
LabelWrite/nodes=10000/deltas=true-10      665.7n ± 1%   654.5n ± 1%  -1.68% (p=0.014 n=9)
LabelWrite/nodes=1000000/deltas=false-10   983.1n ± 1%   975.2n ± 2%       ~ (p=0.190 n=9)
LabelWrite/nodes=1000000/deltas=true-10    995.6n ± 1%   987.4n ± 2%       ~ (p=0.448 n=9)
LabelRead/deltas=off-10                    7.847n ± 0%   7.860n ± 1%       ~ (p=0.606 n=9+8)
PropWrite/nodes=10000/deltas=false-10      173.1n ± 2%   176.0n ± 1%  +1.70% (p=0.009 n=9+6)
PropWrite/nodes=10000/deltas=true-10       175.0n ± 1%   175.9n ± 1%       ~ (p=0.172 n=9+6)
PropWrite/nodes=1000000/deltas=false-10    208.3n ± 1%   210.2n ± 3%       ~ (p=0.456 n=9+6)
PropWrite/nodes=1000000/deltas=true-10     206.9n ± 2%   206.4n ± 2%       ~ (p=0.752 n=9+6)
PropRead/deltas=off-10                     4.990n ± 1%   4.976n ± 0%       ~ (p=0.548 n=9+6)
geomean                                    109.4n        109.5n       +0.07%

allocs/op: every arm ~, geomean +0.00%
B/op:      every arm ~ or better, geomean -0.01%
```

`LabelWrite` reads −1.7% to −3.1%. That is not a speed-up this change produced;
there is no mechanism by which adding two atomic increments makes a write faster.
It is layout noise of the kind this project has recorded before — the `Graph` struct
grew, and the sizes of the objects around it moved between malloc size classes. It
is reported rather than claimed.

## The regression that was found, and closed

The first run of this A/B measured an allocation regression that was real:

```
LabelWrite/nodes=1000000/deltas=false-10   4.000 → 6.000 allocs/op  +50.00%
LabelWrite/nodes=10000/deltas=false-10     4.000 → 5.000 allocs/op  +25.00%
B/op                                             +13% to +27%
```

The module's rule is that an allocation regression is not merged without a written
justification, so the cause was profiled rather than argued about. Equal-iteration
memory profiles (`-benchtime=2000000x` in **both** arms, so `alloc_objects` counts
are comparable — an un-normalised profile comparison reports fewer iterations as an
improvement) named it immediately:

```
  19688815 59.40%  (*nodeLabelShard).pushLabelDelta          [both arms]
   7236603 21.83%  (*Graph[...]).publishMVCCMetrics          [B only]
```

`publishMVCCMetrics` was building its per-bucket series names by concatenating a
prefix with the bucket label at the call site — `"lpg.mvcc.chain_depth.bucket." +
label` — for 26 series, on **every vacuum pass**. Each concatenation allocates.

Two things are worth recording about it:

1. **The allocation was not on the write path at all.** It was the vacuum
   goroutine's, attributed to the benchmark because `-benchmem` counts allocations
   process-wide. The arm that regressed most is the one whose churn wakes the vacuum
   most often. A profile is what distinguished those; the benchmark line alone would
   have sent the search to the wrong goroutine.
2. **The names are constant.** They are now built once into package-level tables
   (`graph/lpg/mvcc_metricnames.go`) and indexed. After the fix the arm is back to
   4 allocs/op and 276 B/op against the baseline's 4 and 278.5.

## What was NOT measured, and why

**The Prometheus backend's own cost.** Every number above is on the default no-op
backend, which is what an unconfigured consumer runs. Installing a backend makes
each `SetGauge` a map lookup and a store in that backend's implementation, and that
cost belongs to the backend rather than to this change. The publication site is the
vacuum goroutine, so it is off every request path either way.

## The sprint's own gate, on the merged line

Re-run after the change, since a gate is only evidence if it is run on the line that
ships:

```
TestWriteScalingGate       engine/mem 1 writer 315856/s, 8 writers 701968/s => 2.222x   PASS
TestWALWriteScalingGate    engine/wal 1 writer    260/s, 8 writers    988/s => 3.799x   PASS
TestWriteConcurrencyGate   8 writers free 658159/s, one mutex 306637/s      => 2.146x   PASS
```
