package server

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// TestSession_HandleRoute_FullPayload verifies the remaining ACs of T746 not
// covered by TestSession_HandleRoute (which only confirms 'rt' key presence):
//
//  1. Response contains entries for READ, WRITE, and ROUTE roles.
//  2. Advertised address is present in every entry.
//  3. ttl is non-negative.
func TestSession_HandleRoute_FullPayload(t *testing.T) {
	t.Parallel()

	const advertised = "testhost:7687"
	sess := newSession(newTestEngine(t), NoAuthHandler{}, advertised)

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

	rawRT, ok := success.Metadata["rt"]
	if !ok {
		t.Fatal("SUCCESS metadata missing 'rt'")
	}
	rt, ok := rawRT.(map[string]packstream.Value)
	if !ok {
		t.Fatalf("'rt' type: %T, want map[string]packstream.Value", rawRT)
	}

	// AC3: ttl must be non-negative.
	rawTTL, ok := rt["ttl"]
	if !ok {
		t.Fatal("routing table missing 'ttl'")
	}
	ttl, ok := rawTTL.(int64)
	if !ok {
		t.Fatalf("'ttl' type: %T, want int64", rawTTL)
	}
	if ttl < 0 {
		t.Fatalf("ttl = %d; must be non-negative", ttl)
	}

	// AC1 + AC2: servers list must contain READ, WRITE, ROUTE entries, each
	// listing the advertised address.
	rawServers, ok := rt["servers"]
	if !ok {
		t.Fatal("routing table missing 'servers'")
	}
	servers, ok := rawServers.([]packstream.Value)
	if !ok {
		t.Fatalf("'servers' type: %T, want []packstream.Value", rawServers)
	}

	roles := map[string]bool{"READ": false, "WRITE": false, "ROUTE": false}
	for i, sv := range servers {
		entry, ok := sv.(map[string]packstream.Value)
		if !ok {
			t.Fatalf("servers[%d] type: %T", i, sv)
		}

		// AC1: role must be one of READ/WRITE/ROUTE.
		roleVal, ok := entry["role"]
		if !ok {
			t.Fatalf("servers[%d] missing 'role'", i)
		}
		role, ok := roleVal.(string)
		if !ok {
			t.Fatalf("servers[%d] 'role' type: %T", i, roleVal)
		}
		if _, known := roles[role]; !known {
			t.Fatalf("servers[%d] unexpected role %q", i, role)
		}
		roles[role] = true

		// AC2: advertised address must appear in every entry's addresses list.
		rawAddrs, ok := entry["addresses"]
		if !ok {
			t.Fatalf("servers[%d] missing 'addresses'", i)
		}
		addrs, ok := rawAddrs.([]packstream.Value)
		if !ok {
			t.Fatalf("servers[%d] 'addresses' type: %T", i, rawAddrs)
		}
		found := false
		for _, a := range addrs {
			if a == packstream.Value(advertised) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("servers[%d] (role=%s) addresses %v do not contain advertised address %q",
				i, role, addrs, advertised)
		}
	}

	// AC1: all three roles must be present.
	for role, present := range roles {
		if !present {
			t.Errorf("role %q missing from routing table", role)
		}
	}
}
