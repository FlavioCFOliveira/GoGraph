package csrfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// errInjected is the fault every backend below returns, so a test can tell an
// injected failure from a real filesystem one.
var errInjected = errors.New("injected fault")

// failParentSyncFS is the production backend with the LAST step failing: the
// rename has already happened when the error is returned.
type failParentSyncFS struct{ osFS }

func (failParentSyncFS) ParentDirSync(string) error { return errInjected }

// failCreateFS fails the FIRST step, standing for every pre-rename failure: the
// previous generation is untouched and nothing was published.
type failCreateFS struct{ osFS }

func (failCreateFS) Create(string) (File, error) { return nil, errInjected }

// publishTestCSR builds a small CSR to publish.
func publishTestCSR(t *testing.T) *csr.CSR[struct{}] {
	t.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 8; i++ {
		if err := a.AddEdge(i, (i+1)%8, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	return csr.BuildFromAdjList(a)
}

// TestPublish_ParentFsyncFailure_IsDistinguishable guards rmp #2581.
//
// Every step before the rename leaves the previous generation intact and the
// temp removed, so an error from WriteToFile reads naturally as "not published".
// The parent-directory fsync is the one step that fails AFTER the rename, over a
// state where publication HAS occurred — and a caller that reacted by assuming
// the old file survived would be wrong.
//
// The test asserts the DISTINCTION, in both directions, because a sentinel that
// were returned for every failure would be no more informative than none.
func TestPublish_ParentFsyncFailure_IsDistinguishable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	out := filepath.Join(dir, "graph.csr")
	c := publishTestCSR(t)

	// A previous generation exists, so "the old file survived" is a claim the
	// test can actually check rather than assume.
	if _, err := WriteToFile(out, c); err != nil {
		t.Fatalf("seed the previous generation: %v", err)
	}
	before, err := os.ReadFile(out) //nolint:gosec // test fixture under t.TempDir
	if err != nil {
		t.Fatalf("read the previous generation: %v", err)
	}

	t.Run("parent fsync fails AFTER the rename", func(t *testing.T) {
		_, err := WriteToFileWith(failParentSyncFS{}, out, c)
		if err == nil {
			t.Fatalf("the parent-fsync fault did not surface at all")
		}
		if !errors.Is(err, ErrPublishedNotDurable) {
			t.Errorf("err = %v; want one satisfying errors.Is(err, ErrPublishedNotDurable). "+
				"Without it a caller cannot tell this from a pre-rename failure and would "+
				"wrongly assume the previous generation survived (rmp #2581)", err)
		}
		if !errors.Is(err, errInjected) {
			t.Errorf("err = %v; the underlying filesystem error must stay reachable through "+
				"the wrap", err)
		}
		// And the claim the sentinel makes must be TRUE: the file really is
		// published. A sentinel that lied would be worse than none.
		r, oerr := Open(out)
		if oerr != nil {
			t.Fatalf("ErrPublishedNotDurable claims the rename succeeded, but the published "+
				"file cannot be opened: %v", oerr)
		}
		_ = r.Close()
	})

	t.Run("a pre-rename failure does NOT claim publication", func(t *testing.T) {
		_, err := WriteToFileWith(failCreateFS{}, out, c)
		if err == nil {
			t.Fatalf("the create fault did not surface at all")
		}
		if errors.Is(err, ErrPublishedNotDurable) {
			t.Errorf("a pre-rename failure reports ErrPublishedNotDurable (%v): the sentinel "+
				"must separate the two failure modes, not be returned for both", err)
		}
		// The previous generation must be exactly as it was.
		after, rerr := os.ReadFile(out) //nolint:gosec // test fixture under t.TempDir
		if rerr != nil {
			t.Fatalf("the previous generation is unreadable after a pre-rename failure: %v", rerr)
		}
		if len(after) != len(before) {
			t.Errorf("the previous generation changed after a pre-rename failure: %d bytes, "+
				"want the original %d", len(after), len(before))
		}
		// The temp must not be left behind either.
		if _, serr := os.Stat(out + ".tmp"); serr == nil {
			t.Errorf("a pre-rename failure left %s behind", out+".tmp")
		}
	})
}

// TestPublish_SuccessReturnsNoSentinel is the control: an ordinary successful
// publish must not report either condition. Without it, a wrap applied
// unconditionally would satisfy the positive above.
func TestPublish_SuccessReturnsNoSentinel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	out := filepath.Join(dir, "graph.csr")

	if _, err := WriteToFile(out, publishTestCSR(t)); err != nil {
		t.Fatalf("an ordinary publish must succeed with a nil error, got %v", err)
	}
}
