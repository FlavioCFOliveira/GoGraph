package cypher

// index_hydration.go — hydrating a recovered secondary index from the snapshot
// payload instead of rebuilding it from the graph (rmp #2490).
//
// # Why the decision lives here and not in store/recovery
//
// Recovery cannot make it. Hydrating an index means writing into a CONCRETE
// index implementation bound to a (label, property) pair, and both the
// implementation choice and the binding are the engine's: recovery has no way to
// build a hash.Index bound to (:Person, name), no way to know that
// `person_name_btree_num` is the internal numeric companion of a user index, and
// no way to know that `__uniq__Person.email` backs a UNIQUE constraint. Nor may
// it register an index at all — WAL replay does not feed the index.Manager
// change fan-out, so anything recovery registered would be frozen at the
// snapshot instant while the planner kept seeking it.
//
// So recovery reports facts (see store/recovery/index_payloads.go) and this file
// applies the semantics, per index, by name.
//
// # The three preconditions, and why each is necessary
//
//  1. The snapshot must have been SELF-SUFFICIENT. Recovery folds this into each
//     payload's reason code, so precondition 1 arrives here as
//     recovery.ErrIndexPayloadStale.
//  2. The payload must be READABLE and CRC-VALID — likewise folded into the
//     reason code, as recovery.ErrIndexPayloadUnreadable.
//  3. Nothing the replayed WAL suffix committed may have touched THIS index's
//     (label, property). Only the engine knows which pair a name covers, so this
//     is the one precondition evaluated here, against the facts recovery
//     collected while replaying.
//
// # Corruption is a per-index REBUILD, never a fail-stop
//
// An index is derived data: a pure function of an already-recovered,
// independently integrity-checked graph. Rebuilding it restores byte-identical
// content and loses no information, so refusing to open a database over a
// damaged index payload would deny service for a fault with a complete local
// repair. Every reference engine reaches the same conclusion — PostgreSQL
// discards and rebuilds pg_internal.init, Memgraph rebuilds its label indexes
// unconditionally on recovery, and Neo4j coerces an unreadable index header to
// POPULATING and repopulates.
//
// The fallback is NOT silent: it carries a typed reason on the payload, a metric
// (`store.recovery.indexes.rebuilt`, with the corrupt and unreadable cases also
// metered separately), a structured warning, and a per-engine counter the tests
// assert in BOTH directions. The one index-related condition that stays
// fail-stop is a manifest index name that would escape the indexes/ directory:
// snapshot.LoadIndexes raises ErrManifestCorrupted for it and the open fails,
// because path traversal from attacker-controlled manifest bytes is a security
// event and not benign corruption.
//
// # A not-yet-populated index is never seekable
//
// Every call site here POPULATES the index — hydrate or backfill — and only then
// hands it to index.Manager.CreateIndex. Until that registration the index is
// reachable by nothing: the planner finds candidates through the manager, and an
// index that is not in the manager cannot be found, let alone seeked. That is
// the same invariant Memgraph enforces with its per-index population status and
// Neo4j with its POPULATING proxies, obtained here by construction rather than
// by a status flag.
//
// # Hydration is confined to construction, and that is ENFORCED
//
// hash.Index.Deserialize swaps its shards one at a time under per-shard locks,
// so it is NOT atomic across shards: a concurrent reader could observe a
// half-replaced index. Every hydration therefore happens inside
// NewEngineWithOptions, before the Engine (and with it the index.Manager the
// planner reads) is published to any other goroutine. A call after that point
// PANICS rather than silently degrading to a backfill — see the guard in
// populateRecoveredIndex. Programmer error must surface immediately.

import (
	"bytes"
	"errors"
	"log/slog"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// IndexPayloadSource supplies the durable secondary-index payloads a recovery
// captured, plus the WAL-suffix facts needed to tell whether one is still valid.
// A [store/recovery.Result] value satisfies it directly, which is the point: the
// engine can accept the payloads without naming recovery's generic Result type
// in its own option struct.
//
// The two methods are the two halves of the hydration decision that recovery and
// the engine own respectively. IndexPayloadFor answers everything recovery can
// judge alone — readable, CRC-valid, from a self-sufficient and watermarked
// image — returning a non-nil error (and nil bytes) for every payload that must
// not be used. WALSuffixTouchesNodeIndex answers the one question recovery
// cannot pose, because only the engine knows which (label, property) an index
// name covers: whether the replayed suffix could have changed that index's
// contents.
//
// Implementations must be safe for concurrent reads and must not mutate the
// returned byte slice; the engine treats it as read-only.
type IndexPayloadSource interface {
	// IndexPayloadFor returns the payload certified hydratable for the index
	// registered under name, or nil bytes and the reason none may be used.
	IndexPayloadFor(name string) ([]byte, error)
	// WALSuffixTouchesNodeIndex reports whether the replayed WAL could have
	// changed the contents of a node index on (label, property).
	WALSuffixTouchesNodeIndex(label, property string) bool
}

// recoveredIndexStats counts how each recovered index was populated, per Engine.
//
// It exists alongside the process-global metrics because a metric cannot
// attribute work to ONE engine: a test that asserts "this reopen hydrated 2 and
// rebuilt 0" needs a counter scoped to the engine it constructed, and a global
// counter is polluted by every other engine the same test binary builds.
//
// Every field is written only during [NewEngineWithOptions], on the constructing
// goroutine, and read only after that constructor has returned — which the Go
// memory model orders — so no synchronisation is required and none is used.
type recoveredIndexStats struct {
	// hydrated counts indexes populated from a snapshot payload.
	hydrated int
	// rebuilt counts indexes populated by scanning the recovered graph.
	rebuilt int
	// backfillNodes counts the node references those rebuilds materialised —
	// the work hydration avoids. It is the mapper's length per rebuild, because
	// that is what a backfill walks: one entry per node in the graph, not per
	// node carrying the indexed label.
	backfillNodes int
	// payloadCorrupted counts payloads recovery certified hydratable whose
	// Deserialize nevertheless failed, and payloadUnreadable those recovery
	// reported unreadable. Both are the fallback-was-not-silent evidence.
	payloadCorrupted  int
	payloadUnreadable int
}

// errHydrationAfterPublish is what a post-construction hydration attempt panics
// with. It is a programmer error inside this package, not a condition any input
// can produce, so it surfaces immediately rather than being returned.
var errHydrationAfterPublish = errors.New(
	"cypher: recovered-index hydration attempted after the engine was published; " +
		"hash.Index.Deserialize swaps shards sequentially and is not atomic, so it must " +
		"run only inside NewEngineWithOptions, before any other goroutine can read the index")

// populateRecoveredIndex fills a freshly built, still-unregistered index for the
// recovered definition (name, label, property) — either from the snapshot
// payload recovery certified for that name, or by running rebuild.
//
// sub is the index being populated; it must implement [index.Serializer] to be
// hydratable, and a subscriber that does not simply takes the rebuild path (the
// rebuild-on-restart contract the index.Manager documents). rebuild performs the
// engine's existing backfill scan for this index kind and must be non-nil.
//
// Callers must not have registered sub on the index.Manager yet: the whole
// no-seekable-unpopulated-index invariant rests on populate-then-register.
//
// It panics when called after the Engine has been published; see
// [errHydrationAfterPublish].
func (e *Engine) populateRecoveredIndex(name, label, property string, sub index.Subscriber, rebuild func()) {
	if e.published {
		panic(errHydrationAfterPublish)
	}
	if e.hydrateRecoveredIndex(name, label, property, sub) {
		return
	}
	rebuild()
	e.recoveredIdx.rebuilt++
	// The work a hydration would have avoided. A backfill materialises one
	// reference per node in the MAPPER — every node in the graph, not only those
	// carrying the indexed label — so the mapper's length is exactly what this
	// rebuild walked.
	visited := e.g.AdjList().Mapper().Len()
	e.recoveredIdx.backfillNodes += visited
	metrics.IncCounter("store.recovery.indexes.rebuilt", 1)
	if visited > 0 {
		metrics.IncCounter("store.recovery.indexes.backfill_nodes", uint64(visited))
	}
}

// hydrateRecoveredIndex attempts to load sub from the recovered snapshot payload
// for name and reports whether it succeeded. Every refusal is accounted for
// before returning false, so the caller's rebuild is never an unexplained
// fallback.
func (e *Engine) hydrateRecoveredIndex(name, label, property string, sub index.Subscriber) bool {
	src := e.recoveredIndexPayloads
	if src == nil {
		// No payload source at all: the pre-#2490 wiring, an in-memory engine, or
		// a caller that opened with a constructor carrying no payloads. Backfill,
		// exactly as before, and do not meter a fallback that had nothing to fall
		// back from.
		return false
	}
	ser, ok := sub.(index.Serializer)
	if !ok {
		// The rebuild-on-restart contract: a subscriber with no serialised form
		// was never persisted, so there is nothing to hydrate from.
		return false
	}
	// PRECONDITION 3, the only one recovery cannot evaluate: did the replayed WAL
	// suffix touch this index's (label, property)? Checked BEFORE fetching the
	// payload, because a stale index is not a payload problem and must not be
	// metered as one.
	if src.WALSuffixTouchesNodeIndex(label, property) {
		return false
	}
	payload, err := src.IndexPayloadFor(name)
	switch {
	case errors.Is(err, recovery.ErrIndexPayloadUnreadable):
		// The snapshot declared this payload and it did not survive: a missing
		// file or a CRC32C that did not match the manifest. Loud, then rebuild.
		e.recoveredIdx.payloadUnreadable++
		metrics.IncCounter("store.recovery.indexes.payload_unreadable", 1)
		slog.Default().Warn("cypher: recovered index payload unreadable, rebuilding the index from the graph",
			slog.String("index", name), slog.String("label", label),
			slog.String("property", property), slog.Any("reason", err))
		return false
	case err != nil:
		// Stale (no mapper.bin, or no indexes_commit_ts) or simply never
		// captured. Neither is a fault, so neither is metered as one: the
		// rebuild counter below is the whole record.
		return false
	}
	if derr := ser.Deserialize(bytes.NewReader(payload)); derr != nil {
		// The bytes matched the manifest CRC but the index's own structural
		// validation or internal CRC trailer rejected them. Same conclusion,
		// separately metered because it names a different failure surface.
		e.recoveredIdx.payloadCorrupted++
		metrics.IncCounter("store.recovery.indexes.payload_corrupted", 1)
		slog.Default().Warn("cypher: recovered index payload did not deserialize, rebuilding the index from the graph",
			slog.String("index", name), slog.String("label", label),
			slog.String("property", property), slog.Any("reason", derr))
		return false
	}
	e.recoveredIdx.hydrated++
	metrics.IncCounter("store.recovery.indexes.hydrated", 1)
	return true
}

// NewEngineWithStoreAndRecovery creates a WAL-backed Engine from a store and the
// [store/recovery.Result] that produced it, re-registering the recovered schema
// constraints and index definitions AND hydrating each index from the snapshot
// payload wherever recovery certified that safe.
//
// It is the recommended constructor for opening a persisted store:
//
//	res, err := recovery.Open[string, float64](dir, ropts)
//	if err != nil { return err }
//	w, err := wal.Open(filepath.Join(dir, "wal"))
//	if err != nil { return err }
//	st := res.NewStore(w, sopts)
//	eng := cypher.NewEngineWithStoreAndRecovery(st, res)
//
// It takes the whole Result rather than three extracted fields for the reason
// [store/recovery.Result.NewStore] takes the WAL: a handoff every caller must
// remember to perform is one some caller will forget, and forgetting THIS one is
// silent — the engine still opens, still answers correctly, and merely rebuilds
// every index from a full graph scan on every restart.
// [NewEngineWithStoreAndSchema] remains available and unchanged for a caller
// that has only the schema slices; it carries no payloads and therefore always
// rebuilds, which is exactly its previous behaviour.
//
// The Result must come from a COMPLETED [store/recovery.Open] over the directory
// this store writes to. Passing a Result from a different directory would offer
// payloads that describe another graph; recovery's own staleness gate cannot
// detect that, because the mismatch is in the caller's wiring rather than on
// disk.
func NewEngineWithStoreAndRecovery(store *txn.Store[string, float64], res recovery.Result[string, float64]) *Engine {
	return NewEngineWithOptions(store.Graph(), EngineOptions{
		Store:                  store,
		RecoveredConstraints:   ConstraintDefsFromRecovery(res.Constraints),
		RecoveredIndexes:       IndexDefsFromRecovery(res.Indexes),
		RecoveredIndexPayloads: res,
	})
}
