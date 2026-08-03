// Package bulkimport builds a labelled property graph from a stream of node and
// edge records, at bulk-loader speed, so that the result can be published as a
// store snapshot.
//
// # Why it exists
//
// The round-3 comparative audit measured GoGraph's Cypher write path at
// 35 m 33 s on a dataset Memgraph loads in 977 ms and Neo4j in 2.39 s — a 2184x
// deficit that dominates any first evaluation of the module — while store/bulk
// ingested the same edge volume in tens of milliseconds and was unreachable from
// anything a user could call. See docs/design-bulk-import.md for the spike that
// measured the alternatives (rmp #2177, #2178).
//
// # Why not store/bulk
//
// store/bulk is deliberately untouched. It ingests adjacency only — its record is
// (src, dst, weight) with no labels and no properties — and it emits a Tier-2
// csrfile, which is exactly what its consumers (bench/ldbc, bench/rmat) want.
// Extending it to carry labels and properties would reimplement, in a second
// place, storage that [lpg.Graph] already owns and that is already tested.
//
// The spike measured the cost of that reuse: driving lpg.Graph inside one
// adjacency commit window reaches 2.72 M edges/s against store/bulk's
// 3.92 M edges/s for adjacency alone — 44 % more time to carry a label and a
// property on every node and every edge.
//
// This package measures 2.068 M edges/s (BenchmarkImport_LabelsAndProperties,
// ±2 % over 6 runs), 24 % below the spike's figure, and the gap is deliberate.
// The spike used the fused AddEdgeLabeledWithProperty; this package uses the
// HANDLE API — AddEdgeH, then SetEdgeLabelByHandle and SetEdgePropertyByHandle —
// because in a multigraph a pair may carry several edges and a pair-addressed
// write would silently overwrite the first edge's type and properties with the
// second's. 24 % is the price of not corrupting parallel edges, and it is worth
// paying: at 2.068 M edges/s the audit's 200 000-edge dataset builds in ~97 ms,
// so the whole import including publication is still ~150 ms — 6.5x faster than
// Memgraph's 977 ms, 16x faster than Neo4j's 2.39 s, and four orders of
// magnitude faster than the 35 m 33 s Cypher write path.
//
// # Concurrency
//
// A [Builder] is NOT safe for concurrent use. It holds an adjacency commit
// window open for its whole life, which is an exclusive-build mode: it assumes
// no concurrent reader and no concurrent writer on the graph it is building.
// Build on one goroutine, call [Builder.Finish], and only then share the result.
package bulkimport

import (
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ErrFinished is returned by every ingest method once [Builder.Finish] has run.
var ErrFinished = errors.New("bulkimport: builder already finished")

// Node is one node record: its natural key, its labels, and its properties.
//
// Labels and Properties may both be empty; a node with neither is still created,
// which matters because an edge endpoint must exist. Duplicate keys are
// idempotent: the second record adds its labels and properties to the node the
// first created rather than failing, which is what a CSV split across files
// needs.
type Node struct {
	// Properties are set in map-iteration order, which is unspecified. That is
	// safe because each key is written once, so no ordering can change the result.
	Properties map[string]lpg.PropertyValue
	Key        string
	Labels     []string
}

// Edge is one edge record: its endpoints, its weight, its relationship type, and
// its properties.
//
// Src and Dst must have been added with [Builder.AddNode] first. That is a
// deliberate precondition rather than an implicit create: a typo in an edge file
// would otherwise silently produce a labelless, propertyless node, which is the
// class of silent-wrong-result the audit's correctness findings were about.
type Edge[W any] struct {
	Properties map[string]lpg.PropertyValue
	Src        string
	Dst        string
	// Type is the relationship type. Empty means an untyped edge.
	Type   string
	Weight W
}

// Options configures a [Builder].
type Options struct {
	// Directed selects a directed graph. openCypher requires directed
	// relationships, so a Cypher-facing import wants true.
	Directed bool

	// Multigraph allows parallel edges between the same pair. openCypher's data
	// model is a multigraph, so a Cypher-facing import wants true; without it a
	// second edge between an existing pair fails.
	Multigraph bool

	// ExpectNodes, when > 0, pre-sizes the interning table to that cardinality.
	// It is a pure capacity hint with no effect on the result.
	ExpectNodes int
}

// Stats reports what an import ingested.
type Stats struct {
	// Nodes is the number of DISTINCT node keys created.
	Nodes int
	// Edges is the number of edge records ingested.
	Edges int
	// NodeRecords is the number of node records consumed, which exceeds Nodes
	// when the input repeats a key.
	NodeRecords int
}

// Builder accumulates node and edge records into an [lpg.Graph].
//
// The graph is built inside ONE adjacency commit window, opened at construction
// and closed by [Builder.Finish]. That is the same exclusive-build mode WAL
// replay uses, and snapshot recovery since rmp #2170: within a window a shard's
// slot array is cloned once on first touch and mutated in place thereafter,
// instead of once per edge. Outside a window the same import would cost
// O(edges x shard size).
//
// Builder is NOT safe for concurrent use. See the package documentation.
type Builder[W any] struct {
	g        *lpg.Graph[string, W]
	seen     map[string]struct{}
	stats    Stats
	finished bool
}

// New returns a Builder over a fresh graph configured by opts.
func New[W any](opts Options) *Builder[W] {
	g := lpg.New[string, W](adjlist.Config{
		Directed:   opts.Directed,
		Multigraph: opts.Multigraph,
	})
	// Open the exclusive-build window for the whole import. Finish closes it,
	// which freezes every touched shard's builder before the graph is handed out.
	g.AdjList().BeginExclusiveBuild()
	hint := opts.ExpectNodes
	if hint < 0 {
		hint = 0
	}
	return &Builder[W]{g: g, seen: make(map[string]struct{}, hint)}
}

// AddNode ingests one node record, creating the node on first sight and merging
// labels and properties into it on any later sight of the same key.
func (b *Builder[W]) AddNode(n Node) error {
	if b.finished {
		return ErrFinished
	}
	if n.Key == "" {
		return fmt.Errorf("bulkimport: node record has an empty key")
	}
	b.stats.NodeRecords++
	if _, ok := b.seen[n.Key]; !ok {
		if err := b.g.AddNode(n.Key); err != nil {
			return fmt.Errorf("bulkimport: add node %q: %w", n.Key, err)
		}
		b.seen[n.Key] = struct{}{}
		b.stats.Nodes++
	}
	for _, l := range n.Labels {
		if l == "" {
			continue
		}
		if err := b.g.SetNodeLabel(n.Key, l); err != nil {
			return fmt.Errorf("bulkimport: label node %q as %q: %w", n.Key, l, err)
		}
	}
	for k, v := range n.Properties {
		if err := b.g.SetNodeProperty(n.Key, k, v); err != nil {
			return fmt.Errorf("bulkimport: set property %q on node %q: %w", k, n.Key, err)
		}
	}
	return nil
}

// AddNodes ingests a batch of node records, stopping at the first error.
func (b *Builder[W]) AddNodes(ns []Node) error {
	for i := range ns {
		if err := b.AddNode(ns[i]); err != nil {
			return err
		}
	}
	return nil
}

// AddEdge ingests one edge record. Both endpoints must already exist; an unknown
// endpoint is an error rather than an implicit node creation, so a mistyped key
// cannot silently produce a bare node.
//
// The edge is created through the HANDLE API ([lpg.Graph.AddEdgeH]) and its type
// and properties are attached to that handle. That is what makes parallel edges
// correct: in a multigraph, addressing an edge by (src, dst) alone is ambiguous,
// so a second edge between the same pair would otherwise overwrite the first
// one's type and properties.
func (b *Builder[W]) AddEdge(e Edge[W]) error {
	if b.finished {
		return ErrFinished
	}
	if _, ok := b.seen[e.Src]; !ok {
		return fmt.Errorf("bulkimport: edge source %q was never added as a node", e.Src)
	}
	if _, ok := b.seen[e.Dst]; !ok {
		return fmt.Errorf("bulkimport: edge target %q was never added as a node", e.Dst)
	}
	handle, err := b.g.AddEdgeH(e.Src, e.Dst, e.Weight)
	if err != nil {
		return fmt.Errorf("bulkimport: add edge %q->%q: %w", e.Src, e.Dst, err)
	}
	b.stats.Edges++
	if e.Type != "" {
		b.g.SetEdgeLabelByHandle(e.Src, e.Dst, handle, e.Type)
	}
	for k, v := range e.Properties {
		if perr := b.g.SetEdgePropertyByHandle(e.Src, e.Dst, handle, k, v); perr != nil {
			return fmt.Errorf("bulkimport: set property %q on edge %q->%q: %w", k, e.Src, e.Dst, perr)
		}
	}
	return nil
}

// AddEdges ingests a batch of edge records, stopping at the first error.
func (b *Builder[W]) AddEdges(es []Edge[W]) error {
	for i := range es {
		if err := b.AddEdge(es[i]); err != nil {
			return err
		}
	}
	return nil
}

// Finish closes the adjacency commit window and returns what was ingested. It
// must be called exactly once, before [Builder.Graph] is used: the window's close
// is what freezes each touched shard's builder, so a graph handed out before it
// could still be mutated in place under a reader.
//
// Finish is idempotent in the sense that a second call returns [ErrFinished]
// rather than closing the window twice.
func (b *Builder[W]) Finish() (Stats, error) {
	if b.finished {
		return b.stats, ErrFinished
	}
	b.finished = true
	b.g.AdjList().EndExclusiveBuild()
	return b.stats, nil
}

// Graph returns the built graph. It is only valid after [Builder.Finish]; before
// that it returns nil, because the adjacency commit window is still open and the
// shards' builders are still mutable.
func (b *Builder[W]) Graph() *lpg.Graph[string, W] {
	if !b.finished {
		return nil
	}
	return b.g
}

// Stats returns the running counts. They are valid at any time.
func (b *Builder[W]) Stats() Stats { return b.stats }
