package sim

import (
	"context"
	"fmt"
	"os"
)

// UpgradeConfig parameterises an upgrade simulation: the seed driving the
// write workload, how many write ops to apply before the simulated upgrade
// boundary, and the workload factory. The write phase is fully deterministic
// from Seed so a failure reproduces exactly.
type UpgradeConfig struct {
	// Seed drives the deterministic write workload and the SimDisk sub-seed.
	Seed uint64
	// Ops is the number of write operations applied before the upgrade boundary.
	// Values <= 0 default to 400 — enough to populate nodes, edges, and a few
	// deletes so the parity check is meaningful.
	Ops int
	// Workload is the actor mix used for the write phase. When nil,
	// [WriteHeavyWorkload] is used so the durable image carries real structure.
	Workload func(*Seed) *Workload
	// IndexSpecs, when non-empty, are created before the write phase and
	// cross-checked for consistency after reopen, so the upgrade also guards
	// index durability across the boundary.
	IndexSpecs []IndexSpec
}

// UpgradeResult summarises an upgrade simulation: the durable counts written,
// the counts recovered after the reopen, and how many WAL ops the reopen's
// recovery replayed. A nil [UpgradeResult.Report] means full parity held.
type UpgradeResult struct {
	// WrittenNodes / WrittenEdges are the oracle-modelled counts at close.
	WrittenNodes int
	WrittenEdges int
	// RecoveredNodes / RecoveredEdges are the engine counts after the reopen.
	RecoveredNodes int64
	RecoveredEdges int64
	// ReplayedWALOps is how many committed WAL ops recovery replayed on reopen.
	ReplayedWALOps int
	// Report is non-nil when the post-reopen parity/durability/index check found
	// a violation (data loss, ghost state, or index drift).
	Report *SimReport
}

// Parity reports whether the upgrade reopened to full parity (no violation).
func (r UpgradeResult) Parity() bool { return r.Report == nil }

// RunUpgrade performs the PRIMARY upgrade simulation: it writes a deterministic
// workload through a real WAL-backed [SimStore] on a [SimDisk] image, closes the
// store gracefully (every ACKed commit durable), then reopens the SAME durable
// image through the real recovery path ([recovery.ReplayWAL], via
// [OpenSimStore]) — the cross-version boundary — and runs the full oracle
// parity, durability, and (optional) index-consistency check against the
// recovered engine.
//
// This guards the class of data-compatibility regressions the project has hit
// before (e.g. the v0.2.0->v0.3.x adjlist recovery panic): if the current
// recovery code cannot faithfully rebuild the graph from a durable image, the
// parity check reports the divergence rather than letting it pass silently.
//
// The returned error is a harness failure (store open/close or a write-phase
// engine error that should not happen on an honest workload); an invariant
// divergence is carried in the result's Report, not the error.
func RunUpgrade(ctx context.Context, cfg UpgradeConfig) (UpgradeResult, error) {
	if cfg.Ops <= 0 {
		cfg.Ops = 400
	}
	wlFactory := cfg.Workload
	if wlFactory == nil {
		wlFactory = WriteHeavyWorkload
	}

	seed := NewSeed(cfg.Seed)
	disk := NewSimDisk(NewSeed(cfg.Seed^diskSeedMix), 0)
	oracle := NewGraphOracle()
	workload := wlFactory(seed)

	// --- Write phase: drive a deterministic workload through a real WAL-backed
	// store, mirroring the simulator's execute+apply pipeline so the oracle ends
	// equal to the durable committed set. ---
	store, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("sim: upgrade: open store: %w", err)
	}
	eng := NewEngineAdapter(store.Engine())

	for _, spec := range cfg.IndexSpecs {
		name := upgradeIndexName(spec)
		ddl := fmt.Sprintf("CREATE INDEX %s FOR (n:%s) ON (n.%s)", name, spec.Label, spec.Property)
		if err := runDDLOnEngine(ctx, eng, ddl); err != nil {
			_ = store.Close()
			return UpgradeResult{}, fmt.Errorf("sim: upgrade: create index %s: %w", name, err)
		}
	}

	for i := 0; i < cfg.Ops; i++ {
		if err := ctx.Err(); err != nil {
			_ = store.Close()
			return UpgradeResult{}, err
		}
		actor := workload.SelectActor(seed)
		op := actor.NextOp(seed, oracle)
		committed := executeOnEngine(ctx, eng, op)
		applyOpToOracle(oracle, op, committed)
	}

	written := UpgradeResult{
		WrittenNodes: oracle.NodeCount(),
		WrittenEdges: oracle.EdgeCount(),
	}

	// Graceful close: flush+fsync the WAL so every ACKed commit is durable in the
	// SimDisk image. This is the pre-upgrade shutdown.
	if err := store.Close(); err != nil {
		return written, fmt.Errorf("sim: upgrade: close store: %w", err)
	}

	// --- Upgrade boundary: reopen the SAME durable image with the current code
	// through real recovery. ---
	reopened, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		return written, fmt.Errorf("sim: upgrade: reopen across boundary: %w", err)
	}
	defer func() { _ = reopened.Close() }()

	if !reopened.Clean() {
		// A clean durable image must reopen clean; a corruption fault here is a
		// hard durability fault.
		written.Report = upgradeReport(cfg.Seed, "recovery reported corruption on a clean image",
			ViolationACIDDurability)
		return written, nil
	}

	recoveredEng := NewEngineAdapter(reopened.Engine())
	n, _ := recoveredEng.NodeCount()
	e, _ := recoveredEng.EdgeCount()
	written.RecoveredNodes = n
	written.RecoveredEdges = e
	written.ReplayedWALOps = reopened.WALOps()

	// Full parity + durability check against the recovered engine. The checker
	// draws only for sampling, so a fixed seed keeps the probe deterministic.
	checker := NewInvariantChecker(NewSeed(cfg.Seed ^ checkerSeedMix))
	tick := int64(cfg.Ops)
	if v := checker.Check(tick, oracle, recoveredEng); len(v) > 0 {
		written.Report = finalReport(cfg.Seed, tick, oracle, v)
		return written, nil
	}
	if v := checker.CheckDurability(tick, oracle, recoveredEng); len(v) > 0 {
		written.Report = finalReport(cfg.Seed, tick, oracle, v)
		return written, nil
	}
	if len(cfg.IndexSpecs) > 0 {
		if v := CheckIndexConsistency(tick, oracle, recoveredEng, cfg.IndexSpecs...); len(v) > 0 {
			written.Report = finalReport(cfg.Seed, tick, oracle, v)
			return written, nil
		}
	}
	return written, nil
}

// CheckCorruptImageRejected verifies the FAIL-STOP guarantee: a durable image
// whose committed WAL prefix has been corrupted must be REJECTED by the reopen
// path, never silently opened onto. It writes a small workload, closes, corrupts
// the durable WAL bytes inside an already-committed frame, and asserts the
// reopen returns an error (the production recovery fail-stop contract, mirrored
// by [OpenSimStore]).
//
// It returns nil when the corruption was correctly rejected, and a descriptive
// error when a corrupt image was silently accepted (a durability bug).
func CheckCorruptImageRejected(ctx context.Context, seed uint64) error {
	s := NewSeed(seed)
	disk := NewSimDisk(NewSeed(seed^diskSeedMix), 0)
	oracle := NewGraphOracle()
	workload := WriteHeavyWorkload(s)

	store, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		return fmt.Errorf("sim: corrupt-image: open store: %w", err)
	}
	eng := NewEngineAdapter(store.Engine())
	// Enough ops to guarantee multiple committed frames so corrupting an early
	// byte lands inside a durable frame, not in an empty file.
	for i := 0; i < 200; i++ {
		if err := ctx.Err(); err != nil {
			_ = store.Close()
			return err
		}
		actor := workload.SelectActor(s)
		op := actor.NextOp(s, oracle)
		committed := executeOnEngine(ctx, eng, op)
		applyOpToOracle(oracle, op, committed)
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("sim: corrupt-image: close: %w", err)
	}

	if err := corruptSimWAL(disk); err != nil {
		return fmt.Errorf("sim: corrupt-image: corrupt WAL: %w", err)
	}

	// The reopen MUST fail-stop on the corruption rather than silently appending
	// onto it (which would permanently embed the corruption and drop every op
	// past the bad frame).
	reopened, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		return nil // correct: corruption rejected fail-stop.
	}
	_ = reopened.Close()
	return fmt.Errorf("sim: corrupt-image: a corrupted durable image was SILENTLY ACCEPTED (durability fail-stop violated)")
}

// corruptSimWAL flips bytes inside the committed prefix of the durable WAL image
// so a reopen's CRC/frame validation must reject it. It targets an offset well
// inside the file (past the header and first frame) so the corruption lands in
// an already-durable frame rather than a benign torn tail.
func corruptSimWAL(disk *SimDisk) error {
	img := disk.Snapshot()
	data, ok := img[simWALPath]
	if !ok || len(data) < 64 {
		return fmt.Errorf("WAL image too small to corrupt (%d bytes)", len(data))
	}
	// Corrupt a run of bytes around the middle of the committed prefix so the
	// damage is inside a frame's CRC-covered payload, not the trailing tail.
	mid := len(data) / 2
	for i := mid; i < mid+16 && i < len(data); i++ {
		data[i] ^= 0xFF
	}
	h, err := disk.OpenFile(simWALPath, os.O_RDWR)
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	if _, err := h.Seek(int64(mid), 0); err != nil {
		return err
	}
	_, err = h.Write(data[mid : mid+16])
	return err
}

// executeOnEngine runs op against the engine via the read or write path and
// reports whether a write committed cleanly, mirroring [Simulator.execute]. It
// is the shared write/read primitive the upgrade and metrics harnesses drive.
func executeOnEngine(ctx context.Context, eng *EngineAdapter, op Op) bool {
	var (
		res Result
		err error
	)
	if op.Kind.IsWrite() {
		res, err = eng.RunWrite(ctx, op.Cypher, op.Params)
	} else {
		res, err = eng.Run(ctx, op.Cypher, op.Params)
	}
	if err != nil {
		return false
	}
	for res.Next() {
	}
	drainErr := res.Err()
	_ = res.Close()
	return drainErr == nil
}

// runDDLOnEngine runs a DDL statement against the engine and drains it.
func runDDLOnEngine(ctx context.Context, eng *EngineAdapter, query string) error {
	res, err := eng.Run(ctx, query, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	drainErr := res.Err()
	_ = res.Close()
	return drainErr
}

// applyOpToOracle advances the oracle for op exactly as [Simulator.applyToOracle]
// does, shared by the upgrade and metrics harnesses.
func applyOpToOracle(oracle *GraphOracle, op Op, committed bool) {
	switch op.Kind {
	case OpCreate:
		if committed {
			oracle.ApplyCreate(op.Cypher, op.Params)
		}
	case OpMerge:
		if committed {
			oracle.ApplyMerge(op.Cypher, op.Params)
		}
	case OpDelete:
		if committed {
			oracle.ApplyDelete(op.Cypher, op.Params)
		}
	case OpUpdate:
		if committed {
			oracle.ApplyMatch(op.Cypher, op.Params)
		}
	case OpMatch:
		oracle.ApplyMatch(op.Cypher, op.Params)
	case OpMalformed:
		oracle.ApplyMalformed(op.Cypher, op.Params)
	}
}

// upgradeIndexName derives a stable, valid index identifier for a spec, so the
// CREATE INDEX statement and any later reference agree.
func upgradeIndexName(spec IndexSpec) string {
	return fmt.Sprintf("sim_upgrade_%s_%s", spec.Label, spec.Property)
}

// upgradeReport builds a single-violation report for an upgrade-boundary fault.
func upgradeReport(seed uint64, msg string, kind ViolationKind) *SimReport {
	return &SimReport{
		Seed:     seed,
		FailedOp: Op{Kind: OpMatch, Cypher: "<upgrade boundary>"},
		Violations: []Violation{{
			Kind:    kind,
			Op:      "<upgrade>",
			Message: msg,
		}},
	}
}
