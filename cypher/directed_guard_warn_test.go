package cypher_test

// directed_guard_warn_test.go — regression test for the production-readiness
// audit backlog bug #1892.
//
// openCypher relationships are directed. Constructing the engine over a
// non-directed (undirected) adjacency silently produced incorrect edge results
// (directed MATCH/traversal over a symmetric store). The constructor now emits
// a clear warning covering directedness, mirroring the existing non-multigraph
// warning, so the misconfiguration surfaces at construction rather than as
// silently wrong output.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestNewEngine_WarnsOnUndirectedBackend(t *testing.T) {
	// Not parallel: swaps the global slog default handler.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	// Directed:false, Multigraph:true → isolates the directedness warning.
	g := lpg.New[string, float64](adjlist.Config{Directed: false, Multigraph: true})
	_ = cypher.NewEngine(g)

	out := buf.String()
	if !strings.Contains(out, "non-directed") || !strings.Contains(out, "directed relationships") {
		t.Fatalf("expected a directedness warning at construction; got: %q", out)
	}
}

func TestNewEngine_NoDirectednessWarnOnDirectedBackend(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	// Directed:true, Multigraph:true → no warning of either kind.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	_ = cypher.NewEngine(g)

	if out := buf.String(); strings.Contains(out, "non-directed") {
		t.Fatalf("directedness warning emitted for a directed backend: %q", out)
	}
}
