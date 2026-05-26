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
// Summary counters note: the PULL SUCCESS carries only "has_more" and
// "bookmark" — no "stats" key. All Counters() fields therefore return 0.
// Tests verify write effects via subsequent MATCH queries.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gograph/bolt/server"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
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
		func(c *neo4j.Config) {
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

// asNodeMap asserts that v is a node map (map[string]any) and returns it.
// Node maps carry keys "id" (int64), "labels" ([]any), "properties" (map[string]any).
func asNodeMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected node map (map[string]any), got %T: %v", v, v)
	}
	return m
}

// nodeProps extracts the "properties" sub-map from a node map.
func nodeProps(t *testing.T, nodeMap map[string]any) map[string]any {
	t.Helper()
	props, ok := nodeMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("node 'properties': expected map[string]any, got %T", nodeMap["properties"])
	}
	return props
}

// nodeLabels extracts the "labels" slice from a node map.
func nodeLabels(t *testing.T, nodeMap map[string]any) []any {
	t.Helper()
	labels, ok := nodeMap["labels"].([]any)
	if !ok {
		t.Fatalf("node 'labels': expected []any, got %T", nodeMap["labels"])
	}
	return labels
}

// nodeID extracts the "id" int64 from a node map.
func nodeID(t *testing.T, nodeMap map[string]any) int64 {
	t.Helper()
	id, ok := nodeMap["id"].(int64)
	if !ok {
		t.Fatalf("node 'id': expected int64, got %T", nodeMap["id"])
	}
	return id
}

// labelSet converts []any labels into an order-independent set of strings.
func labelSet(labels []any) map[string]struct{} {
	out := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		out[fmt.Sprintf("%v", l)] = struct{}{}
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
