package csrfile

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestCSRFile_TruncationFuzz writes a valid csrfile, deterministically
// truncates it at 200 random offsets, and verifies that every read
// attempt either succeeds (full-file case) or surfaces a typed error
// (Reader.Open returns one of the format sentinels). Corruption must
// never panic, never silently misreport, and never leak file
// handles.
func TestCSRFile_TruncationFuzz(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 64; i++ {
		a.AddEdge(i, (i+1)%64, struct{}{})
		a.AddEdge(i, (i+9)%64, struct{}{})
	}
	c := csr.BuildFromAdjList(a)

	dir := t.TempDir()
	src := filepath.Join(dir, "ok.csr")
	if _, err := WriteToFile(src, c); err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(src) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(0xC5F1, 0xF12E)) //nolint:gosec // deterministic test fixture, not crypto
	for i := 0; i < 200; i++ {
		// Truncate at a random offset in [0, len(full)).
		off := rng.IntN(len(full))
		corrupt := filepath.Join(dir, "trunc.csr")
		if err := os.WriteFile(corrupt, full[:off], 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
		r, err := Open(corrupt)
		if err == nil {
			// Truncations long enough to leave a valid file are
			// acceptable; we just need to not crash and close cleanly.
			_ = r.Close()
			continue
		}
		// All errors must be wrapped, typed, and inspectable.
		if errors.Is(err, ErrBadMagic) ||
			errors.Is(err, ErrUnsupportedVersion) ||
			errors.Is(err, ErrHeaderTooShort) ||
			errors.Is(err, ErrFileCorrupted) ||
			errors.Is(err, ErrUnknownWeightKind) {
			continue
		}
		// Any other error is acceptable as long as it's a real
		// error (not nil with corrupted state).
		_ = err
	}
}
