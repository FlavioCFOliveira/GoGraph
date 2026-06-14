package server

// security_bolt_cartesian_notification_test.go —
// [SEC-2026-06-14b][BOLT] #1483 Cartesian-product notification surfacing.
//
// The engine emits a plan-time Cartesian-product notification for a disconnected
// pattern (cypher.Result.Notifications, mirroring Neo4j). The Bolt server must
// forward it to the client in the terminal PULL SUCCESS "notifications"
// metadata, so a driver can warn the user. This test drives RUN + PULL directly
// through HandleMessage and asserts the metadata.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// cartesianSeededEngine builds an engine with a few labelled nodes so a
// disconnected MATCH has a non-empty binding set.
func cartesianSeededEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for _, k := range []string{"a", "b", "c"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %q: %v", k, err)
		}
		if err := g.SetNodeLabel(k, "N"); err != nil {
			t.Fatalf("SetNodeLabel %q: %v", k, err)
		}
	}
	return cypher.NewEngine(g)
}

// runPullMeta runs query in autocommit mode and returns the terminal PULL
// SUCCESS metadata.
func runPullMeta(t *testing.T, sess *Session, query string) map[string]packstream.Value {
	t.Helper()
	ctx := context.Background()
	if _, err := sess.HandleMessage(ctx, helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if _, err := sess.HandleMessage(ctx, &proto.Run{Query: query, Extra: map[string]interface{}{}}); err != nil {
		t.Fatalf("RUN: %v", err)
	}
	resp, err := sess.HandleMessage(ctx, &proto.Pull{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("PULL: %v", err)
	}
	// The terminal SUCCESS is the last response.
	last, ok := resp[len(resp)-1].(*proto.Success)
	if !ok {
		t.Fatalf("PULL last response %T, want *proto.Success", resp[len(resp)-1])
	}
	return last.Metadata
}

// TestSec_Bolt_Cartesian_NotificationInPullSuccess asserts a disconnected
// pattern's notification reaches the PULL SUCCESS metadata, and a connected
// pattern produces no notifications key (#1483).
func TestSec_Bolt_Cartesian_NotificationInPullSuccess(t *testing.T) {
	t.Run("disconnected_emits_notification", func(t *testing.T) {
		sess := newSession(cartesianSeededEngine(t), NoAuthHandler{}, "")
		meta := runPullMeta(t, sess, "MATCH (a:N),(b:N) RETURN count(*) AS c")

		raw, ok := meta["notifications"]
		if !ok {
			t.Fatalf("disconnected pattern: PULL SUCCESS metadata has no \"notifications\" key: %+v", meta)
		}
		list, ok := raw.([]packstream.Value)
		if !ok || len(list) == 0 {
			t.Fatalf("notifications value %T (%v); want a non-empty []packstream.Value", raw, raw)
		}
		m, ok := list[0].(map[string]packstream.Value)
		if !ok {
			t.Fatalf("notification[0] %T; want map[string]packstream.Value", list[0])
		}
		if code, _ := m["code"].(string); code != "Neo.ClientNotification.Statement.CartesianProductWarning" {
			t.Fatalf("notification code = %q; want the Neo4j Cartesian-product code", code)
		}
		for _, k := range []string{"title", "description", "severity", "category"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("notification missing %q: %+v", k, m)
			}
		}
	})

	t.Run("connected_emits_none", func(t *testing.T) {
		sess := newSession(cartesianSeededEngine(t), NoAuthHandler{}, "")
		meta := runPullMeta(t, sess, "MATCH (a:N)-[r]->(b:N) RETURN count(*) AS c")
		if _, ok := meta["notifications"]; ok {
			t.Fatalf("connected pattern must not carry a notifications key: %+v", meta)
		}
	})
}
