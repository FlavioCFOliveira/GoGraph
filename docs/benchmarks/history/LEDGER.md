# Performance history ledger

Each row is one `bench-history.sh` run. The raw numbers live in the
`NNNN__<label>__<commit>.txt` file; the benchstat comparison against the
previous run lives in the matching `.delta.txt`. Fill the **Summary** column
with the one-line outcome of the change (the headline delta from the
`.delta.txt`), so the table reads as a chronological record of gains and
regressions.

| Seq | Date (UTC) | Commit | Label | Summary |
|----:|-----------|--------|-------|---------|
| 0001 | 2026-05-29 | `1634256` | baseline | Reference point. IC1 408µs / 588 KiB / 10794 allocs; IC2 135µs / 3641 allocs; IC9 165µs; IC10 232µs. Graph algos unchanged throughout. |
| 0002 | 2026-05-29 | `1634256-dirty` | opt1-mapper-shardfor-unsafe | **Gain.** Eliminated string interface-boxing in `Mapper.shardFor`. cypher_ldbc geomean **−4.63% time, −14.86% allocs**; IC1 −6.15% time / −18.53% allocs (10794→8794); IC2 −6.18%/−18.35%; IC5 −6.52%; IC9 −4.41%/−18.38%; IC10 −2.80%. Graph-algo guard band flat (no regression). TCK 3897/3897, race-clean. |
