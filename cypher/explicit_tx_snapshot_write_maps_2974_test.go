package cypher_test

// explicit_tx_snapshot_write_maps_2974_test.go — regression gate for rmp #2974.
//
// An explicit write transaction reads ONE snapshot: a commit that lands after
// BEGIN is invisible to every statement of the transaction (Isolation; snapshot
// isolation as the engine documents it). A property read inside the property
// map of a CREATE pattern escaped that snapshot and read the PRESENT, so
// `CREATE (r {date: c.date})` stored the value a concurrent autocommit wrote
// after BEGIN while `RETURN c.date` and `SET r.date = c.date` in the same
// transaction read the snapshot's value.
//
// Every write clause that evaluates an expression against a bound entity is
// checked: CREATE node and relationship maps, MERGE node and relationship maps,
// and ON CREATE SET / ON MATCH SET, each reading a bound node and, where the
// shape allows, a bound relationship. Each case asserts both the value the
// statement returns and the value it stored, read back inside the transaction.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// single2974 runs query in tx and returns the only row's only value as text.
func single2974(t *testing.T, tx *cypher.ExplicitTx, query string) string {
	t.Helper()
	res, err := tx.Exec(query, nil)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = res.Close() }() // read-only use; Close error carries nothing here
	if !res.Next() {
		t.Fatalf("%s: no row (err %v)", query, res.Err())
	}
	got := res.ValueAt(0).String()
	if res.Next() {
		t.Fatalf("%s: more than one row", query)
	}
	return got
}

func TestExplicitTxSnapshotInWriteMaps_2974(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		fixture string // extra autocommit setup, run before BEGIN
		write   string // the statement under test; returns the written value
		stored  string // reads back the stored value inside the transaction
	}{
		{
			name:   "CREATE node map",
			write:  "MATCH (c:Control) CREATE (r:Receipt {date: c.date}) RETURN r.date AS d",
			stored: "MATCH (r:Receipt) RETURN r.date AS d",
		},
		{
			name:   "CREATE relationship map",
			write:  "MATCH (c:Control) CREATE (:A)-[r:R {date: c.date}]->(:B) RETURN r.date AS d",
			stored: "MATCH (:A)-[r:R]->(:B) RETURN r.date AS d",
		},
		{
			name:   "CREATE relationship map from a bound node",
			write:  "MATCH (c:Control) CREATE (c)-[r:R {date: c.date}]->(:B) RETURN r.date AS d",
			stored: "MATCH (:Control)-[r:R]->(:B) RETURN r.date AS d",
		},
		{
			name:   "MERGE node map",
			write:  "MATCH (c:Control) MERGE (m:M {date: c.date}) RETURN m.date AS d",
			stored: "MATCH (m:M) RETURN m.date AS d",
		},
		{
			name:   "MERGE relationship map",
			write:  "MATCH (c:Control) MERGE (c)-[r:MR {date: c.date}]->(:T) RETURN r.date AS d",
			stored: "MATCH (:Control)-[r:MR]->(:T) RETURN r.date AS d",
		},
		{
			name:   "MERGE ON CREATE SET",
			write:  "MATCH (c:Control) MERGE (m:OC {k: 1}) ON CREATE SET m.date = c.date RETURN m.date AS d",
			stored: "MATCH (m:OC) RETURN m.date AS d",
		},
		{
			name:    "MERGE ON MATCH SET",
			fixture: "CREATE (:OM {k: 1})",
			write:   "MATCH (c:Control) MERGE (m:OM {k: 1}) ON MATCH SET m.date = c.date RETURN m.date AS d",
			stored:  "MATCH (m:OM) RETURN m.date AS d",
		},
		{
			name:   "CREATE node map from a relationship",
			write:  "MATCH (:S)-[e:E]->() CREATE (r:Receipt {date: e.date}) RETURN r.date AS d",
			stored: "MATCH (r:Receipt) RETURN r.date AS d",
		},
		{
			name:   "MERGE node map from a relationship",
			write:  "MATCH (:S)-[e:E]->() MERGE (m:M {date: e.date}) RETURN m.date AS d",
			stored: "MATCH (m:M) RETURN m.date AS d",
		},
		{
			name:   "MERGE ON CREATE SET from a relationship",
			write:  "MATCH (:S)-[e:E]->() MERGE (m:OC {k: 1}) ON CREATE SET m.date = e.date RETURN m.date AS d",
			stored: "MATCH (m:OC) RETURN m.date AS d",
		},
		{
			name:   "CREATE then SET (control shape)",
			write:  "MATCH (c:Control) CREATE (r:Receipt) SET r.date = c.date RETURN r.date AS d",
			stored: "MATCH (r:Receipt) RETURN r.date AS d",
		},
	}
	for engName, mk := range enginesB2() {
		for _, c := range cases {
			t.Run(engName+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				run2974Case(t, mk(t).eng, c.fixture, c.write, c.stored)
			})
		}
	}
}

// run2974Case runs one shape: fixture, BEGIN, a concurrent autocommit that
// moves both source values from 1 to 2, then the statement under test inside
// the transaction.
func run2974Case(t *testing.T, e *cypher.Engine, fixture, write, stored string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.RunAny(ctx, "CREATE (:Control {date: 1}), (:S)-[:E {date: 1}]->(:Y)", nil); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if fixture != "" {
		if _, err := e.RunAny(ctx, fixture, nil); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	tx, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }() // teardown only; the outcome is not under test
	// A commit landing after BEGIN: invisible to the transaction.
	if _, err := e.RunAny(ctx, "MATCH (c:Control), (:S)-[e:E]->() SET c.date = 2, e.date = 2", nil); err != nil {
		t.Fatalf("concurrent autocommit: %v", err)
	}
	if got := single2974(t, tx, "MATCH (c:Control) RETURN c.date AS d"); got != "1" {
		t.Fatalf("precondition: the snapshot read returned %s, want 1", got)
	}
	if got := single2974(t, tx, "MATCH (:S)-[e:E]->() RETURN e.date AS d"); got != "1" {
		t.Fatalf("precondition: the snapshot read of the relationship returned %s, want 1", got)
	}
	if got := single2974(t, tx, write); got != "1" {
		t.Errorf("%s returned %s, want 1 (the transaction's snapshot value)", write, got)
	}
	if got := single2974(t, tx, stored); got != "1" {
		t.Errorf("%s stored %s, want 1 (the transaction's snapshot value)", write, got)
	}
}
