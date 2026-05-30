# Bolt Soak CI Report

## Run metadata

| Field         | Value                                  |
|---------------|----------------------------------------|
| Date          | 2026-05-30                             |
| Commit        | `b5453b9` (post-#1077 concurrent read+write fix) |
| Platform      | darwin/arm64 (Darwin 25.5.0)           |
| CPU           | Apple M4 (10 logical cores)            |
| Go version    | go1.26.3 darwin/arm64                  |
| Module        | gograph                                |
| Test variant  | `TestBoltSoak_1024_4h` (full soak)     |
| Connections   | 1024 concurrent                        |
| Duration      | 4 hours                                |
| Snapshot rate | every 30 s (479 snapshots captured)    |
| Race detector | not enabled for this 4 h endurance run; race correctness is covered by the short-layer `-race` suite (incl. the #1077 concurrent read+write reproducer and `go test -race ./cypher/... ./store/...`) |

## Full 4-hour / 1024-connection soak — PASS

**Verdict: PASS** — run against the current quartile-based stability gates.

### Throughput

| Metric | Value |
|---|---|
| Successes | 287,747,162 |
| Failures (backpressure) | 46,452 |
| Success rate | 99.98% |
| Duration | 4h 0m 0s |
| Goroutines after shutdown | 6 |

Failures are transient backpressure rejections (`max connections reached`) — tolerated by design.

### Resource stability (479 snapshots over 4 hours)

| Metric | Min | Max | Avg |
|---|---|---|---|
| Heap alloc | 11.2 MB | 38.2 MB | 21.1 MB |
| Goroutines | 1,257 | 2,094 | 1,677 |

### Stability gate — quartile analysis (1.5× threshold)

The gate fails the run when `last-quartile avg > 1.5 × first-quartile avg`
(implemented as `avgLast*2 > avgFirst*3`).

| Gate | First-quartile avg | Last-quartile avg | Ratio | Result |
|---|---|---|---|---|
| Heap (no monotonic growth) | 20.5 MB | 20.9 MB | 1.02× | **PASS** |
| Goroutines (no monotonic growth) | 1,681 | 1,667 | 0.99× | **PASS** |

Both ratios are far below the 1.5× threshold; heap is essentially flat and the
goroutine last-quartile average is *lower* than the first — no upward trend.

### Other gates

| Gate | Result |
|---|---|
| Zero panics / crashes | PASS |
| `goleak.VerifyNone` after shutdown | PASS (drained to 6 goroutines) |
| Server shutdown cleanly within 10 s | PASS |
| Successes > 0 | PASS (287M successes) |
| Zero data races | covered by the short-layer `-race` suite, not this 4 h run |

### Notes

- Heap oscillation (11.2–38.2 MB) under 1024 concurrent connections is expected: GC cycles under
  high-concurrency load produce swings between collection and allocation.
- Goroutine count (1,257–2,094) reflects the steady-state pool of up to 1024 active connection
  handlers plus server internals; it does not grow over the run and drains to 6 on shutdown.
- The stability gate is last-quartile-vs-first-quartile (1.5×), not peak-vs-idle-baseline, to avoid
  false positives when the baseline is taken before connections are established.
- This run validates the post-#1077 code: concurrent read+write build paths now run under the
  `visMu` barrier, and the 4 h mixed Bolt workload completes with no panic and flat resources.

## Short CI variant (`TestBoltSoak_60s`)

Passes with the race detector on every PR. Zero races, zero leaks, zero failures.

## Re-running

```bash
SOAK_FULL=1 SOAK_ARTEFACTS=soak-artefacts \
  go test -tags=soakfull -run=TestBoltSoak_1024_4h -timeout=5h -v ./bench/soak/...
```
