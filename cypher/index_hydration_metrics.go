package cypher

// index_hydration_metrics.go — the per-ENGINE observability surface over how a
// reopen populated its recovered secondary indexes (rmp #2490).
//
// The process-global counters `store.recovery.indexes.hydrated` /
// `.rebuilt` (index_hydration.go) report the module's aggregate behaviour, and
// that is all they can report: a counter shared by every Engine a process builds
// cannot attribute a decision to ONE of them. An assertion of the form "THIS
// reopen hydrated six indexes and rebuilt none" therefore has no global counter
// to read — a second engine constructed anywhere in the same process, in a test
// binary or in an embedder's connection pool, moves the same number.
//
// This file exports the engine-scoped answer, exactly as
// [Engine.StatsTrackedPairs] and [Engine.CountStoreCells] export the two other
// size/decision indicators the metrics [Backend] cannot express. It reads state
// the constructor already maintains ([recoveredIndexStats]) and adds no
// bookkeeping of its own, so it costs nothing on any path.
//
// # It is NOT a second source of truth
//
// There is exactly one place each decision is recorded: the two call sites in
// index_hydration.go increment the per-engine [recoveredIndexStats] field and
// the process-global counter of the same name TOGETHER, from the same branch,
// under no lock and with no intervening return. This accessor projects those
// same fields and derives nothing of its own, so the accessor and the metric
// cannot disagree about a single engine — the metric is simply the sum over
// every engine the process built.
//
// Their roles are therefore fixed and different: the `store.recovery.indexes.*`
// counters remain the PRODUCTION observable (what an operator's Prometheus
// scrape sees across a whole process), and this accessor is the per-instance
// observable an assertion reads. A test or oracle that needs to attribute a
// decision to one reopen must use the accessor; a dashboard that needs process
// totals must use the metric. Neither substitutes for the other.

// RecoveredIndexPopulation reports how ONE Engine populated each secondary index
// it re-registered while recovering: hydrated from the snapshot payload the
// recovery certified for that index's name, or rebuilt by scanning the recovered
// graph. It is the observable form of the per-index decision described in
// index_hydration.go.
//
// Hydrated + Rebuilt is the number of indexes the constructor populated, which
// counts the internal numeric companion of each user index as well as the user
// index itself, and excludes any name a previous registration in the same
// constructor already claimed (a UNIQUE constraint's backing index, which its
// own registration path populated).
//
// The two payload-fault counters are the evidence that a fallback to a rebuild
// was never silent. They are strictly informative: a corrupt or unreadable
// payload is a per-index REBUILD and never a fail-stop, because an index is
// derived data over an already-recovered, independently integrity-checked graph.
//
// It is an immutable value: the fields are a snapshot of what the constructor
// did, and nothing mutates them afterwards.
type RecoveredIndexPopulation struct {
	// Hydrated counts indexes loaded from a snapshot payload.
	Hydrated int
	// Rebuilt counts indexes populated by scanning the recovered graph.
	Rebuilt int
	// BackfillNodes counts the node references those rebuilds materialised —
	// the work hydration avoids. It is the mapper's length per rebuild, because
	// that is what a backfill walks: one entry per node in the graph, not one
	// per node carrying the indexed label.
	BackfillNodes int
	// PayloadUnreadable counts payloads recovery reported unreadable (a missing
	// indexes/<name>.bin, or bytes that failed the manifest CRC32C).
	PayloadUnreadable int
	// PayloadCorrupted counts payloads recovery certified hydratable whose
	// Deserialize nevertheless rejected them (the index's own structural
	// validation or internal CRC trailer).
	PayloadCorrupted int
}

// RecoveredIndexPopulation reports how this engine populated the secondary
// indexes it re-registered at construction (rmp #2490): the per-engine
// counterpart of the `store.recovery.indexes.*` counters, which are
// process-global and so cannot attribute a decision to one Engine.
//
// Every field is zero for an engine that recovered no index definitions — a
// fresh engine, an in-memory engine, or one built by a constructor that carries
// no recovered schema.
//
// Safe for concurrent use: the counters are written only by
// [NewEngineWithOptions], on the constructing goroutine, before the Engine is
// published, and are read-only thereafter.
func (e *Engine) RecoveredIndexPopulation() RecoveredIndexPopulation {
	return RecoveredIndexPopulation{
		Hydrated:          e.recoveredIdx.hydrated,
		Rebuilt:           e.recoveredIdx.rebuilt,
		BackfillNodes:     e.recoveredIdx.backfillNodes,
		PayloadUnreadable: e.recoveredIdx.payloadUnreadable,
		PayloadCorrupted:  e.recoveredIdx.payloadCorrupted,
	}
}
