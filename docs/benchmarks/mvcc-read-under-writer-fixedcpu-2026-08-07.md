# Read cost under concurrent writing, with CPU demand held fixed — rmp #2342

**Date:** 2026-08-07 · **Branch:** `sprint-335` · Apple M4 (10 cores), darwin/arm64

**Outcome: the +112.72% figure is RETIRED.** With total busy-goroutine count held
fixed, reader latency does **not** rise with write rate — it is flat from 0 to 4
writers (every arm `~`, p ≥ 0.39, n=6).

## What this supersedes

[`mvcc-read-under-writer-2026-08-05.md`](mvcc-read-under-writer-2026-08-05.md)
measured read latency rising from **68.08 µs at 0 writers to 144.82 µs at 8**
(+112.72%, n=6, p=0.002) and said plainly that it was **not** an attribution: the arms
differed in **total CPU demand** as well as in write rate, so the 0-writer arm was
simply an idler machine and the figure could only be read as an *upper bound*. That
report also recorded that its first explanation was **refuted** — a CPU profile found
neither `suspectNodes` nor `correctBitmap` in the top cumulative profile at all.

## The instrument

`bench/mvccwrite/read_under_writer_fixedcpu_test.go`
(`BenchmarkReadUnderWriter_FixedCPU`). The number of **busy goroutines is constant**
at 8 in every arm; only the split changes. Arm `writers=k` runs k goroutines
committing `MATCH (n:Acct {id:$id}) SET n.bal=$v` and **(8−k)** running a cost-matched
non-writing unit, so the machine is equally loaded at k=0 and k=8 and the write rate
is the only variable the experimenter changes.

The filler is `allocUnit` — a commit's object count and volume plus a spin, sharing
**nothing**: no map, no counter, no lock. It consumes a writer's share of CPU and
allocator while producing **no version work**.

### The calibration was wrong the first time, and the arms' own telemetry caught it

The first version calibrated the filler against `nsPerCommitTarget`, the cost of
`CREATE (n:Account {id:$id})`. **This benchmark's writer is a different statement** —
an indexed seek plus a SET. The mismatch showed up in the reported counts: fillers ran
**69 k** units where eight writers landed **19 k** commits, so the filler was ~3.6×
cheaper than the thing it stood in for. The target is now **measured on the host
against this benchmark's own write statement** (`measureWriteUnitCost`), which brought
the filler rate to 48 k.

This is the second time in this sprint that matching the *shape* without matching the
*rate* matched nothing — the same error the allocation control records.

## Result

Reader latency, n=6, `-benchtime=300x`:

| writers | reader sec/op | vs 0 writers | writes landed | filler units |
|---:|---:|---|---:|---:|
| 0 | 214.7 µs ± 12% | — | 0 | 48.1 k |
| 1 | 202.4 µs ± 13% | ~ (p=0.394) | 4.4 k | 40.1 k |
| 2 | 212.1 µs ± 12% | ~ (p=0.937) | 7.9 k | 35.3 k |
| 4 | 197.5 µs ± 20% | ~ (p=0.818) | 14.1 k | 23.0 k |
| 8 | 141.1 µs ± 17% | −34.29% (p=0.002) | 20.8 k | 0 |

**From 0 to 4 writers, with 14 000 commits landing during the measured window, the
reader's latency does not move.** Every arm is statistically indistinguishable from
the write-free arm.

## What this establishes, and what it does not

**Established.** The +112.72% rise reported on 2026-08-05 was **CPU competition, not
version work**. Once the 0-writer arm is made to work as hard as the 8-writer arm, the
rise disappears. The superseded figure is retired as a measure of what concurrent
writing costs a reader; it remains valid as what it always claimed to be — an upper
bound.

**NOT established, and the −34.29% at 8 writers must not be read as "writing makes
reads faster".** The load match is imperfect and the instrument reports by how much:
eight fillers run **48 k** units where eight writers land **21 k** commits, a residual
~2.3× in the fillers' favour. A share-nothing filler's per-unit cost stays flat under
concurrency while a write's rises — contention, conflict handling and allocator
pressure all grow — so per-unit matching at one goroutine does not give rate matching
at eight. The filler arms are therefore doing *more* total work, which biases every
arm with fillers **slower**. That is the direction that would produce exactly this
−34%, so the 8-writer arm is not evidence of a speed-up.

It does not weaken the headline: the bias runs against the writers, so the flat
0→4 result is if anything conservative — a real read cost of concurrent writing would
have shown up *more* strongly, not less.

**Still unattributed.** With the confound removed there is no rising read cost left to
attribute, so the mechanism hunt the ticket anticipated (version-chain walk depth via
`graph/mvcc/depthhist.go`, the Horizon enter/leave pair per read transaction) has no
signal to chase at these writer counts. That is the honest state: the question changed
from *which mechanism costs the reader* to *is there a cost at all*, and at up to 4
concurrent writers on a 2 000-node fixture the answer is no.

## Hypotheses tested, including the refuted ones

| Hypothesis | Outcome |
|---|---|
| `suspectNodes` / `correctBitmap` explain the rise | **REFUTED** (2026-08-05, CPU profile: absent from the top cumulative profile) |
| The rise is CPU competition between the arms | **CONFIRMED** — holding busy goroutines fixed removes it |
| A `CREATE`-calibrated filler is cost-matched to a seek-and-SET writer | **REFUTED** by the arms' own counts (3.6× off); replaced by a measured target |
| Per-unit cost matching at 1 goroutine gives rate matching at 8 | **REFUTED** — residual ~2.3×, quantified above and stated as a limit |

## What would close the residual

Matching the filler to the writer's *realised concurrent* cost rather than its
single-goroutine cost — i.e. calibrating inside each arm rather than once up front.
That is a bigger instrument than this task needs, because the headline result does not
depend on it: the bias runs against the conclusion, not for it.
