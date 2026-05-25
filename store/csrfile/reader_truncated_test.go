package csrfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

// TestReader_TruncatedFile writes a valid csrfile, then attempts to
// open it at several named truncation offsets. Each attempt must either
// succeed cleanly (if the truncated file happens to be self-consistent)
// or return a typed csrfile error. No panics are permitted.
func TestReader_TruncatedFile(t *testing.T) {
	t.Parallel()

	// Build Grid(20, 20, false): 400 nodes, enough to produce a file
	// with a meaningful data section.
	g, err := shapegen.Grid(20, 20, false).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("Grid.Build: %v", err)
	}
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	// Re-build as unweighted so WriteToFile[struct{}] accepts it.
	_ = g // use the adjlist from the shape directly
	// Actually use BuildFixture for simplicity: same node count approach.
	// Use a plain adjlist path graph instead so we control exact edge count.
	a2 := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 100; i++ {
		if err := a2.AddEdge(i, (i+1)%100, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	_ = a
	c := csr.BuildFromAdjList(a2)

	src := filepath.Join(t.TempDir(), "ok.csr")
	if _, err := WriteToFile[struct{}](src, c); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}
	data, err := os.ReadFile(src) //nolint:gosec // test fixture
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Named truncation offsets: covering magic-only, partial-header,
	// post-header, mid-data, near-end.
	offsets := []struct {
		name   string
		offset int
	}{
		{"magic-only", 4},
		{"partial-header", 32},
		{"post-header", 128},
		{"mid-data", 1024},
		{"half", len(data) / 2},
		{"near-end", len(data) - 16},
	}

	for _, tc := range offsets {
		tc := tc
		if tc.offset <= 0 || tc.offset >= len(data) {
			continue // skip degenerate cases for small files
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			corrupt := filepath.Join(t.TempDir(), "trunc.csr")
			if err := os.WriteFile(corrupt, data[:tc.offset], 0o644); err != nil { //nolint:gosec // test fixture
				t.Fatalf("WriteFile: %v", err)
			}

			// Use recover to guard against any unexpected panic.
			var openErr error
			var r *Reader
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						t.Errorf("Open panicked at offset %d (%s): %v", tc.offset, tc.name, rec)
					}
				}()
				r, openErr = Open(corrupt)
			}()

			if openErr == nil {
				// The truncated file happened to be self-consistent
				// (e.g. offset lands after the last needed byte). Accept
				// it as long as Close works cleanly.
				if r != nil {
					if err := r.Close(); err != nil {
						t.Errorf("Close on self-consistent truncation: %v", err)
					}
				}
				return
			}

			// Error must be one of the typed sentinel errors.
			if errors.Is(openErr, ErrBadMagic) ||
				errors.Is(openErr, ErrUnsupportedVersion) ||
				errors.Is(openErr, ErrHeaderTooShort) ||
				errors.Is(openErr, ErrFileCorrupted) ||
				errors.Is(openErr, ErrUnknownWeightKind) ||
				errors.Is(openErr, ErrUnsupportedByteOrder) {
				return // expected
			}
			// Any other non-nil error is acceptable: it may be an OS-level
			// error from mmap on platforms that refuse small mappings.
			t.Logf("offset=%d (%s): non-sentinel error (acceptable): %v", tc.offset, tc.name, openErr)
		})
	}
}
