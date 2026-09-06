package checkpoint

// unreadable_snapshot_gate_test.go — regression tests for rmp #2749.
//
// THE DEFECT: the only gate standing between a published snapshot and the
// destruction of the WAL prefix behind it — [Checkpointer.snapshotIsSelfSufficient]
// — read manifest.json and matched file NAMES. It never opened a component,
// never parsed one, and never asked any reader whether the image it was about
// to rely on could be understood. It returned true and truncatePrefixLocked
// called [wal.Writer.TruncatePrefix].
//
// rmp #2743 closed ONE route to that state (an oversize field is now refused at
// capture time). It did not close the gate. Every other reason a snapshot can be
// byte-perfect and still unreadable — a component format version the reader does
// not accept, a reader bound tightened without the writer, a codec the two sides
// disagree on — produced the identical outcome: WAL truncated, store never
// opens again, committed data gone.
//
// THE FIX: phase 2 now parses the published image with the SAME reader recovery
// uses (snapshot.LoadSnapshotFull, via the snapshotBackend seam) before the WAL
// is touched at all. The gate's name — "self-sufficient", i.e. "this snapshot
// alone can restore the store" — is now established rather than asserted.
//
// The two tests below are deliberately complementary:
//
//   - TestCheckpoint_UnreadableSnapshot_DoesNotTruncateWAL proves the gate now
//     REFUSES an image it used to wave through, and that the data survives the
//     refusal. It fails on the pre-fix code.
//   - TestCheckpoint_PermittedTruncation_LeavesARecoverableStore proves the gate
//     still PERMITS a good image, and that what it permitted really is
//     recoverable from the snapshot alone. Without it the first test would be
//     satisfied by a gate that refuses everything.

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// gateCastagnoli is the CRC32C table the snapshot writer records component
// checksums with (store/snapshot/writer.go). The poisoning helper below
// recomputes an entry with it so the damaged snapshot is CRC-VALID: the point
// of the test is an image whose integrity check passes and whose PARSE fails,
// which is exactly the class the manifest-name gate could not see.
var gateCastagnoli = crc32.MakeTable(crc32.Castagnoli)

// poisonPropertiesVersion rewrites the format-version field in the snapshot's
// properties.bin from 1 to 2 and then re-stamps the manifest entry with the new
// CRC32C and size, so the published snapshot is internally consistent and
// passes every integrity check the reader performs BEFORE parsing.
//
// The damage is deliberately NOT a length/bound violation: rmp #2743 already
// closed that route at capture time and the task explicitly scopes it out. An
// unsupported format version is the "future format field / codec change"
// scenario — snapshot.ReadProperties refuses it at properties.go:796 with
// ErrPropertiesCorrupted — and it is reachable in production by nothing more
// exotic than a reader and a writer that disagree about a version number.
func poisonPropertiesVersion(dir string) error {
	mPath := filepath.Join(dir, "manifest.json")
	m, err := snapshot.ReadManifestFile(mPath)
	if err != nil {
		return err
	}
	idx := -1
	for i := range m.Files {
		if m.Files[i].Name == snapshot.PropertiesFile {
			idx = i
			break
		}
	}
	if idx < 0 {
		// A test that poisons nothing would pass vacuously: fail loudly instead.
		return errors.New("poisonPropertiesVersion: snapshot declares no " +
			snapshot.PropertiesFile + " — the fixture wrote no properties")
	}

	pPath := filepath.Join(dir, snapshot.PropertiesFile)
	b, err := os.ReadFile(pPath) //nolint:gosec // test-owned path under t.TempDir()
	if err != nil {
		return err
	}
	if len(b) < 8 {
		return errors.New("poisonPropertiesVersion: properties.bin shorter than its header")
	}
	// Layout: [0:4] magic 'SPRP', [4:8] format version. Leave the magic alone so
	// the reader gets past it and refuses on the VERSION — a snapshot written by
	// a future GoGraph, not a random corruption.
	binary.LittleEndian.PutUint32(b[4:8], 2)
	if err := os.WriteFile(pPath, b, 0o600); err != nil { //nolint:gosec // G703: test-owned path built from the t.TempDir() the fixture just wrote
		return err
	}

	m.Files[idx].Size = int64(len(b))
	m.Files[idx].CRC32C = crc32.Checksum(b, gateCastagnoli)

	f, err := os.OpenFile(mPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // test-owned path under t.TempDir()
	if err != nil {
		return err
	}
	if err := snapshot.WriteManifest(f, m); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// poisonBackend is the production snapshot backend with ONE behaviour changed:
// the image it publishes is left unreadable. Everything else — the capture, the
// manifest read, and above all the readback under test — is the embedded
// [osSnapshotBackend], so the test drives the real production code path and not
// a mock of it.
//
// This models the real failure mode faithfully: the checkpointer wrote what it
// meant to write, the bytes are intact, the CRC agrees, and the reader still
// cannot parse them.
type poisonBackend[N comparable, W any] struct {
	osSnapshotBackend[N, W]
	err *error
}

func (p poisonBackend[N, W]) WriteCapture(snapDir string, capt *snapshot.Capture[W], constraints []snapshot.ConstraintSpec, indexDefs []snapshot.IndexDefSpec) error {
	if err := p.osSnapshotBackend.WriteCapture(snapDir, capt, constraints, indexDefs); err != nil {
		return err
	}
	*p.err = poisonPropertiesVersion(snapDir)
	return nil
}

// TestCheckpoint_UnreadableSnapshot_DoesNotTruncateWAL is the rmp #2749
// regression gate.
//
// It publishes a snapshot that is CRC-valid and structurally unreadable, then
// asserts the WAL prefix is NOT discarded and that the committed data is still
// there afterwards.
//
// PRE-FIX BEHAVIOUR (verified by running this test against the unfixed tree):
// snapshotIsSelfSufficient saw mapper.bin in the manifest, returned true,
// TruncatePrefix ran, and Stats().WALTruncBytes was non-zero — the WAL that
// held the only readable copy of the data was gone.
func TestCheckpoint_UnreadableSnapshot_DoesNotTruncateWAL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	// Seed committed state. Node PROPERTIES are what make properties.bin exist,
	// and properties.bin is what the fixture poisons.
	type edge struct{ src, dst string }
	edges := []edge{{"alice", "bob"}, {"bob", "carol"}, {"carol", "dave"}}
	for _, e := range edges {
		tx := store.Begin()
		if err := tx.AddEdge(e.src, e.dst, 1); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e.src, e.dst, err)
		}
		if err := tx.SetNodeLabel(e.src, "Person"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", e.src, err)
		}
		if err := tx.SetNodeProperty(e.src, "name", lpg.StringValue(e.src)); err != nil {
			t.Fatalf("SetNodeProperty(%s): %v", e.src, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	walBefore := fileSize(t, walPath)
	if walBefore == 0 {
		t.Fatal("WAL is empty before the checkpoint: the fixture committed nothing, " +
			"so a 'WAL not truncated' assertion could not fail")
	}

	var poisonErr error
	var mu sync.Mutex
	cp := New[string, int64](Config{Dir: dir}, g, w, &mu,
		WithMapperCodec[string, int64](txn.NewStringCodec()),
		WithWeightCodec[string, int64](txn.NewInt64WeightCodec()),
		WithSnapshotFS[string, int64](poisonBackend[string, int64]{err: &poisonErr}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)

	trigErr := cp.Trigger()
	cp.Stop()

	if poisonErr != nil {
		t.Fatalf("fixture failed to poison the published snapshot: %v", poisonErr)
	}

	// 1. The checkpoint must fail LOUDLY. An unreadable published image is
	//    corruption, not a supported degraded mode, so it is fail-stop rather
	//    than the silent WAL-retention path a non-self-sufficient image takes.
	if trigErr == nil {
		t.Error("Trigger returned nil for a snapshot the reader cannot parse: " +
			"the checkpoint reported success on an image recovery would refuse")
	}
	if got := cp.Stats().LastError; got == "" {
		t.Error("Stats().LastError is empty after publishing an unreadable snapshot")
	}

	// 2. THE DURABILITY ASSERTION: the WAL prefix must still be there. This is
	//    the assertion that fails on the pre-fix code.
	if got := cp.Stats().WALTruncBytes; got != 0 {
		t.Errorf("WALTruncBytes = %d, want 0: the checkpointer discarded the WAL prefix "+
			"behind a snapshot nothing can read — committed data is unrecoverable", got)
	}
	if got := fileSize(t, walPath); got != walBefore {
		t.Errorf("WAL size = %d, want %d (unchanged): the WAL was truncated behind an "+
			"unreadable snapshot", got, walBefore)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// 3. And prove the retention was WORTH something: with the poisoned snapshot
	//    removed, the retained WAL alone still reconstructs every committed
	//    transaction. This is what the pre-fix truncation destroyed.
	if err := os.RemoveAll(filepath.Join(dir, "snapshot")); err != nil {
		t.Fatalf("remove poisoned snapshot: %v", err)
	}
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open from the retained WAL: %v", err)
	}
	if res.WALOps == 0 {
		t.Error("WALOps = 0 recovering from the WAL alone: nothing was replayed, so this " +
			"check could not have detected a truncation")
	}
	for _, e := range edges {
		if !res.Graph.AdjList().HasEdge(e.src, e.dst) {
			t.Errorf("edge %s->%s lost: the retained WAL did not restore committed state",
				e.src, e.dst)
		}
	}
}

// TestCheckpoint_PermittedTruncation_LeavesARecoverableStore is the other half
// of the gate's contract, and the reason the test above cannot be satisfied by
// a gate that simply refuses everything.
//
// It runs an ordinary checkpoint on an undamaged store, asserts the gate DID
// permit the truncation (WALTruncBytes > 0), then erases the WAL entirely and
// asserts recovery rebuilds the exact committed graph from the snapshot alone,
// with WALOps == 0. That is the guarantee the gate's name makes, demonstrated
// on the artefact the gate actually approved rather than assumed from the
// approval.
func TestCheckpoint_PermittedTruncation_LeavesARecoverableStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})

	type edge struct{ src, dst string }
	edges := []edge{{"alice", "bob"}, {"bob", "carol"}, {"carol", "dave"}}
	for _, e := range edges {
		tx := store.Begin()
		if err := tx.AddEdge(e.src, e.dst, 1); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e.src, e.dst, err)
		}
		if err := tx.SetNodeLabel(e.src, "Person"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", e.src, err)
		}
		if err := tx.SetNodeProperty(e.src, "name", lpg.StringValue(e.src)); err != nil {
			t.Fatalf("SetNodeProperty(%s): %v", e.src, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	wantOrder := g.AdjList().Order()
	wantSize := g.AdjList().Size()

	var mu sync.Mutex
	cp := New[string, int64](Config{Dir: dir}, g, w, &mu,
		WithMapperCodec[string, int64](txn.NewStringCodec()),
		WithWeightCodec[string, int64](txn.NewInt64WeightCodec()),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	cp.Stop()

	// The gate must have PERMITTED the truncation — otherwise the guarantee
	// below would be about an artefact the gate never approved.
	if got := cp.Stats().WALTruncBytes; got == 0 {
		t.Fatal("WALTruncBytes = 0: the gate refused a perfectly good snapshot, so the " +
			"readback rejects images it must accept")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Erase the WAL completely: whatever recovery finds now came from the
	// snapshot the gate approved, and from nowhere else.
	if err := os.Truncate(walPath, 0); err != nil {
		t.Fatalf("truncate WAL: %v", err)
	}

	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open from the snapshot alone: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false: recovery did not read the snapshot at all")
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0: the WAL still contributed, so this is not a "+
			"snapshot-alone recovery", res.WALOps)
	}
	if got := res.Graph.AdjList().Order(); got != wantOrder {
		t.Errorf("Order = %d, want %d", got, wantOrder)
	}
	if got := res.Graph.AdjList().Size(); got != wantSize {
		t.Errorf("Size = %d, want %d", got, wantSize)
	}
	for _, e := range edges {
		if !res.Graph.AdjList().HasEdge(e.src, e.dst) {
			t.Errorf("edge %s->%s missing after snapshot-alone recovery", e.src, e.dst)
		}
		if !res.Graph.HasNodeLabel(e.src, "Person") {
			t.Errorf("label Person on %s missing after snapshot-alone recovery", e.src)
		}
		v, ok := res.Graph.GetNodeProperty(e.src, "name")
		if !ok {
			t.Errorf("property %s.name missing after snapshot-alone recovery", e.src)
			continue
		}
		if s, _ := v.String(); s != e.src {
			t.Errorf("property %s.name = %q, want %q", e.src, s, e.src)
		}
	}
}

// fileSize returns the size of path, failing the test if it cannot be stat-ed.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}
