// Package checkpoint runs a background goroutine that periodically
// folds the WAL tail into a fresh snapshot and truncates the WAL.
// Without this the WAL would grow unbounded during steady-state
// operation.
//
// The checkpoint is NON-BLOCKING: it holds the store's commit serialisation
// only to capture a transaction-boundary-consistent image — the WAL durable
// offset (the watermark) and a [snapshot.Capture] of the WHOLE graph, taken
// inside one [lpg.Graph.View] — then RELEASES the lock and writes the
// (potentially multi-second) snapshot disk I/O while transactions commit
// concurrently. It re-acquires the lock only briefly at the end to
// prefix-truncate the WAL up to the captured watermark, discarding the frames
// the snapshot folded while preserving every frame committed during the
// lock-free write. Writers therefore stall only for the in-memory capture,
// never for the snapshot write (#1508). The pattern mirrors RocksDB flush (seal
// under a short lock, write without blocking writers) and PostgreSQL CHECKPOINT
// (write concurrently, then trim the WAL up to the redo point).
//
// The capture covers EVERY graph-derived component — adjacency, mapper, labels,
// properties, tombstones, edge handles and index payloads — not the adjacency
// alone. Capturing only the adjacency and letting the lock-free writer walk the
// live graph for the rest published a PARTIAL TRANSACTION: components observed
// different instants, so a snapshot could carry a transaction's two new nodes
// without its edge (rmp #2269). See [snapshot.Capture].
//
// Wire it with [WithCommitSerialiser] ([txn.Store.RunUnderCommitLock]) when the
// store is driven by the Cypher engine, whose commit mutex is private and is
// therefore NOT the storeMu an external checkpointer is constructed with (see
// docs/acid-audit.md F3.5); RunUnderCommitLock also drains in-flight group
// commits, so the captured watermark is a true transaction boundary. The
// snapshot is a consistent transaction-boundary image and the prefix-truncate
// never drops a frame committed after the watermark; a crash at any
// interleaving recovers the exact committed state (recovery loads the
// self-sufficient snapshot and idempotently replays the surviving WAL) — see
// [wal.Writer.TruncatePrefix] and the checkpoint crashpoints.
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

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
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

// Config controls when the checkpointer fires. It is a plain configuration
// value read once when the [Checkpointer] is constructed; it is safe for
// concurrent read use once constructed, but must not be mutated after being
// passed to the constructor.
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
// counters. It is returned by value as a self-contained copy with no
// shared mutable state, so a Stats value is immutable in effect and safe
// for concurrent reads by multiple goroutines.
type Stats struct {
	LastError      string
	Checkpoints    uint64
	WALTruncBytes  uint64
	LastDurationNS uint64
}

// Checkpointer holds the goroutine state.
//
// Concurrency: Start, Stop, Trigger, and Stats are safe to call from
// any number of goroutines. Stop is idempotent (safe to call any
// number of times serially or concurrently). [RunCheckpoint] is the one
// exception — see its own doc for the narrower contract it requires.
type Checkpointer[N comparable, W any] struct {
	// codec, when non-nil, serialises the NodeID->key mapper into a
	// self-sufficient snapshot for ANY key type N (see WithMapperCodec).
	// When nil the checkpointer falls back to the string-only mapper:
	// non-string snapshots are then not self-sufficient and the WAL is
	// retained rather than truncated.
	codec txn.Codec[N]

	// snap is the snapshot-publish/probe backend. It defaults to
	// [osSnapshotBackend] in [New] (byte-identical production path); the
	// deterministic simulation harness injects an in-memory backend via
	// [WithSnapshotFS] so it can crash mid-snapshot and before the WAL
	// prefix-truncate. The checkpointer performs no direct filesystem call
	// of its own — only this backend and the injected WAL writer do.
	snap snapshotBackend[N, W]

	// clk is the wall-clock source for the cadence loop (ticker, MaxAge
	// elapsed comparison, duration measurement). It defaults to
	// [clock.Real] in [New] so production behaviour is unchanged; a test
	// (notably the deterministic simulation harness) may inject a fake via
	// [WithClock] to drive checkpoints by virtual time.
	clk clock.Clock

	// afterCaptureHook, when non-nil, is invoked at the START of phase 2 —
	// immediately after the phase-1 commit lock is released and BEFORE the
	// lock-free snapshot write. It is a test-only seam (set by white-box tests
	// in this package) used to prove the commit lock is NOT held during the
	// snapshot I/O: a test sets it to block and asserts a concurrent committer
	// proceeds meanwhile. It is nil in production.
	afterCaptureHook func()

	// afterWatermarkHook, when non-nil, is invoked INSIDE the phase-1 commit
	// lock, immediately after the durable-offset watermark and the MVCC instant
	// have both been taken and before anything is serialised. It is a test-only
	// seam (set by white-box tests in this package) used to assert the invariant
	// that makes those two numbers describe the SAME transaction boundary — no
	// commit may be durable-but-unpublished at this point, or the WAL prefix the
	// watermark authorises truncating would contain a transaction the image, read
	// at the instant, does not carry. It is nil in production.
	afterWatermarkHook func()

	triggerCh chan chan error
	storeMu   *sync.Mutex
	// constraintsFn, when non-nil, is called once per checkpoint run to
	// collect the current constraint set for persistence in
	// constraints.bin (see WithConstraintSpecs). When nil no
	// constraints.bin component is emitted, which is only safe when the
	// owning engine declares no schema constraints: a checkpoint that
	// truncates the WAL prefix which first declared a constraint would
	// otherwise silently lose it.
	constraintsFn func() []snapshot.ConstraintSpec
	// indexDefsFn, when non-nil, is called once per checkpoint run to collect
	// the current secondary-index definition set for persistence in
	// indexdefs.bin (see WithIndexSpecs). When nil no indexdefs.bin component is
	// emitted, which is only safe when the owning engine declares no indexes: a
	// checkpoint that truncates the WAL prefix which first declared an index
	// would otherwise silently lose its definition (#1755, the index analogue of
	// constraintsFn).
	indexDefsFn func() []snapshot.IndexDefSpec

	wlog *wal.Writer
	g    *lpg.Graph[N, W]

	// stoppedCh is closed by the loop's deferred teardown once the loop
	// has stopped reading triggerCh, regardless of why it exited (Stop
	// closing stopCh, or the Start context being cancelled). It is the
	// authoritative "the loop is gone" gate that TriggerCtx watches on
	// every wait edge so a caller can never block forever on a result
	// the departed loop will never deliver. stopCh alone is insufficient
	// because the context-cancellation exit path leaves stopCh open.
	stoppedCh chan struct{}
	stopCh    chan struct{}
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
	doneCh    chan struct{}
	lastErr   string
	cfg       Config

	checkpoints  atomic.Uint64
	walTrunc     atomic.Uint64
	lastDuration atomic.Uint64
	// errSeq mints one sequence number per checkpoint attempt (rmp #1873),
	// assigned at the start of [Checkpointer.runNonBlocking]. lastErrSeq
	// records the sequence number of the attempt currently reflected in
	// lastErr, so [Checkpointer.setErr] can reject a stale write: a run
	// that STARTED earlier but happens to COMPLETE later (only reachable
	// today via the documented-unsafe concurrent-RunCheckpoint misuse this
	// task also tightens the doc against) can no longer overwrite a
	// more-recently-STARTED run's already-recorded outcome. lastErrSeq and
	// lastErr are both guarded by lastErrMu — lastErrSeq is deliberately
	// NOT its own atomic.Uint64 the way errSeq is, so the (seq, error) pair
	// updates as one atomic unit under the mutex; a reader must never
	// observe a seq/error pair assembled from two different attempts.
	errSeq     atomic.Uint64
	lastErrSeq uint64
	stopOnce   sync.Once
	lastErrMu  sync.Mutex
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

// WithIndexSpecs supplies a callback the checkpointer invokes at each checkpoint
// to capture the current secondary-index definitions for persistence in the
// snapshot's indexdefs.bin component. Wire cypher's
// Engine.IndexSpecsForSnapshot here so user-created indexes survive a checkpoint
// that truncates the WAL prefix that first declared them:
//
//	cp := checkpoint.New(cfg, store.Graph(), wlog, &unusedMu,
//		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
//		checkpoint.WithMapperCodec[string, float64](store.Codec()),
//		checkpoint.WithConstraintSpecs[string, float64](eng.ConstraintSpecsForSnapshot),
//		checkpoint.WithIndexSpecs[string, float64](eng.IndexSpecsForSnapshot))
//
// Why this matters: the engine persists each CREATE INDEX as a WAL op, so a
// plain reopen replays it. A checkpoint, however, folds the WAL into a snapshot
// and truncates the log — without this option the snapshot carries no index
// definitions, so after one checkpoint + restart every user index is silently
// gone and index seeks degrade to full scans with no error (#1755). Only the
// index DEFINITION (label/property/kind/name) is persisted; recovery rebuilds
// each index by backfilling it from the recovered graph.
//
// The callback runs under the checkpoint's commit serialisation (see
// WithCommitSerialiser), so the captured set is transaction-boundary consistent
// with the snapshot it is persisted into.
//
// A nil callback is ignored (no indexdefs.bin emitted).
func WithIndexSpecs[N comparable, W any](fn func() []snapshot.IndexDefSpec) Option[N, W] {
	return func(c *Checkpointer[N, W]) {
		if fn != nil {
			c.indexDefsFn = fn
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

// WithClock supplies the wall-clock source for the cadence loop. When unset
// the checkpointer uses [clock.Real], so production behaviour is unchanged. A
// test or the deterministic simulation harness may inject a [clock.Fake] to
// drive periodic checkpoints by virtual time, making the cadence deterministic.
// A nil clock is ignored (the checkpointer keeps the real clock).
func WithClock[N comparable, W any](clk clock.Clock) Option[N, W] {
	return func(c *Checkpointer[N, W]) {
		if clk != nil {
			c.clk = clk
		}
	}
}

// WithSnapshotFS injects the backend the checkpointer publishes snapshots to
// and reads the manifest back from. It exists for the deterministic-simulation
// harness (internal/sim), which supplies an in-memory backend so a checkpoint
// runs entirely against its in-memory disk and a crash can be injected
// mid-snapshot or before the WAL prefix-truncate. The backend parameter type
// is unexported (mirroring
// [github.com/FlavioCFOliveira/GoGraph/store/wal.OpenWith]); production code
// omits this option and the checkpointer uses the OS-backed snapshot writers,
// which are byte-identical to the pre-seam path.
//
// A nil backend is ignored (the checkpointer keeps the OS backend).
func WithSnapshotFS[N comparable, W any](backend snapshotBackend[N, W]) Option[N, W] {
	return func(c *Checkpointer[N, W]) {
		if backend != nil {
			c.snap = backend
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
	if c.clk == nil {
		c.clk = clock.Real()
	}
	if c.snap == nil {
		c.snap = osSnapshotBackend[N, W]{}
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
	defer metrics.Time("store.checkpoint.Trigger").Stop()
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
	defer metrics.Time("store.checkpoint.TriggerCtx").Stop()
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
	var ticker clock.Ticker
	if c.cfg.Interval > 0 {
		ticker = c.clk.NewTicker(c.cfg.Interval)
		defer ticker.Stop()
	}
	var lastFire time.Time
	for {
		var tickCh <-chan time.Time
		if ticker != nil {
			tickCh = ticker.C()
		}
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case done := <-c.triggerCh:
			err := c.runCheckpoint()
			lastFire = c.clk.Now()
			done <- err
		case <-tickCh:
			if c.cfg.MaxAge > 0 && c.clk.Since(lastFire) >= c.cfg.MaxAge {
				// Periodic firings record the error in Stats.LastError
				// (via setErr inside runCheckpoint) so observability
				// surfaces; there is no caller to return to from the
				// loop, so the value itself is intentionally discarded.
				_ = c.runCheckpoint() //nolint:errcheck // error captured in stats
				lastFire = c.clk.Now()
			}
		}
	}
}

func (c *Checkpointer[N, W]) runCheckpoint() error {
	start := c.clk.Now()
	defer func() {
		c.lastDuration.Store(uint64(c.clk.Since(start).Nanoseconds()))
	}()
	return c.runNonBlocking()
}

// RunCheckpoint performs ONE checkpoint synchronously on the calling goroutine
// and returns its error, without starting (or requiring) the background cadence
// loop. It runs the identical three-phase critical section [Trigger] drives —
// capture under the commit lock, lock-free snapshot publish, prefix-truncate
// under the commit lock — so it is subject to exactly the same ACID guarantees
// (the snapshot is transaction-boundary consistent and the WAL prefix is
// reclaimed only after the self-sufficient snapshot is durable).
//
// It exists for the deterministic-simulation harness (internal/sim), which
// drives the whole store from a single goroutine and so must trigger a
// checkpoint inline — with no extra goroutine to keep the run reproducible —
// rather than via [Start]+[Trigger]. Production code uses [Start]/[Trigger].
//
// Concurrency contract (rmp #1873): do NOT call RunCheckpoint concurrently
// with itself, nor combine it with a running [Start]ed loop — only phase 1
// (the capture) and phase 3 (the prefix-truncate) hold the commit lock; phase
// 2, the dominant-duration snapshot write, is DELIBERATELY lock-free (see the
// package doc), so two checkpoints in flight at once — via two concurrent
// RunCheckpoint calls, or one RunCheckpoint racing the loop's own
// Trigger-driven run — can both be writing to the same snapshot directory
// simultaneously and collide with a real filesystem error. RunCheckpoint
// itself does not enforce this with a dedicated mutex: unlike the commit
// lock (necessarily shared with every writer), a checkpoint-only mutex would
// not stall commits, but it would still (a) mask caller misuse as a merely
// slow, redundant checkpoint instead of the loud failure that reveals it,
// (b) couple the [Start]ed loop's Stop responsiveness to a stranger's
// potentially multi-second RunCheckpoint call blocking on that same mutex,
// unable to observe its own stop signal meanwhile, and (c) add a
// synchronisation path with zero current callers to exercise it. The caller
// must instead ensure single-goroutine, non-overlapping use, exactly as the
// simulator already does. [Stats.LastError] remains correctly attributable
// even if this contract is violated: each attempt is sequence-numbered, and
// an earlier-started attempt's belated outcome can never overwrite a
// later-started attempt's already-recorded one.
func (c *Checkpointer[N, W]) RunCheckpoint() error {
	defer metrics.Time("store.checkpoint.RunCheckpoint").Stop()
	// runCheckpoint -> runNonBlocking already increments the checkpoints counter
	// and records LastError on every completed run, so the Stats surface is
	// identical to a loop/Trigger-driven checkpoint; we only add the call-site
	// error metric.
	err := c.runCheckpoint()
	if err != nil {
		metrics.IncCounter("store.checkpoint.RunCheckpoint.errors", 1)
	}
	return err
}

// runUnderCommitLock runs fn under the store's commit serialisation: the
// store's PRIVATE commit semaphore when a serialiser is wired
// (WithCommitSerialiser, the engine path — it also drains in-flight group
// commits so the call is a true quiesce boundary), or the raw storeMu
// otherwise (correct only when the caller serialises its own writes under
// that same mutex). It is invoked TWICE per non-blocking checkpoint — once to
// take the watermark + the MVCC instant, once to truncate the WAL prefix — so the
// two brief locked windows bracket the lock-free snapshot write.
//
// # The drain is load-bearing, not an implementation detail (rmp #2310)
//
// The wired serialiser closes writer admission and drains the admitted writers to
// zero, and a writer's registration spans its whole commit. Two things depend on
// that and on nothing else: the watermark and the instant describe the same set of
// transactions, and no id is interned by a still-open transaction when the instant
// is taken (see [snapshot.ErrCaptureNotQuiesced]).
//
// The storeMu fallback does NOT drain. It is correct only for the caller it was
// written for — one that serialises its own writes under that same mutex, so there
// is no concurrent writer to drain. A caller that uses the fallback AND writes
// concurrently violates both properties; the capture detects the second and refuses,
// rather than publishing a snapshot recovery cannot load.
func (c *Checkpointer[N, W]) runUnderCommitLock(fn func() error) error {
	if c.serialise != nil {
		return c.serialise(fn)
	}
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	return fn()
}

// commitQuiesceTimeout bounds [Checkpointer.awaitCommitQuiescence].
//
// It is a FAIL-STOP bound, not a schedule. The wait it bounds is for transactions
// that are already past their fsync and owe only in-memory work, with admission
// closed so none can be added — microseconds in practice, and this is four orders
// of magnitude above that so a loaded or coverage-instrumented run never trips it.
// Reaching it means a commit timestamp was allocated and never discharged, which is
// the permanent frontier stall [lpg.MVCCStats.InFlightCommits] exists to report:
// every new reader is already stuck. Failing the checkpoint is then the safe
// outcome, because nothing is truncated and the WAL keeps every record.
const commitQuiesceTimeout = 30 * time.Second

// awaitCommitQuiescence blocks until no transaction is between its WAL fsync and its
// MVCC publish, so the durable offset and the visible instant taken next describe the
// same set of transactions (rmp #2349).
//
// The caller must already hold the commit serialisation with admission closed; see
// the invariant note inside [Checkpointer.runNonBlocking] phase 1 for why that is
// what makes the wait terminate, and for the reference engines.
func (c *Checkpointer[N, W]) awaitCommitQuiescence() error {
	ctx, cancel := context.WithTimeout(context.Background(), commitQuiesceTimeout)
	defer cancel()
	if err := c.g.AwaitCommitQuiescence(ctx); err != nil {
		metrics.IncCounter("store.checkpoint.quiesce.timeouts", 1)
		return fmt.Errorf("checkpoint: %d commit(s) allocated an instant and never published "+
			"it within %s, so the durable WAL prefix and the visible graph image cannot be "+
			"shown to describe the same transactions; refusing to truncate: %w",
			c.g.MVCCStats().InFlightCommits, commitQuiesceTimeout, err)
	}
	return nil
}

// runNonBlocking performs a non-blocking, LSN-watermarked checkpoint in three
// phases so writers stall only for the watermark+CSR capture, never for the
// (potentially multi-second) snapshot disk I/O:
//
//	Phase 1 (under the commit lock — the quiesce boundary): take TWO O(1)
//	  readings, and nothing else that scales with the graph. The WAL durable
//	  offset W, which equals the byte length of every frame committed so far (on a
//	  frame boundary); and an MVCC instant, which is the moment the image
//	  describes. The drain the commit lock performs is what makes those two
//	  readings name the same transaction boundary, and it is also the
//	  precondition the capture requires (see [snapshot.ErrCaptureNotQuiesced]).
//	  The constraint and index-definition sets are read here too. Then the lock
//	  is released.
//	Phase 1b (lock-free): serialise the ENTIRE graph image — adjacency plus
//	  mapper, labels, properties, tombstones, edge handles and index payloads —
//	  at that instant, so every component reflects the same transaction boundary
//	  (rmp #2269) while transactions commit throughout (rmp #2310). The instant is
//	  released as soon as the bytes exist, before any disk I/O.
//	Phase 2 (lock-free): write and publish the self-sufficient snapshot from the
//	  captured bytes — touching no graph — while concurrent transactions commit
//	  and append frames PAST W. fsync the WAL so the suffix [W,end) is durable.
//	Phase 3 (under the commit lock again, briefly): re-verify self-sufficiency
//	  (a constraint DDL may have committed in phase 2) and prefix-truncate the
//	  WAL up to W — discarding ONLY the frames the snapshot folded and
//	  preserving every frame committed during phase 2.
//
// Crash safety (certified by storage-engine-auditor, #1508): the snapshot is
// self-sufficient and recovery replays the WHOLE surviving WAL idempotently on
// top of it, so a crash at any interleaving reconstructs the exact committed
// state — see [wal.Writer.TruncatePrefix] for the atomic-rename argument and
// the per-interleaving crashpoints.
func (c *Checkpointer[N, W]) runNonBlocking() error {
	// One sequence number per attempt (rmp #1873), minted before any work
	// starts so it orders attempts by START time; threaded through every
	// setErr call this attempt makes, however it exits.
	seq := c.errSeq.Add(1)
	// --- Phase 1: capture watermark + the WHOLE graph image under the
	// quiesce boundary. ---
	var (
		capt      *snapshot.Capture[W]
		watermark int64
		// at is the MVCC instant the image is read at, opened under the commit lock
		// together with the watermark so the two describe the same transaction
		// boundary, and released once the serialisation has finished (rmp #2310).
		at *lpg.Snapshot
	)
	var constraints []snapshot.ConstraintSpec
	var indexDefs []snapshot.IndexDefSpec
	if err := c.runUnderCommitLock(func() error {
		// WAIT OUT THE DURABLE-BUT-UNPUBLISHED WINDOW (rmp #2349). This must happen
		// before either reading below, and the "Why the two agree" note on the instant
		// says what it is for and what it cost when it was missing.
		if qerr := c.awaitCommitQuiescence(); qerr != nil {
			return qerr
		}
		// WAL-health gate (rmp #1919). A concurrent schema DDL (CREATE/DROP
		// CONSTRAINT or INDEX) whose WAL commit failed at fsync poisons the
		// writer and discards its frame (DurableOffset excludes it), but the
		// engine's in-memory registry still reflects the attempted change until
		// the DDL's compensator (cypher unwindConstraintRegistration /
		// rewindConstraintDrop, or the inline forgetIndexDef) runs — and that
		// compensator runs OUTSIDE the store's writer admission, so it can lag
		// this capture. Folding constraintsFn() / indexDefsFn() in that window
		// would persist a non-acknowledged schema change into constraints.bin /
		// indexdefs.bin and enforce it (CREATE) or apply it (DROP) after a clean
		// restart — an Atomicity violation for a DDL the client saw fail.
		//
		// This capture runs only after RunUnderCommitLock drains in-flight
		// commits to zero, and the DDL's poison is applied inside SyncGroup
		// BEFORE its in-flight token is released (store/txn markInflight is
		// visible to the drain, doneInflight fires only after SyncGroup
		// returns), so a writer poisoned HERE is precisely that transient
		// window. Abort before capturing the registry or publishing anything.
		// Detecting the poison now — not at the phase-2 wlog.Sync(), which
		// fires only AFTER writeSnapshot has already published the transient
		// component — is what closes the window for BOTH the constraint and the
		// index DDL paths.
		//
		// # Re-justified without the exclusion premise (rmp #2310)
		//
		// This used to read "the poison is sticky, so if the writer is healthy here it
		// stays healthy through this capture (no concurrent commit can run while the
		// commit lock is held)". The parenthesis is no longer true: the serialisation
		// now runs OUTSIDE this lock and transactions commit throughout it.
		//
		// The gate is still sound, and for a reason that never depended on exclusion.
		// What it protects is the SCHEMA REGISTRY read below — constraintsFn and
		// indexDefsFn — against a DDL whose WAL commit failed but whose compensator has
		// not yet run. Both of those reads happen HERE, inside this same locked window,
		// so what the gate must cover is this window and not the capture. A DDL that
		// poisons the writer AFTER this point cannot have been folded into the
		// constraint or index sets already read, and it is caught before anything is
		// published by the pre-truncate wlog.Sync() in phase 3.
		//
		// The GRAPH image is a separate question and needs no health gate at all: it is
		// read at an MVCC instant, and a transaction whose commit failed never publishes
		// its instant, so its versions are invisible to `at` by construction. That is
		// strictly stronger than the poison check, which can only report that SOME
		// writer failed.
		if perr := c.wlog.Poisoned(); perr != nil {
			return perr
		}
		// W is the durable WAL offset at a transaction boundary. Captured under
		// the quiesce boundary (RunUnderCommitLock drains in-flight commits), so
		// every committed frame is durable and none is mid-flight: W is exactly
		// the prefix a self-sufficient snapshot of this state folds, and the only
		// safe WAL cut point (see wal.Writer.DurableOffset / TruncatePrefix).
		watermark = c.wlog.DurableOffset()
		// The ENTIRE graph image — adjacency AND the mapper, labels, properties,
		// tombstones, edge handles and index payloads — is captured at ONE instant, so
		// every component reflects the same transaction boundary (rmp #2269). Until
		// rmp #2310 that instant was enforced by EXCLUSION and the whole serialisation
		// ran here, under the lock; it is now enforced by the MVCC snapshot opened
		// below and the serialisation runs outside the lock.
		//
		// Capturing only the CSR here and handing the LIVE graph to the phase-2
		// writer is what made a checkpoint publish a PARTIAL TRANSACTION: the
		// writer walked the mapper while transactions committed concurrently,
		// so the snapshot carried the endpoints of a `CREATE (a)-[:R]->(b)` that
		// committed during the write but not its edge, and a snapshot-only
		// recovery reconstructed Order > 2*Size — a state no serial schedule
		// could produce. Worse, the engine applies eagerly under visMu and
		// commits to the WAL afterwards, so a lock-free walk could also capture
		// the writes of a transaction whose commit had not yet succeeded and
		// might still be undone.
		//
		// The capture is bytes, not a graph, so phase 2 stays lock-free: what
		// moves under the lock is the in-memory serialisation, never the disk
		// I/O. The component writers take only their own per-shard read locks
		// and never re-enter the barrier, so this cannot deadlock or trip
		// View's re-entrancy guard.
		// THE INSTANT IS OPENED HERE, PAIRED WITH THE WATERMARK (rmp #2310).
		//
		// This is ALL the commit lock still does for the image: it makes the durable
		// offset above and the MVCC instant below describe the SAME transaction
		// boundary. Both are O(1) reads, so the exclusion window no longer scales with
		// the graph — which is the whole point of the task. The serialisation itself
		// happens after the lock is released, at this instant, while writers commit.
		//
		// # Why the two agree — and the argument that was WRONG (rmp #2349)
		//
		// The watermark is a DURABILITY position and the instant is a VISIBILITY
		// position, and they are not the same clock. The dangerous direction is a
		// transaction that is durable below the watermark but whose commit instant is
		// not yet published: `at` would not see it, the image would not carry it, and
		// phase 3 would truncate away the only record of it. An acknowledged commit
		// would be lost — the WAL prefix cut in half that AC3 names.
		//
		// This used to argue the window could not exist, because the commit serialiser
		// closes admission and drains admitted writers to zero and "a writer's
		// registration spans its WHOLE commit — store/txn.Tx.Commit defers exitWriter
		// past ApplyVersioned, which is what publishes the instant".
		//
		// THAT PREMISE WAS CHECKED ON THE WRONG PATH, and the loss was real. The Cypher
		// engine — the only production writer — does not use Tx.Commit. It uses
		// [txn.Tx.CommitWALOnly], which performs no in-memory apply and publishes no
		// instant, so its own `defer exitWriter` fires the moment the fsync returns
		// while the instant is published later, when the lpg write bracket unwinds
		// through lpg.Graph.endWrite. cypher/api.go commitUnderBarrier names that state
		// outright: "DURABLE, BUT NOT YET VISIBLE". Between the two the store's
		// in-flight count is ZERO and the frame is already inside the durable offset,
		// so the drain completed and both readings were taken inside the window.
		// internal/sim ST3 lost exactly one acknowledged commit twice in fifteen
		// coverage runs under parallel fsync load before this was found.
		//
		// SO THE WINDOW IS WAITED OUT INSTEAD OF ARGUED AWAY, at the top of this
		// closure: [lpg.Graph.AwaitCommitQuiescence] blocks until every allocated
		// commit instant has been published or abandoned. Admission is already closed
		// and the admitted writers drained, so no new instant can be allocated while it
		// waits and the ones outstanding are past their fsync with only in-memory work
		// left — which is what bounds it. After it returns, every durable frame belongs
		// to a transaction visible at `at`, by construction rather than by assumption.
		//
		// The wait is on the OBSERVER and the commit path pays nothing, which is
		// PostgreSQL's arrangement for the identical problem: a backend raises
		// DELAY_CHKPT_START around the gap between its commit record and its pg_xact
		// update, and CreateCheckPoint spins until that set is empty rather than
		// lengthening the commit — "it seems better to make checkpoint take a bit
		// longer than to hold off insertions longer than necessary"
		// (src/backend/access/transam/xlog.c:7683-7684, commit b5978350). See
		// [mvcc.Clock.AwaitQuiescent] for the full citation and for why Memgraph's
		// opposite arrangement was not adopted.
		//
		// The seam below still asserts the result rather than trusting it:
		// TestCheckpoint_WatermarkAndInstantDescribeTheSameBoundary and
		// TestCheckpoint_EngineCommitOrdering_KeepsAnAckedCommit both check that no
		// commit is in flight here, the second with a transaction deliberately parked
		// in the window so the reading is a fact about the ordering.
		//
		// The snapshot is released by the deferred EndRead below, on every path.
		at = c.g.BeginRead()
		if c.afterWatermarkHook != nil {
			c.afterWatermarkHook()
		}
		// Capture the constraint set inside the same locked window so it is
		// transaction-boundary consistent with the CSR (see WithConstraintSpecs).
		if c.constraintsFn != nil {
			constraints = c.constraintsFn()
		}
		// Capture the index-definition set in the same locked window, for the
		// same transaction-boundary consistency (see WithIndexSpecs).
		if c.indexDefsFn != nil {
			indexDefs = c.indexDefsFn()
		}
		return nil
	}); err != nil {
		c.setErr(seq, err)
		return err
	}

	// --- Phase 1b: SERIALISE THE IMAGE, LOCK-FREE, AT THE CAPTURED INSTANT ---
	//
	// This is the work that used to hold every writer out, and it is proportional to
	// the graph. It now runs with the commit lock released: every component below
	// resolves through `at`, so the image is the graph as of ONE transactional instant
	// however many transactions commit while it runs.
	//
	// That is what Memgraph's CreateSnapshot does — it runs under a transaction and
	// reads each vertex through storage::View::OLD rather than stopping writers
	// (src/storage/v2/inmemory/storage.cpp CreateSnapshot;
	// src/storage/v2/durability/snapshot.cpp) — and it is the same property PostgreSQL
	// obtains by replaying WAL forward to a consistent point instead of quiescing.
	//
	// # The horizon cost, and its bound
	//
	// A held snapshot pins reclamation: for as long as `at` is open the vacuum cannot
	// free a version newer than it, so version memory grows by whatever the concurrent
	// writers produce in this window. The bound is therefore THIS SERIALISATION's
	// duration and nothing else — it is O(V+E) in memory with no disk I/O, and the
	// snapshot is released before phase 2, which is the multi-second part. The
	// checkpoint never holds the horizon across its own disk writes.
	//
	// MVCCStats.OldestSnapshotAge reports it if a capture ever does stall, and
	// TestCheckpoint_ReleasesTheHorizonBeforeWritingTheSnapshot pins the release
	// point so a future refactor cannot quietly extend it over phase 2.
	{
		// live is resolved AT THE SAME INSTANT as the arcs, so the vertex set and the
		// edge set cannot describe different states: a node removed during the capture
		// is still live here, because its arcs as of `at` are in the image.
		// The vertex ARRAY is sized from the present id space, and that is correct: node
		// ids are packed as (intra << shardBits) | shard, so the space is sparse and an
		// id-indexed array must span it whatever instant is being read. Extra slots are
		// empty and name no node — membership comes from the mapper, which IS filtered at
		// the instant, and from live below.
		cs := csr.BuildFromAdjListAsOf(c.g.AdjList(),
			func(id graph.NodeID) bool { return c.g.NodeExistsAsOf(id, at) },
			at.StartTS(), at.TxID())
		var capErr error
		capt, capErr = c.snap.CaptureGraph(cs, c.g, c.codec, at)

		// Released as soon as the bytes exist, and BEFORE phase 2's disk I/O: holding
		// it longer would pin the reclamation horizon for the whole snapshot write.
		c.g.EndRead(at)
		if capErr != nil {
			c.setErr(seq, capErr)
			return capErr
		}
	}

	// Test-only seam: fires at the start of phase 2, with the commit lock
	// released, so a white-box test can prove writers are not blocked during
	// the lock-free snapshot write. Nil in production.
	if c.afterCaptureHook != nil {
		c.afterCaptureHook()
	}

	// --- Phase 2: write + publish the snapshot LOCK-FREE, then prefix-truncate. ---
	return c.writeAndTruncate(seq, capt, constraints, indexDefs, watermark)
}

// writeAndTruncate is phases 2 and 3 of the non-blocking checkpoint: it writes
// the self-sufficient snapshot from the captured image (lock-free, so writers
// commit concurrently), then re-acquires the commit lock to prefix-truncate the
// WAL up to the captured watermark. capt, constraints, and indexDefs are the
// phase-1 capture; watermark is the durable WAL offset W those reflect. seq is
// this attempt's setErr sequence number (rmp #1873), minted once by the
// calling runNonBlocking and threaded through unchanged.
//
// It writes ONLY captured bytes — it never reads the graph — so nothing it
// publishes can reflect a later state than the phase-1 boundary (rmp #2269).
func (c *Checkpointer[N, W]) writeAndTruncate(seq uint64, capt *snapshot.Capture[W], constraints []snapshot.ConstraintSpec, indexDefs []snapshot.IndexDefSpec, watermark int64) error {
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
	// on the next restart (#1334). Identically for secondary indexes: when the
	// engine has indexes declared (WithIndexSpecs), the snapshot must carry
	// their definitions in indexdefs.bin BEFORE the WAL prefix that first
	// declared them is truncated, or every index silently vanishes on the next
	// restart (#1755).
	// Phase 2 disk I/O runs WITHOUT the commit lock: writers commit
	// concurrently and append frames past the captured watermark, paying no
	// stall for this (potentially multi-second) write. The snapshot is a
	// self-sufficient image of the phase-1 boundary state.
	if err := c.writeSnapshot(snapDir, capt, constraints, indexDefs); err != nil {
		c.setErr(seq, err)
		return err
	}
	// fsync the WAL so the suffix [watermark, end) — the frames committed
	// concurrently during the snapshot write — is durable before we touch the
	// prefix. Snapshot durable (writeSnapshot publishes with its own fsync +
	// parent-dir fsync) THEN suffix durable THEN truncate the prefix: the
	// ordering the auditor required (#1508 Q5).
	if err := c.wlog.Sync(); err != nil {
		c.setErr(seq, err)
		return err
	}
	// Crash-injection point: the new self-sufficient snapshot is published and
	// durable, the FULL WAL (folded prefix [0,W) + concurrently-committed
	// suffix [W,end)) is intact, and NO truncation has happened. A crash here
	// must recover the exact committed state — recovery loads the new snapshot
	// and idempotently replays the WHOLE WAL (prefix re-folded harmlessly,
	// suffix applied on top). This is the non-blocking analogue of
	// "post-snapshot-pre-truncate", now with a non-empty concurrent suffix.
	// No-op in production (GOGRAPH_CRASH_AT unset).
	crashpoint.Breakpoint("checkpoint.p2-snapshot-published-pre-truncate")

	// --- Phase 3: prefix-truncate the WAL under the commit lock, briefly. ---
	return c.runUnderCommitLock(func() error {
		return c.truncatePrefixLocked(seq, snapDir, watermark)
	})
}

// truncatePrefixLocked is phase 3 of the non-blocking checkpoint: it runs
// under the store's commit lock (the quiesce boundary) so no concurrent commit
// races the WAL prefix truncation. It re-verifies snapshot self-sufficiency —
// a constraint or index DDL may have committed during the lock-free phase-2
// write, in which case the snapshot (captured at the watermark, before the DDL)
// cannot stand alone and the WAL must be retained — then discards only the WAL
// bytes in [0, watermark), preserving every frame committed during phase 2.
// seq is this attempt's setErr sequence number (rmp #1873), threaded
// unchanged from runNonBlocking via writeAndTruncate.
func (c *Checkpointer[N, W]) truncatePrefixLocked(seq uint64, snapDir string, watermark int64) error {
	// Re-source needConstraints / needIndexes from the graph's own counts, NOT
	// from the phase-1 captured slices: a constraint or index DDL committed
	// during the lock-free phase-2 write makes HasConstraints / HasIndexes true
	// while the snapshot (captured before the DDL) carries no constraints.bin /
	// indexdefs.bin for it, so the snapshot is correctly judged not
	// self-sufficient and the WAL prefix holding that DDL is retained (#1334 /
	// #1464 / #1755 fail-safe, re-checked under the phase-3 lock per the #1508
	// audit condition C2).
	selfSufficient, err := c.snapshotIsSelfSufficient(snapDir, c.g.HasConstraints(), c.g.HasIndexes())
	if err != nil {
		c.setErr(seq, err)
		return err
	}
	if !selfSufficient {
		// The snapshot cannot reconstruct the graph on its own (no mapper.bin
		// for this key type, a constraint set that did not land in
		// constraints.bin, or an index set that did not land in indexdefs.bin),
		// so truncating the WAL would lose committed data.
		// Skip truncation: the WAL is retained and replayed on top of the
		// snapshot at recovery, preserving Durability at the cost of unbounded
		// WAL growth. Surfaced via a metric so operators can detect the mode.
		metrics.IncCounter("store.checkpoint.truncate_skipped_not_self_sufficient", 1)
		c.checkpoints.Add(1)
		c.setErr(seq, nil)
		return nil
	}
	// Discard ONLY the folded prefix [0, watermark); the suffix [watermark,
	// end) holds transactions committed during phase 2 and is preserved (a
	// truncate-to-zero would lose them — the exact bug WithCommitSerialiser
	// was created to prevent). TruncatePrefix is itself crash-safe via an
	// atomic copy-suffix-then-rename.
	truncated, err := c.wlog.TruncatePrefix(watermark)
	if err != nil {
		c.setErr(seq, err)
		return err
	}
	if truncated > 0 {
		c.walTrunc.Add(uint64(truncated))
		// Surface the bytes reclaimed through the metrics backend so operators
		// monitoring long-running stores can plot WAL-prefix reclamation cadence
		// without polling Stats(). The atomic lifetime counter [c.walTrunc]
		// remains the test-friendly in-process aggregate; this is the
		// observability surface.
		metrics.IncCounter("store.checkpoint.wal_truncated_bytes", uint64(truncated))
	}
	c.checkpoints.Add(1)
	c.setErr(seq, nil)
	return nil
}

// writeSnapshot publishes the phase-1 capture to snapDir. Whether mapper.bin
// is present was decided at capture time by the configured mapper codec
// ([WithMapperCodec]): with a codec it is emitted for every key type, without
// one only for string keys (v2 for other key types). constraints is the schema
// constraint set collected via [WithConstraintSpecs] in the same phase-1 locked
// window; when non-empty it is persisted as the snapshot's constraints.bin
// component (nil or empty emits no constraints.bin, and the output is
// byte-identical to the constraint-unaware writers).
func (c *Checkpointer[N, W]) writeSnapshot(snapDir string, capt *snapshot.Capture[W], constraints []snapshot.ConstraintSpec, indexDefs []snapshot.IndexDefSpec) error {
	return c.snap.WriteCapture(snapDir, capt, constraints, indexDefs)
}

// setErr records err as the outcome of the checkpoint attempt identified by
// seq (rmp #1873), rejecting the write if a later-STARTED attempt already
// recorded its own outcome: seq < c.lastErrSeq means some other, more
// recently started run's setErr call already landed first, so this call is
// stale and must not overwrite it (e.g. a same-outcome or opposite-outcome
// run that started after this one but happened to finish first).
//
// The comparison is seq >= c.lastErrSeq, not seq > c.lastErrSeq: a SINGLE
// attempt's own three-phase run (runNonBlocking -> writeAndTruncate ->
// truncatePrefixLocked) calls setErr exactly once on its way out, always
// with its own unchanging seq, so that one call must always win against
// whatever was recorded before it — rejecting an equal seq would make an
// attempt unable to record its own outcome at all. In this codebase's
// current call graph seq is unique per attempt (minted once by
// runNonBlocking, never reused), so the equal-seq branch is only ever taken
// by an attempt overwriting its OWN prior state, never two attempts
// colliding on the same value.
//
// Within a single, non-concurrent series of attempts (the documented,
// supported usage) each new attempt's seq is strictly greater than the
// last, so every call always wins and this guard changes nothing versus a
// plain unconditional overwrite; it only changes behaviour for the specific
// out-of-order-completion scenario described above, which the documented
// contract on [Checkpointer.RunCheckpoint] forbids callers from creating in
// the first place — this is defence in depth, not a sanctioned usage mode.
func (c *Checkpointer[N, W]) setErr(seq uint64, err error) {
	c.lastErrMu.Lock()
	if seq >= c.lastErrSeq {
		c.lastErrSeq = seq
		if err == nil {
			c.lastErr = ""
		} else {
			c.lastErr = err.Error()
		}
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
// needIndexes is the analogue for secondary indexes: an index DEFINITION
// lives only in the OpCreateIndex WAL frame and in the snapshot's
// [snapshot.IndexDefsFile] component — never reconstructible from the CSR
// alone — so when the graph has any index declared, a snapshot lacking
// indexdefs.bin cannot stand alone; truncating after it would silently
// drop every index on the next restart (#1755).
//
// Detection is by manifest content, not version number, so it stays
// correct if the version scheme evolves: the snapshot is self-sufficient
// iff its manifest lists [snapshot.MapperFile] (and
// [snapshot.ConstraintsFile] when needConstraints is set, and
// [snapshot.IndexDefsFile] when needIndexes is set).
func (c *Checkpointer[N, W]) snapshotIsSelfSufficient(dir string, needConstraints, needIndexes bool) (bool, error) {
	m, err := c.snap.ReadManifest(manifestPath(dir))
	if err != nil {
		return false, fmt.Errorf("checkpoint: read snapshot manifest: %w", err)
	}
	var hasMapper, hasConstraints, hasIndexDefs bool
	for _, f := range m.Files {
		switch f.Name {
		case snapshot.MapperFile:
			hasMapper = true
		case snapshot.ConstraintsFile:
			hasConstraints = true
		case snapshot.IndexDefsFile:
			hasIndexDefs = true
		}
	}
	if !hasMapper {
		return false, nil
	}
	if needConstraints && !hasConstraints {
		return false, nil
	}
	if needIndexes && !hasIndexDefs {
		return false, nil
	}
	return true, nil
}
