# Raw data — `v0.13.0` vs `v0.14.0`, 2026-09-06

Backing evidence for [`../v0130-vs-v0140-2026-09-06.md`](../v0130-vs-v0140-2026-09-06.md).

**Allocations only.** No wall-clock figure is reported in the analysis, because the host
carried a load average of 2.86–19.73 throughout — substantially caused by Spotlight indexing
the two 1.9 GiB worktrees the experiment itself created. The `sec/op` columns are present in
the raw result files because `go test -benchmem` emits them; **they are not evidence and were
not used.**

| File | What it is |
|---|---|
| `alloc-classification-full.txt` | every one of the 171 benchmarks with its verdict |
| `alloc-verdicts.txt` | the analysis: changed / unchanged / unreliable, on `allocs/op` |
| `alloc-verdicts-detailed.txt` | same, with every observed sample listed |
| `noise-floor.txt` | `B` vs `B2` — the same byte-identical binary against itself |
| `allocs.py.txt` | the classifier that produced both |
| `binary-sha256.txt` | proves `B` == `B2`, and `store_wal_A` == `store_wal_B` |
| `build-exits.log` | 15 builds, exit code of each |
| `plandump-v0130.txt`, `plandump-v0140.txt` | physical plans **and** full sorted results, both trees — the attribution for the reorder gain |
| `zzport_reorder_bench_test.go.txt` | the portable harness, placed identically in both worktrees |
| `run_ab2.sh` | the interleaved runner |
| `exits.log` | every invocation's exit code, read from here, never from a pipeline |
| `loadavg.log` | `uptime` bracketed before **and** after every invocation |
| `progress.log` | per-round start/complete with the load at each boundary |
| `cypher*.txt`, `store_*.txt` | reduced-scope run: round 1 complete, round 2 partial |
| `run1-larger-scope/` | first attempt, larger selector: round 1 complete, round 2 partial |
| `benchnames-common.txt` | the 587 benchmark functions present in both trees |
| `benchnames-added-by-v0140.txt` | the 10 added by this release |

**Arms.** `A` = `v0.13.0` (`b439283e`), `B` = `v0.14.0` (`94602f46` + the one uncommitted
doc-comment change), `B2` = a second independent build of the same `v0.14.0` tree, which Go
produces byte-identically — so `B` vs `B2` is the noise floor.

**Build mode is stated because allocation counts are not build-invariant:** plain build,
**no `-race`**, `-trimpath`.

**Two runs, both kept.** The first attempt used a larger benchmark selection and was stopped
when the host load made its pacing open-ended; its completed round 1 is retained in
`run1-larger-scope/` and its samples are pooled into the analysis, because the binaries and
the workloads are the same. The second run used the reduced selection described in the
report.
