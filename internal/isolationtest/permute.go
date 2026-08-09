package isolationtest

import (
	"math/big"
	"strings"
)

// Permutation is one interleaving: a flat sequence of steps, each tagged with
// the index of the session that must run it.
type Permutation struct {
	// Steps are the step names in execution order.
	Steps []string
	// Owner[i] is the session index that owns Steps[i].
	Owner []int
}

// Name is the permutation's identity, and it is what makes a failing
// permutation re-runnable: it is stable across runs (the enumeration order is
// deterministic) and it is the exact string the golden output prints after
// "starting permutation: ".
func (p Permutation) Name() string { return strings.Join(p.Steps, " ") }

// Permutations returns every interleaving of the spec's steps that preserves
// each session's own step order — or, when the spec names permutations
// explicitly, exactly those.
//
// The enumeration is PostgreSQL's "piles" recursion
// (src/test/isolation/isolationtester.c, run_all_permutations_recurse, read at
// 0ec3f048): each session's remaining steps are a pile, and at every position
// the next step may be drawn from any non-empty pile. Drawing in every order
// yields every order-preserving interleaving exactly once, because a
// permutation is fully determined by WHICH pile each position drew from.
//
// The count is the multinomial coefficient (Σnᵢ)! / Πnᵢ!, which grows fast:
// two sessions of four steps is 70, three sessions of 4/4/2 is 3150, and three
// of five is 756 756. [CountPermutations] exists so a spec can be assigned to a
// test layer from its real size instead of a guess.
//
// The emission order is deterministic and is fixed by the session declaration
// order in the spec, so a golden file is stable.
func Permutations(s *Spec) []Permutation {
	if len(s.Permutations) > 0 {
		owner, _ := s.stepIndex()
		out := make([]Permutation, 0, len(s.Permutations))
		for _, names := range s.Permutations {
			p := Permutation{Steps: make([]string, len(names)), Owner: make([]int, len(names))}
			copy(p.Steps, names)
			for i, n := range names {
				p.Owner[i] = owner[n]
			}
			out = append(out, p)
		}
		return out
	}

	total := 0
	for _, sess := range s.Sessions {
		total += len(sess.Steps)
	}
	var (
		out   []Permutation
		piles = make([]int, len(s.Sessions))
		names = make([]string, total)
		owner = make([]int, total)
	)
	var recurse func(depth int)
	recurse = func(depth int) {
		drew := false
		for si, sess := range s.Sessions {
			if piles[si] >= len(sess.Steps) {
				continue
			}
			names[depth] = sess.Steps[piles[si]].Name
			owner[depth] = si
			piles[si]++
			recurse(depth + 1)
			piles[si]--
			drew = true
		}
		// Every pile empty: this branch of the recursion has produced a
		// complete permutation. Copied out, because the workspace is reused.
		if !drew {
			p := Permutation{Steps: make([]string, total), Owner: make([]int, total)}
			copy(p.Steps, names)
			copy(p.Owner, owner)
			out = append(out, p)
		}
	}
	recurse(0)
	return out
}

// CountPermutations returns how many interleavings [Permutations] would produce,
// WITHOUT building them.
//
// It exists so a spec's test-layer assignment can be justified by its real size
// rather than by eyeballing the step counts: the multinomial grows fast enough
// that a spec which looks small can be six figures. Computed in big.Int because
// the intermediate factorials overflow int64 well before the answer does.
func CountPermutations(s *Spec) *big.Int {
	if len(s.Permutations) > 0 {
		return big.NewInt(int64(len(s.Permutations)))
	}
	total := 0
	acc := big.NewInt(1)
	for _, sess := range s.Sessions {
		total += len(sess.Steps)
	}
	num := new(big.Int).MulRange(1, int64(total))
	for _, sess := range s.Sessions {
		if n := len(sess.Steps); n > 1 {
			acc.Mul(acc, new(big.Int).MulRange(1, int64(n)))
		}
	}
	return num.Div(num, acc)
}
