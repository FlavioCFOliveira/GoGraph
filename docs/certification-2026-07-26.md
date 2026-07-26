# Production-readiness certification — GoGraph module

**Date:** 2026-07-26 · **Head:** `afe1681` · Apple M4 (10 cores), `darwin/arm64`, go1.26.5

Scope: the **`GoGraph` module**. The 34 programs under `examples/` are **instruments**, not
subjects — they exercise the module under realistic conditions so its correctness,
performance and efficiency can be observed. An example failing for its own reasons (a
missing CLI argument) is not a module finding, and is reported as such below.

**Verdict: certified for production**, with two documented non-correctness limitations and
one defect found and fixed during this pass.

---

## 1. The defect this pass found

`db.stats.refresh()`, added earlier the same day, **would have deadlocked a production
binary**.

`Engine.RefreshStatistics` wraps its scan in `Graph.View`. A procedure runs inside query
execution, which is already inside `Graph.View`, and `visMu` is a non-re-entrant
`sync.RWMutex` — so the nested acquisition hangs. The engine's re-entrancy guard converts
that into a panic, but #2168 compiled the guard out of the production read path, so it
fires only in a debug or race build.

**Every non-race run passed. Only `make ci` caught it.** That is the strongest evidence in
this report for the race-enabled gate being mandatory rather than advisory: without it, a
hang would have shipped.

Fixed in `afe1681` by separating barrier acquisition from the work — `scanStatsLocked`
(body, no acquisition), `RefreshStatisticsLocked` (for a caller already holding the
barrier), `RefreshStatistics` (takes it, for external callers). Correctness is unchanged
rather than traded: the scan only reads, and the caller's read barrier already pins exactly
the snapshot it needs.

**The class was then checked, not assumed.** `cypher/` contains exactly three barrier
acquisitions; the other two are reachable only while holding the engine's *writer
serialisation* — a different lock, so ordinary lock ordering rather than re-entrancy.
Confirmed by running `cypher`, `graph` and `store` under `-tags=gograph_debug` with the
guard compiled in: clean.

## 2. Correctness evidence

| Instrument | Result |
|---|---|
| openCypher TCK, execution level | **3897 / 3897**, 0 failed, 0 undefined, 0 pending |
| `make ci` (tidy, fmt, vet, build, `-race` short layer, lint, coverage gate) | **exit 0**, coverage **86.9 %** |
| Full suite under `-tags=gograph_debug` (re-entrancy guard active) | clean across `cypher`, `graph`, `store` |
| `golangci-lint` | 0 issues |

## 3. ACID evidence

| Property | Evidence |
|---|---|
| **Atomicity** | `store/txn` green; `TestExplicitTx_Rollback_DurableAbsent` — a rolled-back transaction leaves nothing durable |
| **Consistency** | commit-time NOT NULL check inside the barrier before the fsync; constraint suites green; `#1507` apply gate orders applies so no reader sees a state no serial schedule produces |
| **Isolation** | **DST swarm: 400 runs, 400 passes, 0 failures**, 10 workers, 1 m 25 s. `TestRunInTx_DurableThenVisible_ConcurrentReader` — a concurrent reader never observes a not-yet-durable write |
| **Durability** | `internal/crashinject`, `store/wal`, `store/recovery`, `store/checkpoint`, `store/snapshot` all green. `TestRunInTx_DurableThenVisible_RecoversWithoutClose` — durability survives without a clean close. `TestRunInTx_WALFsyncFailure_RollsBackAndSurfaces` — an fsync failure rolls back rather than reporting success |

## 4. The examples, as instruments

All 34 build against the module. All 34 exercise it cleanly.

- **32 ran unattended to completion.**
- **2 required arguments** the unattended runner did not supply, and are **not module
  findings**: `24_social_network_cli` needs a subcommand and `25_software_house_api` needs
  `-d <dir>`. Exercised properly:
  - example 24 through all six subcommands — `init`, `seed`, `stats`, `snapshot`,
    `plandiff`, `query` — including a full persistence round-trip (`init` → `seed` →
    `snapshot` → `query` returns 3 613 nodes) and the reorder peephole reporting a
    1.09× speed-up;
  - example 25 came up as a service on `:8080` over its data directory and served
    correctly for the observation window.

## 5. Performance and efficiency, as measured this cycle

Every figure is `benchstat` over repeated runs; each has a standing benchmark and a
document under `docs/benchmarks/`.

| Change | Effect |
|---|---|
| Aggregate argument unboxed (#2185) | allocations/row **7.00 → 1.00** — the `count(*)` floor exactly; time −67 % grouped, −77 % global |
| Columnar shape coverage (#2186) | scan **5/15 → 9/15**, hop **4/7 → 7/7**; allocations −98 % to −99.8 %; the labelled-endpoint and inert-`LIMIT` cliffs **removed**, not reduced |
| Partitioned label scan (#2187) | **1.90×** at 10 cores; the label now costs nothing |
| Index seek outranks columnar (#2204) | an indexed query was **slower than unindexed**; now **108×** / **457×** faster, and unindexed workloads pay nothing (probe gated on index count) |
| Expand-into (#2213) | triangle **16.1×** (4.41 s → 275 ms), allocations **−96.4 %** |
| Edge-column validity bitmap (#2205) | 1.750 → **1.625** B/edge at degree 64; 1.679 → **1.531** at 324 |
| Write counters (#2212/#2190) | allocations/write **unchanged**; +4.68 % time, documented and accepted |

## 6. Documented limitations — neither a correctness defect

Both were **verified by test**, not asserted.

**Write throughput does not scale with concurrent committers (#2193).** The engine holds
the exclusive visibility barrier across the WAL fsync, so committers serialise and the
coalescing that `store/txn` already implements is unreachable. This is a **throughput
ceiling, not a fault** — and the barrier that causes it is precisely what
`TestRunInTx_DurableThenVisible_*` proves correct. Removing it requires the engine to move
from eager-apply to stage-then-apply (the #1671/#2051 work, reverted once at 5.4× time and
43× memory), so it is **blocked on a decision**, not on effort. Single-writer throughput is
unaffected.

**Planner statistics have no consumer (#2196, consumer half).** They are collected and feed
only `Explain` text. An unused planner *input* cannot produce a wrong answer, and
`TestStatsRefreshProc_ResultsUnchangedByRefresh` pins that: five query shapes return
byte-identical results before and after a rebuild. The reachability half shipped this cycle
(`db.stats.refresh()`, rate-limited); the consumer half is gated on the #2162 margin-gating
decision.

## 7. Security posture

- The procedure surface is fenced by an allow-list
  (`TestSec_Cypher_Procs_OnlyReadOnlyIntrospection`) that also rejects side-effecting
  namespaces. It refused an unbounded `db.stats.refresh()` during this cycle and was
  right to; the shipped form is rate-limited to one rebuild per 30 s, and the allow-list
  entry carries its own justification because that list is the review record.
- `imp_user` **fails closed**: impersonation is unimplemented and is now refused rather
  than silently ignored, which previously gave a client believing it was restricted full
  authority.
- Bolt driver compatibility is ratcheted at **28/37** checks with the remaining 8 recorded
  with causes.

## 8. What certification does not cover

- **Soak** is a periodic reliability exercise, not a release gate (an OOM-killed runner
  made it one once, and it was deliberately removed). A multi-hour mixed workload under
  `GODEBUG=gctrace=1` should be run before a major release.
- The three architectural findings — reader indicator (#2203), node memory (#2192), space
  reclamation (#2194) — have **design documents** and are specified, not implemented. None
  is a correctness defect: they are a read-scaling ceiling, a memory-efficiency gap against
  the incumbents, and a missing feature respectively.
- Nothing in this cycle is **pushed**. Certification is of the working tree at `afe1681`.
