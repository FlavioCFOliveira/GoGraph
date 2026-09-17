# Bolt extreme-concurrency campaign — closing record (2026-09-16)

Campaign run inside sprint 362 to make the Bolt server hold very many concurrent
connections and dispatch work without connections-at-the-limit degrading the
module or the server. Worked as profiling → optimisation cycles, largest measured
gain first, until the effort stopped paying.

**The sprint continues; what closed is this campaign.**

- **Branch** `feature/362-performance-laboratory-20260915`
- **Host** Apple M4, 10 cores, 32 GB, macOS 25.6.0, Go 1.27.1 darwin/arm64
- **Tasks** rmp #2831–#2840 (all closed); #2841 and #2842 recorded in the backlog, deliberately not worked
- **Reports** `profile-bolt-concurrency-2026-09-16.md` (round 1), `…-round2-…` , `…-round3-…`

## What shipped

| Commit | Change | Measured effect |
|---|---|---|
| `af23e9b8` | Example 23 becomes a reproducible concurrency laboratory | ladder 1/8/64/256/1024 + saturation, five profiles and a trace per rung |
| `a1057d5c` | Coalesce the two Bolt response writes; bound the per-rejection log | write syscalls/query **2.00 → ~1.00**; **+19–20%** at 64/256/saturation, **+16.8%** at 1024; p99 −17.8% at 64; rejection cost **547.3 ns/80 B/4 allocs → 34.9 ns/0/0** |
| `13ad0ed2` | `propMapShards` 64 → 256 | **+7.0%** at 64 connections, **+13.3%** at 1024; p99 −16.1%, p999 −14.8%; reader-under-writer −36.5% |

Cost accepted and recorded at the constant: an empty graph grows 91 KiB and
`New` takes 57% longer, because `New` eagerly allocates one map per stripe in
three of the nine shard arrays.

## What the campaign established about the server

- **Nothing in the Bolt server scales with connection count.** Per-query write
  cost, read cost, syscall counts and allocation are flat over a 16× range of N.
- **The server sits at 95.3% of the platform's real one-write ceiling** (202,252
  req/s for a GoGraph-free loopback control replying with one write).
- **Refusing connections does not degrade the connections already admitted** —
  the saturation rung's throughput sits inside the noise floor of the
  unsaturated rungs.
- **Latency is queueing, not a defect.** Little's Law accounts for the whole
  latency ladder across a 16× range of N.
- **Response batching holds**: a 1000-row reply costs **12** `write(2)`, not
  1000, fitting `ceil(rows × ~46 B / 4096) + 1`.

## What was refuted — with evidence

| Claim | Verdict |
|---|---|
| A −17% throughput knee between 256 and 1024 connections | **Artefact of the measurement window** (−14.9% at 20k queries, −4.4% at 400k), and **half of the remainder was the laboratory's own `/dev/fd` walk** |
| The mutex profile at 1024 is 98.8% unattributable | **Refuted** — lost samples are 0.62–3.26% |
| `cypher/plan_cache.go` is the ceiling | Top mutex site, but sustains **12.0 M gets/s**, 74× the query rate |
| `incCounter`, the buffer pools, the transaction registry contend | **Refuted or exonerated** — 4.478 ns/0 allocs; 0.077% of mutex delay; registry 0.10% of CPU with no throughput cost |
| GC is a cost under extreme concurrency | **Refuted** — 0.62–1.06% of CPU; pauses do not grow with connections |
| No gain is measurable above ~160k q/s | **Refuted** — that control replied with *two* writes; its ceiling was a two-writes ceiling |
| Shortening the property-write critical section will recover the collapse | **Refuted** — two prototypes measured as their own control (−0.31%, +0.48% against a +0.44% floor) |
| The MVCC vacuum costs throughput | **Refuted** — removing it entirely, though it held **21.0% of all mutex delay**, bought **zero** |

**Mutex delay is not a cost function.** A component can dominate a mutex profile
and cost nothing: delay counts time goroutines spent waiting, not time the
workload lost.

## What remains open

The one cost that grows with connection count is concurrent **property writes**,
and it is in `graph/lpg`, not in `bolt/`. Widening the stripe to 256 is a level
shift: the collapse from 64 to 1024 connections moved only 0.660 → 0.698 of
peak. The residual is the side-map work under the lock.

Deleting the delta work entirely measures **+60.96%**, which splits into:

- **~17% allocation and GC pressure** — reachable without a data-model change,
  tracked as **#2841** (stop allocating a `nodePropDelta` per write). On an
  untouched binary, `GOGC=400` alone buys +12.77%.
- **~38% side-map work under the lock** — **not** reachable without a per-node
  struct, which is an architectural decision and was deliberately declined.

Prior art read in source (Memgraph `f8ab137e`, Neo4j `bbf3b91f`, PostgreSQL
`3d2e8573`) is unanimous: **none of them stripes a fixed number of locks over
per-object data.** Per-object locking is the norm, and where striping survives it
guards a *lookup index* held for a hash probe. If this is ever revisited, the Go
design must be an `atomic.Uint32` seqlock rather than an embedded `sync.RWMutex`
— Memgraph affords per-object locking with a **4-byte** spin lock, where Go's
`RWMutex` is 24 bytes and its `RLock` is an atomic read-modify-write on a cold
line.

Also open: **#2842**, publishing `nodeLifeActive` under the shard lock that
publishes the record. Harmless today; a correctness hole the moment that counter
is used to skip a visibility check.

## Method notes worth keeping

- **Gate on foreign CPU, never on total `loadavg1`.** `loadavg1` is a ~1-minute
  exponential average and the rungs last ~35 s, so a loadavg gate reads the
  previous rung's own decay and the campaign rejects itself.
- **No run on this host is idle.** Measured 120 s baseline: loadavg1 median 2.19,
  max 3.44. Every published number carries its loadavg.
- **Use a byte-identical control arm.** Two builds of the same source establish
  the noise floor; without it a 1–2% "gain" is unfalsifiable.
- **A profile shows where time went, never why a delta exists.** Every
  attribution here rests on a single-variable control, not on reading a profile.
- **Background work does not survive the session that starts it.** One campaign
  was orphaned this way and a duplicate then overlapped it, costing a full sweep.
