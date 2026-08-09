# Production-readiness certification — GoGraph module (second cycle, 2026-08-09)

**Date:** 2026-08-09 · **Entry head:** `9cd0d01f` · **Exit head:** see §5 · Apple M4 (10 cores), `darwin/arm64`, go1.26.5

This cycle continues sprint 337. The previous cycle certified the module within an envelope and
left one item explicitly open: **only 2 of the 37 examples could produce a `pprof` profile**, so 35
workloads — persistence, recovery, traversal, search, interchange and Bolt among them — could not
be attributed to a call site at all. That item was the whole reason the cycle's largest performance
finding had been findable: example 26 happened to expose the flag.

This cycle closed that gap and then used it.

---

## Verdict

**Two tasks, both closed. One infrastructure gap removed and one real defect found through it.**

| Gate | Result |
|---|---|
| `make ci` (tidy, fmt, vet, build, `-race ./...`, lint, coverage) | see §4 |
| openCypher TCK execution | baseline `3897` unchanged |
| Examples: build | **37 / 37** |
| Examples: produce a valid `cpu.pprof` + `heap.pprof` | **42 / 42 runs** (37 examples; example 24 counted once per subcommand) |
| Deterministic facts with profiling off vs on | byte-identical |

The headline is not the fix; it is that **the fix was invisible before this cycle's first task
existed**. A defect worth 29% of a workload's total allocation sat in the query planner and no gate,
benchmark or test in the module could see it, because the one instrument that could was not
installed on the workload that exhibited it.

---

## 1. Every example can now produce a profile (rmp #2377)

All 37 examples accept `-profile-dir` (writing `cpu.pprof` and `heap.pprof`) and `-trace` (writing a
`runtime/trace`), on identical terms.

The contract is supplied by one shared package, `examples/internal/exprof`, rather than by 37 copies
of example 26's block. That was the maintainer's decision when the alternative was put to them, and
the reason is that the acceptance criterion — *"the flag's help text and semantics are identical
across all of them"* — is then guaranteed by construction instead of by 37 careful copies. The change
**removed 266 lines** while adding the capability to 35 more programs.

`exprof.Config.Run` makes two hazards unrepresentable rather than merely documented:

- **The profilers stop even when the workload fails.** Examples end a failed run with `log.Fatal`,
  and `os.Exit` does not run deferred calls, so a `defer` would truncate the profile on exactly the
  runs worth profiling. Example 26 had this bug for real: its `columnarExercise` error path called
  `log.Fatal` without stopping the CPU profile, while its three sibling paths did.
- **A setup failure is fail-fast.** If the profilers cannot start, the workload does not run at all.
  The operator asked for evidence; spending the whole runtime and only then reporting that no
  profile exists is fail-silent in the way that matters.

Five examples were not mechanical and were done by hand: **26** drives four batteries from one
closure; **37** had its own copy removed; **24** binds on each of its four subcommand `FlagSet`s, so
all six subcommands profile; **25** covers the whole server lifetime through a `defer` that is safe
because its `run` returns an exit code rather than calling `os.Exit`; and **17**'s crash child is
deliberately **not** profiled — `SIGKILL` runs no teardown, so the artefact would be truncated
garbage, and the parent's re-exec never forwards the flag.

**Verified by running them, not by reading them.** A sweep drove all 37 with the flag and checked
that each artefact decompresses and carries the right sample types (`nanoseconds`/`samples` for CPU,
`inuse_space` for heap). `internal/docscheck.TestEveryExampleCanProduceAProfile` now gates the
property; it was injection-validated by removing `exprof.Bind` from example 08 and observing the
failure.

### A defect this surfaced in example 24

`24 query -d dir "MATCH ..." -profile-dir /tmp/p` returned exit 0 and wrote no profile. Go's `flag`
package stops parsing at the first non-flag argument, so the trailing flag was a positional — and
the example discarded extra positionals in silence. The required `-d` fails loudly in the same
position, so the two behaved inconsistently. Extra arguments are now named and refused (exit 2).
This changed documented, tested behaviour and was therefore put to the maintainer first.

---

## 2. What the new profiles found: the planner's parallel-scan gate (rmp #2380)

Reading the **new** heap profile of `examples/35_mvcc_mixed_workload` at `-sample_index=alloc_space`:

    47.87%  roaring/v2.(*arrayContainer).clone      6262.90MB of 13084.20MB

Reached, 100% of it, through `label.Index.Intersect` ← `lpg.LabelBitmapAsOf` ←
`cypher.lpgLabelResolver.ResolveLabelBitmap` ← `cypher.newLabelWalker` ←
`cypher.tryBuildParallelScanProject`.

**The mechanism.** The planner decides whether a single-label leaf is worth running in parallel by
comparing the label's cardinality against `DefaultParallelScanThreshold` (50 000, strict `>`). It
obtained that cardinality by materialising the label's bitmap — and materialising it *clones* the
label's live set. Example 35 has **3 000 nodes**, so the gate can never admit: every clone was
discarded. Against the 50 MB the serial scans it declined to parallelise actually spent, **the gate
cost about 130× the work it was deciding about.**

**The module had already identified this exact anti-pattern one branch over.** For the *multi-label*
sibling, `exactIntersectionCardinality` states the rule — *"a gate must cost less than the decision
it informs"* — and records `+85.8% B/op on a break-even query the gate went on to DECLINE`. The
single-label branch never got the same treatment.

**The fix** consults the zero-allocation `ResolveLabelCount` first and declines before materialising
anything. Every piece already existed: `lpg.LabelCountExact`, `label.Index.Count`, and the
`labelCounter` interface. It abstains — falling through to today's path — whenever the count is not
exact for the snapshot, so the MVCC-correct behaviour is unchanged.

### Measured, interleaved, three rounds, where the finding was made

| | before | after |
|---|---|---|
| total allocation | 12.75 – 13.01 GB | **9.06 – 9.15 GB** (−29%) |
| roaring clone | 6.18 – 6.36 GB (47.5–47.9%) | **777 – 789 MB** (8.4–8.5%) (−87.5%) |

And it converts into throughput. Arms never overlap between conditions in any phase:

| phase (ops/s, mean of 3) | before | after | change |
|---|---|---|---|
| baseline | 465 629 | 657 296 | **+41.2%** |
| analytics only | 388 488 | 501 938 | **+29.2%** |
| writer only | 451 743 | 599 354 | **+32.7%** |
| analytics + writer | 226 126 | 242 464 | **+7.2%** |

Every deterministic fact the example pins is identical in both arms
(`readers_starved=0`, `phases_measured=4`, `index.created=1`, `analytics.is_long=1`).

> The `verdict.throughput_collapse` telemetry moved 2.0–2.2× → 2.7×. That is a **ratio of two
> numbers that both improved** — the baseline improved more than the contended phase — not a
> regression. The pinned fact `readers_starved` requires `collapse >= 4` and is `0` in both arms.

---

## 3. Method: three instruments lied, and the honest ones corrected them

This is the part worth carrying forward.

**The microbenchmark did not reproduce the finding.** `BenchmarkParallelScanGate` was written to
pin the win and measured, over five interleaved rounds each way: `scan` improved from 91 to **78
allocs/op** (a real, byte-stable 14% drop) but only 1.2% of bytes — and `seek` measured
**byte-identical in both arms**, because that shape never reaches the screen at all. Had the
benchmark been the only evidence, the correct conclusion would have been "this barely matters". The
example is what showed the 29%. **A microbenchmark that does not reproduce the profile's finding is
evidence about the microbenchmark.**

**A probe with the wrong snapshot manufactured a false all-clear.** The regression test was flaky at
**7 failures in 100**. Probing `ResolveLabelCount` directly reported `exact=true, count=4096` on
every attempt, which appeared to refute the obvious explanation — so it was discarded. It was wrong:
the probe passed a **nil** snapshot, and `labelBitmapNeedsFilter(nil)` short-circuits to "no
filtering needed" before it ever consults the delta counters. The engine pins a *real* snapshot,
where live label history makes the count inexact and the screen correctly abstains. Counters placed
at each guard proved the branch *was* reached and the screen simply declined to fire — which is
correct behaviour, and the wait belongs in the test. With the graph quiesced first: **0 failures in
300**.

**"It passed three times" is not a fix.** The full package failed once, then passed three
consecutive runs. Running the test alone 100 times reproduced it at 7%. A flake that disappears when
you look at it has not gone away.

Both regression tests are injection-validated: 5 failures in 5 on the pre-fix build with the exact
diagnostic, 0 in 5 restored. The suite carries a paired positive case (the gate must still admit
above the threshold) so it cannot pass vacuously by disabling parallel scan, and a result-equivalence
case, because the gate selects an execution strategy and never a result.

---

## 4. Gate evidence

`make ci` was run to completion twice, once per task. Both are read from the `MAKE_CI_EXIT` line
written **inside** the log: the harness's completion notification has reported "exit code 0" over a
red gate on three previous cycles, because the shell's status is that of the trailing `echo`, not of
`make`.

| Run | After | Result |
|---|---|---|
| 1 | #2377 | `MAKE_CI_EXIT=0` · coverage aggregate **87.0%** (gate ≥ 85%), every package ≥ 75% |
| 2 | #2380 | `MAKE_CI_EXIT=0` · coverage aggregate **87.0%** · `cypher` 285.8 s and `cypher/tck` 68.4 s under `-race` |

Both logs were also grepped independently for `FAIL`, `--- FAIL`, `DATA RACE`, `panic:`,
`make: ***` and `Error N`: **0 matches in each**. `golangci-lint` reported `0 issues`. The openCypher
TCK baseline constant is unchanged at **3897** and `cypher/tck` passed in both runs.

Per-task validation beyond the gate:

- **#2377** — 42 example runs, every one producing a `cpu.pprof` and `heap.pprof` that decompress
  and carry the right sample types; deterministic facts diffed byte-identical with the flag unset;
  the new `docscheck` gate injection-validated.
- **#2380** — three regression tests, injection-validated at **5 failures in 5** on the pre-fix
  build with the exact diagnostic and **0 in 5** restored; the primary test additionally run
  `-count=100` clean after the quiescence fix (it was 7-in-100 flaky before).

---

## 5. Commits

| Commit | Task | What |
|---|---|---|
| `1fc53bc5` | #2377 | every example can produce a pprof profile; shared `examples/internal/exprof` |
| *(this commit)* | #2380 | screen the parallel-scan gate on the zero-alloc count; this document |

---

## 6. What this cycle did not do

- **The soak and nightly layers were not run**, so nothing here certifies them.
- **No fresh hostile/security audit** (last covered by the 2026-07-26 and 2026-07-31 cycles).
- **The examples were driven at their deterministic defaults**, not at the elevated scales the
  previous cycle swept, except example 26 (reduced to 20 000 users to fit a verification budget).
  So the profiles collected here describe default-scale behaviour; a scale sweep would attribute
  different costs.
- **Node memory remains 378–423 B/node against Neo4j's 128 and Memgraph's 204.**
  `docs/design-node-memory.md` records the analysis and concludes the in-memory model must split;
  that is a representation change requiring the maintainer's agreement and was not attempted.
- **`runtime/trace` is now available on all 37 examples but was not exercised.** Neither was
  `GODEBUG=gctrace=1`, per-example coverage attribution via `go build -cover` + `GOCOVERDIR`, nor
  rendered flame graphs; profiles were read as `-top`/`-peek` tables.
