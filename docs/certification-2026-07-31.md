# Production-readiness certification — GoGraph module

**Date:** 2026-07-31 · **Entry head:** `d820ce77` · **Exit head:** see §8 · Apple M4 (10 cores), `darwin/arm64`, go1.26.5

Scope: the **`GoGraph` module**. The 34 programs under `examples/` are **instruments**, not
subjects — they exercise the module so its correctness, performance and efficiency can be
observed.

This cycle audits the state left by the 2026-07-30 certification, which closed with three
items open and two structural questions deferred to the maintainer.

---

## Verdict: **NOT CERTIFIED for unrestricted production.**

**Certified for production under the envelope in §6.** Two correctness defects were found
and fixed, three open items were closed, and one **CRITICAL availability defect** was
measured, reproduced, and left open by decision — its fix is a multi-sprint programme the
user authorised today.

The blocker is **#2274**: a single long read running concurrently with a single write
starves every short reader for the **entire duration of the long read**. Measured at
**−51.5×** and **−61.2×** throughput with a worst short-read latency of **1m36s** on a query
that normally takes 4.5 µs. Each ingredient alone costs nothing. Because the amplification is
bounded only by the longest concurrent read, a ten-minute analytical query is a ten-minute
read outage, so the module cannot be certified for mixed OLTP-plus-analytics workloads until
it is fixed.

Everything else measured green: `make ci` exit 0, coverage 86.9 %, openCypher TCK 3897/3897
under `-race`, and the store durability suites green.

**What is worth carrying forward from this cycle is that two of the three things it settled
were settled against the written record.** A documented 983× performance cliff turned out to
be 24×; a documented −57× starvation turned out to be −51× to −61×, but only after a first
harness of my own measured −8 % and would have closed the finding as unreproducible; and a
recorded architectural conclusion that MVCC "requires a foundational storage rewrite" turned
out to describe a design neither reference implementation uses. **Numbers inherited from a
previous cycle are premises, not evidence.**

---

## 1. Defects found and fixed

### A projected pattern comprehension silently dropped its WHERE — #2272 (CRITICAL)

`RETURN [ (a)-[:K]->(x) WHERE x:Far | x.w ]` returned **one element per out-edge instead of
one per matching out-edge** — `[10, 20, 30]` where `[20]` was correct. No error, no warning.

A pattern comprehension takes two completely different routes depending on where it is
written. In a `WHERE` clause it survives to evaluation and the expression evaluator applies
its predicate correctly. In a `RETURN` or `WITH` item the IR translator hoists it into a
`RollUpApply` — and that hoist built its filter with `ir.NewSelection`, which carries only the
predicate's **string** form. The executor's documented fallback for a nil `PredicateExpr` is a
**pass-through stub**, so the predicate was never evaluated.

The projection built four lines below it in the same function passes its AST explicitly, with
a comment warning about this exact failure mode.

Fixed by threading the parsed AST through `NewSelectionExpr`. Same family as #2242, which
fixed the identical drop for `COUNT { … WHERE … }`.

**Found while verifying a different task's premise**, and only because that verification
compared the projection spelling against an independent oracle rather than against itself.

### The degree rewrite was unreachable from a projection — #2264

The same hoist consumed every projected comprehension, so `CountPatternComp` could never fire
there and `size([ … ])` built the whole list to measure it.

| spelling, out-degree 100 000 | before | after |
|---|---|---|
| `RETURN COUNT { (a)-[:K]->() }` | 2.159 ms | 2.135 ms |
| `RETURN size([ (a)-[:K]->(x) \| 1 ])` | **52.737 ms** | **782 µs** |
| allocations at out-degree 200 000 | 599 618 | **51** |

**The task's stated premise was 983× (2.813 ms vs 2.762 s). Re-measured at HEAD it is 24.4×** —
earlier rounds had already removed most of it. The defect was real; the figure was an order of
magnitude stale.

The structural half of the eligibility test moved to `ir.DegreeCountableShape` and is now
shared with `cypher.recogniseDegreePattern`, so the translator cannot claim a shape the
runtime refuses. A shape the runtime *does* refuse falls back to the general evaluator —
same answer, same cost as before the change. `CountPatternComp`'s godoc claimed the
projection case worked; it now describes what the code does.

## 2. Gate integrity

### `make ci` was not deterministic — #2268

Two independent causes, both fixed.

A strict per-point **wall-clock** inequality in `bench/cyclicjoin` passed in `test-short` and
failed at `cover-gate` **within one invocation** — 40.31 ms against 39.13 ms, a 3 % miss —
because the coverage step runs the whole repository in parallel. The per-point claim is now
asserted in **allocations**, a property of the plan that ran rather than of the machine it ran
on: measured 2.74× at degree 4 rising to 27.4× at degree 32, with a run-to-run spread below
0.01 %, against a 1.5× floor. Wall clock is kept only as a coarse 1.5× regression guard.
Engagement is asserted too, so a recogniser that silently declines cannot pass.

Separately, `cover_gate.sh` wrote fixed filenames in the repository root, so two concurrent
runs spliced one profile and the gate died parsing `gigithub.com/...`. Each run now writes
PID-suffixed temporaries, reads its own copy, and publishes by atomic rename.

### The checkpoint stalled writers walking an empty graph — #2271

The phase-1 exclusion window stalls every writer for as long as the capture takes, and on
200 000 nodes carrying **no labels and no properties** the two component writers spent
11.886 ms and 14.578 ms walking a graph that had nothing for them.

The registries are append-only and never reassign an id, so an **empty name table proves no
node and no edge carries a label** — a one-directional implication, deliberately not inverted:
a name outlives the last value that used it, so a non-empty table still walks.

| 200 000 nodes / 100 000 edges | before | after |
|---|---|---|
| `CaptureGraph`, attribute-free | 35.853 ms | **8.972 ms** (4.0×) |
| `WriteLabels`, attribute-free | 11.886 ms | 2 µs |
| `WriteProperties`, attribute-free | 14.578 ms | 2 µs |
| `CaptureGraph`, **with** attributes | 72.963 ms | 73.426 ms (unchanged) |

`labels.bin` and `properties.bin` have identical size and CRC before and after across four
fixtures — no attributes, node only, edge only, both — and those literals are now pinned.

**The gate for this needed an engagement counter, and finding that out matters more than the
optimisation.** The first regression test asserted allocations, reasoning that a walk over
200 000 nodes must allocate more than no walk. It does not — the collectors were already
allocation-optimised — so **the test passed against the unfixed code.** The skip changes no
byte and no allocation, and wall clock is inadmissible as a gate, so the production code now
carries explicit skip counters and the test asserts those. Proved to bite.

## 3. Certification evidence added

### An absolute oracle for the degree family — #2273

Four of the last cycles' correctness defects were degree answers wrong in one spelling and
right in another (#2241, #2242, #2258, and #2272 today). Each survived because the tests
compared spellings **to each other**, and both arms shared the broken code.

The new fixture's **generator records the truth as it builds** — every edge tallied by source
and by type, every removed node retracting the tallies of its incident edges — so the expected
answer is independent of every read path, including the Go API. Seven spellings × eight seeds
× ~36 live anchors, all green, over fixtures **asserted non-degenerate**: 37–55 self-loops,
15–22 parallel pairs, 22–44 slots dropped with a removed endpoint, and 18–24 anchors mixing
typed with untyped slots. Proved to bite by injecting a wrong spelling.

### A precondition turned into a gate

`ir.buildPropertySelection` contains a fallback structurally identical to the one behind
#2272 — a `NewSelection` with no parsed predicate, which the executor answers with a
pass-through stub. It is safe **only** because semantic analysis rejects a parameter as an
inline property map, so the sole shape reaching it is an empty map literal, where matching
everything is correct. That claim was unverified, which is exactly how #2272 survived. It is
now a test: relax semantic analysis and the gate fails, telling whoever does so that the
planner fallback needs a real predicate first.

## 4. Conformance and gates

| Instrument | Result |
|---|---|
| openCypher TCK, execution level | **3897 / 3897**, green under `-race` |
| `make ci` (tidy, fmt, vet, build, `-race` short layer, lint, coverage) | **exit 0** |
| Coverage | **86.9 %** aggregate, every package ≥ 75 % |
| `make check-soak-build` | exit 0 |
| `go test -race ./store/...` | green — checkpoint, snapshot, recovery, txn, wal |
| `TestCheckpoint_CaptureIsAtomic`, 20 consecutive runs under `-race` | exit 0 |

## 5. The blocker: reader starvation under a long read plus a write — #2274

Measured on a durable engine over a real WAL-backed `txn.Store`, 20 000 nodes, index on
`:P(w)`, against a 96-second analytical read:

| readers | baseline | + long read | + writer 10/s | **+ both** | collapse | worst short read |
|---|---|---|---|---|---|---|
| 1 | 221 781 op/s | 220 030 | 213 320 | **4 309** | **−51.5×** | **1m36.7s** |
| 8 | 449 719 op/s | 415 334 | 445 839 | **7 352** | **−61.2×** | **1m36.9s** |

**Each ingredient alone is free.** `Engine.Run` holds the read barrier across build and drain;
a write takes the same barrier exclusively; Go's `sync.RWMutex` prefers a waiting writer and
parks every reader arriving behind it. The throughput number is not the sharp end — **the
latency one is.** A 4.5 µs point query inherits the duration of an unrelated analytical query,
so the amplification is unbounded.

**A first harness of my own measured −8 % and would have closed this as unreproducible.** It
used an 11.7 ms long read; the stall lasts exactly as long as the long read. The gate now
calibrates its long read and **fails if it is under 5 seconds** rather than quietly measuring
nothing.

`bench/mtaudit/fairness_soak_test.go` is **KNOWN RED** and is expected to stay red until
#2274 phase P4. It is in the soak layer, which `make ci` does not run and which is not a
release gate — which is also exactly how #2256 stayed red for ~260 sprints, so it is recorded
here with its rmp id rather than left to be rediscovered.

### Prior art, and a corrected conclusion

Read from source at `master`, 2026-07-31:

| | global lock on an ordinary **read** | on an ordinary **write** | writer preference applies to |
|---|---|---|---|
| **Neo4j** | none — reads take no locks | none global; per-node/relationship (Forseti) | n/a |
| **Memgraph** | `main_lock_` **shared** | `main_lock_` **shared** (`WRITE`) | `UNIQUE` only — index creation |
| **GoGraph** | `visMu.RLock()` | `visMu.Lock()` — **exclusive** | **every write** |

Memgraph's lock *is* writer-preferring — `can_acquire<READ>()` requires
`unique_pending_count == 0`, the same rule Go's `RWMutex` applies. **It does not starve
readers because ordinary writes never take the exclusive mode**, not because the lock is
fairer. GoGraph applies UNIQUE-grade exclusion to its commonest operation.

The user chose per-object MVCC, specified in
[`docs/design-mvcc-delta-chains.md`](design-mvcc-delta-chains.md). That document **corrects a
recorded conclusion**: rmp #2051 measured a whole-graph per-shard copy-on-write prototype at
5.4× time and 43× memory and concluded MVCC needs the LPG core maps replaced with HAMT/CTrie
persistent structures. The measurement is sound; the conclusion is a property of the
whole-graph-snapshot model. Memgraph's `Delta` records **one modification**, is bounded at
**56 bytes**, and costs O(1) per write with **zero** dependence on graph size — no persistent
map anywhere. The delta-chain design has never been measured in GoGraph, so #2051 is not
evidence against it. Sprint 330 / #2275 is a P0 spike whose only deliverable is that
measurement, on one structure, before any later phase is authorised.

## 6. The certified envelope

GoGraph is certified for production **within these limits**, each measured, not estimated.

1. **Do not run multi-second analytical reads on the same engine as writes** (#2274). While
   any write is in flight, short-read latency is bounded by the duration of the longest
   concurrent read, measured at 1m36s. Read-only workloads are unaffected; write-only
   workloads are unaffected.
2. **Durable write throughput is ~258 op/s and does not scale with writers** (#2193).
   Re-measured today: 231.4 / 270.2 / 260.6 / 256.5 op/s at 1 / 8 / 64 / 256 writers. This is
   close to the filesystem's measured `F_FULLFSYNC` floor of 265.7/s for a
   one-fsync-per-commit design; the defect is that commits cannot coalesce, because the fsync
   happens inside the visibility barrier. `wal.Writer.SyncGroup` already implements the
   coalescing and reaches 127 582 op/s outside the barrier. Same root cause as #2274; phase
   P5 of the MVCC programme closes it.
3. **A non-columnar projection demotes the filter beneath it** (#2277, found this cycle and
   **not** the item carried from 2026-07-30). All five queries below return **one** row, from
   a 20 000-node label scan:

   | query | plan | allocations |
   |---|---|---|
   | `RETURN n.w AS x` | `ColumnarProject → ColumnarFilter → Scan` | **74** |
   | `RETURN n.w AS x, n.w AS y` | columnar | 87 |
   | `RETURN n.w + 1 AS y` | `Project → Filter → Scan` | **39 575** |
   | `RETURN count(*) AS x` | `Project → GlobalAggregateAdapter → … → Filter → Scan` | **39 572** |
   | `RETURN n` | boxed | 39 568 |

   The trigger is neither cardinality nor the `WHERE`: adding an arithmetic expression, an
   aggregate, or an entity return to the **projection** demotes the **filter**, so every
   *scanned* row is boxed — about 2 allocations per scanned node. The control makes it
   unambiguous: a columnar full scan returning **20 000** rows allocates **89** objects, so a
   query returning one row allocates **445× more** than one returning twenty thousand. At
   1 M nodes a single `RETURN count(*)` costs on the order of a million allocations.
   Architectural: the scan and filter are columnar-eligible in every case, so a chunk-to-row
   adapter beneath a row-based `Project` may be a bounded fix, but it changes how execution
   modes compose and needs a decision.

4. **Operator scratch chunks are built at 4096 rows regardless of cardinality** (#2276),
   ~92 % of the bytes on the columnar path. Analysed but deliberately not fixed this cycle:
   `FillChunk`'s contract makes `n < maxRows` mean end-of-stream, and `eager_aggregation.go`
   compares against `DefaultChunkCapacity` literally, so shrinking a scratch without shrinking
   `maxRows` in lock-step **silently truncates aggregations**. That is not a change to rush.
5. **`db.schema.visualization()` returns an empty result set** rather than an error. Behaviour
   decision outstanding.
6. The MEDIUM set carried forward: snapshot node-path non-determinism, the global label-index
   mutex, and the plan cache bounded by entry count rather than bytes (~1 GiB effective
   ceiling, measured at 1008.96 MiB retained).

## 7. Method notes

- **A background task notification reported "exit code 0" over a red gate three times this
  cycle**, including once over `make: *** [lint] Error 1`. Every gate in this document was
  confirmed by writing `EXIT=$?` into the log and reading it back.
- **Two of my own harnesses lied, in opposite directions.** One measured a degree cliff on a
  node with no edges and reported `n:0` alongside a plausible timing. One measured reader
  starvation at −8 % because its "long" read was 11.7 ms; the real figure is −51× to −61×.
  Both were caught by checking what the harness had actually measured rather than what it
  returned.
- **A gate that cannot fail is not evidence.** Every new assertion this cycle was proved to
  bite by injecting the exact defect it names — and one of them, the #2271 allocation gate,
  failed that check and had to be redesigned around an engagement counter.
- **My own expected values were wrong twice**, both times where the engine was right: an
  ordering assumption on pattern-comprehension output, which openCypher does not specify, and
  an `int64` type assertion on a boxed value.
