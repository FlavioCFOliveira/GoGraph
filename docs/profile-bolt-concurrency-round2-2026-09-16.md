# Bolt extreme-concurrency profile — round 2 (2026-09-16)

Successor to [`profile-bolt-concurrency-2026-09-16.md`](profile-bolt-concurrency-2026-09-16.md),
which it supersedes. Round 1 ranked the write coalescing first at **42.06% of all
process CPU**; that change has since been made, so the ranking it produced is
stale by construction. Every number below was collected at the current HEAD, on
this machine, in this session, from `examples/23_bolt_server`. Nothing is
inherited — where a round-1 figure is quoted it is labelled as such and only for
comparison.

- **Commit** `a1057d5cc23c182dc69c23174de9a101a9ecd7bf`, branch `feature/362-performance-laboratory-20260915`
- **Host** Apple M4 (`Mac16,1`), 10 cores, 32 GB, macOS 26.6.2 (Darwin 25.6.0), Go 1.27.1 darwin/arm64
- **Artefacts** `/private/tmp/gograph-lab-362.noindex/r2/` (`.noindex` inside `/private/tmp`, so Spotlight never indexed the campaign's own output)
- **Task** rmp #2837

---

## 1. The ladder of record at this HEAD

Three independent processes per rung, twelve unprofiled measurement windows each
(**n = 36**), `-queries 400000` (`conn=1`: 100000), identical in every parameter
to round 1's ladder so that only the binary differs. The profiled probe window
is excluded from every throughput and latency figure.

| rung | throughput q/s (mean ±95% CI) | p50 | p95 | p99 | p999 | goroutines | FDs | loadavg1 | round 1 | Δ |
|---|---|---|---|---|---|---|---|---|---|---|
| `conn=1` | 37,342 ± 237 | 26 µs | 31 µs | 37 µs | 123 µs | 8 | 8 | 1.93–6.70 | 36,962 | +1.03% |
| `conn=8` | 111,755 ± 195 | 68 µs | 117 µs | 164 µs | 260 µs | 29 | 22 | 2.22–4.21 | 111,006 | +0.67% |
| `conn=64` | 193,588 ± 221 | 284 µs | 668 µs | 1.37 ms | 2.39 ms | 197 | 134 | 5.85–6.48 | 161,067 | **+20.19%** |
| `conn=256` | 192,649 ± 252 | 1.13 ms | 2.78 ms | 5.82 ms | 9.62 ms | 773 | 520 | 6.77–8.96 | 161,283 | **+19.45%** |
| `conn=1024` | 180,087 ± 301 | 5.14 ms | 9.73 ms | 19.70 ms | 32.38 ms | 3,076 | 2,056 | 7.40–9.05 | 154,185 | **+16.80%** |
| `saturation` (256 offered, 128 admitted) | 192,213 ± 450 | 546 µs | 1.44 ms | 3.10 ms | 4.94 ms | 388 | 264 | 7.28–8.78 | 160,252 | **+19.94%** |

Every rung: exit status 0, read from inside its own `run.log` — 18 ×
`EXIT_STATUS=0` — and cross-checked in **field 13** of `analysis/campaign2.tsv`,
where all 41 rows this round wrote record `exit=0`. (The column index matters:
the TSV gained a `foreign_cpu` column during round 1, which moved `exit` from
field 12 to field 13, and an index read from the original header silently
returns `gate_verdict` instead.)

`counters_settled = true` in every window. Across all 216 effect windows,
`bolt.server.conn.accepted` equals `bolt.server.conn.closed` **exactly** — 36,864
of each at `conn=1024` — so no connection was leaked. Peak goroutines vary by at
most **1** and peak descriptors by at most **2** across the 36 windows of any
rung (`benchstat`: ±0%), and `heap_alloc_bytes` is flat window to window (first
to last: 3.8 → 3.9 MB at `conn=1`, 5.6 → 5.8 MB at `conn=1024`). No goroutine,
descriptor or memory growth was observed.

**Refusing connections still does not degrade the ones being served.** The
saturation rung admitted exactly 128 and rejected exactly 128 in all 36 windows,
and its throughput is **0.23% below `conn=256`** — inside the noise floor below.

### Noise floor, re-derived at this HEAD

Round 1's floor (0.7%, later re-derived as 0.57% in rmp #2834) is **not reused**:
a faster server has a different floor. Three independent processes of the **same**
build and the **same** rung, compared with `benchstat`; the floor is the largest
same-versus-same difference that comes back significant.

| metric | resolvable difference at this HEAD |
|---|---|
| throughput, `conn ≥ 8` | **≥ 1.08%** (saturation +1.08%, `conn=1024` −0.64%, `conn=64` −0.56%; `conn=8` and `conn=256` had no significant same-versus-same delta) |
| throughput, `conn=1` | **≥ 4.03%** |
| p99 | **≥ 2.78%** (`conn=1`: 3.56%); spread reaches ±9% at `conn=1024` |
| p999 | no same-versus-same difference significant above `conn=1`; spread ±21% at `conn=1024` |
| connect time | **not usable at ≥ 256 connections** — same-versus-same spread ±105% to ±164% at `conn=256`, ±20–42% at `conn=1024`, and one same-versus-same delta of **+35.11%** came back significant at `conn=1024` |
| peak goroutines, peak FDs | exact (±0%) |

**Nothing below 1.08% is ranked in this document.**

---

## 2. Where the CPU goes at this HEAD

Merged probe profiles, three processes per rung, 1,200,000 queries each
(`conn=1`: 300,000; `saturation`: 600,000).

| rung | total CPU | CPU/query | server write `serve.go:1807` | µs/query | server read `chunking.go:178` | µs/query | `syscall.rawsyscalln` |
|---|---|---|---|---|---|---|---|
| `conn=1` | 7.55 s | 25.17 µs | 25.96% | 6.53 | 7.28% | 1.83 | 60.00% |
| `conn=8` | 65.51 s | 54.59 µs | 18.91% | 10.33 | 11.02% | 6.02 | 54.68% |
| `conn=64` | 51.96 s | 43.30 µs | 31.76% | 13.75 | 18.34% | 7.94 | 92.46% |
| `conn=256` | 53.08 s | 44.23 µs | **31.42%** | 13.90 | **19.18%** | 8.48 | 94.01% |
| `conn=1024` | 58.64 s | 48.87 µs | 29.28% | 14.31 | 17.36% | 8.48 | 92.50% |
| `saturation` | 26.53 s | 44.22 µs | 30.80% | 13.62 | 20.20% | 8.93 | 93.48% |

**94.01% of all process CPU at `conn=256` is `syscall.rawsyscalln`.** Everything
the module executes in user space — the Cypher engine, packstream, chunking,
the metrics sink and the plan cache, on both sides of the socket — is together
**under about 2%** of process CPU. `runtime.mallocgc` does not clear the
profile's 0.27 s reporting threshold at all.

### Syscalls per query, measured at this HEAD

`go tool trace -pprof=syscall` over each rung's probe trace, then `pprof
-sample_index=contentions` focused on each side's own framing types, so the
server's syscalls separate from the client driver's in the shared process.
400,000 queries per probe window.

| rung | server `write(2)`/q | server `read(2)`/q | client `write(2)`/q | client `read(2)`/q |
|---|---|---|---|---|
| `conn=64` | 1.0007 | 2.0008 | 1.0002 | 2.0003 |
| `conn=256` | 1.0016 | 2.0032 | 1.0006 | 2.0041 |
| `conn=1024` | 1.0052 | 2.0128 | 1.0026 | 2.0154 |

Six syscalls per query in total (2,403,825 for 400,000 queries at `conn=256`),
split evenly: each side performs one write and two reads.

### Contention and GC, re-checked

| observation | `conn=64` | `conn=256` | `conn=1024` |
|---|---|---|---|
| total mutex delay (merged probes) | 1.95 s | 2.51 s | 2.53 s |
| mutex delay per query | 1.63 µs | 2.09 µs | 2.11 µs |
| share held by `cypher/plan_cache.go:85` | 36.33% | 66.44% | 54.64% |
| `sync.(*Pool).pinSlow` share | 0.0079% | 0.011% | 0.011% |
| runtime-internal, unattributable (`_LostContendedRuntimeLock`) | 1.38% | 1.36% | 6.25% |
| GC mark CPU (`runtime.gcDrain`) | 0.62% | 0.92% | 1.06% |

Mutex delay per query fell from round 1's 1.56 / 4.97 / 4.09 µs to 1.63 / 2.09 /
2.11 µs. `cypher/plan_cache.go:85` remains the most contended Go mutex in the
server, and round 1's verdict on it is unchanged and further strengthened —
see §5.

---

## 3. Ranked opportunities

Biggest and simplest first. **Measured** means the number came from an
instrument in this campaign. **Hypothesis** means it did not. No hypothesis is
ranked above a measured cost, and nothing below the 1.08% noise floor is ranked.

| # | Opportunity | Call site | Measured weight | Scales with N? | Status | Expected gain |
|---|---|---|---|---|---|---|
| 1 | **Stop the laboratory's descriptor sampler from walking the descriptor directory every 25 ms.** `openFDs` calls `os.ReadDir` on `/dev/fd`, and on darwin that `lstat`s every entry, so the walk costs one syscall per open descriptor per sample — ~2,056 syscalls, 40 times a second, at `conn=1024`, inside the measurement window. | `examples/23_bolt_server/hostinfo.go:125`, called from `lab.go:581` every `samplerInterval` (`lab.go:56`) | **0.62% / 2.32% / 5.78%** of all process CPU at `conn=64` / `256` / `1024`; 3.39 s of 58.64 s at `conn=1024` | **yes — linear in the connection count. It is the only cost in the whole profile that does.** | **measured** (weight and gain) | **+1.09%** throughput at `conn=256` and **+4.65%** at `conn=1024`, p999 **−12.76%** at `conn=1024`, from a single-variable interleaved A/B, n=36 per arm, p=0.000 |
| 2 | **Nothing else clears the bar.** The residual gap between the module and a GoGraph-free control of the same shape is **4.75%** at `conn=256` (192,649 against 202,252 req/s), and that gap is the *whole* of the module's remaining user-space cost. No single call site inside it exceeds ~1% of process CPU. | — | non-syscall CPU at `conn=256` is 5.99%, of which GC 0.92% and scheduler/netpoll (`kevent`, `usleep`, `pthread_cond_wait`) 3.83%; **all user Go code on both sides ≈ 2%** | no | **measured** | the campaign's effort has stopped paying on this workload — see §6 |

### Carried forward from round 1, status re-checked here

| round-1 row | status at this HEAD |
|---|---|
| **#1 Coalesce the two response writes.** | **Done** (rmp #2834, commit `a1057d5c`). Verified here independently: server writes **1.0016/query** at `conn=256`, and throughput is **+19.45%** against round 1's ladder. The call site is still the largest single server cost (**31.42%** of process CPU, down from 42.06%), but one write per reply is the protocol floor for request/response. **Not an opportunity.** |
| **#2 Per-chunk throwaway slice** (`bolt/proto/chunking.go:243`, rmp #2836). | **Re-measured and recommended for closure without work** — see §4. |
| **#3 Bound the per-rejection WARN.** | **Done** (rmp #2835, commit `a1057d5c`). Re-checked end to end at this HEAD under a 2048-offered / 64-admitted flood: **25,737 rejections produced 7 WARN lines**, each carrying its `suppressed_since_last_line` tally. The reject path does not appear anywhere in the flood rung's CPU profile. |
| **#4 Evaluate the log argument lazily.** | **Done** in the same change (level test before the limiter). |
| **#5 The server reads twice per query.** | **Attributed, and closed as not a defect** — see §4. |
| **Refuted: the 256 → 1024 knee is a window-length artefact.** | **Partly superseded.** The knee is now measured at **−6.49%** with the harness sampler on and **−3.18%** with it off, so **half of it was the harness**, not the window length and not the server. Round 1's ladder carries the same bias. |
| **Refuted: `cypher/plan_cache.go:85` is the ceiling.** | **Refutation stands, and is stronger.** It is still the top Go mutex site (66.44% of 2.51 s at `conn=256`), yet throughput rose 19.45% at that rung with the lock completely unchanged. A lock that did not move cannot have been what moved. |
| **Refuted: GC is a cost under extreme concurrency.** | **Confirmed refuted.** GC mark CPU 0.62 / 0.92 / 1.06% at 64 / 256 / 1024. |
| **Refuted: `incCounter` contends.** | **Confirmed.** Absent from every mutex profile at every rung. |
| **Not observed: pool contention.** | **Confirmed.** `sync.(*Pool).pinSlow` is 0.011% of mutex delay at `conn=256`. |
| **Not observed: the transaction registry.** | **Still a non-observation, not an exoneration.** The workload is a single auto-commit read; `tx.opened` is 0 in every window. |
| **Cannot be ranked: connect cost.** | **Confirmed.** Same-versus-same connect spread is ±105–164% at `conn=256`; one same-versus-same delta of +35.11% at `conn=1024` came back *significant*. Unusable. |

---

## 4. The two questions this round had to settle

### 4.1 Why the server performs 2.00 `read(2)` per query — attributed

**The second `read(2)` is the Go runtime's own non-blocking probe. It is not a
second protocol read, and it is unreachable from module code.**

A profile cannot decide why two calls happen instead of one, so this was settled
by source plus a purpose-built control, not by reading a profile.

**Source** — `go1.27.1`, `src/internal/poll/fd_unix.go:164–172`. `FD.Read` loops:
it issues `syscall.Read`; on `EAGAIN` it parks on the netpoller (`fd.pd.waitRead`)
and **`continue`s**, issuing the syscall again. One logical `Read` that finds no
data therefore costs **two** `read(2)`.

**Control** — `readprobe`, a program with no GoGraph, no Bolt and no framing in
it, changing exactly one variable: whether the data is already in the socket
receive buffer when `Read` is called. Counts from `runtime/trace`, three
repetitions, 20,000 reads per arm.

| arm | descriptor | data present at `Read`? | `read(2)` per logical `Read` |
|---|---|---|---|
| `coldArm` | TCP loopback, runtime poller | no | **2.0000** (all three runs) |
| `coldUnixPollArm` | AF_UNIX socketpair, runtime poller | no | **2.0004** (all three runs) |
| `blockingArm` | **the same AF_UNIX socketpair, blocking mode** | no | **1.0002** (all three runs) |
| `hotArm` | TCP loopback, runtime poller | usually yes | 1.2503–1.2604 |

The decisive pair is the third row against the second: same socket family, same
ordering, same 50 µs delay, same message size — **only the descriptor's blocking
mode differs**, and the count halves.

This reconciles all four previously known bounds at once, which is what an
attribution has to do:

- the `countingConn` unit oracle counts **logical** `Read` calls and sees 1.000/query;
- `runtime/trace` counts **syscalls** and sees 2.00/query;
- the client also sees 2.004/query although the server now writes 1.001, because the client is waiting too;
- writes stay at 1.00 because a write into a non-full socket buffer never returns `EAGAIN`.

**Consequence.** Removing the second read would require blocking descriptors,
i.e. one OS thread per connection — the opposite of what this campaign is for.
Round-1 opportunity #5 is closed: real, quantified (**9.6% of process CPU at
`conn=256`**, half of the read path), and **not available**.

### 4.2 The platform ceiling, re-measured with a one-write control

**Round 1's 159,633 req/s is superseded.** That control replied with *two* writes
per request, so its number was a two-writes ceiling. The same control program was
re-run with `-two-writes` toggled: 256 loopback connections, 400,000 requests,
both arms interleaved with arm order rotated per repetition, three repetitions
each, gated and cooled between runs. All 30 runs exit 0.

| GOMAXPROCS | control, **two** writes/reply | control, **one** write/reply | GoGraph Bolt server, `conn=256` |
|---:|---:|---:|---:|
| 2 | 266,094 | **320,160** | 118,577 ± 1,413 |
| 4 | 220,318 | 269,248 | 173,321 ± 3,762 |
| 6 | 171,064 | 212,725 | 171,070 ± 2,396 |
| 8 | 162,840 | 209,265 | 182,178 ± 1,946 |
| 10 | **159,579** | **202,252** | 188,934 ± 1,995 |

(control: mean of three interleaved runs; GoGraph: 24 unprofiled windows per
point, `-queries 100000` — the ladder-of-record figure at `-queries 400000` is
192,649.)

Three consequences, each measured:

1. **The instrument is validated.** The two-writes arm reproduces round 1's
   159,633 to within **0.03%**, so the difference between the two controls is the
   second write and nothing else.
2. **The real ceiling for the server's current shape is 202,252 req/s**, and
   GoGraph sits at **95.3%** of it. The entire remaining headroom against a
   program with no Cypher, no packstream, no session state and no mutex of its
   own is **4.98%**.
3. **The plateau is still the platform's, not the module's.** The control *loses*
   throughput as cores are added (320,160 → 202,252); GoGraph *gains*
   (118,577 → 188,934).

---

## 5. What did not scale, and therefore is not a concurrency defect

Per-query server write CPU (13.75 → 13.90 → 14.31 µs at 64 → 256 → 1024), server
read CPU (7.94 → 8.48 → 8.48 µs), syscall counts (1.00 write / 2.00 read at every
rung) and allocation (80–84 KB/query) are **flat in connection count over a 16×
range**. With the harness sampler removed the same figures are 14.06 and 8.18
µs/query at `conn=256` against 14.84 and 8.97 at `conn=1024` — a drift under 10%
for 4× the connections.

**The per-query costs this campaign found are constants, not contention.** The
only quantity in the entire profile that grows with the connection count is the
laboratory's own descriptor walk (opportunity #1).

The differential CPU profile `conn=1024` minus `conn=256`, at equal query counts,
attributes the extra 5.56 s as: **+2.16 s the harness descriptor walk**, +1.22 s
the *client driver's* write path (the client is 2,048 of the process's 3,076
goroutines at that rung), +0.49 s the server's write path, +0.21 s the client's
read path, and ~0.9 s scheduler and runtime (`usleep`, `kevent`,
`pthread_cond_signal`, `madvise`).

---

## 6. Where this leaves the campaign

**On this workload, the optimisation cycle has stopped paying.** The evidence:

- 94.01% of process CPU is socket syscalls, and the server already performs the
  minimum write count (1.00) for request/response.
- Of the three server syscalls per query, one is the reply write (irreducible)
  and one of the two reads is the runtime's `EAGAIN` probe (unreachable).
- All module user-space code together is ~2% of process CPU, and the measured
  gap to a GoGraph-free control of the same shape is 4.75%.
- The largest *measured* remaining item in the ranking is a defect in the
  measuring instrument, not in the server.

The honest next questions are about **coverage, not about this workload's hot
path**: the laboratory drives a single auto-commit read returning **one** row, so
the RECORD-heavy path and the `BEGIN`/`COMMIT` path have never been exercised at
concurrency. Both are unmeasured, and this document says nothing about either.

---

## 7. Limits of the method

- **The client and the server share one process and ten cores.** At `conn=1024`
  the client is 2,048 of 3,076 goroutines and ~39% of process CPU. Rung-to-rung
  comparison is sound; comparison against a server measured with a remote client
  is not.
- **The host was never idle, and no rung is called idle.** Measured 120 s
  baseline (round 1, same host): loadavg1 min 1.55, median 2.19, p90 2.76, max
  3.44. Every rung's `loadavg1` is published in §1 and in
  `analysis/campaign2.tsv`. Heaviest foreign processes observed during this
  campaign, from `ps -Ao pcpu,pid,comm -r`: `PerfPowerServices` up to 57%,
  `osascript` 35%, `system_profiler` 29%, a Virtualization XPC 24%, `sysmond`
  19%, iTerm2 16%.
- **`loadavg1` is not a usable gate on this host** and is recorded as an
  observation only. It is an exponential average with a ~1-minute time constant
  while a rung lasts ~30 s, so a loadavg gate reads the decay of the previous
  rung and the campaign rejects itself. The gate in use sums `ps` %CPU over
  **non-campaign** processes (ceiling 120 in percent-of-one-core units) with a
  fixed 15 s cool-down. All 41 rows this round wrote to that TSV — ladder,
  GOMAXPROCS sweep, descriptor-sampler A/B and the pilot — record `exit 0` and
  verdict `foreign-quiet`; 37 passed the gate on the first attempt and 4 on the
  second.
- **The laboratory perturbs its own measurement**, by the amount quantified in
  opportunity #1. Every figure in §1 was taken with that perturbation present,
  because the ladder had to stay parameter-identical to round 1's; §3's
  opportunity #1 states what it costs.
- **Full-rate mutex and block profiling perturbs the contention it measures.**
  The probe window is separated from the effect windows for that reason, and no
  probe throughput is quoted in this document. Absolute mutex-delay figures are
  upper bounds.
- **The loopback control is shape-matched, not byte-matched.** It uses a 48-byte
  request and a 112-byte reply against the module's real Bolt messages for
  `MATCH (n:Person) RETURN count(n) AS c`, so §4.2's 4.98% headroom is an
  approximate upper bound on remaining user-space cost, not an exact one.
- **The transaction path and multi-row results were never exercised.**
- **GC was re-checked from the CPU profiles only.** The `GODEBUG=gctrace=1`
  segment was not re-run at this HEAD, so this round has no new STW-pause or
  GC-frequency figures; round 1's remain the only ones.
- **A profile shows where time went, never why a delta exists.** No conclusion
  here attributes a difference between two rungs to a profile. The read-side
  control (§4.1), the one-write ceiling sweep (§4.2), the descriptor-sampler A/B
  (§3 #1) and the GOMAXPROCS sweep are each a separate experiment designed to
  answer one question with one variable changed.

---

## 8. Reproduction

The laboratory gained one knob for this round, and no production code was
changed: `-fd-sampling` (default `true`, i.e. the behaviour every rung in §1 was
measured with) switches off the window sampler's descriptor walk, which is what
makes opportunity #1 measurable by A/B rather than arguable from a profile.

```sh
LAB=/private/tmp/gograph-lab-362.noindex/r2          # .noindex: Spotlight skips it
go build -o "$LAB/bin/boltlab" ./examples/23_bolt_server

# One rung of the ladder (repeat for 1, 8, 64, 256, 1024 and the saturation rung)
"$LAB/bin/boltlab" -nodes 2000 -knows-min 5 -knows-max 8 \
   -queries 400000 -sessions 1 -seed 42 \
   -connections 256 -max-connections 260 -repetitions 12 \
   -connect-timeout 5s -mutex-fraction 1 -block-rate 1 -server-log default \
   -label conn=256 -artifact-dir "$LAB/runs/s1/conn=256"

# The campaign, segment by segment, each in the foreground with the foreign-CPU gate
bash "$LAB/bin/campaign2.sh" ladder1     # and ladder2, ladder3
bash "$LAB/bin/campaign2.sh" procs       # GOMAXPROCS 2,4,6,8,10
BOLTLAB_BIN="$LAB/bin/boltlab2" bash "$LAB/bin/campaign2.sh" fdab   # descriptor-sampler A/B

# Analysis
python3 "$LAB/bin/table2.py"                          # per-rung table
benchstat "$LAB"/analysis/bench/ladder-s{1,2,3}.txt   # the noise floor, same vs same
benchstat "$LAB"/analysis/bench/fd-{on,off}.txt       # the descriptor-sampler A/B
go tool trace -pprof=syscall "$LAB/runs/s1/conn=256/trace.out" > syscall.pprof
go tool pprof -sample_index=contentions -focus=ChunkedWriter -top -nodecount=1 syscall.pprof
"$LAB/bin/readprobe" -n 20000 -size 64 -trace rp.trace   # the read-side control
bash "$LAB/bin/loopsweep3.sh"                            # the one-write ceiling sweep
```

| Artefact | Path |
|---|---|
| Per-rung artefact sets (42 rungs × 12 files) | `/private/tmp/gograph-lab-362.noindex/r2/runs/` |
| Gate record: loadavg1, foreign CPU, verdict, exit status | `/private/tmp/gograph-lab-362.noindex/r2/analysis/campaign2.tsv` |
| `benchstat` inputs (ladder and descriptor-sampler A/B) | `/private/tmp/gograph-lab-362.noindex/r2/analysis/bench/` |
| Merged profiles, text dumps, per-rung CPU attribution | `/private/tmp/gograph-lab-362.noindex/r2/analysis/prof/` |
| Syscall profiles extracted from the traces | `/private/tmp/gograph-lab-362.noindex/r2/analysis/syscall/` |
| Read-side control: source, traces, logs | `/private/tmp/gograph-lab-362.noindex/r2/readprobe/`, `analysis/readprobe/` |
| One-write ceiling sweep | `/private/tmp/gograph-lab-362.noindex/r2/analysis/loopsweep3.tsv` |
| Segment logs, with each rung's wall time | `/private/tmp/gograph-lab-362.noindex/r2/analysis/*.log` |
| `ps` snapshot before and after every rung | `<rung-dir>/ps-before.txt`, `<rung-dir>/ps-after.txt` |
