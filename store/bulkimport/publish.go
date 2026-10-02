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
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
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
// exercises. The one step the import adds is for the store directory itself:
// when the import creates storeDir or any of its ancestors, it fsyncs each
// directory it created and the first ancestor that already existed before
// writing the snapshot, so the store directory's own entry survives a host
// crash once the import is acknowledged (rmp #2970).
// # Weight types
//
// Publish persists edge weights whose Go type has a fixed width the CSR writer
// knows (the built-in integer, float and bool primitives). For any other weight
// type — a struct, a NAMED integer type such as [time.Duration], a string — it
// returns [snapshot.ErrWeightNotPersistable] and publishes nothing; use
// [PublishWithWeightCodec] to supply a codec for those.
//
// Failing is the point. Until rmp #2526 this path wrote such a graph as having
// NO weights, byte-identical to a genuinely weightless import, and reported
// success. Bulk import bypasses the WAL entirely, so unlike the checkpoint path
// there was no second copy anywhere: the weights were gone the moment the
// import returned nil.
func Publish[W any](ctx context.Context, storeDir string, b *Builder[W]) (PublishResult, error) {
	return PublishWithWeightCodec[W](ctx, storeDir, b, nil)
}

// PublishWithWeightCodec is the weight-codec-aware variant of [Publish]. wcodec
// encodes edge weights of any type W into the snapshot's CSR weights column —
// pass the codec the resulting store will be opened with, so the weights it
// reads back are the ones written here.
//
// The codec is consulted only for weight types the fixed-width layout cannot
// size, so a float64 or int64 import publishes byte-identical bytes with or
// without it. A nil wcodec behaves exactly as [Publish].
func PublishWithWeightCodec[W any](ctx context.Context, storeDir string, b *Builder[W], wcodec txn.WeightCodec[W]) (PublishResult, error) {
	return publish[W](ctx, nil, storeDir, b, wcodec)
}

// fileSystem is the filesystem seam [PublishFS] and [ImportIntoFS] publish
// through. Its method set is the store/snapshot package's own seam plus
// ReadDir, which the empty-directory check needs, so one value both serves this
// package's directory operations and is handed on unchanged to
// [snapshot.WriteSnapshotFullWithWeightCodecCtxFS] for the snapshot write.
//
// The interface is intentionally unexported, exactly as store/snapshot's is: an
// external package cannot name it, but it can pass any value that satisfies it.
// Production callers use [Publish], [PublishWithWeightCodec] and [ImportInto],
// which perform the same operations directly against the operating system; the
// deterministic-simulation harness (internal/sim) supplies an in-memory disk so
// it can inject ENOSPC, sync and rename faults into a publish and crash it part
// way through (rmp #2518).
type fileSystem interface {
	// ReadDir lists the entries of dir. A missing dir is reported with an error
	// wrapping [fs.ErrNotExist], as [os.ReadDir] does.
	ReadDir(dir string) ([]fs.DirEntry, error)
	MkdirAll(dir string, perm fs.FileMode) error
	Create(path string) (snapshot.File, error)
	OpenComponent(path string) (snapshot.ReadFile, error)
	Open(path string) (snapshot.ReadFile, error)
	Rename(oldPath, newPath string) error
	Remove(path string) error
	RemoveAll(path string) error
	Stat(path string) (fs.FileInfo, error)
	DirSync(path string) error
	ParentDirSync(childPath string) error
}

// errNilFS is returned by the seamed entry points when given a nil filesystem.
var errNilFS = errors.New("bulkimport: nil filesystem")

// PublishFS is [PublishWithWeightCodec] over a caller-supplied filesystem: the
// empty-directory check, the store-directory creation and the whole snapshot
// write go through fsys instead of the operating system. The contract is
// [Publish]'s in full, including what is and is not atomic; a nil wcodec
// behaves exactly as [Publish].
//
// It exists for the deterministic-simulation harness, which backs fsys with an
// in-memory disk to prove the publish is all-or-nothing under injected faults.
// The fsys parameter type is intentionally unexported; see [fileSystem]. A nil
// fsys is refused with an error rather than silently falling back to the
// operating system.
func PublishFS[W any](
	ctx context.Context, fsys fileSystem, storeDir string, b *Builder[W], wcodec txn.WeightCodec[W],
) (PublishResult, error) {
	if fsys == nil {
		return PublishResult{}, errNilFS
	}
	return publish[W](ctx, fsys, storeDir, b, wcodec)
}

// publish is the body shared by the OS-backed and seamed entry points. A nil
// fsys selects the operating system, through the same calls the seamed entry
// points make on fsys.
func publish[W any](
	ctx context.Context, fsys fileSystem, storeDir string, b *Builder[W], wcodec txn.WeightCodec[W],
) (PublishResult, error) {
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
	if err := requireEmptyDir(fsys, storeDir); err != nil {
		return res, err
	}
	if err := mkdirAllDurable(fsys, storeDir); err != nil {
		return res, fmt.Errorf("bulkimport: create store directory %q: %w", storeDir, err)
	}

	snapDir := filepath.Join(storeDir, snapshotName)
	c := csr.BuildFromAdjList[string, W](g.AdjList())
	// Branch rather than assign through a variable: a nil txn.WeightCodec must
	// reach the snapshot package as an untyped nil, or its own nil check sees a
	// non-nil interface holding a nil value and it calls through it.
	var perr error
	switch {
	case fsys != nil && wcodec != nil:
		perr = snapshot.WriteSnapshotFullWithWeightCodecCtxFS[string, W](ctx, fsys, snapDir, c, g, wcodec)
	case fsys != nil:
		perr = snapshot.WriteSnapshotFullWithWeightCodecCtxFS[string, W](ctx, fsys, snapDir, c, g, nil)
	case wcodec != nil:
		perr = snapshot.WriteSnapshotFullWithWeightCodecCtx[string, W](ctx, snapDir, c, g, wcodec)
	default:
		perr = snapshot.WriteSnapshotFullCtx[string, W](ctx, snapDir, c, g)
	}
	if perr != nil {
		return res, fmt.Errorf("bulkimport: publish snapshot to %q: %w", snapDir, perr)
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
func requireEmptyDir(fsys fileSystem, dir string) error {
	var (
		entries []fs.DirEntry
		err     error
	)
	if fsys != nil {
		entries, err = fsys.ReadDir(dir)
	} else {
		entries, err = os.ReadDir(dir)
	}
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
	return importInto[W](ctx, nil, storeDir, opts, nodes, edges)
}

// ImportIntoFS is [ImportInto] over a caller-supplied filesystem; every
// filesystem operation of the import goes through fsys, as on [PublishFS]. The
// fsys parameter type is intentionally unexported; see [fileSystem]. A nil fsys
// is refused with an error.
func ImportIntoFS[W any](
	ctx context.Context, fsys fileSystem, storeDir string, opts Options, nodes []Node, edges []Edge[W],
) (PublishResult, error) {
	if fsys == nil {
		return PublishResult{}, errNilFS
	}
	return importInto[W](ctx, fsys, storeDir, opts, nodes, edges)
}

// importInto is the body shared by [ImportInto] and [ImportIntoFS]; a nil fsys
// selects the operating system.
func importInto[W any](
	ctx context.Context, fsys fileSystem, storeDir string, opts Options, nodes []Node, edges []Edge[W],
) (PublishResult, error) {
	var res PublishResult
	// Refuse before doing any work, so a caller with a bad target does not pay for
	// the whole build first.
	if err := requireEmptyDir(fsys, storeDir); err != nil {
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
	return publish[W](ctx, fsys, storeDir, b, nil)
}

// mkdirAllDurable creates dir and any missing ancestors through fsys, or
// through the operating system when fsys is nil, and makes every directory
// entry it created durable before returning (rmp #2970).
//
// A new directory's name lives in its parent, and on POSIX filesystems that
// name is durable only once the parent is fsynced. The snapshot write fsyncs
// dir itself after its publish rename, but not dir's parent; without the syncs
// here a host crash after an acknowledged publish can drop the store
// directory's entry, and recovery then finds no store at all. So, deepest
// first, it fsyncs each directory it created and then the first ancestor that
// already existed.
//
// When dir already exists nothing is created and nothing is synced. The
// ancestors are probed with Stat before the MkdirAll; a [Publish] is concurrent
// with nothing, so no other writer can create one of them in between.
func mkdirAllDurable(fsys fileSystem, dir string) error {
	created := missingDirs(fsys, dir)
	var err error
	if fsys != nil {
		err = fsys.MkdirAll(dir, 0o750)
	} else {
		err = os.MkdirAll(dir, 0o750)
	}
	if err != nil {
		return err
	}
	if len(created) == 0 {
		return nil
	}
	// created is deepest first; append its last entry's parent, the first
	// pre-existing ancestor, so it is synced last.
	created = append(created, filepath.Dir(created[len(created)-1]))
	for _, d := range created {
		if fsys != nil {
			err = fsys.DirSync(d)
		} else {
			err = dirFsync(d)
		}
		if err != nil {
			return fmt.Errorf("fsync directory %q: %w", d, err)
		}
	}
	return nil
}

// missingDirs returns dir and each of its ancestors that does not exist,
// deepest first, stopping at the first one that exists. A path that is its own
// parent — the working directory "." or a filesystem root — always exists and
// is not probed. An ancestor whose Stat fails for a reason other than
// non-existence ends the walk; the MkdirAll that follows reports that
// condition.
func missingDirs(fsys fileSystem, dir string) []string {
	var created []string
	for p := filepath.Clean(dir); filepath.Dir(p) != p; p = filepath.Dir(p) {
		var err error
		if fsys != nil {
			_, err = fsys.Stat(p)
		} else {
			_, err = os.Stat(p)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			break
		}
		created = append(created, p)
	}
	return created
}
