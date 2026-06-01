package graph

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// init registers the child handler before subproc.Dispatch() is called
// from TestMain. The handler interns a deterministic corpus — 500 int
// keys followed by 500 string keys — using the same two fast paths
// exercised in production (fnv1aUint64 and fnv1aString), then writes
// the resulting NodeIDs as a JSON array to stdout. The parent's
// TestMapper_FNV1a_CrossProcess test decodes that array and compares it
// element-by-element against its own interning of the same corpus.
func init() {
	subproc.Register("mapper-cross-process", func(_ []string) int {
		m := NewMapper[uint64]()
		ms := NewMapper[string]()

		type entry struct {
			ID  uint64 `json:"id"`
			Key string `json:"key"`
		}
		const (
			intCount = 500
			strCount = 500
		)
		out := make([]entry, 0, intCount+strCount)

		for i := uint64(0); i < intCount; i++ {
			out = append(out, entry{
				ID:  uint64(m.Intern(i)),
				Key: fmt.Sprintf("int:%d", i),
			})
		}
		for i := 0; i < strCount; i++ {
			k := fmt.Sprintf("key-%04d", i)
			out = append(out, entry{
				ID:  uint64(ms.Intern(k)),
				Key: "str:" + k,
			})
		}

		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "mapper-cross-process: encode: %v\n", err)
			return 1
		}
		return 0
	})
}

// TestMapper_FNV1a_CrossProcess verifies that FNV-1a shard placement is
// stable across separate process instances. It spawns a child via the
// "mapper-cross-process" subproc mode, which interns an identical corpus
// in a fresh process, and asserts that every (key → NodeID) assignment
// matches the parent's own interning.
//
// This is a stronger guarantee than TestMapper_NodeIDStableAcrossInstances
// (same process, two Mapper instances) because it exercises the full
// process boundary: the child has its own heap, its own seed-free
// FNV-1a constants, and its own per-shard intra-shard counters. Any
// residual process-local randomness (e.g. a maphash seed leaked into
// the fallback path) would surface here.
func TestMapper_FNV1a_CrossProcess(t *testing.T) {
	t.Parallel()

	const (
		intCount = 500
		strCount = 500
	)

	// Parent interns the same corpus in the same order.
	mInt := NewMapper[uint64]()
	mStr := NewMapper[string]()

	type entry struct {
		ID  uint64 `json:"id"`
		Key string `json:"key"`
	}

	parent := make([]entry, 0, intCount+strCount)
	for i := uint64(0); i < intCount; i++ {
		parent = append(parent, entry{
			ID:  uint64(mInt.Intern(i)),
			Key: fmt.Sprintf("int:%d", i),
		})
	}
	for i := 0; i < strCount; i++ {
		k := fmt.Sprintf("key-%04d", i)
		parent = append(parent, entry{
			ID:  uint64(mStr.Intern(k)),
			Key: "str:" + k,
		})
	}

	// Spawn child and collect its NodeID assignments.
	stdout, stderr, err := subproc.Run(t, "mapper-cross-process")
	if err != nil {
		t.Fatalf("subproc.Run: %v\nstderr: %s", err, stderr)
	}

	var child []entry
	if err := json.Unmarshal(stdout, &child); err != nil {
		t.Fatalf("decode child output: %v\nstdout: %s", err, stdout)
	}

	total := intCount + strCount
	if len(child) != total {
		t.Fatalf("child returned %d entries, want %d", len(child), total)
	}

	// Compare element-by-element to surface the exact key where drift
	// occurs, if any.
	mismatches := 0
	for i := range parent {
		if parent[i].Key != child[i].Key {
			t.Errorf("entry[%d]: key mismatch: parent=%q child=%q", i, parent[i].Key, child[i].Key)
			mismatches++
			continue
		}
		if parent[i].ID != child[i].ID {
			t.Errorf("key %q: parent NodeID=%d child NodeID=%d", parent[i].Key, parent[i].ID, child[i].ID)
			mismatches++
		}
	}
	if mismatches == 0 {
		t.Logf("all %d NodeIDs (int fast-path: %d, string fast-path: %d) match across process boundary",
			total, intCount, strCount)
	}
}
