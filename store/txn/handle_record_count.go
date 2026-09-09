package txn

// handle_record_count.go — the commit-time bound on how many LABELS, or
// PROPERTIES, one edge-handle record may carry (rmp #2784).
//
// It is the COUNT sibling of the fold bound rmp #2750 put on a property
// VALUE's length (txn.go, [checkSnapshotFoldableLen]), and exists for exactly
// the same reason: store/snapshot refuses to capture a record it could not
// write, the checkpointer's phase-2 failure returns before phase 3 truncates
// the WAL prefix, and a value already committed therefore blocks every
// checkpoint from then on while the WAL grows without bound.

import "github.com/FlavioCFOliveira/GoGraph/graph/lpg"

// maxSnapshotPerRecordCount is the module's cap on the number of LABELS, or of
// PROPERTIES, carried by ONE edge-handle record: 1 Mi.
//
// It is the same number as store/snapshot's maxPerRecordCount (lenguard.go),
// which is the ceiling readEdgeHandleRecord enforces and is therefore the
// largest per-handle record edgehandles.bin can carry. The two are separate
// declarations of one number, because store/txn does not import store/snapshot;
// TestSnapshotPerRecordCountCapAgreement_2784 pins each side to the literal and
// names the other, so neither can be moved alone — the same pinning rmp #2750
// established for [maxSnapshotValueLen].
//
// # Why the transaction layer enforces a CHECKPOINT bound (rmp #2784)
//
// Measured end to end, against a build with store/snapshot's cap lowered to 64
// so the mechanism could be exercised at a cost this machine can pay: an edge
// handle carrying cap+1 properties COMMITTED, and the next three checkpoints
// all failed with
//
//	snapshot: capture edgehandles.bin: snapshot: field too long for the
//	reader's cap: edge handle property count is 65 bytes, maximum 64
//
// while the WAL stayed at 3362 bytes — never truncated, and growing for as long
// as that record lived. The at-cap control committed, checkpointed, and
// truncated the WAL to 0. At the REAL cap the same writer refuses at exactly
// 1_048_577 and accepts at 1_048_576, for the property count and the label
// count alike (measured directly against writeEdgeHandleRecord).
//
// Accepting a write that can never be folded is a bounded-resources defect
// under the module's concurrency mandate and a durability trap under its ACID
// mandate, so the refusal belongs at COMMIT, where the caller can still act on
// it.
//
// # Why this bound cannot be derived from any other
//
// Nothing else on the write path bounds the count. A property is one op, and
// [DefaultMaxTxnOps] admits 16_000_000 of them; the ops need not even share a
// transaction, since a handle's bag accumulates across commits. Each op is its
// own WAL frame of about 53 bytes (measured), so [wal.ErrFrameTooLarge] is
// orders of magnitude away. [lpg.PropertyKeyID] and [lpg.LabelID] are both
// uint32, so the registries do not bind either. The per-record count is written
// behind a uint32 prefix that 1 Mi does not overflow, so nothing truncates and
// nothing complains until capture refuses.
const maxSnapshotPerRecordCount = 1 << 20

// checkSnapshotFoldableCount rejects a per-edge-handle label or property count
// above [maxSnapshotPerRecordCount] — a record that would commit durably and
// then make every checkpoint fail for as long as it lived (rmp #2784).
//
// It takes the count rather than the record so the bound can be pinned at its
// exact boundary without materialising a million properties, the way
// store/snapshot's own checkSnapshotPerRecordCount is pinned.
func checkSnapshotFoldableCount(what string, n int) error {
	if n > maxSnapshotPerRecordCount {
		return errFieldTooLong(what, n, maxSnapshotPerRecordCount)
	}
	return nil
}

// handleRecordKey identifies the one edge-handle record a by-handle op acts on.
// It is comparable because N is, so it serves directly as a map key.
type handleRecordKey[N comparable] struct {
	src, dst N
	handle   uint64
}

// handleRecordSim is the post-commit label and property count of ONE edge
// handle, held as a DELTA over what the graph already holds rather than as a
// rebuilt key set.
//
// # Why a delta and not a set
//
// The obvious shape — copy the handle's keys into a set and fold the staged ops
// into it — costs a map allocation and a copy on every commit that writes a
// relationship property, which is the single commonest write the Cypher engine
// makes. The delta costs neither: `added` and `removed` stay nil unless the
// transaction genuinely introduces or removes a key, so `SET r.x = 1` on a
// property the relationship already has allocates nothing here at all.
// Measured, against the same tree with the guard unwired: 16 allocs/op and
// 1074 B/op for the set shape against 10 and 344 without the guard; the delta
// shape brings that back to the numbers recorded on
// [Tx.checkFoldableHandleRecords].
//
// It also keeps the guard READ-ONLY with respect to the graph. Folding the ops
// into the map [lpg.Graph.EdgePropertiesByHandle] returns would save the same
// allocation, but only by relying on that map being freshly built and owned by
// the caller — which lpg's contract does not promise. A delta needs no such
// assumption.
//
// Each dimension is loaded LAZILY, on the first staged op that touches it: a
// Cypher `SET r.x = 1` stages a property op and no label op, and
// `CREATE ()-[:T]->()` stages a label op and no property op, so a lazy sim reads
// ONE of the two bags per commit instead of both.
type handleRecordSim struct {
	// baseProps is the handle's current property map, read once and never
	// written. baseLabels is the same for its labels.
	baseProps  map[string]lpg.PropertyValue
	baseLabels []string
	// addedProps / addedLabels are the names this transaction introduces that
	// the base does not hold; removedProps the base names it deletes. Each is
	// allocated only when a name actually lands in it.
	addedProps   map[string]struct{}
	removedProps map[string]struct{}
	addedLabels  map[string]struct{}
	propsLoaded  bool
	labelLoaded  bool
}

// propCount is the number of properties the handle would carry after this
// transaction: what it holds now, less the base keys removed, plus the new ones.
func (s *handleRecordSim) propCount() int {
	return len(s.baseProps) - len(s.removedProps) + len(s.addedProps)
}

// labelCount is [handleRecordSim.propCount] for labels. There is no removal op
// for a per-handle label — nothing in the WAL vocabulary drops one name from the
// bag — so only the retire kinds shrink it, and they do so by clearing the base.
func (s *handleRecordSim) labelCount() int {
	return len(s.baseLabels) + len(s.addedLabels)
}

// checkFoldableHandleRecords refuses, before a sequence is minted or a byte is
// written, a transaction that would leave any edge handle carrying more labels
// or more properties than store/snapshot can capture (rmp #2784).
//
// # Why the whole transaction and not each op
//
// The bound is on a quantity NO SINGLE OP CARRIES. rmp #2750's fold bound is a
// pure function of the staged value, so it lives in the encoder; a count is a
// property of the resulting GRAPH — the handle's existing bag, plus the names
// this transaction adds, minus the ones it removes. It is therefore computed
// once per transaction, here, beside the [ErrTransactionTooLarge] check that is
// its structural precedent: both refuse a whole transaction before it can
// consume a sequence or reach the disk.
//
// # What it costs, measured
//
// Interleaved benchstat pairs on an Apple M4, against the same tree with the
// call removed from [Tx.appendOnly], 6 pairs each:
//
//	full commit, one by-handle property   no significant difference (3.700m vs 3.698m, p=0.589)
//	full commit, one node property        no significant difference (3.646m vs 3.645m, p=1.000)
//
// The wall-time figure is decided by the fsync that follows, which costs six
// orders of magnitude more than this walk. What the guard does cost is
// allocations on the by-handle path, and only there: a transaction that stages
// no [OpSetEdgePropertyByHandle] and no [OpSetEdgeLabelByHandle] is turned away
// by [stagesHandleRecordGrowth] after one branch per op and allocates nothing —
// measured at 9 allocs/op and 317 B/op with and without the guard, all samples
// equal.
//
// # Exactness
//
// The simulation replays the five by-handle op kinds IN ORDER against the
// handle's current bags, so a transaction that deletes a key and re-adds it, or
// that clears the instance and repopulates it, is measured at the count it
// actually leaves behind. Over-restriction would be a new defect, not a fix.
//
// It does NOT model [OpRemoveEdge], which names no handle and only sometimes
// strips per-handle metadata. Ignoring it can only make the simulated count too
// LARGE, and only for a transaction that removes a pair and then writes more
// than 1 Mi properties onto a handle of that same pair in the same commit.
//
// # What it cannot promise
//
// The read is taken before the ops are applied, so two transactions racing to
// grow the SAME handle can each pass the check and together cross the cap. The
// bound is slack by six orders of magnitude against anything an engine caller
// produces, and this is a fail-stop against a pathological accumulation rather
// than a security boundary, so the residual race is documented rather than
// closed with a lock the commit path would pay for on every write.
func (t *Tx[N, W]) checkFoldableHandleRecords() error {
	if !stagesHandleRecordGrowth(t.ops) {
		return nil
	}
	sims := make(map[handleRecordKey[N]]*handleRecordSim)
	// order keeps the refusal deterministic: with two offending handles in one
	// transaction, map iteration would name a different one from run to run and
	// the error would not be testable.
	var order []handleRecordKey[N]
	for i := range t.ops {
		op := &t.ops[i]
		switch op.Kind {
		case OpSetEdgePropertyByHandle, OpDelEdgePropertyByHandle,
			OpSetEdgeLabelByHandle, OpRemoveEdgeInstanceByHandle, OpRemoveEdgeByHandle:
		default:
			continue
		}
		key := handleRecordKey[N]{src: op.Src, dst: op.Dst, handle: op.Handle}
		sim, ok := sims[key]
		if !ok {
			sim = &handleRecordSim{}
			sims[key] = sim
			order = append(order, key)
		}
		t.applyToHandleSim(sim, op)
	}
	for _, key := range order {
		sim := sims[key]
		if sim.propsLoaded {
			if err := checkSnapshotFoldableCount("edge handle property count", sim.propCount()); err != nil {
				return err
			}
		}
		if sim.labelLoaded {
			if err := checkSnapshotFoldableCount("edge handle label count", sim.labelCount()); err != nil {
				return err
			}
		}
	}
	return nil
}

// stagesHandleRecordGrowth reports whether ops contains an op that can make an
// edge handle's label or property record LARGER. Only those two kinds can push a
// record over the cap; a transaction holding neither cannot fire the guard, so
// it is not worth building a simulation for.
func stagesHandleRecordGrowth[N comparable, W any](ops []Op[N, W]) bool {
	for i := range ops {
		switch ops[i].Kind {
		case OpSetEdgePropertyByHandle, OpSetEdgeLabelByHandle:
			return true
		}
	}
	return false
}

// applyToHandleSim folds one staged by-handle op into sim, loading the dimension
// it touches from the graph on first use.
func (t *Tx[N, W]) applyToHandleSim(sim *handleRecordSim, op *Op[N, W]) {
	switch op.Kind {
	case OpSetEdgePropertyByHandle:
		t.loadHandleProps(sim, op)
		switch {
		case sim.removedProps != nil && contains(sim.removedProps, op.Key):
			// Deleted earlier in this same transaction and now written again:
			// it is back in the base, not a new key.
			delete(sim.removedProps, op.Key)
		case hasKey(sim.baseProps, op.Key):
			// Already counted by baseProps; a write is an overwrite.
		default:
			if sim.addedProps == nil {
				sim.addedProps = make(map[string]struct{})
			}
			sim.addedProps[op.Key] = struct{}{}
		}
	case OpDelEdgePropertyByHandle:
		t.loadHandleProps(sim, op)
		switch {
		case sim.addedProps != nil && contains(sim.addedProps, op.Key):
			delete(sim.addedProps, op.Key)
		case hasKey(sim.baseProps, op.Key):
			if sim.removedProps == nil {
				sim.removedProps = make(map[string]struct{})
			}
			sim.removedProps[op.Key] = struct{}{}
		}
	case OpSetEdgeLabelByHandle:
		t.loadHandleLabels(sim, op)
		if containsName(sim.baseLabels, op.Label) {
			return
		}
		if sim.addedLabels == nil {
			sim.addedLabels = make(map[string]struct{})
		}
		sim.addedLabels[op.Label] = struct{}{}
	case OpRemoveEdgeInstanceByHandle, OpRemoveEdgeByHandle:
		// Both retire the handle's per-handle metadata, so whatever the bags held
		// is gone. Marking each dimension loaded-and-empty is what keeps a later
		// Set in the same transaction from re-reading the graph and resurrecting
		// the names this op removed.
		*sim = handleRecordSim{propsLoaded: true, labelLoaded: true}
	}
}

// loadHandleProps reads the handle's current property map once. The map is held
// and never written: see [handleRecordSim].
func (t *Tx[N, W]) loadHandleProps(sim *handleRecordSim, op *Op[N, W]) {
	if sim.propsLoaded {
		return
	}
	sim.baseProps = t.store.g.EdgePropertiesByHandle(op.Src, op.Dst, op.Handle)
	sim.propsLoaded = true
}

// loadHandleLabels reads the handle's current label names once.
func (t *Tx[N, W]) loadHandleLabels(sim *handleRecordSim, op *Op[N, W]) {
	if sim.labelLoaded {
		return
	}
	sim.baseLabels = t.store.g.EdgeLabelsByHandle(op.Src, op.Dst, op.Handle)
	sim.labelLoaded = true
}

// contains reports membership in a name set.
func contains(m map[string]struct{}, k string) bool { _, ok := m[k]; return ok }

// hasKey reports membership in the graph's property map.
func hasKey(m map[string]lpg.PropertyValue, k string) bool { _, ok := m[k]; return ok }

// containsName reports membership in the graph's label slice.
//
// It scans rather than indexing a set, because the slice is almost always one
// or two names long and building a set for it would cost the very allocation the
// delta shape exists to avoid. The scan is O(len(baseLabels)) per staged label
// op, which is bounded by the cap this function helps enforce.
func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
