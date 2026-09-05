package sim

// wal_writer_surface.go — the WAL writer's public surface under simulation
// (rmp #2472): the durability watermark, per-transaction frame contiguity under
// concurrency, the whole-file truncate, the poisoned-writer contract, and the
// two process-level guards that no in-memory disk can express.
//
// # What was unreachable, and why it mattered
//
// [wal.Writer] is the component that owns the durable file, but most of what it
// EXPORTS was invisible to the simulator. [wal.Writer.Stats],
// [wal.Writer.DurableOffset], [wal.Writer.Poisoned] and [wal.Writer.SyncBuffered]
// were never read by any scenario, and the whole-file [wal.Writer.Truncate] — as
// distinct from [wal.Writer.TruncatePrefix], which the checkpoint path drives —
// was never called. Each of them states a contract that the rest of the store
// relies on: the checkpointer picks its WAL cut point from DurableOffset and
// aborts on Poisoned, and the txn layer's empty commit resolves through
// SyncBuffered. A silent change in any of them would be adjudicated nowhere.
//
// # The contiguity claim, and why it needed a concurrent witness
//
// [wal.Writer.AppendRun] holds w.mu across a whole transaction's frames so they
// land as one contiguous run. That matters because crash recovery commits the
// ops carrying a marker's own TxnSeq and discards the buffered prefix as
// orphaned, a reading that is correct ONLY IF a transaction's frames are
// contiguous. Before commit 9eee3b18 the contiguity came from the store's
// single-writer semaphore two layers up; AppendRun moved it into the component
// that owns the file, and its doc now claims contiguity BY CONSTRUCTION.
//
// A claim of that shape is only tested by concurrency. [RunWALContiguity] drives
// eight concurrent committers through the real writer, parses the durable image,
// and asserts every transaction occupies exactly one contiguous run — and its
// control arm drives the identical workload through per-frame [wal.Writer.Append]
// instead, which interleaves for real. The control is what makes the assertion
// falsifiable rather than decorative: it demonstrates that the property comes
// from AppendRun and from nothing else in the stack.
//
// # Why the two process-level guards run against a REAL directory
//
// [wal.ErrWALLocked] is produced by flock(2) on a LOCK sentinel, and the symlink
// refusal by O_NOFOLLOW on the final path component. [SimDisk] is a flat
// in-memory key table: it has no links, no inodes and no advisory locks, so
// neither guard can fire against it. The two honest routes were to grow SimDisk
// a lock-and-symlink model, or to drive these two opens against a real temporary
// directory. This file takes the second, for the reason the mandate gives: a
// modelled flock would prove the model rather than the guard, whereas a real
// second [wal.Open] exercises the actual syscall the guard is made of. The
// precedent is established — several arms in this package already use
// os.MkdirTemp where the SimDisk seam does not exist (rmp #2466).
//
// What remains unreachable through SimDisk is stated so no reader infers
// otherwise: [RunWALRealFSGuards] is the ONLY arm here that leaves the
// simulated disk, and the guards it covers stay outside every seeded,
// crash-injecting scenario in this package.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// -----------------------------------------------------------------------------
// Part 1 — the durability-watermark oracle
// -----------------------------------------------------------------------------

// walWatermarkSample is one observation of the writer's durability surface,
// taken immediately after a commit was acknowledged.
type walWatermarkSample struct {
	// durable is [wal.Writer.DurableOffset]: the byte length covered by the last
	// successful fsync.
	durable int64
	// appended is [wal.Stats.Bytes]: every frame byte the writer has ACCEPTED,
	// which is not the same quantity — a poison discards an un-synced suffix
	// whose bytes stay counted here, so appended >= durable always and equality
	// holds only while nothing has been discarded or truncated.
	appended uint64
	// frames and syncs are the remaining lifetime counters.
	frames, syncs uint64
	// imageLen is the byte length of the WAL file on the simulated disk. It is
	// read independently of every counter above, so it cross-checks the writer's
	// own bookkeeping against the bytes that actually exist.
	imageLen int64
	// boundary is the accumulation SUM(HeaderSize + len(payload)) over the whole
	// complete-frame prefix of the durable image. It is what "the expected
	// accumulation for the frames actually acked" means when the payload sizes
	// are not the harness's to choose; see [WALWatermarkEvidence].
	boundary int64
	// boundaryOK reports that the image parsed to a clean tail, so boundary
	// describes the whole image rather than the prefix before a torn frame.
	boundaryOK bool
	// poisoned is [wal.Writer.Poisoned] at the moment of the sample.
	poisoned error
	// wantDurable is the offset the committer was HANDED by
	// [wal.Writer.AppendRun] for this very commit, and -1 when the arm cannot
	// know it (the engine mints its own runs). It is the exact expectation.
	wantDurable int64
	// wantAppended and wantFrames are the exact byte / frame accumulations the
	// arm computed from the payloads it emitted, and 0 with exact=false when the
	// arm did not choose the payloads.
	wantAppended, wantFrames uint64
	// exact reports that wantAppended / wantFrames / wantDurable carry a real
	// expectation rather than a placeholder.
	exact bool
}

// WALWatermarkEvidence is what a watermark run OBSERVED across its commits. It
// holds measurements and no verdict.
//
// # What "expected" means, precisely
//
// Two different statements are made, and they are kept apart because only one of
// them is available to every caller.
//
//   - EXACT, when the arm chose the payloads itself: the durable offset after a
//     commit must equal the watermark [wal.Writer.AppendRun] returned for that
//     commit, and [wal.Stats] must equal SUM(wal.HeaderSize + len(payload)) over
//     every frame emitted so far. Nothing is inferred; both sides are derived
//     from bytes the harness handed the writer.
//
//   - RELATIVE, when the payloads belong to the engine: the durable offset must
//     land on a FRAME BOUNDARY of the durable image — that is, it must equal the
//     accumulation SUM(HeaderSize + len(payload)) over some whole number of
//     leading complete frames — and must never exceed the bytes the writer has
//     accepted. This is the invariant [wal.Writer.DurableOffset] documents ("the
//     value always lands on a frame boundary"), and it is derived from the frames
//     that are actually on disk.
//
// The second form deliberately asserts NO absolute size. rmp #2521 measured that
// the durable image varies with process wall-clock time, because a commit marker
// encodes the instant at which it was written; an oracle pinning a byte count
// would be pinning the clock. Monotonicity and the frame-boundary relation are
// invariant under that variation.
type WALWatermarkEvidence struct {
	// Label names the arm in a failure message.
	Label string
	// Commits is how many acknowledged commits were observed.
	Commits int
	// Samples is one observation per commit, in commit order.
	Samples []walWatermarkSample
	// Exact reports whether the arm supplied exact expectations.
	Exact bool
}

// FinalDurable is the durable offset the last sample observed, or 0 for an empty
// run.
func (e WALWatermarkEvidence) FinalDurable() int64 {
	if len(e.Samples) == 0 {
		return 0
	}
	return e.Samples[len(e.Samples)-1].durable
}

// String renders the evidence for a failure message or a test log.
func (e WALWatermarkEvidence) String() string {
	if len(e.Samples) == 0 {
		return fmt.Sprintf("wal-watermark[%s]: commits=0 (nothing observed)", e.Label)
	}
	last := e.Samples[len(e.Samples)-1]
	return fmt.Sprintf("wal-watermark[%s]: commits=%d exact=%t final{durable=%d appended=%d frames=%d syncs=%d imageLen=%d boundary=%d boundaryOK=%t poisoned=%v}",
		e.Label, e.Commits, e.Exact, last.durable, last.appended, last.frames, last.syncs, last.imageLen, last.boundary, last.boundaryOK, last.poisoned)
}

// walWatermarkMonitor accumulates the per-commit observations of one writer. It
// is the ALWAYS-ON half of this task: a caller attaches it to a writer and calls
// [walWatermarkMonitor.observe] after each acknowledged commit, which costs one
// mutex-guarded read of each counter plus one parse of the durable image.
//
// It is not safe for concurrent use; every arm here observes from the goroutine
// that committed.
type walWatermarkMonitor struct {
	disk     *SimDisk
	w        *wal.Writer
	path     string
	ev       WALWatermarkEvidence
	expBytes uint64
	expFrms  uint64
}

// newWALWatermarkMonitor attaches a monitor to w, whose durable image lives at
// path on disk. exact declares whether the caller will supply per-commit
// expectations; see [WALWatermarkEvidence] for what the two modes assert.
func newWALWatermarkMonitor(disk *SimDisk, w *wal.Writer, path, label string, exact bool) *walWatermarkMonitor {
	return &walWatermarkMonitor{
		disk: disk, w: w, path: path,
		ev: WALWatermarkEvidence{Label: label, Exact: exact},
	}
}

// account records the bytes of one frame the caller is about to emit, so the
// monitor can hold the exact accumulation. Only the exact arms call it.
func (m *walWatermarkMonitor) account(payload []byte) {
	m.expBytes += uint64(wal.HeaderSize + len(payload))
	m.expFrms++
}

// observe takes one post-commit sample. wantDurable is the watermark
// [wal.Writer.AppendRun] returned for the commit just acknowledged, or -1 when
// the caller cannot know it.
func (m *walWatermarkMonitor) observe(wantDurable int64) error {
	img, err := m.disk.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("sim: wal-watermark %q: read durable image: %w", m.ev.Label, err)
	}
	_, plens, tailErr := walFrameLayout(img)
	var boundary int64
	for _, n := range plens {
		boundary += int64(wal.HeaderSize + n)
	}
	st := m.w.Stats()
	m.ev.Samples = append(m.ev.Samples, walWatermarkSample{
		durable:      m.w.DurableOffset(),
		appended:     st.Bytes,
		frames:       st.Frames,
		syncs:        st.Syncs,
		imageLen:     int64(len(img)),
		boundary:     boundary,
		boundaryOK:   tailErr == nil,
		poisoned:     m.w.Poisoned(),
		wantDurable:  wantDurable,
		wantAppended: m.expBytes,
		wantFrames:   m.expFrms,
		exact:        m.ev.Exact,
	})
	m.ev.Commits++
	return nil
}

// walWatermarkMinCommits is the floor on observed commits below which no
// statement about monotonicity means anything: a single sample cannot show a
// sequence never decreased.
const walWatermarkMinCommits = 4

// checkWALWatermarkNonVacuity is the SEPARATE coverage precondition. It asserts
// the run produced a population a monotonicity claim can be made over, BEFORE
// [checkWALWatermark] speaks about the writer. A violation here means the run
// proved nothing, not that the writer is broken (rmp #2470).
//
// The window guard is the one rmp #2471 established: `last - first` is zero by
// construction when there is only one reading, so a "did it advance?" clause
// fires on a record that never observed two points.
func checkWALWatermarkNonVacuity(e WALWatermarkEvidence) []Violation {
	var v []Violation
	add := func(msg string) {
		v = append(v, Violation{Kind: ViolationVacuousRun, Op: "<wal-watermark non-vacuity>", Message: msg + " — " + e.String()})
	}
	if e.Commits < walWatermarkMinCommits {
		add(fmt.Sprintf("only %d commit(s) observed, need >= %d: a monotonicity claim needs a sequence, not a point",
			e.Commits, walWatermarkMinCommits))
		return v
	}
	if e.FinalDurable() <= 0 {
		add("the durable offset never left zero: no commit reached the platter, so every clause below would pass by never being asked")
	}
	last := e.Samples[len(e.Samples)-1]
	if last.frames == 0 {
		add("no frame was ever appended: the run did not exercise the durable write path")
	}
	return v
}

// checkWALWatermark adjudicates the durability watermark over one run's samples.
// It is a PURE function of the observed evidence, which is what lets a test
// falsify it with a doctored record instead of hoping a real run misbehaves.
//
// The clauses, in the order a failure is most usefully read:
//
//   - the writer was never poisoned (a poisoned sample rewinds the accepted
//     offset, so every relation below is being read off a rewound writer);
//   - every counter is MONOTONIC across the run;
//   - the durable offset never exceeds the bytes the writer accepted;
//   - the durable offset lands on a FRAME BOUNDARY of the durable image;
//   - and, when the arm chose the payloads, the exact accumulation matches.
func checkWALWatermark(e WALWatermarkEvidence) []Violation {
	var v []Violation
	add := func(kind ViolationKind, i int, msg string) {
		v = append(v, Violation{
			Kind: kind, Op: "<wal-watermark>",
			Message: fmt.Sprintf("commit %d: %s — %s", i, msg, e.String()),
		})
	}
	var prev walWatermarkSample
	for i, s := range e.Samples {
		if s.poisoned != nil {
			add(ViolationACIDDurability, i,
				fmt.Sprintf("the writer is poisoned (%v) although the commit was acknowledged", s.poisoned))
		}
		if i > 0 {
			if s.durable < prev.durable {
				add(ViolationACIDDurability, i, fmt.Sprintf(
					"the durable offset WENT BACKWARDS, %d -> %d: bytes an earlier commit was told were durable are no longer covered",
					prev.durable, s.durable))
			}
			if s.appended < prev.appended || s.frames < prev.frames || s.syncs < prev.syncs {
				add(ViolationOracleDeviation, i, fmt.Sprintf(
					"a lifetime counter went backwards: bytes %d -> %d, frames %d -> %d, syncs %d -> %d; wal.Stats documents them as monotonic",
					prev.appended, s.appended, prev.frames, s.frames, prev.syncs, s.syncs))
			}
			if s.durable == prev.durable {
				add(ViolationACIDDurability, i, fmt.Sprintf(
					"the durable offset did not advance past %d although a commit was acknowledged: the commit's own frames are not covered by any fsync",
					prev.durable))
			}
		}
		if s.durable > int64(s.appended) {
			add(ViolationOracleDeviation, i, fmt.Sprintf(
				"durable offset %d exceeds the %d byte(s) the writer accepted: the watermark covers frames that were never appended",
				s.durable, s.appended))
		}
		if !s.boundaryOK {
			add(ViolationACIDDurability, i,
				"the durable image does not parse to a clean tail: an acknowledged commit left torn or corrupt bytes behind")
		} else if s.durable != s.boundary {
			add(ViolationACIDDurability, i, fmt.Sprintf(
				"durable offset %d is not the frame-boundary accumulation %d of the durable image (%d image bytes): "+
					"the offset wal.Writer.DurableOffset documents as always landing on a frame boundary does not",
				s.durable, s.boundary, s.imageLen))
		}
		if s.exact {
			if s.wantDurable >= 0 && s.durable != s.wantDurable {
				add(ViolationACIDDurability, i, fmt.Sprintf(
					"durable offset %d != the watermark %d that AppendRun returned for this commit: the committer was acknowledged against somebody else's offset",
					s.durable, s.wantDurable))
			}
			if s.appended != s.wantAppended || s.frames != s.wantFrames {
				add(ViolationOracleDeviation, i, fmt.Sprintf(
					"wal.Stats reports %d byte(s) in %d frame(s); the payloads emitted account for exactly %d byte(s) in %d frame(s)",
					s.appended, s.frames, s.wantAppended, s.wantFrames))
			}
		}
		prev = s
	}
	return v
}

// walWatermarkPath is the WAL the direct watermark arm writes on the SimDisk.
const walWatermarkPath = "wal_watermark.wal"

// walWatermarkCommits / walWatermarkMaxFramesPerTx size the direct arm. The
// frame count per transaction VARIES across the run so a constant-stride
// accumulation cannot pass by coincidence.
const (
	walWatermarkCommits        = 12
	walWatermarkMaxFramesPerTx = 3
)

// RunWALWatermarkDirect drives a real [wal.Writer] over a [SimDisk] through a
// sequence of commits whose payloads the harness chooses, so every expectation
// is EXACT: the durable offset must equal the watermark AppendRun returned, and
// the lifetime counters must equal the accumulation over the emitted frames.
//
// It also exercises [wal.Writer.SyncBuffered] on the final commit — the flush
// path the txn layer takes for a commit that appended nothing of its own — so
// that method is driven under the same watermark assertions as SyncGroup.
func RunWALWatermarkDirect(ctx context.Context, seed uint64) (WALWatermarkEvidence, error) {
	disk := NewSimDisk(NewSeed(seed^durableDiskSeedMix), 0) // faultRate 0: this arm injects nothing
	w, err := wal.OpenFS(simWALFS{disk: disk}, walWatermarkPath)
	if err != nil {
		return WALWatermarkEvidence{}, fmt.Errorf("sim: wal-watermark open: %w", err)
	}
	defer func() { _ = w.Close() }()

	m := newWALWatermarkMonitor(disk, w, walWatermarkPath, "direct", true)
	for i := range walWatermarkCommits {
		if err := ctx.Err(); err != nil {
			return m.ev, err
		}
		nFrames := i%walWatermarkMaxFramesPerTx + 1
		mark, aerr := w.AppendRun(func(emit func([]byte) error) error {
			for k := range nFrames {
				// The payload length varies with both indices, so the accumulation
				// is not a multiple of any single frame size.
				p := walSurfaceFrame(byte(i), uint16(i), 12+2*k)
				if e := emit(p); e != nil {
					return e
				}
				m.account(p)
			}
			return nil
		})
		if aerr != nil {
			return m.ev, fmt.Errorf("sim: wal-watermark append %d: %w", i, aerr)
		}
		// The last commit resolves through SyncBuffered rather than SyncGroup.
		// Both must leave the same watermark: this arm is serial, so everything
		// accepted IS this committer's own — the condition SyncBuffered's doc
		// requires before a caller may read it as its own acknowledgement.
		if i == walWatermarkCommits-1 {
			if serr := w.SyncBuffered(); serr != nil {
				return m.ev, fmt.Errorf("sim: wal-watermark SyncBuffered %d: %w", i, serr)
			}
		} else if serr := w.SyncGroup(mark); serr != nil {
			return m.ev, fmt.Errorf("sim: wal-watermark sync %d: %w", i, serr)
		}
		if oerr := m.observe(mark); oerr != nil {
			return m.ev, oerr
		}
	}
	return m.ev, nil
}

// walWatermarkEngineCommits is how many engine transactions the engine-driven
// arm commits. It is modest so the short layer stays under budget.
const walWatermarkEngineCommits = 8

// tmplCreateWALBeacon is the engine-driven arm's write. Its frames are the
// engine's, not the harness's, which is the whole point of the arm.
const tmplCreateWALBeacon = "CREATE (n:WALBeacon {name:$name})"

// RunWALWatermarkEngine drives the watermark oracle against the REAL stack: a
// WAL-backed [SimStore] whose frames the engine composes, observed after every
// acknowledged commit.
//
// It is the arm that proves the oracle is size-agnostic. The engine's commit
// markers encode the instant they were written, so the durable image is not
// byte-stable across runs (rmp #2521); the relative clauses — monotonicity, the
// accepted-bytes ceiling, and the frame-boundary relation — hold regardless,
// and are exactly what a watermark defect would break.
func RunWALWatermarkEngine(ctx context.Context, seed uint64) (WALWatermarkEvidence, error) {
	disk := NewSimDisk(NewSeed(seed^durableDiskSeedMix), 0) // faultRate 0: this arm injects nothing
	cfg := durableStoreConfig()
	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return WALWatermarkEvidence{}, fmt.Errorf("sim: wal-watermark engine open: %w", err)
	}
	defer func() { _ = st.Close() }()

	m := newWALWatermarkMonitor(disk, st.WAL(), walPathFor(cfg.dir), "engine", false)
	adapter := NewEngineAdapter(st.Engine())
	for i := range walWatermarkEngineCommits {
		if err := ctx.Err(); err != nil {
			return m.ev, err
		}
		res, rerr := adapter.RunWrite(ctx, tmplCreateWALBeacon, map[string]any{
			"name": fmt.Sprintf("wal-beacon-%d", i),
		})
		if rerr != nil {
			return m.ev, fmt.Errorf("sim: wal-watermark engine commit %d: %w", i, rerr)
		}
		if cerr := res.Close(); cerr != nil {
			return m.ev, fmt.Errorf("sim: wal-watermark engine commit %d close: %w", i, cerr)
		}
		// -1: the engine minted the run, so the harness cannot know the watermark
		// it was handed. Only the relative clauses apply.
		if oerr := m.observe(-1); oerr != nil {
			return m.ev, oerr
		}
	}
	return m.ev, nil
}

// -----------------------------------------------------------------------------
// Part 2 — per-transaction frame contiguity under concurrency
// -----------------------------------------------------------------------------

// walSurfaceFrame builds one tagged frame payload: the committer id, the
// transaction id (big-endian uint16) and filler to the requested length. The
// tagging is what lets the durable image be partitioned back into transactions
// without trusting any counter.
func walSurfaceFrame(committer byte, txID uint16, size int) []byte {
	if size < 3 {
		size = 3
	}
	p := make([]byte, size)
	p[0] = committer
	binary.BigEndian.PutUint16(p[1:3], txID)
	for i := 3; i < size; i++ {
		p[i] = byte(txID)
	}
	return p
}

// WALContiguityConfig parameterises a contiguity run.
type WALContiguityConfig struct {
	// Seed is the disk sub-stream seed.
	Seed uint64
	// Committers is how many goroutines commit concurrently. The non-vacuity
	// gate requires at least [walContiguityMinCommitters].
	Committers int
	// TxPerCommitter is how many transactions each committer issues.
	TxPerCommitter int
	// FramesPerTx is how many frames each transaction emits. A value below 2
	// makes contiguity vacuous — a one-frame transaction is contiguous by
	// definition — and the non-vacuity gate rejects it.
	FramesPerTx int
	// PerFrameAppend selects the CONTROL arm: each frame is appended with
	// [wal.Writer.Append] instead of the whole transaction with
	// [wal.Writer.AppendRun]. It is the sensitivity seam — same writer, same
	// workload, no run-level lock — and it is expected to interleave.
	PerFrameAppend bool
}

// walContiguity* are the contiguity arm's defaults and floors.
const (
	// walContiguityMinCommitters is the concurrency floor the task fixes.
	walContiguityMinCommitters = 8
	walContiguityDefaultTx     = 12
	walContiguityDefaultFrames = 4
)

// WALContiguityEvidence is what a contiguity run OBSERVED in the durable image.
type WALContiguityEvidence struct {
	// Committers is the concurrency the run drove; FramesPerTx the run's shape.
	Committers, FramesPerTx int
	// PerFrameAppend records which append path produced this image.
	PerFrameAppend bool
	// Alternating records that the image was produced by the DETERMINISTIC
	// handoff protocol ([RunWALContiguityAlternating]) rather than by racing
	// committers, so its layout is exact rather than a scheduling outcome.
	Alternating bool
	// Transactions is how many distinct transaction ids the image carries, and
	// Frames the total frame count.
	Transactions, Frames int
	// Runs is the number of MAXIMAL contiguous same-transaction blocks. It equals
	// Transactions exactly when every transaction is contiguous, and exceeds it
	// by one for each extra fragment.
	Runs int
	// SplitTransactions is how many transactions occupy more than one run — the
	// count that must be ZERO for AppendRun's claim to hold.
	SplitTransactions int
	// WorstFragments is the largest number of fragments any single transaction
	// was broken into (1 when every transaction is contiguous).
	WorstFragments int
	// ShortTransactions is how many transactions carry a frame count other than
	// FramesPerTx: a lost or duplicated frame, which would make the contiguity
	// census read a different population than the one committed.
	ShortTransactions int
	// CommitterSwitches counts adjacent run boundaries at which the committing
	// goroutine changed. It is the witness that the run genuinely interleaved:
	// zero means the committers happened to serialise and the image proves
	// nothing about concurrency.
	CommitterSwitches int
	// TailErr is the durable image's stop condition; non-nil means the census
	// read a truncated image.
	TailErr error
	// ImageLen is the durable image's byte length, reported for evidence only —
	// never asserted against a constant.
	ImageLen int64
}

// String renders the evidence for a failure message or a test log.
func (e WALContiguityEvidence) String() string {
	path := "AppendRun"
	if e.PerFrameAppend {
		path = "Append(per-frame)"
	}
	shape := "concurrent"
	if e.Alternating {
		shape = "alternating(deterministic)"
	}
	return fmt.Sprintf("wal-contiguity[%s/%s]: committers=%d txs=%d framesPerTx=%d frames=%d runs=%d split=%d worstFragments=%d short=%d committerSwitches=%d imageLen=%d tail=%v",
		path, shape, e.Committers, e.Transactions, e.FramesPerTx, e.Frames, e.Runs, e.SplitTransactions,
		e.WorstFragments, e.ShortTransactions, e.CommitterSwitches, e.ImageLen, e.TailErr)
}

// walContiguityPath is the WAL a contiguity run writes on the SimDisk. The arm
// suffix keeps the two arms' images apart on one disk.
func walContiguityPath(perFrame bool) string {
	if perFrame {
		return "wal_contiguity_perframe.wal"
	}
	return "wal_contiguity_run.wal"
}

// RunWALContiguity drives cfg.Committers concurrent committers through a real
// [wal.Writer] over a [SimDisk] and reports how the durable image is laid out.
//
// The transactions are tagged in their payloads, so the census partitions the
// image by what is actually ON DISK rather than by what any counter claims. No
// fault is injected: the question is purely one of ordering.
func RunWALContiguity(ctx context.Context, cfg WALContiguityConfig) (WALContiguityEvidence, error) {
	if cfg.Committers < 1 {
		cfg.Committers = walContiguityMinCommitters
	}
	if cfg.TxPerCommitter < 1 {
		cfg.TxPerCommitter = walContiguityDefaultTx
	}
	if cfg.FramesPerTx < 1 {
		cfg.FramesPerTx = walContiguityDefaultFrames
	}
	path := walContiguityPath(cfg.PerFrameAppend)
	disk := NewSimDisk(NewSeed(cfg.Seed^durableDiskSeedMix), 0) // faultRate 0
	w, err := wal.OpenFS(simWALFS{disk: disk}, path)
	if err != nil {
		return WALContiguityEvidence{}, fmt.Errorf("sim: wal-contiguity open: %w", err)
	}

	// start releases every committer at once, so the goroutines contend rather
	// than trickle: a staggered start would let each finish before the next
	// arrives and the image would serialise for a reason that has nothing to do
	// with the writer.
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, cfg.Committers)
	for c := range cfg.Committers {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			<-start
			for tx := range cfg.TxPerCommitter {
				txID := uint16(c*cfg.TxPerCommitter + tx)
				if cfg.PerFrameAppend {
					errs[c] = walContiguityPerFrameTx(w, byte(c), txID, cfg.FramesPerTx)
				} else {
					errs[c] = walContiguityRunTx(w, byte(c), txID, cfg.FramesPerTx)
				}
				if errs[c] != nil {
					return
				}
			}
		}(c)
	}
	close(start)
	wg.Wait()
	if cerr := w.Close(); cerr != nil {
		return WALContiguityEvidence{}, fmt.Errorf("sim: wal-contiguity close: %w", cerr)
	}
	for c, cerr := range errs {
		if cerr != nil {
			return WALContiguityEvidence{}, fmt.Errorf("sim: wal-contiguity committer %d: %w", c, cerr)
		}
	}
	if err := ctx.Err(); err != nil {
		return WALContiguityEvidence{}, err
	}
	return walContiguityCensus(disk, path, cfg)
}

// walContiguityRunTx commits one transaction as a single [wal.Writer.AppendRun],
// which is the path whose contiguity claim is under test.
func walContiguityRunTx(w *wal.Writer, committer byte, txID uint16, frames int) error {
	mark, aerr := w.AppendRun(func(emit func([]byte) error) error {
		for range frames {
			if e := emit(walSurfaceFrame(committer, txID, 16)); e != nil {
				return e
			}
		}
		return nil
	})
	if aerr != nil {
		return fmt.Errorf("AppendRun tx %d: %w", txID, aerr)
	}
	if serr := w.SyncGroup(mark); serr != nil {
		return fmt.Errorf("SyncGroup tx %d: %w", txID, serr)
	}
	return nil
}

// walContiguityPerFrameTx commits one transaction as a sequence of individual
// [wal.Writer.Append] calls — the CONTROL. [wal.Writer.AppendCtx] serialises
// each append on its own, so another committer's frames may land between them,
// which is precisely the interleaving AppendRun was introduced to exclude.
func walContiguityPerFrameTx(w *wal.Writer, committer byte, txID uint16, frames int) error {
	for range frames {
		if e := w.Append(walSurfaceFrame(committer, txID, 16)); e != nil {
			return fmt.Errorf("per-frame append, tx %d: %w", txID, e)
		}
	}
	// SyncBuffered, not SyncGroup: this arm has no per-run watermark to hand a
	// group sync, which is itself part of what AppendRun's return value provides.
	if serr := w.SyncBuffered(); serr != nil {
		return fmt.Errorf("SyncBuffered tx %d: %w", txID, serr)
	}
	return nil
}

// -----------------------------------------------------------------------------
// The DETERMINISTIC contiguity arm — a constructed interleaving
// -----------------------------------------------------------------------------

// walContiguityAlternatingFrames is how many frames each of the two committers
// emits in the alternating arm. Four is enough for the split to be unambiguous
// (each transaction ends up in four fragments) and small enough to read in a
// failure message.
const walContiguityAlternatingFrames = 4

// walContiguityAlternatingPath is the WAL an alternating run writes.
func walContiguityAlternatingPath(perFrame bool) string {
	if perFrame {
		return "wal_contiguity_alt_perframe.wal"
	}
	return "wal_contiguity_alt_run.wal"
}

// RunWALContiguityAlternating CONSTRUCTS the frame ordering instead of racing
// for it. Two committers hand a token back and forth, so at every instant
// exactly one goroutine is eligible to append and the resulting durable image is
// fully determined by the protocol — not by the scheduler, the core count, the
// coverage instrumentation, or the load of the rest of the suite.
//
// # Why this replaced a concurrent control arm (rmp #2472, after #2517)
//
// The first version of the control drove eight committers concurrently and
// asserted that the per-frame path produced at least one split. It did — 31 of
// 96 transactions on an idle machine — but under `make ci`'s coverage step, with
// the whole suite running in parallel, the scheduler never overlapped the
// committers: all four retries measured `committerSwitches=7, split=0`, and the
// arm reddened a green tree. An assertion on a scheduling outcome measures the
// MACHINE, which is the defect class rmp #2517 filed. Raising the retry count
// would have traded a red gate for a slow one and still measured the scheduler.
//
// # The two modes, and why the pair is the proof
//
// Both modes run the SAME handoff protocol; only the append API differs, so the
// difference in the durable image is attributable to the API and to nothing else.
//
//   - perFrame=true — strict alternation. A takes the token, appends ONE frame,
//     passes the token to B; B appends one frame and passes it back. Because
//     [wal.Writer.AppendCtx] releases the writer mutex between appends, the
//     partner's frame really does land in the middle, and the image is
//     a0 b0 a1 b1 … — every transaction broken into exactly FramesPerTx
//     fragments. This is a genuine interleaved image produced by the real writer,
//     and it is reproducible byte for byte.
//
//   - perFrame=false — the same handoff, except each committer emits its whole
//     transaction inside one [wal.Writer.AppendRun]. A signals (without waiting)
//     once it is INSIDE its run, and B then attempts its own append. B cannot get
//     in: AppendRun holds the writer mutex across the entire run, so B blocks
//     until A is done and the image is a0…a3 b0…b3 — two contiguous runs. The
//     ordering here is decided by the MUTEX, not by the scheduler, so it is
//     equally deterministic.
//
// A one-way signal is used in the second mode on purpose: a full ping-pong would
// deadlock there, because A would be waiting for B while holding the very mutex
// B needs. That deadlock is the mechanism under test, so the protocol must not
// depend on it resolving.
func RunWALContiguityAlternating(ctx context.Context, seed uint64, perFrame bool) (WALContiguityEvidence, error) {
	const committers, frames = 2, walContiguityAlternatingFrames
	path := walContiguityAlternatingPath(perFrame)
	disk := NewSimDisk(NewSeed(seed^durableDiskSeedMix), 0) // faultRate 0
	w, err := wal.OpenFS(simWALFS{disk: disk}, path)
	if err != nil {
		return WALContiguityEvidence{}, fmt.Errorf("sim: wal-contiguity alternating open: %w", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, committers)
	if perFrame {
		// Strict alternation. turn[i] is buffered with capacity 1 so the final
		// handoff of each side never blocks; A holds the first token.
		turn := [committers]chan struct{}{make(chan struct{}, 1), make(chan struct{}, 1)}
		turn[0] <- struct{}{}
		for c := range committers {
			wg.Add(1)
			go func(c int) {
				defer wg.Done()
				next := (c + 1) % committers
				for range frames {
					<-turn[c]
					if e := w.Append(walSurfaceFrame(byte(c), uint16(c), 16)); e != nil {
						errs[c] = fmt.Errorf("alternating append, committer %d: %w", c, e)
						turn[next] <- struct{}{} // never strand the partner
						return
					}
					turn[next] <- struct{}{}
				}
			}(c)
		}
	} else {
		// One-way handoff: B starts only once A is demonstrably inside its run.
		aInside := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, aerr := w.AppendRun(func(emit func([]byte) error) error {
				for i := range frames {
					if e := emit(walSurfaceFrame(0, 0, 16)); e != nil {
						return e
					}
					if i == 0 {
						// Inside the run, holding the writer mutex. Closing a channel
						// is not a Writer method, so AppendRun's contract is intact.
						close(aInside)
					}
				}
				return nil
			})
			errs[0] = aerr
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-aInside:
			case <-ctx.Done():
				errs[1] = ctx.Err()
				return
			}
			_, berr := w.AppendRun(func(emit func([]byte) error) error {
				for range frames {
					if e := emit(walSurfaceFrame(1, 1, 16)); e != nil {
						return e
					}
				}
				return nil
			})
			errs[1] = berr
		}()
	}
	wg.Wait()
	for c, cerr := range errs {
		if cerr != nil {
			_ = w.Close()
			return WALContiguityEvidence{}, fmt.Errorf("sim: wal-contiguity alternating committer %d: %w", c, cerr)
		}
	}
	if serr := w.SyncBuffered(); serr != nil {
		_ = w.Close()
		return WALContiguityEvidence{}, fmt.Errorf("sim: wal-contiguity alternating sync: %w", serr)
	}
	if cerr := w.Close(); cerr != nil {
		return WALContiguityEvidence{}, fmt.Errorf("sim: wal-contiguity alternating close: %w", cerr)
	}
	if err := ctx.Err(); err != nil {
		return WALContiguityEvidence{}, err
	}
	ev, cerr := walContiguityCensus(disk, path, WALContiguityConfig{
		Committers: committers, FramesPerTx: frames, PerFrameAppend: perFrame,
	})
	ev.Alternating = true
	return ev, cerr
}

// walContiguityAlternatingWant is the EXACT layout the alternating protocol must
// produce. Asserting the whole layout rather than "at least one split" is what
// makes the arm a construction rather than a hope: any deviation — a frame lost,
// a handoff skipped, an append path that stopped releasing the mutex between
// frames — changes one of these numbers.
func walContiguityAlternatingWant(perFrame bool) WALContiguityEvidence {
	const committers, frames = 2, walContiguityAlternatingFrames
	if perFrame {
		return WALContiguityEvidence{
			Committers: committers, FramesPerTx: frames, PerFrameAppend: true, Alternating: true,
			Transactions: committers, Frames: committers * frames,
			// Every frame is its own maximal run, so each transaction is in
			// `frames` fragments and every run boundary changes committer.
			Runs: committers * frames, SplitTransactions: committers,
			WorstFragments: frames, CommitterSwitches: committers*frames - 1,
		}
	}
	return WALContiguityEvidence{
		Committers: committers, FramesPerTx: frames, PerFrameAppend: false, Alternating: true,
		Transactions: committers, Frames: committers * frames,
		Runs: committers, SplitTransactions: 0,
		WorstFragments: 1, CommitterSwitches: committers - 1,
	}
}

// checkWALContiguityAlternatingLayout adjudicates one alternating run against the
// exact layout its protocol determines. A deviation is reported field by field so
// a failure says which part of the construction stopped holding.
func checkWALContiguityAlternatingLayout(got *WALContiguityEvidence) []Violation {
	want := walContiguityAlternatingWant(got.PerFrameAppend)
	var v []Violation
	add := func(field string, g, w int) {
		if g == w {
			return
		}
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<wal-contiguity alternating>",
			Message: fmt.Sprintf("%s = %d, the handoff protocol determines %d — the constructed layout no longer holds, "+
				"so this arm is not the deterministic evidence it claims to be — %s", field, g, w, got.String()),
		})
	}
	add("frames", got.Frames, want.Frames)
	add("transactions", got.Transactions, want.Transactions)
	add("maximal runs", got.Runs, want.Runs)
	add("split transactions", got.SplitTransactions, want.SplitTransactions)
	add("worst fragments", got.WorstFragments, want.WorstFragments)
	add("committer switches", got.CommitterSwitches, want.CommitterSwitches)
	add("short transactions", got.ShortTransactions, 0)
	if got.TailErr != nil {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<wal-contiguity alternating>",
			Message: fmt.Sprintf("the durable image stops at %v — %s", got.TailErr, got.String()),
		})
	}
	return v
}

// walContiguityCensus parses the durable image and partitions it into maximal
// same-transaction runs. It reads the payload tags, so what it measures is the
// physical layout of the file and not any accounting the writer kept.
func walContiguityCensus(disk *SimDisk, path string, cfg WALContiguityConfig) (WALContiguityEvidence, error) {
	ev := WALContiguityEvidence{
		Committers: cfg.Committers, FramesPerTx: cfg.FramesPerTx, PerFrameAppend: cfg.PerFrameAppend,
	}
	img, err := disk.ReadFile(path)
	if err != nil {
		return ev, fmt.Errorf("sim: wal-contiguity read image: %w", err)
	}
	ev.ImageLen = int64(len(img))

	rh, err := disk.OpenFile(path, 0)
	if err != nil {
		return ev, fmt.Errorf("sim: wal-contiguity open image: %w", err)
	}
	defer func() { _ = rh.Close() }()

	r := wal.NewReader(rh, rh)
	fragments := make(map[uint16]int) // txID -> number of maximal runs
	frameCount := make(map[uint16]int)
	var prevTx uint16
	var prevCommitter byte
	first := true
	for f := range r.Frames() {
		if len(f.Payload) < 3 {
			return ev, fmt.Errorf("sim: wal-contiguity: frame %d payload is %d byte(s), too short to carry a tag", ev.Frames, len(f.Payload))
		}
		committer := f.Payload[0]
		txID := binary.BigEndian.Uint16(f.Payload[1:3])
		ev.Frames++
		frameCount[txID]++
		if first || txID != prevTx {
			// A new maximal run begins here.
			ev.Runs++
			fragments[txID]++
			if !first && committer != prevCommitter {
				ev.CommitterSwitches++
			}
		}
		prevTx, prevCommitter, first = txID, committer, false
	}
	ev.TailErr = r.TailError()
	ev.Transactions = len(fragments)
	ev.WorstFragments = 0
	for tx, n := range fragments {
		if n > 1 {
			ev.SplitTransactions++
		}
		if n > ev.WorstFragments {
			ev.WorstFragments = n
		}
		if frameCount[tx] != cfg.FramesPerTx {
			ev.ShortTransactions++
		}
	}
	return ev, nil
}

// checkWALContiguityNonVacuity is the SEPARATE coverage precondition for the
// contiguity census. It asserts the image is a population a contiguity claim can
// be made over — enough concurrent committers, more than one frame per
// transaction, every transaction present and whole, and evidence that the
// committers ACTUALLY interleaved — before [checkWALContiguity] speaks.
//
// The frames-per-transaction floor is the one that matters most: a transaction
// of one frame is contiguous by definition, so a run shaped that way would
// produce a permanently green guard that proves nothing.
func checkWALContiguityNonVacuity(e *WALContiguityEvidence, wantTx int) []Violation {
	var v []Violation
	add := func(msg string) {
		v = append(v, Violation{Kind: ViolationVacuousRun, Op: "<wal-contiguity non-vacuity>", Message: msg + " — " + e.String()})
	}
	if e.Committers < walContiguityMinCommitters {
		add(fmt.Sprintf("only %d concurrent committer(s), need >= %d: nothing could have interleaved",
			e.Committers, walContiguityMinCommitters))
	}
	if e.FramesPerTx < 2 {
		add(fmt.Sprintf("%d frame(s) per transaction: a single-frame transaction is contiguous by definition, so the census cannot fail",
			e.FramesPerTx))
	}
	if e.TailErr != nil {
		add(fmt.Sprintf("the durable image stops at %v: the census read a truncated image, not the committed one", e.TailErr))
	}
	if e.Transactions != wantTx {
		add(fmt.Sprintf("the image carries %d transaction(s), the workload committed %d: the census is not looking at the run it claims to",
			e.Transactions, wantTx))
	}
	if e.ShortTransactions != 0 {
		add(fmt.Sprintf("%d transaction(s) carry a frame count other than %d: frames were lost or duplicated, so a contiguity verdict would be read off the wrong population",
			e.ShortTransactions, e.FramesPerTx))
	}
	return v
}

// walContiguityMinSwitches is the floor on observed committer switches below
// which a CONCURRENT run is treated as having been denied concurrency by the
// machine. It is deliberately well above zero: a run measured under coverage
// instrumentation with the whole suite in parallel produced 7 switches across
// 96 transactions and no overlap at all, which is not a concurrency observation.
const walContiguityMinSwitches = 24

// checkWALContiguityConcurrencyWitness reports whether a CONCURRENT run was
// actually granted concurrency by the machine. It is a THIRD gate, separate from
// both the non-vacuity gate and the verdict, and the separation is the point.
//
// # Why this is not an error
//
// The switch count measures the SCHEDULER, not the code. Under coverage
// instrumentation with the rest of the suite running in parallel, eight
// committers were observed running essentially sequentially — 7 switches, no
// interleaving whatever. Failing on that would be a wall-clock gate measuring
// machine load rather than the module, which is the defect class rmp #2517
// filed, and it would redden a green tree at random.
//
// So a shortfall here means the run was UNINFORMATIVE ABOUT CONCURRENCY. It
// never means the writer is wrong, and it never excuses a split: the contiguity
// verdict ([checkWALContiguity]) is asserted unconditionally, because a split is
// a defect no matter how little concurrency produced it. The DETERMINISTIC
// evidence lives in [RunWALContiguityAlternating], which needs no scheduler
// cooperation at all.
func checkWALContiguityConcurrencyWitness(e *WALContiguityEvidence) []Violation {
	if e.CommitterSwitches >= walContiguityMinSwitches {
		return nil
	}
	return []Violation{{
		Kind: ViolationOracleDeviation,
		Op:   "<wal-contiguity concurrency witness>",
		Message: fmt.Sprintf(
			"only %d committer switch(es) across %d transactions, want >= %d: the machine ran the %d committers essentially "+
				"sequentially, so this run observed no real concurrency. The contiguity verdict still stands on its own; what is "+
				"missing is only the evidence that concurrency was exercised — %s",
			e.CommitterSwitches, e.Transactions, walContiguityMinSwitches, e.Committers, e.String()),
	}}
}

// checkWALContiguity adjudicates the property [wal.Writer.AppendRun] claims:
// every transaction's frames are ONE contiguous run in the durable file.
//
// It is a PURE function of the census, so a test can falsify it with a doctored
// record. The caller must run [checkWALContiguityNonVacuity] first; this
// function assumes the population was already shown to be non-trivial.
//
// A violation here is a genuine Atomicity finding, not a harness complaint:
// store/recovery discards a transaction's buffered prefix as orphaned on the
// stated ground that frames never interleave, so an interleaved image makes
// recovery drop COMMITTED ops.
func checkWALContiguity(e *WALContiguityEvidence) []Violation {
	if e.SplitTransactions == 0 && e.Runs == e.Transactions {
		return nil
	}
	return []Violation{{
		Kind: ViolationACIDAtomicity,
		Op:   "<wal-contiguity>",
		Message: fmt.Sprintf(
			"%d of %d transaction(s) are NOT contiguous in the durable WAL (%d runs for %d transactions, worst transaction split into %d fragments): "+
				"wal.Writer.AppendRun claims a transaction's frames land as one run, and store/recovery discards a transaction's "+
				"buffered prefix as orphaned on exactly that ground — so an interleaved image makes recovery drop committed ops — %s",
			e.SplitTransactions, e.Transactions, e.Runs, e.Transactions, e.WorstFragments, e.String()),
	}}
}

// -----------------------------------------------------------------------------
// Part 3 — whole-file Truncate and the poisoned-writer contract
// -----------------------------------------------------------------------------

// walLifecyclePath is the WAL the lifecycle arm writes on the SimDisk.
const walLifecyclePath = "wal_lifecycle.wal"

// walLifecycleFrames is how many frames the arm makes durable before the
// truncate, and walLifecyclePayload their payload size. Both are fixed so the
// truncate's return value has an exactly derivable expectation.
const (
	walLifecycleFrames  = 5
	walLifecyclePayload = 16
)

// WALLifecycleResult is the measured contract of [wal.Writer.Truncate] and of a
// POISONED writer. Every field is an observation; [checkWALLifecycle] holds them
// to the contract.
//
// # The poisoned contract as MEASURED, not as assumed
//
// Three of these were surprising enough to be worth naming, and all three were
// read off a real run before any assertion was written:
//
//   - [wal.Writer.SyncBuffered] on a poisoned writer returns NIL. The poison
//     rewinds the accepted offset to the durable one, so "make everything
//     accepted durable" is already satisfied and the durable-already fast path
//     fires. It is correct — nothing accepted is un-durable — but it means
//     SyncBuffered is NOT a health probe; [wal.Writer.Poisoned] is.
//   - [wal.Writer.Truncate] on a poisoned writer SUCCEEDS and empties the file,
//     while the writer stays poisoned. Truncate is the one mutator that does not
//     consult the sticky error. It is not a durability hole — the writer still
//     refuses every append, so nothing can be written after the emptied file —
//     and Truncate is documented as a maintenance helper off the production
//     checkpoint path (which cuts the WAL with TruncatePrefix instead). It is
//     pinned here so that a change putting Truncate on a live path is caught.
//   - After Close, Append and Truncate return [wal.ErrWriterClosed] rather than
//     the sticky poison: the closed check precedes the poison check. Poisoned()
//     still reports the sticky error.
type WALLifecycleResult struct {
	// --- healthy truncate ---

	// DurableBeforeTruncate is the offset covered by fsync just before Truncate,
	// and TruncateReturned what Truncate reported freeing. They must be equal:
	// Truncate documents its return as the bytes in the file at truncation.
	DurableBeforeTruncate, TruncateReturned int64
	// TruncateErr is Truncate's error on the healthy writer.
	TruncateErr error
	// DurableAfterTruncate and ImageAfterTruncate are the writer's watermark and
	// the file's real length after the truncate; both must be zero.
	DurableAfterTruncate, ImageAfterTruncate int64
	// StatsBefore / StatsAfter bracket the truncate. Truncate documents that the
	// LIFETIME counters are not reset, so these must be equal.
	StatsBefore, StatsAfter wal.Stats
	// PostTruncateMark is the watermark of the commit issued after the truncate.
	// It must equal one frame's worth of bytes: the append restarts at offset 0.
	PostTruncateMark int64
	// PostTruncateFrames is how many frames the re-read image carries afterwards;
	// it must be exactly the one appended after the truncate.
	PostTruncateFrames int

	// --- the poison ---

	// PoisonSyncErr is what the committer whose fsync failed received, and
	// PoisonSyncIsClass whether it carries [wal.ErrDurabilityFailed].
	PoisonSyncErr     error
	PoisonSyncIsClass bool
	// PoisonedReported is [wal.Writer.Poisoned] after the failure, and
	// PoisonedStable whether two consecutive calls returned the identical value.
	PoisonedReported error
	PoisonedStable   bool
	// AppendAfterPoisonIsSticky / AppendRunAfterPoisonIsSticky report that the
	// two append entry points returned the SAME error value Poisoned() reports.
	AppendAfterPoisonIsSticky, AppendRunAfterPoisonIsSticky bool
	// SyncBufferedAfterPoison is what SyncBuffered returned on the poisoned
	// writer. Measured NIL; see the type doc.
	SyncBufferedAfterPoison error
	// SyncGroupLostMarkIsSticky reports that a SyncGroup for a watermark the
	// poison DISCARDED returns the sticky error, and SyncGroupDurableMarkErr what
	// a SyncGroup for an already-durable watermark returned (nil: the
	// durability-first rule of rmp #2322).
	SyncGroupLostMarkIsSticky bool
	SyncGroupDurableMarkErr   error
	// SyncFailedCount is [wal.Stats.SyncFailed] after the poison: exactly one
	// round failed.
	SyncFailedCount uint64
	// DurableAtPoison is the watermark after the poison; it must still be the
	// offset acknowledged BEFORE the failure — the discarded suffix is gone.
	DurableAtPoison int64
	// AppendedAtPoison is [wal.Stats.Bytes] after the poison. It EXCEEDS
	// DurableAtPoison, because the discarded frame was still accepted and counted
	// — which is why the watermark oracle asserts durable <= appended and not
	// equality.
	AppendedAtPoison uint64
	// TruncateOnPoisonedErr / TruncateOnPoisonedReturned / ImageAfterPoisonTruncate
	// / StillPoisonedAfterTruncate pin the undocumented behaviour above.
	TruncateOnPoisonedErr      error
	TruncateOnPoisonedReturned int64
	ImageAfterPoisonTruncate   int64
	StillPoisonedAfterTruncate error
	// AppendAfterCloseIsClosed / TruncateAfterCloseIsClosed report that the
	// closed check precedes the poison check, and PoisonedAfterClose that the
	// sticky error is still legible on a closed writer.
	AppendAfterCloseIsClosed, TruncateAfterCloseIsClosed bool
	PoisonedAfterClose                                   error
}

// String renders the result for a failure message or a test log.
func (r WALLifecycleResult) String() string {
	return fmt.Sprintf("wal-lifecycle: truncate{durableBefore=%d returned=%d err=%v durableAfter=%d imageAfter=%d statsKept=%t postMark=%d postFrames=%d} "+
		"poison{class=%t stable=%t appendSticky=%t appendRunSticky=%t syncBuffered=%v lostMarkSticky=%t durableMark=%v syncFailed=%d durable=%d appended=%d} "+
		"truncateOnPoisoned{returned=%d err=%v imageAfter=%d stillPoisoned=%t} closed{append=%t truncate=%t poisonedLegible=%t}",
		r.DurableBeforeTruncate, r.TruncateReturned, r.TruncateErr, r.DurableAfterTruncate, r.ImageAfterTruncate,
		r.StatsBefore == r.StatsAfter, r.PostTruncateMark, r.PostTruncateFrames,
		r.PoisonSyncIsClass, r.PoisonedStable, r.AppendAfterPoisonIsSticky, r.AppendRunAfterPoisonIsSticky,
		r.SyncBufferedAfterPoison, r.SyncGroupLostMarkIsSticky, r.SyncGroupDurableMarkErr,
		r.SyncFailedCount, r.DurableAtPoison, r.AppendedAtPoison,
		r.TruncateOnPoisonedReturned, r.TruncateOnPoisonedErr, r.ImageAfterPoisonTruncate, r.StillPoisonedAfterTruncate != nil,
		r.AppendAfterCloseIsClosed, r.TruncateAfterCloseIsClosed, r.PoisonedAfterClose != nil)
}

// RunWALLifecycle drives the whole-file [wal.Writer.Truncate] and then the
// POISONED-writer contract on a fresh writer, over a [SimDisk].
//
// The two halves use separate writers on purpose: a truncate is only meaningful
// on a healthy writer, and the poison is terminal, so folding them into one
// writer would make the truncate half depend on the order the poison was applied.
func RunWALLifecycle(ctx context.Context, seed uint64) (WALLifecycleResult, error) {
	var r WALLifecycleResult
	if err := walLifecycleTruncateHalf(ctx, seed, &r); err != nil {
		return r, err
	}
	if err := walLifecyclePoisonHalf(ctx, seed, &r); err != nil {
		return r, err
	}
	return r, nil
}

// walLifecycleTruncateHalf exercises the whole-file truncate on a healthy
// writer: make frames durable, empty the WAL, and prove the next append restarts
// at offset zero of the freshly-empty file.
func walLifecycleTruncateHalf(ctx context.Context, seed uint64, r *WALLifecycleResult) error {
	disk := NewSimDisk(NewSeed(seed^durableDiskSeedMix), 0) // faultRate 0
	path := walLifecyclePath
	w, err := wal.OpenFS(simWALFS{disk: disk}, path)
	if err != nil {
		return fmt.Errorf("sim: wal-lifecycle open: %w", err)
	}
	defer func() { _ = w.Close() }()

	mark, aerr := w.AppendRun(func(emit func([]byte) error) error {
		for k := range walLifecycleFrames {
			if e := emit(walSurfaceFrame(0xC0, uint16(k), walLifecyclePayload)); e != nil {
				return e
			}
		}
		return nil
	})
	if aerr != nil {
		return fmt.Errorf("sim: wal-lifecycle append: %w", aerr)
	}
	if serr := w.SyncGroup(mark); serr != nil {
		return fmt.Errorf("sim: wal-lifecycle sync: %w", serr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	r.DurableBeforeTruncate = w.DurableOffset()
	r.StatsBefore = w.Stats()
	r.TruncateReturned, r.TruncateErr = w.Truncate()
	r.StatsAfter = w.Stats()
	r.DurableAfterTruncate = w.DurableOffset()
	img, rerr := disk.ReadFile(path)
	if rerr != nil {
		return fmt.Errorf("sim: wal-lifecycle read after truncate: %w", rerr)
	}
	r.ImageAfterTruncate = int64(len(img))

	// The append after the truncate must land at offset 0 of the empty file, and
	// the recovered image must hold ONLY it: that is what makes the truncate a
	// truncate rather than a bookkeeping reset.
	post, aerr := w.AppendRun(func(emit func([]byte) error) error {
		return emit(walSurfaceFrame(0xD0, 0, walLifecyclePayload))
	})
	if aerr != nil {
		return fmt.Errorf("sim: wal-lifecycle post-truncate append: %w", aerr)
	}
	if serr := w.SyncGroup(post); serr != nil {
		return fmt.Errorf("sim: wal-lifecycle post-truncate sync: %w", serr)
	}
	r.PostTruncateMark = post
	img, rerr = disk.ReadFile(path)
	if rerr != nil {
		return fmt.Errorf("sim: wal-lifecycle read after post-truncate commit: %w", rerr)
	}
	_, plens, tailErr := walFrameLayout(img)
	if tailErr != nil {
		return fmt.Errorf("sim: wal-lifecycle post-truncate image tail: %w", tailErr)
	}
	r.PostTruncateFrames = len(plens)
	return nil
}

// walLifecyclePoisonHalf drives one writer into the poison and then interrogates
// every member of its surface, including the two that are NOT documented
// (Truncate and SyncBuffered on a poisoned writer).
func walLifecyclePoisonHalf(ctx context.Context, seed uint64, r *WALLifecycleResult) error {
	disk := NewSimDisk(NewSeed(seed^durableDiskSeedMix), 0) // faultRate 0: only the armed one-shot fault fires
	path := walLifecyclePath + ".poison"
	w, err := wal.OpenFS(simWALFS{disk: disk}, path)
	if err != nil {
		return fmt.Errorf("sim: wal-lifecycle poison open: %w", err)
	}

	// One commit acknowledged BEFORE the failure, so the discarded suffix can be
	// told from what was already durable.
	good, aerr := w.AppendRun(func(emit func([]byte) error) error {
		return emit(walSurfaceFrame(0xC0, 0, walLifecyclePayload))
	})
	if aerr != nil {
		return fmt.Errorf("sim: wal-lifecycle poison prior append: %w", aerr)
	}
	if serr := w.SyncGroup(good); serr != nil {
		return fmt.Errorf("sim: wal-lifecycle poison prior sync: %w", serr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	lost, aerr := w.AppendRun(func(emit func([]byte) error) error {
		return emit(walSurfaceFrame(0xA0, 1, walLifecyclePayload))
	})
	if aerr != nil {
		return fmt.Errorf("sim: wal-lifecycle poison append: %w", aerr)
	}
	disk.ArmSyncFaultAt(1)
	r.PoisonSyncErr = w.SyncGroup(lost)
	r.PoisonSyncIsClass = errors.Is(r.PoisonSyncErr, wal.ErrDurabilityFailed)

	sticky := w.Poisoned()
	r.PoisonedReported = sticky
	// IDENTITY, not errors.Is, in the four comparisons below — deliberately. The
	// Writer doc claims more than a class: "every subsequent Append/Sync returns
	// the ORIGINAL error", and Poisoned's doc says it returns "the same sentinel
	// every subsequent Append/Sync returns". errors.Is would also accept a freshly
	// wrapped error of the same class, which is precisely the drift these clauses
	// exist to catch. The class is checked separately, by PoisonSyncIsClass.
	//nolint:errorlint // identity is the contract under test; see the comment above
	r.PoisonedStable = sticky != nil && w.Poisoned() == sticky
	r.DurableAtPoison = w.DurableOffset()
	st := w.Stats()
	r.SyncFailedCount = st.SyncFailed
	r.AppendedAtPoison = st.Bytes

	//nolint:errorlint // identity is the contract under test; see the comment above
	r.AppendAfterPoisonIsSticky = w.Append(walSurfaceFrame(0xB0, 2, walLifecyclePayload)) == sticky
	_, runErr := w.AppendRun(func(emit func([]byte) error) error {
		return emit(walSurfaceFrame(0xB1, 3, walLifecyclePayload))
	})
	//nolint:errorlint // identity is the contract under test; see the comment above
	r.AppendRunAfterPoisonIsSticky = runErr == sticky
	r.SyncBufferedAfterPoison = w.SyncBuffered()
	r.SyncGroupDurableMarkErr = w.SyncGroup(good)
	//nolint:errorlint // identity is the contract under test; see the comment above
	r.SyncGroupLostMarkIsSticky = w.SyncGroup(lost) == sticky

	// Undocumented, therefore measured: Truncate does not consult the poison.
	r.TruncateOnPoisonedReturned, r.TruncateOnPoisonedErr = w.Truncate()
	img, rerr := disk.ReadFile(path)
	if rerr != nil {
		return fmt.Errorf("sim: wal-lifecycle poison read after truncate: %w", rerr)
	}
	r.ImageAfterPoisonTruncate = int64(len(img))
	r.StillPoisonedAfterTruncate = w.Poisoned()

	// Close is best-effort on a poisoned writer; its error is the sticky one.
	_ = w.Close()
	r.PoisonedAfterClose = w.Poisoned()
	r.AppendAfterCloseIsClosed = errors.Is(w.Append(walSurfaceFrame(0xB3, 4, walLifecyclePayload)), wal.ErrWriterClosed)
	_, terr := w.Truncate()
	r.TruncateAfterCloseIsClosed = errors.Is(terr, wal.ErrWriterClosed)
	return nil
}

// walLifecycleFrameBytes is the byte cost of one lifecycle frame.
const walLifecycleFrameBytes = wal.HeaderSize + walLifecyclePayload

// checkWALLifecycle adjudicates the truncate and poisoned-writer contracts. It
// is a PURE function of the measured result, so a test can falsify it with a
// doctored record.
//
// Where the contract is DOCUMENTED the clause states the doc; where it was only
// MEASURED (SyncBuffered and Truncate on a poisoned writer) the clause pins what
// was measured and says so, so a future change surfaces as a failure to be
// judged rather than as a silent drift.
func checkWALLifecycle(r *WALLifecycleResult) []Violation {
	var v []Violation
	add := func(kind ViolationKind, msg string) {
		v = append(v, Violation{Kind: kind, Op: "<wal-lifecycle>", Message: msg + " — " + r.String()})
	}

	// --- Truncate on a healthy writer ---
	if r.TruncateErr != nil {
		add(ViolationOracleDeviation, fmt.Sprintf("Truncate on a healthy writer failed: %v", r.TruncateErr))
	}
	if want := int64(walLifecycleFrames * walLifecycleFrameBytes); r.DurableBeforeTruncate != want {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"the durable offset before the truncate is %d; %d frame(s) of %d byte(s) account for exactly %d",
			r.DurableBeforeTruncate, walLifecycleFrames, walLifecycleFrameBytes, want))
	}
	if r.TruncateReturned != r.DurableBeforeTruncate {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"Truncate reported freeing %d byte(s) but %d were durable: its return is documented as the bytes in the file at truncation",
			r.TruncateReturned, r.DurableBeforeTruncate))
	}
	if r.DurableAfterTruncate != 0 || r.ImageAfterTruncate != 0 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"after Truncate the watermark is %d and the file %d byte(s); both must be zero, or a later rollback truncates to a stale size",
			r.DurableAfterTruncate, r.ImageAfterTruncate))
	}
	if r.StatsBefore != r.StatsAfter {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"Truncate changed the LIFETIME counters, %+v -> %+v; they are documented as not reset", r.StatsBefore, r.StatsAfter))
	}
	if r.PostTruncateMark != walLifecycleFrameBytes {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"the commit after the truncate landed at offset %d; a %d-byte frame written to a freshly-empty file ends at %d",
			r.PostTruncateMark, walLifecycleFrameBytes, walLifecycleFrameBytes))
	}
	if r.PostTruncateFrames != 1 {
		add(ViolationACIDDurability, fmt.Sprintf(
			"the image after the post-truncate commit holds %d frame(s); expected exactly 1 — the truncate did not discard the previous WAL",
			r.PostTruncateFrames))
	}

	// --- the poison ---
	if !r.PoisonSyncIsClass {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"the failed commit returned %v, which does not carry wal.ErrDurabilityFailed: a caller cannot tell a durability fail-stop from a conflict of its own (rmp #2306)",
			r.PoisonSyncErr))
	}
	if r.PoisonedReported == nil {
		add(ViolationACIDDurability,
			"Poisoned() reports healthy after a failed fsync: the checkpointer consults it to decide whether publishing a snapshot would persist a non-acknowledged change (rmp #1919)")
	}
	if !r.PoisonedStable {
		add(ViolationOracleDeviation,
			"two consecutive Poisoned() calls did not return the identical sticky error: the fail-stop state is not stable")
	}
	if !r.AppendAfterPoisonIsSticky || !r.AppendRunAfterPoisonIsSticky {
		add(ViolationACIDDurability, fmt.Sprintf(
			"an append after the poison did not return the sticky error (Append sticky=%t, AppendRun sticky=%t): frames buffered after a discarded suffix would be acknowledged by the next successful fsync",
			r.AppendAfterPoisonIsSticky, r.AppendRunAfterPoisonIsSticky))
	}
	if !r.SyncGroupLostMarkIsSticky {
		add(ViolationACIDDurability,
			"SyncGroup for a watermark the poison DISCARDED did not return the sticky error: a committer whose frames were thrown away would be acknowledged")
	}
	if r.SyncGroupDurableMarkErr != nil {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"SyncGroup for an ALREADY-DURABLE watermark returned %v on a poisoned writer; rmp #2322 fixed exactly this: durability is tested before the poison, "+
				"because a committer whose marker is on the platter must not be told its commit failed", r.SyncGroupDurableMarkErr))
	}
	if r.SyncFailedCount != 1 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"wal.Stats.SyncFailed is %d after one failed round; expected 1 (rejections by an already-poisoned writer are documented as not counted)",
			r.SyncFailedCount))
	}
	if want := int64(walLifecycleFrameBytes); r.DurableAtPoison != want {
		add(ViolationACIDDurability, fmt.Sprintf(
			"the watermark after the poison is %d; it must still be the %d byte(s) acknowledged BEFORE the failure — the un-synced suffix was discarded",
			r.DurableAtPoison, want))
	}
	if r.AppendedAtPoison <= uint64(r.DurableAtPoison) {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"wal.Stats.Bytes (%d) does not exceed the durable offset (%d) after a discard: the accepted-bytes counter is documented as counting every frame appended, including one a poison threw away",
			r.AppendedAtPoison, r.DurableAtPoison))
	}
	// MEASURED, not documented: SyncBuffered on a poisoned writer returns nil.
	if r.SyncBufferedAfterPoison != nil {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"SyncBuffered on a poisoned writer returned %v; MEASURED behaviour is nil, because the poison rewinds the accepted offset to the durable one "+
				"and the durable-already fast path fires. If this now errors the contract has changed and callers using it as a flush must be re-checked",
			r.SyncBufferedAfterPoison))
	}
	// MEASURED, not documented: Truncate does not consult the poison.
	if r.TruncateOnPoisonedErr != nil || r.ImageAfterPoisonTruncate != 0 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"Truncate on a POISONED writer returned %v leaving %d byte(s); MEASURED behaviour is a successful empty. Truncate is the one mutator that does not "+
				"consult the sticky error; it is safe only because the writer stays poisoned and refuses every later append, and because the production checkpoint "+
				"cuts the WAL with TruncatePrefix instead. A change here needs judging, not absorbing",
			r.TruncateOnPoisonedErr, r.ImageAfterPoisonTruncate))
	}
	if r.StillPoisonedAfterTruncate == nil {
		add(ViolationACIDDurability,
			"the writer is no longer poisoned after Truncate: emptying the file cleared the fail-stop, so appends onto a WAL whose durability failed would resume")
	}
	if !r.AppendAfterCloseIsClosed || !r.TruncateAfterCloseIsClosed {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"after Close, Append/Truncate did not report wal.ErrWriterClosed (append=%t, truncate=%t): the closed check is documented to precede the poison check",
			r.AppendAfterCloseIsClosed, r.TruncateAfterCloseIsClosed))
	}
	if r.PoisonedAfterClose == nil {
		add(ViolationOracleDeviation,
			"Poisoned() reports healthy on a CLOSED writer that was poisoned: the owner can no longer tell why the handle died")
	}
	return v
}

// -----------------------------------------------------------------------------
// Part 4 — the two guards SimDisk cannot express
// -----------------------------------------------------------------------------

// WALGuardResult is what the real-directory arm observed of the two
// process-level guards.
//
// # Why this arm leaves the simulated disk
//
// [wal.ErrWALLocked] comes from flock(2) on a LOCK sentinel and the symlink
// refusal from O_NOFOLLOW on the final path component. [SimDisk] is a flat
// in-memory key table with neither links nor advisory locks, so both guards are
// STRUCTURALLY unreachable through it — not merely undriven. Modelling them in
// SimDisk would test the model; opening a real second writer tests the syscall
// the guard is made of, which is the thing that has to keep working.
//
// The consequence is stated rather than left to be inferred: these two guards
// are NOT covered by any seeded, crash-injecting scenario in this package, and
// they never will be while SimDisk has no link or lock semantics. This arm is
// their only representation here.
type WALGuardResult struct {
	// Skipped reports that the platform can express NOTHING here — Windows has
	// no O_NOFOLLOW and privileged symlink creation — so the arm exercised no
	// guard at all and the adjudicator makes no claim.
	//
	// It is deliberately NOT set when only SOME axis could not be exercised. A
	// whole-record skip raised LATE discards every guard already measured above
	// it, and because the adjudicator returns early on Skipped it then reports
	// success having judged nothing. That is precisely how an unavailable LOCK
	// sentinel used to throw away a live CWE-59 symlink detection (rmp #2745).
	// A partial failure is recorded PER AXIS instead, below.
	Skipped bool
	// SkipReason explains a whole-record skip.
	SkipReason string

	// LockGuardsAttempted, SymlinkWALAttempted, SymlinkLockAttempted and
	// VictimChecked report which axes were actually exercised. The adjudicator
	// judges every attempted axis and stays silent only about the others, so one
	// unavailable axis can no longer silence the rest.
	//
	// They are also this arm's WITNESS: a record that declares no skip and yet
	// attempted nothing is itself a violation, because an oracle that observed
	// nothing cannot fail and therefore proves nothing.
	LockGuardsAttempted  bool
	SymlinkWALAttempted  bool
	SymlinkLockAttempted bool
	VictimChecked        bool
	// Unmeasured names each axis that could not be exercised, and why. It is
	// reported, never silently dropped: an axis nobody ran is unknown, not clean.
	Unmeasured []string

	// FirstOpenErr is the error of the first (expected successful) open.
	FirstOpenErr error
	// SecondOpenErr is the error of a second open against the SAME path while the
	// first writer holds the lock, and SecondOpenIsLocked whether it is
	// [wal.ErrWALLocked].
	SecondOpenErr      error
	SecondOpenIsLocked bool
	// ReopenAfterCloseErr is the error of a third open once the first writer is
	// closed. It must be nil: a lock the owner cannot release is a lock that
	// strands the WAL.
	ReopenAfterCloseErr error

	// SymlinkedWALErr is the error of opening a WAL path whose final component is
	// a symlink to a victim file, and SymlinkedLockErr the same for the LOCK
	// sentinel — a second, distinct O_NOFOLLOW site (lock_unix.go), since the
	// lock file is opened before any WAL data is touched.
	SymlinkedWALErr, SymlinkedLockErr error
	// VictimIntact reports that the victim file's bytes are unchanged after both
	// attempts. This is the property that actually matters: an append through the
	// link would grow it and the O_TRUNC suffix temp would empty it.
	VictimIntact bool
}

// String renders the result for a failure message or a test log.
func (r WALGuardResult) String() string {
	if r.Skipped {
		return "wal-guards: SKIPPED — " + r.SkipReason
	}
	s := fmt.Sprintf("wal-guards: firstOpen=%v secondOpen=%v isLocked=%t reopenAfterClose=%v symlinkedWAL=%v symlinkedLock=%v victimIntact=%t"+
		" [attempted: lock=%t symlinkWAL=%t symlinkLock=%t victim=%t]",
		r.FirstOpenErr, r.SecondOpenErr, r.SecondOpenIsLocked, r.ReopenAfterCloseErr,
		r.SymlinkedWALErr, r.SymlinkedLockErr, r.VictimIntact,
		r.LockGuardsAttempted, r.SymlinkWALAttempted, r.SymlinkLockAttempted, r.VictimChecked)
	if len(r.Unmeasured) > 0 {
		s += " unmeasured: " + strings.Join(r.Unmeasured, "; ")
	}
	return s
}

// noteUnmeasured records that one axis could not be exercised. It is the narrow
// replacement for the whole-record skip this arm used to raise late: the axis is
// reported as unknown while every axis already measured keeps its verdict.
func (r *WALGuardResult) noteUnmeasured(axis string, cause error) {
	r.Unmeasured = append(r.Unmeasured, axis+": "+cause.Error())
}

// The fixed entry names [RunWALRealFSGuards] creates inside the caller's
// directory. They are named constants rather than literals so a test can make a
// specific axis unavailable — by pre-creating the entry it needs — without
// duplicating a path that could later drift out of step with the harness.
const (
	guardWALName       = "guarded.wal"
	guardVictimName    = "victim.bin"
	guardLinkedWALName = "linked.wal"
	guardLockBaseName  = "lockguard.wal"
	// guardLockSuffix is wal.Open's LOCK sentinel suffix for a WAL path.
	guardLockSuffix = ".lock"
)

// RunWALRealFSGuards drives [wal.Open]'s single-writer lock and its symlink
// refusal against a REAL directory, which the caller owns and cleans up (a test
// passes t.TempDir()).
//
// The two opens are made from THIS process. flock(2) associates the lock with
// the open file description rather than the process, so a second open(2) of the
// same sentinel conflicts with the first even in one process — which is what
// makes the guard testable without spawning a subprocess.
func RunWALRealFSGuards(dir string) (WALGuardResult, error) {
	var r WALGuardResult
	if runtime.GOOS == "windows" {
		r.Skipped = true
		r.SkipReason = "O_NOFOLLOW is a no-op on Windows and symlink creation is privileged; the WAL guards are governed by separate OS controls there"
		return r, nil
	}

	// --- the single-writer lock ---
	// Marked attempted BEFORE the first open so a first-open failure is judged
	// by its own clause rather than silently read as "not measured".
	r.LockGuardsAttempted = true
	walPath := filepath.Join(dir, guardWALName)
	w1, err := wal.Open(walPath)
	r.FirstOpenErr = err
	if err != nil {
		return r, fmt.Errorf("sim: wal-guards first open: %w", err)
	}
	w2, err2 := wal.Open(walPath)
	r.SecondOpenErr = err2
	r.SecondOpenIsLocked = errors.Is(err2, wal.ErrWALLocked)
	if err2 == nil {
		_ = w2.Close()
	}
	if cerr := w1.Close(); cerr != nil {
		return r, fmt.Errorf("sim: wal-guards close first: %w", cerr)
	}
	w3, err3 := wal.Open(walPath)
	r.ReopenAfterCloseErr = err3
	if err3 == nil {
		_ = w3.Close()
	}

	// --- the symlink refusal ---
	//
	// Each symlink axis is exercised INDEPENDENTLY. An axis whose link cannot be
	// created is recorded as unmeasured and the run continues, because the guards
	// above and beside it were already observed and their verdicts are real. The
	// LOCK-sentinel link is the one that fails in practice (an existing entry at
	// the sentinel path makes os.Symlink return EEXIST), and it used to discard
	// the WAL-path CWE-59 detection measured immediately before it.
	victim := filepath.Join(dir, guardVictimName)
	want := []byte("OUTSIDE-VICTIM-CONTENT")
	if werr := os.WriteFile(victim, want, 0o600); werr != nil {
		return r, fmt.Errorf("sim: wal-guards write victim: %w", werr)
	}
	linkedWAL := filepath.Join(dir, guardLinkedWALName)
	if lerr := os.Symlink(victim, linkedWAL); lerr != nil {
		r.noteUnmeasured("the WAL-path symlink refusal", lerr)
	} else {
		r.SymlinkWALAttempted = true
		if lw, oerr := wal.Open(linkedWAL); oerr == nil {
			_ = lw.Close()
			r.SymlinkedWALErr = nil
		} else {
			r.SymlinkedWALErr = oerr
		}
	}

	// The LOCK sentinel is opened before any WAL data is read or written, so a
	// symlinked sentinel is a separate escape route with its own O_NOFOLLOW.
	lockBase := filepath.Join(dir, guardLockBaseName)
	if lerr := os.Symlink(victim, lockBase+guardLockSuffix); lerr != nil {
		r.noteUnmeasured("the LOCK-sentinel symlink refusal", lerr)
	} else {
		r.SymlinkLockAttempted = true
		if lw, oerr := wal.Open(lockBase); oerr == nil {
			_ = lw.Close()
			r.SymlinkedLockErr = nil
		} else {
			r.SymlinkedLockErr = oerr
		}
	}

	// The victim is read whenever at least one link was actually opened through:
	// that is the only way its bytes could have changed, and it is the property
	// that ultimately matters. With neither link created there is nothing to say.
	if r.SymlinkWALAttempted || r.SymlinkLockAttempted {
		got, rerr := os.ReadFile(victim) //nolint:gosec // the harness's own temp-dir victim file
		if rerr != nil {
			return r, fmt.Errorf("sim: wal-guards read victim: %w", rerr)
		}
		r.VictimChecked = true
		r.VictimIntact = bytes.Equal(got, want)
	} else {
		r.noteUnmeasured("the victim-file integrity check", errors.New("no symlink could be created, so nothing could have written through one"))
	}
	return r, nil
}

// checkWALRealFSGuards adjudicates the process-level guards. It is a PURE
// function of the observed result, and it makes NO claim about a guard that was
// not exercised: a platform that cannot express an axis is uninformative there,
// not faulty.
//
// # Silence is per axis, never per record
//
// The adjudicator judges every axis the run ATTEMPTED and stays silent only
// about the axes it did not. Only a whole-record [WALGuardResult.Skipped] — the
// platform expressing nothing at all — silences everything, and that flag is now
// raised in exactly one place, before any guard runs.
//
// The distinction is the whole point of rmp #2745. An unavailable LOCK-sentinel
// link used to set Skipped and return, and this function's early return on
// Skipped then threw away four verdicts already MEASURED: the first open, the
// single-writer lock, the lock's release on Close, and — the one that matters
// most — the CWE-59 detection for a symlinked WAL PATH, whose wal.Open had
// already been performed. The gate reported success having judged none of them.
//
// An attempted-nothing record that does not declare a skip is itself a
// violation: an oracle that observed nothing cannot fail, and so proves nothing.
func checkWALRealFSGuards(r *WALGuardResult) []Violation {
	if r.Skipped {
		return nil
	}
	var v []Violation
	add := func(kind ViolationKind, msg string) {
		v = append(v, Violation{Kind: kind, Op: "<wal-guards>", Message: msg + " — " + r.String()})
	}

	if !r.LockGuardsAttempted && !r.SymlinkWALAttempted && !r.SymlinkLockAttempted {
		add(ViolationOracleDeviation,
			"the WAL-guard arm declared no skip and yet exercised NO guard: a gate that observes nothing cannot fail and therefore proves nothing")
		return v
	}

	if r.LockGuardsAttempted {
		switch {
		case r.FirstOpenErr != nil:
			add(ViolationOracleDeviation, fmt.Sprintf(
				"the first wal.Open failed (%v); the remaining lock guards were not reached", r.FirstOpenErr))
		default:
			if !r.SecondOpenIsLocked {
				add(ViolationACIDConsistency, fmt.Sprintf(
					"a second wal.Open against a locked WAL returned %v, want wal.ErrWALLocked: two writers appending to one file interleave their frames and corrupt the log",
					r.SecondOpenErr))
			}
			if r.ReopenAfterCloseErr != nil {
				add(ViolationOracleDeviation, fmt.Sprintf(
					"reopening after Close failed with %v: the lock is not released by its owner, which strands the WAL until the process exits",
					r.ReopenAfterCloseErr))
			}
		}
	}
	if r.SymlinkWALAttempted && r.SymlinkedWALErr == nil {
		add(ViolationACIDConsistency,
			"wal.Open FOLLOWED a symlinked WAL path: the mutation stream would be appended to whatever the link points at (CWE-59, security finding #1843)")
	}
	if r.SymlinkLockAttempted && r.SymlinkedLockErr == nil {
		add(ViolationACIDConsistency,
			"wal.Open FOLLOWED a symlinked LOCK sentinel: the lock file is opened before any WAL data, so this is a second escape route past O_NOFOLLOW (CWE-59)")
	}
	if r.VictimChecked && !r.VictimIntact {
		add(ViolationACIDConsistency,
			"the victim file outside the WAL directory was MODIFIED through a symlink: the refusal is reported but does not hold")
	}
	return v
}
