# Design: split the in-memory model so nodes stop paying edge-shaped costs

**Task #2192** · round-3 audit finding `r3-node-memory-worse-than-both-incumbents` · drafted 2026-07-26

Status: **design only, not implemented.** This is the research-and-design step that must
precede a representation change, per the project's graph-representation mandate.

---

## 1. The measurement, and the conclusion the audit drew from it

Measured in the round-3 three-way head-to-head:

| | GoGraph | Memgraph | Neo4j |
|---|---|---|---|
| bytes per **edge** | **8.71** | — | — |
| bytes per **node** | **378–423** | 204 | 128 |

Edges are best-in-class. Nodes are **3–3.3× worse than Neo4j** and ~2× worse than Memgraph.

The audit's conclusion is the important part, and it is a design conclusion rather than a
tuning one: **the in-memory model must split.** A representation good enough to reach
8.71 B/edge is being applied to nodes, where it is not the right shape. This task is not
"shave bytes off the node record" — it is "give nodes a different layout from edges, and
accept that the two diverge."

## 2. Where the 378–423 bytes go

This has to be established by measurement before any layout is chosen. The candidates,
from what is already known about the code:

- **Per-node property storage.** Node properties are stored per node; a `map`-shaped or
  boxed-value representation costs a header plus per-entry overhead that dwarfs the payload
  for the common 1–3 property node. Compare with the edge side, which already moved to
  typed columns and reached 8.71 B/edge — the precedent is in-repo.
- **Label storage.** A per-node label bag, where the label *index* already holds the
  inverse mapping as roaring bitmaps.
- **The `Mapper`'s external-key → NodeID table**, which retains the caller's key (a `string`
  for the `Graph[string, float64]` instantiation) plus hash-map overhead.
- **Interning gaps.** Property *keys* are interned (`PropertyKeyRegistry`); property
  *values* and label names may not be uniformly.

**First implementation step: a heap profile attributing the 378–423 B to specific
allocation sites**, published as a table. Choosing a layout before that is guessing, and the
audit's own figure is an aggregate that cannot direct the work.

## 3. Candidate layouts, to be compared not assumed

| Layout | Idea | Cost |
|---|---|---|
| **Struct-of-arrays node record** | parallel arrays indexed by NodeID: labels bitmap ref, property-column refs | random access is fine (NodeID is dense); requires dense NodeIDs, which tombstoning perforates |
| **Property columns for nodes** | reuse the edge-property columnar tier, keyed by NodeID instead of edge pair | the machinery exists and reached 8.71 B/edge — strongest candidate on precedent alone |
| **Label plane from the index only** | drop the per-node label bag; derive a node's labels by querying the label index | trades space for time on `labels(n)`; needs measuring, and #2168's work shows label reads are hot |
| Incumbent record formats | Neo4j's fixed-width store records; Memgraph's delta chain | both assume an on-disk record model GoGraph does not have |

The project mandate requires **at least two candidates compared with measured or cited
trade-offs**. §2's profile decides which two are worth building.

## 4. The obligation that makes this expensive

**A node layout change alters what the durable snapshot contains.** The snapshot writer
serialises node state; recovery reads it back. So this task carries:

- a **format-version bump** and a decision on whether a new binary can open an old
  database — and, if so, a read path for both layouts;
- the snapshot/recovery round-trip tests re-run against the new layout;
- the crash-injection battery re-run, because a partially written node record must still be
  detected as torn rather than read as valid.

That is why this cannot be attempted opportunistically, and why the sequencing below puts a
decision before the code.

## 5. Correctness obligations

1. **Value identity.** A property written and read back must be bit-identical, including
   the int/float distinction the project preserves exactly (CIP2016-06-14) and the temporal
   tagging contract.
2. **Label semantics.** Label order is unspecified but *stability within a snapshot* is
   relied on by tests; `HasNodeLabelByID` must stay allocation-free, since #2186's columnar
   label predicate depends on it.
3. **Tombstone behaviour.** Deleted nodes must remain invisible and must not be resurrected
   by reopening — the defect fixed once already in the tombstone-durability work.
4. **Concurrency.** Read paths must stay lock-free where they are today; a shared node
   record must not reintroduce a per-read lock.

## 6. Validation plan

- Heap profile before and after, with the per-node figure computed the same way the audit
  computed it, so the numbers are comparable to 378–423 B.
- `go test -race ./...`, the snapshot/recovery suites, and the crash-injection battery.
- TCK 3897/3897 — the model change must be invisible to conformance.
- A soak run confirming resident size scales as predicted at 1 M nodes rather than only at
  benchmark size.
- Read-path benchmarks (`labels(n)`, single property, scan) to prove the space win did not
  buy a time loss.

## 7. Sequencing and the decision this needs first

**Blocked on a decision that is not the implementer's to make:** may the on-disk format
change, and in which release? If a minor release must open an old database, the task grows a
dual-read path and roughly doubles.

Should be scheduled **before** #2203 (reader indicator), because it changes what a read
touches and therefore the baseline #2203 is measured against.

Interacts with **#2194** (space reclamation): both rework node storage, and doing them in
either order means reworking the same structures twice. Consider a single planned phase
covering both, with reclamation designed against the *new* layout.
