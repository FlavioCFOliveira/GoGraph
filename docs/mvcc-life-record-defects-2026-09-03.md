# Three engine-side ACID defects in the MVCC life-record store

**Date:** 2026-09-03 · **Task:** rmp #2723 · **Diagnosed at:** `bec5b4a9`
**Status: ALL THREE FIXED**, each in its own task with its own regression gate —
family 1 as #2724 (`cd91a8bc`), family 2 as #2725 (`13467da4`), family 3 as #2726.
The diagnosis below is preserved as written on 2026-09-03; **two of its three
mechanisms were subsequently REFUTED by the fixes**, and each is corrected in
place under a "CORRECTED" heading rather than rewritten, so the reasoning that
led there stays legible.

A 1000-seed sweep of the DST MVCC-sessions mode surfaced three violation
families. All three are **engine defects**, not harness modelling errors. This
matters because the immediately preceding task, #2717, presented an identical
symptom — "engine committed but oracle refused" — and turned out to be a
*harness* bug. Each verdict below therefore rests on a one-factor reproduction
against the real store-backed engine, not on the oracle's classification.

No fix is proposed here. Two of the three sit on the seam that already produced
#2443, #2444, #2445, #2686 and #2687, **each fix opening a mirror of the last**,
and a rushed change there is exactly what Mandate 2 warns against.

---

## Family 1 — a rolled-back DELETE destroys a node's committed birth

**Verdict: ENGINE. Mandate 2 — Isolation and Consistency.**

**Invariant violated:** *a node whose birth committed before a reader's snapshot,
and whose only death is uncommitted or newer than that snapshot, must be visible
to that reader.*

### The decisive experiment

Two sessions, identical but for **when the doomed delete rolls back relative to
the reader's BEGIN**:

| arm | reader sees | want | |
|---|---|---|---|
| delete aborts **before** the reader's BEGIN | 2 | 2 | clean |
| delete **pending at** the reader's BEGIN | **1** | 2 | **reproduced** |

The reader is correct right through the rollback, and loses the node the instant
**any later, unrelated delete is merely PENDING**. That is a dirty read and a
snapshot move. Byte-identical over 5 repeats.

### The mechanism, from a white-box life-store dump

Reader pinned at `startTS=1`:

```
committed CREATE      born{ts=1 seq=1}  died{-}                 alive  OK
tx1 delete PENDING    born{ts=1 seq=1}  died{in-flight seq=2}   alive  OK
tx1 ROLLED BACK       born{ts=2 seq=3}  died{ts=2 seq=2}        alive  OK
tx2 delete PENDING    born{ts=2 seq=3}  died{in-flight seq=4}   DEAD   BUG
```

Three facts compose into the defect:

1. **The life store is one record deep per direction** — `mvcc_life.go:230`:
   *"writing this one DISPLACES whatever was there."*
2. **A `Rollback` publishes a real instant**, not `mvcc.AbortedTS`. Its undo
   replay's `Revive` → `noteNodeRevived` therefore **overwrites the committed
   birth (ts=1) with ts=2**. The committed birth is destroyed.
3. **`aliveBefore(born, died) = died.seq < born.seq`** (`mvcc_life.go:84`) masks
   the loss *only while the died half is still the rollback's own*. A later
   unrelated delete replaces the died half, the pair flips to born-then-died,
   and `NodeExistsAsOf` returns **false** for every reader older than the
   rollback.

Confirmed as an existence failure, not a label-index one: all six read paths
agreed the node was gone, and an independently pinned snapshot showed
`NodeExistsAsOf` flipping true → false at exactly the second delete.

### Relation to #2445 — an uncovered case, not a regression

#2445 replaced a chain-level `primordial` flag with `aliveBefore`, and its
comment names this very scenario as what it fixed. **The repair is sound only
while both records belong to one transaction.** Nothing stops a later,
unrelated delete from replacing the died half. Same family, uncovered case.
**The missing artefact is a regression test for the split-pair case.**

Sites: `graph/lpg/mvcc_life.go` — `noteNodeLife`, `noteNodeRevived`,
`aliveBefore`, `NodeExistsAsOf`; driven from `graph/lpg/lpg.go:1950` `revive`.

**Why the obvious fix is not obvious:** the correct shape is *an undo revive must
withdraw its own death record rather than write a new birth*. But `revive` is
constrained by #2687's ordering — the birth record **is** the suspect
registration that raises the churn gate *before* the tombstone flip. Releasing
the death's hold instead would reopen #2687's mirror: a live node absent from
every label bitmap.

---

## Family 2 — two rolled-back transactions leave a permanent edge

**Verdict: ENGINE. Mandate 2 — Atomicity and Consistency.**

**Invariant violated:** *no arc may exist that no committed transaction created.*

### The decisive experiment

Only the **order in which the two transactions end** varies:

| arm | arcs | |
|---|---|---|
| creator rolls back **first**, then the doomed deleter | `["a"->"b"]` | **reproduced** |
| deleter rolls back first | `[]` | clean |
| deleter left open | `[]` | clean |
| no deleter | `[]` | clean |

The republishing append must be on the **same source**: 1 of 13 second-source
names reproduced — only `a` itself.

### The mechanism

1. `W` appends `(a)-[:KNOWS]->(b)`, uncommitted.
2. `D` runs `DETACH DELETE b`. Its **edge-removal phase runs before**
   `removeNodeInfo`'s #2444 node-death claim, so it records a versioned removal
   of `W`'s pending arc; `removeNodeInfo` is then refused and `D` is doomed.
   *Isolation itself holds* — `W` still sees its own arc, and `D` does conflict
   at COMMIT.
3. `W` rolls back; the arc is withdrawn. The present view is still correct.
4. `D` rolls back — **its undo re-inserts the arc `W` already withdrew.** The arc
   now belongs to no transaction.
5. It stays masked by version filtering until the **next append on `a`**
   republishes that adjacency entry, after which it is permanently visible.

In seed 447 the leak is created at tick 215 and becomes visible at **tick 349**,
an append on the same source. Bisected: clean at 348, unclean at 349.

Site **not pinned to a line** — the ordering and trigger are established, the
offending statement is not.

### CORRECTED by #2725 (`13467da4`) — the mechanism above is half wrong

Step 2 says the deleter "records a versioned removal of `W`'s pending arc;
`removeNodeInfo` is then refused". Measured white-box, **the removal is refused
FIRST and mutates nothing** — the writer's arc is physically untouched through
the deleter's whole statement. The leak is entirely an **undo entry journalled
for a removal that never happened**: `cypher/api.go:19197-19210` pre-probes with
`HasEdge`, calls a **void** `RemoveEdge` whose refusal is therefore invisible,
and journals the inverse off the *probe* rather than the *outcome*.

`removeEdgeInfo` now returns its refusal and the adapters gate on it. **This is
#2694 one edge direction over** — that task fixed the bulk out-edge path and left
the per-edge in-edge loop.

---

## Family 3 — an unnamed node the workload never creates

**Verdict: ENGINE (class). Site not localised.**

Crash arm only. The node is `tombstoned=false existsPresent=true`, carries no
`:Person` label and no `name`, and sometimes has an arc pointing at it. Counters
at the point of detection show a **conflict-conceded** rollback rather than a
voluntary one, so that is the candidate producer. Three simple reproductions —
plain rolled-back CREATE, rolled-back CREATE after crash+recovery, and after a
crash that killed an open creating transaction — were all **clean**.

**Unproven hypothesis:** `reclaimAbortedLife`
(`graph/lpg/mvcc_abort_sides.go:186`) decides born/died pairs by
`at() == mvcc.AbortedTS`. Because the store is one-deep, a live record can
displace one half of an aborted pair, and the survivor is then handled by the
wrong single-direction branch — the `died` branch calls `reviveAborted`, which
clears the tombstone with **no birth instant**. That is the same information-loss
root as family 1, which is why one cause behind two symptoms is plausible. Not
established.

### CORRECTED by #2726 — the hypothesis is REFUTED, and so is the shared root

A white-box trace of seed 790 shows **`reclaimAbortedLife` never touches the
leaked node**; `reviveAborted` is never called for it, and the run's only two
reclaim events decide correctly on other ids. **#2724's `wasAlive` neither fixes
nor could have fixed this — different root cause, and no record-layout change is
needed for it.**

The real producer is **#2725's mechanism one entity kind over**:
`cypher/api.go:19373` and `:20322` pre-probe with `IsTombstoned`, call a **void**
`WriteView.RemoveNode` whose refusal is invisible, and journal the revive off the
probe. `writeview.go` was the only removal primitive never converted to report
its outcome. The node returns **bare** because the peer's committed statement had
already removed its labels and properties, and `revive` restores only what the
label bag still holds.

**Why the obvious reproduction fails:** two transactions racing to delete the
same node passes 8/8, because whichever lands first tombstones it and the
loser's own probe then reads `wasLive == false` and suppresses the inverse. The
defect needs the node **still live** at the refusal, which only an
**already-doomed** transaction produces — exactly what `TxRolledBack:0,
TxConflicted:4, TxDoomed:2` was saying.

**A second symptom the matrix caught:** the bogus revive *consumes* the
tombstone, so a legitimate inverse running afterwards finds nothing to revive and
silently no-ops — losing the node entirely.

Seed 790 also proved **more** reliable than recorded here: 12/12 unclean, not
intermittent. The detection tick and the leaked node's id both move; the finding
does not.

---

## The gate's blind spot is SEED BREADTH, not tick depth

The premise that opened this task — *"the gates run Ticks=600, which is why none
has been seen"* — is **wrong**, measured two ways.

1. At Ticks=600, **3 of the 4 crash-arm seeds still fail** (699 at tick 53, 932
   at 279, 790 at ~140-170). Only seed 486 needs depth.
2. **Seeds 1, 7 and 42 are clean at 600, 1200 and 2400 on both arms.**
   Quadrupling the depth on the gate's own seeds finds *nothing*.

Depth buys 1→4 and 3→4 across a 1000-seed space; it buys **zero** on the three
seeds the gates run. The gates miss these because they sample three seeds.
Note `mvccCrashConfig` **already runs Ticks=1200**; only `mvccTestConfig` is 600.

### Rates at `bec5b4a9`, 1000 seeds per arm, `-count=1`

| arm | Ticks | unclean | seeds |
|---|---|---|---|
| no-crash | 1200 | 4 | 447, 760 (edge); 815, 875 (snapshot) |
| no-crash | 600 | 1 | 447 |
| crash | 1200 | 4 | 486, 699, 932 (snapshot); 790 (node) |
| crash | 600 | 3 | 699, 932, 790 |

### Cost of raising the tick count (measured, `-race`, per seed)

| Ticks | no-crash | crash |
|---|---|---|
| 600 | 0.99 s | 0.80 s |
| 1200 | 3.48 s | 3.22 s |
| 2400 | 12.98 s | 16.7 s |

**Superlinear** — 600→1200 is 3.5x, not 2x, because every parity check
full-scans a graph that keeps growing.

**Recommendation:** do **not** raise `mvccTestConfig.Ticks` — it costs ~17 s and
detects nothing. Once fixed, add the eight named seeds as short-layer regression
seeds (~27 s under `-race`), exactly as #2717 did with seeds 22/500/572, and put
the 1000-seed sweep in the **soak** layer. Breadth is a soak property; the short
layer should carry named reproducers.

---

## A DST fidelity defect found on the way

**Seed 790 is not deterministic** — 12 processes gave the same finding at tick
140 (x5), 150 (x4) and 170 (x3). Families 1 and 2 are byte-identical over 5
repeats; only 790 moves.

The cause is a real breach of the mode's own contract. `RunMVCCSessions`
documents that *"the whole run — schedule, statements, outcomes — is a pure
function of cfg.Seed"* and drives one goroutine, but
`graph/lpg/mvcc_vacuum.go` runs `vacuumLoop` on a **background goroutine** with
wall-clock timer backoff. A leaked bare node is invisible until its life record
is reclaimed, so *when* it becomes countable moves with wall-clock.

**The defect is stable; only the detection tick is not.** Consequence for
whoever writes the gates: **a seed regression test on any reclamation-sensitive
mode must not assert an exact tick.**

Minor, related: `mvccHarness.maybeCrash` never closes the pre-crash `SimStore`,
so each injected crash leaves a dead `Graph` whose vacuum goroutine sweeps until
it idles out. `goleak` still passes because it does exit.

---

## Reproducers

Archived verbatim rather than committed, because they fail by design and turning
the gate red is a scope decision:

- `scratchpad/repro_sim_zz2723_probe_test.go.txt` → restore into `internal/sim/`
- `scratchpad/repro_lpg_zz2723_life_probe_test.go.txt` → restore into `graph/lpg/`

## What is unverified

No fix implemented, so no after-fix rate exists. Family 3's site and its
`reclaimAbortedLife` hypothesis are unproven. Family 2 is not pinned to a
file:line. Whether families 1 and 3 share a root cause is unestablished. The TCK
was not run — production code is unchanged, so it cannot have moved.
