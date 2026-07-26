package server_test

// e2e_helpers_test.go — shared helpers for neo4j-go-driver end-to-end tests.
//
// Server encoding contract (as of this sprint):
//
//   - Nodes are encoded as map[string]any with keys "id" (int64), "labels"
//     ([]any of strings), "properties" (map[string]any).
//   - Relationships are encoded as map[string]any with keys "id", "start",
//     "end", "type", "properties".
//   - Paths are encoded as map[string]any with keys "nodes" ([]any of node
//     maps), "relationships" ([]any of rel maps).
//
// The neo4j-go-driver only produces neo4j.Node / neo4j.Relationship / neo4j.Path
// when it receives PackStream structs with the canonical tag bytes
// ('N'=0x4E, 'R'=0x52, 'P'=0x50). Because the server sends plain maps,
// the driver delivers map[string]any to callers.
//
// Summary counters note: the terminal PULL SUCCESS carries "has_more",
// "bookmark" and "db" (rmp #2172) — but no "stats" key. All Counters() fields
// therefore return 0, and tests verify write effects via subsequent MATCH
// queries. summary.Database().Name() is covered by driver_compat_db_test.go;
// the missing "stats" is tracked separately in sprint 321.

import (
	"context"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
)

// newDriverForTest starts a fresh isolated bolt/server.Server and connects a
// neo4j-go-driver v5 driver to it. Both driver and server are cleaned up via
// t.Cleanup when the test exits.
func newDriverForTest(t *testing.T) (neo4j.DriverWithContext, string) {
	t.Helper()
	addr := startTestServer(t, server.Options{
		ConnTimeout: 10 * time.Second,
	})

	driver, err := neo4j.NewDriverWithContext(
		"bolt://"+addr,
		neo4j.NoAuth(),
		func(c *config.Config) {
			c.MaxConnectionPoolSize = 5
			c.ConnectionAcquisitionTimeout = 5 * time.Second
			c.SocketConnectTimeout = 5 * time.Second
		},
	)
	if err != nil {
		t.Fatalf("neo4j.NewDriverWithContext: %v", err)
	}
	t.Cleanup(func() {
		if err := driver.Close(context.Background()); err != nil {
			t.Logf("driver.Close: %v", err)
		}
	})
	return driver, addr
}

// asNodeMap asserts that v is a node the official driver materialised as a
// dbtype.Node and returns it.
//
// Before #2189 the server sent nodes as plain PackStream maps, so the driver could only
// hand back a map[string]any and these helpers read its "id"/"labels"/"properties" keys.
// The server now sends the Bolt 'N' structure, so the driver materialises a real
// dbtype.Node — which is the whole point of that task, and is why this asserts the
// driver's own type rather than a map. The name is kept so the call sites read unchanged.
func asNodeMap(t *testing.T, v any) neo4j.Node {
	t.Helper()
	n, ok := v.(neo4j.Node)
	if !ok {
		t.Fatalf("expected the driver to materialise a neo4j.Node, got %T: %v — the server "+
			"is not sending the Bolt Node structure (#2189)", v, v)
	}
	return n
}

// nodeProps returns the node's properties.
func nodeProps(t *testing.T, n neo4j.Node) map[string]any {
	t.Helper()
	return n.Props
}

// nodeLabels returns the node's labels.
func nodeLabels(t *testing.T, n neo4j.Node) []string {
	t.Helper()
	return n.Labels
}

// nodeID returns the node's numeric id.
func nodeID(t *testing.T, n neo4j.Node) int64 {
	t.Helper()
	return n.Id //nolint:staticcheck // Id is supported until driver 6.0; ElementId is asserted separately
}

// labelSet converts labels into an order-independent set.
func labelSet(labels []string) map[string]struct{} {
	out := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		out[l] = struct{}{}
	}
	return out
}

// collectRows drains a neo4j.ResultWithContext and returns each record as a
// map[string]any, keyed by column name.
func collectRows(ctx context.Context, t *testing.T, result neo4j.ResultWithContext) []map[string]any {
	t.Helper()
	records, err := result.Collect(ctx)
	if err != nil {
		t.Fatalf("result.Collect: %v", err)
	}
	out := make([]map[string]any, len(records))
	for i, rec := range records {
		out[i] = rec.AsMap()
	}
	return out
}

// runWrite executes a write query via ExecuteWrite and discards the result.
func runWrite(ctx context.Context, t *testing.T, session neo4j.SessionWithContext, query string, params map[string]any) {
	t.Helper()
	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx, query, params)
		if err != nil {
			return nil, err
		}
		_, err = result.Consume(ctx)
		return nil, err
	})
	if err != nil {
		t.Fatalf("ExecuteWrite(%q): %v", query, err)
	}
}

// runRead executes a read query and returns the collected rows.
func runRead(ctx context.Context, t *testing.T, session neo4j.SessionWithContext, query string, params map[string]any) []map[string]any {
	t.Helper()
	rows, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx, query, params)
		if err != nil {
			return nil, err
		}
		return collectRows(ctx, t, result), nil
	})
	if err != nil {
		t.Fatalf("ExecuteRead(%q): %v", query, err)
	}
	return rows.([]map[string]any)
}
