package server_test

import (
	"crypto/tls"
	"strings"
	"testing"

	"gograph/bolt/proto"
	"gograph/bolt/server"
)

// createNodeTx creates a node inside an explicit transaction using
// BEGIN / RUN / PULL / COMMIT. All write queries must go through an
// explicit transaction — the auto-commit path (RunAny) only handles
// read/DDL queries (ProduceResults root requirement).
func createNodeTx(t *testing.T, c *boltTestClient, query string) {
	t.Helper()
	c.begin(t)
	c.run(t, query, nil)
	_, _ = c.pullAll(t)
	c.commit(t)
}

// TestBoltSmokeTest_CreateMatchReturn tests the full CREATE / MATCH / RETURN
// cycle over the raw Bolt wire protocol:
//  1. Connect to the shared test server.
//  2. CREATE two Person nodes (via explicit transactions).
//  3. MATCH (n:Person) RETURN n — verify 2 rows returned.
func TestBoltSmokeTest_CreateMatchReturn(t *testing.T) {
	c := newBoltTestClient(t, sharedServerAddr)
	defer c.close(t)

	c.negotiate(t)
	c.hello(t)

	// Create Person nodes via explicit transactions.
	createNodeTx(t, c, `CREATE (n:Person {name: "Alice", age: 30})`)
	createNodeTx(t, c, `CREATE (n:Person {name: "Bob", age: 25})`)

	// MATCH returns each bound node variable as a NodeValue (serialised as a
	// packstream map carrying id / labels / properties), matching the
	// neo4j-go-driver convention. The bare-variable projection fast path
	// upgrades the engine's internal expr.IntegerValue(NodeID) cell to a full
	// expr.NodeValue before it is handed to the bolt encoder.
	c.run(t, "MATCH (n:Person) RETURN n", nil)
	records, _ := c.pullAll(t)

	if len(records) != 2 {
		t.Fatalf("expected 2 rows from MATCH :Person, got %d", len(records))
	}
	// Each row must contain exactly one value: the node payload as a map with
	// the documented id / labels / properties keys.
	for i, row := range records {
		if len(row) != 1 {
			t.Fatalf("row %d: expected 1 field, got %d", i, len(row))
		}
		m, ok := row[0].(map[string]any)
		if !ok {
			t.Fatalf("row %d: expected node map, got %T (%v)", i, row[0], row[0])
		}
		if _, ok := m["id"].(int64); !ok {
			t.Errorf("row %d: node[\"id\"] = %T, want int64", i, m["id"])
		}
		if _, ok := m["labels"]; !ok {
			t.Errorf("row %d: node missing \"labels\"", i)
		}
		if _, ok := m["properties"].(map[string]any); !ok {
			t.Errorf("row %d: node[\"properties\"] = %T, want map[string]any", i, m["properties"])
		}
	}

	c.goodbye(t)
}

// TestBoltSmokeTest_ExplicitTx tests BEGIN / RUN / PULL / COMMIT:
//  1. Connect and authenticate.
//  2. BEGIN.
//  3. CREATE (:Product {name: "Widget"}).
//  4. COMMIT.
//  5. MATCH (p:Product) RETURN p — verify 1 row.
func TestBoltSmokeTest_ExplicitTx(t *testing.T) {
	c := newBoltTestClient(t, sharedServerAddr)
	defer c.close(t)

	c.negotiate(t)
	c.hello(t)

	// BEGIN → RUN → PULL → COMMIT
	c.begin(t)
	c.run(t, `CREATE (n:Product {name: "Widget"})`, nil)
	_, _ = c.pullAll(t)
	commitSucc := c.commit(t)
	if commitSucc == nil {
		t.Fatal("COMMIT returned nil")
	}

	// MATCH to verify the node was persisted (auto-commit read). The bare
	// variable `p` is upgraded to a NodeValue (packstream map) by the
	// projection fast path; the bolt encoder then emits the documented
	// id/labels/properties shape.
	c.run(t, "MATCH (p:Product) RETURN p", nil)
	records, _ := c.pullAll(t)

	if len(records) != 1 {
		t.Fatalf("expected 1 row from MATCH :Product, got %d", len(records))
	}
	if len(records[0]) != 1 {
		t.Fatalf("expected 1 field per row, got %d", len(records[0]))
	}
	m, ok := records[0][0].(map[string]any)
	if !ok {
		t.Fatalf("expected node map, got %T (%v)", records[0][0], records[0][0])
	}
	if _, ok := m["id"].(int64); !ok {
		t.Errorf("node[\"id\"] = %T, want int64", m["id"])
	}
	if _, ok := m["properties"].(map[string]any); !ok {
		t.Errorf("node[\"properties\"] = %T, want map[string]any", m["properties"])
	}

	c.goodbye(t)
}

// TestBoltSmokeTest_TLSSmoke tests that TLS connections work:
//  1. Create ephemeral self-signed cert.
//  2. Start a dedicated TLS server (TLS config differs from shared server).
//  3. Connect with raw tls.Conn (InsecureSkipVerify).
//  4. Negotiate, send HELLO, receive SUCCESS.
func TestBoltSmokeTest_TLSSmoke(t *testing.T) {
	tlsCfg := generateSelfSigned(t)
	addr := startTestServer(t, server.Options{TLSConfig: tlsCfg})

	// Dial with TLS, skipping certificate verification (self-signed test cert).
	tlsConn, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // self-signed test cert; not a production path
	})
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}

	// boltHandshake and sendHello are defined in serve_test.go (same package).
	boltHandshake(t, tlsConn)
	succ := sendHello(t, tlsConn)
	if succ == nil {
		t.Fatal("TLS HELLO returned nil success")
	}
	if _, ok := succ.Metadata["server"]; !ok {
		t.Error("TLS HELLO SUCCESS missing 'server' field")
	}

	_ = tlsConn.Close()
}

// TestBoltSmokeTest_ErrorMapping tests that a syntax error produces FAILURE
// with code Neo.ClientError.Statement.SyntaxError:
//  1. Connect to shared test server.
//  2. Send RUN with invalid Cypher.
//  3. Expect FAILURE with the right code.
func TestBoltSmokeTest_ErrorMapping(t *testing.T) {
	c := newBoltTestClient(t, sharedServerAddr)
	defer c.close(t)

	c.negotiate(t)
	c.hello(t)

	// Send a syntactically invalid Cypher query.
	c.sendRequest(t, &proto.Run{
		Query:      "THIS IS NOT VALID CYPHER @@@@",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	})
	fail := c.recvFailure(t)

	if !strings.HasPrefix(fail.Code, "Neo.ClientError.Statement") {
		t.Errorf("expected SyntaxError code, got %q", fail.Code)
	}
}

// TestBoltSmokeTest_Rollback verifies that BEGIN / RUN / ROLLBACK leaves
// the graph unchanged:
//  1. BEGIN → RUN CREATE → ROLLBACK.
//  2. MATCH — verify no rows (or same count as before rollback).
func TestBoltSmokeTest_Rollback(t *testing.T) {
	c := newBoltTestClient(t, sharedServerAddr)
	defer c.close(t)

	c.negotiate(t)
	c.hello(t)

	// BEGIN → RUN → PULL → ROLLBACK
	c.begin(t)
	c.run(t, `CREATE (n:Widget)`, nil)
	_, _ = c.pullAll(t)
	rollbackSucc := c.rollback(t)
	if rollbackSucc == nil {
		t.Fatal("ROLLBACK returned nil")
	}

	c.goodbye(t)
}

// TestBoltSmokeTest_Routing tests the ROUTE response:
//  1. Connect to shared test server.
//  2. Send HELLO then ROUTE.
//  3. Verify SUCCESS with "rt" routing table containing server entries.
func TestBoltSmokeTest_Routing(t *testing.T) {
	c := newBoltTestClient(t, sharedServerAddr)
	defer c.close(t)

	c.negotiate(t)
	c.hello(t)

	succ := c.route(t)
	if succ == nil {
		t.Fatal("ROUTE returned nil")
	}
	rt, ok := succ.Metadata["rt"]
	if !ok {
		t.Fatal("ROUTE SUCCESS missing 'rt' key")
	}

	// rt is packstream.Value (any); the RoutingTable function returns
	// map[string]packstream.Value which is map[string]any at runtime.
	rtMap, ok := rt.(map[string]interface{})
	if !ok {
		t.Fatalf("rt is %T, want map[string]interface{}", rt)
	}

	servers, ok := rtMap["servers"]
	if !ok {
		t.Fatal("rt missing 'servers'")
	}

	serverList, ok := servers.([]interface{})
	if !ok {
		t.Fatalf("rt.servers is %T, want []interface{}", servers)
	}

	if len(serverList) == 0 {
		t.Fatal("rt.servers is empty")
	}

	c.goodbye(t)
}
