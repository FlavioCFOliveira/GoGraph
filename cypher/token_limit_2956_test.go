package cypher_test

// token_limit_2956_test.go — regression gate for rmp #2956 and #2748 at the
// engine level.
//
// #2956: on the WAL-backed engine, CREATE with a relationship type of 65536
// bytes or more reported success, the commit succeeded, and recovery read the
// relationship back with an EMPTY type. walMutatorAdapter.SetEdgeLabel and
// SetEdgeLabelByHandle discarded txn's staging refusal, which buffers nothing,
// so the encoder backstop the code relied on never saw the type.
//
// #2748: the store-less engine accepted the names the WAL-backed one refused.
//
// Every statement below must now fail with lpg.ErrTokenTooLong before commit,
// leave the live graph exactly as it was, and — on the WAL — recover to that
// same state. The two engines must refuse with the same message.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

const fixture2956 = `CREATE (:Seed {k: 1})-[:R {w: 1}]->(:Seed {k: 2})`

func statements2956() []struct{ name, query string } {
	long := strings.Repeat("T", lpg.MaxTokenLen+1)
	return []struct{ name, query string }{
		{"CREATE relationship type", "CREATE (:A)-[:" + long + "]->(:B)"},
		{"MATCH CREATE relationship type", "MATCH (a:Seed {k: 1}), (b:Seed {k: 2}) CREATE (a)-[:" + long + "]->(b)"},
		{"MERGE relationship type", "MATCH (a:Seed {k: 1}), (b:Seed {k: 2}) MERGE (a)-[:" + long + "]->(b)"},
		{"CREATE node label", "CREATE (:" + long + ")"},
		{"SET node label", "MATCH (n:Seed {k: 1}) SET n:" + long},
		{"REMOVE node label", "MATCH (n:Seed {k: 1}) REMOVE n:" + long},
		{"CREATE node property key", "CREATE (:A {`" + long + "`: 1})"},
		{"SET node property key", "MATCH (n:Seed {k: 1}) SET n.`" + long + "` = 1"},
		{"REMOVE node property key", "MATCH (n:Seed {k: 1}) REMOVE n.`" + long + "`"},
		{"SET relationship property key", "MATCH (:Seed)-[r:R]->() SET r.`" + long + "` = 1"},
		{"REMOVE relationship property key", "MATCH (:Seed)-[r:R]->() REMOVE r.`" + long + "`"},
	}
}

// state2956 renders the whole graph: every node's labels and properties, and
// every relationship's endpoints, type and properties, in a canonical order.
func state2956(t *testing.T, g *lpg.Graph[string, float64]) string {
	t.Helper()
	var keys []string
	g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if !g.IsTombstoned(id) {
			keys = append(keys, key)
		}
		return true
	})
	sort.Strings(keys)
	props := func(m map[string]lpg.PropertyValue) string {
		ks := make([]string, 0, len(m))
		for k := range m {
			ks = append(ks, fmt.Sprintf("%s=%v", k, m[k]))
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	var lines []string
	for _, k := range keys {
		ls := append([]string(nil), g.NodeLabels(k)...)
		sort.Strings(ls)
		lines = append(lines, fmt.Sprintf("node %v {%s}", ls, props(g.NodeProperties(k))))
		for _, d := range keys {
			if !g.AdjList().HasEdge(k, d) {
				continue
			}
			el := append([]string(nil), g.EdgeLabels(k, d)...)
			sort.Strings(el)
			lines = append(lines, fmt.Sprintf("edge %v {%s}", el, props(g.EdgeProperties(k, d))))
		}
	}
	sort.Strings(lines)
	return fmt.Sprintf("%d nodes; %s", len(keys), strings.Join(lines, " | "))
}

func walEngine2956(t *testing.T) (*cypher.Engine, *lpg.Graph[string, float64], *wal.Writer, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{})
	st := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return cypher.NewEngineWithStore(st), g, w, dir
}

func assertRefused2956(t *testing.T, err error, query string) {
	t.Helper()
	if err == nil {
		t.Fatalf("statement was ACCEPTED; want lpg.ErrTokenTooLong\n  query: %.80s…", query)
	}
	if !errors.Is(err, lpg.ErrTokenTooLong) {
		t.Fatalf("error %.200v does not wrap lpg.ErrTokenTooLong", err)
	}
	if !errors.Is(err, txn.ErrTokenTooLong) {
		t.Fatalf("error %.200v does not match txn.ErrTokenTooLong", err)
	}
}

// TestTokenLimit_WALRefusesBeforeCommit_2956 is the #2956 acceptance: refused
// before commit, nothing written to the live graph, and the recovered graph
// identical to the live one.
func TestTokenLimit_WALRefusesBeforeCommit_2956(t *testing.T) {
	t.Parallel()
	for _, st := range statements2956() {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			eng, g, w, dir := walEngine2956(t)
			if err := runToCompletion2747(t, eng, fixture2956); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			before := state2956(t, g)
			assertRefused2956(t, runToCompletion2747(t, eng, st.query), st.query)
			if after := state2956(t, g); after != before {
				t.Fatalf("the refused statement changed the live graph\n  before: %s\n  after:  %s", before, after)
			}
			// The store is still usable after the refusal.
			if err := runToCompletion2747(t, eng, `CREATE (:After)`); err != nil {
				t.Fatalf("the store refused a valid write after the refusal: %v", err)
			}
			live := state2956(t, g)
			if got := state2956(t, recoveredGraph2747(t, w, dir)); got != live {
				t.Fatalf("recovered graph differs from the live graph\n  live:      %s\n  recovered: %s", live, got)
			}
		})
	}
}

// TestTokenLimit_InMemoryMatchesWAL_2748 pins the parity: the store-less engine
// refuses every statement the WAL-backed one does, with the same message, and
// is left unchanged.
func TestTokenLimit_InMemoryMatchesWAL_2748(t *testing.T) {
	t.Parallel()
	for _, st := range statements2956() {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			g := lpg.New[string, float64](adjlist.Config{})
			mem := cypher.NewEngine(g)
			if err := runToCompletion2747(t, mem, fixture2956); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			before := state2956(t, g)
			memErr := runToCompletion2747(t, mem, st.query)
			assertRefused2956(t, memErr, st.query)
			if after := state2956(t, g); after != before {
				t.Fatalf("the refused statement changed the in-memory graph\n  before: %s\n  after:  %s", before, after)
			}

			walEng, _, _, _ := walEngine2956(t)
			if err := runToCompletion2747(t, walEng, fixture2956); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			walErr := runToCompletion2747(t, walEng, st.query)
			assertRefused2956(t, walErr, st.query)
			if tail(memErr) != tail(walErr) {
				t.Errorf("the engines refuse differently\n  in-memory: %s\n  wal:       %s", tail(memErr), tail(walErr))
			}
		})
	}
}

// tail is the refusal's own sentence — "token too long: <what> is N bytes,
// maximum M" — without the wrapping prefixes each engine adds.
func tail(err error) string {
	s := err.Error()
	if i := strings.Index(s, "token too long"); i >= 0 {
		return s[i:]
	}
	return s
}

// TestTokenLimit_AtTheLimitIsAccepted_2748 pins the boundary through the
// engine: a relationship type of exactly lpg.MaxTokenLen bytes commits and
// recovers intact.
func TestTokenLimit_AtTheLimitIsAccepted_2748(t *testing.T) {
	t.Parallel()
	eng, g, w, dir := walEngine2956(t)
	at := strings.Repeat("T", lpg.MaxTokenLen)
	if err := runToCompletion2747(t, eng, "CREATE (:A)-[:"+at+"]->(:B)"); err != nil {
		t.Fatalf("a %d-byte relationship type was refused: %v", lpg.MaxTokenLen, err)
	}
	res, err := eng.Run(context.Background(), "MATCH ()-[r]->() RETURN size(type(r)) AS n", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	if !res.Next() {
		t.Fatal("no relationship")
	}
	if got := fmt.Sprint(res.Record()["n"]); got != fmt.Sprint(lpg.MaxTokenLen) {
		t.Fatalf("type length = %s, want %d", got, lpg.MaxTokenLen)
	}
	live := state2956(t, g)
	if got := state2956(t, recoveredGraph2747(t, w, dir)); got != live {
		t.Fatalf("recovered graph differs\n  live:      %.300s\n  recovered: %.300s", live, got)
	}
}
