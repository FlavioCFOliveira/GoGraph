package graph

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"

	"gograph/internal/subproc"
)

// makePermutedKeys generates 200 fixed string keys ("key-0000"…"key-0199")
// and shuffles them using a PCG source seeded by seed. The same seed always
// produces the same permutation, so parent and child converge on identical
// insertion order.
func makePermutedKeys(seed uint64, n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%04d", i)
	}
	r := rand.New(rand.NewPCG(seed, 0)) //nolint:gosec // intentionally non-cryptographic: deterministic permutation for cross-process reproducibility
	r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	return keys
}

func init() {
	// Register the child handler before subproc.Dispatch() is called from
	// TestMain. The handler receives a decimal seed string, interns 200 keys
	// in the permuted order derived from that seed, and emits a JSON map of
	// key→NodeID to stdout.
	subproc.Register("mapper-permuted-proc", func(args []string) int {
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "mapper-permuted-proc: missing seed argument")
			return 1
		}
		seed, err := strconv.ParseUint(args[0], 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mapper-permuted-proc: bad seed %q: %v\n", args[0], err)
			return 1
		}
		keys := makePermutedKeys(seed, 200)
		m := NewMapper[string]()
		result := make(map[string]uint64, len(keys))
		for _, k := range keys {
			result[k] = uint64(m.Intern(k))
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fmt.Fprintf(os.Stderr, "mapper-permuted-proc: encode: %v\n", err)
			return 1
		}
		return 0
	})
}

// TestMapper_FNV1a_Permuted_CrossProcess verifies that FNV-1a shard assignment
// is stable across processes even when different permutations of the same key
// set are used.
//
// The shard index is fully deterministic (hash of key, no per-process entropy).
// The intra-shard counter is insertion-order-dependent: the n-th distinct key
// that maps to shard S receives intra-shard index n within S. Cross-process
// agreement therefore requires the same insertion order in parent and child.
//
// For each of 100 seeds the parent:
//  1. Derives a permutation of 200 keys using that seed.
//  2. Interns the keys in that order locally.
//  3. Spawns a child with the same seed.
//  4. Decodes the child's JSON map and compares every NodeID.
//
// A mismatch would indicate residual process-local entropy (e.g. a maphash
// seed leaked into the fallback dispatch path of mapperShardFor).
func TestMapper_FNV1a_Permuted_CrossProcess(t *testing.T) {
	t.Parallel()

	const (
		seeds  = 100
		nKeys  = 200
		format = 10
	)

	for i := uint64(0); i < seeds; i++ {
		seed := i
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()

			// Parent: intern in permuted order.
			keys := makePermutedKeys(seed, nKeys)
			m := NewMapper[string]()
			parentIDs := make(map[string]uint64, nKeys)
			for _, k := range keys {
				parentIDs[k] = uint64(m.Intern(k))
			}

			// Child: same permutation, same order.
			stdout, stderr, err := subproc.Run(t, "mapper-permuted-proc",
				strconv.FormatUint(seed, format))
			if err != nil {
				t.Fatalf("subproc.Run (seed=%d): %v\nstderr: %s", seed, err, stderr)
			}

			var childIDs map[string]uint64
			if err := json.Unmarshal(stdout, &childIDs); err != nil {
				t.Fatalf("decode child output (seed=%d): %v\nstdout: %s", seed, err, stdout)
			}

			if len(childIDs) != nKeys {
				t.Fatalf("seed=%d: child returned %d entries, want %d",
					seed, len(childIDs), nKeys)
			}

			mismatches := 0
			for k, parentID := range parentIDs {
				childID, ok := childIDs[k]
				if !ok {
					t.Errorf("seed=%d: key %q missing from child output", seed, k)
					mismatches++
					continue
				}
				if parentID != childID {
					t.Errorf("seed=%d: key %q: parent NodeID=%d child NodeID=%d",
						seed, k, parentID, childID)
					mismatches++
				}
			}
			if mismatches == 0 {
				t.Logf("seed=%d: all %d NodeIDs match across process boundary", seed, nKeys)
			}
		})
	}
}
