package lpg

// mvcc_reclaim.go — MVCC P6 (rmp #2284): reclaiming the node-label and
// node-property version chains.
//
// # The rule, and why it is the same one the adjacency uses
//
// A delta is an UNDO record: a reader applies it only when the change it
// records is NOT visible to that reader. A delta stamped t is invisible only to
// a reader whose start timestamp is older than t. So once every active reader
// began at or after t — that is, once the watermark reaches t — no reader
// applies it, and neither it nor anything behind it in the chain can affect any
// answer.
//
// The chain runs newest-first, so the FIRST delta at or before the watermark
// makes the whole tail behind it unreachable, and one truncation releases all
// of it. That is the identical argument the adjacency reclaimer makes; the
// mechanics differ only because these chains hang off a sparse map guarded by
// the shard's RWMutex rather than off an immutable published entry, so the
// reclaimer can take the write lock and mutate plainly instead of needing an
// atomic store.
//
// # Cost
//
// O(nodes carrying history), not O(nodes): the sparse side maps ARE the index
// of what has versions, so a shard with no history costs one nil check.

// ReclaimVersions frees every VERSION CHAIN record that no reader can reach any
// more — node labels, node properties and the per-edge side stores — and
// returns how many were released.
//
// Node EXISTENCE records are deliberately NOT included: they are a pair of
// instants rather than a value history, they are swept by [Graph.ReclaimNow]
// alongside the adjacency, and folding them in here would make this function's
// count mean two different things.
//
// watermark is the oldest start timestamp among active readers, from
// [mvcc.Horizon.Oldest]. Zero means reclaim nothing, which is what the horizon
// reports while a reader could not be registered — the sound answer when the
// oldest reader is unknown.
//
// Safe for concurrent use with readers. Not safe to run concurrently with
// itself.
func (g *Graph[N, W]) ReclaimVersions(watermark uint64) int {
	if watermark == 0 {
		return 0
	}
	return g.reclaimLabelVersions(watermark) +
		g.reclaimPropVersions(watermark) +
		g.reclaimEdgeSideVersions(watermark)
}

// reclaimLabelVersions truncates the label chains.
func (g *Graph[N, W]) reclaimLabelVersions(watermark uint64) int {
	// The depth histogram is reset even on the nothing-to-do path, so a store with
	// no live versions reports an empty distribution rather than the one its last
	// sweep left behind. See [mvcc.DepthHist].
	hist := g.depth(depthNodeLabels)
	hist.Reset()
	if g.labelDeltaActive.Load() == 0 {
		return 0
	}
	freed := 0
	for i := range g.nodeLabelShards {
		sh := &g.nodeLabelShards[i]
		sh.mu.Lock()
		if sh.d == nil {
			sh.mu.Unlock()
			continue
		}
		for id, head := range sh.d {
			if head == nil {
				delete(sh.d, id)
				continue
			}
			// ABORTED heads first (rmp #2318). They can never satisfy the
			// watermark test below — AbortedTS is the maximum uint64 — and they
			// are the only thing masking the aborted transaction's writes from
			// the stored bag, so they are withdrawn by restoring the bag rather
			// than merely dropped. See mvcc_abort_reclaim.go.
			if n := g.reclaimAbortedLabelsLocked(sh, id); n > 0 {
				freed += n
				if head = sh.d[id]; head == nil {
					continue
				}
			}
			// The head itself unreachable means the node keeps no history at
			// all, so the map entry goes with it and the shard shrinks.
			if head.stampTS() <= watermark {
				freed += releaseLabelChain(head, &g.labelChurn)
				delete(sh.d, id)
				continue
			}
			// retained counts the records this walk LEAVES on the chain — the depth a
			// reader arriving now would step through. It is the walk's own loop
			// counter, so measuring it costs a register increment on the sweeper.
			retained := 1
			for d := head; d.next != nil; d = d.next {
				if d.next.stampTS() <= watermark {
					freed += releaseLabelChain(d.next, &g.labelChurn)
					d.next = nil
					break
				}
				retained++
			}
			hist.Observe(retained)
		}
		if len(sh.d) == 0 {
			sh.d = nil // a shard with no history costs one nil check again
		}
		sh.mu.Unlock()
	}
	if freed > 0 {
		g.labelDeltaActive.Add(-int64(freed))
	}
	return freed
}

// reclaimPropVersions truncates the property chains, by the identical rule.
func (g *Graph[N, W]) reclaimPropVersions(watermark uint64) int {
	hist := g.depth(depthNodeProps)
	hist.Reset()
	if g.propDeltaActive.Load() == 0 {
		return 0
	}
	freed := 0
	for i := range g.nodePropShards {
		s := &g.nodePropShards[i]
		s.mu.Lock()
		if s.d == nil {
			s.mu.Unlock()
			continue
		}
		for id, head := range s.d {
			if head == nil {
				delete(s.d, id)
				continue
			}
			// ABORTED heads first, by the identical argument as the label
			// chains; see [Graph.reclaimAbortedPropsLocked].
			if n := g.reclaimAbortedPropsLocked(s, id); n > 0 {
				freed += n
				if head = s.d[id]; head == nil {
					continue
				}
			}
			if head.stampTS() <= watermark {
				freed += propChainLen(head)
				delete(s.d, id)
				continue
			}
			// See [Graph.reclaimLabelVersions] for what retained measures.
			retained := 1
			for d := head; d.next != nil; d = d.next {
				if d.next.stampTS() <= watermark {
					freed += propChainLen(d.next)
					d.next = nil
					break
				}
				retained++
			}
			hist.Observe(retained)
		}
		if len(s.d) == 0 {
			s.d = nil
		}
		s.mu.Unlock()
	}
	if freed > 0 {
		g.propDeltaActive.Add(-int64(freed))
	}
	return freed
}

// stampTS returns the timestamp a label delta was committed at, from its shared
// record when it has one and from its inline field otherwise.
func (d *nodeLabelDelta) stampTS() uint64 {
	if d.info != nil {
		return d.info.TS()
	}
	return d.ts
}

// stampTS is the property delta's counterpart.
func (d *nodePropDelta) stampTS() uint64 {
	if d.info != nil {
		return d.info.TS()
	}
	return d.ts
}

// releaseLabelChain counts the deltas from d to the end of the chain and drops
// the per-label churn hold each of them owns (rmp #2686).
//
// The count and the release are ONE walk because they must name the same set:
// every delta this reclamation makes unreachable is a suspect that has stopped
// being one, and its hold — taken by [nodeLabelShard.pushLabelDelta] — has to go
// with it or the label is pinned to the slow path for the life of the process.
//
// The caller holds the shard's write lock, so the chain cannot move underneath
// the walk. Releasing under that lock is deliberate: the deltas are already
// unreachable from any reader by the watermark argument this file opens with, so
// the hold can only be over-counting from here, which is the safe direction.
func releaseLabelChain(d *nodeLabelDelta, churn *labelChurn) int {
	n := 0
	for ; d != nil; d = d.next {
		churn.release(d.lid)
		n++
	}
	return n
}

func propChainLen(d *nodePropDelta) int {
	n := 0
	for ; d != nil; d = d.next {
		n++
	}
	return n
}
