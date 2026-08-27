package sim

// txn_oversize.go — transaction-size caps: producer refusal and replay
// fail-stop (rmp #2474).
//
// # The class, and what was actually reachable
//
// An unbounded transaction is the CWE-770 shape of the store: recovery buffers a
// whole transaction's ops in memory before applying them on its [txn.OpCommit]
// marker, so a producer able to commit an arbitrarily large transaction could
// write a WAL that recovery cannot replay without allocating proportionally to
// it. The store answers with TWO caps and two typed sentinels, verified in
// source rather than taken from a description of it:
//
//   - the PRODUCER cap, [txn.DefaultMaxTxnOps] = 16 000 000, enforced in
//     txn.Tx.appendOnly and refusing with [txn.ErrTransactionTooLarge];
//   - the REPLAY cap, [recovery.Options.MaxTxnOps] (same default), enforced in
//     recovery's frame loop and stopping with
//     [recovery.ErrTransactionTooLarge], which
//     recovery.tailErrIsCorruption classifies as genuine corruption.
//
// Neither sentinel had ever been produced under simulation, and the reason was
// structural rather than an oversight of workload size. The simulator's
// simStoreConfig.maxTxnOps reached RECOVERY on both cores and stopped there: the
// store itself was built with the uncapped constructor, so the replay bound was
// configurable and the commit bound was not. Lowering the cap could therefore
// never make the producer refuse, and raising the workload to 16 000 000 ops to
// reach the default is not a test, it is an out-of-memory. The overload actor
// says as much in its own comment — it pushes "toward" DefaultMaxTxnOps — and
// nothing has ever arrived. simstore.go now passes the cap to
// [txn.NewStoreWithOptionsCapped] as well, which is what makes this scenario
// possible; that change is behaviour-neutral for every other caller, all of
// which carry 0 and resolve to the same default as before.
//
// # The boundary is MEASURED, not inferred from the two comparisons
//
// The producer refuses when len(ops) > cap; recovery stops when, BEFORE
// appending another op frame, len(pending) >= cap. The two operators differ
// because the two counts are taken at different moments, and the arms here pin
// what that actually yields rather than arguing it: a transaction of exactly cap
// ops commits AND replays, and one of cap+1 ops is refused by the producer and —
// when the same op stream is written into a WAL by hand — fail-stops the
// replayer. The caps agree exactly, which is the invariant
// [txn.DefaultMaxTxnOps] documents (producer <= replay, so anything durable
// replays).
//
// # Why "an error came back" is not the assertion
//
// A refusal that first appended some frames and then truncated them is how a bug
// becomes permanent loss (rmp #2526). So the producer arm does not settle for the
// typed error: it reads the WHOLE durable WAL image off the [SimDisk] before and
// after each refused attempt and requires them BYTE-IDENTICAL, and reads the live
// graph's node count across the same boundary and requires it unchanged. The
// at-cap attempt is then driven AFTER the refusals and must commit and grow the
// file, so a refusal that had silently poisoned the writer would fail here rather
// than pass as a clean refusal.
//
// # Why the oversize WAL is built by hand
//
// The replay cap cannot be reached by driving the engine: the producer cap is
// <= the replay cap by construction, so any transaction big enough to trouble
// recovery is refused before a frame is written — that is the whole point of the
// pairing. The oversize file is therefore CONSTRUCTED, one v3 op payload at a
// time, through the real [wal.Writer] (so the framing, CRC and fsync path are
// the production ones and only the op stream is hand-made). The unlimited arm
// replays that identical byte image with the cap disabled and recovers every op,
// which is what makes the fail-stop attributable to the cap rather than to a
// file the harness simply built wrong.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// txnOversizeDiskSeedMix decorrelates this scenario's SimDisk sub-stream from
// the workload seed, matching the convention the other durable scenarios use.
const txnOversizeDiskSeedMix = 0x7A6E_4F56_5253_5A45

// The reference transaction sizes, in OPS. txnOversizeCap is the deliberately
// small producer/replay cap the capped arm opens the store with — small enough
// that "one over" is one extra buffered op rather than sixteen million, and the
// whole scenario stays microseconds long.
//
// The sizes are expressed in ops rather than in nodes because ops are what the
// cap counts. txnOversizeMakeTxn turns an op budget into a node workload
// (see there): every op is a real graph mutation, so a transaction that commits
// is one whose effect is observable afterwards.
const (
	// txnOversizeCap is the cap the capped store is opened with, and the
	// reference size the unlimited arm is compared against.
	txnOversizeCap = 32
	// txnOversizeWarmupOps sizes the transaction committed BEFORE any oversize
	// attempt, so the refusals are adjudicated against a non-empty WAL. A cap
	// test on an empty store proves nothing.
	txnOversizeWarmupOps = 8
	// txnOversizeOneOverOps is exactly one op past the cap: the minimal
	// violation, which is the interesting one for an off-by-one.
	txnOversizeOneOverOps = txnOversizeCap + 1
	// txnOversizeFarOverOps is comfortably past the cap, so a refusal cannot be
	// an artefact of the boundary arithmetic alone.
	txnOversizeFarOverOps = txnOversizeCap * 4
)

// txnOversizeLabel is the label every node the scenario creates carries, which
// is how a key's presence is probed after recovery ([lpg.Graph.HasNodeLabel]).
const txnOversizeLabel = "TxnOversize"

// txnOversizePadLabel is the label of the single extra SetNodeLabel op an
// ODD-sized transaction carries. A node costs two ops (AddNode + SetNodeLabel),
// so an odd op budget needs one op that creates no node; a second label on the
// first key is the smallest such op and leaves every key still probeable.
const txnOversizePadLabel = "TxnOversizePad"

// txnOversizeStoreConfig is the simulator's simple directed graph opened with an
// explicit per-transaction op cap. capOps follows the simulator convention (0 ->
// [txn.DefaultMaxTxnOps], negative -> unlimited, positive verbatim) and reaches
// BOTH the producer and the replayer through simMaxTxnOpsOption, so the two
// bounds are equal by construction.
func txnOversizeStoreConfig(capOps int) simStoreConfig {
	cfg := simulatorStoreConfig()
	cfg.maxTxnOps = capOps
	return cfg
}

// TxnOversizeConfig parameterises one producer run.
type TxnOversizeConfig struct {
	// Seed is the master seed for the SimDisk sub-stream. The workload itself is
	// fixed: the arms are boundary sizes, not sampled ones.
	Seed uint64
	// Cap is the per-transaction op cap the store is opened with, in the
	// simulator convention: 0 selects [txn.DefaultMaxTxnOps],
	// [txn.MaxTxnOpsUnlimited] disables the cap, any positive value is taken
	// verbatim. The capped arm passes [txnOversizeCap]; the unlimited arm passes
	// [txn.MaxTxnOpsUnlimited] and drives the SAME op counts.
	Cap int
	// UncappedProducerSeam opens the store with the producer bound left at
	// [txn.DefaultMaxTxnOps] while recovery still honours Cap — the simulator
	// exactly as it behaved before rmp #2474. It is the sensitivity seam of these
	// oracles and must be set only by the test that proves they can fail; see
	// simStoreConfig.uncappedProducerSeam.
	UncappedProducerSeam bool
}

// TxnOversizeAttempt is what ONE commit attempt did. It holds measurements and
// no verdict: whether an attempt SHOULD have been refused is derived by the
// adjudicator from the store's effective cap, never recorded here.
type TxnOversizeAttempt struct {
	// Name identifies the attempt in a failure message.
	Name string
	// Ops is the number of ops the transaction buffered — exactly, by
	// construction, since every buffering call appends exactly one op.
	Ops int
	// Keys are the node keys the transaction created, in issue order.
	Keys []string
	// Refused reports that Commit returned an error.
	Refused bool
	// Sentinel reports that the refusal satisfies
	// errors.Is(err, txn.ErrTransactionTooLarge) — the typed class a caller needs
	// to tell an over-cap refusal from a conflict or an I/O fault.
	Sentinel bool
	// Err is the commit error rendered for a failure message ("" when none).
	Err string
	// WALBefore / WALAfter are the durable WAL image lengths on the SimDisk,
	// read immediately before and after the attempt.
	WALBefore, WALAfter int
	// WALIdentical reports that the two durable images compared BYTE-FOR-BYTE
	// equal, not merely equal in length. This is the assertion that a refusal
	// wrote nothing, as distinct from having written and then truncated.
	WALIdentical bool
	// OrderBefore / OrderAfter are the live graph's node count across the
	// attempt, the in-memory half of the same question.
	OrderBefore, OrderAfter uint64
}

// String renders one attempt for a failure message or a test log.
func (a TxnOversizeAttempt) String() string {
	return fmt.Sprintf(
		"%s: ops=%d keys=%d refused=%t sentinel=%t wal=%d->%d identical=%t order=%d->%d err=%q",
		a.Name, a.Ops, len(a.Keys), a.Refused, a.Sentinel,
		a.WALBefore, a.WALAfter, a.WALIdentical, a.OrderBefore, a.OrderAfter, a.Err)
}

// TxnOversizeEvidence is what a producer run OBSERVED, across the attempts and
// the reopen that follows them.
type TxnOversizeEvidence struct {
	// Cap is the value the store was opened with, in the simulator convention.
	Cap int
	// EffectiveCap is that value RESOLVED the way txn.resolveMaxTxnOps resolves
	// it: the op count above which a commit is refused, or 0 for "no cap". It is
	// what the adjudicator derives every per-attempt expectation from.
	EffectiveCap int
	// Attempts is every commit attempt, in the order driven.
	Attempts []TxnOversizeAttempt
	// PreOversizeWALBytes is the durable WAL length immediately before the FIRST
	// attempt that exceeds [txnOversizeCap]. It is the non-vacuity measurement:
	// a cap adjudicated against an empty file proves nothing.
	PreOversizeWALBytes int
	// MaxAttemptOps is the largest transaction the run drove, which the
	// non-vacuity gate compares against [txnOversizeCap].
	MaxAttemptOps int
	// ReopenClean reports that the reopen's recovery found no genuine corruption.
	ReopenClean bool
	// RecoveredOrder is the reopened graph's live node count, read independently
	// of the per-key probes below.
	RecoveredOrder uint64
	// ModelKeys are the keys of every COMMITTED attempt: what recovery must
	// return, and nothing else.
	ModelKeys []string
	// MissingKeys are the model keys the reopened graph did NOT hold — durable
	// loss.
	MissingKeys []string
	// RefusedKeys are the keys of every refused attempt.
	RefusedKeys []string
	// ResurrectedKeys are the refused keys the reopened graph DID hold — a
	// rejected transaction made durable, the atomicity half of the contract.
	ResurrectedKeys []string
}

// Committed counts the attempts whose commit returned nil.
func (e *TxnOversizeEvidence) Committed() int {
	n := 0
	for _, a := range e.Attempts {
		if !a.Refused {
			n++
		}
	}
	return n
}

// String renders the run for a failure message or a test log.
func (e *TxnOversizeEvidence) String() string {
	return fmt.Sprintf(
		"txn-oversize[cap=%d effective=%d]: attempts=%d committed=%d maxOps=%d preOversizeWAL=%dB "+
			"reopenClean=%t recoveredOrder=%d model=%d missing=%d refused=%d resurrected=%d",
		e.Cap, e.EffectiveCap, len(e.Attempts), e.Committed(), e.MaxAttemptOps, e.PreOversizeWALBytes,
		e.ReopenClean, e.RecoveredOrder, len(e.ModelKeys), len(e.MissingKeys),
		len(e.RefusedKeys), len(e.ResurrectedKeys))
}

// txnOversizeMakeTxn buffers exactly ops operations into tx and returns the node
// keys it created.
//
// The shape is two ops per node — AddNode(key) then SetNodeLabel(key,
// [txnOversizeLabel]) — plus, for an ODD budget, one trailing SetNodeLabel on
// the first key with [txnOversizePadLabel]. Every buffering call appends exactly
// one op (verified in store/txn/txn.go: each is an unconditional append), so the
// resulting op count is exactly ops with no dependence on graph state.
func txnOversizeMakeTxn(tx *txn.Tx[string, float64], name string, ops int) ([]string, error) {
	nodes := ops / 2
	keys := make([]string, 0, nodes)
	for i := range nodes {
		key := fmt.Sprintf("%s-%04d", name, i)
		if err := tx.AddNode(key); err != nil {
			return nil, fmt.Errorf("add node %s: %w", key, err)
		}
		if err := tx.SetNodeLabel(key, txnOversizeLabel); err != nil {
			return nil, fmt.Errorf("label node %s: %w", key, err)
		}
		keys = append(keys, key)
	}
	if ops%2 == 1 {
		if len(keys) == 0 {
			return nil, fmt.Errorf("sim: txn-oversize: a 1-op transaction has no key to pad")
		}
		if err := tx.SetNodeLabel(keys[0], txnOversizePadLabel); err != nil {
			return nil, fmt.Errorf("pad label on %s: %w", keys[0], err)
		}
	}
	return keys, nil
}

// txnOversizePlan is the fixed sequence of attempts every producer run drives.
// The oversize attempts come BEFORE the at-cap one deliberately: the at-cap
// commit that follows them is the control proving the refusals left a working
// writer behind rather than a poisoned one.
var txnOversizePlan = []struct {
	name string
	ops  int
}{
	{"warmup", txnOversizeWarmupOps},
	{"one-over", txnOversizeOneOverOps},
	{"far-over", txnOversizeFarOverOps},
	{"at-cap", txnOversizeCap},
}

// RunTxnOversizeProducer opens a store with cfg.Cap, drives the fixed sequence
// of boundary-sized transactions through it, and reopens it through real
// recovery — reporting what each attempt did to the durable WAL image and to the
// live graph, and what survived the reopen.
//
// The store is driven directly through [txn.Store] rather than through the
// Cypher engine, because the cap counts OPS and only the transaction layer lets
// a scenario buffer an exact number of them. A statement's op count is a
// property of the planner, which would make "exactly at the cap" unreproducible.
func RunTxnOversizeProducer(ctx context.Context, cfg TxnOversizeConfig) (TxnOversizeEvidence, error) {
	scfg := txnOversizeStoreConfig(cfg.Cap)
	scfg.uncappedProducerSeam = cfg.UncappedProducerSeam
	ev := TxnOversizeEvidence{
		Cap:          cfg.Cap,
		EffectiveCap: txnOversizeEffectiveCap(cfg.Cap),
	}

	disk := NewSimDisk(NewSeed(cfg.Seed^txnOversizeDiskSeedMix), 0) // faultRate 0: this scenario injects nothing
	st, err := openSimTypedStore(disk, scfg, txn.NewStringCodec(), txn.NewFloat64WeightCodec())
	if err != nil {
		return ev, fmt.Errorf("sim: txn-oversize open store: %w", err)
	}

	walPath := walPathFor(scfg.dir)
	var model, refused []string
	seenOversize := false

	for _, step := range txnOversizePlan {
		if cerr := ctx.Err(); cerr != nil {
			_ = st.Close()
			return ev, cerr
		}
		before, rerr := txnOversizeWALImage(disk, walPath)
		if rerr != nil {
			_ = st.Close()
			return ev, rerr
		}
		// The non-vacuity measurement is taken at the first attempt that exceeds
		// the reference size, whether or not this store's cap will refuse it, so
		// the capped and unlimited runs are held to the same shape.
		if !seenOversize && step.ops > txnOversizeCap {
			ev.PreOversizeWALBytes = len(before)
			seenOversize = true
		}

		attempt := TxnOversizeAttempt{
			Name:        step.name,
			Ops:         step.ops,
			WALBefore:   len(before),
			OrderBefore: st.graph.LiveOrder(),
		}

		tx, berr := st.store.BeginCtx(ctx)
		if berr != nil {
			_ = st.Close()
			return ev, fmt.Errorf("sim: txn-oversize begin %s: %w", step.name, berr)
		}
		keys, merr := txnOversizeMakeTxn(tx, step.name, step.ops)
		if merr != nil {
			_ = tx.Rollback()
			_ = st.Close()
			return ev, fmt.Errorf("sim: txn-oversize build %s: %w", step.name, merr)
		}
		attempt.Keys = keys

		commitErr := tx.Commit()
		if commitErr != nil {
			attempt.Refused = true
			attempt.Sentinel = errors.Is(commitErr, txn.ErrTransactionTooLarge)
			attempt.Err = commitErr.Error()
			refused = append(refused, keys...)
		} else {
			model = append(model, keys...)
		}

		after, rerr := txnOversizeWALImage(disk, walPath)
		if rerr != nil {
			_ = st.Close()
			return ev, rerr
		}
		attempt.WALAfter = len(after)
		attempt.WALIdentical = bytes.Equal(before, after)
		attempt.OrderAfter = st.graph.LiveOrder()
		ev.Attempts = append(ev.Attempts, attempt)
		ev.MaxAttemptOps = max(ev.MaxAttemptOps, step.ops)
	}

	if cerr := st.Close(); cerr != nil {
		return ev, fmt.Errorf("sim: txn-oversize close store: %w", cerr)
	}

	// Reopen through real recovery over the surviving byte image.
	re, err := openSimTypedStore(disk, scfg, txn.NewStringCodec(), txn.NewFloat64WeightCodec())
	if err != nil {
		return ev, fmt.Errorf("sim: txn-oversize reopen: %w", err)
	}
	defer func() { _ = re.Close() }()

	ev.ReopenClean = re.clean
	ev.RecoveredOrder = re.graph.LiveOrder()
	slices.Sort(model)
	slices.Sort(refused)
	ev.ModelKeys = model
	ev.RefusedKeys = refused
	for _, key := range model {
		if !re.graph.HasNodeLabel(key, txnOversizeLabel) {
			ev.MissingKeys = append(ev.MissingKeys, key)
		}
	}
	for _, key := range refused {
		if re.graph.HasNodeLabel(key, txnOversizeLabel) {
			ev.ResurrectedKeys = append(ev.ResurrectedKeys, key)
		}
	}
	return ev, nil
}

// txnOversizeEffectiveCap resolves a simulator-convention cap to the op count
// above which a commit is refused, or 0 for "no cap". It reproduces
// txn.resolveMaxTxnOps composed with the store's own `maxTxnOps > 0` guard, so
// the adjudicator derives its expectations from the same arithmetic the engine
// applies rather than from what the caller intended.
func txnOversizeEffectiveCap(capOps int) int {
	resolved := simMaxTxnOpsOption(capOps)
	switch {
	case resolved == 0:
		return txn.DefaultMaxTxnOps
	case resolved < 0:
		return 0
	default:
		return resolved
	}
}

// txnOversizeWALImage reads the whole durable WAL byte image off the SimDisk,
// returning an empty slice when no WAL exists yet.
func txnOversizeWALImage(disk *SimDisk, path string) ([]byte, error) {
	if !disk.Exists(path) {
		return nil, nil
	}
	b, err := disk.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("sim: txn-oversize read WAL image: %w", err)
	}
	return b, nil
}

// checkTxnOversizeNonVacuity is the SEPARATE coverage precondition, kept apart
// from [checkTxnOversizeProducer] so an uninformative run never reads as a
// faulty one. It asserts the run had the SHAPE a cap adjudication needs — an
// attempt genuinely larger than the reference cap, a non-empty WAL underneath it,
// and at least one transaction that actually committed — before any statement
// about the cap is made.
//
// It is shape-only by design: it says nothing about whether a refusal happened,
// because on the unlimited arm none should. A violation here means the RUN
// proved nothing, not that the STORE is broken.
func checkTxnOversizeNonVacuity(e *TxnOversizeEvidence) []Violation {
	var v []Violation
	add := func(msg string) {
		v = append(v, Violation{
			Kind:    ViolationVacuousRun,
			Op:      "<txn-oversize non-vacuity>",
			Message: msg + " — " + e.String(),
		})
	}
	if len(e.Attempts) == 0 {
		add("no commit was attempted at all")
		return v
	}
	if e.MaxAttemptOps <= txnOversizeCap {
		add(fmt.Sprintf(
			"the largest transaction buffered %d ops, which does not exceed the reference cap %d: "+
				"no attempt was oversize, so nothing about a cap was exercised",
			e.MaxAttemptOps, txnOversizeCap))
	}
	if e.PreOversizeWALBytes <= 0 {
		add("the WAL was EMPTY when the first oversize transaction was attempted: " +
			"a byte-unchanged assertion over an absent file is satisfied by definition")
	}
	if e.Committed() == 0 {
		add("no transaction committed: the run never exercised the durable write path, " +
			"so the reopen has nothing to adjudicate")
	}
	return v
}

// checkTxnOversizeProducer is the UNCONDITIONAL verdict on the producer cap. It
// derives each attempt's expected outcome from the store's effective cap — an
// attempt is expected to be refused exactly when its op count EXCEEDS that cap —
// so one adjudicator serves the capped and the unlimited arms without being told
// which it is looking at.
//
// It fails however little the workload exercised the cap; [checkTxnOversizeNonVacuity]
// is what reports an uninformative run, and the caller must run it separately.
func checkTxnOversizeProducer(e *TxnOversizeEvidence) []Violation {
	var v []Violation
	add := func(op, msg string) {
		v = append(v, Violation{Kind: ViolationOracleDeviation, Op: op, Message: msg + " — " + e.String()})
	}

	for _, a := range e.Attempts {
		op := "<txn-oversize " + a.Name + ">"
		overCap := e.EffectiveCap > 0 && a.Ops > e.EffectiveCap
		if !overCap {
			if a.Refused {
				add(op, fmt.Sprintf(
					"a WITHIN-cap transaction of %d ops was refused under cap %d: %s",
					a.Ops, e.EffectiveCap, a.String()))
				continue
			}
			if a.WALAfter <= a.WALBefore {
				add(op, fmt.Sprintf(
					"the transaction committed but the durable WAL did not grow (%d -> %d bytes): "+
						"a commit that appended nothing cannot have been made durable — %s",
					a.WALBefore, a.WALAfter, a.String()))
			}
			continue
		}
		if !a.Refused {
			add(op, fmt.Sprintf(
				"an OVER-cap transaction of %d ops was COMMITTED under cap %d: the producer bound is not "+
					"enforced, so a transaction recovery may be unable to replay can reach the disk — %s",
				a.Ops, e.EffectiveCap, a.String()))
			continue
		}
		if !a.Sentinel {
			add(op, fmt.Sprintf(
				"the over-cap transaction was refused with an error that is NOT txn.ErrTransactionTooLarge: "+
					"a caller cannot tell an over-cap refusal from a conflict or an I/O fault — %s",
				a.String()))
		}
		if !a.WALIdentical {
			add(op, fmt.Sprintf(
				"the durable WAL image CHANGED across a refused transaction (%d -> %d bytes): the refusal "+
					"happened after frames were appended, not before, so a partial write reached the file — %s",
				a.WALBefore, a.WALAfter, a.String()))
		}
		if a.OrderAfter != a.OrderBefore {
			add(op, fmt.Sprintf(
				"the live graph MUTATED across a refused transaction (order %d -> %d): the rejection did not "+
					"precede the in-memory apply — %s",
				a.OrderBefore, a.OrderAfter, a.String()))
		}
	}

	if !e.ReopenClean {
		add("<txn-oversize reopen>",
			"recovery over the surviving WAL reported genuine corruption: the refused transactions did not "+
				"leave a replayable file behind")
	}
	if len(e.MissingKeys) > 0 {
		add("<txn-oversize reopen>", fmt.Sprintf(
			"%d committed key(s) did not survive recovery, first %q: acknowledged work was lost",
			len(e.MissingKeys), e.MissingKeys[0]))
	}
	if len(e.ResurrectedKeys) > 0 {
		add("<txn-oversize reopen>", fmt.Sprintf(
			"%d key(s) from a REFUSED transaction came back after recovery, first %q: a rejected "+
				"transaction was made durable",
			len(e.ResurrectedKeys), e.ResurrectedKeys[0]))
	}
	if e.RecoveredOrder != uint64(len(e.ModelKeys)) {
		add("<txn-oversize reopen>", fmt.Sprintf(
			"the recovered graph holds %d live node(s) but the model holds %d: recovery and the model "+
				"disagree beyond the per-key probes",
			e.RecoveredOrder, len(e.ModelKeys)))
	}
	return v
}

// -----------------------------------------------------------------------------
// The replay half: a hand-crafted oversize WAL
// -----------------------------------------------------------------------------

// txnOversizeReplayCap is the replay bound the crafted-WAL arms are driven with.
// It is independent of [txnOversizeCap] so a reader cannot mistake one arm's
// boundary for the other's.
const txnOversizeReplayCap = 16

// txnOversizeReplayPriorOps is the size of the COMMITTED transaction written at
// the head of every crafted file. It is what recovery must still recover when
// the oversize run behind it fail-stops: the documented contract is that the
// committed prefix up to the bad frame survives, and a file with no prefix could
// not tell a fail-stop from a total loss.
const txnOversizeReplayPriorOps = 4

// TxnOversizeReplayArm is one crafted-WAL replay arm: the file that was built
// and what replaying it under a given cap did.
type TxnOversizeReplayArm struct {
	// Name identifies the arm in a failure message.
	Name string
	// RunOps is the number of op frames in the marker-less run that follows the
	// committed prefix, before its own commit marker closes it.
	RunOps int
	// Cap is the replay cap the arm drives, in the simulator convention.
	Cap int
	// EffectiveCap is Cap resolved to the buffered-op count at which replay
	// stops, or 0 for "no cap".
	EffectiveCap int
	// Bytes is the crafted file's length on the SimDisk.
	Bytes int
	// TailErr is the replay stop reason rendered for a message ("" when nil).
	TailErr string
	// Sentinel reports errors.Is(TailErr, recovery.ErrTransactionTooLarge).
	Sentinel bool
	// Clean is [recovery.ReplayResult.IsClean]: false when the stop reason is
	// classified as genuine corruption, which is what makes a fail-stop
	// fail-STOP rather than a tolerated tail.
	Clean bool
	// WALOps is how many ops replay applied to the graph.
	WALOps int
	// Order is the replayed graph's live node count, read independently of
	// WALOps.
	Order uint64
	// HarnessRefused reports that opening the same image through the simulator's
	// own store-open path returned an error rather than a store, which is the
	// end-to-end half: a fail-stop that the embedder swallowed would append onto
	// the corruption.
	HarnessRefused bool
	// HarnessSentinel reports that the harness error carries
	// [recovery.ErrTransactionTooLarge].
	HarnessSentinel bool
}

// String renders one arm for a failure message or a test log.
func (a TxnOversizeReplayArm) String() string {
	return fmt.Sprintf(
		"%s: runOps=%d cap=%d effective=%d bytes=%d clean=%t sentinel=%t walOps=%d order=%d "+
			"harnessRefused=%t harnessSentinel=%t tailErr=%q",
		a.Name, a.RunOps, a.Cap, a.EffectiveCap, a.Bytes, a.Clean, a.Sentinel, a.WALOps, a.Order,
		a.HarnessRefused, a.HarnessSentinel, a.TailErr)
}

// TxnOversizeReplayEvidence is what the crafted-WAL sweep observed.
type TxnOversizeReplayEvidence struct {
	// PriorOps is the committed prefix every arm's file carries.
	PriorOps int
	// Arms is every arm, in the order driven.
	Arms []TxnOversizeReplayArm
}

// String renders the sweep for a failure message or a test log.
func (e TxnOversizeReplayEvidence) String() string {
	out := fmt.Sprintf("txn-oversize replay: priorOps=%d arms=%d", e.PriorOps, len(e.Arms))
	for _, a := range e.Arms {
		out += "\n  " + a.String()
	}
	return out
}

// txnOversizeV3AddNode builds ONE v3 op payload for AddNode(key) under txnSeq.
//
// The layout is written out here rather than borrowed, because store/txn's
// encoder is unexported: version tag, kind, 8-byte little-endian sequence, then
// the v2 body for a node-only op — the codec-encoded key, the codec-encoded ZERO
// key (the unused dst endpoint), and a uint16 label length of zero. It matches
// txn.encodeOpTypedV3Into composed with txn.encodeOpNodeOnly, and the match is
// PROVEN rather than asserted: the within-cap arms replay these frames through
// the real decoder and must recover exactly the nodes they name, which a wrong
// layout could not do.
func txnOversizeV3AddNode(codec txn.Codec[string], key string, txnSeq uint64) ([]byte, error) {
	buf := []byte{txn.OpRecordV3, byte(txn.OpAddNode)}
	buf = binary.LittleEndian.AppendUint64(buf, txnSeq)
	var err error
	if buf, err = codec.Encode(buf, key); err != nil {
		return nil, fmt.Errorf("encode key %q: %w", key, err)
	}
	if buf, err = codec.Encode(buf, ""); err != nil {
		return nil, fmt.Errorf("encode zero endpoint: %w", err)
	}
	return binary.LittleEndian.AppendUint16(buf, 0), nil // no label
}

// txnOversizeV3Commit builds the [txn.OpCommit] marker payload for txnSeq. The
// commit timestamp is zero, exactly as the store's own commit path writes it
// (the MVCC instant belongs to Tx.CommitWALOnly, not to Tx.Commit).
func txnOversizeV3Commit(txnSeq uint64) []byte {
	buf := []byte{txn.OpRecordV3, byte(txn.OpCommit)}
	buf = binary.LittleEndian.AppendUint64(buf, txnSeq)
	return binary.LittleEndian.AppendUint64(buf, 0)
}

// craftTxnOversizeWAL writes a WAL holding two complete transactions: a
// committed prefix of priorOps AddNode frames under sequence 1, then a run of
// runOps AddNode frames under sequence 2, each closed by its own OpCommit
// marker.
//
// Only the op stream is hand-made. The framing, CRC and fsync all go through the
// real [wal.Writer] over the SimDisk, so the file a replayer sees is
// indistinguishable from one a producer wrote — which is the point: a file the
// harness malformed would be caught by the reader long before the cap, and the
// unlimited arm replaying this same image cleanly is what rules that out.
func craftTxnOversizeWAL(disk *SimDisk, path string, priorOps, runOps int) error {
	w, err := wal.OpenFS(simWALFS{disk: disk}, path)
	if err != nil {
		return fmt.Errorf("sim: txn-oversize craft open WAL: %w", err)
	}
	codec := txn.NewStringCodec()
	emitTxn := func(seq uint64, prefix string, ops int) error {
		mark, aerr := w.AppendRun(func(emit func([]byte) error) error {
			for i := range ops {
				payload, perr := txnOversizeV3AddNode(codec, fmt.Sprintf("%s-%04d", prefix, i), seq)
				if perr != nil {
					return perr
				}
				if eerr := emit(payload); eerr != nil {
					return eerr
				}
			}
			return emit(txnOversizeV3Commit(seq))
		})
		if aerr != nil {
			return fmt.Errorf("sim: txn-oversize craft append seq %d: %w", seq, aerr)
		}
		if serr := w.SyncGroup(mark); serr != nil {
			return fmt.Errorf("sim: txn-oversize craft sync seq %d: %w", seq, serr)
		}
		return nil
	}
	if err := emitTxn(1, "prior", priorOps); err != nil {
		_ = w.Close()
		return err
	}
	if err := emitTxn(2, "run", runOps); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("sim: txn-oversize craft close WAL: %w", err)
	}
	return nil
}

// txnOversizeReplayPlan is the fixed sweep. The three arms are chosen so that
// the third REFUTES the alternative explanation of the second: `over-cap` and
// `over-cap-unlimited` build the byte-identical file and differ only in the cap,
// so a fail-stop in one and a clean replay in the other is attributable to the
// cap and to nothing else.
var txnOversizeReplayPlan = []struct {
	name   string
	runOps int
	capOps int
}{
	{"at-cap", txnOversizeReplayCap, txnOversizeReplayCap},
	{"over-cap", txnOversizeReplayCap + 1, txnOversizeReplayCap},
	{"over-cap-unlimited", txnOversizeReplayCap + 1, txn.MaxTxnOpsUnlimited},
}

// RunTxnOversizeReplay builds a hand-crafted WAL per arm and replays it under
// that arm's cap, reporting what recovery did with it.
//
// Each arm gets its OWN SimDisk, so an arm's replay (and the harness open that
// follows it, which truncates a recovered tail) cannot perturb the next.
func RunTxnOversizeReplay(ctx context.Context, seed uint64) (TxnOversizeReplayEvidence, error) {
	ev := TxnOversizeReplayEvidence{PriorOps: txnOversizeReplayPriorOps}
	for i, step := range txnOversizeReplayPlan {
		if cerr := ctx.Err(); cerr != nil {
			return ev, cerr
		}
		scfg := txnOversizeStoreConfig(step.capOps)
		walPath := walPathFor(scfg.dir)
		disk := NewSimDisk(NewSeed(seed^txnOversizeDiskSeedMix^uint64(i+1)), 0)
		if err := craftTxnOversizeWAL(disk, walPath, txnOversizeReplayPriorOps, step.runOps); err != nil {
			return ev, err
		}
		image, err := txnOversizeWALImage(disk, walPath)
		if err != nil {
			return ev, err
		}

		arm := TxnOversizeReplayArm{
			Name:         step.name,
			RunOps:       step.runOps,
			Cap:          step.capOps,
			EffectiveCap: txnOversizeEffectiveCap(step.capOps),
			Bytes:        len(image),
		}

		// (1) The replay core directly, so the arm reads recovery's OWN report
		// rather than only the harness's translation of it.
		res, g, err := txnOversizeReplay(ctx, disk, walPath, scfg, step.capOps)
		if err != nil {
			return ev, err
		}
		if res.TailErr != nil {
			arm.TailErr = res.TailErr.Error()
			arm.Sentinel = errors.Is(res.TailErr, recovery.ErrTransactionTooLarge)
		}
		arm.Clean = res.IsClean()
		arm.WALOps = res.WALOps
		arm.Order = g.LiveOrder()

		// (2) The harness store-open path over the same image: an embedder must
		// refuse to append onto a fail-stop, never swallow it.
		st, oerr := openSimTypedStore(disk, scfg, txn.NewStringCodec(), txn.NewFloat64WeightCodec())
		if oerr != nil {
			arm.HarnessRefused = true
			arm.HarnessSentinel = errors.Is(oerr, recovery.ErrTransactionTooLarge)
		} else {
			_ = st.Close()
		}

		ev.Arms = append(ev.Arms, arm)
	}
	return ev, nil
}

// txnOversizeReplay replays the WAL image at path into a fresh graph under the
// given cap, through the same [recovery.ReplayWAL] core the simulator's WAL-only
// reopen drives.
func txnOversizeReplay(
	ctx context.Context, disk *SimDisk, path string, scfg simStoreConfig, capOps int,
) (recovery.ReplayResult, *lpg.Graph[string, float64], error) {
	g := lpg.New[string, float64](scfg.graphConfig)
	rh, err := disk.OpenFile(path, os.O_RDONLY)
	if err != nil {
		return recovery.ReplayResult{}, g, fmt.Errorf("sim: txn-oversize open crafted WAL: %w", err)
	}
	reader := wal.NewReader(rh, rh)
	res, rerr := recovery.ReplayWAL[string, float64](
		ctx, reader, g, txn.NewStringCodec(), txn.NewFloat64WeightCodec(),
		resolveSimMaxTxnOps(capOps),
	)
	_ = reader.Close()
	if rerr != nil {
		return res, g, fmt.Errorf("sim: txn-oversize replay crafted WAL: %w", rerr)
	}
	return res, g, nil
}

// checkTxnOversizeReplayNonVacuity is the SEPARATE coverage precondition for the
// crafted-WAL sweep: the files must have had the shape the adjudication assumes —
// a non-empty image, a committed prefix to survive a fail-stop, and a run that
// really exceeds the arm's cap where one is set.
//
// A violation here means the FILES were built wrong, not that recovery is broken.
func checkTxnOversizeReplayNonVacuity(e TxnOversizeReplayEvidence) []Violation {
	var v []Violation
	add := func(msg string) {
		v = append(v, Violation{
			Kind:    ViolationVacuousRun,
			Op:      "<txn-oversize replay non-vacuity>",
			Message: msg + " — " + e.String(),
		})
	}
	if len(e.Arms) == 0 {
		add("no arm was driven at all")
		return v
	}
	if e.PriorOps <= 0 {
		add("the crafted files carry no committed prefix: a fail-stop that recovered nothing " +
			"would be indistinguishable from one that lost everything")
	}
	overCap := 0
	for _, a := range e.Arms {
		if a.Bytes <= 0 {
			add(fmt.Sprintf("arm %q produced an EMPTY file: nothing was replayed", a.Name))
		}
		if a.EffectiveCap > 0 && a.RunOps > a.EffectiveCap {
			overCap++
		}
	}
	if overCap == 0 {
		add("no arm drove a run that exceeds its own replay cap: the sweep never presented " +
			"recovery with an oversize transaction")
	}
	return v
}

// checkTxnOversizeReplay is the UNCONDITIONAL verdict on the replay cap. Like
// the producer adjudicator it derives each arm's expectation from that arm's
// effective cap — replay must fail-stop exactly when the marker-less run would
// buffer MORE ops than the cap allows — so the capped and unlimited arms are
// judged by one rule.
func checkTxnOversizeReplay(e TxnOversizeReplayEvidence) []Violation {
	var v []Violation
	add := func(op, msg string) {
		v = append(v, Violation{Kind: ViolationOracleDeviation, Op: op, Message: msg + " — " + e.String()})
	}
	for _, a := range e.Arms {
		op := "<txn-oversize replay " + a.Name + ">"
		overCap := a.EffectiveCap > 0 && a.RunOps > a.EffectiveCap
		if !overCap {
			// Within cap: the whole file must replay, both transactions applied.
			want := e.PriorOps + a.RunOps
			if !a.Clean {
				add(op, fmt.Sprintf(
					"a WITHIN-cap file was rejected as corrupt under cap %d: %s", a.EffectiveCap, a.String()))
			}
			if a.WALOps != want {
				add(op, fmt.Sprintf(
					"replay applied %d op(s), want %d (prefix %d + run %d): the within-cap file did not "+
						"replay whole — %s",
					a.WALOps, want, e.PriorOps, a.RunOps, a.String()))
			}
			if a.Order != uint64(want) {
				add(op, fmt.Sprintf(
					"the replayed graph holds %d live node(s), want %d: the op count and the graph "+
						"disagree — %s",
					a.Order, want, a.String()))
			}
			if a.HarnessRefused {
				add(op, fmt.Sprintf(
					"the harness store-open REFUSED a within-cap file: %s", a.String()))
			}
			continue
		}
		// Over cap: fail-stop, with the committed prefix intact.
		if a.Clean {
			add(op, fmt.Sprintf(
				"an OVER-cap marker-less run of %d op(s) under cap %d replayed as CLEAN: recovery buffered "+
					"the whole run rather than stopping, so a crafted or corrupt file can drive it to "+
					"allocate without bound — %s",
				a.RunOps, a.EffectiveCap, a.String()))
		}
		if !a.Sentinel {
			add(op, fmt.Sprintf(
				"replay stopped with an error that is NOT recovery.ErrTransactionTooLarge, so the stop "+
					"cannot be attributed to the op cap — %s", a.String()))
		}
		if a.WALOps != e.PriorOps {
			add(op, fmt.Sprintf(
				"replay applied %d op(s), want exactly the committed prefix %d: a fail-stop must keep the "+
					"prefix and apply none of the oversize run — %s",
				a.WALOps, e.PriorOps, a.String()))
		}
		if a.Order != uint64(e.PriorOps) {
			add(op, fmt.Sprintf(
				"the replayed graph holds %d live node(s), want the prefix's %d — %s",
				a.Order, e.PriorOps, a.String()))
		}
		if !a.HarnessRefused {
			add(op, fmt.Sprintf(
				"the harness store-open ACCEPTED a file recovery fail-stopped on: an embedder that appends "+
					"onto it embeds the fault permanently — %s", a.String()))
		} else if !a.HarnessSentinel {
			add(op, fmt.Sprintf(
				"the harness refused but its error does not carry recovery.ErrTransactionTooLarge: the "+
					"caller cannot tell an op-cap fail-stop from any other corruption — %s", a.String()))
		}
	}
	return v
}
