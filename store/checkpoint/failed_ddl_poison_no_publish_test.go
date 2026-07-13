package checkpoint_test

// failed_ddl_poison_no_publish_test.go — regression for rmp #1919 / backlog
// #1902: a CREATE/DROP CONSTRAINT (or the sibling INDEX DDL) that FAILS at WAL
// fsync must leave NO durable effect across a restart, even when a concurrent
// non-blocking checkpoint runs in the failed-DDL window.
//
// The defect: the DDL updates the engine's in-memory registry (RegisterUnique /
// UnregisterUnique / recordIndexDef / forgetIndexDef) BEFORE committing the WAL
// frame, and its compensator (unwindConstraintRegistration / rewindConstraintDrop
// / the inline forgetIndexDef) runs OUTSIDE the store single-writer lock. On an
// fsync failure the WAL poison-truncates the frame (DurableOffset excludes it)
// and permanently fail-stops the writer. A checkpoint that captures in the window
// AFTER the DDL released its in-flight token but BEFORE the compensator runs sees
// the registry in its transient state (constraint/index registered on a failed
// CREATE, or absent on a failed DROP) while the WAL no longer backs it. The old
// code only detected the poison at the phase-2 wlog.Sync() — AFTER writeSnapshot
// had already published the transient constraints.bin / indexdefs.bin — so a
// clean restart loaded the transient snapshot and enforced (CREATE) or applied
// (DROP) a schema change the client saw fail. That violates ACID Atomicity.
//
// The fix: the checkpoint verifies WAL health (wal.Writer.Poisoned) under the
// commit lock at phase-1 capture, BEFORE capturing the registry or publishing
// anything, and aborts when poisoned. Because the poison is applied inside
// SyncGroup before the DDL's in-flight token is released, and the capture runs
// only after RunUnderCommitLock drains in-flight commits to zero, a writer
// poisoned at capture time is exactly the transient window — so no transient
// state can ever reach the snapshot, for both the constraint and index paths.
//
// How this test reproduces the (poisoned WAL, transient registry) pair
// deterministically, without racing the sub-microsecond compensator window:
//
//   - The WAL poison is REAL: a fault-injecting walFS (faultWALFS, wired via
//     wal.OpenFS) fails exactly one fsync, driving the real Writer.poison path
//     (frame discarded, DurableOffset frozen, sticky syncErr set) — the same
//     state a rare commit-time I/O error produces.
//   - The transient registry is modelled by the checkpointer's capture callback:
//     the engine's ConstraintSpecsForSnapshot / IndexSpecsForSnapshot read the
//     live registry (cypher/api.go), which in the failed-DDL window transiently
//     contains the phantom CREATE (registered at createConstraintLocked before
//     commitConstraintTx; unwound only after CommitWALOnly returns) or omits the
//     phantom DROP. The callback returns exactly that set — the precise input the
//     checkpoint would capture in the race — paired with the genuinely poisoned
//     WAL.
//
// The assertion is the durability property itself: after a clean reopen the
// failed CREATE must be ABSENT and the failed DROP must have left the constraint
// PRESENT. Trigger itself returns an error in BOTH the fixed and unfixed code
// (the WAL is poisoned either way); the load-bearing difference is whether the
// transient snapshot was PUBLISHED, which only the post-reopen state reveals.
//
// Layer: short. goleak-clean (checkpointer stopped, WAL closed, context cancelled).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// errInjectedFsync is the fault a faultWALFile returns for the one Sync a test
// arms, poisoning the WAL exactly as a rare commit-time I/O error would.
var errInjectedFsync = errors.New("failed_ddl_poison_test: injected fsync failure")

// faultWALFS delegates every WAL filesystem operation to the real OS but wraps
// each opened file so a single fsync can be made to fail on demand. It
// structurally satisfies the wal package's unexported walFS interface (all its
// methods are exported and its file type is the exported wal.WALFile), so it can
// be passed to wal.OpenFS — the same seam the deterministic-simulation harness
// uses. Routing through OpenFS (not the path-less OpenWith) keeps the real,
// crash-safe TruncatePrefix working for the happy checkpoint #1 while leaving the
// per-commit fsync injectable for the poison.
type faultWALFS struct {
	failNextSync atomic.Bool // when set, the NEXT faultWALFile.Sync fails once
}

func (fs *faultWALFS) OpenFile(path string, flag int) (wal.WALFile, error) {
	f, err := os.OpenFile(path, flag, 0o600) //nolint:gosec // test-controlled temp path
	if err != nil {
		return nil, err
	}
	return &faultWALFile{File: f, fs: fs}, nil
}

func (fs *faultWALFS) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func (fs *faultWALFS) Remove(path string) error { return os.Remove(path) }

func (fs *faultWALFS) ParentDirSync(childPath string) error {
	d, err := os.Open(filepath.Dir(childPath))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// faultWALFile wraps an *os.File and overrides Sync so a single, armed fsync
// fails. It is deliberately NOT an *os.File, so wal.dataSync's *os.File type
// assertion falls through to this Sync on every platform (Linux fdatasync path
// included), and the injected fault fires on the commit hot path.
type faultWALFile struct {
	*os.File
	fs *faultWALFS
}

func (f *faultWALFile) Sync() error {
	if f.fs.failNextSync.CompareAndSwap(true, false) {
		return errInjectedFsync
	}
	return f.File.Sync()
}

// armAndPoison injects one fsync failure and drives a single Sync so the WAL
// writer fail-stops (poisons) exactly as a rare commit-time I/O error would,
// leaving DurableOffset at the last durable frame boundary. It is the terminal
// state a failed CREATE/DROP DDL leaves the shared WAL in.
func armAndPoison(t *testing.T, w *wal.Writer, fs *faultWALFS) {
	t.Helper()
	fs.failNextSync.Store(true)
	if err := w.Sync(); err == nil {
		t.Fatalf("armAndPoison: expected injected fsync failure to poison the WAL, got nil")
	}
	if perr := w.Poisoned(); perr == nil {
		t.Fatalf("armAndPoison: WAL not reported poisoned after injected fsync failure")
	}
}

// TestCheckpointer_FailedCreateConstraintNotPersistedUnderPoison: a CREATE
// CONSTRAINT whose WAL fsync fails, captured by a concurrent checkpoint in the
// transient window, must NOT be enforced after a clean restart.
func TestCheckpointer_FailedCreateConstraintNotPersistedUnderPoison(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	fs := &faultWALFS{}
	w, err := wal.OpenFS(fs, walPath)
	if err != nil {
		t.Fatalf("wal.OpenFS: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())
	eng := cypher.NewEngineWithStore(store)

	// A durable, acknowledged constraint that MUST survive.
	if err := csRunOne(t, eng, `CREATE CONSTRAINT c_keep FOR (n:Person) REQUIRE n.email IS UNIQUE`); err != nil {
		t.Fatalf("CREATE c_keep: %v", err)
	}

	// The transient registry: in the failed-CREATE window the registry holds the
	// phantom (registered before the WAL commit, not yet unwound). Model exactly
	// that captured set, gated so checkpoint #1 sees only the acknowledged state.
	var injectPhantom atomic.Bool
	constraintsFn := func() []snapshot.ConstraintSpec {
		specs := eng.ConstraintSpecsForSnapshot()
		if injectPhantom.Load() {
			specs = append(specs, snapshot.ConstraintSpec{Kind: 0, Label: "Account", Property: "ssn", Name: "c_phantom"})
		}
		return specs
	}

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](store.Codec()),
		checkpoint.WithConstraintSpecs[string, float64](constraintsFn),
		checkpoint.WithIndexSpecs[string, float64](eng.IndexSpecsForSnapshot),
	)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(cctx)

	// Checkpoint #1 (healthy): fold the acknowledged constraint into a
	// self-sufficient snapshot and truncate the WAL prefix.
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint #1: %v", terr)
	}

	// Enter the failed-DDL window: phantom in the registry, WAL poisoned.
	injectPhantom.Store(true)
	armAndPoison(t, w, fs)

	// Checkpoint #2 captures (poisoned WAL, transient registry). It must abort
	// before publishing, so the phantom never reaches constraints.bin. The
	// poisoned WAL makes Trigger fail either way; the durability property is
	// checked after reopen.
	if terr := cp.Trigger(); terr == nil {
		t.Fatalf("checkpoint #2 unexpectedly succeeded on a poisoned WAL")
	}
	cp.Stop()
	_ = w.Close() // returns the sticky poison error; the on-disk WAL is already durable

	// Restart: c_keep must survive; c_phantom must be ABSENT.
	res, err := recovery.Open[string, float64](dir, csRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	got := map[string]bool{}
	for i := range res.Constraints {
		got[res.Constraints[i].Name] = true
	}
	if !got["c_keep"] {
		t.Errorf("acknowledged constraint c_keep was lost; recovered=%v", res.Constraints)
	}
	if got["c_phantom"] {
		t.Errorf("failed CREATE CONSTRAINT c_phantom was PERSISTED across restart (rmp #1919); recovered=%v", res.Constraints)
	}
}

// TestCheckpointer_FailedDropConstraintNotPersistedUnderPoison: a DROP CONSTRAINT
// whose WAL fsync fails, captured by a concurrent checkpoint in the transient
// window, must NOT take effect — the constraint must still be present after a
// clean restart.
func TestCheckpointer_FailedDropConstraintNotPersistedUnderPoison(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	fs := &faultWALFS{}
	w, err := wal.OpenFS(fs, walPath)
	if err != nil {
		t.Fatalf("wal.OpenFS: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())
	eng := cypher.NewEngineWithStore(store)

	if err := csRunOne(t, eng, `CREATE CONSTRAINT c_keep FOR (n:Person) REQUIRE n.email IS UNIQUE`); err != nil {
		t.Fatalf("CREATE c_keep: %v", err)
	}
	if err := csRunOne(t, eng, `CREATE CONSTRAINT c_drop FOR (n:Person) REQUIRE n.nickname IS UNIQUE`); err != nil {
		t.Fatalf("CREATE c_drop: %v", err)
	}

	// The transient registry: in the failed-DROP window the registry has already
	// unregistered c_drop, while its DROP frame is poison-truncated (never
	// durable). Model that by hiding c_drop from the captured set once armed.
	var hideDrop atomic.Bool
	constraintsFn := func() []snapshot.ConstraintSpec {
		specs := eng.ConstraintSpecsForSnapshot()
		if !hideDrop.Load() {
			return specs
		}
		out := make([]snapshot.ConstraintSpec, 0, len(specs))
		for _, s := range specs {
			if s.Name != "c_drop" {
				out = append(out, s)
			}
		}
		return out
	}

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](store.Codec()),
		checkpoint.WithConstraintSpecs[string, float64](constraintsFn),
		checkpoint.WithIndexSpecs[string, float64](eng.IndexSpecsForSnapshot),
	)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(cctx)

	// Checkpoint #1 (healthy): fold BOTH constraints into the snapshot and
	// truncate the WAL prefix, so c_drop now lives ONLY in constraints.bin — a
	// later stale capture that omits it, if published, would lose it for good.
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint #1: %v", terr)
	}

	// Enter the failed-DROP window: c_drop unregistered, WAL poisoned.
	hideDrop.Store(true)
	armAndPoison(t, w, fs)

	// Checkpoint #2 must abort before publishing a snapshot that omits c_drop.
	if terr := cp.Trigger(); terr == nil {
		t.Fatalf("checkpoint #2 unexpectedly succeeded on a poisoned WAL")
	}
	cp.Stop()
	_ = w.Close()

	// Restart: both constraints must be present (the failed DROP had no effect).
	res, err := recovery.Open[string, float64](dir, csRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	got := map[string]bool{}
	for i := range res.Constraints {
		got[res.Constraints[i].Name] = true
	}
	if !got["c_keep"] {
		t.Errorf("surviving constraint c_keep was lost; recovered=%v", res.Constraints)
	}
	if !got["c_drop"] {
		t.Errorf("failed DROP CONSTRAINT c_drop TOOK EFFECT across restart (rmp #1919); recovered=%v", res.Constraints)
	}
}

// TestCheckpointer_FailedCreateIndexNotPersistedUnderPoison proves the fix also
// closes the identical window on the sibling INDEX DDL path: a CREATE INDEX whose
// WAL fsync fails, captured in the transient window, must NOT be present after a
// clean restart.
func TestCheckpointer_FailedCreateIndexNotPersistedUnderPoison(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	fs := &faultWALFS{}
	w, err := wal.OpenFS(fs, walPath)
	if err != nil {
		t.Fatalf("wal.OpenFS: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())
	eng := cypher.NewEngineWithStore(store)

	if err := csRunOne(t, eng, `CREATE INDEX ix_keep FOR (n:Person) ON (n.email)`); err != nil {
		t.Fatalf("CREATE INDEX ix_keep: %v", err)
	}

	var injectPhantomIdx atomic.Bool
	indexDefsFn := func() []snapshot.IndexDefSpec {
		specs := eng.IndexSpecsForSnapshot()
		if injectPhantomIdx.Load() {
			specs = append(specs, snapshot.IndexDefSpec{Kind: 1, Name: "ix_phantom", Label: "Account", Property: "ssn"})
		}
		return specs
	}

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](store.Codec()),
		checkpoint.WithConstraintSpecs[string, float64](eng.ConstraintSpecsForSnapshot),
		checkpoint.WithIndexSpecs[string, float64](indexDefsFn),
	)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(cctx)

	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint #1: %v", terr)
	}

	injectPhantomIdx.Store(true)
	armAndPoison(t, w, fs)

	if terr := cp.Trigger(); terr == nil {
		t.Fatalf("checkpoint #2 unexpectedly succeeded on a poisoned WAL")
	}
	cp.Stop()
	_ = w.Close()

	res, err := recovery.Open[string, float64](dir, csRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	got := map[string]bool{}
	for i := range res.Indexes {
		got[res.Indexes[i].Name] = true
	}
	if !got["ix_keep"] {
		t.Errorf("acknowledged index ix_keep was lost; recovered=%v", res.Indexes)
	}
	if got["ix_phantom"] {
		t.Errorf("failed CREATE INDEX ix_phantom was PERSISTED across restart (rmp #1919); recovered=%v", res.Indexes)
	}
}
