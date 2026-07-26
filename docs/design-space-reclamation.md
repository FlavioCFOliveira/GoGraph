# Design: return space from deleted nodes and edges

**Task #2194** · round-3 audit finding `r3-no-space-reclamation` · drafted 2026-07-26

Status: **design only, not implemented.**

---

## 1. The problem

A deleted node is **tombstoned**, not removed: the `Mapper` entry survives so a stale
`NodeID` resolves, scans skip it via `IsTombstoned`, and the durable tombstone set keeps it
dead across reopen. Nothing ever returns the space.

Consequence: a database under a sustained create/delete workload grows **monotonically**.
Resident size and on-disk size track the *high-water mark* of live entities, not the current
count. A queue-shaped workload — insert, process, delete — is the worst case and a
completely ordinary one.

This is a missing **feature**, not a defect: nothing is incorrect today, it simply never
shrinks. That framing matters for how it is scheduled, because there is no failing test to
point at and therefore no natural forcing function.

## 2. What must be true of any strategy

Reclaiming a slot means making it reusable. That is only safe when the slot is unreachable
by **all four** of these, and each is a distinct hazard:

1. **Any live reader.** Readers run under `Graph.View`; a slot freed while a reader holds an
   interior pointer into it is a use-after-free in Go terms (a stale value, not a crash) —
   an **Isolation** violation.
2. **Any replayable WAL record.** Recovery replays from the last checkpoint. If a record
   references a slot that has since been reused for a different entity, replay writes the
   old entity's data into the new one — **Consistency** loss, and silent.
3. **Any durable snapshot** still serving as a recovery base.
4. **Any retained `NodeID` in a client's hands.** `id()`/`elementId()` are documented stable
   *for the lifetime of the entity*; reuse must not make a stale id resolve to a **different**
   entity, which would turn a stale-read bug into a wrong-entity bug.

Hazard 4 is the one most likely to be overlooked and the most damaging: it converts an
obvious error into a plausible wrong answer.

## 3. Candidate strategies

| Strategy | Mechanism | Verdict |
|---|---|---|
| **Free-list reuse** | tombstoned NodeIDs onto a free list, reissued to new nodes | Cheapest, but hazard 4 is fatal as stated: a reissued id makes a client's retained id point at a different entity. Would need a generation counter in the id, i.e. an id-format change. |
| **Compacting checkpoint** | the checkpointer writes only live entities, renumbering; recovery reads the compacted form | Hazards 2 and 3 handled by construction — the checkpoint *is* the new base and the WAL is truncated at it. Hazard 1 handled by doing it at a barrier. Hazard 4 still needs a decision on id stability across checkpoints. **Strongest candidate.** |
| **Segment rewrite** | rewrite individual WAL/snapshot segments dropping dead entries | Incremental, so no stop-the-world; but needs per-segment liveness tracking and a segment-aware recovery, and the audit's own WAL-segmentation item (#2195) is unbuilt. |
| **In-place `Mapper` shrink** | drop tombstoned keys, keep NodeIDs | Returns only the key table, which the §2 profile of #2192 may show is a minority of the footprint. Cheap, partial, and safe — a viable **phase 1**. |

## 4. Recommendation: phase it, cheapest-and-safest first

**Phase 1 — measure.** There is no figure for how much space tombstones actually hold. A
delete-heavy soak that reports resident and on-disk size against live-entity count, before
any reclamation exists. Without it there is no way to tell a successful reclamation from a
no-op, and no way to size the work.

**Phase 2 — `Mapper` key shrink.** Reclaim the external-key table for tombstoned nodes,
keeping NodeIDs assigned. Sidesteps hazard 4 entirely because no id is reused. Partial by
design, and its value is exactly what phase 1 measures.

**Phase 3 — compacting checkpoint**, if phase 1 shows the remaining footprint justifies it,
and only after the id-stability decision below.

Phasing is not caution for its own sake: phases 1 and 2 are independently shippable and
carry none of the hazards that make phase 3 expensive.

## 5. The decision this needs first

**May a NodeID be reused after the entity is deleted?**

- **No** (recommended): reclamation can never reuse ids, so free-list reuse is off the table
  and the compacting checkpoint must preserve the id → entity mapping while dropping dead
  entries. More constrained, but `id()`/`elementId()` keep their documented meaning.
- **Yes, with generations**: ids gain a generation field so a stale id is *detectably* stale
  rather than silently wrong. Unlocks free-list reuse, but changes the id format — which
  `elementId()` renders in decimal and which #2189 puts on the Bolt wire, so it is a
  client-visible change.

This is not an implementation detail; it determines which strategies are legal.

## 6. Validation plan

- Delete-heavy soak: resident and on-disk size must **fall**, not merely stop growing.
  Reported as a table against live-entity count, which is the claim being made.
- Crash injection with a crash **during** reclamation, in every window the strategy opens.
- Snapshot/recovery round-trip after reclamation: a reclaimed database must reopen with
  exactly the live set.
- A stale-id test: an id retained across a delete must not resolve to a **different**
  entity. Under "no reuse" it must resolve to nothing.
- `go test -race ./...`, TCK 3897/3897.

## 7. Sequencing

Pair with **#2192** (node memory). Both rework node storage, and doing them in either order
means reworking the same structures twice; reclamation should be designed against the new
layout. Phase 1 (measure) can run immediately and independently, and should — it is the
input to both.
