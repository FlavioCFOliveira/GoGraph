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
	// dir, when non-empty, opens the store in FULL-STACK mode: the WAL lives at
	// dir/wal, a checkpoint publishes a self-sufficient snapshot at dir/snapshot
	// and truncates the WAL prefix it folded, and recovery reconstructs the graph
	// through the full snapshot+WAL path ([recovery.OpenFS]). An empty dir keeps
	// the legacy WAL-only layout (root-level [simWALPath], recovered via
	// [recovery.ReplayWAL]).
	dir         string
	graphConfig adjlist.Config
	maxTxnOps   int
	// noResumeTxnSeq opens the store WITHOUT seeding [txn.Options.ResumeTxnSeq]
	// from what recovery derived — the embedder as it behaved before rmp #2302
	// taught recovery to report [recovery.Result.MaxTxnSeq]. A store opened this
	// way restarts its transaction sequence at 0, so ONE WAL image ends up
	// holding two different transactions under one sequence number.
	//
	// It exists solely as the SENSITIVITY SEAM of the MVCC clock/sequence
	// recovery oracles (rmp #2469): without it, an oracle that never fires is
	// indistinguishable from an invariant that always holds. Never set it in a
	// scenario that is asserting correct behaviour.
	noResumeTxnSeq bool
	// uncappedProducerSeam opens the store with the UNCAPPED producer bound —
	// [txn.DefaultMaxTxnOps] — regardless of maxTxnOps, while recovery still
	// honours the configured cap. It reproduces this harness exactly as it
	// behaved before rmp #2474, when maxTxnOps reached the replayer and not the
	// producer.
	//
	// It exists solely as the SENSITIVITY SEAM of the transaction-size cap
	// oracles (rmp #2474). A store opened this way violates the invariant
	// [txn.DefaultMaxTxnOps] documents — producer cap <= replay cap — so it
	// ACKNOWLEDGES a transaction recovery then refuses to replay, and the whole
	// store fails to reopen. Never set it in a scenario that is asserting
	// correct behaviour.
	uncappedProducerSeam bool
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
// of the engine across a crash. A scenario whose oracle DOES model edges per
// instance (edge-properties, rmp #2449) opts into a multigraph via
// [Config.Multigraph], which [New] applies on top of this base shape.
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
	graph  *lpg.Graph[string, float64]
	store  *txn.Store[string, float64]
	wlog   *wal.Writer
	engine *cypher.Engine
	cfg    simStoreConfig
	// walOps is the number of WAL ops the most recent recovery replayed back
	// into the graph on open. It is 0 for a freshly-created store.
	walOps int
	// clean records whether the most recent recovery completed without genuine
	// on-disk corruption (a benign torn tail is clean). A corrupt reopen is a
	// hard durability fault the caller must surface, never silently append onto.
	clean bool
	// resumedTxnSeq is the transaction sequence this store continued FROM: the
	// highest sequence the recovered WAL carried, fed back into
	// [txn.Options.ResumeTxnSeq]. It is 0 for a fresh store and for a recovery
	// whose surviving WAL held no v3 frame (a pure-snapshot recovery).
	resumedTxnSeq uint64
	// maxCommitTS is the highest durable MVCC commit instant the recovery folded
	// into the clock floor — the maximum over every replayed commit marker AND
	// the snapshot's recorded capture instant. It is 0 when nothing durable
	// carried one.
	maxCommitTS uint64
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
//
// OpenSimStore is the string/float64 SPECIALISATION of [openSimTypedStore]
// (rmp #2473): it opens the codec-generic core with [txn.NewStringCodec] and
// [txn.NewFloat64WeightCodec] and bolts a [cypher.Engine] on top. The engine is
// what pins the specialisation — [cypher.NewEngineWithStoreAndSchema] takes a
// *txn.Store[string, float64] and nothing else — so every Cypher-driven
// scenario necessarily runs on the string key codec, and the codec matrix
// (codec_matrix.go) drives the other pairs through the typed core directly.
func OpenSimStore(disk *SimDisk, cfg simStoreConfig) (*SimStore, error) {
	ts, err := openSimTypedStore(disk, cfg, txn.NewStringCodec(), txn.NewFloat64WeightCodec())
	if err != nil {
		return nil, err
	}
	// Re-register recovered schema so a UNIQUE/index declared before the crash
	// is enforced after restart, mirroring the production reopen path
	// ([cypher.NewEngineWithStoreAndSchema]).
	engine := cypher.NewEngineWithStoreAndSchema(ts.store, ts.schema.constraints, ts.schema.indexes)

	return &SimStore{
		disk:          ts.disk,
		cfg:           ts.cfg,
		graph:         ts.graph,
		store:         ts.store,
		wlog:          ts.wlog,
		engine:        engine,
		walOps:        ts.schema.walOps,
		clean:         ts.clean,
		resumedTxnSeq: ts.resumedTxnSeq,
		maxCommitTS:   ts.schema.maxCommitTS,
	}, nil
}

// simTypedStore is the codec-parameterised core of a durable simulator store:
// the recovered graph, the WAL-backed [txn.Store] and the recovery facts, for
// an ARBITRARY key/weight codec pair (rmp #2473). [SimStore] is its
// string/float64 specialisation.
//
// It exists because the codec surface is otherwise unreachable from the
// simulator. Every Cypher-driven scenario goes through [cypher.Engine], which
// is fixed at *txn.Store[string, float64], so the string key codec and the
// float64 weight codec were the only two the whole DST ever exercised — and
// with them the only mapper.bin layout ever written was the frozen
// string-specialised version-1 one ([snapshot.WriteMapper] delegates to
// WriteMapperString for N == string). A store opened here with any other key
// codec drives the version-2 byte-mapper on the way out and
// [snapshot.ApplyMapperToGraphWithCodec] on the way back in.
//
// # Concurrency contract
//
// simTypedStore is NOT safe for concurrent use, matching [SimStore]: the codec
// matrix drives it from a single goroutine.
type simTypedStore[N comparable, W any] struct {
	disk   *SimDisk
	graph  *lpg.Graph[N, W]
	store  *txn.Store[N, W]
	wlog   *wal.Writer
	codec  txn.Codec[N]
	wcodec txn.WeightCodec[W]
	// schema carries what the reopen's recovery reported: the durable schema,
	// the replayed op count, and the sequence/clock floors.
	schema recoveredSchema
	cfg    simStoreConfig
	// clean mirrors [SimStore.clean]: the most recent recovery found no genuine
	// on-disk corruption (a benign torn tail is clean).
	clean bool
	// resumedTxnSeq mirrors [SimStore.resumedTxnSeq].
	resumedTxnSeq uint64
}

// openSimTypedStore opens (or reopens) a durable store over disk with an
// explicit key/weight codec pair. It is the shared body of [OpenSimStore] and
// of every codec-matrix arm, so a non-string arm exercises byte-for-byte the
// same recovery, WAL-reopen and transaction-sequence-resume plumbing the string
// arm does — the codecs are the ONLY difference between them.
func openSimTypedStore[N comparable, W any](
	disk *SimDisk, cfg simStoreConfig, codec txn.Codec[N], wcodec txn.WeightCodec[W],
) (*simTypedStore[N, W], error) {
	if disk == nil {
		return nil, fmt.Errorf("sim: OpenSimStore: nil disk")
	}
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
	// RESUME THE TRANSACTION SEQUENCE (rmp #2302, wired for rmp #2469). Recovery
	// derives the highest sequence any durable v3 frame carried; the EMBEDDER has
	// to feed it back, and this harness is an embedder. Left unseeded, the
	// reopened store mints 1 again and one WAL image ends up holding two different
	// transactions under one sequence number — which recovery's TxnSeq-suffix
	// atomicity filter survives only because frame contiguity happens to
	// disambiguate it, and which fuses an orphaned prefix into the wrong
	// transaction the moment a reopen follows a torn tail.
	resumeSeq := recovered.maxTxnSeq
	if cfg.noResumeTxnSeq {
		resumeSeq = 0 // sensitivity seam only; see simStoreConfig.noResumeTxnSeq.
	}
	// CAP THE PRODUCER, not only the replayer (rmp #2474). Until this task the
	// store was built with the UNCAPPED constructor, so cfg.maxTxnOps reached
	// recovery (recoverSimGraph, both cores) and nothing else: the simulator
	// could lower the replay bound but never the commit bound, and
	// [txn.ErrTransactionTooLarge] was unreachable under simulation at any
	// configured cap. Passing it here is behaviour-neutral for every existing
	// caller — they all carry maxTxnOps 0, which both constructors resolve to
	// [txn.DefaultMaxTxnOps] — and it is what lets a scenario open a store whose
	// producer refuses before its replayer would.
	producerCap := simMaxTxnOpsOption(cfg.maxTxnOps)
	if cfg.uncappedProducerSeam {
		producerCap = 0 // sensitivity seam only; see simStoreConfig.uncappedProducerSeam.
	}
	store := txn.NewStoreWithOptionsCapped(g, wlog, txn.Options[N, W]{
		Codec:        codec,
		WeightCodec:  wcodec,
		ResumeTxnSeq: resumeSeq,
	}, producerCap)
	return &simTypedStore[N, W]{
		disk:          disk,
		graph:         g,
		store:         store,
		wlog:          wlog,
		codec:         codec,
		wcodec:        wcodec,
		schema:        recovered,
		cfg:           cfg,
		clean:         clean,
		resumedTxnSeq: resumeSeq,
	}, nil
}

// Checkpoint runs ONE synchronous checkpoint over the typed store, publishing a
// self-sufficient snapshot through the SAME production checkpointer
// [SimStore.Checkpoint] drives. The store's own key codec is wired in as the
// mapper codec, so a non-string arm publishes the version-2 byte-mapper.
//
// No constraint/index spec providers are wired: a typed store has no
// [cypher.Engine] and therefore no DDL, so there is no schema for the snapshot
// to carry. DDL-across-the-boundary stays the string arm's scenario
// (ddl_checkpoint_crash.go).
func (s *simTypedStore[N, W]) Checkpoint() error {
	return runSimCheckpoint(s.disk, s.cfg.dir, s.graph, s.store, s.wlog)
}

// Crash models a HOST crash over a typed store, with the same semantics as
// [SimStore.Crash]: no graceful flush, every file reverts to the bytes a
// successful Sync made durable, and every not-yet-dir-fsync'd dirent is revoked.
func (s *simTypedStore[N, W]) Crash() {
	if s.disk != nil {
		s.disk.CrashHost()
	}
	s.store = nil
	s.wlog = nil
	s.graph = nil
}

// Close shuts the typed store down gracefully, flushing and fsyncing the WAL so
// every acknowledged commit is durable, with the same semantics as
// [SimStore.Close].
func (s *simTypedStore[N, W]) Close() error {
	if s.wlog == nil {
		return nil
	}
	err := s.wlog.Close()
	s.wlog = nil
	s.store = nil
	return err
}

// recoveredSchema carries the schema and op-count a recovery produced, decoupled
// from whether the WAL-only ([recovery.ReplayWAL]) or full snapshot+WAL
// ([recovery.OpenFS]) core ran.
type recoveredSchema struct {
	constraints []recovery.ConstraintRecord
	indexes     []recovery.IndexRecord
	walOps      int
	// maxTxnSeq is [recovery.Result.MaxTxnSeq]: the highest per-transaction
	// sequence any replayed v3 frame carried, which the reopened store resumes
	// from so no sequence is minted twice (rmp #2302).
	maxTxnSeq uint64
	// maxCommitTS is [recovery.Result.MaxCommitTS]: the highest durable MVCC
	// commit instant, over the WAL AND (on the full-stack path) the snapshot's
	// recorded capture instant. Recovery has already raised the graph's clock to
	// maxCommitTS+1 by the time it is read here (rmp #2309); it is carried out so
	// a scenario can adjudicate the floor against the durable record.
	maxCommitTS uint64
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
//
// It is generic over the key/weight codec pair (rmp #2473) so a codec-matrix
// arm recovers through exactly this code, and therefore through
// [snapshot.ApplyMapperToGraphWithCodec] on the full-stack path whenever the
// snapshot carries a version-2 byte-mapper.
func recoverSimGraph[N comparable, W any](
	disk *SimDisk,
	cfg simStoreConfig,
	codec txn.Codec[N],
	wcodec txn.WeightCodec[W],
) (*lpg.Graph[N, W], recoveredSchema, bool, error) {
	walPath := walPathFor(cfg.dir)

	// Full-stack recovery: a published snapshot folds an arbitrarily long WAL
	// prefix, so the graph MUST be rebuilt from it (the WAL alone no longer holds
	// the truncated prefix). recovery.OpenFS reads dir/snapshot + dir/wal, honours
	// the snapshot's persisted graph config, and truncates the benign WAL tail it
	// returns as part of the recovered Result.
	if cfg.dir != "" && hasDurableSnapshot(disk, cfg.dir) {
		res, err := recovery.OpenFS[N, W](
			simRecoveryFS{disk: disk}, cfg.dir,
			recovery.Options[N, W]{
				Codec:       codec,
				WeightCodec: wcodec,
				MaxTxnOps:   simMaxTxnOpsOption(cfg.maxTxnOps),
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
			maxTxnSeq:   res.MaxTxnSeq,
			maxCommitTS: res.MaxCommitTS,
		}, true, nil
	}

	// WAL-only recovery (no snapshot, or legacy mode): replay the committed prefix
	// into a graph with the simulator's configured shape.
	g := lpg.New[N, W](cfg.graphConfig)
	if !disk.Exists(walPath) {
		return g, recoveredSchema{}, true, nil
	}
	rh, err := disk.OpenFile(walPath, os.O_RDONLY)
	if err != nil {
		return nil, recoveredSchema{}, false, fmt.Errorf("sim: open WAL for replay: %w", err)
	}
	reader := wal.NewReader(rh, rh)
	replay, err := recovery.ReplayWAL[N, W](
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
	// The MVCC clock floor is restored by [recovery.ReplayWAL] itself (rmp #2522),
	// as it always was on the full-stack path: this harness used to carry a
	// hand-copied duplicate of that restore, which is exactly the shape of bug
	// #2522 was filed about. Both recovery cores now leave the graph with its
	// floor already raised past every durable instant.
	return g, recoveredSchema{
		constraints: replay.Constraints,
		indexes:     replay.Indexes,
		walOps:      replay.WALOps,
		maxTxnSeq:   replay.MaxTxnSeq,
		maxCommitTS: replay.MaxCommitTS,
	}, true, nil
}

// hasDurableSnapshot reports whether dir holds a snapshot the FULL
// snapshot+WAL recovery core must run over — either a published one at
// dir/snapshot, or one STRANDED at dir/snapshot.bak by a publish that a crash
// interrupted between its archive rename and its publish rename.
//
// The stranded case is the load-bearing half. Recovery owns the repair for it
// (store/recovery promotes the backup back to the live name before probing for
// a manifest), but it can only run that repair if it is called at all: a gate
// that admits only the live manifest would route an interrupted-publish
// directory to the WAL-only core, which replays a WAL whose prefix the previous
// checkpoint already truncated and so silently recovers a graph missing every
// checkpointed transaction. Production never faces this because
// [recovery.Open] is called unconditionally on the directory and decides
// internally; this restores the same decision boundary to the simulation.
//
// A directory with neither manifest still takes the WAL-only path, so every
// scenario that never publishes a snapshot is unaffected.
func hasDurableSnapshot(disk *SimDisk, dir string) bool {
	return disk.Exists(dir+"/"+simSnapshotName+"/manifest.json") ||
		disk.Exists(dir+"/"+simSnapshotName+".bak/manifest.json")
}

// simMaxTxnOpsOption maps the simulator's maxTxnOps convention (0 -> default,
// <0 -> unlimited) onto the convention BOTH the recovery options
// ([recovery.Options.MaxTxnOps]) and the capped store constructors
// ([txn.NewStoreWithOptionsCapped]) share: 0 -> default,
// [txn.MaxTxnOpsUnlimited] -> no cap, positive verbatim.
//
// One helper serves both sides deliberately. The producer cap must be <= the
// replay cap or a transaction could be made durable that recovery then refuses
// to replay (the invariant [txn.DefaultMaxTxnOps] documents); feeding both from
// a single simulator-side value makes them equal by construction, so the
// simulator cannot accidentally configure the unreplayable combination.
func simMaxTxnOpsOption(maxTxnOps int) int {
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

// WAL returns the live WAL writer this store commits through, so an oracle can
// read the writer's OWN view of durability — [wal.Writer.DurableOffset],
// [wal.Writer.Stats] and [wal.Writer.Poisoned] — alongside the durable byte
// image on the [SimDisk] (rmp #2472). Reading those two independently is the
// point: the byte image is what exists, the writer's counters are what it
// believes, and a watermark defect is a disagreement between them.
//
// The returned writer is OWNED by the store: a caller must only READ it.
// Appending or syncing through it directly bypasses the transaction layer's
// sequence minting and apply gate, and [SimStore.Close] closes it.
func (s *SimStore) WAL() *wal.Writer { return s.wlog }

// WALOps reports how many WAL ops the most recent recovery replayed back into
// the graph on open (0 for a freshly-created store).
func (s *SimStore) WALOps() int { return s.walOps }

// Clean reports whether the most recent recovery completed without genuine
// on-disk corruption (a benign torn tail counts as clean).
func (s *SimStore) Clean() bool { return s.clean }

// ResumedTxnSeq reports the transaction sequence this store continued from —
// [recovery.Result.MaxTxnSeq] fed back into [txn.Options.ResumeTxnSeq], so the
// first transaction it commits is assigned ResumedTxnSeq+1. It is 0 for a fresh
// store and for a recovery whose surviving WAL carried no v3 frame.
func (s *SimStore) ResumedTxnSeq() uint64 { return s.resumedTxnSeq }

// RecoveredMaxCommitTS reports the highest durable MVCC commit instant the
// recovery observed — over every replayed commit marker and, on the full-stack
// path, the snapshot's recorded capture instant. Recovery has already raised the
// graph's MVCC clock to one past it, so it is the durable floor a post-recovery
// commit must exceed (rmp #2309). It is 0 when nothing durable carried one.
func (s *SimStore) RecoveredMaxCommitTS() uint64 { return s.maxCommitTS }

// ClockNow reports the MVCC clock's currently published instant. Read
// immediately after a reopen it is the RECOVERED clock floor; read during a run
// it advances by one per published commit, which is what makes it a measure of
// how much MVCC traffic overlapped a concurrent operation.
func (s *SimStore) ClockNow() uint64 {
	if s.graph == nil {
		return 0
	}
	return s.graph.MVCCStats().Now
}

// Crash models a HOST crash — power failure, hard reset, hypervisor kill. It is
// an alias for [SimStore.CrashHost], so every scenario that has not consciously
// chosen otherwise gets the stronger of the two models. A scenario that really
// means SIGKILL calls [SimStore.CrashProcess] and says so.
func (s *SimStore) Crash() { s.CrashHost() }

// CrashHost models a power failure. It discards the in-memory engine, store, and
// WAL writer WITHOUT a graceful close, and then applies [SimDisk.CrashHost] to
// the disk: every file reverts to the bytes a [SimFileHandle.Sync] returning nil
// actually made durable, and every not-yet-dir-fsync'd dirent is revoked. What
// remains is exactly what stable storage held, ready for [OpenSimStore] to
// reopen and replay. The SimStore must not be used afterwards.
//
// It deliberately does NOT call s.wlog.Close(): a clean Close would flush and
// fsync the buffer, which is the opposite of a crash. Dropping the references
// lets the GC reclaim them; the only durable state is the SimDisk image.
//
// A frame the WAL had written but never fsync'd is therefore GONE, which is what
// makes an oracle of the form "the commit was acked, so the bytes are recovered"
// able to fail. Before rmp #2535 the byte image survived untouched and that
// oracle held whatever the engine did with fsync.
func (s *SimStore) CrashHost() {
	if s.disk != nil {
		s.disk.CrashHost()
	}
	s.dropRefs()
}

// CrashProcess models a SIGKILL of the process (kill -9). The process dies and
// its in-memory engine, store and WAL writer go with it, but the kernel does
// not: every byte already accepted by a write(2) and every directory entry
// already created survives for the next process to read, fsync'd or not (see
// [SimDisk.CrashProcess]).
//
// Only the WAL writer's own bufio buffer is lost, since that lives in the dead
// process's address space. Use it when the scenario means "the process was
// killed", and [SimStore.CrashHost] when it means "the machine went down" — the
// two are physically different events and, since rmp #2535, they leave
// different durable images.
func (s *SimStore) CrashProcess() {
	if s.disk != nil {
		s.disk.CrashProcess()
	}
	s.dropRefs()
}

// dropRefs releases the in-memory objects a crash of either kind destroys. It is
// what makes a crashed SimStore unusable rather than merely stale.
func (s *SimStore) dropRefs() {
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
func (s *SimStore) Checkpoint() error { return s.runCheckpoint(true) }

// checkpointWithoutSchemaSpecs publishes a checkpoint with the constraint and
// index spec providers DELIBERATELY unwired — the checkpointer as it behaved
// before #1464/#1755 taught it to persist the schema. It exists solely as the
// SENSITIVITY SEAM of the ddl-checkpoint-crash scenario (rmp #2464): a snapshot
// that carries no constraints.bin/indexdefs.bin for a graph that HAS
// constraints or indexes is not self-sufficient, so the checkpointer's phase-3
// re-verification refuses to truncate the WAL prefix, and the scenario's
// "the DDL frames are gone from the WAL" oracle must fire. Never call it from a
// scenario that is asserting correct behaviour.
func (s *SimStore) checkpointWithoutSchemaSpecs() error { return s.runCheckpoint(false) }

// runCheckpoint is the shared body of [SimStore.Checkpoint] and
// [SimStore.checkpointWithoutSchemaSpecs]. withSchemaSpecs selects whether the
// engine's constraint/index spec providers are wired into the checkpointer.
func (s *SimStore) runCheckpoint(withSchemaSpecs bool) error {
	var extra []checkpoint.Option[string, float64]
	if withSchemaSpecs {
		extra = append(extra,
			checkpoint.WithConstraintSpecs[string, float64](s.engine.ConstraintSpecsForSnapshot),
			checkpoint.WithIndexSpecs[string, float64](s.engine.IndexSpecsForSnapshot),
		)
	}
	return runSimCheckpoint(s.disk, s.cfg.dir, s.graph, s.store, s.wlog, extra...)
}

// runSimCheckpoint is the shared, codec-generic checkpoint body behind
// [SimStore.Checkpoint] and [simTypedStore.Checkpoint] (rmp #2473). It wires
// the production [checkpoint.Checkpointer] over the SimDisk with the store's
// OWN key codec as the mapper codec, so the mapper layout the snapshot carries
// is decided by the key type exactly as it is in production: string keys take
// the frozen version-1 layout, every other key type the version-2 codec-framed
// one.
//
// extra options are appended AFTER the three base ones, preserving the option
// order the string path used before the generalisation.
func runSimCheckpoint[N comparable, W any](
	disk *SimDisk, dir string, g *lpg.Graph[N, W], store *txn.Store[N, W], wlog *wal.Writer,
	extra ...checkpoint.Option[N, W],
) error {
	if dir == "" {
		return fmt.Errorf("sim: Checkpoint requires a full-stack store (opened with a checkpoint dir)")
	}
	if store == nil || wlog == nil {
		return fmt.Errorf("sim: Checkpoint on a closed/crashed store")
	}
	// storeMu is a throwaway: WithCommitSerialiser supersedes it (the engine's
	// commit mutex is private), exactly as the production engine wiring does.
	var unusedMu sync.Mutex
	opts := make([]checkpoint.Option[N, W], 0, 4+len(extra))
	opts = append(opts,
		checkpoint.WithCommitSerialiser[N, W](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[N, W](store.Codec()),
		// The store's own weight codec, so the snapshot's CSR weights column
		// survives a checkpoint for EVERY weight type the matrix drives and not
		// only the fixed-width primitives (rmp #2526).
		checkpoint.WithWeightCodec[N, W](store.WeightCodec()),
		checkpoint.WithSnapshotFS[N, W](simCheckpointBackend[N, W]{disk: disk}),
	)
	opts = append(opts, extra...)
	cp := checkpoint.New[N, W](checkpoint.Config{Dir: dir}, g, wlog, &unusedMu, opts...)
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
