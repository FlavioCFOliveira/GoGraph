package bulkimport

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
)

// snapshotName is the ONE directory name recovery reads a snapshot from.
//
// This is not a free choice and getting it wrong fails silently. Recovery looks
// for the snapshot at exactly <storeDir>/snapshot (store/recovery/recovery.go)
// and nowhere else; the #2177 spike wrote a complete, valid snapshot to a
// different path, called recovery.Open on the store directory, and got back
// SnapshotHit=false with an EMPTY graph and no error at all.
const snapshotName = "snapshot"

// ErrStoreNotEmpty reports that the target directory already holds files, so it
// cannot be imported into. See [Publish] for why this is refused rather than
// merged.
var ErrStoreNotEmpty = errors.New("bulkimport: target store directory is not empty")

// ErrNotFinished reports that [Publish] was given a Builder whose
// [Builder.Finish] has not run, so its adjacency commit window is still open.
var ErrNotFinished = errors.New("bulkimport: builder has not been finished")

// PublishResult reports what was written.
type PublishResult struct {
	// SnapshotDir is the published directory, <storeDir>/snapshot.
	SnapshotDir string
	// Stats are the builder's ingest counts, carried through for convenience.
	Stats Stats
}

// Publish writes b's graph into storeDir as the store's snapshot, so that
// recovery.Open(storeDir) reconstructs it.
//
// b must already have been finished with [Builder.Finish]; publishing a graph
// whose commit window is still open would hand out shards that are still mutable
// in place.
//
// # The directory must be empty
//
// storeDir must not exist, or must exist and be empty. A non-empty directory is
// refused with [ErrStoreNotEmpty], and the check is enforced here rather than
// left to documentation, because the failure it prevents is silent corruption
// rather than an error: this path writes NO write-ahead log, so if the directory
// already held one, recovery would replay that WAL ON TOP of the freshly
// published snapshot. That is not a merge of old and new data — it is the old
// log's operations applied to an unrelated graph.
//
// A store that must absorb bulk data into existing content uses the ordinary
// transactional write path. This one builds a store; it does not extend one.
//
// # What is atomic
//
// The whole import. [snapshot.WriteSnapshotFullCtx] assembles the snapshot under
// <storeDir>/snapshot.tmp and renames it to <storeDir>/snapshot on success, and a
// rename within a directory is atomic, so at every instant the store either has
// no snapshot or has a complete one. There is no state in which a reader can
// observe part of the imported graph.
//
// A crash before the rename leaves the assembly directory behind. Recovery
// neither opens it — it is not the name recovery reads — nor keeps it: recovery
// removes a stale <snapshot>.tmp on open. So a crashed import leaves a store that
// looks exactly as it did before the import started.
//
// # What is NOT atomic, and must not be read as such
//
//   - This is not a transaction. It has no transaction id, appends no WAL
//     record, participates in no isolation level, and cannot be rolled back once
//     published. Undoing it means deleting the directory.
//   - There is no per-record durability acknowledgement and no resumption point.
//     Nothing is durable until the rename; a caller that streams ten million
//     edges and crashes at nine million has no partial result to resume from and
//     re-runs the import.
//   - It is concurrent with nothing. No reader, no writer and no checkpointer may
//     touch storeDir during the import. That is why this is an offline Go call and
//     not a Cypher clause: publishing a snapshot under a live server would race
//     the checkpointer and invalidate open readers' view.
//
// Durability of the published bytes rests on the snapshot writer's existing fsync
// discipline — each file fsynced, then the parent directory — which is the same
// protocol the checkpointer uses and which the crash-injection battery already
// exercises. This adds no new durability mechanism, deliberately.
func Publish[W any](ctx context.Context, storeDir string, b *Builder[W]) (PublishResult, error) {
	var res PublishResult
	if b == nil {
		return res, fmt.Errorf("bulkimport: nil builder")
	}
	if !b.finished {
		return res, ErrNotFinished
	}
	g := b.Graph()
	if g == nil {
		return res, ErrNotFinished
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if err := requireEmptyDir(storeDir); err != nil {
		return res, err
	}
	if err := os.MkdirAll(storeDir, 0o750); err != nil {
		return res, fmt.Errorf("bulkimport: create store directory %q: %w", storeDir, err)
	}

	snapDir := filepath.Join(storeDir, snapshotName)
	c := csr.BuildFromAdjList[string, W](g.AdjList())
	if err := snapshot.WriteSnapshotFullCtx[string, W](ctx, snapDir, c, g); err != nil {
		return res, fmt.Errorf("bulkimport: publish snapshot to %q: %w", snapDir, err)
	}
	res.SnapshotDir = snapDir
	res.Stats = b.stats
	return res, nil
}

// requireEmptyDir returns nil when dir does not exist or exists and contains
// nothing, and [ErrStoreNotEmpty] when it holds any entry.
//
// Every entry counts, including a leftover snapshot.tmp from a previous crashed
// import: proceeding would let WriteSnapshotFull remove and reuse that name,
// which is harmless in itself, but a directory with debris in it is not one whose
// contents this function can vouch for. Refusing is the honest answer; the
// operator deletes the directory or picks another.
func requireEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil // will be created
	case err != nil:
		return fmt.Errorf("bulkimport: inspect target directory %q: %w", dir, err)
	case len(entries) == 0:
		return nil
	}
	names := make([]string, 0, len(entries))
	for i, e := range entries {
		if i == 4 {
			names = append(names, "...")
			break
		}
		names = append(names, e.Name())
	}
	return fmt.Errorf("%w: %q holds %d entries (%v). This path writes no WAL, so "+
		"importing into a directory that already holds one would have recovery replay that "+
		"WAL on top of the new snapshot. Import into a fresh directory",
		ErrStoreNotEmpty, dir, len(entries), names)
}

// ImportInto is the one-call form: it builds a graph from nodes and edges and
// publishes it into storeDir. It is the entry point most callers want; use
// [Builder] plus [Publish] directly when the records must be streamed rather than
// held in slices.
//
// The contract is [Publish]'s in full: storeDir must be absent or empty, the
// import is atomic as a whole and is not a transaction, and it is concurrent with
// nothing.
func ImportInto[W any](
	ctx context.Context, storeDir string, opts Options, nodes []Node, edges []Edge[W],
) (PublishResult, error) {
	var res PublishResult
	// Refuse before doing any work, so a caller with a bad target does not pay for
	// the whole build first.
	if err := requireEmptyDir(storeDir); err != nil {
		return res, err
	}
	if opts.ExpectNodes == 0 {
		opts.ExpectNodes = len(nodes)
	}
	b := New[W](opts)
	if err := b.AddNodes(nodes); err != nil {
		return res, err
	}
	if err := b.AddEdges(edges); err != nil {
		return res, err
	}
	if _, err := b.Finish(); err != nil {
		return res, err
	}
	return Publish[W](ctx, storeDir, b)
}
