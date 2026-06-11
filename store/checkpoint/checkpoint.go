// Package checkpoint runs a background goroutine that periodically
// folds the WAL tail into a fresh snapshot and truncates the WAL.
// Without this the WAL would grow unbounded during steady-state
// operation.
//
// The checkpointer takes the snapshot and truncates the WAL under the
// store's commit serialisation, so no transaction can be mid-apply or
// mid-commit during that window: the snapshot is a consistent
// transaction-boundary image and the truncation never drops a frame
// committed after the snapshot. Wire it with [WithCommitSerialiser]
// ([txn.Store.RunUnderCommitLock]) when the store is driven by the Cypher
// engine, whose commit mutex is private and is therefore NOT the storeMu an
// external checkpointer is constructed with (see docs/acid-audit.md F3.5).
// The serialisation is held for the whole snapshot write, so the checkpointer
// blocks writers for the duration of the disk I/O; a non-blocking,
// watermark-bounded truncate is the documented future optimisation
// (docs/isolation-design.md, "Checkpoint and recovery").
package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/crashpoint"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// ErrCheckpointerStopped is returned by Trigger and TriggerCtx when the
// checkpointer has stopped: its background loop has exited because Stop
// was called or because the context passed to Start was cancelled. It is
// a clean terminal signal, not a failure — a checkpoint cannot run once
// the loop is gone, so the call returns promptly with this sentinel
// instead of blocking forever on a result that will never arrive.
var ErrCheckpointerStopped = errors.New("checkpoint: checkpointer stopped")

// Config controls when the checkpointer fires.
type Config struct {
	// Dir is the snapshot directory and the location of the WAL.
	Dir string
	// MaxAge fires a checkpoint when more than this duration has
	// elapsed since the previous one. Zero disables age-based
	// triggering.
	MaxAge time.Duration
	// Interval is the polling interval; if MaxAge fires before the
	// next tick the checkpointer still waits Interval to re-poll.
	// Defaults to MaxAge/4 when zero.
	Interval time.Duration
}

// Stats is a monotonic snapshot of the checkpointer's lifetime
// counters.
type Stats struct {
	Checkpoints    uint64
	WALTruncBytes  uint64
	LastDurationNS uint64
	LastError      string
}

// Checkpointer holds the goroutine state.
//
// Concurrency: Start, Stop, Trigger, and Stats are safe to call from
// any number of goroutines. Stop is idempotent (safe to call any
// number of times serially or concurrently).
type Checkpointer[N comparable, W any] struct {
	cfg     Config
	g       *lpg.Graph[N, W]
	wlog    *wal.Writer
	storeMu *sync.Mutex
	// serialise, when non-nil, runs the whole snapshot+truncate critical
	// section under the store's real commit serialisation
	// ([txn.Store.RunUnderCommitLock]) instead of the raw storeMu. It is
	// the correct seam for an engine-driven store, whose commit mutex is
	// private and therefore never the same object as storeMu: holding it
	// excludes the engine's eager write+WAL-commit window so the snapshot
	// can never capture a partially-applied transaction and the WAL
	// truncation can never drop a frame committed after the snapshot
	// (see WithCommitSerialiser and docs/acid-audit.md F3.5). When nil the
	// checkpointer falls back to locking storeMu directly (the historical
	// behaviour, correct only when storeMu IS the commit mutex, e.g. a
	// caller that serialises its own writes under storeMu).
	serialise func(func() error) error
	// codec, when non-nil, serialises the NodeID->key mapper into a
	// self-sufficient snapshot for ANY key type N (see WithMapperCodec).
	// When nil the checkpointer falls back to the string-only mapper:
	// non-string snapshots are then not self-sufficient and the WAL is
	// retained rather than truncated.
	codec txn.Codec[N]
	// constraintsFn, when non-nil, is called once per checkpoint run to
	// collect the current constraint set for persistence in
	// constraints.bin (see WithConstraintSpecs). When nil no
	// constraints.bin component is emitted, which is only safe when the
	// owning engine declares no schema constraints: a checkpoint that
	// truncates the WAL prefix which first declared a constraint would
	// otherwise silently lose it.
	constraintsFn func() []snapshot.ConstraintSpec

	stopCh    chan struct{}
	triggerCh chan chan error
	doneCh    chan struct{}
	// stoppedCh is closed by the loop's deferred teardown once the loop
	// has stopped reading triggerCh, regardless of why it exited (Stop
	// closing stopCh, or the Start context being cancelled). It is the
	// authoritative "the loop is gone" gate that TriggerCtx watches on
	// every wait edge so a caller can never block forever on a result
	// the departed loop will never deliver. stopCh alone is insufficient
	// because the context-cancellation exit path leaves stopCh open.
	stoppedCh chan struct{}
	stopOnce  sync.Once

	checkpoints  atomic.Uint64
	walTrunc     atomic.Uint64
	lastDuration atomic.Uint64
	lastErrMu    sync.Mutex
	lastErr      string
}

// Option customises a [Checkpointer] at construction. Options are
// applied in order by [New].
type Option[N comparable, W any] func(*Checkpointer[N, W])

// WithMapperCodec supplies the node-identifier codec the checkpointer
// uses to persist the NodeID->key interning table (mapper.bin) for ANY
// key type N. Pass the owning store's codec ([txn.Store.Codec]).
//
// Without this option the checkpointer persists the mapper only for
// string-keyed graphs; non-string snapshots are then not
// self-sufficient and the WAL is retained (never truncated) to avoid
// data loss, at the cost of unbounded WAL growth. With this option the
// snapshot is self-sufficient for every key type, so the checkpointer
// can truncate the WAL after each successful checkpoint and keep the
// log bounded (audit gap F3).
//
// A nil codec is ignored (the checkpointer keeps the string-only
// fallback).
func WithMapperCodec[N comparable, W any](codec txn.Codec[N]) Option[N, W] {
	return func(c *Checkpointer[N, W]) {
		if codec != nil {
			c.codec = codec
		}
	}
}

// WithConstraintSpecs supplies a callback the checkpointer invokes at
// each checkpoint to capture the current schema constraints for
// persistence in the snapshot's constraints.bin component. Wire
// cypher's Engine.ConstraintSpecsForSnapshot here so constraints survive
// a checkpoint that truncates the WAL prefix that first declared them:
//
//	cp := checkpoint.New(cfg, store.Graph(), wlog, &unusedMu,
//		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
//		checkpoint.WithMapperCodec[string, float64](store.Codec()),
//		checkpoint.WithConstraintSpecs[string, float64](eng.ConstraintSpecsForSnapshot))
//
// Why this matters: the engine persists each CREATE CONSTRAINT as a WAL
// op, so a plain reopen replays it. A checkpoint, however, folds the WAL
// into a snapshot and truncates the log — without this option the
// snapshot carries no constraint set, so after one checkpoint + restart
// every constraint is silently unenforced and duplicates are accepted
// with no error (a Consistency violation).
//
// The callback runs under the checkpoint's commit serialisation (see
// WithCommitSerialiser), so the captured set is transaction-boundary
// consistent with the snapshot it is persisted into.
//
// A nil callback is ignored (no constraints.bin emitted).
func WithConstraintSpecs[N comparable, W any](fn func() []snapshot.ConstraintSpec) Option[N, W] {
	return func(c *Checkpointer[N, W]) {
		if fn != nil {
			c.constraintsFn = fn
		}
	}
}

// WithCommitSerialiser makes the checkpointer run its entire
// snapshot-capture + WAL-truncate critical section under serialise instead
// of locking the raw storeMu passed to [New]. Pass the owning store's
// [txn.Store.RunUnderCommitLock]:
//
//	cp := checkpoint.New(cfg, store.Graph(), wlog, &unusedMu,
//		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
//		checkpoint.WithMapperCodec[string, float64](store.Codec()))
//
// Why this matters: the engine write path (cypher.Engine over a txn.Store)
// applies its in-memory mutations and appends its WAL frames while holding
// the store's PRIVATE commit mutex, from txn.Store.Begin until the
// transaction's commit in cypher Result.Close. That mutex is not the storeMu
// an external checkpointer is constructed with, so without this option the
// checkpointer never excludes the commit window: it can build the snapshot
// from a half-applied transaction, and it can truncate a WAL frame committed
// after the snapshot was taken — both violations of the ACID guarantees
// (docs/acid-audit.md F3.5). Running the critical section under
// RunUnderCommitLock closes both windows because no transaction can be
// between Begin and commit while serialise holds the commit mutex.
//
// The snapshot is additionally taken inside [lpg.Graph.View] regardless of
// this option, so the captured adjacency is always barrier-consistent; the
// serialiser is what additionally makes the truncate safe and the snapshot
// transaction-boundary aligned for the engine wiring.
//
// A nil serialiser is ignored (the checkpointer keeps locking storeMu).
func WithCommitSerialiser[N comparable, W any](serialise func(func() error) error) Option[N, W] {
	return func(c *Checkpointer[N, W]) {
		if serialise != nil {
			c.serialise = serialise
		}
	}
}

// New returns a Checkpointer; call Start to launch the goroutine.
//
// storeMu is the serialisation the checkpointer holds across the
// snapshot+truncate window WHEN no [WithCommitSerialiser] is supplied. It is
// correct only when the caller performs every write while holding this same
// mutex. For a [txn.Store] driven by the Cypher engine the commit mutex is
// private and unreachable, so storeMu can never be that mutex; such callers
// MUST pass [WithCommitSerialiser]([txn.Store.RunUnderCommitLock]), which
// supersedes storeMu and excludes the engine's eager write+commit window
// (docs/acid-audit.md F3.5). When a serialiser is supplied storeMu is unused
// and may be any mutex (a throwaway is fine).
//
// Pass [WithMapperCodec] to make non-string-keyed snapshots
// self-sufficient so the WAL can be truncated for every key type; see
// that option's documentation for the durability rationale.
func New[N comparable, W any](
	cfg Config,
	g *lpg.Graph[N, W],
	wlog *wal.Writer,
	storeMu *sync.Mutex,
	opts ...Option[N, W],
) *Checkpointer[N, W] {
	if cfg.Interval == 0 && cfg.MaxAge > 0 {
		cfg.Interval = cfg.MaxAge / 4
		if cfg.Interval < time.Millisecond {
			cfg.Interval = time.Millisecond
		}
	}
	c := &Checkpointer[N, W]{
		cfg:       cfg,
		g:         g,
		wlog:      wlog,
		storeMu:   storeMu,
		stopCh:    make(chan struct{}),
		triggerCh: make(chan chan error, 4),
		doneCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Start launches the background goroutine. ctx cancellation stops
// the goroutine. The goroutine is tagged with pprof labels
// (goroutine=checkpoint-loop, dir=<cfg.Dir>) so it appears named
// in pprof goroutine profiles rather than as anonymous "go c.loop".
func (c *Checkpointer[N, W]) Start(ctx context.Context) {
	labels := pprof.Labels(
		"goroutine", "checkpoint-loop",
		"dir", c.cfg.Dir,
	)
	go pprof.Do(ctx, labels, c.loop)
}

// Stop signals the goroutine to exit and blocks until it does.
// Stop is idempotent: subsequent calls are no-ops once the
// goroutine has exited.
func (c *Checkpointer[N, W]) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
	<-c.doneCh
}

// Trigger requests a checkpoint and blocks until it completes,
// returning its error. Equivalent to TriggerCtx(context.Background()).
//
// Once the checkpointer has stopped (Stop called, or the Start context
// cancelled) Trigger returns [ErrCheckpointerStopped] promptly instead
// of blocking: a checkpoint can no longer run, and the loop that would
// answer the request is gone. While the loop is running, a saturated
// trigger buffer (rare, but possible if many Trigger calls race ahead
// of the loop) can still delay Trigger until the loop drains it; prefer
// TriggerCtx in production code to bound that latency with a deadline.
func (c *Checkpointer[N, W]) Trigger() error {
	defer metrics.Time("store.checkpoint.Trigger")()
	err := c.TriggerCtx(context.Background())
	if err != nil {
		metrics.IncCounter("store.checkpoint.Trigger.errors", 1)
	}
	return err
}

// TriggerCtx requests a checkpoint and blocks until it completes,
// honouring ctx cancellation on every wait edge (queue-submit and
// result-wait) and returning [ErrCheckpointerStopped] promptly if the
// checkpointer has stopped. Returns ctx.Err() wrapped on cancellation
// or deadline expiry.
//
// Three independent guards make a permanent block impossible:
//
//  1. A non-blocking fast-path check of stoppedCh: once the loop has
//     exited, the request is never buffered, so it cannot be orphaned.
//  2. A stoppedCh arm on the buffered-submit select: a submit racing the
//     loop's exit either lands in the buffer (then guard 3 applies) or
//     observes the stop and returns the sentinel.
//  3. A stoppedCh arm on the result-wait select: even a request buffered
//     at the exact instant the loop departs is woken — the loop's
//     teardown closes stoppedCh after it stops reading triggerCh, so a
//     buffered request that the teardown's drain does not reach in time
//     still completes here rather than waiting forever on a result the
//     departed loop will never send.
func (c *Checkpointer[N, W]) TriggerCtx(ctx context.Context) error {
	defer metrics.Time("store.checkpoint.TriggerCtx")()
	// Fast path: never enter the buffered send once the loop is gone, or
	// the request could sit unread in the buffer until GC with no one to
	// answer it.
	select {
	case <-c.stoppedCh:
		metrics.IncCounter("store.checkpoint.TriggerCtx.errors", 1)
		return ErrCheckpointerStopped
	default:
	}
	done := make(chan error, 1)
	select {
	case c.triggerCh <- done:
		// Submitted; now wait for the result.
	case <-c.stoppedCh:
		metrics.IncCounter("store.checkpoint.TriggerCtx.errors", 1)
		return ErrCheckpointerStopped
	case <-ctx.Done():
		metrics.IncCounter("store.checkpoint.TriggerCtx.errors", 1)
		return fmt.Errorf("checkpoint: trigger submit cancelled: %w", ctx.Err())
	}
	select {
	case err := <-done:
		if err != nil {
			metrics.IncCounter("store.checkpoint.TriggerCtx.errors", 1)
		}
		return err
	case <-c.stoppedCh:
		// The loop exited after we buffered our request. Its teardown
		// drains the buffer and answers each entry with the sentinel
		// (see loop); this arm is the race-free backstop for the entry
		// that slips into the buffer at the exact boundary.
		metrics.IncCounter("store.checkpoint.TriggerCtx.errors", 1)
		return ErrCheckpointerStopped
	case <-ctx.Done():
		metrics.IncCounter("store.checkpoint.TriggerCtx.errors", 1)
		return fmt.Errorf("checkpoint: trigger wait cancelled: %w", ctx.Err())
	}
}

// Stats returns the current lifetime counters.
func (c *Checkpointer[N, W]) Stats() Stats {
	c.lastErrMu.Lock()
	last := c.lastErr
	c.lastErrMu.Unlock()
	return Stats{
		Checkpoints:    c.checkpoints.Load(),
		WALTruncBytes:  c.walTrunc.Load(),
		LastDurationNS: c.lastDuration.Load(),
		LastError:      last,
	}
}

func (c *Checkpointer[N, W]) loop(ctx context.Context) {
	// Teardown order matters. close(stoppedCh) first: it shuts the gate
	// TriggerCtx watches, so new callers take the fast path or the
	// stop arm instead of buffering into a channel no one will read, and
	// any caller already parked on the result-wait edge is woken with the
	// sentinel. Then drain whatever is already buffered and answer each
	// pending request with ErrCheckpointerStopped so those callers return
	// a clean stopped signal rather than relying solely on the wait-edge
	// backstop. close(doneCh) last so Stop unblocks only after the gate is
	// shut and the buffer is drained. Each done channel is buffered (cap
	// 1), so answering never blocks even if the caller has already
	// returned via the stoppedCh arm.
	defer func() {
		close(c.stoppedCh)
		for {
			select {
			case done := <-c.triggerCh:
				done <- ErrCheckpointerStopped
			default:
				close(c.doneCh)
				return
			}
		}
	}()
	var ticker *time.Ticker
	if c.cfg.Interval > 0 {
		ticker = time.NewTicker(c.cfg.Interval)
		defer ticker.Stop()
	}
	var lastFire time.Time
	for {
		var tickCh <-chan time.Time
		if ticker != nil {
			tickCh = ticker.C
		}
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case done := <-c.triggerCh:
			err := c.runCheckpoint()
			lastFire = time.Now()
			done <- err
		case <-tickCh:
			if c.cfg.MaxAge > 0 && time.Since(lastFire) >= c.cfg.MaxAge {
				// Periodic firings record the error in Stats.LastError
				// (via setErr inside runCheckpoint) so observability
				// surfaces; there is no caller to return to from the
				// loop, so the value itself is intentionally discarded.
				_ = c.runCheckpoint() //nolint:errcheck // error captured in stats
				lastFire = time.Now()
			}
		}
	}
}

func (c *Checkpointer[N, W]) runCheckpoint() error {
	start := time.Now()
	defer func() {
		c.lastDuration.Store(uint64(time.Since(start).Nanoseconds()))
	}()

	// Run the entire snapshot+truncate window under the store's commit
	// serialisation so that no transaction commits new WAL frames — and no
	// transaction's in-memory apply runs — between the snapshot capture and
	// the truncation. When a commit serialiser is wired (WithCommitSerialiser,
	// the engine path) the window holds the store's PRIVATE commit mutex,
	// which the engine actually takes from Begin to commit; without one we
	// fall back to locking the raw storeMu (correct only when the caller
	// serialises its own writes under that same mutex). Either way the
	// snapshot itself is captured inside Graph.View so the adjacency is
	// barrier-consistent even if the serialisation is misconfigured.
	//
	// The trade-off is that the serialisation is held during disk I/O for the
	// duration of the snapshot write; for very large graphs this can stall
	// writers and may be reworked later to a position-tracked truncate
	// (capture LSN under lock, write snapshot lock-free, truncate up-to-LSN
	// under lock). For v1 the simple correctness-first path is preferred.
	if c.serialise != nil {
		return c.serialise(c.runCheckpointLocked)
	}
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	return c.runCheckpointLocked()
}

// runCheckpointLocked performs the snapshot capture and WAL truncation. It
// MUST be called with the store's commit serialisation held (either via
// c.serialise or with c.storeMu locked) so no transaction can be mid-apply
// or mid-commit while it runs. The CSR is built inside [lpg.Graph.View] so
// the captured adjacency reflects a single transaction-boundary state with
// no partially-applied transaction visible.
func (c *Checkpointer[N, W]) runCheckpointLocked() error {
	var cs *csr.CSR[W]
	c.g.View(func() {
		cs = csr.BuildFromAdjList(c.g.AdjList())
	})
	snapDir := filepath.Join(c.cfg.Dir, "snapshot")
	// Durability invariant (audit gaps F2/F3): the snapshot MUST be a
	// self-sufficient image of the committed state — CSR adjacency PLUS
	// labels, properties, indexes, and the NodeID->key mapper — before
	// the WAL is truncated. The legacy WriteSnapshotCSR captured
	// adjacency only, so truncating the WAL afterwards destroyed every
	// committed label/property and, because v1 snapshots carry no mapper,
	// the NodeID->key mapping too: recovery then yielded an empty graph.
	//
	// When a mapper codec is wired in (WithMapperCodec, F3) the snapshot
	// is self-sufficient for EVERY key type, so the WAL can always be
	// truncated. Without a codec the mapper is persisted for string keys
	// only; non-string snapshots are then not self-sufficient and
	// runCheckpoint guards against data loss below by refusing to
	// truncate when the snapshot cannot stand alone. See
	// docs/acid-audit.md (F2/F3).
	//
	// The same self-sufficiency rule covers schema constraints: when the
	// engine has constraints declared (WithConstraintSpecs), the snapshot
	// must carry them in constraints.bin BEFORE the WAL prefix that first
	// declared them is truncated, or every constraint silently vanishes
	// on the next restart (#1334).
	var constraints []snapshot.ConstraintSpec
	if c.constraintsFn != nil {
		constraints = c.constraintsFn()
	}
	if err := c.writeSnapshot(snapDir, cs, constraints); err != nil {
		c.setErr(err)
		return err
	}
	selfSufficient, err := snapshotIsSelfSufficient(snapDir, len(constraints) > 0)
	if err != nil {
		c.setErr(err)
		return err
	}
	if err := c.wlog.Sync(); err != nil {
		c.setErr(err)
		return err
	}
	// Crash-injection point: the snapshot is durable on disk but the WAL
	// has NOT been truncated yet. A crash here must leave the store fully
	// recoverable — recovery loads the self-sufficient snapshot and
	// idempotently replays the still-intact WAL on top. No-op in
	// production (GOGRAPH_CRASH_AT unset).
	crashpoint.Breakpoint("checkpoint.post-snapshot-pre-truncate")
	if !selfSufficient {
		// The snapshot cannot reconstruct the graph on its own (no
		// mapper.bin for this key type, or a declared constraint set that
		// did not land in constraints.bin), so truncating the WAL would
		// lose committed data. Skip truncation: the WAL is retained and replayed
		// on top of the snapshot at recovery, preserving Durability at the
		// cost of unbounded WAL growth for this key type. Surfaced via a
		// metric so operators can detect the degraded mode.
		metrics.IncCounter("store.checkpoint.truncate_skipped_not_self_sufficient", 1)
		c.checkpoints.Add(1)
		c.setErr(nil)
		return nil
	}
	truncated, err := c.wlog.Truncate()
	if err != nil {
		c.setErr(err)
		return err
	}
	if truncated > 0 {
		c.walTrunc.Add(uint64(truncated))
		// Surface the bytes reclaimed through the metrics backend so
		// operators monitoring long-running stores can plot WAL-prefix
		// reclamation cadence without polling Stats(). The atomic
		// lifetime counter [c.walTrunc] remains the test-friendly
		// in-process aggregate; this counter is the observability
		// surface.
		metrics.IncCounter("store.checkpoint.wal_truncated_bytes", uint64(truncated))
	}
	c.checkpoints.Add(1)
	c.setErr(nil)
	return nil
}

// writeSnapshot publishes a self-sufficient snapshot of the current
// graph state to snapDir. When a mapper codec is configured
// ([WithMapperCodec]) it threads the codec so mapper.bin is emitted for
// every key type; otherwise it uses the string-only writer (mapper.bin
// for string keys, v2 without a mapper for other key types). constraints
// is the schema constraint set collected via [WithConstraintSpecs] for
// this run; when non-empty it is persisted as the snapshot's
// constraints.bin component (nil or empty emits no constraints.bin, and
// the output is byte-identical to the constraint-unaware writers).
func (c *Checkpointer[N, W]) writeSnapshot(snapDir string, cs *csr.CSR[W], constraints []snapshot.ConstraintSpec) error {
	if c.codec != nil {
		return snapshot.WriteSnapshotFullWithMapperCodecAndConstraints(snapDir, cs, c.g, c.codec, constraints)
	}
	return snapshot.WriteSnapshotFullWithConstraints(snapDir, cs, c.g, constraints)
}

func (c *Checkpointer[N, W]) setErr(err error) {
	c.lastErrMu.Lock()
	if err == nil {
		c.lastErr = ""
	} else {
		c.lastErr = err.Error()
	}
	c.lastErrMu.Unlock()
}

// snapshotIsSelfSufficient reports whether the snapshot just written to
// dir can reconstruct the graph WITHOUT replaying the WAL. A snapshot is
// self-sufficient when it carries a mapper.bin: the durable NodeID->key
// interning table that recovery needs to apply the CSR adjacency and the
// label/property records independently of the WAL. Only a self-sufficient
// snapshot makes WAL truncation safe — truncating the WAL after a snapshot
// that lacks the mapper would destroy committed data, since the NodeID->key
// mapping lived only in the now-erased WAL frames (audit gap F2).
//
// needConstraints additionally requires the snapshot to carry a
// constraints.bin component: when the engine has schema constraints
// declared, the WAL prefix being truncated holds the constraint ops, so
// a snapshot without the durable constraint set cannot stand alone —
// truncating after it would silently drop every constraint on the next
// restart (#1334).
//
// Detection is by manifest content, not version number, so it stays
// correct if the version scheme evolves: the snapshot is self-sufficient
// iff its manifest lists [snapshot.MapperFile] (and
// [snapshot.ConstraintsFile] when needConstraints is set).
func snapshotIsSelfSufficient(dir string, needConstraints bool) (bool, error) {
	m, err := snapshot.ReadManifestFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return false, fmt.Errorf("checkpoint: read snapshot manifest: %w", err)
	}
	var hasMapper, hasConstraints bool
	for _, f := range m.Files {
		switch f.Name {
		case snapshot.MapperFile:
			hasMapper = true
		case snapshot.ConstraintsFile:
			hasConstraints = true
		}
	}
	if !hasMapper {
		return false, nil
	}
	if needConstraints && !hasConstraints {
		return false, nil
	}
	return true, nil
}
