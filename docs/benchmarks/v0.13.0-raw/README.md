# Raw measurement data — v0.13.0 comparative campaign

Every figure in [`../v0.13.0.md`](../v0.13.0.md) traces to a file here.

- `<label>.base.txt` — the `v0.12.0` arm (`f97bbfec`), benchstat-ready.
- `<label>.head.txt` — the `release/0.13.0` arm (`b523878b`), benchstat-ready.
- `<label>.load.log` — the load-average gate at block open and the 1-minute load
  average each round started at.
- `ab_bench.sh` — the interleaved A/B harness that produced them, as patched
  (see §7 of the report for the two defects found in it).
- `block0.log`, `b13.log`, `b3c.log` — driver logs with per-label wall-clock and
  result-line counts.

Compare any label with:

```bash
benchstat <label>.base.txt <label>.head.txt
```

## The `r-*` files — measured on the repaired harness

`r-ladder-readtx`, `r-exec`, `r-columnar` and `r-count` were measured **after** both
harness defects were fixed: benchmark `stderr` now goes to `<label>.stderr.log` instead of
being merged into the data stream, and only strict result lines reach the data files.

Each carries a `<label>.stderr.log` holding what was diverted — `r-ladder-readtx.stderr.log`
has **487** `WARN` lines and `r-count.stderr.log` has **48**, none of which reached the
data. All eight `r-*` data files verify as 0 malformed, 0 `WARN` leaked, matching
base/head counts.

`r-ladder-readtx` supersedes the unusable `ladder-cypher-readtx` below; both are kept, the
older one as evidence of the defect.

## Files that are not clean, and must not be used as-is

- **`ladder-txn.{base,head}.txt`** — 30 of 84 result lines per arm are corrupted by a
  `WARN cypher: …` log line spliced into the result line. Use
  **`ladder-txn-repaired.{base,head}.txt`**, from which those lines are removed. The five
  `BenchmarkWriteScaling_Cypher` rows are unrecoverable.
- **`ladder-cypher-readtx.{base,head}.txt`** — **total loss**: 60 of 60 result lines
  corrupted in *both* arms by the same defect. Retained as evidence only; it carries no
  usable measurement.

Validate before trusting any file:

```bash
grep '^Benchmark' <file> | grep -cvE '^Benchmark\S*[[:space:]]+[0-9]+[[:space:]]+[0-9.]+[[:space:]]+\S+'
```

A non-zero count means result lines were destroyed, and `benchstat` will not tell you.
