package lpg

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/subproc"
)

// fixedLabels is the ordered sequence interned by both the child
// process and the parent. The IDs produced must be identical across
// processes (sequential allocation, no per-process entropy).
var fixedLabels = []string{"Person", "Company", "Location", "Event", "Product"}

func init() {
	// "label-registry-intern" interns fixedLabels in order via a fresh
	// LabelRegistry and prints the resulting label→ID map as JSON to
	// stdout.
	subproc.Register("label-registry-intern", func(_ []string) int {
		reg := NewLabelRegistry()
		m := make(map[string]LabelID, len(fixedLabels))
		for _, l := range fixedLabels {
			m[l] = reg.Intern(l)
		}
		if err := json.NewEncoder(os.Stdout).Encode(m); err != nil {
			fmt.Fprintf(os.Stderr, "label-registry-intern: encode: %v\n", err)
			return 1
		}
		return 0
	})
}

// TestCrossProc_LabelRegistry_StableIDs verifies that LabelRegistry
// assigns the same sequential IDs for the same insertion order in an
// independent child process. This guards against any source of
// per-process entropy (e.g. map-iteration randomisation) influencing
// ID assignment.
func TestCrossProc_LabelRegistry_StableIDs(t *testing.T) {
	t.Parallel()

	// Parent: intern the same labels in the same order.
	reg := NewLabelRegistry()
	wantIDs := make(map[string]LabelID, len(fixedLabels))
	for _, l := range fixedLabels {
		wantIDs[l] = reg.Intern(l)
	}

	// Child: independent process, same label order.
	stdout, stderr, err := subproc.Run(t, "label-registry-intern")
	if err != nil {
		t.Fatalf("child process failed: %v\nstderr: %s", err, stderr)
	}

	var gotIDs map[string]LabelID
	if decErr := json.Unmarshal(stdout, &gotIDs); decErr != nil {
		t.Fatalf("decode child JSON: %v\nstdout: %s", decErr, stdout)
	}

	for _, l := range fixedLabels {
		want := wantIDs[l]
		got, ok := gotIDs[l]
		if !ok {
			t.Errorf("label %q: missing from child output", l)
			continue
		}
		if got != want {
			t.Errorf("label %q: child ID=%d, parent ID=%d", l, got, want)
		}
	}
}
