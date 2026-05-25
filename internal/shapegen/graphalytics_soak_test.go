//go:build soak || nightly

package shapegen

import (
	"errors"
	"os"
	"testing"
)

// TestGraphalytics_DotaLeague_Soak exercises AC#1 / AC#2 / AC#3 on
// dota-league, the smallest Graphalytics reference graph by node
// count (61 170 nodes, 50 870 313 edges). It downloads the
// .tar.zst archive on first run, verifies the MD5 digest (SHA-256
// promoted once the SURF mirror re-stages and audits the entry),
// inflates it, parses the vertex and edge files, and asserts that
// Order / Size match the LDBC-published metadata.
//
// The test also exercises AC#2 by calling [LoadGraphalyticsReference]
// for each of the six canonical algorithm outputs and confirming that
// each returned reader is non-nil and can be closed cleanly.
func TestGraphalytics_DotaLeague_Soak(t *testing.T) {
	testGraphalyticsDataset(t, "dota-league")
}

// TestGraphalytics_KGS_Soak exercises the same AC chain on kgs
// (832 247 nodes, 17 891 698 edges). It is the medium-size canonical
// soak-layer fixture for the Graphalytics loader.
func TestGraphalytics_KGS_Soak(t *testing.T) {
	testGraphalyticsDataset(t, "kgs")
}

// TestGraphalytics_CitPatents_Soak exercises the same AC chain on
// cit-Patents (3 774 768 nodes, 16 518 947 edges). It is the
// largest of the three pinned datasets and may take several minutes
// to download from the SURF cold-storage mirror.
func TestGraphalytics_CitPatents_Soak(t *testing.T) {
	testGraphalyticsDataset(t, "cit-Patents")
}

// testGraphalyticsDataset is the shared body for all soak-layer
// Graphalytics tests. It runs the full AC#1/AC#2/AC#3 sequence for
// the named dataset.
func testGraphalyticsDataset(t *testing.T, name string) {
	t.Helper()
	cacheDir := graphalyticsTestCacheDir(t)

	// AC#1 / AC#3 — load and verify Order/Size against LDBC metadata.
	g, err := LoadGraphalytics(name, cacheDir)
	if errors.Is(err, ErrGraphalyticsStaging) {
		t.Skipf("Graphalytics %q is offline at SURF mirror (HTTP 409 staging) — skipped: %v", name, err)
	}
	if errors.Is(err, ErrGraphalyticsOffline) {
		t.Skipf("Graphalytics %q unreachable (no network or SURF error) — skipped: %v", name, err)
	}
	if err != nil {
		t.Fatalf("LoadGraphalytics(%q): %v", name, err)
	}

	ds := GraphalyticsDatasets[name]
	if got := g.AdjList().Order(); got != ds.Nodes {
		t.Errorf("Order = %d, want %d (LDBC-published)", got, ds.Nodes)
	}
	if got := g.AdjList().Size(); got != ds.Edges {
		t.Errorf("Size = %d, want %d (LDBC-published)", got, ds.Edges)
	}

	// AC#2 — reference output files accessible via LoadGraphalyticsReference.
	for _, alg := range GraphalyticsAlgorithms {
		rc, err := LoadGraphalyticsReference(name, alg, cacheDir)
		if os.IsNotExist(err) {
			// Some datasets ship only a subset of algorithm outputs;
			// a missing reference file is not a failure here, but we
			// log it for visibility.
			t.Logf("LoadGraphalyticsReference(%q, %q): reference file not present in archive (skipping)", name, alg)
			continue
		}
		if err != nil {
			t.Errorf("LoadGraphalyticsReference(%q, %q): %v", name, alg, err)
			continue
		}
		if rc == nil {
			t.Errorf("LoadGraphalyticsReference(%q, %q): returned nil reader", name, alg)
			continue
		}
		if cerr := rc.Close(); cerr != nil {
			t.Errorf("LoadGraphalyticsReference(%q, %q): Close: %v", name, alg, cerr)
		}
	}

	// Cache-replay: a second call must succeed without touching the network.
	if _, err := LoadGraphalytics(name, cacheDir); err != nil {
		t.Fatalf("LoadGraphalytics(%q) replay returned %v, want nil — cache must be re-used", name, err)
	}
}

// graphalyticsTestCacheDir returns the directory soak / nightly tests
// should use. When $GOGRAPH_GRAPHALYTICS_DIR is set (the convention
// pinned by [GraphalyticsDefaultCacheDir]) the tests reuse the
// pre-populated cache; otherwise they fall back to the default
// $HOME/.cache/gograph-graphalytics location.
func graphalyticsTestCacheDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv(graphalyticsCacheDirEnv); dir != "" {
		return dir
	}
	return GraphalyticsDefaultCacheDir()
}
