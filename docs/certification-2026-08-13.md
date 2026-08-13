# Production certification — extreme load and concurrency — 2026-08-13

Scope: certify that GoGraph is fit for production environments of extreme load and
concurrency, that CPU is spent effectively, that RAM is held only for what is
necessary, and that concurrency is handled correctly. Ranked throughout by the
project's own order: **correct → secure → efficient → fast**.

This is the second day of the sprint-343 certification. It continues
[`certification-2026-08-12.md`](certification-2026-08-12.md), which refused the
certification on its first rung with one blocker — rmp #2420, a pinned MVCC snapshot
observing half a transaction — diagnosed to a stated contradiction but not closed.
That contradiction is resolved here.

Entry commit: `9167d3d3`'s parent, `30b5ecb3`. Working branch `sprint-343`.
Host: Apple M4, 10 cores, 32 GB, `darwin/arm64`, Go 1.26.

---

## Verdict

**PENDING** — filled in once the exit gate and the remaining efficiency measurements
are recorded. The correctness rung is now clear; §5 states exactly what has and has
not been established.

---

## 1. The blocker is CLOSED — rmp #2420, and the defect was in `Horizon.Oldest`

### 1.1 What the previous cycle left

It had refuted fourteen mechanisms, established the reproduction recipe, validated
its instruments, and ended in three statements that could not all be true:

- at the violation the property version chain had depth 1, missing the record a
  reader at `startTS` needs;
- no removal path can free a record above the watermark it was given (asserted, 0
  breaches);
- the watermark never passes a live reader (asserted, 0 in 1 122 311 checks).

Both assertions were TRUE. Both were blind to the defect, and the blindness is the
interesting part: the reclaimer never did free a record above its own watermark, and
a reader checking the watermark **at its own birth** is always visible to its own
scan. The hole was a reader that is invisible to a scan already in progress.

### 1.2 The defect

`mvcc.Horizon.Oldest(fallback)` computes the reclamation watermark by scanning the
occupancy words once and taking the oldest start instant it finds. It carried a
`found` flag and took the FIRST occupied slot's timestamp unconditionally:

```go
if !found || ts < oldest { oldest, found = ts, true }
```

So the fallback — the published frontier, sampled by the caller **before** the scan —
was discarded the moment any reader was seen, and with every live reader newer than
it the returned watermark came out **above** it:

```
reclaimer  samples fallback = 100 (the published frontier)
reader X   claims a slot in a word the scan has ALREADY PASSED, then publishes 104
reader Y   is seen, at 105
reclaimer  returns 105, having thrown 100 away
sweep      frees every version stamped <= 105, including the one stamped 105 that
           X must undo to resolve back to 104
reader X   reads the value committed at 105 through a snapshot pinned at 104
```

Claiming the occupancy bit before reading the clock (`EnterHolding`) protects a reader
the scan SEES — it reads zero and the scan suspends. It cannot protect one the scan
never looks at, because that word was read before the bit was set. **The fallback is
the only thing that covers that reader**, and only because it was sampled before the
scan: every reader appearing afterwards begins at or after the frontier of that
moment.

That is #2420's exact signature — over-visible by one transaction, with the record
stamped `startTS+1` absent from a chain no reclaimer freed above its own watermark.

### 1.3 The fix, and the instrument that found it

`Oldest` now caps at the fallback (a minimum, not first-wins). A pass whose readers
are all newer simply frees less, and the next pass samples a newer fallback, so
nothing is retained beyond one pass.

Two supporting changes, and **they are what made the defect findable**:

- **`Horizon.Leave` invalidates the slot's timestamp** before clearing the occupancy
  bit, so an occupied slot reads either zero — "claimed, instant not yet known", which
  suspends reclamation — or its own occupant's published instant, never a previous
  occupant's stale one. The watermark is therefore monotone. The order matters: the
  timestamp is cleared while the bit is still ours, or a re-claimer's publish would be
  clobbered.
- **`Graph.publishWatermark` counts a watermark that moves BACKWARDS**, judged against
  the caller's own earlier sample so a reordered publish of two near-simultaneous
  scans cannot read as a breach. Surfaced as `MVCCStats.WatermarkRegressions` and as a
  gauge; zero is the only correct value.

The detector is worth more than the fix, because it fires **at the corruption** rather
than at the read that suffers from it, and it turned a 2 %-per-run symptom into a
5-per-run signal:

| tree | watermark regressions observed | arm |
|---|---:|---|
| residue in place, `Oldest` unfixed | 1 734 – 4 165 per run, **all benign** | 4 runs of the whole `graph/lpg` package under peer load |
| residue removed, `Oldest` unfixed | **5 in one run, all real** | `go test -race ./graph/...` |
| `Oldest` capped | **0** | the same `./graph/...` run, and all 40 runs of §1.4 |

The first row is the honest record of an instrument of mine being wrong before it was
right — and the two causes were predicted from the code before the run confirmed
them, which is the only reason the 1 734 firings were read as "my detector is
unsound" rather than as "the substrate is on fire".

Two further detectors of the same family, both pinned on sound AND broken controls:
`mvcc.Horizon.StaleLeaves` (a slot released that nobody held — free, it is a branch on
a value `Leave` already has) and `Horizon.SlotState` (a reader verifying its own slot
still announces its own instant, checked on EVERY read rather than once per snapshot,
which is what the previous cycle's version could not see).

### 1.4 The demonstration, against a criterion fixed in advance

`TestHorizon_OldestNeverExceedsItsFallback` is deterministic and fails against the
previous behaviour: it returned 105 and 900 where the fallback 100 was required.

The criterion for the concurrent arm was written down before the run: **40 package
runs of the whole `graph/lpg` package, no `-run` filter, under a tree-wide
`go test -race ./...` peer load, with ZERO firings of all five oracles** — the shipped
equality oracle, a new ABSOLUTE oracle (`value == startTS-1`, which fires on a single
wrong read rather than needing two reads to straddle a write), the per-read slot check,
`StaleLeaves`, and the watermark-regression counter.

**Result: 40/40 green, zero firings of all five.**

Stated with the same care the criterion was: at ~5 regressions per run pre-fix, 40 runs
of the heavier recipe would have produced well over a hundred events, so zero is a
strong statement **about the mechanism**. The original symptom's own rate is ~2 % per
run, so 40 runs expect 0.8 events and P(0 | unchanged) ≈ 45 % — this arm alone does not
decide the symptom, and the deterministic unit test is what carries it.

### 1.5 The absolute oracle, and why it was worth building

The shipped oracle only fires when two reads straddle a write. The absolute one fires
on a single wrong read, and the fixture hands it over exactly: the seed transaction is
the graph's first commit, so value *i* publishes at instant *i+1* and a snapshot at
`startTS` must read exactly `startTS-1`. It was validated by injecting an off-by-one
into the expected instant: **522 346 of 522 346 reads flagged**, with the capture naming
the injected offset. An oracle that cannot fail proves nothing, and this one was made
to fail on purpose before it was trusted.

---

## 2. FIXED — rmp #2366: a rolled-back writer re-reserved a value a peer had freed

Deterministic, and re-verified at `9167d3d3` — before anything was changed on this
path — because the ticket's own technical requirements say to re-verify rather than
trust the recorded mechanism: `CREATE (y:Person {email:'old'})` was refused although no
live `:Person` held `'old'`.

The two directions of a UNIQUE value-set change are **not symmetric**, and treating
them as if they were is the defect. A reservation is eager and its rollback is sound —
the value was reserved throughout, so no committed peer can hold it. A release applied
eagerly hands the value to any peer that asks, so its rollback had to write a
RE-RESERVATION into shared state, judged against the rolling-back transaction's own
view while a peer's committed release had already changed the same value.

A release is now recorded on the transaction (`exec.ConstraintTxn`) and applied to the
shared value-sets only by `CommitTxn`, inside the same barrier window that publishes
the writes. Rollback drops a private mark and touches nothing shared, so there is
nothing to order against a peer's commit.

It also closes a **mirror hazard the eager release had**: a peer could take a value an
uncommitted transaction had vacated and end up sharing it if that transaction rolled
back — a duplicate, which is worse than the unusable value the ticket describes.

The `peer commits` arm of `TestUnique_RollbackLeavesNoPhantomReservation` was
deliberately omitted by the previous cycle so as not to encode a defect as correct; it
is now enabled and passes.

---

## 3. NEW, and the most serious finding of the day — rmp #2423

Found while fixing #2366, whose fix uncovered it: with `'old'` correctly reusable, a
sibling test began failing on a **second, independent** wrong answer that the phantom
reservation had been masking.

Measured against the merged state after a committed label removal:

```
MATCH (n:Person) RETURN count(n)                        -> 0   correct
MATCH (n:Person) WHERE n.k     = 'b' RETURN count(n)    -> 0   correct
MATCH (n:Person) WHERE n.email = 'old' RETURN count(n)  -> 1   WRONG
MATCH (n:Person) WHERE n.email = 'old' RETURN labels(n) -> []  self-contradictory
```

One row said both "matches `:Person`" and "carries no labels". Serially — one committed
`REMOVE`, no peer — every one of those reads answers 0.

**Root cause.** The planner rewrote `Selection(n.email = 'old')` over
`NodeByLabelScan(Person)` into a **bare** `NodeByIndexSeek`, on the assumption that
membership in a label-scoped index implies the label. An index is a CANDIDATE source
and over-reports: a label removal leaves the node's entries in that label's property
indexes behind. The label scan was never affected, because it resolves through lpg's
snapshot-aware `LabelBitmapAsOf`, which filters exactly this; the seek bypassed that
path entirely. The plans confirm the attribution rather than leaving it inferred — only
this shape plans as a bare seek, and the correct ones plan as `Filter` over
`NodeByLabelScan`.

**Fix.** Every rewrite that replaces a labelled scan leaf now qualifies its candidates,
and one that cannot **declines** rather than answering:

| rewrite | how the label is enforced |
|---|---|
| `NodeByIndexSeek` (hash equality) | residual per-candidate predicate, applied in place at `Init` |
| `NodeByIndexSeekSet` (key set, #2183) | same, after the dedupe and before the budget test |
| `NodeByIndexRangeScan` (range/prefix) | INTERSECT with the label's own bitmap |
| index intersection (#2134) | same intersection |
| EXPLAIN and probe paths | ask with the same label source, so a rendered plan cannot claim a rewrite the build declines |

**Reachability, stated honestly.** Only the hash-equality rewrite REPRODUCED. The
plans show the range, prefix, numeric-equality and key-set shapes all planning as
`Filter` over `NodeByLabelScan` in that fixture, so their correct answers came from the
scan and prove nothing about those rewrites. Reading the range builder shows it drops
the label the same way, gated behind a population floor — so it is fixed as a **latent**
defect, not a demonstrated one, and the difference is recorded rather than blurred.

**Two existing tests were asserting the defect**: both seeded a node with NO label and
still expected a labelled seek to return it. Their fixtures now label the node, which
preserves what each was written to cover. A guard that makes two tests fail is a guard
that was needed.

---

## 4. Efficient and fast

_Pending — the measurements owed for the changed paths, and the axes carried over from
the previous cycle._

---

## 5. What this certification does NOT establish

Stated plainly, because an unmeasured axis reported as sound is how a certification
becomes misleading.

1. **The symptom's own rate was not re-measured to significance.** §1.4 says why: the
   mechanism is carried by a deterministic test and by a detector whose pre-fix rate
   was 5 per run, not by the 40-run arm, which at the symptom's ~2 % rate cannot
   distinguish a fix from luck on its own.
2. **#2423's sibling rewrites are fixed as LATENT, not demonstrated.** Only the
   hash-equality shape reproduced; the range, prefix, numeric-equality and key-set
   shapes planned as `Filter` over `NodeByLabelScan` in that fixture. Reading the range
   builder shows it drops the label the same way behind a population floor, so the
   guard is applied there on the strength of a code reading plus a control that the
   seeks still fire — not on a reproduction.
3. **Latency was not measured at the published levels.** §4 reports throughput and the
   A/B of the changed paths; no p50/p99 at 1, 8, 64, 256, 1024 goroutines, so half of
   that requirement remains unmet, exactly as it was on 2026-08-12.
4. **The write-scaling half of the concurrency sweep was not run**
   (`store/txn/write_scaling_bench_test.go`).
5. **The whole-tree soak layer was not run**, and remains unmeasured for the third
   consecutive certification.
6. **No container was run.** #2421 derives the engine-wide ceilings from a cgroup limit
   and is unit-tested against injected limits, but the cgroup read paths have never
   executed on this host, which is `darwin/arm64` and has no cgroup filesystem.
7. **The memory findings are untouched.** rmp #2404 (the synthetic string node key,
   44.55 B/node), #2405 (the per-node record — an architecture decision that is the
   user's to take) and #2407 (peak versus steady during shard growth) are open and
   were not addressed today.
8. **Single host, single architecture.** Apple M4, 10 cores, `darwin/arm64`. No Linux,
   no NUMA, no multi-socket, no container.

---

## 6. Findings

| id | severity | title | status |
|---|---|---|---|
| #2420 | 9 / 9 | The reclamation watermark could exceed a live reader's start instant | **FIXED** `9167d3d3` |
| #2423 | 9 / 9 | An index-seek rewrite dropped the label predicate; a self-contradictory row | **FIXED** `fca34a0c` |
| #2366 | 8 / 7 | A rolled-back writer re-reserved a value a peer's committed release had freed | **FIXED** `fca34a0c` |

## 7. Reproduce

```bash
# The gate. Read the status from INSIDE the log, never from a wrapper.
make ci > ci.log 2>&1; echo "MAKE_CI_EXIT=$?" >> ci.log; grep MAKE_CI_EXIT ci.log

# #2420 — deterministic, and it fails against the pre-fix behaviour.
go test -count=1 -run '^TestHorizon_OldestNeverExceedsItsFallback$' ./graph/mvcc/
go test -count=1 -run '^TestHorizon_StaleLeaveIsDetected$' ./graph/mvcc/
go test -count=1 -race -run '^TestVacuum_WatermarkRegressionIsDetected$' ./graph/lpg/

# #2420 — the concurrent arm. THE ENVIRONMENT IS PART OF THE REPRODUCTION: the
# whole package, NO -run filter, with the tree-wide race suite as peer load.
go test -race -count=1 -timeout 30m ./... > peer.log 2>&1 &
go test -race -count=1 -timeout 30m ./graph/lpg/

# #2366 and #2423 — both deterministic.
go test -count=1 -run '^TestUnique_RollbackLeavesNoPhantomReservation$' ./cypher/
go test -count=1 -run '^TestIndexSeek_LabelGuard' ./cypher/
```

## 8. Commits

| commit | what |
|---|---|
| `9167d3d3` | fix(mvcc): the reclamation watermark is CAPPED by its fallback (#2420) |
| `fca34a0c` | fix(cypher): defer UNIQUE releases to commit, and qualify every index seek by its label (#2366, #2423) |
