// Command fmtfixture regenerates the frozen on-disk fixtures used by
// the rolling-upgrade compatibility tests in store/wal,
// store/snapshot, and store/csrfile. Run it whenever a writer in one
// of those packages changes the on-disk shape; commit the refreshed
// fixtures alongside the writer change.
//
// Usage:
//
//	go run ./cmd/fmtfixture            # refresh every fixture
//	go run ./cmd/fmtfixture -pkg wal   # refresh just the WAL fixture
//
// The fixtures themselves live under each package's testdata/v<N>/
// tree and are read by *_compat_test.go in that same package.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/store/csrfile"
	"gograph/store/snapshot"
	"gograph/store/wal"
)

// FixedTime is the deterministic timestamp embedded in any
// time-bearing fixture (e.g., the snapshot manifest's CreatedAt).
// Pinning it makes the fixtures byte-for-byte reproducible.
var FixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func main() {
	pkg := flag.String("pkg", "all", "target package: wal|snapshot|csrfile|all")
	flag.Parse()
	switch *pkg {
	case "wal":
		mustWriteWALFixture()
	case "snapshot":
		mustWriteSnapshotFixture()
	case "csrfile":
		mustWriteCSRFileFixture()
	case "all":
		mustWriteWALFixture()
		mustWriteSnapshotFixture()
		mustWriteCSRFileFixture()
	default:
		log.Fatalf("unknown -pkg %q (want one of: wal, snapshot, csrfile, all)", *pkg)
	}
}

func mustWriteWALFixture() {
	dst := filepath.Join("store", "wal", "testdata", "v1", "sample.wal")
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		log.Fatal(err)
	}
	_ = os.Remove(dst)
	w, err := wal.Open(dst)
	if err != nil {
		log.Fatal(err)
	}
	payloads := []string{"alpha", "beta", "gamma", "delta", "omega"}
	for _, p := range payloads {
		if err := w.Append([]byte(p)); err != nil {
			log.Fatal(err)
		}
	}
	if err := w.Sync(); err != nil {
		log.Fatal(err)
	}
	if err := w.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("fmtfixture: wrote %s (5 frames)\n", dst)
}

func mustWriteSnapshotFixture() {
	dst := filepath.Join("store", "snapshot", "testdata", "v1", "sample")
	if err := os.RemoveAll(dst); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o750); err != nil {
		log.Fatal(err)
	}
	// Build a deterministic 3-node graph.
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	a.AddEdge(0, 1, 11)
	a.AddEdge(0, 2, 12)
	a.AddEdge(1, 2, 22)
	c := csr.BuildFromAdjList(a)

	csrPath := filepath.Join(dst, snapshot.CSRFile)
	cf, err := os.Create(csrPath) //nolint:gosec // testdata path
	if err != nil {
		log.Fatal(err)
	}
	size, csum, err := snapshot.WriteCSR(cf, c)
	if err != nil {
		log.Fatal(err)
	}
	if err := cf.Close(); err != nil {
		log.Fatal(err)
	}

	m := snapshot.Manifest{
		// Pin to manifest version 1 explicitly: this fixture is the
		// canonical v1 sample used by store/snapshot's compat tests.
		// snapshot.ManifestVersion bumps over time as new components
		// are added (v2 added labels.bin); the v1 fixture must stay
		// v1 forever to preserve the forward-compat guarantee.
		Version:   1,
		CreatedAt: FixedTime,
		Order:     c.Order(),
		Size:      c.Size(),
		Files: []snapshot.FileEntry{
			{Name: snapshot.CSRFile, Size: size, CRC32C: csum},
		},
	}
	manifestPath := filepath.Join(dst, "manifest.json")
	mf, err := os.Create(manifestPath) //nolint:gosec // testdata path
	if err != nil {
		log.Fatal(err)
	}
	enc := json.NewEncoder(mf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		log.Fatal(err)
	}
	if err := mf.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("fmtfixture: wrote %s/{manifest.json, csr.bin}\n", dst)
}

func mustWriteCSRFileFixture() {
	dst := filepath.Join("store", "csrfile", "testdata", "v1", "sample.csr")
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		log.Fatal(err)
	}
	_ = os.Remove(dst)
	c := csrfile.BuildFixture(csrfile.FixtureSpec{
		Vertices: 32,
		Edges:    96,
		Seed:     0x1337,
	})
	if _, err := csrfile.WriteToFile(dst, c); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("fmtfixture: wrote %s (V=32, E=96, unweighted)\n", dst)
}
