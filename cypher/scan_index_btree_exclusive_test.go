package cypher_test

// scan_index_btree_exclusive_test.go — T625: range query with EXCLUSIVE bounds
// on City nodes with a "population" int64 property.
//
// Same fixture as T616. These tests use strictly-less and strictly-greater
// comparisons (> and <) to verify that boundary values are excluded.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// citiesExclusiveRange runs the exclusive range query (> lo AND < hi) and
// returns the set of city names returned.
func citiesExclusiveRange(t *testing.T, eng *cypher.Engine, lo, hi int64) map[string]bool {
	t.Helper()
	q := fmt.Sprintf(
		"MATCH (n:City) WHERE n.population > %d AND n.population < %d RETURN n.name",
		lo, hi,
	)
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	names := map[string]bool{}
	for res.Next() {
		rec := res.Record()
		sv, ok := rec["n.name"].(expr.StringValue)
		if !ok {
			t.Fatalf("n.name: expected StringValue, got %T", rec["n.name"])
		}
		names[string(sv)] = true
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	return names
}

// TestBTreeExclusive_MiddleRange verifies that exclusive bounds (500k, 800k)
// return only the cities strictly inside, excluding the boundary values.
func TestBTreeExclusive_MiddleRange(t *testing.T) {
	t.Parallel()

	_, eng := newCityEngine(t)
	got := citiesExclusiveRange(t, eng, 500_000, 800_000)

	// Strictly between 500k and 800k: Zeta (600k) and Eta (700k).
	want := []string{"Zeta", "Eta"}
	if len(got) != len(want) {
		t.Fatalf("(500k,800k): want %v, got %v", want, got)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("(500k,800k): missing %q; full result: %v", w, got)
		}
	}
}

// TestBTreeExclusive_BoundaryExclusion verifies that boundary values are
// excluded when using strict > and < comparisons.
func TestBTreeExclusive_BoundaryExclusion(t *testing.T) {
	t.Parallel()

	_, eng := newCityEngine(t)
	got := citiesExclusiveRange(t, eng, 500_000, 800_000)

	if got["Epsilon"] {
		t.Error("exclusive lower bound 500_000: Epsilon must NOT be included")
	}
	if got["Theta"] {
		t.Error("exclusive upper bound 800_000: Theta must NOT be included")
	}
}

// TestBTreeExclusive_SingletonInterior verifies that (400k, 600k) returns only
// Epsilon (500k).
func TestBTreeExclusive_SingletonInterior(t *testing.T) {
	t.Parallel()

	_, eng := newCityEngine(t)
	got := citiesExclusiveRange(t, eng, 400_000, 600_000)
	if len(got) != 1 || !got["Epsilon"] {
		t.Errorf("(400k,600k): want {Epsilon}, got %v", got)
	}
}

// TestBTreeExclusive_EmptyRange verifies that (500k, 600k) returns zero rows
// because no city has population strictly between 500k and 600k (Zeta is
// exactly 600k, Epsilon is exactly 500k).
func TestBTreeExclusive_EmptyRange(t *testing.T) {
	t.Parallel()

	_, eng := newCityEngine(t)

	// No city has population strictly in (500k, 600k).
	got := citiesExclusiveRange(t, eng, 500_000, 600_000)
	if len(got) != 0 {
		t.Errorf("(500k,600k): want 0 cities, got %d (%v)", len(got), got)
	}
}
