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

**Read latency rises with write rate, and the graph size is no longer the reason.**
That much is established: the 0-writer baseline is measured in the same build, so the
delta is the cost of concurrent writing rather than of a growing graph. It replaces the
+39.02%, which could not distinguish the two.

**It is NOT yet an attribution, and the next section explains why** — the profile shows
the writers' own queries consuming ~39% of CPU, so these arms differ in total CPU demand
as well as in write rate. Read the figures as an upper bound on what concurrent writing
costs a reader, not as a measurement of MVCC version work.

(The pre-profile reading of the curve — super-linear in landed writes, therefore a
per-read walk rather than lock contention — is left in the refuted-hypothesis section
below, because that inference is exactly what the profile overturned.)

## PROFILED: the hypothesis below is REFUTED, and this instrument is still confounded

Run immediately after the table, on the 8-writer arm
(`-benchtime=3s -cpuprofile`), before attempting any remediation. It refuted the
explanation and found a defect in the instrument itself.

`suspectNodes` and `correctBitmap` **do not appear anywhere** in the top cumulative
costs. The version-walk explanation below is therefore wrong, and the +112.72% is not a
version-walk figure.

What the profile actually shows:

| symbol | cum |
|---|---|
| `cypher/exec.(*ResultSet).Next` | 41.78% |
| `cypher.(*Engine).RunInTx` | 39.22% |
| `cypher/exec.(*SetProperty).Next` | 38.51% |
| `cypher/exec.(*Filter).Next` | 38.08% |
| `cypher.newRowPredicate.func1` | 34.18% |
| `lpg.(*PropertyKeyRegistry).Lookup` | 2.73% (490 ms flat) |

**Nearly 39% of all CPU is the WRITERS' own work, not the reader's.** The writers run
`MATCH (n:Acct {id: $id}) SET n.bal = $v`, and there is no index on `:Acct(id)` in this
harness — so that `MATCH` is a full label scan with a per-row property filter, costing
O(2000) per write. Each writer is doing a scan comparable to the reader's on every single
commit.

**So the arms do not differ only in write rate; they differ in total CPU demand.** Adding
writers adds scanning work, and on 10 cores the 8-writer arm is contending for CPU with
the reader. The +112.72% therefore mixes "concurrent writing costs a reader" with "eight
extra O(N) scans are running", which is a different confound from the one this benchmark
was built to remove, but a confound nonetheless.

**The figure stands only as a bound, not as an attribution.** Read latency does rise with
write rate — that much survives, since the 0-writer baseline is measured in the same build
— but how much of the rise is MVCC version work versus plain CPU competition is unresolved,
and the honest reading is that no material *version-walk* cost has been demonstrated.

**The instrument was then fixed and re-measured** — see the next section.

This is the second instrument in this task to be caught by measurement rather than by
review. The lesson is the one already written into the file header, and it applies to the
replacement as much as to the original: hold *everything* constant except the variable,
and profile the arms before believing the delta.

## Re-measured with a CONSTANT-COST writer (the corrected instrument)

`CREATE INDEX acct_id FOR (n:Acct) ON (n.id)` is now created before seeding, so the
writer's `MATCH` is a seek rather than a scan. The reader stays a full label scan by
design — that is the workload under test — but the writer is now constant-cost, which is
what makes the write rate the independent variable.

n=6 per arm, every comparison p=0.002:

| writers | 0 | 1 | 2 | 4 | 8 |
|---|---|---|---|---|---|
| read `sec/op` | 68.62µ ± 2% | 79.76µ ± 1% | 90.72µ ± 3% | 104.26µ ± 4% | 130.73µ ± 3% |
| vs 0 writers | — | +16.24% | +32.20% | +51.94% | +90.51% |
| writes landed | 0 | **232.9k** | 380.8k | 429.3k | 574.9k |

**The write rate rose ~80×** at one writer (2.93k → 232.9k), which confirms directly what
the profile alleged: the previous arms were spending their time in the writer's own scan,
not in writing. Two consequences:

- **read degradation FELL** at 8 writers, from +112.72% to +90.51%, while the churn
  driving it rose ~59× (9.75k → 574.9k writes). Per unit of write churn the read cost is
  therefore roughly two orders of magnitude lower than the first instrument implied;
- the writers' share of CPU **halved**, from 39.22% to 20.53% cumulative under `RunInTx`.

**The version-walk hypothesis is refuted a second time.** `suspectNodes` and
`correctBitmap` do not appear in this profile either, at any write rate. Whatever the
residual +90.51% is, it is not the suspect walk, and it is not `LabelCountExact` declining
its fast path.

**The residual is NOT attributed, and I am not going to guess at it.** Only ~41% of CPU
lands in GoGraph symbols at all in this arm (writers 20.53%, the reader's `ResultSet.Next`
11.53%); the remainder is runtime — GC and scheduling — which is consistent with 574.9k
committed transactions' worth of allocation pressure but is not established as the cause.
Naming it needs a heap profile and a `GODEBUG=gctrace=1` run, not another inference.

**Verdict for AC 3.** No material MVCC *version-walk* cost has been demonstrated at any
write rate on either instrument, so there is nothing identified to reduce, and a code
change now would be optimising against an unattributed residual. That is the finding, and
under this task's AC 3 it is the "not confirmed" branch: close with the finding, no code
change — with the caveat that "not confirmed" here means *not attributable to MVCC*, not
*no cost at all*, since the reader does slow measurably under a saturating writer.

## AC 4: the realistic-rate bound is BREACHED (+4.17% against ≤2.5%)

Every arm above **saturates**, which answers the worst case and not the production
question. `BenchmarkReadAtRealisticWriteRate` throttles each writer to 1000 commits/s —
about 230× below saturation, and a rate a real service might sustain. The achieved rate is
reported per arm so the bound is never read without evidence the throttle held; it held
exactly.

n=6, p=0.002:

| writers | 0 | 1 | 4 |
|---|---|---|---|
| read `sec/op` | 67.79µ ± 2% | 70.61µ ± 1% | 73.71µ ± 1% |
| vs 0 writers | — | **+4.17%** | +8.75% |
| achieved writes/s | 0 | 999.7 | 3997.0 |

**This FAILS AC 4, which requires ≤2.5%.** At a realistic 1000 commits/s a single writer
costs a concurrent reader 4.17%, and four writers cost 8.75%. The bound is breached by
1.67 percentage points at the single-writer rate.

**This changes the verdict recorded above.** The saturating arms showed no *attributable*
MVCC cost, and that still stands — but "no attributable cost" is not "within the bound",
and AC 4 is a bound, not an attribution. So #2292 does **not** close on the "not
confirmed" branch: there is a measured, reproducible, statistically significant breach at
a realistic write rate, whatever its mechanism turns out to be.

### The indexed-read arm settles it: the overhead is FIXED PER READ, and 4.17% was the OPTIMISTIC figure

`BenchmarkIndexedReadAtRealisticWriteRate` runs an indexed point lookup against the same
throttled writer, with the writer confined to the top half of the id space and the reader
to the bottom, so the reader never seeks a row a writer is versioning. Throttle held at
999.3 writes/s.

| reader | baseline | with 1 writer @1000/s | relative | absolute Δ |
|---|---|---|---|---|
| indexed seek (1 row) | 5.669µ ± 2% | 6.371µ ± 1% | **+12.38%** | 0.70µ |
| full scan (2000 rows) | 67.79µ ± 2% | 70.61µ ± 1% | +4.17% | 2.82µ |

n=6, p=0.002 both arms.

**The two candidate mechanisms predict very different ratios, and the measurement
discriminates cleanly.** A purely per-row cost would differ ~2000× in absolute terms
between a 2000-row scan and a 1-row seek. A purely fixed per-read cost would be identical.
The observed absolute difference is **4×** — so the overhead is overwhelmingly a FIXED
per-read cost, with a small per-row component.

**Consequence, and it is the worse direction.** A fixed cost is worst for the cheapest
reads, which are exactly the OLTP reads this module targets. The realistic-rate breach is
therefore **+12.38%, not +4.17%** — nearly 5× AC 4's ≤2.5% bound, on the read shape a real
service issues most.

**Leading candidate, stated as a prediction because this task has already refuted two of
my hypotheses.** A fixed per-read cost that appears only when writers are active points at
work done once per query and gated on churn: the snapshot acquisition, the horizon slot
(`Enter`/`Leave`), or the `labelBitmapNeedsFilter` decision and what follows it. Note that
`suspectNodes` is sampled **twice** per read since #2326's fix (`01bc9019`), once before
the bitmap clone and once after — a fixed per-read cost that engages only when churn is
live, which matches this signature exactly. That does **not** contradict the two profiles
above: both were taken on the SCAN arm, where a sub-microsecond fixed cost is 1% of the
runtime and invisible. On a 5.7 µs read it would be most of the 0.70 µs.

**The measurement that settles it:** profile the INDEXED arm, not the scan arm. If
`suspectNodes` dominates the delta there, the fix is to sample once and close the
clone-versus-sample race differently — keeping the correctness property #2326 established,
which is not negotiable. If it does not, the candidate list above is wrong too and the cost
is elsewhere.

## The refuted hypothesis, kept for the record

Written before the profile. **The profile refuted it** — see above. Kept because the
prediction it made is what made it refutable, and because the reasoning was sound even
though the conclusion was wrong.

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
should show `correctBitmap` / `suspectNodes` dominating the reader's time. **It does not.
The explanation is wrong**, and the corollary about #2326 having made this worse is
therefore unsupported — nothing here shows the doubled suspect sample costs a reader
anything measurable at this write rate.

## What is NOT yet done

- **AC 3 remediation.** Now required after all, because AC 4 fails: the cost is within
  no bound even though it is attributable to no MVCC structure yet. Attribution first
  (heap profile, gctrace, and the indexed-read arm), remediation second.
- **AC 4 is MEASURED and FAILING**, and worse than first recorded: +12.38% on an indexed
  read at 1000 commits/s against a ≤2.5% bound (+4.17% on a full scan). The overhead is
  fixed per read, so the cheapest reads suffer most. What remains is a profile of the
  INDEXED arm to attribute it — the scan arm's profiles cannot see a sub-microsecond fixed
  cost.
- **The pre-MVCC cross-check.** Deliberately not required to reach the verdict above: the
  0-writer arm is the baseline, so the cost of *concurrent writing* is measured within one
  build. An absolute comparison against `b66b4e25` would answer a different question —
  what everything since then cost — and would need this benchmark copied into a worktree
  at that commit.
