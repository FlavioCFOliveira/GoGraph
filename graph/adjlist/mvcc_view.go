package adjlist

// mvcc_view.go — the snapshot-consistent read of one node's adjacency
// (rmp #2289, MVCC P4b).
//
// # Why one view rather than four loads
//
// The existing accessors — [AdjList.LoadEntry], [AdjList.LoadEntryH],
// [AdjList.LoadEntryLabels], [AdjList.LoadEntryAux] — each resolve the entry
// independently, so a caller wanting neighbours AND the label column loads the
// slot twice and may get two different entries. Today's callers work around
// that by bounding their scan with the SHORTER of the two lengths, which is a
// comment in three places:
//
//	"labs is published in lockstep with neighbours, but a concurrent writer may
//	 publish a longer neighbours snapshot after we loaded labs; bound the scan
//	 by the shorter length so an index is never out of range."
//
// That keeps the index in range; it does not make the two columns agree. Under
// a snapshot they must, because the whole point is that a read sees ONE version
// of the graph. So a versioned read resolves the entry ONCE and returns every
// column of it, and the mismatch has nowhere left to come from.
//
// # The parallel columns are still parallel
//
// `Labels`, `Handles` and `Weights` are nil when the graph never used them, and
// the same length as `Neighbours` otherwise — the same contract the individual
// accessors document. The difference is only that they now come from one entry.

import "github.com/FlavioCFOliveira/GoGraph/graph"

// EntryView is a consistent read of every column of one node's adjacency entry.
//
// All slices alias the immutable entry and MUST NOT be mutated. They are
// mutually consistent: every non-nil column is the same length as Neighbours,
// because all of them came from one atomically-published entry.
//
// The zero value is what a node with no outgoing edges reads as.
type EntryView[W any] struct {
	// Neighbours is the out-neighbour column.
	Neighbours []graph.NodeID
	// Weights is the parallel weight column.
	Weights []W
	// Handles is the parallel stable-handle column, or nil when this graph
	// carries none.
	Handles []uint64
	// Labels is the parallel per-slot relationship-type column, or nil when no
	// slot of this node has ever been typed. A zero entry means "no type".
	Labels []uint32
	// Aux is the opaque per-slot side column the higher layer attaches — for
	// lpg, the de-boxed columnar edge properties. Nil when none was attached.
	Aux AuxColumn
}

// LoadEntryView returns every column of id's CURRENT adjacency entry, resolved
// from one atomic load so the columns are mutually consistent.
//
// It is what a writer inside the visibility barrier reads: the current value,
// including its own not-yet-published work.
//
// Safe for concurrent use.
func (a *AdjList[N, W]) LoadEntryView(id graph.NodeID) EntryView[W] {
	return viewOf(loadEntry[W](&a.shards[id&shardMask], uint64(id)>>shardBits))
}

// At names the instant an entry read resolves at.
//
// The zero value means "the current entry", which is what every pre-MVCC caller
// wants and what a writer inside the visibility barrier requires: it applies
// eagerly and must see its own not-yet-published work. Versioned is a separate
// field rather than a StartTS sentinel because zero is a legitimate timestamp
// for a reader that started before any commit.
type At struct {
	Versioned bool
	StartTS   uint64
	TxID      uint64
}

// LoadEntryHAt is [AdjList.LoadEntryH] resolving the entry at the instant at
// names, rather than always at the current one.
//
// It exists so a bulk scan — the CSR build (rmp #2293) — can be written once and
// select its instant with a loop-invariant branch, instead of paying an extra
// call frame per node to a wrapper above this layer. That wrapper measured
// +15.84% on the build; folding the branch in here keeps the call count per node
// at exactly what LoadEntryH costs.
//
// Safe for concurrent use.
func (a *AdjList[N, W]) LoadEntryHAt(
	id graph.NodeID, at At,
) (neighbours []graph.NodeID, weights []W, handles []uint64) {
	e := loadEntry[W](&a.shards[id&shardMask], uint64(id)>>shardBits)
	if at.Versioned {
		e = a.entryAsOfLoaded(e, at.StartTS, at.TxID)
	}
	if e == nil {
		return nil, nil, nil
	}
	return e.neighbours, e.weights, e.handles
}

// EntryViewAsOf returns every column of id's adjacency entry as it was at
// startTS for a reader running as txID.
//
// The fast path is one atomic load plus one uncontended atomic gate read, which
// is what a non-versioned read already costs; the chain walk runs only for a
// node a concurrent writer has actually touched.
//
// Safe for concurrent use.
func (a *AdjList[N, W]) EntryViewAsOf(id graph.NodeID, startTS, txID uint64) EntryView[W] {
	s := &a.shards[id&shardMask]
	return viewOf(a.entryAsOf(s, uint64(id)>>shardBits, startTS, txID))
}

// NeighboursAsOf returns the out-neighbours of id as they were at startTS, or
// nil when it had none. The returned slice aliases an immutable entry and MUST
// NOT be mutated.
//
// It is [AdjList.EntryNeighboursAsOf] under the name the rest of the versioned
// surface uses; both are kept because the older one is already referenced by
// its own tests.
//
// Safe for concurrent use.
func (a *AdjList[N, W]) NeighboursAsOf(id graph.NodeID, startTS, txID uint64) []graph.NodeID {
	return a.EntryNeighboursAsOf(id, startTS, txID)
}

// HasEdgeAsOf reports whether a directed edge srcID→dstID existed at startTS
// for a reader running as txID.
//
// Safe for concurrent use.
func (a *AdjList[N, W]) HasEdgeAsOf(srcID, dstID graph.NodeID, startTS, txID uint64) bool {
	s := &a.shards[srcID&shardMask]
	e := a.entryAsOf(s, uint64(srcID)>>shardBits, startTS, txID)
	if e == nil {
		return false
	}
	for _, n := range e.neighbours {
		if n == dstID {
			return true
		}
	}
	return false
}

// OutDegreeAsOf returns how many outgoing slots id had at startTS.
//
// Safe for concurrent use.
func (a *AdjList[N, W]) OutDegreeAsOf(id graph.NodeID, startTS, txID uint64) int {
	s := &a.shards[id&shardMask]
	e := a.entryAsOf(s, uint64(id)>>shardBits, startTS, txID)
	if e == nil {
		return 0
	}
	return len(e.neighbours)
}

// LoadEntrySlotLabels returns id's CURRENT neighbour and per-slot label
// columns, resolved from ONE entry so the two agree.
//
// It exists beside [AdjList.LoadEntryView] because the label-resolution path
// wants exactly these two columns and nothing else, and returning the full
// five-field view there cost 21.7 ns per call — over half the whole read —
// purely in copying slice headers the caller discards. Measured; see
// BenchmarkEdgeSideRead_LabelsByID.
//
// Safe for concurrent use.
func (a *AdjList[N, W]) LoadEntrySlotLabels(id graph.NodeID) (neighbours []graph.NodeID, labels []uint32) {
	e := loadEntry[W](&a.shards[id&shardMask], uint64(id)>>shardBits)
	if e == nil {
		return nil, nil
	}
	return e.neighbours, e.labels
}

// EntrySlotLabelsAsOf is [AdjList.LoadEntrySlotLabels] as the entry stood at
// startTS for a reader running as txID.
//
// Safe for concurrent use.
func (a *AdjList[N, W]) EntrySlotLabelsAsOf(id graph.NodeID, startTS, txID uint64) (neighbours []graph.NodeID, labels []uint32) {
	e := a.entryAsOf(&a.shards[id&shardMask], uint64(id)>>shardBits, startTS, txID)
	if e == nil {
		return nil, nil
	}
	return e.neighbours, e.labels
}

// viewOf projects an entry into its columns, or the zero view when there is no
// entry.
func viewOf[W any](e *adjEntry[W]) EntryView[W] {
	if e == nil {
		return EntryView[W]{}
	}
	return EntryView[W]{
		Neighbours: e.neighbours,
		Weights:    e.weights,
		Handles:    e.handles,
		Labels:     e.labels,
		Aux:        e.aux,
	}
}
