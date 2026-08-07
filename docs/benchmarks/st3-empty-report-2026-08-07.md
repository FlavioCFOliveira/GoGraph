# The ST3 durability sighting that reported nothing — rmp #2347

**Date:** 2026-08-07
**Branch:** `sprint-335`
**Status:** reporting defect FIXED and regression-guarded; underlying violation
REPRODUCED under load and raised as rmp #2349 — fixed and made deterministic in
[`checkpoint-instant-boundary-2026-08-07.md`](checkpoint-instant-boundary-2026-08-07.md)

> The heading of section (b) below still reads "NOT reproduced" because that is
> what it said when the search was started; the section itself records the
> reproduction that followed. It is left in that order deliberately — the search
> and its negative arms are the evidence, and rewriting them to the conclusion
> would hide how the conclusion was reached.

## The sighting

During a full `make ci` coverage pass on 2026-08-07, `internal/sim`'s
`TestST3_CheckpointTeardown_Scenario` failed with exactly this and nothing more:

```
--- FAIL: TestST3_CheckpointTeardown_Scenario (…)
    durable_scenarios_test.go:193: ST3 seed 0xc0ffee violation:
FAIL	github.com/FlavioCFOliveira/GoGraph/internal/sim	…
```

ST3 asserts the DURABILITY invariant — recovered ⊇ acked, no phantom, no torn
CREATE — so an unexplained ST3 violation is a potential ACID-durability defect.
The test reaches line 193 only when `report != nil`, and the scenario builds the
report under a `len(v) > 0` guard, so at least one violation had been recorded
and its rendering produced nothing.

Two distinct defects were in play and were deliberately not conflated:

- **(a) the reporting defect** — certain, because a violation that renders empty
  tells the operator nothing;
- **(b) whatever produced the violation** — unknown.

## (a) Root cause: the log filter, not the renderer

`(*SimReport).String()` **cannot** return an empty string: it unconditionally
writes a `SIMULATION FAILED` header, the seed, the failed op, the oracle
summary, the violation list and a `Reproduce with:` line. Reading the renderer
therefore refutes the obvious hypothesis outright.

The body was discarded **downstream**, by the gate that surfaces a failing
coverage run. `scripts/cover_gate.sh` filtered the captured log with a
line-pattern grep:

```
grep -E '(^--- FAIL|^FAIL[[:space:]]|panic:|fatal error:|_test\.go:[0-9]+:|signal:|DATA RACE)'
```

Every line of the report body is an *indented continuation* of the
`durable_scenarios_test.go:193:` line, and none of them matches any of those
patterns. The filter keeps the first line of a failure and throws away the rest.

Reproduced byte-exactly by feeding the real output shape through the old filter:

| | input | survives the old filter |
|---|---|---|
| `--- FAIL: TestST3_…` | ✓ | ✓ |
| `    durable_scenarios_test.go:193: …violation:` | ✓ | ✓ |
| `        SIMULATION FAILED` | ✓ | ✗ |
| `          Violations (1):` | ✓ | ✗ |
| `            - [ACID_DURABILITY] …` | ✓ | ✗ |
| `        Reproduce with: go run ./cmd/sim …` | ✓ | ✗ |
| `FAIL	github.com/…/internal/sim	…` | ✓ | ✓ |

The output is exactly the sighting: header, nothing, verdict. The scope is not
ST3 — **every** test in the module whose failure detail spans more than its
first line was affected, including race reports (whose frames start at column 0)
and panic goroutine dumps.

A second, compounding fault: the complete log *was* published to
`cover.out.testlog`, but the next run — green or not — overwrote it, and the
script never told the operator the file existed. The re-run performed to
investigate the failure is what destroyed the evidence of it.

### The fix

- `scripts/failblocks.awk` replaces the line-pattern grep with a block
  extractor that understands the two shapes go test emits: **indented** blocks
  (`--- FAIL` plus its indented detail) and **verbatim** blocks (panics, fatal
  errors and race reports, whose bodies are not indented). Its line cap
  announces itself instead of truncating silently, and keeps the *first*
  failure rather than the last.
- A failing run's log is now also written to `cover.out.failed.<pid>.log`,
  which no later run can clobber, and the path is printed.
- `SimReport` carries the scenario name and exec mode, so a sighting says which
  workload broke, not only which invariant.
- A report that carries **no** violation now renders a loud
  `*** REPORTING DEFECT ***` line, and `durableReport` **panics** when handed an
  empty violation slice. A non-nil report means the scenario failed; one that
  names nothing is a bug in the harness, and must cost a stack trace rather than
  an investigation.

### Guards

| Guard | Proves |
|---|---|
| `scripts/test_failblocks.sh` (run by `internal/scriptgate.TestFailblocksGate`) | Runs the OLD filter beside the new one on the same input and asserts the old one loses the invariant name, the seed and the race/panic frames. The guard demonstrates the defect rather than asserting its absence. |
| `internal/sim.TestSimReportNeverRendersEmptyShort` | A report with a violation names the invariant, the observation, the scenario, the mode and the seed; one without announces itself as a defect. |
| `internal/sim.TestDurableReportPanicsWithoutViolationShort` | The fail-loud contract at the point of construction. |

Both Go guards were **validated against a defective build**: forcing
`String()` to return `""` fails both subtests, and reverting it restores green.

## (b) The underlying violation: NOT reproduced

**Instrument.** The sighting occurred only under `make ci`'s coverage pass,
never in isolation. That pass differs from an isolated run in three ways that
all change timing, and the search reproduces all three: coverage
instrumentation (`-covermode=atomic -coverpkg=./...`) rather than the race
detector; heavy competing **fsync** load from the durable packages
(`store/wal`, `store/txn`, `store/recovery`, `store/checkpoint`) looped in
parallel; and sustained duration. A CPU-burner load was deliberately **not**
used — it had already been shown not to reproduce this class of failure,
whereas parallel WAL fsync load did.

**Instrument validated on a defective build.** Injecting a synthetic
`ACID_DURABILITY` violation into `checkpointTeardownResult.violations` makes the
search loop fail and print the complete report, naming scenario, mode,
invariant, observation and seed. The injection was then reverted and the suite
re-run green, so the search is known to be able to fire.

**Result — REPRODUCED, and it is a real ACID Durability violation.**

| Arm | Iterations | Failures |
|---|---:|---:|
| ST3 under coverage instrumentation **with** parallel fsync load | 15 | **2** |
| Same, **without** the parallel load | 17 | 0 |

Both failures were `TestST3_CheckpointTeardown_Scenario` at seed `0xBADF00D`
— the *clean* scenario (`faultAtTeardown=false`), so no injected fault is
involved — and both reported the same shape:

```
  Scenario:    checkpoint-teardown (mode=concurrent)
  Violations (1):
    - [ACID_DURABILITY] op="<teardown-durability>": acknowledged commit
      "…-n7-48082217" lost across the durable teardown (acked=142 recovered=141)
```

Exactly one acknowledged commit lost, both times. A client was told SUCCESS and
the write is absent after recovery: the unrecoverable direction.

The two arms are not a controlled comparison — 2/15 against 0/17 is a weak
contrast on its own — but they are consistent with a load-dependent race, and
the mechanism below was established from the code rather than from the rate.

### Mechanism (tracked as rmp #2349)

`store/checkpoint`'s non-blocking checkpoint takes, in phase 1 under
`RunUnderCommitLock`, a **durability** position (`wlog.DurableOffset()`) and a
**visibility** position (`g.BeginRead()`), and in phase 3 prefix-truncates the
WAL up to the durability position. `checkpoint.go` names the hazard itself:

> The dangerous direction is a transaction that is durable below the watermark
> but whose commit instant is not yet published: `at` would not see it, the
> image would not carry it, and phase 3 would truncate away the only record of
> it. An acknowledged commit would be lost.

and argues it cannot happen because "`store/txn.Tx.Commit` defers `exitWriter`
past `ApplyVersioned`, which is what publishes the instant".

**The premise is checked on a path production does not use.** The Cypher engine
commits through `Tx.CommitWALOnly`, which performs no in-memory apply and
publishes no instant, and whose `defer t.store.exitWriter()` fires as soon as
the fsync returns. `cypher/api.go` states the resulting state outright —
*"DURABLE, BUT NOT YET VISIBLE … the instant itself is published later, when the
write bracket unwinds through `lpg.Graph.endWrite`"*. In that window the
in-flight writer count is zero while the frame is already inside the durable
offset, so the drain completes, the watermark includes the commit, `at` excludes
it, and the truncate discards it.

The gate that exists to catch precisely this —
`TestCheckpoint_WatermarkAndInstantDescribeTheSameBoundary` — drives all four of
its writers through `st.Begin()`/`tx.Commit()`, the one path on which the
invariant genuinely holds. It is careful to prove its oracle non-vacuous, and it
still could never observe this defect.

### A second stale premise found on the way (rmp #2345 ledger)

`internal/sim/durable_scenarios.go` justified folding ST1 into ST2 by asserting
that the engine "serialises every write commit … under one exclusive visibility
barrier", so `SyncGroup` "is ALWAYS a solo leader with zero followers".
**Measured and refuted:** driving 12 concurrent Bolt writers × 40 commits
through a real store, 480 `SyncGroup` calls produced **16 followers**
(`store.wal.SyncGroup.coalesced`). Multi-member group commit is reachable
through the engine. The fold still stands, but on coverage grounds, not
unreachability; the comment has been corrected.

### Standing-search status

The search is not closed by a green re-run. What has changed is that the next
sighting will be *actionable* — it names the invariant, the observation, the
scenario, the mode and the seed, and its complete log survives the re-run that
follows it.
