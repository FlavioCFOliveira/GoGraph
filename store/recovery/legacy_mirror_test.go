package recovery

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// The v015_* fixtures are REAL store directories written by the v0.15.0 engine
// (git archive v0.15.0): a version-3 snapshot manifest and a single-file WAL.
// Each holds the undirected (or simple) graph its test describes; the
// v015_mirror_* stores were written by an undirected multigraph whose edge a-b
// (handle 101) carries since=2020 and, on handle 101, w=1, both written a -> b
// (node ids a=140 < b=165), and then:
//
//	snapconflict  b -> a since=1999 in the snapshot
//	snapsame      b -> a since=2020 and handle w=1 in the snapshot; WAL a -> b tail=1
//	walconflict   b -> a since=1999 in the WAL
//	walsame       b -> a since=2020 and handle w=1 in the WAL

// treeDigest returns the SHA-256 of every regular file under dir, by relative
// path, so a test can prove a directory was left byte-for-byte as it was.
func treeDigest(t *testing.T, dir string) map[string][sha256.Size]byte {
	t.Helper()
	out := map[string][sha256.Size]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // a fixture copy under t.TempDir
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p) // p is under dir by construction
		out[rel] = sha256.Sum256(b)
		return nil
	})
	if err != nil {
		t.Fatalf("digest %s: %v", dir, err)
	}
	return out
}

// TestRecovery_LegacyMirrorConflict_Refused proves the refusal (rmp #3072) on
// stores the v0.15.0 engine wrote as undirected multigraphs, whose edge a-b
// (a=140 < b=165) disagrees in its two directions once the WAL is replayed:
//
//	v015_mirror_snapconflict  since=2020 / since=1999, both in the snapshot
//	v015_mirror_walconflict   since=1999 written b -> a in the WAL
//	v015_conf_late            x=1 / x=2, both in the snapshot
//	v015_conf_early           x=1 / x=2, both in the WAL
//	v015_slot_wal             relationships 101 and 102 typed T, U from a and
//	                          U, T from b (SetEdgeLabel labels only the free
//	                          slots of the direction it names), in the WAL
//	v015_slot_snap            the same, in the snapshot
//
// Open returns an error wrapping ErrLegacyMirrorConflict that names the value,
// and the directory is left byte-for-byte as it was, so a second Open refuses
// identically.
func TestRecovery_LegacyMirrorConflict_Refused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ fixture, needle string }{
		{"v015_mirror_snapconflict", `property "since"`},
		{"v015_mirror_walconflict", `property "since"`},
		{"v015_conf_late", `property "x"`},
		{"v015_conf_early", `property "x"`},
		{"v015_slot_wal", "relationship 101 type"},
		{"v015_slot_snap", "relationship 102 type"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			dir := copyFixture(t, tc.fixture)
			requireLegacyManifest(t, filepath.Join(dir, "snapshot"), `"directed": false`)
			before := treeDigest(t, dir)
			for attempt := 1; attempt <= 2; attempt++ {
				_, err := Open[string, int64](dir, legacyOpts())
				if !errors.Is(err, ErrLegacyMirrorConflict) {
					t.Fatalf("Open #%d: err = %v, want ErrLegacyMirrorConflict", attempt, err)
				}
				if !strings.Contains(err.Error(), tc.needle) {
					t.Errorf("Open #%d: error %q does not name %s", attempt, err, tc.needle)
				}
				if after := treeDigest(t, dir); !reflect.DeepEqual(after, before) {
					t.Fatalf("Open #%d changed the refused store:\nbefore %v\nafter  %v", attempt, before, after)
				}
			}
		})
	}
}

// TestRecovery_LegacyMirrorAgreeing_Migrates is the counterpart of
// TestRecovery_LegacyMirrorConflict_Refused: stores the v0.15.0 engine wrote
// as undirected multigraphs whose edge a-b (handle 101) agrees in its two
// directions once the WAL is replayed migrate to the single relationship
// a -> b with the values present:
//
//	v015_mirror_snapsame  since=2020 and handle w=1 in both directions
//	v015_mirror_walsame   the same, the b -> a copies written by the WAL
//	v015_conf_delmirror   x=1 written a -> b, then x deleted through b -> a:
//	                      a no-op on that engine, so x=1 survives (F2a)
//	v015_conf_resolved    x=1 / x=2 in the snapshot, the b -> a x deleted by
//	                      the WAL, so only x=1 remains (F2c)
func TestRecovery_LegacyMirrorAgreeing_Migrates(t *testing.T) {
	t.Parallel()
	w1 := map[string]lpg.PropertyValue{"w": lpg.Int64Value(1)}
	for _, tc := range []struct {
		props       map[string]lpg.PropertyValue
		handleProps map[string]lpg.PropertyValue
		fixture     string
	}{
		{map[string]lpg.PropertyValue{"since": lpg.Int64Value(2020), "tail": lpg.Int64Value(1)}, w1, "v015_mirror_snapsame"},
		{map[string]lpg.PropertyValue{"since": lpg.Int64Value(2020)}, w1, "v015_mirror_walsame"},
		{map[string]lpg.PropertyValue{"x": lpg.Int64Value(1)}, nil, "v015_conf_delmirror"},
		{map[string]lpg.PropertyValue{"x": lpg.Int64Value(1)}, nil, "v015_conf_resolved"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			dir := copyFixture(t, tc.fixture)
			res, err := Open[string, int64](dir, legacyOpts())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			g := res.Graph
			if got := g.AdjList().Size(); got != 1 || !g.HasEdgeHandle("a", "b", 101) {
				t.Fatalf("Size = %d, a->b handles %v: want the single relationship 101 a -> b",
					got, g.AppendEdgeHandles("a", "b", nil))
			}
			if got := g.EdgeProperties("a", "b"); !reflect.DeepEqual(got, tc.props) {
				t.Errorf("a -> b properties = %v, want %v", got, tc.props)
			}
			if got := g.EdgePropertiesByHandle("a", "b", 101); len(got)+len(tc.handleProps) > 0 && !reflect.DeepEqual(got, tc.handleProps) {
				t.Errorf("relationship 101 properties = %v, want %v", got, tc.handleProps)
			}
			requireMigratedOnDisk(t, dir)
		})
	}
}

// TestRecovery_LegacyV015Undirected_Migrates recovers a store the v0.15.0
// engine wrote as an undirected multigraph: a version-3 manifest and a
// single-file WAL. Node ids: d=115, a=140, b=165, c=242. The snapshot holds
// a-b (101, since=2020 written a -> b) and c-a (102, added c -> a); the WAL
// adds d-a (108, added d -> a, handle property k=8 written a -> d), writes
// note="m" on b -> a and removes 102 through a -> c. The migrated graph is
// a -> b (101) with both properties and d -> a (108) with k=8, and the next
// open replays nothing.
func TestRecovery_LegacyV015Undirected_Migrates(t *testing.T) {
	t.Parallel()
	dir := copyFixture(t, "v015_undirected")
	requireLegacyManifest(t, filepath.Join(dir, "snapshot"), `"version": 3`)
	check := checkV015Undirected
	res, err := Open[string, int64](dir, legacyOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !res.IsClean() || res.WALOps == 0 {
		t.Fatalf("IsClean=%v WALOps=%d, want a clean replay of the single-file WAL", res.IsClean(), res.WALOps)
	}
	check(t, res.Graph)
	requireMigratedOnDisk(t, dir)
	res2, err := Open[string, int64](dir, legacyOpts())
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if res2.WALOps != 0 {
		t.Fatalf("second Open replayed %d ops, want 0", res2.WALOps)
	}
	check(t, res2.Graph)
}

// checkV015Undirected checks the migrated graph of testdata/v015_undirected.
func checkV015Undirected(t *testing.T, g *lpg.Graph[string, int64]) {
	t.Helper()
	if got := g.AdjList().Size(); got != 2 {
		t.Fatalf("Size = %d, want 2 relationships", got)
	}
	if !g.HasEdgeHandle("a", "b", 101) || !g.HasEdgeHandle("d", "a", 108) {
		t.Fatalf("handles a->b=%v d->a=%v, want relationships 101 a -> b and 108 d -> a",
			g.AppendEdgeHandles("a", "b", nil), g.AppendEdgeHandles("d", "a", nil))
	}
	want := map[string]lpg.PropertyValue{"since": lpg.Int64Value(2020), "note": lpg.StringValue("m")}
	if got := g.EdgeProperties("a", "b"); !reflect.DeepEqual(got, want) {
		t.Errorf("a -> b properties = %v, want %v", got, want)
	}
	if v, ok := g.EdgePropertyByHandle("d", "a", 108, "k"); !ok || !reflect.DeepEqual(v, lpg.Int64Value(8)) {
		t.Errorf("108 k = %v (ok=%v), want 8", v, ok)
	}
}

// TestRecovery_LegacyV015Simple_ReaddAfterDeleteIsKept recovers a store the
// v0.15.0 engine wrote as a directed SIMPLE graph: the snapshot holds a -> b
// (201); the WAL repeats a -> b (301, a no-op on that engine), adds c -> a
// (302), removes it, and adds c -> a again (303). The repeat is skipped and
// the re-add, which found the pair empty, is kept.
func TestRecovery_LegacyV015Simple_ReaddAfterDeleteIsKept(t *testing.T) {
	t.Parallel()
	dir := copyFixture(t, "v015_simple")
	requireLegacyManifest(t, filepath.Join(dir, "snapshot"), `"multigraph": false`)
	check := func(t *testing.T, g *lpg.Graph[string, int64]) {
		t.Helper()
		if got := g.AdjList().Size(); got != 2 {
			t.Fatalf("Size = %d, want 2 relationships", got)
		}
		if !g.HasEdgeHandle("a", "b", 201) || !g.HasEdgeHandle("c", "a", 303) {
			t.Errorf("handles a->b=%v c->a=%v, want 201 and 303",
				g.AppendEdgeHandles("a", "b", nil), g.AppendEdgeHandles("c", "a", nil))
		}
		if g.HasEdgeHandle("a", "b", 301) || g.HasEdgeHandle("c", "a", 302) {
			t.Error("a skipped duplicate or a removed relationship is present")
		}
	}
	res, err := Open[string, int64](dir, legacyOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	check(t, res.Graph)
	requireMigratedOnDisk(t, dir)
	res2, err := Open[string, int64](dir, legacyOpts())
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	check(t, res2.Graph)
}

// TestRecovery_LegacySimple_SkippedDuplicateReservesHandle pins F1 of the
// rmp #3072 audit: the simple-graph engine drew an edge handle for an
// insertion it then ignored as a duplicate, and its recovery seeded the handle
// counter past it, so migration must too. v015_simple_duphigh, written by the
// v0.15.0 engine as a directed simple graph, holds a -> b (201) in its
// snapshot and, in its WAL, b -> c (301) and then a repeated a -> b carrying
// handle 401, the highest the store names. NextEdgeHandle consumes a handle,
// so the graph is probed once.
//
// The migration checkpoint folds the WAL, and a snapshot persists live
// handles only, so a later reopen seeds the counter from the live maximum
// (301): no durable record names 401 any more.
func TestRecovery_LegacySimple_SkippedDuplicateReservesHandle(t *testing.T) {
	t.Parallel()
	dir := copyFixture(t, "v015_simple_duphigh")
	res, err := Open[string, int64](dir, legacyOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := res.Graph.AdjList().Size(); got != 2 || res.Graph.HasEdgeHandle("a", "b", 401) {
		t.Fatalf("Size = %d, has 401 = %v: want 2 relationships and the duplicate skipped",
			got, res.Graph.HasEdgeHandle("a", "b", 401))
	}
	if next := res.Graph.NextEdgeHandle(); next <= 401 {
		t.Fatalf("NextEdgeHandle after migration = %d, want > 401 (the skipped duplicate's handle)", next)
	}
	res2, err := Open[string, int64](dir, legacyOpts())
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if next := res2.Graph.NextEdgeHandle(); next <= 301 {
		t.Fatalf("NextEdgeHandle after reopen = %d, want > 301 (the highest live handle)", next)
	}
}

// TestReserveSkippedHandle pins both forms of the reservation: a frame that
// names its handle seeds the counter past it, and one that names none consumes
// one handle, as the simple-graph engine's insertion did.
func TestReserveSkippedHandle(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{})
	body := binary.LittleEndian.AppendUint64([]byte{0, 0}, 500) // empty label, handle 500
	if !reserveSkippedHandle(g, txn.OpAddEdgeH, body) {
		t.Fatal("OpAddEdgeH: refused a well-formed body")
	}
	if reserveSkippedHandle(g, txn.OpAddEdgeH, body[:7]) {
		t.Fatal("OpAddEdgeH: accepted a body too short to hold a handle")
	}
	first := g.NextEdgeHandle()
	if first <= 500 {
		t.Fatalf("after an OpAddEdgeH reservation of 500, NextEdgeHandle = %d, want > 500", first)
	}
	if !reserveSkippedHandle(g, txn.OpAddEdge, nil) {
		t.Fatal("OpAddEdge: refused")
	}
	if next := g.NextEdgeHandle(); next != first+2 {
		t.Fatalf("after an OpAddEdge reservation, NextEdgeHandle = %d, want %d (one handle consumed)", next, first+2)
	}
}

// TestLegacyMigration_NeedsCodec pins the sentinel a clean recovery of a
// legacy store returns without a node-identifier codec.
func TestLegacyMigration_NeedsCodec(t *testing.T) {
	t.Parallel()
	err := migrateLegacyStore[string, int64](osBackend{}, t.TempDir(), &legacyReplay{simple: true}, true, legacyCheckpoint[string, int64]{})
	if !errors.Is(err, ErrLegacyMigrationNeedsCodec) {
		t.Fatalf("err = %v, want ErrLegacyMigrationNeedsCodec", err)
	}
}
