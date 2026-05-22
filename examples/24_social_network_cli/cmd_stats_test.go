package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestRunStats_AfterSeedMatchesGolden verifies that the stats output
// after seeding equals the byte-exact golden fixture in
// testdata/seed_stats.json. The single-process flow keeps the mapper
// seed stable across calls so NodeIDs are reproducible.
func TestRunStats_AfterSeedMatchesGolden(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}

	// Apply the fixture via the same helper cmdSeed calls.
	o, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("openStore (seed): %v", err)
	}
	if _, err := seedFixture(o.store); err != nil {
		_ = o.Close()
		t.Fatalf("seedFixture: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatalf("close (seed): %v", err)
	}

	var buf bytes.Buffer
	if err := runStats(context.Background(), dir, &buf); err != nil {
		t.Fatalf("runStats: %v", err)
	}

	golden, err := os.ReadFile(filepath.Join("testdata", "seed_stats.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(buf.Bytes()) != string(golden) {
		t.Fatalf("stats output mismatch:\n  got:  %s\n  want: %s", buf.String(), string(golden))
	}
}

// TestRunStats_EmptyDir verifies that stats over a freshly-initialised
// directory returns zeros for every key.
func TestRunStats_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := initEmpty(dir); err != nil {
		t.Fatalf("initEmpty: %v", err)
	}
	var buf bytes.Buffer
	if err := runStats(context.Background(), dir, &buf); err != nil {
		t.Fatalf("runStats: %v", err)
	}
	want := `{"authored":0,"comments":0,"follows":0,"likes":0,"on":0,"posts":0,"replies":0,"users":0}` + "\n"
	if buf.String() != want {
		t.Fatalf("empty stats mismatch:\n  got:  %s\n  want: %s", buf.String(), want)
	}
}
