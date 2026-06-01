package csv_test

import (
	"bytes"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	csv "github.com/FlavioCFOliveira/GoGraph/graph/io/csv"
)

// TestCSVRead_UnicodeAstralPlane verifies that node IDs spanning the
// full Unicode range — including multi-byte BMP sequences, CJK, and
// astral-plane code points — survive a Write → ReadInto round-trip.
func TestCSVRead_UnicodeAstralPlane(t *testing.T) {
	t.Parallel()

	pairs := []struct{ src, dst string }{
		{"café", "naïve"},
		{"日本語", "한국어"},
		{"\U0001D49C\U0001D4B7\U0001D4B8", "math-script"}, // U+1D49C 𝒜 range
		{"\U0001F600node", "\U0001F601node"},              // emoji
		{"naïve", "café"},
	}

	for _, p := range pairs {
		p := p
		t.Run(p.src+"→"+p.dst, func(t *testing.T) {
			t.Parallel()

			a := adjlist.New[string, int64](adjlist.Config{Directed: true})
			if err := a.AddEdge(p.src, p.dst, 1); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}

			var buf bytes.Buffer
			if _, err := csv.Write(&buf, a, csv.DefaultOptions()); err != nil {
				t.Fatalf("Write: %v", err)
			}

			b, _, err := csv.ReadInto(&buf, csv.DefaultOptions())
			if err != nil {
				t.Fatalf("ReadInto: %v", err)
			}
			if !b.HasEdge(p.src, p.dst) {
				t.Errorf("edge (%q -> %q) lost after Unicode round-trip", p.src, p.dst)
			}
		})
	}
}
