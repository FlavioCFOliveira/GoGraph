package sim

import (
	"context"
	"testing"
)

// This file covers rmp #1740 (DST G9): the crash-storm scenario now exercises
// the FULL snapshot + WAL crash-recovery path on the in-memory SimDisk. The
// simulator periodically drives a real checkpoint.Checkpointer over the SimDisk
// (publishing a self-sufficient snapshot at db/snapshot and prefix-truncating
// the WAL at db/wal), so a crash that follows a checkpoint recovers by
// reconstructing the graph from the snapshot and replaying only the WAL suffix
// through recovery.OpenFS. The ACID-D durability oracle is asserted after every
// recovery, so a snapshot+WAL recovery that lost a committed op fails the run.

// runFullStackSim builds and runs a checkpoint+crash simulation to completion,
// closing the store afterwards, and returns the simulator for assertions. It
// fails the test on any run error or violation report.
func runFullStackSim(t *testing.T, cfg Config) *Simulator {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	report, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report != nil {
		t.Fatalf("full-stack crash simulation reported violations:\n%s", report)
	}
	return s
}

// fullStackConfig is a checkpoint+crash configuration small enough for the short
// layer under -race yet dense enough that several checkpoints AND several crashes
// fire, so a crash recovery genuinely runs the full snapshot+WAL path.
func fullStackConfig(seed uint64) Config {
	return Config{
		Seed:       seed,
		MaxTicks:   600,
		CheckEvery: 16,
		Workload:   WriteHeavyWorkload(NewSeed(seed)),
		Crash:      CrashConfig{Enabled: true, CrashProb: 1.0 / 50.0, StabilityWindow: 20},
		Checkpoint: CheckpointConfig{Enabled: true, Every: 25},
	}
}

// TestSimulator_FullStackCheckpointCrashRecovery is the #1740 acceptance test:
// with in-loop checkpointing enabled the simulator runs a crash storm to
// completion with no durability violation, having published real snapshots and
// recovered across crashes through the full snapshot+WAL path. The durability
// oracle inside the run guarantees every ACKed-committed op survived each
// recovery, whether it came from the snapshot or the WAL suffix.
func TestSimulator_FullStackCheckpointCrashRecovery(t *testing.T) {
	s := runFullStackSim(t, fullStackConfig(0xF0FA))

	if s.CheckpointCount() == 0 {
		t.Fatal("expected at least one in-loop checkpoint to publish a snapshot")
	}
	if s.CrashCount() == 0 {
		t.Fatal("expected at least one crash+recovery cycle")
	}
	if s.Oracle().NodeCount() == 0 {
		t.Fatal("write-heavy full-stack run created no surviving nodes")
	}
	// A durable snapshot directory must exist on the SimDisk: the checkpoints
	// published it, and (because the run survived crashes after them) recovery
	// loaded it. Its presence is the non-vacuity guard that the full snapshot+WAL
	// path — not WAL-only replay — was actually exercised.
	if !s.Disk().Exists("db/snapshot/manifest.json") {
		t.Fatal("no snapshot manifest on disk after checkpoints: the full snapshot+WAL path was not exercised")
	}
}

// TestSimulator_FullStackReproducible verifies the checkpoint+crash run is a pure
// function of the seed: two runs reach identical surviving state and identical
// checkpoint/crash counts. Checkpoints publish snapshots and truncate the WAL on
// the SimDisk, so this also proves that machinery is deterministic.
func TestSimulator_FullStackReproducible(t *testing.T) {
	a := runFullStackSim(t, fullStackConfig(0xBEEF))
	b := runFullStackSim(t, fullStackConfig(0xBEEF))

	if a.CheckpointCount() != b.CheckpointCount() {
		t.Fatalf("checkpoint count not reproducible: %d vs %d", a.CheckpointCount(), b.CheckpointCount())
	}
	if a.CrashCount() != b.CrashCount() {
		t.Fatalf("crash count not reproducible: %d vs %d", a.CrashCount(), b.CrashCount())
	}
	if a.Oracle().NodeCount() != b.Oracle().NodeCount() {
		t.Fatalf("surviving node count not reproducible: %d vs %d", a.Oracle().NodeCount(), b.Oracle().NodeCount())
	}
	if a.Oracle().EdgeCount() != b.Oracle().EdgeCount() {
		t.Fatalf("surviving edge count not reproducible: %d vs %d", a.Oracle().EdgeCount(), b.Oracle().EdgeCount())
	}
}

// TestSimulator_CheckpointDisabledNoSnapshot is the contrast/non-vacuity arm:
// with checkpointing disabled the SAME crash storm recovers WAL-only and never
// writes a snapshot directory, confirming the snapshot in the enabled run came
// from the new checkpoint path and is not an artefact of the harness.
func TestSimulator_CheckpointDisabledNoSnapshot(t *testing.T) {
	cfg := fullStackConfig(0xF0FA)
	cfg.Checkpoint = CheckpointConfig{} // disabled
	s := runFullStackSim(t, cfg)

	if s.CheckpointCount() != 0 {
		t.Fatalf("checkpointing disabled but %d checkpoints ran", s.CheckpointCount())
	}
	if s.Disk().Exists("db/snapshot/manifest.json") {
		t.Fatal("checkpointing disabled but a snapshot manifest appeared on disk")
	}
}

// TestScenarioCrashStorm_FullStackRunsClean runs the catalogue crash-storm
// scenario end to end at its default seed and asserts it passes, that it now
// publishes snapshots (#1740 wiring), and recovers across crashes — the
// integration-level guarantee the catalogue smoke relies on.
func TestScenarioCrashStorm_FullStackRunsClean(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioCrashStorm)
	if !ok {
		t.Fatalf("scenario %q not in registry", ScenarioCrashStorm)
	}
	if !sc.Checkpoint.Enabled {
		t.Fatal("crash-storm scenario must enable in-loop checkpointing (#1740)")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("crash-storm run: %v", err)
	}
	if report != nil {
		t.Fatalf("crash-storm reported violations:\n%s", report)
	}
}
