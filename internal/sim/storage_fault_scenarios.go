package sim

// storage_fault_scenarios.go adds the MECHANICAL fault-injection cluster of the
// DST storage battery (rmp sprint 270, tasks ST4/ST5/ST6/ST8). Unlike the
// concurrent-durable cluster in durable_scenarios.go, every scenario here is
// single-goroutine, seed-driven and bit-reproducible: it drives one storage
// component (the csrfile atomic-publish path, the WAL recovery corruption
// gate, the checkpoint WAL-prefix reclamation, or the graph/io import/export
// round-trip) against a [SimDisk] fault and asserts the storage invariant the
// component must uphold.
//
//   - ST4 (csrfile-publish-fault): a fault mid-publish leaves EITHER no file at
//     the path OR the complete prior file, never a torn/partial one.
//   - ST5 (wal-corruption-failstop): genuine corruption inside an already-durable
//     interior WAL frame fail-stops recovery — the committed prefix before the
//     bad frame is intact, nothing after it is replayed, and the store refuses to
//     append onto the corruption.
//   - ST6 (checkpoint-dirfsync-fault): a post-rename parent-dir fsync failure in
//     the checkpoint's WAL-prefix truncation poisons the WAL writer yet leaves
//     the durable state fully recoverable on reopen.
//   - ST8 (io-roundtrip-fault): a graph/io export/import round-trip either
//     reproduces the graph exactly, or fails cleanly with a typed error and
//     leaves no partial artifact a re-import silently accepts as the graph.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"syscall"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// Standard names for the mechanical storage-fault scenarios (sprint 270).
const (
	// ScenarioCSRFilePublishFault is ST4: the csrfile atomic tmp->fsync->rename->
	// parent-fsync publish under an ENOSPC bound and an armed Sync fault.
	ScenarioCSRFilePublishFault = "csrfile-publish-fault"
	// ScenarioWALCorruptionFailStop is ST5: genuine corruption of an interior WAL
	// frame, then recovery fail-stop.
	ScenarioWALCorruptionFailStop = "wal-corruption-failstop"
	// ScenarioCheckpointDirFsyncFault is ST6: a checkpoint WAL-prefix truncation
	// whose post-rename parent-dir fsync faults, poisoning the writer.
	ScenarioCheckpointDirFsyncFault = "checkpoint-dirfsync-fault"
	// ScenarioIORoundTripFault is ST8: a graph/io export/import round-trip under a
	// clean run and under an ENOSPC export fault.
	ScenarioIORoundTripFault = "io-roundtrip-fault"
)

// -----------------------------------------------------------------------------
// ST4 — csrfile atomic publish under fault
// -----------------------------------------------------------------------------

// csrFileFaultTxns / csrFilePath name the ST4 fixture. The CSR is small so the
// short layer stays well under budget; the path is a subdirectory key so the
// publish exercises the parent-dir fsync step (a root-level key would be treated
// as durably linked on creation, see [SimDisk] isRootLevel).
const csrFilePath = "bulk/graph.csr"

// csrfilePublishFaultScenario is ST4. It publishes a deterministic CSR through
// the atomic [csrfile.WriteToFileWith] publish (tmp write -> fsync -> rename ->
// parent-dir fsync) over a [SimDisk], then attempts to REPUBLISH under (a) a
// capacity/ENOSPC bound and (b) an armed one-shot Sync fault — both faults land
// on the temp file BEFORE the atomic rename. The invariant it asserts is
// atomic publish: a failed publish leaves the path holding EITHER the complete
// previously-published CSR (verified byte-for-byte and by reconstructing it with
// the real reader) OR — for a first publish that never succeeded — nothing at
// all, never a torn/partial file. It is deterministic and bit-reproducible.
//
// Since rmp #2478 the arm is no longer a single float64 fixture read back
// through the default access pattern. Two parts were added:
//
//   - Part 3 enumerates the whole [csrfile.WeightKind] x [csrfile.AccessPattern]
//     grid, adjudicates the round-trip for every combination, and runs the
//     truncation battery — see csrfile_access_matrix.go.
//   - Part 4 replays the ENOSPC atomicity invariant at a weight kind DRAWN FROM
//     THE SEED and verifies the survivor through a seed-drawn access pattern,
//     so the atomicity guarantee is exercised somewhere other than float64.
//
// The two are complementary on purpose: the grid is enumerated so coverage does
// not depend on the draw, and the fault arm is drawn so the fault regime is not
// permanently welded to one weight width.
func csrfilePublishFaultScenario() Scenario {
	return Scenario{
		Name:        ScenarioCSRFilePublishFault,
		Description: "csrfile atomic publish under ENOSPC + armed Sync fault (prior file intact or absent, never torn), across every weight kind and access pattern",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xC5F11E00,
		run:         runCSRFilePublishFault,
	}
}

// runCSRFilePublishFault performs one ST4 run.
func runCSRFilePublishFault(_ context.Context, seed uint64) (*SimReport, error) {
	// --- Part 1: a fault BEFORE the rename must not touch the prior file. ---
	disk := NewSimDisk(NewSeed(seed), 0)
	fsys := simCSRFS{disk: disk}
	s := NewSeed(seed)

	c1, err := buildDeterministicCSR(s)
	if err != nil {
		return nil, fmt.Errorf("sim: ST4 build CSR_1: %w", err)
	}
	if _, err := csrfile.WriteToFileWith[float64](fsys, csrFilePath, c1); err != nil {
		return nil, fmt.Errorf("sim: ST4 clean publish of CSR_1: %w", err)
	}
	published1, err := disk.ReadFile(csrFilePath)
	if err != nil {
		return nil, fmt.Errorf("sim: ST4 read published CSR_1: %w", err)
	}
	// The reader must reconstruct CSR_1 exactly from the clean publish.
	if v := verifyCSRReadback(fsys, csrFilePath, c1); len(v) > 0 {
		return storageFaultReport(seed, v), nil
	}

	c2, err := buildDeterministicCSR(s) // a second CSR from the same draw stream
	if err != nil {
		return nil, fmt.Errorf("sim: ST4 build CSR_2: %w", err)
	}

	// (a) ENOSPC bound: cap the disk at exactly the current byte total so the
	// temp file's grow (its Truncate to the full layout size) breaches the budget
	// and returns ENOSPC BEFORE the rename. WriteToFile removes the temp and never
	// touches the path.
	disk.SetCapacity(int64(len(published1)), false)
	_, errENOSPC := csrfile.WriteToFileWith[float64](fsys, csrFilePath, c2)
	disk.SetCapacity(0, false) // lift the bound for the next arm
	if errENOSPC == nil {
		return storageFaultReport(seed, []Violation{{
			Kind: ViolationOracleDeviation, Op: "<csrfile-enospc-nonvacuity>",
			Message: "republish under an ENOSPC bound unexpectedly succeeded — the fault did not bite",
		}}), nil
	}
	if v := assertCSRFileUnchanged(disk, fsys, published1, c1, "enospc"); len(v) > 0 {
		return storageFaultReport(seed, v), nil
	}

	// (b) Armed one-shot Sync fault: ArmSyncFaultAt resets the Sync counter, so
	// the NEXT Sync — the temp file's fsync in this republish — faults BEFORE the
	// rename. Same atomic-publish guarantee: the path keeps CSR_1.
	disk.ArmSyncFaultAt(1)
	_, errSync := csrfile.WriteToFileWith[float64](fsys, csrFilePath, c2)
	if errSync == nil {
		return storageFaultReport(seed, []Violation{{
			Kind: ViolationOracleDeviation, Op: "<csrfile-sync-nonvacuity>",
			Message: "republish under an armed Sync fault unexpectedly succeeded — the fault did not bite",
		}}), nil
	}
	if v := assertCSRFileUnchanged(disk, fsys, published1, c1, "sync-fault"); len(v) > 0 {
		return storageFaultReport(seed, v), nil
	}

	// --- Part 2: a FIRST-publish fault must leave NO file (a reader sees the
	// absent-file error, never a torn artefact). ---
	disk2 := NewSimDisk(NewSeed(seed^0x9E3779B97F4A7C15), 0)
	fsys2 := simCSRFS{disk: disk2}
	c3, err := buildDeterministicCSR(NewSeed(seed))
	if err != nil {
		return nil, fmt.Errorf("sim: ST4 build CSR_3: %w", err)
	}
	// A tiny capacity guarantees the temp file's grow fails on the first publish,
	// so the path is never created.
	disk2.SetCapacity(csrFileFirstPublishCap, false)
	_, errFirst := csrfile.WriteToFileWith[float64](fsys2, csrFilePath, c3)
	if errFirst == nil {
		return storageFaultReport(seed, []Violation{{
			Kind: ViolationOracleDeviation, Op: "<csrfile-first-nonvacuity>",
			Message: "first publish under a tiny capacity unexpectedly succeeded — the fault did not bite",
		}}), nil
	}
	var v []Violation
	if disk2.Exists(csrFilePath) {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: "<csrfile-first>",
			Message: "a failed first publish left a file at the path (torn/partial artefact)",
		})
	}
	if disk2.Exists(csrFilePath + ".tmp") {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: "<csrfile-first>",
			Message: "a failed first publish left the temp file behind (not cleaned up)",
		})
	}
	if _, rerr := csrfile.OpenWith(fsys2, csrFilePath); rerr == nil {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: "<csrfile-first>",
			Message: "the reader accepted a file at the path after a failed first publish",
		})
	}
	if len(v) > 0 {
		return storageFaultReport(seed, v), nil
	}

	// --- Part 3: the WeightKind x AccessPattern grid, the truncation battery
	// and the Reinterpret alignment sweep (rmp #2478). ---
	matrix, err := runCSRFileAccessMatrix(seed)
	if err != nil {
		return nil, fmt.Errorf("sim: ST4 access matrix: %w", err)
	}
	// The scenario reports one list, but the two gates stay distinguishable by
	// Kind: a verdict violation is an ACID_* kind, a non-vacuity violation is
	// ORACLE_DEVIATION. The dedicated tests report them apart.
	v = append(v, matrix.verdict...)
	v = append(v, matrix.vacuity...)
	if len(v) > 0 {
		return storageFaultReport(seed, v), nil
	}

	// --- Part 4: the atomicity invariant at a SEED-DRAWN weight kind. ---
	if fv, ferr := csrfileFaultAtDrawnWeightKind(seed); ferr != nil {
		return nil, fmt.Errorf("sim: ST4 drawn-weight fault arm: %w", ferr)
	} else if len(fv) > 0 {
		return storageFaultReport(seed, fv), nil
	}
	return nil, nil
}

// csrfileFaultAtDrawnWeightKind replays ST4's ENOSPC atomicity invariant at a
// weight kind and an access pattern drawn from the seed, rather than at the
// float64 fixture the arm was welded to before rmp #2478.
//
// The republish uses a DIFFERENT topology from the published one, so "the prior
// file is unchanged" is a real comparison: had any byte of the failed republish
// reached the path, the image would differ from the one captured before it.
func csrfileFaultAtDrawnWeightKind(seed uint64) ([]Violation, error) {
	s := NewSeed(seed ^ 0xC5F1_2478)
	arms := csrfileWeightArms()
	patterns := csrfileAccessPatterns()
	arm := arms[s.IntN(len(arms))]
	pattern := patterns[s.IntN(len(patterns))]
	first := buildCSRFileShape(s)
	second := buildCSRFileShape(s)

	disk := NewSimDisk(NewSeed(seed), 0)
	fsys := simCSRFS{disk: disk}
	if err := arm.publish(fsys, csrfileMatrixPath, first); err != nil {
		return nil, fmt.Errorf("clean publish at %s: %w", arm.label(), err)
	}
	published, err := disk.ReadFile(csrfileMatrixPath)
	if err != nil {
		return nil, fmt.Errorf("read published %s: %w", arm.label(), err)
	}
	if _, v := arm.verify(fsys, csrfileMatrixPath, first, pattern); len(v) > 0 {
		return v, nil
	}

	// Cap the disk at exactly its current total so the republish's temp-file
	// grow breaches the budget and fails with ENOSPC BEFORE the rename.
	disk.SetCapacity(int64(len(published)), false)
	republishErr := arm.publish(fsys, csrfileMatrixPath, second)
	disk.SetCapacity(0, false)
	if republishErr == nil {
		return []Violation{{
			Kind: ViolationOracleDeviation, Op: "<csrfile-drawn-weight-nonvacuity>",
			Message: fmt.Sprintf("the republish of a %s csrfile under an ENOSPC bound succeeded —"+
				" the fault did not bite, so the atomicity assertion below is vacuous", arm.label()),
		}}, nil
	}

	var v []Violation
	if disk.Exists(csrfileMatrixPath + ".tmp") {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: "<csrfile-drawn-weight>",
			Message: fmt.Sprintf("[%s] a failed republish left the temp file behind", arm.label()),
		})
	}
	got, err := disk.ReadFile(csrfileMatrixPath)
	if err != nil {
		return append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<csrfile-drawn-weight>",
			Message: fmt.Sprintf("[%s] the prior published csrfile vanished after a failed"+
				" republish: %v", arm.label(), err),
		}), nil
	}
	if !bytes.Equal(got, published) {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: "<csrfile-drawn-weight>",
			Message: fmt.Sprintf("[%s] the prior published csrfile changed after a failed republish"+
				" (a torn write reached the path): got %d bytes want %d",
				arm.label(), len(got), len(published)),
		})
	}
	// The survivor must still reconstruct the FIRST topology exactly, read back
	// through the drawn access pattern.
	_, rv := arm.verify(fsys, csrfileMatrixPath, first, pattern)
	return append(v, rv...), nil
}

// csrFileFirstPublishCap is a byte budget smaller than any real csrfile layout,
// so a first publish's temp-file grow always fails with ENOSPC.
const csrFileFirstPublishCap = 32

// assertCSRFileUnchanged checks that after a failed republish the path still
// holds the previously-published CSR byte-for-byte AND that the real reader
// reconstructs it exactly, and that no temp file leaked. tag names the fault for
// the violation message.
func assertCSRFileUnchanged(disk *SimDisk, fsys simCSRFS, wantBytes []byte, wantCSR *csr.CSR[float64], tag string) []Violation {
	var v []Violation
	if disk.Exists(csrFilePath + ".tmp") {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: "<csrfile-" + tag + ">",
			Message: "a failed republish left the temp file behind (not cleaned up)",
		})
	}
	got, err := disk.ReadFile(csrFilePath)
	if err != nil {
		return append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<csrfile-" + tag + ">",
			Message: "the prior published csrfile vanished after a failed republish: " + err.Error(),
		})
	}
	if !bytes.Equal(got, wantBytes) {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: "<csrfile-" + tag + ">",
			Message: fmt.Sprintf("the prior published csrfile changed after a failed republish (a torn/partial write reached the path): got %d bytes want %d", len(got), len(wantBytes)),
		})
	}
	v = append(v, verifyCSRReadback(fsys, csrFilePath, wantCSR)...)
	return v
}

// verifyCSRReadback opens the csrfile at csrFilePath with the real reader and
// checks it reconstructs want exactly (vertices, edges, and float64 weights).
func verifyCSRReadback(fsys simCSRFS, path string, want *csr.CSR[float64]) []Violation {
	r, err := csrfile.OpenWith(fsys, path)
	if err != nil {
		return []Violation{{
			Kind: ViolationACIDConsistency, Op: "<csrfile-read>",
			Message: "the real reader rejected the published csrfile: " + err.Error(),
		}}
	}
	defer func() { _ = r.Close() }()
	if !slices.Equal(r.Vertices(), want.VerticesSlice()) {
		return []Violation{{
			Kind: ViolationACIDConsistency, Op: "<csrfile-read>",
			Message: "reconstructed vertices offsets differ from the published CSR",
		}}
	}
	if !slices.Equal(r.Edges(), want.EdgesSlice()) {
		return []Violation{{
			Kind: ViolationACIDConsistency, Op: "<csrfile-read>",
			Message: "reconstructed edges differ from the published CSR",
		}}
	}
	gotW, _ := r.WeightsFloat64()
	if !slices.Equal(gotW, want.WeightsSlice()) {
		return []Violation{{
			Kind: ViolationACIDConsistency, Op: "<csrfile-read>",
			Message: "reconstructed weights differ from the published CSR",
		}}
	}
	return nil
}

// buildDeterministicCSR builds a small, valid, seed-derived directed CSR with
// float64 weights. Every destination is in range and the offsets are monotonic
// by construction, so [csr.CSR.Validate] passes; it is returned only as a
// defensive check against a future edit that breaks the invariant.
func buildDeterministicCSR(s *Seed) (*csr.CSR[float64], error) {
	order := 8 + s.IntN(24) // 8..31 nodes
	vertices := make([]uint64, order+1)
	var edges []graph.NodeID
	var weights []float64
	for src := 0; src < order; src++ {
		deg := s.IntN(4) // 0..3 out-edges
		for j := 0; j < deg; j++ {
			edges = append(edges, graph.NodeID(s.Uint64N(uint64(order))))
			weights = append(weights, float64(s.Uint64N(1000)))
		}
		vertices[src+1] = uint64(len(edges))
	}
	c := csr.FromArrays[float64](vertices, edges, weights, uint64(order), uint64(len(edges)))
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// -----------------------------------------------------------------------------
// ST5 — recovery genuine-corruption fail-stop
// -----------------------------------------------------------------------------

// walCorruptTxns is how many single-node transactions ST5 commits before
// corrupting an interior frame. It is large enough that the corrupted middle
// frame sits well past the first committed transaction and well before the last,
// so a non-empty prefix survives and a non-empty suffix is lost.
const walCorruptTxns = 24

// walCorruptionFailStopScenario is ST5. It commits many single-node
// transactions to a WAL-only [SimStore] over a [SimDisk], closes cleanly (no
// torn tail), then flips a byte inside an already-durable INTERIOR frame with
// the [SimDisk.CorruptRange] sector-corruption injector so that frame fails its
// CRC. It asserts the recovery corruption gate: replay reports genuine
// corruption (not a benign torn tail), recovers EXACTLY the committed prefix
// before the bad frame (no acknowledged commit before it lost, no phantom commit
// after it replayed), and [OpenSimStore] fail-stops rather than appending onto
// the corruption. It is deterministic and bit-reproducible.
func walCorruptionFailStopScenario() Scenario {
	return Scenario{
		Name:        ScenarioWALCorruptionFailStop,
		Description: "corrupt an interior committed WAL frame; recovery fail-stops (prefix intact, suffix not replayed, refuses to append)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x3A1C0FFA,
		run:         runWALCorruptionFailStop,
	}
}

// walCorruptionOptions varies one ST5 run. The zero value is the scenario
// exactly as registered; the field exists for the CONTROL arm that must observe
// what the run emits with the fault WITHHELD.
type walCorruptionOptions struct {
	// skipCorruption withholds the interior-frame byte flip, leaving every other
	// step of the run untouched: the same commits, the same clean close, the same
	// clean and prefix replays, the same reopen. It exists because
	// "store.wal.Decode.errors" is emitted for a BENIGN torn tail as well as for
	// genuine corruption (store/wal/format.go), so the required-counters
	// declaration for this scenario is only falsifiable against a run that proves
	// the counter stays at zero when nothing is corrupted. See
	// [walCorruptionDecl]. The run's own oracles are EXPECTED to report
	// violations under it — the control reads the metrics, not the verdict.
	skipCorruption bool
}

// runWALCorruptionFailStop performs one ST5 run as registered.
func runWALCorruptionFailStop(ctx context.Context, seed uint64) (*SimReport, error) {
	return runWALCorruptionFailStopWith(ctx, seed, walCorruptionOptions{})
}

// runWALCorruptionFailStopWith performs one ST5 run under opts.
func runWALCorruptionFailStopWith(ctx context.Context, seed uint64, opts walCorruptionOptions) (*SimReport, error) {
	disk := NewSimDisk(NewSeed(seed), 0)
	cfg := defaultSimStoreConfig() // WAL-only layout (dir == ""), recovered via ReplayWAL
	walPath := walPathFor(cfg.dir)
	codec := txn.NewStringCodec()
	wcodec := txn.NewFloat64WeightCodec()
	maxTxnOps := resolveSimMaxTxnOps(cfg.maxTxnOps)

	// Commit walCorruptTxns single-node transactions, each its own durable commit.
	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: ST5 open store: %w", err)
	}
	for i := 0; i < walCorruptTxns; i++ {
		if err := commitCreatePerson(ctx, st.Engine(), fmt.Sprintf("wc%d", i), i); err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("sim: ST5 commit %d: %w", i, err)
		}
	}
	// Clean shutdown flushes + fsyncs, so the WAL image is a sequence of complete
	// frames with no torn tail.
	if err := st.Close(); err != nil {
		return nil, fmt.Errorf("sim: ST5 close: %w", err)
	}

	cleanBytes, err := disk.ReadFile(walPath)
	if err != nil {
		return nil, fmt.Errorf("sim: ST5 read WAL image: %w", err)
	}

	// Enumerate the durable frame boundaries. The reader stops at clean EOF, so
	// TailError must be nil — a torn tail here would mean the close did not flush.
	offsets, plens, tailErr := walFrameLayout(cleanBytes)
	if tailErr != nil {
		return nil, fmt.Errorf("sim: ST5 clean WAL has a non-clean tail: %w", tailErr)
	}
	if len(offsets) < 3 {
		return nil, fmt.Errorf("sim: ST5 too few WAL frames (%d) to corrupt an interior one", len(offsets))
	}

	// Sanity anchor: the clean WAL replays to exactly walCorruptTxns nodes with no
	// corruption. This pins that each transaction added one node and that the full
	// prefix is what corruption is measured against.
	kFull, cleanErr, err := replayOrder(ctx, cleanBytes, cfg.graphConfig, codec, wcodec, maxTxnOps)
	if err != nil {
		return nil, fmt.Errorf("sim: ST5 clean replay: %w", err)
	}
	if cleanErr != nil {
		return nil, fmt.Errorf("sim: ST5 clean replay was not clean: %w", cleanErr)
	}
	if kFull != walCorruptTxns {
		return nil, fmt.Errorf("sim: ST5 clean replay recovered %d nodes, want %d", kFull, walCorruptTxns)
	}

	// Corrupt the first payload byte of an interior frame (the middle one) so it
	// fails CRC. The length field is untouched, so this is a genuine CRC mismatch,
	// not a torn/over-declared tail.
	mid := len(offsets) / 2
	if plens[mid] == 0 {
		return nil, fmt.Errorf("sim: ST5 middle frame %d has an empty payload", mid)
	}
	corruptOff := offsets[mid] + int64(wal.HeaderSize)
	if !opts.skipCorruption {
		if err := disk.CorruptRange(walPath, corruptOff, 1); err != nil {
			return nil, fmt.Errorf("sim: ST5 corrupt frame %d: %w", mid, err)
		}
	}

	// The clean prefix up to (but excluding) the corrupt frame is what recovery
	// must reconstruct — nothing more, nothing less.
	kExpected, prefixErr, err := replayOrder(ctx, cleanBytes[:offsets[mid]], cfg.graphConfig, codec, wcodec, maxTxnOps)
	if err != nil {
		return nil, fmt.Errorf("sim: ST5 prefix replay: %w", err)
	}
	if prefixErr != nil {
		return nil, fmt.Errorf("sim: ST5 prefix replay was not clean: %w", prefixErr)
	}

	corruptBytes, err := disk.ReadFile(walPath)
	if err != nil {
		return nil, fmt.Errorf("sim: ST5 read corrupt WAL image: %w", err)
	}
	kRecovered, corruptErr, err := replayOrder(ctx, corruptBytes, cfg.graphConfig, codec, wcodec, maxTxnOps)
	if err != nil {
		return nil, fmt.Errorf("sim: ST5 corrupt replay: %w", err)
	}

	var v []Violation
	// The stop condition must be classified as genuine corruption, not a benign
	// torn tail.
	if corruptErr == nil {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: "<wal-corruption>",
			Message: "replay of a corrupt interior frame reported clean — corruption was not detected",
		})
	} else if !errors.Is(corruptErr, wal.ErrCRCMismatch) {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: "<wal-corruption>",
			Message: "corrupt interior frame produced " + corruptErr.Error() + ", want a CRC mismatch",
		})
	}
	// Recovery must reconstruct EXACTLY the committed prefix: no acknowledged
	// commit before the corrupt frame is lost, and no commit after it is replayed.
	if kRecovered != kExpected {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<wal-corruption>",
			Message: fmt.Sprintf("recovery of the corrupt WAL kept %d nodes, want exactly the committed prefix of %d (nothing after the bad frame, nothing before it lost)", kRecovered, kExpected),
		})
	}
	// Non-vacuity: the corruption is genuinely interior — a non-empty prefix
	// survived and a non-empty suffix was lost.
	if kExpected < 1 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<wal-corruption-nonvacuity>",
			Message: "no committed transaction survived before the corrupt frame — the fault was not interior",
		})
	}
	if kExpected >= walCorruptTxns {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<wal-corruption-nonvacuity>",
			Message: fmt.Sprintf("the corrupt frame was past every commit (prefix %d >= %d) — no committed transaction was lost", kExpected, walCorruptTxns),
		})
	}

	// The store must fail-stop on reopen and refuse to append onto the corruption.
	st2, reopenErr := OpenSimStore(disk, cfg)
	if reopenErr == nil {
		_ = st2.Close()
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<wal-corruption>",
			Message: "OpenSimStore succeeded over a corrupt WAL — it would append onto the corruption",
		})
	} else if !errors.Is(reopenErr, wal.ErrCRCMismatch) {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<wal-corruption>",
			Message: "OpenSimStore over a corrupt WAL failed with " + reopenErr.Error() + ", want a CRC-mismatch fail-stop",
		})
	}

	if len(v) > 0 {
		return storageFaultReport(seed, v), nil
	}
	return nil, nil
}

// walFrameLayout iterates the frames of a WAL byte image and returns, for each
// frame, its start byte offset and payload length, plus the tail error (nil at
// a clean EOF). It never mutates the input.
func walFrameLayout(image []byte) (offsets []int64, plens []int, tailErr error) {
	r := wal.NewReader(bytes.NewReader(image), nil)
	var off int64
	for f := range r.Frames() {
		offsets = append(offsets, off)
		plens = append(plens, len(f.Payload))
		off += int64(wal.HeaderSize + len(f.Payload))
	}
	return offsets, plens, r.TailError()
}

// replayOrder replays a WAL byte image into a fresh graph and returns the
// recovered node count (adjacency order), the replay stop condition
// ([recovery.ReplayResult.TailErr] — nil when clean or a benign torn tail), and
// a harness error (only for a ctx cancellation). It is the shared measurement
// ST5 uses for the clean, prefix, and corrupt images.
func replayOrder(ctx context.Context, image []byte, gcfg adjlist.Config, codec txn.Codec[string], wcodec txn.WeightCodec[float64], maxTxnOps int) (order uint64, tailErr, err error) {
	g := lpg.New[string, float64](gcfg)
	r := wal.NewReader(bytes.NewReader(image), nil)
	res, rerr := recovery.ReplayWAL[string, float64](ctx, r, g, codec, wcodec, maxTxnOps)
	if rerr != nil {
		return 0, nil, rerr
	}
	return g.AdjList().Order(), res.TailErr, nil
}

// -----------------------------------------------------------------------------
// ST6 — checkpoint WAL-truncate post-rename dir-fsync fault + poison
// -----------------------------------------------------------------------------

// checkpointDirFsyncTxns is how many single-node transactions ST6 commits before
// the faulted checkpoint. It is modest so the short layer stays under budget.
const checkpointDirFsyncTxns = 16

// checkpointDirFsyncFaultScenario is ST6. Against a full-stack durable
// [SimStore] (WAL + snapshot) on a [SimDisk] it commits several node
// transactions, arms a one-shot fault on the WAL's post-rename parent-dir fsync
// ([SimDisk.ArmParentDirSyncFaultForPath]), then runs a checkpoint. The
// checkpoint publishes a durable self-sufficient snapshot, then its WAL-prefix
// truncation renames the suffix-only WAL and fsyncs the parent dir — which
// faults. It asserts the [wal.Writer] poisonAfterRename contract: the checkpoint
// errors, the WAL writer is poisoned (every later Append/Sync returns the sticky
// error), YET a reopen through real recovery reconstructs the exact committed
// state (no committed op lost, no phantom, no torn transaction). It is
// deterministic and bit-reproducible.
func checkpointDirFsyncFaultScenario() Scenario {
	return Scenario{
		Name:        ScenarioCheckpointDirFsyncFault,
		Description: "checkpoint WAL-truncate post-rename dir-fsync fault poisons the writer yet recovery restores the exact committed state",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xD125F00D,
		run:         runCheckpointDirFsyncFault,
	}
}

// runCheckpointDirFsyncFault performs one ST6 run. It abandons the poisoned
// writer (the production contract is that the owner reopens the WAL) and reopens
// over the SAME durable image WITHOUT crashing the disk, so the invariant is the
// "on reopen" recovery the poisonAfterRename doc describes.
func runCheckpointDirFsyncFault(ctx context.Context, seed uint64) (*SimReport, error) {
	disk := NewSimDisk(NewSeed(seed^durableDiskSeedMix), 0)
	cfg := fullStackStoreConfig()  // dir == "db": WAL at db/wal, snapshot at db/snapshot
	walPath := walPathFor(cfg.dir) // "db/wal"

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: ST6 open store: %w", err)
	}

	committed := make(map[string]struct{}, checkpointDirFsyncTxns)
	for i := 0; i < checkpointDirFsyncTxns; i++ {
		name := fmt.Sprintf("cp%d", i)
		if err := commitCreatePerson(ctx, st.Engine(), name, i); err != nil {
			st.Crash()
			return nil, fmt.Errorf("sim: ST6 commit %d: %w", i, err)
		}
		committed[name] = struct{}{}
	}

	// Arm the one-shot fault on the WAL's post-rename parent-dir fsync. The
	// snapshot publish that precedes the truncate fsyncs only snapshot-component
	// paths, so this fault fires precisely on the WAL-truncate dir-fsync.
	disk.ArmParentDirSyncFaultForPath(walPath)

	cpErr := st.Checkpoint()
	if cpErr == nil {
		st.Crash()
		return storageFaultReport(seed, []Violation{{
			Kind: ViolationOracleDeviation, Op: "<dirfsync-nonvacuity>",
			Message: "the checkpoint succeeded despite the armed post-rename dir-fsync fault — the fault did not bite",
		}}), nil
	}

	// Invariant (a): the fault poisons the WAL writer — every later Sync/Append
	// returns the sticky error.
	var v []Violation
	if syncErr := st.wlog.Sync(); syncErr == nil {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<poison>",
			Message: "WAL Sync succeeded after a post-rename dir-fsync fault — the writer was not poisoned",
		})
	}
	if appendErr := st.wlog.Append([]byte("post-poison")); appendErr == nil {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<poison>",
			Message: "WAL Append succeeded after a post-rename dir-fsync fault — the writer was not poisoned",
		})
	}
	if len(v) > 0 {
		st.Crash()
		return storageFaultReport(seed, v), nil
	}

	// Abandon the poisoned writer WITHOUT crashing the disk (Close is best-effort;
	// it touches only the orphaned pre-truncate handle, never the durable
	// suffix-only WAL). Then reopen over the same image through real recovery.
	_ = st.Close()

	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: ST6 reopen: %w", err)
	}
	defer func() { _ = st2.Close() }()

	recovered, partial, err := recoveredPersonNames(ctx, st2.Engine())
	if err != nil {
		return nil, fmt.Errorf("sim: ST6 read recovered graph: %w", err)
	}

	// Invariant (b): recovery restores the exact committed state.
	for _, missing := range setMinus(committed, recovered) {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: "<recovery>",
			Message: fmt.Sprintf("committed node %q lost after the poisoned checkpoint (recovered=%d committed=%d)", missing, len(recovered), len(committed)),
		})
	}
	for _, phantom := range setMinus(recovered, committed) {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: "<recovery>",
			Message: fmt.Sprintf("recovered node %q was never committed (phantom after the poisoned checkpoint)", phantom),
		})
	}
	for _, torn := range partial {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: "<recovery>",
			Message: fmt.Sprintf("recovered node %q lacks its age property (a torn transaction was resurrected)", torn),
		})
	}
	if len(v) > 0 {
		return storageFaultReport(seed, v), nil
	}
	return nil, nil
}

// -----------------------------------------------------------------------------
// ST8 — import/export round-trip under fault
// -----------------------------------------------------------------------------

// ioRoundTripFaultScenario is ST8. It builds a deterministic directed graph and
// round-trips it through the graph/io CSV and JSONL edge-list formats over a
// [SimDisk]: first a clean export/import that must reproduce the graph exactly,
// then an export under a tight ENOSPC bound that must fail cleanly with a typed
// error and leave no partial artefact a re-import silently accepts as the graph.
// GraphML round-trips an lpg.Graph with labels and properties rather than the
// weighted edge list the CSV/JSONL phase models, so it is covered by a dedicated
// property-graph round-trip ([graphmlRoundTripClean] / [graphmlExportFaultFailsClean])
// alongside the edge-list formats. It is deterministic and bit-reproducible.
func ioRoundTripFaultScenario() Scenario {
	return Scenario{
		Name:        ScenarioIORoundTripFault,
		Description: "graph/io CSV+JSONL+GraphML export/import round-trip: exact when clean, clean typed failure with no silently-accepted partial under ENOSPC",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x109F0117,
		run:         runIORoundTripFault,
	}
}

// ioFormat abstracts one graph/io edge-list format so the round-trip logic is
// written once and driven for both CSV and JSONL.
type ioFormat struct {
	export func(w io.Writer, a *adjlist.AdjList[string, int64]) (int, error)
	imp    func(r io.Reader) (*adjlist.AdjList[string, int64], error)
	name   string
	path   string
}

// ioRoundTripFormats returns the edge-list formats ST8 exercises. The config is
// directed simple, matching the modelled graph, so the import rebuilds the same
// edge set. CSV uses the default options (comma, no header, the default byte
// cap); JSONL takes the same adjacency config.
func ioRoundTripFormats() []ioFormat {
	cfg := adjlist.Config{Directed: true, Multigraph: false}
	csvOpts := csv.DefaultOptions()
	csvOpts.Directed = true
	return []ioFormat{
		{
			name: "csv",
			path: "io/graph.csv",
			export: func(w io.Writer, a *adjlist.AdjList[string, int64]) (int, error) {
				return csv.Write(w, a, csvOpts)
			},
			imp: func(r io.Reader) (*adjlist.AdjList[string, int64], error) {
				a, _, err := csv.ReadInto(r, csvOpts)
				return a, err
			},
		},
		{
			name:   "jsonl",
			path:   "io/graph.jsonl",
			export: jsonl.Write,
			imp: func(r io.Reader) (*adjlist.AdjList[string, int64], error) {
				a, _, err := jsonl.ReadInto(r, cfg)
				return a, err
			},
		},
	}
}

// runIORoundTripFault performs one ST8 run.
func runIORoundTripFault(_ context.Context, seed uint64) (*SimReport, error) {
	model, err := buildDeterministicAdjList(NewSeed(seed))
	if err != nil {
		return nil, fmt.Errorf("sim: ST8 build model: %w", err)
	}
	wantTriples := edgeTriples(model)

	for _, f := range ioRoundTripFormats() {
		// --- Phase A: a clean round-trip reproduces the model exactly. ---
		fullSize, v, herr := ioRoundTripClean(f, model, wantTriples)
		if herr != nil {
			return nil, fmt.Errorf("sim: ST8 %s clean round-trip: %w", f.name, herr)
		}
		if len(v) > 0 {
			return storageFaultReport(seed, v), nil
		}

		// --- Phase B: an export under a tight ENOSPC bound fails cleanly and
		// leaves no partial artefact a re-import accepts as the model. ---
		v, herr = ioExportFaultFailsClean(f, model, wantTriples, seed, fullSize)
		if herr != nil {
			return nil, fmt.Errorf("sim: ST8 %s export-fault: %w", f.name, herr)
		}
		if len(v) > 0 {
			return storageFaultReport(seed, v), nil
		}
	}

	// --- GraphML: a labelled/propertied property-graph round-trip (distinct from
	// the edge-list formats), clean then under an ENOSPC export fault. ---
	if v, herr := graphmlRoundTripClean(seed); herr != nil {
		return nil, fmt.Errorf("sim: ST8 graphml clean round-trip: %w", herr)
	} else if len(v) > 0 {
		return storageFaultReport(seed, v), nil
	}
	if v, herr := graphmlExportFaultFailsClean(seed); herr != nil {
		return nil, fmt.Errorf("sim: ST8 graphml export-fault: %w", herr)
	} else if len(v) > 0 {
		return storageFaultReport(seed, v), nil
	}
	return nil, nil
}

// graphmlModel builds a small deterministic labelled/propertied directed graph
// for the GraphML round-trip: nodes g0..g3 each carry the label GNode and an
// integer property w, joined by a fixed edge set.
func graphmlModel() (*lpg.Graph[string, int64], []string) {
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	keys := []string{"g0", "g1", "g2", "g3"}
	for i, k := range keys {
		_ = g.AddNode(k)
		_ = g.SetNodeLabel(k, "GNode")
		_ = g.SetNodeProperty(k, "w", lpg.Int64Value(int64(i*10)))
	}
	_ = g.AddEdge("g0", "g1", 1)
	_ = g.AddEdge("g1", "g2", 1)
	_ = g.AddEdge("g0", "g2", 1)
	return g, keys
}

// graphmlEqualsModel reports whether got round-tripped the model faithfully:
// same order/size and, for every model key, the GNode label and the w property.
func graphmlEqualsModel(got, want *lpg.Graph[string, int64], keys []string) bool {
	if got == nil {
		return false
	}
	if got.AdjList().Order() != want.AdjList().Order() || got.AdjList().Size() != want.AdjList().Size() {
		return false
	}
	for _, k := range keys {
		if !slices.Contains(got.NodeLabels(k), "GNode") {
			return false
		}
		gp, wp := got.NodeProperties(k), want.NodeProperties(k)
		if gv, wv := gp["w"], wp["w"]; gv != wv {
			return false
		}
	}
	return true
}

// graphmlRoundTripClean exports the property-graph model to a fresh SimDisk file
// via graphml.WriteWithProps, re-imports it via graphml.ReadWithProps, and checks
// the imported graph reproduces the model (order/size/labels/properties). It
// returns the exported byte size via the closure so the fault phase can size a
// sub-full capacity.
func graphmlRoundTripClean(seed uint64) ([]Violation, error) {
	model, keys := graphmlModel()
	const path = "io/graph.graphml"
	disk := NewSimDisk(NewSeed(seed), 0)
	wh, err := disk.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return nil, fmt.Errorf("open for export: %w", err)
	}
	if werr := graphml.WriteWithProps(wh, model); werr != nil {
		_ = wh.Close()
		return nil, fmt.Errorf("clean export: %w", werr)
	}
	if err := wh.Close(); err != nil {
		return nil, fmt.Errorf("close after export: %w", err)
	}
	rh, err := disk.OpenFile(path, os.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("open for import: %w", err)
	}
	got, _, ierr := graphml.ReadWithProps(rh)
	_ = rh.Close()
	if ierr != nil {
		return nil, fmt.Errorf("clean import: %w", ierr)
	}
	if !graphmlEqualsModel(got, model, keys) {
		return []Violation{{
			Kind: ViolationOracleDeviation, Op: "<io-graphml-clean>",
			Message: "GraphML clean round-trip did not reproduce the model (order/size/label/property mismatch)",
		}}, nil
	}
	return nil, nil
}

// graphmlExportFaultFailsClean exports the model under a sub-full ENOSPC bound
// and asserts the export fails with a typed ENOSPC error and that a re-import of
// whatever landed does NOT silently reconstruct the full model.
func graphmlExportFaultFailsClean(seed uint64) ([]Violation, error) {
	model, keys := graphmlModel()
	const path = "io/graph.graphml"

	// Size the full export once to pick a sub-full capacity.
	var full bytes.Buffer
	if err := graphml.WriteWithProps(&full, model); err != nil {
		return nil, fmt.Errorf("size export: %w", err)
	}
	if full.Len() < 2 {
		return nil, fmt.Errorf("graphml export size %d too small to force a fault", full.Len())
	}

	disk := NewSimDisk(NewSeed(seed), 0)
	disk.SetCapacity(int64(full.Len()/2), false)
	wh, err := disk.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return nil, fmt.Errorf("open for faulted export: %w", err)
	}
	werr := graphml.WriteWithProps(wh, model)
	_ = wh.Close()

	var v []Violation
	if werr == nil {
		return []Violation{{
			Kind: ViolationOracleDeviation, Op: "<io-graphml-nonvacuity>",
			Message: "GraphML export under a sub-full ENOSPC bound unexpectedly succeeded — the fault did not bite",
		}}, nil
	}
	if !errors.Is(werr, syscall.ENOSPC) {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: "<io-graphml-fault>",
			Message: "faulted GraphML export failed with " + werr.Error() + ", want a typed ENOSPC error",
		})
	}
	rh, err := disk.OpenFile(path, os.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("open for faulted re-import: %w", err)
	}
	got, _, ierr := graphml.ReadWithProps(rh)
	_ = rh.Close()
	if ierr == nil && graphmlEqualsModel(got, model, keys) {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: "<io-graphml-fault>",
			Message: "a partial GraphML export re-imported as the full model — a torn artefact was silently accepted",
		})
	}
	return v, nil
}

// ioRoundTripClean exports the model to a fresh SimDisk file, re-imports it, and
// checks the imported edge multiset equals wantTriples. It returns the exported
// byte size (so the fault phase can size a sub-full capacity), any invariant
// violations, and a harness error.
func ioRoundTripClean(f ioFormat, model *adjlist.AdjList[string, int64], wantTriples map[string]int) (int, []Violation, error) {
	disk := NewSimDisk(NewSeed(1), 0)
	wh, err := disk.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return 0, nil, fmt.Errorf("open for export: %w", err)
	}
	if _, err := f.export(wh, model); err != nil {
		_ = wh.Close()
		return 0, nil, fmt.Errorf("clean export: %w", err)
	}
	if err := wh.Close(); err != nil {
		return 0, nil, fmt.Errorf("close after export: %w", err)
	}
	full, err := disk.ReadFile(f.path)
	if err != nil {
		return 0, nil, fmt.Errorf("read exported file: %w", err)
	}

	rh, err := disk.OpenFile(f.path, os.O_RDONLY)
	if err != nil {
		return 0, nil, fmt.Errorf("open for import: %w", err)
	}
	got, ierr := f.imp(rh)
	_ = rh.Close()
	if ierr != nil {
		return 0, nil, fmt.Errorf("clean import: %w", ierr)
	}
	if got == nil {
		return 0, []Violation{{
			Kind: ViolationACIDConsistency, Op: "<io-" + f.name + "-clean>",
			Message: "a clean import returned a nil graph",
		}}, nil
	}
	if !triplesEqual(wantTriples, edgeTriples(got)) {
		return 0, []Violation{{
			Kind: ViolationACIDConsistency, Op: "<io-" + f.name + "-clean>",
			Message: "a clean " + f.name + " round-trip did not reproduce the modelled edge set exactly",
		}}, nil
	}
	return len(full), nil, nil
}

// ioExportFaultFailsClean caps the disk below the export's full size so the
// export fails partway with ENOSPC, then re-imports whatever landed and checks
// no partial artefact is silently accepted as the model. It returns any
// invariant violations and a harness error.
func ioExportFaultFailsClean(f ioFormat, model *adjlist.AdjList[string, int64], wantTriples map[string]int, seed uint64, fullSize int) ([]Violation, error) {
	if fullSize < 2 {
		return nil, fmt.Errorf("exported %s size %d too small to force a mid-export fault", f.name, fullSize)
	}
	disk := NewSimDisk(NewSeed(seed), 0)
	// Cap below the full size so the final flush breaches the budget with ENOSPC.
	disk.SetCapacity(int64(fullSize/2), false)

	wh, err := disk.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return nil, fmt.Errorf("open for faulted export: %w", err)
	}
	_, werr := f.export(wh, model)
	_ = wh.Close()

	var v []Violation
	if werr == nil {
		return []Violation{{
			Kind: ViolationOracleDeviation, Op: "<io-" + f.name + "-nonvacuity>",
			Message: "export under a sub-full ENOSPC bound unexpectedly succeeded — the fault did not bite",
		}}, nil
	}
	if !errors.Is(werr, syscall.ENOSPC) {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: "<io-" + f.name + "-fault>",
			Message: "faulted export failed with " + werr.Error() + ", want a typed ENOSPC error",
		})
	}

	// Re-import whatever landed. It must NOT silently reconstruct the full model:
	// either the reader rejects the truncated file, or it yields a graph that
	// differs from the model.
	rh, err := disk.OpenFile(f.path, os.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("open for faulted re-import: %w", err)
	}
	got, ierr := f.imp(rh)
	_ = rh.Close()
	if ierr == nil && got != nil && triplesEqual(wantTriples, edgeTriples(got)) {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: "<io-" + f.name + "-fault>",
			Message: "a partial " + f.name + " export re-imported as the full model — a torn artefact was silently accepted",
		})
	}
	return v, nil
}

// buildDeterministicAdjList builds a seed-derived directed simple graph with
// int64 weights and no isolated nodes, so the CSV edge-list round-trip is a
// faithful reproduction test.
func buildDeterministicAdjList(s *Seed) (*adjlist.AdjList[string, int64], error) {
	a := adjlist.New[string, int64](adjlist.Config{Directed: true, Multigraph: false})
	order := 40 + s.IntN(24) // 40..63 nodes
	for src := 0; src < order; src++ {
		deg := 1 + s.IntN(4) // 1..4 out-edges (no isolated nodes)
		for j := 0; j < deg; j++ {
			dst := int(s.Uint64N(uint64(order)))
			w := int64(s.Uint64N(1000))
			if err := a.AddEdge(nodeKeyIO(src), nodeKeyIO(dst), w); err != nil {
				return nil, fmt.Errorf("AddEdge %d->%d: %w", src, dst, err)
			}
		}
	}
	return a, nil
}

// nodeKeyIO names an ST8 model node.
func nodeKeyIO(i int) string { return "io" + strconv.Itoa(i) }

// edgeTriples builds the name-keyed edge multiset of a, so two graphs can be
// compared for edge-set identity independently of internal NodeID assignment.
// The key is src\x00dst\x00weight; the value is the multiplicity (always 1 for a
// simple graph, but a multiset keeps the comparison correct for any config).
func edgeTriples(a *adjlist.AdjList[string, int64]) map[string]int {
	names := make(map[graph.NodeID]string)
	a.Mapper().Walk(func(id graph.NodeID, name string) bool {
		names[id] = name
		return true
	})
	triples := make(map[string]int)
	maxID := uint64(a.MaxNodeID())
	for id := uint64(0); id < maxID; id++ {
		src, ok := names[graph.NodeID(id)]
		if !ok {
			continue
		}
		nb, ws := a.LoadEntry(graph.NodeID(id))
		for i, n := range nb {
			dst, ok := names[n]
			if !ok {
				continue
			}
			var w int64
			if ws != nil {
				w = ws[i]
			}
			triples[src+"\x00"+dst+"\x00"+strconv.FormatInt(w, 10)]++
		}
	}
	return triples
}

// triplesEqual reports whether two edge multisets are identical.
func triplesEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, na := range a {
		if b[k] != na {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// Shared helpers
// -----------------------------------------------------------------------------

// commitCreatePerson commits one Person node through the engine's autocommit
// write path and drains the result, returning any error. Each call is one
// durable transaction.
func commitCreatePerson(ctx context.Context, eng *cypher.Engine, name string, age int) error {
	q := fmt.Sprintf("CREATE (:Person {name:'%s', age:%d})", name, age)
	res, err := eng.RunInTx(ctx, q, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	drainErr := res.Err()
	_ = res.Close()
	return drainErr
}

// storageFaultReport renders a set of violations as a SimReport for a mechanical
// storage-fault scenario.
func storageFaultReport(seed uint64, v []Violation) *SimReport {
	return &SimReport{
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<storage fault>"},
		Violations: v,
	}
}
