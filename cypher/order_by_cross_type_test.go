package cypher

import (
	"context"
	"math"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestOrderBy_CrossTypeGlobalSortOrder_2809 pins the ascending global sort order
// of disjoint types defined by CIP2016-06-14 ("Orderability"):
//
//	Map (regular map < Node < Relationship) < List < Path < DateTime <
//	LocalDateTime < Date < Time < LocalTime < Duration < String < Boolean <
//	Number (NaN largest) < null
//
// and its exact reverse for DESC. One value of every type is sorted through the
// engine; nodes, relationships and paths are bound from a MATCH. Before rmp
// #2809 the temporal kinds sorted after every number (after NaN, which the CIP
// forbids) and in a different relative order.
func TestOrderBy_CrossTypeGlobalSortOrder_2809(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	ctx := context.Background()
	setup, err := eng.RunAny(ctx, `CREATE (:A {k: 1})-[:R]->(:B)`, nil)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	for setup.Next() {
	}
	if err := setup.Err(); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_ = setup.Close()

	const query = `MATCH p = (n:A)-[r:R]->(m) ` +
		`UNWIND [2, 'a', true, null, 0.0/0.0, 1.5, 1, n, r, p, [1], {a: 1}, ` +
		`duration('P1D'), localtime('10:00'), time('10:00Z'), date('2020-01-01'), ` +
		`localdatetime('2020-01-01T10:00'), datetime('2020-01-01T10:00Z')] AS v ` +
		`RETURN v ORDER BY v `

	// ascKinds is the CIP2016-06-14 ascending order; the three numbers are
	// checked by value below because Integer and Float are one type (Number).
	ascKinds := []expr.Kind{
		expr.KindMap, expr.KindNode, expr.KindRelationship, expr.KindList, expr.KindPath,
		expr.KindDateTime, expr.KindLocalDateTime, expr.KindDate, expr.KindTime,
		expr.KindLocalTime, expr.KindDuration,
		expr.KindString, expr.KindBool,
		expr.KindInteger, expr.KindFloat, expr.KindInteger, expr.KindFloat, // 1, 1.5, 2, NaN
		expr.KindNull,
	}
	ascNumbers := []float64{1, 1.5, 2, math.NaN()}

	for _, dir := range []string{"ASC", "DESC"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			res, err := eng.Run(ctx, query+dir, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			defer func() { _ = res.Close() }()
			var kinds []expr.Kind
			var numbers []float64
			for res.Next() {
				v := res.ValueAt(0)
				kinds = append(kinds, v.Kind())
				switch x := v.(type) {
				case expr.IntegerValue:
					numbers = append(numbers, float64(x))
				case expr.FloatValue:
					numbers = append(numbers, float64(x))
				}
			}
			if err := res.Err(); err != nil {
				t.Fatalf("iterate: %v", err)
			}
			wantKinds := slices.Clone(ascKinds)
			wantNumbers := slices.Clone(ascNumbers)
			if dir == "DESC" {
				slices.Reverse(wantKinds)
				slices.Reverse(wantNumbers)
			}
			if !slices.Equal(kinds, wantKinds) {
				t.Fatalf("ORDER BY v %s kinds =\n  %v\nwant (CIP2016-06-14 global sort order)\n  %v", dir, kinds, wantKinds)
			}
			if len(numbers) != len(wantNumbers) {
				t.Fatalf("ORDER BY v %s numbers = %v, want %v", dir, numbers, wantNumbers)
			}
			for i := range numbers {
				if math.IsNaN(wantNumbers[i]) != math.IsNaN(numbers[i]) ||
					(!math.IsNaN(wantNumbers[i]) && numbers[i] != wantNumbers[i]) {
					t.Fatalf("ORDER BY v %s numbers = %v, want %v", dir, numbers, wantNumbers)
				}
			}
		})
	}
}
