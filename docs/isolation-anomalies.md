# Classifying isolation anomalies — the Adya phenomena

`internal/anomaly` turns an observed transaction history into a **named,
classified violation**, so a sighting points at a mechanism instead of starting a
search.

Task: rmp #2341, sprint 335. Companion to
[docs/isolation-harness.md](isolation-harness.md), which enumerates
interleavings; this classifies what one of them produced.

---

## Why

When the DST and example batteries observe an isolation failure they report a
domain symptom and nothing else:

```
ISOLATION VIOLATION: readers observed a torn total 1 time(s);
first torn value 46625044410, expected 46625986168
```

That symptom is compatible with several distinct defects with different root
causes. It is a direct cause of what rmp #2333 cost and of why rmp #2336 is
still open: **the evidence names what looked wrong, not what was violated.**

A classified report says the other thing:

```
VIOLATION G-single (cycle with exactly one anti-dependency):
  74 -wr(a2)-> 91  91 -rw(a3)-> 74
  one anti-dependency closes a cycle of 2 transactions;
  this is the shape of a LOST UPDATE and snapshot isolation must prevent it
```

---

## Sources

The definitions are cited, not remembered.

| Source | What was taken |
|---|---|
| Adya, Liskov & O'Neil, *Generalized Isolation Level Definitions*, ICDE 2000 | the DSG over read-, write- and anti-dependencies; G0, G1a, G1b, G1c, G2, G2-item; PL-SI |
| Berenson et al., *A Critique of ANSI SQL Isolation Levels*, SIGMOD 1995 | P4 lost update, A5B write skew, and that SI permits the latter |
| Kingsbury & Alvaro, *Elle*, VLDB 2020, and `jepsen-io/elle` at `src/elle/consistency_model.clj` (read 2026-08-08) | the machine-checked anomaly lattice the level boundaries were **verified against** |
| Cerone, Bernardi & Gotsman, CONCUR 2015 | the characterisation of generalized SI that makes **G-nonadjacent** the right boundary |

**Reading Elle's source changed the answer.** From memory, the boundary would
have been written as "SI forbids G-single". Elle forbids **G-nonadjacent**, and
says why in a comment: *"Chatting with Alexey Gotsman about this confirms my
suspicion: generalized SI forbids any history where all rw edges are
nonadjacent, not just G-single."* G-single is the degenerate case of it.

---

## The boundary, which is the substance

GoGraph targets **snapshot isolation**. A checker that flagged legal write skew
would be worse than none: every clean run would carry a false violation, and the
real ones would stop being read.

| Level | Forbids |
|---|---|
| read uncommitted (PL-1) | G0 |
| read committed (PL-2) | + G1a, G1b, G1c |
| **snapshot isolation (PL-SI)** | **+ G-nonadjacent** (⊇ G-single) |
| serializable (PL-3) | + G2, G2-item |

The two classic anomalies fall on opposite sides of it, and that is the whole
check:

- **Lost update (P4)** — `T1 -rw-> T2 -ww-> T1`. One anti-dependency ⇒ G-single
  ⊂ G-nonadjacent ⇒ **FORBIDDEN**. SI prevents it by first-committer-wins on the
  shared key.
- **Write skew (A5B)** — `T1 -rw-> T2 -rw-> T1`. Two anti-dependencies, and they
  are **adjacent** — each transaction is both entered and left by one — so the
  cycle is G2-item but not G-nonadjacent ⇒ **PERMITTED**. The two transactions
  write different keys, so nothing conflicts.

Both directions are asserted, on the same checker at the same level, in
`TestSnapshotIsolationBoundary`. A permitted anomaly is **reported as permitted**
rather than dropped, so a checker that found nothing can never be mistaken for
one that found something legal and swallowed it.

---

## Validation

**Per phenomenon, on constructed histories** (`check_test.go`): one history built
to exhibit each of G0, G1a, G1b, G1c, G-single, G2-item and the
incomplete-history report, each asserting the exact name rather than "some
violation". Plus a serial history that must be clean at every level — the
positive control without which a checker that always fired would pass every
detection case.

**On a healthy real engine** (`engine_test.go`): a 4-account bank under 4 writers
and 4 readers, atomic reads. **155 transactions, 985 dependency edges, verdict
CLEAN**, and the domain invariant saw zero torn totals.

**On a defective real engine** — the mandatory negative control: the reader
resolves each account at its own instant while the history records the one
logical read it was meant to be, which is exactly the shape rmp #2336 describes.
Measured: **71 G-single and 253 G-nonadjacent violations across 284
transactions**, every one of them in the family snapshot isolation actually
forbids, and **zero** false G2-item reports. The domain invariant saw 57 torn
totals over the same run — the same defect, counted instead of named.

---

## Does observing change what is observed?

The module's own record says a probe can hide the defect it was added to find: a
single instrumentation print turned 8/8 FAIL into 8/8 PASS. So the recorder is
measured against an unrecorded control **on the defective build**, where there is
a real failure rate to move. Comparing 0 with 0 would establish nothing.

Ratio of anomaly rate with recorder to without, interleaved arms, 30 repetitions:

| recorder design | six runs | mean |
|---|---|---:|
| one mutex-guarded slice | 0.877 0.844 0.817 0.814 0.937 0.898 | **0.865** |
| one shard per goroutine | 0.993 1.021 0.854 0.841 0.776 0.993 | 0.913 |
| sharded **and** pre-sized | 1.159 1.163 0.937 0.821 0.881 1.002 | **0.994** |

The first row is a **real 13% suppression**, not noise — all six runs below 1.0.
A lock at the end of every transaction serialises the writers just enough to
reduce their overlap, and reduced overlap is reduced tearing. Sharding removed
the shared word; pre-sizing removed the reallocation traffic left behind; the
third row straddles 1.0 symmetrically and there is no effect left to find.

**It took an interleaved design to see any of this.** Run as all-recorded-then-
all-unrecorded, the same shared-lock recorder measured 0.95, 0.74, 1.27, 0.73,
0.86 — a spread wide enough to look like noise, because the ordering drift was
larger than the effect.

---

## The rmp #2336 attempt, and what it does not claim

On 2026-08-06 a `make ci` reported one torn total in `examples/27_concurrent_txn`
— a reader's sum 941 758 low. #2336 had already established that the delta is not
any transfer amount, so "one debit observed without its credit" is false, and the
leading hypothesis is that **one account resolved at a different instant from the
rest**.

**That hypothesis now has a name.** A reader in that position observes a
transaction's write to one key and misses its write to another: a
read-dependency into the reader, an anti-dependency out of it, closing a cycle
with exactly one anti-dependency — **G-single**, which snapshot isolation
forbids. The defective-build test above demonstrates the classifier producing
exactly that name from a build with exactly that defect.

**It did not reproduce.** The #2336-shaped run with the healthy engine and the
checker attached was clean, which adds to the ~245M observations already on the
ticket and resolves nothing on its own. A non-reproduction is not a resolution.
What changed is that the next sighting is classifiable rather than merely
countable.

---

## Bounds are never silent

Simple-cycle enumeration is exponential in the worst case, so it is bounded. When
the bound is hit, `Report.Truncated` is set and the report renders
**`VERDICT: INCONCLUSIVE`** rather than clean. A bound that quietly turned a large
history into a clean verdict would be precisely the failure mode this package
exists to remove.

An `Unwritten` finding — a read of a version no transaction in the history wrote
— is reported for the same reason: it means the **history is incomplete**, and a
clean verdict computed from missing data is the worst output this package could
give.
