package flow

import "testing"

func TestStoerWagner_Triangle(t *testing.T) {
	t.Parallel()
	// Triangle 0-1-2 with weights 1, 2, 3.
	// Min cut = 1+2 = 3 (cut between {2} and {0,1}).
	n := 3
	w := make([]int, n*n)
	set := func(i, j, v int) { w[i*n+j] = v; w[j*n+i] = v }
	set(0, 1, 1)
	set(1, 2, 2)
	set(0, 2, 3)
	r := StoerWagner(w, n)
	if r.Weight != 3 {
		t.Fatalf("Weight = %d, want 3", r.Weight)
	}
}

func TestStoerWagner_StoerWagnerExample(t *testing.T) {
	t.Parallel()
	// The standard 8-node example from Stoer & Wagner 1997.
	// Result: minimum cut weight = 4.
	const n = 8
	w := make([]int, n*n)
	set := func(i, j, v int) { w[i*n+j] = v; w[j*n+i] = v }
	set(0, 1, 2)
	set(0, 4, 3)
	set(1, 2, 3)
	set(1, 4, 2)
	set(1, 5, 2)
	set(2, 3, 4)
	set(2, 6, 2)
	set(3, 6, 2)
	set(3, 7, 2)
	set(4, 5, 3)
	set(5, 6, 1)
	set(6, 7, 3)
	r := StoerWagner(w, n)
	if r.Weight != 4 {
		t.Fatalf("min cut = %d, want 4", r.Weight)
	}
}

func TestStoerWagner_SingleNode(t *testing.T) {
	t.Parallel()
	if r := StoerWagner([]int{}, 0); r.Weight != 0 {
		t.Fatalf("empty: %+v", r)
	}
}
