// checkpoint.go is the REMOVABLE half of the cross-release helper (rmp #2477).
//
// It installs [publishCheckpointHook] so the helper publishes a durable
// checkpoint — a snapshot directory under <dir>/snapshot plus a WAL truncated
// to the snapshot's watermark — before it exits. That is what puts a PRIOR
// RELEASE'S snapshot bytes in front of the current code: manifest.json,
// csr.bin, labels.bin, properties.bin and mapper.bin as that release wrote
// them, opened by the current reader.
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
//     that column cannot size (rmp #2526).
//   - No WithCommitSerialiser. The helper drives the store from ONE goroutine
//     and the checkpoint runs after the last op, so there is no concurrent
//     writer for the serialiser to drain and the storeMu fallback's precondition
//     ("the caller serialises its own writes under that same mutex") holds
//     vacuously.
//   - No constraint or index specs. The cross-release op stream is CREATE /
//     MERGE / MATCH / SET / DELETE over nodes and edges and declares no DDL, so
//     there is no durable schema for the truncated WAL prefix to strand.
package main

import (
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func init() { publishCheckpointHook = publishCheckpoint }

// publishCheckpoint runs one synchronous checkpoint over the helper's store.
//
// [checkpoint.Checkpointer.RunCheckpoint] is used rather than
// Start/Trigger/Stop because the helper is a short-lived, single-goroutine
// process: RunCheckpoint's contract (single-goroutine, non-overlapping, no
// running loop) is met by construction, and it spawns no background goroutine
// that would then have to be joined before the WAL is closed.
func publishCheckpoint(dir string, g *lpg.Graph[string, float64], _ *txn.Store[string, float64], wlog *wal.Writer) error {
	// storeMu is the checkpointer's commit-serialisation fallback. The helper
	// takes no such lock of its own because it never writes concurrently, so a
	// fresh mutex is the honest expression of "there is nothing to serialise
	// against"; see the package comment.
	var storeMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, wlog, &storeMu)
	return cp.RunCheckpoint()
}
