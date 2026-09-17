# Campaign record — Cypher statement parsing

**Sprint 362, "Performance laboratory 20260915".** Opened and closed 2026-09-16.
Branch `feature/362-performance-laboratory-20260915`.

**Objective, as the user set it:** make the parsing of Cypher statements ultra
efficient and ultra fast, driving the work with profiling tools over the
project's own examples, and taking informed hypotheses from how Neo4j, Memgraph
and PostgreSQL solve the same problem.

Every number below was measured in this project, on this host, in this campaign.
Nothing is carried from another engine, another version, or memory.

## What shipped

| Commit | Change | Measured effect |
|---|---|---|
| `4fa1689b` | The parsing laboratory: an 83-statement corpus harvested from four examples, per-stage benchmarks, a one-command reproduction, and a replica that establishes the noise floor | baseline, not a gain |
| `3ab42f2b` | Round-1 profiling: eight profiles, `gctrace`, the benchstat series, and the end-to-end example run | evidence, not a gain |
| `be5019f4` | Prior art from Neo4j, Memgraph and PostgreSQL, read in source at pinned commits | hypotheses, not a gain |
| `49cc8bb5` | The ranking, on three bounded spikes | decisions, not a gain |
| **`cb55c3c9`** | **`StripLiterals` classifies clause keywords without allocating** | corpus **−47.83%** sec/op, **148 → 32** allocs/op; the twelve quoted statements **−52.02%** |
| **`1630e9f6`** | **Two-stage parsing: bail on the accepting path, full diagnostics on retry** | accepting path **−6.01%**, allocations **+0.31%**; error path **+57.4%**, recorded as a cost |
| `2944b184` | Restored the branch's lint gate, red since `3ab42f2b` | gate green |
| **`6ca705e3`** | **The plan cache sharded 16 ways**, plus the first hit-rate measurement this project has ever taken | 32 keys at ten cores **119.10 → 42.42 ns, −64.38%** |

**openCypher TCK: 3897/3897 throughout, `tckExecutionBaseline` never lowered**, re-run
by the coordinator rather than accepted from an executor. `-race` clean, `goleak`
clean, `golangci-lint` 0 issues.

## What the campaign learned that it did not set out to learn

**1. The headline share was not the opportunity — it was the floor.** Round 1
measured the ANTLR `Script` call at **54.9%** of the front end. Attribution then
showed that **69.2% of front-end CPU belongs to the ANTLR runtime** and the
allocation it drives, 16.4% to generated code, and only **14.3% to GoGraph's own
hand-written code**. Inside the parse stage, 53.1% of self time is runtime and
3.0% is generated. The campaign's reachable ceiling was therefore far below its
headline from the start.

**2. The prior art's best hypothesis was wrong, and measurement caught it.**
Neo4j runs SLL prediction with a bail strategy and retries under LL; the Go
runtime exports both APIs; GoGraph ran full LL and never set it. That looked like
the campaign's largest structural lever. Measured, **`PredictionModeSLL` bought
nothing**: −0.49%, p=0.529, below the 1.05% noise floor, refuted on two
independent runs. **The entire −6.46% belonged to the error strategy**, and
`LL + Bail` delivers it at parity with `SLL + Bail` **without touching prediction
semantics** — which is strictly safer, because the retry then only has to
reproduce error *messages*, never to rescue a valid input that SLL wrongly
rejected.

**3. The largest measured effect was not in `cypher/parser` at all.** The
campaign set out to optimise the front end. The biggest number it found was one
layer above: `(*planCache).get` held a single mutex across the map lookup, the
LRU promotion and two counters, on a path every statement takes. **Ten cores
delivered 16% of one core's throughput.** `CLAUDE.md` classifies that as a defect
against the extreme-concurrency mandate rather than a missed optimisation. It was
put to the user, who chose sharding, and it shipped.

**4. The campaign's own ranking rationale was refuted by its last measurement.**
The ranking ordered the keyword fold ahead of two-stage parsing on a **97.62%
hit-rate break-even**. The first hit-rate measurement this project has ever taken
found **95.51%, 45.16%, 0.00% and 0.00%** across the four corpus examples — **no
example reaches the break-even and three are far below it**, so two-stage parsing
wins by 1.9x to 41.9x. Both had already landed, so nothing needed undoing. What
is refuted is the reasoning, and the lesson is that **miss-path cost dominates
every workload this project has measured**.

## Hypotheses refuted, with the number that refuted each

| Hypothesis | Refuted by |
|---|---|
| `PredictionModeSLL` is the largest available lever | −0.49%, p=0.529, twice, below a 1.05% floor |
| A raw-text index in front of the plan cache would pay | max gain 202 ns; worst case converts a 72 ns warm prepare into a 20,438 ns cold one — **100x asymmetry** |
| A no-backtracking scanner gate transfers from PostgreSQL | ANTLR emits an ATN, not a backtracking DFA — **ceiling 0 ns** |
| The single-core sharding cost is false sharing *(the coordinator's own hypothesis)* | an **unpadded** build is indistinguishable at one core, p=0.142; the cost is the split LRU splice, p=0.093 against the unsharded baseline |
| A high plan-cache hit rate justified ordering the warm path first | measured 95.51%, 45.16%, 0.00%, 0.00% against a 97.62% break-even |
| Hoisting literals in `RETURN`/`WITH` would pay | at most 2 of 83 statements affected, both already resolving under their own raw text |

Two instruments were also found at fault and corrected: **`pprof -show=<pkg>`
cannot give a flat per-package sum** — it redistributes a dropped node's cost into
its nearest retained ancestor, and inflated a split **3x** — and **`pprof`
defaults to `-nodefraction=0.005`**, which hid **4.66 s of 20.70 s** on this
profile.

## What was deliberately not done

* **Replacing or bypassing ANTLR.** No prior art exists for a hand-written Cypher
  parser: Neo4j, Memgraph and Kùzu all use ANTLR4, Apache AGE uses flex+bison,
  libcypher-parser a PEG — and **Neo4j removed its recursive-descent JavaCC
  parser in 2024 in favour of ANTLR**.
* **Neo4j's thin tokens.** Closed to Go: `setTokenFactory` is unexported *as a
  method of the `TokenSource` interface*, so no type outside package `antlr` can
  implement it.
* **Fusing the AST build into the parse.** Its ceiling is unmeasured, its memory
  half is closed in Go (`ParserRuleContext.children` is unexported), and the
  `Visit` stage's noise floor is 11.97% median / 34.88% max, so a ceiling below
  ~12% could not be distinguished from noise even if measured.
* **256 shards**, which reaches the headline ceiling (−83.77%). A second ladder
  priced it: at 75% load the hit rate falls from 99.5% at 16 shards to 66.0% at
  256, and one lost point costs ~18 ns per lookup against a measured 1.8 µs miss
  — two to three times what doubling the count saves.

## Left in the backlog, not worked

| # | Item |
|---|---|
| #2847 | `runReadPrefix` discards the auto-parameters `runRead` merges, and its own drift guard is blind to it because both its queries are quote-free |
| #2848 | An index created before a raw `lpg` seed stays empty: a seek returns 0 rows where a label scan returns 100 |
| #2853 | The two-stage retry re-lexes from scratch; reusing the filled stream would remove most of the +57.4% error-path cost |
| #2854 | `create_index.go:318-320` claims a barrier the plan-cache refill path does not take — pre-existing and bounded today |
| #2855 | Three plan-cache tests that do not test what their names claim |

## Limits of what was established

* The hit rates are **short example runs, not a long-lived server**. They do not
  describe a connection pool reissuing the same statements for hours, which is
  the regime `DefaultPlanCacheCapacity`'s own comment assumes. What they
  establish is that **that regime has never been measured anywhere in this
  project**.
* **DDL never reaches `cypher/parser`** (`Engine.Run` diverts at `ir.IsDDL`), so
  every parsing claim here **excludes DDL**.
* The front end's GC attribution (1.07x its own CPU again in background marking)
  is **established by exclusion**, not by a differential.
* Two harnesses report `Full` 11.6% apart because the spike loops the whole
  corpus per op with no per-statement warm-up; **only within-harness deltas were
  used**.
* **This host is never idle.** Every series records `uptime` before and after;
  the envelope is median 2.19, max 3.44, and the series that ran outside it are
  named in their own artefacts rather than hidden.
