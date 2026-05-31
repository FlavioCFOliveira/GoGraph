package tck_test

import (
	"archive/zip"
	"io"
	"testing"
	"time"
)

// fixtureZip is the vendored, pinned IANA time-zone database that the CI
// workflows and the Makefile expose to the test process via $ZONEINFO. See
// cypher/tck/testdata/README.md for provenance and regeneration.
const fixtureZip = "testdata/zoneinfo-slim.zip"

// TestZoneinfoFixtureIsUsableAndSlim guards the two properties the TCK temporal
// scenarios depend on, in a way that cannot be masked by Go silently falling
// back to the host's own system database:
//
//  1. Every entry is STORED (uncompressed). Go's minimal time-package zip
//     reader rejects compressed entries with "unsupported compression" and then
//     falls back to the next $ZONEINFO source — reintroducing host-dependence.
//  2. Europe/Stockholm at a pre-standard-time instant resolves to +00:53:28,
//     the offset fixed verbatim in the upstream openCypher TCK
//     (features/expressions/temporal/Temporal2.feature). This is read straight
//     from the archive via time.LoadLocationFromTZData, so it reflects the
//     vendored data — not whatever tzdata the test host happens to ship.
func TestZoneinfoFixtureIsUsableAndSlim(t *testing.T) {
	r, err := zip.OpenReader(fixtureZip)
	if err != nil {
		t.Fatalf("open %s: %v", fixtureZip, err)
	}
	defer r.Close()

	if len(r.File) == 0 {
		t.Fatalf("%s contains no entries", fixtureZip)
	}

	var stockholm []byte
	for _, f := range r.File {
		if f.Method != zip.Store {
			t.Errorf("entry %q uses compression method %d; Go's time-package zip "+
				"reader only accepts stored (method 0) entries and will otherwise "+
				"fall back to the host system tzdata — regenerate with "+
				"scripts/gen_tck_tzdata.sh (which uses `zip -0`)", f.Name, f.Method)
		}
		if f.Name == "Europe/Stockholm" {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open %q: %v", f.Name, err)
			}
			stockholm, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatalf("read %q: %v", f.Name, err)
			}
		}
	}
	if t.Failed() {
		return
	}
	if stockholm == nil {
		t.Fatalf("Europe/Stockholm not found in %s", fixtureZip)
	}

	loc, err := time.LoadLocationFromTZData("Europe/Stockholm", stockholm)
	if err != nil {
		t.Fatalf("parse Europe/Stockholm tzdata: %v", err)
	}
	got := time.Date(1818, 7, 21, 21, 40, 32, 142000000, loc).Format("-07:00:00")
	const want = "+00:53:28"
	if got != want {
		t.Errorf("Europe/Stockholm @ 1818-07-21 offset = %s, want %s; the pinned "+
			"IANA release or zic build mode no longer matches the upstream "+
			"openCypher TCK expectation", got, want)
	}
}
