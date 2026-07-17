package jsonl_test

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/jsonl"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestJSONL_EmptyIdentifierRoundtrip is the regression guard for rmp #2043.
// The shared Record used omitempty on id/src/dst/key, so an empty-but-present
// identifier was omitted on write and re-read as absent, and the record was
// wrongly rejected ("missing id" / "missing src/dst" / "missing id/key"). The
// engine accepts an empty node id, edge endpoint, and property key, so each
// must now survive a write -> read cycle, while a genuinely absent field is
// still rejected.
func TestJSONL_EmptyIdentifierRoundtrip(t *testing.T) {
	t.Parallel()

	// A node whose id is the empty string round-trips, alongside a normal node.
	t.Run("node_empty_id", func(t *testing.T) {
		t.Parallel()
		a := adjlist.New[string, int64](adjlist.Config{Directed: true})
		if err := a.AddNode(""); err != nil {
			t.Fatalf("AddNode(%q): %v", "", err)
		}
		if err := a.AddNode("real"); err != nil {
			t.Fatalf("AddNode(real): %v", err)
		}

		var sb strings.Builder
		if _, err := jsonl.Write(&sb, a); err != nil {
			t.Fatalf("Write: %v", err)
		}
		out := sb.String()
		if !strings.Contains(out, `{"type":"node","id":""}`) {
			t.Fatalf("empty-id node not serialised with a present empty id:\n%s", out)
		}

		b, _, err := jsonl.ReadInto(strings.NewReader(out), adjlist.Config{Directed: true})
		if err != nil {
			t.Fatalf("ReadInto: %v", err)
		}
		if _, ok := b.Mapper().Lookup(""); !ok {
			t.Errorf("empty-id node lost after roundtrip")
		}
		if _, ok := b.Mapper().Lookup("real"); !ok {
			t.Errorf("node %q lost after roundtrip", "real")
		}
		if got := b.Order(); got != 2 {
			t.Errorf("node count = %d, want 2", got)
		}
	})

	// An edge with an empty-string endpoint round-trips.
	t.Run("edge_empty_endpoint", func(t *testing.T) {
		t.Parallel()
		a := adjlist.New[string, int64](adjlist.Config{Directed: true})
		if err := a.AddEdge("", "b", 5); err != nil {
			t.Fatalf("AddEdge(%q -> b): %v", "", err)
		}

		var sb strings.Builder
		if _, err := jsonl.Write(&sb, a); err != nil {
			t.Fatalf("Write: %v", err)
		}
		out := sb.String()
		if !strings.Contains(out, `"src":""`) {
			t.Fatalf("empty edge endpoint not serialised as present empty value:\n%s", out)
		}

		b, _, err := jsonl.ReadInto(strings.NewReader(out), adjlist.Config{Directed: true})
		if err != nil {
			t.Fatalf("ReadInto: %v", err)
		}
		if !b.HasEdge("", "b") {
			t.Errorf("edge (%q -> b) lost after roundtrip", "")
		}
	})

	// A property whose key is the empty string round-trips with its value.
	t.Run("empty_property_key", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, int64](adjlist.Config{Directed: true})
		if err := g.AddNode("n"); err != nil {
			t.Fatalf("AddNode(n): %v", err)
		}
		if err := g.SetNodeProperty("n", "", lpg.StringValue("v")); err != nil {
			t.Fatalf("SetNodeProperty(n, %q): %v", "", err)
		}

		var sb strings.Builder
		if _, err := jsonl.WriteWithProps(&sb, g); err != nil {
			t.Fatalf("WriteWithProps: %v", err)
		}
		out := sb.String()
		if !strings.Contains(out, `"key":""`) {
			t.Fatalf("empty property key not serialised as present empty value:\n%s", out)
		}

		h, _, err := jsonl.ReadWithProps(strings.NewReader(out), adjlist.Config{Directed: true})
		if err != nil {
			t.Fatalf("ReadWithProps: %v", err)
		}
		pv, ok := h.GetNodeProperty("n", "")
		if !ok {
			t.Fatalf("empty-key property lost after roundtrip")
		}
		if s, _ := pv.String(); s != "v" {
			t.Errorf("empty-key property value = %q, want %q", s, "v")
		}
	})

	// A genuinely ABSENT id must still be rejected — the fix must not turn a
	// malformed record into a silently-accepted one.
	t.Run("absent_id_still_errors", func(t *testing.T) {
		t.Parallel()
		if _, _, err := jsonl.ReadInto(strings.NewReader(`{"type":"node"}`+"\n"), adjlist.Config{Directed: true}); err == nil {
			t.Fatal("absent node id must still error")
		}
		if _, _, err := jsonl.ReadInto(strings.NewReader(`{"type":"edge","dst":"b"}`+"\n"), adjlist.Config{Directed: true}); err == nil {
			t.Fatal("absent edge src must still error")
		}
	})
}
