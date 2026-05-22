package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

// TestSeedFixture_CountsMatch verifies that the fixture writes the
// exact number of nodes and edges documented in the schema.
func TestSeedFixture_CountsMatch(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}
	o, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer func() {
		if err := o.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}()

	seeded, err := seedFixture(o.store)
	if err != nil {
		t.Fatalf("seedFixture: %v", err)
	}
	if !seeded {
		t.Fatalf("seeded=false on a freshly-initialised data dir")
	}

	g := o.store.Graph()
	for _, u := range fixtureUsers {
		if !g.HasNodeLabel(u.username, labelUser) {
			t.Fatalf("missing User: %s", u.username)
		}
	}
	for _, p := range fixturePosts {
		if !g.HasNodeLabel(p.id, labelPost) {
			t.Fatalf("missing Post: %s", p.id)
		}
		if !g.HasEdgeLabel(p.author, p.id, relAuthored) {
			t.Fatalf("missing AUTHORED edge: %s -> %s", p.author, p.id)
		}
	}
	for _, c := range fixtureComments {
		if !g.HasNodeLabel(c.id, labelComment) {
			t.Fatalf("missing Comment: %s", c.id)
		}
		if !g.HasEdgeLabel(c.author, c.id, relAuthored) {
			t.Fatalf("missing AUTHORED edge: %s -> %s", c.author, c.id)
		}
		if !g.HasEdgeLabel(c.id, c.post, relOn) {
			t.Fatalf("missing ON edge: %s -> %s", c.id, c.post)
		}
		if c.replyTo != "" && !g.HasEdgeLabel(c.id, c.replyTo, relReplyOf) {
			t.Fatalf("missing REPLY_OF edge: %s -> %s", c.id, c.replyTo)
		}
	}
	for _, e := range fixtureFollows {
		if !g.HasEdgeLabel(e.from, e.to, relFollows) {
			t.Fatalf("missing FOLLOWS edge: %s -> %s", e.from, e.to)
		}
	}
	for _, l := range fixtureLikes {
		if !g.HasEdgeLabel(l.from, l.to, relLiked) {
			t.Fatalf("missing LIKED edge: %s -> %s", l.from, l.to)
		}
	}
}

// TestSeedFixture_Idempotent verifies that the second call to
// seedFixture is a no-op (seeded=false) and does not duplicate any
// node or edge.
func TestSeedFixture_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}
	o, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("openStore (1): %v", err)
	}
	if seeded, err := seedFixture(o.store); err != nil {
		t.Fatalf("seedFixture (1): %v", err)
	} else if !seeded {
		t.Fatalf("first seed reported seeded=false")
	}
	if err := o.Close(); err != nil {
		t.Fatalf("close (1): %v", err)
	}

	// Reopen and run seedFixture again.
	o2, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("openStore (2): %v", err)
	}
	defer func() {
		if err := o2.Close(); err != nil {
			t.Fatalf("close (2): %v", err)
		}
	}()

	if seeded, err := seedFixture(o2.store); err != nil {
		t.Fatalf("seedFixture (2): %v", err)
	} else if seeded {
		t.Fatalf("idempotent: second seed reported seeded=true")
	}
}

// TestRunSeed_OutputShape verifies that the JSON reply has the
// documented key set (status, seeded) and alphabetical ordering.
func TestRunSeed_OutputShape(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}
	var buf bytes.Buffer
	if err := runSeed(context.Background(), dir, &buf); err != nil {
		t.Fatalf("runSeed: %v", err)
	}
	var reply map[string]any
	if err := json.Unmarshal(buf.Bytes(), &reply); err != nil {
		t.Fatalf("invalid JSON reply: %v (%q)", err, buf.String())
	}
	if reply["status"] != "ok" {
		t.Fatalf("status: got %v, want ok", reply["status"])
	}
	if reply["seeded"] != true {
		t.Fatalf("seeded: got %v, want true", reply["seeded"])
	}
	if got := buf.String(); got != `{"seeded":true,"status":"ok"}`+"\n" {
		t.Fatalf("byte-exact reply mismatch: %q", got)
	}
}
