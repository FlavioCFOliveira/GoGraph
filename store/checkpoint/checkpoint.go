// Package checkpoint runs a background goroutine that periodically
// folds the WAL tail into a fresh snapshot and truncates the WAL.
// Without this the WAL would grow unbounded during steady-state
// operation.
//
// The checkpointer is non-blocking for writers: it takes the
// snapshot under the same store mutex used by [store/txn.Tx], holds
// it only for the brief moment needed to swap files, then releases
// it for the next transaction.
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

	"gograph/graph/csr"
	"gograph/graph/lpg"
	"gograph/internal/metrics"
	"gograph/store/snapshot"
	"gograph/store/wal"
)

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

	stopCh    chan struct{}
	triggerCh chan chan error
	doneCh    chan struct{}
	stopOnce  sync.Once

	checkpoints  atomic.Uint64
	walTrunc     atomic.Uint64
	lastDuration atomic.Uint64
	lastErrMu    sync.Mutex
	lastErr      string
}

// New returns a Checkpointer; call Start to launch the goroutine.
//
// storeMu must be the same mutex the transaction layer holds during
// commit; the checkpointer acquires it briefly to take a consistent
// snapshot of the graph state.
func New[N comparable, W any](
	cfg Config,
	g *lpg.Graph[N, W],
	wlog *wal.Writer,
	storeMu *sync.Mutex,
) *Checkpointer[N, W] {
	if cfg.Interval == 0 && cfg.MaxAge > 0 {
		cfg.Interval = cfg.MaxAge / 4
		if cfg.Interval < time.Millisecond {
			cfg.Interval = time.Millisecond
		}
	}
	return &Checkpointer[N, W]{
		cfg:       cfg,
		g:         g,
		wlog:      wlog,
		storeMu:   storeMu,
		stopCh:    make(chan struct{}),
		triggerCh: make(chan chan error, 4),
		doneCh:    make(chan struct{}),
	}
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
// On a saturated trigger buffer (rare, but possible if many Trigger
// calls race ahead of the loop) Trigger can block indefinitely;
// prefer TriggerCtx in production code to bound the latency.
func (c *Checkpointer[N, W]) Trigger() error {
	defer metrics.Time("store.checkpoint.Trigger")()
	err := c.TriggerCtx(context.Background())
	if err != nil {
		metrics.IncCounter("store.checkpoint.Trigger.errors", 1)
	}
	return err
}

// TriggerCtx requests a checkpoint and blocks until it completes,
// honouring ctx cancellation on every wait edge: queue-submit,
// in-flight, and stop-signal. Returns ctx.Err() wrapped on
// cancellation or deadline expiry.
func (c *Checkpointer[N, W]) TriggerCtx(ctx context.Context) error {
	defer metrics.Time("store.checkpoint.TriggerCtx")()
	done := make(chan error, 1)
	select {
	case c.triggerCh <- done:
		// Submitted; now wait for the result.
	case <-c.stopCh:
		metrics.IncCounter("store.checkpoint.TriggerCtx.errors", 1)
		return errors.New("checkpoint: checkpointer stopped")
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
	defer close(c.doneCh)
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

	// Hold storeMu around the entire snapshot+truncate window so that
	// no transaction commits new WAL frames between the snapshot
	// capture and the truncation. The trade-off is that the lock is
	// held during disk I/O for the duration of the snapshot write;
	// for very large graphs this can stall writers and may be
	// reworked later to a position-tracked truncate (capture LSN
	// under lock, write snapshot lock-free, truncate up-to-LSN under
	// lock). For v1 the simple correctness-first path is preferred.
	c.storeMu.Lock()
	defer c.storeMu.Unlock()

	cs := csr.BuildFromAdjList(c.g.AdjList())
	snapDir := filepath.Join(c.cfg.Dir, "snapshot")
	if err := snapshot.WriteSnapshotCSR(snapDir, cs); err != nil {
		c.setErr(err)
		return err
	}
	if err := c.wlog.Sync(); err != nil {
		c.setErr(err)
		return err
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

func (c *Checkpointer[N, W]) setErr(err error) {
	c.lastErrMu.Lock()
	if err == nil {
		c.lastErr = ""
	} else {
		c.lastErr = err.Error()
	}
	c.lastErrMu.Unlock()
}
