package txn_test

// Regression battery for rmp #2742 — an acknowledged commit that silently
// loses or corrupts data (ACID Durability + Consistency, Compliance Mandate 2).
//
// Five op-body encoders in store/txn wrote a uint16 length prefix in front of a
// caller-supplied string with NO bound on that string's length, while their
// siblings — the constraint encoder and the index encoder, both added by #1903
// for exactly this reason — were bounded by checkWALSchemaString. A label or a
// property key longer than 65535 bytes therefore wrapped its own length prefix
// modulo 65536 and every call on the path still returned nil:
//
//	label written | SetNodeLabel | Commit | recovered
//	65,535 B      | nil          | nil    | 65,535 B, correct
//	65,536 B      | nil          | nil    | 0 B, silently lost
//	70,000 B      | nil          | nil    | 4,464 B, silently WRONG
//
// The tests here drive the PUBLIC API only — Store.Begin, the Tx mutators,
// Tx.Commit, wal.Writer.Close, recovery.Open — and assert the commit is
// REFUSED, not that recovery happens to agree with the writer. Recovery is the
// gate: a refused write must never reach the WAL, and everything that does
// reach it must replay identically.
//
// Every case also asserts a CONTROL mutation of the same transaction shape IS
// recovered. Without it "the oversize label is absent" would pass vacuously on
// a recovery that returned an empty graph for an unrelated reason.

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// uint16PrefixMax is the largest byte length a uint16 length prefix can carry.
// It is the boundary the encoders must bound at — and must NOT bound below.
const uint16PrefixMax = 1<<16 - 1

// boundHarness is one store + WAL + directory, reopened through recovery.
type boundHarness struct {
	dir string
	w   *wal.Writer
	s   *txn.Store[string, int64]
}

func newBoundHarness(t *testing.T) *boundHarness {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	return &boundHarness{
		dir: dir,
		w:   w,
		s: txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewInt64WeightCodec(),
		}),
	}
}

// recoverGraph closes the WAL and replays the directory, returning the
// post-recovery graph — the only state that decides whether the commit was
// honest.
func (h *boundHarness) recoverGraph(t *testing.T) *lpg.Graph[string, int64] {
	t.Helper()
	if err := h.w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	res, err := recovery.Open[string, int64](h.dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	return res.Graph
}

// seedControl commits the control fixture every case shares: a node and an
// edge, each carrying a short label and a short property. Recovering it proves
// the replay actually ran.
func (h *boundHarness) seedControl(t *testing.T) {
	t.Helper()
	tx := h.s.Begin()
	mustNil(t, "AddNode(alice)", tx.AddNode("alice"))
	mustNil(t, "AddNode(bob)", tx.AddNode("bob"))
	mustNil(t, "SetNodeLabel(control)", tx.SetNodeLabel("alice", "ControlLabel"))
	mustNil(t, "SetNodeProperty(control)", tx.SetNodeProperty("alice", "controlKey", lpg.StringValue("controlValue")))
	mustNil(t, "AddEdge", tx.AddEdge("alice", "bob", 7))
	mustNil(t, "SetEdgeLabel(control)", tx.SetEdgeLabel("alice", "bob", "CONTROL_EDGE"))
	mustNil(t, "SetEdgeProperty(control)", tx.SetEdgeProperty("alice", "bob", "controlEdgeKey", lpg.StringValue("controlEdgeValue")))
	mustNil(t, "Commit(control)", tx.Commit())
}

// assertControlRecovered fails unless every control mutation survived replay.
// This is the "assert something WAS seen" oracle: it is what stops the
// absence checks below from passing on an empty graph.
func assertControlRecovered(t *testing.T, g *lpg.Graph[string, int64]) {
	t.Helper()
	if !g.HasNodeLabel("alice", "ControlLabel") {
		t.Fatalf("control not recovered: node label ControlLabel missing (recovered labels %q)", g.NodeLabels("alice"))
	}
	v, ok := g.GetNodeProperty("alice", "controlKey")
	if !ok {
		t.Fatal("control not recovered: node property controlKey missing")
	}
	if s, _ := v.String(); s != "controlValue" {
		t.Fatalf("control not recovered: node property controlKey = %q, want %q", s, "controlValue")
	}
	if !containsString(g.EdgeLabels("alice", "bob"), "CONTROL_EDGE") {
		t.Fatalf("control not recovered: edge label CONTROL_EDGE missing (recovered %q)", g.EdgeLabels("alice", "bob"))
	}
	if _, ok := g.EdgeProperties("alice", "bob")["controlEdgeKey"]; !ok {
		t.Fatal("control not recovered: edge property controlEdgeKey missing")
	}
}

func mustNil(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", what, err)
	}
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// oversizeCase is one mutator whose string field is length-prefixed with a
// uint16 on the wire.
type oversizeCase struct {
	name string
	// buffer stages the mutation carrying the supplied string on tx.
	buffer func(tx *txn.Tx[string, int64], s string) error
	// present reports whether the recovered graph holds the string — used both
	// to assert a refused write is ABSENT and to assert an at-cap write is
	// present and byte-exact.
	present func(g *lpg.Graph[string, int64], s string) bool
	// anyTail reports whether the recovered graph holds ANY value in the field
	// under test beyond the control fixture. It catches the silent-truncation
	// signature: an empty or 4464-byte residue where nothing should exist.
	anyTail func(g *lpg.Graph[string, int64]) []string
}

func oversizeCases() []oversizeCase {
	return []oversizeCase{
		{
			name:   "node_label", // -> encodeOpEdgeWithLabel (OpSetNodeLabel)
			buffer: func(tx *txn.Tx[string, int64], s string) error { return tx.SetNodeLabel("carol", s) },
			present: func(g *lpg.Graph[string, int64], s string) bool {
				return g.HasNodeLabel("carol", s)
			},
			anyTail: func(g *lpg.Graph[string, int64]) []string { return g.NodeLabels("carol") },
		},
		{
			name:   "removed_node_label", // -> encodeOpNodeOnly (OpRemoveNodeLabel)
			buffer: func(tx *txn.Tx[string, int64], s string) error { return tx.RemoveNodeLabel("alice", s) },
			// A removal of a label that was never set is a no-op in the graph, so
			// presence is decided by the WAL round trip alone: the frame must
			// either be refused outright or replay without disturbing the control
			// label. What must never happen is the truncated key colliding with a
			// real label and removing it.
			present: func(g *lpg.Graph[string, int64], _ string) bool {
				return g.HasNodeLabel("alice", "ControlLabel")
			},
			anyTail: func(g *lpg.Graph[string, int64]) []string { return nil },
		},
		{
			name: "node_property_key", // -> encodeOpNodeProperty
			buffer: func(tx *txn.Tx[string, int64], s string) error {
				return tx.SetNodeProperty("carol", s, lpg.StringValue("v"))
			},
			present: func(g *lpg.Graph[string, int64], s string) bool {
				_, ok := g.GetNodeProperty("carol", s)
				return ok
			},
			anyTail: func(g *lpg.Graph[string, int64]) []string {
				return mapKeys(g.NodeProperties("carol"))
			},
		},
		{
			name:   "edge_label", // -> encodeOpEdgeWithLabel (OpSetEdgeLabel)
			buffer: func(tx *txn.Tx[string, int64], s string) error { return tx.SetEdgeLabel("alice", "bob", s) },
			present: func(g *lpg.Graph[string, int64], s string) bool {
				return containsString(g.EdgeLabels("alice", "bob"), s)
			},
			anyTail: func(g *lpg.Graph[string, int64]) []string {
				var extra []string
				for _, l := range g.EdgeLabels("alice", "bob") {
					if l != "CONTROL_EDGE" {
						extra = append(extra, l)
					}
				}
				return extra
			},
		},
		{
			name: "edge_property_key", // -> encodeOpEdgeProperty
			buffer: func(tx *txn.Tx[string, int64], s string) error {
				return tx.SetEdgeProperty("alice", "bob", s, lpg.StringValue("v"))
			},
			present: func(g *lpg.Graph[string, int64], s string) bool {
				_, ok := g.EdgeProperties("alice", "bob")[s]
				return ok
			},
			anyTail: func(g *lpg.Graph[string, int64]) []string {
				var extra []string
				for k := range g.EdgeProperties("alice", "bob") {
					if k != "controlEdgeKey" {
						extra = append(extra, k)
					}
				}
				return extra
			},
		},
	}
}

func mapKeys(m map[string]lpg.PropertyValue) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCommitRefusesOversizeUint16Field_2742 is the failing-on-HEAD regression.
//
// For every uint16-prefixed field reachable through the public API, a string
// one byte past the prefix's capacity must make Commit return an error, and the
// recovered graph must hold no residue of it.
func TestCommitRefusesOversizeUint16Field_2742(t *testing.T) {
	t.Parallel()

	sizes := []int{uint16PrefixMax + 1, 70000}
	var observed atomic.Int64
	t.Cleanup(func() {
		if got := observed.Load(); got != int64(len(oversizeCases())*len(sizes)) {
			t.Errorf("vacuous run: %d oversize cases reached their assertions, want %d",
				got, len(oversizeCases())*len(sizes))
		}
	})

	for _, c := range oversizeCases() {
		for _, n := range sizes {
			t.Run(fmt.Sprintf("%s_%dB", c.name, n), func(t *testing.T) {
				t.Parallel()
				h := newBoundHarness(t)
				h.seedControl(t)

				big := strings.Repeat("L", n)
				tx := h.s.Begin()
				mustNil(t, "AddNode(carol)", tx.AddNode("carol"))
				// Buffering may legitimately refuse (a fail-fast API-boundary
				// design) or accept (an encode-time fail-stop design). Either is
				// acceptable; what is NOT acceptable is Commit acknowledging a
				// write it cannot represent.
				bufErr := c.buffer(tx, big)
				commitErr := tx.Commit()

				if bufErr == nil && commitErr == nil {
					t.Fatalf("ACID Durability breach: a %d-byte value in a uint16-prefixed field "+
						"was ACKNOWLEDGED (buffer err=nil, Commit err=nil); the length prefix "+
						"wraps to %d and recovery reads a value that was never written",
						n, n%(uint16PrefixMax+1))
				}

				g := h.recoverGraph(t)
				assertControlRecovered(t, g)

				if c.present(g, big) != (c.name == "removed_node_label") {
					// For every case but removed_node_label, present() must be
					// false: the refused value must be absent. For
					// removed_node_label, present() checks the control label
					// survived, so it must be true.
					t.Fatalf("recovered graph disagrees with the refusal (case %s, %d bytes)", c.name, n)
				}
				if tail := c.anyTail(g); len(tail) != 0 {
					lens := make([]int, len(tail))
					for i, s := range tail {
						lens[i] = len(s)
					}
					t.Fatalf("refused write left a residue in the recovered graph: %d entries with byte lengths %v "+
						"(the truncation signature is %d or 0)", len(tail), lens, n%(uint16PrefixMax+1))
				}
				observed.Add(1)
			})
		}
	}
}

// TestCommitAcceptsUint16FieldAtCap_2742 is the over-restriction guard: 65535
// bytes is representable and must keep working, byte-for-byte, through commit
// and recovery. A fix that capped labels at some tidier number would break
// legitimate callers and would fail here.
func TestCommitAcceptsUint16FieldAtCap_2742(t *testing.T) {
	t.Parallel()

	var observed atomic.Int64
	const wantCases = 4 // every case but removed_node_label
	t.Cleanup(func() {
		if got := observed.Load(); got != wantCases {
			t.Errorf("vacuous run: %d at-cap cases reached their assertions, want %d", got, wantCases)
		}
	})
	for _, c := range oversizeCases() {
		if c.name == "removed_node_label" {
			// A removal of a never-set label is a graph no-op; there is nothing
			// to observe after recovery, so it cannot witness the at-cap round
			// trip. The other four cases do.
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newBoundHarness(t)
			h.seedControl(t)

			atCap := strings.Repeat("C", uint16PrefixMax)
			tx := h.s.Begin()
			mustNil(t, "AddNode(carol)", tx.AddNode("carol"))
			mustNil(t, "buffer at cap", c.buffer(tx, atCap))
			mustNil(t, "Commit at cap", tx.Commit())

			g := h.recoverGraph(t)
			assertControlRecovered(t, g)
			if !c.present(g, atCap) {
				t.Fatalf("over-restricted: a %d-byte value (the exact uint16 capacity) did not survive recovery", uint16PrefixMax)
			}
			observed.Add(1)
		})
	}
}

// TestCommitAcceptsLargePropertyValue_2742 pins the scope of the bound. A
// property VALUE is uint32-prefixed, not uint16-prefixed, so 70000 bytes is
// well within its capacity and must not be refused by a fix aimed at the
// uint16 fields.
func TestCommitAcceptsLargePropertyValue_2742(t *testing.T) {
	t.Parallel()

	h := newBoundHarness(t)
	h.seedControl(t)

	const n = 70000
	big := strings.Repeat("V", n)

	tx := h.s.Begin()
	mustNil(t, "AddNode(carol)", tx.AddNode("carol"))
	mustNil(t, "SetNodeProperty(big value)", tx.SetNodeProperty("carol", "blob", lpg.StringValue(big)))
	mustNil(t, "SetEdgeProperty(big value)", tx.SetEdgeProperty("alice", "bob", "blob", lpg.StringValue(big)))
	mustNil(t, "SetNodeProperty(big bytes)", tx.SetNodeProperty("carol", "raw", lpg.BytesValue([]byte(big))))
	mustNil(t, "Commit(big value)", tx.Commit())

	g := h.recoverGraph(t)
	assertControlRecovered(t, g)

	v, ok := g.GetNodeProperty("carol", "blob")
	if !ok {
		t.Fatal("over-restricted: a 70000-byte node property value did not survive recovery")
	}
	if s, _ := v.String(); s != big {
		t.Fatalf("node property value corrupted: recovered %d bytes, wrote %d", len(s), n)
	}
	ev, ok := g.EdgeProperties("alice", "bob")["blob"]
	if !ok {
		t.Fatal("over-restricted: a 70000-byte edge property value did not survive recovery")
	}
	if s, _ := ev.String(); s != big {
		t.Fatalf("edge property value corrupted: recovered %d bytes, wrote %d", len(s), n)
	}
	rv, ok := g.GetNodeProperty("carol", "raw")
	if !ok {
		t.Fatal("over-restricted: a 70000-byte node property byte value did not survive recovery")
	}
	if b, _ := rv.Bytes(); len(b) != n {
		t.Fatalf("byte property value corrupted: recovered %d bytes, wrote %d", len(b), n)
	}
}

// TestStoreSurvivesRefusedOversizeCommit_2742 proves the fail-stop is a refusal
// and not a poisoning: after a commit is refused for an unencodable field, the
// store must still accept and durably record the next transaction.
func TestStoreSurvivesRefusedOversizeCommit_2742(t *testing.T) {
	t.Parallel()

	h := newBoundHarness(t)
	h.seedControl(t)

	big := strings.Repeat("X", uint16PrefixMax+1)
	tx := h.s.Begin()
	mustNil(t, "AddNode(doomed)", tx.AddNode("doomed"))
	bufErr := tx.SetNodeLabel("doomed", big)
	commitErr := tx.Commit()
	if bufErr == nil && commitErr == nil {
		t.Fatal("ACID Durability breach: the oversize label commit was acknowledged")
	}

	// The store must still work.
	tx2 := h.s.Begin()
	mustNil(t, "AddNode(dave)", tx2.AddNode("dave"))
	mustNil(t, "SetNodeLabel(after refusal)", tx2.SetNodeLabel("dave", "AfterRefusal"))
	mustNil(t, "Commit(after refusal)", tx2.Commit())

	g := h.recoverGraph(t)
	assertControlRecovered(t, g)
	if !g.HasNodeLabel("dave", "AfterRefusal") {
		t.Fatalf("the transaction after a refused commit did not survive recovery (labels %q)", g.NodeLabels("dave"))
	}
	if labels := g.NodeLabels("doomed"); len(labels) != 0 {
		lens := make([]int, len(labels))
		for i, s := range labels {
			lens[i] = len(s)
		}
		t.Fatalf("the refused oversize label left a residue after recovery: %d labels, byte lengths %v", len(labels), lens)
	}
}
