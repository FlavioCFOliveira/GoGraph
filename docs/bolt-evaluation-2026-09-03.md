# Bolt server evaluation — 2026-09-03

Sprint 353 (GoGraph Optimization Laboratory), at HEAD `d4f49b85`, on darwin/arm64,
10 cores, 32 GiB.

## Verdict

**Bolt is not throttling the engine. The engine is throttling Bolt.**

At 64 goroutines through the Bolt wire, **92.2% of all mutex delay is `graph/lpg`
property-info shard locks** and **under 0.2%** is anything inside `bolt/server`.
Unsharing the server buys **10.353x at 64** — and that ceiling sits on the engine
behind Bolt, not on Bolt.

This inverts the framing the evaluation was commissioned under, which asked whether
the Bolt layer was capping the engine's exercisable throughput.

## Two corrections to `docs/contention-inventory-round2.md`

Both concern the same published row, and both are defects in how the number was
derived rather than in the measurement itself.

### 1. Site #3 is misattributed at the leaf, twice over

Round 2 publishes site #3 as `bolt/server/serve.go:802` → `applyVersionedInstant`,
holding 98.55% of blocked time at 1024.

* `serve.go:802` is the `handleConn` call. It is a **cumulative frame** that
  inherits every byte of Bolt work beneath it by construction, so it cannot
  localise anything.
* `applyVersionedInstant` is the write barrier, and **#2697 already established
  the barrier holds zero delay**.

Peeked to its callers, the delay is `graph/lpg` property info:

| site | share |
|---|---|
| `graph/lpg/property.go:397` `delNodePropertyInfo` | **67.99%** |
| `graph/lpg/property.go:293` `setNodePropertyInfo` | **24.25%** |
| `bolt/server` frames (`sim.halfPipe.write`, `setWriteDeadline`) | **< 0.2%** |

Registered as task **#2710**.

### 2. Ceiling arms are never normalised by their own level-1 cell

A ceiling arm must read 1.000x at level 1, where there is nothing yet to unshare.
`dst-concurrent-bolt-ceiling` reads **1.095x**: its ten replicas each accumulate a
tenth of the writes, so each answers cheaper queries against a smaller graph.

Round 2's handicap table lists six arms whose level-1 cells sit *below* 1.00 and
concludes that **every** ceiling is therefore a lower bound. This arm's cell sits
*above* 1.00, which makes its ceiling an **upper** bound to be discounted — and the
arm is omitted from that table entirely. Its published headline of **16.307x** was
never normalised at all.

Corrected figure for the ceiling measured here: 10.353 / 1.095 ≈ **9.46x**.

Registered as task **#2712**.

## Re-baselined scaling at HEAD

Settled host, loadavg `1.78 / 2.17 / 2.52` before → `9.19 / 5.69 / 3.94` after (the
sweep is its own load), exit 0 read from inside the log.

| workload | 1 | 8 | 64 | 256 | 1024 | ops/s@1 |
|---|---|---|---|---|---|---:|
| `bolt-wire-read` | 1.000 | 1.459 | 1.597 | 1.683 | 1.714 | 77,002 |
| `bolt-connect-churn` | 1.000 | 1.640 | 2.115 | 2.605 | 3.142 | 26,680 |
| `dst-concurrent-bolt` | 1.000 | 2.200 | **0.453** | **0.344** | **0.130** | 600 |
| `bolt-tx-read` *(new)* | 1.000 | 1.706 | 1.926 | 2.143 | 1.828 | 47,998 |
| `bolt-tx-read-noquota` *(new)* | 1.000 | 1.714 | 1.869 | 2.110 | 1.792 | 48,465 |
| `bolt-wire-rows` *(new)* | 1.000 | **1.087** | **1.115** | 1.315 | 1.312 | 12,106 |
| `bolt-wire-read-metrics` *(new)* | 1.000 | 1.458 | 1.549 | 1.632 | 1.624 | 78,380 |
| `bolt-connect-churn-quiet` *(new)* | 1.000 | 1.669 | 2.063 | 2.511 | 2.937 | 27,720 |

### Noise floor

A-vs-A on a settled host (loadavg `1.45 / 1.80 / 2.80`), by the same machinery that
produced every ratio above. Nine ratios that should read 1.000 landed **within
±1.7%**, arm spreads ≤3.5%.

**One cell has its own, much wider floor.** `dst-concurrent-bolt`@64 A-vs-A reads
**0.885x with spreads of 11.79% / 21.47%**, so that cell's floor is **±11.5%** and
must be applied separately. Any future claim about that cell is judged against
±11.5%, not ±1.7%.

An earlier floor was measured, found contaminated by a concurrent validation run on
the same host, and **discarded whole** rather than partially salvaged — a partial
salvage would have left the two arms with different sample counts. It is quarantined
under the campaign artefacts with the reason recorded. The replacement floor was
therefore taken *after* the campaign rather than before it; same machinery, same
host, same HEAD, but the ordering is stated rather than implied.

### Reproduced and refuted against round 2

**Reproduced:** `bolt-wire-read` at every level (≤1.7%, at the floor);
`bolt-connect-churn` at 8/64/256; `dst-concurrent-bolt` at every level — 2.200 vs
2.206, 0.453 vs 0.470, and 0.130 vs 0.127 at 1024.

**Refuted:** `bolt-connect-churn`@1024 reads 3.142 vs round 2's 2.733, **+15%**,
clearing the floor. That cell also carries 2708/30000 (**9.0%**) backpressure
refusals against round 2's 5.6%, so it measures the refusal path as much as the
accept path and is not a clean scaling number at either round.

## Four hypotheses refuted

Each was tested by a design built so that it *could* refute, and each null was
re-taken on a settled host after a first take at loadavg 4.7–10.8.

| hypothesis | test | result |
|---|---|---|
| per-RECORD `SetWriteDeadline` (`serve.go:1409`) costs | **differential**: the 100-row op sets it 102× against the count op's 3×, so a real cost must show ~34× the delta | count **1.012**, rows **0.999**. No. |
| `txQuota` global mutex throttles the wire | single-variable A/B with `MaxOpenTxPerPrincipal=-1` | wire **1.004**; ceiling 0.996 / 0.982 / 1.002 / 0.998 |
| the metrics backend contaminates Bolt numbers | backend off vs on | 1.458 vs 1.459 at 8. Bolt emits only 2 counters per message |
| rejection logging confounds the 1024 cell | quiet-logger arm | quiet is *slower* (2.937 vs 3.142) |

Also refuted: **sharding the quota by principal**. Distinct-key against same-key
reads 0.990–1.038, so the cost is the mutex itself, not the shared counter.

The two Bolt global mutexes *are* expensive in isolation — the quota lock 5.7–10.4×
against no lock, `RegistryUpdate` 8.9× worse from 1 to 8 goroutines — but at
37–215 ns they are roughly **2.6% of a 27.5 µs transaction**, which is why the wire
never sees them. Registered as **#2714** on Mandate 3 principle, priority 5, with
that sizing stated in the task rather than hidden.

## Findings that are not contention

* **`bolt-wire-rows` scales 1.087x at 8**, the worst Bolt path measured — but its
  block profile is **38–53% `sync.(*Cond).Wait`** in the harness's own `SimConn`
  condvar (`internal/sim/simconn.go:193`), so it is **not attributed** to the module.
* **The observatory's Bolt numbers may understate the real server.** Over real TCP
  with the official driver, `examples/23_bolt_server` scales **2.96x at 8** and
  **3.97x at 64** (35,627 → 105,471 → 141,437 q/s; p50 71 µs, p99 166 µs) where the
  pipe-based `bolt-wire-read` reads 1.459x at 8. The two workloads also differ in
  query cost, so the gap is **not claimed** as transport. Registered as spike **#2711**.
* **Allocations are already tight.** On a 100-row read the *entire server* is
  **15.35%** of allocated objects (~2 objects per row); the rest is the client
  driver, so the 1,449 allocs/op figure must not be attributed to Bolt.
  `chunking.go:234` allocates exactly 1 object per inbound message. Registered as
  **#2716** at priority 2, worth ~2% of allocations.
* **Explicit transactions cost 2.08–2.21× an autocommit query** (68,160 vs
  32,713 ns), mostly the two extra round trips rather than the locks.
* **`bolt/server` publishes no per-message latency histogram**, against the
  observability mandate. Registered as **#2715**, sequenced behind #2698 because
  adding histograms moves Bolt onto the emission path #2698 indicts.

Checked and cleared: the RECORD flush policy matches Memgraph byte for byte; the
pooled decoder retains only fixed-size state; `crypto/rand.Read` is a per-P DRBG in
Go 1.27.1, not a syscall.

## A measurement-integrity defect in the observatory itself

A re-taken noise floor returned nine ratios **byte-identical** to a run ten minutes
earlier, including its 102.86 s duration, in a window that completed in under a
second. Go's test cache served the whole thing. The only tell is `(cached)` on the
`ok` line, which nothing in the harness checks.

All 13 campaign logs were audited: **exactly one cache hit, and its first window was
real**, so no number published here came from cache. That audit covers this campaign
only — **round 1 and round 2 were produced with nothing warning about `-count=1`**
and should be re-checked. Registered as **#2713**.

## Prior art

Read at pinned SHAs, structural insight only.

* **Memgraph `145e5c26`** — touches its transaction registry twice per *connection
  lifetime*, stores query text per-Interpreter, and resolves the per-user counter
  once at login then uses a bare atomic.
* **Neo4j `f213380f`** — a lock-free `ConcurrentHashMap` set plus one `volatile`
  query-text field.

Neither locks per message, and neither sets a per-message write deadline. The first
observation motivated #2714; the second was **measured and refuted** here, which is
the reminder that prior art is evidence about a design space, never evidence about a
cost in this codebase.

## Scope and limits

Not run: `make ci`, the openCypher TCK, `-race`, and the soak and nightly layers.
Nothing added by this evaluation is production code, but that remains unverified
until sprint close.

`examples/34_bolt_transactions` produced **no performance evidence** — its scenarios
complete in 4.5 ms, so its profile is startup. It contributed correctness only: auth
rejected and accepted, commit, rollback, FAILURE recovery and TLS all green.

Round 2's 16.307x at 1024 was not re-measured at 1024; the ceiling measured here is
10.353x at 64.
