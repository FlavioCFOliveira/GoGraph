// checkpoint.go is the REMOVABLE half of the cross-release helper (rmp #2477,
// reworked by rmp #2531).
//
// It installs [publishCheckpointHook] so the helper publishes a durable
// checkpoint — a snapshot directory under <dir>/snapshot plus a WAL truncated
// to the snapshot's watermark — before it exits. That is what puts a PRIOR
// RELEASE'S snapshot bytes in front of the current code: manifest.json,
// csr.bin, labels.bin, properties.bin, mapper.bin and edgehandles.bin as that
// release wrote them, opened by the current reader.
//
// # Why Start/TriggerCtx/Stop and not RunCheckpoint (rmp #2531)
//
// This file is compiled against the target TAG's packages, so every symbol it
// names must exist at that tag. The obvious entry point,
// [checkpoint.Checkpointer.RunCheckpoint], is the WRONG choice for exactly that
// reason: it was only exported from v0.6.0 onwards. At v0.1.0..v0.5.0 the same
// body exists solely as the unexported runCheckpoint, so naming it there fails
// the build with
//
//	cp.RunCheckpoint undefined (type *checkpoint.Checkpointer[string, float64]
//	has no field or method RunCheckpoint, but does have unexported method
//	runCheckpoint)
//
// which cost this file its build at every tag the harness actually exercises and
// silently reduced cross-release coverage to HEAD-as-prior.
//
// Start/Trigger/TriggerCtx/Stop, by contrast, have been exported with an
// UNCHANGED shape since v0.1.0 — as have [checkpoint.New] and
// [checkpoint.Config]'s Dir field — so this route reaches every release tag the
// repository holds. It is also the same body: the checkpoint loop's triggerCh
// arm calls precisely the runCheckpoint that RunCheckpoint later exposed, so
// nothing about the artefact on disk depends on which door was used.
//
// The sequence is New → Start → TriggerCtx → Stop, and each step is load-bearing:
//
//   - Start is REQUIRED, not optional. TriggerCtx submits a request on a
//     buffered channel and then waits for the loop to answer it. With no loop
//     running, the submit succeeds into the buffer and the wait never completes:
//     Trigger alone would HANG rather than fail, so the loop must be up first.
//   - TriggerCtx rather than Trigger, so the wait is bounded. A checkpoint that
//     cannot complete becomes a reported error at [checkpointPublishTimeout]
//     instead of a wedged subprocess the harness can only kill on its own outer
//     deadline.
//   - Stop joins the loop goroutine before this function returns, so the
//     checkpoint has demonstrably finished writing through the WAL writer that
//     main.go closes immediately afterwards.
//
// # Why this is a second file and not part of main.go
//
// Both files are staged into a git worktree of the target tag and compiled
// against THAT tag's packages. main.go is pinned to the API stable across
// v0.2.0..HEAD; the checkpoint API is younger and has moved more. If the
// checkpoint call did not compile at some tag and lived in main.go, the whole
// binary would fail to build and the harness would report the tag as
// "unbuildable" — a clean SKIP that silently removes an entire release from
// cross-release coverage, which is exactly the vacuity this task exists to
// avoid. Kept apart, the harness builds with both files, and on failure drops
// THIS one and rebuilds: the tag still runs, still writes a WAL image, and the
// wire protocol reports checkpoint=false so the caller knows which of the two
// shapes the image has. See [github.com/FlavioCFOliveira/GoGraph/internal/sim]
// BuildPriorReleaseHelper for the two-stage build.
//
// That fallback is retained deliberately even though every tag in the repository
// now builds WITH this file. It is the harness's insurance against a FUTURE tag
// whose checkpoint API moves again, and its loudness — BuildFallbackErr surfaced
// in the run report — is what made the rmp #2531 gap visible in the first place.
//
// # Why the option set is deliberately minimal
//
// Every option passed here is another symbol that must exist at every tag, and
// each one that does not costs the whole checkpoint at that tag. Only what the
// helper's fixed shape actually needs is used:
//
//   - No WithMapperCodec. N is string, for which the checkpointer already writes
//     the self-sufficient string mapper, so the WAL prefix is truncated anyway.
//   - No WithWeightCodec. W is float64, a fixed-width primitive the dense CSR
//     weights column persists natively; a codec is only needed for weight types
//     that column cannot size (rmp #2526). It is also younger than most tags —
//     it does not exist before HEAD — so naming it would reintroduce the very
//     build failure rmp #2531 removed.
//   - No WithCommitSerialiser. The helper drives the store from ONE goroutine
//     and the checkpoint runs after the last op, so there is no concurrent
//     writer for the serialiser to drain and the storeMu fallback's precondition
//     ("the caller serialises its own writes under that same mutex") holds
//     vacuously — the checkpoint now runs on the loop goroutine, but it is still
//     the only party touching the store while it runs.
//   - No constraint or index specs. The cross-release op stream is CREATE /
//     MERGE / MATCH / SET / DELETE over nodes and edges and declares no DDL, so
//     there is no durable schema for the truncated WAL prefix to strand.
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func init() { publishCheckpointHook = publishCheckpoint }

// checkpointPublishTimeout bounds the whole publish sequence. The cross-release
// op streams are a few hundred ops over a few dozen nodes, so a healthy
// checkpoint completes in milliseconds; this is a wedge detector, not a budget.
// It is generous enough that a loaded CI machine never trips it, and finite so a
// prior release that cannot check-point reports an error instead of hanging.
const checkpointPublishTimeout = 90 * time.Second

// publishCheckpoint runs one synchronous checkpoint over the helper's store and
// returns only after it has completed and the checkpoint loop has been joined.
func publishCheckpoint(dir string, g *lpg.Graph[string, float64], _ *txn.Store[string, float64], wlog *wal.Writer) error {
	// storeMu is the checkpointer's commit-serialisation fallback. The helper
	// takes no such lock of its own because it never writes concurrently, so a
	// fresh mutex is the honest expression of "there is nothing to serialise
	// against"; see the package comment.
	var storeMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, wlog, &storeMu)

	// Interval and MaxAge are both zero, so the loop installs no ticker and fires
	// on nothing but our explicit trigger: the artefact on disk is the product of
	// exactly one checkpoint, which is what makes the image reproducible.
	ctx, cancel := context.WithTimeout(context.Background(), checkpointPublishTimeout)
	defer cancel()

	cp.Start(ctx)
	triggerErr := cp.TriggerCtx(ctx)
	// Stop unconditionally, even when the trigger failed: the loop goroutine owns
	// the WAL writer for the duration of a checkpoint, and main.go closes that
	// writer the moment this returns. Joining is what makes "the checkpoint is
	// finished" true rather than probable.
	cp.Stop()
	if triggerErr != nil {
		return fmt.Errorf("trigger checkpoint at %q: %w", dir, triggerErr)
	}
	return nil
}
