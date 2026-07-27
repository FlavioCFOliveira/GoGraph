package cypher_test

// show_valueat_test.go — mechanism-level regression gate for rmp #2215.
//
// Result.ValueAt served only a MATERIALISED result. SHOW builds a STREAMING one,
// so ValueAt returned a bare nil (not even expr.Null) for every column, and the
// Bolt PULL path — which reads positionally — delivered a row of nulls with no
// error. Record() was unaffected, which is why this went unnoticed.
//
// This file asserts the two properties that together make the surface correct:
// ValueAt and Record must agree, and neither may return a nil interface for a
// column the result declares. The Bolt contract itself is covered end to end by
// bolt/server/show_values_e2e_test.go.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func showFixture(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	eng := cypher.NewEngine(g)
	for _, q := range []string{
		`CREATE (:P {name: 'x'})`,
		`CREATE INDEX p_name FOR (n:P) ON (n.name)`,
	} {
		res, err := eng.RunAny(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
		for res.Next() {
		}
		if err := res.Close(); err != nil {
			t.Fatalf("setup close %q: %v", q, err)
		}
	}
	return eng
}

// TestShow_ValueAtDeliversValues is the gate: every declared column of a SHOW
// row must come back non-nil through the positional accessor.
func TestShow_ValueAtDeliversValues(t *testing.T) {
	for _, q := range []string{
		`SHOW INDEXES`,
		`SHOW INDEXES YIELD name`,
		`SHOW INDEXES YIELD name, labelsOrTypes WHERE name = 'p_name'`,
	} {
		t.Run(q, func(t *testing.T) {
			eng := showFixture(t)
			res, err := eng.RunAny(context.Background(), q, nil)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			cols := res.Columns()
			rows := 0
			for res.Next() {
				rows++
				for i, c := range cols {
					if res.ValueAt(i) == nil {
						t.Errorf("column %d (%s) is a bare nil through ValueAt (#2215)", i, c)
					}
				}
			}
			if err := res.Err(); err != nil {
				t.Fatalf("drain: %v", err)
			}
			if err := res.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if rows == 0 {
				t.Fatal("no rows, so the assertions above proved nothing")
			}
		})
	}
}

// TestShow_ValueAtAgreesWithRecord pins the invariant the defect broke: the
// positional accessor and the map accessor must describe the same row. Record()
// kept working throughout, so comparing the two is what would have caught this.
func TestShow_ValueAtAgreesWithRecord(t *testing.T) {
	eng := showFixture(t)
	res, err := eng.RunAny(context.Background(), `SHOW INDEXES`, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	cols := res.Columns()
	rows := 0
	for res.Next() {
		rows++
		rec := res.Record()
		for i, c := range cols {
			want, ok := rec[c]
			if !ok {
				t.Fatalf("Record() has no column %q", c)
			}
			// Compared by rendered value: some column types (ListValue) are Go
			// slices and therefore not comparable with ==.
			got := res.ValueAt(i)
			if got == nil || want == nil {
				t.Errorf("column %q: ValueAt=%v Record=%v — neither may be nil", c, got, want)
				continue
			}
			wv, ok := want.(expr.Value)
			if !ok {
				t.Fatalf("column %q: Record holds %T, not an expr.Value", c, want)
			}
			if got.String() != wv.String() {
				t.Errorf("column %q: ValueAt=%v Record=%v — the two accessors disagree", c, got, wv)
			}
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if rows == 0 {
		t.Fatal("no rows, so the assertions above proved nothing")
	}
}
