# Soak test (multi-hour mixed-workload reliability gate)

The soak harness exercises the library's reliability guarantees in
the same shape they must hold under real production load: many
concurrent readers (BFS, Dijkstra) running against an atomically
swapped CSR snapshot while a single writer mutates the underlying
adjacency list and the orchestrator rebuilds the CSR on a fixed
cadence. Periodic heap profiles are written to disk so the steady-
state behaviour can be inspected after the run.

The acceptance gate codifies the project's reliability mandate:

- Post-warmup **heap delta less than 5 %** across the run.
- File-descriptor count steady (no incremental leaks from the
  per-iteration CSR rebuild or from the searches themselves).
- Goroutine count steady — verified by the soak log lines that
  print `runtime.NumGoroutine()` at every sample interval.

The harness is intentionally simple to keep the reliability signal
clean: the workload is a stable mix of read- and write-heavy
operations; the only allocation churn should come from the CSR
rebuild and from per-call search scratch (both already pool-backed
after Sprint 11).

## Running

```bash
# Full 4-hour acceptance run.
make soak

# 60-second smoke run to verify the harness itself works.
make soak-smoke

# Custom run (override duration, reader count, etc.).
make soak SOAK_FLAGS="-duration=10m -readers=16 -graph-size=65536"
```

Output:

- `soak-artefacts/heap-NNN.pb.gz` — heap profile snapshot at each
  sample interval. Inspect with `go tool pprof`.
- `stderr` — periodic `soak: t=Hh:Mm:Ss reads=N writes=N rebuilds=N
  goroutines=N` lines, plus the `GODEBUG=gctrace=1` GC trace.

## Inspecting

```bash
# Compare the first and last heap snapshot.
go tool pprof -base soak-artefacts/heap-000.pb.gz soak-artefacts/heap-007.pb.gz

# Plot heap-in-use over time (one entry per snapshot).
for f in soak-artefacts/heap-*.pb.gz; do
  printf "%s  %s\n" "$f" "$(go tool pprof -inuse_space -top -unit=mb "$f" \
    | awk '/^Showing/ { print $4, $5 }')"
done

# Goroutine and FD counters: extract from the stderr log.
grep "^soak:" run.log | awk -F'goroutines=' '{ print $2 }'
```

A run is **green** when:

- The heap-delta between the snapshot at `t = warmup_end` (≈ first
  10 % of duration) and the final snapshot is below 5 %.
- The goroutine counter has the same value at warmup and at the
  end (modulo background `runtime` goroutines).
- No `runtime.MutexProfile` contention spike is visible in the GC
  trace.
