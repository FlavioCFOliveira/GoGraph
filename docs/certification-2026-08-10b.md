# Production-readiness certification — GoGraph under extreme load and concurrency (2026-08-10, round 2)

**Date:** 2026-08-10 · **Entry head:** `b78f78b7` · **Exit head:** `013df3c5` (see §11) · Apple M4 (10 cores),
`darwin/arm64`, macOS 26.5.2, go1.26.5, `kern.ipc.somaxconn=128`, `ulimit -n` 1048576 · Peer read at
**Memgraph `9ddfa27`** (2026-08-10)

This cycle answers the same question as [round 1](certification-2026-08-10.md) — **is the module fit
for production environments of extreme demand in load and concurrency?** — and it exists because round
1 could not finish answering it. Round 1's verdict was *"CERTIFIED for correctness, safety and
reliability under sustained concurrent load. NOT CERTIFIED on absolute throughput at extreme
concurrency — because the instrument that would establish it does not yet exist."* It also found that
two of the gates it had relied on could not fail.

Both of those are closed here. Ranked by the project's decision framework —
**correct → secure → fast → efficient** — with every axis evaluated against Memgraph read at source.

---

## Verdict

**CERTIFIED for production environments of extreme load and concurrency, within the envelope in §9.**

The clause that round 1 could not supply is now supplied: **engine query throughput at 1, 8, 64, 256
and 1024 concurrent clients is measured**, for read, write and mixed workloads, with zero errors and
monotonic degradation past saturation. The gates that could not fail now can, demonstrated by
injection — and the 60-minute leak gate, re-run under the corrected instrument, both **passed and
recorded that it asserted**. The security rung, which round 1 explicitly did not re-audit, was audited
and yielded one **HIGH** defect, now fixed.

In one line: **1024 concurrent Bolt clients, 6 798 reads/s and 3 674 committed writes/s with zero
errors, 55 minutes of sustained load with zero goroutine and zero descriptor growth, openCypher TCK
3897/3897 and `-race` clean throughout.**

| Rung | Verdict | Basis |
|---|---|---|
| **Correct** | **PASS** | `make ci` green at **four** heads, `MAKE_CI_EXIT=0` read inside each log; `-race` clean; openCypher TCK baseline **3897** unchanged; coverage 87.0–87.1% |
| **Secure** | **PASS, and stronger than at entry** | One HIGH defect found and fixed: both engine-wide memory ceilings were **unlimited by default**. Three further surfaces examined and recorded as sound. Scope in §9 |
| **Fast** | **PASS — established, not inferred** | Pooled sweep, 105/105 sub-benchmarks, zero errors: peak **24 959 reads/s**; at **1024 clients** still **6 798 reads/s, 3 674 writes/s, 5 878 mixed ops/s**. Monotonic degradation at 102× core over-subscription |
| **Efficient** | **PASS with two open, quantified gaps** | Handshake cost quantified and removed from the instrument (−60% allocations/op). Plan-cache miss quantified at 1.61× and its exposure **bounded**. Node memory 378–423 B/node vs Memgraph's 204 remains open |
| **Reliable under sustained load** | **PASS, and this time it asserted** | 60-minute leak gate over **330 samples**: goroutine and fd slopes exactly **0.000**, heap slope **+37 B/sample** against a 1 MB epsilon, and the gate printed `SLOPE CHECK ASSERTED` — the positive case is now observable, not just the negative (§8) |

**The most consequential finding is a security default, not a performance number.** Both of GoGraph's
engine-wide memory ceilings resolved to *unlimited* whenever `GOMEMLIMIT` was unset — which is the Go
runtime's default state. Per-unit budgets were finite, but under extreme concurrency the **aggregate**
is the binding constraint, and it was switched off (§3).

---

## 1. Method, and what would have made it wrong

Every figure below was produced in this cycle. The disciplines applied, each because it has produced a
false result on this project before:

- **The exit status is read from inside the log**, never from the harness notification. The harness
  reported "exit code 0" for runs whose status had to be confirmed from the recorded
  `MAKE_CI_EXIT=` line.
- **Benchmarks waited for the host to settle.** The sweep launched at a 1-minute load average of
  **2.38**, after a probe had driven it to 16.5. A loaded host biases concurrency ratios
  *systematically*, not noisily.
- **Arms differ in exactly one respect.** Enforced after my own first draft violated it (§5.1).
- **Every gate was validated by injection.** A gate that has never been observed to fail is not
  evidence (§4.1).
- **Premises were re-measured before being built on**, and two were refuted: one in the task
  description (§6.1) and one of my own (§3.1).
- **Peer claims were read at source**, and doing so overturned a comparison round 1 had recorded in
  GoGraph's favour (§10).

---

## 2. Correct

| Gate | Head | Result |
|---|---|---|
| `make ci` | after #2396 | `MAKE_CI_EXIT=0` · coverage **87.0%** |
| `make ci` | after #2398 | `MAKE_CI_EXIT=0` · coverage **87.0%** · TCK `ok` 62.191 s |
| `make ci` | after #2397 | `MAKE_CI_EXIT=0` · coverage **87.1%** |
| `make ci` | after #2393 | `MAKE_CI_EXIT=0` · coverage **87.0%** |
| Entry baseline | `b78f78b7` | `MAKE_CI_EXIT=0` · 122 packages `ok` · TCK `ok` 63.101 s |

Every log was independently grepped for `FAIL`, `--- FAIL`, `DATA RACE`, `panic:`, `make: ***` and
`Error N`: **0 matches in all five runs**. `const tckExecutionBaseline = 3897` is unchanged, and the
TCK ran green after the security change — which matters, because that change altered a default that
every query path traverses.

---

## 3. Secure — the rung round 1 did not audit

Round 1 recorded *"No fresh hostile or security audit"* as an explicit envelope hole. Audited here
(rmp #2398), with one HIGH finding fixed and three surfaces recorded as examined-and-sound.

### 3.1 HIGH — both engine-wide memory ceilings were unlimited by default

`bolt/server.resolveMaxInboundDecodeBytes` and `cypher.resolveGlobalMaxResultBytes` both derive their
ceiling from the Go soft memory limit and both returned **0 — unlimited** when no limit was set.
Measured, not assumed: an unconfigured Go process reports `debug.SetMemoryLimit(-1) ==
math.MaxInt64`, so the fall-through branch was the **default** path, and `NewInboundBudget(limit)` with
`limit <= 0` makes `tryReserve` always succeed.

Consequences, on a default-configured server:

- **Bolt, pre-authentication.** Aggregate inbound memory was bounded only by `MaxConnections` ×
  per-connection limits: 1024 × 16 MiB of reassembly buffers plus 1024 × 128 MiB of decoded
  collections. It is reachable **before authentication** because a `HELLO` must be decoded before it
  can be authenticated.
- **Cypher.** The per-query budgets are finite (10 M rows / 1 GiB), but a per-query bound says nothing
  about the sum: N concurrent clients each inside their own 1 GiB still materialise N GiB, and this
  was the only bound governing the aggregate.

**This is the mitigation for a DoS the project had already identified.** Audit #1842 (2026-07-02)
described it in exactly these terms — *"N concurrent clients could each materialise a per-query-capped
result whose SUM exhausts the host — a load-dependent memory-DoS the per-query cap alone cannot
stop"* — and shipped the fix deriving from `GOMEMLIMIT` *"else unlimited"*, deliberately, so as never
to reject a legitimate workload on a host whose memory the module cannot know. That reasoning left the
identified exposure open in every deployment that had not set `GOMEMLIMIT`.

Fixed with the maintainer's agreement on the magnitudes: `DefaultMaxInboundDecodeBytes` = 1 GiB
(matching `cypher.DefaultMaxResultBytes`, 64× the largest single message) and
`DefaultGlobalMaxResultBytes` = 4 GiB (4× the per-query cap). Deployments that set `GOMEMLIMIT` keep
the derived `lim/8` and `lim/2` unchanged; the `…Unlimited` sentinels remain the only route to no
ceiling. The existence of those sentinels is what settles intent — an opt-out is redundant if zero
already meant unlimited.

**One regression test per site**, because a fix landing on one of two call sites is a closed ticket
with a live defect. Both fail at the parent commit with a diagnostic naming the exposure.

**Checked in the opposite direction too**, since a finite ceiling can refuse legitimate work: the whole
tree is green under `make ci` — which builds and tests all **38 example packages**, only the *coverage*
gate filters them — the TCK is green, and `examples/35_mvcc_mixed_workload` run as a program at
`-nodes 3000 -readers 8` exits 0 with **zero** memory-refusal messages, `readers_starved=0` and a peak
heap of **44.9 MB** against the new 4 GiB ceiling. A legitimate overlapping-result workload is
additionally pinned as admitted by `TestEngine_GlobalMemoryBudget_DefaultAdmitsLegitimateOverlap`.

**Two pre-existing tests were pinning the defect as though it were the contract**, and one of them is
the more instructive: `TestEngine_GlobalMemoryBudget_UnlimitedByDefault` kept **passing** after the
change — its workload is ~160 KB against a 4 GiB ceiling — while its name and doc comment asserted
that *"the engine imposes no global bound"*. A test that passes while documenting the opposite of the
truth is worse than one that fails: a future reader would have cited it as evidence. Renamed to
`…_DefaultAdmitsLegitimateOverlap`, which is what it actually pins.

### 3.2 Examined and found sound

Recorded so that coverage can be **stated** rather than implied by silence:

| Surface | Verdict |
|---|---|
| **PackStream length and amplification defences** | Sound. Any 32-bit prefix above `MaxInt32` is rejected **while still unsigned** (so a 32-bit platform cannot wrap it negative past the budget checks); a declared length is bounded against the bytes actually remaining; and a cumulative per-message decoded-memory budget is charged **before** allocating, with per-element costs calibrated against measured `MemStats` and pinned by `TestDecoder_ChargeUpperBoundsGoAllocation`. Verified by running the project's own amplification gates (`ok bolt/packstream`), not by reading them |
| **Per-principal transaction quota** | Sound by design. It keys on the authenticated principal, so it is exactly as strong as authentication; under the opt-in-only `NoAuthHandler` the client chooses its own principal, but total concurrent transactions remain bounded by the `MaxConnections` semaphore, which **refuses and counts** rather than queueing |
| **Slow / silent peer (slowloris)** | Sound. A separate handshake deadline precedes `ConnTimeout`; the per-read idle `ConnTimeout` (30 s default) is reset before **every** read; a per-message write deadline applies. An idle connection cannot hold a slot indefinitely |

---

## 4. The gates that could not fail — closed

### 4.1 rmp #2396 — three instruments, and the worst was not the one reported

Round 1 found that `TestNoGrowth_HeapFDGoroutine` and `TestLatencyP99_Stable` both logged
*"insufficient samples … skipping slope check"* and returned success, so the two assertions the
project names as its soak acceptance criterion had evaluated nothing while the layer reported `ok`.

A third instance, not named in the task, was worse. `TestGCPause_Stable`'s insufficient-samples early
return sat **before** its 200 ms max-pause ceiling — and a ceiling needs no regression at all, one
sample suffices. So on a host collecting a single sample, *"max GC pause 0.19 ms against a 200 ms
ceiling"* — the strongest single figure round 1 quotes — would silently not have been asserted. The
ceiling now asserts first and independently.

Three underlying defects, none cosmetic:

- **`no_growth`'s sample count was a race between two timers.** Its context deadline equalled
  `warmup + measurement`, so the final ticker tick and the deadline fell on the same instant. Round 1
  collected **1** sample where the arithmetic says 2; with one interval of grace it now deterministically
  collects 2.
- **`p99` regressed over its own warm-up.** It dropped warm-up windows only when *more* than
  `p99WarmupWindows` existed, so a short run fitted a slope to precisely the windows it declares
  unrepresentative.
- **A sentinel drawn from the answer's value space.** `ngCountFDs` returns `-1` when no fd directory is
  readable, and that `-1` was recorded as a **sample value**. A metric constant across every sample has
  slope 0.000 — so on such a host the fd criterion passed *while measuring nothing*. Now a hard failure.

The sample floor rises from 2 to **6** on stated grounds: OLS spends two points on the fit, so two
points regress with zero residual degrees of freedom and three leave one; six leave four, the smallest
count at which one anomalous sample cannot dictate the slope.

**Abstention is now `t.Skip`**, carrying the setting that would make the check assert — and it is a
**failure** under `SOAK_FULL=1`, `GOGRAPH_NIGHTLY=1`, or any explicit `SOAK_*` window override, because
those configurations assert that the criterion will be evaluated.

**Validated four ways**, because a gate that has not been seen to fail is not evidence:

| Configuration | Expected | Observed |
|---|---|---|
| Short layer, default window | abstain visibly | **3× `--- SKIP`** where there had been 3 silent `PASS`; 89.6 s vs 89.7 s, so the default runtime did not increase |
| Override set, too few samples | fail closed | **`--- FAIL`, exit 1** |
| Documented intermediate windows | assert | 9 samples / 8 windows / 11 samples — all three **`SLOPE CHECK ASSERTED`** |
| Intermediate window + injected 2 MB/sample leak | fail | slope **2 115 450 B/sample** against the 1e6 epsilon → **FAIL, exit 1** |

The last row is the one that matters: the gate can fail on real growth, and the measured rate matches
the injected rate, which validates the regression arithmetic itself.

The decision rule is extracted as `slopeGateDecide` in an **untagged** file so `make ci` pins it — a
gate's own logic must not be guarded only by the tag that failed to exercise it. Its regression test
fails against the pre-fix rule with the two round-1 cases named explicitly.

---

## 5. Fast — engine throughput, established

Full tables in
[`docs/benchmarks/bolt-concurrency-pooled-vs-churn-2026-08-10.md`](benchmarks/bolt-concurrency-pooled-vs-churn-2026-08-10.md).
**105/105 sub-benchmarks, `count=3`, medians, host settled to load 2.38, zero errors.**

`ns/op` under `b.RunParallel` is the inverse of **aggregate** throughput, not per-client latency.

| Clients | Read ops/s | Write ops/s | Mixed 80/20 ops/s | Read allocs/op |
|---:|---:|---:|---:|---:|
| 1 | 21 348 | 11 377 | 17 973 | 134 |
| 8 | **24 959** | **12 537** | **20 809** | 134 |
| 64 | 18 906 | 9 080 | 15 898 | 134 |
| 256 | 9 569 | 4 753 | 7 830 | 135 |
| 1024 | 6 798 | 3 674 | 5 878 | 141 |

On a work-sized graph (2 000 nodes, label-scoped count) the pooled read arm peaks at **20 749 ops/s**
at 8 clients and holds **5 282 ops/s at 1024**.

**What the previous figures were actually measuring.** At matched concurrency — same seed, same query,
differing only in connection reuse — pooling removes **199 of 333 allocations (60%)** and **37.9 kB of
64.4 kB (59%)** per read, and **194 of 391 (50%)** per write. Round 1's absolute figures were
majority connection setup, exactly as it suspected but could not quantify.

**The 1024-client cliff was largely connection churn, not the engine.** The comparison that carries
this is **within this cycle**, between the two matched arms measured in the same sweep on the same
settled host: churn read **2 363** ops/s at 1024 clients against pooled **6 798** (**2.88×**), and
churn write **993** against pooled **3 674** (**3.70×**). This is consistent with the host's
**128-deep** accept backlog being overrun by 1024 concurrent connects — there, the churn arm measures
the kernel as much as the server.

Round 1's own churn figures at 1024 were different again — 1 215 read, 2 018 write, 2 719 mixed —
which is a further reason not to build on them: a churn arm at 1024 clients on a 128-deep backlog is
measuring an admission queue, and it is not reproducible run to run. The 2 363/993 figures above are
this cycle's, not round 1's, and only same-sweep pairs are compared.

**Per-operation allocation is flat in the concurrency level** (134 → 141 from 1 to 1024 clients). That
flatness is also evidence about the harness: a per-operation metric that tracked the peer count would
be a fixture bug rather than a finding.

### 5.1 One result goes the other way, and my own first draft was wrong

At **256** clients the pooled read arm is **slower** than churn — 9 569 vs 13 192 ops/s (0.73×) — the
only inversion in 15 matched pairs, with all three pooled runs below all three churn runs. A plausible
mechanism is that connection churn *accidentally throttles* concurrency via the accept backlog, so its
apparent advantage is admission control rather than an engine property. **That mechanism is untested
and is recorded as an open question, not as a finding.**

My first draft of the pooled arm changed connection handling **and** the workload together, and
measured 1 923 allocs/op against the churn arm's 333 — a 5.8× apparent regression that was in fact the
2 000-node scan swamping the handshake it was meant to isolate. Hence two seed sizes in separate arms,
each differing from its comparator in one respect only.

---

## 6. Efficient

- **Per-operation cost at the wire, engine-bound:** 134 allocs/op and 26.5 kB/op for a read over a live
  Bolt session; 197 and 43.1 kB for a committed write.
- **Plan-cache literal miss: quantified and bounded** (§6.1).
- **Node memory remains the largest open gap: 378–423 B/node against Memgraph's 204 and Neo4j's 128.**
  **These figures were NOT re-measured this cycle**; they are carried from the round-3 three-way
  head-to-head of 2026-07-26 and recorded in `docs/design-node-memory.md`, which also notes that
  GoGraph's **8.71 B/edge** is best-in-class. They are cited here as an open item, not as a fresh
  result. Closing the gap is a representation change — the design concludes the in-memory model must
  split — and requires the maintainer's agreement.

### 6.1 rmp #2393 — the premise was half wrong, and the control is the finding

Measured interleaved in one process, medians of 5, on a graph small enough that the front end rather
than execution dominates:

| Arm | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| hit (parameterised) | 33 482 | 17 100 | 90 |
| **miss (2048 distinct texts)** | **53 804** | **34 674** | **408** |
| control (inlined literals, 64 texts → hits) | 33 272 | 16 844 | 98 |

A miss costs **1.61× latency, 2.03× bytes, 4.53× allocations** — an extra 20.3 µs and 318 allocations
per query. But the control measures **0.994×** the hit arm: **inlining a literal costs nothing by
itself.** The entire penalty is the cache miss, which only occurs once a workload's distinct-statement
count exceeds the 1024-entry LRU.

So #2393's premise — *"a workload that inlines literals … misses on every statement"* — is true only
for workloads whose statement texts are effectively unbounded (generated Cypher inlining
high-cardinality identifiers, ad-hoc traffic). Below 1024 distinct texts inlining is free, which
includes every GoGraph example and the whole TCK. Closed on that evidence by the maintainer's
decision; extraction is filed as **#2399** with the numbers.

---

## 7. Reliability under sustained load

### 7.1 The short soak layer, now reporting honestly

`go test -tags=soak -count=1 -v ./bench/soak/...` — **`SOAK_EXIT=0`, 89.647 s** (round 1: 89.7 s):

| Result | Tests |
|---|---|
| **5 PASS** | `TestBoltSoak_60s`, `TestBoltCypherMixed_Smoke`, `TestCypherRW_Analytics_Smoke`, `TestPprofCapture`, `TestSlopeGateDecide` + `TestSlopeGateFloorRejectsExactFits` |
| **4 SKIP** | `TestGCPause_Stable`, `TestLatencyP99_Stable`, `TestNoGrowth_HeapFDGoroutine` (slope checks cannot assert in this window — and now say so), `TestCypherRW_Analytics_30m` |

Round 1 reported this same layer as **7 PASS, 1 SKIP**. The layer does not do less than it did; it now
**states** what it does. `TestGCPause_Stable` still asserts its **200 ms max-pause ceiling** here, from
3 samples, having been lifted out of the sample gate.

---

## 8. The 60-minute leak gate — the variant that asserts

`SOAK_FULL=1 go test -tags=soak -run TestNoGrowth_HeapFDGoroutine ./bench/soak/` — 5-minute warm-up
plus 55-minute measurement, sampling heap, file descriptors and goroutine count every 10 s with a
linear regression on each. This is the configuration that meets the project's soak criterion on its own
terms, and under this cycle's change **insufficient samples here is a failure**, so a `PASS` now
entails that the criterion was actually evaluated rather than skipped.

**`NOGROWTH_EXIT=0` · PASS in 3600.00 s · 330 samples · `SLOPE CHECK ASSERTED over 330 samples` · CSV
`soak-artefacts/no-growth-20260810T164717Z.csv`**

| Metric | Regression slope (per 10-s sample) | First sample (t=10 s) | Last sample (t=55 m 0 s) |
|---|---:|---:|---:|
| Heap | **+37 B** | 843 008 B | 852 048 B |
| Goroutines | **0.000** | 6 | 6 |
| File descriptors | **0.000** | 6 | 6 |

Over 55 minutes of sustained mixed load the goroutine and descriptor slopes are **exactly zero** and
the heap slope is **+37 B per 10-second sample** — about 12 kB across the whole window, against a
1 MB/sample epsilon. That is the project's soak criterion met on its own terms.

**Two details make this result stronger than round 1's, beyond the numbers themselves:**

- **It sampled 330 times, which is exactly `measurement ÷ interval`.** Round 1 collected 329, one short,
  because the final tick raced the context deadline. The grace fix removed that race, so the sample
  count is now deterministic rather than a coin flip on the last tick.
- **`SLOPE CHECK ASSERTED over 330 samples` is printed by the gate itself.** Round 1's identical-looking
  `PASS` could not distinguish "asserted and held" from "skipped the assertion", which is precisely how
  its short-layer siblings passed while checking nothing. Under this cycle's change, insufficient
  samples in this configuration is a **failure**, so this `PASS` entails that the criterion was
  evaluated — the positive case is now observable, not merely the negative one.

The heap slope is **+37** here against round 1's **−517**; both are three to four orders of magnitude
below the epsilon and the sign is oscillation, not trend. The heap moves between roughly 0.84 and
0.85 MB across the window, which is why the slope over all 330 samples — not the difference between two
endpoints — is the figure that decides it.

---

## 9. The envelope — what this certification does NOT cover

1. **A changed security default has not been exercised in production.** §3.1 makes two ceilings finite
   that were previously absent. `make ci` and the TCK are green and a legitimate overlapping-result
   workload is pinned as admitted, but an embedder whose workload genuinely exceeded 1 GiB of
   concurrent inbound decode or 4 GiB of concurrent result bytes will now receive a typed rejection
   where it previously received none. That is the intended behaviour; it is still a behaviour change.
2. **The 256-client inversion is unexplained** (§5.1). A mechanism is proposed and explicitly untested.
3. **No fuzzing, and no audit of the per-query budget accounting race.** §3 covers the four surfaces it
   names; it did not fuzz the wire decoder, and it did not freshly audit whether result-byte accounting
   can be outrun by a query that allocates before the check, or whether every typed-error path releases
   its budget. Those continue to rest on existing tests.
4. **`go test -tags=soak ./...` was NOT run** — only `./bench/soak/...`. The whole-tree soak layer
   remains unmeasured; the Makefile records that `graph/io/csv` alone takes 800.8 s under `-race`.
5. **The 4-hour p99 variant was not run.** p99 stability is asserted only over the documented
   intermediate window (8 post-warm-up windows, ~100 s), which is enough to prove the gate asserts, not
   enough to characterise a 4-hour tail.
6. **Node memory (378–423 B/node vs 204)**, the **plan-cache extraction** (#2399) and the
   **per-execution plan rebuild** (#2391) are open, quantified, and not addressed here.
7. **Single host, single architecture.** Apple M4, 10 cores, `darwin/arm64`, `somaxconn=128`. No Linux,
   no NUMA, no multi-socket, no cgroup-constrained container — and §10 notes that the absolute
   magnitudes chosen in §3.1 are *not* host-adaptive, which matters most precisely in a container.
8. **Durability under crash was not re-exercised**; it rests on `internal/crashinject` and the
   `store/wal` + `store/recovery` suites, green as part of `make ci`.
9. **Dated audits and certifications were deliberately not rewritten.** They are point-in-time records.

---

## 10. Evaluation against Memgraph

Read at source, **Memgraph `9ddfa27`**, not from memory. Both are in-memory-first Label-Property-Graph
engines, so the comparison is like for like.

### 10.1 A correction to round 1's comparison

Round 1 recorded, as a GoGraph advantage: *"GoGraph's per-query budget is finite by default where
Memgraph's is `UNLIMITED_MEMORY`."* The first half is verified and still true
(`query_memory_control.hpp:27`, `interpreter.cpp:3702` — Memgraph's per-query limit is opt-in). **But
the comparison was one-sided, and reading the peer's source overturned it.**

`flags/memory_limit.cpp:20-38`: Memgraph's **global** memory limit defaults to **100% of physical
memory when swap is enabled and 90% otherwise**, obtained by *reading the host* via
`utils::sysinfo::InstalledMemory()`. So the two engines had **opposite** default postures:

| | Per query / per message | **Aggregate, engine-wide** |
|---|---|---|
| **GoGraph at entry** | finite (10 M rows / 1 GiB; 16 MiB / 128 MiB) | **unlimited unless `GOMEMLIMIT` was set** |
| **Memgraph** | unlimited by default | **finite by default: 90–100% of installed RAM** |
| **GoGraph at exit** | finite | **finite: 4 GiB results, 1 GiB inbound decode** |

Round 1 compared GoGraph's per-query strength against Memgraph's per-query weakness and omitted the
axis on which the peer was ahead — the one that binds under extreme concurrency. This is precisely why
the project's rule is to read the source rather than quote a previous conclusion.

**Memgraph's mechanism is also better than the one adopted here.** GoGraph's new defaults are fixed
absolute constants; Memgraph's scale with the machine. On a 512 GiB host GoGraph's 4 GiB aggregate
result ceiling is very conservative; inside a 2 GiB container it is far too generous. Memgraph pays for
this with per-OS code to read installed memory — which is exactly why the host-memory option was
rejected here — but the adaptivity is a real advantage, and it is the natural next step for GoGraph.
Setting `GOMEMLIMIT` restores proportionality today.

### 10.2 Where GoGraph is ahead

| Axis | GoGraph | Memgraph `9ddfa27` |
|---|---|---|
| **Bulk load keeps ACID** | The bracket is a *transaction*: versioning stays on, only the adjacency's copy-on-write granularity changes. Rollback, isolation and durability intact, content proven byte-identical | `IN_MEMORY_ANALYTICAL` makes `CreateAndLinkDelta` **return immediately** (`mvcc.hpp:243-245`, `316-318`), so no deltas exist — and `Abort()` skips undo entirely when `transaction_.deltas` is empty (`inmemory/storage.cpp:1496-1498`, *"if we have no deltas then no need to do any undo work during Abort"*). **Its fast bulk mode gives up Atomicity** |
| **Per-query bound** | Finite by default: 10 M rows / 1 GiB, typed error | Opt-in; `UNLIMITED_MEMORY{0}` |
| **Bulk-load surface** | One scoped callback that cannot leak; no mode switch, no global state | A per-database *storage mode* with transition preconditions |

### 10.3 Where the two agree — and why GoGraph's write ceiling is not a defect

**Memgraph also serialises its commit under one global lock,** re-verified at this commit:
`inmemory/storage.cpp:1158` takes `std::unique_lock{storage_->engine_lock_}` and holds it across
taking the commit timestamp and **appending to the WAL**, for the reason the comment gives at 1173–1176
— *"Write transaction to WAL while holding the engine lock to make sure that committed transactions are
sorted by the commit timestamp in the WAL files."*

GoGraph's write path has the same shape: concurrency in the pre-commit work, a serialised commit
instant, group commit to amortise the WAL. A write-throughput ceiling set by a serialised commit
section is therefore **the design point the leading peer also chose**, not a GoGraph-specific weakness
— and §5's write arm degrades monotonically from its peak rather than collapsing, sustaining 3 674
committed writes/s at 1024 concurrent clients.

### 10.4 Where Memgraph is ahead

| Axis | Memgraph | GoGraph |
|---|---|---|
| **Aggregate memory bound** | Finite by default **and host-adaptive** (§10.1) | Finite by default as of this cycle, but a fixed constant |
| **Plan-cache key** | Keys on **stripped** query text, so `{id:1}` and `{id:2}` share one entry | Keys on **raw** text; miss costs 1.61× — bounded to workloads exceeding the 1024-entry LRU (§6.1). Open: **#2399** |
| **Per-execution work** | Immutable shared plan tree; per execution only a cursor and an arena frame | Rebuilds the physical plan per execution. Open: **#2391** |
| **Node memory** | ~204 B/node | 378–423 B/node — *inherited from the 2026-07-26 head-to-head, not re-measured here* |

### 10.5 Reading the comparison

GoGraph's remaining deficits are concentrated in **per-execution planning cost, memory density, and
the adaptivity of its new bounds**. Its advantages are in **not trading ACID for speed** and in
**bounding per-unit resources by default**. Under the project's own ranking — correct → secure → fast →
efficient — GoGraph is ahead on the two higher rungs and behind on parts of the two lower ones, which
is the right way round. What changed this cycle is that the *aggregate* security bound, where the peer
was ahead, is no longer absent.

---

## 11. Commits

| Commit | Task | What |
|---|---|---|
| `7782e785` | #2396 | make the reliability gates able to fail |
| `59dae9ad` | #2398 | bound the engine-wide memory ceilings by default (**HIGH**) |
| `70a2495d` | #2397 | add a pooled-connection arm so the sweep measures the engine |
| `95eeec20` | #2393 | measure the plan-cache literal miss, and bound who it affects |
| `013df3c5` | — | this document (sprint 338 closes with it) |

**Exit head `013df3c5`: `make ci` green, `MAKE_CI_EXIT=0` read inside the log, coverage 87.0%, TCK
`ok` in 65.768 s, no `FAIL` / `DATA RACE` / `panic:` / `make: ***` anywhere in the log.**

All four are on branch `sprint-338`, which branches off the `sprint-337` line. **Note on branch state:**
`sprint-337` was never merged into `main` — `main` is still at `Merge branch 'sprint-336'` — so this
cycle's work sits two sprints ahead of the production branch. Nothing is lost, but merging to `main` is
a separate, outward-facing decision and was deliberately not taken here. Nothing has been pushed.

## 12. Findings filed

| Task | Priority | What |
|---|---|---|
| **#2399** | 6 | Plan-cache literal extraction: designed and quantified (1.61× miss), deferred with the maintainer's agreement because it is a front-end architecture change with TCK risk |

## 13. Reproduce

```bash
# Correctness gate — read the status from inside the log, not from a wrapper.
make ci > ci.log 2>&1; echo "MAKE_CI_EXIT=$?" >> ci.log

# The concurrency sweep. Check `uptime` first: a 1-minute load average above
# ~2.5 biases concurrency ratios systematically.
go test -run='^$' -bench=BenchmarkBolt -benchmem -count=3 \
  -timeout=7200s ./bench/soak/ > sweep.out 2> sweep.err

# Soak layer. Use -v: without it Go prints only `ok` and shows neither the
# skips nor their reasons, so a non-verbose run is not evidence.
go test -tags=soak -count=1 -v -timeout=3600s ./bench/soak/...

# The leak gate that asserts (60 min). Insufficient samples FAILS here.
SOAK_FULL=1 go test -tags=soak -count=1 -v \
  -run TestNoGrowth_HeapFDGoroutine -timeout=90m ./bench/soak/

# The intermediate windows that make each slope check assert in ~100 s.
SOAK_NOGROWTH_MEASURE=90s go test -tags=soak -count=1 -v \
  -run TestNoGrowth_HeapFDGoroutine ./bench/soak/
SOAK_P99_DURATION=100s SOAK_P99_WINDOW=10s go test -tags=soak -count=1 -v \
  -run TestLatencyP99_Stable ./bench/soak/

# Plan-cache miss vs hit.
go test -run=^$ -bench=BenchmarkPlanCache -benchmem -count=5 ./cypher/
```
