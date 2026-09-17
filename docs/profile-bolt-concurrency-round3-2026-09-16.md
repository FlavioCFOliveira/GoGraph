# Bolt extreme-concurrency profile — round 3: the RECORD-heavy and explicit-transaction paths (2026-09-16)

Rounds 1 and 2 ([`profile-bolt-concurrency-2026-09-16.md`](profile-bolt-concurrency-2026-09-16.md),
[`profile-bolt-concurrency-round2-2026-09-16.md`](profile-bolt-concurrency-round2-2026-09-16.md))
drove **one shape of work**: a single auto-commit read returning **one row**.
Round 2 closed by saying so, and by recording two gaps honestly rather than
quietly: the RECORD-heavy path had never been exercised at concurrency, and
`bolt.server.tx.opened` was **0 in every window of every rung**, so
`bolt/server/txregistry.go` had never appeared in a profile at all — *a
non-observation, not an exoneration*.

This round closes both gaps. It does **not** supersede round 2: round 2's
ladder, its one-write ceiling and its read-side attribution stand, and are cited
here only for comparison. Nothing below is inherited; every number was collected
at the current HEAD, on this machine, in this session, from
`examples/23_bolt_server`.

- **Commit** `992b145e143bdb7a8e8f86cf58b18d8f8b5475d9`, branch `feature/362-performance-laboratory-20260915`
- **Host** Apple M4 (`Mac16,1`), 10 cores, 32 GB, macOS 26.6.2 (Darwin 25.6.0), Go 1.27.1 darwin/arm64
- **Artefacts** `/private/tmp/gograph-lab-362.noindex/r3/`
- **Task** rmp #2838
- **No production code was changed.** The laboratory is the vehicle; `bolt/`, `cypher/` and `store/` are untouched.

---

## 0. The verdict in four lines

1. **Response batching holds at 256 concurrent connections, to the byte.** A
   1000-row reply costs **12 `write(2)`, not 1000**.
2. **`bolt/server/txregistry.go` and `bolt/server/txquota.go` are exonerated**
   against a workload that genuinely drove them (`tx.opened` = 140,000 per
   window). Their mutex *wait* grows **11.6×** from 64 to 1024 connections and
   is 90.2% of all Go-mutex delay — and it costs **0.10% of process CPU** and
   **no throughput at all**.
3. **Three of the four shapes have no cost that grows with the connection
   count.** CPU per query moves by **+0.89%** (`txread`), **+4.3%** (`records`)
   and **+7.2%** (`count`) across a 16× range in N.
4. **The explicit-WRITE shape is the exception, and the cause is not in
   `bolt/`.** Throughput falls **33.4%** from its peak at `conn=64` (−27.3%
   against `conn=256`), against a floor of 0.36–1.04%, and the growth is a
   `sync.RWMutex` in **`graph/lpg`** — `property.go:360` and
   `mvcc_reclaim.go:167` — whose wait grows **48×** over the same range.

---

## 1. The four shapes

`-connections` decides how many clients talk; the new `-workload` decides what
they say. The shapes are **nested so consecutive pairs differ by one thing**,
which is what makes a difference between two ladders attributable rather than
merely observable.

| shape | one unit | rows/reply | server syscalls/unit |
|---|---|---|---|
| `count` *(the default; rounds 1–2's shape)* | auto-commit `RUN` of `MATCH (n:Person) RETURN count(n) AS c` | 1 | 1 write, 2 read |
| `records` | the same read returning `-rows` rows of `(id, name)` | `-rows` | 1 write per ~4 KB, 2 read |
| `txread` | `BEGIN`/`RUN`/`PULL`/`COMMIT` around **the same count query** | 1 | 3 write, 6 read |
| `txwrite` | `BEGIN`/`RUN`/`PULL`/`COMMIT` around an increment of this connection's own `:Counter` | 1 | 3 write, 6 read |

- `count` → `txread` isolates **`BEGIN`/`COMMIT` and nothing else**: same
  statement, same result, same row count, same client code.
- `count` → `records` isolates **reply cardinality and nothing else**.
- `txread` → `txwrite` isolates **the statement being a write**.

**Method.** For each shape, the full ladder — 1, 8, 64, 256, 1024 — plus the
saturation rung (256 offered, 128 admitted). Three independent processes per
rung, twelve unprofiled measurement windows each (**n = 36**) plus one profiled
probe window that is excluded from every throughput and latency figure. Every
rung runs `-fd-sampling=false`, so round 2's opportunity #1 — the laboratory's
own descriptor walk, the only cost in that profile that grew with N — is not
charged to the server here. The four shapes' sweeps were **interleaved**, with
the shape order rotated on each of the three sweeps, so no shape occupies a
fixed position in time. Query budgets are per shape, sized so every rung runs
~2 s windows.

---

## 2. The ladder of record, per shape

Throughput is the mean of 36 unprofiled windows with its 95% CI; latencies are
medians of the same 36. `tx.opened` is the per-window delta of
`bolt.server.tx.opened` — **the evidence the transaction path ran**.

### `count` — the round-1/2 shape, re-measured here with the sampler off

| rung | thr q/s (mean ±95% CI) | p50 | p95 | p99 | p999 | goroutines | FDs | loadavg1 | `tx.opened` |
|---|---|---|---|---|---|---|---|---|---|
| `conn=1` | 37,543 ± 201 | 26 µs | 31 µs | 36 µs | 123 µs | 8 | 8 | 1.70–6.93 | 0 |
| `conn=8` | 111,367 ± 162 | 68 µs | 118 µs | 164 µs | 260 µs | 29 | 22 | 1.84–4.28 | 0 |
| `conn=64` | 194,436 ± 314 | 283 µs | 664 µs | 1.32 ms | 2.36 ms | 197 | 134 | 6.54–7.94 | 0 |
| **`conn=256`** | **195,406 ± 250** | 1.12 ms | 2.70 ms | 5.71 ms | 9.17 ms | 772 | 518 | 6.72–8.07 | 0 |
| `conn=1024` | 189,536 ± 278 | 4.90 ms | 9.25 ms | 18.61 ms | 27.54 ms | 3,076 | 2,054 | 7.83–8.29 | 0 |
| `saturation` | 193,664 ± 439 | 543 µs | 1.42 ms | 3.02 ms | 4.85 ms | 388 | 262 | 8.22–8.52 | 0 |

Scales **5.20×** from 1 to its peak; **−3.00%** from peak to 1024. It reproduces
round 2 (192,649 at `conn=256`) to **+1.43%**, which is what round 2's own
descriptor-sampler A/B predicted for switching the sampler off (+1.09%). The
instrument is consistent with itself across rounds.

### `records` — 100 rows per reply

| rung | thr q/s (mean ±95% CI) | p50 | p95 | p99 | p999 | goroutines | FDs | loadavg1 | `tx.opened` |
|---|---|---|---|---|---|---|---|---|---|
| `conn=1` | 8,745 ± 20 | 99 µs | 240 µs | 348 µs | 413 µs | 8 | 8 | 6.68–7.90 | 0 |
| `conn=8` | 26,933 ± 69 | 258 µs | 565 µs | 701 µs | 884 µs | 29 | 22 | 4.54–4.60 | 0 |
| `conn=64` | 31,264 ± 185 | 952 µs | 7.36 ms | 11.21 ms | 16.26 ms | 197 | 134 | 5.47–6.01 | 0 |
| `conn=256` | 33,910 ± 171 | 4.05 ms | 29.55 ms | 44.84 ms | 71.92 ms | 773 | 518 | 6.04–6.46 | 0 |
| **`conn=1024`** | **34,431 ± 142** | 28.47 ms | 36.30 ms | 87.76 ms | 132.87 ms | 3,077 | 2,054 | 5.26–6.82 | 0 |
| `saturation` | 32,480 ± 107 | 1.65 ms | 15.86 ms | 24.99 ms | 37.08 ms | 388 | 262 | 6.83–7.32 | 0 |

**Throughput peaks at the top of the ladder**, and **7,000,000 rows** were
consumed and verified in every window of every ladder rung from 8 upwards
(1,800,000 at `conn=1`). The saturation rung deliberately loses the queries of
the connections it refuses, so its row count is an observation, not an
invariant.

### `txread` — explicit read transaction

| rung | thr q/s (mean ±95% CI) | p50 | p95 | p99 | p999 | goroutines | FDs | loadavg1 | `tx.opened` |
|---|---|---|---|---|---|---|---|---|---|
| `conn=1` | 15,739 ± 35 | 63 µs | 72 µs | 81 µs | 234 µs | 8 | 8 | 5.68–7.17 | **35,000** |
| `conn=8` | 40,505 ± 81 | 192 µs | 279 µs | 341 µs | 436 µs | 29 | 22 | 4.15–4.51 | **140,000** |
| `conn=64` | 66,602 ± 202 | 910 µs | 1.44 ms | 2.31 ms | 3.29 ms | 197 | 134 | 5.88–6.73 | **140,000** |
| `conn=256` | 69,955 ± 109 | 3.45 ms | 5.75 ms | 7.95 ms | 11.30 ms | 773 | 518 | 5.77–7.12 | **140,000** |
| **`conn=1024`** | **70,505 ± 109** | 13.88 ms | 18.76 ms | 24.63 ms | 29.29 ms | 3,077 | 2,054 | 7.28–7.75 | **140,000** |
| `saturation` | 69,493 ± 141 | 1.72 ms | 3.06 ms | 4.61 ms | 7.06 ms | 388 | 262 | 7.98–9.03 | 69,990–70,013 |

**Throughput peaks at the top of the ladder.** `tx.opened` equals the number of
successful units **exactly** in every window, and equals `tx.closed`; `tx.abandoned`
is 0 everywhere.

### `txwrite` — explicit write transaction

| rung | thr q/s (mean ±95% CI) | p50 | p95 | p99 | p999 | goroutines | FDs | loadavg1 | `tx.opened` |
|---|---|---|---|---|---|---|---|---|---|
| `conn=1` | 4,585 ± 12 | 214 µs | 229 µs | 368 µs | 550 µs | 8 | 8 | 6.60–7.07 | **9,000** |
| `conn=8` | 15,981 ± 25 | 443 µs | 821 µs | 1.09 ms | 1.34 ms | 29 | 22 | 3.98–5.01 | **36,000** |
| **`conn=64`** | **19,564 ± 27** | 2.67 ms | 7.50 ms | 10.72 ms | 15.42 ms | 196 | 134 | 5.21–6.85 | **36,000** |
| `conn=256` | 17,917 ± 43 | 10.09 ms | 37.88 ms | 54.88 ms | 77.92 ms | 772 | 518 | 6.16–8.01 | **36,000** |
| `conn=1024` | **13,031 ± 48** | 67.38 ms | 144.63 ms | 186.48 ms | 236.63 ms | 3,077 | 2,054 | 6.60–8.57 | **36,000** |
| `saturation` | **18,901 ± 45** | 4.93 ms | 17.17 ms | 25.55 ms | 36.61 ms | 388 | 262 | 8.52–9.44 | 17,993–18,034 |

**Throughput peaks at 64 connections and falls 33.39% by 1024**, and p50 rises
from 2.67 ms to 67.38 ms over the same range. The saturation rung, which admits
**128** of the 256 offered, is **5.49% FASTER than admitting all 256** — refusing
half the writers makes the server quicker.

### Summary

| shape | scale 1 → peak | peak rung | 1024 vs peak | saturation vs `conn=256` |
|---|---|---|---|---|
| `count` | 5.20× | `conn=256` | **−3.00%** | −0.89% |
| `records` | 3.94× | `conn=1024` | **0.00%** | −4.22% |
| `txread` | 4.48× | `conn=1024` | **0.00%** | −0.66% |
| `txwrite` | 4.27× | **`conn=64`** | **−33.39%** | **+5.49%** |

---

## 3. Noise floor, re-derived per shape

Round 2's 1.08% was derived for the one-row read and does **not** transfer:
every shape has its own floor. Three independent processes of the **same** build
and the **same** rung, all three pairings compared with `benchstat`; the floor is
the largest same-versus-same difference that comes back significant. `n.s.` means
no same-versus-same delta at that rung was significant at all.

**Throughput (`queries/sec`)**

| shape | `conn=1` | `conn=8` | `conn=64` | `conn=256` | `conn=1024` | `saturation` | **floor** |
|---|---|---|---|---|---|---|---|
| `count` | 3.06% | n.s. | 0.63% | 0.30% | n.s. | n.s. | **3.06%** |
| `records` | 1.00% | n.s. | 3.91% | 3.13% | 2.47% | 1.50% | **3.91%** |
| `txread` | 0.81% | n.s. | 1.36% | n.s. | n.s. | n.s. | **1.36%** |
| `txwrite` | 1.04% | 0.71% | 0.36% | n.s. | n.s. | n.s. | **1.04%** |

The rows sweep, all at 256 connections, has its own floor per arm: `rows=1`
0.50%, `rows=10` 1.28%, `rows=100` 1.54%, **`rows=1000` 6.16%** — the arm with
the fewest queries per window and the widest spread.

**p99**

| shape | `conn=1` | `conn=8` | `conn=64` | `conn=256` | `conn=1024` | `saturation` | **floor** |
|---|---|---|---|---|---|---|---|
| `count` | 3.34% | 0.88% | 4.40% | n.s. | n.s. | n.s. | **4.40%** |
| `records` | 3.34% | n.s. | 4.23% | n.s. | **78.50%** | 3.11% | **78.50%** |
| `txread` | n.s. | n.s. | 3.86% | n.s. | n.s. | n.s. | **3.86%** |
| `txwrite` | 4.26% | 1.40% | n.s. | n.s. | n.s. | n.s. | **4.26%** |

**p999** floors, with the rung each was measured at: `count` **2.65%**
(`conn=1`), `txwrite` **3.40%** (`conn=1`), `txread` **9.77%** (`conn=64`),
`records` **57.88%** (`conn=1024`).

Two consequences, stated because they bound what may be claimed:

- **`records` p99 and p999 at `conn=1024` are unusable.** A same-versus-same
  p99 delta of **78.50%** came back *significant* between two identical
  processes. The `records` tail at that rung is reported as an observation and
  nothing is ranked from it.
- **Connect time remains unusable at every rung**, as in round 2: the largest
  same-versus-same connect delta was **+668.50%** (`txwrite/saturation`).

`txwrite`'s **−33.39%** is **32×** its own worst-case floor (1.04%, at
`conn=1`) and **93×** the 0.36% floor measured at `conn=64`, the rung it falls
from. It is not noise.

---

## 4. Question 1 — does response batching still hold at concurrency?

**Yes, and to the byte.** Round 2's write coalescing changed *when* the
connection writer flushes; the only evidence batching survived it was three unit
tests on a single connection. Measured here at **256 concurrent connections**,
by `go tool trace -pprof=syscall` over each probe window, with the server's own
framing types isolated from the client driver's in the shared process:

| rows/reply | queries | server `write(2)`/query | server `read(2)`/query |
|---|---|---|---|
| 1 | 400,000 | **1.0028** | 2.0032 |
| 10 | 200,000 | **1.0038** | 2.0064 |
| 100 | 70,000 | **2.0103** | 2.0184 |
| 1000 | 10,000 | **12.0529** | 2.1274 |

A 1000-row reply costs **12 writes, not 1000** — **83× fewer**. The counts fit
`ceil(rows × ~46 bytes / 4096) + 1` at every point, which is exactly the
`O(bytes/bufsize)` law that `cw.SetAutoFlush(false)` at
`bolt/server/serve.go:1302` claims, with the 4096-byte `bufio.Writer` behind it.
Throughput in **rows** per second rises monotonically with the batch — 127k,
1.07M, 3.39M, **5.71M** rows/s at 1, 10, 100, 1000 rows — which is the same law
seen from the other side.

**The claim is verified; there is no defect here.**

### Syscalls per query are a constant in N, in every shape

| shape | `conn=64` | `conn=256` | `conn=1024` |
|---|---|---|---|
| `count` | 1.0006 W / 2.0007 R | 1.0016 / 2.0032 | 1.0052 / 2.0129 |
| `records` (100 rows) | 2.0048 / 2.0045 | 2.0105 / 2.0183 | 2.0300 / 2.0733 |
| `txread` | **3.0011 / 6.0023** | 3.0038 / 6.0092 | 3.0143 / 6.0370 |
| `txwrite` | **3.0036 / 6.0089** | 3.0141 / 6.0355 | 3.0569 / 6.1419 |

An explicit transaction costs exactly **three** replies and three request reads —
`BEGIN`, `RUN`+`PULL`, `COMMIT` — which is the protocol's own floor for
`BEGIN`/`COMMIT` and not an implementation cost. The doubled read count is round
2's attribution unchanged: the second `read(2)` is the Go runtime's `EAGAIN`
probe in `internal/poll.FD.Read`, reachable from no module code. From 64 to 1024
connections every count moves by at most **+1.8%**.

---

## 5. Question 2 — the transaction registry, measured at last

### 5.1 It ran

`bolt.server.tx.opened` equals the number of successful units **exactly** in
every window of every explicit rung: 35,000 at `txread/conn=1`, 140,000 at every
other `txread` rung, 9,000 and 36,000 for `txwrite`. `tx.closed` equals
`tx.opened` in all 432 explicit ladder windows and in the 132 windows of the two
write controls, and `tx.abandoned` is 0 in all of them.
The laboratory **fails the run** if `tx.opened` is zero on an explicit shape, so
this is enforced rather than merely reported.

### 5.2 It is the only thing in the read-transaction profile that grows with N

Mutex **delay** per transaction, merged probe windows, three processes per rung,
420,000 transactions per rung:

| site | `conn=64` | `conn=256` | `conn=1024` | 64 → 1024 |
|---|---|---|---|---|
| `bolt/server/txregistry.go:299` `nextID` | 1.21 µs | 4.29 µs | **13.17 µs** | **×10.9** |
| `bolt/server/txregistry.go:326` `unregister` | 0.74 µs | 3.26 µs | 8.69 µs | ×11.7 |
| `bolt/server/txregistry.go:315` `register` | 0.28 µs | 1.38 µs | 3.90 µs | ×14.0 |
| `bolt/server/txquota.go:89` `acquire` | 0.11 µs | 0.50 µs | 1.86 µs | ×17.3 |
| `bolt/server/txquota.go:108` `release` | 0.13 µs | 0.76 µs | 0.98 µs | ×7.4 |
| **the five together** | **2.47 µs** | **10.19 µs** | **28.60 µs** | **×11.6** |
| share of ALL Go-mutex delay | 57.7% | 81.2% | **90.2%** | |
| ALL Go-mutex delay | 4.28 µs | 12.55 µs | 31.71 µs | ×7.4 |

The structure behind the numbers, read from source: an explicit transaction takes
**five** process-global mutex acquisitions on structures every connection shares —
`nextID`, `register`, `unregister` on `txRegistry.mu`, and `acquire`/`release` on
`txQuota.mu`. `nextID` is the largest of the five because it formats the
transaction id with `fmt.Sprintf` **inside** its critical section
(`txregistry.go:299`). The quota is on by default
(`DefaultMaxOpenTxPerPrincipal = 2048`, `bolt/server/serve.go:257`) and, under
`NoAuthHandler`, every connection authenticates as the same principal, so all
1024 connections contend for one map entry.

### 5.3 And it costs nothing measurable — **exonerated**

| evidence | value at `conn=1024` |
|---|---|
| `Session.handleBegin`, cumulative process CPU | **0.10%** (0.05 s of 49.96 s) |
| `Session.registerTx`, cumulative | **0.08%** (0.04 s) |
| `Session.handleCommit`, cumulative | **0.06%** (0.03 s) |
| `fmt.Sprintf` | **does not clear the profile's reporting threshold at all** |
| `txread` CPU per query, 64 → 1024 | 117.90 → **118.95 µs** (**+0.89%**) |
| `txread` throughput at 1024 | **70,505 q/s — the shape's peak** |

**Hypothesis refuted.** The five acquisitions are real and their *wait* grows
11.6×, but the lock is held so briefly that the wait overlaps with other work:
a delay that buys back no throughput is queueing, not a bottleneck. The registry
and the quota are given a **measured weight of 0.1% of process CPU and no
measurable throughput cost**, at 1024 concurrent explicit transactions, against
a workload that opened 140,000 of them per window. That is an exoneration, and
it is the first one this campaign has been entitled to make.

**Reservation, stated rather than hidden:** the mutex-delay growth is the one
quantity in the read profile with an N-dependence, and it is 90.2% of all Go
mutex delay at 1024. On a host with more cores, or with a workload whose
transactions are shorter relative to the lock hold, the same structure could
begin to cost throughput. This exoneration is for **this shape, this machine,
this concurrency range**, and it is stated as such.

---

## 6. What grows with N, and what does not

**The discriminator: a per-query constant is not a concurrency defect; only a
cost that grows with the connection count is.** CPU per query, merged probe
profiles, three processes per rung, equal query counts within a rung:

| shape | `conn=1` | `conn=8` | `conn=64` | `conn=256` | `conn=1024` | **64 → 1024** |
|---|---|---|---|---|---|---|
| `count` | 26.30 µs | 54.91 µs | 42.89 µs | 43.59 µs | 45.96 µs | **+7.2%** |
| `records` | 153.33 µs | 248.10 µs | 250.33 µs | 253.81 µs | 261.00 µs | **+4.3%** |
| `txread` | 66.29 µs | 140.88 µs | 117.90 µs | 118.36 µs | 118.95 µs | **+0.89%** |
| `txwrite` | 225.56 µs | 456.02 µs | 455.28 µs | 516.20 µs | **701.20 µs** | **+54.0%** |

The unprofiled ladder agrees with the profiled one, so the probe did not
manufacture the growth: throughput implies **511 / 558 / 767 µs** of CPU per
query at 64 / 256 / 1024 for `txwrite`, against the profiled 455 / 516 / 701.

### Costs that grow with N — ranked

| # | Cost | Call site | Measured weight | Grows with N? | Status |
|---|---|---|---|---|---|
| 1 | **Concurrent property writes serialise on the 64 node-property shard locks.** | `graph/lpg/property.go:360` (`setNodePropertyInfo`), the `sync.RWMutex` at `graph/lpg/lpg.go:228`; shard chosen at `lpg.go:1746` from `propMapShards = 64` (`lpg.go:213`) | mutex delay **79.81 → 677.22 → 3848.24 µs per write** at 64 / 256 / 1024, **×48.2**; drives `txwrite` CPU/query +54.0% and throughput **−33.39%** from peak | **yes — the only one that costs throughput** | **measured and attributed** |
| 2 | **The MVCC vacuum competes with the writers for the same shard locks.** | `graph/lpg/mvcc_reclaim.go:167` (`reclaimPropVersions`), on the background vacuum goroutine started at `graph/lpg/mvcc_vacuum.go:594` (`go g.vacuumLoop()`) | mutex delay **48.15 → 434.26 → 1039.54 µs per write**, **×21.6**; 21.0% of all mutex delay at 1024 | **yes** | **measured**; same lock as #1, so the two are not additive |
| 3 | **The transaction registry and the per-principal quota.** | `txregistry.go:299/315/326`, `txquota.go:89/108` | mutex delay **2.47 → 10.19 → 28.60 µs per transaction**, **×11.6**, 90.2% of all Go-mutex delay at 1024 — but **0.10% of process CPU** and **no measurable throughput cost** | **yes (wait only)** | **measured — below the bar, see §5.3** |
| 4 | **A socket write costs more per call at 1024 connections than at 64**, with the call count unchanged. | `bolt/server/serve.go:1840` `writeResponse` → `syscall.rawsyscalln` | `records`: `writeResponse` cumulative CPU **5.09 → 5.39 → 6.11 s** per 210,000 queries (**+20.0%**), against a within-rung half-range across the three processes of ±3.7% at `conn=64` and ±3.9% at `conn=1024`; syscalls/query flat at 2.01 → 2.03 | **yes** | **measured — platform, not module.** The module issues the same number of syscalls; each one costs more. Nothing in `bolt/` can remove it. |

### Costs that are constants — listed, not ranked

These are per-query costs, some of them large, that do **not** grow with the
connection count and are therefore not concurrency defects:

| Cost | Call site | Measured weight | Behaviour in N |
|---|---|---|---|
| One `SetWriteDeadline` **per RECORD**, not per reply | `bolt/server/serve.go:1840`, reached from the record sink at `serve.go:1334` | **0.70%** of process CPU at `records/conn=64` — 0.37 s over **21,000,000 records**, ≈ **17.6 ns per record** | flat; below the reporting threshold at 256 and 1024 |
| Three replies and three request reads per explicit transaction | protocol | 3.00 W / 6.00 R per unit | flat (+1.8% over 16×) |
| The runtime's `EAGAIN` read probe | `internal/poll/fd_unix.go:164–172` (Go 1.27.1) | doubles every read count | flat; round 2 attributed it and closed it as unavailable |
| The reply write | `bolt/server/serve.go:1807` | 1.00 write per auto-commit reply | flat; the protocol floor |

---

## 7. Attributing the write-path collapse

**A profile shows where time went, never why a delta exists.** The ranking above
rests on four separate pieces of evidence, not on reading one profile.

### 7.1 The single-variable comparison: `txread` against `txwrite`

At the same rung the two shapes share **everything** except the statement: the
same 1024 connections, the same 3,077 goroutines, the same `BEGIN`/`RUN`/`PULL`/
`COMMIT`, the same five registry and quota acquisitions per transaction, the same
3.0 W / 6.0 R syscalls per unit, the same client code. `txread`'s CPU per query
moves **+0.89%** across a 16× range in N; `txwrite`'s moves **+54.0%**. The
growth is therefore in the write statement, not in connection handling and not in
the transaction bookkeeping.

### 7.2 The lock, named

Mutex delay per write transaction, merged probe windows, 108,000 transactions:

| site | `conn=8` | `conn=64` | `conn=256` | `conn=1024` | 64 → 1024 |
|---|---|---|---|---|---|
| `graph/lpg/property.go:360` `setNodePropertyInfo` | 0.51 µs | 79.81 µs | 677.22 µs | **3848.24 µs** | **×48.2** |
| `graph/lpg/mvcc_reclaim.go:167` `reclaimPropVersions` (vacuum) | 0.38 µs | 48.15 µs | 434.26 µs | 1039.54 µs | ×21.6 |
| `graph/lpg/mvcc_reclaim.go:131` `reclaimPropVersions` (writer) | 0.85 µs | 6.85 µs | — | — | — |
| **ALL mutex delay** | 12.26 µs | 148.52 µs | 1140.65 µs | **4951.30 µs** | ×33.3 |

Both top entries are the **same** `sync.RWMutex`: `nodePropShard.mu`
(`graph/lpg/lpg.go:228`). The node-property store is striped into
**`propMapShards = 64`** shards selected by `uint64(id) & 63`, so the number of
concurrent writers per shard lock is `connections / 64` — **1 at `conn=64`, 4 at
256, 16 at 1024**. Throughput peaks exactly where that reaches one writer per
shard. The vacuum goroutine takes the same locks to reclaim versions, so a
sustained write load makes the background reclaimer a competitor rather than a
bystander.

### 7.3 The control: it is the WRITERS, not the NODES

A purpose-built single-variable experiment, because §7.2 alone cannot separate
"many concurrent writers on 64 locks" from "many written nodes in 64 maps".
`-write-slots` multiplies the `:Counter` nodes **without changing the number of
concurrent writers**. Both arms, at both rungs, 24 unprofiled windows each, arm
order rotated on each of three sweeps, gated and cooled between runs:

| arm | `conn=64` | `conn=1024` | change | **extra CPU per query, 64 → 1024** |
|---|---|---|---|---|
| 1024 slots | 19,533 ± 34 | 13,106 ± 45 | **−32.90%** | **25.10 µs** |
| 4096 slots | 6,487 ± 13 | 5,740 ± 15 | **−11.52%** | **20.07 µs** |

Quadrupling the nodes leaves the **absolute** extra cost the ladder imposes
essentially unchanged (25.10 µs against 20.07 µs). A node-density explanation
predicted roughly 4×; the measurement refutes it. The 4096-slot arm degrades
*less* in percentage terms only because its longer label scan — a constant,
lock-free cost — dilutes the same absolute contention. **The N-scaling cost is
per-writer contention on the shard locks, independent of how many distinct nodes
are written.** Absolute throughput is not comparable across the two arms (the
scan length differs); only each arm's own ladder is, which is how it is read.

### 7.4 The independent confirmation: refusing writers makes the server faster

The saturation rung is a one-variable change in **admitted** writers at constant
offered load. 256 offered with **128 admitted** yields **18,901 ± 45 q/s**
against **17,917 ± 43** when all 256 are admitted — **+5.49%**, five times the
shape's 1.04% floor. Admitting fewer concurrent writers increases total
throughput, which is what a contention explanation predicts and what a
per-connection-cost explanation does not.

### 7.5 It is not a window-length artefact

Round 2 found that half of its own `256 → 1024` knee was the harness, so a
throughput drop at the top rung is not taken on trust here. At `conn=1024` the
ladder's 36,000 queries are only **35 per connection**, so a window could in
principle be dominated by ramp-up and tail drain rather than by steady state.
Quadrupling the queries per connection at the same rung, changing nothing else,
18 unprofiled windows per arm, arm order rotated on each of three sweeps:

| arm | queries/connection | window length | throughput | p50 |
|---|---|---|---|---|
| 36,000 queries | 35 | 2.74 s | 13,153 ± 34 | 66.6 ms |
| 144,000 queries | **141** | **11.12 s** | **12,955 ± 38** | 69.4 ms |

**−1.50%**, barely above this shape's 1.04% floor, for a **4× longer window**.
Measured against `conn=64` the long-window arm is **−33.78%**, against the
ladder's **−33.39%**. The collapse is steady-state, not a transient. Both arms
committed their effect exactly in all 36 windows.

---

## 8. Correctness, stability, and the gate

**Correctness was a pass/fail condition of every window, not a side check.** A
window that failed any of these failed the run.

| check | result |
|---|---|
| `records` returned **exactly** `-rows` rows per query, each with two columns and a 24-char `:Person` id | **pass** — 7,000,000 records per window at `conn≥8`, 1,800,000 at `conn=1`, exact in all 216 windows |
| `txwrite` committed effect, read back **through the engine** between windows | **pass** — the `:Counter` total moved by exactly one per successful unit in **all 216 ladder windows and all 132 control windows**; zero mismatches |
| `tx.opened` = successful units, `tx.closed` = `tx.opened`, `tx.abandoned` = 0 | **pass** in all **432 explicit ladder windows** and the **132 windows** of the two write controls |
| `bolt.server.conn.accepted == bolt.server.conn.closed` | **pass** in **all 1,092 windows** the campaign measured — every shape, every rung, all three controls |
| `bolt.server.conn.panics` | **0** everywhere |
| `counters_settled` | **true** in all 1,092 windows |
| goroutine leak | **none.** Peak goroutines vary by at most **1** across the 36 windows of any rung (3,076–3,077 at `conn=1024`), and peak descriptors are **exact** (2,054) |
| memory growth | **none.** `count/conn=1024` heap 5.60 → 5.79 MB across 12 windows; `txwrite/conn=1024` oscillates 6.18–10.64 MB with **no trend** — MVCC versions accruing and being reclaimed, not a leak |
| deadlock, panic, crash | none observed in 102 gated rungs, 1,092 measurement windows |

**Module validation at this HEAD**

| gate | result |
|---|---|
| `go test -race ./bolt/...` | **ok** — `packstream` 3.171 s, `proto` 2.242 s, `server` 17.667 s; **zero races**; exit status 0, read from inside the log |
| `go test ./cypher/tck/...` | **ok** — **3897 scenarios (3897 passed)**, `tckExecutionBaseline = 3897` **unchanged**; no failed, undefined or pending step |
| `go test -race ./examples/23_bolt_server/...` | **ok** 2.168 s, `goleak.VerifyTestMain` clean |
| `golangci-lint run ./examples/23_bolt_server/...` | **0 issues** |

`make ci` was **not** run: it is reserved for sprint close.

**The gate record.** **102 gated rungs**, **0 non-zero exits** — read from
`EXIT_STATUS=` inside each rung's own `run.log`, never off a pipeline's tail —
and cross-checked against the `exit` column of `analysis/campaign3.tsv`, located
by **header name** rather than by index. All 102 rows record verdict
`foreign-quiet`; 98 passed the gate on the first attempt and 4 on the second.
loadavg1 over the campaign: min **1.32**, median **6.69**, max **10.08**. Foreign
CPU: min 24.1, median 45.9, max 115.7 (percent-of-one-core units). Total gated
wall time 46.9 min.

---

## 9. What this changes, and what it does not

- **Round 2's stopping condition held for the shape it was measured on, and only
  for that shape.** On `count`, `records` and `txread` there is still nothing to
  optimise in `bolt/`: 94% of process CPU is socket syscalls, the syscall counts
  are at the protocol floor, and no user-space call site in the server grows
  with N.
- **The campaign's own question — "does holding connections at the limit degrade
  the module or the server?" — now has a two-part answer.** For reads, of either
  kind, and for large results: **no**, at up to 1024 connections. For concurrent
  **writes**: **yes**, and the cause is `graph/lpg`'s 64-way property shard lock,
  not the Bolt server.
- **Nothing here is a `bolt/` defect.** The Bolt server's per-query costs are
  constants in the connection count in all four shapes.

**Follow-up work identified, and not done here** (this task captures and
attributes only; no production code was changed):

1. Raise or reshape the node-property striping so that concurrent property
   writers do not queue behind 64 locks — `graph/lpg/lpg.go:213`, `property.go:360`.
2. Keep the MVCC vacuum off the shard locks the writers are using, or schedule it
   against write pressure — `graph/lpg/mvcc_reclaim.go:167`,
   `graph/lpg/mvcc_vacuum.go:594`.
3. Move `fmt.Sprintf` out of `txRegistry.nextID`'s critical section and consider
   an atomic sequence — `bolt/server/txregistry.go:299`. **Below the bar on this
   evidence (0.10% of CPU); recorded for completeness, not recommended on these
   numbers.**

---

## 10. Limits of the method

- **The client and the server share one process and ten cores.** At 1024
  connections the client is 2,048 of the process's 3,077 goroutines. Rung-to-rung
  comparison within a shape is sound; comparison against a server measured with a
  remote client is not, and **absolute throughput is not comparable across shapes**
  either, because the client's own per-unit cost differs between them.
- **The host was never idle, and no rung is called idle.** Every rung's
  `loadavg1` is published in §2 and in `analysis/campaign3.tsv`. `loadavg1` is
  recorded as an observation only; the gate is foreign CPU, for the reason round
  2 established.
- **`records` p99 and p999 at `conn=1024` are unusable** (same-versus-same
  78.50% and 57.88%), and connect time is unusable at every rung
  (same-versus-same up to +668.50%). Nothing is ranked from either.
- **Full-rate mutex and block profiling perturbs the contention it measures.**
  Every absolute mutex-delay figure in §5.2 and §7.2 is an **upper bound**. The
  probe window is separated from the effect windows for that reason, and no probe
  throughput is quoted. The CPU-per-query growth was cross-checked against the
  unprofiled ladder (§6) precisely because the profile alone could not establish it.
- **The write shape's statement is a label scan over 1024 `:Counter` nodes plus a
  property set.** That constant is deliberate — it keeps the per-query engine cost
  identical at every rung — but it means the write shape's *absolute* throughput
  is a property of that statement, not of the server. Only the ladder's shape is
  interpretable.
- **The `:Counter` slots map perfectly uniformly onto the 64 shards** (node ids
  are consecutive and the shard function is `id & 63`), so the measured contention
  is the *best* case for a uniform write workload; a random workload would have
  Poisson variance in per-shard load and would fare no better.
- **The `-write-slots` control changes two things at once** — node count and scan
  length — so only each arm's own 64 → 1024 ratio is used, never the absolute
  difference between arms. §7.3 states this where it is used.
- **`txwrite` was measured with `NoAuthHandler`**, so every connection shares one
  principal in the quota map. A server with distinct principals would spread that
  one map entry; the registry lock would not change.
- **GC was read from the CPU profiles only.** No `GODEBUG=gctrace=1` segment was
  run at this HEAD, so this round has no STW-pause or GC-frequency figures.
- **Only the RECORD-heavy shape's batching was swept over rows.** The rows sweep
  was run at 256 connections only; the batching law was not re-measured at 1024.
- **The per-shape query budgets give different queries per connection at the top
  rung** — 390 for `count`, 137 for `txread`, 68 for `records` and 35 for
  `txwrite` — because the budgets equalise window *duration*, not query count.
  §7.5 tests the consequence for the shape it could matter to and finds it worth
  −1.50%; it was **not** tested for `records` or `txread`, whose 1024 rungs show
  no degradation to explain away in the first place.
- **The window-length control (§7.5) and the node-density control (§7.3) were run
  after the ladder**, not interleaved with it, so they are compared against the
  ladder across a time gap. Each is internally interleaved and its own arms are
  directly comparable; the cross-comparison to the ladder's `conn=64` mean is
  not, and is quoted only where the effect is 30× the floor.

---

## 11. Reproduction

The laboratory gained three knobs for this round, and no production code was
changed: `-workload` (`count` | `records` | `txread` | `txwrite`, default
`count`, so every earlier rung still means what it said), `-rows` (default 100),
and `-write-slots` (default 1024, used only by §7.3).

```sh
LAB=/private/tmp/gograph-lab-362.noindex/r3        # .noindex: Spotlight skips it
go build -o "$LAB/bin/boltlab3" ./examples/23_bolt_server

# One rung of one shape
"$LAB/bin/boltlab3" -nodes 2000 -knows-min 5 -knows-max 8 \
   -queries 140000 -sessions 1 -seed 42 -workload txread \
   -connections 256 -max-connections 260 -repetitions 12 \
   -connect-timeout 5s -mutex-fraction 1 -block-rate 1 -server-log default \
   -fd-sampling=false -label txread/conn=256 -artifact-dir "$LAB/runs/txread/s1/conn=256"

# The campaign, segment by segment, each in the FOREGROUND with the foreign-CPU gate.
# Shape order was rotated per sweep; sweep 1 ran count, records, txread, txwrite.
bash "$LAB/bin/campaign3.sh" ladder count   1     # ... x 4 shapes x 3 sweeps
bash "$LAB/bin/campaign3.sh" rows 1               # the batching law, sweeps 1..3
BOLTLAB_BIN="$LAB/bin/boltlab3b" bash "$LAB/bin/campaign3.sh" slots 1   # the node-density control
bash "$LAB/bin/campaign3.sh" window 1                     # the window-length control

# Analysis
python3 "$LAB/bin/table3.py"                              # the per-shape tables of §2
benchstat "$LAB"/analysis/bench/txwrite-s{1,2}.txt        # the noise floor, same vs same
go tool trace -pprof=syscall "$LAB/runs/rows/s1/r1000/trace.out" > sys.pprof
go tool pprof -sample_index=contentions -focus=ChunkedWriter -top -nodecount=1 sys.pprof
go tool pprof -top -nodecount=400 -lines "$LAB/analysis/prof/txwrite-mutex-1024.pb.gz"
go tool pprof -peek 'sync\.\(\*RWMutex\)\.Unlock$' "$LAB/analysis/prof/txwrite-mutex-1024.pb.gz"
```

| Artefact | Path |
|---|---|
| Per-rung artefact sets (102 rungs × 12 files) | `/private/tmp/gograph-lab-362.noindex/r3/runs/` |
| Gate record: loadavg1, foreign CPU, verdict, exit status, wall time | `/private/tmp/gograph-lab-362.noindex/r3/analysis/campaign3.tsv` |
| `benchstat` inputs, per shape per sweep | `/private/tmp/gograph-lab-362.noindex/r3/analysis/bench/` |
| All 15 same-versus-same `benchstat` pairings (§3) | `/private/tmp/gograph-lab-362.noindex/r3/analysis/noisefloor.txt` |
| Merged CPU and mutex profiles, per shape per rung | `/private/tmp/gograph-lab-362.noindex/r3/analysis/prof/` |
| Syscall profiles extracted from the traces | `/private/tmp/gograph-lab-362.noindex/r3/analysis/syscall/` |
| Per-shape ladder tables | `/private/tmp/gograph-lab-362.noindex/r3/analysis/table3.md` |
| Segment logs, with each rung's wall time | `/private/tmp/gograph-lab-362.noindex/r3/analysis/*-s[123].log` |
| `ps` snapshot before and after every rung | `<rung-dir>/ps-before.txt`, `<rung-dir>/ps-after.txt` |
| Campaign driver and analysis scripts | `/private/tmp/gograph-lab-362.noindex/r3/bin/` |
