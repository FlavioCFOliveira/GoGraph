# Can an MVCC snapshot reader observe an open commit window's adjacency? — 2026-08-05

rmp #2327. Branch `sprint-334`. This is the determination the task asked for, with
file:line evidence, and the outcome of acting on it.

## The question

Two comments in `graph/adjlist/adjlist.go` justified **in-place mutation of a published
slot array** by claiming that every reader holds `visMu.RLock`:

- `loadEntry`: *"Concurrent readers cannot run during a window (it is held under
  visMu.Lock while reads are under visMu.RLock), so the only goroutine that ever
  observes the builder through slotsRef is the window-owning writer itself."*
- `storeEntry`, commit-window dedup: *"sound because the window is held under
  visMu.Lock and F3.2 reads stay under visMu.RLock, so no reader can observe the
  builder while it is mutated."*

Sprint 334 made that premise false. The MVCC read path resolves through a start
timestamp and takes no barrier: the only three `visMu.RLock()` sites left in
`graph/lpg/lpg.go` are a writer's shared hold, `EndVersionedTx`, and the legacy
`Graph.View` API. So: **can a snapshot reader reach `loadEntry` on a shard whose
builder is being mutated in place inside an open commit window?**

## Finding: YES it can — and it is sound, for reasons that are not the stated ones

Reachability is not merely possible in principle; it is **demonstrated**.
`TestLoadEntry_SnapshotReaderReachesAnOpenWindowAndStepsBackOverIt` opens a window,
performs the in-place mutation, and reads the same slot through the present-time path
(`AdjList.LoadEntry` → `loadEntry`), which observes the in-flight state. That read is
the test's positive control, and it is what makes the isolation assertion beside it
meaningful rather than lucky.

The task's dichotomy — *reachable ⇒ defect, unreachable ⇒ stale comment* — is
incomplete. The window is reachable **and** the behaviour is correct. What was wrong
was only the recorded reason.

### What actually makes it safe

Four properties, none of them exclusion:

| # | property | evidence |
|---|---|---|
| 1 | the in-window write is an `atomic.StorePointer`, paired with `loadEntry`'s `atomic.LoadPointer`, so the slot pointer is never torn | `adjlist.go:2515` / `adjlist.go:2343` |
| 2 | an `adjEntry` is **immutable** once published — a write replaces the pointer, never the entry | `storeEntry` publishes a freshly built `entry` on every path |
| 3 | the slot **array** is never resized in place: growth allocates a fresh `shardSlots` and republishes `slotsRef`, so a reader's slice header is stable | `adjlist.go:2488-2496` |
| 4 | **isolation** comes from the version chain: `linkVersion` runs before any branch publishes, so a reader whose start timestamp precedes the commit steps back over the in-flight write | `adjlist.go:2477` |

Property 4 carries the isolation guarantee. Properties 1–3 are what make the read
well-defined at all.

This is the argument the **third** comment block in `storeEntry` (the "F3.5 / #1671
unwind item") already made. The defect was that two older comments contradicted it and
the newer one was not treated as superseding them.

### The checkpointer (AC 4)

Covered by the same determination, and it was never covered by the `visMu` argument
either — `adjlist.go:2552-2556` already records that the non-blocking checkpointer's
`WalkEdgeHandles`/`LoadEntryH` reads adjacency under the store commit lock rather than
`visMu`. It is precisely why the in-window store must be atomic. It is a second
barrier-free reader of the same slot and it is safe for the same four reasons.

### A fourth stale site, found while fixing the first three

`adjlist.go:285` claimed that `building` is *"published into slotsRef only at
EndCommit"* and that *"a reader never observes it"*. **Both halves are false.**
`storeEntry` stores the builder into `slotsRef` on the shard's **first** touch in the
window — deliberately, so the writer gets read-your-own-writes — which is exactly what
makes it reachable by a barrier-free reader. Corrected.

## The instrument was validated against a defective build

A test that has never failed is not evidence. The in-window version link was removed
(`storeEntry`'s `if a.versioning` gated to skip exactly the in-place path), and every
test claimed to cover this was run against that build:

| test | against the defect |
|---|---|
| `TestLoadEntry_SnapshotReaderReachesAnOpenWindowAndStepsBackOverIt` | **FAILS** — reader sees 3 neighbours, want 1 |
| `TestLoadEntry_ConcurrentSnapshotReadersNeverObserveAnOpenWindow` | **FAILS** — 3745 uncommitted counts observed |
| `TestStoreEntry_InPlaceWindowMutationIsSoundUnderVersioning` | **FAILS** |
| `TestStoreEntry_InPlaceWindowMutationIsRaceFree` | **FAILS** |

The injection was then fully reverted (`git diff` clean on `adjlist.go` before the fix
commit).

## A pre-existing test was weaker than its comment claimed

`TestStoreEntry_InPlaceWindowMutationIsRaceFree` — the concurrent companion
`adjlist.go` cites as pinning this very argument — asserted that *"the count must
change, at the commit boundary"*. **It never changed.**

`mvcc.CommitInfo.Commit` only stamps the commit record; the clock's visible frontier
advances through `mvcc.Clock.PublishCommitTS` (`graph/mvcc/mvcc.go:158`), which the
fixture never called. So `clk.ReadTS()` returned 0 on every iteration, the reader read
at start timestamp 0 forever, and the only count it could observe was 0 — the empty
pre-transaction adjacency.

It was still a genuine negative control, as the table above shows: an unversioned
in-window write is visible at any start timestamp, so it does fail against the defect.
What it could not do is distinguish *"the reader correctly stepped back"* from *"the
reader never saw anything at all"*. It now publishes, and a guard enforces that it
observed the committed adjacency, so the claim in its comment is true rather than
aspirational.

**The transferable point:** the new test caught this only because it asserted that its
readers observed **at least two distinct values**. A concurrency test whose oracle is
"nothing bad was seen" should always also assert that something was seen.

## Outcome against the acceptance criteria

1. **Determined** from the code, with file:line evidence — above. A snapshot reader
   *can* reach `loadEntry` inside an open window.
2. Not applicable as written: reachable, but no defect. Two new tests drive the
   barrier-free reader against the in-place mutation under `-race`, and both were
   validated against a defective build.
3. The real reason is documented at all the comment sites, naming the four properties.
4. The checkpointer path is covered by the same determination.
5. **No comment in `graph/adjlist/adjlist.go` still justifies soundness by a claim that
   all readers hold `visMu.RLock`** — four sites corrected, verified by grep.
