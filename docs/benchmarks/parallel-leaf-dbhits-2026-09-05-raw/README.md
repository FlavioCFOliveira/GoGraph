# rmp #2762 — raw A/B/C data for the morsel-parallel leaves' db-hits counter

Three arms, rotated **within** every round so thermal drift and background activity
bias all three equally:

| file prefix | arm | what it is |
|---|---|---|
| `*.base.txt` | `base` | worktree at `e6f6384b` — the pre-#2762 code |
| `*.base2.txt` | `base2` | a separately built copy of the SAME source — the **noise floor** |
| `*.head.txt` | `head` | the working tree, with the per-morsel storage counter |

* `A.*` — `-benchmem -count=1` per round × **5 rounds**, `-cpu=1,4,10`, over
  `BenchmarkParallelScan_CountBig`, `BenchmarkParallelScanProject_Scan{Big,FilterBig}`,
  `BenchmarkParallelAggregate_{MinBig,GroupMinBig}` and
  `BenchmarkParallelLabelScan_LabelledProject` — parallel and serial arms of each
  (36 benchmark lines per round, 180 per arm).
* `B.*` — `BenchmarkParallelAggregate_Concurrent` at **1, 8 and 64 concurrent queries
  on one shared `exec.ParallelGovernor`**, 8 rounds (6 lines per round, 48 per arm).

`benchstat` outputs: `noisefloor_A.txt`, `effect_A.txt`, `noisefloor_B.txt`,
`effect_B.txt`. Read the noise floor FIRST — it produced "significant" verdicts of up
to ±2% between two builds of identical source, which is the bar any claimed effect has
to clear.

`loadavg.log` records the host's load average immediately before each of the 39
invocations; `exits.log` records each one's exit status (all 0). The sweep itself drove
the 1-minute load average to 9.6–10.1 on this 10-core machine and ran niced (NI 5): both
conditions applied identically to all three arms and to the noise floor, which is what
makes the COMPARISON valid rather than the absolute numbers portable.

Driver: `rmp2762-parallel-dbhits-ab.sh` (copied here verbatim).

Environment: Apple M4, 10 cores, macOS 26.5.2 (darwin 25.5.0, arm64), go1.27.1,
**no `-race`** — `-race` changes allocation counts, so the `B/op` and `allocs/op`
figures here are the non-race ones.

Conclusion: **no parallel arm moved significantly**, at any `-cpu` level or any
concurrency level. Every significant `sec/op` verdict landed on a `DisableParallelScan`
serial control, which this change cannot reach; `B/op` and `allocs/op` are unchanged
throughout. Full reading in the rmp #2762 addendum to
`docs/explain-profile-honesty-audit-2026-09-03.md`.
