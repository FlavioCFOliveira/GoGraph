package sim

import (
	"context"
	"fmt"
	"os"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// simWALPath is the fixed key under which the WAL byte image lives inside a
// [SimDisk]. The in-memory filesystem treats paths as opaque keys (it has no
// directory tree), so a single stable key is sufficient: the durable WAL of the
// simulated store. The OS recovery path joins dir/"wal"; the simulator opens the
// WAL directly because it backs only the WAL-only recovery path in Phase 2
// (snapshot/checkpoint-on-SimDisk is the tracked follow-up).
const simWALPath = "wal"

// simStoreConfig describes the graph shape and codecs a [SimStore] is opened
// with. The simulator uses a directed multigraph keyed by string with float64
// weights, matching the engine the safety loop drives ([Simulator]); the fields
// are kept explicit so a future workload can vary the shape without touching the
// open/reopen plumbing.
type simStoreConfig struct {
	graphConfig adjlist.Config
	maxTxnOps   int
}

// defaultSimStoreConfig is a directed multigraph (openCypher's additive-CREATE
// relationship model) with the default per-transaction op cap, matching the
// production recovery default so a simulated commit replays exactly as a real
// one would. It is used by the standalone SimStore tests.
func defaultSimStoreConfig() simStoreConfig {
	return simStoreConfig{
		graphConfig: adjlist.Config{Directed: true, Multigraph: true},
		maxTxnOps:   0, // 0 -> txn.DefaultMaxTxnOps, the production default.
	}
}

// simulatorStoreConfig is the shape the crash-mode [Simulator] drives. It is a
// SIMPLE directed graph (Multigraph: false), matching both the simulator's
// non-crash in-memory engine and the [GraphOracle]'s edge model, which keys an
// edge by (src, dst, label) and so collapses parallel edges. A multigraph here
// would let two CREATE (a)-[:KNOWS]->(b) statements on the same pair produce two
// engine edges where the oracle models one, a spurious count divergence after
// recovery. Keeping the durable store simple makes the oracle a faithful model
// of the engine across a crash.
func simulatorStoreConfig() simStoreConfig {
	return simStoreConfig{
		graphConfig: adjlist.Config{Directed: true, Multigraph: false},
		maxTxnOps:   0,
	}
}

// SimStore is a real GoGraph persistence stack — a WAL-backed [txn.Store] and a
// [cypher.Engine] — whose durability layer is an in-memory [SimDisk] rather than
// the OS filesystem. It lets the deterministic simulation harness exercise the
// genuine WAL append+sync and recovery-replay code paths without touching real
// disk, so a crash (drop the in-memory engine, keep the SimDisk byte image) and
// a restart (reopen via real recovery) are fully reproducible from a seed.
//
// The crash/restart boundary is the SimDisk: [SimStore.Crash] discards the live
// engine and store but the WAL bytes (and their injected fault state) persist in
// the SimDisk, and [OpenSimStore] reopens them through [recovery.ReplayWAL] —
// the same replay core that [recovery.Open] drives over an OS file.
//
// # Concurrency contract
//
// SimStore is NOT safe for concurrent use; the simulator drives it from a single
// goroutine.
//
//nolint:revive // "Sim" prefix is the DST harness naming scheme (see SimDisk).
type SimStore struct {
	disk   *SimDisk
	cfg    simStoreConfig
	graph  *lpg.Graph[string, float64]
	store  *txn.Store[string, float64]
	wlog   *wal.Writer
	engine *cypher.Engine
	// walOps is the number of WAL ops the most recent recovery replayed back
	// into the graph on open. It is 0 for a freshly-created store.
	walOps int
	// clean records whether the most recent recovery completed without genuine
	// on-disk corruption (a benign torn tail is clean). A corrupt reopen is a
	// hard durability fault the caller must surface, never silently append onto.
	clean bool
}

// OpenSimStore opens (or reopens) a store whose WAL lives in disk under
// [simWALPath]. When the WAL is absent the store starts empty; when it holds
// bytes from a prior session, [recovery.ReplayWAL] rebuilds the graph from the
// committed WAL prefix before the writer is reopened for further appends.
//
// Reopen-for-append truncates the WAL to the last durable frame boundary
// ([recovery.ReplayResult.WALTailOffset]) BEFORE the writer seeks to end
// (auditor finding F1): a crash between two fsyncs leaves a benign torn tail
// past the committed prefix, and appending after it would strand every new
// frame behind junk that every subsequent reader stops at. Truncating to the
// recovered offset makes the reopened WAL a clean append target.
//
// A reopen that detects genuine corruption ([recovery.ReplayResult.IsClean] ==
// false) is a hard fault: the function returns an error rather than appending
// onto the corruption (which would permanently embed it and drop every op past
// the bad frame), mirroring the production [recovery.Open] fail-stop contract.
func OpenSimStore(disk *SimDisk, cfg simStoreConfig) (*SimStore, error) {
	if disk == nil {
		return nil, fmt.Errorf("sim: OpenSimStore: nil disk")
	}
	codec := txn.NewStringCodec()
	wcodec := txn.NewFloat64WeightCodec()
	g := lpg.New[string, float64](cfg.graphConfig)

	var replay recovery.ReplayResult
	clean := true
	if disk.Exists(simWALPath) {
		// Replay the committed WAL prefix into the fresh graph through the real
		// recovery core, reading the WAL bytes back from the SimDisk image.
		rh, err := disk.OpenFile(simWALPath, os.O_RDONLY)
		if err != nil {
			return nil, fmt.Errorf("sim: open WAL for replay: %w", err)
		}
		reader := wal.NewReader(rh, rh)
		replay, err = recovery.ReplayWAL[string, float64](
			context.Background(), reader, g, codec, wcodec,
			resolveSimMaxTxnOps(cfg.maxTxnOps),
		)
		_ = reader.Close()
		if err != nil {
			return nil, fmt.Errorf("sim: WAL replay: %w", err)
		}
		clean = replay.IsClean()
		if !clean {
			// Genuine corruption inside an already-durable frame: fail-stop,
			// never append onto it.
			return nil, fmt.Errorf("sim: WAL recovery found corruption: %w", replay.TailErr)
		}
		// Auditor finding F1: truncate the benign torn tail (the bytes past the
		// last durable frame) before reopening for append, so new frames are not
		// written behind junk every reader stops at.
		if err := truncateSimWAL(disk, replay.WALTailOffset); err != nil {
			return nil, fmt.Errorf("sim: truncate torn WAL tail: %w", err)
		}
	}

	// Open the WAL for append over the (now clean-tailed) SimDisk image through
	// the path-backed FS seam (wal.OpenFS), so a Checkpointer driving this store
	// can reclaim the WAL prefix via Writer.TruncatePrefix — the temp-write,
	// rename, parent-dir fsync and reopen all route through the SimDisk. The
	// benign torn tail was already discarded above (auditor finding F1), which is
	// the precondition OpenFS documents in lieu of its own discardTornTail.
	wlog, err := wal.OpenFS(simWALFS{disk: disk}, simWALPath)
	if err != nil {
		return nil, fmt.Errorf("sim: WAL OpenFS: %w", err)
	}
	store := txn.NewStoreWithOptions(g, wlog, txn.Options[string, float64]{
		Codec:       codec,
		WeightCodec: wcodec,
	})
	// Re-register recovered schema so a UNIQUE/index declared before the crash
	// is enforced after restart, mirroring the production reopen path
	// ([cypher.NewEngineWithStoreAndSchema]).
	engine := cypher.NewEngineWithStoreAndSchema(store, replay.Constraints, replay.Indexes)

	return &SimStore{
		disk:   disk,
		cfg:    cfg,
		graph:  g,
		store:  store,
		wlog:   wlog,
		engine: engine,
		walOps: replay.WALOps,
		clean:  clean,
	}, nil
}

// resolveSimMaxTxnOps mirrors the recovery-side resolution so the simulator's
// reopen uses the same finite default the producer commits under (0 ->
// txn.DefaultMaxTxnOps). A negative value disables the cap.
func resolveSimMaxTxnOps(maxTxnOps int) int {
	switch {
	case maxTxnOps == 0:
		return txn.DefaultMaxTxnOps
	case maxTxnOps < 0:
		return 0
	default:
		return maxTxnOps
	}
}

// truncateSimWAL resizes the WAL byte image to off bytes via a SimDisk handle.
// It is the in-memory analogue of the OS truncate the WAL writer performs on a
// torn tail; off is the last durable frame boundary reported by recovery.
func truncateSimWAL(disk *SimDisk, off int64) error {
	h, err := disk.OpenFile(simWALPath, os.O_RDWR)
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	return h.Truncate(off)
}

// Engine returns the live cypher engine bound to the recovered graph and the
// WAL-backed store, for the simulator to drive queries through.
func (s *SimStore) Engine() *cypher.Engine { return s.engine }

// Graph returns the live recovered graph.
func (s *SimStore) Graph() *lpg.Graph[string, float64] { return s.graph }

// WALOps reports how many WAL ops the most recent recovery replayed back into
// the graph on open (0 for a freshly-created store).
func (s *SimStore) WALOps() int { return s.walOps }

// Clean reports whether the most recent recovery completed without genuine
// on-disk corruption (a benign torn tail counts as clean).
func (s *SimStore) Clean() bool { return s.clean }

// Crash models a SIGKILL: it discards the in-memory engine, store, and WAL
// writer WITHOUT a graceful close, so any buffered-but-unsynced frame is lost
// exactly as a real crash would lose it. The durable WAL byte image inside the
// SimDisk (and its fault state) survives untouched, ready for [OpenSimStore] to
// reopen and replay. The SimStore must not be used after Crash.
//
// Crash deliberately does NOT call s.wlog.Close(): a clean Close would flush and
// fsync the buffer, which is the opposite of a crash. Dropping the references
// lets the GC reclaim them; the only durable state is the SimDisk image.
//
// Crash also revokes every not-yet-dir-fsync'd dirent on the SimDisk (see
// [SimDisk.Crash]), modelling the loss of a create or rename whose parent
// directory was never fsync'd. For a WAL-only store the WAL is a root-level
// file treated as durably linked on creation, so this is a no-op; once
// snapshots back onto the same SimDisk it drops an interrupted snapshot publish
// exactly as production would.
func (s *SimStore) Crash() {
	if s.disk != nil {
		s.disk.Crash()
	}
	s.engine = nil
	s.store = nil
	s.wlog = nil
	s.graph = nil
}

// Close shuts the store down gracefully, flushing and fsyncing the WAL so every
// acknowledged commit is durable, then releasing the WAL writer. Use it for a
// clean teardown (end of a run); use [SimStore.Crash] to model a crash.
func (s *SimStore) Close() error {
	if s.wlog == nil {
		return nil
	}
	err := s.wlog.Close()
	s.wlog = nil
	s.store = nil
	s.engine = nil
	return err
}
