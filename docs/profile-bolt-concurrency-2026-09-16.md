# Bolt extreme-concurrency profile — round 1 (2026-09-16)

Ranked optimisation opportunities for the Bolt server under extreme connection
counts, from the sprint 362 laboratory. Every number here was collected on this
machine, in this session, from `examples/23_bolt_server`. Nothing is inherited.

- **Commit** `af23e9b8f976dbb16c4ffbfd2011125b302bfbd4`, branch `feature/362-performance-laboratory-20260915`
- **Host** Apple M4, 10 cores, 32 GB, macOS 25.6.0, Go 1.27.1 darwin/arm64
- **Artefacts** `/private/tmp/gograph-lab-362.noindex/` (`.noindex` inside `/private/tmp`, so Spotlight never indexed the campaign's own output)
- **Tasks** rmp #2832 (capture), #2833 (attribution)

---

## 1. The ladder of record

Three independent processes per rung, twelve unprofiled measurement windows
each (**n = 36**), `-queries 400000` (`conn=1`: 100000, sized so every window
lasts ~3 s). The profiled probe window is excluded from every throughput and
latency figure, as `examples/23_bolt_server/README.md` requires.

| rung | throughput q/s (mean ±95% CI) | p50 | p95 | p99 | p999 | goroutines | FDs | loadavg1 |
|---|---|---|---|---|---|---|---|---|
| `conn=1` | 36,962 ± 186 | 26 µs | 33 µs | 43 µs | 120 µs | 8 | 8 | 1.23–7.05 |
| `conn=8` | 111,006 ± 90 | 68 µs | 116 µs | 168 µs | 273 µs | 29 | 22 | 1.79–5.32 |
| `conn=64` | 161,067 ± 271 | 339 µs | 801 µs | 1.65 ms | 3.05 ms | 197 | 134 | 6.43–7.77 |
| `conn=256` | 161,283 ± 230 | 1.37 ms | 3.27 ms | 6.74 ms | 11.40 ms | 773 | 520 | 7.75–8.52 |
| `conn=1024` | 154,185 ± 174 | 6.12 ms | 10.50 ms | 19.04 ms | 34.24 ms | 3,076 | 2,056 | 7.42–8.83 |
| `saturation` (256 offered, 128 admitted) | 160,252 ± 351 | 661 µs | 1.74 ms | 3.59 ms | 5.93 ms | 389 | 262 | 8.67–8.92 |

Every rung: exit status 0, `counters_settled = true`, all nine artefacts
written. The saturation rung admitted exactly 128 and rejected exactly 128 in
all 36 windows, and the admitted connections' throughput was **statistically
within the 0.7% noise floor of `conn=64` and `conn=256`** (−0.64% against
`conn=256`) — refusing connections does not degrade the connections already
being served.

**Latency is queueing, not a defect.** At every saturated rung the measured
p50 is 85–92% of N ÷ throughput: 64 → 0.397 ms predicted against 0.339 ms
measured; 256 → 1.587 ms against 1.370 ms; 1024 → 6.641 ms against 6.120 ms.
Little's Law accounts for the whole latency ladder across a 16× range of N.

### Noise floor (same versus same)

Three independent processes of the **same** rung, compared by `benchstat`:

| metric | resolvable difference |
|---|---|
| throughput | **≥ 0.7%** (`conn=1`: ≥ 3.5%) |
| p99 / p999 | no sweep-to-sweep difference was significant; spread reaches ±16% at `conn=1024` |
| connect time | **not usable at ≥ 256 connections** — spread ±163% at `conn=256`, ±35–54% at `conn=1024` |
| peak goroutines | exact (±0%) |

A delta below these thresholds is not a finding, and nothing below them is
ranked here.

---

## 2. Where the CPU goes

Merged probe profiles, three processes per rung.

| rung | cores busy (of 10) | CPU/query | write path `serve.go:1814` | read path `chunking.go:178` | server share | mutex delay/query | alloc/query |
|---|---|---|---|---|---|---|---|
| `conn=1` | 1.31 | 39.4 µs | 8.3 µs | 1.9 µs | 26.0% | 0.02 µs | 78.4 KB |
| `conn=8` | 6.41 | 62.5 µs | 18.9 µs | 7.1 µs | 41.6% | 2.42 µs | 78.0 KB |
| `conn=64` | 7.45 | 51.1 µs | 21.6 µs | 8.0 µs | 57.9% | 1.56 µs | 78.2 KB |
| `conn=256` | 7.45 | 52.3 µs | 22.0 µs | 7.7 µs | 56.9% | 4.97 µs | 79.1 KB |
| `conn=1024` | 7.25 | 56.5 µs | 22.7 µs | 8.0 µs | 54.2% | 4.09 µs | 82.5 KB |
| `saturation` | 6.96 | 52.4 µs | 21.8 µs | 7.6 µs | 56.1% | 2.81 µs | 79.7 KB |

**93–95% of all process CPU is `syscall.rawsyscalln`.** The Cypher engine,
packstream and the transaction registry are collectively a rounding error
beside the socket syscalls.

**The process never uses more than 7.45 of 10 cores.** It is not CPU-bound.

**Mutex delay does not scale monotonically with connections** (0.02 → 2.42 →
1.56 → 4.97 → 4.09 µs/query) and its composition changes: at `conn=8` it is
99.58% `runtime.unlock`, i.e. runtime-internal and not attributable to any Go
call site; from `conn=64` upward it is dominated by `cypher/plan_cache.go:85`
(37.87% → 86.01% → 77.05%). It is also inflated by full-rate profiling. It is
therefore reported but not ranked — see the refutation below.

---

## 3. Ranked opportunities

Biggest and simplest first. **Measured** means the number came from an
instrument in this campaign. **Hypothesis** means it did not.

| # | Opportunity | Call site | Measured weight | Bites at | Status | Expected ceiling |
|---|---|---|---|---|---|---|
| 1 | **Coalesce the two response writes into one.** The server performs exactly **2.00 `write(2)` per query**; the client delivers RUN+PULL in **1.00 `write(2)`**. The flush after the RUN `SUCCESS` is premature whenever the PULL is already decodable. | `bolt/server/serve.go:1814` | **42.06% of all process CPU** (26.39 s of 62.74 s at `conn=256`); 22.0 µs CPU/query; 800,761 write syscalls for 400,000 queries (`runtime/trace`) | **every** connection count ≥ 8; flat in N | **measured** (weight, syscall count); **measured on a control** (gain) | **+26.4%** throughput — 159,952 → 202,105 req/s, interleaved A/B, n=5 each, sd ≈ 1k, on a GoGraph-free loopback program with the same shape |
| 2 | **Stop allocating a throwaway slice per inbound chunk.** `msg = append(msg, make([]byte, chunkLen)...)` allocates the temporary *and* may grow `msg`, on every chunk of every inbound message. | `bolt/proto/chunking.go:243` | **1.43 GB and 31.85 M objects** per probe window (1.2 M queries) = **1.25 KB / 26.5 objects per query** | every connection count; flat in N | **measured** (weight); **hypothesis** (gain) | removes ~1.6% of allocated bytes and ~1.6% of objects; GC is only 3% CPU, so the throughput gain is expected to be small |
| 3 | **Bound or sample the per-rejection WARN.** Every refused connection formats and writes an 89-byte log line **on the accept goroutine**, unsampled and unbounded. | `bolt/server/serve.go:1042` | **1.670 µs ± 8% per rejection** (standalone benchmark) and **1.68 µs** (flood CPU profile, independent) — the two agree. 80 B / 4 allocs per rejection. 0.97% of process CPU at the achievable flood rate | only when `MaxConnections` is saturated | **measured** | caps the accept loop at ~599 k rejections/s and writes 89 B of log per refusal; at the 11.9 k rejections/s this harness can generate it is **2.0% of one goroutine** — real, small, and linear in the flooder's rate |
| 4 | **Evaluate the log argument lazily.** `conn.RemoteAddr().String()` is evaluated before the call in every arm, so `Options.Logger` at `LevelError` does not remove it. | `bolt/server/serve.go:1042` | **45.38 ns ± 2%, 32 B, 3 allocs per rejection**, paid even when the record is discarded | only when `MaxConnections` is saturated | **measured** | 2.7% of the WARN's cost; matters only if #3 is fixed by lowering the level rather than by sampling |
| 5 | **The server reads twice per query where the client writes once.** 2.00 `read(2)` per query, one per inbound Bolt message, although both messages arrive in a single client write. | `bolt/proto/chunking.go:178`, reader goroutine `bolt/server/serve.go:1395` | **8.0 µs CPU/query**, 800,998 read syscalls for 400,000 queries | every connection count; flat in N | **measured** (the asymmetry); **NOT attributed** (its cause) | unknown. Coupled to #1: a server that defers its flush would read both messages before replying, so #1 may remove this by construction |

### Refuted, and why

| Lead (from stage 1) | Verdict |
|---|---|
| **A scaling knee of −17% from 256 → 1024 connections.** | **Refuted as a steady-state defect.** The knee is a function of the *measurement window*, not of the connection count. Varying only `-queries` at fixed connections: **−14.90%** at 20 k, **−6.89%** at 100 k, **−4.40%** at 400 k queries (n = 36 each). A fixed per-connection start-up cost of **24–31 µs** is amortised over fewer queries in a short window. The true steady-state cost of 4× the connections is **−4.4% ± 0.2%**. |
| **The mutex profile at 1024 is 98.8% runtime-internal and unattributable (`_LostContendedRuntimeLock` 32.89%).** | **Refuted.** Merged over three processes, lost samples are **0.62%** (`conn=256`), **1.71%** (`conn=64`), **3.26%** (`conn=1024`). The profile is attributable. Stage 1's figure came from one short window. |
| **A global lock limits throughput — `cypher/plan_cache.go:85` is 86.01% of all mutex delay.** | **The attribution holds; the conclusion does not.** `planCache.get` takes a process-global `sync.Mutex` on every query and promotes the LRU entry inside it, and it is indeed the most contended Go mutex in the server. But a standalone benchmark of that exact structure sustains **12.0 M gets/s under 10-way contention** (83.24 ns/op; removing the LRU promotion changes nothing — the cost is the lock, not the promotion). That is **74× above** the 161 k queries/s measured. It cannot be the ceiling. Its absolute delay is also inflated by full-rate mutex profiling. |
| **`incCounter` (`bolt/server/metrics.go:122`) contends.** | **Refuted.** **4.478 ns, 0 B, 0 allocs.** The laboratory's sink is a read-only map of `atomic.Uint64`; the shipped default is a no-op behind an atomic pointer. Neither can contend. |
| **`reqDecPool` / `respBufPool` contend.** | **Not observed.** `sync.(*Pool).pinSlow` is **0.077%** of mutex delay at `conn=256`. The pools do their job. |
| **The transaction registry (`bolt/server/txregistry.go`) contends.** | **Not observed.** It does not appear in any CPU, mutex or block profile at any rung. The workload is auto-commit reads, so it is not exercised — this is a **non-observation, not an exoneration**. |
| **`container/list` churn (93 M objects per window) is GoGraph's.** | **Refuted.** 100% of it is the **client driver's** (`messageQueue.enqueueCallback`, `stream.push`, `pool.getIdle`, `pool.returnBusy`). |
| **GC is a cost under extreme concurrency.** | **Refuted.** **3% GC CPU** at both 256 and 1024. STW p99 **0.250 ms → 0.270 ms**, max 1.299 ms → 0.290 ms — pauses do **not** grow with connections. GC frequency *falls* (31.0/s → 11.2/s) as the larger live heap raises the goal. |
| **Connect cost is a rankable defect** (`210.8 ms` for 1024 connections). | **Cannot be ranked.** Connect time's own noise floor is ±163% at 256 connections and ±35–54% at 1024. No connect-time difference measured in this campaign clears it. |

### What did not scale, and therefore is not a concurrency defect

Per-query write-path CPU, read-path CPU, syscall counts (2.00 write / 2.01 read
at both 256 and 1024) and allocation (78–83 KB) are **flat in connection count**
from `conn=64` upward. The per-query costs this campaign found are constants,
not contention.

---

## 4. The ceiling this laboratory cannot see

**The measurement vehicle saturates at the same throughput as the module.** A
control program containing **no GoGraph code** — 256 loopback TCP connections in
one process, server replying with two writes per request, no Cypher, no
packstream, no mutex of its own — measures:

| GOMAXPROCS | control (no GoGraph) | GoGraph Bolt server, `conn=256` |
|---:|---:|---:|
| 2 | 269,181 req/s | 105,756 q/s |
| 4 | 221,774 | 151,710 |
| 6 | 171,040 | 146,305 |
| 8 | 161,584 | 153,473 |
| 10 | **159,633** | **158,294** |

The control **loses** throughput as cores are added and lands at 160 k req/s —
within 1% of GoGraph's 158 k q/s. Three consequences:

1. **GoGraph's Bolt server is already running at this platform's
   two-writes-per-reply loopback ceiling.** Its user-space work is nearly free
   beside the syscalls.
2. **The apparent "saturates at 4 of 10 cores" result is the platform's, not
   GoGraph's.** GoGraph's curve *rises* with cores (105.7 k → 158.3 k) where the
   control's *falls*.
3. **No optimisation that would push the server above ~160 k q/s is measurable
   here.** Opportunity #1 is ranked on the control's +26.4%, which is the
   headroom the platform itself grants when the second write is removed.

Confirming any of these requires a **remote client on separate hardware**. That
is outside this laboratory.

---

## 5. Limits of the method

- **The client and the server share one process and ten cores.** At
  `conn=1024` the client is itself 2,048 of the process's 3,076 goroutines and
  ~36% of its CPU. Rung-to-rung comparison is sound; comparison against a
  server measured with a remote client is not.
- **The host was never idle.** A measured 120 s baseline with no campaign
  running gave loadavg1 min 1.55, median 2.19, p90 2.76, max 3.44. The
  CLAUDE.md ideal (`NumCPU × 0.10` = 1.00) is unreachable on this machine.
  **No rung in this campaign was certified idle.** Every rung's loadavg1 is
  published in the tables above and in `analysis/campaign.tsv`.
- **`loadavg1` is not a usable gate here** and was replaced mid-campaign. It is
  an exponential average with a ~1-minute time constant while a rung lasts
  ~35 s, so each gate read the decay of the *previous rung*: verdicts tracked
  rung size exactly (`conn=1` 2.04 → `conn=1024` 3.23) — the campaign was
  rejecting itself. The replacement gates on `ps` %CPU summed over
  **non-campaign** processes (measured foreign floor 28.8–91.0; ceiling 120),
  with a fixed 15 s cool-down. Every rung passed on the first or second try.
  loadavg1 is still recorded and published, as an observation.
- **Full-rate mutex and block profiling perturbs the contention it measures.**
  The probe window is separated from the effect windows for exactly this
  reason, and no probe throughput is quoted anywhere in this document. Absolute
  mutex-delay figures should be read as upper bounds.
- **Connect time is not measurable at scale in this harness** (±163% at 256).
- **The transaction path was never exercised.** The workload is a single
  auto-commit read. Nothing here says anything about `BEGIN`/`COMMIT` under
  concurrency.
- **A profile shows where time went, never why a delta exists.** No conclusion
  above attributes a difference between two rungs to a profile. The window-length
  control (§3), the GOMAXPROCS sweep (§4), the loopback control (§4) and the
  interleaved write-coalescing A/B (#1) are each a separate experiment designed
  to answer one question.

---

## 6. Reproduction

```sh
LAB=/private/tmp/gograph-lab-362.noindex          # .noindex: Spotlight skips it
go build -o "$LAB/bin/boltlab" ./examples/23_bolt_server

# One rung of the ladder (repeat for 1, 8, 64, 256, 1024 and the saturation rung)
"$LAB/bin/boltlab" -nodes 2000 -knows-min 5 -knows-max 8 \
   -queries 400000 -sessions 1 -seed 42 \
   -connections 256 -max-connections 260 -repetitions 12 \
   -connect-timeout 5s -mutex-fraction 1 -block-rate 1 -server-log default \
   -label conn=256 -artifact-dir "$LAB/runs/s1/conn=256"

# The whole campaign, segment by segment, each with the foreign-CPU gate
bash "$LAB/bin/campaign.sh" ladder1     # and ladder2, ladder3
bash "$LAB/bin/campaign.sh" gc          # GODEBUG=gctrace=1 at 256 and 1024
bash "$LAB/bin/campaign.sh" connect     # connect-dominated windows
bash "$LAB/bin/campaign.sh" flood       # 2048 offered, 64 admitted, 3 logger arms
bash "$LAB/bin/campaign.sh" window      # window-length control
bash "$LAB/bin/campaign.sh" procs       # GOMAXPROCS 2,4,6,8,10

# Analysis
python3 "$LAB/bin/table.py"                       # per-rung table
bash    "$LAB/bin/mkbench.sh" && benchstat "$LAB/analysis/bench/ladder-all.txt"
bash    "$LAB/bin/prof.sh"                        # merge profiles, dump tops
go tool trace -pprof=syscall "$LAB/runs/s1/conn=256/trace.out" > syscall.pprof  # syscall COUNTS
bash    "$LAB/bin/loopsweep2.sh"                  # the write-coalescing A/B
```

| Artefact | Path |
|---|---|
| Per-rung artefact sets (35 rungs × 9 files) | `/private/tmp/gograph-lab-362.noindex/runs/` |
| Gate record: loadavg1, foreign CPU, verdict, exit status | `/private/tmp/gograph-lab-362.noindex/analysis/campaign.tsv` |
| `benchstat` inputs | `/private/tmp/gograph-lab-362.noindex/analysis/bench/` |
| Merged profiles and text dumps | `/private/tmp/gograph-lab-362.noindex/analysis/prof/` |
| Per-rejection logging benchmark | `/private/tmp/gograph-lab-362.noindex/warnbench/`, results in `analysis/warnbench2.txt` |
| Plan-cache ceiling benchmark | `/private/tmp/gograph-lab-362.noindex/plancachebench/` |
| Loopback control program and sweeps | `/private/tmp/gograph-lab-362.noindex/loopbench/`, `analysis/loopsweep*.txt` |
| `ps` snapshot before and after every rung | `<rung-dir>/ps-before.txt`, `<rung-dir>/ps-after.txt` |

Heaviest foreign processes throughout, from `ps -Ao pcpu,pid,comm -r | head`:
`osascript` 10.6–19.2, iTerm2 6.9–23.8, a Virtualization XPC 2.7–15.0,
WindowServer 4.2–9.2, `claude` 3.5–4.8. `mdworker_shared`, measured at 78.7%
during stage 1, stayed at or below 0.3% for this campaign — the `.noindex`
artefact directory kept the indexer out of it.
