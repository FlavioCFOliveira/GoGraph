package recovery

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// testdata/undirected_v3 is a REAL store directory written before storage became
// directed-only (rmp #3072), by an lpg graph configured as an undirected
// multigraph (the since-removed adjlist options Directed=false and
// Multigraph=true) behind a txn.Store with the string codec and the int64
// weight codec. Its manifest carries
// "directed": false. Node ids: d=115, a=140, b=165, c=242.
//
// The snapshot (one checkpoint) holds, with explicit handles:
//
//	101 a-b  pair label KNOWS, pair property since=2020
//	102 c-a  added high->low; handle label LIKES, handle property w=1
//	103 d-d  self-loop; handle label SELF, handle property x="loop"
//	104 b-c  parallel #1; R1, p=1
//	105 b-c  parallel #2; R2, p=2
//	106 c-b  reciprocal, i.e. a third b-c relationship; R3, p=3
//	107 b-d  handle label GONE
//
// csr.bin stores every non-loop edge twice (13 arcs for 7 relationships), and
// the handle records of 102, 106 and 107 are keyed high id -> low id.
//
// The WAL above the snapshot holds ONE transaction the undirected engine
// wrote, most of it naming edges in the mirror orientation:
//
//	AddEdgeWithHandle(d, a, 108); SetEdgeLabelByHandle(a, d, 108, LATE);
//	SetEdgePropertyByHandle(a, d, 108, k=8); SetEdgeProperty(b, a, note="mirror");
//	SetEdgePropertyByHandle(c, a, 102, w=11); RemoveEdgeByHandle(c, b, 104);
//	RemoveEdge(d, b)
const legacyUndirectedFixture = "undirected_v3"

// legacyRel is one expected relationship of the migrated fixture.
type legacyRel struct {
	src, dst string
	handle   uint64
	labels   []string
	props    map[string]lpg.PropertyValue
}

// legacyUndirectedWant is the migrated graph: one relationship per surviving
// edge, oriented lower node id -> higher node id, with the WAL tail applied.
var legacyUndirectedWant = []legacyRel{
	{src: "a", dst: "b", handle: 101},
	{src: "a", dst: "c", handle: 102, labels: []string{"LIKES"}, props: map[string]lpg.PropertyValue{"w": lpg.Int64Value(11)}},
	{src: "d", dst: "d", handle: 103, labels: []string{"SELF"}, props: map[string]lpg.PropertyValue{"x": lpg.StringValue("loop")}},
	{src: "b", dst: "c", handle: 105, labels: []string{"R2"}, props: map[string]lpg.PropertyValue{"p": lpg.Int64Value(2)}},
	{src: "b", dst: "c", handle: 106, labels: []string{"R3"}, props: map[string]lpg.PropertyValue{"p": lpg.Int64Value(3)}},
	{src: "d", dst: "a", handle: 108, labels: []string{"LATE"}, props: map[string]lpg.PropertyValue{"k": lpg.Int64Value(8)}},
}

func legacyOpts() Options[string, int64] {
	return Options[string, int64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewInt64WeightCodec()}
}

// copyLegacyFixture copies the undirected fixture into a fresh directory and
// returns it.
func copyLegacyFixture(t *testing.T) string {
	t.Helper()
	return copyFixture(t, legacyUndirectedFixture)
}

// copyFixture copies testdata/name into a fresh directory and returns it.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("testdata", name))); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dir
}

// assertLegacyMigrated checks g against want plus extra, and that no record is
// left keyed in the dropped (higher id -> lower id) orientation.
func assertLegacyMigrated(t *testing.T, g *lpg.Graph[string, int64], extra ...legacyRel) {
	t.Helper()
	want := append(slices.Clone(legacyUndirectedWant), extra...)
	keys := []string{"a", "b", "c", "d"}
	m := g.AdjList().Mapper()
	id := func(k string) uint64 {
		v, ok := m.Lookup(k)
		if !ok {
			t.Fatalf("node %q not interned", k)
		}
		return uint64(v)
	}
	if got := g.AdjList().Size(); got != uint64(len(want)) {
		t.Errorf("Size = %d, want %d relationships", got, len(want))
	}
	seen := map[uint64]bool{}
	for _, s := range keys {
		for _, d := range keys {
			hs := g.AppendEdgeHandles(s, d, nil)
			for _, h := range hs {
				if seen[h] {
					t.Errorf("handle %d stored twice", h)
				}
				seen[h] = true
			}
			var wantHs []uint64
			for _, r := range want {
				if r.src == s && r.dst == d {
					wantHs = append(wantHs, r.handle)
				}
			}
			slices.Sort(hs)
			slices.Sort(wantHs)
			if !slices.Equal(hs, wantHs) {
				t.Errorf("%s->%s handles = %v, want %v", s, d, hs, wantHs)
			}
			if len(hs) > 0 && id(s) > id(d) && !slices.ContainsFunc(extra, func(r legacyRel) bool { return r.src == s && r.dst == d }) {
				t.Errorf("%s->%s: a migrated relationship is oriented higher id -> lower id", s, d)
			}
			if id(s) > id(d) && len(hs) == 0 {
				// The dropped orientation: no pair record may survive under it.
				if l := g.EdgeLabels(s, d); len(l) != 0 {
					t.Errorf("%s->%s: mirror-key labels survived: %v", s, d, l)
				}
				if p := g.EdgeProperties(s, d); len(p) != 0 {
					t.Errorf("%s->%s: mirror-key properties survived: %v", s, d, p)
				}
				for h := uint64(101); h <= 108; h++ {
					if l := g.EdgeLabelsByHandle(s, d, h); len(l) != 0 {
						t.Errorf("%s->%s handle %d: mirror-key labels survived: %v", s, d, h, l)
					}
					if p := g.EdgePropertiesByHandle(s, d, h); len(p) != 0 {
						t.Errorf("%s->%s handle %d: mirror-key properties survived: %v", s, d, h, p)
					}
				}
			}
		}
	}
	for _, r := range want {
		if r.labels != nil {
			if got := g.EdgeLabelsByHandle(r.src, r.dst, r.handle); !slices.Equal(got, r.labels) {
				t.Errorf("%s->%s handle %d labels = %v, want %v", r.src, r.dst, r.handle, got, r.labels)
			}
		}
		for k, v := range r.props {
			got, ok := g.EdgePropertyByHandle(r.src, r.dst, r.handle, k)
			if !ok || !reflect.DeepEqual(got, v) {
				t.Errorf("%s->%s handle %d property %s = %v (ok=%v), want %v", r.src, r.dst, r.handle, k, got, ok, v)
			}
		}
	}
	// The pair-level label and properties of a-b: the snapshot's and the one the
	// WAL tail wrote through the mirror orientation (b, a).
	if got := g.EdgeLabels("a", "b"); !slices.Equal(got, []string{"KNOWS"}) {
		t.Errorf("a->b labels = %v, want [KNOWS]", got)
	}
	props := g.EdgeProperties("a", "b")
	if v, ok := props["since"]; !ok || !reflect.DeepEqual(v, lpg.Int64Value(2020)) {
		t.Errorf("a->b since = %v (ok=%v), want 2020", v, ok)
	}
	if v, ok := props["note"]; !ok || !reflect.DeepEqual(v, lpg.StringValue("mirror")) {
		t.Errorf("a->b note = %v (ok=%v), want \"mirror\"", v, ok)
	}
	if got := g.EdgeLabelsByHandle("d", "b", 107); len(got) != 0 {
		t.Errorf("removed relationship 107 kept labels %v", got)
	}
}

// TestRecovery_LegacyUndirectedSnapshot_Migrates is the acceptance test of the
// legacy undirected migration (rmp #3072): recovering the fixture yields one
// relationship per original edge, oriented lower id -> higher id, self-loop
// once, labels and properties intact, handles unique, and the WAL tail the
// undirected engine wrote in the mirror orientation applied to those
// relationships. Recovery then checkpoints the store in the current format
// before it returns, so the next open replays nothing and declares no legacy
// shape.
func TestRecovery_LegacyUndirectedSnapshot_Migrates(t *testing.T) {
	t.Parallel()
	dir := copyLegacyFixture(t)
	snapDir := filepath.Join(dir, "snapshot")
	requireLegacyManifest(t, snapDir, `"directed": false`)

	res, err := Open[string, int64](dir, legacyOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !res.IsClean() || res.WALOps != 7 {
		t.Fatalf("IsClean=%v WALOps=%d, want a clean replay of the 7-op tail", res.IsClean(), res.WALOps)
	}
	assertLegacyMigrated(t, res.Graph)
	requireMigratedOnDisk(t, dir)

	res2, err := Open[string, int64](dir, legacyOpts())
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if res2.WALOps != 0 {
		t.Fatalf("second Open replayed %d ops, want 0: the migration checkpoint folded the legacy WAL", res2.WALOps)
	}
	assertLegacyMigrated(t, res2.Graph)
}

// TestRecovery_LegacyUndirected_PostMigrationWritesKeepDirection proves that
// once recovery returns the store is entirely in the current format: a
// relationship written after migration, oriented higher id -> lower id
// (c -> a), is replayed as written by the next recovery.
func TestRecovery_LegacyUndirected_PostMigrationWritesKeepDirection(t *testing.T) {
	t.Parallel()
	dir := copyLegacyFixture(t)
	res, err := Open[string, int64](dir, legacyOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	st := res.NewStore(w, txn.Options[string, int64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewInt64WeightCodec()})
	tx := st.Begin()
	mustTx(t, tx.AddEdgeWithHandle("c", "a", 0, 200))
	mustTx(t, tx.SetEdgeLabelByHandle("c", "a", 200, "NEW"))
	mustTx(t, tx.SetEdgeProperty("c", "a", "dir", lpg.StringValue("c->a")))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	newRel := legacyRel{src: "c", dst: "a", handle: 200, labels: []string{"NEW"}}

	res2, err := Open[string, int64](dir, legacyOpts())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	assertLegacyMigrated(t, res2.Graph, newRel)
	if v, ok := res2.Graph.GetEdgeProperty("c", "a", "dir"); !ok || !reflect.DeepEqual(v, lpg.StringValue("c->a")) {
		t.Errorf("c->a dir = %v (ok=%v), want \"c->a\"", v, ok)
	}
}

// TestRecovery_LegacySimpleGraph_SkipsUnacknowledgedDuplicates is the legacy
// simple-graph migration (rmp #3072). testdata/simple_v4 is a REAL store
// directory written before every graph became a multigraph, by an lpg graph
// configured as a directed SIMPLE graph (the since-removed Multigraph=false
// option) behind a txn.Store with the string codec and the int64 weight codec;
// its manifest carries "multigraph": false. The snapshot holds a -> b (handle
// 201, KNOWS, since=2020), b -> a (202) and a -> c (203). The WAL tail holds
// three transactions: AddEdge(a, b) again (handle 301), AddEdge(c, a) (302)
// with w=4, and AddEdge(c, a) again. On that engine both repeats were no-ops,
// though their records were written; the acknowledged graph has 4 edges.
func TestRecovery_LegacySimpleGraph_SkipsUnacknowledgedDuplicates(t *testing.T) {
	t.Parallel()
	dir := copyFixture(t, "simple_v4")
	requireLegacyManifest(t, filepath.Join(dir, "snapshot"), `"multigraph": false`)

	check := func(t *testing.T, g *lpg.Graph[string, int64]) {
		t.Helper()
		if got := g.AdjList().Size(); got != 4 {
			t.Fatalf("Size = %d, want the 4 acknowledged relationships", got)
		}
		for _, p := range [][2]string{{"a", "b"}, {"b", "a"}, {"a", "c"}, {"c", "a"}} {
			if hs := g.AppendEdgeHandles(p[0], p[1], nil); len(hs) != 1 {
				t.Errorf("%s->%s handles = %v, want exactly one relationship", p[0], p[1], hs)
			}
		}
		if g.HasEdgeHandle("a", "b", 301) {
			t.Error("the unacknowledged duplicate a->b (handle 301) was materialised")
		}
		if got := g.EdgeLabelsByHandle("a", "b", 201); !slices.Equal(got, []string{"KNOWS"}) {
			t.Errorf("a->b handle 201 labels = %v, want [KNOWS]", got)
		}
		if v, ok := g.EdgePropertyByHandle("a", "b", 201, "since"); !ok || !reflect.DeepEqual(v, lpg.Int64Value(2020)) {
			t.Errorf("a->b since = %v (ok=%v), want 2020", v, ok)
		}
		if v, ok := g.GetEdgeProperty("c", "a", "w"); !ok || !reflect.DeepEqual(v, lpg.Int64Value(4)) {
			t.Errorf("c->a w = %v (ok=%v), want 4", v, ok)
		}
	}
	res, err := Open[string, int64](dir, legacyOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	check(t, res.Graph)
	requireMigratedOnDisk(t, dir)

	// After migration every graph is a multigraph: a repeated AddEdge appends.
	if err := res.Graph.AddEdge("a", "b", 7); err != nil {
		t.Fatalf("post-migration AddEdge: %v", err)
	}
	if got := res.Graph.AdjList().Size(); got != 5 {
		t.Fatalf("post-migration Size = %d, want 5 (multigraph append)", got)
	}

	res2, err := Open[string, int64](dir, legacyOpts())
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if res2.WALOps != 0 {
		t.Fatalf("second Open replayed %d ops, want 0", res2.WALOps)
	}
	check(t, res2.Graph)
}

// requireLegacyManifest fails unless the fixture manifest at snapDir carries
// frag, so a test cannot pass on a fixture that is not legacy.
func requireLegacyManifest(t *testing.T, snapDir, frag string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(snapDir, "manifest.json")) //nolint:gosec // fixture copy under t.TempDir
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !containsJSON(raw, frag) {
		t.Fatalf("fixture manifest does not carry %s", frag)
	}
}

// requireMigratedOnDisk asserts that dir holds no legacy state: the snapshot
// manifest is the current version, declares no legacy shape and carries no
// "directed" or "multigraph" key.
func requireMigratedOnDisk(t *testing.T, dir string) {
	t.Helper()
	snapDir := filepath.Join(dir, "snapshot")
	raw, err := os.ReadFile(filepath.Join(snapDir, "manifest.json")) //nolint:gosec // under t.TempDir
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if containsJSON(raw, `"directed"`) || containsJSON(raw, `"multigraph"`) {
		t.Fatal("migrated manifest still carries a legacy shape key")
	}
	ls, err := snapshot.LoadSnapshotFull(snapDir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	if ls.Legacy.Any() || ls.Manifest.Version != snapshot.ManifestVersion {
		t.Fatalf("migrated snapshot: legacy=%+v version=%d, want none and %d",
			ls.Legacy, ls.Manifest.Version, snapshot.ManifestVersion)
	}
}

// containsJSON reports whether the JSON body of the manifest bytes raw — the
// bytes before the binary integrity trailer — is valid JSON containing frag.
func containsJSON(raw []byte, frag string) bool {
	end := bytes.LastIndexByte(raw, '}') + 1
	return json.Valid(raw[:end]) && bytes.Contains(raw[:end], []byte(frag))
}
