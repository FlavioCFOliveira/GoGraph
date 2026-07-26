# Design: a distributed reader indicator for the visibility barrier

**Task #2203** · round-3 audit finding `r3-rwmutex-reader-counter-anti-scales` · drafted 2026-07-26

Status: **design only, not implemented.** This document is the research-and-design step
that must precede any change to the barrier, per the project's hot-path mandate.

---

## 1. The measurement, and what it does and does not blame

Measured on Apple M4, 10 cores, `benchstat` over 6 runs at `-cpu=1,10`, after #2168
removed the re-entrancy guard from the production read path:

| Path | 1 core | 10 cores | factor |
|---|---|---|---|
| bare `sync.RWMutex` RLock/RUnlock | 3.691 ns | 64.84 ns | **17.6×** |
| `Graph.View` | 4.007 ns | 84.56 ns | **21.1×** |
| realistic read (labels + one property inside `View`) | 34.57 ns | 105.9 ns | 3.06× |

Serially `Graph.View` costs 4.032 ns against the bare mutex's 3.689 ns — **1.09×**, zero
allocations. So the barrier's own bookkeeping is already near-free.

**The attribution matters.** The bare mutex degrades by 17.6×, essentially the same factor
as `View`'s 21.1×. Since the bare mutex has none of the barrier's logic, the residual
cannot be barrier code: it is `sync.RWMutex` itself — one shared `readerCount` word that
every reader atomically increments and decrements, so every core contends the same cache
line and pays a coherence round-trip per acquisition.

This is therefore **not** a "make `View` cheaper" task. `View` is already 9 % over its
primitive. It is a task about replacing the primitive.

## 2. Prior art, and what each one settles

| Design | Reader path | Writer path | Transferable? |
|---|---|---|---|
| Linux `percpu-rwsem` | per-CPU counter, no atomic in the fast path | RCU grace period, then drains every CPU counter | Model yes; needs a stable per-CPU id |
| FreeBSD `rmlock` | per-CPU tracker list, reader pins to its CPU | IPI to each CPU, collects trackers | Same dependency |
| Folly `SharedMutex` | 4–8 sharded reader slots on separate cache lines, chosen by a thread-local index | scans all slots | **Closest fit** — sharding is not per-CPU, it is per-*slot* |
| Rust `parking_lot::RwLock` | single word, but parks instead of spinning | fairness via parking | Reduces the *cost* of contention, not the contention itself |
| Classic big-reader lock | per-CPU flag | sets a global "writer wants in" flag, waits for all flags clear | Model yes; same id dependency |

The literature converges on one idea — **give each reader its own cache line and make the
writer pay to collect them** — and shifts the cost from the read path (frequent) to the
write path (rare), which is exactly the trade a read-mostly graph wants.

Three of the five depend on a stable per-CPU identity. **Go does not provide one.** That is
the decision this document has to settle, because the prior art does not.

## 3. The sharding key: the actual design decision

| Option | Read cost | Problem |
|---|---|---|
| Hashed goroutine id | cheap once obtained | obtaining it needs `runtime.Stack` parsing — **the exact cost #2168 removed**. Non-starter. |
| `runtime_procPin` via `go:linkname` | true per-P, no atomic | unexported runtime dependency; breaks on a runtime change, and the project forbids unjustified `unsafe`/linkname |
| `sync.Pool`-leased slot | pool `Get`/`Put` per acquisition | a `sync.Pool` round-trip per read is likely to cost more than the contended atomic it replaces |
| **Fixed slot array, index from a cheap per-goroutine seed** | one atomic on a *private* line | needs a seed that is stable per goroutine and cheap; candidate: the address of a stack-local, which is per-goroutine-stack and free to obtain |

**Recommendation: the fourth**, with the seed derived from the address of a function-local
variable. A goroutine's stack address is stable for the duration of one `View` call (the
only lifetime that matters), costs nothing to obtain, and distributes across slots without
any runtime internals. Slot count should be the next power of two at or above
`GOMAXPROCS`, capped (16 or 32) so the writer's drain stays bounded.

This must be validated, not assumed: if stack addresses cluster (a real possibility with
stack reuse), the distribution collapses and the design degenerates to the status quo. **The
first implementation step is a distribution measurement, not a lock.**

## 4. Correctness obligations

These are not optional and each maps to a declared invariant.

1. **The writer must observe every slot drained before proceeding.** A missed slot is a
   reader observing a partial write — an **Isolation** violation. The drain needs the
   acquire/release pairing spelled out per slot, and a reader that *begins* after the
   writer sets its intent must be turned away, not admitted into an already-scanned slot.
2. **Writer-preferring admission must be preserved exactly.** The re-entrancy analysis
   depends on it: a queued writer blocks new readers, which is what prevents reader
   starvation of the writer and what the `gograph_debug` suites assert.
3. **No new allocation on the read path.** `View` is currently zero-alloc; a slot lease
   that allocates would be a regression regardless of the contention win.
4. **The barrier's re-entrancy contract is unchanged.** #2168 build-tags the guard out of
   production; this must not reintroduce a per-acquisition identity lookup by the back door
   — which is precisely what option 1 in §3 would do.

## 5. Validation plan

- `BenchmarkBarrier_ViewParallel` at `-cpu=1,2,4,8,10` — the acceptance criterion is
  **flat or falling** per-operation cost, not merely "better".
- Serial `View` must not regress past 4.032 ns and must stay zero-alloc.
- Slot-distribution histogram at each core count, published in the benchmark doc: an even
  spread is the mechanism, so it is evidence, not diagnostics.
- `go test -race ./...` plus the re-entrancy suites under `-tags=gograph_debug`, including
  the queued-writer gate.
- A writer-starvation test: sustained readers must not prevent a writer acquiring within a
  bounded time.
- DST/soak: no goroutine growth, no missed drain under a long mixed workload.

## 6. Sequencing

Independent of the other open findings, so it can be scheduled on its own. But it should
come **after** #2192 (node memory), because a node layout change alters what a read
touches and therefore the realistic-read baseline this task is measured against — doing it
first means measuring twice.

## 7. Explicitly rejected

- **Replacing `RWMutex` with a plain `Mutex`.** Serialises readers; the opposite of the goal.
- **Lock-free reads via an immutable snapshot pointer swap.** This is #1671/#2051, which was
  implemented and **reverted** at 5.4× time and 43× memory. Not revisited here.
- **Optimistic reads with a version counter (seqlock).** Readers would have to re-run on a
  concurrent write, and graph reads return interior pointers (label slices, property maps),
  so a retry cannot be made transparent without copying — which reintroduces allocation.
