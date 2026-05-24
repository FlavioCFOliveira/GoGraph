//go:build nightly

package shapegen

import (
	"errors"
	"testing"
)

// TestSNAP_SocLiveJournal1_Nightly exercises AC #4: the heaviest
// dataset (soc-LiveJournal1, ~250 MB compressed, 4.85 M nodes,
// 69 M edges) lives in the nightly layer because of its size and
// parse time. Failing to reach snap.stanford.edu yields a clean
// t.Skip rather than a hard failure.
func TestSNAP_SocLiveJournal1_Nightly(t *testing.T) {
	cacheDir := snapTestCacheDir(t)
	g, err := SocLiveJournal1(cacheDir)
	if errors.Is(err, ErrSNAPOffline) {
		t.Skipf("SNAP offline (no network, %v) — soc-LiveJournal1 nightly skipped", err)
	}
	if err != nil {
		t.Fatalf("SocLiveJournal1: %v", err)
	}
	ds := SNAPDatasets["soc-LiveJournal1"]
	if got := g.AdjList().Order(); got != ds.Nodes {
		t.Errorf("Order = %d, want %d (SNAP-published)", got, ds.Nodes)
	}
	if got := g.AdjList().Size(); got != ds.Edges {
		t.Errorf("Size = %d, want %d (SNAP-published)", got, ds.Edges)
	}
}
