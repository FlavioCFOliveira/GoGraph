//go:build soak || nightly

package shapegen

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSNAP_CitHepPh_Soak exercises AC#1 / AC#2 / AC#3 on the small
// cit-HepPh archive (~1.5 MB). It downloads on first run, verifies
// SHA-256, parses the gzipped edge list, and asserts Order/Size
// match the SNAP-published metadata. A second invocation against
// the same cache directory must not touch the network.
func TestSNAP_CitHepPh_Soak(t *testing.T) {
	cacheDir := snapTestCacheDir(t)
	g, err := CitHepPh(cacheDir)
	if errors.Is(err, ErrSNAPOffline) {
		t.Skipf("SNAP offline (no network, %v) — cit-HepPh soak skipped", err)
	}
	if err != nil {
		t.Fatalf("CitHepPh: %v", err)
	}
	ds := SNAPDatasets["cit-HepPh"]
	if got := g.AdjList().Order(); got != ds.Nodes {
		t.Errorf("Order = %d, want %d (SNAP-published)", got, ds.Nodes)
	}
	if got := g.AdjList().Size(); got != ds.Edges {
		t.Errorf("Size = %d, want %d (SNAP-published)", got, ds.Edges)
	}
	// Cache-replay: cache file must exist and the next invocation
	// must succeed without invoking the network.
	cachePath := filepath.Join(cacheDir, ds.Name+".txt.gz")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file missing after first load: %v", err)
	}
	if _, err := CitHepPh(cacheDir); err != nil {
		t.Fatalf("CitHepPh (replay) returned %v, want nil — cache must be re-used", err)
	}
}

// TestSNAP_WebGoogle_Soak exercises the same AC chain on web-Google
// (~20 MB, 875 713 nodes). It is the canonical soak-layer fixture
// declared by the task AC #4.
func TestSNAP_WebGoogle_Soak(t *testing.T) {
	cacheDir := snapTestCacheDir(t)
	g, err := WebGoogle(cacheDir)
	if errors.Is(err, ErrSNAPOffline) {
		t.Skipf("SNAP offline (no network, %v) — web-Google soak skipped", err)
	}
	if err != nil {
		t.Fatalf("WebGoogle: %v", err)
	}
	ds := SNAPDatasets["web-Google"]
	if got := g.AdjList().Order(); got != ds.Nodes {
		t.Errorf("Order = %d, want %d (SNAP-published)", got, ds.Nodes)
	}
	if got := g.AdjList().Size(); got != ds.Edges {
		t.Errorf("Size = %d, want %d (SNAP-published)", got, ds.Edges)
	}
}

// snapTestCacheDir returns the cache directory the soak/nightly
// tests should use. When the user has populated $GOGRAPH_SNAP_DIR
// (the convention pinned by SNAPDefaultCacheDir) the tests reuse
// it; otherwise they fall back to the default
// $HOME/.cache/gograph-snap location so a cold first run still
// works without manual setup.
func snapTestCacheDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv(snapCacheDirEnv); dir != "" {
		return dir
	}
	return SNAPDefaultCacheDir()
}
