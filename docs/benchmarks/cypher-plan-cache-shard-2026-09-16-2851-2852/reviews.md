# Specialist reviews of the sharded plan cache

rmp #2852. Two specialists, consulted **in series** as CLAUDE.md requires:
`concurrency-architect` for the correctness of the concurrent design, then
`go-developer` for Go idiom. Neither modified a file; both reported and I applied
what was in scope.

## concurrency-architect — no concurrency defect

Verdict: **no concurrency defect.** Eight questions were put; all eight came back
sound, and two properties are strictly better than before the change.

| # | question | verdict |
|---|---|---|
| 1 | lock ordering / deadlock freedom in `clear()` | **SOUND** |
| 2 | `clear()` atomicity and the DDL invalidation guarantee | **SOUND** as implemented; one godoc claim overbroad |
| 3 | `Len()` as a sound bound without a global lock | **SOUND**, unconditionally |
| 4 | `metrics.IncCounter` moved outside the shard mutex | **SOUND**, and strictly strengthened |
| 5 | visibility of the published `*planCacheEntry` | **SOUND** in effect; "immutable" was the wrong word |
| 6 | the per-cache `maphash` seed | **SOUND**, no package-level mutable state |
| 7 | `planBuildGroup` unchanged | **SOUND**, no new interleaving |
| 8 | godoc concurrency contract | **DEFECT (documentation)** in the exported package doc |

Points worth keeping:

- **The proof of deadlock freedom is stronger than the lock order.** Every shard
  mutex is a *leaf*: between each `Lock` and `Unlock` the only operations are map
  index/assign/delete, `container/list` calls and a type assertion. Nothing
  acquires another lock, blocks, or calls caller-supplied code — moving
  `metrics.IncCounter` out was what removed the one call that could have reached
  arbitrary user code. The enclosing order from DDL is
  `schemaMu → visMu → {shard₀ … shard₁₅}` and the inverse is structurally
  impossible.
- **`clear()`'s atomicity comes from its three-loop shape**, not from the
  ascending order: acquire-all mutates nothing, so a single-shard observer sees
  either a wholly pre-clear or a wholly post-clear cache. Fusing the reset into
  the acquisition loop would destroy that. This is now stated in the godoc.
- **Moving the metrics emission out of the lock removed two hazards**, not just
  hold time: a `Backend` that blocked on I/O previously did so while holding the
  one global plan-cache mutex, stalling every query in the process, and a
  `Backend` that re-entered the Engine would have self-deadlocked on the
  non-reentrant mutex. The lock is no longer a re-entrancy trap.

### Applied

- `cypher/plan_cache.go` — `clear()`'s godoc no longer claims that *no* observer
  can see a half-cleared cache; `Len()` is named as the one that can, and the
  three-loop shape and the leaf-lock property are stated as the reasons the
  design works. A note records that the list reset and the map clear must stay
  in one lock hold, because `list.List.Init` leaves old elements pointing at the
  list and a survivor would pass `MoveToFront`'s ownership guard.
- `cypher/plan_cache.go` and `cypher/api.go` — "the cached `*planCacheEntry` is
  immutable once published" was **false**: `scalarUse` and `countVarRewrite` are
  filled lazily after publication and carry their own synchronisation. Both
  places now say what is actually true.
- `cypher/api.go` package doc — it still said the plan cache "serialises its
  structural updates on a single `sync.Mutex`" and that "eviction is
  least-recently-used". This change made both false; both are corrected, and the
  per-shard eviction contract is now stated where an embedder reads it.
- `cypher/plan_cache_shard_test.go` — the cache-line test asserted 128 bytes
  unconditionally and would fail on a 32-bit build; it now asserts the property
  on any build and the exact stride on 64-bit. The seed-dependence of the two
  saturation equalities is recorded rather than hidden.

### Reported, NOT acted on

**The DDL invalidation window is wider than `cypher/exec/create_index.go:318-320`
claims.** That comment says the barrier means "no cache can be refilled from the
pre-change catalog between the registration and the invalidation". For the plan
cache that is **false**, because the plan cache's refill path does not take the
barrier: a build can read the index catalog, be overtaken by a DDL registration
plus `clear()`, and then publish an entry compiled against the old catalog.

It is **pre-existing** — the single-mutex version admitted the identical
interleaving — and this change neither widens nor narrows it, which is why it was
not touched. The exposure is bounded: the only schema-dependent field on a
`planCacheEntry` is `paramTypes`, consumed solely by `checkParamTypesCached` for
a parameter type-check verdict. Index selection happens in the physical build
against the live `IndexManager`, so a stale surviving entry cannot make a query
read through a dropped index or miss a new one. Recorded for the coordinator to
schedule, not smuggled into this task.

## go-developer — idiomatic; three fixes, all applied

Verdict: **the implementation is idiomatic Go.** `gofmt -l` and `goimports -l`
print nothing and `golangci-lint run ./cypher/` reports **zero findings in any of
the five reviewed files**.

Two judgements worth keeping:

- **Allocation-freedom was shown statically, not assumed.** `go build -gcflags=-m`
  reports `maphash.String` inlining into `shardFor`, `shardFor` inlining into both
  `get` and `loadOrStore`, and `key does not escape` in both — which is what the
  measured 0 B/op and 0 allocs/op rest on. `shardFor` inlines at cost 70 against
  a budget of 80: ten points of headroom that no test pins, so a future statement
  added to it would silently de-inline the hot path.
- **The `_ [N]byte` padding idiom is the right one** (`x/sys/cpu.CacheLinePad` is
  the same shape), and hard-coding the pad is acceptable *because* the size test
  turns a silent overflow to 192 bytes into a failing test.

### Applied

- `clear(s.by)` in place of the range-delete loop (the module is `go 1.26`).
- `n, limit := 1, min(shards, capacity)` so the clamp loop reads as its own doc
  sentence.
- The size test now uses `unsafe.Sizeof` rather than pointer arithmetic over a
  two-element slice — the spec makes them the same number, and it drops a
  `//nolint:gosec` that `nolintlint.allow-unused: false` would otherwise have
  flagged once unnecessary. Renamed `TestPlanCacheShard_IsCacheLineSized` for
  consistency with its six siblings.
- The `planCacheShards` doc no longer reproduces both measured ladders — it
  states the decision, the direction of each trade and a pointer to
  `shard-count.md`. CLAUDE.md's own test-layer note records what happens when a
  number is copied into two places.
- `DefaultPlanCacheCapacity`'s doc said a negative capacity is "rejected at
  constructor time as a configuration error". Nothing rejects it; it is clamped.
  Corrected, and it now agrees with `EngineOptions.PlanCacheCapacity`'s doc.
- The `planCacheShardPadBytes` doc said 128 is the line size arm64 and amd64
  "share"; they do not (`internal/cpu.CacheLinePadSize` is 128 on arm64, 64 on
  amd64). It now names both and says 128 is the larger.
- `mu` now states what it guards; the broken `[metrics]` doc link is plain text;
  `sameShardKeys` fails loudly on a one-shard cache instead of passing while
  testing nothing; three goroutines no longer pass the loop variable explicitly.

### Reported, NOT acted on

1. **The branch's lint gate is RED, from an earlier commit in this sprint.**
   Independently re-run and confirmed — `golangci-lint run ./cypher/` exits 1 with
   two findings, **both in `cypher/frontend_share_bench_test.go`**, committed at
   `3ab42f2b`:
   - `:205:31 context-as-argument: context.Context should be the first parameter of a function (revive)`
   - `:223:2 var shareSinkErr is unused (unused)`

   Neither is in a file this task touches, and neither is caused by the sharding.
   **It will fail `make ci` at sprint close.**
2. `TestEngineOptions_PlanCacheCapacity_Applied` does not test what its name says:
   it never builds an `Engine` or an `EngineOptions`, it calls `newPlanCache`
   directly, so the `EngineOptions → newPlanCache` wiring is untested and the test
   duplicates `TestPlanCache_NonPositiveCapacity_UsesDefault`. Pre-existing.
3. `newTestEntry(tag string)` discards `tag` while its comment says it "aids
   future debugging" — documented intent the body does not implement.
   Pre-existing.

### Refuted by measurement

The review flagged that the per-insert `c.Len()` in the new bound tests is
O(shards) and could move the short-layer per-package budget, explicitly noting it
had not measured it. Measured: the seven new tests run in **0.02 s** in total
(whole filtered package run, including build, 0.513 s). Not a budget concern; the
oracle stays as it is.
