package server_test

// e2e_match_return_test.go — T771: MATCH and RETURN via neo4j-go-driver.
//
// Seeds multiple nodes then asserts driver row count and property values.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_MatchReturn seeds three Person nodes and verifies that
// MATCH (n:Person) RETURN n.name returns all three names.
func TestE2E_MatchReturn(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	names := []string{"Alice", "Bob", "Carol"}

	for _, name := range names {
		runWrite(ctx, t, session,
			`CREATE (n:Person {name: $name})`,
			map[string]any{"name": name},
		)
	}

	// MATCH all Person nodes and return name property.
	rows := runRead(ctx, t, session, `MATCH (n:Person) RETURN n.name AS name`, nil)

	if len(rows) != len(names) {
		t.Fatalf("MATCH returned %d rows, want %d", len(rows), len(names))
	}

	// Collect returned names into a set for order-independent comparison.
	got := make(map[string]bool, len(rows))
	for _, row := range rows {
		n, ok := row["name"].(string)
		if !ok {
			t.Errorf("row 'name': expected string, got %T (%v)", row["name"], row["name"])
			continue
		}
		got[n] = true
	}

	for _, want := range names {
		if !got[want] {
			t.Errorf("name %q not found in returned rows %v", want, got)
		}
	}
}

// TestE2E_MatchReturn_NodeMap seeds a node with multiple properties and
// verifies the full node map returned by RETURN n is correct.
func TestE2E_MatchReturn_NodeMap(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	runWrite(ctx, t, session,
		`CREATE (n:Product {sku: $sku, price: $price})`,
		map[string]any{"sku": "X-100", "price": float64(9.99)},
	)

	rows := runRead(ctx, t, session, `MATCH (n:Product) RETURN n`, nil)
	if len(rows) != 1 {
		t.Fatalf("MATCH returned %d rows, want 1", len(rows))
	}

	nodeMap := asNodeMap(t, rows[0]["n"])
	props := nodeProps(t, nodeMap)

	if got, want := props["sku"], "X-100"; got != want {
		t.Errorf("property sku: got %v, want %v", got, want)
	}
	if got, ok := props["price"].(float64); !ok || got != 9.99 {
		t.Errorf("property price: got %v (%T), want 9.99", props["price"], props["price"])
	}
}
