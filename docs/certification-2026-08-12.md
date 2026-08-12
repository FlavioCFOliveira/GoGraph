# Production certification — extreme load and concurrency — 2026-08-12

Scope: certify that GoGraph is fit for production environments of extreme load and
concurrency, that CPU is spent effectively, that RAM is held only for what is necessary,
and that concurrency is handled correctly. Ranked throughout by the project's own order:
**correct → secure → efficient → fast**.

Entry commit: `50bf53fd` (branch `sprint-342`, sprint 342 closed).
Working branch: `sprint-343`. Host: Apple M4, 10 cores, 32 GB, `darwin/arm64`, Go 1.26.

---

## Verdict

**NOT CERTIFIED.**

The certification is refused on the first rung — **correctness** — and therefore does not
proceed to a verdict on the others. Two findings are sufficient on their own:

1. **The gate went RED at the entry commit.** `make ci` exited **2** because a reader
   holding a pinned MVCC snapshot observed **half of one transaction**. That is a direct
   breach of Compliance Mandate 2 (100% ACID), Isolation clause: *"Readers never observe the
   partial writes of an in-flight transaction."* Filed as **rmp #2420**. Not yet fixed.

   **Read that as intermittent, not deterministic.** The defect fires at roughly **1.2% per
   invocation** of its test (§2.1), so `make ci` passes far more often than it fails. The
   three full gate runs of this cycle were:

   | run | tree | result |
   |---|---|---|
   | 1 | entry commit `50bf53fd` | **`MAKE_CI_EXIT=2`** — the Isolation tear |
   | 2 | `sprint-343`, mid-cycle | `MAKE_CI_EXIT=2` — **lint**, on a defect of mine (§1); test stage green |
   | 3 | `sprint-343`, mid-cycle | `MAKE_CI_EXIT=0` — green |
   | 4 | `sprint-343`, exit | **`MAKE_CI_EXIT=0`** — green, coverage 87.1% |

   Beyond the gate itself, the targeted reproduction ran the `graph/lpg` package **240 more
   times** under peer load across this cycle, producing **3 further violations** — the three
   observations §2.1 is built on.

   The green runs are **not evidence that #2420 is fixed**, and are not offered as any. Nothing in this
   cycle touched the mechanism; at a 1.2% rate a green run is simply the likely outcome.
   Running the gate once and seeing green does not contradict this report — it is what the
   measured rate predicts. **An intermittent breach of an ACID guarantee is still a breach**,
   and a defect that surfaces in roughly one CI run in eighty is more dangerous than a
   deterministic one, because it will be dismissed as flakiness.
2. **A silent wrong answer in openCypher conformance**, confirmed by measurement at the
   entry commit: a reverse-direction, type-filtered `MATCH` over parallel relationships
   returned an over-count on one type and **zero** on every other. Filed long ago as
   **rmp #2250**; **fixed in this cycle** (commit `588f75b8`).

A third defect, **rmp #2366**, is confirmed deterministically at HEAD and remains open:
a UNIQUE value becomes permanently unusable, leaking one reservation per occurrence.

Nothing here is a performance objection. The efficiency and speed axes were not the
binding constraint and are not what refuses the certification.

### What "not certified" does and does not mean

It means the module **must not be relied upon for a workload whose correctness depends on
snapshot isolation under concurrent writes**, until #2420 is diagnosed and closed. It does
not mean the engine is broadly unsound: 332 sprints of prior work, a green TCK at
3897/3897, and the crash-injection and WAL-recovery batteries all stand. The defects are
narrow, specific, and now precisely characterised.

---

## 1. Method, and the two things that would have made this report wrong

**The harness lied about the gate, exactly as expected.** `make ci` was run with its exit
status recorded *inside* the log rather than read from the wrapper. The wrapper reported
**"exit code 0"**; the log recorded **`MAKE_CI_EXIT=2`**. The trailing `tail` in the
command is what the wrapper's status described. Had the wrapper been believed, this
certification would have opened by declaring a red gate green — which is how the same
mistake has been made on this project before.

**The gate caught a defect I introduced while fixing another one.** The second full `make ci`
of this cycle exited 2 with the entire race-enabled test stage green: `golangci-lint`'s gosec
raised G304 on the `os.ReadFile` in the new `internal/memlimit`. It was a false positive on
the substance — the path is one of two unexported package constants — but it was a real gate
failure, and it is recorded here because the lint stage is the stage most easily dismissed as
noise. Fixed in `1a38378a` with the justification the rest of the tree already uses.

**A rate stated from one sample was wrong, and the pre-stated criterion caught it.** The
first reproduction of #2420 gave 1 failure in 20 invocations, and I recorded "~5%". The
criterion written down *before* the next run said that 100 invocations should then yield
about 5 failures (range roughly 1–11). The run produced **0 in 100**, which at 5% has a
probability of 0.6%. The honest reading is that the rate is **~1.2%** (2 observations in
161 invocations at HEAD), not 5%. No claim in this report rests on the discarded figure.

---

## 2. Correct

### 2.1 BLOCKER — rmp #2420: a pinned snapshot observed a partial transaction

`make ci` at `50bf53fd` fails in `graph/lpg`:

```
--- FAIL: TestIsolation_ApplyAtomically_View_NoPartialReads (38.89s)
    isolation_test.go:276: observed 1 partial-transaction violations
                           (a.v != b.v inside a pinned SNAPSHOT)
```

A writer sets `a.v` and `b.v` to the **same** loop counter inside **one**
`ApplyAtomicallyTx`. Eight readers pin a snapshot and read both. The invariant is that a
snapshot observes all of a transaction's writes or none of them. It observed some.

**Reproduction and rate.** The environment is part of the reproduction — this defect's
sibling (#2378) returned green in five substitute environments. Under the full race suite
as concurrent peer load:

| run | invocations | failures |
|---|---:|---:|
| `make ci` | 1 | 1 |
| targeted, peer load | 20 | 1 |
| targeted, instrumented, peer load | 40 | 0 |
| targeted, instrumented, **continuous** peer load | 100 | 0 |
| **total at HEAD** | **161** | **2 (≈1.2%)** |

**What has been established by measurement, not by reading.**

- The two writes **share one `commitInfo`** — verified directly by a white-box probe
  reading both delta chains after an `ApplyAtomicallyTx` (`info=0x…e4d8` for both). So the
  visibility predicate `mvcc.Visible` is fed the *same* timestamp for both properties and
  cannot, by itself, return different answers.
- The two nodes hash to **different property shards** (`sameShard=false`). So the two
  reads consult two independent per-shard gates and two independent delta chains.

**What has been excluded by reading the source (each is a refutation of a hypothesis I
formed, not a diagnosis).**

- The commit-publish order is correct at all three sites: `info.Commit(ts)` precedes
  `PublishCommitTS(ts)` (`graph/lpg/mvcc_write.go:537-538`, `mvcc_txn.go:182-183`), which
  is the ordering `mvcc.Clock.ReadTS` documents as the fix for exactly this tear.
- The horizon claim order is correct: `BeginRead` calls `EnterHolding()` *before*
  `ReadTS()` (`graph/lpg/snapshot.go:154-160`), and `Horizon.Oldest` returns 0 — suspending
  reclamation entirely — both when a slot is unpublished and when any reader failed to
  register.
- The reclamation watermark is conservative: a stale watermark frees strictly less, and
  `ReadTS()` is monotonic, so no later reader can start below a watermark already used.

**What the previous cycle already refuted, and why it does not close this one.** Commit
`1d5f6609` records that for the *label* tear, instrumentation fired at the violation found
**both nodes holding live delta chains**, so the reclaim is **not** dropping a chain a live
snapshot still needs, and the `d == nil` present-time fallback is **not** taken. That is
eight mechanisms refuted by measurement. Its remaining lead is that labels and adjacency
classify visibility through **different machinery** — `commitInfo.TS()` versus the
adjacency's own write stamp. **That lead cannot explain this failure**: this arm is
property-versus-property, both through the same `propBagAsOfLocked` and the same shared
record. This is a distinct tear, and it is now narrowed to a per-shard difference in how
two reads of one snapshot resolve.

**The exposure does not scale with reads — which is a finding in itself.** An amplified probe
running the identical workload with ten times the transactions per invocation was run under
continuous peer load, and its capture was first **validated against a deliberately
unversioned reader**, which produced 48 283 violations in 356 984 reads. So the instrument
can fire, and a clean result from it is evidence rather than an instrument that cannot fail.

It then recorded **0 violations in 290 236 345 reads across all 25 invocations.**

| model | expected violations in that run | P(observing 0) |
|---|---:|---:|
| exposure scales with **reads** (1 per ≈113M, from the shipped test) | ≈2.6 | ≈8% |
| exposure scales with **invocations** (≈1.2% per invocation) | ≈0.30 | ≈74% |

The per-invocation model fits the data substantially better. That points at a **transient
condition near the start of a workload** rather than a steady-state race. There is such a
thing: `reclaimThreshold` is 4096 versions, so the vacuum first engages after roughly 2048
two-property transactions and then keeps running — a 50 000-transaction workload and a
500 000-transaction one each cross that boundary exactly **once**.

**So I tested it, and it is refuted too.** A second probe ran **400 short workloads on fresh
graphs** in one process, each sized past the vacuum threshold, multiplying the transient
instead of the read count: **0 violations in 34 901 832 reads across 400 workload starts.**
At 1.2% per start that predicts ≈4.8 hits, so P(0) ≈ **0.8%**.

| model | prediction for the probe that tested it | P(observing 0) | verdict |
|---|---|---:|---|
| exposure ∝ **reads** | ≈2.6 in 290M reads | ≈8% | disfavoured |
| exposure ∝ **workload starts** | ≈4.8 in 400 starts | ≈0.8% | **refuted** |

### 2.1.1 The methodological error, and what it means for the next cycle

Both probes isolated the test with a `-run` filter. **That is what removed the reproduction.**
`TestIsolation_ApplyAtomically_View_NoPartialReads` calls `t.Parallel()`, and its package
contains **229 `t.Parallel()` calls** — so under `make ci` it executes concurrently with a
large set of sibling tests, each building its own graphs, driving its own vacuum, and
allocating, all inside **one process**. A `-run` filter deletes every one of them and leaves
only *inter-process* peer load, which is a different thing.

This is the identical error recorded for the sibling defect at commit `1d5f6609`, where five
substitute environments returned green and only the real one reproduced it in three minutes.
**Modelling the load is not running it.** The two "refuted" models above are therefore
refutations of *my probes*, and are only evidence about the defect to the extent the probes
shared its environment — which, it turns out, they did not.

**The standing instruction is therefore:** reproduce with the whole `graph/lpg` package running
(`go test -race -count=N ./graph/lpg/`, **no `-run` filter**) under inter-process peer load,
never with an isolating filter.

### 2.1.2 Reproduced in the real environment, and the capture fired

Run under that corrected recipe — whole package, `-race`, inter-process peer load — it
reproduced. **The rate, pooled over both runs at that recipe, is 1 failure in 40 package runs
(≈2.5%)**: 1 in 15, then 0 in a subsequent 25. The first run alone would have said 6.7%, and
quoting that would have repeated this cycle's own single-sample error a third time. The self-diagnosing oracle produced the first real observation this
defect has ever yielded:

```
first observation a.v=36406 b.v=36408 (delta -2,
  the LATER read (b) saw the newer transaction — the visibility basis moved FORWARD
  between the two reads);
re-read of the SAME snapshot (startTS=36407) gave a.v=36408 b.v=36408 —
  TRANSIENT — the same pinned snapshot AGREED on re-read
```

Three facts fall out of it, and together they are the most specific evidence this defect has
produced:

1. **The distance is 2, not 1.** A single transaction observed half-applied would read as a
   difference of one. Two transactions apart is time passing between the reads, not one
   write landing between them.
2. **The snapshot's answer for `a` CHANGED under the pin**: 36406 on the first read, 36408 on
   the re-read, same `*Snapshot`. A pinned snapshot re-reading one property and getting a
   newer value is the violation in its purest form, and it is a stronger statement than the
   original `a.v != b.v`.
3. **Both properties converged on the newer value**, and `startTS=36407` sits *between* the
   two observed values.

**What that points at — stated as a direction, not a diagnosis.** All three are consistent
with the reads resolving to **present-time values** rather than as-of the snapshot, i.e. one
of the fall-through paths being taken: `sh.d == nil` at shard level
(`graph/lpg/snapshot_read.go:193`) or `s.d[id] == nil` at node level
(`graph/lpg/mvcc_props.go:151-159`), either of which returns the CURRENT bag. That is the
family the previous cycle refuted **for labels** — and it has never been tested for
**properties**, which is precisely this arm.

### 2.1.3 The substrate capture fired, and it refutes the fall-through reading

An 80-run capture at the corrected recipe produced a second violation, this time with the
substrate state sampled at the violation:

```
a.v=42607 b.v=42657 (delta -50)   startTS=42608
re-read of the SAME snapshot gave a.v=42657 b.v=42657 — TRANSIENT
substrate at the violation:
  a: live chain, head stamped 85468 | b: live chain, head stamped 42735
```

**Both nodes held LIVE delta chains.** So the fall-through reading of §2.1.2 is **refuted for
properties**, exactly as the previous cycle refuted it for labels: neither the shard-level nil
gate nor the per-node chain-absent branch was taken. Reclamation is not dropping a chain a live
snapshot needs. That is now established for both substructures and should not be re-proposed.

**AND THE OTHER NUMBER WAS MY OWN BUG — RETRACTED.** This report previously recorded
`a`'s head stamp of **85468** as an anomaly, on the grounds that the graph's per-graph clock
can never issue an instant above ~50 001. That claim is **withdrawn**. The stamp was never a
commit timestamp: the capture packed it as `stampTS() << 2` with the low two bits as flags,
and **a stamp is not a small number** — an *in-flight* record carries the transaction id,
which is `mvcc.TxIDBase + k = 2^63 + k`, so the shift **overflowed**:

```
(2^63 + k) << 2  mod 2^64  ==  4k        →  decoder's >>2 prints k
```

The "85468" was the *sequence number of a transaction that was still in flight*, rendered as
a plausible commit timestamp. **The one fact that mattered — that this node's head was
uncommitted — is precisely the fact the encoding erased.**

It was found by a probe that asserted the bound continuously instead of waiting for a tear:
it reported **338 830 breaches in 861 490 samples** with a worst value of
`9223372036854875804`, and `9223372036854875804 − 2^63 = 99 996`. Those are ordinary in-flight
transaction ids, not a defect — the probe's *premise* was wrong, and that is what exposed the
encoding.

**Two lessons, both already in this project's own notes and both re-learned here.** A harness
must never fold a flag into a field drawn from the answer's own value space. And the value
space must be checked against its real range before packing — `stampTS` spans both sides of
`2^63`, which no two-bit shift can survive.

The capture now returns the stamp unmodified and names the state explicitly — `COMMITTED at
N`, `IN FLIGHT as transaction N`, or `ABORTED` — and additionally reports **whether the two
heads share one commit record**, which is the premise the whole guarantee rests on. A fresh 120-run capture with the corrected instrument then produced **two** clean
observations, and they agree:

| # | startTS | a.v → its instant | b.v → its instant | heads |
|---|---:|---|---|---|
| 1 | 17142 | 17141 → **17142** ✓ | 17144 → **17145** ✗ | both COMMITTED at 17160, **sharing ONE record** |
| 2 | 31421 | 31420 → **31421** ✓ | 31421 → **31422** ✗ | both COMMITTED at 31425, **sharing ONE record** |

(The instant of a value is fixed empirically, not assumed: a snapshot pinned immediately
after the seed reports `startTS=1` with `a.v=0`, so value *v* was written by the transaction
that committed at instant *v+1*.)

So in both observations **`a` resolved CORRECTLY for the snapshot's start instant, and `b` was
OVER-VISIBLE by one to three transactions** — the second read admitted a transaction that
committed strictly *after* the snapshot began. And the premise holds under concurrency: the
two heads share one commit record, so the shared-record hypothesis is confirmed rather than
assumed.

### 2.1.4 Two more hypotheses of mine, refuted by measurement

Every observation says **TRANSIENT — the same pinned snapshot agreed on re-read**, with both
properties then at the newer value. That suggested a pinned snapshot's answers **drift forward**
as the writer advances — which, if true, would mean isolation is simply not provided, and the
oracle merely catches it when two reads straddle a write. It would also be deterministic.

**It is not true.** Pin one snapshot and read one property while a writer advances:

| probe | reads | drifts |
|---|---:|---:|
| snapshot pinned on a quiescent graph, held across 20 000 writes | 20 692 | **0** |
| snapshot pinned **mid-stream**, concurrently with the writer (3 runs) | ~62 000 | **0** |

A pinned snapshot is stable, including one born while a writer is running. Both forms of the
drift hypothesis are refuted.

**What that leaves, and it is a sharp localisation.** The failing oracle differs from those
probes in exactly one respect: it creates a **short-lived snapshot per read** —
`BeginRead` → two reads → `EndRead`, thousands of times a second — where the probes hold one
snapshot for the whole run. A snapshot held for 20 000 writes never answers wrongly; a
snapshot that lives for two reads occasionally does, and then answers correctly on the very
next read. **So the fault is bound to the birth of a snapshot concurrent with a commit, and it
self-corrects.** It is not a property of resolution over time.

**Two instruments are therefore in flight for the next cycle**, and both corrections below
were needed before either could be trusted.

**Chain COMPLETENESS, not chain existence.** "Live chain" is established, but a chain can be
live and still be missing the record needed to undo back to a given instant — and the walk in
`propBagAsOfLocked` stops at the first visible record, so an absent record can never be
undone. The oracle now walks the chain at the violation and reports the stamp sequence, whether
the record stamped `startTS+1` is present, and whether the sequence has a gap.

**An ABSOLUTE oracle, which is strictly more sensitive than the one in place.** The shipped
test only fires when the two reads disagree (`a != b`), which needs both reads to straddle a
write. But this fixture makes the correct answer arithmetic — the seed writes value 0 at
instant 1 and transaction *i* writes value *i* at instant *i+1*, so a snapshot at `startTS`
must read exactly `startTS-1`. That fires on **a single wrong read**.

### 2.1.5 Two corrections to my own instruments, both of which had produced clean results

Neither of the following results should be read as evidence about the engine, and both were
nearly reported as such.

**The drift probe was a differential against its own first sample.** It checked that repeated
reads of a pinned snapshot never *changed*, and reported 0 drifts in ~82 000 reads. But a
snapshot that was **already over-visible when created** agrees with itself for ever, so the
probe reports perfect health. It established that the answer is **stable**, never that it is
**correct** — and stability was not the question.

**And both replacement probes were run under a `-run` filter — the very trap §2.1.1 documents.**
Having written that finding down, I then built a drift probe and an absolute-correctness probe
and isolated both, collecting a further "0 wrong in 94 017 reads" that means nothing, because
that environment has already produced 0-in-290M over a defect that reproduces at 2.5% in the
real one. Writing the rule did not disarm the reflex; re-reading this report did. **Both
probes have been moved into the package, given eight concurrent readers, and run with no
filter.**

**Work landed for the next cycle.** The oracle was given the self-diagnosing treatment its
sibling received at `469aea82` and did not have: it now captures the first violating pair,
classifies the direction, and — the decisive addition — **re-reads both properties from the
same pinned snapshot**, which separates a transient window from a snapshot that resolves
inconsistently as a matter of state. Because the writer sets both properties to the loop
counter, the captured values *are* the transactions that wrote them and their difference is
how many transactions apart the two reads landed.

**Mechanisms refuted this cycle, by reading the code at the named lines.** Recorded so they
are not re-proposed: the commit-publish order; the horizon claim order; the conservatism of
the reclamation watermark; and the property WRITE path, which pushes the undo record and
stores the bag under one acquisition of the same shard lock, leaving no window in which the
stored value is newer than the chain that would undo it (`graph/lpg/property.go:313-360`).

**Status: OPEN. This is the certification blocker.**

### 2.2 FIXED — rmp #2250: a reverse type filter admitted the pair, not the instance

Confirmed at the entry commit, reproducing the ticket's matrix cell for cell. `want` is
`nDest` for every type in every direction:

| nPar | nDest | type | fwd | rev | rev (anon) | undirected | want |
|---:|---:|---|---:|---:|---:|---:|---:|
| 2 | 1 | T1 | 1 | **2** | **2** | 1 | 1 |
| 2 | 1 | T2 | 1 | **0** | **0** | 1 | 1 |
| 3 | 2 | T1 | 2 | **6** | **6** | 2 | 2 |
| 3 | 2 | T2, T3 | 2 | **0** | **0** | 2 | 2 |

Both directions of error at once — a silent over-count on the first type and a silent zero
on every other — on data whose forward traversal was correct throughout.

**Cause.** `Expand.reverseEdgePassesFilter` resolved the **pair** and not the **instance**.
The edge-type filter is keyed by forward position, so the reverse hop mapped itself back
with a lower bound, which returns the *first* slot carrying that destination, and then
applied that one slot's verdict to every parallel sibling.

This is the same class of defect `revTypeAdmitSet` was built for on the `shortestPath` side
(#2236), whose own godoc says a reverse-side type test that resolves the position first is
"either slow … or permissive". `Expand` was never migrated.

**Fix.** The reverse slot already carries the handle identifying its own instance, and since
#2141/#2142 the forward run for a pair is contiguous, so `matchFwdByHandle` resolves the
exact sibling in **O(log d + r)**. `revTypeAdmitSet` was deliberately *not* used: this
operator's `Init` runs once per outer row, and an O(V+E) table built there is precisely what
#2220 measured as a 26% regression. The lower bound remains the fallback for a CSR with no
handles, where a pair occupies a single slot and the two answers agree.

**Why the TCK could not catch it.** The TCK is green at **3897/3897** both before and after.
No scenario walks a type-filtered reverse pattern over parallel relationships of *distinct*
types between one ordered pair — the exact and only shape that reproduces it.

**Note on the undirected column.** It was *correct* while the reverse arm was wrong, because
cyphermorphism dedupes the two hops on the handle and hid the over-admission. It is kept as
a guard, not mistaken for a witness.

**Evidence.** `TestOrdering_ParallelEdgeTypes_ReverseDirection` covers forward, reverse
anchored, reverse anonymous, undirected and multi-type `[r:A|B]` across nPar 1–3 and
nDest 1–2. It was **demonstrated by injection** to fail on the pre-fix resolution and pass
after. `go test -race` on `./cypher/...` and `./graph/...` exits 0. Commit `588f75b8`.

### 2.3 OPEN — rmp #2366: a UNIQUE value becomes permanently unusable

Confirmed at HEAD, **deterministically**, with no timing race. The harness already contained
the interleaving; its table deliberately omitted the arm, because — as the file itself says
— "encoding it here as expected would be encoding a defect as correct". Adding the arm:

```
--- FAIL: TestUnique_RollbackLeavesNoPhantomReservation/peer_commits
    PHANTOM: no live :Person holds 'old', yet it is still reserved:
    UNIQUE constraint on (Person).email: value "old" already exists
```

A rolled-back writer's journaled inverse re-reserves a value that a peer's **committed**
release had already freed. Fail-safe in direction — a value becomes unusable, never
duplicated — so it is an availability defect rather than a consistency one. **But it
accumulates**: one leaked reservation per occurrence, for the life of the process, which is
exactly the profile that matters under sustained load.

**Design work completed this cycle.** The user selected *version the value-set entries*. In
deriving it I traced **two interleavings that defeat the simpler schemes**, and they are
worth recording so they are not re-proposed:

- *"Journal the inverse only when the forward action actually changed state"* fixes the
  reported ordering but **not** the one where T2 releases first (really removing the value),
  T1's later release is a silent no-op, and T2 then rolls back and re-inserts.
- *A sequence-number guard alone* ("apply the inverse only if nobody touched the entry
  since") fixes that second ordering but **not the reported one**, because when T2 rolls
  back, T1 has not yet committed and so nothing has moved the entry.

The structure that is order-independent by construction holds, per entry, the **committed**
state plus each uncommitted transaction's own contribution; rollback drops that
transaction's contributions and commit folds them in. Rollback then needs no inverse journal
for the value-set at all.

**Status: OPEN — designed, not implemented.** Its acceptance criteria additionally require
interleaved contention arms with `benchstat` at 1/4/16/32 writers on both a UNIQUE schema
and a bare one, because the registry lock this touches was measured at **57% of all lock
delay at sixteen writers on a schema with no constraints at all**.

---

## 3. Secure

### 3.1 Verified in force — the engine-wide ceilings are finite

The two ceilings the 2026-08-10 certification made finite are still finite, and pinned:
`cypher.DefaultGlobalMaxResultBytes` is **4 GiB** (`cypher/api.go`) and
`bolt/server.DefaultMaxInboundDecodeBytes` is **1 GiB**, each with a test asserting it is
finite and positive. That closed a denial-of-service reachable **pre-authentication**, and it
has not regressed.

### 3.2 FIXED — rmp #2421: a fixed ceiling is not a ceiling inside a smaller container

Those ceilings are **constants**, applied whenever the process has no Go soft memory limit to
derive from — which is the default state of every Go process. Three facts compose into a
defect:

- The constants are 4 GiB and 1 GiB.
- **`GOMEMLIMIT` is set nowhere in the module** — a grep over the tree returns only comments.
- **There is no cgroup or host-memory awareness anywhere** — no read of `memory.max`,
  `memory.limit_in_bytes`, or installed RAM.

Inside a container capped below those numbers, the ceiling is larger than the whole container,
so it cannot bind before the kernel does. The failure mode is the **OOM killer**, not a typed
error — the opposite of the mandate that the library "degrades predictably rather than failing
catastrophically". It is not theoretical: the 2026-08-11 memory audit OOM-killed `ggserver`
loading 4M relationships in an 8 GB container (`Killed process (ggserver)
anon-rss:8370788kB`), on a graph Memgraph held in 658 MB, and the identical fixture with
`GOMEMLIMIT=6GiB` completed at 4520 MB. **The cap was the missing input, not the workload.**

**Fix.** New `internal/memlimit` reads the smallest bound it can observe — Go soft memory
limit, then cgroup v2 `memory.max`, then cgroup v1 `memory.limit_in_bytes` — and both
resolvers consult it before their constant, keeping their existing fractions (½ for results,
⅛ for inbound decode). Three deliberate non-decisions:

- **It never sets `GOMEMLIMIT`.** That is process-global state belonging to the embedder;
  reading a bound to lower our own ceilings is a library's business, installing one is not.
- **It does not fall back to installed RAM.** A bound the package cannot vouch for is worse
  than none — reporting "not found" selects the caller's documented finite constant.
- **It may only ever lower a ceiling**, never raise one above what an unconstrained host
  already gets.

Shape taken from Memgraph, whose global limit defaults to 90–100% of physical memory read
from the host (`src/flags/memory_limit.cpp:20-38`, v3.9.0); the magnitudes remain GoGraph's.

**Evidence.** Behaviour is byte-for-byte unchanged when a soft limit is set and when no bound
is discoverable — which is every developer machine, and precisely why the derivation is tested
against **injected** limits rather than whatever the host reports. Demonstrated by injection:
with the derivation removed, a 512 MiB container resolves to a 4 GiB ceiling and the test
fails. The cgroup read is asserted once-per-process across 64 concurrent callers. Commit
`d369f4f1`.

**One acceptance criterion is NOT met and is not claimed:** the container reproduction of the
4M-relationship load was not run, because no container runtime was exercised in this cycle.

---

### 3.3 FIXED — rmp #2422: the leak gate did not cover the concurrency substrate

The Reliability and Concurrency Mandates require that every goroutine the library spawns is
"verified via `go.uber.org/goleak` in test teardown for **every** package that spawns
goroutines". Audited at HEAD:

- `graph/mvcc/gate.go:341` spawns a helper on the context-aware acquisition path.
- `graph/mvcc` had **no goleak verification at all** — no import in any `_test.go`, no
  `TestMain`. Other packages carry it per-test (`bench/soak`, `bench/ldbc`, `bolt/server`,
  `cypher`, `cypher/exec`).

So the one package whose entire subject is concurrency was the one the leak gate missed.

**This is not a report of a leak, and is not filed as one.** The helper always terminates: it
blocks in `lock()`, then either hands the lock off and returns or unlocks and returns, so it
cannot outlive the holder's tenure — and the godoc already documents the transient's cost with
measurements. What was wrong is that **nothing verified it**, so a future change that did leak
would have been caught by nothing. A documented exemption would have been acceptable; silence
was not.

The gate is at `TestMain` rather than per test, deliberately: the helper's lifetime is bounded
by *another party's* lock tenure, so a per-test check after a deliberately abandoned
acquisition can observe a helper still parked and report a false positive. After every test in
the package has finished, no holder remains, so a parked helper really is a leak.

**Validated by injection, not by one green run.** A goroutine blocked in `select{}` makes the
package fail with the leaked frame named; removing it restores green. Stability: 5 runs of
`-count=4` under `-race` at load average 25, no flake — a leak assertion is a timing assertion,
and on this project such assertions are load questions before they are code questions.

**The audit was generalised so the next cycle need not re-derive it.** Every module package
that spawns a goroutine in non-test code now carries goleak: `bolt/server`, `cypher`,
`cypher/exec`, `graph/adjlist`, `graph/generation`, `graph/index/count`, `graph/lpg`,
`graph/mvcc`, `internal/crashinject`, `internal/goldens`, `internal/isolationtest`,
`internal/shapegen`, `internal/sim`, `search`, `search/centrality`, `store/bulk`,
`store/checkpoint`, `store/txn`, `store/wal`. Four packages appear to lack it —
`cypher/parser`, `internal/metrics`, `internal/testlayers`, `search/community` — and all four
are **false positives** whose only matches are inside comments (`go generate`, `can go down`,
`go test`, `Reads go through`). `examples/` and `cmd/` are excluded: examples are instruments,
not part of the module.

## 4. Efficient and fast — what was measured

These were taken **after** the reproduction environment had drained (load average 6–10 on 10
cores), because a figure taken under the saturation #2420 requires would be worthless. They
characterise the module; they are not what refuses the certification.

### 4.1 RAM — marginal cost per element, one arm per process

The probe harness refuses to run when the baseline live heap is above its floor, which is
what stops an arm from measuring the previous arm's residue. Each figure below is therefore a
**separate process**; running them together produced two refusals, correctly.

| shape | B/element |
|---|---:|
| node + 1 label, string key | 210.66 |
| node + 1 label + 1 property | 327.42 |
| edge, multigraph, untyped | 66.20 |
| edge, simple graph, untyped | 66.19 |
| edge, multigraph, typed | 70.20 |
| edge, typed + one int64 property | 104.20 |
| mapper entry, synthetic string key | 93.11 |
| mapper entry, `uint64` key | 48.56 |

Two things follow directly.

- **The synthetic string node key costs 44.55 B/node** (93.11 − 48.56) for an identity Cypher
  never exposes. That reproduces the 2026-08-11 audit's 44.5 B/node to within 0.05 B, at
  HEAD, and is rmp **#2404** — still open.
- **The storage layer is not where the weight is.** A typed edge with a property costs
  104.20 B at the Go API. The same shape measured through Cypher in the 2026-08-11 audit cost
  2 610.91 B/edge before the sprint-339 remediation and 976.36 after. The amplification lives
  above the storage layer, exactly as that audit concluded.

### 4.2 CPU — where it actually goes

`examples/35_mvcc_mixed_workload`, the MVCC mixed-workload example, driven at **60 000
nodes / 8 readers / 6 s phase windows** — deliberately far above its 3 000-node default,
because the previous sweep established that these examples produce useless profiles at
default scale. `-profile-dir` writes `cpu.pprof` and `heap.pprof` through the shared
`exprof` harness. Duration 176.23 s, 330.03 s of samples (≈1.87 cores average).

**Flat CPU, the hot leaves:**

| flat % | symbol |
|---:|---|
| 9.15% | `runtime.pthread_cond_signal` |
| 6.69% | `runtime.usleep` |
| 3.91% | `runtime.mallocgc` (**16.15% cumulative**) |
| 3.51% | `runtime.pthread_cond_wait` |
| 2.97% | `runtime.kevent` |
| 2.97% | `runtime.mallocgcTiny` |
| 2.61% | `runtime.mapaccess1_fast64` |
| 2.51% | `exec.(*Project).Next` (**65.64% cumulative** — top of the read pipeline) |
| 2.17% | `aeshashbody` (map hashing) |
| 2.08% | `runtime.pthread_kill` |

Two things stand out, and both are stated as **measurements, not attributions** — a profile
locates cost, it does not explain it:

1. **Roughly a quarter of all CPU is spent in thread signalling and parking**, not in graph
   work: `pthread_cond_signal` + `usleep` + `pthread_cond_wait` + `kevent` + `pthread_kill`
   sum to **24.4%**. That is the signature of frequent goroutine handoff. The workload runs
   `ParallelScanProject` (33.21% cumulative) with morsel dispatch plus 8 reader goroutines,
   so a handoff-heavy shape is expected — but a quarter of CPU is a large number to leave
   uninvestigated, and no mechanism is claimed here.
2. **Allocation is 16.15% cumulative.** Adding the map machinery — `mapaccess1_fast64`,
   `aeshashbody`, `getWithoutKeySmallFastStr` — brings hashed-map work to ≈6.5% on top.

The engine's own hot path is expression evaluation: `newRowPredicate.func1` 29.56%,
`evalRowPooledWalk` 29.45%, `evalRow` 21.21%, `expr.EvalWith` 21.07%, `evalExpr` 14.87%,
`evalBinaryOp` 14.73% (all cumulative).

**Live heap, and it corroborates the 2026-08-11 memory audit independently:**

| inuse % | symbol |
|---:|---|
| 25.82% | `lpg.setNodeLabelInfo` |
| 23.48% | `lpg.setNodePropertyInfo` |
| 16.35% | `runtime.mallocgc` |
| 12.39% | `graph.Mapper.internSlowHook` |

The label store, the property store and the identity mapper are **61.7% of live heap between
them** — the same three structures that audit named, arrived at from a different direction.

### 4.3 Concurrency — the levels the module publishes

`BenchmarkBoltPooledReadWork`, the arm whose own godoc says it is the engine-throughput
figure to quote because neither the connection nor a trivial query dominates it. Pooled
connections, so this measures the engine and not the handshake — the previous certification
established that 60% of the churn sweep's cost was the handshake. Three runs per level;
`ns/op` under `RunParallel` is inverse **aggregate** throughput.

| goroutines | median ns/op | aggregate ops/s | vs. 1 | B/op | allocs/op |
|---:|---:|---:|---:|---:|---:|
| 1 | 128 138 | 7 804 | 1.00× | 97 890 | 1 923 |
| 8 | 38 097 | **26 249** | **3.36×** | 98 038 | 1 923 |
| 64 | 50 199 | 19 921 | 2.55× | 102 122 | 1 927 |
| 256 | 65 811 | 15 195 | 1.95× | 102 390 | 1 928 |
| 1024 | 173 711 | 5 757 | 0.74× | 103 396 | 1 933 |

**The result that matters most for the concurrency mandate is the last two columns.**
Allocations per operation move from 1 923 to 1 933 — **0.5% across a 1024-fold increase in
concurrency**. A per-operation metric that tracked the peer count would be a defect (or a
fixture bug); this one is flat, so the engine does no per-peer work per operation.

**Throughput peaks at 8 and declines monotonically thereafter**, and at 1024 it is below the
single-client figure. Three things should be said honestly about that:

- It is **degradation, not collapse**: no error, no crash, no deadlock, no timeout, across
  every level. That is what the "graceful degradation" clause asks for.
- The harness sets `GOMAXPROCS` to the concurrency level, so 1024 means 1024 on a **10-core**
  host — deliberate, heavy oversubscription. The decline is substantially scheduler
  contention rather than an engine limit, and this host cannot separate the two.
- It **corroborates the previous certification independently**: that cycle recorded an engine
  peak of 24 959 reads/s at 8 clients and 6 798 at 1024 pooled; this one measures 26 249 and
  5 757. Two sweeps, weeks apart, agreeing on the shape.

**What this does not establish:** the mandate asks for latency *and* throughput at these
levels, and this arm reports throughput only. No p99 was captured, so the latency half of the
published-levels requirement remains unmeasured.

---

## 5. What this certification did NOT establish

Stated plainly, because an unmeasured axis reported as sound is how a certification becomes
misleading.

1. **The 24.4% in thread signalling is located, not explained.** §4.2 measures it; nothing
   here attributes it to a mechanism, and a profile cannot do that on its own. It is the
   single largest unexplained item this certification produced and it is the obvious next
   piece of CPU work.
2. **Latency was not measured at the published levels.** §4.3 reports throughput only; no
   p50/p99 was captured, so half of the "latency and throughput at 1, 8, 64, 256, 1024"
   requirement is unmet.
3. **The write-scaling half of the concurrency sweep was not run**
   (`store/txn/write_scaling_bench_test.go`). §4.3 is a read workload.
4. **The whole-tree soak layer was not run**, and remains unmeasured, as it was at the
   previous certification.
5. **No container was actually run.** §3.2 makes the ceilings derive from a cgroup limit, and
   that derivation is tested against injected limits — but **no container reproduction was
   performed**, so the claim that a memory-capped deployment now degrades instead of being
   OOM-killed is *reasoned and unit-tested, not demonstrated end to end*. The cgroup read
   paths themselves have never executed on this host, which is `darwin/arm64` and has no
   cgroup filesystem at all. This is #2421's one unmet acceptance criterion and it is
   recorded as unmet rather than waived.
6. **The remaining memory findings are untouched.** rmp #2404 (the synthetic string node key,
   re-measured here at 44.55 B/node), #2405 (the per-node record — an architecture decision
   that is the user's to take) and #2407 (peak versus steady during shard growth) are open
   and were not addressed.
7. **Single host, single architecture.** Apple M4, 10 cores, `darwin/arm64`. No Linux, no
   NUMA, no multi-socket, no container.

---

## 6. Findings

| id | severity | title | status |
|---|---|---|---|
| #2420 | 9 / 9 | A pinned snapshot observes a partial transaction; `make ci` RED | **OPEN — blocker** |
| #2250 | 9 / 8 | Reverse type filter admitted the pair, not the instance | **FIXED** `588f75b8` |
| #2421 | 8 / 7 | Fixed engine-wide ceilings exceed a container's cap; OOM instead of a typed error | **FIXED** `d369f4f1` |
| #2366 | 8 / 7 | UNIQUE value permanently unusable after a peer's committed release | **OPEN — designed, not implemented** |
| #2422 | 5 / 4 | `graph/mvcc` spawns goroutines and had no goleak verification | **FIXED** — gate added |
| #2413 | 8 / 6 | By-handle latch oracle (stale: fixed at `6fc5a693`) | **CLOSED — verified 50/50** |

`#2420` was opened by this certification. `#2421` was opened by it too — it had been recorded
as an envelope note by the previous certification and as an aside on #2407, but never filed as
an actionable defect. `#2413` was already fixed and had been left open in the backlog, which
overstated the module's open-defect count; it is verified and closed rather than assumed.

## 7. Reproduce

```bash
# The gate. Read the status from INSIDE the log, never from a wrapper.
make ci > ci.log 2>&1; echo "MAKE_CI_EXIT=$?" >> ci.log; grep MAKE_CI_EXIT ci.log

# #2420 — the environment is part of the reproduction.
go test -race -count=1 ./... > peer.log 2>&1 &
go test -race -count=100 -run '^TestIsolation_ApplyAtomically_View_NoPartialReads$' ./graph/lpg/

# #2250 — deterministic, fixed.
go test -count=1 -run '^TestOrdering_ParallelEdgeTypes_ReverseDirection$' ./cypher/

# #2366 — deterministic. Add {name: "peer commits", peerFate: "commit"} to the
# table in TestUnique_RollbackLeavesNoPhantomReservation.
go test -count=1 -run '^TestUnique_RollbackLeavesNoPhantomReservation$' ./cypher/

# #2421 — the derivation, against injected limits (a dev host is unconstrained).
go test -count=1 -run 'Ceiling' ./cypher/ ./bolt/server/
go test -race -count=1 ./internal/memlimit/

# §4.1 RAM — ONE ARM PER PROCESS. The probes refuse to run on a dirty heap, so
# running them together produces refusals rather than wrong numbers.
for t in NodesWithLabel NodesWithLabelAndProp EdgesTyped EdgesTypedWithProp \
         MapperStringKey MapperUint64Key; do
  go test -tags=threeway -count=1 -v -run "^TestProbe_${t}\$" ./bench/memprobe/
done

# §4.3 concurrency at the published levels. Pooled, so it measures the engine
# and not the handshake.
go test -run='^$' -bench='^BenchmarkBoltPooledReadWork$' -benchmem -count=3 ./bench/soak/

# §4.2 CPU + heap. The scale flags matter: at its 3000-node default this example
# produces a useless profile.
go run ./examples/35_mvcc_mixed_workload \
  -nodes=60000 -readers=8 -phase-window=6s -profile-dir=/tmp/prof35
go tool pprof -top -nodecount=18 /tmp/prof35/cpu.pprof
go tool pprof -top -sample_index=inuse_space -nodecount=12 /tmp/prof35/heap.pprof
```

## 8. Commits

| commit | what |
|---|---|
| `588f75b8` | fix(cypher): a reverse type filter now admits the INSTANCE, not the pair (#2250) |
| `32e9c3ae` | test(lpg): make the partial-read oracle self-diagnosing when it fires (#2420) |
| `d369f4f1` | fix(cypher,bolt): derive the engine-wide ceilings from the container's cap (#2421) |
| `1a38378a` | fix(memlimit): annotate the cgroup read for gosec (#2421) |
| `26a9780a` | docs(certification): NOT CERTIFIED — an ACID Isolation tear refuses the first rung |
| `925f3713` | docs(certification): record all three gate runs, and what the green one does not prove |
| `2f279543` | test(mvcc): give the concurrency substrate the leak gate the mandate requires (#2422) |
| `3be02ecd` | test(lpg): capture the per-shard substrate at the partial-read violation (#2420) |
| `e77a247d` | docs(certification): the reproduction recipe was wrong, and correcting it worked |
| `18a6e684` | docs(certification): profile the CPU, and correct a rate from one sample for the third time |
| `2e6d6b50` | docs(certification): the substrate capture fired — fall-through refuted, one stamp does not fit |
| `0f0f1256` | fix(lpg): the capture packed a flag into a field that spans 2^63 — retract the anomaly (#2420) |
| `22df24d4` | docs(certification): two clean captures localise the tear to snapshot BIRTH |
| `ac035676` | docs(certification): record the exit gate and the full reproduction tally |
| `03533ee0` | test(lpg): walk the chain at the violation, and correct two instruments of my own (#2420) |
