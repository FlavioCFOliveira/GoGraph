package cypher_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// edgeStateDump renders every relationship's handle-keyed type and properties,
// every pair's union surfaces, and the query-visible relationships, so two
// graphs can be compared edge by edge.
func edgeStateDump(t *testing.T, g *lpg.Graph[string, float64]) string {
	t.Helper()
	m := g.AdjList().Mapper()
	var rows []string
	g.WalkEdgeHandles(func(e lpg.EdgeHandleTriple) bool {
		s, _ := m.Resolve(e.Src)
		d, _ := m.Resolve(e.Dst)
		labels := g.EdgeLabelsByHandle(s, d, e.Handle)
		sort.Strings(labels)
		rows = append(rows, fmt.Sprintf("handle %s->%s#%d labels=%v props=%s",
			s, d, e.Handle, labels, propString(g.EdgePropertiesByHandle(s, d, e.Handle))))
		pair := g.EdgeLabels(s, d)
		sort.Strings(pair)
		rows = append(rows, fmt.Sprintf("pair %s->%s labels=%v props=%s", s, d, pair, propString(g.EdgeProperties(s, d))))
		return true
	})
	sort.Strings(rows)
	r, err := cypher.NewEngine(g).Run(context.Background(),
		"MATCH (a)-[r]->(b) RETURN a.id AS a, b.id AS b, type(r) AS t, properties(r) AS p ORDER BY a, b, t", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for r.Next() {
		rows = append(rows, fmt.Sprint(r.Record()))
	}
	return strings.Join(rows, "\n")
}

func propString(m map[string]lpg.PropertyValue) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for _, k := range keys {
		out += fmt.Sprintf("%s:%v;", k, m[k])
	}
	return out
}

// TestDurableEdgeInstanceWrites_SurviveRecoveryAndLeaveNoOrdinalEntry pins rmp
// #2968. The durable engine's SetEdgeLabelAt, SetEdgePropertyAt and
// RemoveEdgeInstance used to write the per-CREATE-ordinal edge side store, which
// has no WAL frame: the effect lived in memory only, and a deleted
// relationship's ordinal entry outlived it. The store is retired on the durable
// path; every relationship's type and properties live in the handle store, which
// is WAL-described. Checked: no ordinal entry exists in memory, in particular
// none after DELETE r, and after recovery every relationship's labels and
// properties — by handle, per pair, and as a query sees them — equal the
// acknowledged state, on parallel and on single-relationship pairs.
func TestDurableEdgeInstanceWrites_SurviveRecoveryAndLeaveNoOrdinalEntry(t *testing.T) {
	shapes := []struct {
		name string
		qs   []string
	}{
		{"parallel pairs", []string{
			"CREATE (:A {id:1}), (:B {id:2})",
			"MATCH (a:A),(b:B) CREATE (a)-[:T {p:1}]->(b)",
			"MATCH (a:A),(b:B) CREATE (a)-[:U {p:2}]->(b)",
			"MATCH (a:A),(b:B) CREATE (a)-[:W {p:5}]->(b)",
			"MATCH ()-[r:U]->() SET r.q = 3 REMOVE r.p",
			"MATCH ()-[r:T]->() DELETE r",
			"MATCH (a:A),(b:B) MERGE (a)-[:V {p:4}]->(b)",
		}},
		{"single pairs", []string{
			"CREATE (:A {id:1}), (:B {id:2}), (:C {id:3})",
			"MATCH (a:A),(b:B) CREATE (a)-[:T {p:1}]->(b)",
			"MATCH (b:B),(c:C) CREATE (b)-[:U {p:2}]->(c)",
			"MATCH ()-[r:T]->() SET r.q = 3 REMOVE r.p",
			"MATCH ()-[r:U]->() DELETE r",
			"MATCH (b:B),(c:C) MERGE (b)-[:V {p:4}]->(c)",
		}},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			dir := t.TempDir()
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatal(err)
			}
			g := lpg.New[string, float64](adjlist.Config{})
			opts := txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
			eng := cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, w, opts))
			for _, q := range sh.qs {
				noopRunAuto(t, eng, q)
			}
			// No ordinal entry on the durable path, in particular none left by the
			// deleted relationship.
			var keys []string
			g.AdjList().Mapper().Walk(func(_ graph.NodeID, k string) bool { keys = append(keys, k); return true })
			for _, s := range keys {
				for _, d := range keys {
					for i := int64(0); i <= g.EdgeCreateCount(s, d)+1; i++ {
						if l := g.EdgeLabelsAt(s, d, i); len(l) > 0 {
							t.Errorf("ordinal type entry %s->%s #%d = %v on the durable path", s, d, i, l)
						}
						if p := g.EdgePropertiesAt(s, d, i); len(p) > 0 {
							t.Errorf("ordinal property entry %s->%s #%d = %v on the durable path", s, d, i, p)
						}
					}
				}
			}
			mem := edgeStateDump(t, g)
			// The control: the acknowledged state carries every operation's effect,
			// so an empty dump cannot make the comparison below pass.
			for _, want := range []string{`t:"V"`, "q: 3", "handle "} {
				if !strings.Contains(mem, want) {
					t.Fatalf("the acknowledged state lacks %q:\n%s", want, mem)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			res, err := recovery.Open[string, float64](dir, recovery.OptionsFromTxn(opts))
			if err != nil {
				t.Fatalf("recovery: %v", err)
			}
			if rec := edgeStateDump(t, res.Graph); rec != mem {
				t.Errorf("recovered relationships differ from the acknowledged state\n--- memory\n%s\n--- recovered\n%s", mem, rec)
			}
		})
	}
}
