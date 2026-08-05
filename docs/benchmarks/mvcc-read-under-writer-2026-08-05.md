# Read cost under a saturating writer — 2026-08-05

rmp #2292. Head `e98dd00f`, branch `sprint-334`. Host: Apple M4, 10 cores,
`darwin/arm64`, idle.

```
go test -run '^$' -bench BenchmarkReadUnderConstantSizeWriter -benchmem -count=6 \
    ./bench/mvccwrite/
benchstat -col /writers <output>
```

Harness: `bench/mvccwrite/read_under_writer_test.go`.

## The instrument, and why the old figure could not be trusted

`BenchmarkEngReadUnderWriter` ran readers doing `MATCH (n) RETURN count(n)` — cost
proportional to the node count — against an unthrottled writer doing
`CREATE (:W {id:...})`, which **adds** a node. Graph size was an uncontrolled variable,
and the MVCC work changed it by two orders of magnitude because the writer stopped being
starved by readers. Its recorded **+39.02%** therefore mixed "reads got slower" with "the
graph got bigger" in unknown proportion. That figure is retired, not refined.

The replacement holds the population **fixed** at 2000 `:Acct` nodes and makes the
**write rate** the independent variable. Writers `SET` a property on a node that already
exists, so they generate version churn on live chains — the version-walk cost this task
is named for — without changing the node count. Each arm also reports the writes actually
landed, so a latency figure can never again be read without knowing how much writing
produced it.

## The measurement

Reader: `MATCH (n:Acct) RETURN count(n)`. n=6 per arm, every comparison p=0.002.

| writers | 0 | 1 | 2 | 4 | 8 |
|---|---|---|---|---|---|
| read `sec/op` | 68.08µ ± 2% | 70.67µ ± 1% | 76.36µ ± 0% | 104.56µ ± 4% | 144.82µ ± 3% |
| vs 0 writers | — | +3.80% | +12.16% | +53.58% | **+112.72%** |
| writes landed | 0 | 2.93k | 5.22k | 6.28k | 9.75k |

**A material cost is CONFIRMED.** Read latency roughly doubles under 8 saturating
writers with the graph size controlled, so the original +39.02% was not merely a
confound artefact — the real cost at high write rates is larger than that, and the old
instrument was understating it as much as it was mismeasuring it.

The rise is also **super-linear in landed writes**: from 1 to 8 writers the write count
rises 3.3× while the read cost rises 2.05× over a baseline that is already 68µ — i.e.
the marginal cost per unit of churn grows. That is the signature of a per-read walk whose
length scales with outstanding churn, not of lock contention, which would flatten as
writers queue.

## The likely mechanism, and it is the one the task named

Not yet profiled, so this is a **hypothesis with a specific prediction**, not a finding.

The reader's `count` is served by `Graph.LabelCountExact`, which declines the O(1)
bitmap answer whenever `labelBitmapNeedsFilter` is true — and concurrent writers make it
true, because label/property churn is live. The count then falls back to the filtered
scan, which calls `correctBitmap`, which walks the **suspect set**: every node with a
live version chain. That set grows with outstanding write churn, so the reader's cost
grows with the write rate. "The version walk, not the lock", exactly as this task's title
says.

**If that is right, then rmp #2326 made it worse, and knowingly.** The fix in `01bc9019`
samples the suspect set **twice** — once before the bitmap clone and once after — and
corrects against the union, because a single post-clone sample could be emptied by a
sweep and silently serve a stale count. That doubles the walk this benchmark is
measuring. It was the correct trade (a wrong count has no predicate left to catch it),
but its read-path cost was not measured at the time.

**The prediction that would confirm or refute it:** a CPU profile of the 8-writer arm
should show `correctBitmap` / `suspectNodes` dominating the reader's time. If it does
not, this explanation is wrong and the cost is elsewhere. Run that before attempting any
remediation — this project's record is that the stated bottleneck is usually not the
measured one, and #2323 has just supplied another example.

## What is NOT yet done

- **AC 3 remediation.** A material cost is confirmed, so the task requires it be
  materially reduced. That is unstarted, and it should not begin before the profile above
  names the cost.
- **AC 4, the realistic-write-rate bound of ≤2.5%.** Not answerable on this instrument:
  every arm here is **saturating**, so even the 1-writer arm (+3.80%) is a writer running
  flat out rather than a realistic rate. A throttled arm — writes/second as the
  independent variable — is needed, and it is a different benchmark.
- **The pre-MVCC cross-check.** Deliberately not required to reach the verdict above: the
  0-writer arm is the baseline, so the cost of *concurrent writing* is measured within one
  build. An absolute comparison against `b66b4e25` would answer a different question —
  what everything since then cost — and would need this benchmark copied into a worktree
  at that commit.
