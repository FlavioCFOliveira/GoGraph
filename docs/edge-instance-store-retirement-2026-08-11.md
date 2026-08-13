# Can the ordinal-keyed edge-instance stores be retired? (rmp #2403)

**Sprint** 339 · **Date** 2026-08-11 · **Status** SPIKE COMPLETE — recommendation below needs a decision

The memory audit measured two near-duplicate per-edge stores costing **977 B/edge together**,
87.9 % of the Cypher relationship write path. Sprint 339's #2401 took them to 242 B/edge by
tiering their inner maps. This spike asks the question that tiering deliberately left alone:
**is one of the two redundant?**

## 1. The answer, from an experiment rather than from reading

`cypher/exec/create_relationship.go` writes every relationship's type and properties through both
surfaces. Deleting the two ordinal-keyed writes and running the suite gives the answer directly:

| | result |
|---|---|
| **openCypher TCK** | **3897 scenarios, 3897 passed, 0 failed, 0 undefined** |
| whole `cypher/...` tree | **exactly one test fails** |

The one failure is `TestMergePattern_ParallelEdgeMultiplicity_MixedTypesFiltered`:

```cypher
CREATE (a)-[:T]->(b)   CREATE (a)-[:T]->(b)   CREATE (a)-[:X]->(b)
MATCH (a:A),(b:B) MERGE (a)-[r:T]->(b) RETURN count(r)   -- must be 2, not 3
```

So the ordinal store is load-bearing for **one behaviour**: MERGE's parallel-edge multiplicity,
filtered by relationship type — `MergePattern.countMatchingInstances` and
`instanceMatchesHop` (`cypher/exec/merge_pattern.go:843-892`), which walk ordinals `1..EdgeCreateCount`
and ask the ordinal store for each instance's labels and properties.

## 2. Every reader, with a verdict

| Reader | Citation | Can the handle store serve it? |
|---|---|---|
| `MergePattern.instanceMatchesHop` | `cypher/exec/merge_pattern.go:875-891` | **No, not as written** — see §3 |
| `MergePattern.countMatchingInstances` | `merge_pattern.go:843-864` | **No** — it counts CREATEs, not slots |
| relationship-type resolution, fallback | `cypher/api.go:13123-13130` | **Yes** — already gated behind `if !handled`, i.e. it runs only when the by-handle lookup found nothing |
| per-slot label resolution | `cypher/api.go:18879-18892` | **Yes** — the multigraph branch resolves a slot, which a handle names precisely |
| type collection over all instances | `cypher/api.go:19038` | **Yes** — same shape |
| `ReadView.EdgeLabelsAt` / `EdgePropertiesAt` | `graph/lpg/readview.go:166,216` | Public API; retiring would be a **breaking change** |
| `graph/csr/order.go:34` | a comment only, describing the handle-less path | n/a |

The experiment corroborates the table: everything marked "Yes" kept passing with the ordinal
writes gone.

## 3. Why RETIRE is not available

**Simple-graph storage collapses N CREATEs into one slot with one handle, and MERGE must still
see N.** `IncEdgeCreateCount`'s own contract says so (`graph/lpg/edge_create_count.go`, and
`cypher/exec/create_relationship.go:166-170`): `MATCH (a:A),(b:B) MERGE (a)-[r:T]->(b) RETURN
count(r)` must yield one row per CREATE *statement*, not one per distinct storage entry
(TCK Merge5 [3]).

Handle enumeration cannot reproduce that count, because the collapsed CREATEs never got a slot to
stamp a handle onto. The ordinal store is keyed by CREATE ORDER precisely so it can. That is what
`cypher/undo_record.go:405-410` already records: *"The per-CREATE-INDEX store is the simple-graph
fallback and is keyed by CREATE order, not by adjacency slot … In multigraph mode the per-handle
store is the authoritative per-instance surface."*

## 4. The two properties the task asked to be demonstrated

Both are pinned by `graph/lpg/instance_vs_handle_demo_test.go`, not argued.

**Deleting a sibling.** Three parallel edges of distinct types; the middle one removed through
the handle-precise path. The handle surface is instance-precise — the removed one reports nothing,
the survivors still resolve to their own types. The ordinal surface is **delete-agnostic**: all
three ordinals still answer, because no removal path mutates that store. That is not a defect in
itself, but it means an ordinal no longer names a *live* edge — which is exactly why a read path
that re-derived the ordinal positionally from slot order broke after a delete, and why the handle
store was introduced.

**Resolving as of a snapshot.** Both surfaces carry their own side-version chains and both
reconstruct a pre-image at a reader's start timestamp. **Retiring the ordinal store would lose no
MVCC capability.**

## 5. What it is worth

Measured on the clean tree against a prototype with the ordinal label write removed, 100 000 nodes
and 800 000 typed relationships created through Cypher (`bench/memprobe`, `TestProbe_CypherEdges`):

| | B/edge |
|---|---:|
| ordinal store on (current) | 323.93 |
| ordinal store off (prototype) | **202.46** |
| **saving** | **121.47 B/edge — a 1.60× reduction (−37.5 %)** |

## 6. Recommendation — KEEP BOTH, BUT GATE

Not RETIRE (§3 forbids it) and not KEEP BOTH unchanged (§5 is too large to leave on the table).

**Write the ordinal store only where it is the authoritative surface — simple-graph storage — and
resolve the MERGE readers through handles in multigraph mode.** `AdjList.Multigraph()` already
exposes the discriminator (`graph/adjlist/adjlist.go:550`), and in multigraph mode every CREATE
gets its own slot and handle, so handle enumeration and CREATE enumeration coincide exactly.

**It is not free, and the cost is one missing primitive.** `WalkEdgeHandles`
(`graph/lpg/edge_handle_durable.go:205`) walks the WHOLE graph, and `exec.GraphMutator` exposes no
per-pair handle enumeration at all. The work is therefore:

1. a per-pair handle enumeration primitive on `Graph` and on `exec.GraphMutator`;
2. `countMatchingInstances` / `instanceMatchesHop` reimplemented on it for the multigraph case,
   keeping the ordinal path for simple-graph storage;
3. gating the two ordinal writes in `CreateRelationship` on `!Multigraph`;
4. `ReadView.EdgeLabelsAt` / `EdgePropertiesAt` keep working — they are public API and this
   proposal does not remove them, only stops *populating* what they read in multigraph mode, which
   is itself an observable change and needs a decision of its own.

Point 4 is the reason this is a recommendation and not a patch: **it narrows a public surface's
behaviour in one storage mode**, which under
[Decision autonomy](../CLAUDE.md#decision-autonomy) is the user's call, not this spike's.

## 7. What was NOT investigated

- **Snapshot and WAL persistence.** Whether the durable formats encode ordinals, and what a
  mode-gated write would mean for recovery of a graph written by an older build, was not
  established. It must be settled before any implementation.
- The **per-instance property** readers were disabled in the same experiment and produced no
  additional failure, but no separate analysis of their readers was done beyond the table in §2.
- No prototype of the *recommended* option was built; §5 measures the upper bound (the store gone
  entirely), which the gated design reaches only in multigraph mode.
