package lpg

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"gograph/internal/subproc"
)

// fixedPropKeys is the ordered sequence interned by both the child
// process and the parent. The IDs produced must be identical across
// processes (sequential allocation, no per-process entropy).
var fixedPropKeys = []string{"name", "age", "score", "email", "active"}

func init() {
	// "prop-key-intern" interns fixedPropKeys in order via a fresh
	// PropertyKeyRegistry and prints the resulting key→ID map as JSON
	// to stdout.
	subproc.Register("prop-key-intern", func(_ []string) int {
		reg := NewPropertyKeyRegistry()
		m := make(map[string]PropertyKeyID, len(fixedPropKeys))
		for _, k := range fixedPropKeys {
			m[k] = reg.Intern(k)
		}
		if err := json.NewEncoder(os.Stdout).Encode(m); err != nil {
			fmt.Fprintf(os.Stderr, "prop-key-intern: encode: %v\n", err)
			return 1
		}
		return 0
	})
}

// TestCrossProc_PropertyKeyRegistry_StableIDs verifies that
// PropertyKeyRegistry assigns the same sequential IDs for the same
// insertion order in an independent child process. This guards against
// any source of per-process entropy (e.g. map-iteration randomisation)
// influencing ID assignment.
func TestCrossProc_PropertyKeyRegistry_StableIDs(t *testing.T) {
	t.Parallel()

	// Parent: intern the same keys in the same order.
	reg := NewPropertyKeyRegistry()
	wantIDs := make(map[string]PropertyKeyID, len(fixedPropKeys))
	for _, k := range fixedPropKeys {
		wantIDs[k] = reg.Intern(k)
	}

	// Child: independent process, same key order.
	stdout, stderr, err := subproc.Run(t, "prop-key-intern")
	if err != nil {
		t.Fatalf("child process failed: %v\nstderr: %s", err, stderr)
	}

	var gotIDs map[string]PropertyKeyID
	if decErr := json.Unmarshal(stdout, &gotIDs); decErr != nil {
		t.Fatalf("decode child JSON: %v\nstdout: %s", decErr, stdout)
	}

	for _, k := range fixedPropKeys {
		want := wantIDs[k]
		got, ok := gotIDs[k]
		if !ok {
			t.Errorf("key %q: missing from child output", k)
			continue
		}
		if got != want {
			t.Errorf("key %q: child ID=%d, parent ID=%d", k, got, want)
		}
	}
}
