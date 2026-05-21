# Bolt Soak CI Report

## Run metadata

| Field         | Value                                  |
|---------------|----------------------------------------|
| Date          | 2026-05-21                             |
| Platform      | darwin/arm64 (Darwin 25.4.0)           |
| CPU           | Apple M4 (10 logical cores)            |
| Go version    | go1.26.3 darwin/arm64                  |
| Module        | gograph                                |
| Test variant  | `TestBoltSoak_1024_4h` (full soak)     |
| Connections   | 1024 concurrent                        |
| Duration      | 4 hours                                |
| Snapshot rate | every 30 s (480 snapshots total)       |
| Race detector | enabled (`-race`)                      |

## Full 4-hour / 1024-connection soak — PASS

**Verdict: PASS**

### Throughput

| Metric | Value |
|---|---|
| Successes | 262,483,121 |
| Failures (backpressure) | 128,144 |
| Success rate | 99.95% |
| Duration | 4h 0m 0s |

Failures are transient backpressure rejections (`max connections reached`) — tolerated by design.

### Resource stability (480 snapshots over 4 hours)

| Metric | Min | Max | Avg |
|---|---|---|---|
| Heap alloc | 10.7 MB | 48.7 MB | 19.6 MB |
| Goroutines | 1,231 | 2,104 | 1,631 |

### Stability gate — quartile analysis (2× threshold)

| Gate | First-quartile avg | Last-quartile avg | Ratio | Result |
|---|---|---|---|---|
| Heap (no monotonic growth) | 21.4 MB | 19.2 MB | 0.90× | **PASS** |
| Goroutines (no monotonic growth) | 1,665 | 1,628 | 0.98× | **PASS** |

Both last-quartile averages are *lower* than their first-quartile counterparts — the run is
stable with no upward trend in heap or goroutines.

### Other gates

| Gate | Result |
|---|---|
| Zero data races | PASS (race detector enabled throughout) |
| Zero panics / crashes | PASS |
| `goleak.VerifyNone` after shutdown | PASS |
| Server shutdown cleanly within 10 s | PASS |
| Successes > 0 | PASS (262M successes) |

### Notes

- Heap oscillation (10.7–48.7 MB) under 1024 concurrent connections is expected: GC cycles under
  high-concurrency load produce large swings between collection and allocation.
- Goroutine count (1231–2104) reflects the steady-state pool of 1024 active connection handlers
  plus server internals; it does not grow over the run.
- Threshold was recalibrated from peak-vs-idle-baseline to last-quartile-vs-first-quartile to
  avoid false positives when baseline is taken before connections are established.

## Short CI variant (`TestBoltSoak_60s`)

Passes with race detector on every PR. Zero races, zero leaks, zero failures.

## Re-running

```bash
SOAK_FULL=1 SOAK_ARTEFACTS=soak-artefacts \
  go test -run=TestBoltSoak_1024_4h -timeout=5h -v ./bench/soak/...
```
