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

**CERTIFIED for production use under extreme load and concurrency, WITHIN THE
ENVELOPE STATED IN §5.**

The certification is granted rung by rung, in the project's own order:

- **Correct.** The gate is green at the **exit commit** — `MAKE_CI_EXIT=0`, read from
  inside the log, not from the wrapper: 123 packages ok with zero `FAIL` lines under
  `go test -race ./...`, `golangci-lint` with 0 issues, the openCypher TCK at
  **3897/3897**, coverage 87.1 % aggregate with every package above its 75 % floor. It
  ran three times across the cycle — mid-cycle, after the last correctness fix, and at
  the exit commit — and all three are green; the later runs are what matter, because each
  covers fixes the earlier ones could not.

  The blocker that refused the 2026-08-12 certification is closed **at its root cause**
  (§1), not worked around, and the mechanism is pinned by a deterministic test that
  fails against the previous behaviour. Three further defects were found and
  fixed, one of them a p9 silent wrong answer that a phantom reservation had been
  masking (§3) and one a permanent breach of the bounded-resources mandate (§4.2). ACID
  Isolation now carries three permanent, always-on detectors for the corruption class
  that produced #2420, each proven on a broken control as well as a sound one.
- **Secure.** Nothing new was examined this cycle, and nothing regressed: the previous
  cycle's container-aware ceilings (#2421) stand, with their one unmet criterion — no
  container was ever run — restated in §5 rather than quietly retired.
- **Efficient.** The READ path costs nothing measurable (§4.1); the one microcost is
  quantified — `Horizon.Leave`'s extra store is +7.55 % of a 4 ns operation, which is
  0.0005 % of the read transaction containing it — and allocation rises 0.39–1.58 % on
  reads under concurrent writers, with the mechanism named as a hypothesis rather than a
  diagnosis. The WRITE path carries a **real, reproduced cost of 3–10 % on CONSTRAINED
  writes only** (§4.3), with the unconstrained control flat. It is accepted, not hidden:
  it is the price of closing a consistency defect, correctness outranks speed by this
  project's order, and it is filed as rmp #2425 with three candidate causes already
  eliminated.
- **Fast.** No regression, and nothing is claimed as faster: this cycle bought
  correctness, not throughput.

**The condition is discharged.** rmp #2420's acceptance criterion asks for **150
package runs** at the corrected recipe with zero violations. The full 150 ran on the
final tree: **150 clean, zero firings of all five oracles**, verified as genuine
executions rather than cache replays (§1.4.1).

The criterion was honoured to the letter rather than reinterpreted downward on the
grounds that the new detector — which fired ~5 times per run before the fix, against
the symptom's own ~2 % per run — already made forty runs the stronger evidence. **That
decision paid for itself:** the runs the criterion demanded are what found #2424.

**And a fourth defect changes the verdict's shape.** Three of the four defects closed
this cycle were found BY this certification rather than inherited by it, and two of
them were found by fixing another one or by running the arm a criterion demanded. The
honest reading is not "the module is now correct" but "this module rewards being
measured, and the measurements are not exhausted" — which is why the envelope in §5 is
part of the verdict rather than a footnote to it.

**What "certified within an envelope" means here.** It means the module is fit for the
workloads this report measured, on the platform it measured them on, with the
guarantees it verified. It does **not** mean the eight items in §5 are sound — they are
unmeasured, and two of them — **latency at the published concurrency levels** and the
**whole-tree soak layer** — have now gone unmeasured for three consecutive
certifications, which is itself a finding worth acting on.

The write path is no longer among them: §4.3 measures write scaling at 1–32 writers for
both a constrained and an unconstrained schema. What remains unrun there is the
different, store-level sweep in `store/txn/write_scaling_bench_test.go`, and §5 says so
in those terms rather than leaving "the write-scaling sweep" ambiguous between the two.

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

**The ticket's own criterion asks for 150 runs, and it is being honoured to the
letter.** A second arm of 110 runs is running on the final tree — the tree including
the two follow-ups in §6.1 — bringing the total to 150 at the corrected recipe. The
result is recorded in §1.4.1 rather than summarised here, so that the number reported
is the number measured.

#### 1.4.1 The second arm found a defect instead, and the third one discharged the criterion

The second arm was **stopped at 71 runs** — 70 clean, and one failure that was **not
one of the five oracles**. It is worth more than the 110th run of a clean arm would
have been:

```
--- FAIL: TestVacuum_CommitPathPerformsNoReclamation (5.23s)
    version memory did not settle within 5s: 4883 records held against a bound of 4096
    (ceiling 16384), with 0 ACTIVE READERS, 0 unregistered, oldest reader age 0;
    vacuum {Running:FALSE Starts:2 Exits:2 Passes:214 Reclaimed:11502 BACKLOG:3767 …}
```

No reader was holding anything back, and the sweeper had **exited with 3767 records of
reclamation debt outstanding**. Filed as **rmp #2424** and fixed; §4.2 has the account.

A third arm then ran the **full 150** on the tree that includes that fix, because a
150-run demonstration of a superseded tree demonstrates nothing about the shipped one:

> **150 runs, 150 clean, zero firings of all five oracles.**

The arm's own execution was verified rather than assumed, which on this project is not
a formality — a previous cycle published four parameter sweeps that were all the same
cached run. Run 1 at 12:00:24 and run 150 at 14:22:59, so **2 h 22 min** of real
elapsed time; per-run package durations spread **35.3 s to 71.9 s**, which is the
signature of 150 genuine executions under varying peer load; and `-count=1` on every
invocation with **zero** runs reporting `(cached)`.

That discharges #2420's acceptance criterion 2 in full, and criterion 3 with it: the
sibling oracle `TestIsolation_CrossSubstructure_EdgeImpliesLabels` lives in the same
package and therefore ran in all 150.

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

Everything changed this cycle sits on a read path, so the question is not whether the
module got faster — it is whether three correctness fixes cost anything measurable.
Four arms, **interleaved** HEAD against `30b5ecb3` one round each and repeated ten
times, because a back-to-back A/B on this host manufactures significant-looking deltas
from nothing (a byte-identical control once produced 22 of 36 flat-by-construction rows
as "significant"). rmp #2420's own acceptance criterion asks for exactly this shape,
`n >= 10`.

What each arm is for:

| arm | what it exercises of the change |
|---|---|
| `BenchmarkHorizonReal64` (`graph/mvcc`) | `Enter`/`Leave` — `Leave` gained a store — and `Oldest`, which lost its `found` flag |
| `BenchmarkIndexSeek_vs_LabelScan` (`cypher`) | the residual label test the seek now applies per candidate |
| `BenchmarkReadAtRealisticWriteRate` (`bench/mvccwrite`) | reads under CONCURRENT WRITERS: the chain walk, the visibility memo and the horizon together |
| `BenchmarkEngReadProjectLargeSerial` (`bench/mtaudit`) | the benchmark #2420's criterion names — expected byte-identical, and why is the point |

The last two are a pair, and the pairing is the lesson the previous cycle recorded:
`EngReadProjectLargeSerial` reads a quiescent graph, which has no delta chain to walk,
so it cannot see a change to the chain walk at all — it reported 8.23 ms against
8.21 ms with `B/op` and `allocs/op` byte-identical, which on this project is the signal
that an arm never reaches the code under test. The concurrent-writer arm is what
actually exercises it, and it is the measurement that cycle recorded as OWED.

### 4.1 Results — nothing the module does measurably slower, and one honest microcost

`sec/op`, HEAD against `30b5ecb3`, ten interleaved rounds, `n=10`:

| arm | sec/op | verdict |
|---|---|---|
| `ReadAtRealisticWriteRate/writers=0` | 61.34 µ → 61.03 µ | ~ (p=0.912) |
| `ReadAtRealisticWriteRate/writers=1` | 61.88 µ → 61.86 µ | ~ (p=1.000) |
| `ReadAtRealisticWriteRate/writers=4` | 63.37 µ → 63.74 µ | ~ (p=0.063) |
| `IndexSeek_vs_LabelScan/IndexSeek` | 956.4 µ → 959.9 µ | ~ (p=0.853) |
| `IndexSeek_vs_LabelScan/LabelScan` | 6.015 m → 6.117 m | ~ (p=0.436) |
| `EngReadProjectLargeSerial` | 8.068 m → 8.118 m | ~ (p=0.971) |
| `HorizonReal64/enter-leave/near-empty` | 4.072 n → 4.379 n | **+7.55 % (p=0.000)** |
| `HorizonReal64/enter-leave/near-full` | 7.676 n → 7.853 n | **+2.31 % (p=0.000)** |
| `HorizonReal64/oldest/near-full` | 718.4 n → 724.4 n | **+0.84 % (p=0.006)** |
| `HorizonReal64/oldest/near-empty` | 7.869 n → 7.877 n | ~ (p=0.754) |

**The microcost is real and it is 0.3 ns.** `Horizon.Leave` gained one store, and on a
4-nanosecond operation that is +7.55 %. `Enter`/`Leave` run once per read
transaction, and a read transaction in this same tree costs ~61 µs — so 0.3 ns is
**0.0005 %** of it, which is why the end-to-end arms show no change at all. Both
numbers are reported because quoting only the microbenchmark would overstate the cost
and quoting only the end-to-end arm would hide that it exists. Zero allocations on
every horizon arm, unchanged.

**Allocation, where it moved:**

| arm | B/op | allocs/op |
|---|---|---|
| `IndexSeek` | +0.03 % (p=0.000) | 2 913 → 2 915, +0.07 % |
| `ReadAtRealisticWriteRate/writers=1` | +0.39 % (p=0.000) | +0.05 % |
| `ReadAtRealisticWriteRate/writers=4` | +1.58 % (p=0.000) | +0.22 % |
| `EngReadProjectLargeSerial` | byte-identical | byte-identical |
| `HorizonReal64` (all four) | 0, unchanged | 0, unchanged |

The seek's two extra allocations per operation are the residual-predicate closure,
built once per plan build rather than per row — which is why the time is unmoved.

The `writers=4` figure is +1.58 % of bytes with **no** time cost, and it grows with
the writer count. The expected mechanism is the fix itself: a watermark capped at the
fallback frees slightly less on a pass whose readers are all newer than it, so a few
more version records are live when the allocation is sampled, and the next pass frees
them. **That is a hypothesis, not a diagnosis** — it was not separately measured, and
this project has a standing rule that a plausible mechanism named for a number is not
the same as testing it. What is established is the size and the direction.

**And the pairing predicted the right thing.** `EngReadProjectLargeSerial` — the
benchmark #2420's own criterion names — came out byte-identical in both memory
columns and statistically flat in time, exactly as §4 said it would: it reads a
quiescent graph, so it never walks a chain and never touches the changed code. Had it
been the only arm, the honest conclusion would have been "nothing to see", and the
concurrent-writer arm is what gives that claim any content.

---

### 4.2 FIXED — rmp #2424: version memory could settle ABOVE the stated bound, permanently

The 150-run arm's own peer load found this, which is the argument for running the arm
at all rather than reasoning about it.

`MVCCStats` admits exactly two reasons for retained versions to exceed `Bound`: a
reader is legitimately holding them back, or the vacuum has not yet caught up with a
burst — and `Ceiling` bounds the second. The observed state was **neither**: zero
active readers, the sweeper **exited**, and 3767 records of reclamation debt
outstanding with 4883 records held against a bound of 4096.

**Mechanism.** `Graph.chargeReclaimDebt` signals the vacuum only once the accumulated
debt passes `reclaimThreshold`. The threshold amortises the SIGNAL, and that is sound
only while a sweeper is alive to do the work eventually. With none alive the debt is
invisible to everything: the churn signal fires only above the threshold, and the drain
signal only when a reader leaves. A workload that stops **just short** of the threshold
after the sweeper's idle exit therefore keeps its versions for the life of the process.

**Fix.** A sub-threshold charge now ensures a sweeper EXISTS, testing `running` first so
the ordinary path pays one atomic load of a line written only when a sweeper starts or
exits — not a channel send per commit. The vacuum loop's exit path already re-checks the
wake CHANNEL after clearing `running`, so a sender that reads `running` false starts a
sweeper itself; this is the same cover for the case that leaves no signal at all.

**Pinned by the invariant, not the symptom.** The symptom needs a residue *plus* a burst
to cross the bound, which makes it a race to assert — it appeared once in 71 runs. The
invariant is exact: there must never be an outstanding backlog with `Running` false. It
fails deterministically against the previous build with **4095 records stranded**, and a
proportionality control asserts the amortisation survives (the sweeper starts a small
constant number of times across 16 384 modifications, not once per commit).

**Attribution is NOT established, and is stated as unknown.** The mechanism is
independent of §1's watermark fix. It is possible that capping the watermark made passes
free less and so reach the idle exit sooner, raising the frequency without being the
cause; that was not measured, and the failure mode is the one the existing debt guard's
own comment already describes.

**The first fix was too aggressive, and four test fixtures said so.** It woke the vacuum
on ANY sub-threshold charge, which starts a sweeper for a single write on an idle graph.
Four white-box tests then failed in succession — two reclaim tests, the adjacency-stamp
bound test, and a label-delta accounting test — every one of them because it counts live
versions and had been deterministic only while no sweeper existed to race it. Patching
them one at a time would have been treating the messenger: a debt below the threshold
cannot breach the bound, because the bound IS that threshold, so the condition was
simply wrong. It is now the mandate's own statement — retention above the bound with no
sweeper alive — and it perturbs none of them.

Two of those fixtures keep an explicit hold anyway, because they compare SEPARATE
samples of a live count and that agreement should not depend on the wake policy. Their
comments say which of the two reasons applies, so a later reader is not told the hold is
required when it is defensive.

**Two white-box tests were passing only because no sweeper was alive.** They count
deltas and drive `ReclaimVersions` from a PRIVATE `mvcc.Horizon` the graph knows nothing
about, so a background pass is entitled to free the same records. Their shared fixture
now claims a graph horizon slot with `EnterHolding` — the documented
hold-everything-back state — which suspends the sweeper's EFFECT rather than its
existence. That is the third and fourth test this cycle found to be passing for a reason
other than the one it asserted.

---

### 4.3 The write path — a measured cost, and three attributions I had to retract

rmp #2366 and #2424 both require an interleaved write-contention comparison, and it is
the one measurement of this cycle that came back negative. It also took five arms,
because four of my own conclusions along the way were wrong.

**The benchmark had to be built, and its first version measured itself.**
`BenchmarkWriteScaling` covers a schema declaring NOTHING, which cannot show what the
constraint machinery costs a workload that has one, so `BenchmarkWriteScalingUnique` was
added: each writer moves its own nodes' constrained value to a fresh one, driving both a
release and a reserve per commit. Its first version found the node by `k`, which nothing
indexes, making the lookup a LABEL SCAN over a population of 64 × writers. It reported
24.4k commits/s falling to 7.8k at 32 writers — **a 0.32× "collapse" under a UNIQUE
constraint**, which would have been a headline finding about extreme concurrency and was
entirely the fixture. Seeking the indexed property instead: **234k at one writer, 311k
at thirty-two, scaling 1.33×**. A per-operation cost that moves with the peer count is a
fixture bug every time, and this project has manufactured that exact result before.

**The result, from two independent interleaved arms (n = 10 each) against the cycle's
entry commit:**

| writers | constrained (`WriteScalingUnique`) | unconstrained control |
|---:|---|---|
| 1 | **−6.05 %** (p=0.000) | ~ |
| 2 | **−2.82 %** (p=0.035) | ~ |
| 4 | ~ | ~ |
| 8 | ~ (p=0.063) | ~ |
| 16 | **−10.12 %** (p=0.002) | ~ |
| 32 | **−9.09 %** (p=0.035) | ~ |
| geomean | **−3.23 %** | flat |

The shape is informative: ~6 % at ONE writer is added per-statement work, not contention,
and the further loss at 16 and 32 is a contention component on top of it.

**Three named causes, all cleared by measurement rather than by argument:**

| suspect | how it was tested | verdict |
|---|---|---|
| the per-transaction map in `ConstraintTxn` | replaced with a four-slot inline array + spill | **cleared** — the number did not move |
| #2424's sub-threshold vacuum wake | arm isolating it (HEAD vs the commit before it) | **cleared** — HEAD is FASTER, +4.7 % to +12.5 % |
| #2423's per-candidate label guard | arm with the guard branch disabled, one line apart | **cleared** — geomean **+0.06 %**, five of six arms flat |

**And one phantom of my own.** The first arm also showed the UNCONSTRAINED control
regressing 6.9 % and 7.7 %, which none of this cycle's changes can reach — with no
constraint declared the whole path short-circuits on an atomic load. It ran immediately
after a 2.5-hour 150-run arm and a full `make ci`; re-run on a rested host the control is
flat on every writer count. That is the rule about back-to-back A/Bs on this machine
being worthless, arriving as a bill rather than as advice.

**What remains, and what is NOT claimed.** The cost belongs to #2366's deferred-release
machinery, by elimination rather than by attribution — the next step is a profile, not
another A/B, and it is filed as **rmp #2425** with the three cleared suspects recorded so
they are not re-proposed. It is **not** grounds to revert: #2366 closes a consistency
defect in which two nodes held one UNIQUE value, and correctness outranks speed in this
project's own order. So the certification records a 3–10 % cost on constrained writes as
**the documented price of that fix**, which is the one thing the mandate asks of a
regression it accepts.

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
4. **The STORE-LEVEL write-scaling sweep was not run**
   (`store/txn/write_scaling_bench_test.go`). §4.3 measures write scaling through the
   Cypher engine at 1–32 writers for two schemas, which is a different benchmark against
   a different surface; this one exercises the store API directly and remains unrun.
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
9. **rmp #2366's write-contention criterion is NOT yet discharged.** It asks for
   `BenchmarkWriteScaling/mem` at 1/4/16/32 writers, interleaved with `n >= 10`, for a
   schema declaring UNIQUE **and** one declaring nothing. §4.1 measured the read path,
   the horizon and the seek — not that. The change's direction on the write path is
   favourable by construction (a deferred release takes **no** registry lock where the
   eager one took the global write lock, and the commit takes it once per transaction
   instead of once per release), and this registry's lock is on record as 57 % of all
   lock delay at sixteen writers. But "favourable by construction" is exactly the kind
   of claim this project requires to be measured, so it is listed here as owed rather
   than asserted.

---

## 6. Findings

| id | severity | title | status |
|---|---|---|---|
| #2420 | 9 / 9 | The reclamation watermark could exceed a live reader's start instant | **FIXED** `9167d3d3` |
| #2423 | 9 / 9 | An index-seek rewrite dropped the label predicate; a self-contradictory row | **FIXED** `fca34a0c` |
| #2366 | 8 / 7 | A rolled-back writer re-reserved a value a peer's committed release had freed | **FIXED** `fca34a0c`, `3b7bec2f` |
| #2424 | 8 / 8 | Version memory could settle above the stated bound, permanently: the sweeper exited with an outstanding debt | **FIXED** `f3e8f48f`, `5909b3ad` |
| #2425 | 6 / 3 | The deferred UNIQUE release costs 3–10 % of constrained write throughput; cause not yet located | **OPEN — measured, three suspects cleared** |

### 6.1 A defect this cycle introduced, and caught before shipping

#2366's first fix moved the problem instead of solving it. `ReserveSetProperty` spends
the transaction's own pending release of the value it takes, so for those labels the
value was already in the shared set — put there by a COMMITTED writer — and the
reservation's inverse must restore the mark, not delete the value. It deleted:

```
a holds 'v'
SET a.email = 'moved'   releases 'v' (a mark; nothing shared changed)
SET b.email = 'v'       reserves 'v', allowed because THIS transaction freed it
ROLLBACK                inverses run LIFO: the reserve's inverse deleted 'v'
```

leaving `'v'` free while node `a` still held it — **two `:Person` nodes carrying one
UNIQUE value**, which is the CONSISTENCY direction and strictly worse than the
availability defect #2366 set out to fix.

It is recorded here rather than quietly amended for two reasons. The whole `cypher`
suite passed with the hole in it, so nothing but review would have found it. And a fix
that trades one direction of a defect for the other has moved it, not closed it —
which is why both directions are now pinned by tests
(`TestUnique_RollbackLeavesNoPhantomReservation` and
`TestUnique_ReleaseThenReserveOfTheSameValueRollsBackWhole`), and the second one fails
against the first fix.

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
| `3b7bec2f` | fix(cypher): a reserve that spends its own pending release must RESTORE it on rollback (#2366) |
| `f3e8f48f` | fix(lpg): a reclamation debt below the wake threshold must still find a sweeper (#2424) |
| `5909b3ad` | fix(lpg): wake the vacuum when RETENTION exceeds the bound, not on any debt (#2424) |
| `c69fcc5b` | test(bench): write scaling under a UNIQUE constraint, beside the schema that declares nothing (#2366, #2424) |
| `29483335` | perf(cypher): hold a transaction's UNIQUE releases inline, and retract the cause I had named |
