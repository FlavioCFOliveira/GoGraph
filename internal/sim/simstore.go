package sim

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// simWALPath is the fixed key under which the WAL byte image of a WAL-ONLY
// [SimStore] lives inside a [SimDisk]. The in-memory filesystem treats paths as
// opaque keys, so a single stable root-level key is sufficient for the legacy
// WAL-only recovery path ([recovery.ReplayWAL]). A full-stack [SimStore] (one
// opened with a checkpoint directory) instead lays the WAL out under that
// directory at dir/"wal" so a snapshot can sit beside it at dir/"snapshot" and
// recovery goes through the full [recovery.OpenFS] snapshot+WAL path.
const simWALPath = "wal"

// simWALName / simSnapshotName are the file-name components the full-stack
// layout uses inside the checkpoint directory, matching the production
// checkpoint/recovery layout (recovery joins dir+"/wal" and probes
// dir+"/snapshot"). Kept in sync with store/recovery and store/checkpoint.
const (
	simWALName      = "wal"
	simSnapshotName = "snapshot"
)

// walPathFor returns the SimDisk key of the WAL for a store opened with dir. An
// empty dir selects the legacy root-level WAL-only key ([simWALPath]); a
// non-empty dir places the WAL under it (dir/wal) so the full snapshot+WAL
// recovery path applies.
func walPathFor(dir string) string {
	if dir == "" {
		return simWALPath
	}
	return dir + "/" + simWALName
}

// simStoreConfig describes the graph shape and codecs a [SimStore] is opened
// with. The simulator uses a directed multigraph keyed by string with float64
// weights, matching the engine the safety loop drives ([Simulator]); the fields
// are kept explicit so a future workload can vary the shape without touching the
// open/reopen plumbing.
type simStoreConfig struct {
	graphConfig adjlist.Config
	maxTxnOps   int
	// dir, when non-empty, opens the store in FULL-STACK mode: the WAL lives at
	// dir/wal, a checkpoint publishes a self-sufficient snapshot at dir/snapshot
	// and truncates the WAL prefix it folded, and recovery reconstructs the graph
	// through the full snapshot+WAL path ([recovery.OpenFS]). An empty dir keeps
	// the legacy WAL-only layout (root-level [simWALPath], recovered via
	// [recovery.ReplayWAL]).
	dir string
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
	walPath := walPathFor(cfg.dir)

	g, recovered, clean, err := recoverSimGraph(disk, cfg, codec, wcodec)
	if err != nil {
		return nil, err
	}

	// Open the WAL for append over the (now clean-tailed) SimDisk image through
	// the path-backed FS seam (wal.OpenFS), so a Checkpointer driving this store
	// can reclaim the WAL prefix via Writer.TruncatePrefix — the temp-write,
	// rename, parent-dir fsync and reopen all route through the SimDisk. The
	// benign torn tail was already discarded by recoverSimGraph (auditor finding
	// F1), which is the precondition OpenFS documents in lieu of its own
	// discardTornTail.
	wlog, err := wal.OpenFS(simWALFS{disk: disk}, walPath)
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
	engine := cypher.NewEngineWithStoreAndSchema(store, recovered.constraints, recovered.indexes)

	return &SimStore{
		disk:   disk,
		cfg:    cfg,
		graph:  g,
		store:  store,
		wlog:   wlog,
		engine: engine,
		walOps: recovered.walOps,
		clean:  clean,
	}, nil
}

// recoveredSchema carries the schema and op-count a recovery produced, decoupled
// from whether the WAL-only ([recovery.ReplayWAL]) or full snapshot+WAL
// ([recovery.OpenFS]) core ran.
type recoveredSchema struct {
	constraints []recovery.ConstraintRecord
	indexes     []recovery.IndexRecord
	walOps      int
}

// recoverSimGraph rebuilds the durable graph from the SimDisk image and returns
// it ready for a fresh append-side WAL writer. It selects the recovery core by
// the durable layout present on disk:
//
//   - Full-stack mode (cfg.dir != "") with a published snapshot at dir/snapshot:
//     the FULL snapshot+WAL path ([recovery.OpenFS]) reconstructs the graph from
//     the self-sufficient snapshot and replays the WAL tail (dir/wal) on top. The
//     snapshot carries the persisted graph config, so the simple/multigraph shape
//     is preserved across the checkpoint+crash boundary.
//   - Otherwise (no snapshot, or legacy WAL-only mode): the WAL-only core
//     ([recovery.ReplayWAL]) replays the committed prefix into a graph built with
//     cfg.graphConfig — the only place the simulator's SIMPLE shape is asserted
//     when no snapshot has yet persisted that config.
//
// In every case the benign torn WAL tail is truncated to the last durable frame
// boundary BEFORE the caller reopens for append (auditor finding F1), and
// genuine corruption fail-stops with an error rather than appending onto it.
func recoverSimGraph(
	disk *SimDisk,
	cfg simStoreConfig,
	codec txn.Codec[string],
	wcodec txn.WeightCodec[float64],
) (*lpg.Graph[string, float64], recoveredSchema, bool, error) {
	walPath := walPathFor(cfg.dir)

	// Full-stack recovery: a published snapshot folds an arbitrarily long WAL
	// prefix, so the graph MUST be rebuilt from it (the WAL alone no longer holds
	// the truncated prefix). recovery.OpenFS reads dir/snapshot + dir/wal, honours
	// the snapshot's persisted graph config, and truncates the benign WAL tail it
	// returns as part of the recovered Result.
	if cfg.dir != "" && disk.Exists(cfg.dir+"/"+simSnapshotName+"/manifest.json") {
		res, err := recovery.OpenFS[string, float64](
			simRecoveryFS{disk: disk}, cfg.dir,
			recovery.Options[string, float64]{
				Codec:       codec,
				WeightCodec: wcodec,
				MaxTxnOps:   recoveryMaxTxnOpsOption(cfg.maxTxnOps),
			},
		)
		if err != nil {
			// A snapshot-corruption or WAL-corruption fail-stop is a hard fault the
			// run must surface, never silently append onto.
			return nil, recoveredSchema{}, false, fmt.Errorf("sim: full-stack recovery: %w", err)
		}
		// recovery.OpenFS already discarded the benign WAL tail it recovered up to,
		// leaving dir/wal a clean append target for the reopened writer.
		return res.Graph, recoveredSchema{
			constraints: res.Constraints,
			indexes:     res.Indexes,
			walOps:      res.WALOps,
		}, true, nil
	}

	// WAL-only recovery (no snapshot, or legacy mode): replay the committed prefix
	// into a graph with the simulator's configured shape.
	g := lpg.New[string, float64](cfg.graphConfig)
	if !disk.Exists(walPath) {
		return g, recoveredSchema{}, true, nil
	}
	rh, err := disk.OpenFile(walPath, os.O_RDONLY)
	if err != nil {
		return nil, recoveredSchema{}, false, fmt.Errorf("sim: open WAL for replay: %w", err)
	}
	reader := wal.NewReader(rh, rh)
	replay, err := recovery.ReplayWAL[string, float64](
		context.Background(), reader, g, codec, wcodec,
		resolveSimMaxTxnOps(cfg.maxTxnOps),
	)
	_ = reader.Close()
	if err != nil {
		return nil, recoveredSchema{}, false, fmt.Errorf("sim: WAL replay: %w", err)
	}
	if !replay.IsClean() {
		return nil, recoveredSchema{}, false, fmt.Errorf("sim: WAL recovery found corruption: %w", replay.TailErr)
	}
	// Auditor finding F1: truncate the benign torn tail before the caller reopens
	// for append, so new frames are not written behind junk every reader stops at.
	if err := truncateSimWALAt(disk, walPath, replay.WALTailOffset); err != nil {
		return nil, recoveredSchema{}, false, fmt.Errorf("sim: truncate torn WAL tail: %w", err)
	}
	return g, recoveredSchema{
		constraints: replay.Constraints,
		indexes:     replay.Indexes,
		walOps:      replay.WALOps,
	}, true, nil
}

// recoveryMaxTxnOpsOption maps the simulator's maxTxnOps convention (0 ->
// default, <0 -> unlimited) onto the recovery.Options convention (0 -> default,
// txn.MaxTxnOpsUnlimited -> no cap, positive verbatim), so the full-stack path
// caps recovery exactly as the WAL-only path does.
func recoveryMaxTxnOpsOption(maxTxnOps int) int {
	if maxTxnOps < 0 {
		return txn.MaxTxnOpsUnlimited
	}
	return maxTxnOps
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

// truncateSimWALAt resizes the WAL byte image at path to off bytes via a
// SimDisk handle. It is the in-memory analogue of the OS truncate the WAL writer
// performs on a torn tail; off is the last durable frame boundary reported by
// recovery.
func truncateSimWALAt(disk *SimDisk, path string, off int64) error {
	h, err := disk.OpenFile(path, os.O_RDWR)
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	return h.Truncate(off)
}

// Config returns the store configuration this SimStore was opened with,
// including the durable layout (the checkpoint directory in full-stack mode). It
// is preserved across [SimStore.Crash] so the simulator can reopen the store
// with the identical layout during crash recovery.
func (s *SimStore) Config() simStoreConfig { return s.cfg }

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

// Checkpoint runs ONE synchronous, real checkpoint over the SimDisk: it
// publishes a self-sufficient snapshot of the live graph to <dir>/snapshot and
// then prefix-truncates the WAL (<dir>/wal) for the prefix the snapshot folded.
// It drives the production [checkpoint.Checkpointer] through its synchronous
// [checkpoint.Checkpointer.RunCheckpoint] entry point, with the snapshot publish
// routed through the SimDisk snapshot seam ([simCheckpointBackend]) and the WAL
// truncation through the path-backed [wal.OpenFS] writer this store already
// holds — so the entire snapshot+WAL+truncate stack exercises the in-memory
// disk and a subsequent crash recovers via the FULL [recovery.OpenFS] path.
//
// The critical section runs under the store's real commit serialisation
// ([txn.Store.RunUnderCommitLock]) so the snapshot is transaction-boundary
// consistent and the WAL prefix is reclaimed only after the self-sufficient
// snapshot is durable (docs/acid-audit.md F3.5). The string-key mapper codec,
// the engine's constraint specs and its index-definition specs are all wired so
// a checkpoint that truncates the WAL prefix which first declared a
// constraint/index cannot lose it (#1464/#1755).
//
// Checkpoint is only meaningful in full-stack mode (the store was opened with a
// checkpoint directory); on a WAL-only store it returns an error rather than
// silently doing nothing, since a WAL-only layout has no snapshot directory to
// recover the truncated prefix from.
func (s *SimStore) Checkpoint() error {
	if s.cfg.dir == "" {
		return fmt.Errorf("sim: Checkpoint requires a full-stack store (opened with a checkpoint dir)")
	}
	if s.store == nil || s.wlog == nil {
		return fmt.Errorf("sim: Checkpoint on a closed/crashed store")
	}
	// storeMu is a throwaway: WithCommitSerialiser supersedes it (the engine's
	// commit mutex is private), exactly as the production engine wiring does.
	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](
		checkpoint.Config{Dir: s.cfg.dir}, s.graph, s.wlog, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](s.store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](s.store.Codec()),
		checkpoint.WithSnapshotFS[string, float64](simCheckpointBackend{disk: s.disk}),
		checkpoint.WithConstraintSpecs[string, float64](s.engine.ConstraintSpecsForSnapshot),
		checkpoint.WithIndexSpecs[string, float64](s.engine.IndexSpecsForSnapshot),
	)
	if err := cp.RunCheckpoint(); err != nil {
		return fmt.Errorf("sim: checkpoint: %w", err)
	}
	return nil
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
