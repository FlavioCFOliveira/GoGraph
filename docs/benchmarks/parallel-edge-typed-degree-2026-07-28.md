# Typed degree over parallel edges — a silently wrong count (rmp #2241)

**Date:** 2026-07-28 · **Sprint:** 327 · **Host:** Apple M4 (10 cores, 32 GB).

## 1. The defect

The typed degree rewrite shipped by #2232 (sprint 326) under-counted parallel edges. On a
multigraph with three `:K` edges and one `:M` edge between the same pair:

```
MATCH (a {id: 0}) RETURN COUNT { MATCH (a)-[r:K]->(b) RETURN r }   -> 3   correct (enumerates)
MATCH (a {id: 0}) RETURN COUNT { (a)-[:K]->() }                    -> 1   WRONG
MATCH (a {id: 0}) RETURN COUNT { (a)-->() }                        -> 4   correct (untyped)
```

A wrong answer, not a slow one, on a graph shape the openCypher TCK model requires — the
Cypher engine warns when it is constructed over a non-multigraph graph, so nearly every real
Cypher graph is affected.

## 2. Root cause, measured

The adjacency holds four slots for the pair, but only the **first** carries an encoded
relationship label:

```
OutDegreeByID (untyped)        = 4
OutDegreeByTypeBoundedByID(:K) = 1
  slot -> dst=165 encodedLabel=3     (encodeSlotLabel(:K))
  slot -> dst=165 encodedLabel=0
  slot -> dst=165 encodedLabel=0
  slot -> dst=165 encodedLabel=0
```

`AdjList.SetEdgeLabelSlot` scans src's neighbours for the **first** slot matching dst and stops
there, so `SetEdgeLabel` called once per parallel CREATE writes the same slot four times. The
label column can therefore never distinguish parallel edges of a pair.

The authoritative per-edge type is the one `CreateRelationship` records against the slot's
**handle** — `AddEdgeH` stamps a distinct handle per CREATE and `SetEdgeLabelByHandle` records
the type against it, precisely because a positional index does not survive the deletion of a
parallel sibling (`graph/lpg/edge_handle.go` file comment).

## 3. The fix

`Graph.slotCarriesType` consults the handle store first and falls back to the label column:

- **Handle store, when it has a record** — authoritative, and free of the collision.
- **Label column otherwise** — and it is not vestigial. Simple-graph storage collapses a
  duplicate pair and stores no handle, and an edge added through the Go API
  (`Graph.AddEdgeLabeled`) carries a slot label with no handle record. In both cases a pair
  holds at most one edge, so the collision cannot arise.

Probing the handle store needs the slot's handle, which `AdjList.OutDegreeFuncBoundedByID` does
not expose, so the walk moved to `AdjList.LoadEntryH`. All four typed walkers —
`OutDegreeByType`, `OutDegreeByTypeBounded`, `OutDegreeByTypeBoundedByID` and the new
`OutDegreeMatchingBoundedByID` — now share one body, `outDegreeMatchingByID`, so the bounded
and unbounded forms cannot disagree about *which* edges count. That shared-predicate guarantee
was already documented on the bounded walkers; it is now structural.

`edgeHandleHasLabel` is the allocation-free, `LabelID`-level probe added for this:
`EdgeLabelsByHandleID` resolves every id back to a name and builds a slice, which a per-slot
walk over a hub's adjacency cannot pay.

## 4. Cost of the fix

`bench/r4audit` `TestPerOuterRowCost_DegreeEligible`, µs per outer row at N=8000. The baseline
arm is the same binary with the handle probe disabled, so only this change differs.

| Shape | Column-only (wrong) | Handle-aware (correct) |
|---|---|---|
| `baseline-scan` | 0.028 | 0.027 |
| `ELIGIBLE count>0 untyped` | 0.485 | 0.447 |
| `ELIGIBLE count>0 typed` | 0.477 | 0.493 |
| `ELIGIBLE exists typed` | 0.258 | 0.255 |
| `ELIGIBLE count=n typed` | 0.527 | 0.504 |
| `ELIGIBLE size(pattern)` | 2.202 | 2.200 |

The typed shapes move by 3–5 % in both directions — this harness is a single-run timer, not
`benchstat`, and the untyped and `count=n` shapes moved the *other* way, so the spread is
noise. There is no material regression: the handle probe costs one uncontended mutex
acquisition per slot, and only on a graph that has handles at all.

## 5. A second defect found alongside it

`EXISTS { pattern WHERE predicate }`, evaluated as an **expression**, discarded its inline
WHERE entirely:

```
MATCH (a:P {id: 1}) RETURN EXISTS { (a)-[:K]->(b) WHERE b.id = 999 }   -> true   WRONG
```

A predicate that can never hold, answered true. `existsToSingleQuery` built the inner
`ast.Match` from `sub.Pattern` and never threaded `sub.Where`. The WHERE-**position** spelling
was unaffected, because the planner lowers it to a SemiApply carrying its own Selection — which
is why only an expression-position case exposes it. Fixed by threading the clause; the COUNT
half of the same family is tracked separately (rmp #2242), because `ast.CountSubquery` has no
`Where` field for the parser to populate at all.

## 6. Why the original suite missed both

`cypher/degree_rewrite_test.go`'s `degreeFixture` never creates two edges between the same
pair, so no case in the shipped suite exercises a parallel edge. And every EXISTS case that
carried an inline WHERE happened to choose a predicate the bare pattern also satisfied, so the
discarded clause changed no verdict.

Both were found while implementing #2235, whose acceptance criteria demand a parallel-edge
multigraph case and a differential control. The **absolute oracle** is what caught them: the
rewritten and control forms disagreed, and a hand-computed value said which was right.

## 7. Gates

- `make ci` green: tidy, fmt, vet, build, `go test -race` short layer, `golangci-lint`,
  cover-gate.
- openCypher TCK **3897/3897 scenarios, 0 failed, 0 undefined**.
- `TestDegreeRewrite_ParallelEdges_*` and `TestExistsSubquery_HonoursInlineWhere` both fail on
  the pre-fix behaviour and pass after, verified by reverting each fix in turn.
