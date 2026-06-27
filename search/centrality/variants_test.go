package centrality

import (
	"errors"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// variants_test.go — regression gates for #1800 (sprint 252): closeness,
// harmonic, eigenvector, and Katz centrality. Expected values are hand-computed
// from the canonical definitions (see each algorithm's godoc citation).

const ctol = 1e-4

// buildC builds a CSR from the given undirected/directed edges and returns it
// with a key→NodeID resolver (NodeIDs are hashed by the adjlist mapper).
func buildC(t *testing.T, directed bool, nodes []string, edges [][2]string) (*csr.CSR[struct{}], func(string) int) {
	t.Helper()
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: directed})
	for _, nkey := range nodes {
		if err := a.AddNode(nkey); err != nil {
			t.Fatalf("AddNode %s: %v", nkey, err)
		}
	}
	for _, e := range edges {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			t.Fatalf("AddEdge %s->%s: %v", e[0], e[1], err)
		}
	}
	c := csr.BuildFromAdjList(a)
	return c, func(k string) int {
		id, ok := a.Mapper().Lookup(k)
		if !ok {
			t.Fatalf("node %q not interned", k)
		}
		return int(id)
	}
}

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > ctol {
		t.Errorf("%s = %.6f, want %.6f", name, got, want)
	}
}

func TestCloseness_1800(t *testing.T) {
	t.Run("single node", func(t *testing.T) {
		c, id := buildC(t, false, []string{"a"}, nil)
		out := Closeness(c)
		approx(t, "C(a)", out[id("a")], 0)
	})
	t.Run("path P3", func(t *testing.T) {
		c, id := buildC(t, false, nil, [][2]string{{"a", "b"}, {"b", "c"}})
		out := Closeness(c)
		approx(t, "C(b)", out[id("b")], 1.0)
		approx(t, "C(a)", out[id("a")], 2.0/3.0)
		approx(t, "C(c)", out[id("c")], 2.0/3.0)
	})
	t.Run("disconnected P3 + isolated d", func(t *testing.T) {
		c, id := buildC(t, false, []string{"d"}, [][2]string{{"a", "b"}, {"b", "c"}})
		out := Closeness(c)
		// n=4: WF C(b) = (2/3)*(2/2)=0.667; C(a)=(2/3)*(2/3)=0.444; C(d)=0.
		approx(t, "C(b)", out[id("b")], 2.0/3.0)
		approx(t, "C(a)", out[id("a")], 4.0/9.0)
		approx(t, "C(d)", out[id("d")], 0)
	})
	t.Run("star S3", func(t *testing.T) {
		c, id := buildC(t, false, nil, [][2]string{{"c", "x"}, {"c", "y"}, {"c", "z"}})
		out := Closeness(c)
		approx(t, "C(center)", out[id("c")], 1.0)
		approx(t, "C(leaf)", out[id("x")], 0.6)
	})
}

func TestHarmonic_1800(t *testing.T) {
	t.Run("path P3", func(t *testing.T) {
		c, id := buildC(t, false, nil, [][2]string{{"a", "b"}, {"b", "c"}})
		out := Harmonic(c)
		approx(t, "H(b)", out[id("b")], 1.0)  // (1+1)/2
		approx(t, "H(a)", out[id("a")], 0.75) // (1+0.5)/2
		approx(t, "H(c)", out[id("c")], 0.75)
	})
	t.Run("disconnected P3 + isolated d", func(t *testing.T) {
		c, id := buildC(t, false, []string{"d"}, [][2]string{{"a", "b"}, {"b", "c"}})
		out := Harmonic(c)
		// n=4, ÷3: H(b)=2/3, H(a)=1.5/3=0.5, H(d)=0.
		approx(t, "H(b)", out[id("b")], 2.0/3.0)
		approx(t, "H(a)", out[id("a")], 0.5)
		approx(t, "H(d)", out[id("d")], 0)
	})
	t.Run("directed path a->b->c (outgoing)", func(t *testing.T) {
		c, id := buildC(t, true, nil, [][2]string{{"a", "b"}, {"b", "c"}})
		out := Harmonic(c)
		// Outgoing convention, ÷(n-1)=÷2: a reaches b(1),c(2)=1.5/2=0.75; c reaches none=0.
		approx(t, "H(a)", out[id("a")], 0.75)
		approx(t, "H(c)", out[id("c")], 0)
	})
}

func TestEigenvector_1800(t *testing.T) {
	opts := DefaultEigenvectorOptions()
	t.Run("triangle K3", func(t *testing.T) {
		c, id := buildC(t, false, nil, [][2]string{{"a", "b"}, {"b", "c"}, {"a", "c"}})
		out, _, err := Eigenvector(c, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"a", "b", "c"} {
			approx(t, "x("+k+")", out[id(k)], 1.0/math.Sqrt(3))
		}
	})
	t.Run("K4", func(t *testing.T) {
		c, id := buildC(t, false, nil, [][2]string{{"a", "b"}, {"a", "c"}, {"a", "d"}, {"b", "c"}, {"b", "d"}, {"c", "d"}})
		out, _, err := Eigenvector(c, opts)
		if err != nil {
			t.Fatal(err)
		}
		approx(t, "x(a)", out[id("a")], 0.5)
	})
	t.Run("star S3", func(t *testing.T) {
		c, id := buildC(t, false, nil, [][2]string{{"c", "x"}, {"c", "y"}, {"c", "z"}})
		out, _, err := Eigenvector(c, opts)
		if err != nil {
			t.Fatal(err)
		}
		approx(t, "x(center)", out[id("c")], 1.0/math.Sqrt2) // √3/√6
		approx(t, "x(leaf)", out[id("x")], 1.0/math.Sqrt(6)) // 0.4082
	})
	t.Run("edgeless yields zeros", func(t *testing.T) {
		c, id := buildC(t, false, []string{"a", "b"}, nil)
		out, _, err := Eigenvector(c, opts)
		if err != nil {
			t.Fatal(err)
		}
		approx(t, "x(a)", out[id("a")], 0)
	})
	t.Run("bad options rejected", func(t *testing.T) {
		c, _ := buildC(t, false, nil, [][2]string{{"a", "b"}})
		if _, _, err := Eigenvector(c, EigenvectorOptions{MaxIterations: 100, Tolerance: 0}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("want ErrInvalidInput, got %v", err)
		}
	})
	t.Run("non-convergence reported", func(t *testing.T) {
		// One iteration with an impossibly tight tolerance cannot converge.
		c, _ := buildC(t, false, nil, [][2]string{{"a", "b"}, {"b", "c"}, {"a", "c"}, {"c", "d"}})
		if _, _, err := Eigenvector(c, EigenvectorOptions{MaxIterations: 1, Tolerance: 1e-15}); !errors.Is(err, ErrMaxStepsExceeded) {
			t.Fatalf("want ErrMaxStepsExceeded, got %v", err)
		}
	})
}

func TestKatz_1800(t *testing.T) {
	t.Run("triangle K3 alpha=0.1", func(t *testing.T) {
		c, id := buildC(t, false, nil, [][2]string{{"a", "b"}, {"b", "c"}, {"a", "c"}})
		out, _, err := Katz(c, KatzOptions{Alpha: 0.1, Beta: 1, MaxIterations: 1000, Tolerance: 1e-9})
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"a", "b", "c"} {
			approx(t, "katz("+k+")", out[id(k)], 1.0/math.Sqrt(3)) // all equal by symmetry
		}
	})
	t.Run("single directed edge a->b alpha=0.1", func(t *testing.T) {
		c, id := buildC(t, true, nil, [][2]string{{"a", "b"}})
		out, _, err := Katz(c, KatzOptions{Alpha: 0.1, Beta: 1, MaxIterations: 1000, Tolerance: 1e-12})
		if err != nil {
			t.Fatal(err)
		}
		// prenorm (x_a=1, x_b=1.1); L2 norm = sqrt(1+1.21)=1.48661.
		norm := math.Sqrt(1 + 1.21)
		approx(t, "katz(a)", out[id("a")], 1.0/norm)
		approx(t, "katz(b)", out[id("b")], 1.1/norm)
	})
	t.Run("directed path a->b->c alpha=0.1", func(t *testing.T) {
		c, id := buildC(t, true, nil, [][2]string{{"a", "b"}, {"b", "c"}})
		out, _, err := Katz(c, KatzOptions{Alpha: 0.1, Beta: 1, MaxIterations: 1000, Tolerance: 1e-12})
		if err != nil {
			t.Fatal(err)
		}
		norm := math.Sqrt(1 + 1.1*1.1 + 1.11*1.11)
		approx(t, "katz(a)", out[id("a")], 1.0/norm)
		approx(t, "katz(b)", out[id("b")], 1.1/norm)
		approx(t, "katz(c)", out[id("c")], 1.11/norm)
	})
	t.Run("isolated nodes score 0 (representation note)", func(t *testing.T) {
		// The immutable CSR cannot distinguish an isolated node from an unused
		// slot, so isolated nodes get 0 (not the textbook β floor) — consistent
		// with PageRank/Eigenvector. Pin that documented behaviour.
		c, id := buildC(t, false, []string{"a", "b"}, nil)
		out, _, err := Katz(c, DefaultKatzOptions())
		if err != nil {
			t.Fatal(err)
		}
		approx(t, "katz(a)", out[id("a")], 0)
		approx(t, "katz(b)", out[id("b")], 0)
	})
	t.Run("auto alpha converges", func(t *testing.T) {
		c, _ := buildC(t, false, nil, [][2]string{{"a", "b"}, {"b", "c"}, {"a", "c"}, {"c", "d"}})
		if _, _, err := Katz(c, DefaultKatzOptions()); err != nil {
			t.Fatalf("auto-alpha Katz should converge: %v", err)
		}
	})
}
