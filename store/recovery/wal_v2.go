package recovery

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"strconv"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// Snapshot-reaches-WAL refusals (docs/design-wal-v2.md §4.6, rmp #3014). Each is
// open-fatal: recovery returns it as the function error, replays nothing that
// depends on the mismatch, and [Result.IsClean] reports false.
var (
	// ErrForeignSnapshot is returned when the snapshot names a store id other
	// than the WAL control file's: the snapshot belongs to another store.
	ErrForeignSnapshot = errors.New("recovery: snapshot belongs to another store")
	// ErrSnapshotTooOld is returned when the snapshot covers the log only up to
	// a position R below the first retained position F: the frames in [R, F)
	// were discarded by a later checkpoint and exist nowhere else. A snapshot
	// that records no position, beside a log whose first retained position is
	// not 0, is refused the same way.
	ErrSnapshotTooOld = errors.New("recovery: snapshot is older than the oldest retained WAL position")
	// ErrSnapshotAheadOfWAL is returned when the snapshot covers the log up to
	// a position R beyond the end E of the valid log: the log is older than
	// the snapshot (for example segments restored from an older copy).
	ErrSnapshotAheadOfWAL = errors.New("recovery: snapshot covers WAL positions beyond the end of the log")
	// ErrRedoPointNotFrameBoundary is returned when no frame starts at the
	// snapshot's redo position R although the log extends past it.
	ErrRedoPointNotFrameBoundary = errors.New("recovery: snapshot redo position is not a WAL frame boundary")
	// ErrRedoPointMidTransaction is returned when a transaction has ops below
	// the snapshot's redo position R and its commit marker at or above it: a
	// checkpoint redo position is always a transaction boundary.
	ErrRedoPointMidTransaction = errors.New("recovery: snapshot redo position falls inside a transaction")
)

// isWALV2Corruption reports the WAL v2 container and snapshot-reaches-WAL
// sentinels, every one of which is open-fatal corruption.
func isWALV2Corruption(err error) bool {
	for _, s := range []error{
		wal.ErrMissingControl, wal.ErrControlCorrupt, wal.ErrSegmentHeader,
		wal.ErrForeignStore, wal.ErrSegmentGap, wal.ErrTornSegment,
		wal.ErrFramePosition, wal.ErrPrevLink, wal.ErrLegacyNotSealed,
		ErrForeignSnapshot, ErrSnapshotTooOld, ErrSnapshotAheadOfWAL,
		ErrRedoPointNotFrameBoundary, ErrRedoPointMidTransaction,
	} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

// snapshotRedo returns the redo position R a snapshot manifest records and
// whether it records one (manifest version 4 with wal_format 2).
func snapshotRedo(m *snapshot.Manifest) (int64, bool) {
	if m.WALFormat != 2 {
		return 0, false
	}
	return int64(m.WALRedoPos), true //nolint:gosec // G115: positions are bounded by int64 file arithmetic
}

// checkSnapshotReachesWAL applies the checks that need no frame
// (docs/design-wal-v2.md §4.3 and §4.6): the snapshot's store id against the
// control file, R against F, and the missing-snapshot rule. markerPresent
// reports the legacy prefix-truncation marker.
func checkSnapshotReachesWAL(ctl wal.Control, haveManifest bool, m *snapshot.Manifest, markerPresent bool) error {
	f := ctl.OldestRetainedPos
	if !haveManifest {
		if f != 0 || ctl.Flags&wal.ControlPrefixTruncated != 0 || markerPresent {
			metrics.IncCounter("store.recovery.openCodec.missingSnapshot", 1)
			return fmt.Errorf("%w: the WAL control file records a truncated prefix (oldest retained position %d) and no snapshot covers it",
				ErrMissingSnapshot, f)
		}
		return nil
	}
	if m.StoreID != "" {
		id, err := strconv.ParseUint(m.StoreID, 16, 64)
		if err != nil || id != ctl.StoreID {
			return fmt.Errorf("%w: snapshot store id %q, WAL store id %016x", ErrForeignSnapshot, m.StoreID, ctl.StoreID)
		}
	}
	r, haveR := snapshotRedo(m)
	switch {
	case haveR && uint64(r) < f: //nolint:gosec // G115: r is non-negative
		return fmt.Errorf("%w: snapshot covers the WAL up to position %d, the oldest retained position is %d", ErrSnapshotTooOld, r, f)
	case !haveR && f != 0:
		return fmt.Errorf("%w: snapshot records no WAL position and the oldest retained position is %d", ErrSnapshotTooOld, f)
	}
	if haveR && m.WALRedoPos < ctl.CheckpointRedoPos {
		// Complete data (F <= R), but an older snapshot than the last
		// truncating checkpoint published: a promoted backup.
		metrics.IncCounter("store.recovery.openCodec.olderSnapshotThanCheckpoint", 1)
	}
	return nil
}

// chainSource is the frame source of a segmented store: the legacy single-file
// log (when its history is replayed) and then the segment frames.
//
// The legacy file ends with its seal; a legacy file that does not, followed by
// segment frames, is [wal.ErrLegacyNotSealed]. A seal naming another store is
// [wal.ErrForeignStore].
type chainSource struct {
	log     *wal.Log
	legacy  *wal.Reader
	storeID uint64
	tailErr error
	tail    int64
}

func (c *chainSource) Frames() iter.Seq[wal.Frame] {
	return func(yield func(wal.Frame) bool) {
		sealed, legacyFrames := false, false
		if c.legacy != nil {
			for f := range c.legacy.Frames() {
				legacyFrames = true
				seal, ok := wal.DecodeLegacySeal(f.Payload)
				sealed = ok
				if ok && seal.StoreID != c.storeID {
					c.tailErr = fmt.Errorf("%w: legacy seal names store %016x", wal.ErrForeignStore, seal.StoreID)
					return
				}
				if !yield(f) {
					return
				}
			}
			if err := c.legacy.TailError(); err != nil {
				c.tailErr, c.tail = err, c.legacy.TailOffset()
				return
			}
		}
		first := true
		for f := range c.log.Frames() {
			if first && legacyFrames && !sealed {
				c.tailErr = wal.ErrLegacyNotSealed
				return
			}
			first = false
			if !yield(f) {
				return
			}
		}
		c.tailErr, c.tail = c.log.TailError(), c.log.TailOffset()
	}
}

func (c *chainSource) TailError() error { return c.tailErr }

func (c *chainSource) TailOffset() int64 {
	if c.tailErr == nil || c.tail == 0 {
		return c.log.TailOffset()
	}
	return c.tail
}

// replayLogWithoutSnapshot is [ReplayWAL] over a store's whole log with no
// snapshot: the legacy file, then the segments. A log whose control file
// records a truncated prefix is refused with [ErrMissingSnapshot], reported in
// [ReplayResult.TailErr] like every other stop reason of this core.
func replayLogWithoutSnapshot[N comparable, W any](
	ctx context.Context, log *wal.Log, g *lpg.Graph[N, W], codec txn.Codec[N], wcodec txn.WeightCodec[W],
	maxTxnOps int, cAcc *constraintSet, iAcc *indexSet, touched *touchSet,
) (ReplayResult, error) {
	lr, err := log.LegacyReader()
	if err != nil {
		return ReplayResult{}, err
	}
	if lr != nil {
		defer func() { _ = lr.Close() }()
	}
	ctl, segmented := log.Control()
	if !segmented {
		if lr == nil {
			return ReplayResult{}, nil
		}
		return replayWALInto(ctx, lr, g, codec, wcodec, maxTxnOps, cAcc, iAcc, touched, -1, false)
	}
	if cerr := checkSnapshotReachesWAL(ctl, false, &snapshot.Manifest{}, false); cerr != nil {
		return ReplayResult{TailErr: cerr}, nil
	}
	return replayWALInto(ctx, &chainSource{log: log, legacy: lr, storeID: ctl.StoreID}, g, codec, wcodec, maxTxnOps, cAcc, iAcc, touched, -1, false)
}

// openWALChecked opens the write-ahead log at walPath and applies the checks
// that need no frame: for a segmented log, that the snapshot reaches it
// ([checkSnapshotReachesWAL]); for a legacy log with no snapshot, that no
// prefix marker records a truncated history (rmp #2990). refused reports that
// err is such a refusal rather than an I/O failure.
func openWALChecked(fsys recoveryFS, walPath, snapDir string, haveManifest bool, m *snapshot.Manifest) (log *wal.Log, refused bool, err error) {
	log, err = fsys.OpenWALLog(walPath)
	if err != nil {
		return nil, true, fmt.Errorf("recovery: open WAL: %w", err)
	}
	markerPresent := false
	marker := wal.PrefixTruncatedMarkerPath(walPath)
	if _, serr := fsys.Stat(marker); serr == nil {
		markerPresent = true
	} else if !errors.Is(serr, os.ErrNotExist) {
		return nil, false, fmt.Errorf("recovery: probe WAL prefix marker: %w", serr)
	}
	ctl, segmented := log.Control()
	switch {
	case segmented:
		if cerr := checkSnapshotReachesWAL(ctl, haveManifest, m, markerPresent); cerr != nil {
			metrics.IncCounter("store.recovery.openCodec.snapshotWALMismatch", 1)
			return nil, true, cerr
		}
	case !haveManifest && markerPresent:
		// A WAL whose prefix a checkpoint truncated is a SUFFIX of the history,
		// and only the snapshot that folded the prefix makes it whole; replaying
		// it onto an empty graph would report a shorter history as clean.
		metrics.IncCounter("store.recovery.openCodec.missingSnapshot", 1)
		return nil, true, fmt.Errorf("%w: %s records a truncated WAL prefix and %s holds no snapshot manifest",
			ErrMissingSnapshot, marker, snapDir)
	}
	return log, false, nil
}

// walReplayPlan is how recovery replays a directory's log.
type walReplayPlan struct {
	src    wal.FrameSource
	legacy *wal.Reader
	// redo is the snapshot's redo position R, or -1; haveR reports one.
	redo  int64
	haveR bool
	// schemaBelowRedo selects the schema-DDL accumulation below R.
	schemaBelowRedo bool
}

// planWALReplay chooses the frame source. A legacy-only store replays its single
// file as before. A segmented store replays the legacy file only when the
// snapshot records no redo position (its history sits before position 0), then
// the segments; with a redo position R, frames below R are validated but only
// frames at or above R are applied (docs/design-wal-v2.md §4.8-§4.9).
//
// Schema DDL below R is accumulated only when the control file does not record
// this snapshot's checkpoint: a checkpoint records its redo position only for a
// snapshot that stands alone (constraints.bin and indexdefs.bin included).
// Otherwise — a checkpoint that kept the log because the snapshot lacked the
// schema, a crash before the control write, a promoted older snapshot — the
// frames below R are the schema's record, exactly as when the whole log was
// replayed, and re-applying a suffix of DDL to the snapshot's set is idempotent.
func planWALReplay(log *wal.Log, haveManifest bool, m *snapshot.Manifest) (walReplayPlan, error) {
	p := walReplayPlan{redo: -1}
	ctl, segmented := log.Control()
	if haveManifest && segmented {
		// A redo position names a position of a segmented log; a legacy log
		// has none and replays whole as before.
		if r, ok := snapshotRedo(m); ok {
			p.redo, p.haveR = r, true
		}
	}
	if !segmented || !p.haveR {
		lr, err := log.LegacyReader()
		if err != nil {
			return p, err
		}
		p.legacy = lr
	}
	switch {
	case segmented:
		p.src = &chainSource{log: log, legacy: p.legacy, storeID: ctl.StoreID}
	case p.legacy != nil:
		p.src = p.legacy
	}
	p.schemaBelowRedo = p.haveR && uint64(p.redo) != ctl.CheckpointRedoPos //nolint:gosec // G115: redo is non-negative when haveR
	return p, nil
}

// close releases the legacy reader; best-effort, read-only.
func (p walReplayPlan) close() {
	if p.legacy != nil {
		_ = p.legacy.Close()
	}
}

// checkReachesEnd reports [ErrSnapshotAheadOfWAL] when the snapshot's redo
// position lies beyond the end of the valid log. A corruption that cut the log
// short keeps its own, more precise sentinel.
func (p walReplayPlan) checkReachesEnd(tailErr error, end int64) error {
	if p.haveR && !tailErrIsCorruption(tailErr) && end < p.redo {
		return fmt.Errorf("%w: snapshot covers the WAL up to position %d, the log ends at %d",
			ErrSnapshotAheadOfWAL, p.redo, end)
	}
	return tailErr
}
