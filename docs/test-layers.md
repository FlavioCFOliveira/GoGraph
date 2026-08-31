# Test layers

GoGraph organises its test corpus into three layers ordered by runtime
budget. Every test belongs to exactly one layer; deeper layers are
strict supersets of the shallower ones.

| Layer | Budget | Selector | Default? |
|---|---|---|---|
| `short` | < 60 s per package soft, 240 s hard (**enforced on `make ci`** — see below) | none (default) | yes — the default local run |
| `soak` | minutes | `-tags=soak` or `SOAK_FULL=1` | no |
| `nightly` | hours | `-tags=nightly` or `GOGRAPH_NIGHTLY=1` | no |

The mapping is monotonic: the `soak` layer always includes the `short`
layer; the `nightly` layer always includes both `short` and `soak`.
There is no way to run a deeper layer alone, by design — a regression
in the short layer must surface before the longer suites are even
considered.

### The `drivercompat` suite

One suite sits outside those three layers because it is not a duration
tier but a *capability* gate: the standing driver-compatibility suite
(`bolt/server/driver_compat_test.go`, task #2191). It drives the official
`neo4j-go-driver` against the in-process Bolt server and asserts 37
checks, reporting a pass/fail/degraded tally and **ratcheting** the
passing count — a check that regresses from pass to fail breaks the
build, exactly as the TCK execution baseline does for query results.

Run it explicitly:

```bash
go test -tags=drivercompat -run TestDriverCompatibility -v ./bolt/server/
```

It is tag-gated rather than layered because it stands up a server and a
real driver connection per check. It is excluded from the default run so
`make ci` keeps its short-layer budget; the current floor and the reason
each remaining check fails are recorded in the file's own header, which
is the authoritative list.

### The short-layer budget, and what enforces it (rmp #2566, #2577, #2599)

The per-package cost budget is **enforced on the routine gate**: `test-short`
pipes its output through `scripts/pkg_time_budget.sh`, so every `make ci` reads
it. `SOFT_BUDGET` (60 s) warns; `HARD_BUDGET` (240 s) fails the run.

The script echoes the `go test` output through **verbatim** and reads the plain
`ok<TAB>pkg<TAB>0.330s` summary lines, so **what you see on `make test-short` is
unchanged**. It deliberately does not use `-json` on that path: `-json` implies
`-v` and would bury the routine run in per-test noise to obtain the identical
per-package numbers.

| Target | What it is |
|---|---|
| `make test-short` | the short layer **and** the budget check — one run, both outcomes |
| `make test-short-timings` | an alias for `test-short`, kept as the named entry point for ad-hoc exploration (`SOFT_BUDGET=30 make test-short-timings`) |
| `make test-timing` | a different gate entirely: the serial wall-clock phase (rmp #2517), not this |

`test-short-timings` is now an alias because its own copy of the command had
**drifted**: it omitted `GOGRAPH_PARALLEL_SUITE`, making it the one target that
ran the whole parallel suite with the quiet-machine gates *asserting* — exactly
the contention rmp #2517 removed everywhere else. Delegation makes that class of
drift impossible.

#### Measured cost, 2026-08-25

Whole suite, `GOGRAPH_PARALLEL_SUITE=1 go test -race -count=1 -timeout=30m ./...`,
darwin/arm64, 10 cores, **load average 2.33 before / 5.11 after**, 11 m 39 s wall,
exit 0: **124 packages, 2586.6 s summed**, eleven over the 60 s soft budget, two
over the 240 s hard ceiling.

**The budget is only meaningful with the pass named, and the difference is a
factor of 2.62.** `make ci` runs the suite twice: `test-short` under `-race`, and
`scripts/cover_gate.sh` under `-coverpkg=./... -covermode=atomic` with **no**
`-race`. The same tree, measured the same day:

| Pass | Total | Packages over 60 s |
|---|---|---|
| `-race` (`test-short`) | 2586.6 s | **11** |
| coverage, no `-race` (`cover-gate`) | 988.7 s | **1** |

So every figure in this section is **under `-race`**, which is the stricter of the
two and the one the budget gates.

#### The known exceptions

Every package over the soft budget is listed, with its measured cost and the
reason. The reason is the same for all of them and it is measured, not asserted:
**the race detector**, whose per-package amplification is what puts them over.
Under coverage instrumentation only `internal/sim` exceeds 60 s at all. The
eleven unmarked rows are the 2026-08-25 in-suite run; the marked row is a later,
standalone addition explained under the table.

| Package | `-race` | no `-race` | Amplification |
|---|---|---|---|
| `internal/sim` | 565.8 s | 103.9 s | 5.4× |
| `cypher` | 321.7 s | 54.4 s | 5.9× |
| `cypher/exec` ‡‡ | 177.8 s → **18.1 s** | 3.0 s → **1.3 s** | 58.4× → **13.6×** |
| `bench/audit352` † | 180.6 s | 50.1 s | 3.6× |
| `examples/26_social_scale_bench` | 167.8 s | 20.5 s | 8.2× |
| `bench/csrorder` | 110.4 s | 30.2 s | 3.7× |
| `bench/cyclicjoin` | 96.9 s | 10.6 s | 9.1× |
| `bench/cypher_scale` | 89.2 s | 16.7 s | 5.4× |
| `examples/24_social_network_cli` | 86.5 s | 40.2 s | 2.2× |
| `search` | 71.4 s | 35.2 s | 2.0× |
| `cypher/tck` | 68.5 s | 7.9 s | 8.7× |
| `store/recovery` | 66.3 s | 58.6 s | 1.1× |

Three packages exceed the 240 s hard ceiling — `internal/sim`, `cypher` and
`bench/audit352` — and each carries a named override below. `cypher/exec` was a
fourth breach and is **not** on that list: it was fixed rather than accommodated
(rmp #2672), and the reasoning is recorded under the table. `bench/audit352` is
the one whose breach is invisible in this table, because the figure shown for it
is standalone: its in-suite cost is recorded in the footnote. The other nine
**warn and pass**: they are over the soft budget, which is what a soft budget is
for.

† `bench/audit352` did not exist on 2026-08-25 and its two figures were measured
**standalone**, not in-suite, on 2026-08-29 on the rmp #2645 tree (the commit this
footnote ships in) — same host, load average 1.94 before / 3.29 after (`-race`) and
2.93 / 3.20 (no `-race`). They replace the 174.4 s / 50.0 s measured earlier the
same day at commit `d7116485`: rmp #2645 added `schemawalk_hoist_test.go` and
`schemawalk_ab_test.go` to the package, which cost **+6.2 s under `-race`** and left
the no-`-race` column unmoved. A
standalone figure is a **lower bound** on the in-suite one, because it carries none
of the co-tenancy the parallel suite adds, so it is marked rather than silently
mixed with the rest of the column.

**The in-suite cost, measured at last (rmp #2670).** That lower bound was for a
while the only figure the package had, and the ceiling was inferred from it. It
was an inference, not a measurement: runs 1–3 below are the first in-suite
observations `bench/audit352` has ever had, all on 2026-08-29, all `make ci` on
the same tree, and they put the package at **1.76×** the standalone figure —
comfortably over the 240 s global ceiling it had never been tested against. Run 4
is the post-fix verification and is marked separately.

| Run | In-suite `-race` | Load average before | Load average after |
|---|---|---|---|
| 1 (12:06 → 12:16) | 321.5 s | 3.51 12.74 11.71 | 5.58 11.15 13.78 |
| 2 (12:46 → 12:56) | **328.7 s** | 1.34 2.00 5.05 | 4.92 10.24 11.21 |
| 3 (13:28 → 13:39) | 318.1 s | 2.40 2.32 4.38 | 4.45 9.08 9.69 |
| 4 (14:29 → 14:40) ‡ | 328.3 s | 2.19 8.54 9.79 | 4.13 9.96 12.55 |

Each run is bracketed by `uptime` before and after, so the load the figure was
taken under is auditable rather than asserted. The spread is **3.3 %** — narrow
for a mid-sized package, and narrower than `cypher`'s 16 % — and the worst,
328.7 s, is what the override rule below reads. Note that the run with the
**quietest** start (run 2, 1.34) produced the **worst** figure, which is why the
rule takes the worst observation rather than the one measured on the quietest
host: co-tenancy inside the suite, not ambient load, dominates here.

‡ Run 4 is the **verification** run, taken after the override landed: the package
came in at 328.3 s and the gate passed it. Its one-minute load at start was 2.19
but its five-minute load was 8.54 — the host was still shedding the load of an
earlier, interrupted run — so it is recorded as a **contended** observation. It
does not move the ceiling: 328.3 s < 328.7 s, so the worst observed is unchanged
and the derivation below stands as it was. It is kept because a fourth
independent figure landing within **0.4 s** of the worst of the first three is
worth having on the record: it is what makes the 3.3 % spread look like a
property of the package rather than an accident of three runs.

The package is the purpose-built exercise harness for the rmp #352 bottleneck
audit. Its **profiling sweeps are soak-gated** (`//go:build soak || nightly`) in
`sortprofile_soak_test.go` (rmp #2652), `gctax_soak_test.go` and
`relprops_soak_test.go` (rmp #2667); the 180.6 s above is what remains in the short
layer once they are gated. Ungated it measured **399.77 s**, over the hard ceiling,
and the four tests moved under #2667 accounted for 225.0 s of that:
`TestGCTax_ResidentGraph` 138.37 s, `TestRelationshipPropsPlans` 82.75 s,
`TestRelPropertyMaterialisationCount` 3.69 s and `TestSubqueryShapeRowCounts`
0.21 s. The last three share one fixture and had to move together: `buildRelGraph`
caches into a package-level variable, so ~82.5 s of that total is the fixture and
whichever consumer ran first paid it. None of the four asserts anything about the
quantity it measures. `TestScaling_SubqueryComplexity` (33.40 s) was
**deliberately left in the short layer**: it asserts that every subquery shape
ships exactly one row per outer row at every graph size, which is a check that can
catch a cardinality regression, and this document's own rule is that coverage is
not traded for a cost target.

‡‡ **`cypher/exec`: the 58.4× was ONE test, and it is now fixed (rmp #2672).**
The arrow figures above are before → after that task; the whole note below
replaces an earlier one that read the 58× as a property of the package and
guessed at "heavily shared mutable state under concurrent access". The guess was
directionally right about the mechanism and wrong about the scale and the
location, so it is corrected here rather than quietly amended.

The concentration was total. Measured standalone at HEAD `a136fcd0`, quiet host,
whole package, 580 top-level tests, 0 skips, all passing, per-test times from
`-v`: **177.621 s** under `-race` (1583 s of CPU at 889 %) against **1.334 s**
without it (2.16 s of CPU at 124 %). Of that 177.7 s summed `-race` cost,
**`TestIndexBuffer_ConcurrentStress` alone was 173.57 s — 97.7 %** — against
0.080 s for the same test without `-race`, an amplification of **2170×**. Every
other test in the package amplified 1× to 21×, which is ordinary for
ThreadSanitizer. So the true package amplification was 133× on wall and ~733× on
CPU, not 58.4×: the 3.0 s denominator in the table above is a *coverage-
instrumented* figure, which inflates it.

**What caused it.** That test started 100 writers plus **1000 reader
goroutines**, each spinning on `label.Index.Count`, which takes `i.mu.RLock()` on
one shared `sync.RWMutex` (`graph/index/label/index.go:206`). The cost is
ThreadSanitizer's per-sync-object happens-before bookkeeping: each acquire must
be ordered against every other goroutine participating in that object, so the
cost grows superlinearly in the number of distinct goroutines sharing it. Two
control arms, run under `-race` on a quiet host against a replica built outside
the repo, isolate it from the two obvious alternative explanations:

| Arm | Shape | Cost |
|---|---|---|
| A | 1000 readers, **shared** index | 82.22 s |
| B | 1000 readers, spinning but touching nothing | **0.00 s** |
| F | 1000 readers, each on its **own** `label.Index` | **0.24 s** |
| D / I / H / C / G | 10 / 25 / 50 / 100 / 200 readers, shared | 0.12 / 4.43 / 9.98 / 34.24 / 112.55 s |

Arm B rules out CPU starvation from spinning; arm F rules out the sheer volume
of instrumented work, since the identical reader work on private indexes is
**343×** cheaper. The sweep is superlinear: 10 → 200 readers is 20× the
goroutines for 938× the cost (~N^2.3). Those sweep arms are single observations
each, so the exponent is a characterisation rather than a precise measurement.

**It also explains the instability.** The same test, `-count=3` back to back on a
quiet host, measured **49.35 s, 76.59 s and 174.32 s — a 253 % spread**. A
package that is 97.7 % one test with a 253 % intrinsic spread cannot have a
stable in-suite figure, which is why its seven recorded in-suite observations
range from 154.8 s to 252.3 s. The swing was the test, not the host and not
co-tenancy.

**The fix relocates the cost; it removes no coverage.** The short-layer test
keeps its 100 writers and drops to the measured **50 readers** (the 9.98 s point
above, still oversubscribing all 10 cores 5×), and the 1024-goroutine level that
CLAUDE.md's EXTREME/MASSIVE Concurrent Ready mandate publishes (1, 8, 64, 256,
1024) moves to a soak-gated variant — see "The 1024-reader soak variant" below.
Trimming alone was rejected because dropping a published measurement level would
trade a mandate for a cost target; soak-gating the whole test was rejected
because it would leave the short layer with no concurrent index stress at all.

Detection power was not traded away, and this was verified against the shipped
test rather than argued. With the synchronisation it defends deliberately
removed — `i.mu.RLock()`/`RUnlock()` deleted from `label.Index.Count` — the
50-reader test reported **10 `WARNING: DATA RACE`** findings and failed (exit 1);
with the mutation reverted it passed clean. The race detector needs only two
conflicting accesses, so 1000 readers bought no detection power that 50 does not
have.

**Result, same host, quiet, `uptime` bracketed:** standalone `-race` **177.621 s
→ 18.092 s** (−89.8 %; CPU 1583 s → 132 s), with the dominant test at
173.57 s → 13.99 s and all 580 tests still passing.

#### The 1024-reader soak variant

`cypher/exec/index_stress_soak_test.go` (`//go:build soak || nightly`,
rmp #2672) runs `TestIndexBuffer_MassiveConcurrentStress` — the same driver and
the same assertion as the short-layer test — at **1024 readers**, so the
published EXTREME-concurrency level is still exercised. It joins the soak-gating
precedent set by `bench/audit352`'s `sortprofile_soak_test.go` (#2652),
`gctax_soak_test.go` and `relprops_soak_test.go` (#2667).

Measured cost, this host, `-tags=soak -race`, load average 2,29 5,22 5,66 before
/ 9,98 7,45 6,50 after: **157.28 s**, passing. It is *highly* variable for the
reason the arms above establish — an earlier attempt begun at a 1-minute load of
5,76 had not finished after **600 s** and was abandoned — so treat 157 s as a
quiet-host figure and not a ceiling. `SOAK_TIMEOUT` is 4 h, so it has ample room.
Both layers share one driver function, `runIndexBufferConcurrentStress`, declared
in the untagged file, so the two variants cannot drift apart in shape; only the
reader count differs. The two counts are **not** in the same file: the short-layer
`shortLayerStressReaders` is in `index_stress_test.go`, and the 1024 level's
`massiveConcurrencyStressReaders` is in `index_stress_soak_test.go`, because a
constant used only from a `soak || nightly` file is `unused` in the default build
and `golangci-lint` rejects it. The two files cross-reference each other so
neither count can be changed in ignorance of the other.

The driver reads the index **before** testing its stop channel, so every reader is
guaranteed at least one `Count`, and it rejects a reader count of zero or less.
Without that ordering a reader losing the race to `close(stop)` would perform no
reads at all and the test would still pass — the concurrent read pressure would be
absent while the gate stayed green.

#### `cypher/exec` gets NO per-package override, deliberately

`HARD_BUDGET` stays **240 s** and `PKG_HARD_BUDGET_OVERRIDES` gains **no fourth
entry**. Recorded here so it is not re-derived: the override rule would have read
the worst in-suite observation, 252.3 s, and produced 252.3 × 1.25 = 315.4 →
**360 s**. It was rejected on three measured grounds.

* It would have been a different *kind* of accommodation from the three existing
  entries. `internal/sim`, `cypher` and `bench/audit352` are genuinely heavy
  packages doing real work; `cypher/exec` costs **1.334 s** without `-race`. Its
  budget problem was not size, it was one constant in one test.
* It would not even have bought reliability. A 360 s ceiling sits above a single
  test that varied 49.35–174.32 s on its own, with the upper tail unmeasured, so
  the gate would still have gone red intermittently.
* It would have blinded the gate. At 360 s the package's other 579 tests, which
  together cost ~4 s under `-race`, could have grown by two orders of magnitude
  unnoticed.

After the fix the package needs no accommodation: it lands at 18.1 s standalone,
comfortably inside the global 240 s ceiling. Its post-fix in-suite figure is
recorded as run 8 in the observation table above.

#### `cypher/exec` in-suite observations

The four figures the breach was filed on, and the three quiet-host `make ci` runs
taken under rmp #2672 to establish whether it was genuine at rest, in the same
run-table form #2670 used for `bench/audit352`. All on this host; runs 5–7 are on
HEAD `a136fcd0` with the tree clean and nothing else running; run 8 is the
post-fix verification.

| Run | In-suite `-race` | Load average before | Load average after |
|---|---|---|---|
| 1 | 223.5 s | not recorded | not recorded |
| 2 | 240.6 s | not recorded | not recorded |
| 3 | 224.0 s | not recorded | not recorded |
| 4 | **252.3 s** | 5-minute load 8.54 (contended) | not recorded |
| 5 (17:14 → 17:28) | 194.1 s | 1,75 1,72 1,80 | 4,28 8,57 9,68 |
| 6 (17:36 → 17:50) | 211.1 s | 1,20 2,83 6,13 | 4,89 8,16 10,85 |
| 7 (17:56 → 18:09) | 154.8 s | 2,48 3,70 7,63 | 4,73 7,37 9,97 |
| 8 (post-fix, 19:31 → 19:44) | **12.4 s** | 2,17 3,52 4,87 | 7,13 7,49 7,90 |
| 9 (post-fix, 19:51 → 20:04) † | **15.1 s** | 2,43 3,84 5,93 | 4,41 6,35 7,61 |

† Run 9 is the verification run for the fix: `make ci` exit 0, read from inside
the log, with no `FAIL` and no `--- FAIL:` line anywhere in it, and `test-timing`,
`lint` and `cover-gate` all reached and green (`cover_gate: OK (aggregate 88.4 %)`).
Run 8 is the run before it, which reached `test-short` green — hence its usable
figure — but stopped at `lint`. Both post-fix figures are single observations, and
they are reported as such: a drop from 154.8–252.3 s to 12.4–15.1 s is roughly an
order of magnitude, far outside any noise floor measured here, so it needs no
statistics to be believed.

Runs 5–7 did **not** breach the 240 s ceiling: worst 211.1 s, 12.0 % under it,
with a 36.4 % spread across the three. So the 2-of-4 breach was not reproducible
at rest, and it was not co-tenancy relief either — `bench/audit352` was fully
present alongside them at 309.6 / 314.7 / 294.6 s. What the runs did establish is
that the figure is unstable, and the note above identifies why.

**Reconciling the 177.8 s row.** This document recorded `cypher/exec` in-suite at
177.8 s on 2026-08-25, which is *below* four of the seven observations, so the
override rule as written would have read a figure beneath the observed cost. The
asymmetry resolves in an unexpected way: 177.8 s (in-suite, 2026-08-25) and
**177.621 s** (standalone at HEAD, measured under #2672) coincide almost exactly.
The usual reasoning that a standalone figure is a lower bound on the in-suite one
is therefore **weak for this package**, because its cost was one test that
saturates ~9 of 10 cores by itself: such a test is barely slowed by co-tenancy,
and its own 253 % run-to-run spread swamps whatever co-tenancy adds. The 177.8 s
row was not wrong; it was one draw from a very wide distribution.

#### The premise that did not reproduce

rmp #2585 was filed against a 2026-08-20 run reporting **16 packages over budget
and a 4699 s total**. Re-measured at HEAD, the suite totals **2586.6 s** — 55 % of
that — with **11** packages over. Individual packages diverge far more than the
whole: `cypher/exec` was cited at 295.0 s against 177.8 s here, and
`bench/cyclicjoin` at 306.3 s against 96.9 s.

Those figures carry **no recorded load average**, and a suite total 1.82× a
load-qualified measurement of the same tree is evidence about the machine rather
than about the code. They are therefore cited but **excluded** from the
worst-observed rule below. A measurement without its conditions cannot set a
threshold.

#### The override rule

`PKG_HARD_BUDGET_OVERRIDES` entries are **one stated rule, not a number fitted
per package**: the **worst** in-suite figure ever recorded for that package in
this document × 1.25, rounded up to the whole minute.

| Package | Worst in-suite | × 1.25 | Ceiling |
|---|---|---|---|
| `internal/sim` | 602.9 s | 753.6 s | **780 s** |
| `cypher` | 321.7 s | 402.1 s | **420 s** |
| `bench/audit352` | 328.7 s | 410.9 s | **420 s** |

There is deliberately **no `cypher/exec` entry**, though it breached the ceiling
in 2 of its first 4 in-suite runs. The rule would have produced 360 s; it was
rejected and the package was fixed instead. The full reasoning is under
"`cypher/exec` gets NO per-package override, deliberately" above — the short
version is that the package costs 1.334 s without `-race`, so an override would
have raised the ceiling for 579 cheap tests in order to accommodate one
expensive one.

**Worst-observed, not last-measured.** `internal/sim` has been recorded in-suite
on this hardware at 545.8 s, 557.4 s, 564.0 s, 565.8 s and 602.9 s — a **10.5 %
spread** — and twice more at 600.7 s and 601.7 s, both of which were
`panic: test timed out` against the old 600 s default and so are *lower bounds*
on the real cost. A ceiling fitted to whichever run happened to be measured
would false-red on a busier day; 780 s leaves **29 %** headroom over the worst of
them while still tripping on a genuine 25 % cost regression, and stays far clear
of `SHORT_TIMEOUT` (30 m).

The `cypher` figure was **re-derived when its second observation arrived**, which
is what the single-observation caveat was for. Two in-suite runs on 2026-08-25,
both with load recorded, gave **276.4 s and 321.7 s** — a **16 %** swing, against
`internal/sim`'s **0.3 %** (564.0 s and 565.8 s) across the same pair. Mid-sized
packages vary far more run to run than the big one does, because their co-tenancy
changes with scheduling order. That is also why the global 240 s ceiling, not a
per-package one, is the right instrument for everything below these three.

`bench/audit352` was added under rmp #2670 and is the one entry here that
corrects **no regression**. The package had no entry, so it was gated by the
global 240 s ceiling — a ceiling nothing had ever measured it against, because its
only figures were standalone. When `make ci` was finally run to completion on this
tree the gate fired three times out of three, at 321.5 s, 328.7 s and 318.1 s;
the three observations and their load averages are tabulated in the footnote
above. The rule reads the worst, 328.7 s × 1.25 = 410.9 → **420 s**, which leaves
**27.8 %** headroom over it. The entry is named rather than absorbed into a raised
global ceiling for the reason the `Makefile` comment gives: an accommodation has
to stay visible per package, or it silently starts covering the next package to
drift over.

Two things about this entry are worth keeping in view. First, **420 s is not
`cypher`'s 420 s** — the two ceilings coincide by arithmetic, not by kinship, and
neither constrains the other. Second, the package is a *purpose-built audit
harness*, so a cost regression in it is a regression in an instrument rather than
in the module; the right response to it drifting again is to gate more sweeps to
the soak layer, as #2652 and #2667 already did, not to raise this number.

Keys match as a **suffix** of the import path, not as a substring. This matters:
substring matching would have let `/cypher` also cover `cypher/tck`, `cypher/ir`
and `bench/cypher_scale`, handing an unrelated package a ceiling nobody measured
for it. `internal/scriptgate` asserts every key still names an existing package
directory, because a stale suffix left behind after a rename is not inert — it
starts covering whatever package later ends with it.

#### Where `internal/sim` spends its time

Recorded so the next person does not re-derive it. Whole package,
`go test -race -count=1 -v`, quiet host, 2026-08-25: **974 top-level tests** (22
skipped), **570.1 s** summed against a ~530 s wall.

| Test | Cost | Share |
|---|---|---|
| `TestSchemaMutation_NonVacuityGatesAreNotVerdicts` | 97.20 s | 17.1 % |
| `TestCypherSurface_Scenario_Passes` | 34.09 s | 6.0 % |
| `TestMVCCSessionsCrash_Deterministic` | 25.58 s | 4.5 % |
| `TestMVCCSessions_IsolationGreen20Seeds` | 25.56 s | 4.5 % |
| `TestMergeRel_MultiSeed` | 19.82 s | 3.5 % |
| *top 20 combined* | *331.4 s* | *58.1 %* |

**The package was re-baselined rather than split, and that was a coverage
decision, not an arithmetic one.** Removing even the single largest test leaves
~433 s — still above the old 420 s figure — so no surgical cut reaches the
ceiling; the remaining 954 tests hold the other 41.9 %. Moving the battery to
`soak` was rejected because this document requires it to run under `-race` on
**every** push to preserve the ACID/DST guarantee, and trading that for a cost
target inverts the correct-before-fast order.

The drift is real and is why the gate now runs: rmp #2577 measured **818 tests /
460.9 s** at commit `147e28e4` on 2026-08-20. Five days later the same package is
**974 tests / 570.1 s** — +19 % tests, +24 % cost, unremarked, because nothing on
the routine path was reading the number.

#### What rmp #2566 found, kept as the record

Until rmp #2566 this section claimed the budget was "enforced, not merely
documented". All three of its supporting claims were untrue at HEAD, and each was
verified individually: `make ci` did not invoke the gate; `HARD_BUDGET` defaulted
to `0`, which the script documents as *disabled*; and `PKG_HARD_BUDGET_OVERRIDES`
was set nowhere, so the `internal/sim` override attributed to "the `build + test +
race` job" existed no more than that job did — the only workflow is `release.yml`.

That paragraph is kept because it names the failure mode this section now exists
to prevent: **a ceiling nothing reads is decoration, and it reports success in
exactly the same way as a passing one.** `internal/scriptgate` holds the
regression tests, which fail on the tree described above.

When a package approaches the budget, split it or move its slow cases to the
`soak` layer rather than relaxing the threshold — unless, as with `internal/sim`,
moving it would cost coverage the layer exists to provide. Then re-baseline
deliberately, with the measurement recorded here.

## How a test selects its layer

Two mechanisms are supported. Prefer the first whenever practical.

### 1. Compile-time build tag (preferred)

Place layer-specific tests in their own file with a build-tag header
on the first line:

```go
//go:build soak

package myfeature
```

```go
//go:build nightly

package myfeature
```

Tests in such files are not compiled when the tag is absent, so they
have **zero runtime cost** outside their layer. This is the canonical
mechanism; the existing `internal/stress/` package uses the same
pattern with the `stress` tag (which is part of the soak family — see
below).

### 2. Runtime gate via `testlayers` helpers

When splitting a test into its own file is impractical — for example,
when a single test function mixes short-layer assertions with optional
soak-only steps — call one of the helpers in `internal/testlayers`:

```go
import "github.com/FlavioCFOliveira/GoGraph/internal/testlayers"

func TestSomething(t *testing.T) {
    testlayers.RequireSoak(t)
    // ... soak-only body ...
}

func TestSomethingHeavier(t *testing.T) {
    testlayers.RequireNightly(t)
    // ... nightly-only body ...
}
```

`RequireSoak` and `RequireNightly` call `t.Skip` with a descriptive
message when the corresponding layer is inactive. Skipped tests are
near-instant; the cost is the `t.Skip` call itself.

## Build tags and environment variables

| Variable / tag | Effect |
|---|---|
| `-tags=soak` | activates the soak layer at compile time |
| `-tags=nightly` | activates the nightly layer at compile time (and implies soak) |
| `SOAK_FULL=1` | activates the soak layer at runtime via the helpers |
| `GOGRAPH_NIGHTLY=1` | activates the nightly layer at runtime via the helpers (and implies soak) |
| `GOGRAPH_PARALLEL_SUITE=1` | declares that packages are being tested **in parallel**, so `testlayers.RequireQuietMachine` skips wall-clock/throughput/CPU-time assertions. Set by `test-short` and `cover-gate`; deliberately NOT set by `test-timing`. Detected by PRESENCE, not value — an empty expansion still counts as set, so a Makefile slip cannot silently re-enable a timing gate under load (rmp #2517) |

`SOAK_FULL` is preserved verbatim from the pre-existing toolchain so
existing scripts and developer aliases continue to work.
`GOGRAPH_NIGHTLY` is new; it pairs with the new `nightly` build tag.

### Diagnostic tags — not layers

Two further tags select *assertion* builds rather than test layers. They
do not admit or exclude tests by workload cost; they compile extra
run-time checks into the library itself, and the tests that pin those
checks are tagged to match.

| Tag | Effect |
|---|---|
| `-tags=gograph_debug` | compiles the development-only assertions into the library |
| `-race` | implies the barrier re-entrancy guard, in addition to the Go race detector |

Two assertions are currently gated this way:

- The **integer-distance overflow assertion** in `search/`
  (`overflow_assert_enabled.go` / `_disabled.go`), which panics when a
  Bellman-Ford or Johnson relaxation wraps around. Gated on
  `gograph_debug` alone.
- The **barrier re-entrancy guard** in `graph/lpg/`
  (`reentrancy_enabled.go` / `reentrancy_disabled.go`), which turns a
  same-goroutine nested `Graph.View` / `Graph.ApplyAtomically` — an
  engine-wide deadlock — into an immediate panic. Gated on
  `race || gograph_debug`, so the local gate (`go test -race ./...`,
  run by `make ci`) enforces it on every change. It is excluded from
  released binaries because identifying the calling goroutine needs a
  `runtime.Stack` call that measured **97–99 % of every `Graph.View`**
  (2088 ns against 4.0 ns for the RWMutex pair it guards, plus a 64 B
  allocation per call) and serialised readers on the Go runtime's
  process-global `debuglock`.

Reach for `-tags=gograph_debug` when diagnosing a suspected engine
freeze or a wrapped path distance in a build where `-race` is too slow.

The two mechanisms are independent: build tags gate compilation, env
vars gate runtime behaviour of helpers. A test guarded by
`//go:build soak` is invisible to `SOAK_FULL=1 go test ./...` because
the file is not compiled into the binary in the first place. Conversely,
a test guarded by `testlayers.RequireSoak(t)` compiles in every layer
and is admitted at runtime when either the `soak` tag is set or the
env var is set.

## Makefile targets

The three layers are wired into named `make` targets so the layering
discipline is enforced by tooling, not folklore.

| Target | Layer | Equivalent command |
|---|---|---|
| `make test-short` | short | `GOGRAPH_PARALLEL_SUITE=1 go test -race -count=1 -timeout=$(SHORT_TIMEOUT) ./... \| bash scripts/pkg_time_budget.sh` |
| `make test-timing` | short (serial phase) | `go test -race -count=1 -p 1 -timeout=$(TIMING_TIMEOUT) -run '$(TIMING_RUN)' $(TIMING_PKGS)` |
| `make test-soak` | soak | `go test -race -count=1 -timeout=$(SOAK_TIMEOUT) -tags=soak ./...` |
| `make test-nightly` | nightly | `go test -race -count=1 -timeout=$(NIGHTLY_TIMEOUT) -tags=nightly ./...` |

`test-timing` is not a fourth layer. It is the **same** short layer re-run
serially for the subset of tests whose assertion is a duration, a rate, or a
ratio of them — the phase in which that measurement is valid. `make ci`,
`make ci-soak` and `make ci-nightly` all invoke it, so those assertions gate
every push. See [`RequireQuietMachine` and the `test-timing` phase](#requirequietmachine-and-the-test-timing-phase).

### Why every layer passes an explicit `-timeout`

Go applies a default of **10 minutes per package** when no `-timeout` is given.
Every layer target therefore sets one explicitly, through an overridable
variable, so the timeout is a **backstop against a hung package** and never a
budget the suite is expected to approach.

| Variable | Default | Applied by |
|---|---|---|
| `SHORT_TIMEOUT` | `30m` | `test-short`, `test-short-timings`, `race` |
| `SOAK_TIMEOUT` | `4h` | `test-soak` |
| `NIGHTLY_TIMEOUT` | `12h` | `test-nightly` |
| `NIGHTLY_CI_TIMEOUT` | `4h` | `test-nightly-ci` |

All four are overridable on the command line, so a slower or faster host needs
no edit to the Makefile. `scripts/cover_gate.sh` — the *second* whole-suite pass
inside `make ci` — carries its own hard-coded `-timeout=20m` for the same
reason.

**A `-timeout` is not a cost budget.** The per-package *cost* budget is a
separate concern with its own instrument, `scripts/pkg_time_budget.sh`, which
`test-short` pipes through on every run (60 s soft, 240 s hard, plus the two
measured overrides above). Raising a timeout must never be read as raising that
budget: a 30 m timeout catches a package that HUNG, the budget catches one that
merely grew.

#### The short layer: `SHORT_TIMEOUT` (rmp #2584)

`SHORT_TIMEOUT` defaults to **30m**. Before it existed, `test-short` passed no
`-timeout` at all, so Go's 600 s default applied and two packages ran close
enough to it that ordinary machine load turned a green gate red.

Measured on the reference host (Apple M4, 10 cores, `darwin/arm64`, go1.26.6)
on **2026-08-20**:

| Observation | Result |
|---|---|
| Commit `147e28e4`, isolated worktree, `go test -race -count=1 ./...` | Whole suite green, exit 0 — but `ok internal/sim 545.794s`, i.e. only **~9% headroom** under the 600 s default, and `ok cypher 433.236s` |
| With the rmp #2488 work in the tree, two consecutive `make ci` runs | `FAIL internal/sim 600.705s` and `FAIL internal/sim 601.675s`, both `panic: test timed out after 10m0s` |
| Was it hung? | **No.** The second run was 3 s into `TestTypeCoverage_NonVacuous` when the alarm fired — cumulative budget exhaustion, not a deadlock |
| Run-to-run variance | `cypher`, which #2488 does not touch, measured **433.2 s / 589.5 s / 576.8 s** across three runs (**+33%**) |
| Parallel-package contention | `internal/sim` takes **487.6 s** in isolation against **545.8 s** inside `go test ./...`, which runs package binaries concurrently |

The variance alone (+33% on an untouched package) exceeds the headroom, so this
is a **headroom** problem, not an attribution problem: no amount of care in
apportioning the cost would make a 600 s ceiling survive it.

`make race` carries the same variable. It is not a gate — `ci` is
`tidy fmt vet build test-short lint cover-gate` — but it runs the same corpus
under the same detector, so it has the same exposure.

**Why 30m.** It is **3.05×** the slowest package measured that actually
completed (`cypher`, 589.5 s) and **1.5×** the `-timeout=20m` the coverage pass
of the same `make ci` gate already applies. That margin is what makes a machine
under sustained competing load reach the same verdict as an idle one, while
still failing a genuinely hung package in bounded time. Check growth against
the measurements above: a package approaching 30m is a cost regression to
investigate, not a reason to raise the value.

#### The deferred layers: `SOAK_TIMEOUT`, `NIGHTLY_TIMEOUT`, `NIGHTLY_CI_TIMEOUT`

The soak layer cannot satisfy the 10-minute default at all.

Measured on an Apple M4 (10 cores, `darwin/arm64`, go1.26.5), in an isolated
worktree so machine contention could not be the explanation:

| Package | Under `-race`, `-tags=soak` | Verdict |
|---|---|---|
| `graph/io/csv` | **800.8 s — passes** | 1.33× the 10-minute default: could never finish inside it, on any machine |
| `cypher` | did not complete at **45 min** | slow, not hung — see below |
| `internal/shapegen` | did not complete at **45 min** | `TestStructured_Hypercube_Soak` still running at 40m19s |
| `internal/sim` | did not complete at **45 min** | `TestSimulator_Soak` still running at 30m57s |

**These packages are slow, not deadlocked.** The decisive check:
`cypher/TestDetachDelete_Hub1M_Soak` builds 1 000 000 nodes and 1 000 000 edges
and then issues one `DETACH DELETE`. With the race detector **off** it passes in
**724.2 s**; with it **on** it had not finished after 44m24s. The goroutine dumps
at timeout show the test bodies executing rather than blocked.

The defaults above are therefore chosen so that the timeout is **not the binding
constraint** — not to encode a known total runtime, because the three long
packages have not been measured to completion under `-race`.

> **Open question, deliberately not decided here.** This layer is specified as
> "minutes-long workloads", yet three packages exceed 45 minutes under `-race`,
> which is nightly-class by that definition. Either those specific tests belong
> in the nightly layer, or the soak target should not use `-race`, or the layer
> definition should change. Moving a test between layers changes what each gate
> means, so it is recorded here rather than settled unilaterally.

Three composite pipeline targets wrap these:

| Target | Purpose |
|---|---|
| `make ci` | Full local gate: tidy + fmt + vet + build + **test-short** + lint + cover-gate |
| `make ci-soak` | Like `ci` but runs **test-soak** instead of test-short |
| `make ci-nightly` | Like `ci` but runs **test-nightly** instead of test-short |

## What the soak layer asserts — and what it does not

**Running `-tags=soak` does not, on its own, certify no-growth or latency
stability.** The reliability instruments in `bench/soak/` fit a linear
regression over a measurement window, and the short layer's window is far too
small to hold one. This is a property of the layer's 60-second budget, not a
defect — but it must never again be read as evidence, because the 2026-08-10
certification did exactly that (rmp #2396): the layer reported `ok` while
neither slope check had evaluated anything.

| Instrument | Asserts in the short layer | Needs a longer window |
|---|---|---|
| `TestGCPause_Stable` | **max GC pause < 200 ms** — a ceiling needs no regression, so it asserts from a single sample | pause-slope regression |
| `TestNoGrowth_HeapFDGoroutine` | nothing; it exercises the workload and writes the CSV | heap / goroutine / fd slopes |
| `TestLatencyP99_Stable` | that round-trips succeeded at all (zero successes is an error) | p99-over-time slope |
| `TestBoltSoak_60s`, `TestBoltCypherMixed_Smoke`, `TestCypherRW_Analytics_Smoke` | success/failure counts, cap errors, goroutine deltas | — |

A slope check that cannot run **skips** rather than passing, so `go test -v`
shows `--- SKIP` and names the setting that would make it assert. Run the soak
layer with **`-v`**: without it `go test` prints only `ok` for the package, and
Go shows neither the skip nor its reason — so a non-verbose soak run still
cannot be read as evidence that anything was asserted. Skipping is
allowed *only* in the short layer at its default window: under `SOAK_FULL=1`,
`GOGRAPH_NIGHTLY=1`, or any explicit `SOAK_*` window override, too few samples
is a **failure**, because those configurations assert that the criterion will be
evaluated. The floor is `minRegressionPoints` = 6 samples
(`bench/soak/soakenv_test.go`): ordinary least squares spends two points on the
fit itself, so two points regress with zero residual degrees of freedom and
three leave one, while six leave four — the smallest count at which one
anomalous sample cannot dictate the slope.

Intermediate windows that make each slope check assert without paying for the
full multi-hour variant:

```bash
# Heap / fd / goroutine growth — 9 samples in ~95 s.
SOAK_NOGROWTH_MEASURE=90s go test -tags=soak -count=1 -v \
  -run TestNoGrowth_HeapFDGoroutine ./bench/soak/

# p99 stability — 10 windows, 8 post-warm-up, in ~100 s.
SOAK_P99_DURATION=100s SOAK_P99_WINDOW=10s go test -tags=soak -count=1 -v \
  -run TestLatencyP99_Stable ./bench/soak/

# GC pause slope — 12 samples in ~60 s.
SOAK_GCPAUSE_MEASURE=60s go test -tags=soak -count=1 -v \
  -run TestGCPause_Stable ./bench/soak/
```

The sample *interval* is deliberately not overridable. Shrinking it is the one
adjustment that manufactures samples without lengthening the observation, which
would convert the regression into a noise detector over a handful of adjacent
points. The window is what gets extended.

The release-grade variants remain `SOAK_FULL=1` (5 min warm-up + 55 min
measurement for no-growth, ~330 samples; 4 h at 64 goroutines for p99).

## Wall clock is not a short-layer instrument

The short layer is not a quiet machine. `make ci` runs `go test -race ./...`,
which executes package binaries concurrently, so a wall-clock measurement taken
there reads machine load as much as it reads the code.

**A last/first ratio is not a defence against this.** It cancels load that is
*constant* across the measurement, and `make ci` load is not constant — sibling
packages start and finish during a run. Measured on a 10-core `darwin/arm64`
host under `-race`, against the *flat* (post-fix) delete path of
`cypher/delete_scaling_test.go`, with `yes > /dev/null` workers as load:

| Load regime | Wall per cycle, first … last | Wall last/first | CPU last/first |
|---|---|---|---|
| idle | 239 ms … 240 ms | 1.00× | 1.03× |
| 300 workers, constant | 7.80 s … 9.10 s | 1.17× | 0.99× |
| 300 workers, ramping up during the run | 620 ms … 9.91 s | **15.98×** | 1.00× |
| idle at the first cycle, saturated at the last | 239 ms … 8.59 s | **35.88×** | 1.50× |

The defect these gates gate against moves the same statistic by 5.2×. Wall
clock in the short layer therefore carries up to 35.9× of noise against 5.2× of
signal, and it duly failed `make ci` on a flat engine.

An absolute **ceiling** — the shape recommended above, because it needs no
window — does not rescue the measurement. To survive constant saturation it
would have to sit above 9.10 s, while the regression it guards costs about
1.2 s on an idle machine: it would be blinder than the ratio, not safer. A
ceiling is the right short-layer shape only for a quantity the machine's load
cannot inflate.

### Resolution (rmp #2589): allocation volume is the instrument these gates needed

The delete-scaling gates no longer measure time at all, and therefore no longer
need `RequireQuietMachine`. They assert on **allocation volume**, which returned
all three to asserting in `test-short` on every push — strictly more coverage than
the serial phase gave them, and `cypher` left `TIMING_PKGS` entirely.

Allocation was previously rejected for these gates on a precise ground, recorded in
#2589: it is load-invariant here (mallocs differed 0.5% across a 35.9× wall
inflation) but *"nothing establishes that the pre-fix O(k·n) Mapper.Walk allocated
in proportion to the nodes it scanned. An oracle whose power against the actual
defect is unknown is cover, not a gate."* Invariance was established; **power was
not**.

The missing experiment was then run, using the method #2572 had supplied — model
the defect and measure whether the instrument sees it:

| workload | per-cycle allocated bytes, last/first |
|---|---|
| flat (healthy, fixed work per cycle) | **0.87×** |
| degrading (6× more work in the last cycle) | **8.66×** |

A 10× separation, with the existing 2.5× threshold sitting between them — 2.9× of
headroom below and 3.5× of margin above. Against the CPU ratio, which read 2.90× on
the *flat* workload (a false red) and 1.52× on the *degrading* one (a false pass).
The threshold did not change; only the instrument did.

**The general lesson.** When a load-invariant instrument is rejected for unknown
power, that is a missing measurement, not a dead end. Build a control that models
the defect and measure the separation.

### Correction (rmp #2517, 2026-08-25): process CPU time is NOT load-invariant

The rule below used to read "in the short layer, assert on an instrument load
cannot move — process CPU time is the load-invariant analogue of wall time",
and quoted a worst-case CPU noise of 1.50× from the table above. **That rule was
wrong, and following it is what produced rmp #2517 and #2589.**

Measured on the same flat engine under `make ci`:

| Test | Under `make ci` | In isolation |
|---|---|---|
| `TestDetachDeleteDoesNotDegradeAcrossCycles` | **2.90×** CPU ratio against a 2.5× limit; per-cycle 852 ms → 2.48 s | **0.94×**; per-cycle 19.3, 17.8, 18.5, 18.9, 18.2, 18.1 ms |

The cycles ran 45–130× slower under the gate, and 2.90× is well past the 1.50×
the table records as CPU's worst case — that figure was measured against
synthetic `yes` workers, not against sibling Go test binaries under `-race`,
which contend for cache, TLB, and the scheduler in ways that charge real CPU to
the measured goroutine. Its sibling power control fails the mirror way: contention
*compresses* the ratio it must exceed, so the control stops firing (#2589).

Contention inflates CPU time. There is no cheap load-invariant proxy for a
duration; the honest options are a genuinely invariant *different* quantity
(allocation counts, operation counts, a fitted complexity exponent) or a quiet
machine.

The rules this yields:

- **Assert on a duration, a rate, or a ratio of them only where the machine is
  quiet.** Two places qualify: the soak layer, and the `test-timing` phase
  described below.
- **In the short layer, prefer a quantity that is invariant by construction** —
  allocation counts, operation counts, fitted exponents — and only where its
  power against the actual defect is established. `bench/cyclicjoin` is the
  worked example: its per-point win is asserted in allocations (2.7×–27× margin,
  run-to-run spread below 0.01%) while its wall-clock arm is only a coarse guard.
- **Where no invariant quantity exists, guard the timing assertion with
  `testlayers.RequireQuietMachine`** rather than widening its tolerance. For
  roughly a third of the audited assertions — the plain "elapsed vs a constant"
  hang detectors — the duration *is* the claim, and there is nothing to substitute.

### `RequireQuietMachine` and the `test-timing` phase

`testlayers.RequireQuietMachine(tb, detail)` skips a timing assertion when
`GOGRAPH_PARALLEL_SUITE` is set. `test-short` and `cover-gate` set it, because
both run packages concurrently; `test-timing` deliberately does not.

**The default is to ASSERT.** The variable's presence is what causes a skip, so a
single-package run — `go test ./cypher/`, or a `-run` filter while investigating —
still checks the gate. No gate silently disappears from a developer's own runs.

**Nothing moves out of the pre-push gate.** `make ci` invokes `test-timing` as its
own phase, which re-runs exactly the guarded gates serially (`-p 1`) and without
the variable, so every one of them still gates every push — in the phase where the
measurement means something. Measured cost: **59 s** for the gates guarded so far,
against 193 s for the `cypher` package alone under `-race` in the same `make ci`.

`TIMING_PKGS` and `TIMING_RUN` in the `Makefile` list only the gates actually
guarded today. The complete inventory of affected assertions — 39 across 12
packages, with the measurement that motivated each — is
[`short-layer-wallclock-audit.md`](short-layer-wallclock-audit.md).

The skip is **loud by contract**: every caller passes the quantity it was about to
assert on, and the message names `test-timing` as the place the assertion still
runs, so a skipped gate can never be mistaken for a passing one. This is the same
contract `RequireUninstrumented` carries for coverage instrumentation, and the two
compose — `bench/mvccwrite`'s instrument controls call both, because the same
precondition is defeated by two different causes.

- **A test that measures process CPU must not call `t.Parallel()`.** `getrusage`
  is process-scoped, so a concurrent sibling is charged to the measurement. Go
  runs non-parallel top-level tests to completion before resuming any test that
  called `t.Parallel()`, which is what makes the measurement attributable. Load
  from *other packages* is irrelevant: that is a different process.
- **`runtime/metrics` `/cpu/classes/*` is not CPU time** for this purpose. It is
  derived from `GOMAXPROCS` × wall-clock accounting, so a goroutine descheduled
  by the OS still accrues it. In the very run where `getrusage` moved 1.50×,
  `/cpu/classes/user:cpu-seconds` moved 50.2× — it is wall clock wearing a CPU
  name, and substituting it would look like a fix while changing nothing.
- **A load-invariant instrument still needs proven power.** Allocation counts
  are perfectly invariant here (0.5% apart across a 35.9× wall inflation) but
  nothing establishes that the regression allocated in proportion to the work it
  did, so they are not asserted on. Where the defective build cannot be rebuilt,
  ship a control that drives a deliberately degrading workload and requires the
  gate to *fire*.

The delete-scaling gates are split accordingly:

| Test | Layer | Asserts |
|---|---|---|
| `TestDeleteDoesNotDegradeAcrossCycles`, `TestDetachDeleteDoesNotDegradeAcrossCycles` | short | last/first **ALLOCATION** ratio ≤ 2.5× (rmp #2400 / #2418, instrument changed by #2589). Measures 0.97× and 0.88×; **no guard needed** — see below |
| `TestDeleteCycleGateDetectsDegradation` | short | that the 2.5× gate still **fires** on a workload engineered to degrade: allocation ratio **8.84×** against the 2.5× limit |
| `TestDeleteWallTimeDoesNotDegradeAcrossCycles`, `TestDetachDeleteWallTimeDoesNotDegradeAcrossCycles` | soak | last/first **wall** ratio ≤ 2.5×, on a quiet machine |
| `TestSingleStatementDeleteOfNinetyThousandNodes` | soak | an absolute 10 s budget for a 90 000-node single-statement delete |

## Sample invocations

```bash
# Short layer only (the default local run) — via make or directly.
make test-short
go test ./... -count=1 -race

# Short + soak.
make test-soak
go test -tags=soak ./... -count=1 -race
SOAK_FULL=1 go test ./... -count=1 -race

# Short + soak + nightly.
make test-nightly
go test -tags=nightly ./... -count=1 -race
GOGRAPH_NIGHTLY=1 go test ./... -count=1 -race
```

## How each layer is run

There is no per-push or scheduled CI; the only GitHub Actions workflow is
`.github/workflows/release.yml` (tag-triggered). Each layer is run locally
by developer discipline before pushing or tagging:

| Layer | When to run | Command |
|---|---|---|
| **short** | before every push (also folded into `make ci`) | `make test-short` |
| **soak** | periodically and before a major release | dedicated soak harness / `SOAK_FULL=1 make soak` |
| **nightly** | periodically | `make test-nightly` |

The nightly target can also run benchmarks with `-cpuprofile` and
`-memprofile` so nightly regressions can be investigated with
`go tool pprof`.

## Relationship to the existing `stress` tag

The `internal/stress/` package is gated by `//go:build stress` and was
introduced before the three-layer scheme. It runs a short concurrent
workload under `-race` to catch scheduler-dependent issues, run locally
via its `stress` build tag. The `stress` tag is considered part of the
**soak family**: anything gated by `stress` belongs conceptually to the
soak layer, even though it uses a distinct tag for historical reasons.
There is no plan to rename it; new soak-layer work should use the
`soak` tag or `SOAK_FULL=1` instead.

The longer-running 4-hour Bolt soak in `bench/soak/` continues to use
its own `soakfull` tag. Like `stress`, it is considered part of the
soak family.

The `soakfull` tag also gates the two multi-hour DST endurance scenarios
in `internal/sim/phase4_long_running_soak_test.go` (2,000,000 and
1,000,000 ticks). Under the race detector these alone exceed the 600 s
`go test` default timeout, so they are excluded from the
`make test-nightly-ci` subset — which passes only `-tags=soak,nightly`
— while the full `make test-nightly` target passes
`-tags=soak,nightly,soakfull` and runs them. Excluding them loses no
scenario coverage: the `ScenarioLongRunning` run-path is exercised by the
short-layer `TestCatalogue_SmokeSubsetRunsClean` (part of `make ci`) and at
a 2,000-tick budget by the soak-layer `TestCatalogue_EachScenarioRunsClean`;
the endurance budget is a periodic heap/goroutine-stability watch, not a
release gate. This is the per-test analogue of the package-level
`search/extern` exclusion in `NIGHTLY_CI_PKGS`.

## Helpers reference

`github.com/FlavioCFOliveira/GoGraph/internal/testlayers` exposes the runtime API. The package is
internal: it is consumed from elsewhere in this module and is not part
of GoGraph's public surface.

| Symbol | Kind | Description |
|---|---|---|
| `RequireSoak(tb testing.TB)` | function | skips `tb` unless the soak layer is active |
| `RequireNightly(tb testing.TB)` | function | skips `tb` unless the nightly layer is active |
| `IsSoak` | constant `bool` | compile-time flag, true under `-tags=soak` |
| `IsNightly` | constant `bool` | compile-time flag, true under `-tags=nightly` |
| `RequireUninstrumented(tb, detail)` | function | skips `tb` when built with coverage instrumentation, which compresses the arms a concurrency-**effect control** compares. For controls only (rmp #2319) |
| `Instrumented() bool` | function | reports whether coverage instrumentation is active, for a caller that adjusts rather than skips |
| `RequireQuietMachine(tb, detail)` | function | skips `tb` when `GOGRAPH_PARALLEL_SUITE` is set, i.e. when packages are being tested in parallel and a **timing** assertion would measure machine load. Defaults to ASSERTING; the assertion still runs in `make test-timing` (rmp #2517) |
| `InParallelSuite() bool` | function | reports whether this is a parallel whole-suite run, for a test with BOTH load-independent and timing arms that must guard only the timing one — `bench/cyclicjoin` is the worked example |

`detail` is mandatory on both `Require*` guards above and must name the quantity
the caller was about to assert on. That is what makes a skip self-explaining, so a
skipped gate can never be mistaken for a passing one.

The two constants are useful when a test must branch its body on
layer membership rather than skip wholesale, for example to enlarge a
workload size from "hundreds of nodes" to "millions of nodes" under
the deeper layer.
