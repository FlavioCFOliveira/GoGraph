# `shortestPath` admitted an edge its type filter excludes (rmp #2236, obstacle 2)

**Date:** 2026-07-28 · **Sprint:** 327.

A correctness record. #2236's technical requirements ordered this investigated **first** and
resolved **separately** from the performance work, and treated as a defect until proven
otherwise. It is a defect.

## 1. What #2220 saw, and could not explain

Sprint 326's bidirectional `shortestPath` (#2220) recorded a blocker on `canBidirectional`:

> Adding a relationship-type filter to the differential suite showed the two searches
> disagreeing under DirIn and DirBoth — and, more importantly, showed the **forward-only
> reference** returning a path whose hops use an edge the filter excludes. Both algorithms
> agreeing on a path that the filter should have rejected points at the shared reverse-slot
> type check, not at the new code.

That reading was right, and the shared check is where the defect was.

## 2. Root cause: a sentinel that cannot mean what it is asked to mean

`resolvedFwdPosOrSelf(revPos)` maps a reverse-CSR position to its forward counterpart and
"signals failure" by returning `revPos` unchanged. `passesTypeFilter` then read that back as

```go
fwdPos = op.resolvedFwdPosOrSelf(pos)
if fwdPos == pos {
    return true // "cannot type-check it; keep it permissive"
}
```

The sentinel is drawn from the same space as the answer, so it conflates two different
outcomes:

- the mapping is genuinely **unknown** (no `revToFwd`, out of range, or the `unresolvedFwdPos`
  marker) — where staying permissive is the documented, defensible choice; and
- the mapping is **known and happens to equal `revPos`**.

The second case is not exotic. With a single edge `0→1` both CSRs hold exactly one slot at
index 0, so `revToFwd[0] == 0`. Every such slot skipped the filter entirely and was admitted
regardless of type.

## 3. Reproduction

`TestShortestPath_TypeFilterRejectsAnExcludedReverseSlot`, on the two-node one-edge graph with
an **empty** admit set, so no edge may be traversed and no path can exist:

| Direction | Before | After |
|---|---|---|
| `DirOut` | correctly no path | no path |
| `DirIn` | **path found over the excluded edge** | no path |
| `DirBoth` | no path | no path |

`DirBoth` passed before the fix only because this fixture's reverse side is empty from the
source — an accident of the graph, not correctness. Each case is paired with an
**admitted** control that must find the path, so the excluded case cannot pass by the search
being broken outright.

`TestShortestPath_TypeFilterPartialAdmissionAcrossDirections` widens it to a 3-hop chain with
the middle hop excluded, so the rejection has to happen inside frontier expansion rather than
at the seed. Same verdict: `DirIn` routed over the excluded hop before the fix.

## 4. The fix

`resolveFwdPosKnown(revPos) (uint64, bool)` returns the mapping **and whether it is known**.
`passesTypeFilter` consults the boolean; `resolvedFwdPosOrSelf` is kept, delegating to it, for
the callers that genuinely want a usable position either way — path hydration, where the
reverse position is a serviceable stand-in.

`AllShortestPaths` carried the identical check and therefore the identical wrong answer, and is
fixed the same way. #2236's acceptance criterion 4 asks that `allShortestPaths` stay untouched;
that is a bar on *widening the two-sided search* into it, not a licence to leave a wrong answer
in a shared predicate.

## 5. What this unblocks, and what it does not

Obstacle (2) is closed: the forward-only walk's `DirIn` / `DirBoth` type filter is now correct,
so the differential suite can be restored across all three directions on a foundation that is
no longer suspect.

It does **not** on its own admit the two-sided search for typed or reverse-direction shapes.
That remains blocked on obstacle (1) — an exact reverse type check costing O(1) per slot —
because the two options #2220 measured were rejected: the prebuilt `revToFwd` table cost more
than the search saved (277.7 → 351.1 ms end to end at N=20000, a 26 % regression), and the
per-slot forward-CSR scan had the unresolvable case this fix has now made *visible* rather than
silently permissive. Note that the fix changes the calculus: with resolution status reported
honestly, a per-slot scan is no longer "occasionally wrong" — it is correct but conservative,
falling back to permissive only where the mapping truly is unknown.

## 6. Gates

- `make ci` green: tidy, fmt, vet, build, `go test -race` short layer, `golangci-lint`,
  cover-gate.
- openCypher TCK **3897/3897 scenarios, 0 failed, 0 undefined**. No TCK scenario covers a
  type-filtered reverse-direction `shortestPath`, which is why the suite was green throughout.
- Both regression tests fail on the pre-fix behaviour.
