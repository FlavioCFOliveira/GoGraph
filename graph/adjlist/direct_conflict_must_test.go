package adjlist

import "testing"

// mustT unwraps a mutator result whose error the test does not expect: these
// tests run without concurrent transactions, so the direct-write conflict of
// rmp #2947 cannot occur, and an error fails the test rather than being
// discarded. It is a method value so the multi-value call can be its sole
// argument: must(t).B(a.RemoveEdgeByHandle(...)).
type mustT struct{ tb testing.TB }

func must(tb testing.TB) mustT { return mustT{tb: tb} }

// B unwraps a (bool, error) result.
func (m mustT) B(ok bool, err error) bool {
	m.tb.Helper()
	if err != nil {
		m.tb.Fatalf("unexpected error: %v", err)
	}
	return ok
}

// N unwraps an (int, error) result.
func (m mustT) N(n int, err error) int {
	m.tb.Helper()
	if err != nil {
		m.tb.Fatalf("unexpected error: %v", err)
	}
	return n
}

// E fails the test on an unexpected error.
func (m mustT) E(err error) {
	m.tb.Helper()
	if err != nil {
		m.tb.Fatalf("unexpected error: %v", err)
	}
}
