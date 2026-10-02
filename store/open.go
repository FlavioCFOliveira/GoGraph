package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// walFileName is the WAL's file name inside a store directory. It is the name
// [recovery.Open] replays, so [Open] appends to exactly the file it recovered.
const walFileName = "wal"

// ErrUncleanRecovery is returned by [Open] and [OpenCtx], wrapped in an
// [*UncleanRecoveryError], when recovery of the directory did not complete
// cleanly ([recovery.Result.IsClean] reported false). Test for it with
// errors.Is.
var ErrUncleanRecovery = errors.New("store: recovery did not complete cleanly; refusing to append to the WAL")

// UncleanRecoveryError reports that [Open] refused a directory whose recovery
// did not complete cleanly. It wraps both [ErrUncleanRecovery] and the
// recovery's [recovery.Result.TailErr], so errors.Is matches the refusal and
// the specific corruption sentinel (for example [wal.ErrCRCMismatch] or
// [recovery.ErrCommittedTxnCorruptOp]).
//
// Result is the recovery that was refused. Its graph holds the committed
// prefix and is provided for diagnostics only; no WAL writer was opened, so
// nothing was appended and the directory is byte-for-byte as recovery left it.
//
// Concurrency: an UncleanRecoveryError is read-only once returned and safe for
// concurrent reads; Result.Graph follows the [lpg.Graph] contract.
type UncleanRecoveryError[N comparable, W any] struct {
	// Dir is the directory that was refused.
	Dir string
	// TailErr is the recovery's [recovery.Result.TailErr].
	TailErr error
	// Result is the refused recovery, for diagnostics.
	Result recovery.Result[N, W]
}

// Error implements error.
func (e *UncleanRecoveryError[N, W]) Error() string {
	return fmt.Sprintf("store: open %q: %v: %v", e.Dir, ErrUncleanRecovery, e.TailErr)
}

// Unwrap returns [ErrUncleanRecovery] and the recovery's TailErr, so errors.Is
// matches either.
func (e *UncleanRecoveryError[N, W]) Unwrap() []error {
	return []error{ErrUncleanRecovery, e.TailErr}
}

// Options configures [Open] and [OpenCtx]. It is a plain configuration value:
// populate it before the call and do not mutate it while the call is in
// flight.
type Options[N comparable, W any] struct {
	// Codec serialises node identifiers. It must not be nil, and it must be the
	// codec the directory was written with: it is used both to replay the WAL
	// and to encode every later append, so recovery and the store cannot
	// disagree.
	Codec txn.Codec[N]
	// WeightCodec serialises edge weights. It must mirror the producer that
	// wrote the directory: non-nil for a weight-codec store, nil for a
	// codec-only store. It is used for both replay and appends.
	WeightCodec txn.WeightCodec[W]
	// ReplayMaxTxnOps bounds the ops recovery buffers for one transaction
	// ([recovery.Options.MaxTxnOps]): 0 selects [txn.DefaultMaxTxnOps],
	// [txn.MaxTxnOpsUnlimited] disables the bound, any other positive value is
	// the bound verbatim.
	ReplayMaxTxnOps int
	// MaxTxnOps is the producer's per-transaction op cap, in the same
	// convention. It is clamped down to the replay bound by
	// [recovery.Result.NewStoreCapped], so the reopened store can never commit
	// a transaction its own next recovery would refuse.
	MaxTxnOps int
	// ResumeTxnSeq is an optional caller floor for the transaction sequence.
	// It is ratcheted with the recovered [recovery.Result.MaxTxnSeq] and never
	// lowers it; leave it 0 unless a floor is required for another reason.
	ResumeTxnSeq uint64
	// CloseOptions are applied to the returned [DB] after the options [Open]
	// always applies ([WithQuiesce] bound to the store's commit lock).
	CloseOptions []Option
}

// Opened is a durable store directory opened for reading and writing by [Open]
// or [OpenCtx]: the recovered graph, the transactional store bound to the WAL,
// and the recovery result, assembled in one step so no recovered piece can be
// dropped by omission.
//
// It embeds the composed teardown owner [*DB], so [DB.Close] and [DB.CloseCtx]
// close it in the crash-safe order, and the store's commit lock is already
// wired as the quiesce ([WithQuiesce]), so a Close concurrent with an
// in-flight commit lets that commit finish first.
//
// Concurrency: Opened is safe for concurrent use. Its accessors return values
// fixed at open and may be called from any goroutine at any time. The returned
// [txn.Store] and [lpg.Graph] carry their own contracts (both are safe for
// concurrent use as documented on those types). Close is safe from any number
// of goroutines, as documented on [DB].
type Opened[N comparable, W any] struct {
	*DB
	store  *txn.Store[N, W]
	wlog   *wal.Writer
	result recovery.Result[N, W]
}

// Store returns the transactional store bound to the WAL. Its transaction
// sequence resumes from the recovered maximum, its op cap is clamped to the
// replay bound, and it carries the codecs the directory was recovered with.
func (o *Opened[N, W]) Store() *txn.Store[N, W] { return o.store }

// Graph returns the recovered in-memory graph the store writes to. It is the
// same graph as Store().Graph() and Recovery().Graph.
func (o *Opened[N, W]) Graph() *lpg.Graph[N, W] { return o.store.Graph() }

// Recovery returns the recovery result this store was built from: the
// recovered schema ([recovery.Result.Constraints], [recovery.Result.Indexes]),
// the index payloads, the derived counters, and the replay diagnostics. Hand
// it whole to the query layer (for the string/float64 Cypher engine,
// cypher.NewEngineWithOpened) so the schema is re-registered.
func (o *Opened[N, W]) Recovery() recovery.Result[N, W] { return o.result }

// WAL returns the WAL writer the store appends to. It is exposed so a
// background checkpointer can be wired to it. Do not close it directly: the
// embedded [DB] owns it, and closing it out of order is the defect [DB.Close]
// exists to prevent.
func (o *Opened[N, W]) WAL() *wal.Writer { return o.wlog }

// Open is [OpenCtx] with a background context.
func Open[N comparable, W any](dir string, opts Options[N, W]) (*Opened[N, W], error) {
	return OpenCtx(context.Background(), dir, opts)
}

// OpenCtx opens the durable store in dir for reading and writing. It is the
// composed counterpart of [DB.Close]: the one correct reopen sequence, so an
// embedder no longer hand-writes it and cannot drop recovered state.
//
// It runs, in order:
//
//  1. Recovery ([recovery.OpenCtx]) over dir: the snapshot, if any, then the
//     WAL tail at dir/wal. A recovery error is returned wrapped, and nothing is
//     opened for writing.
//  2. The clean gate. When [recovery.Result.IsClean] is false the open is
//     refused with an [*UncleanRecoveryError] wrapping [ErrUncleanRecovery]
//     and the recovery's TailErr, and no WAL writer is opened. The check is on
//     IsClean, not on the recovery error alone: one not-clean outcome
//     ([recovery.ErrCommittedTxnCorruptOp]) returns a nil error from recovery.
//  3. The WAL is opened for append ([wal.Open]), which takes the directory's
//     WAL lock and truncates a benign torn tail.
//  4. The transactional store is built from the recovery result
//     ([recovery.Result.NewStoreCapped]), carrying the recovered graph (and
//     with it the graph configuration, the restored MVCC clock, labels,
//     properties, tombstones and edge handles), the transaction-sequence
//     floor ([recovery.Result.MaxTxnSeq], ratcheted with
//     [Options.ResumeTxnSeq]), the producer op cap clamped to the replay
//     bound ([recovery.Result.MaxTxnOps]), and the codecs recovery used.
//  5. The [DB] teardown owner is assembled over the WAL, with the store's
//     commit lock wired as the quiesce, followed by [Options.CloseOptions].
//
// The recovered schema and index payloads live in [Opened.Recovery]; the query
// layer consumes them from there (cypher.NewEngineWithOpened).
//
// When any step after the WAL open fails, the WAL is closed before OpenCtx
// returns, so a failed open leaks no file handle and no lock.
//
// # Why an unclean directory is always refused
//
// Appending to a WAL that did not replay cleanly does not repair it: every
// later recovery stops at the same damaged frame, so each commit appended
// after it is acknowledged and then discarded at the next open. Refusing the
// open keeps those commits from being acknowledged at all. A caller
// that needs the committed prefix of such a directory reads it from
// [UncleanRecoveryError.Result], or recovers it with [recovery.Open], without
// opening the WAL for append.
//
// # Prior art
//
// The shape follows two reference engines' single open entry point. bbolt
// (go.etcd.io/bbolt, db.go, func Open) takes the file lock, reads or
// initialises the meta pages, maps the file, and on every failure path calls
// db.close() before returning, so a failed open releases what it took. Badger
// (github.com/dgraph-io/badger, db.go, func Open) acquires the directory
// lock, replays the manifest, levels and value log, and seeds its
// transaction-timestamp oracle from the replayed maximum
// (orc.nextTxnTs = db.MaxVersion(), then incrementNextTs) inside Open, rather
// than returning the maximum for the caller to apply. OpenCtx takes the same
// two decisions: the recovered counters are applied inside the open, and a
// failed open unwinds what it opened.
//
// # Concurrency
//
// OpenCtx is a one-shot bootstrap step. It must not run concurrently with
// another OpenCtx, [recovery.Open], or a writer on the same directory: the WAL
// lock is taken at step 3, after recovery has read (and, for an interrupted
// snapshot publish, repaired) the directory. A second process that reaches
// step 3 while the first holds the lock fails with [wal.ErrWALLocked]. Opens
// of distinct directories are independent and may run concurrently.
//
// ctx bounds recovery (step 1); it is checked by recovery at the snapshot
// boundary and periodically during WAL replay.
func OpenCtx[N comparable, W any](ctx context.Context, dir string, opts Options[N, W]) (*Opened[N, W], error) {
	defer metrics.Time("store.Open").Stop()
	o, err := openCtx(ctx, dir, opts)
	if err != nil {
		metrics.IncCounter("store.Open.errors", 1)
	}
	return o, err
}

// openCtx is the body of [OpenCtx], separated so the metrics wrapper counts
// every error path once.
func openCtx[N comparable, W any](ctx context.Context, dir string, opts Options[N, W]) (*Opened[N, W], error) {
	if dir == "" {
		return nil, errors.New("store: open: empty directory")
	}
	if opts.Codec == nil {
		return nil, fmt.Errorf("store: open %q: nil codec", dir)
	}
	res, err := recovery.OpenCtx(ctx, dir, recovery.Options[N, W]{
		Codec:       opts.Codec,
		WeightCodec: opts.WeightCodec,
		MaxTxnOps:   opts.ReplayMaxTxnOps,
	})
	if err != nil {
		if !res.IsClean() {
			return nil, &UncleanRecoveryError[N, W]{Dir: dir, TailErr: err, Result: res}
		}
		return nil, fmt.Errorf("store: open %q: recover: %w", dir, err)
	}
	if !res.IsClean() {
		return nil, &UncleanRecoveryError[N, W]{Dir: dir, TailErr: res.TailErr, Result: res}
	}
	wlog, err := wal.Open(filepath.Join(dir, walFileName))
	if err != nil {
		return nil, fmt.Errorf("store: open %q: wal: %w", dir, err)
	}
	st := res.NewStoreCapped(wlog, txn.Options[N, W]{
		Codec:        opts.Codec,
		WeightCodec:  opts.WeightCodec,
		ResumeTxnSeq: opts.ResumeTxnSeq,
	}, opts.MaxTxnOps)
	closeOpts := make([]Option, 0, 1+len(opts.CloseOptions))
	closeOpts = append(closeOpts, WithQuiesce(st.RunUnderCommitLock))
	closeOpts = append(closeOpts, opts.CloseOptions...)
	return &Opened[N, W]{
		DB:     New(wlog, closeOpts...),
		store:  st,
		wlog:   wlog,
		result: res,
	}, nil
}
