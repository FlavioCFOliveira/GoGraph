package isolationtest_test

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/isolationtest"
)

// discardWriter counts bytes without keeping them, so a test can assert that
// nothing was written.
type discardWriter struct{ n int }

func (d *discardWriter) Write(p []byte) (int, error) { d.n += len(p); return len(p), nil }

// runToString runs a spec and returns its transcript, failing the test on error.
func runToString(t *testing.T, s *isolationtest.Spec, r *isolationtest.Runner) string {
	t.Helper()
	var b strings.Builder
	if err := r.Run(t.Context(), s, &b); err != nil {
		t.Fatalf("run spec %s: %v", s.Name, err)
	}
	return b.String()
}

// sectionOf extracts one permutation's block from a transcript: the "starting
// permutation:" line and everything up to the next one.
func sectionOf(t *testing.T, transcript, perm string) string {
	t.Helper()
	head := "starting permutation: " + perm + "\n"
	i := strings.Index(transcript, head)
	if i < 0 {
		t.Fatalf("transcript has no permutation %q", perm)
	}
	rest := transcript[i:]
	if j := strings.Index(rest[len(head):], "\nstarting permutation: "); j >= 0 {
		rest = rest[:len(head)+j]
	}
	return strings.TrimRight(rest, "\n")
}
