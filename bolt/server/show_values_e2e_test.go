package server_test

// show_values_e2e_test.go — regression gate for rmp #2215.
//
// # The defect
//
// `SHOW INDEXES` and `SHOW CONSTRAINTS` build a STREAMING result over static
// rows (cypher/show.go, exec.Run over exec.NewStaticRows), while
// cypher.Result.ValueAt read only the MATERIALISED backing store — whose
// matRowLen is zero for a streaming result. session.go reads every column with
// ValueAt on the stated premise that "the engine result is always materialised",
// which is false for SHOW. The server therefore answered `SHOW INDEXES` with a
// well-formed row carrying six correctly-named columns and a null in every one
// of them, and no error: fail-silent, which the failure-handling rule forbids.
//
// Result.Record() was unaffected, which is exactly why the pre-existing e2e
// tests — which all use Record() — missed it. This test reads the values the way
// the server does.
//
// # Why it lives here and not in cypher/
//
// The defect needed all three layers to appear: a streaming result, a positional
// read, and the Bolt encoder. A cypher-package test on ValueAt would have caught
// the mechanism, but only an end-to-end read through the official driver proves
// what a real client actually receives. cypher/show_valueat_test.go covers the
// mechanism; this covers the contract.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_ShowIndexesDeliversValues asserts that a driver reading SHOW output
// receives populated columns, not nulls.
func TestE2E_ShowIndexesDeliversValues(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	for _, setup := range []string{
		"CREATE (:P {name: 'x'})",
		"CREATE INDEX p_name FOR (n:P) ON (n.name)",
	} {
		if _, err := session.Run(ctx, setup, nil); err != nil {
			t.Fatalf("setup %q: %v", setup, err)
		}
	}

	res, err := session.Run(ctx, "SHOW INDEXES", nil)
	if err != nil {
		t.Fatalf("SHOW INDEXES: %v", err)
	}
	rows := 0
	for res.Next(ctx) {
		rec := res.Record()
		rows++
		if len(rec.Keys) == 0 {
			t.Fatal("SHOW INDEXES returned a row with no columns")
		}
		for _, k := range rec.Keys {
			v, ok := rec.Get(k)
			if !ok {
				t.Errorf("column %q missing from the record", k)
				continue
			}
			if v == nil {
				t.Errorf("column %q is null; SHOW is delivering nulls again (#2215)", k)
			}
		}
		// The index just created must be identifiable by name, which is the
		// whole point of the surface.
		if name, _ := rec.Get("name"); name != "p_name" {
			t.Errorf("name = %v, want p_name", name)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("SHOW INDEXES drain: %v", err)
	}
	if rows == 0 {
		t.Fatal("SHOW INDEXES returned no rows, so this test proves nothing; the fixture must create an index")
	}
}
