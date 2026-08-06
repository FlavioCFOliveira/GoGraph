package snapshot

// registry_capture_selfheal_test.go — regression gate for #1880. The checkpoint
// phase-2 label/property-key component writers capture the registry name table
// and then walk the live nodes/edges without a lock spanning the two. A
// concurrent commit that interns a brand-new label/key between the two makes
// that name visible to the walk but absent from the capture; the collectors
// detect this. Because the registries are monotonic and append-only, a
// re-capture is guaranteed to include the new name, so WriteLabels/
// WriteProperties now retry (self-heal) instead of aborting the whole
// checkpoint attempt.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestSnapshotRegistryCapture_LabelSelfHeals reproduces the race deterministically
// at the collector level: capture a stale registry snapshot, then intern+attach
// a brand-new label (the concurrent-commit event), and confirm (a) the collector
// detects the missing name against the stale capture, and (b) a re-capture — the
// exact step the retry loop performs — makes the collector succeed. Timing-
// independent: the "concurrent" intern is applied explicitly between the two.
func TestSnapshotRegistryCapture_LabelSelfHeals(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}

	stale := snapshotRegistry(g.Registry())

	// The race: a fresh label is interned and attached after the capture.
	if err := g.SetNodeLabel("bob", "Admin"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}

	if _, err := collectNodeLabelRecords(g, nil, stale); err == nil {
		t.Fatal("collectNodeLabelRecords with a stale registry snapshot must detect the missing name and error")
	}

	fresh := snapshotRegistry(g.Registry())
	if _, err := collectNodeLabelRecords(g, nil, fresh); err != nil {
		t.Fatalf("collectNodeLabelRecords after re-capture must succeed (self-heal), got %v", err)
	}
}

// TestSnapshotRegistryCapture_PropertyKeySelfHeals is the property-key
// counterpart of the label self-heal test above.
func TestSnapshotRegistryCapture_PropertyKeySelfHeals(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("alice", "name", lpg.StringValue("a")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	stale := snapshotPropertyKeys(g.PropertyKeys())

	if err := g.SetNodeProperty("bob", "age", lpg.StringValue("b")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	if _, err := collectNodePropertyRecords(g, nil, stale, &propValueArena{}); err == nil {
		t.Fatal("collectNodePropertyRecords with a stale key snapshot must detect the missing key and error")
	}

	fresh := snapshotPropertyKeys(g.PropertyKeys())
	if _, err := collectNodePropertyRecords(g, nil, fresh, &propValueArena{}); err != nil {
		t.Fatalf("collectNodePropertyRecords after re-capture must succeed (self-heal), got %v", err)
	}
}
