package bulkimport_test

// token_limit_2748_test.go — regression gate for rmp #2748.
//
// Bulk import bypasses the WAL, and it accepted a 70000-byte label, published
// it into a snapshot, and recovery loaded it — a durable store holding a token
// its own WAL can never log (measured at d1a9cfe7). The importer now refuses a
// record carrying a label, relationship type or property key over
// lpg.MaxTokenLen bytes, adds none of that record, and so never publishes one.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/bulkimport"
)

func TestBulkImport_RefusesOverLongTokensBeforePublish_2748(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("L", lpg.MaxTokenLen+1)
	ok := []bulkimport.Node{{Key: "a"}, {Key: "b"}}
	cases := []struct {
		name  string
		nodes []bulkimport.Node
		edges []bulkimport.Edge[int64]
	}{
		{"node label", append(ok, bulkimport.Node{Key: "c", Labels: []string{"Ok", long}}), nil},
		{"node property key", append(ok, bulkimport.Node{Key: "c", Properties: map[string]lpg.PropertyValue{long: lpg.Int64Value(1)}}), nil},
		{"relationship type", ok, []bulkimport.Edge[int64]{{Src: "a", Dst: "b", Type: long}}},
		{"relationship property key", ok, []bulkimport.Edge[int64]{{Src: "a", Dst: "b", Type: "T",
			Properties: map[string]lpg.PropertyValue{long: lpg.Int64Value(1)}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "store")
			_, err := bulkimport.ImportInto[int64](context.Background(), dir,
				bulkimport.Options{Directed: true, Multigraph: true}, c.nodes, c.edges)
			if !errors.Is(err, lpg.ErrTokenTooLong) {
				t.Fatalf("ImportInto = %v, want lpg.ErrTokenTooLong", err)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "snapshot")); !os.IsNotExist(statErr) {
				t.Fatalf("a snapshot was published although the import was refused (stat: %v)", statErr)
			}
		})
	}
}

// TestBulkImport_RefusedRecordAddsNothing_2748 pins "before any state change"
// at the record level: the refused node is not created, and the builder stays
// usable for the next record.
func TestBulkImport_RefusedRecordAddsNothing_2748(t *testing.T) {
	t.Parallel()
	b := bulkimport.New[int64](bulkimport.Options{Directed: true, Multigraph: true})
	long := strings.Repeat("L", lpg.MaxTokenLen+1)
	err := b.AddNode(bulkimport.Node{Key: "x", Labels: []string{"Ok"},
		Properties: map[string]lpg.PropertyValue{"p": lpg.Int64Value(1), long: lpg.Int64Value(2)}})
	if !errors.Is(err, lpg.ErrTokenTooLong) {
		t.Fatalf("AddNode = %v, want lpg.ErrTokenTooLong", err)
	}
	if st := b.Stats(); st.Nodes != 0 || st.NodeRecords != 0 {
		t.Fatalf("Stats = %+v after a refused record, want zero", st)
	}
	if err := b.AddNode(bulkimport.Node{Key: "y", Labels: []string{strings.Repeat("L", lpg.MaxTokenLen)}}); err != nil {
		t.Fatalf("a %d-byte label was refused: %v", lpg.MaxTokenLen, err)
	}
	if _, err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	g := b.Graph()
	if _, found := g.AdjList().Mapper().Lookup("x"); found {
		t.Fatal("the refused node record created its node")
	}
	if _, found := g.AdjList().Mapper().Lookup("y"); !found {
		t.Fatal("the valid record after the refusal was not added")
	}
	if _, found := g.Registry().Lookup("Ok"); found {
		t.Fatal("the refused record interned its label")
	}
}
