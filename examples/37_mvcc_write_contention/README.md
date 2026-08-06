# Example 37 — MVCC under concurrent WRITERS

## Scenario

An order-processing service ingests from several producers at once. Each producer
writes to its own customer records, but every order also touches a small set of
shared **inventory** nodes — so most transactions are independent and a predictable
fraction genuinely collide. Readers run throughout, answering queries while the
ingest continues.

That shape is deliberate. A workload with no contention cannot demonstrate that
conflict detection works; one where everything contends cannot demonstrate that
independent writers scale. This example has both, in a ratio the operator sets.

## Objective

Exercise and **measure** the write side of GoGraph's MVCC, which is the half the two
existing MVCC examples do not reach:

- example 35 measures reader latency under a mixed workload;
- example 36 checks snapshot isolation on the topology dimension.

Neither drives concurrent **writers**, so neither can observe a write-write conflict,
a retriable serialization error, a logical abort, read-your-own-writes inside an open
transaction, or a commit timestamp surviving a restart. Every one of those is a
property sprint 334 added.

## Purpose

Produce evidence, not a demonstration. The example must let a reader answer, from its
output alone:

1. **Does write throughput scale with writer count?** commits/s at 1, 2, 4, 8, 16
   writers, with the ratio against one writer stated. This is the number the sprint
   exists to move.
2. **What does contention actually cost?** conflict rate by store, retry count, and
   retry success — measured, with the hot-spot fraction that produced it.
3. **Are aborts correct?** a rolled-back transaction must leave no trace. Asserted by
   a conservation invariant that no external oracle can supply: the sum of a
   quantity that every transaction preserves.
4. **What do writers cost readers?** reader latency percentiles taken *beside* the
   writers, not after them.
5. **What does versioning retain?** version-chain depth distribution and version
   count against the bound, sampled while the workload runs.
6. **Does it survive a restart?** the data AND the MVCC commit clock, so a
   post-restart transaction cannot re-mint an instant a previous process published.

## Why it must be able to FAIL

The standing rule in this project is that an instrument which cannot fail on the
defective build proves nothing — three of GoGraph's own instruments have been caught
reporting a number they could only ever have produced.

So this example is **validated in both directions**, and the result is recorded here:

- run against a worktree at the pre-sprint head, or with the property deliberately
  broken, it must FAIL;
- the failure output is reproduced in this README so a reader can see what a broken
  build looks like, not merely be told it would differ.

## The self-contradictory check

The bracket-invariant discipline that made example 36 able to detect — bracketing each
observation between acknowledged commits — is **structurally blind to a read of the
wrong legal instant**: a stale-but-valid snapshot passes every bracket.

So this example also carries at least one check that needs **no external oracle**: a
statement whose two halves must agree with each other within one transaction, so a
disagreement is self-evidently a defect regardless of which instant was read. The
conservation invariant in (3) is that check — a transfer that debits one node and
credits another cannot change the total, whatever instant observes it.

Its cost is stated, and it is sized for the gate that has to run it. A check nobody
can afford to run is not a check.

## Evidence collected

Per the project's examples mandate, every relevant indicator is measured:

| Vector | How |
| --- | --- |
| CPU | `runtime/pprof` CPU profile over the workload |
| Heap | `runtime/pprof` heap profile plus `runtime.MemStats` sampling |
| Scheduling | `runtime/trace`, to see writers blocking and GC pauses |
| Latency | per-operation histograms, reported as percentiles |
| Storage | on-disk store size before and after, and its growth |
| Coverage | `go build -cover` + `GOCOVERDIR`, reported with `go tool covdata` |

## Status

**SPECIFICATION ONLY — the implementation is rmp #2313 and is not yet written.**

This README is the first step of the project's mandated Specify → Implement → Test →
Document sequence, recorded before any code so the acceptance criteria are fixed in
advance rather than fitted to whatever the implementation happens to produce. It is
deliberately committed in this state rather than held back: the specification is
useful on its own, and a half-written example would not be.
