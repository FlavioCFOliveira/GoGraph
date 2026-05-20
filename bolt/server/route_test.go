package server

import (
	"context"
	"testing"

	"gograph/bolt/packstream"
	"gograph/bolt/proto"
)

// ─────────────────────────────────────────────────────────────────────────────
// Task 316: Routing table tests
// ─────────────────────────────────────────────────────────────────────────────

// TestRoutingTable verifies the structure returned by RoutingTable for a
// single-host address.
func TestRoutingTable(t *testing.T) {
	t.Parallel()

	const addr = "localhost:7687"
	rt := RoutingTable(addr)

	// Top-level must have "rt".
	rawRT, ok := rt["rt"]
	if !ok {
		t.Fatal("RoutingTable missing 'rt' key")
	}
	inner, ok := rawRT.(map[string]packstream.Value)
	if !ok {
		t.Fatalf("RoutingTable 'rt' value type: %T", rawRT)
	}

	// TTL must be 300.
	ttl, ok := inner["ttl"]
	if !ok {
		t.Fatal("routing table missing 'ttl'")
	}
	if ttl != int64(300) {
		t.Errorf("ttl: got %v, want 300", ttl)
	}

	// Servers list.
	rawServers, ok := inner["servers"]
	if !ok {
		t.Fatal("routing table missing 'servers'")
	}
	servers, ok := rawServers.([]packstream.Value)
	if !ok {
		t.Fatalf("'servers' type: %T", rawServers)
	}
	if len(servers) != 3 {
		t.Fatalf("servers count: got %d, want 3", len(servers))
	}

	roles := map[string]bool{"WRITE": false, "READ": false, "ROUTE": false}
	for i, sv := range servers {
		entry, ok := sv.(map[string]packstream.Value)
		if !ok {
			t.Fatalf("servers[%d] type: %T", i, sv)
		}
		role, ok := entry["role"]
		if !ok {
			t.Fatalf("servers[%d] missing 'role'", i)
		}
		roleStr, ok := role.(string)
		if !ok {
			t.Fatalf("servers[%d] 'role' type: %T", i, role)
		}
		roles[roleStr] = true

		// Each entry must list the address.
		rawAddrs, ok := entry["addresses"]
		if !ok {
			t.Fatalf("servers[%d] missing 'addresses'", i)
		}
		addrs, ok := rawAddrs.([]packstream.Value)
		if !ok {
			t.Fatalf("servers[%d] 'addresses' type: %T", i, rawAddrs)
		}
		if len(addrs) != 1 {
			t.Fatalf("servers[%d] addresses count: %d", i, len(addrs))
		}
		if addrs[0] != packstream.Value(addr) {
			t.Errorf("servers[%d] address: got %v, want %s", i, addrs[0], addr)
		}
	}

	for role, found := range roles {
		if !found {
			t.Errorf("role %q not present in routing table", role)
		}
	}
}

// TestSession_HandleRoute verifies that a ROUTE message in READY state returns
// a SUCCESS with the routing table metadata.
func TestSession_HandleRoute(t *testing.T) {
	t.Parallel()

	sess := newSession(newTestEngine(t), NoAuthHandler{}, "localhost:7687")

	// Move to READY via HELLO.
	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}

	msgs, err := sess.HandleMessage(context.Background(), &proto.Route{
		Routing:   map[string]packstream.Value{},
		Bookmarks: nil,
		DB:        nil,
	})
	if err != nil {
		t.Fatalf("ROUTE: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("ROUTE response count: %d, want 1", len(msgs))
	}
	success, ok := msgs[0].(*proto.Success)
	if !ok {
		t.Fatalf("ROUTE response type: %T, want *proto.Success", msgs[0])
	}
	if _, ok := success.Metadata["rt"]; !ok {
		t.Fatal("ROUTE SUCCESS metadata missing 'rt'")
	}
}
