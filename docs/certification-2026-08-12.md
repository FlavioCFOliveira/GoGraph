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
   | 3 | `sprint-343`, exit | **`MAKE_CI_EXIT=0`** — green, coverage 87.1% |

   Run 3 is **not evidence that #2420 is fixed**, and is not offered as any. Nothing in this
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
condition near the start of a workload** rather than a steady-state race — which would also
explain why running the shipped test longer has never been what reproduces it. This is
stated as *favoured*, not established: the two models are separated only at p≈0.08, and the
amplified probe differs from the shipped test in more than exposure.

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

### 4.2 Concurrency — the levels the module publishes

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

1. **CPU was not profiled in this cycle.** No `pprof` capture was taken and no example was
   exercised under a profiler, so no statement is made about where CPU time goes. The
   standing figures are those of `docs/cpu-vs-neo4j-memgraph-2026-08-11.md` and the
   sprint-340/341 remediation, not re-derived here. §4 measures throughput and allocation,
   which is not the same thing as attributing CPU.
2. **Latency was not measured at the published levels.** §4.2 reports throughput only; no
   p50/p99 was captured, so half of the "latency and throughput at 1, 8, 64, 256, 1024"
   requirement is unmet.
3. **The write-scaling half of the concurrency sweep was not run**
   (`store/txn/write_scaling_bench_test.go`). §4.2 is a read workload.
4. **The whole-tree soak layer was not run**, and remains unmeasured, as it was at the
   previous certification.
5. **Container and cgroup behaviour was not exercised.** This remains the most significant
   known gap: the module sets `GOMEMLIMIT` **nowhere**, the engine-wide ceilings introduced
   at the previous certification are fixed constants rather than host-adaptive, and a
   4M-relationship Cypher load was **OOM-killed** in an 8 GB container on 2026-08-11 while
   Memgraph held the same graph in 658 MB. Filed as rmp #2407/#2405/#2404; unaddressed.
6. **Single host, single architecture.** No Linux, no NUMA, no multi-socket.

---

## 6. Findings

| id | severity | title | status |
|---|---|---|---|
| #2420 | 9 / 9 | A pinned snapshot observes a partial transaction; `make ci` RED | **OPEN — blocker** |
| #2250 | 9 / 8 | Reverse type filter admitted the pair, not the instance | **FIXED** `588f75b8` |
| #2421 | 8 / 7 | Fixed engine-wide ceilings exceed a container's cap; OOM instead of a typed error | **FIXED** `d369f4f1` |
| #2366 | 8 / 7 | UNIQUE value permanently unusable after a peer's committed release | **OPEN — designed, not implemented** |
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

# §4.2 concurrency at the published levels. Pooled, so it measures the engine
# and not the handshake.
go test -run='^$' -bench='^BenchmarkBoltPooledReadWork$' -benchmem -count=3 ./bench/soak/
```

## 8. Commits

| commit | what |
|---|---|
| `588f75b8` | `fix(cypher)`: a reverse type filter admits the INSTANCE, not the pair (#2250) |
| `32e9c3ae` | `test(lpg)`: make the partial-read oracle self-diagnosing when it fires (#2420) |
| `d369f4f1` | `fix(cypher,bolt)`: derive the engine-wide ceilings from the container's cap (#2421) |
