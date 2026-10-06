# Example 37 — MVCC package coverage (rmp #2934)

Statement coverage of `graph/mvcc`, `graph/lpg` and `cypher` driven by example 37
(`examples/37_mvcc_write_contention`), measured with a coverage-instrumented binary.

## Command

```bash
go build -cover \
  -coverpkg=./graph/mvcc,./graph/lpg,./cypher,./examples/37_mvcc_write_contention \
  -o /tmp/ex37 ./examples/37_mvcc_write_contention
mkdir -p /tmp/ex37cov
GOCOVERDIR=/tmp/ex37cov /tmp/ex37 -ladder-levels 1,8,64
go tool covdata percent -i /tmp/ex37cov
```

The example's own package must be in `-coverpkg`. With only the three engine packages
listed, the binary wrote no counter files (`go tool covdata` reported "no applicable
files found"); adding the main package fixed it.

## Result

Environment: Apple M4, 10 cores, darwin/arm64, go1.27.1, HEAD `aef83046` plus the
phase 6 changes, store directories on a RAM drive, load average 2.91 after the run.

| Package | Phases 1-5 | Phases 1-6 (ladder at 1, 8, 64) |
|---|---:|---:|
| `graph/mvcc` | 65.7% | 69.0% |
| `graph/lpg` | 55.0% | 56.8% |
| `cypher` | 43.0% | 46.6% |
| `examples/37_mvcc_write_contention` | 50.7% | 85.9% |

"Phases 1-5" is the same binary run with `-ladder-levels ""`. Each figure is one run;
coverage of concurrent paths varies with scheduling, so a difference of a few tenths
between runs is expected.
